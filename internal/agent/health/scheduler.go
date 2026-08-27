package health

import (
	"container/heap"
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/safego"
)

// commandKind 区分只校验、注册和注销三种控制动作。
type commandKind uint8

const (
	// validate 只检查 Candidate 与已注册计划是否冲突，不改变调度状态。
	commandValidate commandKind = iota
	// register 把 Candidate 纳入观察范围，并立即按 Active Gate 重新对账。
	commandRegister
	// unregister 移除已退出的 Candidate，并重新选择当前生效计划。
	commandUnregister
)

// command 是外部协程与调度 Owner 沟通的控制消息。外部协程不能直接读写
// scheduler；它只能提交命令，并通过 response 等待 Owner 串行处理完成。
type command struct {
	kind     commandKind
	plan     *plan
	response chan error
}

// target 保存一个 Service 的全部可变调度状态。这些字段只由 Owner goroutine 读写，
// 因此不需要再用互斥锁保护；Worker 只会调用随 checkJob 复制出去的 cancel 函数。
type target struct {
	spec targetSpec

	// epoch 是检查任务的代际号。配置切代或暂停时推进它，旧 Worker 即使稍后返回，
	// 结果也会因 epoch 不匹配而被丢弃。
	epoch              uint64
	state              State
	consecutiveSuccess uint32
	consecutiveFailure uint32
	// due 是期望开始检查的时间；latestDue 是本轮最晚可接受的开始时间。
	// 超过 latestDue 说明调度预算已经错过，不能再把迟到检查伪装成实时健康状态。
	due       time.Time
	latestDue time.Time
	inFlight  bool
	cancel    context.CancelFunc
	// index 由 container/heap 维护，-1 表示当前不在待调度堆中。
	index int
}

// checkJob 是 Owner 发给 Worker 的不可变检查快照。Worker 只使用这里的 spec 和
// Context 做网络检查，绝不直接修改 target 或 scheduler。
type checkJob struct {
	ctx    context.Context
	cancel context.CancelFunc
	spec   targetSpec
	epoch  uint64
}

// observation 是 Checker 对一次 Origin 探测的原始结论；它还没有经过连续成功/失败
// 阈值状态机处理。
type observation struct {
	success    bool
	latency    time.Duration
	originCode protocolv1.ErrorCode
	failure    FailureKind
}

// checkResult 给原始结论带上 Service、Revision 和 epoch 身份，Owner 据此判断结果
// 是否仍属于当前配置代际。
type checkResult struct {
	serviceID       string
	serviceRevision uint64
	originKey       string
	epoch           uint64
	finishedAt      time.Time
	observation     observation
}

// scheduler 是健康检查的单 Owner 状态机。commands、results 和定时器事件最终都在
// ownerLoop 中串行落到这里；Worker 只生产 checkResult。这种所有权模型避免了堆、
// target 状态和并发预算之间出现跨 goroutine 竞态。
type scheduler struct {
	manager *Manager
	// plans 可能同时包含旧 Candidate 和新 Candidate，但任何时刻最多只能有一个
	// Gate 为 Active；active 指向当前允许发布健康状态的计划。
	plans   map[*plan]struct{}
	active  *plan
	targets map[string]*target
	// queue 是按 due 排序的最小堆，只包含已启用且当前没有在途检查的 target。
	queue targetHeap

	// running 与 runningByOrigin 分别执行全局、单 Origin 并发上限；nextPermit
	// 执行跨 Origin 共用的启动速率限制。
	nextEpoch       uint64
	running         int
	runningByOrigin map[string]int
	nextPermit      time.Time
}

func randomFloat64() float64 { return rand.Float64() }

// 每个 Worker 都有明确生命周期：由 Manager 启动，随 manager.ctx 取消而退出，并在
// workerWait 中登记，保证 Owner 收尾时可以等待所有 Worker 停止。
func (manager *Manager) startWorker() {
	safego.Go(manager.fail, manager.workerWait.Done, manager.workerLoop)
}

// Owner 的退出回调只负责关闭 done；ownerLoop 自己负责取消 Worker、回收检查和清空
// 已发布状态，确保对外的“已停止”信号只发送一次。
func (manager *Manager) startOwner() {
	safego.Go(manager.fail, func() {
		manager.doneOnce.Do(func() { close(manager.done) })
	}, manager.ownerLoop)
}

// workerLoop 只消费 checkJob、运行 Checker、回传 checkResult，不参与健康状态迁移。
// job 自带超时 Context；检查结束后立即 cancel，释放它关联的定时器等资源。
func (manager *Manager) workerLoop() {
	for {
		select {
		case <-manager.ctx.Done():
			return
		case job := <-manager.jobs:
			result := checkResult{
				serviceID:       job.spec.serviceID,
				serviceRevision: job.spec.serviceRevision,
				originKey:       job.spec.originKey,
				epoch:           job.epoch,
				observation:     manager.options.checker(job.ctx, job.spec, job.spec.dialer),
				finishedAt:      manager.options.now(),
			}
			job.cancel()
			select {
			case manager.results <- result:
			case <-manager.ctx.Done():
				return
			}
		}
	}
}

// ownerLoop 是调度器的唯一事件循环：
//
//  1. reconcile 根据 Active Gate 把计划对账为当前 targets；
//  2. dispatch 把已经到期且预算允许的任务交给 Worker；
//  3. publishStates 发布当前可见快照，并把定时器设到下一次必要唤醒；
//  4. 串行处理取消、控制命令、Worker 结果或定时器事件。
//
// 因为所有状态变化都回到这一循环，所以 Worker 数量增加也不会改变状态机的所有权。
func (manager *Manager) ownerLoop() {
	state := scheduler{
		manager: manager, plans: make(map[*plan]struct{}), targets: make(map[string]*target),
		runningByOrigin: make(map[string]int),
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	defer func() {
		// 先取消根 Context 和所有在途检查，再等 Worker 退出，最后清空对外快照。
		// 这个顺序保证不会在“已清空”之后又收到 Worker 发布的新结果。
		manager.cancel()
		state.cancelAll()
		manager.workerWait.Wait()
		manager.publishStates(nil, nil)
	}()

	for {
		now := manager.options.now()
		if err := state.reconcile(now); err != nil {
			manager.fail(err)
			return
		}
		state.dispatch(now)
		if state.active == nil {
			manager.publishStates(nil, nil)
		} else {
			manager.publishStates(state.targets, state.active.gate)
		}
		resetTimer(timer, state.waitDuration(now))

		select {
		case <-manager.ctx.Done():
			return
		case value := <-manager.commands:
			value.response <- state.handleCommand(value, manager.options.now())
		case result := <-manager.results:
			state.handleResult(result)
		case <-timer.C:
		}
	}
}

// handleCommand 在 Owner 内串行执行控制请求。注册前再次 validate，是为了把
// “先校验、后注册”之间可能出现的新计划也纳入冲突检查。
func (state *scheduler) handleCommand(value command, now time.Time) error {
	if value.plan == nil {
		return ErrInvalidConfig
	}
	switch value.kind {
	case commandValidate:
		return state.validate(value.plan)
	case commandRegister:
		if err := state.validate(value.plan); err != nil {
			return err
		}
		state.plans[value.plan] = struct{}{}
		return state.reconcile(now)
	case commandUnregister:
		delete(state.plans, value.plan)
		return state.reconcile(now)
	default:
		return ErrInvalidConfig
	}
}

// validate 冻结同一 Service Revision 对应的健康检查输入。同 Revision 却出现不同
// fingerprint 代表控制面改写了已发布版本，必须作为协议违例拒绝，而不是猜测哪份为准。
func (state *scheduler) validate(candidate *plan) error {
	for registered := range state.plans {
		for serviceID, next := range candidate.targets {
			current, exists := registered.targets[serviceID]
			if exists && current.serviceRevision == next.serviceRevision && current.fingerprint != next.fingerprint {
				return fmt.Errorf("%w: service %s revision %d changed health inputs", configruntime.ErrProtocolViolation, serviceID, next.serviceRevision)
			}
		}
	}
	return nil
}

// reconcile 把所有已注册 Candidate 与当前 Active Gate 对账为一代可运行 targets。
// Gate 决定“哪份计划现在有发布资格”，plan 本身只描述该代包含哪些检查目标。
func (state *scheduler) reconcile(now time.Time) error {
	var active *plan
	for candidate := range state.plans {
		if !candidate.gate.Active() {
			continue
		}
		if active != nil && active != candidate {
			return fmt.Errorf("%w: multiple active health plans", ErrScheduler)
		}
		active = candidate
	}
	if active == nil {
		if len(state.plans) == 0 {
			// 已无任何 Candidate：彻底取消并丢弃旧状态，下次注册从空状态开始。
			state.cancelAll()
			state.targets = make(map[string]*target)
			state.queue = nil
			state.active = nil
			return nil
		}
		// ConfigRuntime 的 Swap 与 APPLIED Ack 之间会短暂没有 Active Gate。
		// 这里只暂停检查并隐藏状态，保留未变化 Service 的状态与 nextDue。
		if state.active != nil {
			state.pause()
		}
		state.active = nil
		return nil
	}
	if active == state.active {
		// Gate 和计划均未切代，保留当前堆、连续计数和 nextDue。
		return nil
	}

	// 新计划中配置身份未变化的 Service 复用既有健康状态和调度时间；配置变化或新增
	// Service 则创建新 target，从 UNKNOWN（关闭检查时为 HEALTHY）重新开始。
	next := make(map[string]*target, len(active.targets))
	for serviceID, spec := range active.targets {
		current, exists := state.targets[serviceID]
		if exists && current.spec.serviceRevision == spec.serviceRevision && current.spec.fingerprint == spec.fingerprint {
			if current.inFlight {
				// Scoped Dialer 属于旧 Candidate；切代时即使配置未变，也必须取消并
				// 推进 epoch，防止旧 goroutine 的结果发布到新 generation。
				current.stop()
				current.inFlight = false
				state.nextEpoch++
				current.epoch = state.nextEpoch
			}
			current.spec = spec
			next[serviceID] = current
			continue
		}
		if exists {
			current.stop()
		}
		state.nextEpoch++
		created := &target{spec: spec, epoch: state.nextEpoch, index: -1}
		if !spec.checkEnabled {
			created.state = State{Status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, ServiceRevision: spec.serviceRevision}
		} else {
			created.state = State{Status: protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN, ServiceRevision: spec.serviceRevision}
			created.scheduleInitial(now, state.manager.options.random())
		}
		next[serviceID] = created
	}
	for serviceID, current := range state.targets {
		if _, exists := next[serviceID]; !exists {
			// 新计划已删除该 Service，及时取消旧代仍在执行的检查。
			current.stop()
		}
	}
	state.active = active
	state.targets = next
	state.rebuildQueue()
	return nil
}

// dispatch 从最早 due 的 target 开始派发。本函数只在 Owner 中运行，因此“检查预算、
// 记账、标记 inFlight、投递 job”是一个不会被其他结果插入打断的状态转换。
func (state *scheduler) dispatch(now time.Time) {
	if state.active == nil || !state.active.gate.Active() {
		return
	}
	deferred := make([]*target, 0)
	for state.queue.Len() > 0 {
		current := state.queue[0]
		if current.due.After(now) {
			// 最小堆堆顶尚未到期，后面的 target 也都无需检查。
			break
		}
		heap.Pop(&state.queue)
		if !current.latestDue.After(now) {
			// 超出当前 interval 的调度预算时 fail closed；预算 miss 不计入
			// Origin 连续失败，避免容量压力伪装成后端故障。
			current.markBudgetMiss(now)
			current.scheduleNext(now, state.manager.options.random())
			heap.Push(&state.queue, current)
			continue
		}
		if state.running >= state.manager.options.globalLimit ||
			state.runningByOrigin[current.spec.originKey] >= state.manager.options.perOriginLimit ||
			(!state.nextPermit.IsZero() && now.Before(state.nextPermit)) {
			// 预算暂时不足时先移出堆，继续观察其他已到期 target。这样某个 Origin 达到
			// 单 Origin 上限时，不会阻塞仍有配额的其他 Origin；循环结束再统一放回。
			deferred = append(deferred, current)
			continue
		}
		jobContext, cancel := context.WithTimeout(state.manager.ctx, current.spec.timeout)
		current.inFlight = true
		current.cancel = cancel
		state.running++
		state.runningByOrigin[current.spec.originKey]++
		// 每次实际派发都会推进全局启动许可，限制检查突发速率；并发额度则在结果
		// 返回时释放。
		state.nextPermit = now.Add(state.manager.options.rateInterval)
		state.manager.jobs <- checkJob{ctx: jobContext, cancel: cancel, spec: current.spec, epoch: current.epoch}
	}
	for _, current := range deferred {
		heap.Push(&state.queue, current)
	}
}

// handleResult 先释放任务占用的并发额度，再判断结果能否写入当前 target。即使结果
// 属于已取消的旧代，它也确实占用过 Worker/Origin 配额，因此记账必须先归还。
func (state *scheduler) handleResult(result checkResult) {
	if state.running > 0 {
		state.running--
	}
	if state.runningByOrigin[result.originKey] > 1 {
		state.runningByOrigin[result.originKey]--
	} else {
		delete(state.runningByOrigin, result.originKey)
	}
	current, exists := state.targets[result.serviceID]
	// epoch 防止旧 Candidate 的 goroutine 污染新代；Revision 防止 Service 身份错配；
	// inFlight 防止重复或已经被 pause/cancel 的结果二次落地。
	if !exists || current.epoch != result.epoch || current.spec.serviceRevision != result.serviceRevision || !current.inFlight {
		return
	}
	current.inFlight = false
	current.cancel = nil
	if state.active == nil || !state.active.gate.Active() {
		// Gate 已失活时结果不可见，也不安排下一轮；reconcile/pause 负责之后的恢复。
		return
	}
	current.apply(result.finishedAt, result.observation)
	current.scheduleNext(result.finishedAt, state.manager.options.random())
	heap.Push(&state.queue, current)
}

// waitDuration 取 Gate 轮询周期、最近 due 和下一次速率许可中的最短等待时间。
// 即使没有任务，也要按 gatePoll 醒来，以发现 Gate 在外部发生的切换。
func (state *scheduler) waitDuration(now time.Time) time.Duration {
	wait := state.manager.options.gatePoll
	if state.queue.Len() == 0 {
		return wait
	}
	head := state.queue[0]
	if !head.due.After(now) {
		if !state.nextPermit.IsZero() && now.Before(state.nextPermit) {
			untilPermit := state.nextPermit.Sub(now)
			if untilPermit < wait {
				return untilPermit
			}
		}
		return wait
	}
	untilDue := head.due.Sub(now)
	if untilDue < wait {
		return untilDue
	}
	return wait
}

// rebuildQueue 从 targets 重建待调度最小堆。在途任务必须等结果返回后再入堆，禁用
// 健康检查的 target 则只保留对外状态，永远不进入调度队列。
func (state *scheduler) rebuildQueue() {
	state.queue = make(targetHeap, 0, len(state.targets))
	for _, current := range state.targets {
		current.index = -1
		if current.spec.checkEnabled && !current.inFlight {
			state.queue = append(state.queue, current)
		}
	}
	heap.Init(&state.queue)
}

// cancelAll 解除全部阻塞中的检查。它不负责等待 Worker；等待统一由 Owner 退出流程
// 通过 workerWait 完成。
func (state *scheduler) cancelAll() {
	for _, current := range state.targets {
		current.stop()
	}
}

// pause 用于 Candidate 已注册但暂时没有 Active Gate 的切换窗口。健康状态和 nextDue
// 会保留，所有在途检查则取消并推进 epoch，保证窗口期返回的旧结果无法发布。
func (state *scheduler) pause() {
	for _, current := range state.targets {
		if !current.inFlight {
			continue
		}
		current.stop()
		current.inFlight = false
		state.nextEpoch++
		current.epoch = state.nextEpoch
	}
	state.rebuildQueue()
}

// stop 通过 job Context 解除 Checker 的超时等待或网络阻塞；重复调用不会重复取消。
func (current *target) stop() {
	if current.cancel != nil {
		current.cancel()
		current.cancel = nil
	}
}

// scheduleInitial 把首次检查均匀散布在 [now, now+interval]，避免大量 Service 在
// 新计划生效瞬间同时探测 Origin。latestDue 再给这一轮一个 interval 的调度窗口。
func (current *target) scheduleInitial(now time.Time, random float64) {
	random = clampRandom(random)
	current.due = now.Add(time.Duration(float64(current.spec.interval) * random))
	current.latestDue = current.due.Add(current.spec.interval)
}

// scheduleNext 为后续检查加入 0.8~1.2 倍 interval 的 jitter，避免多个 Agent 或
// Service 长期同频。latestDue 最晚截在 now+2*interval，防止容量拥塞后仍执行已经
// 失去时效性的检查。
func (current *target) scheduleNext(now time.Time, random float64) {
	random = clampRandom(random)
	delay := time.Duration(float64(current.spec.interval) * (0.8 + 0.4*random))
	current.due = now.Add(delay)
	current.latestDue = current.due.Add(current.spec.interval)
	staleDeadline := now.Add(2 * current.spec.interval)
	if current.latestDue.After(staleDeadline) {
		current.latestDue = staleDeadline
	}
}

// markBudgetMiss 表示“调度器没能及时安排检查”，而不是“Origin 检查失败”。因此状态
// 转为 UNKNOWN、记录专用错误码，并清空连续计数，避免容量问题触发 UNHEALTHY。
func (current *target) markBudgetMiss(now time.Time) {
	current.consecutiveFailure = 0
	current.consecutiveSuccess = 0
	current.state = State{
		Status:          protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN,
		ServiceRevision: current.spec.serviceRevision,
		CheckedAt:       now,
		OriginErrorCode: protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED,
		Failure:         FailureBudget,
	}
}

// apply 把一次 observation 输入带迟滞的健康状态机：
//
//   - UNKNOWN 首次成功即可变为 HEALTHY；
//   - HEALTHY/UNKNOWN 连续失败达到 failureThreshold 才变为 UNHEALTHY；
//   - UNHEALTHY 连续成功达到 successThreshold 才恢复 HEALTHY。
//
// 连续计数在方向变化或状态完成迁移时清零，防止不连续样本被错误累计。
func (current *target) apply(at time.Time, observation observation) {
	current.state.CheckedAt = at
	current.state.Latency = observation.latency
	current.state.OriginErrorCode = observation.originCode
	current.state.Failure = observation.failure
	if observation.success {
		current.consecutiveFailure = 0
		switch current.state.Status {
		case protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN:
			current.state.Status = protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY
			current.consecutiveSuccess = 0
		case protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY:
			current.consecutiveSuccess++
			if current.consecutiveSuccess >= current.spec.successThreshold {
				current.state.Status = protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY
				current.consecutiveSuccess = 0
			}
		default:
			current.consecutiveSuccess = 0
		}
		return
	}
	current.consecutiveSuccess = 0
	if current.state.Status == protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY {
		current.consecutiveFailure = 0
		return
	}
	current.consecutiveFailure++
	if current.consecutiveFailure >= current.spec.failureThreshold {
		current.state.Status = protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY
		current.consecutiveFailure = 0
	}
}

// clampRandom 同时约束生产随机数和测试注入值，确保 jitter 不会越出设计区间。
func clampRandom(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// resetTimer 集中处理 Owner 唯一定时器的复用：负等待归零，先停止当前周期，再设置
// 下一次唤醒时间。Go 1.27 已保证 Stop/Reset 后不会读到旧 tick；非阻塞读取只是保留
// 明确的停止清理步骤，不承担旧版本 Timer 语义兼容职责。
func resetTimer(timer *time.Timer, wait time.Duration) {
	if wait < 0 {
		wait = 0
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(wait)
}

// targetHeap 按 due 从早到晚排列；due 相同时再按 Service ID 排序，使同一输入下的
// 派发顺序稳定。Swap/Push/Pop 同步维护 target.index 与“是否在堆中”的约定。
type targetHeap []*target

func (values targetHeap) Len() int { return len(values) }
func (values targetHeap) Less(left, right int) bool {
	if values[left].due.Equal(values[right].due) {
		return values[left].spec.serviceID < values[right].spec.serviceID
	}
	return values[left].due.Before(values[right].due)
}
func (values targetHeap) Swap(left, right int) {
	values[left], values[right] = values[right], values[left]
	values[left].index, values[right].index = left, right
}
func (values *targetHeap) Push(value any) {
	current := value.(*target)
	current.index = len(*values)
	*values = append(*values, current)
}
func (values *targetHeap) Pop() any {
	old := *values
	last := len(old) - 1
	current := old[last]
	old[last] = nil
	current.index = -1
	*values = old[:last]
	return current
}
