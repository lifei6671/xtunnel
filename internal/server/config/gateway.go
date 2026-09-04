package config

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

// PublicEndpoint 将配置中的主机或主机:端口解析为 Token 和 TLS 共用的地址。
// 显式公网端口用于端口映射；省略端口时沿用监听端口。IPv6 带端口必须使用方括号。
func (gateway AgentGateway) PublicEndpoint() (string, uint16, error) {
	host := gateway.PublicHostname
	explicitPort := false
	_, portText, err := net.SplitHostPort(gateway.Listen)
	if err != nil {
		return "", 0, fmt.Errorf("parse agent_gateway.listen: %w", err)
	}
	if _, err := netip.ParseAddr(host); err != nil && strings.ContainsAny(host, ":[]") {
		explicitPort = true
		host, portText, err = net.SplitHostPort(host)
		if err != nil {
			return "", 0, fmt.Errorf("agent_gateway.public_hostname must be a host or host:port: %w", err)
		}
		if strings.HasPrefix(gateway.PublicHostname, "[") {
			address, err := netip.ParseAddr(host)
			if err != nil || !address.Is6() {
				return "", 0, fmt.Errorf("agent_gateway.public_hostname brackets require an IPv6 address")
			}
		}
	}
	if host == "" || strings.ContainsAny(host, "/\\@?#[]") || strings.IndexFunc(host, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return "", 0, fmt.Errorf("agent_gateway.public_hostname contains an invalid host")
	}
	if strings.Contains(host, ":") {
		address, err := netip.ParseAddr(host)
		if err != nil || address.Zone() != "" {
			return "", 0, fmt.Errorf("agent_gateway.public_hostname contains an invalid IP address")
		}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	// Listen 的 0 端口仅用于程序内动态绑定；生产配置由 validateListenAddress 拒绝。
	// 显式发布的公网端口不能为 0，否则无法生成可连接的 Token。
	if err != nil || (port == 0 && explicitPort) {
		return "", 0, fmt.Errorf("agent gateway public port must be between 1 and 65535")
	}
	return host, uint16(port), nil
}
