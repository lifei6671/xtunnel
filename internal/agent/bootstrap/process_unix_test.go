//go:build !windows

package bootstrap

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

func TestProcessExitsOnSIGTERM(t *testing.T) {
	if os.Getenv("GO_WANT_XTUNNEL_AGENT_HELPER_PROCESS") == "1" {
		for index, arg := range os.Args {
			if arg == "--" {
				os.Exit(Execute("xtunnel-agent", os.Args[index+1:], os.Environ(), os.Stdout, os.Stderr))
			}
		}
		os.Exit(2)
	}

	identity, err := servergateway.LoadOrCreatePinnedIdentity(t.TempDir(), "gateway.example.test", true, time.Now())
	if err != nil {
		t.Fatalf("创建测试 Gateway 身份失败: %v", err)
	}
	connected := make(chan struct{})
	var connectedOnce sync.Once
	server, err := servergateway.NewServer(servergateway.ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
		Handle: func(ctx context.Context, _ *tls.Conn, _ servergateway.Protocol) {
			// 本测试只需让生产 Agent 停在真实 Gateway 连接生命周期中；收到进程信号后，
			// Agent 必须依靠 Context 和连接 Deadline 自行解除阻塞并以零状态退出。
			connectedOnce.Do(func() { close(connected) })
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
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("解析测试 Gateway 端口失败: %v", err)
	}
	spkiHash := identity.SPKIHash()
	token, err := connectiontoken.Encode(&protocolv1.ConnectionToken{
		FormatVersion: connectiontoken.FormatVersionV1,
		Endpoint:      &protocolv1.GatewayEndpoint{Host: host, Port: uint32(port)},
		TlsTrust: &protocolv1.TlsTrustDescriptor{
			Mode: &protocolv1.TlsTrustDescriptor_PinnedSpkiSha256{
				PinnedSpkiSha256: &protocolv1.PinnedSPKITrust{SpkiSha256: spkiHash[:]},
			},
		},
		TunnelId:             "tun_01J00000000000000000000000",
		TokenId:              "tok_01J00000000000000000000000",
		TokenVersion:         1,
		AuthenticationSecret: bytes.Repeat([]byte{0x37}, 32),
	})
	if err != nil {
		t.Fatalf("编码测试 Connection Token 失败: %v", err)
	}
	args := []string{
		"-test.run=^TestProcessExitsOnSIGTERM$", "--",
		"run", "--token", token,
	}
	command := exec.Command(os.Args[0], args...)
	command.Env = append(os.Environ(), "GO_WANT_XTUNNEL_AGENT_HELPER_PROCESS=1")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("command.Start() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	exited := false
	t.Cleanup(func() {
		if exited {
			return
		}
		_ = command.Process.Kill()
		<-done
	})

	select {
	case err := <-done:
		exited = true
		t.Fatalf("process exited before signal: %v\n%s", err, output.String())
	case <-connected:
		// 只有子进程已经完成真实 TLS/ALPN 接入后才发送信号，避免用固定休眠
		// 猜测调度时序，确保测试验证的是运行中进程的优雅退出。
	case <-time.After(2 * time.Second):
		t.Fatalf("process did not connect to test Gateway\n%s", output.String())
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Process.Signal(SIGTERM) error = %v", err)
	}

	select {
	case err := <-done:
		exited = true
		if err != nil {
			t.Fatalf("process exit error = %v\n%s", err, output.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
	if strings.Contains(output.String(), token) {
		t.Fatal("process output leaked Token")
	}
}
