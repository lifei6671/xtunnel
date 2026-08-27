// Package health 实现 Agent 进程级唯一的 Origin 健康检查调度器。
package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/originconfig"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

const (
	maxConcurrent          = 64
	maxConcurrentPerOrigin = 4
	maxChecksPerSecond     = 50
	gatePollInterval       = 50 * time.Millisecond
	rateInterval           = time.Second / maxChecksPerSecond

	minimumIntervalMS uint32 = 1_000
	maximumIntervalMS uint32 = 3_600_000
	minimumTimeoutMS  uint32 = 100
	minimumThreshold  uint32 = 1
	maximumThreshold  uint32 = 20
	minimumHTTPStatus uint32 = 100
	maximumHTTPStatus uint32 = 599
)

var (
	// ErrInvalidConfig 表示 Manager、Snapshot 或 Health Policy 不符合内部契约。
	ErrInvalidConfig = errors.New("agent health configuration is invalid")
	// ErrNotStarted 表示调用依赖 Owner 的操作前尚未启动 Manager。
	ErrNotStarted = errors.New("agent health manager is not started")
	// ErrClosed 表示 Manager 已进入最终关闭状态。
	ErrClosed = errors.New("agent health manager is closed")
	// ErrScheduler 表示中心 Owner 的内部不变量被破坏。
	ErrScheduler = errors.New("agent health scheduler failed")
)

// OriginDialer 使用指定 Snapshot Candidate 的不可变 Origin 连接目标 Service。
type OriginDialer interface {
	DialOrigin(context.Context, string) (net.Conn, protocolv1.ErrorCode, error)
}

// FailureKind 是进程内 Health 失败分类，不定义新的 Wire 字符串契约。
type FailureKind uint8

const (
	// FailureNone 表示最近一次检查成功，或尚无失败分类。
	FailureNone FailureKind = iota
	// FailureOrigin 表示 DNS、连接、TLS 或 Socket IO 失败。
	FailureOrigin
	// FailureHTTPStatus 表示 HTTP 响应状态码不在 Snapshot 期望范围内。
	FailureHTTPStatus
	// FailureHTTPProtocol 表示 HTTP 请求或响应无法按协议完成。
	FailureHTTPProtocol
	// FailureBudget 表示固定 Rate/Concurrency 预算无法在截止时间前调度检查。
	FailureBudget
)

// State 是单个 Service 在当前已激活 Snapshot 下的内存 Health 状态。
type State struct {
	Status          protocolv1.HealthStatus
	ServiceRevision uint64
	CheckedAt       time.Time
	Latency         time.Duration
	OriginErrorCode protocolv1.ErrorCode
	Failure         FailureKind
}

type checkerFunc func(context.Context, targetSpec, OriginDialer) observation

// managerOptions 集中保存调度预算和可替换的时间、随机数、检查器依赖。
// 生产环境使用固定默认值；测试通过注入这些函数稳定地推进时钟和构造检查结果。
type managerOptions struct {
	workers        int
	globalLimit    int
	perOriginLimit int
	rateInterval   time.Duration
	gatePoll       time.Duration
	now            func() time.Time
	random         func() float64
	checker        checkerFunc
}

func defaultOptions() managerOptions {
	return managerOptions{
		workers: maxConcurrent, globalLimit: maxConcurrent, perOriginLimit: maxConcurrentPerOrigin,
		rateInterval: rateInterval, gatePoll: gatePollInterval,
		now: time.Now, random: randomFloat64, checker: checkTarget,
	}
}

// Manager 是一个 Connector 进程唯一的 Health Scheduler Owner。
//
// 调度中的 Plan、Target、并发计数和定时队列只由 Owner goroutine 修改；调用方不能
// 直接触碰这些状态，只能经 commands 发送请求。三个互斥锁分别保护生命周期、首个
// 后台错误和对外只读状态快照，不能把它们误认为调度状态本身的锁。
type Manager struct {
	options managerOptions

	// lifecycleMu 只保护启动/关闭标志和 Owner Context；锁内不等待 goroutine 或 IO。
	lifecycleMu  sync.Mutex
	started      bool
	closed       bool
	ctx          context.Context
	cancel       context.CancelFunc
	shutdownOnce sync.Once
	doneOnce     sync.Once
	done         chan struct{}

	commands chan command     // API 调用方发给唯一 Owner 的串行控制请求。
	results  chan checkResult // Worker 回传给 Owner 的检查结果。
	jobs     chan checkJob    // Owner 派发给固定 Worker Pool 的有界任务队列。

	workerWait sync.WaitGroup

	errMu sync.Mutex
	err   error // 只保留第一个致命后台错误，便于定位真正的失败起点。

	// states 是 Owner 发布的只读副本；State 不会让读者接触调度器的可变 target。
	stateMu      sync.RWMutex
	states       map[string]State
	stateGate    configruntime.Gate
	stateVisible bool
	changed      chan struct{}
}

// New 创建尚未启动的进程级 Health Manager。
func New() *Manager {
	return newManager(defaultOptions())
}

func newManager(options managerOptions) *Manager {
	// 非法的测试注入整体回退到生产默认值，避免生成一组部分有效、语义混杂的配置。
	if options.workers <= 0 || options.globalLimit <= 0 || options.perOriginLimit <= 0 ||
		options.rateInterval < 0 || options.gatePoll <= 0 || options.now == nil ||
		options.random == nil || options.checker == nil {
		options = defaultOptions()
	}
	return &Manager{
		options: options,
		done:    make(chan struct{}), commands: make(chan command, 64),
		results: make(chan checkResult, options.workers),
		jobs:    make(chan checkJob, options.globalLimit), states: make(map[string]State),
		changed: make(chan struct{}, 1),
	}
}

// Start 启动唯一 Owner 和固定 Worker Pool。
// Worker 只执行具体探测，Owner 负责所有调度决策；二次启动属于调用顺序错误。
func (manager *Manager) Start(parent context.Context) error {
	if manager == nil || parent == nil {
		return ErrInvalidConfig
	}
	manager.lifecycleMu.Lock()
	defer manager.lifecycleMu.Unlock()
	if manager.closed {
		return ErrClosed
	}
	if manager.started {
		return ErrInvalidConfig
	}
	manager.ctx, manager.cancel = context.WithCancel(parent)
	manager.started = true

	// WaitGroup 必须在 goroutine 启动前一次性登记，保证 Owner 收尾时可以可靠等待。
	manager.workerWait.Add(manager.options.workers)
	for range manager.options.workers {
		manager.startWorker()
	}
	manager.startOwner()
	return nil
}

// Prepare 校验完整 Snapshot 并构建尚未发布的 gated Health Candidate。
//
// 该阶段只完成“编译 + 与已注册 Plan 做一致性校验”，不会让新配置开始调度。
// ConfigRuntime 后续调用 Candidate.Start 注册 Plan，并以 Gate.Active 决定哪一代可见。
func (manager *Manager) Prepare(
	ctx context.Context,
	snapshot *protocolv1.TunnelSnapshot,
	gate configruntime.Gate,
	scopedDialer OriginDialer,
) (configruntime.Candidate, error) {
	if manager == nil || ctx == nil || snapshot == nil || gate == nil || isNilDialer(scopedDialer) {
		return nil, ErrInvalidConfig
	}
	if err := manager.runningError(); err != nil {
		return nil, err
	}
	compiled, err := compilePlan(snapshot, gate, scopedDialer)
	if err != nil {
		return nil, err
	}
	if err := manager.sendCommand(ctx, command{kind: commandValidate, plan: compiled}); err != nil {
		return nil, err
	}
	return &candidate{manager: manager, plan: compiled}, nil
}

// State 返回当前激活 Plan 中单个 Service 的内存 Health 状态。
// Gate 非 Active 时故意不暴露旧状态，避免配置切代窗口把上一代结果当成当前结果。
func (manager *Manager) State(serviceID string) (State, bool) {
	if manager == nil {
		return State{}, false
	}
	manager.stateMu.RLock()
	defer manager.stateMu.RUnlock()
	if manager.stateGate == nil || !manager.stateGate.Active() {
		return State{}, false
	}
	state, exists := manager.states[serviceID]
	return state, exists
}

// Snapshot 返回当前 Active Plan 的完整 Health 状态副本。
//
// Changed 只负责非阻塞唤醒，调用方收到通知后总是重新读取这里的权威快照。这样即使
// 多次状态变化合并为一个通知，也不会丢失最终状态，且 Reporter 不会反向阻塞 Scheduler。
func (manager *Manager) Snapshot() map[string]State {
	if manager == nil {
		return nil
	}
	manager.stateMu.RLock()
	defer manager.stateMu.RUnlock()
	if manager.stateGate == nil || !manager.stateGate.Active() {
		return nil
	}
	view := make(map[string]State, len(manager.states))
	for serviceID, state := range manager.states {
		view[serviceID] = state
	}
	return view
}

// Changed 返回容量为一的合并通知通道。通知本身不携带状态，也不要求逐条消费；
// Snapshot 才是无丢失读取面。
func (manager *Manager) Changed() <-chan struct{} {
	if manager == nil {
		return nil
	}
	return manager.changed
}

// Done 在 Owner、Worker 与全部受控检查退出后关闭。
func (manager *Manager) Done() <-chan struct{} {
	if manager == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return manager.done
}

// Err 返回 Scheduler 的首个后台失败；正常 Shutdown 返回 nil。
func (manager *Manager) Err() error {
	if manager == nil {
		return ErrInvalidConfig
	}
	manager.errMu.Lock()
	defer manager.errMu.Unlock()
	return manager.err
}

// Shutdown 停止调度、取消全部检查并等待 Owner 与 Worker 退出。
//
// Shutdown 可重复调用：只有第一次调用负责取消 Owner Context。即使调用方的等待
// Context 先到期，也仍会等内部 goroutine 真正退出后再返回，避免把后台资源遗留给上层。
func (manager *Manager) Shutdown(ctx context.Context) error {
	if manager == nil || ctx == nil {
		return ErrInvalidConfig
	}
	manager.lifecycleMu.Lock()
	if !manager.started {
		manager.closed = true
		manager.doneOnce.Do(func() { close(manager.done) })
		manager.lifecycleMu.Unlock()
		return manager.Err()
	}
	manager.closed = true
	cancel := manager.cancel
	manager.lifecycleMu.Unlock()

	manager.shutdownOnce.Do(cancel)
	select {
	case <-manager.done:
		return manager.Err()
	case <-ctx.Done():
		<-manager.done
		return errors.Join(ctx.Err(), manager.Err())
	}
}

func (manager *Manager) runningError() error {
	// 先在生命周期锁内取得一致快照，再在锁外读取 Context 和后台错误，避免把外部
	// 调用或潜在等待放进锁的临界区。
	manager.lifecycleMu.Lock()
	closed := manager.closed
	started := manager.started
	ownerContext := manager.ctx
	manager.lifecycleMu.Unlock()
	if closed {
		return ErrClosed
	}
	if !started {
		return ErrNotStarted
	}
	if ownerContext.Err() != nil {
		return errors.Join(ErrClosed, manager.Err())
	}
	return nil
}

func (manager *Manager) fail(err error) {
	if err == nil {
		return
	}
	manager.errMu.Lock()
	// safego 会把 Owner/Worker 的 panic 转为 error 后调用这里。首错即根因，后续并发
	// 失败不覆盖它；取消共享 Context 会把整个 Owner、Worker 和在途检查统一收敛退出。
	if manager.err == nil {
		manager.err = fmt.Errorf("%w: %w", ErrScheduler, err)
	}
	manager.errMu.Unlock()
	manager.lifecycleMu.Lock()
	cancel := manager.cancel
	manager.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (manager *Manager) sendCommand(ctx context.Context, value command) error {
	// 每个命令携带独立的缓冲响应通道，形成一次请求/一次响应。缓冲容量 1 很重要：
	// 调用方取消后可以立即离开，Owner 随后写回结果也不会被无人接收的响应阻塞。
	response := make(chan error, 1)
	value.response = response
	manager.lifecycleMu.Lock()
	ownerContext := manager.ctx
	started := manager.started
	closed := manager.closed
	manager.lifecycleMu.Unlock()
	if !started {
		return ErrNotStarted
	}
	if closed || ownerContext == nil {
		return ErrClosed
	}
	// 发送和等待响应两个阶段都同时监听调用方与 Owner Context：前者提供本次调用的
	// 取消，后者保证后台失败或 Shutdown 后不会永久等待一个已退出的 Owner。
	select {
	case manager.commands <- value:
	case <-ctx.Done():
		return ctx.Err()
	case <-ownerContext.Done():
		return errors.Join(ErrClosed, manager.Err())
	}
	select {
	case err := <-response:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-ownerContext.Done():
		return errors.Join(ErrClosed, manager.Err())
	}
}

func (manager *Manager) publishStates(states map[string]*target, gate configruntime.Gate) {
	// Owner 每轮生成新的值副本再整体发布；读者只看到某一轮完整视图，不会与 Owner
	// 争用或意外修改 target 中的调度字段。
	view := make(map[string]State, len(states))
	for serviceID, target := range states {
		view[serviceID] = target.state
	}
	manager.stateMu.Lock()
	oldVisible := manager.stateVisible
	newVisible := gate != nil && gate.Active()
	changed := oldVisible != newVisible || (newVisible && !reflect.DeepEqual(manager.states, view))
	manager.states = view
	manager.stateGate = gate
	manager.stateVisible = newVisible
	manager.stateMu.Unlock()
	if changed {
		select {
		case manager.changed <- struct{}{}:
		default:
		}
	}
}

type originFingerprint struct {
	scheme, host, tlsServerName, httpHost string
	port, connectTimeout                  uint32
	tlsVerify, enabled                    bool
}

type healthFingerprint struct {
	typeValue                                       protocolv1.HealthType
	path                                            string
	interval, timeout, minimumStatus, maximumStatus uint32
	failureThreshold, successThreshold              uint32
}

type serviceFingerprint struct {
	origin originFingerprint
	health healthFingerprint
}

type targetSpec struct {
	serviceID        string
	serviceRevision  uint64
	originKey        string
	dialer           OriginDialer
	fingerprint      serviceFingerprint
	checkEnabled     bool
	healthType       protocolv1.HealthType
	path             string
	interval         time.Duration
	timeout          time.Duration
	minimumStatus    int
	maximumStatus    int
	failureThreshold uint32
	successThreshold uint32
	hostHeader       string
}

type plan struct {
	gate    configruntime.Gate
	targets map[string]targetSpec
}

// compilePlan 把 Wire Snapshot 中的 Service 健康配置编译为调度器使用的不可变 Plan。
// 这里校验整份健康计划并以 Service ID 建索引；任一 Service 的 Origin/Health 输入非法
// 都会拒绝整代配置，不允许只启用其中“看起来正常”的部分。
func compilePlan(snapshot *protocolv1.TunnelSnapshot, gate configruntime.Gate, dialer OriginDialer) (*plan, error) {
	compiled := &plan{gate: gate, targets: make(map[string]targetSpec, len(snapshot.GetServices()))}
	for index, service := range snapshot.GetServices() {
		spec, err := compileTarget(service, dialer)
		if err != nil {
			return nil, fmt.Errorf("%w: %w: service index=%d", configruntime.ErrProtocolViolation, err, index)
		}
		if _, exists := compiled.targets[spec.serviceID]; exists {
			return nil, fmt.Errorf("%w: duplicate service index=%d", configruntime.ErrProtocolViolation, index)
		}
		compiled.targets[spec.serviceID] = spec
	}
	return compiled, nil
}

func compileTarget(service *protocolv1.ServiceConfig, dialer OriginDialer) (targetSpec, error) {
	if service == nil || !identity.ValidServiceID(service.GetServiceId()) {
		return targetSpec{}, ErrInvalidConfig
	}
	origin := originFingerprint{
		scheme: service.GetOriginScheme(), host: service.GetOriginHost(), port: service.GetOriginPort(),
		connectTimeout: service.GetConnectTimeoutMs(), tlsVerify: service.GetTlsVerify(),
		tlsServerName: service.GetTlsServerName(), httpHost: service.GetOriginHttpHost(), enabled: service.GetEnabled(),
	}
	if err := originconfig.Validate(originconfig.Fields{
		Scheme: origin.scheme, Host: origin.host, Port: origin.port, ConnectTimeoutMS: origin.connectTimeout,
		TLSVerify: origin.tlsVerify, TLSServerName: origin.tlsServerName, HTTPHostHeader: origin.httpHost,
	}); err != nil {
		return targetSpec{}, ErrInvalidConfig
	}
	health, err := validateHealth(service.GetHealth())
	if err != nil {
		return targetSpec{}, err
	}
	hostHeader := origin.httpHost
	if hostHeader == "" {
		// 未显式配置 HTTP Host 时，由已校验的 Origin 地址派生，后续 Checker 无需再次
		// 理解 Snapshot 的默认规则。
		hostHeader = net.JoinHostPort(origin.host, fmt.Sprint(origin.port))
	}
	return targetSpec{
		serviceID: service.GetServiceId(), serviceRevision: service.GetRequiredRevision(),
		originKey: net.JoinHostPort(origin.host, fmt.Sprint(origin.port)), dialer: dialer,
		fingerprint:  serviceFingerprint{origin: origin, health: health},
		checkEnabled: origin.enabled && health.typeValue != protocolv1.HealthType_HEALTH_TYPE_DISABLED,
		healthType:   health.typeValue, path: health.path,
		interval:      time.Duration(health.interval) * time.Millisecond,
		timeout:       time.Duration(health.timeout) * time.Millisecond,
		minimumStatus: int(health.minimumStatus), maximumStatus: int(health.maximumStatus),
		failureThreshold: health.failureThreshold, successThreshold: health.successThreshold,
		hostHeader: hostHeader,
	}, nil
}

func validateHealth(health *protocolv1.HealthCheckConfig) (healthFingerprint, error) {
	if health == nil {
		return healthFingerprint{}, ErrInvalidConfig
	}
	value := healthFingerprint{
		typeValue: health.GetType(), path: health.GetPath(), interval: health.GetIntervalMs(),
		timeout: health.GetTimeoutMs(), minimumStatus: health.GetExpectedStatusMin(),
		maximumStatus: health.GetExpectedStatusMax(), failureThreshold: health.GetFailureThreshold(),
		successThreshold: health.GetSuccessThreshold(),
	}
	if value.typeValue == protocolv1.HealthType_HEALTH_TYPE_DISABLED {
		// Disabled 必须是唯一表达：其余字段一律为零值，避免同一语义出现多种 Wire 形态。
		if value.path != "" || value.interval != 0 || value.timeout != 0 || value.minimumStatus != 0 ||
			value.maximumStatus != 0 || value.failureThreshold != 0 || value.successThreshold != 0 {
			return healthFingerprint{}, ErrInvalidConfig
		}
		return value, nil
	}
	if value.interval < minimumIntervalMS || value.interval > maximumIntervalMS ||
		value.timeout < minimumTimeoutMS || value.timeout >= value.interval ||
		value.failureThreshold < minimumThreshold || value.failureThreshold > maximumThreshold ||
		value.successThreshold < minimumThreshold || value.successThreshold > maximumThreshold {
		return healthFingerprint{}, ErrInvalidConfig
	}
	switch value.typeValue {
	case protocolv1.HealthType_HEALTH_TYPE_TCP:
		// TCP 探测只关心连接是否建立，不接受 HTTP 专属字段。
		if value.path != "" || value.minimumStatus != 0 || value.maximumStatus != 0 {
			return healthFingerprint{}, ErrInvalidConfig
		}
	case protocolv1.HealthType_HEALTH_TYPE_HTTP:
		// HTTP 探测要求合法请求目标和闭区间状态码范围。
		if !validRequestPath(value.path) || value.minimumStatus < minimumHTTPStatus ||
			value.maximumStatus > maximumHTTPStatus || value.minimumStatus > value.maximumStatus {
			return healthFingerprint{}, ErrInvalidConfig
		}
	default:
		return healthFingerprint{}, ErrInvalidConfig
	}
	return value, nil
}

func validRequestPath(value string) bool {
	// ParseRequestURI 只接受请求目标；额外排除 scheme、authority、userinfo 和 fragment，
	// 防止 Health Path 被解释成另一个远端地址或注入额外的 HTTP 行。
	if value == "" || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\r\n") {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	return err == nil && parsed.Scheme == "" && parsed.Host == "" && parsed.User == nil && parsed.Fragment == ""
}

func isNilDialer(dialer OriginDialer) bool {
	// 接口值本身非 nil 时，底层仍可能装着 typed nil；提前拒绝可避免 Checker 调用时 panic。
	if dialer == nil {
		return true
	}
	value := reflect.ValueOf(dialer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

type candidate struct {
	manager *Manager
	plan    *plan

	// started/terminal 描述 Candidate 生命周期；removing/removed 则只描述 Plan 注销进度。
	// 两组状态分开，才能让并发 Abort/Retire 共用一次可等待、失败后可重试的注销动作。
	mu         sync.Mutex
	started    bool
	terminal   bool
	removing   bool
	removed    bool
	removeDone chan struct{}
}

// Start 把已通过 Prepare 的 Plan 注册到 Owner。
// 注册成功仅表示 Plan 可参与切代；真正开始检查仍取决于该 Plan 的 Gate 是否 Active。
func (candidate *candidate) Start(ctx context.Context) error {
	if candidate == nil || ctx == nil {
		return ErrInvalidConfig
	}
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	if candidate.started || candidate.terminal || candidate.manager == nil || candidate.plan == nil {
		return ErrInvalidConfig
	}
	if err := candidate.manager.sendCommand(ctx, command{kind: commandRegister, plan: candidate.plan}); err != nil {
		return err
	}
	candidate.started = true
	return nil
}

func (candidate *candidate) Abort(ctx context.Context) error {
	if candidate == nil || ctx == nil {
		return ErrInvalidConfig
	}
	candidate.mu.Lock()
	started := candidate.started
	// 先进入 terminal，立即阻止 Runtime 再导出资源；若此前已注册，再走统一注销路径。
	candidate.terminal = true
	candidate.mu.Unlock()
	if started {
		return candidate.unregister(ctx)
	}
	return nil
}

func (candidate *candidate) Runtime() configruntime.Resources {
	if candidate == nil {
		return nil
	}
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	if !candidate.started || candidate.terminal {
		return nil
	}
	// Resources 只持有 Candidate 引用，Retire 与 Abort 因而共享同一个幂等注销状态机。
	return &resources{candidate: candidate}
}

func (candidate *candidate) unregister(ctx context.Context) error {
	// 注销可能由 Abort 与 Retire 并发触发。任一时刻只允许一个请求发给 Owner；其余
	// 调用等待本轮完成后重新检查状态。失败不会标记 removed，因此后续调用可以重试。
	for {
		candidate.mu.Lock()
		if candidate.removed {
			candidate.mu.Unlock()
			return nil
		}
		if candidate.removing {
			done := candidate.removeDone
			candidate.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		candidate.removing = true
		candidate.removeDone = make(chan struct{})
		done := candidate.removeDone
		manager := candidate.manager
		plan := candidate.plan
		candidate.mu.Unlock()

		err := manager.sendCommand(ctx, command{kind: commandUnregister, plan: plan})
		candidate.mu.Lock()
		candidate.removing = false
		if err == nil {
			// 只有 Owner 确认注销后才进入永久幂等状态；超时或关闭错误保留重试机会。
			candidate.removed = true
		}
		close(done)
		candidate.mu.Unlock()
		return err
	}
}

type resources struct{ candidate *candidate }

// Retire 释放 Runtime 接管的 Plan；与 Abort 并发时仍只会执行一次成功注销。
func (resources *resources) Retire(ctx context.Context) error {
	if resources == nil || resources.candidate == nil || ctx == nil {
		return ErrInvalidConfig
	}
	return resources.candidate.unregister(ctx)
}

var _ configruntime.Candidate = (*candidate)(nil)
var _ configruntime.Resources = (*resources)(nil)
