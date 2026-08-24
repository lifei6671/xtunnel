// Package config 定义并加载 XTunnel Agent 配置契约。
package config

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"

	configschemas "github.com/lifei6671/xtunnel/configs"
	baseconfig "github.com/lifei6671/xtunnel/internal/config"
)

// Config 是完成四层合并和校验后的 Agent 配置。
type Config struct {
	Server    Server    `json:"server"`
	Auth      Auth      `json:"auth"`
	DataDir   string    `json:"data_dir"`
	Transport Transport `json:"transport"`
	Reconnect Reconnect `json:"reconnect"`
	Control   Control   `json:"control"`
	Health    Health    `json:"health"`
	Logging   Logging   `json:"logging"`
}

// Server 定义 Agent Gateway 地址和 TLS 信任模式。
type Server struct {
	Endpoint string    `json:"endpoint"`
	TLS      ServerTLS `json:"tls"`
}

// ServerTLS 定义 public 或 pinned TLS 模式需要的字段。
type ServerTLS struct {
	Mode      string `json:"mode"`
	ServerPin string `json:"server_pin"`
}

// Auth 定义 Agent Token 的受控文件来源。
type Auth struct {
	TokenFile string `json:"token_file"`
}

// Transport 定义 Agent 的传输参数。
type Transport struct {
	TCP TransportTCP `json:"tcp"`
}

// TransportTCP 定义本地 WorkConn Pool 的有界容量。
type TransportTCP struct {
	MinIdle       int `json:"min_idle"`
	TargetIdle    int `json:"target_idle"`
	MaxIdle       int `json:"max_idle"`
	MaxConnecting int `json:"max_connecting"`
	MaxTotal      int `json:"max_total"`
}

// Reconnect 定义断线后的指数退避窗口和抖动比例。
type Reconnect struct {
	InitialDelay baseconfig.Duration `json:"initial_delay"`
	MaxDelay     baseconfig.Duration `json:"max_delay"`
	Jitter       float64             `json:"jitter"`
}

// Control 定义 Control Session 有界队列和写超时。
type Control struct {
	HighPriorityQueue int                 `json:"high_priority_queue"`
	NormalQueue       int                 `json:"normal_queue"`
	WriteTimeout      baseconfig.Duration `json:"write_timeout"`
}

// Health 定义中心健康检查执行与批量上报预算。
type Health struct {
	MaxConcurrent          int                 `json:"max_concurrent"`
	MaxChecksPerSecond     int                 `json:"max_checks_per_second"`
	MaxConcurrentPerOrigin int                 `json:"max_concurrent_per_origin"`
	InitialJitter          float64             `json:"initial_jitter"`
	IntervalJitter         float64             `json:"interval_jitter"`
	ReportFlushInterval    baseconfig.Duration `json:"report_flush_interval"`
	ReportBatchSize        int                 `json:"report_batch_size"`
}

// Logging 定义结构化日志级别和固定 JSON 格式。
type Logging struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// Load 合并并校验一份 Agent 配置。
func Load(options baseconfig.Options) (Config, error) {
	return baseconfig.Load[Config](configschemas.AgentSchema(), options, validate)
}

func validate(value *Config) error {
	_, portText, err := net.SplitHostPort(value.Server.Endpoint)
	if err != nil {
		return fmt.Errorf("server.endpoint must use host:port form: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("server.endpoint port must be an integer between 1 and 65535")
	}
	if !filepath.IsAbs(value.DataDir) {
		return fmt.Errorf("data_dir must be absolute")
	}
	if !filepath.IsAbs(value.Auth.TokenFile) {
		return fmt.Errorf("auth.token_file must be absolute")
	}

	pool := value.Transport.TCP
	if pool.MinIdle > pool.TargetIdle || pool.TargetIdle > pool.MaxIdle || pool.MaxIdle > pool.MaxTotal {
		return fmt.Errorf("transport.tcp must satisfy min_idle <= target_idle <= max_idle <= max_total")
	}
	if pool.MaxConnecting > pool.MaxTotal {
		return fmt.Errorf("transport.tcp.max_connecting must not exceed max_total")
	}
	if value.Reconnect.InitialDelay.Duration > value.Reconnect.MaxDelay.Duration {
		return fmt.Errorf("reconnect.initial_delay must not exceed max_delay")
	}
	if value.Health.MaxConcurrentPerOrigin > value.Health.MaxConcurrent {
		return fmt.Errorf("health.max_concurrent_per_origin must not exceed max_concurrent")
	}
	if value.Reconnect.InitialDelay.Duration <= 0 || value.Reconnect.MaxDelay.Duration <= 0 ||
		value.Control.WriteTimeout.Duration <= 0 || value.Health.ReportFlushInterval.Duration <= 0 {
		return fmt.Errorf("agent duration values must be greater than zero")
	}
	return nil
}
