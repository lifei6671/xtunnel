package managementapi

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"

	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
)

const managementMaxForwardedHops = 32

var errInvalidManagementRequest = errors.New("invalid management request metadata")

type requestMetadata struct {
	clientIP  netip.Addr
	scheme    string
	authority string
}

type managementSecurityPolicy struct {
	publicOrigin string
	localHTTP    bool
	allowedHosts map[string]struct{}
	trusted      []netip.Prefix
}

func newManagementSecurityPolicy(publicURL string, allowedHosts, trustedProxies []string) (*managementSecurityPolicy, error) {
	parsed, err := url.Parse(publicURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("invalid management public URL")
	}
	localHTTP := parsed.Scheme == "http"
	if localHTTP {
		address, err := netip.ParseAddr(parsed.Hostname())
		if err != nil || !address.IsLoopback() || address.Zone() != "" {
			return nil, errors.New("management HTTP origin must use a loopback IP")
		}
	}
	publicAuthority, err := normalizeManagementAuthority(parsed.Host, parsed.Scheme)
	if err != nil {
		return nil, fmt.Errorf("normalize management public URL: %w", err)
	}
	allowed := map[string]struct{}{publicAuthority: {}}
	for _, value := range allowedHosts {
		authority, err := normalizeManagementAuthority(value, parsed.Scheme)
		if err != nil {
			return nil, fmt.Errorf("normalize management allowed host: %w", err)
		}
		allowed[authority] = struct{}{}
	}
	prefixes := make([]netip.Prefix, 0, len(trustedProxies))
	for _, value := range trustedProxies {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("parse management trusted proxy: %w", err)
		}
		if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return &managementSecurityPolicy{
		publicOrigin: parsed.Scheme + "://" + publicAuthority,
		localHTTP:    localHTTP,
		allowedHosts: allowed,
		trusted:      prefixes,
	}, nil
}

// metadata 只在真实 TCP Peer 位于 Management 自己的可信代理集合时读取 Forwarded Header。
// Host、Origin 和 Login 限流必须共享这一个结果，避免各层以不同来源解释同一请求。
func (policy *managementSecurityPolicy) metadata(request *http.Request) (requestMetadata, error) {
	peer, err := parseManagementPeer(request.RemoteAddr)
	if err != nil {
		return requestMetadata{}, errInvalidManagementRequest
	}
	metadata := requestMetadata{clientIP: peer, scheme: "http", authority: request.Host}
	if request.TLS != nil {
		metadata.scheme = "https"
	}
	// 本机 HTTP 模式固定直接 Peer、Host 和 Scheme；代理头不能扩大其访问边界。
	if policy.localHTTP {
		if !peer.IsLoopback() || request.TLS != nil {
			return requestMetadata{}, errInvalidManagementRequest
		}
	}
	if !policy.localHTTP && policy.trusts(peer) {
		if value, present, err := managementHeader(request.Header, "X-Forwarded-For"); err != nil {
			return requestMetadata{}, err
		} else if present {
			chain, err := parseManagementForwardedFor(value)
			if err != nil {
				return requestMetadata{}, err
			}
			for index := len(chain) - 1; index >= 0; index-- {
				metadata.clientIP = chain[index]
				if !policy.trusts(chain[index]) {
					break
				}
			}
		}
		if value, present, err := managementHeader(request.Header, "X-Forwarded-Proto"); err != nil {
			return requestMetadata{}, err
		} else if present {
			switch strings.ToLower(value) {
			case "http", "https":
				metadata.scheme = strings.ToLower(value)
			default:
				return requestMetadata{}, errInvalidManagementRequest
			}
		}
		if value, present, err := managementHeader(request.Header, "X-Forwarded-Host"); err != nil {
			return requestMetadata{}, err
		} else if present {
			metadata.authority = value
		}
	}
	metadata.authority, err = normalizeManagementAuthority(metadata.authority, metadata.scheme)
	if err != nil {
		return requestMetadata{}, errInvalidManagementRequest
	}
	if _, ok := policy.allowedHosts[metadata.authority]; !ok {
		return requestMetadata{}, errInvalidManagementRequest
	}
	if policy.localHTTP && "http://"+metadata.authority != policy.publicOrigin {
		return requestMetadata{}, errInvalidManagementRequest
	}
	// 远端客户端只能经 HTTPS 或声明 HTTPS 的受信前置代理进入 Management。
	// 明文 HTTP 仅允许真实规范化 Client IP 为 Loopback 的本机维护/开发路径。
	if metadata.scheme != "https" && !metadata.clientIP.IsLoopback() {
		return requestMetadata{}, errInvalidManagementRequest
	}
	return metadata, nil
}

func (policy *managementSecurityPolicy) allowsOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	authority, err := normalizeManagementAuthority(parsed.Host, strings.ToLower(parsed.Scheme))
	if err != nil {
		return false
	}
	return strings.ToLower(parsed.Scheme)+"://"+authority == policy.publicOrigin
}

func (policy *managementSecurityPolicy) trusts(address netip.Addr) bool {
	address = address.WithZone("").Unmap()
	for _, prefix := range policy.trusted {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func parseManagementPeer(remote string) (netip.Addr, error) {
	addressPort, err := netip.ParseAddrPort(remote)
	if err == nil {
		return addressPort.Addr().WithZone("").Unmap(), nil
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return netip.Addr{}, err
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return address.WithZone("").Unmap(), nil
}

func managementHeader(header http.Header, name string) (string, bool, error) {
	var values []string
	for key, candidates := range header {
		if strings.EqualFold(key, name) {
			values = append(values, candidates...)
		}
	}
	if len(values) == 0 {
		return "", false, nil
	}
	if len(values) != 1 {
		return "", false, errInvalidManagementRequest
	}
	value := strings.TrimSpace(values[0])
	if value == "" || strings.Contains(value, ",") && !strings.EqualFold(name, "X-Forwarded-For") {
		return "", false, errInvalidManagementRequest
	}
	return value, true, nil
}

func parseManagementForwardedFor(value string) ([]netip.Addr, error) {
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > managementMaxForwardedHops {
		return nil, errInvalidManagementRequest
	}
	chain := make([]netip.Addr, 0, len(parts))
	for _, part := range parts {
		address, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil {
			return nil, errInvalidManagementRequest
		}
		chain = append(chain, address.WithZone("").Unmap())
	}
	return chain, nil
}

func normalizeManagementAuthority(authority, scheme string) (string, error) {
	if authority == "" || !httpguts.ValidHostHeader(authority) || strings.Contains(authority, ",") {
		return "", errInvalidManagementRequest
	}
	host := authority
	port := ""
	if strings.HasPrefix(authority, "[") {
		closing := strings.LastIndexByte(authority, ']')
		if closing < 0 {
			return "", errInvalidManagementRequest
		}
		host = authority[:closing+1]
		if closing+1 < len(authority) {
			if authority[closing+1] != ':' {
				return "", errInvalidManagementRequest
			}
			port = authority[closing+2:]
		}
	} else if strings.Count(authority, ":") == 1 {
		var ok bool
		host, port, ok = strings.Cut(authority, ":")
		if !ok {
			return "", errInvalidManagementRequest
		}
	} else if strings.Count(authority, ":") > 1 {
		return "", errInvalidManagementRequest
	}
	canonicalHost, err := serverroute.CanonicalHostname(host)
	if err != nil {
		return "", errInvalidManagementRequest
	}
	if port == "" {
		switch strings.ToLower(scheme) {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return "", errInvalidManagementRequest
		}
	}
	parsedPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || parsedPort == 0 {
		return "", errInvalidManagementRequest
	}
	if address, err := netip.ParseAddr(canonicalHost); err == nil && address.Is6() {
		canonicalHost = "[" + canonicalHost + "]"
	}
	return canonicalHost + ":" + strconv.FormatUint(parsedPort, 10), nil
}
