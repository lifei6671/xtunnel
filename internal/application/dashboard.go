package application

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	serverrecenterror "github.com/lifei6671/xtunnel/internal/server/recenterror"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
)

var (
	// ErrDashboardInput 表示 Dashboard 聚合依赖或调用上下文无效。
	ErrDashboardInput = errors.New("dashboard input is invalid")
	// ErrDashboardServerStatus 表示 Server Status owner 返回了契约外状态。
	ErrDashboardServerStatus = errors.New("dashboard server status is invalid")
)

// DashboardServerStatus 是 Server owner 发布的权威 Dashboard 状态。
type DashboardServerStatus string

const (
	DashboardServerStatusReady    DashboardServerStatus = "READY"
	DashboardServerStatusDegraded DashboardServerStatus = "DEGRADED"
)

// DashboardAvailability 表示一个 M6 Read Model 当前是否可用。
type DashboardAvailability string

const (
	DashboardAvailabilityAvailable   DashboardAvailability = "AVAILABLE"
	DashboardAvailabilityUnavailable DashboardAvailability = "UNAVAILABLE"
)

// DashboardCounts 汇总已有 Tunnel、Service 和 Runtime owner 的权威投影。
type DashboardCounts struct {
	TunnelsTotal      uint64
	TunnelsOnline     uint64
	TunnelsOffline    uint64
	ConnectorsOnline  uint64
	ServicesTotal     uint64
	ServicesReady     uint64
	ServicesError     uint64
	ActiveConnections uint64
}

// DashboardUsageSummary 投影当日全局 Usage；AVAILABLE 时三个计数器均非 nil。
type DashboardUsageSummary struct {
	Availability      DashboardAvailability
	ConnectionsToday  *int64
	IngressBytesToday *int64
	EgressBytesToday  *int64
}

// DashboardRecentError 是 M6 Error Read Model 向 Dashboard 提供的稳定错误投影。
type DashboardRecentError struct {
	Code       string
	Message    string
	OccurredAt time.Time
	RequestID  *string
}

// DashboardRecentErrorsSummary 以 AVAILABLE 区分已就绪但暂时无错误的空投影。
type DashboardRecentErrorsSummary struct {
	Availability DashboardAvailability
	Items        []DashboardRecentError
}

// DashboardSnapshot 是一次请求内形成的只读值快照。各 owner 不承诺跨方法原子一致，
// 因此它与 Tunnel/Service API 一样允许并发变更期间短暂最终一致；聚合完成后不保留
// owner 返回切片或 Runtime 对象的引用。
type DashboardSnapshot struct {
	ServerStatus DashboardServerStatus
	Counts       DashboardCounts
	Traffic      DashboardUsageSummary
	RecentErrors DashboardRecentErrorsSummary
	GeneratedAt  time.Time
}

// DashboardTunnelReader 复用 Tunnel Management API 已完成状态投影的列表入口。
type DashboardTunnelReader interface {
	List(context.Context) ([]TunnelView, error)
}

// DashboardServiceReader 复用 Service API 已完成状态投影的列表入口。
type DashboardServiceReader interface {
	ListAll(context.Context) ([]ServiceView, error)
}

// DashboardUsageReader 独立读取全局 Usage，避免从 Service 投影循环汇总。
type DashboardUsageReader interface {
	UsageToday(context.Context, time.Time) (repository.UsageTotals, error)
}

// DashboardServerStatusOwner 提供 Server 生命周期 owner 已判定的值型状态。
// Dashboard 不根据 Tunnel 或 Service 计数重新推导 Server Status。
type DashboardServerStatusOwner interface {
	DashboardServerStatus(context.Context) (DashboardServerStatus, error)
}

// DashboardRecentErrorReader 返回进程内固定五槽的不可变最新错误投影。
type DashboardRecentErrorReader interface {
	Snapshot() []serverrecenterror.Item
}

var (
	_ DashboardTunnelReader  = (*TunnelManagementService)(nil)
	_ DashboardServiceReader = (*ServiceAPIService)(nil)
	_ DashboardUsageReader   = (*ServiceAPIService)(nil)
)

// DashboardService 聚合已有权威只读投影，不读取或持久化 Runtime 状态。
type DashboardService struct {
	tunnels  DashboardTunnelReader
	services DashboardServiceReader
	status   DashboardServerStatusOwner
	usage    DashboardUsageReader
	errors   DashboardRecentErrorReader
	now      func() time.Time
}

// NewDashboardService 绑定 Dashboard 所需的最小只读 owner。
func NewDashboardService(
	tunnels DashboardTunnelReader,
	services DashboardServiceReader,
	status DashboardServerStatusOwner,
	usage DashboardUsageReader,
	recentErrors DashboardRecentErrorReader,
) *DashboardService {
	return &DashboardService{
		tunnels: tunnels, services: services, status: status, usage: usage,
		errors: recentErrors, now: time.Now,
	}
}

// Snapshot 聚合 Tunnel 与 Service 的权威展示状态。ServicesError 只统计稳定故障态；
// DISABLED 与 CONFIG_SYNCING 分别是管理选择和暂态，不得伪装成错误。
func (service *DashboardService) Snapshot(ctx context.Context) (DashboardSnapshot, error) {
	if !service.valid(ctx) {
		return DashboardSnapshot{}, ErrDashboardInput
	}
	if err := ctx.Err(); err != nil {
		return DashboardSnapshot{}, err
	}
	generatedAt := service.now().UTC()
	serverStatus, err := service.status.DashboardServerStatus(ctx)
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("read dashboard server status: %w", err)
	}
	if serverStatus != DashboardServerStatusReady && serverStatus != DashboardServerStatusDegraded {
		return DashboardSnapshot{}, fmt.Errorf("%w: %q", ErrDashboardServerStatus, serverStatus)
	}

	tunnels, err := service.tunnels.List(ctx)
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("read dashboard tunnels: %w", err)
	}
	services, err := service.services.ListAll(ctx)
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("read dashboard services: %w", err)
	}
	usage, err := service.usage.UsageToday(ctx, generatedAt)
	if err != nil {
		return DashboardSnapshot{}, fmt.Errorf("read dashboard usage: %w", err)
	}
	traffic, err := dashboardUsageSummary(usage)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	counts := DashboardCounts{TunnelsTotal: uint64(len(tunnels))}
	for _, tunnel := range tunnels {
		switch tunnel.Status {
		case serverstatus.TunnelStatusOnline:
			counts.TunnelsOnline++
		case serverstatus.TunnelStatusOffline:
			counts.TunnelsOffline++
		}
		counts.ConnectorsOnline += tunnel.ConnectorsOnline
		counts.ActiveConnections += tunnel.ActiveConnections
	}
	counts.ServicesTotal = uint64(len(services))
	for _, serviceView := range services {
		switch serviceView.Status {
		case serverstatus.ServiceStatusReady:
			counts.ServicesReady++
		case serverstatus.ServiceStatusApplyFailed,
			serverstatus.ServiceStatusTunnelOffline,
			serverstatus.ServiceStatusOriginUnhealthy,
			serverstatus.ServiceStatusNoCapacity:
			counts.ServicesError++
		}
	}
	recentErrorItems := service.errors.Snapshot()
	recentErrors := make([]DashboardRecentError, 0, len(recentErrorItems))
	for _, item := range recentErrorItems {
		recentErrors = append(recentErrors, DashboardRecentError{
			Code: string(item.Code), Message: item.Message,
			OccurredAt: item.OccurredAt.UTC(), RequestID: item.RequestID,
		})
	}

	return DashboardSnapshot{
		ServerStatus: serverStatus,
		Counts:       counts,
		Traffic:      traffic,
		RecentErrors: DashboardRecentErrorsSummary{
			Availability: DashboardAvailabilityAvailable,
			Items:        recentErrors,
		},
		GeneratedAt: generatedAt,
	}, nil
}

// UsageToday 在一次 Repository Read 内读取全局今日 Usage，供 Dashboard 独立消费。
func (service *ServiceAPIService) UsageToday(ctx context.Context, now time.Time) (repository.UsageTotals, error) {
	if !service.validQuery(ctx) || now.IsZero() {
		return repository.UsageTotals{}, ErrServiceManagementInput
	}
	if err := ctx.Err(); err != nil {
		return repository.UsageTotals{}, err
	}
	var usage repository.UsageTotals
	if err := service.owner.store.Read(ctx, func(view repository.RepositoryView) error {
		var err error
		usage, err = view.Usage().Today(ctx, now.UTC(), "", "")
		return err
	}); err != nil {
		return repository.UsageTotals{}, fmt.Errorf("read global usage: %w", err)
	}
	return usage, nil
}

func dashboardUsageSummary(usage repository.UsageTotals) (DashboardUsageSummary, error) {
	if usage.Connections > math.MaxInt64 || usage.IngressBytes > math.MaxInt64 || usage.EgressBytes > math.MaxInt64 {
		return DashboardUsageSummary{}, repository.ErrUsageOverflow
	}
	connections := int64(usage.Connections)
	ingress := int64(usage.IngressBytes)
	egress := int64(usage.EgressBytes)
	return DashboardUsageSummary{
		Availability: DashboardAvailabilityAvailable, ConnectionsToday: &connections,
		IngressBytesToday: &ingress, EgressBytesToday: &egress,
	}, nil
}

// ListAll 为 Dashboard 在一次 Repository/Runtime 投影中读取全部 Service，避免按
// Tunnel 重复抓取快照。状态仍只由既有 Service API 和 server/status 计算。
func (service *ServiceAPIService) ListAll(ctx context.Context) ([]ServiceView, error) {
	if !service.validQuery(ctx) {
		return nil, ErrServiceManagementInput
	}
	return service.readViews(ctx, "", "")
}

func (service *DashboardService) valid(ctx context.Context) bool {
	return service != nil && ctx != nil && service.tunnels != nil &&
		service.services != nil && service.status != nil && service.usage != nil &&
		service.errors != nil && service.now != nil
}
