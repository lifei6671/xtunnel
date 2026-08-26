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

	pendingMu     sync.Mutex
	pendingGroups map[string]*pendingGroup
}

type pendingGroup struct {
	session serverruntime.Session
	pool    *serverworkpool.Pool
	waiters uint32
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
	return &Proxy{options: options, pendingGroups: make(map[string]*pendingGroup)}, nil
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

	connectorLease, session, pool, selectedWork, err := tunnelProxy.acquireWork(ctx, tunnelID)
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
	connectionID, err := identity.NewConnectionID()
	if err != nil {
		return err
	}

	var active *serveropen.Active
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			selectedWork, err = pool.Acquire(ctx, tunnelProxy.options.AcquireTimeout)
			if err != nil {
				return err
			}
		}
		protocolState := selectedWork.ProtocolState()
		idle := serverworkauth.Idle{
			TunnelID: session.TunnelID, ConnectorID: session.ConnectorID,
			SessionID: session.SessionID, WorkID: selectedWork.ID(), State: protocolState,
		}
		request := &protocolv1.OpenRequest{
			ProtocolVersion: 1, ConnectionId: connectionID, ServiceId: serviceID,
			ClientAddr: peer.RemoteAddr().String(), TimestampMs: uint64(time.Now().UnixMilli()),
			IngressType: ingress,
		}
		active, err = tunnelProxy.options.OpenHandler.Handle(ctx, selectedWork.Conn(), idle, request)
		if err == nil {
			if err := selectedWork.MarkActive(); err != nil {
				_ = selectedWork.Close()
				return err
			}
			break
		}
		_ = selectedWork.Close()
		selectedWork = nil
		// 明确 OPEN_ERROR 代表 Origin/Service 结果，协议错误也不可安全重放；只有
		// RAW 前传输失败允许在同一 Connector 的另一个 WorkConn 上重试一次。
		if attempt != 0 || errors.Is(err, serveropen.ErrRejected) || errors.Is(err, serveropen.ErrProtocol) || ctx.Err() != nil {
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

func (tunnelProxy *Proxy) acquireWork(
	ctx context.Context,
	tunnelID string,
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
		connectorLease, session, pool, membership, err := tunnelProxy.selectConnector(tunnelID)
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
			work, acquireErr = pool.Acquire(waitContext, tunnelProxy.options.AcquireTimeout)
		}
		var releaseErr error
		if membership != nil {
			releaseErr = membership.Release()
		}
		if acquireErr == nil && releaseErr == nil {
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
			errors.Is(acquireErr, serverworkpool.ErrPoolDraining) {
			continue
		}
		if errors.Is(acquireErr, context.DeadlineExceeded) && ctx.Err() == nil {
			acquireErr = fmt.Errorf("%w: %w", serverworkpool.ErrAcquireTimeout, acquireErr)
		}
		return nil, serverruntime.Session{}, nil, nil, errors.Join(acquireErr, releaseErr)
	}
}

func (tunnelProxy *Proxy) selectConnector(
	tunnelID string,
) (*serverruntime.ConnectorLease, serverruntime.Session, *serverworkpool.Pool, *pendingMembership, error) {
	connectorLease, err := tunnelProxy.options.Registry.AcquireConnectorWhere(tunnelID, func(session serverruntime.Session) bool {
		pool, exists := tunnelProxy.options.Sessions.Pool(session)
		if !exists {
			return false
		}
		counts := pool.Snapshot()
		return !counts.Closed && !counts.Draining && counts.Idle > 0
	})
	if err == nil {
		session := connectorLease.Session()
		pool, exists := tunnelProxy.options.Sessions.Pool(session)
		if !exists {
			connectorLease.Release()
			return nil, serverruntime.Session{}, nil, nil, ErrSessionPoolUnavailable
		}
		return connectorLease, session, pool, nil, nil
	}
	if !errors.Is(err, serverruntime.ErrNoAvailableConnector) {
		return nil, serverruntime.Session{}, nil, nil, err
	}

	membership, err := tunnelProxy.joinPendingGroup(tunnelID)
	if err != nil {
		return nil, serverruntime.Session{}, nil, nil, err
	}
	group := membership.group
	connectorLease, err = tunnelProxy.options.Registry.AcquireConnectorWhere(tunnelID, func(session serverruntime.Session) bool {
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

func (tunnelProxy *Proxy) joinPendingGroup(tunnelID string) (*pendingMembership, error) {
	tunnelProxy.pendingMu.Lock()
	defer tunnelProxy.pendingMu.Unlock()
	group := tunnelProxy.pendingGroups[tunnelID]
	if group != nil {
		pool, exists := tunnelProxy.options.Sessions.Pool(group.session)
		if !exists || pool != group.pool {
			delete(tunnelProxy.pendingGroups, tunnelID)
			group = nil
		} else {
			counts := pool.Snapshot()
			if counts.Closed || counts.Draining {
				delete(tunnelProxy.pendingGroups, tunnelID)
				group = nil
			}
		}
	}
	if group == nil {
		selector, err := tunnelProxy.options.Registry.AcquireConnectorWhere(tunnelID, func(session serverruntime.Session) bool {
			pool, exists := tunnelProxy.options.Sessions.Pool(session)
			if !exists {
				return false
			}
			counts := pool.Snapshot()
			return !counts.Closed && !counts.Draining
		})
		if err != nil {
			return nil, err
		}
		session := selector.Session()
		selector.Release()
		pool, exists := tunnelProxy.options.Sessions.Pool(session)
		if !exists {
			return nil, ErrSessionPoolUnavailable
		}
		group = &pendingGroup{session: session, pool: pool}
		tunnelProxy.pendingGroups[tunnelID] = group
	}
	group.waiters++
	if err := tunnelProxy.options.Sessions.SetPendingOpens(group.session, group.waiters); err != nil {
		group.waiters--
		if group.waiters == 0 {
			delete(tunnelProxy.pendingGroups, tunnelID)
		}
		return nil, err
	}
	return &pendingMembership{proxy: tunnelProxy, tunnelID: tunnelID, group: group}, nil
}

func (membership *pendingMembership) Release() error {
	if membership == nil || membership.proxy == nil || membership.group == nil {
		return nil
	}
	membership.once.Do(func() {
		proxy := membership.proxy
		proxy.pendingMu.Lock()
		defer proxy.pendingMu.Unlock()
		if proxy.pendingGroups[membership.tunnelID] != membership.group {
			return
		}
		if membership.group.waiters == 0 {
			panic("tunnel pending OPEN group counter invariant violated")
		}
		membership.group.waiters--
		membership.err = proxy.options.Sessions.SetPendingOpens(membership.group.session, membership.group.waiters)
		if membership.group.waiters == 0 {
			delete(proxy.pendingGroups, membership.tunnelID)
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
