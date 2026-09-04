package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

func TestDiagnosePinnedHostnameAndIPLiteral(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		wantDNSMsg string
	}{
		{name: "hostname", host: "localhost", wantDNSMsg: "gateway hostname resolution completed"},
		{name: "IP literal", host: "127.0.0.1", wantDNSMsg: "IP literal does not require DNS lookup"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tokenText := startPinnedDiagnosticGateway(t, test.host)
			var closeCalls atomic.Int32
			var deadlinesMu sync.Mutex
			var deadlines []time.Time
			dialer := &net.Dialer{}
			result := diagnose(context.Background(), tokenText, dialDependencies{
				dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatal("诊断拨号 Context 缺少总 Deadline")
					}
					deadlinesMu.Lock()
					deadlines = append(deadlines, deadline)
					deadlinesMu.Unlock()
					connection, err := dialer.DialContext(ctx, network, address)
					if err != nil {
						return nil, err
					}
					return &countingConn{Conn: connection, closeCalls: &closeCalls}, nil
				},
			}, time.Now)

			if result.Summary != DiagnosticReady {
				t.Fatalf("Diagnose() summary = %q, want %q; steps = %#v", result.Summary, DiagnosticReady, result.Steps)
			}
			wantStages := []string{
				"TOKEN", "ENDPOINT",
				"CONTROL_DNS", "CONTROL_TCP", "CONTROL_TLS", "CONTROL_TRUST", "CONTROL_ALPN",
				"WORK_DNS", "WORK_TCP", "WORK_TLS", "WORK_TRUST", "WORK_ALPN",
			}
			if len(result.Steps) != len(wantStages) {
				t.Fatalf("Diagnose() steps = %#v, want %d stages", result.Steps, len(wantStages))
			}
			for index, wantStage := range wantStages {
				if got := result.Steps[index]; got.Stage != wantStage || got.Status != DiagnosticPass {
					t.Fatalf("step %d = %#v, want PASS %s", index, got, wantStage)
				}
			}
			if result.Steps[2].Message != test.wantDNSMsg || result.Steps[7].Message != test.wantDNSMsg {
				t.Fatalf("DNS messages = %q/%q, want %q", result.Steps[2].Message, result.Steps[7].Message, test.wantDNSMsg)
			}
			if closeCalls.Load() != 2 {
				t.Fatalf("诊断关闭连接次数 = %d, want 2", closeCalls.Load())
			}
			deadlinesMu.Lock()
			defer deadlinesMu.Unlock()
			if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
				t.Fatalf("Control/Work 未共用同一个总 Deadline: %v", deadlines)
			}
		})
	}
}

func TestDiagnosePublicCATrustAndExpiryWarning(t *testing.T) {
	tests := []struct {
		name        string
		validFor    time.Duration
		wantSummary DiagnosticSummary
		wantStatus  DiagnosticStatus
	}{
		{name: "normal certificate", validFor: 90 * 24 * time.Hour, wantSummary: DiagnosticReady, wantStatus: DiagnosticPass},
		{name: "expiring certificate", validFor: 7 * 24 * time.Hour, wantSummary: DiagnosticReadyDegraded, wantStatus: DiagnosticWarning},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			address, roots, _ := startPublicCAGateway(
				t,
				now,
				test.validFor,
				fixedALPN(servergateway.ControlALPN, servergateway.WorkALPN),
			)
			host, portText, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatalf("net.SplitHostPort() error = %v", err)
			}
			port, err := strconv.ParseUint(portText, 10, 16)
			if err != nil {
				t.Fatalf("strconv.ParseUint() error = %v", err)
			}
			tokenText := encodePublicCAToken(t, host, uint32(port))
			result := diagnose(context.Background(), tokenText, dialDependencies{
				configureTLS: func(config *tls.Config) { config.RootCAs = roots },
			}, func() time.Time { return now })

			if result.Summary != test.wantSummary {
				t.Fatalf("Diagnose() summary = %q, want %q; steps = %#v", result.Summary, test.wantSummary, result.Steps)
			}
			for _, stage := range []string{"CONTROL_TRUST", "WORK_TRUST"} {
				step := findStep(t, result, stage)
				if step.Status != test.wantStatus || !bytes.Contains([]byte(step.Message), []byte("public CA")) {
					t.Fatalf("%s = %#v, want %s Public CA result", stage, step, test.wantStatus)
				}
			}
		})
	}
}

func TestDiagnoseReportsUniqueFailureStage(t *testing.T) {
	hostnameToken := encodePinnedToken(t, "gateway.example.invalid", 443, bytes.Repeat([]byte{0x11}, 32))
	tests := []struct {
		name      string
		setup     func(*testing.T) (string, dialDependencies)
		wantStage string
	}{
		{
			name: "DNS",
			setup: func(*testing.T) (string, dialDependencies) {
				return hostnameToken, dialDependencies{dialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, &net.DNSError{Err: "injected DNS failure", Name: "sensitive.example"}
				}}
			},
			wantStage: "CONTROL_DNS",
		},
		{
			name: "TCP",
			setup: func(*testing.T) (string, dialDependencies) {
				return hostnameToken, dialDependencies{dialContext: func(context.Context, string, string) (net.Conn, error) {
					return nil, errors.New("injected TCP failure")
				}}
			},
			wantStage: "CONTROL_TCP",
		},
		{
			name: "TLS",
			setup: func(t *testing.T) (string, dialDependencies) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("net.Listen() error = %v", err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				go func() {
					connection, acceptErr := listener.Accept()
					if acceptErr == nil {
						_, _ = connection.Write([]byte("not TLS"))
						_ = connection.Close()
					}
				}()
				host, port := splitAddress(t, listener.Addr().String())
				return encodePinnedToken(t, host, port, bytes.Repeat([]byte{0x22}, 32)), dialDependencies{}
			},
			wantStage: "CONTROL_TLS",
		},
		{
			name: "Pin",
			setup: func(t *testing.T) (string, dialDependencies) {
				tokenText := startPinnedDiagnosticGateway(t, "127.0.0.1")
				parsed, err := connectiontoken.Parse(tokenText)
				if err != nil {
					t.Fatalf("connectiontoken.Parse() error = %v", err)
				}
				return encodePinnedToken(t, parsed.GetEndpoint().GetHost(), parsed.GetEndpoint().GetPort(), bytes.Repeat([]byte{0x33}, 32)), dialDependencies{}
			},
			wantStage: "CONTROL_TRUST",
		},
		{
			name: "Public CA trust",
			setup: func(t *testing.T) (string, dialDependencies) {
				now := time.Now().UTC()
				address, _, _ := startPublicCAGateway(
					t,
					now,
					90*24*time.Hour,
					fixedALPN(servergateway.ControlALPN, servergateway.WorkALPN),
				)
				host, port := splitAddress(t, address)
				return encodePublicCAToken(t, host, port), dialDependencies{
					configureTLS: func(config *tls.Config) { config.RootCAs = x509.NewCertPool() },
				}
			},
			wantStage: "CONTROL_TRUST",
		},
		{
			name: "Work ALPN",
			setup: func(t *testing.T) (string, dialDependencies) {
				now := time.Now().UTC()
				address, roots, _ := startPublicCAGateway(t, now, 90*24*time.Hour, func(offered []string) []string {
					if len(offered) == 1 && offered[0] == servergateway.ControlALPN {
						return []string{servergateway.ControlALPN}
					}
					return []string{"http/1.1"}
				})
				host, port := splitAddress(t, address)
				return encodePublicCAToken(t, host, port), dialDependencies{
					configureTLS: func(config *tls.Config) { config.RootCAs = roots },
				}
			},
			wantStage: "WORK_ALPN",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tokenText, dependencies := test.setup(t)
			result := diagnose(context.Background(), tokenText, dependencies, time.Now)
			if result.Summary != DiagnosticNotReady {
				t.Fatalf("Diagnose() summary = %q, want NOT_READY", result.Summary)
			}
			last := result.Steps[len(result.Steps)-1]
			if last.Stage != test.wantStage || last.Status != DiagnosticFail {
				t.Fatalf("last step = %#v, want FAIL %s; all = %#v", last, test.wantStage, result.Steps)
			}
			serialized := formatDiagnosticForTest(result)
			for _, sentinel := range []string{"gateway.example.invalid", "sensitive.example", "injected DNS failure", "injected TCP failure"} {
				if bytes.Contains(serialized, []byte(sentinel)) {
					t.Fatalf("diagnostic output leaked sensitive/error sentinel %q: %s", sentinel, serialized)
				}
			}
		})
	}
}

func TestDiagnoseCancellationTimeoutAndConnectionClose(t *testing.T) {
	tokenText := encodePinnedToken(t, "gateway.example.test", 443, bytes.Repeat([]byte{0x44}, 32))

	t.Run("caller timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		result := diagnose(ctx, tokenText, dialDependencies{dialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}, time.Now)
		if result.Summary != DiagnosticNotReady || findStep(t, result, "CONTROL_TCP").Status != DiagnosticFail {
			t.Fatalf("timeout result = %#v", result)
		}
	})

	t.Run("cancellation closes acquired connection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		closed := make(chan struct{})
		resultChannel := make(chan DiagnosticResult, 1)
		go func() {
			resultChannel <- diagnose(ctx, tokenText, dialDependencies{dialContext: func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				close(started)
				go func() {
					_, _ = io.Copy(io.Discard, server)
					_ = server.Close()
					close(closed)
				}()
				return client, nil
			}}, time.Now)
		}()
		<-started
		cancel()
		select {
		case result := <-resultChannel:
			if result.Summary != DiagnosticNotReady || findStep(t, result, "CONTROL_TLS").Status != DiagnosticFail {
				t.Fatalf("canceled result = %#v", result)
			}
		case <-time.After(time.Second):
			t.Fatal("诊断未响应取消")
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("取消后已取得的 TCP 连接未关闭")
		}
	})
}

func TestDiagnoseRejectsMalformedTokenWithoutDialOrLeak(t *testing.T) {
	const secret = "xta_invalid_sensitive_sentinel"
	var dialCalls atomic.Int32
	result := diagnose(context.Background(), secret, dialDependencies{dialContext: func(context.Context, string, string) (net.Conn, error) {
		dialCalls.Add(1)
		return nil, errors.New("must not dial")
	}}, time.Now)
	if result.Summary != DiagnosticNotReady || len(result.Steps) != 1 || result.Steps[0].Stage != "TOKEN" || result.Steps[0].Status != DiagnosticFail {
		t.Fatalf("malformed Token result = %#v", result)
	}
	if dialCalls.Load() != 0 {
		t.Fatalf("malformed Token dial calls = %d, want 0", dialCalls.Load())
	}
	if bytes.Contains(formatDiagnosticForTest(result), []byte(secret)) {
		t.Fatal("malformed Token diagnostic leaked Token")
	}
}

func TestDiagnoseDoesNotSendAuthOrSnapshotMessages(t *testing.T) {
	now := time.Now().UTC()
	address, roots, applicationData := startPublicCAGateway(
		t,
		now,
		90*24*time.Hour,
		fixedALPN(servergateway.ControlALPN, servergateway.WorkALPN),
	)
	host, port := splitAddress(t, address)
	result := diagnose(context.Background(), encodePublicCAToken(t, host, port), dialDependencies{
		configureTLS: func(config *tls.Config) { config.RootCAs = roots },
	}, func() time.Time { return now })
	if result.Summary != DiagnosticReady {
		t.Fatalf("Diagnose() summary = %q, want READY; steps = %#v", result.Summary, result.Steps)
	}
	for connection := 0; connection < 2; connection++ {
		select {
		case data := <-applicationData:
			if len(data) != 0 {
				t.Fatalf("diagnostic connection %d sent %d application bytes", connection, len(data))
			}
		case <-time.After(time.Second):
			t.Fatalf("diagnostic connection %d did not close", connection)
		}
	}
}

type countingConn struct {
	net.Conn
	closeCalls *atomic.Int32
}

func (connection *countingConn) Close() error {
	connection.closeCalls.Add(1)
	return connection.Conn.Close()
}

func startPinnedDiagnosticGateway(t *testing.T, tokenHost string) string {
	t.Helper()
	identity, err := servergateway.LoadOrCreatePinnedIdentity(newGatewayTestDataDir(t), tokenHost, true, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	server, err := servergateway.NewServer(servergateway.ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                identity,
		MaxPendingTLSHandshakes: 4,
		Handle: func(ctx context.Context, _ *tls.Conn, _ servergateway.Protocol) {
			<-ctx.Done()
		},
	})
	if err != nil {
		t.Fatalf("gateway.NewServer() error = %v", err)
	}
	if err := server.Start(context.Background()); err != nil {
		t.Fatalf("gateway.Server.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	_, port := splitAddress(t, server.Addr().String())
	spkiHash := identity.SPKIHash()
	return encodePinnedToken(t, tokenHost, port, spkiHash[:])
}

func startPublicCAGateway(
	t *testing.T,
	now time.Time,
	validFor time.Duration,
	selectALPN func([]string) []string,
) (string, *x509.CertPool, <-chan []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey(CA) error = %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "XTunnel Diagnostic Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate(CA) error = %v", err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("x509.ParseCertificate(CA) error = %v", err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey(server) error = %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validFor),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCertificate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate(server) error = %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	tlsCertificate := tls.Certificate{Certificate: [][]byte{serverDER, caDER}, PrivateKey: serverKey}
	applicationData := make(chan []byte, 4)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(raw net.Conn) {
				defer raw.Close()
				serverConfig := &tls.Config{
					MinVersion:   tls.VersionTLS13,
					Certificates: []tls.Certificate{tlsCertificate},
				}
				serverConfig.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					selected := serverConfig.Clone()
					selected.GetConfigForClient = nil
					selected.NextProtos = selectALPN(hello.SupportedProtos)
					return selected, nil
				}
				tlsConnection := tls.Server(raw, serverConfig)
				if handshakeErr := tlsConnection.Handshake(); handshakeErr != nil {
					return
				}
				data, _ := io.ReadAll(tlsConnection)
				applicationData <- data
			}(connection)
		}
	}()
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	return listener.Addr().String(), roots, applicationData
}

func fixedALPN(protocols ...string) func([]string) []string {
	return func([]string) []string { return protocols }
}

func encodePublicCAToken(t *testing.T, host string, port uint32) string {
	t.Helper()
	tokenText, err := connectiontoken.Encode(&protocolv1.ConnectionToken{
		FormatVersion: connectiontoken.FormatVersionV1,
		Endpoint:      &protocolv1.GatewayEndpoint{Host: host, Port: port},
		TlsTrust: &protocolv1.TlsTrustDescriptor{
			Mode: &protocolv1.TlsTrustDescriptor_PublicCa{PublicCa: &protocolv1.PublicCATrust{}},
		},
		TunnelId:             "tun_01J00000000000000000000000",
		TokenId:              "tok_01J00000000000000000000000",
		TokenVersion:         1,
		AuthenticationSecret: bytes.Repeat([]byte{0x55}, sha256.Size),
	})
	if err != nil {
		t.Fatalf("connectiontoken.Encode() error = %v", err)
	}
	return tokenText
}

func splitAddress(t *testing.T, address string) (string, uint32) {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("net.SplitHostPort() error = %v", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("strconv.ParseUint() error = %v", err)
	}
	return host, uint32(port)
}

func findStep(t *testing.T, result DiagnosticResult, stage string) DiagnosticStep {
	t.Helper()
	for _, step := range result.Steps {
		if step.Stage == stage {
			return step
		}
	}
	t.Fatalf("diagnostic stage %s missing from %#v", stage, result.Steps)
	return DiagnosticStep{}
}

func formatDiagnosticForTest(result DiagnosticResult) []byte {
	var output bytes.Buffer
	for _, step := range result.Steps {
		output.WriteString(string(step.Status))
		output.WriteByte(' ')
		output.WriteString(step.Stage)
		output.WriteByte(' ')
		output.WriteString(step.Message)
		output.WriteByte('\n')
	}
	output.WriteString(string(result.Summary))
	return output.Bytes()
}
