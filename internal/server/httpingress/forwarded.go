package httpingress

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"golang.org/x/net/http/httpguts"

	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
)

const maxForwardedHops = 32

var errInvalidForwardedHeader = errors.New("invalid forwarded header")

type forwardedMetadata struct {
	clientIP netip.Addr
	scheme   string
	host     string
}

type trustedProxySet struct {
	prefixes []netip.Prefix
}

func newTrustedProxySet(values []string) (trustedProxySet, error) {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return trustedProxySet{}, errors.Join(errInvalidForwardedHeader, err)
		}
		prefixes = append(prefixes, normalizedPrefix(prefix))
	}
	return trustedProxySet{prefixes: prefixes}, nil
}

func normalizedPrefix(prefix netip.Prefix) netip.Prefix {
	if prefix.Addr().Is4In6() && prefix.Bits() >= 96 {
		return netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96).Masked()
	}
	return prefix.Masked()
}

func (set trustedProxySet) contains(address netip.Addr) bool {
	address = address.WithZone("").Unmap()
	for _, prefix := range set.prefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// normalizeForwarded 只在 TCP Peer 已由配置明确列为受信代理时消费代理元数据。
// 未受信 Peer 提供的 Header 不参与任何判断，后续 Rewrite 会全部删除并从真实 Peer
// 重建。X-Forwarded-For 只允许一个 Header 行；该行内部的逗号才表示可验证的多跳链。
func (set trustedProxySet) normalizeForwarded(request *http.Request) (forwardedMetadata, error) {
	peer, err := parsePeerIP(request.RemoteAddr)
	if err != nil {
		return forwardedMetadata{}, errInvalidForwardedHeader
	}
	metadata := forwardedMetadata{clientIP: peer, scheme: directRequestScheme(request), host: request.Host}
	if !set.contains(peer) {
		return metadata, nil
	}

	forwardedFor, present, err := singleHeaderValue(request.Header, "X-Forwarded-For")
	if err != nil {
		return forwardedMetadata{}, err
	}
	if present {
		chain, err := parseForwardedFor(forwardedFor)
		if err != nil {
			return forwardedMetadata{}, err
		}
		// Peer 是链的可信锚点。由近到远剥离可信代理，首个不可信地址才是原始
		// Client；更左侧内容可能由该 Client 伪造，绝不能参与限流或审计身份。
		for index := len(chain) - 1; index >= 0; index-- {
			metadata.clientIP = chain[index]
			if !set.contains(chain[index]) {
				break
			}
		}
	}

	forwardedProto, present, err := singleHeaderValue(request.Header, "X-Forwarded-Proto")
	if err != nil {
		return forwardedMetadata{}, err
	}
	if present {
		if strings.Contains(forwardedProto, ",") {
			return forwardedMetadata{}, errInvalidForwardedHeader
		}
		switch strings.ToLower(forwardedProto) {
		case "http", "https":
			metadata.scheme = strings.ToLower(forwardedProto)
		default:
			return forwardedMetadata{}, errInvalidForwardedHeader
		}
	}

	forwardedHost, present, err := singleHeaderValue(request.Header, "X-Forwarded-Host")
	if err != nil {
		return forwardedMetadata{}, err
	}
	if present {
		if strings.Contains(forwardedHost, ",") || !validForwardedHost(forwardedHost) {
			return forwardedMetadata{}, errInvalidForwardedHeader
		}
		metadata.host = forwardedHost
	}
	return metadata, nil
}

func parsePeerIP(remoteAddress string) (netip.Addr, error) {
	if addressPort, err := netip.ParseAddrPort(remoteAddress); err == nil {
		return addressPort.Addr().WithZone("").Unmap(), nil
	}
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return netip.Addr{}, err
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return address.WithZone("").Unmap(), nil
}

func singleHeaderValue(header http.Header, name string) (string, bool, error) {
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
		return "", false, errInvalidForwardedHeader
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return "", false, errInvalidForwardedHeader
	}
	return value, true, nil
}

func parseForwardedFor(value string) ([]netip.Addr, error) {
	chain := make([]netip.Addr, 0, maxForwardedHops)
	remaining := value
	for {
		if len(chain) == maxForwardedHops {
			return nil, errInvalidForwardedHeader
		}
		part, rest, found := strings.Cut(remaining, ",")
		address, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil {
			return nil, errInvalidForwardedHeader
		}
		chain = append(chain, address.WithZone("").Unmap())
		if !found {
			return chain, nil
		}
		remaining = rest
	}
}

func validForwardedHost(value string) bool {
	if !httpguts.ValidHostHeader(value) {
		return false
	}
	// X-Forwarded-Host 是 HTTP authority，而不是 Route 持久化键。公网 authority
	// 中的 IPv6 必须带方括号，不能沿用 CanonicalHostname 对 bare IPv6 的宽松输入。
	if !strings.HasPrefix(value, "[") && strings.Count(value, ":") > 1 {
		return false
	}
	_, err := serverroute.CanonicalHostname(value)
	return err == nil
}

func directRequestScheme(request *http.Request) string {
	if request.TLS != nil {
		return "https"
	}
	return "http"
}

func rewriteForwardedHeaders(header http.Header, metadata forwardedMetadata) {
	for key := range header {
		if strings.EqualFold(key, "Forwarded") || strings.EqualFold(key, "X-Real-IP") ||
			strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {
			delete(header, key)
		}
	}
	header.Set("X-Forwarded-For", metadata.clientIP.String())
	header.Set("X-Forwarded-Proto", metadata.scheme)
	header.Set("X-Forwarded-Host", metadata.host)
}
