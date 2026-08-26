// Package config 定义并加载 XTunnel Server 配置契约。
package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"

	configschemas "github.com/lifei6671/xtunnel/configs"
	baseconfig "github.com/lifei6671/xtunnel/internal/config"
)

// Config 是完成四层合并和校验后的 Server 配置。
type Config struct {
	Server           Server           `json:"server"`
	Management       Management       `json:"management"`
	HTTPIngress      HTTPIngress      `json:"http_ingress"`
	AgentGateway     AgentGateway     `json:"agent_gateway"`
	Transport        Transport        `json:"transport"`
	Control          Control          `json:"control"`
	TCPIngress       TCPIngress       `json:"tcp_ingress"`
	ConnectorRuntime ConnectorRuntime `json:"connector_runtime"`
	Metrics          Metrics          `json:"metrics"`
	Logging          Logging          `json:"logging"`
	Limits           Limits           `json:"limits"`
}

// Server 定义 Server 自身的数据目录配置。
type Server struct {
	DataDir string `json:"data_dir"`
}

// Management 定义管理端监听、外部 Origin 和可信代理边界。
type Management struct {
	Listen         string   `json:"listen"`
	PublicURL      string   `json:"public_url"`
	AllowedHosts   []string `json:"allowed_hosts"`
	TrustedProxies []string `json:"trusted_proxies"`
}

// HTTPIngress 定义公网 HTTP 入口和可信代理边界。
type HTTPIngress struct {
	Listen         string   `json:"listen"`
	TrustedProxies []string `json:"trusted_proxies"`
}

// AgentGateway 定义 Agent Gateway 的监听地址和 TLS 身份。
type AgentGateway struct {
	Listen         string          `json:"listen"`
	PublicHostname string          `json:"public_hostname"`
	TLS            AgentGatewayTLS `json:"tls"`
}

// AgentGatewayTLS 定义 public 或 pinned TLS 模式需要的字段。
type AgentGatewayTLS struct {
	Mode     string `json:"mode"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

// Transport 定义隧道传输参数。
type Transport struct {
	TCP TransportTCP `json:"tcp"`
}

// TransportTCP 定义 TCP WorkConn 获取参数。
type TransportTCP struct {
	WorkAcquireTimeout baseconfig.Duration `json:"work_acquire_timeout"`
}

// Control 定义 Control Session 有界队列和写超时。
type Control struct {
	HighPriorityQueue int                 `json:"high_priority_queue"`
	NormalQueue       int                 `json:"normal_queue"`
	WriteTimeout      baseconfig.Duration `json:"write_timeout"`
}

// TCPIngress 定义产品 TCP Listener 可使用的绑定地址和端口范围。
type TCPIngress struct {
	Bind    string `json:"bind"`
	MinPort int    `json:"min_port"`
	MaxPort int    `json:"max_port"`
}

// ConnectorRuntime 定义 Server 判定 Connector 存活的心跳窗口。
type ConnectorRuntime struct {
	HeartbeatInterval baseconfig.Duration `json:"heartbeat_interval"`
	HeartbeatTimeout  baseconfig.Duration `json:"heartbeat_timeout"`
}

// Metrics 定义 Prometheus 监听地址和路径。
type Metrics struct {
	Listen string `json:"listen"`
	Path   string `json:"path"`
}

// Logging 定义结构化日志级别和固定 JSON 格式。
type Logging struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// Limits 定义 Server 的硬资源预算；默认值和范围只来自 Server Schema。
type Limits struct {
	MaxTunnels                          int `json:"max_tunnels"`
	MaxConnectors                       int `json:"max_connectors"`
	MaxConnectorsPerTunnel              int `json:"max_connectors_per_tunnel"`
	MaxServicesPerTunnel                int `json:"max_services_per_tunnel"`
	MaxHealthTargetsPerTunnel           int `json:"max_health_targets_per_tunnel"`
	MaxHealthTargetsGlobal              int `json:"max_health_targets_global"`
	MaxTunnelSnapshotBytes              int `json:"max_tunnel_snapshot_bytes"`
	MaxActiveConnections                int `json:"max_active_connections"`
	MaxConnectionsPerTunnel             int `json:"max_connections_per_tunnel"`
	MaxConnectionsPerService            int `json:"max_connections_per_service"`
	MaxConnectionsPerSourceIP           int `json:"max_connections_per_source_ip"`
	MaxOpenRatePerSourceIP              int `json:"max_open_rate_per_source_ip"`
	MaxOpenBurstPerSourceIP             int `json:"max_open_burst_per_source_ip"`
	MaxHTTPRequestsPerSourceIPPerSecond int `json:"max_http_requests_per_source_ip_per_second"`
	MaxWorkConnections                  int `json:"max_work_connections"`
	MaxIdleWorkConnections              int `json:"max_idle_work_connections"`
	MaxConnectingWorkConnections        int `json:"max_connecting_work_connections"`
	MaxPendingOpens                     int `json:"max_pending_opens"`
	MaxPendingAuth                      int `json:"max_pending_auth"`
	MaxPendingTLSHandshakes             int `json:"max_pending_tls_handshakes"`
	MaxReplayEntriesPerSession          int `json:"max_replay_entries_per_session"`
	MaxControlFrameBytes                int `json:"max_control_frame_bytes"`
	MaxAuthFrameBytes                   int `json:"max_auth_frame_bytes"`
	MaxWorkFrameBytes                   int `json:"max_work_frame_bytes"`
	MaxHTTPHeaderBytes                  int `json:"max_http_header_bytes"`
}

// Load 合并并校验一份 Server 配置。
func Load(options baseconfig.Options) (Config, error) {
	return baseconfig.Load[Config](configschemas.ServerSchema(), options, validate)
}

func validate(value *Config) error {
	if !filepath.IsAbs(value.Server.DataDir) {
		return fmt.Errorf("server.data_dir must be absolute")
	}
	for name, address := range map[string]string{
		"management.listen":    value.Management.Listen,
		"http_ingress.listen":  value.HTTPIngress.Listen,
		"agent_gateway.listen": value.AgentGateway.Listen,
		"metrics.listen":       value.Metrics.Listen,
	} {
		if err := validateListenAddress(address); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	for name, prefixes := range map[string][]string{
		"management.trusted_proxies":   value.Management.TrustedProxies,
		"http_ingress.trusted_proxies": value.HTTPIngress.TrustedProxies,
	} {
		for _, prefix := range prefixes {
			if _, err := netip.ParsePrefix(prefix); err != nil {
				return fmt.Errorf("%s contains invalid CIDR %q: %w", name, prefix, err)
			}
		}
	}
	if _, err := netip.ParseAddr(value.TCPIngress.Bind); err != nil {
		return fmt.Errorf("tcp_ingress.bind must be an IP address: %w", err)
	}

	publicURL, err := url.Parse(value.Management.PublicURL)
	if err != nil {
		return fmt.Errorf("parse management.public_url: %w", err)
	}
	if publicURL.Scheme != "https" || publicURL.Host == "" {
		return fmt.Errorf("management.public_url must be an absolute https URL")
	}
	if publicURL.User != nil || publicURL.RawQuery != "" || publicURL.Fragment != "" || (publicURL.Path != "" && publicURL.Path != "/") {
		return fmt.Errorf("management.public_url must not contain userinfo, query, fragment, or a non-root path")
	}

	if value.TCPIngress.MinPort > value.TCPIngress.MaxPort {
		return fmt.Errorf("tcp_ingress.min_port must not exceed tcp_ingress.max_port")
	}
	if value.ConnectorRuntime.HeartbeatInterval.Duration*3 > value.ConnectorRuntime.HeartbeatTimeout.Duration {
		return fmt.Errorf("connector_runtime.heartbeat_interval must not exceed one third of heartbeat_timeout")
	}
	if value.Transport.TCP.WorkAcquireTimeout.Duration <= 0 || value.Control.WriteTimeout.Duration <= 0 ||
		value.ConnectorRuntime.HeartbeatInterval.Duration <= 0 || value.ConnectorRuntime.HeartbeatTimeout.Duration <= 0 {
		return fmt.Errorf("server duration values must be greater than zero")
	}
	if value.Limits.MaxConnectorsPerTunnel > value.Limits.MaxConnectors {
		return fmt.Errorf("limits.max_connectors_per_tunnel must not exceed max_connectors")
	}
	if value.Limits.MaxHealthTargetsPerTunnel > value.Limits.MaxHealthTargetsGlobal {
		return fmt.Errorf("limits.max_health_targets_per_tunnel must not exceed max_health_targets_global")
	}
	if value.Limits.MaxIdleWorkConnections > value.Limits.MaxWorkConnections {
		return fmt.Errorf("limits.max_idle_work_connections must not exceed max_work_connections")
	}
	if value.Limits.MaxConnectingWorkConnections > value.Limits.MaxWorkConnections {
		return fmt.Errorf("limits.max_connecting_work_connections must not exceed max_work_connections")
	}
	return nil
}

func validateListenAddress(address string) error {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("must use host:port form: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be an integer between 1 and 65535")
	}
	return nil
}
