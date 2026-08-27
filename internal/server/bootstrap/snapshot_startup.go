package bootstrap

import (
	"context"
	"fmt"
	"sort"

	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/snapshot"
)

const snapshotProtocolVersion uint32 = 1

// initializeStoredSnapshotsAndHealthBudget 在同一个只读 Repository 视图中完成存量
// Snapshot Gate 与 Health Target 启动重建。返回的 Manager 必须原样注入 Runtime
// Registry。调用方必须已经持有 Stable Data Target External Lock，并在 Migration
// 完成后、任何进程内写入口、Gateway 或本机 Bootstrap Listener 启动前调用。
func initializeStoredSnapshotsAndHealthBudget(
	ctx context.Context,
	config serverconfig.Config,
	store repository.Store,
) (*healthbudget.Manager, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("validate stored Tunnel Snapshots before repository read: %w", err)
	}

	builder, err := snapshot.New(snapshot.Config{
		ProtocolVersion:      snapshotProtocolVersion,
		MaxServices:          config.Limits.MaxServicesPerTunnel,
		MaxSnapshotBytes:     config.Limits.MaxTunnelSnapshotBytes,
		MaxControlFrameBytes: config.Limits.MaxControlFrameBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("configure stored Tunnel Snapshot validator: %w", err)
	}
	budget, err := healthbudget.New(healthbudget.Options{
		MaxTargetsPerTunnel: uint64(config.Limits.MaxHealthTargetsPerTunnel),
		MaxTargetsGlobal:    uint64(config.Limits.MaxHealthTargetsGlobal),
	})
	if err != nil {
		return nil, fmt.Errorf("construct startup Health Target budget: %w", err)
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
			var enabledCount uint64
			for _, service := range services {
				if service.Enabled && service.Health != nil {
					enabledCount++
				}
			}
			if err := budget.InitializeTunnel(tunnel.ID, uint64(tunnel.DesiredRevision), enabledCount); err != nil {
				return fmt.Errorf(
					"initialize stored Health Target budget tunnel_id=%s desired_revision=%d enabled_count=%d: %w",
					tunnel.ID,
					tunnel.DesiredRevision,
					enabledCount,
					err,
				)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("validate stored Tunnel Snapshots and initialize Health Target budget: %w", err)
	}
	return budget, nil
}
