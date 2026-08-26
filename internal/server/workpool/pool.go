package workpool

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
)

var (
	// ErrInvalidOptions 表示 Session 身份、容量或时钟配置不合法。
	ErrInvalidOptions = errors.New("server work pool options are invalid")
	// ErrInvalidWork 表示 Work ID、连接或 Work 句柄不合法。
	ErrInvalidWork = errors.New("server work is invalid")
	// ErrDuplicateWork 表示同一 Session 当前已经持有相同 Work ID。
	ErrDuplicateWork = errors.New("server work already exists")
	// ErrPoolCapacity 表示该 Session 的 WorkConn 总量已经达到硬上限。
	ErrPoolCapacity = errors.New("server work pool capacity is exhausted")
	// ErrConnectingCapacity 表示该 Session 的 CONNECTING 数已经达到硬上限。
	ErrConnectingCapacity = errors.New("server connecting work capacity is exhausted")
	// ErrInvalidTransition 表示调用方尝试了冻结状态机不允许的转换。
	ErrInvalidTransition = errors.New("server work state transition is invalid")
	// ErrPoolClosed 表示 Session Pool 已停止接收和分配 WorkConn。
	ErrPoolClosed = errors.New("server work pool is closed")
	// ErrPoolDraining 表示 Session 已摘出选择集合，不再接受新 Work 或 Acquire。
	ErrPoolDraining = errors.New("server work pool is draining")
	// ErrAcquireTimeout 表示在给定等待时间内没有可用 IDLE WorkConn。
	ErrAcquireTimeout = errors.New("server idle work acquire timed out")
)

// State 是 Server 观察到的单条 WorkConn 生命周期状态。
type State uint8

const (
	// StateConnecting 表示 TCP/TLS/WorkHello 已开始但尚未完成 WorkReady 成功提交。
	StateConnecting State = iota + 1
	// StateIdle 表示 WorkReady 已成功写出，可被一个公网连接获取。
	StateIdle
	// StateOpening 表示 WorkConn 已被唯一公网连接获取，正在执行 OPEN 握手。
	StateOpening
	// StateActive 表示 OPEN_OK 已提交，连接只能传输 RAW 数据直至关闭。
	StateActive
	// StateClosed 是终态；WorkConn 永远不能从该状态返回 Pool。
	StateClosed
)

// String 返回稳定的状态名称。
func (state State) String() string {
	switch state {
	case StateConnecting:
		return "CONNECTING"
	case StateIdle:
		return "IDLE"
	case StateOpening:
		return "OPENING"
	case StateActive:
		return "ACTIVE"
	case StateClosed:
		return "CLOSED"
	default:
		return "UNKNOWN"
	}
}

// Session 是一个 WorkPool 独占的完整 Session 身份。
type Session struct {
	TunnelID    string
	ConnectorID string
	SessionID   string
	Generation  uint64
}

// Clock 返回同一进程起点以来的单调时长，Demand Lease 禁止使用跨主机 wall clock。
type Clock func() time.Duration

// Options 固定 per-Session Pool 的身份、容量和可测试时钟。
type Options struct {
	Session       Session
	MaxTotal      uint32
	MaxConnecting uint32
	// LimitManager 为 nil 时只应用 per-Session 上限，供纯单元测试使用；生产装配
	// 必须传入同一个进程级 Manager，才能跨 Session 约束真实 WorkConn 总量。
	LimitManager *serverlimits.Manager
	Clock        Clock
	// DeadlineNow 只用于在锁外解除连接 IO；生产环境通常传入 time.Now。
	DeadlineNow func() time.Time
}

// Counts 是某一线性化时刻的 WorkConn 计数快照。
type Counts struct {
	Connecting uint32
	Idle       uint32
	Opening    uint32
	Active     uint32
	Total      uint32
	Closed     bool
	Draining   bool
}

// Pool 线性化一个 Session 的 WorkConn 状态、IDLE 队列和 Demand generation。
//
// mu 临界区内只更新内存；禁止网络 IO、Conn.Close、阻塞等待或跨组件调用。
// changed 使用关闭旧 channel 的方式广播状态变化，不创建后台 goroutine。
type Pool struct {
	mu sync.Mutex

	session       Session
	maxTotal      uint32
	maxConnecting uint32
	limitManager  *serverlimits.Manager
	clock         Clock
	deadlineNow   func() time.Time

	works    map[string]*Work
	idle     list.List
	counts   [StateClosed]uint32
	changed  chan struct{}
	closed   bool
	draining bool

	demand demandState
}

// Work 是 Pool 中一条 WorkConn 的稳定句柄。
//
// 状态由所属 Pool.mu 保护；连接终止由 closeOnce 保护，复制指针或并发 Close 都只会
// 执行一次 SetDeadline 与 Close。
type Work struct {
	pool       *Pool
	id         string
	connection net.Conn
	state      State
	idleEntry  *list.Element
	done       chan struct{}
	protocol   *state.Work
	limitLease *serverlimits.WorkLease

	closeOnce sync.Once
	closeErr  error
}

// New 创建一个空的 per-Session WorkPool。
func New(options Options) (*Pool, error) {
	if identity.ValidateTunnelID(options.Session.TunnelID) != nil ||
		identity.ValidateConnectorID(options.Session.ConnectorID) != nil ||
		identity.ValidateSessionID(options.Session.SessionID) != nil || options.Session.Generation == 0 ||
		options.MaxTotal == 0 || options.MaxConnecting == 0 || options.MaxConnecting > options.MaxTotal ||
		options.Clock == nil || options.DeadlineNow == nil {
		return nil, ErrInvalidOptions
	}
	return &Pool{
		session: options.Session, maxTotal: options.MaxTotal, maxConnecting: options.MaxConnecting,
		clock: options.Clock, deadlineNow: options.DeadlineNow,
		limitManager: options.LimitManager,
		works:        make(map[string]*Work), changed: make(chan struct{}),
	}, nil
}

// Session 返回该 Pool 的不可变 Session 身份。
func (pool *Pool) Session() Session {
	if pool == nil {
		return Session{}
	}
	return pool.session
}

// RegisterConnecting 在线性化点预留总容量与 CONNECTING 容量，并接管连接。
//
// 失败时连接所有权仍属于调用方，本方法不会关闭它。Work ID 只允许在当前 Pool 的
// 非 CLOSED 条目中出现一次；协议层的跨 Lease Replay 仍由 WorkHello Authenticator
// 负责，避免在本包保留无界历史集合。
func (pool *Pool) RegisterConnecting(workID string, connection net.Conn) (*Work, error) {
	if pool == nil || validate.ValidateID(workID, "work_") != nil || connection == nil {
		return nil, ErrInvalidWork
	}
	var limitLease *serverlimits.WorkLease
	if pool.limitManager != nil {
		var err error
		limitLease, err = pool.limitManager.AcquireWork()
		if err != nil {
			return nil, err
		}
	}
	pool.mu.Lock()
	if pool.closed {
		pool.mu.Unlock()
		limitLease.Release()
		return nil, ErrPoolClosed
	}
	if pool.draining {
		pool.mu.Unlock()
		limitLease.Release()
		return nil, ErrPoolDraining
	}
	if _, exists := pool.works[workID]; exists {
		pool.mu.Unlock()
		limitLease.Release()
		return nil, ErrDuplicateWork
	}
	if pool.totalLocked() >= pool.maxTotal {
		pool.mu.Unlock()
		limitLease.Release()
		return nil, ErrPoolCapacity
	}
	if pool.countLocked(StateConnecting) >= pool.maxConnecting {
		pool.mu.Unlock()
		limitLease.Release()
		return nil, ErrConnectingCapacity
	}
	work := &Work{
		pool: pool, id: workID, connection: connection, state: StateConnecting,
		done: make(chan struct{}), limitLease: limitLease,
	}
	pool.works[workID] = work
	pool.incrementLocked(StateConnecting)
	pool.mu.Unlock()
	return work, nil
}

// ID 返回不可变 Work ID。
func (work *Work) ID() string {
	if work == nil {
		return ""
	}
	return work.id
}

// Conn 返回该 Work 持有的连接；调用方不得绕过 Work.Close 转移或关闭所有权。
func (work *Work) Conn() net.Conn {
	if work == nil {
		return nil
	}
	return work.connection
}

// Done 在 Work 从 Pool 线性化移除且底层 Deadline/Close 完成后关闭。
// Gateway 连接处理协程使用它保持连接所有权不提前归还。
func (work *Work) Done() <-chan struct{} {
	if work == nil || work.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return work.done
}

// AttachProtocolState 在 CONNECTING 阶段把 WorkHello/WorkReady 状态所有权绑定到
// Work。只允许绑定一次且必须已经处于 WorkIdle；失败不会改变原有状态。
func (work *Work) AttachProtocolState(protocol *state.Work) error {
	if work == nil || work.pool == nil || protocol == nil || protocol.Phase() != state.WorkIdle {
		return ErrInvalidWork
	}
	pool := work.pool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.works[work.id] != work || work.state != StateConnecting || work.protocol != nil {
		return ErrInvalidTransition
	}
	work.protocol = protocol
	return nil
}

// ProtocolState 返回 OPEN Handler 在 IDLE→OPENING→ACTIVE 中独占使用的协议状态。
func (work *Work) ProtocolState() *state.Work {
	if work == nil || work.pool == nil {
		return nil
	}
	work.pool.mu.Lock()
	defer work.pool.mu.Unlock()
	return work.protocol
}

// State 返回当前状态快照。
func (work *Work) State() State {
	if work == nil || work.pool == nil {
		return StateClosed
	}
	work.pool.mu.Lock()
	defer work.pool.mu.Unlock()
	return work.state
}

// MarkIdle 在 WorkReady READY Frame 完整写出后提交 CONNECTING → IDLE。
func (work *Work) MarkIdle() error {
	if work == nil || work.pool == nil {
		return ErrInvalidWork
	}
	pool := work.pool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.works[work.id] != work || work.state != StateConnecting {
		return ErrInvalidTransition
	}
	// 固定锁序是 Pool.mu -> LimitManager.mu。Manager 不会反向进入 Pool，且这里只
	// 更新内存计数，因此全局 IDLE 校验与本地状态提交不会被并发 Close 拆开。
	if work.limitLease != nil {
		if err := work.limitLease.MarkIdle(); err != nil {
			return err
		}
	}
	pool.decrementLocked(StateConnecting)
	pool.incrementLocked(StateIdle)
	work.state = StateIdle
	work.idleEntry = pool.idle.PushBack(work)
	pool.signalLocked()
	return nil
}

// Acquire 等待一个 IDLE WorkConn，并原子提交 IDLE → OPENING 且移出 IDLE 队列。
// timeout 是本次等待的硬上限；调用方 Context 更早取消时直接返回其错误。
func (pool *Pool) Acquire(ctx context.Context, timeout time.Duration) (*Work, error) {
	if pool == nil || ctx == nil || timeout <= 0 {
		return nil, ErrInvalidOptions
	}
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := waitContext.Err(); err != nil {
			return nil, acquireContextError(ctx, err)
		}
		pool.mu.Lock()
		if pool.closed {
			pool.mu.Unlock()
			return nil, ErrPoolClosed
		}
		if pool.draining {
			pool.mu.Unlock()
			return nil, ErrPoolDraining
		}
		if pool.idle.Front() != nil {
			work, err := pool.acquireIdleLocked()
			pool.mu.Unlock()
			return work, err
		}
		changed := pool.changed
		pool.mu.Unlock()

		select {
		case <-changed:
		case <-waitContext.Done():
			return nil, acquireContextError(ctx, waitContext.Err())
		}
	}
}

// TryAcquire 原子尝试取得当前已有的 IDLE WorkConn，但绝不等待补池。
// ok=false 表示该线性化时刻没有 IDLE；Pool 关闭或排空仍返回对应错误。
func (pool *Pool) TryAcquire() (work *Work, ok bool, err error) {
	if pool == nil {
		return nil, false, ErrInvalidOptions
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed {
		return nil, false, ErrPoolClosed
	}
	if pool.draining {
		return nil, false, ErrPoolDraining
	}
	if pool.idle.Front() == nil {
		return nil, false, nil
	}
	work, err = pool.acquireIdleLocked()
	return work, err == nil, err
}

func (pool *Pool) acquireIdleLocked() (*Work, error) {
	front := pool.idle.Front()
	work := front.Value.(*Work)
	if work.limitLease != nil {
		if err := work.limitLease.MarkOpening(); err != nil {
			return nil, err
		}
	}
	pool.idle.Remove(front)
	work.idleEntry = nil
	pool.decrementLocked(StateIdle)
	pool.incrementLocked(StateOpening)
	work.state = StateOpening
	return work, nil
}

// MarkActive 在 OPEN_OK Frame 完整提交后执行 OPENING → ACTIVE。
func (work *Work) MarkActive() error {
	if work == nil || work.pool == nil {
		return ErrInvalidWork
	}
	pool := work.pool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.works[work.id] != work || work.state != StateOpening {
		return ErrInvalidTransition
	}
	if work.limitLease != nil {
		if err := work.limitLease.MarkActive(); err != nil {
			return err
		}
	}
	pool.decrementLocked(StateOpening)
	pool.incrementLocked(StateActive)
	work.state = StateActive
	pool.signalLocked()
	return nil
}

// BeginDrain 原子停止新 Work 注册和新 IDLE Acquire；重复调用幂等。
func (pool *Pool) BeginDrain() error {
	if pool == nil {
		return ErrInvalidOptions
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed {
		return ErrPoolClosed
	}
	if !pool.draining {
		pool.draining = true
		pool.signalLocked()
	}
	return nil
}

// WaitOpeningAndCloseNonActive 等待已 Acquire 的 OPENING 自然进入 ACTIVE 或失败；
// deadline 到达时强制关闭仍未进入 RAW 的 OPENING。随后关闭 CONNECTING/IDLE，
// 返回 Ack 应携带的当前 ACTIVE 数。所有网络 IO 均在 Pool 锁外执行。
func (pool *Pool) WaitOpeningAndCloseNonActive(ctx context.Context) (uint32, error) {
	if pool == nil || ctx == nil {
		return 0, ErrInvalidOptions
	}
	if err := pool.BeginDrain(); err != nil && !errors.Is(err, ErrPoolDraining) {
		return 0, err
	}
	for {
		pool.mu.Lock()
		if pool.countLocked(StateOpening) == 0 || ctx.Err() != nil {
			works := make([]*Work, 0, len(pool.works))
			for _, work := range pool.works {
				if work.state != StateActive && pool.detachLocked(work) {
					works = append(works, work)
				}
			}
			active := pool.countLocked(StateActive)
			pool.mu.Unlock()
			var closeErrors []error
			for _, work := range works {
				if err := work.closeOutsideLock(); err != nil {
					closeErrors = append(closeErrors, fmt.Errorf("close draining work %s: %w", work.id, err))
				}
			}
			// 超时是受控的强制收束，不阻止 Server 发送 DrainAck；连接关闭错误仍必须传播。
			return active, errors.Join(closeErrors...)
		}
		changed := pool.changed
		pool.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
	}
}

// Close 从任意非终态线性化移除 Work，然后在 Pool 锁外解除并关闭连接。
// OPEN 失败路径直接调用 Close；冻结状态机不允许 OPENING 返回 IDLE。
func (work *Work) Close() error {
	if work == nil || work.pool == nil {
		return ErrInvalidWork
	}
	work.pool.detach(work)
	return work.closeOutsideLock()
}

// Snapshot 返回当前状态计数；Total 恒等于四个非终态计数之和。
func (pool *Pool) Snapshot() Counts {
	if pool == nil {
		return Counts{Closed: true}
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.snapshotLocked()
}

// CloseNonActive 停止该 Session 接收新 Work，并关闭 CONNECTING/IDLE/OPENING。
// 已经 ACTIVE 的 Work 保留，供旧 generation 自然结束；所有连接都在锁外关闭。
func (pool *Pool) CloseNonActive() error {
	return pool.closeMatching(func(state State) bool { return state != StateActive })
}

// Close 停止 Pool 并关闭包括 ACTIVE 在内的全部 WorkConn。
func (pool *Pool) Close() error {
	return pool.closeMatching(func(State) bool { return true })
}

func (pool *Pool) closeMatching(match func(State) bool) error {
	if pool == nil {
		return nil
	}
	pool.mu.Lock()
	pool.closed = true
	works := make([]*Work, 0, len(pool.works))
	for _, work := range pool.works {
		if match(work.state) && pool.detachLocked(work) {
			works = append(works, work)
		}
	}
	pool.signalLocked()
	pool.mu.Unlock()

	closeErrors := make([]error, 0, len(works))
	for _, work := range works {
		if err := work.closeOutsideLock(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close work %s: %w", work.id, err))
		}
	}
	return errors.Join(closeErrors...)
}

func (pool *Pool) detach(work *Work) bool {
	pool.mu.Lock()
	detached := pool.detachLocked(work)
	pool.mu.Unlock()
	return detached
}

func (pool *Pool) detachLocked(work *Work) bool {
	if pool.works[work.id] != work || work.state == StateClosed {
		return false
	}
	if work.idleEntry != nil {
		pool.idle.Remove(work.idleEntry)
		work.idleEntry = nil
	}
	pool.decrementLocked(work.state)
	delete(pool.works, work.id)
	work.state = StateClosed
	// Release 是纯内存、幂等操作，并继续遵守 Pool.mu -> LimitManager.mu 固定锁序。
	// 先在线性化点归还预算，网络 Deadline/Close 仍在 Pool 锁外执行。
	work.limitLease.Release()
	pool.signalLocked()
	return true
}

func (work *Work) closeOutsideLock() error {
	work.closeOnce.Do(func() {
		defer close(work.done)
		deadlineErr := work.connection.SetDeadline(work.pool.deadlineNow())
		closeErr := work.connection.Close()
		work.closeErr = errors.Join(
			wrapConnectionError("set deadline", deadlineErr),
			wrapConnectionError("close", closeErr),
		)
	})
	return work.closeErr
}

func (pool *Pool) countLocked(state State) uint32 {
	return pool.counts[state-1]
}

func (pool *Pool) incrementLocked(state State) {
	pool.counts[state-1]++
}

func (pool *Pool) decrementLocked(state State) {
	// 所有调用点都先验证 Map 所有权与当前状态；若这里为零就是内部不变量破坏，
	// 因此不做静默下溢或自动修复。
	if pool.counts[state-1] == 0 {
		panic("server work pool counter invariant violated")
	}
	pool.counts[state-1]--
}

func (pool *Pool) totalLocked() uint32 {
	return pool.countLocked(StateConnecting) + pool.countLocked(StateIdle) +
		pool.countLocked(StateOpening) + pool.countLocked(StateActive)
}

func (pool *Pool) nonActiveLocked() uint32 {
	return pool.countLocked(StateConnecting) + pool.countLocked(StateIdle) + pool.countLocked(StateOpening)
}

func (pool *Pool) snapshotLocked() Counts {
	return Counts{
		Connecting: pool.countLocked(StateConnecting), Idle: pool.countLocked(StateIdle),
		Opening: pool.countLocked(StateOpening), Active: pool.countLocked(StateActive),
		Total: pool.totalLocked(), Closed: pool.closed,
		Draining: pool.draining,
	}
}

func (pool *Pool) signalLocked() {
	close(pool.changed)
	pool.changed = make(chan struct{})
}

func acquireContextError(parent context.Context, waitErr error) error {
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if errors.Is(waitErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrAcquireTimeout, waitErr)
	}
	return waitErr
}

func wrapConnectionError(operation string, err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("%s WorkConn: %w", operation, err)
}
