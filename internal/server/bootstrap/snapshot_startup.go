package bootstrap

import (
	"context"
	"fmt"
	"sort"

	"github.com/lifei6671/xtunnel/internal/repository"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/snapshot"
)

const snapshotProtocolVersion uint32 = 1

// validateStoredSnapshots 在一个只读 Repository 视图中校验全部持久化 Tunnel 的完整 Snapshot。
// 调用方必须已经持有 Stable Data Target External Lock，并在 Migration 完成后、任何
// 进程内写入口或 Public Listener 启动前调用；该生命周期保证多次查询看到静止数据集。
func validateStoredSnapshots(ctx context.Context, config serverconfig.Config, store repository.Store) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("validate stored Tunnel Snapshots before repository read: %w", err)
	}

	builder, err := snapshot.New(snapshot.Config{
		ProtocolVersion:      snapshotProtocolVersion,
		MaxServices:          config.Limits.MaxServicesPerTunnel,
		MaxSnapshotBytes:     config.Limits.MaxTunnelSnapshotBytes,
		MaxControlFrameBytes: config.Limits.MaxControlFrameBytes,
	})
	if err != nil {
		return fmt.Errorf("configure stored Tunnel Snapshot validator: %w", err)
	}

	err = store.Read(ctx, func(view repository.RepositoryView) error {
		tunnels, err := view.Tunnels().List(ctx)
		if err != nil {
			return fmt.Errorf("list stored Tunnels: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("list stored Tunnels: %w", err)
		}

		// Repository 的稳定顺序不能成为启动安全检查的隐式前提；复制后排序也不改写调用方切片。
		tunnels = append([]repository.Tunnel(nil), tunnels...)
		sort.SliceStable(tunnels, func(left, right int) bool {
			return tunnels[left].ID < tunnels[right].ID
		})

		for _, tunnel := range tunnels {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("validate stored Tunnel Snapshot tunnel_id=%s: %w", tunnel.ID, err)
			}
			services, err := view.Services().ListByTunnel(ctx, tunnel.ID)
			if err != nil {
				return fmt.Errorf("list stored Services tunnel_id=%s: %w", tunnel.ID, err)
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("list stored Services tunnel_id=%s: %w", tunnel.ID, err)
			}
			if err := builder.Validate(tunnel.ID, tunnel.DesiredRevision, services); err != nil {
				return fmt.Errorf(
					"validate stored Tunnel Snapshot tunnel_id=%s service_count=%d: %w",
					tunnel.ID,
					len(services),
					err,
				)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate stored Tunnel Snapshots: %w", err)
	}
	return nil
}
