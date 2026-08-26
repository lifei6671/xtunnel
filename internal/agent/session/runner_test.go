package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

const (
	testTunnelID  = "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testTokenID   = "tok_01ARZ3NDEKTSV4RRFFQ69G5FAW"
	testSessionID = "sess_01ARZ3NDEKTSV4RRFFQ69G5FAX"
	nextSessionID = "sess_01ARZ3NDEKTSV4RRFFQ69G5FAY"
)

func TestRunnerReusesConnectorAcrossSequentialControlSessions(t *testing.T) {
	config := testRunnerConfig(t)
	originalCapabilities := config.Capabilities
	releaseSecond := make(chan struct{})
	requests := make(chan *protocolv1.ConnectorAuthRequest, 2)
	serverResults := make(chan error, 2)
	heartbeatRead := make(chan struct{}, 1)
	dialCount := 0
	sessionIDs := []string{testSessionID, nextSessionID}

	dial := func(_ context.Context, token, alpn string) (net.Conn, error) {
		if token != config.ConnectionToken {
			t.Fatalf("Dial Token 未逐字节复用")
		}
		if alpn != servergateway.ControlALPN {
			t.Fatalf("Dial ALPN = %q, want %q", alpn, servergateway.ControlALPN)
		}
		attempt := dialCount
		dialCount++
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			serverResults <- serveControlAttempt(server, attempt, sessionIDs[attempt], requests, heartbeatRead, releaseSecond)
		}()
		return client, nil
	}

	runner, err := newRunner(config, dependencies{
		dial:         dial,
		authenticate: controlauth.Authenticate,
		newOwner:     controlsession.NewOwner,
	})
	if err != nil {
		t.Fatalf("newRunner() error = %v", err)
	}
	// 构造后修改调用方切片，后续 AUTH 仍必须使用 Runner 自己的不可变副本。
	originalCapabilities[0] = "mutated-after-construction"

	firstContext, cancelFirst := context.WithCancel(context.Background())
	first, err := runner.Start(firstContext)
	if err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	firstRequest := receiveRequest(t, requests)
	assertAuthRequest(t, firstRequest, config.ConnectionToken, config.Connector.ID(), "tcp")

	if _, err := runner.Start(context.Background()); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("concurrent Start() error = %v, want ErrSessionActive", err)
	}
	if dialCount != 1 {
		t.Fatalf("并发 Start 后 Dial 次数 = %d, want 1", dialCount)
	}

	workAuthentication, err := first.WorkAuthSession()
	if err != nil {
		t.Fatalf("first WorkAuthSession() error = %v", err)
	}
	if workAuthentication.TunnelID != testTunnelID || workAuthentication.ConnectorID != config.Connector.ID() ||
		workAuthentication.SessionID != testSessionID || workAuthentication.Control != nil {
		t.Fatalf("first WorkAuthSession() = %+v", workAuthentication)
	}
	// 返回值必须是副本；调用方清理自己的 Secret 不能破坏 Session 内的认证材料。
	clear(workAuthentication.SessionSecret[:])
	secondCopy, err := first.WorkAuthSession()
	if err != nil || !bytes.Equal(secondCopy.SessionSecret[:], bytes.Repeat([]byte{0x41}, 32)) {
		t.Fatalf("第二份 WorkAuthSession Secret 被调用方修改: error=%v secret=%x", err, secondCopy.SessionSecret)
	}

	select {
	case inbound := <-first.Inbound():
		if inbound.Envelope.GetConfigSnapshot().GetRevision() != 7 {
			t.Fatalf("入站 Snapshot revision = %d, want 7", inbound.Envelope.GetConfigSnapshot().GetRevision())
		}
	case <-time.After(time.Second):
		t.Fatal("等待 Owner 入站 Snapshot 超时")
	}
	if err := first.Enqueue(&protocolv1.ControlEnvelope{
		ProtocolVersion: 1,
		Payload: &protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{
			ObservedRevision: 7,
		}},
	}); err != nil {
		t.Fatalf("Session.Enqueue() error = %v", err)
	}
	select {
	case <-heartbeatRead:
	case <-time.After(time.Second):
		t.Fatal("等待 Server 读取 Heartbeat 超时")
	}

	cancelFirst()
	if err := first.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Wait() error = %v, want context.Canceled", err)
	}
	if err := <-serverResults; err != nil {
		t.Fatalf("first server attempt error = %v", err)
	}
	if _, err := first.WorkAuthSession(); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("closed WorkAuthSession() error = %v, want ErrSessionClosed", err)
	}
	if !allZero(first.authentication.SessionSecret[:]) {
		t.Fatalf("Control Session 结束后 Secret 未清理: %x", first.authentication.SessionSecret)
	}

	second, err := runner.Start(context.Background())
	if err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	secondRequest := receiveRequest(t, requests)
	assertAuthRequest(t, secondRequest, config.ConnectionToken, config.Connector.ID(), "tcp")
	secondAuthentication, err := second.WorkAuthSession()
	if err != nil {
		t.Fatalf("second WorkAuthSession() error = %v", err)
	}
	if secondAuthentication.SessionID != nextSessionID {
		t.Fatalf("second SessionID = %q, want %q", secondAuthentication.SessionID, nextSessionID)
	}
	close(releaseSecond)
	if err := second.Wait(); err != nil {
		t.Fatalf("second Wait() error = %v, want nil", err)
	}
	if err := <-serverResults; err != nil {
		t.Fatalf("second server attempt error = %v", err)
	}
	if dialCount != 2 {
		t.Fatalf("顺序重连后的 Dial 次数 = %d, want 2", dialCount)
	}
}

func TestStartDetachedKeepsControlWritableUntilExplicitClose(t *testing.T) {
	config := testRunnerConfig(t)
	heartbeatRead := make(chan struct{}, 1)
	serverResult := make(chan error, 1)
	runner, err := newRunner(config, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				request := &protocolv1.ConnectorAuthRequest{}
				if err := frame.ReadAuth(server, request); err != nil {
					serverResult <- err
					return
				}
				if err := frame.WriteAuth(server, authSuccess(testSessionID)); err != nil {
					serverResult <- err
					return
				}
				heartbeat := &protocolv1.ControlEnvelope{}
				if err := frame.ReadControl(server, heartbeat); err != nil {
					serverResult <- err
					return
				}
				if heartbeat.GetHeartbeat() == nil {
					serverResult <- errors.New("expected draining heartbeat")
					return
				}
				heartbeatRead <- struct{}{}
				_, readErr := server.Read(make([]byte, 1))
				if errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
					serverResult <- nil
					return
				}
				serverResult <- readErr
			}()
			return client, nil
		},
		authenticate: controlauth.Authenticate,
		newOwner:     controlsession.NewOwner,
	})
	if err != nil {
		t.Fatalf("newRunner() error = %v", err)
	}

	attemptContext, cancelAttempt := context.WithCancel(context.Background())
	session, err := runner.StartDetached(attemptContext)
	if err != nil {
		t.Fatalf("StartDetached() error = %v", err)
	}
	cancelAttempt()
	select {
	case <-session.Done():
		t.Fatal("建连 Context 取消提前关闭了 detached Control Session")
	case <-time.After(20 * time.Millisecond):
	}
	if err := session.Enqueue(&protocolv1.ControlEnvelope{
		ProtocolVersion: 1,
		Payload: &protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{
			TimestampMs: 1,
		}},
	}); err != nil {
		t.Fatalf("detached Session.Enqueue() error = %v", err)
	}
	select {
	case <-heartbeatRead:
	case <-time.After(time.Second):
		t.Fatal("取消后 Control socket 不再可写")
	}
	session.Close()
	if err := session.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Session.Wait() error = %v, want context.Canceled", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server result = %v", err)
	}
}

func TestLateOldGenerationCleanupCannotReleaseCurrentAttempt(t *testing.T) {
	runner := &Runner{}
	oldGeneration, err := runner.reserveAttempt()
	if err != nil {
		t.Fatalf("reserve old attempt: %v", err)
	}
	runner.releaseAttempt(oldGeneration)
	currentGeneration, err := runner.reserveAttempt()
	if err != nil {
		t.Fatalf("reserve current attempt: %v", err)
	}
	// 模拟旧 Session 的迟到清理；generation fencing 必须保留当前代 busy 所有权。
	runner.releaseAttempt(oldGeneration)
	if _, err := runner.reserveAttempt(); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("late old cleanup released current attempt: %v", err)
	}
	runner.releaseAttempt(currentGeneration)
	lastGeneration, err := runner.reserveAttempt()
	if err != nil {
		t.Fatalf("current cleanup did not release runner: %v", err)
	}
	runner.releaseAttempt(lastGeneration)
}

func TestRunnerClosesConnectionWhenAuthenticationFails(t *testing.T) {
	config := testRunnerConfig(t)
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	wantErr := errors.New("test authentication failure")
	runner, err := newRunner(config, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) {
			return client, nil
		},
		authenticate: func(context.Context, net.Conn, controlauth.Config) (*controlauth.Session, error) {
			return nil, wantErr
		},
		newOwner: controlsession.NewOwner,
	})
	if err != nil {
		t.Fatalf("newRunner() error = %v", err)
	}

	if _, err := runner.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start() error = %v, want auth failure", err)
	}
	var one [1]byte
	if _, err := server.Read(one[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("认证失败后对端 Read() error = %v, want EOF", err)
	}
	if runner.busy {
		t.Fatal("认证失败后 Runner 仍被标记为 busy")
	}
}

func TestRunnerCleansAuthenticatedStateWhenOwnerCreationFails(t *testing.T) {
	config := testRunnerConfig(t)
	client, server := net.Pipe()
	serverResult := make(chan error, 1)
	go func() {
		defer server.Close()
		request := &protocolv1.ConnectorAuthRequest{}
		if err := frame.ReadAuth(server, request); err != nil {
			serverResult <- err
			return
		}
		if err := frame.WriteAuth(server, authSuccess(testSessionID)); err != nil {
			serverResult <- err
			return
		}
		var one [1]byte
		_, err := server.Read(one[:])
		if errors.Is(err, io.EOF) {
			err = nil
		}
		serverResult <- err
	}()

	wantErr := errors.New("test owner creation failure")
	var authentication *controlauth.Session
	runner, err := newRunner(config, dependencies{
		dial: func(context.Context, string, string) (net.Conn, error) {
			return client, nil
		},
		authenticate: func(ctx context.Context, connection net.Conn, authConfig controlauth.Config) (*controlauth.Session, error) {
			var err error
			authentication, err = controlauth.Authenticate(ctx, connection, authConfig)
			return authentication, err
		},
		newOwner: func(net.Conn, *state.Control, controlsession.Options) (*controlsession.Owner, error) {
			return nil, wantErr
		},
	})
	if err != nil {
		t.Fatalf("newRunner() error = %v", err)
	}

	if _, err := runner.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start() error = %v, want owner creation failure", err)
	}
	if authentication == nil {
		t.Fatal("认证成功结果未被记录")
	}
	if !allZero(authentication.SessionSecret[:]) {
		t.Fatalf("Owner 创建失败后 Secret 未清理: %x", authentication.SessionSecret)
	}
	if authentication.Control.Phase() != state.ControlClosed {
		t.Fatalf("Owner 创建失败后 Control phase = %v, want CLOSED", authentication.Control.Phase())
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server result = %v", err)
	}
}

func TestNewRunnerRejectsInvalidConfig(t *testing.T) {
	valid := testRunnerConfig(t)
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing token", mutate: func(config *Config) { config.ConnectionToken = "" }},
		{name: "oversize token", mutate: func(config *Config) {
			config.ConnectionToken = string(bytes.Repeat([]byte{'x'}, maxConnectionTokenBytes+1))
		}},
		{name: "zero connector", mutate: func(config *Config) { config.Connector = identity.Connector{} }},
		{name: "missing auth write timeout", mutate: func(config *Config) { config.AuthWriteTimeout = 0 }},
		{name: "missing auth read timeout", mutate: func(config *Config) { config.AuthReadTimeout = 0 }},
		{name: "missing high priority capacity", mutate: func(config *Config) { config.OwnerOptions.HighPriorityCapacity = 0 }},
		{name: "missing normal capacity", mutate: func(config *Config) { config.OwnerOptions.NormalCapacity = 0 }},
		{name: "missing inbound capacity", mutate: func(config *Config) { config.OwnerOptions.InboundCapacity = 0 }},
		{name: "missing owner write timeout", mutate: func(config *Config) { config.OwnerOptions.WriteTimeout = 0 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := NewRunner(config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewRunner() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func serveControlAttempt(
	connection net.Conn,
	attempt int,
	sessionID string,
	requests chan<- *protocolv1.ConnectorAuthRequest,
	heartbeatRead chan<- struct{},
	releaseSecond <-chan struct{},
) error {
	request := &protocolv1.ConnectorAuthRequest{}
	if err := frame.ReadAuth(connection, request); err != nil {
		return err
	}
	requests <- request
	if err := frame.WriteAuth(connection, authSuccess(sessionID)); err != nil {
		return err
	}
	if attempt == 1 {
		<-releaseSecond
		return nil
	}
	if err := frame.WriteControl(connection, &protocolv1.ControlEnvelope{
		ProtocolVersion: 1,
		Payload: &protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: &protocolv1.TunnelSnapshot{
			TunnelId: testTunnelID,
			Revision: 7,
		}},
	}); err != nil {
		return err
	}
	heartbeat := &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(connection, heartbeat); err != nil {
		return err
	}
	if heartbeat.GetHeartbeat() == nil {
		return errors.New("server expected Heartbeat")
	}
	heartbeatRead <- struct{}{}
	if err := frame.ReadControl(connection, &protocolv1.ControlEnvelope{}); errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	} else {
		return err
	}
}

func testRunnerConfig(t *testing.T) Config {
	t.Helper()
	connector, err := identity.NewConnector()
	if err != nil {
		t.Fatalf("identity.NewConnector() error = %v", err)
	}
	return Config{
		ConnectionToken:  testConnectionToken(t),
		Connector:        connector,
		Hostname:         "connector-a",
		Version:          "v0.1.0-test",
		OS:               "linux",
		Arch:             "amd64",
		Capabilities:     []string{"tcp", "health"},
		AuthWriteTimeout: time.Second,
		AuthReadTimeout:  time.Second,
		OwnerOptions: controlsession.Options{
			HighPriorityCapacity: 4,
			NormalCapacity:       8,
			InboundCapacity:      4,
			WriteTimeout:         time.Second,
		},
	}
}

func testConnectionToken(t *testing.T) string {
	t.Helper()
	text, err := connectiontoken.Encode(&protocolv1.ConnectionToken{
		FormatVersion: connectiontoken.FormatVersionV1,
		Endpoint:      &protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 7844},
		TlsTrust: &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{
			PublicCa: &protocolv1.PublicCATrust{},
		}},
		TunnelId:             testTunnelID,
		TokenId:              testTokenID,
		TokenVersion:         1,
		AuthenticationSecret: bytes.Repeat([]byte{0x31}, 32),
	})
	if err != nil {
		t.Fatalf("token.Encode() error = %v", err)
	}
	return text
}

func authSuccess(sessionID string) *protocolv1.ConnectorAuthResult {
	return &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Success{
		Success: &protocolv1.ConnectorAuthSuccess{
			TunnelId:            testTunnelID,
			SessionId:           sessionID,
			SessionSecret:       bytes.Repeat([]byte{0x41}, 32),
			ProtocolVersion:     1,
			DesiredRevision:     7,
			HeartbeatIntervalMs: 1_000,
		},
	}}
}

func receiveRequest(t *testing.T, requests <-chan *protocolv1.ConnectorAuthRequest) *protocolv1.ConnectorAuthRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("等待 ConnectorAuthRequest 超时")
		return nil
	}
}

func assertAuthRequest(t *testing.T, request *protocolv1.ConnectorAuthRequest, token, connectorID, capability string) {
	t.Helper()
	if request.GetConnectionToken() != token || request.GetConnectorId() != connectorID {
		t.Fatalf("AUTH 身份不匹配: token_equal=%v connector=%q", request.GetConnectionToken() == token, request.GetConnectorId())
	}
	if len(request.GetCapabilities()) != 2 || request.GetCapabilities()[0] != capability {
		t.Fatalf("AUTH capabilities = %v", request.GetCapabilities())
	}
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
