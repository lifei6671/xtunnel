package controlauth

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
)

const (
	testTunnelID  = "tun_01J00000000000000000000000"
	testTokenID   = "tok_01J00000000000000000000000"
	testSessionID = "sess_01J00000000000000000000000"
)

func TestAuthenticateSuccess(t *testing.T) {
	config := testConfig(t)
	secret := bytes.Repeat([]byte{0x5a}, 32)
	response := successResult(testTunnelID, testSessionID, secret, 1, 7, 15_000)

	session, err, request := exchange(t, config, func(connection net.Conn) error {
		return frame.WriteAuth(connection, response)
	})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if session.TunnelID != testTunnelID || session.ConnectorID != config.Connector.ID() ||
		session.SessionID != testSessionID || session.ProtocolVersion != 1 ||
		session.DesiredRevision != 7 || session.HeartbeatInterval != 15*time.Second {
		t.Fatalf("Authenticate() session = %+v", session)
	}
	if !bytes.Equal(session.SessionSecret[:], bytes.Repeat([]byte{0x5a}, 32)) {
		t.Fatal("Authenticate() did not preserve the 32-byte session secret")
	}
	if session.Control == nil || session.Control.Phase() != state.ControlEstablished {
		t.Fatalf("Authenticate() control phase = %v, want ESTABLISHED", session.Control.Phase())
	}
	if request.GetConnectionToken() != config.ConnectionToken || request.GetConnectorId() != config.Connector.ID() ||
		request.GetHostname() != config.Hostname || request.GetVersion() != config.Version ||
		request.GetOs() != config.OS || request.GetArch() != config.Arch ||
		request.GetMinProtocol() != config.MinProtocol || request.GetMaxProtocol() != config.MaxProtocol ||
		len(request.GetCapabilities()) != 2 {
		t.Fatalf("ConnectorAuthRequest = %+v", request)
	}
	// Authenticate 会清理响应消息中的 Secret 副本；返回 Session 仍持有独立固定长度副本。
	if !bytes.Equal(secret, bytes.Repeat([]byte{0x5a}, 32)) {
		t.Fatal("caller-owned source secret was unexpectedly modified")
	}
}

func TestAuthenticateRejectsInvalidSuccessSemantics(t *testing.T) {
	tests := []struct {
		name     string
		response *protocolv1.ConnectorAuthResult
	}{
		{name: "Tunnel identity mismatch", response: successResult("tun_01J00000000000000000000001", testSessionID, make([]byte, 32), 1, 0, 1_000)},
		{name: "bad session id", response: successResult(testTunnelID, "sess-invalid", make([]byte, 32), 1, 0, 1_000)},
		{name: "short session secret", response: successResult(testTunnelID, testSessionID, make([]byte, 31), 1, 0, 1_000)},
		{name: "unsupported version", response: successResult(testTunnelID, testSessionID, make([]byte, 32), 2, 0, 1_000)},
		{name: "zero protocol version", response: successResult(testTunnelID, testSessionID, make([]byte, 32), 0, 0, 1_000)},
		{name: "zero heartbeat", response: successResult(testTunnelID, testSessionID, make([]byte, 32), 1, 0, 0)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err, _ := exchange(t, testConfig(t), func(connection net.Conn) error {
				return frame.WriteAuth(connection, test.response)
			})
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Authenticate() error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestAuthenticateClassifiesExplicitFailure(t *testing.T) {
	tests := []struct {
		name      string
		code      protocolv1.ErrorCode
		retryMS   uint32
		wantClass FailureClass
		wantRetry bool
		wantProto bool
	}{
		{name: "invalid token is permanent", code: protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID, wantClass: FailurePermanent},
		{name: "revoked token is permanent", code: protocolv1.ErrorCode_ERROR_CODE_TOKEN_REVOKED, wantClass: FailurePermanent},
		{name: "unsupported version is permanent", code: protocolv1.ErrorCode_ERROR_CODE_VERSION_UNSUPPORTED, wantClass: FailurePermanent},
		{name: "capacity failure is retryable", code: protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED, retryMS: 1_500, wantClass: FailureRetryable, wantRetry: true},
		{name: "internal failure is retryable", code: protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, wantClass: FailureRetryable, wantRetry: true},
		{name: "permanent failure cannot request retry", code: protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID, retryMS: 10, wantProto: true},
		{name: "OK is not a failure", code: protocolv1.ErrorCode_ERROR_CODE_OK, wantProto: true},
		{name: "non-auth code is rejected", code: protocolv1.ErrorCode_ERROR_CODE_SERVICE_NOT_FOUND, wantProto: true},
		{name: "unknown code is rejected", code: protocolv1.ErrorCode(99_999), wantProto: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Failure{
				Failure: &protocolv1.ConnectorAuthFailure{ErrorCode: test.code, RetryAfterMs: test.retryMS},
			}}
			_, err, _ := exchange(t, testConfig(t), func(connection net.Conn) error {
				return frame.WriteAuth(connection, response)
			})
			if test.wantProto {
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("Authenticate() error = %v, want ErrProtocol", err)
				}
				return
			}
			var failure *Failure
			if !errors.As(err, &failure) {
				t.Fatalf("Authenticate() error = %v, want *Failure", err)
			}
			if failure.Code != test.code || failure.Class != test.wantClass ||
				failure.RetryAfter != time.Duration(test.retryMS)*time.Millisecond || failure.Retryable() != test.wantRetry {
				t.Fatalf("Failure = %+v, retryable=%t", failure, failure.Retryable())
			}
		})
	}
}

func TestAuthenticateRejectsMalformedUnknownAndOversizedResults(t *testing.T) {
	tests := []struct {
		name    string
		handler func(net.Conn) error
	}{
		{
			name: "non-canonical frame length",
			handler: func(connection net.Conn) error {
				_, err := connection.Write([]byte{0x80, 0x00})
				return err
			},
		},
		{
			name: "malformed protobuf",
			handler: func(connection net.Conn) error {
				return frame.WritePayload(connection, []byte{0xff}, frame.MaxAuthFrameSize)
			},
		},
		{
			name: "recursive unknown field",
			handler: func(connection net.Conn) error {
				response := successResult(testTunnelID, testSessionID, make([]byte, 32), 1, 0, 1_000)
				response.GetSuccess().ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
				return frame.WriteAuth(connection, response)
			},
		},
		{
			name: "oversized frame",
			handler: func(connection net.Conn) error {
				var prefix [binary.MaxVarintLen64]byte
				length := binary.PutUvarint(prefix[:], frame.MaxAuthFrameSize+1)
				_, err := connection.Write(prefix[:length])
				return err
			},
		},
		{
			name: "missing result oneof",
			handler: func(connection net.Conn) error {
				return frame.WriteAuth(connection, &protocolv1.ConnectorAuthResult{})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err, _ := exchange(t, testConfig(t), test.handler)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Authenticate() error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestClassifyReadErrorPreservesTruncatedAuthResultCause(t *testing.T) {
	tests := []struct {
		name      string
		frameData []byte
		wantCause error
	}{
		{name: "truncated length", frameData: []byte{0x80}, wantCause: io.EOF},
		{name: "truncated payload", frameData: []byte{0x03, 0x01, 0x02}, wantCause: io.ErrUnexpectedEOF},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			readErr := frame.ReadAuth(bytes.NewReader(test.frameData), &protocolv1.ConnectorAuthResult{})
			err := classifyReadError(readErr)
			if !errors.Is(err, frame.ErrTruncatedFrame) {
				t.Fatalf("classifyReadError() error = %v, want ErrTruncatedFrame identity", err)
			}
			if !errors.Is(err, test.wantCause) {
				t.Fatalf("classifyReadError() error = %v, want cause %v", err, test.wantCause)
			}
			if errors.Is(err, ErrProtocol) {
				t.Fatalf("classifyReadError() error = %v, must not be permanent ErrProtocol", err)
			}
		})
	}
}

func TestClassifyReadErrorPreservesConnectionResetCause(t *testing.T) {
	connectionReset := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	truncated := fmt.Errorf("%w: payload: %w", frame.ErrTruncatedFrame, connectionReset)

	err := classifyReadError(truncated)
	if !errors.Is(err, frame.ErrTruncatedFrame) {
		t.Fatalf("classifyReadError() error = %v, want ErrTruncatedFrame identity", err)
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("classifyReadError() error = %v, want ECONNRESET identity", err)
	}
	if errors.Is(err, ErrProtocol) {
		t.Fatalf("classifyReadError() error = %v, must not be permanent ErrProtocol", err)
	}
}

func TestAuthenticateRejectsInvalidInputBeforeNetworkIO(t *testing.T) {
	valid := testConfig(t)
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "invalid token", mutate: func(config *Config) { config.ConnectionToken = "xta_invalid" }},
		{name: "token exceeds byte limit", mutate: func(config *Config) {
			config.ConnectionToken = string(bytes.Repeat([]byte{'x'}, maxConnectionTokenBytes+1))
		}},
		{name: "zero connector", mutate: func(config *Config) { config.Connector = identity.Connector{} }},
		{name: "protocol range excludes v1", mutate: func(config *Config) { config.MinProtocol = 2; config.MaxProtocol = 2 }},
		{name: "missing write timeout", mutate: func(config *Config) { config.WriteTimeout = 0 }},
		{name: "missing read timeout", mutate: func(config *Config) { config.ReadTimeout = 0 }},
		{name: "auth request exceeds frame limit", mutate: func(config *Config) {
			config.Capabilities = []string{string(bytes.Repeat([]byte{'x'}, int(frame.MaxAuthFrameSize)))}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			_, err := Authenticate(context.Background(), left, config)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Authenticate() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestAuthenticateWriteAndReadTimeouts(t *testing.T) {
	t.Run("write timeout", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		config := testConfig(t)
		config.WriteTimeout = 120 * time.Millisecond

		_, err := Authenticate(context.Background(), client, config)
		if !errors.Is(err, ErrWriteTimeout) {
			t.Fatalf("Authenticate() error = %v, want ErrWriteTimeout", err)
		}
	})

	t.Run("read timeout", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		requestRead := make(chan error, 1)
		go func() {
			requestRead <- frame.ReadAuth(server, &protocolv1.ConnectorAuthRequest{})
		}()
		config := testConfig(t)
		config.ReadTimeout = 120 * time.Millisecond

		_, err := Authenticate(context.Background(), client, config)
		if !errors.Is(err, ErrReadTimeout) {
			t.Fatalf("Authenticate() error = %v, want ErrReadTimeout", err)
		}
		if err := <-requestRead; err != nil {
			t.Fatalf("server ReadAuth() error = %v", err)
		}
	})
}

func TestAuthenticateContextCancellationInterruptsBlockedWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(80*time.Millisecond, cancel)
	defer timer.Stop()
	config := testConfig(t)
	config.WriteTimeout = 5 * time.Second

	_, err := Authenticate(ctx, client, config)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Authenticate() error = %v, want context.Canceled", err)
	}
}

func TestAuthenticateContextCancellationInterruptsBlockedRead(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	requestRead := make(chan error, 1)
	go func() {
		requestRead <- frame.ReadAuth(server, &protocolv1.ConnectorAuthRequest{})
	}()
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(80*time.Millisecond, cancel)
	defer timer.Stop()
	config := testConfig(t)
	config.ReadTimeout = 5 * time.Second

	_, err := Authenticate(ctx, client, config)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Authenticate() error = %v, want context.Canceled", err)
	}
	if err := <-requestRead; err != nil {
		t.Fatalf("server ReadAuth() error = %v", err)
	}
}

func TestAuthenticateClearsTemporaryDeadlinesBeforeHandoff(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	serverResult := make(chan error, 1)
	go func() {
		request := &protocolv1.ConnectorAuthRequest{}
		if err := frame.ReadAuth(server, request); err != nil {
			serverResult <- err
			return
		}
		if err := frame.WriteAuth(server, successResult(testTunnelID, testSessionID, make([]byte, 32), 1, 0, 1_000)); err != nil {
			serverResult <- err
			return
		}
		var payload [1]byte
		if _, err := server.Read(payload[:]); err != nil {
			serverResult <- err
			return
		}
		_, err := server.Write(payload[:])
		serverResult <- err
	}()
	config := testConfig(t)
	config.WriteTimeout = 120 * time.Millisecond
	config.ReadTimeout = 120 * time.Millisecond

	if _, err := Authenticate(context.Background(), client, config); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	// 等认证阶段 Deadline 过期后再复用连接，证明成功返回前已清除临时边界。
	time.Sleep(180 * time.Millisecond)
	if _, err := client.Write([]byte{0x42}); err != nil {
		t.Fatalf("post-auth Write() error = %v", err)
	}
	var response [1]byte
	if _, err := client.Read(response[:]); err != nil || response[0] != 0x42 {
		t.Fatalf("post-auth Read() = (%x, %v)", response, err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server post-auth exchange error = %v", err)
	}
}

func exchange(t *testing.T, config Config, handler func(net.Conn) error) (*Session, error, *protocolv1.ConnectorAuthRequest) {
	t.Helper()
	client, server := net.Pipe()
	serverResult := make(chan error, 1)
	requestResult := make(chan *protocolv1.ConnectorAuthRequest, 1)
	go func() {
		request := &protocolv1.ConnectorAuthRequest{}
		if err := frame.ReadAuth(server, request); err != nil {
			serverResult <- err
			return
		}
		requestResult <- request
		serverResult <- handler(server)
	}()

	session, err := Authenticate(context.Background(), client, config)
	_ = client.Close()
	_ = server.Close()
	serverErr := <-serverResult
	if serverErr != nil && !errors.Is(serverErr, net.ErrClosed) {
		t.Fatalf("server exchange error = %v", serverErr)
	}
	request := <-requestResult
	return session, err, request
}

func testConfig(t *testing.T) Config {
	t.Helper()
	connector, err := identity.NewConnector()
	if err != nil {
		t.Fatalf("identity.NewConnector() error = %v", err)
	}
	return Config{
		ConnectionToken: testToken(t),
		Connector:       connector,
		Hostname:        "connector-a",
		Version:         "v0.1.0-test",
		OS:              "linux",
		Arch:            "amd64",
		MinProtocol:     1,
		MaxProtocol:     1,
		Capabilities:    []string{"tcp", "health"},
		WriteTimeout:    time.Second,
		ReadTimeout:     time.Second,
	}
}

func testToken(t *testing.T) string {
	t.Helper()
	text, err := connectiontoken.Encode(&protocolv1.ConnectionToken{
		FormatVersion: connectiontoken.FormatVersionV1,
		Endpoint:      &protocolv1.GatewayEndpoint{Host: "gateway.example.com", Port: 7844},
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

func successResult(tunnelID, sessionID string, secret []byte, version uint32, revision uint64, heartbeatMS uint32) *protocolv1.ConnectorAuthResult {
	return &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Success{
		Success: &protocolv1.ConnectorAuthSuccess{
			TunnelId:            tunnelID,
			SessionId:           sessionID,
			SessionSecret:       append([]byte(nil), secret...),
			ProtocolVersion:     version,
			DesiredRevision:     revision,
			HeartbeatIntervalMs: heartbeatMS,
		},
	}}
}

func TestFailureStringContainsNoSensitiveInput(t *testing.T) {
	failure := &Failure{Code: protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID, Class: FailurePermanent}
	if got := failure.Error(); got != "connector control auth rejected: code=ERROR_CODE_TOKEN_INVALID class=permanent retry_after=0s" {
		t.Fatalf("Failure.Error() = %q", got)
	}
	if fmt.Sprint(failure) != failure.Error() {
		t.Fatal("Failure formatting is unstable")
	}
}
