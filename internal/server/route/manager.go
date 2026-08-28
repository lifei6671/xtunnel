package route

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/lifei6671/xtunnel/internal/repository"
)

var (
	// ErrInvalidSource 表示 Manager 没有可用的权威数据源。
	ErrInvalidSource = errors.New("route source is invalid")
	// ErrAlreadyStarted 表示同一个 Manager 被重复启动。
	ErrAlreadyStarted = errors.New("route manager already started")
)

// Source 是 Route Manager 消费 SQLite 权威 Desired State 的最小读取边界。
//
// LoadRouteDesiredState 必须返回一个一致性读事务中的完整状态；
// CurrentRouteGeneration 是候选发布前的轻量 fencing 读取。接口定义在消费方附近，
// 使 sqlite.Store 可以直接实现，而无需让热路径依赖具体数据库类型。
type Source interface {
	LoadRouteDesiredState(context.Context) (repository.RouteDesiredState, error)
	CurrentRouteGeneration(context.Context) (uint64, error)
}

// Manager 拥有路由快照的唯一调和 goroutine 与原子发布点。
//
// 外部写入方只调用 MarkDirty，不参与构建，也不能直接替换 current。dirty Channel
// 容量固定为 1，因此突发提交只保留“需要重建”的事实，不会按提交数量堆积工作。
// owner 由 Start 传入的 Context 停止，done 提供唯一等待退出路径。
type Manager struct {
	source Source

	current atomic.Pointer[Snapshot]
	dirty   chan struct{}
	done    chan struct{}
	// dirtyGeneration 保存写入协调器已提交并通知的最大代次。Channel 只负责唤醒，
	// generation 本身不能放在 Channel 中，否则容量 1 合并时会丢失最新 fencing 值。
	dirtyGeneration atomic.Uint64

	lifecycleMu sync.Mutex
	started     bool

	errorMu   sync.RWMutex
	lastError error
}

// NewManager 创建尚未启动的路由快照管理器。
func NewManager(source Source) (*Manager, error) {
	if source == nil {
		return nil, ErrInvalidSource
	}
	return &Manager{
		source: source,
		dirty:  make(chan struct{}, 1),
		done:   make(chan struct{}),
	}, nil
}

// Start 启动唯一调和 owner，并等待首次完整快照成功发布。
//
// 首次构建失败会终止 owner 并把错误返回给 Bootstrap，确保公网入口不会在没有
// 权威路由视图时继续启动。Start 成功后，后续构建失败只记录 LastError 并保留旧
// 快照；新的 dirty 唤醒会再次尝试收敛。调用方取消 ctx 后应调用 Wait 等待退出。
func (manager *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("start route manager: nil context")
	}
	manager.lifecycleMu.Lock()
	if manager.started {
		manager.lifecycleMu.Unlock()
		return ErrAlreadyStarted
	}
	manager.started = true
	manager.lifecycleMu.Unlock()

	ready := make(chan error, 1)
	go manager.run(ctx, ready)

	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		return fmt.Errorf("start route manager: %w", ctx.Err())
	}
}

// MarkDirty 非阻塞地通知 owner 指定 generation 已经提交。
//
// 多个调用先以 CAS 保留最大 generation，再通过容量 1 Channel 合并唤醒。即使构建
// 正在进行，owner 也能在发布前看到最新 fencing 值，而不会因 Channel 合并而丢代次。
func (manager *Manager) MarkDirty(generation uint64) {
	for {
		observed := manager.dirtyGeneration.Load()
		if generation < observed {
			return
		}
		if generation == observed {
			// 正常构建期间的重复通知继续合并；只有上一次调和已经明确失败时，
			// 才允许同代次重新入队，使运维修复瞬时读取错误后无需伪造新代次。
			if manager.LastError() == nil {
				return
			}
			break
		}
		if manager.dirtyGeneration.CompareAndSwap(observed, generation) {
			break
		}
	}
	select {
	case manager.dirty <- struct{}{}:
	default:
	}
}

// Current 返回当前完整快照。返回 nil 表示首次构建尚未成功。
// Snapshot 自身只暴露值或副本，因此指针可安全供并发读路径长期持有。
func (manager *Manager) Current() *Snapshot {
	return manager.current.Load()
}

// LastError 返回最近一次调和错误；成功发布新快照后清空。
func (manager *Manager) LastError() error {
	manager.errorMu.RLock()
	defer manager.errorMu.RUnlock()
	return manager.lastError
}

// Wait 等待 Start 创建的唯一 owner 退出。
// 调用方必须先调用 Start；owner 会在 Context 取消或首次构建失败时关闭 done。
func (manager *Manager) Wait() {
	<-manager.done
}

// run 是 Manager 全部可变调和状态的唯一 owner。
//
// 它先完成启动构建，再串行处理 dirty；没有其他 goroutine 能并发构建或发布。
// Context 取消会解除遵守契约的 Source 调用，并让 select 退出；defer 关闭 done，
// 使 Bootstrap 能等待 goroutine 完整结束，不留下 orphan goroutine。
func (manager *Manager) run(ctx context.Context, ready chan<- error) {
	defer close(manager.done)

	if err := manager.reconcile(ctx); err != nil {
		manager.setLastError(err)
		ready <- err
		return
	}
	ready <- nil

	for {
		select {
		case <-ctx.Done():
			return
		case <-manager.dirty:
			if err := manager.reconcile(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				manager.setLastError(err)
			}
		}
	}
}

// reconcile 总是从权威 Source 全量构建，不基于旧快照做增量修改。
//
// 候选构建完成后再次读取 generation：若权威代次已经前进，候选立即丢弃并在
// 同一 owner 中重建最新状态；若代次倒退则视为权威不变量破坏。只有 fencing
// 通过的完整候选才能 atomic.Store，因此旧构建结果不会覆盖已观察到的新状态，
// 任一读取或校验失败也只会保留此前发布的完整快照。
func (manager *Manager) reconcile(ctx context.Context) error {
	for {
		state, err := manager.source.LoadRouteDesiredState(ctx)
		if err != nil {
			return fmt.Errorf("load route desired state: %w", err)
		}
		candidate, err := buildSnapshot(state)
		if err != nil {
			return fmt.Errorf("build route snapshot generation %d: %w", state.Generation, err)
		}
		// SQLite 内部损坏或错误恢复可能让本轮读取返回更小代次。已发布快照是
		// 运行时单调下界，必须在 dirty 快速重建判断之前拒绝回退；否则一个更高
		// dirty generation 会让 owner 围绕永久回退的 Source 无界忙循环。
		published := manager.current.Load()
		if published != nil && state.Generation < published.Generation() {
			return fmt.Errorf(
				"%w: candidate generation moved backwards from published %d to %d",
				ErrInvalidDesiredState,
				published.Generation(),
				state.Generation,
			)
		}
		// 构建期间到达的 dirty 只表示“权威状态可能已变化”。先合并该信号，再读取
		// 最新 generation：若确有新提交，下面会立即重建；若只是同代次的重复唤醒，
		// 则无需在发布后再做一次完全相同的全量构建。必须在 fencing 读取之前清空，
		// 否则会误吞掉 fencing 之后提交的新代次唤醒。
		select {
		case <-manager.dirty:
		default:
		}
		latest, err := manager.source.CurrentRouteGeneration(ctx)
		if err != nil {
			return fmt.Errorf("read current route generation: %w", err)
		}
		if latest < state.Generation {
			return fmt.Errorf("%w: generation moved backwards from %d to %d", ErrInvalidDesiredState, state.Generation, latest)
		}
		if latest > state.Generation {
			continue
		}
		// ConfigWriteCoordinator 在提交事务后调用 MarkDirty(generation)。这次原子读取
		// 覆盖 fencing 数据库查询与 atomic.Store 之间的通知竞态；只要已经观察到
		// 更高代次，旧候选就不能短暂覆盖当前快照。
		if manager.dirtyGeneration.Load() > state.Generation {
			continue
		}
		manager.current.Store(candidate)
		// 首次启动可能直接从非零 generation 恢复，且没有对应 MarkDirty 调用。
		// 把已发布代次提升为通知下界，避免后续同代次或旧代次通知重建出不同内容。
		for {
			observed := manager.dirtyGeneration.Load()
			if state.Generation <= observed || manager.dirtyGeneration.CompareAndSwap(observed, state.Generation) {
				break
			}
		}
		manager.setLastError(nil)
		return nil
	}
}

func (manager *Manager) setLastError(err error) {
	manager.errorMu.Lock()
	manager.lastError = err
	manager.errorMu.Unlock()
}
