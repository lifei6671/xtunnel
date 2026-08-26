package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/lifei6671/xtunnel/internal/identity"
)

const (
	minimumHealthIntervalMS uint32 = 1_000
	maximumHealthIntervalMS uint32 = 3_600_000
	minimumHealthTimeoutMS  uint32 = 100
	minimumHealthThreshold  uint32 = 1
	maximumHealthThreshold  uint32 = 20
	minimumHTTPStatus       uint32 = 100
	maximumHTTPStatus       uint32 = 599
	maximumOriginPort       uint32 = 65_535
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
	Health           *HealthCheck
	Enabled          bool

	// Version 是独立于 Tunnel ETag 与 Desired Revision 的 Service 乐观锁版本。
	Version   int64
	CreatedAt int64
	UpdatedAt int64
}

// Validate 检查 Service、Origin 和可选 Health 的全部持久化不变量。
func (service Service) Validate() error {
	if !identity.ValidServiceID(service.ID) || !identity.ValidTunnelID(service.TunnelID) ||
		strings.TrimSpace(service.Name) == "" || strings.TrimSpace(service.OriginHost) == "" ||
		service.RequiredRevision < 0 || service.OriginPort < 1 || service.OriginPort > maximumOriginPort ||
		service.ConnectTimeoutMS == 0 || service.Version < 1 || service.CreatedAt <= 0 || service.UpdatedAt <= 0 {
		return ErrInvalidService
	}

	if !validOriginFields(service) || !validHealthCheck(service.Health) {
		return ErrInvalidService
	}
	return nil
}

func validOriginFields(service Service) bool {
	if whitespaceOnly(service.TLSServerName) || whitespaceOnly(service.OriginHTTPHost) {
		return false
	}
	switch service.OriginScheme {
	case OriginSchemeHTTP:
		return service.TLSServerName == ""
	case OriginSchemeHTTPS:
		return true
	case OriginSchemeTCP:
		return service.TLSServerName == "" && service.OriginHTTPHost == ""
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

func whitespaceOnly(value string) bool {
	return value != "" && strings.TrimSpace(value) == ""
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
