package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
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
	identity, err := servergateway.LoadOrCreatePinnedIdentity(newGatewayTestDataDir(t), "gateway.example.test", true, time.Now())
	if err != nil {
		t.Fatalf("创建测试 Gateway 身份失败: %v", err)
	}
	accepted := make(chan struct{}, 1)
	server, err := servergateway.NewServer(servergateway.ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
		Handle: func(ctx context.Context, _ *tls.Conn, _ servergateway.Protocol) {
			accepted <- struct{}{}
			<-ctx.Done()
		},
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
	// 客户端握手返回与服务端释放握手预算不是同一个调度点。必须等到 Handler
	// 已接管首条连接，才能确认唯一握手槽位已经释放；否则第二条连接可能在收到
	// 服务端证书前被预算边界关闭，只得到普通 EOF，无法据此判断 Pin 是否错误。
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("正确 Pin 的连接未进入 Gateway Handler")
	}

	wrongPin := bytes.Repeat([]byte{0x42}, 32)
	wrongTokenText := encodePinnedToken(t, host, uint32(parsedPort), wrongPin)
	if _, err := DialContext(context.Background(), wrongTokenText, servergateway.ControlALPN); !errors.Is(err, ErrPinnedCertificate) {
		t.Fatalf("错误 Pin 的 DialContext() error = %v，want ErrPinnedCertificate", err)
	}
}

func TestDialContextDoesNotClassifyNetworkEOFAsPinFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("创建测试 TCP Listener 失败: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan struct{})
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
		close(accepted)
	}()

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("拆分测试 Listener 地址失败: %v", err)
	}
	parsedPort, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("解析测试 Listener 端口失败: %v", err)
	}
	tokenText := encodePinnedToken(t, host, uint32(parsedPort), bytes.Repeat([]byte{0x11}, 32))
	_, err = DialContext(context.Background(), tokenText, servergateway.ControlALPN)
	<-accepted
	if err == nil {
		t.Fatal("对端在 TLS 握手前关闭时 DialContext() error = nil")
	}
	if errors.Is(err, ErrPinnedCertificate) {
		t.Fatalf("普通网络关闭被错误归类为 Pin 校验失败: %v", err)
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
