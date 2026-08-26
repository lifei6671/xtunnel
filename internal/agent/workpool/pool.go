// Package workpool 实现 Agent 侧由 Server WorkDemand 驱动的 WorkConn 池。
//
// Pool 只负责 Budget Lease 合并、Work ALPN 建连、WorkHello 认证以及
// CONNECTING -> IDLE 的线性化。生产 OPEN Handler 还会把 OPENING/ACTIVE 提交给
// Pool 同步计数；OpenRequest/RAW 的网络 IO 仍由 Handler 独占，本包不会读取业务帧。
package workpool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
	"github.com/lifei6671/xtunnel/internal/agent/workauth"
	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

const (
	// MaxConnecting 是 V0.1 Binary 不可由远端放大的并发建连硬上限。
	MaxConnecting = 16
	// MaxTotal 是 Pool 持有的 CONNECTING + IDLE（后续含 OPENING/ACTIVE）硬上限。
	MaxTotal = 256

	maxConnectionTokenBytes = 8192
)

var (
	// ErrInvalidConfig 表示 Token、Session、Handler 或认证超时不满足固定边界。
	ErrInvalidConfig = errors.New("agent work pool config is invalid")
	// ErrInvalidDemand 表示 WorkDemand 不是完整 Grant 或完整 lowering/cancel 形状。
	ErrInvalidDemand = errors.New("agent work pool demand is invalid")
	// ErrPoolNotRunning 表示 Start 前调用了 ApplyDemand 或 Wait。
	ErrPoolNotRunning = errors.New("agent work pool is not running")
	// ErrPoolAlreadyStarted 表示同一个 Pool 被重复启动。
	ErrPoolAlreadyStarted = errors.New("agent work pool has already started")
	// ErrPoolClosed 表示 Session 或 Context 已经结束，不再接受 Demand。
	ErrPoolClosed = errors.New("agent work pool is closed")
	// ErrPoolDraining 表示 DrainAck 已提交，当前 Pool 不再接受新的 OPEN 相位迁移。
	ErrPoolDraining = errors.New("agent work pool is draining")
	// ErrInvalidReady 表示认证函数没有返回处于 IDLE 的完整 WorkConn 状态。
	ErrInvalidReady = errors.New("agent work pool received invalid authenticated work connection")
	// ErrInvalidTransition 表示可观察 Handler 提交了不合法或乱序的 Work 相位迁移。
	ErrInvalidTransition = errors.New("agent work pool handler transition is invalid")
)

// Handler 同步处理一条 IDLE WorkConn，后续 M1-10/M1-11 在这里接入 OPEN/RAW。
// Handle 调用期间 Handler 是 connection 和 Ready.State 的唯一 IO/协议状态使用者；
// Pool 只保留 Session 结束时关闭底层连接的权力。Handle 返回即归还独占权，Pool
// 随后确保连接和状态关闭。Handler 不得转交给调用结束后仍运行的 goroutine。
// 该签名与 internal/agent/open.Handler 直接一致，不需要额外适配层。
type Handler interface {
	Handle(context.Context, net.Conn, *workauth.Ready) error
}

// phaseObservingHandler 是生产 OPEN Handler 提供的可选能力。
//
// transition 必须同步调用：Pool 会在同一个临界区内执行纯状态 commit，并同步更新
// IDLE/OPENING/ACTIVE 计数，使协议状态与池计数对 ApplyDemand 呈现一个原子提交点。
// 普通 Handler 无需实现此接口，Pool 会保守地把其整个生命周期计作 IDLE。
type phaseObservingHandler interface {
	Handler
	HandleObserved(
		context.Context,
		net.Conn,
		*workauth.Ready,
		func(state.WorkPhase, func() error) error,
	) error
}

// HandlerFunc 让普通函数满足 Handler。
type HandlerFunc func(context.Context, net.Conn, *workauth.Ready) error

// Handle 调用 function。
func (function HandlerFunc) Handle(ctx context.Context, connection net.Conn, ready *workauth.Ready) error {
	return function(ctx, connection, ready)
}

// Config 是一个已认证 Control Session 对应 WorkPool 的固定输入。
type Config struct {
	// ConnectionToken 只用于 Token-derived Work ALPN TLS 拨号，不从其他来源取地址或信任。
	ConnectionToken string
	// Session 是当前 Control Session 的 WorkAuth 材料；Pool 会清除自己的 Secret 副本。
	Session controlauth.Session
	// SessionDone 必须是对应 Control Session 的 Done；关闭后 Pool 立即停止补池并回收
	// 非 ACTIVE 连接，已确认 ACTIVE 的旧代连接继续自然完成。
	SessionDone <-chan struct{}
	// Handler 接收已经线性化为 IDLE 的连接，Pool 本身不读取 OpenRequest；生产
	// open.Handler 的可选观察能力会让 Pool 精确区分 IDLE/OPENING/ACTIVE。
	Handler Handler
	// AuthWriteTimeout 与 AuthReadTimeout 限制 WorkHello/WorkReady 完整 Frame IO。
	AuthWriteTimeout time.Duration
	AuthReadTimeout  time.Duration
}

// Stats 是 Pool 锁内线性化取得的当前计数快照。
type Stats struct {
	DemandGeneration uint64
	Connecting       int
	Idle             int
	Opening          int
	Active           int
	Total            int
	Failures         uint64
	// LastFailure 保留最近一次非取消的单连接失败，供上层结构化记录；错误不含 Token/Secret。
	LastFailure error
}

// DemandResult 描述一次 WorkDemand 是否成为最新 generation 及立即安排的工作。
// 后续 Connecting 完成后，Pool 可能继续使用同一 Lease 安排剩余槽位。
type DemandResult struct {
	Accepted           bool
	Started            int
	CanceledConnecting int
	ClosedIdle         int
}

type dialFunc func(context.Context, string, string) (net.Conn, error)
type authenticateFunc func(context.Context, net.Conn, workauth.Config) (*workauth.Ready, error)

type dependencies struct {
	dial         dialFunc
	authenticate authenticateFunc
	now          func() time.Time
}

// Budget 是同一 Agent Binary/Connector 跨 Control generation 共享的 WorkConn
// 硬预算。旧代 ACTIVE 只有在其 worker 真正退出后才释放槽位。
type Budget struct {
	mu      sync.Mutex
	used    int
	changed chan struct{}
}

// NewBudget 创建固定为 Binary max_total 的共享预算。
func NewBudget() *Budget {
	return &Budget{changed: make(chan struct{})}
}

func (budget *Budget) reserveUpTo(requested int) int {
	if budget == nil || requested <= 0 {
		return 0
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	granted := min(requested, MaxTotal-budget.used)
	budget.used += granted
	return granted
}

func (budget *Budget) release() {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.used == 0 {
		panic("agent WorkConn shared budget invariant violated")
	}
	budget.used--
	close(budget.changed)
	budget.changed = make(chan struct{})
}

func (budget *Budget) changedSignal() <-chan struct{} {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.changed
}

func (budget *Budget) usedCount() int {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.used
}

type demandState struct {
	generation uint64
	leaseID    string
	desired    int
	remaining  uint64
	deadline   time.Time
}

type workPhase uint8

const (
	workConnecting workPhase = iota + 1
	workIdle
	workOpening
	workActive
)

type workEntry struct {
	id             uint64
	generation     uint64
	phase          workPhase
	cancel         context.CancelFunc
	handlerContext context.Context
	handlerCancel  context.CancelFunc
	canceled       bool
	// observable 表示 Handler 会同步报告真实 OPENING/ACTIVE 相位；只有这类明确仍为
	// IDLE 的项，普通 Demand 降低时才允许主动关闭，避免误断旧 Handler 的业务流量。
	observable bool

	connectionMu sync.Mutex
	connection   net.Conn
	closeOnce    sync.Once
	closeErr     error
	done         chan struct{}
}

type workJob struct {
	entry   *workEntry
	ctx     context.Context
	session controlauth.Session
	leaseID string
}

// Pool 是一个 Control Session 独占的 WorkConn 池。
//
// mu 只保护 generation、Lease、Registry 和 Counter；持锁期间禁止 Dial、AUTH、
// Handler、Channel 等待或 Close。所有网络操作都在有限的 worker goroutine 中完成。
type Pool struct {
	config       Config
	dependencies dependencies
	budget       *Budget

	mu          sync.Mutex
	started     bool
	closed      bool
	draining    bool
	drainAcked  bool
	ctx         context.Context
	cancel      context.CancelFunc
	demand      demandState
	nextID      uint64
	entries     map[uint64]*workEntry
	connecting  int
	idle        int
	opening     int
	active      int
	failures    uint64
	lastFailure error

	workers sync.WaitGroup
	// waitDone 在本代非 ACTIVE WorkConn 已完成清理后关闭；普通 Control Session
	// 断开时，Wait 可据此返回并允许下一代重连，不等待已脱离本代的旧 ACTIVE。
	waitDone chan struct{}
	waitOnce sync.Once
	// done 仍表示包括已脱离旧 ACTIVE 在内的全部 worker 真正退出。
	done     chan struct{}
	doneOnce sync.Once
	result   error
	// detachedActive 保存普通 Session 断开后继续自然运行的旧代 ACTIVE。它们不再
	// 计入本 Pool Stats，但进程取消仍可通过父 Context 升级为强制关闭。
	detachedActive map[uint64]*workEntry
	// phaseChanged 在 ACTIVE 计数或关闭状态变化时按 close-and-replace 广播，
	// 让优雅排空等待者不需要轮询，也不会在检查与等待之间漏掉归零事件。
	phaseChanged chan struct{}
	// detachedCloseErr 收集 Dial 返回后才发现 Registry 已由 shutdown 删除时的关闭错误。
	detachedCloseErr error
}

// New 创建生产 WorkPool。Pool 在 Start 前不会启动 goroutine 或建立网络连接。
func New(config Config) (*Pool, error) {
	return NewWithBudget(config, NewBudget())
}

// NewWithBudget 创建使用 Connector 进程级共享预算的 WorkPool。
// Runtime 的每个 Control generation 必须复用同一个 Budget。
func NewWithBudget(config Config, budget *Budget) (*Pool, error) {
	return newPoolWithBudget(config, dependencies{
		dial: func(ctx context.Context, token, alpn string) (net.Conn, error) {
			return agentgateway.DialContext(ctx, token, alpn)
		},
		authenticate: workauth.Authenticate,
		now:          time.Now,
	}, budget)
}

func newPool(config Config, dependencies dependencies) (*Pool, error) {
	return newPoolWithBudget(config, dependencies, NewBudget())
}

func newPoolWithBudget(config Config, dependencies dependencies, budget *Budget) (*Pool, error) {
	if len(config.ConnectionToken) == 0 || len(config.ConnectionToken) > maxConnectionTokenBytes ||
		config.SessionDone == nil || !validHandler(config.Handler) ||
		config.AuthWriteTimeout <= 0 || config.AuthReadTimeout <= 0 ||
		dependencies.dial == nil || dependencies.authenticate == nil || dependencies.now == nil || budget == nil {
		return nil, ErrInvalidConfig
	}
	if err := identity.ValidateTunnelID(config.Session.TunnelID); err != nil {
		return nil, fmt.Errorf("%w: tunnel id: %v", ErrInvalidConfig, err)
	}
	if err := identity.ValidateConnectorID(config.Session.ConnectorID); err != nil {
		return nil, fmt.Errorf("%w: connector id: %v", ErrInvalidConfig, err)
	}
	if err := identity.ValidateSessionID(config.Session.SessionID); err != nil {
		return nil, fmt.Errorf("%w: session id: %v", ErrInvalidConfig, err)
	}
	// WorkPool 永远不能取得 Control 状态指针；它只保存 WorkHello HMAC 所需的值副本。
	config.Session.Control = nil
	return &Pool{
		config:         config,
		dependencies:   dependencies,
		budget:         budget,
		entries:        make(map[uint64]*workEntry),
		detachedActive: make(map[uint64]*workEntry),
		waitDone:       make(chan struct{}),
		done:           make(chan struct{}),
		phaseChanged:   make(chan struct{}),
	}, nil
}

// Start 把 Pool 绑定到当前 Control Session 生命周期并立即返回。
// Context 结束会关闭全部连接；普通 SessionDone 只关闭非 ACTIVE，并让旧 ACTIVE
// 脱离本代 Pool 自然结束。
func (pool *Pool) Start(parent context.Context) error {
	if parent == nil {
		return ErrInvalidConfig
	}
	if err := parent.Err(); err != nil {
		return err
	}

	pool.mu.Lock()
	if pool.started {
		pool.mu.Unlock()
		return ErrPoolAlreadyStarted
	}
	pool.ctx, pool.cancel = context.WithCancel(parent)
	pool.started = true
	pool.mu.Unlock()

	go pool.observeLifetime(parent)
	return nil
}

// ApplyDemand 合并一个已经由 Control Owner 校验方向的 WorkDemand。
//
// 只有严格更高的 demand_generation 会替换当前 Lease；旧或重复 generation 安全忽略。
// 新 generation 会取消仍处于 CONNECTING 的旧 Lease 尝试。每次安排一个连接就保守
// 消耗一个 max_new_connections 槽位，因为 WorkHello 是否已经被 Server 消费在网络
// 失败时无法可靠判定，禁止重用可能已经消费的 Lease 槽位。
func (pool *Pool) ApplyDemand(message *protocolv1.WorkDemand) (DemandResult, error) {
	demand, err := parseDemand(message, pool.dependencies.now())
	if err != nil {
		return DemandResult{}, err
	}

	pool.mu.Lock()
	if !pool.started {
		pool.mu.Unlock()
		return DemandResult{}, ErrPoolNotRunning
	}
	if pool.closed || pool.ctx.Err() != nil {
		pool.mu.Unlock()
		return DemandResult{}, ErrPoolClosed
	}
	// 发出 DrainRequest 后 Server 可能仍有已经排队的旧 WorkDemand。此时协议层仍处于
	// ESTABLISHED，消息方向合法，但业务层必须忽略它，保证本地“停止补池”不可逆。
	if pool.draining {
		pool.mu.Unlock()
		return DemandResult{}, nil
	}
	select {
	case <-pool.config.SessionDone:
		pool.mu.Unlock()
		return DemandResult{}, ErrPoolClosed
	default:
	}
	if demand.generation <= pool.demand.generation {
		pool.mu.Unlock()
		return DemandResult{}, nil
	}

	previousDesired := pool.demand.desired
	pool.demand = demand
	stale := pool.cancelStaleConnectingLocked(demand.generation)
	trimmed := pool.trimIdleLocked(previousDesired, demand.desired)
	jobs := pool.reconcileLocked(pool.dependencies.now())
	pool.workers.Add(len(jobs))
	pool.mu.Unlock()

	// Cancel/Close 和 worker 启动都在锁外执行；先回收旧连接，避免瞬时突破实际 FD 上限。
	var cancelErr error
	for _, entry := range stale {
		entry.cancel()
		cancelErr = errors.Join(cancelErr, closeTransport(entry))
	}
	for _, entry := range trimmed {
		entry.cancel()
		entry.handlerCancel()
		cancelErr = errors.Join(cancelErr, closeTransport(entry))
	}
	pool.launch(jobs)
	return DemandResult{
		Accepted: true, Started: len(jobs), CanceledConnecting: len(stale), ClosedIdle: len(trimmed),
	}, cancelErr
}

// BeginDrain 原子停止补池，并取消尚未完成认证的 CONNECTING WorkConn。
//
// 已经 IDLE/OPENING/ACTIVE 的连接暂时保留：Server 只有在摘除 Connector、并等待
// 已 Acquire 的 OPENING 完成后才会发送 DrainAck，因此 Ack 前 Agent 必须继续接住
// 这些在途 OPEN。重复调用幂等，不会重复关闭同一连接。
func (pool *Pool) BeginDrain() error {
	pool.mu.Lock()
	if !pool.started {
		pool.mu.Unlock()
		return ErrPoolNotRunning
	}
	if pool.closed {
		pool.mu.Unlock()
		return ErrPoolClosed
	}
	if pool.draining {
		pool.mu.Unlock()
		return nil
	}
	pool.draining = true
	pool.demand.leaseID = ""
	pool.demand.remaining = 0
	connecting := make([]*workEntry, 0, pool.connecting)
	for _, entry := range pool.entries {
		if entry.phase != workConnecting || entry.canceled {
			continue
		}
		entry.canceled = true
		connecting = append(connecting, entry)
	}
	pool.mu.Unlock()

	var closeErr error
	for _, entry := range connecting {
		entry.cancel()
		closeErr = errors.Join(closeErr, closeTransport(entry))
	}
	return closeErr
}

// CompleteDrain 提交匹配 DrainAck，关闭所有非 ACTIVE WorkConn，并等待 ACTIVE 自然结束。
// ctx 必须使用收到 SIGTERM 后新建的相对 Deadline Context，不能继承已经取消的进程
// Context。Deadline 到达时会强制关闭剩余连接，保证 goroutine、FD 和计数最终归零。
func (pool *Pool) CompleteDrain(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidConfig
	}

	pool.mu.Lock()
	if !pool.started {
		pool.mu.Unlock()
		return ErrPoolNotRunning
	}
	if pool.closed {
		pool.mu.Unlock()
		return ErrPoolClosed
	}
	if !pool.draining {
		pool.mu.Unlock()
		return ErrInvalidTransition
	}
	pool.drainAcked = true
	nonActive := make([]*workEntry, 0, pool.connecting+pool.idle+pool.opening)
	for id, entry := range pool.entries {
		if entry.phase == workActive {
			continue
		}
		entry.canceled = true
		delete(pool.entries, id)
		switch entry.phase {
		case workConnecting:
			pool.connecting--
		case workIdle:
			pool.idle--
		case workOpening:
			pool.opening--
		}
		nonActive = append(nonActive, entry)
	}
	pool.mu.Unlock()

	var closeErr error
	for _, entry := range nonActive {
		entry.cancel()
		entry.handlerCancel()
		closeErr = errors.Join(closeErr, closeTransport(entry))
	}

	for {
		pool.mu.Lock()
		if pool.active == 0 {
			pool.mu.Unlock()
			return closeErr
		}
		if pool.closed {
			pool.mu.Unlock()
			return errors.Join(closeErr, ErrPoolClosed)
		}
		changed := pool.phaseChanged
		pool.mu.Unlock()

		select {
		case <-ctx.Done():
			// 仅取消 Context 无法唤醒阻塞中的 net.Conn.Read；统一 shutdown 会给所有
			// 连接设置立即 Deadline 并 Close，再等待 worker 完整退出。
			pool.shutdown(ctx.Err())
			return errors.Join(closeErr, ctx.Err())
		case <-changed:
		}
	}
}

// Stats 返回不会与 ApplyDemand 或 worker 状态迁移竞争的计数快照。
func (pool *Pool) Stats() Stats {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return Stats{
		DemandGeneration: pool.demand.generation,
		Connecting:       pool.connecting,
		Idle:             pool.idle,
		Opening:          pool.opening,
		Active:           pool.active,
		Total:            len(pool.entries),
		Failures:         pool.failures,
		LastFailure:      pool.lastFailure,
	}
}

// Done 在包括普通 SessionDone 后保留的旧 ACTIVE 在内，全部 worker 退出后关闭。
func (pool *Pool) Done() <-chan struct{} {
	return pool.done
}

// Wait 等待本代非 ACTIVE 清理完成。普通 SessionDone 不等待旧 ACTIVE，避免阻塞重连；
// Context 取消会强制关闭全部连接并作为终止原因返回。
func (pool *Pool) Wait() error {
	pool.mu.Lock()
	started := pool.started
	pool.mu.Unlock()
	if !started {
		return ErrPoolNotRunning
	}
	<-pool.waitDone
	return pool.result
}

func (pool *Pool) observeLifetime(parent context.Context) {
	for {
		changed := pool.budget.changedSignal()
		select {
		case <-parent.Done():
			pool.shutdown(parent.Err())
			return
		case <-pool.config.SessionDone:
			pool.shutdown(nil)
			// 普通 Session 清理完成后，旧 ACTIVE 仍可自然运行；进程 Context 后续
			// 取消时必须升级为强制关闭，全部 worker 先结束则直接退出 owner。
			select {
			case <-parent.Done():
				pool.shutdown(parent.Err())
			case <-pool.done:
			}
			return
		case <-changed:
			// 旧 generation 的 ACTIVE 真正退出后，当前 Pool 立即重新协调尚有效
			// Demand，避免共享预算释放后长期低于 desired_non_active。
			pool.refill()
		}
	}
}

func (pool *Pool) shutdown(cause error) {
	pool.mu.Lock()
	if pool.closed {
		if cause == nil || len(pool.detachedActive) == 0 {
			pool.mu.Unlock()
			return
		}
		pool.cancel()
		entries := make([]*workEntry, 0, len(pool.detachedActive))
		for id, entry := range pool.detachedActive {
			entries = append(entries, entry)
			delete(pool.detachedActive, id)
		}
		pool.mu.Unlock()
		var shutdownErr error
		for _, entry := range entries {
			entry.cancel()
			entry.handlerCancel()
			shutdownErr = errors.Join(shutdownErr, closeTransport(entry))
		}
		pool.workers.Wait()
		pool.finishShutdown(cause, shutdownErr)
		return
	}
	pool.closed = true
	entries := make([]*workEntry, 0, len(pool.entries))
	for id, entry := range pool.entries {
		if cause == nil && entry.phase == workActive {
			pool.detachedActive[id] = entry
			continue
		}
		entries = append(entries, entry)
	}
	pool.entries = make(map[uint64]*workEntry)
	pool.connecting = 0
	pool.idle = 0
	pool.opening = 0
	pool.active = 0
	pool.signalPhaseChangedLocked()
	if cause != nil {
		pool.cancel()
	}
	pool.mu.Unlock()

	var shutdownErr error
	for _, entry := range entries {
		entry.cancel()
		entry.handlerCancel()
		shutdownErr = errors.Join(shutdownErr, closeTransport(entry))
	}
	if cause != nil {
		pool.workers.Wait()
		pool.finishShutdown(cause, shutdownErr)
		return
	}
	for _, entry := range entries {
		<-entry.done
	}
	pool.finishWait(nil, shutdownErr)
	go func() {
		pool.workers.Wait()
		pool.doneOnce.Do(func() { close(pool.done) })
	}()
}

func (pool *Pool) finishShutdown(cause, shutdownErr error) {
	pool.finishWait(cause, shutdownErr)
	pool.doneOnce.Do(func() { close(pool.done) })
}

func (pool *Pool) finishWait(cause, shutdownErr error) {
	pool.waitOnce.Do(func() {
		pool.mu.Lock()
		clear(pool.config.Session.SessionSecret[:])
		pool.result = errors.Join(cause, shutdownErr, pool.detachedCloseErr)
		pool.mu.Unlock()
		close(pool.waitDone)
	})
}

func (pool *Pool) launch(jobs []workJob) {
	for index := range jobs {
		job := jobs[index]
		go pool.connect(job)
	}
}

func (pool *Pool) connect(job workJob) {
	defer pool.workers.Done()
	defer close(job.entry.done)
	defer pool.budget.release()
	defer clear(job.session.SessionSecret[:])
	defer job.entry.cancel()
	defer job.entry.handlerCancel()

	connection, err := pool.dependencies.dial(job.ctx, pool.config.ConnectionToken, servergateway.WorkALPN)
	if err != nil {
		if connection != nil {
			err = errors.Join(err, closeConnection(connection))
		}
		pool.finishConnecting(job.entry, fmt.Errorf("dial WorkConn: %w", err))
		return
	}
	if !pool.attachConnection(job.entry, connection) {
		pool.recordDetachedCloseError(closeConnection(connection))
		return
	}

	ready, err := pool.dependencies.authenticate(job.ctx, connection, workauth.Config{
		Session:       job.session,
		BudgetLeaseID: job.leaseID,
		WriteTimeout:  pool.config.AuthWriteTimeout,
		ReadTimeout:   pool.config.AuthReadTimeout,
	})
	if err != nil {
		err = errors.Join(fmt.Errorf("authenticate WorkConn: %w", err), closeTransport(job.entry))
		pool.finishConnecting(job.entry, err)
		return
	}
	if ready == nil || ready.State == nil || ready.State.Phase() != state.WorkIdle || ready.WorkID == "" {
		if ready != nil && ready.State != nil {
			ready.State.Close()
		}
		err = errors.Join(ErrInvalidReady, closeTransport(job.entry))
		pool.finishConnecting(job.entry, err)
		return
	}
	if !pool.transitionToIdle(job.ctx, job.entry) {
		ready.State.Close()
		closeErr := closeTransport(job.entry)
		pool.finishConnecting(job.entry, nil)
		pool.recordDetachedCloseError(closeErr)
		return
	}

	// Lease Deadline 只约束 Dial/AUTH。进入 IDLE 后取消 attempt Context，后续 Handler
	// 使用整个 Control Session 的 Context，不能因 Lease 到期关闭已经消费成功的 WorkConn。
	job.entry.cancel()
	pool.refill()
	var handlerErr error
	if observed, ok := pool.config.Handler.(phaseObservingHandler); ok {
		handlerErr = observed.HandleObserved(job.entry.handlerContext, connection, ready, func(
			phase state.WorkPhase,
			commit func() error,
		) error {
			return pool.commitHandlerTransition(job.entry, phase, commit)
		})
	} else {
		// 旧 Handler 不报告相位时保守保持 IDLE：这可能少补连接，但绝不会因 Demand
		// 降低误断一条实际上已经 OPENING/ACTIVE 的业务连接。
		handlerErr = pool.config.Handler.Handle(job.entry.handlerContext, connection, ready)
	}
	// Handler 返回后才重新取得非并发安全 Work 状态的所有权，避免 shutdown 与
	// OPEN/RAW 状态迁移竞争；shutdown 只会并发关闭 net.Conn 来解除阻塞。
	ready.State.Close()
	closeErr := closeTransport(job.entry)
	pool.finishHandled(job.entry, errors.Join(handlerErr, closeErr))
}

func (pool *Pool) attachConnection(entry *workEntry, connection net.Conn) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	current, exists := pool.entries[entry.id]
	if !exists || current != entry || entry.phase != workConnecting {
		return false
	}
	entry.connectionMu.Lock()
	entry.connection = connection
	entry.connectionMu.Unlock()
	return true
}

func (pool *Pool) transitionToIdle(ctx context.Context, entry *workEntry) bool {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	current, exists := pool.entries[entry.id]
	if !exists || current != entry || entry.phase != workConnecting || entry.canceled ||
		pool.closed || pool.draining || ctx.Err() != nil || entry.generation != pool.demand.generation {
		return false
	}
	entry.phase = workIdle
	pool.connecting--
	pool.idle++
	return true
}

// commitHandlerTransition 把协议状态提交和 Pool 计数迁移放进同一临界区。
// commit 由 open.Handler 保证只修改当前 Work 状态且不做网络 IO；Pool 持锁期间
// 执行它，能避免 Demand 裁剪在线上协议已进入 OPENING 后仍把该项当作 IDLE 关闭。
func (pool *Pool) commitHandlerTransition(
	entry *workEntry,
	target state.WorkPhase,
	commit func() error,
) error {
	if commit == nil {
		return ErrInvalidTransition
	}

	pool.mu.Lock()
	current, exists := pool.entries[entry.id]
	if !exists || current != entry || pool.closed {
		pool.mu.Unlock()
		return ErrPoolClosed
	}
	if pool.drainAcked {
		pool.mu.Unlock()
		return ErrPoolDraining
	}

	switch target {
	case state.WorkOpening:
		if entry.phase != workIdle {
			pool.mu.Unlock()
			return ErrInvalidTransition
		}
		if err := commit(); err != nil {
			pool.mu.Unlock()
			return err
		}
		entry.phase = workOpening
		pool.idle--
		pool.opening++
		pool.mu.Unlock()
		return nil
	case state.WorkActive:
		if entry.phase != workOpening {
			pool.mu.Unlock()
			return ErrInvalidTransition
		}
		if err := commit(); err != nil {
			pool.mu.Unlock()
			return err
		}
		entry.phase = workActive
		pool.opening--
		pool.active++
		// ACTIVE 不属于 desired_non_active；提交后立即按同一 Demand 尝试补回待命连接。
		jobs := pool.reconcileLocked(pool.dependencies.now())
		pool.workers.Add(len(jobs))
		pool.mu.Unlock()
		pool.launch(jobs)
		return nil
	default:
		pool.mu.Unlock()
		return ErrInvalidTransition
	}
}

func (pool *Pool) finishConnecting(entry *workEntry, failure error) {
	pool.finish(entry, workConnecting, failure)
}

func (pool *Pool) finishHandled(entry *workEntry, failure error) {
	pool.finish(entry, 0, failure)
}

func (pool *Pool) finish(entry *workEntry, expected workPhase, failure error) {
	pool.mu.Lock()
	current, exists := pool.entries[entry.id]
	if !exists && pool.detachedActive[entry.id] == entry {
		delete(pool.detachedActive, entry.id)
		pool.mu.Unlock()
		return
	}
	if !exists || current != entry || (expected != 0 && entry.phase != expected) {
		pool.mu.Unlock()
		return
	}
	delete(pool.entries, entry.id)
	switch entry.phase {
	case workConnecting:
		pool.connecting--
	case workIdle:
		pool.idle--
	case workOpening:
		pool.opening--
	case workActive:
		pool.active--
		pool.signalPhaseChangedLocked()
	}
	if failure != nil && !entry.canceled && pool.ctx.Err() == nil {
		pool.failures++
		pool.lastFailure = failure
	}
	jobs := pool.reconcileLocked(pool.dependencies.now())
	pool.workers.Add(len(jobs))
	pool.mu.Unlock()
	pool.launch(jobs)
}

func (pool *Pool) refill() {
	pool.mu.Lock()
	jobs := pool.reconcileLocked(pool.dependencies.now())
	pool.workers.Add(len(jobs))
	pool.mu.Unlock()
	pool.launch(jobs)
}

func (pool *Pool) recordDetachedCloseError(err error) {
	if err == nil {
		return
	}
	pool.mu.Lock()
	pool.detachedCloseErr = errors.Join(pool.detachedCloseErr, err)
	pool.mu.Unlock()
}

func (pool *Pool) reconcileLocked(now time.Time) []workJob {
	if pool.closed || pool.draining || pool.demand.generation == 0 || pool.demand.leaseID == "" ||
		!now.Before(pool.demand.deadline) {
		return nil
	}
	// 冻结公式严格是 CONNECTING + IDLE + OPENING；已请求取消但尚未退出的 worker
	// 仍处于 CONNECTING，继续同时占用目标计数、并发硬上限和 FD 总量。待它真正
	// 退出后 finish 会再次协调并补足目标。ACTIVE 只参与 MaxTotal，不参与待命目标。
	nonActive := pool.connecting + pool.idle + pool.opening
	needed := pool.demand.desired - nonActive
	if needed <= 0 {
		return nil
	}
	availableConnecting := MaxConnecting - pool.connecting
	availableTotal := MaxTotal - len(pool.entries)
	count := min(needed, availableConnecting, availableTotal, int(min(pool.demand.remaining, uint64(MaxTotal))))
	count = pool.budget.reserveUpTo(count)
	if count <= 0 {
		return nil
	}

	jobs := make([]workJob, 0, count)
	for range count {
		attemptContext, cancel := context.WithDeadline(pool.ctx, pool.demand.deadline)
		handlerContext, handlerCancel := context.WithCancel(pool.ctx)
		pool.nextID++
		entry := &workEntry{
			id:             pool.nextID,
			generation:     pool.demand.generation,
			phase:          workConnecting,
			cancel:         cancel,
			handlerContext: handlerContext,
			handlerCancel:  handlerCancel,
			observable:     pool.handlerObservesPhases(),
			done:           make(chan struct{}),
		}
		pool.entries[entry.id] = entry
		pool.connecting++
		pool.demand.remaining--
		sessionCopy := pool.config.Session
		jobs = append(jobs, workJob{
			entry:   entry,
			ctx:     attemptContext,
			session: sessionCopy,
			leaseID: pool.demand.leaseID,
		})
	}
	return jobs
}

func (pool *Pool) signalPhaseChangedLocked() {
	close(pool.phaseChanged)
	pool.phaseChanged = make(chan struct{})
}

// trimIdleLocked 只在新 generation 明确降低目标时裁剪可观察 Handler 的真实 IDLE。
// OPENING/ACTIVE 从不进入候选；旧 Handler 的相位不可知，也不会被普通 Demand 打断。
// 被裁剪项在锁内先从 Registry/计数移除，实际 Cancel/Close 由调用方在锁外完成。
func (pool *Pool) trimIdleLocked(previousDesired, desired int) []*workEntry {
	if desired >= previousDesired {
		return nil
	}
	excess := pool.connecting + pool.idle + pool.opening - desired
	if excess <= 0 {
		return nil
	}

	trimmed := make([]*workEntry, 0, min(excess, pool.idle))
	for id, entry := range pool.entries {
		if excess == 0 {
			break
		}
		if entry.phase != workIdle || !entry.observable {
			continue
		}
		entry.canceled = true
		delete(pool.entries, id)
		pool.idle--
		excess--
		trimmed = append(trimmed, entry)
	}
	return trimmed
}

func (pool *Pool) cancelStaleConnectingLocked(generation uint64) []*workEntry {
	stale := make([]*workEntry, 0)
	for _, entry := range pool.entries {
		if entry.phase != workConnecting || entry.generation >= generation || entry.canceled {
			continue
		}
		// 旧 worker 必须继续占用 Connecting/Total 计数直到真正退出；否则新 generation
		// 可能在旧 Dial/AUTH 尚未停止时补入 16 条新连接，瞬时突破硬并发上限。
		entry.canceled = true
		stale = append(stale, entry)
	}
	return stale
}

func parseDemand(message *protocolv1.WorkDemand, now time.Time) (demandState, error) {
	if message == nil || message.GetDemandGeneration() == 0 {
		return demandState{}, ErrInvalidDemand
	}
	if err := validate.RejectUnknownFields(message); err != nil {
		return demandState{}, fmt.Errorf("%w: unknown fields: %v", ErrInvalidDemand, err)
	}
	desired := min(int(message.GetDesiredNonActive()), MaxTotal)
	leaseID := message.GetBudgetLeaseId()
	maximum := message.GetMaxNewConnections()
	ttlMS := message.GetLeaseTtlMs()

	// Server 在目标降低/取消时发送纯目标形状：空 Lease、max_new=0、ttl=0。
	// 一旦任一 Lease 字段出现，则三个字段必须完整，且 grant 必须能创建至少一条
	// 非 ACTIVE 连接；禁止接受半个 Grant 与取消字段混搭后猜测调用方意图。
	if leaseID == "" {
		if maximum != 0 || ttlMS != 0 {
			return demandState{}, ErrInvalidDemand
		}
		return demandState{generation: message.GetDemandGeneration(), desired: desired}, nil
	}
	if maximum == 0 || ttlMS == 0 || desired == 0 {
		return demandState{}, ErrInvalidDemand
	}
	if err := validate.ValidateID(leaseID, "lease_"); err != nil {
		return demandState{}, fmt.Errorf("%w: lease id: %v", ErrInvalidDemand, err)
	}
	return demandState{
		generation: message.GetDemandGeneration(),
		leaseID:    leaseID,
		desired:    desired,
		remaining:  uint64(maximum),
		deadline:   now.Add(time.Duration(ttlMS) * time.Millisecond),
	}, nil
}

func closeTransport(entry *workEntry) error {
	entry.connectionMu.Lock()
	connection := entry.connection
	entry.connectionMu.Unlock()
	if connection == nil {
		// Dial 尚未返回时只取消 Context；不能提前消耗 closeOnce，否则迟到连接无法关闭。
		return nil
	}
	entry.closeOnce.Do(func() {
		deadlineErr := connection.SetDeadline(time.Now())
		closeErr := connection.Close()
		entry.closeErr = errors.Join(
			wrapCloseError("set deadline", deadlineErr),
			wrapCloseError("close connection", closeErr),
		)
	})
	return entry.closeErr
}

func closeConnection(connection net.Conn) error {
	deadlineErr := connection.SetDeadline(time.Now())
	closeErr := connection.Close()
	return errors.Join(
		wrapCloseError("set deadline", deadlineErr),
		wrapCloseError("close connection", closeErr),
	)
}

func wrapCloseError(operation string, err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("WorkConn shutdown %s: %w", operation, err)
}

func validHandler(handler Handler) bool {
	if handler == nil {
		return false
	}
	if function, ok := handler.(HandlerFunc); ok {
		return function != nil
	}
	return true
}

func (pool *Pool) handlerObservesPhases() bool {
	_, ok := pool.config.Handler.(phaseObservingHandler)
	return ok
}
