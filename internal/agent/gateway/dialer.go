// Package gateway 仅根据已通过 Protocol v1 完整性校验的 Connection Token 建立 Agent Gateway TLS 连接。
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

const (
	gatewayDialTimeout = 10 * time.Second
	// RFC 7301/8446 将 no_application_protocol 固定为 TLS Alert 120。
	tlsAlertNoApplicationProtocol = 120
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
	dialContext, cancel := context.WithTimeout(ctx, gatewayDialTimeout)
	defer cancel()

	connectionToken, err := connectiontoken.Parse(connectionTokenText)
	if err != nil {
		return nil, fmt.Errorf("parse connection token before gateway dial: %w", err)
	}
	return dialParsedToken(dialContext, connectionToken, alpn, nil, dialDependencies{})
}

type dialPhase uint8

const (
	dialPhaseDNS dialPhase = iota + 1
	dialPhaseTCP
	dialPhaseTLS
	dialPhaseTrust
	dialPhaseALPN
)

type phaseResult struct {
	phase    dialPhase
	passed   bool
	literal  bool
	trust    string
	notAfter time.Time
}

type phaseObserver func(phaseResult)

type dialDependencies struct {
	dialContext  func(context.Context, string, string) (net.Conn, error)
	configureTLS func(*tls.Config)
}

func dialParsedToken(
	ctx context.Context,
	connectionToken *protocolv1.ConnectionToken,
	alpn string,
	observe phaseObserver,
	dependencies dialDependencies,
) (*tls.Conn, error) {
	if alpn != servergateway.ControlALPN && alpn != servergateway.WorkALPN {
		return nil, ErrUnsupportedALPN
	}
	endpoint := connectionToken.GetEndpoint()
	address := net.JoinHostPort(endpoint.GetHost(), strconv.FormatUint(uint64(endpoint.GetPort()), 10))
	config, err := tlsConfigForToken(connectionToken, alpn)
	if err != nil {
		return nil, err
	}
	if dependencies.configureTLS != nil {
		dependencies.configureTLS(config)
	}
	dialContext := dependencies.dialContext
	if dialContext == nil {
		dialer := &net.Dialer{}
		dialContext = dialer.DialContext
	}
	literal := false
	if _, parseErr := netip.ParseAddr(endpoint.GetHost()); parseErr == nil {
		literal = true
		observePhase(observe, phaseResult{phase: dialPhaseDNS, passed: true, literal: true})
	}
	connection, err := dialContext(ctx, "tcp", address)
	if err != nil {
		if !literal {
			var dnsError *net.DNSError
			if errors.As(err, &dnsError) {
				observePhase(observe, phaseResult{phase: dialPhaseDNS})
				return nil, fmt.Errorf("resolve agent gateway: %w", err)
			}
			observePhase(observe, phaseResult{phase: dialPhaseDNS, passed: true})
		}
		observePhase(observe, phaseResult{phase: dialPhaseTCP})
		return nil, fmt.Errorf("dial agent gateway: %w", err)
	}
	if !literal {
		observePhase(observe, phaseResult{phase: dialPhaseDNS, passed: true})
	}
	observePhase(observe, phaseResult{phase: dialPhaseTCP, passed: true})

	// TCP 建连后由当前函数唯一持有连接。TLS、Trust 或 ALPN 任一步失败都会在
	// 返回前关闭底层 Socket；只有全部通过时才把所有权转交给调用方。
	tlsConnection := tls.Client(connection, config)
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		_ = tlsConnection.Close()
		if isNoApplicationProtocolAlert(err) {
			observePhase(observe, phaseResult{phase: dialPhaseALPN})
			return nil, fmt.Errorf("negotiate agent gateway ALPN: %w", errors.Join(ErrUnsupportedALPN, err))
		}
		if isTrustError(err) {
			observePhase(observe, phaseResult{phase: dialPhaseTLS, passed: true})
			observePhase(observe, phaseResult{phase: dialPhaseTrust})
			return nil, fmt.Errorf("verify agent gateway certificate: %w", err)
		}
		observePhase(observe, phaseResult{phase: dialPhaseTLS})
		return nil, fmt.Errorf("handshake agent gateway TLS: %w", err)
	}
	state := tlsConnection.ConnectionState()
	if state.Version < tls.VersionTLS13 {
		_ = tlsConnection.Close()
		observePhase(observe, phaseResult{phase: dialPhaseTLS})
		return nil, errors.New("agent gateway TLS version is unsupported")
	}
	observePhase(observe, phaseResult{phase: dialPhaseTLS, passed: true})
	if len(state.PeerCertificates) == 0 {
		_ = tlsConnection.Close()
		observePhase(observe, phaseResult{phase: dialPhaseTrust})
		return nil, errors.New("agent gateway did not provide a certificate")
	}
	observePhase(observe, phaseResult{
		phase:    dialPhaseTrust,
		passed:   true,
		trust:    trustMode(connectionToken),
		notAfter: state.PeerCertificates[0].NotAfter,
	})
	if state.NegotiatedProtocol != alpn {
		_ = tlsConnection.Close()
		observePhase(observe, phaseResult{phase: dialPhaseALPN})
		return nil, ErrUnsupportedALPN
	}
	observePhase(observe, phaseResult{phase: dialPhaseALPN, passed: true})
	return tlsConnection, nil
}

func isNoApplicationProtocolAlert(err error) bool {
	if errors.Is(err, tls.AlertError(tlsAlertNoApplicationProtocol)) {
		return true
	}
	var operationError *net.OpError
	if !errors.As(err, &operationError) || operationError.Err == nil {
		return false
	}
	// Go 1.27 的 TCP TLS 路径把远端 Alert 保存为 net.OpError.Err 内的
	// crypto/tls.alert；公开的 tls.AlertError 只用于 QUIC。项目固定精确
	// Go 1.27 工具链，因此在没有公开 TCP Alert 类型时只读取该 uint8 Alert
	// 编号，不匹配本地化或可能漂移的错误字符串。
	alertType := reflect.TypeOf(operationError.Err)
	if alertType.PkgPath() != "crypto/tls" || alertType.Name() != "alert" || alertType.Kind() != reflect.Uint8 {
		return false
	}
	return reflect.ValueOf(operationError.Err).Uint() == tlsAlertNoApplicationProtocol
}

func observePhase(observe phaseObserver, result phaseResult) {
	if observe != nil {
		observe(result)
	}
}

func isTrustError(err error) bool {
	if errors.Is(err, ErrPinnedCertificate) {
		return true
	}
	var certificateError *tls.CertificateVerificationError
	return errors.As(err, &certificateError)
}

func trustMode(connectionToken *protocolv1.ConnectionToken) string {
	switch connectionToken.GetTlsTrust().GetMode().(type) {
	case *protocolv1.TlsTrustDescriptor_PublicCa:
		return "public_ca"
	case *protocolv1.TlsTrustDescriptor_PinnedSpkiSha256:
		return "pinned_spki"
	default:
		return ""
	}
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
