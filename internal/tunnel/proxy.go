package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/proxy"
	"github.com/lifei6671/xtunnel/internal/safego"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
	serverworkpool "github.com/lifei6671/xtunnel/internal/server/workpool"
)

var (
	// ErrInvalidOptions 表示数据面缺少 Runtime、Session Pool 或 OPEN Handler。
	ErrInvalidOptions = errors.New("tunnel proxy options are invalid")
	// ErrSessionPoolUnavailable 表示负载算法选中的 Connector 已经被新 generation 替换。
	ErrSessionPoolUnavailable = errors.New("selected connector session pool is unavailable")
)

// Options 固定 Tunnel 数据面依赖与等待 IDLE WorkConn 的上限。
type Options struct {
	Registry       *serverruntime.Registry
	Sessions       *sessionruntime.Manager
	OpenHandler    *serveropen.Handler
	AcquireTimeout time.Duration
	// LimitManager 为 nil 时保留纯单元测试的无限预算路径；生产装配和 M1 集成测试
	// 必须传共享 Manager，使 PendingOpen 与 ACTIVE 多维限制作用于真实公网连接。
	LimitManager *serverlimits.Manager
}

// Proxy 使用 Route 已解析出的 Tunnel/Service 身份，把一个公网连接交给默认负载选择的
// Connector，并在 OPEN_OK 后逐字节双向转发。
type Proxy struct {
	options Options

	pendingMu        sync.Mutex
	pendingGroups    map[string]*pendingGroup
	pendingBySession map[serverruntime.Session]uint32
	newConnectionID  func() (string, error)
	// afterAlternateAcquire 仅为同 package 的确定性竞态测试提供提交前同步点。
	afterAlternateAcquire func(serverruntime.Session)
}

type pendingGroup struct {
	session serverruntime.Session
	pool    *serverworkpool.Pool
	waiters uint32
	done    chan struct{}
}

type pendingMembership struct {
	proxy    *Proxy
	tunnelID string
	group    *pendingGroup
	once     sync.Once
	err      error
}

// NewProxy 创建与具体 TCP/HTTP Listener 无关的数据面。
func NewProxy(options Options) (*Proxy, error) {
	if options.Registry == nil || options.Sessions == nil || options.OpenHandler == nil || options.AcquireTimeout <= 0 {
		return nil, ErrInvalidOptions
	}
	return &Proxy{
		options:          options,
		pendingGroups:    make(map[string]*pendingGroup),
		pendingBySession: make(map[serverruntime.Session]uint32),
		newConnectionID:  identity.NewConnectionID,
	}, nil
}

// Serve 处理一条业务连接。Service 直接属于 Tunnel；线协议只发送 service_id，Agent
// 必须从该 Tunnel 当前已应用的 Service Snapshot 解析 Origin。
func (tunnelProxy *Proxy) Serve(
	ctx context.Context,
	tunnelID, serviceID string,
	ingress protocolv1.IngressType,
	peer net.Conn,
) (resultErr error) {
	if tunnelProxy == nil || ctx == nil || peer == nil ||
		identity.ValidateTunnelID(tunnelID) != nil || validate.ValidateID(serviceID, "svc_") != nil ||
		ingress == protocolv1.IngressType_INGRESS_TYPE_UNSPECIFIED {
		if peer != nil {
			_ = peer.Close()
		}
		return ErrInvalidOptions
	}
	defer func() {
		if err := peer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close public peer: %w", err))
		}
	}()
	var openLease *serverlimits.OpenLease
	if tunnelProxy.options.LimitManager != nil {
		sourceIP, err := sourceAddress(peer.RemoteAddr())
		if err != nil {
			return err
		}
		openLease, err = tunnelProxy.options.LimitManager.AcquirePendingOpen(serverlimits.ConnectionKey{
			TunnelID: tunnelID, ServiceID: serviceID, SourceIP: sourceIP,
		})
		if err != nil {
			return err
		}
		defer openLease.Release()
	}

	// connection_id 在任何 Connector/Work 所有权取得前生成。若系统随机源
	// 失败，直接返回，不会把 IDLE Work 留在 OPENING。
	connectionID, err := tunnelProxy.newConnectionID()
	if err != nil {
		return err
	}

	connectorLease, session, pool, selectedWork, err := tunnelProxy.acquireWork(ctx, tunnelID, serviceID)
	if err != nil {
		return err
	}
	leaseOwnedByActive := false
	defer func() {
		if !leaseOwnedByActive {
			connectorLease.Release()
		}
	}()
	tunnelRuntime, err := tunnelProxy.options.Registry.Tunnel(tunnelID)
	if err != nil {
		return err
	}
	openSelectedWork := func(selectedSession serverruntime.Session, work *serverworkpool.Work) (*serveropen.Active, error) {
		protocolState := work.ProtocolState()
		idle := serverworkauth.Idle{
			TunnelID: selectedSession.TunnelID, ConnectorID: selectedSession.ConnectorID,
			SessionID: selectedSession.SessionID, WorkID: work.ID(), State: protocolState,
		}
		request := &protocolv1.OpenRequest{
			ProtocolVersion: 1, ConnectionId: connectionID, ServiceId: serviceID,
			ClientAddr: peer.RemoteAddr().String(), TimestampMs: uint64(time.Now().UnixMilli()),
			IngressType: ingress,
		}
		active, openErr := tunnelProxy.options.OpenHandler.Handle(ctx, work.Conn(), idle, request)
		if openErr != nil {
			return nil, openErr
		}
		if err := work.MarkActive(); err != nil {
			return nil, err
		}
		return active, nil
	}

	var (
		active         *serveropen.Active
		openErr        error
		crossConnector bool
	)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if contextErr := ctx.Err(); contextErr != nil {
				return errors.Join(openErr, contextErr)
			}
			var acquired bool
			selectedWork, acquired, err = pool.TryAcquire()
			if err != nil || !acquired {
				if contextErr := ctx.Err(); contextErr != nil {
					return errors.Join(openErr, err, contextErr)
				}
				if err != nil && !errors.Is(err, serverworkpool.ErrPoolClosed) &&
					!errors.Is(err, serverworkpool.ErrPoolDraining) {
					return errors.Join(openErr, err)
				}
				openErr = errors.Join(openErr, err)
				crossConnector = true
				break
			}
		}
		active, openErr = openSelectedWork(session, selectedWork)
		if openErr == nil {
			break
		}
		_ = selectedWork.Close()
		selectedWork = nil
		if isOpenDraining(openErr) {
			crossConnector = true
			break
		}
		// 只有 RAW 前 Transport 失败可在首 Connector 的同一 Pool 内重试一次。
		// Protocol、普通 OPEN_ERROR、Context Cancel、RawCommitted 和本地提交失败均直接结束。
		if contextErr := ctx.Err(); contextErr != nil {
			return errors.Join(openErr, contextErr)
		}
		if !errors.Is(openErr, serveropen.ErrPreRAWTransport) {
			return openErr
		}
		if attempt == 1 {
			crossConnector = true
		}
	}
	if crossConnector {
		if contextErr := ctx.Err(); contextErr != nil {
			return errors.Join(openErr, contextErr)
		}
		failedConnectorID := session.ConnectorID
		if !connectorLease.Release() {
			return errors.Join(openErr, errors.New("release failed Connector lease before cross-Connector reselect"))
		}
		connectorLease = nil
		connectorLease, session, pool, selectedWork, err = tunnelProxy.tryAcquireAlternateWork(
			ctx, tunnelID, serviceID, failedConnectorID,
		)
		if err != nil {
			return errors.Join(openErr, err)
		}
		if contextErr := ctx.Err(); contextErr != nil {
			_ = selectedWork.Close()
			selectedWork = nil
			return errors.Join(openErr, contextErr)
		}
		active, err = openSelectedWork(session, selectedWork)
		if err != nil {
			_ = selectedWork.Close()
			selectedWork = nil
			return err
		}
	}
	if active == nil || selectedWork == nil {
		return errors.New("OPEN completed without an ACTIVE WorkConn")
	}
	// OPEN_OK 已把 Work 协议切入 RAW，但在全局/Tunnel/Service/Source 配额提交
	// 之前仍不能发布成业务 ACTIVE；超限时直接关闭该 Work，不做跨 Connector 重选。
	if openLease != nil {
		if err := openLease.Activate(); err != nil {
			_ = selectedWork.Close()
			return err
		}
	}
	workContext, cancelWork := context.WithCancel(ctx)
	activeWork, err := tunnelRuntime.RegisterActiveWork(serverruntime.ActiveWorkSpec{
		Session: session, WorkID: active.Identity.WorkID, ConnectionID: connectionID,
		Cancel: cancelWork, WorkConn: active.Connection, PeerConn: peer, Lease: connectorLease,
	})
	if err != nil {
		cancelWork()
		_ = selectedWork.Close()
		return err
	}
	leaseOwnedByActive = true

	proxyErr := proxy.ProxyBidirectional(workContext, peer, active.Connection)
	finishErr := activeWork.Finish()
	workCloseErr := selectedWork.Close()
	return errors.Join(proxyErr, finishErr, workCloseErr)
}

func (tunnelProxy *Proxy) tryAcquireAlternateWork(
	ctx context.Context,
	tunnelID, serviceID, excludedConnectorID string,
) (*serverruntime.ConnectorLease, serverruntime.Session, *serverworkpool.Pool, *serverworkpool.Work, error) {
	attempted := map[string]struct{}{excludedConnectorID: {}}
	var attemptErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, serverruntime.Session{}, nil, nil, errors.Join(attemptErr, err)
		}
		pools := tunnelProxy.options.Sessions.Pools()
		connectorLease, err := tunnelProxy.options.Registry.AcquireEligibleConnectorWhere(tunnelID, serviceID, func(session serverruntime.Session) bool {
			if _, exists := attempted[session.ConnectorID]; exists {
				return false
			}
			pool, exists := pools[session]
			if !exists {
				return false
			}
			counts := pool.Snapshot()
			return !counts.Closed && !counts.Draining && counts.Idle > 0
		})
		if err != nil {
			return nil, serverruntime.Session{}, nil, nil, errors.Join(attemptErr, err)
		}
		session := connectorLease.Session()
		attempted[session.ConnectorID] = struct{}{}
		if tunnelProxy.afterAlternateAcquire != nil {
			tunnelProxy.afterAlternateAcquire(session)
		}
		if err := ctx.Err(); err != nil {
			connectorLease.Release()
			return nil, serverruntime.Session{}, nil, nil, errors.Join(attemptErr, err)
		}
		pool, exists := pools[session]
		if !exists {
			connectorLease.Release()
			attemptErr = errors.Join(attemptErr, ErrSessionPoolUnavailable)
			continue
		}
		work, acquired, err := pool.TryAcquire()
		if err != nil {
			connectorLease.Release()
			if errors.Is(err, serverworkpool.ErrPoolClosed) || errors.Is(err, serverworkpool.ErrPoolDraining) {
				attemptErr = errors.Join(attemptErr, err, ErrSessionPoolUnavailable)
				continue
			}
			return nil, serverruntime.Session{}, nil, nil, errors.Join(attemptErr, err)
		}
		if !acquired {
			connectorLease.Release()
			attemptErr = errors.Join(attemptErr, ErrSessionPoolUnavailable)
			continue
		}
		if err := ctx.Err(); err != nil {
			_ = work.Close()
			connectorLease.Release()
			return nil, serverruntime.Session{}, nil, nil, errors.Join(attemptErr, err)
		}
		if !tunnelProxy.options.Registry.Eligible(session, serviceID) {
			_ = work.Close()
			connectorLease.Release()
			attemptErr = errors.Join(attemptErr, ErrSessionPoolUnavailable)
			continue
		}
		return connectorLease, session, pool, work, nil
	}
}

func isOpenDraining(err error) bool {
	var rejected *serveropen.Rejected
	return errors.As(err, &rejected) && rejected.Code == protocolv1.ErrorCode_ERROR_CODE_OPEN_DRAINING
}

func (tunnelProxy *Proxy) acquireWork(
	ctx context.Context,
	tunnelID, serviceID string,
) (*serverruntime.ConnectorLease, serverruntime.Session, *serverworkpool.Pool, *serverworkpool.Work, error) {
	waitContext, cancelWait := context.WithTimeout(ctx, tunnelProxy.options.AcquireTimeout)
	defer cancelWait()
	for {
		if err := waitContext.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				err = fmt.Errorf("%w: %w", serverworkpool.ErrAcquireTimeout, err)
			}
			return nil, serverruntime.Session{}, nil, nil, err
		}
		connectorLease, session, pool, membership, err := tunnelProxy.selectConnector(waitContext, tunnelID, serviceID)
		if err != nil {
			if errors.Is(err, ErrSessionPoolUnavailable) {
				continue
			}
			return nil, serverruntime.Session{}, nil, nil, err
		}
		var (
			work       *serverworkpool.Work
			acquireErr error
		)
		if membership == nil {
			// 选择时看到的 IDLE 可能刚被并发请求抢走。这里只做非阻塞提交；
			// 失败立即回到全 Tunnel 选择，不能在一个已空 Pool 上消耗总等待时间。
			var acquired bool
			work, acquired, acquireErr = pool.TryAcquire()
			if acquireErr == nil && !acquired {
				connectorLease.Release()
				continue
			}
		} else {
			work, acquireErr = tunnelProxy.acquirePendingWork(waitContext, session, serviceID, pool)
		}
		var releaseErr error
		if membership != nil {
			releaseErr = membership.Release()
		}
		if acquireErr == nil && releaseErr == nil {
			if !tunnelProxy.options.Registry.Eligible(session, serviceID) {
				_ = work.Close()
				connectorLease.Release()
				continue
			}
			return connectorLease, session, pool, work, nil
		}
		connectorLease.Release()
		if work != nil {
			_ = work.Close()
		}
		if acquireErr == nil {
			return nil, serverruntime.Session{}, nil, nil, releaseErr
		}
		if errors.Is(acquireErr, serverworkpool.ErrPoolClosed) ||
			errors.Is(acquireErr, serverworkpool.ErrPoolDraining) ||
			errors.Is(acquireErr, ErrSessionPoolUnavailable) {
			continue
		}
		if errors.Is(acquireErr, context.DeadlineExceeded) && ctx.Err() == nil {
			acquireErr = fmt.Errorf("%w: %w", serverworkpool.ErrAcquireTimeout, acquireErr)
		}
		return nil, serverruntime.Session{}, nil, nil, errors.Join(acquireErr, releaseErr)
	}
}

type pendingAcquireResult struct {
	work *serverworkpool.Work
	err  error
}

// acquirePendingWork 让 Pool IDLE 与 Runtime Eligibility 共享同一个等待窗口。
// Pool.Acquire 的受控 goroutine 在本方法返回前必然结束；Eligibility 失效时先取消
// 阻塞，再由上层 exactly-once 释放 membership/Connector Lease 并重新选择。
func (tunnelProxy *Proxy) acquirePendingWork(
	waitContext context.Context,
	session serverruntime.Session,
	serviceID string,
	pool *serverworkpool.Pool,
) (*serverworkpool.Work, error) {
	acquireContext, cancelAcquire := context.WithCancel(waitContext)
	defer cancelAcquire()
	result := make(chan pendingAcquireResult, 1)
	safego.Go(func(err error) {
		result <- pendingAcquireResult{err: err}
	}, nil, func() {
		work, err := pool.Acquire(acquireContext, tunnelProxy.options.AcquireTimeout)
		result <- pendingAcquireResult{work: work, err: err}
	})

	for {
		watch, eligible := tunnelProxy.options.Registry.WatchEligibility(session, serviceID)
		if !eligible {
			cancelAcquire()
			acquired := <-result
			if acquired.work != nil {
				_ = acquired.work.Close()
			}
			return nil, ErrSessionPoolUnavailable
		}

		var (
			expiryTimer *time.Timer
			expired     <-chan time.Time
		)
		if watch.HasExpiry {
			expiryTimer = time.NewTimer(watch.ExpiresAfter)
			expired = expiryTimer.C
		}
		select {
		case acquired := <-result:
			stopPendingTimer(expiryTimer)
			return acquired.work, acquired.err
		case <-watch.Changed:
			stopPendingTimer(expiryTimer)
		case <-expired:
		}
	}
}

func stopPendingTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (tunnelProxy *Proxy) selectConnector(
	ctx context.Context,
	tunnelID, serviceID string,
) (*serverruntime.ConnectorLease, serverruntime.Session, *serverworkpool.Pool, *pendingMembership, error) {
	pools := tunnelProxy.options.Sessions.Pools()
	connectorLease, err := tunnelProxy.options.Registry.AcquireEligibleConnectorWhere(tunnelID, serviceID, func(session serverruntime.Session) bool {
		pool, exists := pools[session]
		if !exists {
			return false
		}
		counts := pool.Snapshot()
		return !counts.Closed && !counts.Draining && counts.Idle > 0
	})
	if err == nil {
		session := connectorLease.Session()
		pool, exists := pools[session]
		if !exists {
			connectorLease.Release()
			return nil, serverruntime.Session{}, nil, nil, ErrSessionPoolUnavailable
		}
		return connectorLease, session, pool, nil, nil
	}
	if !errors.Is(err, serverruntime.ErrNoAvailableConnector) {
		return nil, serverruntime.Session{}, nil, nil, err
	}

	membership, err := tunnelProxy.joinPendingGroup(ctx, tunnelID, serviceID)
	if err != nil {
		return nil, serverruntime.Session{}, nil, nil, err
	}
	group := membership.group
	connectorLease, err = tunnelProxy.options.Registry.AcquireEligibleConnectorWhere(tunnelID, serviceID, func(session serverruntime.Session) bool {
		return session == group.session
	})
	if err != nil {
		releaseErr := membership.Release()
		if errors.Is(err, serverruntime.ErrNoAvailableConnector) {
			// join 与 Lease 提交之间发生 generation replacement；旧 membership 已
			// exactly-once 释放，交给 acquireWork 在剩余 Deadline 内重新全局选择。
			err = fmt.Errorf("%w: stale pending Session: %w", ErrSessionPoolUnavailable, err)
		}
		return nil, serverruntime.Session{}, nil, nil, errors.Join(err, releaseErr)
	}
	return connectorLease, group.session, group.pool, membership, nil
}

func (tunnelProxy *Proxy) joinPendingGroup(
	ctx context.Context,
	tunnelID, serviceID string,
) (*pendingMembership, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tunnelProxy.pendingMu.Lock()
		group := tunnelProxy.pendingGroups[tunnelID]
		if group != nil && group.waiters == 0 {
			delete(tunnelProxy.pendingGroups, tunnelID)
			close(group.done)
			group = nil
		}
		if group != nil {
			pools := tunnelProxy.options.Sessions.Pools()
			pool, exists := pools[group.session]
			counts := group.pool.Snapshot()
			if !exists || pool != group.pool || counts.Closed || counts.Draining ||
				!tunnelProxy.options.Registry.Eligible(group.session, serviceID) {
				// 冻结契约只允许每个 Tunnel 存在一个投机 Demand。当前组不再适合
				// 新 Service 时，等待已有 membership exactly-once 离场后再重选，
				// 不能并行创建第二个 Service 级组。
				done := group.done
				tunnelProxy.pendingMu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-done:
					continue
				}
			}
		}
		if group == nil {
			pools := tunnelProxy.options.Sessions.Pools()
			selector, err := tunnelProxy.options.Registry.AcquireEligibleConnectorWhere(tunnelID, serviceID, func(session serverruntime.Session) bool {
				pool, exists := pools[session]
				if !exists {
					return false
				}
				counts := pool.Snapshot()
				return !counts.Closed && !counts.Draining
			})
			if err != nil {
				tunnelProxy.pendingMu.Unlock()
				return nil, err
			}
			session := selector.Session()
			selector.Release()
			pool, exists := pools[session]
			if !exists {
				tunnelProxy.pendingMu.Unlock()
				return nil, ErrSessionPoolUnavailable
			}
			group = &pendingGroup{session: session, pool: pool, done: make(chan struct{})}
			tunnelProxy.pendingGroups[tunnelID] = group
		}
		group.waiters++
		tunnelProxy.pendingBySession[group.session]++
		if err := tunnelProxy.options.Sessions.SetPendingOpens(group.session, tunnelProxy.pendingBySession[group.session]); err != nil {
			group.waiters--
			tunnelProxy.pendingBySession[group.session]--
			if tunnelProxy.pendingBySession[group.session] == 0 {
				delete(tunnelProxy.pendingBySession, group.session)
			}
			if group.waiters == 0 {
				delete(tunnelProxy.pendingGroups, tunnelID)
				close(group.done)
			}
			tunnelProxy.pendingMu.Unlock()
			return nil, err
		}
		tunnelProxy.pendingMu.Unlock()
		return &pendingMembership{proxy: tunnelProxy, tunnelID: tunnelID, group: group}, nil
	}
}

func (membership *pendingMembership) Release() error {
	if membership == nil || membership.proxy == nil || membership.group == nil {
		return nil
	}
	membership.once.Do(func() {
		proxy := membership.proxy
		proxy.pendingMu.Lock()
		defer proxy.pendingMu.Unlock()
		if membership.group.waiters == 0 {
			panic("tunnel pending OPEN group counter invariant violated")
		}
		membership.group.waiters--
		pending := proxy.pendingBySession[membership.group.session]
		if pending == 0 {
			panic("tunnel pending OPEN session counter invariant violated")
		}
		pending--
		if pending == 0 {
			delete(proxy.pendingBySession, membership.group.session)
		} else {
			proxy.pendingBySession[membership.group.session] = pending
		}
		membership.err = proxy.options.Sessions.SetPendingOpens(membership.group.session, pending)
		if proxy.pendingGroups[membership.tunnelID] == membership.group && membership.group.waiters == 0 {
			delete(proxy.pendingGroups, membership.tunnelID)
			close(membership.group.done)
		}
	})
	return membership.err
}

func sourceAddress(address net.Addr) (netip.Addr, error) {
	if address == nil {
		return netip.Addr{}, serverlimits.ErrInvalidConnectionKey
	}
	if tcpAddress, ok := address.(*net.TCPAddr); ok {
		source, valid := netip.AddrFromSlice(tcpAddress.IP)
		if !valid {
			return netip.Addr{}, serverlimits.ErrInvalidConnectionKey
		}
		return source.Unmap().WithZone(""), nil
	}
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse public peer address: %w", err)
	}
	source, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse public peer source IP: %w", err)
	}
	return source.Unmap().WithZone(""), nil
}
