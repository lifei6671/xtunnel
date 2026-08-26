package gateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

func TestDialContextUsesOnlyConnectionTokenPin(t *testing.T) {
	identity, err := servergateway.LoadOrCreatePinnedIdentity(t.TempDir(), "gateway.example.test", true, time.Now())
	if err != nil {
		t.Fatalf("创建测试 Gateway 身份失败: %v", err)
	}
	server, err := servergateway.NewServer(servergateway.ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
	})
	if err != nil {
		t.Fatalf("创建测试 Gateway 失败: %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("启动测试 Gateway 失败: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	host, portText, err := net.SplitHostPort(server.Addr().String())
	if err != nil {
		t.Fatalf("拆分测试 Gateway 地址失败: %v", err)
	}
	parsedPort, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("解析测试 Gateway 端口失败: %v", err)
	}

	spkiHash := identity.SPKIHash()
	tokenText := encodePinnedToken(t, host, uint32(parsedPort), spkiHash[:])
	connection, err := DialContext(context.Background(), tokenText, servergateway.ControlALPN)
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer connection.Close()
	if got := connection.ConnectionState().NegotiatedProtocol; got != servergateway.ControlALPN {
		t.Fatalf("协商 ALPN = %q，want %q", got, servergateway.ControlALPN)
	}

	wrongPin := bytes.Repeat([]byte{0x42}, 32)
	wrongTokenText := encodePinnedToken(t, host, uint32(parsedPort), wrongPin)
	if _, err := DialContext(context.Background(), wrongTokenText, servergateway.ControlALPN); !errors.Is(err, ErrPinnedCertificate) {
		t.Fatalf("错误 Pin 的 DialContext() error = %v，want ErrPinnedCertificate", err)
	}
}

func TestDialContextRejectsInvalidTokenAndALPN(t *testing.T) {
	if _, err := DialContext(context.Background(), "xta_invalid", servergateway.ControlALPN); err == nil {
		t.Fatal("畸形 Token 的 DialContext() error = nil")
	}

	tokenText := encodePinnedToken(t, "127.0.0.1", 1, bytes.Repeat([]byte{0x11}, 32))
	if _, err := DialContext(context.Background(), tokenText, "http/1.1"); !errors.Is(err, ErrUnsupportedALPN) {
		t.Fatalf("未知 ALPN 的 DialContext() error = %v，want ErrUnsupportedALPN", err)
	}
}

// encodePinnedToken 仅构造测试用、不含真实凭据的 Connection Token。
func encodePinnedToken(t *testing.T, host string, port uint32, pin []byte) string {
	t.Helper()
	tokenText, err := connectiontoken.Encode(&protocolv1.ConnectionToken{
		FormatVersion: connectiontoken.FormatVersionV1,
		Endpoint:      &protocolv1.GatewayEndpoint{Host: host, Port: port},
		TlsTrust: &protocolv1.TlsTrustDescriptor{
			Mode: &protocolv1.TlsTrustDescriptor_PinnedSpkiSha256{
				PinnedSpkiSha256: &protocolv1.PinnedSPKITrust{SpkiSha256: pin},
			},
		},
		TunnelId:             "tun_01J00000000000000000000000",
		TokenId:              "tok_01J00000000000000000000000",
		TokenVersion:         1,
		AuthenticationSecret: bytes.Repeat([]byte{0x11}, 32),
	})
	if err != nil {
		t.Fatalf("编码测试 Connection Token 失败: %v", err)
	}
	return tokenText
}
