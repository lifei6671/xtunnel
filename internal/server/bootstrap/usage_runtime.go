package bootstrap

import (
	"context"
	"errors"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverusage "github.com/lifei6671/xtunnel/internal/server/usage"
)

// serverUsageRepository 是 Bootstrap 唯一的 Usage 持久化适配层。Owner 只认识
// 进程内增量，SQLite Repository 继续拥有事务、Rollup 和 Retention 规则。
type serverUsageRepository struct {
	store *sqlite.Store
}

func (adapter *serverUsageRepository) Flush(ctx context.Context, deltas []serverusage.Delta) error {
	if adapter == nil || adapter.store == nil {
		return errors.New("server usage repository is unavailable")
	}
	persisted := make([]repository.UsageDelta, len(deltas))
	for index, delta := range deltas {
		persisted[index] = repository.UsageDelta{
			Bucket: delta.BucketTime, TunnelID: delta.TunnelID, ServiceID: delta.ServiceID,
			Connections: delta.Connections, IngressBytes: delta.IngressBytes,
			EgressBytes: delta.EgressBytes, Errors: delta.Errors,
		}
	}
	return adapter.store.Flush(ctx, persisted)
}

func (adapter *serverUsageRepository) Rollup(ctx context.Context, completedBefore time.Time) error {
	if adapter == nil || adapter.store == nil {
		return errors.New("server usage repository is unavailable")
	}
	return adapter.store.Rollup(ctx, completedBefore)
}

// serverUsageBridge 保持 Tunnel 只依赖最小接口；所有计数错误原样返回数据面，
// 让容量耗尽、停止 Fence 或溢出快速关闭连接，禁止继续运行并静默丢账。
type serverUsageBridge struct {
	owner *serverusage.Owner
}

func (bridge *serverUsageBridge) ObserveOpen(tunnelID, serviceID string, success bool) error {
	if bridge == nil || bridge.owner == nil {
		return errors.New("server usage owner is unavailable")
	}
	return bridge.owner.ObserveOpen(tunnelID, serviceID, success)
}

func (bridge *serverUsageBridge) AddIngressBytes(tunnelID, serviceID string, count uint64) error {
	if bridge == nil || bridge.owner == nil {
		return errors.New("server usage owner is unavailable")
	}
	return bridge.owner.AddIngressBytes(tunnelID, serviceID, count)
}

func (bridge *serverUsageBridge) AddEgressBytes(tunnelID, serviceID string, count uint64) error {
	if bridge == nil || bridge.owner == nil {
		return errors.New("server usage owner is unavailable")
	}
	return bridge.owner.AddEgressBytes(tunnelID, serviceID, count)
}
