package origin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

const (
	testTunnelID  = "tun_01J00000000000000000000000"
	testServiceID = "svc_01J00000000000000000000000"
)

func TestResolverPublicationGateAndServiceErrors(t *testing.T) {
	manager := New()
	gate := &testGate{}
	candidate := buildCandidate(t, manager, snapshot(service("tcp", "127.0.0.1", 8080)), gate)
	if _, err := manager.Resolve(testServiceID); !errors.Is(err, ErrConfigNotObserved) {
		t.Fatalf("Resolve() before Start error = %v", err)
	}
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatalf("Candidate.Start() error = %v", err)
	}
	if _, err := manager.Resolve(testServiceID); !errors.Is(err, ErrConfigNotObserved) {
		t.Fatalf("Resolve() before publication error = %v", err)
	}
	gate.active.Store(true)
	resolved, err := manager.Resolve(testServiceID)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Scheme != "tcp" || resolved.Host != "127.0.0.1" || resolved.Port != 8080 ||
		resolved.ConnectTimeout != 50*time.Millisecond {
		t.Fatalf("Resolve() = %#v", resolved)
	}
	if _, err := manager.Resolve("svc_01J00000000000000000000001"); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("Resolve() unknown error = %v", err)
	}

	gate.active.Store(false)
	if err := candidate.Runtime().Retire(context.Background()); err != nil {
		t.Fatalf("Resources.Retire() error = %v", err)
	}
	if _, err := manager.Resolve(testServiceID); !errors.Is(err, ErrConfigNotObserved) {
		t.Fatalf("Resolve() after Retire error = %v", err)
	}
}

func TestResolverRejectsInvalidOriginSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*protocolv1.ServiceConfig)
	}{
		{name: "unsupported scheme", mutate: func(value *protocolv1.ServiceConfig) { value.OriginScheme = "udp" }},
		{name: "uppercase scheme", mutate: func(value *protocolv1.ServiceConfig) { value.OriginScheme = "TCP" }},
		{name: "zero port", mutate: func(value *protocolv1.ServiceConfig) { value.OriginPort = 0 }},
		{name: "large port", mutate: func(value *protocolv1.ServiceConfig) { value.OriginPort = 65_536 }},
		{name: "zero timeout", mutate: func(value *protocolv1.ServiceConfig) { value.ConnectTimeoutMs = 0 }},
		{name: "URI host", mutate: func(value *protocolv1.ServiceConfig) { value.OriginHost = "http://origin.test" }},
		{name: "host with port", mutate: func(value *protocolv1.ServiceConfig) { value.OriginHost = "origin.test:80" }},
		{name: "uppercase DNS", mutate: func(value *protocolv1.ServiceConfig) { value.OriginHost = "Origin.Test" }},
		{name: "absolute DNS", mutate: func(value *protocolv1.ServiceConfig) { value.OriginHost = "origin.test." }},
		{name: "invalid dotted decimal", mutate: func(value *protocolv1.ServiceConfig) { value.OriginHost = "127.0.0.999" }},
		{name: "IPv6 zone", mutate: func(value *protocolv1.ServiceConfig) { value.OriginHost = "fe80::1%eth0" }},
		{name: "TCP TLS name", mutate: func(value *protocolv1.ServiceConfig) { value.TlsServerName = "origin.test" }},
		{name: "TCP HTTP host", mutate: func(value *protocolv1.ServiceConfig) { value.OriginHttpHost = "origin.test" }},
		{name: "HTTP TLS name", mutate: func(value *protocolv1.ServiceConfig) {
			value.OriginScheme = "http"
			value.TlsServerName = "origin.test"
		}},
		{name: "header injection", mutate: func(value *protocolv1.ServiceConfig) {
			value.OriginScheme = "https"
			value.OriginHttpHost = "origin.test\r\nx-test: bad"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := New()
			configured := service("tcp", "origin.test", 8080)
			test.mutate(configured)
			candidate, err := manager.Build(context.Background(), snapshot(configured), &testGate{})
			if candidate != nil || !errors.Is(err, configruntime.ErrProtocolViolation) || !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("Build() = (%T, %v), want protocol-invalid snapshot", candidate, err)
			}
		})
	}
}

func TestResolverAcceptsSupportedHostAndTLSModels(t *testing.T) {
	tests := []struct {
		name       string
		configured *protocolv1.ServiceConfig
		wantHost   string
		wantSNI    string
	}{
		{name: "HTTP DNS", configured: service("http", "origin.test", 80), wantHost: "origin.test"},
		{name: "TCP IPv4 loopback", configured: service("tcp", "127.0.0.1", 80), wantHost: "127.0.0.1"},
		{name: "TCP IPv6 private", configured: service("tcp", "fd00::1", 80), wantHost: "fd00::1"},
		{name: "HTTPS DNS fallback SNI", configured: service("https", "origin.test", 443), wantHost: "origin.test", wantSNI: "origin.test"},
		{name: "HTTPS explicit SNI", configured: func() *protocolv1.ServiceConfig {
			value := service("https", "127.0.0.1", 443)
			value.TlsServerName = "origin.test"
			return value
		}(), wantHost: "127.0.0.1", wantSNI: "origin.test"},
		{name: "HTTPS IP SAN name", configured: service("https", "127.0.0.1", 443), wantHost: "127.0.0.1", wantSNI: "127.0.0.1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := New()
			gate := &testGate{}
			gate.active.Store(true)
			candidate := buildCandidate(t, manager, snapshot(test.configured), gate)
			if err := candidate.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			resolved, err := manager.Resolve(testServiceID)
			if err != nil || resolved.Host != test.wantHost || resolved.TLSServerName != test.wantSNI {
				t.Fatalf("Resolve() = (%#v, %v)", resolved, err)
			}
		})
	}
}

func TestDialOriginUsesPerServiceTimeoutAndErrorCodes(t *testing.T) {
	tests := []struct {
		name     string
		dial     func(context.Context, string, string) (net.Conn, error)
		wantCode protocolv1.ErrorCode
		wantErr  error
	}{
		{
			name: "timeout",
			dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
			wantCode: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT, wantErr: context.DeadlineExceeded,
		},
		{
			name: "refused", dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, syscall.ECONNREFUSED
			},
			wantCode: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED, wantErr: syscall.ECONNREFUSED,
		},
		{
			name: "unreachable", dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("fixture unreachable")
			},
			wantCode: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE, wantErr: ErrDial,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := activeManager(t, service("tcp", "origin.test", 8080))
			manager.dialContext = test.dial
			started := time.Now()
			connection, code, err := manager.DialOrigin(context.Background(), testServiceID)
			if connection != nil || code != test.wantCode || !errors.Is(err, test.wantErr) {
				t.Fatalf("DialOrigin() = (%v, %s, %v)", connection, code, err)
			}
			if test.name == "timeout" && time.Since(started) > 500*time.Millisecond {
				t.Fatalf("per-Service timeout took %v", time.Since(started))
			}
		})
	}
}

func TestDialOriginMapsSnapshotVisibilityAndServiceState(t *testing.T) {
	tests := []struct {
		name      string
		manager   func(*testing.T) *Manager
		serviceID string
		wantCode  protocolv1.ErrorCode
		wantErr   error
	}{
		{
			name: "unobserved", manager: func(*testing.T) *Manager { return New() }, serviceID: testServiceID,
			wantCode: protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED, wantErr: ErrConfigNotObserved,
		},
		{
			name: "missing", manager: func(t *testing.T) *Manager {
				return activeManager(t)
			}, serviceID: testServiceID,
			wantCode: protocolv1.ErrorCode_ERROR_CODE_SERVICE_NOT_FOUND, wantErr: ErrServiceNotFound,
		},
		{
			name: "disabled", manager: func(t *testing.T) *Manager {
				configured := service("tcp", "127.0.0.1", 8080)
				configured.Enabled = false
				return activeManager(t, configured)
			}, serviceID: testServiceID,
			wantCode: protocolv1.ErrorCode_ERROR_CODE_SERVICE_DISABLED, wantErr: ErrServiceDisabled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection, code, err := test.manager(t).DialOrigin(context.Background(), test.serviceID)
			if connection != nil || code != test.wantCode || !errors.Is(err, test.wantErr) {
				t.Fatalf("DialOrigin() = (%v, %s, %v)", connection, code, err)
			}
		})
	}
}

func TestDialOriginFailsClosedWhenMultipleResolversRemainActive(t *testing.T) {
	manager := New()
	for revision := uint64(1); revision <= 2; revision++ {
		gate := &testGate{}
		gate.active.Store(true)
		candidate := buildCandidate(t, manager, snapshotRevision(revision, service("tcp", "127.0.0.1", 8080)), gate)
		if err := candidate.Start(context.Background()); err != nil {
			t.Fatalf("Candidate.Start() error = %v", err)
		}
		t.Cleanup(func() { _ = candidate.Runtime().Retire(context.Background()) })
	}
	connection, code, err := manager.DialOrigin(context.Background(), testServiceID)
	if connection != nil || code != protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR || !errors.Is(err, ErrResolverState) {
		t.Fatalf("DialOrigin() = (%v, %s, %v), want fail-closed internal error", connection, code, err)
	}
}

func TestCandidateScopedDialerNeverCrossesSnapshotGeneration(t *testing.T) {
	manager := New()
	addresses := make(chan string, 2)
	manager.dialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		addresses <- address
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}

	firstGate := &testGate{}
	firstGate.active.Store(true)
	first := buildCandidate(t, manager, snapshotRevision(1, service("tcp", "127.0.0.1", 8080)), firstGate)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("first Candidate.Start() error = %v", err)
	}
	defer func() { _ = first.Runtime().Retire(context.Background()) }()
	firstDialer, ok := first.(interface {
		DialOrigin(context.Context, string) (net.Conn, protocolv1.ErrorCode, error)
	})
	if !ok {
		t.Fatal("origin Candidate does not expose a generation-scoped dialer")
	}

	secondGate := &testGate{}
	second := buildCandidate(t, manager, snapshotRevision(2, service("tcp", "127.0.0.1", 9090)), secondGate)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("second Candidate.Start() error = %v", err)
	}
	defer func() { _ = second.Runtime().Retire(context.Background()) }()
	secondDialer := second.(interface {
		DialOrigin(context.Context, string) (net.Conn, protocolv1.ErrorCode, error)
	})

	connection, code, err := firstDialer.DialOrigin(context.Background(), testServiceID)
	if err != nil || code != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("first scoped DialOrigin() = (%v, %s, %v)", connection, code, err)
	}
	_ = connection.Close()
	if address := <-addresses; address != "127.0.0.1:8080" {
		t.Fatalf("first scoped address = %q", address)
	}
	if connection, code, err = secondDialer.DialOrigin(context.Background(), testServiceID); connection != nil ||
		code != protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED || !errors.Is(err, ErrConfigNotObserved) {
		t.Fatalf("inactive second scoped DialOrigin() = (%v, %s, %v)", connection, code, err)
	}

	firstGate.active.Store(false)
	secondGate.active.Store(true)
	if connection, code, err = firstDialer.DialOrigin(context.Background(), testServiceID); connection != nil ||
		code != protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED || !errors.Is(err, ErrConfigNotObserved) {
		t.Fatalf("retired first scoped DialOrigin() = (%v, %s, %v)", connection, code, err)
	}
	connection, code, err = secondDialer.DialOrigin(context.Background(), testServiceID)
	if err != nil || code != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("second scoped DialOrigin() = (%v, %s, %v)", connection, code, err)
	}
	_ = connection.Close()
	if address := <-addresses; address != "127.0.0.1:9090" {
		t.Fatalf("second scoped address = %q", address)
	}
}

func TestDialOriginUsesSnapshotTimeoutBeyondFormerHandlerLimit(t *testing.T) {
	configured := service("tcp", "origin.test", 8080)
	configured.ConnectTimeoutMs = 20_000
	manager := activeManager(t, configured)
	manager.dialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		deadline, exists := ctx.Deadline()
		if !exists {
			t.Fatal("DialContext did not receive Snapshot timeout deadline")
		}
		remaining := time.Until(deadline)
		if remaining < 19*time.Second || remaining > 20*time.Second {
			t.Fatalf("DialContext remaining deadline = %v, want about 20s", remaining)
		}
		return nil, errors.New("fixture stop after observing deadline")
	}
	_, code, err := manager.DialOrigin(context.Background(), testServiceID)
	if code != protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE || !errors.Is(err, ErrDial) {
		t.Fatalf("DialOrigin() = (%s, %v)", code, err)
	}
}

func TestDialOriginDoesNotResolveDuringBuildOrCacheConnections(t *testing.T) {
	manager := activeManager(t, service("tcp", "origin.test", 8080))
	var calls atomic.Int32
	manager.dialContext = func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "origin.test:8080" {
			t.Fatalf("DialContext() = (%q, %q)", network, address)
		}
		calls.Add(1)
		client, peer := net.Pipe()
		t.Cleanup(func() { _ = peer.Close() })
		return client, nil
	}
	if calls.Load() != 0 {
		t.Fatalf("Build resolved or dialed Origin %d times", calls.Load())
	}
	for index := 0; index < 2; index++ {
		connection, code, err := manager.DialOrigin(context.Background(), testServiceID)
		if err != nil || code != protocolv1.ErrorCode_ERROR_CODE_OK {
			t.Fatalf("DialOrigin() = (%v, %s, %v)", connection, code, err)
		}
		_ = connection.Close()
	}
	if calls.Load() != 2 {
		t.Fatalf("DialContext calls = %d, want one per connection", calls.Load())
	}
}

func TestDialOriginHTTPSVerifiesCertificateAndUsesSNI(t *testing.T) {
	certificate, roots := testCertificate(t, "origin.test", nil)
	address, serverNames := startTLSOrigin(t, certificate)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("LookupPort() error = %v", err)
	}
	manager := activeManager(t, func() *protocolv1.ServiceConfig {
		value := service("https", host, uint32(port))
		value.ConnectTimeoutMs = 1_000
		value.TlsServerName = "origin.test"
		return value
	}())
	manager.rootCAs = roots

	connection, code, err := manager.DialOrigin(context.Background(), testServiceID)
	if err != nil || code != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("DialOrigin() = (%v, %s, %v)", connection, code, err)
	}
	_ = connection.Close()
	select {
	case serverName := <-serverNames:
		if serverName != "origin.test" {
			t.Fatalf("TLS SNI = %q", serverName)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS server did not observe ClientHello")
	}
}

func TestDialOriginHTTPSRejectsNameMismatchWithoutDowngrade(t *testing.T) {
	certificate, roots := testCertificate(t, "other.test", nil)
	address, _ := startTLSOrigin(t, certificate)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("LookupPort() error = %v", err)
	}
	configured := service("https", host, uint32(port))
	configured.ConnectTimeoutMs = 1_000
	configured.TlsServerName = "origin.test"
	manager := activeManager(t, configured)
	manager.rootCAs = roots

	connection, code, err := manager.DialOrigin(context.Background(), testServiceID)
	if connection != nil || code != protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TLS_ERROR || !errors.Is(err, ErrTLSHandshake) {
		t.Fatalf("DialOrigin() = (%v, %s, %v)", connection, code, err)
	}
}

func TestDialOriginHTTPSUsesIPSAN(t *testing.T) {
	certificate, roots := testCertificate(t, "", []net.IP{net.ParseIP("127.0.0.1")})
	address, _ := startTLSOrigin(t, certificate)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("Atoi() error = %v", err)
	}
	configured := service("https", host, uint32(port))
	configured.ConnectTimeoutMs = 1_000
	manager := activeManager(t, configured)
	manager.rootCAs = roots

	connection, code, err := manager.DialOrigin(context.Background(), testServiceID)
	if err != nil || code != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("DialOrigin() = (%v, %s, %v)", connection, code, err)
	}
	_ = connection.Close()
}

func TestDialOriginHTTPSVerifyFalseStillSendsExplicitSNI(t *testing.T) {
	certificate, _ := testCertificate(t, "other.test", nil)
	address, serverNames := startTLSOrigin(t, certificate)
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("Atoi() error = %v", err)
	}
	configured := service("https", host, uint32(port))
	configured.ConnectTimeoutMs = 1_000
	configured.TlsVerify = false
	configured.TlsServerName = "virtual.origin.test"
	manager := activeManager(t, configured)

	connection, code, err := manager.DialOrigin(context.Background(), testServiceID)
	if err != nil || code != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("DialOrigin() = (%v, %s, %v)", connection, code, err)
	}
	_ = connection.Close()
	select {
	case serverName := <-serverNames:
		if serverName != "virtual.origin.test" {
			t.Fatalf("TLS SNI = %q", serverName)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS server did not observe ClientHello")
	}
}

func TestConfigRuntimeKeepsResolverUnpublishedUntilAckAndRejectsInvalidOrigin(t *testing.T) {
	resolver := New()
	manager, err := configruntime.New(context.Background(), configruntime.Config{
		ProtocolVersion: 1, MaxServices: configruntime.MaxServicesPerTunnel,
		MaxSnapshotBytes: configruntime.MaxSnapshotSize, MaxControlFrameBytes: int(frame.MaxControlFrameSize),
		RetireTimeout: time.Second, Builder: resolver,
	})
	if err != nil {
		t.Fatalf("configruntime.New() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Errorf("Manager.Close() error = %v", err)
		}
	})
	session, err := manager.NewSession(testTunnelID)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}

	releaseAck := make(chan struct{})
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- session.Apply(context.Background(), snapshot(service("tcp", "127.0.0.1", 8080)), ackSinkFunc(
			func(*protocolv1.ControlEnvelope) error {
				<-releaseAck
				return nil
			},
		))
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if view, exists := manager.Current(); exists && !view.Acked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Snapshot was not atomically installed before Ack")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := resolver.Resolve(testServiceID); !errors.Is(err, ErrConfigNotObserved) {
		t.Fatalf("Resolve() before Ack error = %v", err)
	}
	close(releaseAck)
	if err := <-applyDone; err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if _, err := resolver.Resolve(testServiceID); err != nil {
		t.Fatalf("Resolve() after Ack error = %v", err)
	}

	releaseSecondAck := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- session.Apply(context.Background(), snapshotRevision(2, service("tcp", "127.0.0.1", 9090)), ackSinkFunc(
			func(*protocolv1.ControlEnvelope) error {
				<-releaseSecondAck
				return nil
			},
		))
	}()
	deadline = time.Now().Add(time.Second)
	for {
		if view, exists := manager.Current(); exists && view.Revision == 2 && !view.Acked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second Snapshot was not installed before Ack")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := resolver.Resolve(testServiceID); !errors.Is(err, ErrConfigNotObserved) {
		t.Fatalf("Resolve() during replacement Ack window error = %v", err)
	}
	close(releaseSecondAck)
	if err := <-secondDone; err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	if resolved, err := resolver.Resolve(testServiceID); err != nil || resolved.Port != 9090 {
		t.Fatalf("Resolve() after replacement Ack = (%#v, %v)", resolved, err)
	}

	invalid := service("tcp", "bad host", 8080)
	var rejected *protocolv1.ConfigAck
	err = session.Apply(context.Background(), snapshotRevision(3, invalid), ackSinkFunc(func(envelope *protocolv1.ControlEnvelope) error {
		rejected = envelope.GetConfigAck()
		return nil
	}))
	if !errors.Is(err, configruntime.ErrConfigRejected) || rejected == nil ||
		rejected.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR {
		t.Fatalf("invalid Apply() = (%v, %#v)", err, rejected)
	}
	if _, err := resolver.Resolve(testServiceID); err != nil {
		t.Fatalf("old Resolver was not retained after rejected Candidate: %v", err)
	}
}

func activeManager(t *testing.T, configured ...*protocolv1.ServiceConfig) *Manager {
	t.Helper()
	manager := New()
	gate := &testGate{}
	gate.active.Store(true)
	candidate := buildCandidate(t, manager, snapshot(configured...), gate)
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatalf("Candidate.Start() error = %v", err)
	}
	t.Cleanup(func() { _ = candidate.Runtime().Retire(context.Background()) })
	return manager
}

func buildCandidate(t *testing.T, manager *Manager, configured *protocolv1.TunnelSnapshot, gate configruntime.Gate) configruntime.Candidate {
	t.Helper()
	candidate, err := manager.Build(context.Background(), configured, gate)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return candidate
}

func snapshot(configured ...*protocolv1.ServiceConfig) *protocolv1.TunnelSnapshot {
	return snapshotRevision(1, configured...)
}

func snapshotRevision(revision uint64, configured ...*protocolv1.ServiceConfig) *protocolv1.TunnelSnapshot {
	for _, value := range configured {
		value.RequiredRevision = revision
	}
	return &protocolv1.TunnelSnapshot{TunnelId: testTunnelID, Revision: revision, Services: configured}
}

func service(scheme, host string, port uint32) *protocolv1.ServiceConfig {
	return &protocolv1.ServiceConfig{
		ServiceId: testServiceID, OriginScheme: scheme, OriginHost: host, OriginPort: port,
		ConnectTimeoutMs: 50, TlsVerify: true, Enabled: true, RequiredRevision: 1,
	}
}

type testGate struct{ active atomic.Bool }

func (gate *testGate) Active() bool { return gate.active.Load() }

type ackSinkFunc func(*protocolv1.ControlEnvelope) error

func (sink ackSinkFunc) Enqueue(envelope *protocolv1.ControlEnvelope) error { return sink(envelope) }

func testCertificate(t *testing.T, dnsName string, ipAddresses []net.IP) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate certificate key: %v", err)
	}
	dnsNames := []string(nil)
	if dnsName != "" {
		dnsNames = []string{dnsName}
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: dnsName},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: dnsNames, IPAddresses: ipAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey}
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	roots.AddCert(parsed)
	return certificate, roots
}

func startTLSOrigin(t *testing.T, certificate tls.Certificate) (string, <-chan string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen TLS origin: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverNames := make(chan string, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		tlsServer := tls.Server(connection, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				serverNames <- hello.ServerName
				return nil, nil
			},
		})
		_ = tlsServer.Handshake()
	}()
	return listener.Addr().String(), serverNames
}
