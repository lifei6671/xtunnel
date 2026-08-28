package route

import (
	"errors"
	"fmt"
	"sort"

	"github.com/lifei6671/xtunnel/internal/repository"
)

var (
	// ErrInvalidDesiredState 表示权威数据虽然可读取，但无法组成完整且自洽的路由快照。
	ErrInvalidDesiredState = errors.New("route desired state is invalid")
)

// HTTPRoute 是 HTTP 热路径所需的已解析路由值。
//
// TunnelID 与 RequiredRevision 在构建阶段由 Service 关联得到，使后续请求无需再次
// 查询 Repository。该类型不包含 slice、map 或指针，按值返回不会泄漏快照内部状态。
type HTTPRoute struct {
	// ID、ServiceID、TunnelID 构成热路径所需的完整归属信息。
	ID        string
	ServiceID string
	TunnelID  string
	// Hostname 与 PathPrefix 是 M4-02 已验证的 canonical 匹配输入。
	Hostname   string
	PathPrefix string
	// PreserveHost 决定反向代理是否保留公网请求 Host。
	PreserveHost bool
	// OriginScheme、OriginHost、OriginPort、OriginHTTPHost 与 ProxyOptions 来自同一代
	// Service Desired State，使 M4-03 能在热路径完成 Host 回落而不查询 SQLite。
	// M4-03 的 Host 优先级固定为显式 OriginHTTPHost、PreserveHost、Origin Host；
	// Transport 连接池必须同时使用 TunnelID、ServiceID 与 RequiredRevision 隔离。
	OriginScheme   repository.OriginScheme
	OriginHost     string
	OriginPort     uint16
	OriginHTTPHost string
	ProxyOptions   HTTPProxyOptions
	// RequiredRevision 阻止请求回落到尚未观察到该 Service 配置的旧 Connector。
	RequiredRevision int64
}

// HTTPProxyOptions 是 Route 热路径创建 HTTP Transport 所需的不可变值。
// 全局 WorkConn 预算仍是硬上限，MaxIdleConnections 只是单 Service 请求上限。
type HTTPProxyOptions struct {
	DisableChunkedEncoding  bool
	IdleConnectionTimeoutMS uint32
	MaxIdleConnections      uint32
}

// TCPRoute 是 TCP 热路径所需的已解析路由值。
type TCPRoute struct {
	// ID、ServiceID、TunnelID 构成热路径所需的完整归属信息。
	ID        string
	ServiceID string
	TunnelID  string
	// PublicPort 是该 Route 唯一占用的公网 TCP 端口。
	PublicPort uint16
	// RequiredRevision 阻止请求回落到尚未观察到该 Service 配置的旧 Connector。
	RequiredRevision int64
}

// TunnelRuntime 保存路由热路径判断 Tunnel 代次所需的不可变字段。
type TunnelRuntime struct {
	// TunnelID 是运行时关联键。
	TunnelID string
	// DesiredRevision 是该 Tunnel 当前完整 Desired State 的最新代次。
	DesiredRevision int64
}

// HostRoutes 是同一 canonical hostname 下的一组不可变 HTTP 路由。
// 内部 slice 只在构建期间写入；发布后调用方只能通过 Routes 取得副本。
type HostRoutes struct {
	routes []HTTPRoute
}

// Routes 返回该 Host 的全部路由副本。
// 路由顺序不构成匹配优先级；M4-02 Matcher 会显式选择最长路径段前缀。
func (routes HostRoutes) Routes() []HTTPRoute {
	return append([]HTTPRoute(nil), routes.routes...)
}

// Snapshot 是按一个持久化 generation 构建的完整只读路由视图。
//
// 所有 map 和 slice 均为私有字段；构建完成后不再修改。Manager 只通过
// atomic.Pointer 发布完整对象，因此读路径无需锁，也不可能观察到部分构建结果。
type Snapshot struct {
	generation uint64
	http       map[string]HostRoutes
	tcp        map[uint16]TCPRoute
	tunnels    map[string]TunnelRuntime
}

// Generation 返回该快照对应的权威配置代次。
func (snapshot *Snapshot) Generation() uint64 {
	if snapshot == nil {
		return 0
	}
	return snapshot.generation
}

// HTTP 返回 canonical hostname 对应的路由集合。
// HostRoutes 不暴露内部 slice；调用方读取具体路由时仍会获得值副本。
func (snapshot *Snapshot) HTTP(hostname string) (HostRoutes, bool) {
	if snapshot == nil {
		return HostRoutes{}, false
	}
	routes, ok := snapshot.http[hostname]
	return routes, ok
}

// TCP 返回 public_port 对应的路由值副本。
func (snapshot *Snapshot) TCP(publicPort uint16) (TCPRoute, bool) {
	if snapshot == nil {
		return TCPRoute{}, false
	}
	route, ok := snapshot.tcp[publicPort]
	return route, ok
}

// TCPRoutes 返回全部已启用 TCP Route 的稳定值副本，供 Listener Reconciler 构造
// 完整 Desired 集合。调用方不能取得或修改快照内部 map。
func (snapshot *Snapshot) TCPRoutes() []TCPRoute {
	if snapshot == nil {
		return nil
	}
	routes := make([]TCPRoute, 0, len(snapshot.tcp))
	for _, route := range snapshot.tcp {
		routes = append(routes, route)
	}
	sort.Slice(routes, func(left, right int) bool {
		if routes[left].PublicPort != routes[right].PublicPort {
			return routes[left].PublicPort < routes[right].PublicPort
		}
		return routes[left].ID < routes[right].ID
	})
	return routes
}

// Tunnel 返回 tunnel_id 对应的运行时值副本。
func (snapshot *Snapshot) Tunnel(tunnelID string) (TunnelRuntime, bool) {
	if snapshot == nil {
		return TunnelRuntime{}, false
	}
	tunnel, ok := snapshot.tunnels[tunnelID]
	return tunnel, ok
}

// buildSnapshot 先完整校验并关联 Tunnel、Service 与 Route，再返回一次性候选。
// 任一行不自洽都会放弃整个候选，避免把部分路由发布到公网热路径。
func buildSnapshot(state repository.RouteDesiredState) (*Snapshot, error) {
	tunnels := make(map[string]TunnelRuntime, len(state.Tunnels))
	tunnelActive := make(map[string]bool, len(state.Tunnels))
	for index := range state.Tunnels {
		tunnel := state.Tunnels[index]
		if err := tunnel.Validate(); err != nil {
			return nil, fmt.Errorf("%w: tunnel at index %d: %w", ErrInvalidDesiredState, index, err)
		}
		if _, exists := tunnels[tunnel.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate tunnel %q", ErrInvalidDesiredState, tunnel.ID)
		}
		tunnels[tunnel.ID] = TunnelRuntime{
			TunnelID:        tunnel.ID,
			DesiredRevision: tunnel.DesiredRevision,
		}
		tunnelActive[tunnel.ID] = tunnel.RevokedAt == nil
	}

	type serviceBinding struct {
		tunnelID         string
		requiredRevision int64
		active           bool
		originScheme     repository.OriginScheme
		originHost       string
		originPort       uint16
		originHTTPHost   string
		httpProxyOptions HTTPProxyOptions
	}
	services := make(map[string]serviceBinding, len(state.Services))
	for index := range state.Services {
		service := state.Services[index]
		if err := service.Validate(); err != nil {
			return nil, fmt.Errorf("%w: service at index %d: %w", ErrInvalidDesiredState, index, err)
		}
		if _, exists := services[service.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate service %q", ErrInvalidDesiredState, service.ID)
		}
		active, exists := tunnelActive[service.TunnelID]
		if !exists {
			return nil, fmt.Errorf("%w: service %q references unknown tunnel %q", ErrInvalidDesiredState, service.ID, service.TunnelID)
		}
		proxyOptions := service.ProxyOptions.WithDefaults()
		services[service.ID] = serviceBinding{
			tunnelID:         service.TunnelID,
			requiredRevision: service.RequiredRevision,
			active:           active && service.Enabled,
			originScheme:     service.OriginScheme,
			originHost:       service.OriginHost,
			originPort:       uint16(service.OriginPort),
			originHTTPHost:   service.OriginHTTPHost,
			httpProxyOptions: HTTPProxyOptions{
				DisableChunkedEncoding:  proxyOptions.DisableChunkedEncoding,
				IdleConnectionTimeoutMS: proxyOptions.HTTPIdleConnectionTimeoutMS,
				MaxIdleConnections:      proxyOptions.HTTPMaxIdleConnections,
			},
		}
	}

	http := make(map[string]HostRoutes)
	httpKeys := make(map[string]struct{}, len(state.HTTPRoutes))
	httpIDs := make(map[string]struct{}, len(state.HTTPRoutes))
	for index := range state.HTTPRoutes {
		desired := state.HTTPRoutes[index]
		if err := desired.Validate(); err != nil {
			return nil, fmt.Errorf("%w: HTTP route at index %d: %w", ErrInvalidDesiredState, index, err)
		}
		canonicalHostname, err := CanonicalHostname(desired.Hostname)
		if err != nil || canonicalHostname != desired.Hostname {
			return nil, fmt.Errorf("%w: HTTP route %q hostname is not canonical", ErrInvalidDesiredState, desired.ID)
		}
		if !validCanonicalPathPrefix(desired.PathPrefix) {
			return nil, fmt.Errorf("%w: HTTP route %q path prefix is not canonical", ErrInvalidDesiredState, desired.ID)
		}
		binding, exists := services[desired.ServiceID]
		if !exists {
			return nil, fmt.Errorf("%w: HTTP route %q references unknown service %q", ErrInvalidDesiredState, desired.ID, desired.ServiceID)
		}
		if _, exists := httpIDs[desired.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate HTTP route ID %q", ErrInvalidDesiredState, desired.ID)
		}
		httpIDs[desired.ID] = struct{}{}
		key := desired.Hostname + "\x00" + desired.PathPrefix
		if _, exists := httpKeys[key]; exists {
			return nil, fmt.Errorf("%w: duplicate HTTP route key %q %q", ErrInvalidDesiredState, desired.Hostname, desired.PathPrefix)
		}
		httpKeys[key] = struct{}{}
		if !desired.Enabled || !binding.active {
			continue
		}
		hostRoutes := http[desired.Hostname]
		hostRoutes.routes = append(hostRoutes.routes, HTTPRoute{
			ID:               desired.ID,
			ServiceID:        desired.ServiceID,
			TunnelID:         binding.tunnelID,
			Hostname:         desired.Hostname,
			PathPrefix:       desired.PathPrefix,
			PreserveHost:     desired.PreserveHost,
			OriginScheme:     binding.originScheme,
			OriginHost:       binding.originHost,
			OriginPort:       binding.originPort,
			OriginHTTPHost:   binding.originHTTPHost,
			ProxyOptions:     binding.httpProxyOptions,
			RequiredRevision: binding.requiredRevision,
		})
		http[desired.Hostname] = hostRoutes
	}
	for hostname, routes := range http {
		// 稳定顺序只为可复现快照和测试；Matcher 独立选择最长前缀。
		sort.Slice(routes.routes, func(left, right int) bool {
			return routes.routes[left].ID < routes.routes[right].ID
		})
		http[hostname] = routes
	}

	tcp := make(map[uint16]TCPRoute, len(state.TCPRoutes))
	seenTCPPorts := make(map[uint16]struct{}, len(state.TCPRoutes))
	tcpIDs := make(map[string]struct{}, len(state.TCPRoutes))
	for index := range state.TCPRoutes {
		desired := state.TCPRoutes[index]
		if err := desired.Validate(); err != nil {
			return nil, fmt.Errorf("%w: TCP route at index %d: %w", ErrInvalidDesiredState, index, err)
		}
		binding, exists := services[desired.ServiceID]
		if !exists {
			return nil, fmt.Errorf("%w: TCP route %q references unknown service %q", ErrInvalidDesiredState, desired.ID, desired.ServiceID)
		}
		if _, exists := tcpIDs[desired.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate TCP route ID %q", ErrInvalidDesiredState, desired.ID)
		}
		tcpIDs[desired.ID] = struct{}{}
		if _, exists := seenTCPPorts[desired.PublicPort]; exists {
			return nil, fmt.Errorf("%w: duplicate TCP public port %d", ErrInvalidDesiredState, desired.PublicPort)
		}
		seenTCPPorts[desired.PublicPort] = struct{}{}
		if !desired.Enabled || !binding.active {
			continue
		}
		tcp[desired.PublicPort] = TCPRoute{
			ID:               desired.ID,
			ServiceID:        desired.ServiceID,
			TunnelID:         binding.tunnelID,
			PublicPort:       desired.PublicPort,
			RequiredRevision: binding.requiredRevision,
		}
	}

	return &Snapshot{
		generation: state.Generation,
		http:       http,
		tcp:        tcp,
		tunnels:    tunnels,
	}, nil
}
