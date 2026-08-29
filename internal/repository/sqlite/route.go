package sqlite

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	// RouteConfigStateTable 是单行全局 Route Generation 的固定表名。
	RouteConfigStateTable = "route_config_state"
	// HTTPRouteTable 是 HTTP Route Desired State 的固定表名。
	HTTPRouteTable = "http_routes"
	// TCPRouteTable 是 TCP Route Desired State 的固定表名。
	TCPRouteTable = "tcp_routes"
)

const singletonRouteConfigStateID = 1

// routeConfigStateRecord 是全局 Route Generation 的单行 SQLite 映射。
type routeConfigStateRecord struct {
	ID         int   `gorm:"column:id;primaryKey"`
	Generation int64 `gorm:"column:generation"`
}

// TableName 把全局 Route Generation 固定到唯一权威表。
func (routeConfigStateRecord) TableName() string { return RouteConfigStateTable }

// httpRouteRecord 是 HTTP Route Desired State 的 SQLite 存储形状。
type httpRouteRecord struct {
	ID           string `gorm:"column:id;primaryKey"`
	ServiceID    string `gorm:"column:service_id"`
	Hostname     string `gorm:"column:hostname"`
	PathPrefix   string `gorm:"column:path_prefix"`
	PreserveHost bool   `gorm:"column:preserve_host"`
	Enabled      bool   `gorm:"column:enabled"`
	CreatedAt    int64  `gorm:"column:created_at"`
	UpdatedAt    int64  `gorm:"column:updated_at"`
}

// TableName 把 HTTP Route 映射固定到机器契约定义的表。
func (httpRouteRecord) TableName() string { return HTTPRouteTable }

// tcpRouteRecord 是 TCP Route Desired State 的 SQLite 存储形状。
type tcpRouteRecord struct {
	ID         string `gorm:"column:id;primaryKey"`
	ServiceID  string `gorm:"column:service_id"`
	PublicPort uint16 `gorm:"column:public_port"`
	Enabled    bool   `gorm:"column:enabled"`
	CreatedAt  int64  `gorm:"column:created_at"`
	UpdatedAt  int64  `gorm:"column:updated_at"`
}

// TableName 把 TCP Route 映射固定到机器契约定义的表。
func (tcpRouteRecord) TableName() string { return TCPRouteTable }

// serviceExposureRecord 是一次 UNION ALL 查询的内部投影，使 HTTP/TCP 类型切换
// 即使发生在普通只读视图旁边，也只能看到同一个 SQLite Statement Snapshot。
type serviceExposureRecord struct {
	Kind         string
	ID           string
	ServiceID    string
	Hostname     string
	PathPrefix   string
	PreserveHost bool
	PublicPort   uint16
	Enabled      bool
	CreatedAt    int64
	UpdatedAt    int64
}

// routeRepository 绑定当前只读视图或事务连接。写入只能由 ConfigWriteCoordinator
// 在 BEGIN IMMEDIATE 中统一提交；Application 层负责把所属 Service/Tunnel 版本与
// 全局 Route Generation 一并推进。
type routeRepository struct {
	database *gorm.DB
	readOnly bool
}

var _ repository.RouteRepository = routeRepository{}

// LoadRouteDesiredState 在同一个 SQLite 只读事务中加载完整 Route Desired State。
// 该便捷入口负责建立一致性边界，防止调用方先后从不同提交读取 Generation、Tunnel、
// Service 和 Route，再把互不匹配的数据拼成候选快照。
func (store *Store) LoadRouteDesiredState(ctx context.Context) (repository.RouteDesiredState, error) {
	var state repository.RouteDesiredState
	if err := store.ReadConsistent(ctx, func(view repository.RepositoryView) error {
		loaded, err := view.Routes().LoadDesiredState(ctx)
		if err != nil {
			return err
		}
		state = loaded
		return nil
	}); err != nil {
		return repository.RouteDesiredState{}, fmt.Errorf("load consistent route desired state: %w", err)
	}
	return state, nil
}

// CurrentRouteGeneration 轻量读取 SQLite 当前提交的 Generation，供候选快照发布前
// 执行 fencing。它不持有写锁；若值已推进，构建层必须丢弃旧候选并重建最新状态。
func (store *Store) CurrentRouteGeneration(ctx context.Context) (uint64, error) {
	var generation uint64
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		current, err := view.Routes().CurrentGeneration(ctx)
		if err != nil {
			return err
		}
		generation = current
		return nil
	}); err != nil {
		return 0, fmt.Errorf("read current route generation: %w", err)
	}
	return generation, nil
}

// LoadDesiredState 按稳定次序读取一次完整 Desired State。调用方必须从
// Store.LoadRouteDesiredState 进入，确保下面的多次查询共享同一个 WAL 快照。
func (store routeRepository) LoadDesiredState(ctx context.Context) (repository.RouteDesiredState, error) {
	generation, err := store.CurrentGeneration(ctx)
	if err != nil {
		return repository.RouteDesiredState{}, err
	}

	tunnels, err := (tunnelRepository{database: store.database, readOnly: true}).List(ctx)
	if err != nil {
		return repository.RouteDesiredState{}, fmt.Errorf("load route tunnels: %w", err)
	}

	services, err := store.loadServices(ctx)
	if err != nil {
		return repository.RouteDesiredState{}, err
	}
	httpRoutes, err := store.loadHTTPRoutes(ctx)
	if err != nil {
		return repository.RouteDesiredState{}, err
	}
	tcpRoutes, err := store.loadTCPRoutes(ctx)
	if err != nil {
		return repository.RouteDesiredState{}, err
	}

	return repository.RouteDesiredState{
		Generation: generation,
		Tunnels:    tunnels,
		Services:   services,
		HTTPRoutes: httpRoutes,
		TCPRoutes:  tcpRoutes,
	}, nil
}

// CurrentGeneration 只读取单行全局 Generation；缺行或非法负数都表示数据库权威
// 状态已损坏，必须阻止发布，而不是将其猜测为零。
func (store routeRepository) CurrentGeneration(ctx context.Context) (uint64, error) {
	var record routeConfigStateRecord
	if err := store.database.WithContext(ctx).
		Where("id = ?", singletonRouteConfigStateID).
		Take(&record).Error; err != nil {
		return 0, fmt.Errorf("read route config generation: %w", err)
	}
	if record.Generation < 0 {
		return 0, fmt.Errorf("stored route config generation is negative: %d", record.Generation)
	}
	return uint64(record.Generation), nil
}

// GetExposureByService 以一条 UNION 查询读取 Service 唯一的 HTTP 或 TCP Exposure，
// 避免类型切换提交夹在两次查询之间。数据库约束负责阻止新重复；这里仍验证查询
// 行数，避免损坏的历史状态被静默解释为任一类型。
func (store routeRepository) GetExposureByService(ctx context.Context, serviceID string) (repository.ServiceExposure, error) {
	if !identity.ValidServiceID(serviceID) {
		return repository.ServiceExposure{}, repository.ErrInvalidRoute
	}

	var records []serviceExposureRecord
	if err := store.database.WithContext(ctx).Raw(`
		SELECT 'http' AS kind, id, service_id, hostname, path_prefix, preserve_host,
		       0 AS public_port, enabled, created_at, updated_at
		FROM http_routes WHERE service_id = ?
		UNION ALL
		SELECT 'tcp' AS kind, id, service_id, '' AS hostname, '' AS path_prefix,
		       0 AS preserve_host, public_port, enabled, created_at, updated_at
		FROM tcp_routes WHERE service_id = ?
		LIMIT 2`, serviceID, serviceID).Scan(&records).Error; err != nil {
		return repository.ServiceExposure{}, fmt.Errorf("get exposure by service: %w", err)
	}
	if len(records) == 0 {
		return repository.ServiceExposure{}, repository.ErrNotFound
	}
	if len(records) != 1 {
		return repository.ServiceExposure{}, fmt.Errorf("stored service %q exposure is not unique: %w", serviceID, repository.ErrInvalidRoute)
	}

	exposure := repository.ServiceExposure{}
	record := records[0]
	switch record.Kind {
	case "http":
		route, err := httpRouteDomain(httpRouteRecord{
			ID: record.ID, ServiceID: record.ServiceID, Hostname: record.Hostname,
			PathPrefix: record.PathPrefix, PreserveHost: record.PreserveHost, Enabled: record.Enabled,
			CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		})
		if err != nil {
			return repository.ServiceExposure{}, fmt.Errorf("stored HTTP route %q is invalid: %w", record.ID, err)
		}
		exposure.HTTP = &route
	case "tcp":
		route, err := tcpRouteDomain(tcpRouteRecord{
			ID: record.ID, ServiceID: record.ServiceID, PublicPort: record.PublicPort,
			Enabled: record.Enabled, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		})
		if err != nil {
			return repository.ServiceExposure{}, fmt.Errorf("stored TCP route %q is invalid: %w", record.ID, err)
		}
		exposure.TCP = &route
	default:
		return repository.ServiceExposure{}, fmt.Errorf("stored service %q exposure type %q is invalid: %w", serviceID, record.Kind, repository.ErrInvalidRoute)
	}
	return exposure, nil
}

// GetHTTP 按稳定 Route ID 读取单条 HTTP Desired Route。
func (store routeRepository) GetHTTP(ctx context.Context, id string) (repository.HTTPRoute, error) {
	if strings.TrimSpace(id) == "" {
		return repository.HTTPRoute{}, repository.ErrInvalidRoute
	}
	var record httpRouteRecord
	if err := store.database.WithContext(ctx).Where("id = ?", id).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.HTTPRoute{}, repository.ErrNotFound
		}
		return repository.HTTPRoute{}, fmt.Errorf("get HTTP route: %w", err)
	}
	return httpRouteDomain(record)
}

// CreateHTTP 在当前写事务中插入经过领域校验的 HTTP Route。
func (store routeRepository) CreateHTTP(ctx context.Context, route repository.HTTPRoute) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if err := route.Validate(); err != nil {
		return err
	}
	if err := store.database.WithContext(ctx).Create(httpRouteRecordFromDomain(route)).Error; err != nil {
		return fmt.Errorf("create HTTP route: %w", err)
	}
	return nil
}

// UpdateHTTP 以全量替换方式更新可变字段；调用方在同一事务内推进 Generation。
func (store routeRepository) UpdateHTTP(ctx context.Context, route repository.HTTPRoute) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if err := route.Validate(); err != nil {
		return err
	}
	result := store.database.WithContext(ctx).Model(&httpRouteRecord{}).
		Where("id = ?", route.ID).
		Updates(map[string]any{
			"service_id":    route.ServiceID,
			"hostname":      route.Hostname,
			"path_prefix":   route.PathPrefix,
			"preserve_host": route.PreserveHost,
			"enabled":       route.Enabled,
			"updated_at":    route.UpdatedAt,
		})
	if result.Error != nil {
		return fmt.Errorf("update HTTP route: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		if _, err := store.GetHTTP(ctx, route.ID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteHTTP 删除指定 HTTP Desired Route；调用方负责在同一事务推进 Generation。
func (store routeRepository) DeleteHTTP(ctx context.Context, id string) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if strings.TrimSpace(id) == "" {
		return repository.ErrInvalidRoute
	}
	result := store.database.WithContext(ctx).Where("id = ?", id).Delete(&httpRouteRecord{})
	if result.Error != nil {
		return fmt.Errorf("delete HTTP route: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// GetTCP 按稳定 Route ID 读取单条 TCP Desired Route。
func (store routeRepository) GetTCP(ctx context.Context, id string) (repository.TCPRoute, error) {
	if strings.TrimSpace(id) == "" {
		return repository.TCPRoute{}, repository.ErrInvalidRoute
	}
	var record tcpRouteRecord
	if err := store.database.WithContext(ctx).Where("id = ?", id).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.TCPRoute{}, repository.ErrNotFound
		}
		return repository.TCPRoute{}, fmt.Errorf("get TCP route: %w", err)
	}
	return tcpRouteDomain(record)
}

// ListTCP 返回全部 TCP Desired Route。禁用 Route 仍占用公网端口，因此不能过滤。
func (store routeRepository) ListTCP(ctx context.Context) ([]repository.TCPRoute, error) {
	return store.loadTCPRoutes(ctx)
}

// CreateTCP 在当前写事务中插入经过领域校验的 TCP Route。
func (store routeRepository) CreateTCP(ctx context.Context, route repository.TCPRoute) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if err := route.Validate(); err != nil {
		return err
	}
	if err := store.database.WithContext(ctx).Create(tcpRouteRecordFromDomain(route)).Error; err != nil {
		return fmt.Errorf("create TCP route: %w", err)
	}
	return nil
}

// UpdateTCP 以全量替换方式更新可变字段；调用方在同一事务内推进全局 Generation。
func (store routeRepository) UpdateTCP(ctx context.Context, route repository.TCPRoute) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if err := route.Validate(); err != nil {
		return err
	}
	result := store.database.WithContext(ctx).Model(&tcpRouteRecord{}).
		Where("id = ?", route.ID).
		Updates(map[string]any{
			"service_id":  route.ServiceID,
			"public_port": route.PublicPort,
			"enabled":     route.Enabled,
			"updated_at":  route.UpdatedAt,
		})
	if result.Error != nil {
		return fmt.Errorf("update TCP route: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		if _, err := store.GetTCP(ctx, route.ID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteTCP 删除指定 TCP Desired Route；调用方负责在同一事务推进 Generation。
func (store routeRepository) DeleteTCP(ctx context.Context, id string) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if strings.TrimSpace(id) == "" {
		return repository.ErrInvalidRoute
	}
	result := store.database.WithContext(ctx).Where("id = ?", id).Delete(&tcpRouteRecord{})
	if result.Error != nil {
		return fmt.Errorf("delete TCP route: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return repository.ErrNotFound
	}
	return nil
}

// AdvanceGeneration 使用当前 Generation 作为乐观锁，只允许一次单调递增。
func (store routeRepository) AdvanceGeneration(ctx context.Context, expected uint64) (uint64, error) {
	if store.readOnly {
		return 0, errRepositoryWriteOutsideTransaction
	}
	if expected >= math.MaxInt64 {
		return 0, repository.ErrVersionConflict
	}
	next := expected + 1
	result := store.database.WithContext(ctx).Model(&routeConfigStateRecord{}).
		Where("id = ? AND generation = ?", singletonRouteConfigStateID, expected).
		Update("generation", next)
	if result.Error != nil {
		return 0, fmt.Errorf("advance route config generation: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return 0, repository.ErrVersionConflict
	}
	return next, nil
}

// loadServices 从当前事务读取全部 Service，并在持久化边界重新验证每一行。
func (store routeRepository) loadServices(ctx context.Context) ([]repository.Service, error) {
	var records []serviceRecord
	if err := store.database.WithContext(ctx).
		Order(ServiceColumns.TunnelID + " ASC").
		Order(ServiceColumns.ID + " ASC").
		Find(&records).Error; err != nil {
		return nil, fmt.Errorf("load route services: %w", err)
	}
	services := make([]repository.Service, 0, len(records))
	for _, record := range records {
		service, err := serviceDomainFromRecord(record)
		if err != nil {
			return nil, fmt.Errorf("load route services: %w", err)
		}
		services = append(services, service)
	}
	return services, nil
}

// loadHTTPRoutes 按匹配键和 ID 稳定排序，保证相同 Desired State 产生确定性输入。
func (store routeRepository) loadHTTPRoutes(ctx context.Context) ([]repository.HTTPRoute, error) {
	var records []httpRouteRecord
	if err := store.database.WithContext(ctx).
		Order("hostname ASC").
		Order("path_prefix ASC").
		Order("id ASC").
		Find(&records).Error; err != nil {
		return nil, fmt.Errorf("load HTTP routes: %w", err)
	}
	routes := make([]repository.HTTPRoute, 0, len(records))
	for _, record := range records {
		route, err := httpRouteDomain(record)
		if err != nil {
			return nil, fmt.Errorf("stored HTTP route %q is invalid: %w", record.ID, err)
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func httpRouteRecordFromDomain(route repository.HTTPRoute) httpRouteRecord {
	return httpRouteRecord{
		ID: route.ID, ServiceID: route.ServiceID, Hostname: route.Hostname,
		PathPrefix: route.PathPrefix, PreserveHost: route.PreserveHost, Enabled: route.Enabled,
		CreatedAt: route.CreatedAt, UpdatedAt: route.UpdatedAt,
	}
}

func httpRouteDomain(record httpRouteRecord) (repository.HTTPRoute, error) {
	route := repository.HTTPRoute{
		ID: record.ID, ServiceID: record.ServiceID, Hostname: record.Hostname,
		PathPrefix: record.PathPrefix, PreserveHost: record.PreserveHost, Enabled: record.Enabled,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if err := route.Validate(); err != nil {
		return repository.HTTPRoute{}, err
	}
	return route, nil
}

// loadTCPRoutes 按公开端口和 ID 稳定排序，保证相同 Desired State 产生确定性输入。
func (store routeRepository) loadTCPRoutes(ctx context.Context) ([]repository.TCPRoute, error) {
	var records []tcpRouteRecord
	if err := store.database.WithContext(ctx).
		Order("public_port ASC").
		Order("id ASC").
		Find(&records).Error; err != nil {
		return nil, fmt.Errorf("load TCP routes: %w", err)
	}
	routes := make([]repository.TCPRoute, 0, len(records))
	for _, record := range records {
		route, err := tcpRouteDomain(record)
		if err != nil {
			return nil, fmt.Errorf("stored TCP route %q is invalid: %w", record.ID, err)
		}
		routes = append(routes, route)
	}
	return routes, nil
}

func tcpRouteRecordFromDomain(route repository.TCPRoute) tcpRouteRecord {
	return tcpRouteRecord{
		ID: route.ID, ServiceID: route.ServiceID, PublicPort: route.PublicPort,
		Enabled: route.Enabled, CreatedAt: route.CreatedAt, UpdatedAt: route.UpdatedAt,
	}
}

func tcpRouteDomain(record tcpRouteRecord) (repository.TCPRoute, error) {
	route := repository.TCPRoute{
		ID: record.ID, ServiceID: record.ServiceID, PublicPort: record.PublicPort,
		Enabled: record.Enabled, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
	if err := route.Validate(); err != nil {
		return repository.TCPRoute{}, err
	}
	return route, nil
}
