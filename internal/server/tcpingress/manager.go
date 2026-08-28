// Package tcpingress 调和 TCP Route Desired State 与实际公网 Listener。
package tcpingress

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lifei6671/xtunnel/internal/safego"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
	"github.com/lifei6671/xtunnel/internal/tcpport"
)

const (
	// ListenFailedErrorCode 是 TCP Listener 无法达到 Desired State 的稳定错误码。
	ListenFailedErrorCode = "LISTEN_FAILED"
	defaultRetryInterval  = 5 * time.Second
)

var ErrInvalidOptions = errors.New("TCP ingress options are invalid")

// RouteSource 提供已经原子发布的完整 Route Snapshot。
type RouteSource interface {
	Current() *serverroute.Snapshot
}

// Handler 接收一次 Accept 时捕获的不可变 Route。Listener 更新不会改写在途连接。
type Handler func(context.Context, net.Conn, serverroute.TCPRoute)

// SourceLimiter 在公网 Socket 已经 Accept、但尚未登记为 ACTIVE 前执行来源级 OPEN 门禁。
// 失败的连接由 Manager 直接关闭，不进入 Handler，也不会占用连接 WaitGroup。
type SourceLimiter interface {
	AllowOpen(netip.Addr) error
}

// Options 是 TCP Listener Manager 的固定生产依赖。
type Options struct {
	Bind               netip.Addr
	MinPort            int
	MaxPort            int
	Reserved           []uint16
	Routes             RouteSource
	SourceLimiter      SourceLimiter
	Handler            Handler
	ReportRuntimeError func(error)

	listen        func(context.Context, string, string) (net.Listener, error)
	retryInterval time.Duration
	now           func() time.Time
}

// ApplyFailure 是当前 generation 下单条 Desired Route 的 Listener 失败事实。
type ApplyFailure struct {
	RouteID          string
	ServiceID        string
	PublicPort       uint16
	RequiredRevision int64
	Generation       uint64
	ErrorCode        string
	FailedAt         time.Time
}

// ActualListener 是已经原子发布并接受新连接的 Listener 值快照。
type ActualListener struct {
	Route   serverroute.TCPRoute
	Address string
}

type publishedState struct {
	generation uint64
	actual     []ActualListener
	failures   []ApplyFailure
}

type managedListener struct {
	listener net.Listener
	route    atomic.Pointer[serverroute.TCPRoute]
	// accepting 与 Manager.accepting 共用 admissionMu。Route 删除/换口发布前先
	// 清除此位，使旧 Listener 中晚到的 Accept 不能在新 Actual 发布后重新准入。
	accepting bool
}

// Manager 以唯一 reconcile goroutine 拥有 Actual Listener 集合。Listen、Close、
// Accept 和 Handler IO 都不在状态锁内执行；generation 在发布前再次 fencing。
type Manager struct {
	options Options
	policy  tcpport.Policy
	network string

	mu                sync.Mutex
	started           bool
	stopping          bool
	actual            map[uint16]*managedListener
	residual          map[*managedListener]struct{}
	failures          map[string]ApplyFailure
	generation        uint64
	dirty             chan struct{}
	dirtyGeneration   atomic.Uint64
	cancelAccept      context.CancelFunc
	cancelConnections context.CancelFunc
	ownerDone         chan struct{}
	published         atomic.Pointer[publishedState]

	admissionMu    sync.Mutex
	accepting      bool
	connections    map[net.Conn]struct{}
	acceptWait     sync.WaitGroup
	connectionWait sync.WaitGroup

	stopOnce      sync.Once
	closeOnce     sync.Once
	stopErr       error
	closeErr      error
	errorMu       sync.Mutex
	runtimeErr    error
	connectionErr error
}

// NewManager 校验全局 Listener 配置并构造尚未启动的 Manager。
func NewManager(options Options) (*Manager, error) {
	if !options.Bind.IsValid() || options.Routes == nil {
		return nil, ErrInvalidOptions
	}
	policy, err := tcpport.New(options.MinPort, options.MaxPort, options.Reserved)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	if options.listen == nil {
		listenConfig := new(net.ListenConfig)
		options.listen = listenConfig.Listen
	}
	if options.retryInterval <= 0 {
		options.retryInterval = defaultRetryInterval
	}
	if options.now == nil {
		options.now = time.Now
	}
	network := "tcp6"
	if options.Bind.Is4() {
		network = "tcp4"
	}
	manager := &Manager{
		options: options, policy: policy, network: network,
		actual: make(map[uint16]*managedListener), failures: make(map[string]ApplyFailure),
		residual: make(map[*managedListener]struct{}), dirty: make(chan struct{}, 1),
		connections: make(map[net.Conn]struct{}), ownerDone: make(chan struct{}),
	}
	manager.publishLocked()
	return manager, nil
}

// Start 启动唯一 reconcile owner，并等待首次 Desired/Actual 调和完成。单个端口绑定
// 失败只发布 ApplyFailure，不使 Start 失败；全局配置或 Route Snapshot 缺失才失败。
func (manager *Manager) Start(parent context.Context) error {
	if manager == nil || parent == nil {
		return ErrInvalidOptions
	}
	manager.mu.Lock()
	if manager.started || manager.stopping {
		manager.mu.Unlock()
		return errors.New("TCP ingress manager has already started or stopped")
	}
	acceptContext, cancelAccept := context.WithCancel(parent)
	connectionContext, cancelConnections := context.WithCancel(parent)
	manager.started = true
	manager.cancelAccept = cancelAccept
	manager.cancelConnections = cancelConnections
	manager.admissionMu.Lock()
	manager.accepting = true
	manager.admissionMu.Unlock()
	manager.mu.Unlock()

	ready := make(chan error, 1)
	safego.Go(
		func(err error) {
			manager.recordRuntimeError(fmt.Errorf("TCP listener reconcile owner: %w", err))
			ready <- err
		},
		func() { close(manager.ownerDone) },
		func() { manager.run(acceptContext, connectionContext, ready) },
	)
	select {
	case err := <-ready:
		return err
	case <-parent.Done():
		return fmt.Errorf("start TCP ingress manager: %w", parent.Err())
	}
}

func (manager *Manager) run(acceptContext, connectionContext context.Context, ready chan<- error) {
	if err := manager.reconcile(acceptContext, connectionContext); err != nil {
		ready <- err
		return
	}
	ready <- nil
	ticker := time.NewTicker(manager.options.retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-acceptContext.Done():
			return
		case <-manager.dirty:
			if err := manager.reconcile(acceptContext, connectionContext); err != nil {
				manager.recordRuntimeError(fmt.Errorf("reconcile dirty TCP listeners: %w", err))
			}
		case <-ticker.C:
			if err := manager.reconcile(acceptContext, connectionContext); err != nil {
				manager.recordRuntimeError(fmt.Errorf("reconcile TCP listeners: %w", err))
			}
		}
	}
}

// MarkDirty 合并 Route 写入后的即时唤醒。generation 只用于保存调用方已经提交的
// 最大代次；真正发布仍以 RouteSource 当前完整 Snapshot 为准，周期重试负责覆盖
// Route Snapshot 尚在构建以及外部端口冲突稍后解除的情况。
func (manager *Manager) MarkDirty(generation uint64) {
	if manager == nil {
		return
	}
	for {
		observed := manager.dirtyGeneration.Load()
		if generation <= observed || manager.dirtyGeneration.CompareAndSwap(observed, generation) {
			break
		}
	}
	select {
	case manager.dirty <- struct{}{}:
	default:
	}
}

// reconcile 先在锁外建立本轮所需的新 Socket，最后复核 generation 后一次发布。
// A→B 迁移只有在 B 成功发布后才停止 A；显式删除/禁用则不等待替代入口。
func (manager *Manager) reconcile(acceptContext, connectionContext context.Context) error {
	for {
		snapshot := manager.options.Routes.Current()
		if snapshot == nil {
			return errors.New("TCP ingress route snapshot is unavailable")
		}
		generation := snapshot.Generation()
		desiredRoutes := snapshot.TCPRoutes()
		desiredByPort := make(map[uint16]serverroute.TCPRoute, len(desiredRoutes))
		desiredByID := make(map[string]serverroute.TCPRoute, len(desiredRoutes))
		for _, desired := range desiredRoutes {
			desiredByPort[desired.PublicPort] = desired
			desiredByID[desired.ID] = desired
		}

		manager.mu.Lock()
		if manager.stopping {
			manager.mu.Unlock()
			return nil
		}
		actual := make(map[uint16]*managedListener, len(manager.actual))
		maps.Copy(actual, manager.actual)
		manager.mu.Unlock()

		candidates := make(map[uint16]*managedListener)
		nextFailures := make(map[string]ApplyFailure)
		stale := false
		for _, desired := range desiredRoutes {
			if existing := actual[desired.PublicPort]; existing != nil {
				continue
			}
			if err := manager.policy.ValidateExplicit(desired.PublicPort, nil); err != nil {
				nextFailures[desired.ID] = manager.failure(generation, desired)
				continue
			}
			address := net.JoinHostPort(manager.options.Bind.String(), strconv.Itoa(int(desired.PublicPort)))
			listener, err := manager.options.listen(acceptContext, manager.network, address)
			if err != nil {
				if acceptContext.Err() != nil {
					if closeErr := manager.closeOwnedListeners(candidates); closeErr != nil {
						return fmt.Errorf("close canceled TCP listener candidates: %w", closeErr)
					}
					return nil
				}
				nextFailures[desired.ID] = manager.failure(generation, desired)
				continue
			}
			managed := &managedListener{listener: listener, accepting: true}
			routeCopy := desired
			managed.route.Store(&routeCopy)
			candidates[desired.PublicPort] = managed
			if current := manager.options.Routes.Current(); current == nil || current.Generation() != generation {
				stale = true
				break
			}
		}
		if !stale {
			current := manager.options.Routes.Current()
			stale = current == nil || current.Generation() != generation
		}
		if stale {
			if err := manager.closeOwnedListeners(candidates); err != nil {
				return fmt.Errorf("close stale TCP listener candidate: %w", err)
			}
			continue
		}

		manager.mu.Lock()
		currentSnapshot := manager.options.Routes.Current()
		if manager.stopping || currentSnapshot == nil || currentSnapshot.Generation() != generation {
			manager.mu.Unlock()
			closeErr := manager.closeOwnedListeners(candidates)
			if manager.stopping {
				return closeErr
			}
			if closeErr != nil {
				return fmt.Errorf("close fenced TCP listener candidate: %w", closeErr)
			}
			continue
		}
		nextActual := make(map[uint16]*managedListener, len(manager.actual)+len(candidates))
		for port, listener := range manager.actual {
			nextActual[port] = listener
		}
		manager.admissionMu.Lock()
		for port, desired := range desiredByPort {
			if listener := nextActual[port]; listener != nil {
				routeCopy := desired
				listener.route.Store(&routeCopy)
			}
		}
		for port, listener := range candidates {
			nextActual[port] = listener
			manager.acceptWait.Add(1)
		}
		removed := make(map[uint16]*managedListener)
		for port, listener := range nextActual {
			if _, stillDesiredAtPort := desiredByPort[port]; stillDesiredAtPort {
				continue
			}
			oldRoute := listener.route.Load()
			if oldRoute != nil {
				if replacementRoute, moving := desiredByID[oldRoute.ID]; moving {
					replacement := nextActual[replacementRoute.PublicPort]
					if replacement == nil || replacement.route.Load().ID != oldRoute.ID {
						// B 尚未绑定成功时继续保留 A，但同步 Service/Tunnel/Revision
						// 等非端口字段，使旧入口仍能通过当前 Connector revision 门禁。
						fallbackRoute := replacementRoute
						fallbackRoute.PublicPort = port
						listener.route.Store(&fallbackRoute)
						continue
					}
				}
			}
			removed[port] = listener
			delete(nextActual, port)
		}
		for _, listener := range removed {
			listener.accepting = false
		}
		manager.actual = nextActual
		manager.failures = nextFailures
		manager.generation = generation
		manager.publishLocked()
		manager.admissionMu.Unlock()
		manager.mu.Unlock()

		for _, listener := range candidates {
			listener := listener
			safego.Go(
				func(err error) { manager.recordRuntimeError(fmt.Errorf("TCP listener accept owner: %w", err)) },
				manager.acceptWait.Done,
				func() { manager.accept(connectionContext, listener) },
			)
		}
		if err := manager.closeOwnedListeners(removed); err != nil {
			return fmt.Errorf("close retired TCP listener: %w", err)
		}
		return nil
	}
}

func (manager *Manager) accept(ctx context.Context, listener *managedListener) {
	for {
		connection, err := listener.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			manager.retireAcceptFailure(listener)
			return
		}
		// OS Accept 已经产生一次真实公网 OPEN 尝试，但限流必须早于准入登记和
		// goroutine 创建。Token 一旦消费便不因后续 Route fencing 或 OPEN 失败退还。
		if manager.options.SourceLimiter != nil {
			sourceIP, sourceErr := remoteSourceIP(connection.RemoteAddr())
			if sourceErr != nil || manager.options.SourceLimiter.AllowOpen(sourceIP) != nil {
				if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
					manager.recordConnectionError(fmt.Errorf(
						"close TCP ingress connection rejected by source gate: %w",
						closeErr,
					))
				}
				continue
			}
		}
		route, admitted := manager.admit(listener, connection)
		if !admitted {
			if closeErr := connection.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				manager.recordConnectionError(fmt.Errorf(
					"close TCP ingress connection rejected by admission fence: %w",
					closeErr,
				))
			}
			continue
		}
		var (
			cleanupErr      error
			handlerPanicked bool
		)
		safego.Go(
			func(err error) {
				handlerPanicked = true
				manager.recordRuntimeError(errors.Join(
					fmt.Errorf("TCP ingress connection handler: %w", err),
					cleanupErr,
				))
			},
			func() {
				defer manager.connectionWait.Done()
				if !handlerPanicked && cleanupErr != nil {
					manager.recordConnectionError(cleanupErr)
				}
			},
			func() {
				// Close 必须先于 ownership 删除完成。Handler panic 时 safego 会在
				// 本函数的 defer 全部执行后才恢复并报告，因此 Shutdown 不会把仍未
				// 关闭的 Socket 误判为已经排空。
				defer manager.releaseConnection(connection)
				defer func() {
					if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
						cleanupErr = fmt.Errorf("close TCP ingress connection: %w", err)
					}
				}()
				if manager.options.Handler != nil {
					manager.options.Handler(ctx, connection, route)
				}
			},
		)
	}
}

func remoteSourceIP(address net.Addr) (netip.Addr, error) {
	if address == nil {
		return netip.Addr{}, errors.New("TCP ingress peer address is unavailable")
	}
	if tcpAddress, ok := address.(*net.TCPAddr); ok {
		sourceIP, valid := netip.AddrFromSlice(tcpAddress.IP)
		if !valid {
			return netip.Addr{}, errors.New("TCP ingress peer source IP is invalid")
		}
		return sourceIP.Unmap().WithZone(""), nil
	}
	addressPort, err := netip.ParseAddrPort(address.String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse TCP ingress peer address: %w", err)
	}
	return addressPort.Addr().Unmap().WithZone(""), nil
}

// retireAcceptFailure 把意外终止的 Accept owner 从 Actual 原子摘除，并留下当前
// Route 的 LISTEN_FAILED 事实。其他 Listener 不受影响；关闭旧 Socket 后同代 dirty
// 立即触发重建，周期 Reconcile 继续覆盖持续性系统错误。
func (manager *Manager) retireAcceptFailure(listener *managedListener) {
	manager.mu.Lock()
	if manager.stopping {
		manager.mu.Unlock()
		return
	}
	manager.admissionMu.Lock()
	var (
		port  uint16
		route serverroute.TCPRoute
		found bool
	)
	for actualPort, actual := range manager.actual {
		if actual != listener {
			continue
		}
		current := listener.route.Load()
		if current != nil {
			port, route, found = actualPort, *current, true
		}
		break
	}
	if found {
		listener.accepting = false
		delete(manager.actual, port)
		manager.failures[route.ID] = manager.failure(manager.generation, route)
		manager.publishLocked()
	}
	manager.admissionMu.Unlock()
	generation := manager.generation
	manager.mu.Unlock()
	if !found {
		return
	}
	if err := manager.closeOwnedListeners(map[uint16]*managedListener{port: listener}); err != nil {
		manager.recordRuntimeError(fmt.Errorf("close failed TCP accept listener: %w", err))
	}
	manager.MarkDirty(generation)
}

func (manager *Manager) admit(
	listener *managedListener,
	connection net.Conn,
) (serverroute.TCPRoute, bool) {
	manager.admissionMu.Lock()
	defer manager.admissionMu.Unlock()
	if !manager.accepting || !listener.accepting {
		return serverroute.TCPRoute{}, false
	}
	// Route Snapshot 先于下游 observer 通知原子发布。以这里的 Current 读取作为
	// 准入线性化点，确保新代次一旦可见，尚未 reconcile 的旧 Listener 不能再
	// 登记连接；若新代次在读取之后发布，本次连接则线性化在发布之前。
	current := manager.options.Routes.Current()
	if current == nil || current.Generation() != manager.generation {
		return serverroute.TCPRoute{}, false
	}
	route := listener.route.Load()
	if route == nil {
		return serverroute.TCPRoute{}, false
	}
	manager.connections[connection] = struct{}{}
	// Add 与 admission fence 共用锁。StopAccepting 设置 accepting=false 后，
	// Shutdown 才会开始 Wait，因此不会发生 WaitGroup.Add/Wait 竞态。
	manager.connectionWait.Add(1)
	return *route, true
}

func (manager *Manager) releaseConnection(connection net.Conn) {
	manager.admissionMu.Lock()
	delete(manager.connections, connection)
	manager.admissionMu.Unlock()
}

// StopAccepting 建立 admission fence、停止 reconcile，并关闭全部 Listener 解除 Accept。
// 已接受连接继续运行，由 Shutdown Deadline 或 Close 负责最终收敛。
func (manager *Manager) StopAccepting() error {
	if manager == nil {
		return ErrInvalidOptions
	}
	manager.stopOnce.Do(func() {
		manager.mu.Lock()
		manager.stopping = true
		manager.admissionMu.Lock()
		manager.accepting = false
		manager.admissionMu.Unlock()
		cancel := manager.cancelAccept
		listeners := manager.actual
		manager.actual = make(map[uint16]*managedListener)
		manager.failures = make(map[string]ApplyFailure)
		manager.publishLocked()
		started := manager.started
		manager.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		closeErr := manager.closeOwnedListeners(listeners)
		// Close 返回错误时对象仍留在 residual ownership。立即再尝试一次，确保
		// 瞬时错误不会让仍阻塞在 Accept 的 owner 卡住后续 Wait；持续错误则对
		// 生产 TCPListener 设置立即 Deadline，显式解除 Accept 后仍保留 residual。
		retryErr := manager.closeResidualListeners()
		interruptErr := manager.interruptResidualAccepts()
		manager.stopErr = errors.Join(closeErr, retryErr, interruptErr)
		if started {
			<-manager.ownerDone
			if interruptErr == nil {
				manager.acceptWait.Wait()
			}
		}
	})
	return manager.stopErr
}

// Shutdown 停止新 Accept，并在 Deadline 内等待已接受连接自然结束；超时后主动关闭。
func (manager *Manager) Shutdown(ctx context.Context) error {
	if manager == nil || ctx == nil {
		return ErrInvalidOptions
	}
	stopErr := manager.StopAccepting()
	done := make(chan struct{})
	safego.Go(func(error) {}, func() { close(done) }, manager.connectionWait.Wait)
	select {
	case <-done:
		return stopErr
	case <-ctx.Done():
		manager.mu.Lock()
		cancel := manager.cancelConnections
		manager.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		connectionErr := manager.closeConnections()
		<-done
		return errors.Join(stopErr, connectionErr, ctx.Err())
	}
}

// Close 立即停止全部入口、取消 Handler Context、关闭残留连接并等待 owner 退出。
func (manager *Manager) Close() error {
	if manager == nil {
		return ErrInvalidOptions
	}
	manager.closeOnce.Do(func() {
		stopErr := manager.StopAccepting()
		manager.mu.Lock()
		cancel := manager.cancelConnections
		manager.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		residualErr := manager.closeResidualListeners()
		if residualErr == nil {
			manager.acceptWait.Wait()
		}
		connectionErr := manager.closeConnections()
		manager.connectionWait.Wait()
		manager.errorMu.Lock()
		runtimeErr := manager.runtimeErr
		recordedConnectionErr := manager.connectionErr
		manager.errorMu.Unlock()
		manager.closeErr = errors.Join(stopErr, connectionErr, residualErr, runtimeErr, recordedConnectionErr)
	})
	return manager.closeErr
}

// Actual 返回当前发布的稳定 Listener 值副本。
func (manager *Manager) Actual() []ActualListener {
	if manager == nil {
		return nil
	}
	state := manager.published.Load()
	if state == nil {
		return nil
	}
	return append([]ActualListener(nil), state.actual...)
}

// ApplyFailures 返回当前 generation 的全部失败，按端口和 Route ID 稳定排序。
func (manager *Manager) ApplyFailures() []ApplyFailure {
	if manager == nil {
		return nil
	}
	state := manager.published.Load()
	if state == nil {
		return nil
	}
	return append([]ApplyFailure(nil), state.failures...)
}

// ServiceApplyFailure 将当前 per-route 失败折叠为状态计算器需要的 Service 摘要。
// 详情仍由 ApplyFailures 保留；任一匹配当前 RequiredRevision 的 Route 失败即可使
// Service 进入 APPLY_FAILED，确定性选择排序后的第一个失败。
func (manager *Manager) ServiceApplyFailure(serviceID string, requiredRevision uint64) *serverstatus.ApplyFailure {
	for _, failure := range manager.ApplyFailures() {
		if failure.ServiceID == serviceID && failure.RequiredRevision >= 0 &&
			uint64(failure.RequiredRevision) == requiredRevision {
			return &serverstatus.ApplyFailure{
				RequiredRevision: requiredRevision,
				ErrorCode:        failure.ErrorCode,
				FailedAt:         failure.FailedAt,
			}
		}
	}
	return nil
}

func (manager *Manager) failure(generation uint64, route serverroute.TCPRoute) ApplyFailure {
	return ApplyFailure{
		RouteID: route.ID, ServiceID: route.ServiceID, PublicPort: route.PublicPort,
		RequiredRevision: route.RequiredRevision, Generation: generation,
		ErrorCode: ListenFailedErrorCode, FailedAt: manager.options.now().UTC(),
	}
}

func (manager *Manager) publishLocked() {
	actual := make([]ActualListener, 0, len(manager.actual))
	for _, listener := range manager.actual {
		route := listener.route.Load()
		if route != nil {
			actual = append(actual, ActualListener{Route: *route, Address: listener.listener.Addr().String()})
		}
	}
	sort.Slice(actual, func(left, right int) bool {
		return actual[left].Route.PublicPort < actual[right].Route.PublicPort
	})
	failures := make([]ApplyFailure, 0, len(manager.failures))
	for _, failure := range manager.failures {
		failures = append(failures, failure)
	}
	sort.Slice(failures, func(left, right int) bool {
		if failures[left].PublicPort != failures[right].PublicPort {
			return failures[left].PublicPort < failures[right].PublicPort
		}
		return failures[left].RouteID < failures[right].RouteID
	})
	manager.published.Store(&publishedState{generation: manager.generation, actual: actual, failures: failures})
}

func (manager *Manager) closeConnections() error {
	manager.admissionMu.Lock()
	connections := make([]net.Conn, 0, len(manager.connections))
	for connection := range manager.connections {
		connections = append(connections, connection)
	}
	manager.admissionMu.Unlock()
	errs := make([]error, 0, len(connections))
	for _, connection := range connections {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (manager *Manager) recordRuntimeError(err error) {
	if err == nil {
		return
	}
	manager.errorMu.Lock()
	first := manager.runtimeErr == nil
	if first {
		manager.runtimeErr = err
	}
	manager.errorMu.Unlock()
	if first && manager.options.ReportRuntimeError != nil {
		manager.options.ReportRuntimeError(err)
	}
}

// recordConnectionError 保留首个普通连接清理错误供最终 Close 汇总，但不通知
// 进程 Fatal Runtime Channel。单个客户端 Socket 的清理失败不能终止整个 Server。
func (manager *Manager) recordConnectionError(err error) {
	if err == nil {
		return
	}
	manager.errorMu.Lock()
	if manager.connectionErr == nil {
		manager.connectionErr = err
	}
	manager.errorMu.Unlock()
}

// closeOwnedListeners 先从运行集合转移到 residual ownership，再执行 Close。
// Close 失败的对象继续由 Manager 持有，最终 Close 会再次尝试，避免 FD 所有权
// 因一次系统调用失败而从状态中静默消失。
func (manager *Manager) closeOwnedListeners(listeners map[uint16]*managedListener) error {
	manager.mu.Lock()
	for _, listener := range listeners {
		manager.residual[listener] = struct{}{}
	}
	manager.mu.Unlock()

	errs := make([]error, 0, len(listeners))
	for _, listener := range listeners {
		err := listener.listener.Close()
		if err == nil || errors.Is(err, net.ErrClosed) {
			manager.mu.Lock()
			delete(manager.residual, listener)
			manager.mu.Unlock()
			continue
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (manager *Manager) closeResidualListeners() error {
	manager.mu.Lock()
	listeners := make([]*managedListener, 0, len(manager.residual))
	for listener := range manager.residual {
		listeners = append(listeners, listener)
	}
	manager.mu.Unlock()
	errs := make([]error, 0, len(listeners))
	for _, listener := range listeners {
		err := listener.listener.Close()
		if err == nil || errors.Is(err, net.ErrClosed) {
			manager.mu.Lock()
			delete(manager.residual, listener)
			manager.mu.Unlock()
			continue
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (manager *Manager) interruptResidualAccepts() error {
	manager.mu.Lock()
	listeners := make([]*managedListener, 0, len(manager.residual))
	for listener := range manager.residual {
		listeners = append(listeners, listener)
	}
	manager.mu.Unlock()
	errs := make([]error, 0, len(listeners))
	for _, listener := range listeners {
		deadlineListener, ok := listener.listener.(interface{ SetDeadline(time.Time) error })
		if !ok {
			errs = append(errs, errors.New("TCP listener cannot interrupt Accept after Close failure"))
			continue
		}
		if err := deadlineListener.SetDeadline(time.Now()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
