package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/originconfig"
)

const (
	minimumHealthIntervalMS uint32 = 1_000
	maximumHealthIntervalMS uint32 = 3_600_000
	minimumHealthTimeoutMS  uint32 = 100
	minimumHealthThreshold  uint32 = 1
	maximumHealthThreshold  uint32 = 20
	minimumHTTPStatus       uint32 = 100
	maximumHTTPStatus       uint32 = 599

	defaultHTTPIdleConnectionTimeoutMS uint32 = 90_000
	defaultHTTPMaxIdleConnections      uint32 = 100
	defaultTCPKeepAliveIntervalMS      uint32 = 30_000
)

var (
	// ErrInvalidService 表示 Service 的持久化字段不符合领域约束。
	ErrInvalidService = errors.New("service is invalid")
)

// OriginScheme 是 Agent 连接 Service Origin 时使用的传输协议。
type OriginScheme string

const (
	// OriginSchemeHTTP 表示不启用 TLS 的 HTTP Origin。
	OriginSchemeHTTP OriginScheme = "http"
	// OriginSchemeHTTPS 表示启用 TLS 的 HTTP Origin。
	OriginSchemeHTTPS OriginScheme = "https"
	// OriginSchemeTCP 表示普通 TCP Origin。
	OriginSchemeTCP OriginScheme = "tcp"
)

// HealthType 是 Connector 对 Service Origin 执行的健康检查类型。
type HealthType string

const (
	// HealthTypeTCP 表示只建立 TCP 连接的健康检查。
	HealthTypeTCP HealthType = "TCP"
	// HealthTypeHTTP 表示发送 HTTP 请求并校验响应状态码的健康检查。
	HealthTypeHTTP HealthType = "HTTP"
)

// HealthCheck 是 Service 的完整健康检查策略。Service.Health 为 nil 表示 Disabled。
type HealthCheck struct {
	Type              HealthType
	Path              string
	IntervalMS        uint32
	TimeoutMS         uint32
	ExpectedStatusMin uint32
	ExpectedStatusMax uint32
	FailureThreshold  uint32
	SuccessThreshold  uint32
}

// ServiceProxyOptions 是 Server 构建 Route Snapshot 时随 Service 传递的代理策略。
// HTTP 前缀字段只对 HTTP/HTTPS Origin 生效；TCP Keepalive 与 Happy Eyeballs
// 属于连接级策略。全部字段以类型化列持久化，不使用 JSON 或 map 形成第二套默认值。
type ServiceProxyOptions struct {
	// DisableChunkedEncoding 禁止 HTTP/HTTPS 代理使用分块编码；TCP 必须保持 false。
	DisableChunkedEncoding bool
	// DisableHappyEyeballs 禁止连接 Origin 时并行竞速 IPv6/IPv4 地址。
	DisableHappyEyeballs bool
	// HTTPIdleConnectionTimeoutMS 是 HTTP/HTTPS 空闲连接保留时间，范围 1..uint32 最大值，
	// 默认 90000ms；TCP 只能保留默认值。
	HTTPIdleConnectionTimeoutMS uint32
	// HTTPMaxIdleConnections 是单个 Service 的 HTTP/HTTPS 空闲连接上限，
	// 范围 1..uint32 最大值，默认 100；
	// 运行时仍必须服从 Server 全局连接预算，不能把该值当成绕过全局上限的配额。
	HTTPMaxIdleConnections uint32
	// TCPKeepAliveIntervalMS 是连接级 Keepalive 间隔，范围 0..uint32 最大值，
	// 默认 30000ms；0 显式禁用。
	TCPKeepAliveIntervalMS uint32
}

// WithDefaults 将“整个结构全零”的未指定态替换为 V0.1 冻结默认值。
//
// Presence 规则只认整个结构：一旦任一字段被显式设置，其余零值都原样保留。因此调用方
// 可同时写入 HTTP 默认值并把 TCPKeepAliveIntervalMS 设为 0，明确关闭 Keepalive；不会
// 因局部字段补默认而把禁用意图改回 30000ms。
func (options ServiceProxyOptions) WithDefaults() ServiceProxyOptions {
	if options != (ServiceProxyOptions{}) {
		return options
	}
	return ServiceProxyOptions{
		HTTPIdleConnectionTimeoutMS: defaultHTTPIdleConnectionTimeoutMS,
		HTTPMaxIdleConnections:      defaultHTTPMaxIdleConnections,
		TCPKeepAliveIntervalMS:      defaultTCPKeepAliveIntervalMS,
	}
}

// Service 是直接归属于一个 Tunnel 的 Origin 与 Health 配置聚合。
type Service struct {
	// ID 固定为 svc_ 加 26 位大写 ULID。
	ID string
	// TunnelID 是不可变的所属 Tunnel 身份。
	TunnelID string
	Name     string

	RequiredRevision int64
	OriginScheme     OriginScheme
	OriginHost       string
	OriginPort       uint32
	TLSVerify        bool
	TLSServerName    string
	OriginHTTPHost   string
	ConnectTimeoutMS uint32
	// ProxyOptions 由 Repository 以类型化列持久化，并原样进入 Server Snapshot 构建输入。
	// 全零值仅表示使用 WithDefaults 冻结的 V0.1 默认值。
	ProxyOptions ServiceProxyOptions
	Health       *HealthCheck
	Enabled      bool

	// Version 是独立于 Tunnel ETag 与 Desired Revision 的 Service 乐观锁版本。
	Version   int64
	CreatedAt int64
	UpdatedAt int64
}

// Validate 检查 Service、Origin 和可选 Health 的全部持久化不变量。
func (service Service) Validate() error {
	if !identity.ValidServiceID(service.ID) || !identity.ValidTunnelID(service.TunnelID) ||
		strings.TrimSpace(service.Name) == "" || strings.TrimSpace(service.OriginHost) == "" ||
		service.RequiredRevision < 0 || service.Version < 1 || service.CreatedAt <= 0 || service.UpdatedAt <= 0 {
		return ErrInvalidService
	}

	if err := originconfig.Validate(originconfig.Fields{
		Scheme:           string(service.OriginScheme),
		Host:             service.OriginHost,
		Port:             service.OriginPort,
		ConnectTimeoutMS: service.ConnectTimeoutMS,
		TLSVerify:        service.TLSVerify,
		TLSServerName:    service.TLSServerName,
		HTTPHostHeader:   service.OriginHTTPHost,
	}); err != nil || !validServiceProxyOptions(service.OriginScheme, service.ProxyOptions) || !validHealthCheck(service.Health) {
		return ErrInvalidService
	}
	return nil
}

// validServiceProxyOptions 在持久化边界验证协议适用性。HTTP 专属参数落到 TCP 时只
// 允许冻结默认值，避免运行时悄悄忽略错误配置；Keepalive 的 0 则是合法禁用值。
func validServiceProxyOptions(scheme OriginScheme, options ServiceProxyOptions) bool {
	options = options.WithDefaults()
	if options.HTTPIdleConnectionTimeoutMS == 0 || options.HTTPMaxIdleConnections == 0 {
		return false
	}
	switch scheme {
	case OriginSchemeHTTP, OriginSchemeHTTPS:
		return true
	case OriginSchemeTCP:
		return !options.DisableChunkedEncoding &&
			options.HTTPIdleConnectionTimeoutMS == defaultHTTPIdleConnectionTimeoutMS &&
			options.HTTPMaxIdleConnections == defaultHTTPMaxIdleConnections
	default:
		return false
	}
}

func validHealthCheck(health *HealthCheck) bool {
	if health == nil {
		return true
	}
	if health.IntervalMS < minimumHealthIntervalMS || health.IntervalMS > maximumHealthIntervalMS ||
		health.TimeoutMS < minimumHealthTimeoutMS || health.TimeoutMS >= health.IntervalMS ||
		health.FailureThreshold < minimumHealthThreshold || health.FailureThreshold > maximumHealthThreshold ||
		health.SuccessThreshold < minimumHealthThreshold || health.SuccessThreshold > maximumHealthThreshold {
		return false
	}
	switch health.Type {
	case HealthTypeTCP:
		return health.Path == "" && health.ExpectedStatusMin == 0 && health.ExpectedStatusMax == 0
	case HealthTypeHTTP:
		return strings.HasPrefix(health.Path, "/") &&
			health.ExpectedStatusMin >= minimumHTTPStatus && health.ExpectedStatusMax <= maximumHTTPStatus &&
			health.ExpectedStatusMin <= health.ExpectedStatusMax
	default:
		return false
	}
}

// ServiceRepository 定义 Service 的最小持久化边界。
// Repository 实现不得自行开启或提交事务。
type ServiceRepository interface {
	Create(ctx context.Context, service Service) error
	Get(ctx context.Context, tunnelID, serviceID string) (Service, error)
	ListByTunnel(ctx context.Context, tunnelID string) ([]Service, error)
	CountByTunnel(ctx context.Context, tunnelID string) (int64, error)
	// Update 要求 Service.Version 等于 expectedVersion；成功实现必须将版本加一并返回新值。
	Update(ctx context.Context, service Service, expectedVersion int64) (Service, error)
	Delete(ctx context.Context, tunnelID, serviceID string, expectedVersion int64) error
}
