package gateway

import (
	"bytes"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/logging"
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
	var logs bytes.Buffer
	logger, err := logging.New(&logs, logging.Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 2,
		Logger:                  logger,
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
	var renewalLog map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &renewalLog); err != nil {
		t.Fatalf("decode certificate renewal log: %v; output = %q", err, logs.String())
	}
	if renewalLog[logging.EventKey] != certificateRenewedEvent || renewalLog[logging.ComponentKey] != "server" ||
		int64(renewalLog["previous_certificate_expiry_seconds"].(float64)) != identity.Leaf().NotAfter.Unix() ||
		int64(renewalLog["certificate_expiry_seconds"].(float64)) != wantRenewedExpiry {
		t.Fatalf("certificate renewal log = %#v", renewalLog)
	}
}

func TestServerPinnedRenewalPublishesCompleteIdentityToConcurrentHandshakes(t *testing.T) {
	const (
		readerCount = 32
		readRounds  = 4
	)
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	releaseHandlers := make(chan struct{})
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: readerCount * readRounds,
		renewalCheckInterval:    time.Hour,
		now:                     func() time.Time { return createdAt.Add(367 * 24 * time.Hour) },
		Handle: func(context.Context, *tls.Conn, Protocol) {
			<-releaseHandlers
		},
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() {
		close(releaseHandlers)
		_ = server.Close()
	})

	oldConnection, err := dialTLS(server.Addr().String(), ControlALPN)
	if err != nil {
		t.Fatalf("dialTLS(before concurrent renewal) error = %v", err)
	}
	t.Cleanup(func() { _ = oldConnection.Close() })
	oldCertificate := bytes.Clone(oldConnection.ConnectionState().PeerCertificates[0].Raw)
	if !bytes.Equal(oldCertificate, identity.Leaf().Raw) {
		t.Fatal("old TLS handshake did not receive the original certificate")
	}

	type observation struct {
		kind        string
		certificate []byte
		expiry      int64
	}
	observations := make(chan observation, readerCount*(readRounds*3+4)+4)
	observations <- observation{kind: "TLS handshake", certificate: bytes.Clone(oldCertificate)}
	errorsByReader := make(chan error, readerCount)
	readersHoldingIdentity := make(chan struct{}, readerCount)
	releaseIdentityReaders := make(chan struct{})
	var releaseIdentityReadersOnce sync.Once
	renewalDone := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(readerCount)
	for reader := 0; reader < readerCount; reader++ {
		go func(reader int) {
			defer readers.Done()
			// 每个 reader 在续签启动前先通过三条正式读取路径观察旧代，再持有
			// 身份锁跨越发布边界；释放后继续通过同样入口读取，避免手工锁读取
			// 代偿 GetCertificate、Metric 或真实 TLS 握手的覆盖。
			certificateBeforePublish, getErr := server.getCertificate(&tls.ClientHelloInfo{})
			if getErr != nil {
				errorsByReader <- fmt.Errorf("reader %d GetCertificate before publication: %w", reader, getErr)
				return
			}
			if validationErr := validateCompleteTLSIdentity(certificateBeforePublish); validationErr != nil {
				errorsByReader <- fmt.Errorf("reader %d GetCertificate before publication: %w", reader, validationErr)
				return
			}
			observations <- observation{kind: "GetCertificate", certificate: bytes.Clone(certificateBeforePublish.Certificate[0])}
			observations <- observation{kind: "Metric", expiry: server.MetricsSnapshot().CertificateExpiryUnixSeconds}
			connectionBeforePublish, dialErr := dialTLSWithTimeout(server.Addr().String(), ControlALPN, 5*time.Second)
			if dialErr != nil {
				errorsByReader <- fmt.Errorf("reader %d TLS handshake before publication: %w", reader, dialErr)
				return
			}
			observations <- observation{
				kind:        "TLS handshake",
				certificate: bytes.Clone(connectionBeforePublish.ConnectionState().PeerCertificates[0].Raw),
			}
			_ = connectionBeforePublish.Close()

			server.identityMu.RLock()
			observedBeforePublish := server.identity
			readersHoldingIdentity <- struct{}{}
			<-releaseIdentityReaders
			server.identityMu.RUnlock()
			beforePublish := &tls.Certificate{
				Certificate: observedBeforePublish.CertificateChain(),
				PrivateKey:  observedBeforePublish.PrivateKey(),
				Leaf:        observedBeforePublish.Leaf(),
			}
			if validationErr := validateCompleteTLSIdentity(beforePublish); validationErr != nil {
				errorsByReader <- fmt.Errorf("reader %d identity held across publication: %w", reader, validationErr)
				return
			}
			observations <- observation{kind: "identity reader", certificate: bytes.Clone(beforePublish.Certificate[0])}
			for round := 0; round < readRounds; round++ {
				certificate, getErr := server.getCertificate(&tls.ClientHelloInfo{})
				if getErr != nil {
					errorsByReader <- fmt.Errorf("reader %d round %d GetCertificate: %w", reader, round, getErr)
					return
				}
				if validationErr := validateCompleteTLSIdentity(certificate); validationErr != nil {
					errorsByReader <- fmt.Errorf("reader %d round %d GetCertificate: %w", reader, round, validationErr)
					return
				}
				observations <- observation{kind: "GetCertificate", certificate: bytes.Clone(certificate.Certificate[0])}
				observations <- observation{kind: "Metric", expiry: server.MetricsSnapshot().CertificateExpiryUnixSeconds}

				connection, dialErr := dialTLSWithTimeout(server.Addr().String(), ControlALPN, 5*time.Second)
				if dialErr != nil {
					errorsByReader <- fmt.Errorf("reader %d round %d TLS handshake: %w", reader, round, dialErr)
					return
				}
				peerCertificate := bytes.Clone(connection.ConnectionState().PeerCertificates[0].Raw)
				_ = connection.Close()
				observations <- observation{kind: "TLS handshake", certificate: peerCertificate}
			}
			errorsByReader <- nil
		}(reader)
	}
	readersFinished := make(chan struct{})
	go func() {
		readers.Wait()
		close(readersFinished)
	}()
	defer func() {
		releaseIdentityReadersOnce.Do(func() { close(releaseIdentityReaders) })
		select {
		case <-readersFinished:
		case <-time.After(20 * time.Second):
		}
	}()
	readyTimeout := time.NewTimer(5 * time.Second)
	defer readyTimeout.Stop()
	for reader := 0; reader < readerCount; reader++ {
		select {
		case <-readersHoldingIdentity:
		case <-readyTimeout.C:
			t.Fatal("concurrent pinned TLS identity readers did not acquire the publication barrier")
		}
	}

	// 所有 reader 均持有读锁后才启动正式续签。TryRLock 失败证明续签已离开无锁的
	// 磁盘阶段并排队写入成功身份或失败状态；完成后的断言再确认落盘成功。
	// 这样无需循环打开证书文件，避免测试与 Windows 原子替换竞争。
	renewalContext, cancelRenewal := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRenewal()
	go func() {
		defer close(renewalDone)
		server.renewPinnedIdentity(renewalContext)
	}()
	defer func() {
		releaseIdentityReadersOnce.Do(func() { close(releaseIdentityReaders) })
		cancelRenewal()
		select {
		case <-renewalDone:
		case <-time.After(10 * time.Second):
		}
	}()
	publicationDeadline := time.Now().Add(10 * time.Second)
	for {
		if !server.identityMu.TryRLock() {
			break
		}
		server.identityMu.RUnlock()
		if time.Now().After(publicationDeadline) {
			t.Fatal("timed out waiting for pinned renewal to reach the identity publication barrier")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-renewalDone:
		t.Fatal("pinned renewal published while certificate readers still held identityMu")
	default:
	}
	releaseIdentityReadersOnce.Do(func() { close(releaseIdentityReaders) })

	select {
	case <-readersFinished:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent pinned TLS identity readers did not finish")
	}
	select {
	case <-renewalDone:
	case <-time.After(10 * time.Second):
		t.Fatal("pinned identity renewal did not finish")
	}
	for reader := 0; reader < readerCount; reader++ {
		if readerErr := <-errorsByReader; readerErr != nil {
			t.Fatal(readerErr)
		}
	}
	if err := server.LastRenewalError(); err != nil {
		t.Fatalf("Server.LastRenewalError() = %v", err)
	}
	renewedOnDisk, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after concurrent renewal error = %v", err)
	}
	if renewedOnDisk.Leaf() == nil || bytes.Equal(renewedOnDisk.Leaf().Raw, identity.Leaf().Raw) {
		t.Fatal("pinned identity did not persist a distinct renewed certificate")
	}
	if renewedOnDisk.SPKIHash() != identity.SPKIHash() {
		t.Fatal("pinned renewal changed the private key/SPKI")
	}
	certificateAfterPublish, err := server.getCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate(after concurrent renewal) error = %v", err)
	}
	if validationErr := validateCompleteTLSIdentity(certificateAfterPublish); validationErr != nil {
		t.Fatalf("GetCertificate(after concurrent renewal): %v", validationErr)
	}
	observations <- observation{
		kind:        "GetCertificate",
		certificate: bytes.Clone(certificateAfterPublish.Certificate[0]),
	}
	observations <- observation{kind: "Metric", expiry: server.MetricsSnapshot().CertificateExpiryUnixSeconds}
	newConnection, err := dialTLSWithTimeout(server.Addr().String(), ControlALPN, 5*time.Second)
	if err != nil {
		t.Fatalf("dialTLS(after concurrent renewal) error = %v", err)
	}
	observations <- observation{
		kind:        "TLS handshake",
		certificate: bytes.Clone(newConnection.ConnectionState().PeerCertificates[0].Raw),
	}
	_ = newConnection.Close()

	close(observations)
	oldObservations := make(map[string]int)
	newObservations := make(map[string]int)
	for observed := range observations {
		switch observed.kind {
		case "Metric":
			switch observed.expiry {
			case identity.Leaf().NotAfter.Unix():
				oldObservations[observed.kind]++
			case renewedOnDisk.Leaf().NotAfter.Unix():
				newObservations[observed.kind]++
			default:
				t.Fatalf("concurrent Metric expiry = %d, want complete old or new identity expiry", observed.expiry)
			}
		case "identity reader":
			if !bytes.Equal(observed.certificate, identity.Leaf().Raw) {
				t.Fatal("identity reader held across publication did not retain the complete old identity")
			}
		case "GetCertificate", "TLS handshake":
			switch {
			case bytes.Equal(observed.certificate, identity.Leaf().Raw):
				oldObservations[observed.kind]++
			case bytes.Equal(observed.certificate, renewedOnDisk.Leaf().Raw):
				newObservations[observed.kind]++
			default:
				t.Fatalf("concurrent %s observed a certificate outside the complete old/new identity set", observed.kind)
			}
		default:
			t.Fatalf("unexpected observation kind %q", observed.kind)
		}
	}
	for _, kind := range []string{"GetCertificate", "Metric", "TLS handshake"} {
		if oldObservations[kind] == 0 || newObservations[kind] == 0 {
			t.Fatalf("%s observations old/new = %d/%d, want both generations", kind, oldObservations[kind], newObservations[kind])
		}
	}
	if !bytes.Equal(oldConnection.ConnectionState().PeerCertificates[0].Raw, oldCertificate) {
		t.Fatal("pinned renewal changed the certificate already negotiated by an old connection")
	}
}

func TestServerPublicIdentitySupportsConcurrentHandshakeAndStatusReads(t *testing.T) {
	const (
		readerCount = 24
		readRounds  = 4
	)
	issuedAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity := testPublicIdentity(t, issuedAt)
	// Public 身份由外部证书系统负责，Gateway 当前没有运行时发布入口；本用例只验证
	// 启动时载入的不可变身份可被握手、GetCertificate 与状态采集安全并发读取。
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: readerCount * readRounds,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	readersStart := make(chan struct{})
	errorsByReader := make(chan error, readerCount)
	var readers sync.WaitGroup
	readers.Add(readerCount)
	for reader := 0; reader < readerCount; reader++ {
		go func(reader int) {
			defer readers.Done()
			<-readersStart
			for round := 0; round < readRounds; round++ {
				certificate, getErr := server.getCertificate(&tls.ClientHelloInfo{})
				if getErr != nil {
					errorsByReader <- fmt.Errorf("reader %d round %d GetCertificate: %w", reader, round, getErr)
					return
				}
				if validationErr := validateCompleteTLSIdentity(certificate); validationErr != nil {
					errorsByReader <- fmt.Errorf("reader %d round %d GetCertificate: %w", reader, round, validationErr)
					return
				}
				if !bytes.Equal(certificate.Certificate[0], identity.Leaf().Raw) {
					errorsByReader <- fmt.Errorf("reader %d round %d observed a different public certificate", reader, round)
					return
				}
				if expiry := server.MetricsSnapshot().CertificateExpiryUnixSeconds; expiry != identity.Leaf().NotAfter.Unix() {
					errorsByReader <- fmt.Errorf("reader %d round %d public Metric expiry = %d, want %d", reader, round, expiry, identity.Leaf().NotAfter.Unix())
					return
				}
				connection, dialErr := dialTLSWithTimeout(server.Addr().String(), ControlALPN, 5*time.Second)
				if dialErr != nil {
					errorsByReader <- fmt.Errorf("reader %d round %d TLS handshake: %w", reader, round, dialErr)
					return
				}
				peerCertificate := bytes.Clone(connection.ConnectionState().PeerCertificates[0].Raw)
				_ = connection.Close()
				if !bytes.Equal(peerCertificate, identity.Leaf().Raw) {
					errorsByReader <- fmt.Errorf("reader %d round %d TLS handshake observed a different public certificate", reader, round)
					return
				}
			}
			errorsByReader <- nil
		}(reader)
	}
	close(readersStart)
	readersFinished := make(chan struct{})
	go func() {
		readers.Wait()
		close(readersFinished)
	}()
	select {
	case <-readersFinished:
	case <-time.After(20 * time.Second):
		t.Fatal("concurrent public TLS identity readers did not finish")
	}
	for reader := 0; reader < readerCount; reader++ {
		if readerErr := <-errorsByReader; readerErr != nil {
			t.Fatal(readerErr)
		}
	}
}

func TestServerMetricsSnapshotReadsPublicIdentityExpiry(t *testing.T) {
	issuedAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	identity := testPublicIdentity(t, issuedAt)
	server, err := NewServer(ServerOptions{
		Listen: "127.0.0.1:0", Identity: identity, MaxPendingTLSHandshakes: 1,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	if got := server.MetricsSnapshot().CertificateExpiryUnixSeconds; got != identity.Leaf().NotAfter.Unix() {
		t.Fatalf("public certificate expiry = %d, want %d", got, identity.Leaf().NotAfter.Unix())
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
	var logs bytes.Buffer
	logger, err := logging.New(&logs, logging.Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 1,
		Logger:                  logger,
		now:                     func() time.Time { return createdAt.Add(367 * 24 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	server.renewPinnedIdentity(context.Background())
	if server.LastRenewalError() == nil {
		t.Fatal("Server.LastRenewalError() = nil, want renewal failure")
	}
	certificate, err := server.getCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("TLS GetCertificate() error = %v", err)
	}
	if !bytes.Equal(certificate.Certificate[0], identity.Leaf().Raw) {
		t.Fatal("renewal failure replaced the old valid TLS identity")
	}
	var renewalLog map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &renewalLog); err != nil {
		t.Fatalf("decode certificate renewal failure log: %v; output = %q", err, logs.String())
	}
	if renewalLog[logging.EventKey] != certificateRenewalFailedEvent || renewalLog[logging.ComponentKey] != "server" ||
		renewalLog[logging.ErrorCodeKey] != "CERTIFICATE_RENEWAL_FAILED" ||
		int64(renewalLog["certificate_expiry_seconds"].(float64)) != identity.Leaf().NotAfter.Unix() {
		t.Fatalf("certificate renewal failure log = %#v", renewalLog)
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

func testPublicIdentity(t *testing.T, issuedAt time.Time) Identity {
	t.Helper()
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
	return identity
}

func validateCompleteTLSIdentity(certificate *tls.Certificate) error {
	if certificate == nil || certificate.Leaf == nil || len(certificate.Certificate) == 0 || certificate.PrivateKey == nil {
		return errors.New("TLS identity is incomplete")
	}
	if !bytes.Equal(certificate.Certificate[0], certificate.Leaf.Raw) {
		return errors.New("TLS certificate chain and parsed leaf belong to different identities")
	}
	signer, ok := certificate.PrivateKey.(crypto.Signer)
	if !ok {
		return fmt.Errorf("TLS private key type %T does not implement crypto.Signer", certificate.PrivateKey)
	}
	leafSPKI, err := x509.MarshalPKIXPublicKey(certificate.Leaf.PublicKey)
	if err != nil {
		return fmt.Errorf("marshal TLS leaf public key: %w", err)
	}
	privateSPKI, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return fmt.Errorf("marshal TLS private key public component: %w", err)
	}
	if !bytes.Equal(leafSPKI, privateSPKI) {
		return errors.New("TLS certificate and private key belong to different identities")
	}
	return nil
}

func dialTLSWithTimeout(address, alpn string, timeout time.Duration) (*tls.Conn, error) {
	config := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // #nosec G402 -- 本测试只验证服务端证书发布与 ALPN 边界。
	}
	if alpn != "" {
		config.NextProtos = []string{alpn}
	}
	return tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", address, config)
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
