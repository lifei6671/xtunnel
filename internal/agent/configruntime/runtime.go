// Package configruntime 管理 Agent 进程内唯一的已发布配置及每代 Control Session 的观测基线。
package configruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	protocolvalidate "github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/safego"
)

const (
	// MaxServicesPerTunnel 是 Agent 二进制接受的 V0.1 单 Tunnel Service 绝对上限。
	MaxServicesPerTunnel = 1000
	// MaxSnapshotSize 是 Protocol v1 完整 TunnelSnapshot 的绝对业务上限。
	MaxSnapshotSize = 768 << 10
)

var (
	ErrInvalidConfig     = errors.New("agent config runtime config is invalid")
	ErrInvalidSession    = errors.New("agent config runtime session is invalid")
	ErrClosed            = errors.New("agent config runtime is closed")
	ErrConcurrentApply   = errors.New("agent config runtime apply is already running")
	ErrProtocolViolation = errors.New("agent config snapshot violates protocol")
	// ErrConfigRejected 表示 REJECTED Ack 已成功入队，调用方必须保留当前 Control
	// Session 并等待后续 Snapshot。Ack 入队失败只返回 ErrAckEnqueue，不返回本错误。
	ErrConfigRejected   = errors.New("agent config snapshot was rejected")
	ErrCandidateCleanup = errors.New("agent config candidate cleanup failed")
	ErrRetire           = errors.New("agent config resources retire failed")
	ErrAckEnqueue       = errors.New("agent config ack enqueue failed")
)

// Gate 是 Candidate 后台资源唯一允许使用的发布门。
// 只有 Candidate 已成为当前 Runtime 且 APPLIED Ack 已成功入队时才返回 true。
type Gate interface {
	Active() bool
}

// Resources 是一代已发布配置拥有的后台资源。它不得拥有或关闭 WorkPool、Origin
// Socket 或已经进入 ACTIVE 的 WorkConn。
type Resources interface {
	Retire(context.Context) error
}

// Candidate 是尚未发布的完整配置。Abort 只用于未发布 Candidate 的清理；Runtime
// 返回的 Resources 在发布后由 Manager 独占回收。Runtime 必须无阻塞、无失败且只
// 返回已经由 Start 完整准备好的不可变资源句柄。
type Candidate interface {
	Start(context.Context) error
	Abort(context.Context) error
	Runtime() Resources
}

// Builder 从已复制、已排序且完成通用 Wire 校验的 Snapshot 构建 Candidate。
// 更细的 Origin 与 Health 语义由实现方校验。Build 失败时若存在待清理资源，必须
// 返回非 nil Candidate 交给 Manager Abort；否则 Builder 必须在返回前自行清理。
type Builder interface {
	Build(context.Context, *protocolv1.TunnelSnapshot, Gate) (Candidate, error)
}

// AckEnqueuer 接受已经完整构造的 ConfigAck Envelope。
type AckEnqueuer interface {
	Enqueue(*protocolv1.ControlEnvelope) error
}

// Config 固定原子 Apply 的 Protocol v1 边界、资源回收窗口与 Candidate Builder。
type Config struct {
	ProtocolVersion      uint32
	MaxServices          int
	MaxSnapshotBytes     int
	MaxControlFrameBytes int
	RetireTimeout        time.Duration
	Builder              Builder
}

// View 是一次 atomic Load 得到的完整只读元组。Snapshot 是独立副本。
type View struct {
	Snapshot *protocolv1.TunnelSnapshot
	Revision uint64
	Digest   [sha256.Size]byte
	Acked    bool
}

type state struct {
	snapshot  *protocolv1.TunnelSnapshot
	revision  uint64
	digest    [sha256.Size]byte
	resources Resources
	cancel    context.CancelFunc
	acked     atomic.Bool
}

type observed struct {
	revision uint64
	digest   [sha256.Size]byte
}

type publicationGate struct {
	manager *Manager
	target  atomic.Pointer[state]
}

func (gate *publicationGate) Active() bool {
	if gate == nil || gate.manager == nil {
		return false
	}
	target := gate.target.Load()
	return target != nil && gate.manager.current.Load() == target && target.acked.Load()
}

// Manager 持有进程级 current；不同 Control Session 只在 Session 中维护各自 observed。
type Manager struct {
	config      Config
	current     atomic.Pointer[state]
	ownerCtx    context.Context
	ownerCancel context.CancelFunc

	lifecycleMu sync.Mutex
	closed      bool
	applying    atomic.Bool
	applyCancel context.CancelFunc
	applyWG     sync.WaitGroup
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error

	statesMu  sync.Mutex
	pending   map[*state]struct{}
	retiring  map[*state]context.CancelFunc
	retireWG  sync.WaitGroup
	retireErr error
}

// Session 表示一代已经认证的 Control Session。新建 Session 时 observed 基线始终为空。
type Session struct {
	manager          *Manager
	expectedTunnelID string
	observed         atomic.Pointer[observed]
}

// New 创建进程级配置 Manager；parent 取消会终止 Candidate 与全部受控 Retire。
func New(parent context.Context, config Config) (*Manager, error) {
	if parent == nil || config.ProtocolVersion != 1 || config.MaxServices < 1 || config.MaxServices > MaxServicesPerTunnel || config.MaxSnapshotBytes < 1 ||
		config.MaxSnapshotBytes > MaxSnapshotSize || config.MaxControlFrameBytes < 1 ||
		uint64(config.MaxControlFrameBytes) > frame.MaxControlFrameSize || config.RetireTimeout <= 0 ||
		isNilInterface(config.Builder) {
		return nil, ErrInvalidConfig
	}
	ownerContext, cancelOwner := context.WithCancel(parent)
	return &Manager{
		config:      config,
		ownerCtx:    ownerContext,
		ownerCancel: cancelOwner,
		closeDone:   make(chan struct{}),
		pending:     make(map[*state]struct{}),
		retiring:    make(map[*state]context.CancelFunc),
	}, nil
}

func (manager *Manager) NewSession(expectedTunnelID string) (*Session, error) {
	if manager == nil || !identity.ValidTunnelID(expectedTunnelID) {
		return nil, ErrInvalidSession
	}
	manager.lifecycleMu.Lock()
	defer manager.lifecycleMu.Unlock()
	if manager.closed || manager.ownerCtx.Err() != nil {
		return nil, ErrClosed
	}
	return &Session{manager: manager, expectedTunnelID: expectedTunnelID}, nil
}

// Current 返回进程当前发布状态的一致副本。
func (manager *Manager) Current() (View, bool) {
	if manager == nil {
		return View{}, false
	}
	return viewOf(manager.current.Load())
}

// Observed 返回当前 Session 最后一个成功入队 APPLIED Ack 对应的 Revision 与 Digest。
func (session *Session) Observed() (uint64, [sha256.Size]byte, bool) {
	if session == nil {
		return 0, [sha256.Size]byte{}, false
	}
	value := session.observed.Load()
	if value == nil {
		return 0, [sha256.Size]byte{}, false
	}
	return value.revision, value.digest, true
}

// Apply 串行构建并发布一个完整 Snapshot。并发调用不会排队，而是快速失败。
func (session *Session) Apply(ctx context.Context, snapshot *protocolv1.TunnelSnapshot, sink AckEnqueuer) error {
	if session == nil || session.manager == nil || ctx == nil || isNilInterface(sink) {
		return ErrInvalidSession
	}
	manager := session.manager
	applyContext, finish, err := manager.beginApply(ctx)
	if err != nil {
		return err
	}
	defer finish()

	owned, digest, err := manager.validateAndOwn(session.expectedTunnelID, snapshot)
	if err != nil {
		if errors.Is(err, protocolvalidate.ErrUnknownFields) {
			return fmt.Errorf("%w: %w", ErrProtocolViolation, err)
		}
		return session.reject(sink, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, err)
	}

	gate := &publicationGate{manager: manager}
	// Builder 取得独立副本。即使后续实现为校验或构建缓存而改写、保留输入，
	// Manager 持有的权威 Snapshot、Revision 与 Digest 也不能被别名破坏。
	buildInput := proto.Clone(owned).(*protocolv1.TunnelSnapshot)
	candidate, buildErr := manager.config.Builder.Build(applyContext, buildInput, gate)
	if buildErr != nil {
		if abortErr := manager.abortCandidate(ctx, candidate); abortErr != nil {
			return errors.Join(buildErr, abortErr)
		}
		return session.reject(sink, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, buildErr)
	}
	if isNilInterface(candidate) {
		return session.reject(sink, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, ErrInvalidConfig)
	}
	// Candidate 后台资源必须继承 Manager 生命周期，而不是随单次 Apply 返回即取消。
	// Apply 取消只在发布前通过 bridge 终止 Candidate；成功提交前会解除该联动。
	candidateContext, cancelCandidate := context.WithCancel(manager.ownerCtx)
	stopApplyCancellation := context.AfterFunc(applyContext, cancelCandidate)
	if startErr := candidate.Start(candidateContext); startErr != nil {
		stopApplyCancellation()
		cancelCandidate()
		if abortErr := manager.abortCandidate(ctx, candidate); abortErr != nil {
			return errors.Join(startErr, abortErr)
		}
		return session.reject(sink, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, startErr)
	}
	// Start 可能与进程关停或 Session 取消同时完成。发布前必须再次观察取消，
	// 否则一个已经失去 Owner 的 Candidate 会越过最后提交边界。
	if err := applyContext.Err(); err != nil {
		stopApplyCancellation()
		cancelCandidate()
		if abortErr := manager.abortCandidate(ctx, candidate); abortErr != nil {
			return errors.Join(err, abortErr)
		}
		return err
	}
	resources := candidate.Runtime()
	if isNilInterface(resources) {
		stopApplyCancellation()
		cancelCandidate()
		if abortErr := manager.abortCandidate(ctx, candidate); abortErr != nil {
			return errors.Join(ErrInvalidConfig, abortErr)
		}
		return session.reject(sink, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, ErrInvalidConfig)
	}
	// Candidate 已经完整启动并给出不可变 Runtime；从此解除单次 Apply 取消联动，
	// 已发布资源只由 Manager owner、Retire 或 Close 结束。
	stopApplyCancellation()

	next := &state{
		snapshot: owned, revision: owned.GetRevision(), digest: digest,
		resources: resources, cancel: cancelCandidate,
	}
	// Close 与 Publish 共用这个极短提交栅栏。锁内只检查内存状态并执行原子交换，
	// 不调用 Candidate、Ack、Retire 或任何外部 IO。
	manager.lifecycleMu.Lock()
	commitErr := applyContext.Err()
	if commitErr == nil && (manager.closed || manager.ownerCtx.Err() != nil) {
		commitErr = ErrClosed
	}
	var previous *state
	if commitErr == nil {
		gate.target.Store(next)
		previous = manager.current.Swap(next)
	}
	manager.lifecycleMu.Unlock()
	if commitErr != nil {
		cancelCandidate()
		if abortErr := manager.abortCandidate(ctx, candidate); abortErr != nil {
			return errors.Join(commitErr, abortErr)
		}
		return commitErr
	}
	if previous != nil {
		manager.statesMu.Lock()
		manager.pending[previous] = struct{}{}
		manager.statesMu.Unlock()
	}

	ack := configAck(manager.config.ProtocolVersion, next.revision,
		protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED, protocolv1.ErrorCode_ERROR_CODE_OK)
	if err := sink.Enqueue(ack); err != nil {
		return fmt.Errorf("%w: %w", ErrAckEnqueue, err)
	}
	session.observed.Store(&observed{revision: next.revision, digest: next.digest})
	next.acked.Store(true)
	manager.retirePending()
	return nil
}

func (manager *Manager) beginApply(ctx context.Context) (context.Context, func(), error) {
	manager.lifecycleMu.Lock()
	defer manager.lifecycleMu.Unlock()
	if manager.closed || manager.ownerCtx.Err() != nil {
		return nil, nil, ErrClosed
	}
	if !manager.applying.CompareAndSwap(false, true) {
		return nil, nil, ErrConcurrentApply
	}
	applyContext, cancel := context.WithCancel(ctx)
	stopOwnerCancellation := context.AfterFunc(manager.ownerCtx, cancel)
	manager.applyCancel = cancel
	manager.applyWG.Add(1)
	return applyContext, func() {
		stopOwnerCancellation()
		cancel()
		manager.lifecycleMu.Lock()
		manager.applyCancel = nil
		manager.applying.Store(false)
		manager.lifecycleMu.Unlock()
		manager.applyWG.Done()
	}, nil
}

func (manager *Manager) validateAndOwn(expectedTunnelID string, snapshot *protocolv1.TunnelSnapshot) (*protocolv1.TunnelSnapshot, [sha256.Size]byte, error) {
	if snapshot == nil {
		return nil, [sha256.Size]byte{}, ErrProtocolViolation
	}
	if err := protocolvalidate.RejectUnknownFields(snapshot); err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	owned := proto.Clone(snapshot).(*protocolv1.TunnelSnapshot)
	sort.SliceStable(owned.Services, func(left, right int) bool {
		if owned.Services[left] == nil {
			return owned.Services[right] != nil
		}
		if owned.Services[right] == nil {
			return false
		}
		return owned.Services[left].GetServiceId() < owned.Services[right].GetServiceId()
	})
	if !identity.ValidTunnelID(owned.GetTunnelId()) || owned.GetTunnelId() != expectedTunnelID {
		return nil, [sha256.Size]byte{}, ErrProtocolViolation
	}
	if len(owned.Services) > manager.config.MaxServices {
		return nil, [sha256.Size]byte{}, fmt.Errorf("%w: service count=%d limit=%d", ErrProtocolViolation, len(owned.Services), manager.config.MaxServices)
	}
	seen := make(map[string]struct{}, len(owned.Services))
	for index, service := range owned.Services {
		if service == nil || !identity.ValidServiceID(service.GetServiceId()) || service.GetRequiredRevision() > owned.GetRevision() {
			return nil, [sha256.Size]byte{}, fmt.Errorf("%w: invalid service at index %d", ErrProtocolViolation, index)
		}
		if _, exists := seen[service.GetServiceId()]; exists {
			return nil, [sha256.Size]byte{}, fmt.Errorf("%w: duplicate service at index %d", ErrProtocolViolation, index)
		}
		seen[service.GetServiceId()] = struct{}{}
	}
	encoded, err := deterministic.MarshalSnapshot(owned)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("%w: deterministic snapshot: %w", ErrProtocolViolation, err)
	}
	if len(encoded) > manager.config.MaxSnapshotBytes {
		return nil, [sha256.Size]byte{}, fmt.Errorf("%w: snapshot bytes=%d limit=%d", ErrProtocolViolation, len(encoded), manager.config.MaxSnapshotBytes)
	}
	envelope := &protocolv1.ControlEnvelope{
		ProtocolVersion: manager.config.ProtocolVersion,
		Payload:         &protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: owned},
	}
	envelopeBytes, err := deterministic.Marshal(envelope)
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("%w: deterministic envelope: %w", ErrProtocolViolation, err)
	}
	if len(envelopeBytes) > manager.config.MaxControlFrameBytes {
		return nil, [sha256.Size]byte{}, fmt.Errorf("%w: envelope bytes=%d limit=%d", ErrProtocolViolation, len(envelopeBytes), manager.config.MaxControlFrameBytes)
	}
	return owned, sha256.Sum256(encoded), nil
}

func (session *Session) reject(sink AckEnqueuer, code protocolv1.ErrorCode, cause error) error {
	revision, _, observedOK := session.Observed()
	if !observedOK {
		revision = 0
	}
	ack := configAck(session.manager.config.ProtocolVersion, revision,
		protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED, code)
	rejected := fmt.Errorf("%w: %w", ErrConfigRejected, cause)
	if err := sink.Enqueue(ack); err != nil {
		return errors.Join(cause, fmt.Errorf("%w: %w", ErrAckEnqueue, err))
	}
	return rejected
}

func (manager *Manager) abortCandidate(parent context.Context, candidate Candidate) error {
	if isNilInterface(candidate) {
		return nil
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(parent), manager.config.RetireTimeout)
	defer cancel()
	if err := candidate.Abort(cleanupContext); err != nil {
		return fmt.Errorf("%w: %w", ErrCandidateCleanup, err)
	}
	return nil
}

func (manager *Manager) retirePending() {
	manager.statesMu.Lock()
	states := make([]*state, 0, len(manager.pending))
	for old := range manager.pending {
		delete(manager.pending, old)
		states = append(states, old)
	}
	manager.statesMu.Unlock()
	manager.scheduleRetire(states)
}

func (manager *Manager) scheduleRetire(states []*state) {
	for _, old := range states {
		if old == nil || isNilInterface(old.resources) {
			continue
		}
		manager.statesMu.Lock()
		if _, exists := manager.retiring[old]; exists {
			manager.statesMu.Unlock()
			continue
		}
		retireContext, cancel := context.WithTimeout(manager.ownerCtx, manager.config.RetireTimeout)
		manager.retiring[old] = cancel
		manager.retireWG.Add(1)
		manager.statesMu.Unlock()

		safego.Go(func(err error) {
			manager.finishRetire(old, cancel, err)
		}, manager.retireWG.Done, func() {
			old.cancel()
			err := old.resources.Retire(retireContext)
			manager.finishRetire(old, cancel, err)
		})
	}
}

func (manager *Manager) finishRetire(retiring *state, cancel context.CancelFunc, err error) {
	cancel()
	manager.statesMu.Lock()
	defer manager.statesMu.Unlock()
	delete(manager.retiring, retiring)
	if err != nil {
		manager.retireErr = errors.Join(manager.retireErr, fmt.Errorf("%w: %w", ErrRetire, err))
	}
}

// Close 阻止新 Apply，取消正在构建的 Candidate，并有界回收 current、pending 与 retiring。
// 即使 ctx 先到期，Close 也会等待所有受控清理 goroutine 退出，再合并返回 Deadline。
func (manager *Manager) Close(ctx context.Context) error {
	if manager == nil || ctx == nil {
		return ErrInvalidConfig
	}
	manager.closeOnce.Do(func() {
		manager.lifecycleMu.Lock()
		manager.closed = true
		cancelApply := manager.applyCancel
		manager.lifecycleMu.Unlock()
		if cancelApply != nil {
			cancelApply()
		}
		manager.ownerCancel()
		safego.Go(manager.recordClosePanic, func() { close(manager.closeDone) }, manager.finishClose)
	})
	select {
	case <-manager.closeDone:
		return manager.closeErr
	case <-ctx.Done():
		// Close 是 Manager 的最终 Owner，不能把尚未退出的清理 goroutine 留给调用方。
		// ownerCancel 已解除全部受控等待；继续等到资源确认退出，再合并调用方 Deadline。
		<-manager.closeDone
		return errors.Join(ctx.Err(), manager.closeErr)
	}
}

func (manager *Manager) finishClose() {
	manager.applyWG.Wait()

	manager.statesMu.Lock()
	states := make([]*state, 0, len(manager.pending)+1)
	for old := range manager.pending {
		delete(manager.pending, old)
		states = append(states, old)
	}
	if current := manager.current.Swap(nil); current != nil {
		states = append(states, current)
	}
	cancels := make([]context.CancelFunc, 0, len(manager.retiring))
	for _, cancel := range manager.retiring {
		cancels = append(cancels, cancel)
	}
	manager.statesMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	manager.scheduleRetire(states)
	manager.retireWG.Wait()

	manager.statesMu.Lock()
	manager.closeErr = manager.retireErr
	manager.statesMu.Unlock()
}

func (manager *Manager) recordClosePanic(err error) {
	manager.statesMu.Lock()
	defer manager.statesMu.Unlock()
	manager.closeErr = errors.Join(manager.closeErr, manager.retireErr,
		fmt.Errorf("finish Agent config runtime close: %w", err))
}

func configAck(version uint32, revision uint64, status protocolv1.ConfigApplyStatus, code protocolv1.ErrorCode) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: version,
		Payload: &protocolv1.ControlEnvelope_ConfigAck{ConfigAck: &protocolv1.ConfigAck{
			ObservedRevision: revision,
			ApplyStatus:      status,
			ErrorCode:        code,
		}},
	}
}

func viewOf(value *state) (View, bool) {
	if value == nil {
		return View{}, false
	}
	return View{
		Snapshot: proto.Clone(value.snapshot).(*protocolv1.TunnelSnapshot),
		Revision: value.revision,
		Digest:   value.digest,
		Acked:    value.acked.Load(),
	}, true
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
