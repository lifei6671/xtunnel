package connector

import (
	"context"
	"errors"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	agenthealth "github.com/lifei6671/xtunnel/internal/agent/health"
	"github.com/lifei6671/xtunnel/internal/agent/open"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

// 本文件是 Connector 配置应用链中 Origin 与 Health 的组合层。
//
// configruntime 只认识一个 Builder/Candidate/Resources，但一个 Snapshot 实际会同时
// 产生两类资源：Origin 保存本代 Service 的不可变连接目标，Health 保存基于这些目标的
// 检查计划。组合层保证它们要么作为同一代配置一起发布，要么按依赖关系一起回收，避免
// 出现“Health 已切到新配置、拨号却仍读取旧 Origin”之类的跨代状态。
//
// 生命周期顺序固定为：
//
//	Build:   Origin -> Health
//	Start:   Origin -> Health
//	Cleanup: Health -> Origin
//
// Health 依赖 Origin Candidate 提供的 scoped Dialer，因此构建和启动必须先 Origin；
// 清理则反向执行，确保 Health 不会在 Origin 已注销后继续发起检查。

// healthPreparer 构造尚未发布的 Health 计划。Origin Candidate 提供与该
// Snapshot generation 绑定的 Dialer，避免旧检查在切换竞态中拨到新 Origin。
type healthPreparer interface {
	Prepare(context.Context, *protocolv1.TunnelSnapshot, configruntime.Gate, agenthealth.OriginDialer) (configruntime.Candidate, error)
}

// snapshotBuilder 把 Origin 与 Health 作为同一个 configruntime Candidate 原子应用。
// 它只协调生命周期，不复制两个子系统的校验或运行时状态。
type snapshotBuilder struct {
	origin configruntime.Builder
	health healthPreparer
}

// Build 为同一份 Snapshot 构造 Origin 与 Health 两个尚未发布的 Candidate。
// Build 本身不会让任何资源对业务流量可见；真正的可见性仍由 configruntime.Gate 控制。
//
// 即使中途失败，也尽量返回已经构造出的组合 Candidate。外层 configruntime 会对非 nil
// Candidate 调用 Abort，从而集中完成半成品清理，避免各错误分支各自维护一套回滚逻辑。
func (builder snapshotBuilder) Build(
	ctx context.Context,
	snapshot *protocolv1.TunnelSnapshot,
	gate configruntime.Gate,
) (configruntime.Candidate, error) {
	if builder.origin == nil || builder.health == nil {
		return nil, ErrInvalidConfig
	}
	// Origin Candidate 先完成 Snapshot 语义校验并冻结本代连接目标。
	originCandidate, err := builder.origin.Build(ctx, snapshot, gate)
	if err != nil {
		return originCandidate, err
	}
	// Health 不从全局 Resolver 查目标，只允许使用刚构造的 Origin Candidate。
	// 这样旧 generation 的检查即使稍晚开始或仍在执行，也不会因全局配置切换而改拨
	// 新 generation 的地址。
	scopedDialer, ok := originCandidate.(open.OriginDialer)
	if !ok {
		return &snapshotCandidate{origin: originCandidate}, ErrInvalidConfig
	}
	healthCandidate, healthErr := builder.health.Prepare(ctx, snapshot, gate, scopedDialer)
	candidate := &snapshotCandidate{origin: originCandidate, health: healthCandidate}
	if healthErr != nil {
		return candidate, healthErr
	}
	if healthCandidate == nil {
		return candidate, ErrInvalidConfig
	}
	return candidate, nil
}

// snapshotCandidate 表示一代尚未发布的完整 Connector 配置。
// 两个子 Candidate 必须共用同一个 Gate，并作为一个整体 Start、Abort 和转换为 Runtime。
type snapshotCandidate struct {
	origin configruntime.Candidate
	health configruntime.Candidate
}

// Start 按依赖顺序准备资源：先注册 Origin，再启动可能发起拨号的 Health 计划。
// 若 Health 启动失败，调用方会随后调用 Abort，按 Health -> Origin 逆序清理。
func (candidate *snapshotCandidate) Start(ctx context.Context) error {
	if candidate == nil || candidate.origin == nil || candidate.health == nil {
		return ErrInvalidConfig
	}
	if err := candidate.origin.Start(ctx); err != nil {
		return err
	}
	return candidate.health.Start(ctx)
}

// Abort 清理尚未发布或启动失败的组合 Candidate。
// 两个清理动作都必须尝试，因此使用 errors.Join 保留双方错误，而不是因 Health 清理失败
// 就跳过 Origin 清理。逆序清理同时保证依赖方 Health 先停止。
func (candidate *snapshotCandidate) Abort(ctx context.Context) error {
	if candidate == nil {
		return ErrInvalidConfig
	}
	var healthErr error
	if candidate.health != nil {
		healthErr = candidate.health.Abort(ctx)
	}
	var originErr error
	if candidate.origin != nil {
		originErr = candidate.origin.Abort(ctx)
	}
	return errors.Join(healthErr, originErr)
}

// Runtime 把已经 Start 完成的两个 Candidate 转换为 configruntime 持有的资源句柄。
// 这里采用 all-or-nothing：任一子系统拿不到 Resources 都返回 nil，禁止发布只有 Origin
// 或只有 Health 的残缺配置代。
func (candidate *snapshotCandidate) Runtime() configruntime.Resources {
	if candidate == nil || candidate.origin == nil || candidate.health == nil {
		return nil
	}
	originResources := candidate.origin.Runtime()
	healthResources := candidate.health.Runtime()
	if originResources == nil || healthResources == nil {
		return nil
	}
	return &snapshotResources{origin: originResources, health: healthResources}
}

// snapshotResources 是一代已发布配置的组合资源句柄。它不保存业务状态，只负责在该代
// Snapshot 被新代替换或 Runtime 关闭时，协调两个子系统的最终回收。
type snapshotResources struct {
	origin configruntime.Resources
	health configruntime.Resources
}

// Retire 按 Health -> Origin 逆序退休已发布资源。
// 与 Abort 一样，即使一个子系统返回错误也继续回收另一个，并把错误链完整交给上层。
func (resources *snapshotResources) Retire(ctx context.Context) error {
	if resources == nil {
		return ErrInvalidConfig
	}
	var healthErr error
	if resources.health != nil {
		healthErr = resources.health.Retire(ctx)
	}
	var originErr error
	if resources.origin != nil {
		originErr = resources.origin.Retire(ctx)
	}
	return errors.Join(healthErr, originErr)
}

var _ configruntime.Builder = snapshotBuilder{}
