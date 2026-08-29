package application

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// DashboardUsageSummary 在 M6 Usage Read Model 就绪前显式返回不可用。
// nil 计数器对应 OpenAPI 的 null，不能用 0 伪装成真实观测值。
type DashboardUsageSummary struct {
	Availability      DashboardAvailability
	ConnectionsToday  *int64
	IngressBytesToday *int64
	EgressBytesToday  *int64
}

// DashboardRecentError 是未来 M6 Error Read Model 向 Dashboard 提供的稳定错误投影。
type DashboardRecentError struct {
	Code       string
	Message    string
	OccurredAt time.Time
	RequestID  *string
}

// DashboardRecentErrorsSummary 在 M6 Error Read Model 就绪前显式返回空 items。
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

// DashboardServerStatusOwner 提供 Server 生命周期 owner 已判定的值型状态。
// Dashboard 不根据 Tunnel 或 Service 计数重新推导 Server Status。
type DashboardServerStatusOwner interface {
	DashboardServerStatus(context.Context) (DashboardServerStatus, error)
}

var (
	_ DashboardTunnelReader  = (*TunnelManagementService)(nil)
	_ DashboardServiceReader = (*ServiceAPIService)(nil)
)

// DashboardService 聚合已有权威只读投影，不读取或持久化 Runtime 状态。
type DashboardService struct {
	tunnels  DashboardTunnelReader
	services DashboardServiceReader
	status   DashboardServerStatusOwner
	now      func() time.Time
}

// NewDashboardService 绑定 Dashboard 所需的最小只读 owner。
func NewDashboardService(
	tunnels DashboardTunnelReader,
	services DashboardServiceReader,
	status DashboardServerStatusOwner,
) *DashboardService {
	return &DashboardService{tunnels: tunnels, services: services, status: status, now: time.Now}
}

// Snapshot 聚合 Tunnel 与 Service 的权威展示状态。ServicesError 只统计
// APPLY_FAILED；其他非 READY 状态有独立业务语义，留待 M6-05 的诊断聚合区分。
func (service *DashboardService) Snapshot(ctx context.Context) (DashboardSnapshot, error) {
	if !service.valid(ctx) {
		return DashboardSnapshot{}, ErrDashboardInput
	}
	if err := ctx.Err(); err != nil {
		return DashboardSnapshot{}, err
	}
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
		case serverstatus.ServiceStatusApplyFailed:
			counts.ServicesError++
		}
	}

	return DashboardSnapshot{
		ServerStatus: serverStatus,
		Counts:       counts,
		Traffic: DashboardUsageSummary{
			Availability: DashboardAvailabilityUnavailable,
		},
		RecentErrors: DashboardRecentErrorsSummary{
			Availability: DashboardAvailabilityUnavailable,
			Items:        make([]DashboardRecentError, 0),
		},
		GeneratedAt: service.now().UTC(),
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
		service.services != nil && service.status != nil && service.now != nil
}
