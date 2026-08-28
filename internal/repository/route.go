package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/lifei6671/xtunnel/internal/identity"
)

var (
	// ErrInvalidRoute 表示持久化 Route 的字段不符合当前领域约束。
	ErrInvalidRoute = errors.New("route is invalid")
)

// HTTPRoute 是一个 Host + Path Prefix 到 Service 的期望路由。
// Route ID 的外部格式尚未冻结，因此这里只要求它是非空稳定标识，不引入前缀契约。
type HTTPRoute struct {
	// ID 是 Route 的稳定主键；V0.1 尚未冻结其外部前缀格式。
	ID string
	// ServiceID 指向该 Route 唯一绑定的 Service。
	ServiceID string
	// Hostname 与 PathPrefix 共同组成 HTTP Route 的唯一匹配键。
	Hostname   string
	PathPrefix string
	// PreserveHost 决定后续反向代理是否保留公网请求 Host。
	PreserveHost bool
	// Enabled=false 时 Desired State 仍保留该行，但运行时快照不发布它。
	Enabled bool
	// CreatedAt、UpdatedAt 使用 UTC Unix 秒。
	CreatedAt int64
	UpdatedAt int64
}

// Validate 检查 HTTP Route 从 SQLite 进入快照构建边界时必须满足的不变量。
func (route HTTPRoute) Validate() error {
	if strings.TrimSpace(route.ID) == "" || !identity.ValidServiceID(route.ServiceID) ||
		strings.TrimSpace(route.Hostname) == "" || !strings.HasPrefix(route.PathPrefix, "/") ||
		route.CreatedAt <= 0 || route.UpdatedAt <= 0 {
		return ErrInvalidRoute
	}
	return nil
}

// TCPRoute 是一个公开 TCP 端口到 Service 的期望路由。
type TCPRoute struct {
	// ID 是 Route 的稳定主键；V0.1 尚未冻结其外部前缀格式。
	ID string
	// ServiceID 指向该 Route 唯一绑定的 Service。
	ServiceID string
	// PublicPort 是 Server 对公网监听的唯一 TCP 端口。
	PublicPort uint16
	// Enabled=false 时 Desired State 仍保留该行，但运行时快照不发布它。
	Enabled bool
	// CreatedAt、UpdatedAt 使用 UTC Unix 秒。
	CreatedAt int64
	UpdatedAt int64
}

// Validate 检查 TCP Route 从 SQLite 进入快照构建边界时必须满足的不变量。
func (route TCPRoute) Validate() error {
	if strings.TrimSpace(route.ID) == "" || !identity.ValidServiceID(route.ServiceID) ||
		route.PublicPort == 0 || route.CreatedAt <= 0 || route.UpdatedAt <= 0 {
		return ErrInvalidRoute
	}
	return nil
}

// RouteDesiredState 是一次 SQLite 一致性事务读取的完整 Route Desired State。
// 各切片都由 Repository 独立分配；运行时快照的不可变封装由构建层负责。
type RouteDesiredState struct {
	// Generation 是本次一致性读取对应的全局配置代次。
	Generation uint64
	// Tunnels、Services 是 Route 关联与运行时 revision fencing 的完整输入。
	Tunnels  []Tunnel
	Services []Service
	// HTTPRoutes、TCPRoutes 是两个产品入口的完整 Desired State，而非增量。
	HTTPRoutes []HTTPRoute
	TCPRoutes  []TCPRoute
}

// RouteRepository 定义 Route Desired State 的读写持久化边界。
// Repository 实现不得自行开启事务；完整读取必须由 Store 的一致性读边界包裹，
// Mutation 必须从 Store.WithTx 进入并在同一事务推进全局 Generation。
type RouteRepository interface {
	LoadDesiredState(context.Context) (RouteDesiredState, error)
	CurrentGeneration(context.Context) (uint64, error)
	GetTCP(context.Context, string) (TCPRoute, error)
	ListTCP(context.Context) ([]TCPRoute, error)
	CreateTCP(context.Context, TCPRoute) error
	UpdateTCP(context.Context, TCPRoute) error
	DeleteTCP(context.Context, string) error
	AdvanceGeneration(context.Context, uint64) (uint64, error)
}
