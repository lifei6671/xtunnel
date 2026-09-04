package application

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"time"

	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
)

const systemProtocolVersionV1 = "v1"

// ErrSystemReadInput 表示构造参数、查询 Context 或注入检查结果不满足冻结边界。
var ErrSystemReadInput = errors.New("system read input is invalid")

// SystemHealthStatus 是 System Health 只读投影允许的聚合状态。
type SystemHealthStatus string

const (
	// SystemHealthReady 表示全部已注入检查均就绪。
	SystemHealthReady SystemHealthStatus = "READY"
	// SystemHealthDegraded 表示没有真实检查或至少一个检查未就绪。
	SystemHealthDegraded SystemHealthStatus = "DEGRADED"
)

// SystemInfo 是进程构建信息与启动时钟的只读投影。
type SystemInfo struct {
	Version         string
	GoVersion       string
	ProtocolVersion string
	OS              string
	Arch            string
	StartedAt       time.Time
	UptimeSeconds   int64
}

// SystemHealthCheckResult 是一个真实健康检查返回的安全公开结果。
// Message 由检查 owner 负责脱敏，Application 不把底层 error 自动暴露给 API。
type SystemHealthCheckResult struct {
	Name    string
	Status  SystemHealthStatus
	Message *string
}

// SystemHealthCheck 由 Bootstrap 注入真实检查。函数必须尊重 Context，并只返回
// 已脱敏、允许公开的 Message；现有 owner 的方法值可以直接作为检查注入。
type SystemHealthCheck func(context.Context) SystemHealthCheckResult

// SystemHealth 是所有已注入检查在同一请求中的确定性聚合结果。
type SystemHealth struct {
	Status    SystemHealthStatus
	Checks    []SystemHealthCheckResult
	CheckedAt time.Time
}

// SystemManagementConfig 是 Management Config 的公开子集。
type SystemManagementConfig struct {
	PublicURL string
}

// SystemAgentGatewayConfig 是 Agent Gateway Config 的公开子集。
type SystemAgentGatewayConfig struct {
	PublicHostname string
	TLSMode        string
}

// SystemTCPIngressConfig 是 TCP Ingress Config 的公开端口范围。
type SystemTCPIngressConfig struct {
	MinPort int
	MaxPort int
}

// SystemLoggingConfig 只公开日志级别，不公开 Sink 或内部格式配置。
type SystemLoggingConfig struct {
	Level string
}

// SystemPublicLimits 只包含 OpenAPI PublicLimits 明确允许公开的硬限制。
type SystemPublicLimits struct {
	MaxTunnels                          int
	MaxConnectors                       int
	MaxConnectorsPerTunnel              int
	MaxServicesPerTunnel                int
	MaxActiveConnections                int
	MaxConnectionsPerTunnel             int
	MaxConnectionsPerService            int
	MaxConnectionsPerSourceIP           int
	MaxOpenRatePerSourceIP              int
	MaxOpenBurstPerSourceIP             int
	MaxHTTPRequestsPerSourceIPPerSecond int
	MaxHTTPHeaderBytes                  int
	MaxHTTPBodyBytes                    int64
}

// SystemConfig 是 Server Config 的显式白名单投影。Listen、Path、Trusted Proxy、
// TLS Key/Certificate 和内部预算在类型层不可达，禁止直接序列化 serverconfig.Config。
type SystemConfig struct {
	Management            SystemManagementConfig
	AgentGateway          SystemAgentGatewayConfig
	TCPIngress            SystemTCPIngressConfig
	Logging               SystemLoggingConfig
	Limits                SystemPublicLimits
	ChangesRequireRestart bool
}

// SystemReadService 持有进程级只读快照。Config 在构造时投影，后续配置对象的修改
// 不会扩大 API 暴露面；健康检查按注入顺序串行执行，避免自行制造并发 owner。
type SystemReadService struct {
	version   string
	startedAt time.Time
	config    SystemConfig
	checks    []SystemHealthCheck
	now       func() time.Time
}

// NewSystemReadService 构造 System Info/Health/Config 的唯一 Application 投影层。
// version 和 startedAt 由进程 Bootstrap 注入，checks 必须是真实运行时检查 owner。
func NewSystemReadService(
	version string,
	startedAt time.Time,
	config serverconfig.Config,
	checks ...SystemHealthCheck,
) (*SystemReadService, error) {
	if strings.TrimSpace(version) == "" || startedAt.IsZero() {
		return nil, ErrSystemReadInput
	}
	projected, err := projectSystemConfig(config)
	if err != nil {
		return nil, err
	}
	for _, check := range checks {
		if check == nil {
			return nil, ErrSystemReadInput
		}
	}
	return &SystemReadService{
		version: version,
		// 保留 Bootstrap time.Now() 携带的 monotonic reading，Uptime 不受 wall clock
		// 校时影响；只在输出 StartedAt 时转换为 UTC。
		startedAt: startedAt,
		config:    projected,
		checks:    append([]SystemHealthCheck(nil), checks...),
		now:       time.Now,
	}, nil
}

// Info 返回当前进程构建信息。Uptime 只由注入的启动时间和当前时钟计算。
func (service *SystemReadService) Info(ctx context.Context) (SystemInfo, error) {
	if service == nil || ctx == nil || service.now == nil {
		return SystemInfo{}, ErrSystemReadInput
	}
	if err := ctx.Err(); err != nil {
		return SystemInfo{}, err
	}
	now := service.now()
	if now.Before(service.startedAt) {
		return SystemInfo{}, ErrSystemReadInput
	}
	return SystemInfo{
		Version:         service.version,
		GoVersion:       runtime.Version(),
		ProtocolVersion: systemProtocolVersionV1,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		StartedAt:       service.startedAt.UTC(),
		UptimeSeconds:   int64(now.Sub(service.startedAt) / time.Second),
	}, nil
}

// Health 执行并聚合所有真实注入检查。没有检查时结果固定为 DEGRADED，禁止把
// “尚未接线”误报为 READY；任何一个检查降级都会使总状态降级。
func (service *SystemReadService) Health(ctx context.Context) (SystemHealth, error) {
	if service == nil || ctx == nil || service.now == nil {
		return SystemHealth{}, ErrSystemReadInput
	}
	if err := ctx.Err(); err != nil {
		return SystemHealth{}, err
	}
	status := SystemHealthDegraded
	if len(service.checks) > 0 {
		status = SystemHealthReady
	}
	results := make([]SystemHealthCheckResult, 0, len(service.checks))
	names := make(map[string]struct{}, len(service.checks))
	for _, check := range service.checks {
		if err := ctx.Err(); err != nil {
			return SystemHealth{}, err
		}
		result := check(ctx)
		if strings.TrimSpace(result.Name) == "" ||
			(result.Status != SystemHealthReady && result.Status != SystemHealthDegraded) {
			return SystemHealth{}, ErrSystemReadInput
		}
		if _, duplicate := names[result.Name]; duplicate {
			return SystemHealth{}, ErrSystemReadInput
		}
		names[result.Name] = struct{}{}
		if result.Message != nil {
			message := *result.Message
			result.Message = &message
		}
		results = append(results, result)
		if result.Status == SystemHealthDegraded {
			status = SystemHealthDegraded
		}
	}
	if err := ctx.Err(); err != nil {
		return SystemHealth{}, err
	}
	checkedAt := service.now().UTC()
	if checkedAt.IsZero() {
		return SystemHealth{}, ErrSystemReadInput
	}
	return SystemHealth{Status: status, Checks: results, CheckedAt: checkedAt}, nil
}

// DashboardServerStatus 复用同一组 System Health 权威检查。Dashboard 只消费
// READY/DEGRADED 值，不根据资源数量在第二处重算 Server 状态。
func (service *SystemReadService) DashboardServerStatus(ctx context.Context) (DashboardServerStatus, error) {
	health, err := service.Health(ctx)
	if err != nil {
		return "", err
	}
	switch health.Status {
	case SystemHealthReady:
		return DashboardServerStatusReady, nil
	case SystemHealthDegraded:
		return DashboardServerStatusDegraded, nil
	default:
		return "", ErrSystemReadInput
	}
}

// Config 返回构造时冻结的安全白名单快照。
func (service *SystemReadService) Config(ctx context.Context) (SystemConfig, error) {
	if service == nil || ctx == nil {
		return SystemConfig{}, ErrSystemReadInput
	}
	if err := ctx.Err(); err != nil {
		return SystemConfig{}, err
	}
	return service.config, nil
}

func projectSystemConfig(config serverconfig.Config) (SystemConfig, error) {
	publicURL, err := config.Management.EffectivePublicURL()
	if err != nil {
		return SystemConfig{}, ErrSystemReadInput
	}
	if strings.TrimSpace(config.AgentGateway.PublicHostname) == "" ||
		(config.AgentGateway.TLS.Mode != "public" && config.AgentGateway.TLS.Mode != "pinned") ||
		config.TCPIngress.MinPort < 1 || config.TCPIngress.MinPort > 65535 ||
		config.TCPIngress.MaxPort < config.TCPIngress.MinPort || config.TCPIngress.MaxPort > 65535 ||
		!validSystemLoggingLevel(config.Logging.Level) || !validSystemPublicLimits(config.Limits) {
		return SystemConfig{}, ErrSystemReadInput
	}
	return SystemConfig{
		Management: SystemManagementConfig{PublicURL: publicURL},
		AgentGateway: SystemAgentGatewayConfig{
			PublicHostname: config.AgentGateway.PublicHostname,
			TLSMode:        config.AgentGateway.TLS.Mode,
		},
		TCPIngress: SystemTCPIngressConfig{
			MinPort: config.TCPIngress.MinPort,
			MaxPort: config.TCPIngress.MaxPort,
		},
		Logging: SystemLoggingConfig{Level: config.Logging.Level},
		Limits: SystemPublicLimits{
			MaxTunnels:                          config.Limits.MaxTunnels,
			MaxConnectors:                       config.Limits.MaxConnectors,
			MaxConnectorsPerTunnel:              config.Limits.MaxConnectorsPerTunnel,
			MaxServicesPerTunnel:                config.Limits.MaxServicesPerTunnel,
			MaxActiveConnections:                config.Limits.MaxActiveConnections,
			MaxConnectionsPerTunnel:             config.Limits.MaxConnectionsPerTunnel,
			MaxConnectionsPerService:            config.Limits.MaxConnectionsPerService,
			MaxConnectionsPerSourceIP:           config.Limits.MaxConnectionsPerSourceIP,
			MaxOpenRatePerSourceIP:              config.Limits.MaxOpenRatePerSourceIP,
			MaxOpenBurstPerSourceIP:             config.Limits.MaxOpenBurstPerSourceIP,
			MaxHTTPRequestsPerSourceIPPerSecond: config.Limits.MaxHTTPRequestsPerSourceIPPerSecond,
			MaxHTTPHeaderBytes:                  config.Limits.MaxHTTPHeaderBytes,
			MaxHTTPBodyBytes:                    config.Limits.MaxHTTPBodyBytes,
		},
		ChangesRequireRestart: true,
	}, nil
}

func validSystemLoggingLevel(level string) bool {
	switch level {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}

func validSystemPublicLimits(limits serverconfig.Limits) bool {
	return limits.MaxTunnels > 0 && limits.MaxConnectors > 0 && limits.MaxConnectorsPerTunnel > 0 &&
		limits.MaxServicesPerTunnel > 0 && limits.MaxActiveConnections > 0 &&
		limits.MaxConnectionsPerTunnel > 0 && limits.MaxConnectionsPerService > 0 &&
		limits.MaxConnectionsPerSourceIP > 0 && limits.MaxOpenRatePerSourceIP > 0 &&
		limits.MaxOpenBurstPerSourceIP > 0 && limits.MaxHTTPRequestsPerSourceIPPerSecond > 0 &&
		limits.MaxHTTPHeaderBytes > 0 && limits.MaxHTTPBodyBytes > 0
}
