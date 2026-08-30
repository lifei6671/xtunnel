// Package usage 在 Server 数据面与持久化 Repository 之间聚合 Usage 增量。
package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/safego"
)

const (
	flushInterval     = 60 * time.Second
	operationTimeout  = 5 * time.Second
	maxCounter        = uint64(math.MaxInt64)
	maxPendingBuckets = 65_536
)

var (
	ErrInvalidOptions  = errors.New("usage owner options are invalid")
	ErrInvalidKey      = errors.New("usage key is invalid")
	ErrAlreadyStarted  = errors.New("usage owner has already started")
	ErrStopped         = errors.New("usage owner has stopped")
	ErrCapacity        = errors.New("usage owner capacity exceeded")
	ErrCounterOverflow = errors.New("usage counter overflow")
)

// counters 是一份尚未持久化的数据面增量；Usage 不会扣减已提交的流量或连接事件。
type counters struct {
	Connections  uint64
	IngressBytes uint64
	EgressBytes  uint64
	Errors       uint64
}

// Delta 是等待 Repository 原子写入的一份 UTC minute Bucket 增量。
// 所有计数都不会超过 SQLite 有符号 INTEGER 的安全范围。
type Delta struct {
	BucketTime   time.Time
	TunnelID     string
	ServiceID    string
	Connections  uint64
	IngressBytes uint64
	EgressBytes  uint64
	Errors       uint64
}

// Repository 是 Owner 消费的最小持久化边界。Flush 必须原子提交完整批次：返回错误
// 表示本批没有任何 Delta 提交，Owner 才能安全合回内存。Rollup 对同一个
// completedBefore 必须幂等，并遵循先提交上层 Bucket、再删除明细的事务顺序。
type Repository interface {
	Flush(context.Context, []Delta) error
	Rollup(context.Context, time.Time) error
}

// Options 保存 Usage Owner 的进程级固定依赖。ReportError 只观察可恢复的周期失败，
// 回调不得阻塞；生产适配层只能记录稳定 error_code，不得输出原始数据库错误。
type Options struct {
	Repository  Repository
	ReportError func(error)
}

type bucketKey struct {
	bucketTime int64
	tunnelID   string
	serviceID  string
}

// Owner 只拥有一个周期 goroutine。数据面计数只在更新内存 Bucket 时持有 mu；Flush
// 在 mu 内交换整张 Map，释放锁后才访问 Repository，失败时再把批次合回。flushGate
// 串行化显式、周期与关停写入，使 Rollup 不会和 Flush 竞态，已经确认提交的批次也
// 不会再次提交。
type Owner struct {
	repository  Repository
	reportError func(error)

	mu           sync.Mutex
	buckets      map[bucketKey]counters
	inFlightKeys map[bucketKey]struct{}
	pendingKeys  int
	started      bool
	stopped      bool
	cancel       context.CancelFunc
	done         chan struct{}
	runErr       error

	flushGate chan struct{}

	shutdownOnce sync.Once
	shutdownErr  error

	now            func() time.Time
	flushEvery     time.Duration
	operationLimit time.Duration
	maxBuckets     int
}

// New 创建尚未启动的 Usage Owner。显式 Start 让生产装配先完整发布全部依赖，
// 再启动唯一周期 goroutine。
func New(options Options) (*Owner, error) {
	if options.Repository == nil {
		return nil, ErrInvalidOptions
	}
	owner := &Owner{
		repository: options.Repository, reportError: options.ReportError,
		buckets: make(map[bucketKey]counters), now: time.Now,
		flushGate: make(chan struct{}, 1), flushEvery: flushInterval, operationLimit: operationTimeout,
		maxBuckets: maxPendingBuckets,
	}
	owner.flushGate <- struct{}{}
	return owner, nil
}

// Start 启动唯一的周期 Flush/Rollup owner。父 Context 取消会停止 Ticker，但在
// Shutdown 建立停止 Fence 前仍接受计数：优雅排空期间，已经准入的连接仍须统计，
// 最后由 Shutdown 的最终 Flush 持久化。
func (owner *Owner) Start(parent context.Context) error {
	if owner == nil || parent == nil {
		return ErrInvalidOptions
	}
	owner.mu.Lock()
	if owner.stopped {
		owner.mu.Unlock()
		return ErrStopped
	}
	if owner.started {
		owner.mu.Unlock()
		return ErrAlreadyStarted
	}
	runContext, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	owner.started = true
	owner.cancel = cancel
	owner.done = done
	owner.mu.Unlock()

	safego.Go(owner.handlePanic, func() { close(done) }, func() {
		owner.run(runContext)
	})
	return nil
}

// ObserveOpen 把一次成功的逻辑 OPEN 计为一个连接，把一次最终失败的逻辑 OPEN
// 计为一个错误。调用方只能在最外层 OPEN 生命周期调用一次；内部 Failover attempt
// 不能重复计数。
func (owner *Owner) ObserveOpen(tunnelID, serviceID string, success bool) error {
	if success {
		return owner.add(tunnelID, serviceID, counters{Connections: 1})
	}
	return owner.add(tunnelID, serviceID, counters{Errors: 1})
}

// AddIngressBytes 记录 Public Client 到 Origin 的字节数。合法 Key 的零增量是 no-op，
// 不会分配 Bucket。
func (owner *Owner) AddIngressBytes(tunnelID, serviceID string, bytes uint64) error {
	return owner.add(tunnelID, serviceID, counters{IngressBytes: bytes})
}

// AddEgressBytes 记录 Origin 到 Public Client 的字节数。合法 Key 的零增量是 no-op，
// 不会分配 Bucket。
func (owner *Owner) AddEgressBytes(tunnelID, serviceID string, bytes uint64) error {
	return owner.add(tunnelID, serviceID, counters{EgressBytes: bytes})
}

// add 把增量计入事件发生时的 UTC minute Bucket，只更新内存且绝不等待 Repository
// IO。Shutdown 的停止 Fence 拒绝后续调用；待提交 Bucket 达到固定容量时快速失败，
// 避免 Repository 长期故障造成无界内存。计数溢出时饱和到 MaxInt64 并返回稳定
// 错误，禁止未来 SQLite 值回绕为负数。
func (owner *Owner) add(tunnelID, serviceID string, increment counters) error {
	if owner == nil {
		return ErrInvalidOptions
	}
	if !validate.ValidID(tunnelID, "tun_") || !validate.ValidID(serviceID, "svc_") {
		return ErrInvalidKey
	}
	if increment == (counters{}) {
		return nil
	}
	bucketTime := owner.now().UTC().Truncate(time.Minute).Unix()
	key := bucketKey{bucketTime: bucketTime, tunnelID: tunnelID, serviceID: serviceID}

	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.stopped {
		return ErrStopped
	}
	current := owner.buckets[key]
	if _, currentExists := owner.buckets[key]; !currentExists {
		_, inFlight := owner.inFlightKeys[key]
		if !inFlight {
			if owner.pendingKeys >= owner.maxBuckets {
				return ErrCapacity
			}
			owner.pendingKeys++
		}
	}
	merged, overflow := mergeIncrement(current, increment)
	owner.buckets[key] = merged
	if overflow {
		return ErrCounterOverflow
	}
	return nil
}

// Flush 原子摘下全部当前 minute 增量并持久化。失败时，摘下的原批次会与并发到达的
// 新增量做饱和合并，使下一次重试保持完整，同时不重复提交 Repository 已确认的批次。
func (owner *Owner) Flush(ctx context.Context) error {
	if owner == nil || ctx == nil {
		return ErrInvalidOptions
	}
	operationContext, cancel := owner.operationContext(ctx)
	defer cancel()
	if err := owner.acquireFlush(operationContext); err != nil {
		return err
	}
	defer owner.releaseFlush()
	return owner.flushLocked(operationContext)
}

// Rollup 要求 Repository 幂等汇总当前 UTC minute 之前的已完成 Bucket。
// minute/hour/day 的事务顺序与 Retention 规则由 Repository 单独拥有。
func (owner *Owner) Rollup(ctx context.Context) error {
	if owner == nil || ctx == nil {
		return ErrInvalidOptions
	}
	operationContext, cancel := owner.operationContext(ctx)
	defer cancel()
	if err := owner.acquireFlush(operationContext); err != nil {
		return err
	}
	defer owner.releaseFlush()
	return owner.rollupLocked(operationContext, owner.now())
}

// Shutdown 停止唯一 goroutine、拒绝后续计数，然后执行一次最终 Flush 与 Rollup。
// 首个调用方拥有关闭过程，并发或重复调用取得同一结果。Repository 调用同时受调用方
// Deadline 与固定操作上限约束，等待串行化 Gate 也不能越过该边界。
func (owner *Owner) Shutdown(ctx context.Context) error {
	if owner == nil || ctx == nil {
		return ErrInvalidOptions
	}
	owner.shutdownOnce.Do(func() {
		owner.shutdownErr = owner.shutdown(ctx)
	})
	return owner.shutdownErr
}

func (owner *Owner) run(ctx context.Context) {
	ticker := time.NewTicker(owner.flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			operationContext, cancel := owner.operationContext(ctx)
			err := owner.flushAndRollup(operationContext, now)
			cancel()
			if err != nil {
				owner.report(err)
			}
		}
	}
}

func (owner *Owner) shutdown(ctx context.Context) error {
	owner.mu.Lock()
	owner.stopped = true
	cancel := owner.cancel
	done := owner.done
	owner.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	operationContext, cancelOperation := owner.operationContext(ctx)
	defer cancelOperation()
	if err := owner.acquireFlush(operationContext); err != nil {
		return err
	}
	defer owner.releaseFlush()
	operationErr := owner.flushLocked(operationContext)
	rollupErr := owner.rollupLocked(operationContext, owner.now())
	owner.mu.Lock()
	runErr := owner.runErr
	owner.mu.Unlock()
	return errors.Join(runErr, operationErr, rollupErr)
}

func (owner *Owner) flushAndRollup(ctx context.Context, now time.Time) error {
	if err := owner.acquireFlush(ctx); err != nil {
		return err
	}
	defer owner.releaseFlush()
	return errors.Join(owner.flushLocked(ctx), owner.rollupLocked(ctx, now))
}

func (owner *Owner) flushLocked(ctx context.Context) error {
	batch := owner.swapBuckets()
	if len(batch) == 0 {
		return nil
	}
	deltas := makeDeltas(batch)
	operationContext, cancel := owner.operationContext(ctx)
	err := owner.repository.Flush(operationContext, deltas)
	cancel()
	if err == nil {
		owner.finishBatch(batch)
		return nil
	}
	overflow := owner.mergeBuckets(batch)
	if overflow {
		return errors.Join(fmt.Errorf("flush usage deltas: %w", err), ErrCounterOverflow)
	}
	return fmt.Errorf("flush usage deltas: %w", err)
}

func (owner *Owner) rollupLocked(ctx context.Context, now time.Time) error {
	completedBefore := now.UTC().Truncate(time.Minute)
	operationContext, cancel := owner.operationContext(ctx)
	err := owner.repository.Rollup(operationContext, completedBefore)
	cancel()
	if err != nil {
		return fmt.Errorf("roll up usage buckets: %w", err)
	}
	return nil
}

func (owner *Owner) swapBuckets() map[bucketKey]counters {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	batch := owner.buckets
	owner.buckets = make(map[bucketKey]counters)
	owner.inFlightKeys = make(map[bucketKey]struct{}, len(batch))
	for key := range batch {
		owner.inFlightKeys[key] = struct{}{}
	}
	return batch
}

func (owner *Owner) mergeBuckets(batch map[bucketKey]counters) bool {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	overflow := false
	for key, increment := range batch {
		merged, saturated := mergeIncrement(owner.buckets[key], increment)
		owner.buckets[key] = merged
		overflow = overflow || saturated
	}
	owner.inFlightKeys = nil
	return overflow
}

func (owner *Owner) finishBatch(batch map[bucketKey]counters) {
	owner.mu.Lock()
	defer owner.mu.Unlock()
	for key := range batch {
		if _, remains := owner.buckets[key]; !remains {
			owner.pendingKeys--
		}
	}
	owner.inFlightKeys = nil
}

func makeDeltas(batch map[bucketKey]counters) []Delta {
	deltas := make([]Delta, 0, len(batch))
	for key, increment := range batch {
		deltas = append(deltas, Delta{
			BucketTime: time.Unix(key.bucketTime, 0).UTC(), TunnelID: key.tunnelID, ServiceID: key.serviceID,
			Connections: increment.Connections, IngressBytes: increment.IngressBytes,
			EgressBytes: increment.EgressBytes, Errors: increment.Errors,
		})
	}
	sort.Slice(deltas, func(left, right int) bool {
		if !deltas[left].BucketTime.Equal(deltas[right].BucketTime) {
			return deltas[left].BucketTime.Before(deltas[right].BucketTime)
		}
		if deltas[left].TunnelID != deltas[right].TunnelID {
			return deltas[left].TunnelID < deltas[right].TunnelID
		}
		return deltas[left].ServiceID < deltas[right].ServiceID
	})
	return deltas
}

func mergeIncrement(left, right counters) (counters, bool) {
	connections, connectionOverflow := saturatingAdd(left.Connections, right.Connections)
	ingress, ingressOverflow := saturatingAdd(left.IngressBytes, right.IngressBytes)
	egress, egressOverflow := saturatingAdd(left.EgressBytes, right.EgressBytes)
	errorsCount, errorOverflow := saturatingAdd(left.Errors, right.Errors)
	return counters{
		Connections: connections, IngressBytes: ingress, EgressBytes: egress, Errors: errorsCount,
	}, connectionOverflow || ingressOverflow || egressOverflow || errorOverflow
}

func saturatingAdd(left, right uint64) (uint64, bool) {
	if left >= maxCounter || right > maxCounter-left {
		return maxCounter, true
	}
	return left + right, false
}

func (owner *Owner) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, owner.operationLimit)
}

func (owner *Owner) acquireFlush(ctx context.Context) error {
	select {
	case <-owner.flushGate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (owner *Owner) releaseFlush() {
	owner.flushGate <- struct{}{}
}

func (owner *Owner) handlePanic(err error) {
	owner.mu.Lock()
	owner.runErr = errors.Join(owner.runErr, fmt.Errorf("run usage owner: %w", err))
	owner.mu.Unlock()
	owner.report(err)
}

func (owner *Owner) report(err error) {
	if err != nil && owner.reportError != nil {
		owner.reportError(err)
	}
}
