// Package gateway 仅根据已通过 Protocol v1 完整性校验的 Connection Token 建立 Agent Gateway TLS 连接。
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

var (
	// ErrUnsupportedALPN 表示调用方请求了当前 Gateway 不支持的协议。
	ErrUnsupportedALPN = errors.New("agent gateway ALPN is unsupported")
	// ErrPinnedCertificate 表示 pinned 模式下证书有效期或 SPKI 摘要不符合 Token。
	ErrPinnedCertificate = errors.New("agent gateway pinned certificate verification failed")
)

// DialContext 先严格 Parse Connection Token，再从其中唯一的 Endpoint/Trust Descriptor 构造 TLS 连接。
// 该 API 不接受独立 Endpoint、CA 文件、Pin 或 insecure 开关，从接口层避免 TOFU 和降级路径。
func DialContext(ctx context.Context, connectionTokenText, alpn string) (*tls.Conn, error) {
	connectionToken, err := connectiontoken.Parse(connectionTokenText)
	if err != nil {
		return nil, fmt.Errorf("parse connection token before gateway dial: %w", err)
	}
	return dialParsedToken(ctx, connectionToken, alpn)
}

func dialParsedToken(ctx context.Context, connectionToken *protocolv1.ConnectionToken, alpn string) (*tls.Conn, error) {
	if alpn != servergateway.ControlALPN && alpn != servergateway.WorkALPN {
		return nil, ErrUnsupportedALPN
	}
	endpoint := connectionToken.GetEndpoint()
	address := net.JoinHostPort(endpoint.GetHost(), strconv.FormatUint(uint64(endpoint.GetPort()), 10))
	config, err := tlsConfigForToken(connectionToken, alpn)
	if err != nil {
		return nil, err
	}
	dialer := &tls.Dialer{Config: config, NetDialer: &net.Dialer{Timeout: 10 * time.Second}}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial agent gateway: %w", err)
	}
	tlsConnection, ok := connection.(*tls.Conn)
	if !ok {
		_ = connection.Close()
		return nil, errors.New("agent gateway dial did not return TLS connection")
	}
	if tlsConnection.ConnectionState().Version < tls.VersionTLS13 || tlsConnection.ConnectionState().NegotiatedProtocol != alpn {
		_ = tlsConnection.Close()
		return nil, ErrUnsupportedALPN
	}
	return tlsConnection, nil
}

func tlsConfigForToken(connectionToken *protocolv1.ConnectionToken, alpn string) (*tls.Config, error) {
	endpoint := connectionToken.GetEndpoint()
	config := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: endpoint.GetHost(),
		NextProtos: []string{alpn},
	}
	switch trust := connectionToken.GetTlsTrust().GetMode().(type) {
	case *protocolv1.TlsTrustDescriptor_PublicCa:
		return config, nil
	case *protocolv1.TlsTrustDescriptor_PinnedSpkiSha256:
		wanted := append([]byte(nil), trust.PinnedSpkiSha256.GetSpkiSha256()...)
		// Pinned 自签证书不属于系统根；这里禁用的是默认 CA 链校验，而不是安全校验。
		// VerifyConnection 仍会在握手提交前严格校验证书有效期和 SPKI，且没有任何回退分支。
		config.InsecureSkipVerify = true // #nosec G402 -- 完整的 pinned SPKI 校验见 VerifyConnection。
		config.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return ErrPinnedCertificate
			}
			leaf := state.PeerCertificates[0]
			now := time.Now()
			if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
				return ErrPinnedCertificate
			}
			actual := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			if len(wanted) != len(actual) || !constantTimeEqual(actual[:], wanted) {
				return ErrPinnedCertificate
			}
			return nil
		}
		return config, nil
	default:
		return nil, errors.New("connection token TLS trust mode is invalid")
	}
}

func constantTimeEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}
