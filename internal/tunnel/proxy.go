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

// serviceRevisionConstraint 贯穿一次 OPEN 的初选、Pending、提交复核与重选。
// exact=false 仅用于没有 Route Revision 输入的 TCP/Raw Serve；HTTP Dial 始终
// exact=true，包括合法的 Revision 0。
type serviceRevisionConstraint struct {
	exact    bool
	revision uint64
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
	if peer == nil || !validDialInput(tunnelProxy, ctx, tunnelID, serviceID, ingress, "public-peer") {
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
	connection, err := tunnelProxy.openConnection(
		ctx, tunnelID, serviceID, serviceRevisionConstraint{},
		ingress, peer.RemoteAddr().String(), peer.RemoteAddr(), peer,
	)
	if err != nil {
		return err
	}
	proxyErr := proxy.ProxyBidirectional(connection.lifecycleContext, peer, connection)
	return errors.Join(proxyErr, connection.Close())
}

// Dial 在 ctx 的等待窗口内取得 IDLE WorkConn 并完成 OPEN，返回可交给
// http.Transport 连接池复用的数据连接。
//
// ctx 只约束 acquire/OPEN。成功发布 ACTIVE 后，连接不再绑定单个 HTTP Request；
// 它只由返回连接 Close、Tunnel Revoke、Registry Drain 或进程关闭收敛。clientAddr
// 原样进入 OpenRequest；启用公网限制时必须是可解析的 IP:port，作为 Source 维度键。
// requiredRevision 在初选、等待与跨 Connector 重选中始终精确匹配；Revision 0
// 也是合法 Route Revision，不会退化为“接受当前版本”。
func (tunnelProxy *Proxy) Dial(
	ctx context.Context,
	tunnelID, serviceID string,
	requiredRevision uint64,
	ingress protocolv1.IngressType,
	clientAddr string,
) (net.Conn, error) {
	if !validDialInput(tunnelProxy, ctx, tunnelID, serviceID, ingress, clientAddr) {
		return nil, ErrInvalidOptions
	}
	connection, err := tunnelProxy.openConnection(
		ctx, tunnelID, serviceID,
		serviceRevisionConstraint{exact: true, revision: requiredRevision},
		ingress, clientAddr, nil, nil,
	)
	if err != nil {
		return nil, err
	}
	return connection, nil
}

// ServiceConfigObserved 只暴露 HTTP 错误映射需要的配置可见性，不把 Health、容量或
// Connector 内部状态泄漏给 Ingress。普通不可用与未观察到新 Revision 必须使用
// 不同稳定错误码，但两者都不能绕过 Dial 内的完整 Eligible 门禁。
func (tunnelProxy *Proxy) ServiceConfigObserved(tunnelID, serviceID string, requiredRevision int64) bool {
	if tunnelProxy == nil || tunnelProxy.options.Registry == nil || requiredRevision < 0 {
		return false
	}
	return tunnelProxy.options.Registry.ServiceConfigObserved(
		tunnelID, serviceID, uint64(requiredRevision),
	)
}

// openConnection 是 Serve 与 Dial 共用的唯一 OPEN 提交路径。函数返回前，Work、
// Pending/Active 限额和 Connector Lease 仍由本栈帧持有；只有 ActiveWork 注册成功
// 才把三者原子地交给 pooledConnection 与 Runtime。任一步失败都会逆序关闭 Work 并
// 释放 Lease，禁止把 OPENING/ACTIVE 资源遗留在 Pool。
func (tunnelProxy *Proxy) openConnection(
	ctx context.Context,
	tunnelID, serviceID string,
	revision serviceRevisionConstraint,
	ingress protocolv1.IngressType,
	clientAddr string,
	clientNetworkAddr net.Addr,
	peer net.Conn,
) (_ *managedConnection, resultErr error) {
	var openLease *serverlimits.OpenLease
	openLeaseOwnedByConnection := false
	defer func() {
		if openLease != nil && !openLeaseOwnedByConnection {
			openLease.Release()
		}
	}()
	if tunnelProxy.options.LimitManager != nil {
		var (
			sourceIP netip.Addr
			err      error
		)
		if clientNetworkAddr != nil {
			sourceIP, err = sourceAddress(clientNetworkAddr)
		} else {
			sourceIP, err = sourceAddressString(clientAddr)
		}
		if err != nil {
			return nil, err
		}
		openLease, err = tunnelProxy.options.LimitManager.AcquirePendingOpen(serverlimits.ConnectionKey{
			TunnelID: tunnelID, ServiceID: serviceID, SourceIP: sourceIP,
		})
		if err != nil {
			return nil, err
		}
	}

	// connection_id 在任何 Connector/Work 所有权取得前生成。若系统随机源
	// 失败，直接返回，不会把 IDLE Work 留在 OPENING。
	connectionID, err := tunnelProxy.newConnectionID()
	if err != nil {
		return nil, err
	}

	connectorLease, session, pool, selectedWork, err := tunnelProxy.acquireWork(
		ctx, tunnelID, serviceID, revision,
	)
	if err != nil {
		return nil, err
	}
	leaseOwnedByActive := false
	defer func() {
		if connectorLease != nil && !leaseOwnedByActive {
			connectorLease.Release()
		}
	}()
	workOwnedByConnection := false
	defer func() {
		if selectedWork != nil && !workOwnedByConnection {
			resultErr = errors.Join(resultErr, selectedWork.Close())
		}
	}()
	tunnelRuntime, err := tunnelProxy.options.Registry.Tunnel(tunnelID)
	if err != nil {
		return nil, err
	}
	openSelectedWork := func(selectedSession serverruntime.Session, work *serverworkpool.Work) (*serveropen.Active, error) {
		protocolState := work.ProtocolState()
		idle := serverworkauth.Idle{
			TunnelID: selectedSession.TunnelID, ConnectorID: selectedSession.ConnectorID,
			SessionID: selectedSession.SessionID, WorkID: work.ID(), State: protocolState,
		}
		request := &protocolv1.OpenRequest{
			ProtocolVersion: 1, ConnectionId: connectionID, ServiceId: serviceID,
			ClientAddr: clientAddr, TimestampMs: uint64(time.Now().UnixMilli()),
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
				return nil, errors.Join(openErr, contextErr)
			}
			var acquired bool
			selectedWork, acquired, err = pool.TryAcquire()
			if err != nil || !acquired {
				if contextErr := ctx.Err(); contextErr != nil {
					return nil, errors.Join(openErr, err, contextErr)
				}
				if err != nil && !errors.Is(err, serverworkpool.ErrPoolClosed) &&
					!errors.Is(err, serverworkpool.ErrPoolDraining) {
					return nil, errors.Join(openErr, err)
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
			return nil, errors.Join(openErr, contextErr)
		}
		if !errors.Is(openErr, serveropen.ErrPreRAWTransport) {
			return nil, openErr
		}
		if attempt == 1 {
			crossConnector = true
		}
	}
	if crossConnector {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, errors.Join(openErr, contextErr)
		}
		failedConnectorID := session.ConnectorID
		if !connectorLease.Release() {
			return nil, errors.Join(openErr, errors.New("release failed Connector lease before cross-Connector reselect"))
		}
		connectorLease = nil
		connectorLease, session, pool, selectedWork, err = tunnelProxy.tryAcquireAlternateWork(
			ctx, tunnelID, serviceID, revision, failedConnectorID,
		)
		if err != nil {
			return nil, errors.Join(openErr, err)
		}
		if contextErr := ctx.Err(); contextErr != nil {
			_ = selectedWork.Close()
			selectedWork = nil
			return nil, errors.Join(openErr, contextErr)
		}
		active, err = openSelectedWork(session, selectedWork)
		if err != nil {
			_ = selectedWork.Close()
			selectedWork = nil
			return nil, err
		}
	}
	if active == nil || selectedWork == nil {
		return nil, errors.New("OPEN completed without an ACTIVE WorkConn")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// OPEN_OK 已把 Work 协议切入 RAW，但在全局/Tunnel/Service/Source 配额提交
	// 之前仍不能发布成业务 ACTIVE；超限时直接关闭该 Work，不做跨 Connector 重选。
	if openLease != nil {
		if err := openLease.Activate(); err != nil {
			return nil, err
		}
	}
	// OPEN 已受调用 ctx 约束。Serve 继续继承公网连接 ctx；Dial 没有独立 Public
	// Peer，发布后切换为独立生命周期，避免一次 HTTP Request 的取消误杀连接池。
	lifecycleParent := ctx
	if peer == nil {
		lifecycleParent = context.WithoutCancel(ctx)
	}
	lifecycleContext, cancelWork := context.WithCancel(lifecycleParent)
	pooled := &pooledConnection{Conn: active.Connection, work: selectedWork, openLease: openLease}
	activeWork, err := tunnelRuntime.RegisterActiveWork(serverruntime.ActiveWorkSpec{
		Session: session, WorkID: active.Identity.WorkID, ConnectionID: connectionID,
		Cancel: cancelWork, WorkConn: pooled, PeerConn: peer, Lease: connectorLease,
	})
	if err != nil {
		cancelWork()
		return nil, err
	}
	leaseOwnedByActive = true
	workOwnedByConnection = true
	openLeaseOwnedByConnection = true
	connection := &managedConnection{
		Conn: pooled, activeWork: activeWork, lifecycleContext: lifecycleContext,
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, connection.Close())
	}
	return connection, nil
}

// pooledConnection 是 WorkPool ACTIVE Work 与公网 Active 限额的共同 owner。
// ActiveWork 无论由自然 Close、Revoke 还是 Drain 终止，都会调用这里的 Close；
// sync.Once 保证 Work.Close 和 OpenLease.Release 只执行一次。
type pooledConnection struct {
	net.Conn
	work      *serverworkpool.Work
	openLease *serverlimits.OpenLease

	closeOnce sync.Once
	closeErr  error
}

func (connection *pooledConnection) Close() error {
	connection.closeOnce.Do(func() {
		connection.closeErr = connection.work.Close()
		if connection.openLease != nil {
			connection.openLease.Release()
		}
	})
	return connection.closeErr
}

func (connection *pooledConnection) CloseWrite() error {
	if closer, ok := connection.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return proxy.ErrHalfCloseUnsupported
}

func (connection *pooledConnection) CloseRead() error {
	if closer, ok := connection.Conn.(interface{ CloseRead() error }); ok {
		return closer.CloseRead()
	}
	return nil
}

// managedConnection 把调用方 Close 收敛到 ActiveWork 的线性化终止入口。
// Registry 先关闭连接时，后续 Transport Close 仍只会重复观察同一终态错误。
type managedConnection struct {
	net.Conn
	activeWork       *serverruntime.ActiveWork
	lifecycleContext context.Context
}

func (connection *managedConnection) Close() error {
	return connection.activeWork.Finish()
}

func (connection *managedConnection) CloseWrite() error {
	return connection.Conn.(interface{ CloseWrite() error }).CloseWrite()
}

func (connection *managedConnection) CloseRead() error {
	return connection.Conn.(interface{ CloseRead() error }).CloseRead()
}

func validDialInput(
	tunnelProxy *Proxy,
	ctx context.Context,
	tunnelID, serviceID string,
	ingress protocolv1.IngressType,
	clientAddr string,
) bool {
	return tunnelProxy != nil && ctx != nil && clientAddr != "" &&
		identity.ValidateTunnelID(tunnelID) == nil && validate.ValidateID(serviceID, "svc_") == nil &&
		ingress != protocolv1.IngressType_INGRESS_TYPE_UNSPECIFIED
}

// 以下三个入口集中翻译一次 OPEN 的 Revision 模式，保证初选、Pending Watch、
// Work 提交复核与跨 Connector 重选不会在中途丢失 exact 语义。
func (tunnelProxy *Proxy) connectorEligible(
	session serverruntime.Session,
	serviceID string,
	revision serviceRevisionConstraint,
) bool {
	if revision.exact {
		return tunnelProxy.options.Registry.EligibleAtRevision(session, serviceID, revision.revision)
	}
	return tunnelProxy.options.Registry.Eligible(session, serviceID)
}

func (tunnelProxy *Proxy) watchConnectorEligibility(
	session serverruntime.Session,
	serviceID string,
	revision serviceRevisionConstraint,
) (serverruntime.EligibilityWatch, bool) {
	if revision.exact {
		return tunnelProxy.options.Registry.WatchEligibilityAtRevision(session, serviceID, revision.revision)
	}
	return tunnelProxy.options.Registry.WatchEligibility(session, serviceID)
}

func (tunnelProxy *Proxy) acquireEligibleConnectorWhere(
	tunnelID, serviceID string,
	revision serviceRevisionConstraint,
	predicate func(serverruntime.Session) bool,
) (*serverruntime.ConnectorLease, error) {
	if revision.exact {
		return tunnelProxy.options.Registry.AcquireEligibleConnectorAtRevisionWhere(
			tunnelID, serviceID, revision.revision, predicate,
		)
	}
	return tunnelProxy.options.Registry.AcquireEligibleConnectorWhere(
		tunnelID, serviceID, predicate,
	)
}

func (tunnelProxy *Proxy) tryAcquireAlternateWork(
	ctx context.Context,
	tunnelID, serviceID string,
	revision serviceRevisionConstraint,
	excludedConnectorID string,
) (*serverruntime.ConnectorLease, serverruntime.Session, *serverworkpool.Pool, *serverworkpool.Work, error) {
	attempted := map[string]struct{}{excludedConnectorID: {}}
	var attemptErr error
	for {
		if err := ctx.Err(); err != nil {
			return nil, serverruntime.Session{}, nil, nil, errors.Join(attemptErr, err)
		}
		pools := tunnelProxy.options.Sessions.Pools()
		connectorLease, err := tunnelProxy.acquireEligibleConnectorWhere(tunnelID, serviceID, revision, func(session serverruntime.Session) bool {
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
		if !tunnelProxy.connectorEligible(session, serviceID, revision) {
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
	revision serviceRevisionConstraint,
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
		connectorLease, session, pool, membership, err := tunnelProxy.selectConnector(
			waitContext, tunnelID, serviceID, revision,
		)
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
			work, acquireErr = tunnelProxy.acquirePendingWork(
				waitContext, session, serviceID, revision, pool,
			)
		}
		var releaseErr error
		if membership != nil {
			releaseErr = membership.Release()
		}
		if acquireErr == nil && releaseErr == nil {
			if !tunnelProxy.connectorEligible(session, serviceID, revision) {
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
	revision serviceRevisionConstraint,
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
		watch, eligible := tunnelProxy.watchConnectorEligibility(session, serviceID, revision)
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
	revision serviceRevisionConstraint,
) (*serverruntime.ConnectorLease, serverruntime.Session, *serverworkpool.Pool, *pendingMembership, error) {
	pools := tunnelProxy.options.Sessions.Pools()
	connectorLease, err := tunnelProxy.acquireEligibleConnectorWhere(tunnelID, serviceID, revision, func(session serverruntime.Session) bool {
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

	membership, err := tunnelProxy.joinPendingGroup(ctx, tunnelID, serviceID, revision)
	if err != nil {
		return nil, serverruntime.Session{}, nil, nil, err
	}
	group := membership.group
	connectorLease, err = tunnelProxy.acquireEligibleConnectorWhere(tunnelID, serviceID, revision, func(session serverruntime.Session) bool {
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
	revision serviceRevisionConstraint,
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
				!tunnelProxy.connectorEligible(group.session, serviceID, revision) {
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
			selector, err := tunnelProxy.acquireEligibleConnectorWhere(tunnelID, serviceID, revision, func(session serverruntime.Session) bool {
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

func sourceAddressString(address string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse public client address: %w", err)
	}
	source, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse public client source IP: %w", err)
	}
	return source.Unmap().WithZone(""), nil
}
