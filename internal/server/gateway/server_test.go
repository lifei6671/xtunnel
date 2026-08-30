package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/safego"
)

func TestServerAcceptsOnlyFrozenALPN(t *testing.T) {
	identity := testIdentity(t)
	accepted := make(chan Protocol, 1)
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 2,
		Handle: func(_ context.Context, connection *tls.Conn, protocol Protocol) {
			accepted <- protocol
			_ = connection.Close()
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := server.Start(ctx); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	tests := []struct {
		name       string
		alpn       string
		wantAccept bool
		dialFails  bool
	}{
		{name: "control", alpn: ControlALPN, wantAccept: true},
		{name: "work", alpn: WorkALPN, wantAccept: true},
		{name: "unknown", alpn: "http/1.1", wantAccept: false, dialFails: true},
		{name: "empty", alpn: "", wantAccept: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection, err := dialTLS(server.Addr().String(), test.alpn)
			if test.dialFails {
				if err == nil {
					_ = connection.Close()
					t.Fatal("TLS handshake accepted unknown ALPN")
				}
				return
			}
			if err != nil {
				t.Fatalf("TLS dial error = %v", err)
			}
			defer connection.Close()
			if test.wantAccept {
				select {
				case protocol := <-accepted:
					if (test.alpn == ControlALPN && protocol != ControlProtocol) || (test.alpn == WorkALPN && protocol != WorkProtocol) {
						t.Fatalf("accepted protocol = %q", protocol)
					}
				case <-time.After(time.Second):
					t.Fatal("Gateway did not dispatch supported ALPN")
				}
				return
			}
			select {
			case protocol := <-accepted:
				t.Fatalf("Gateway dispatched unsupported ALPN as %q", protocol)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestServerBoundsPendingTLSHandshakes(t *testing.T) {
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                testIdentity(t),
		MaxPendingTLSHandshakes: 1,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	first, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial(first) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial(second) error = %v", err)
	}
	defer second.Close()
	if err := second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("second.SetReadDeadline() error = %v", err)
	}
	buffer := make([]byte, 1)
	if _, err := second.Read(buffer); err == nil {
		t.Fatal("second connection remained open after handshake budget was exhausted")
	}
}

func TestServerReleasesHandshakeBudgetBeforeLongLivedProtocolHandler(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                testIdentity(t),
		MaxPendingTLSHandshakes: 1,
		Handle: func(context.Context, *tls.Conn, Protocol) {
			entered <- struct{}{}
			<-release
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() {
		close(release)
		_ = server.Close()
	})

	first, err := dialTLS(server.Addr().String(), ControlALPN)
	if err != nil {
		t.Fatalf("dial first established protocol connection: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first protocol handler did not start")
	}

	// 第一条协议连接仍在 Handler 内存活，但它已经完成 TLS Handshake，不应继续
	// 占用仅有的握手槽位。第二条连接必须仍可完成握手并进入 Handler。
	second, err := dialTLS(server.Addr().String(), WorkALPN)
	if err != nil {
		t.Fatalf("dial second connection after first handshake completed: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second protocol handler did not start after handshake budget release")
	}
}

func TestServerStopAcceptingPreservesEstablishedHandlerUntilClose(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                testIdentity(t),
		MaxPendingTLSHandshakes: 1,
		Handle: func(ctx context.Context, _ *tls.Conn, _ Protocol) {
			close(entered)
			<-ctx.Done()
			close(exited)
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	connection, err := dialTLS(server.Addr().String(), ControlALPN)
	if err != nil {
		t.Fatalf("dial established handler: %v", err)
	}
	defer connection.Close()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("protocol handler did not start")
	}
	address := server.Addr().String()
	if err := server.StopAccepting(); err != nil {
		t.Fatalf("Server.StopAccepting() error = %v", err)
	}
	if late, err := net.DialTimeout("tcp", address, 50*time.Millisecond); err == nil {
		_ = late.Close()
		t.Fatal("new TCP connection succeeded after StopAccepting")
	}
	select {
	case <-exited:
		t.Fatal("StopAccepting canceled an established handler")
	case <-time.After(20 * time.Millisecond):
	}
	if err := server.Close(); err != nil {
		t.Fatalf("Server.Close() error = %v", err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the established handler")
	}
}

func TestServerStartOnlyOnce(t *testing.T) {
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                testIdentity(t),
		MaxPendingTLSHandshakes: 1,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("first Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.Start(context.Background()); err == nil {
		t.Fatal("second Server.Start() error = nil, want rejection")
	}
}

func TestServerRecoversProtocolHandlerPanicAndStops(t *testing.T) {
	runtimeErrors := make(chan error, 1)
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                testIdentity(t),
		MaxPendingTLSHandshakes: 1,
		Handle: func(context.Context, *tls.Conn, Protocol) {
			panic("test protocol handler panic")
		},
		ReportRuntimeError: func(err error) {
			runtimeErrors <- err
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	address := server.Addr().String()
	connection, err := dialTLS(address, ControlALPN)
	if err != nil {
		t.Fatalf("dial protocol handler that panics: %v", err)
	}
	defer connection.Close()

	select {
	case runtimeErr := <-runtimeErrors:
		if !errors.Is(runtimeErr, safego.ErrPanic) {
			t.Fatalf("runtime error = %v, want safego.ErrPanic", runtimeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Gateway did not report protocol handler panic")
	}
	waitForGatewayCondition(t, func() bool {
		late, dialErr := net.DialTimeout("tcp", address, 10*time.Millisecond)
		if dialErr != nil {
			return true
		}
		_ = late.Close()
		return false
	})
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("connection.SetReadDeadline() error = %v", err)
	}
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Fatal("panicking protocol handler left its connection open")
	}
	if err := server.Close(); !errors.Is(err, safego.ErrPanic) {
		t.Fatalf("Server.Close() error = %v, want safego.ErrPanic", err)
	}
}

func TestServerRecoversRenewalLoopPanicAndStops(t *testing.T) {
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity, err := LoadOrCreatePinnedIdentity(t.TempDir(), "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	runtimeErrors := make(chan error, 1)
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
		renewalCheckInterval:    time.Millisecond,
		now: func() time.Time {
			panic("test renewal loop panic")
		},
		ReportRuntimeError: func(err error) {
			runtimeErrors <- err
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	select {
	case runtimeErr := <-runtimeErrors:
		if !errors.Is(runtimeErr, safego.ErrPanic) {
			t.Fatalf("runtime error = %v, want safego.ErrPanic", runtimeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Gateway did not report renewal loop panic")
	}
	if err := server.Close(); !errors.Is(err, safego.ErrPanic) {
		t.Fatalf("Server.Close() error = %v, want safego.ErrPanic", err)
	}
}

func TestServerRenewsRunningPinnedIdentityAndHotLoadsNewHandshakes(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 2,
		// 将周期设为很长，令测试显式触发一次续签并验证运行中热加载。
		renewalCheckInterval: time.Hour,
		now:                  func() time.Time { return createdAt.Add(367 * 24 * time.Hour) },
		Handle: func(ctx context.Context, _ *tls.Conn, _ Protocol) {
			// 保持已经完成握手的连接，直到 Server 关闭，验证续签不影响旧连接。
			<-ctx.Done()
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if got := server.MetricsSnapshot().CertificateExpiryUnixSeconds; got != identity.Leaf().NotAfter.Unix() {
		t.Fatalf("certificate expiry before renewal = %d, want %d", got, identity.Leaf().NotAfter.Unix())
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	oldConnection, err := dialTLS(server.Addr().String(), ControlALPN)
	if err != nil {
		t.Fatalf("dialTLS(before renewal) error = %v", err)
	}
	defer oldConnection.Close()
	oldCertificate := oldConnection.ConnectionState().PeerCertificates[0].Raw
	if !bytes.Equal(oldCertificate, identity.Leaf().Raw) {
		t.Fatal("old TLS handshake did not receive the original certificate")
	}

	server.renewPinnedIdentity(context.Background())
	if err := server.LastRenewalError(); err != nil {
		t.Fatalf("Server.LastRenewalError() = %v", err)
	}
	wantRenewedExpiry := createdAt.Add(367 * 24 * time.Hour).Add(397 * 24 * time.Hour).Unix()
	if got := server.MetricsSnapshot().CertificateExpiryUnixSeconds; got != wantRenewedExpiry {
		t.Fatalf("certificate expiry after renewal = %d, want %d", got, wantRenewedExpiry)
	}
	newConnection, err := dialTLS(server.Addr().String(), ControlALPN)
	if err != nil {
		t.Fatalf("dialTLS(after renewal) error = %v", err)
	}
	defer newConnection.Close()
	newCertificate := newConnection.ConnectionState().PeerCertificates[0].Raw
	if bytes.Equal(oldCertificate, newCertificate) {
		t.Fatal("new TLS handshake did not receive the renewed certificate")
	}
	if !bytes.Equal(oldConnection.ConnectionState().PeerCertificates[0].Raw, oldCertificate) {
		t.Fatal("renewal changed the certificate already negotiated by an old connection")
	}
}

func TestServerMetricsSnapshotReadsPublicIdentityExpiry(t *testing.T) {
	issuedAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	certificate, err := newSelfSignedCertificate("public-gateway.example.test", issuedAt)
	if err != nil {
		t.Fatalf("newSelfSignedCertificate() error = %v", err)
	}
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "public.key")
	certPath := filepath.Join(directory, "public.crt")
	if err := writeKeyPair(keyPath, certPath, certificate); err != nil {
		t.Fatalf("writeKeyPair() error = %v", err)
	}
	identity, err := LoadPublicIdentity(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadPublicIdentity() error = %v", err)
	}
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0", Identity: identity, MaxPendingTLSHandshakes: 1,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if got := server.MetricsSnapshot().CertificateExpiryUnixSeconds; got != certificate.leaf.NotAfter.Unix() {
		t.Fatalf("public certificate expiry = %d, want %d", got, certificate.leaf.NotAfter.Unix())
	}
}

func TestServerPinnedRenewalWaitsForMaintenanceBarrier(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	barrierEntered := make(chan struct{})
	barrierRelease := make(chan struct{})
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
		now:                     func() time.Time { return createdAt.Add(367 * 24 * time.Hour) },
		AcquireMaintenanceBarrier: func(ctx context.Context) (func(), error) {
			close(barrierEntered)
			select {
			case <-barrierRelease:
				return func() {}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.renewPinnedIdentity(context.Background())
	}()
	<-barrierEntered

	persisted, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() while barrier held error = %v", err)
	}
	if !bytes.Equal(persisted.Leaf().Raw, identity.Leaf().Raw) {
		t.Fatal("pinned identity changed before the maintenance barrier was acquired")
	}
	close(barrierRelease)
	<-done
	persisted, err = LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after barrier release error = %v", err)
	}
	if bytes.Equal(persisted.Leaf().Raw, identity.Leaf().Raw) {
		t.Fatal("pinned identity was not renewed after the maintenance barrier was acquired")
	}
}

func TestServerRenewalLoopStopsWithClose(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
		renewalCheckInterval:    time.Millisecond,
		now:                     func() time.Time { return createdAt.Add(367 * 24 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	waitForGatewayCondition(t, func() bool {
		certificate, getErr := server.getCertificate(&tls.ClientHelloInfo{})
		return getErr == nil && !bytes.Equal(certificate.Certificate[0], identity.Leaf().Raw)
	})
	if err := server.Close(); err != nil {
		t.Fatalf("Server.Close() error = %v", err)
	}
}

func TestServerCloseCancelsRenewalWaitingForMaintenanceBarrier(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	barrierWaiting := make(chan struct{})
	var waitingOnce sync.Once
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
		renewalCheckInterval:    time.Millisecond,
		now:                     func() time.Time { return createdAt.Add(367 * 24 * time.Hour) },
		AcquireMaintenanceBarrier: func(ctx context.Context) (func(), error) {
			waitingOnce.Do(func() { close(barrierWaiting) })
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	select {
	case <-barrierWaiting:
	case <-time.After(time.Second):
		t.Fatal("renewal did not begin waiting for the maintenance barrier")
	}
	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Server.Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Server.Close() did not cancel a barrier-blocked renewal")
	}
}

func TestServerRenewalFailureRetainsOldIdentityAndExposesError(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	// 续签临时文件无法创建时，后台任务必须保留旧身份并公开失败状态。
	identity.pinnedRenewal = &pinnedRenewalSource{
		paths:    identityPaths(filepath.Join(t.TempDir(), "missing-data-dir")),
		hostname: "gateway.example.test",
	}
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
		renewalCheckInterval:    time.Millisecond,
		now:                     func() time.Time { return createdAt.Add(367 * 24 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	waitForGatewayCondition(t, func() bool { return server.LastRenewalError() != nil })
	certificate, err := server.getCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("TLS GetCertificate() error = %v", err)
	}
	if !bytes.Equal(certificate.Certificate[0], identity.Leaf().Raw) {
		t.Fatal("renewal failure replaced the old valid TLS identity")
	}
}

func waitForGatewayCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for Gateway background renewal")
}

func testIdentity(t *testing.T) Identity {
	t.Helper()
	identity, err := LoadOrCreatePinnedIdentity(t.TempDir(), "gateway.example.test", true, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	return identity
}

func dialTLS(address, alpn string) (*tls.Conn, error) {
	config := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // #nosec G402 -- 本测试只验证服务端 ALPN 边界。
	}
	if alpn != "" {
		config.NextProtos = []string{alpn}
	}
	connection, err := tls.Dial("tcp", address, config)
	return connection, err
}
