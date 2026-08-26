package snapshot

import (
	"context"
	"errors"
	"fmt"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/repository"
)

var (
	// ErrInvalidSource 表示 Source 依赖或调用输入无效。
	ErrInvalidSource = errors.New("snapshot source is invalid")
	// ErrTunnelRevoked 表示 Tunnel 已被撤销，不能再向 Connector 发布配置。
	ErrTunnelRevoked = errors.New("snapshot source tunnel is revoked")
)

// ConsistentReader 为生产 Snapshot 定义最小跨表一致读取边界。
type ConsistentReader interface {
	ReadConsistent(context.Context, func(repository.RepositoryView) error) error
}

// Source 从持久化的一致视图构建当前完整 Tunnel Snapshot。
type Source struct {
	reader  ConsistentReader
	builder *Builder
}

// NewSource 创建只依赖一致读取边界和现有 Builder 的生产 Snapshot Source。
func NewSource(reader ConsistentReader, builder *Builder) (*Source, error) {
	if reader == nil || builder == nil {
		return nil, ErrInvalidSource
	}
	return &Source{reader: reader, builder: builder}, nil
}

// Current 在单次一致视图内读取完整 Service 集合和 Tunnel Revision，并构建 Snapshot。
func (source *Source) Current(ctx context.Context, tunnelID string) (Result, error) {
	if source == nil || source.reader == nil || source.builder == nil || ctx == nil || !identity.ValidTunnelID(tunnelID) {
		return Result{}, ErrInvalidSource
	}

	var result Result
	if err := source.reader.ReadConsistent(ctx, func(view repository.RepositoryView) error {
		services, err := view.Services().ListByTunnel(ctx, tunnelID)
		if err != nil {
			return fmt.Errorf("list current snapshot services for tunnel %q: %w", tunnelID, err)
		}
		tunnel, err := view.Tunnels().Get(ctx, tunnelID)
		if err != nil {
			return fmt.Errorf("read current snapshot tunnel %q: %w", tunnelID, err)
		}
		if err := tunnel.Validate(); err != nil || tunnel.ID != tunnelID {
			return fmt.Errorf("%w: stored tunnel %q", ErrInvalidSnapshot, tunnelID)
		}
		if tunnel.RevokedAt != nil {
			return fmt.Errorf("%w: tunnel %q", ErrTunnelRevoked, tunnelID)
		}

		result, err = source.builder.Build(tunnel.ID, tunnel.DesiredRevision, services)
		if err != nil {
			return fmt.Errorf("build current snapshot for tunnel %q: %w", tunnelID, err)
		}
		return nil
	}); err != nil {
		return Result{}, err
	}
	return result, nil
}
