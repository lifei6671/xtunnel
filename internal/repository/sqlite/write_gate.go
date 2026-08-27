package sqlite

import (
	"context"
	"sync"
)

// writeGate 是 Store 内全部运行时写操作和维护 Barrier 的唯一排队入口。
// 它只保护“谁可以开始写”这一所有权，不持有数据库连接，也不在 mu 内执行 IO。
// 队列按到达顺序直接交接租约，因此备份请求一旦入队，后续写事务不能越过它。
//
// writeGate 的核心不变量是：held 为 true 时，当前调用方或某个已获授权的队首
// waiter 必须最终释放租约；held 为 false 时 waiters 必须为空。取消方负责从队列
// 摘除自己；若取消与授权同时发生，则主动归还已取得的租约，避免写入永久阻塞。
type writeGate struct {
	mu sync.Mutex
	// held 表示唯一写租约已被持有或正在无空窗地移交给队首。
	held bool
	// waiters 按 acquire 到达顺序保存尚未获授权的请求。
	waiters []*writeWaiter
}

// writeWaiter 是一次排队请求。只有 writeGate 可以关闭 ready；关闭即表示租约
// 所有权已经从前任持有者转移给该请求，而不是普通的“可以重试”通知。
type writeWaiter struct {
	ready chan struct{}
}

// writeLease 表示 writeGate 唯一租约的调用方所有权。once 使清理路径可以安全地
// 重复调用 Release，而不会重复唤醒队首或破坏 held 不变量。
type writeLease struct {
	gate *writeGate
	once sync.Once
}

// newWriteGate 创建未持有且无等待者的写入队列。
func newWriteGate() *writeGate {
	return &writeGate{}
}

// acquire 按 FIFO 获取唯一写租约。等待只依赖 ctx 和 waiter.ready，不创建
// goroutine；因此调用方取消或获授权后，队列中不会遗留后台任务。
func (gate *writeGate) acquire(ctx context.Context) (*writeLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	gate.mu.Lock()
	// 仅当既无持有者又无队列时走快速路径。检查 waiters 防止新请求绕过
	// 已排队但尚未被交接的旧请求。
	if !gate.held && len(gate.waiters) == 0 {
		gate.held = true
		gate.mu.Unlock()
		return &writeLease{gate: gate}, nil
	}
	waiter := &writeWaiter{ready: make(chan struct{})}
	gate.waiters = append(gate.waiters, waiter)
	gate.mu.Unlock()

	select {
	case <-waiter.ready:
		return &writeLease{gate: gate}, nil
	case <-ctx.Done():
		gate.mu.Lock()
		// waiter 仍在队列说明尚未获授权，可以在锁内直接撤销排队请求。
		for index, queued := range gate.waiters {
			if queued == waiter {
				gate.waiters = append(gate.waiters[:index], gate.waiters[index+1:]...)
				gate.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		gate.mu.Unlock()

		// 授权与取消可能同时发生。waiter 已不在队列表示租约已经交给本请求；
		// 此时必须代替未开始的调用方归还它。
		gate.release()
		return nil, ctx.Err()
	}
}

// Release 归还租约。该方法是幂等的，允许成功、取消和 defer 清理路径汇合。
func (lease *writeLease) Release() {
	lease.once.Do(lease.gate.release)
}

// release 在锁内把租约直接交给 FIFO 队首；没有等待者时才把 Gate 置为空闲。
// 它不执行数据库操作，也不等待接收方运行，因此不会把不可控阻塞带入 mu。
func (gate *writeGate) release() {
	gate.mu.Lock()
	defer gate.mu.Unlock()

	if len(gate.waiters) == 0 {
		gate.held = false
		return
	}
	next := gate.waiters[0]
	gate.waiters = gate.waiters[1:]
	// held 保持为 true，租约直接移交给队首，期间不存在可插队的空窗。
	close(next.ready)
}
