package controlauth

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

const (
	testTunnelID       = "tun_01J00000000000000000000000"
	testConnectorID    = "con_01J00000000000000000000000"
	testConnectorIDTwo = "con_01J00000000000000000000001"
	testToken          = "xta_test-secret-must-not-appear-in-errors"
)

var errTestRandom = errors.New("test random source failed")

type verifierFunc func(context.Context, string) (application.VerifiedConnectionToken, error)

func (verify verifierFunc) Verify(ctx context.Context, token string) (application.VerifiedConnectionToken, error) {
	return verify(ctx, token)
}

type handleOutcome struct {
	established Established
	err         error
}

func TestHandleSuccessFlushesBeforePublishingSession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x5a}, 256)))
	request := validRequest(testConnectorID)

	response, outcome := exchange(t, handler, request)
	success := response.GetSuccess()
	if success == nil {
		t.Fatalf("ConnectorAuthResult = %#v, want success", response)
	}
	if success.GetTunnelId() != testTunnelID || !identity.ValidSessionID(success.GetSessionId()) ||
		len(success.GetSessionSecret()) != sessionSecretSize || success.GetProtocolVersion() != protocolVersionV1 ||
		success.GetDesiredRevision() != 7 || success.GetHeartbeatIntervalMs() != 10_000 {
		t.Fatalf("ConnectorAuthSuccess = %#v, want complete negotiated state", success)
	}
	if outcome.err != nil {
		t.Fatalf("Handle() error = %v", outcome.err)
	}
	if outcome.established.Control == nil || outcome.established.Control.Phase() != state.ControlEstablished {
		t.Fatalf("Control phase = %v, want ESTABLISHED", outcome.established.Control)
	}
	if outcome.established.Session.SessionID != success.GetSessionId() || outcome.established.Session.Generation != 1 {
		t.Fatalf("Established Session = %#v, want response Session generation 1", outcome.established.Session)
	}
	if outcome.established.ConnectorMetadata != (serverruntime.ConnectorMetadata{
		Hostname: request.GetHostname(), OS: request.GetOs(), Arch: request.GetArch(), Version: request.GetVersion(),
	}) {
		t.Fatalf("Established ConnectorMetadata = %#v, want validated request metadata", outcome.established.ConnectorMetadata)
	}
	if !bytes.Equal(outcome.established.SessionSecret[:], bytes.Repeat([]byte{0x5a}, sessionSecretSize)) ||
		!bytes.Equal(success.GetSessionSecret(), outcome.established.SessionSecret[:]) {
		t.Fatal("session_secret was not the injected 32-byte CSPRNG output")
	}
	current, exists := registry.Current(testTunnelID, testConnectorID)
	if !exists || current != outcome.established.Session {
		t.Fatalf("Current() = %#v, %v, want established Session", current, exists)
	}
}

func TestHandleConnectorCapacityRejectsBeforeSuccessAndAllowsReplacement(t *testing.T) {
	limitManager, err := serverlimits.New(serverlimits.Options{
		MaxConnectors: 1, MaxConnectorsPerTunnel: 1,
		MaxWorkConnections: 1, MaxIdleWorkConnections: 1, MaxConnectingWorkConnections: 1,
		MaxPendingOpens: 1, MaxActiveConnections: 1, MaxConnectionsPerTunnel: 1,
		MaxConnectionsPerService: 1, MaxConnectionsPerSourceIP: 1,
	})
	if err != nil {
		t.Fatalf("limits.New() error = %v", err)
	}
	registry := serverruntime.NewRegistryWithLimits(limitManager)
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x6a}, 256)))
	if response, outcome := exchange(t, handler, validRequest(testConnectorID)); outcome.err != nil || response.GetSuccess() == nil {
		t.Fatalf("first Connector auth = %#v, %v, want Success", response, outcome.err)
	}
	if response, outcome := exchange(t, handler, validRequest(testConnectorID)); outcome.err != nil || response.GetSuccess() == nil {
		t.Fatalf("same Connector replacement auth = %#v, %v, want Success", response, outcome.err)
	}
	response, outcome := exchange(t, handler, validRequest(testConnectorIDTwo))
	assertFailure(t, response, outcome.err, protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED, 1_500)
	if _, exists := registry.Current(testTunnelID, testConnectorIDTwo); exists {
		t.Fatal("over-limit Connector was published after failure response")
	}
	if got := limitManager.Snapshot().Connectors; got != 1 {
		t.Fatalf("Connector count = %d, want one replacement identity", got)
	}
}

func TestHandleHealthBudgetRejectsBeforeSuccessAndRollsBackFailedReplacement(t *testing.T) {
	budget, err := healthbudget.New(healthbudget.Options{MaxTargetsPerTunnel: 1, MaxTargetsGlobal: 1})
	if err != nil {
		t.Fatalf("healthbudget.New() error = %v", err)
	}
	if err := budget.InitializeTunnel(testTunnelID, 7, 1); err != nil {
		t.Fatalf("InitializeTunnel() error = %v", err)
	}
	registry := serverruntime.NewRegistryWithLimitsAndHealthBudget(nil, budget)
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x6b}, 256)))

	response, first := exchange(t, handler, validRequest(testConnectorID))
	if first.err != nil || response.GetSuccess() == nil {
		t.Fatalf("first Connector auth = %#v, %v, want Success", response, first.err)
	}
	response, replacement := exchange(t, handler, validRequest(testConnectorID))
	if replacement.err != nil || response.GetSuccess() == nil {
		t.Fatalf("at-cap replacement auth = %#v, %v, want Success", response, replacement.err)
	}
	if replacement.established.Session.Generation != first.established.Session.Generation+1 {
		t.Fatalf("replacement Generation = %d, want %d", replacement.established.Session.Generation, first.established.Session.Generation+1)
	}

	var encoded bytes.Buffer
	if err := frame.WriteAuth(&encoded, validRequest(testConnectorID)); err != nil {
		t.Fatalf("WriteAuth(replacement request) error = %v", err)
	}
	connection := &failingWriteConn{reader: bytes.NewReader(encoded.Bytes()), writeErr: io.ErrClosedPipe}
	if _, err := handler.Handle(context.Background(), connection); err == nil || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Handle(failed replacement write) error = %v, want write failure", err)
	}
	if current, exists := registry.Current(testTunnelID, testConnectorID); !exists || current != replacement.established.Session {
		t.Fatalf("Current() = %#v, %v, want prior replacement %#v", current, exists, replacement.established.Session)
	}
	key := healthbudget.ConnectorKey{TunnelID: testTunnelID, ConnectorID: testConnectorID}
	snapshot := budget.Snapshot()
	if snapshot.TargetsGlobal != 1 || snapshot.ConnectorReferences[key] != 1 {
		t.Fatalf("budget after failed replacement write = %#v, want prior generation only", snapshot)
	}

	response, rejected := exchange(t, handler, validRequest(testConnectorIDTwo))
	assertFailure(t, response, rejected.err, protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED, 1_500)
	if _, exists := registry.Current(testTunnelID, testConnectorIDTwo); exists {
		t.Fatal("over-budget Connector was published after failure response")
	}
	snapshot = budget.Snapshot()
	if snapshot.TargetsGlobal != 1 || len(snapshot.ConnectorReferences) != 1 || snapshot.ConnectorReferences[key] != 1 {
		t.Fatalf("budget after rejected Connector = %#v, want prior owner unchanged", snapshot)
	}
}

func TestHandleFirstConnectorHealthBudgetExceededReturnsRetryableFailure(t *testing.T) {
	budget, err := healthbudget.New(healthbudget.Options{MaxTargetsPerTunnel: 1, MaxTargetsGlobal: 1})
	if err != nil {
		t.Fatalf("healthbudget.New() error = %v", err)
	}
	// 两个启用 Health 的 Service 在没有 Connector 时不占 Target；首个 Connector
	// 认证会一次需要两个 Target，因此必须在 Success 发布前被预算拒绝。
	if err := budget.InitializeTunnel(testTunnelID, 7, 2); err != nil {
		t.Fatalf("InitializeTunnel() error = %v", err)
	}
	registry := serverruntime.NewRegistryWithLimitsAndHealthBudget(nil, budget)
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x6c}, 64)))

	response, outcome := exchange(t, handler, validRequest(testConnectorID))
	assertFailure(t, response, outcome.err, protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED, 1_500)
	if response.GetSuccess() != nil {
		t.Fatalf("ConnectorAuthResult = %#v, want Failure without Success", response)
	}
	if _, exists := registry.Current(testTunnelID, testConnectorID); exists {
		t.Fatal("first over-budget authentication published a partial Session")
	}
	snapshot := budget.Snapshot()
	if snapshot.TargetsGlobal != 0 || len(snapshot.ConnectorReferences) != 0 {
		t.Fatalf("budget after first rejected authentication = %#v, want no reservation", snapshot)
	}
}

func TestHandleWriteFailureDoesNotPublishSession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x3c}, 64)))

	var encoded bytes.Buffer
	if err := frame.WriteAuth(&encoded, validRequest(testConnectorID)); err != nil {
		t.Fatalf("WriteAuth(request) error = %v", err)
	}
	connection := &failingWriteConn{reader: bytes.NewReader(encoded.Bytes()), writeErr: io.ErrClosedPipe}
	_, err := handler.Handle(context.Background(), connection)
	if err == nil || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Handle() error = %v, want write failure", err)
	}
	if _, exists := registry.Current(testTunnelID, testConnectorID); exists {
		t.Fatal("write failure left a Current Session in Registry")
	}
	if !connection.isClosed() {
		t.Fatal("write failure did not close the authentication connection")
	}
}

func TestHandleStopsBlockedAuthIOWhenContextCanceled(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, net.Conn, *deadlineObservedConn)
	}{
		{
			name: "read request",
			prepare: func(t *testing.T, _ net.Conn, connection *deadlineObservedConn) {
				t.Helper()
				select {
				case <-connection.readDeadlineSet:
				case <-time.After(time.Second):
					t.Fatal("Handle() did not start the blocked AUTH read")
				}
			},
		},
		{
			name: "write result",
			prepare: func(t *testing.T, client net.Conn, connection *deadlineObservedConn) {
				t.Helper()
				setDeadline(t, client)
				if err := frame.WriteAuth(client, validRequest(testConnectorID)); err != nil {
					t.Fatalf("WriteAuth(request) error = %v", err)
				}
				select {
				case <-connection.writeDeadlineSet:
				case <-time.After(time.Second):
					t.Fatal("Handle() did not start the blocked AUTH write")
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := testHandler(t, serverruntime.NewRegistry(), successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x2a}, 64)))
			server, client := net.Pipe()
			connection := newDeadlineObservedConn(server)
			ctx, cancel := context.WithCancel(context.Background())
			outcomes := make(chan handleOutcome, 1)
			go func() {
				established, err := handler.Handle(ctx, connection)
				outcomes <- handleOutcome{established: established, err: err}
			}()

			test.prepare(t, client, connection)
			cancel()
			select {
			case outcome := <-outcomes:
				if !errors.Is(outcome.err, context.Canceled) {
					t.Fatalf("Handle() error = %v, want context.Canceled", outcome.err)
				}
			case <-time.After(time.Second):
				t.Fatal("Handle() did not stop blocked AUTH IO after Context cancellation")
			}
			_ = client.Close()
		})
	}
}

func TestHandleReplacementPreFlushFailureRestoresPreviousSession(t *testing.T) {
	tests := []struct {
		name             string
		writeDeadlineErr error
		writeErr         error
		wantErr          error
	}{
		{name: "set write deadline", writeDeadlineErr: errSetAuthWriteDeadline, wantErr: errSetAuthWriteDeadline},
		{name: "write success frame", writeErr: io.ErrClosedPipe, wantErr: io.ErrClosedPipe},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := serverruntime.NewRegistry()
			initialHandler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x3d}, 64)))
			_, initial := exchange(t, initialHandler, validRequest(testConnectorID))
			if initial.err != nil {
				t.Fatalf("initial Handle() error = %v", initial.err)
			}

			var encoded bytes.Buffer
			if err := frame.WriteAuth(&encoded, validRequest(testConnectorID)); err != nil {
				t.Fatalf("WriteAuth(request) error = %v", err)
			}
			connection := &failingWriteConn{
				reader: bytes.NewReader(encoded.Bytes()), writeDeadlineErr: test.writeDeadlineErr, writeErr: test.writeErr,
			}
			replacementHandler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x3e}, 64)))
			_, err := replacementHandler.Handle(context.Background(), connection)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("replacement Handle() error = %v, want %v", err, test.wantErr)
			}
			if current, exists := registry.Current(testTunnelID, testConnectorID); !exists || current != initial.established.Session {
				t.Fatalf("Current() = %#v, %v, want restored previous %#v", current, exists, initial.established.Session)
			}
			if !connection.isClosed() {
				t.Fatal("replacement failure did not close the authentication connection")
			}
		})
	}
}

func TestHandleConcurrentRevokeCannotInvalidateFlushedSuccess(t *testing.T) {
	registry := serverruntime.NewRegistry()
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x4d}, 64)))
	var encoded bytes.Buffer
	if err := frame.WriteAuth(&encoded, validRequest(testConnectorID)); err != nil {
		t.Fatalf("WriteAuth(request) error = %v", err)
	}
	connection := newBlockingWriteConn(bytes.NewReader(encoded.Bytes()))
	outcomes := make(chan handleOutcome, 1)
	go func() {
		established, err := handler.Handle(context.Background(), connection)
		outcomes <- handleOutcome{established: established, err: err}
	}()

	select {
	case <-connection.writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Handle() did not reach blocked Success write")
	}
	installed, exists := registry.Current(testTunnelID, testConnectorID)
	if !exists {
		t.Fatal("Session was not atomically installed before the Success write")
	}
	revokeResult := make(chan error, 1)
	go func() { revokeResult <- registry.RevokeTunnel(testTunnelID) }()
	select {
	case err := <-revokeResult:
		if err != nil {
			t.Fatalf("RevokeTunnel() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RevokeTunnel() blocked on the in-flight network write")
	}
	close(connection.allowWrite)

	outcome := <-outcomes
	if outcome.err != nil {
		t.Fatalf("Handle() error after commit won concurrent Revoke = %v", outcome.err)
	}
	response := &protocolv1.ConnectorAuthResult{}
	if err := frame.ReadAuth(bytes.NewReader(connection.writtenBytes()), response); err != nil {
		t.Fatalf("ReadAuth(response) error = %v", err)
	}
	if response.GetSuccess() == nil || outcome.established.Session.SessionID != response.GetSuccess().GetSessionId() {
		t.Fatalf("response=%+v established=%+v, want matching committed Success", response, outcome.established)
	}
	if outcome.established.Session != installed {
		t.Fatalf("established Session = %#v, want pre-write installed %#v", outcome.established.Session, installed)
	}
	if _, err := registry.AcquireConnector(testTunnelID); !errors.Is(err, serverruntime.ErrTunnelRuntimeRevoked) {
		t.Fatalf("AcquireConnector() error = %v, want revoked fencing after prepared commit", err)
	}
}

func TestHandlePostFlushDeadlineFailureDoesNotRestorePreviousSession(t *testing.T) {
	registry := serverruntime.NewRegistry()
	initialHandler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x11}, 64)))
	_, initial := exchange(t, initialHandler, validRequest(testConnectorID))
	if initial.err != nil {
		t.Fatalf("initial Handle() error = %v", initial.err)
	}
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x12}, 64)))
	serverRaw, client := net.Pipe()
	connection := &clearDeadlineFailConn{
		Conn: serverRaw,
		onClear: func() {
			if _, exists := registry.Current(testTunnelID, testConnectorID); !exists {
				t.Error("Success Frame 已写出，但清理 Deadline 前 Session 尚未提交")
			}
		},
	}
	outcomes := make(chan handleOutcome, 1)
	go func() {
		established, err := handler.Handle(context.Background(), connection)
		outcomes <- handleOutcome{established: established, err: err}
	}()
	setDeadline(t, client)
	if err := frame.WriteAuth(client, validRequest(testConnectorID)); err != nil {
		t.Fatalf("WriteAuth(request) error = %v", err)
	}
	response := &protocolv1.ConnectorAuthResult{}
	if err := frame.ReadAuth(client, response); err != nil {
		t.Fatalf("ReadAuth(response) error = %v", err)
	}
	if response.GetSuccess() == nil {
		t.Fatalf("ConnectorAuthResult = %+v, want Success", response)
	}
	outcome := <-outcomes
	if !errors.Is(outcome.err, errClearAuthDeadline) {
		t.Fatalf("Handle() error = %v, want deadline cleanup failure", outcome.err)
	}
	if current, exists := registry.Current(testTunnelID, testConnectorID); exists {
		t.Fatalf("post-flush cleanup restored or retained Session %#v; previous was %#v", current, initial.established.Session)
	}
	if !connection.isClosed() {
		t.Fatal("deadline cleanup failure did not close the Control connection")
	}
	if err := client.Close(); err != nil {
		t.Fatalf("client Close() error = %v", err)
	}
}

func TestHandleRejectsUnsafeFramesWithoutFailureResponse(t *testing.T) {
	tests := []struct {
		name  string
		bytes []byte
		want  error
	}{
		{name: "malformed protobuf", bytes: []byte{0x01, 0xff}, want: frame.ErrMalformedMessage},
		{name: "truncated payload", bytes: []byte{0x02, 0x08}, want: frame.ErrTruncatedFrame},
		{name: "oversize", bytes: frameLength(frame.MaxAuthFrameSize + 1), want: frame.ErrFrameTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := serverruntime.NewRegistry()
			handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x11}, 64)))
			server, client := net.Pipe()
			outcomes := make(chan handleOutcome, 1)
			go func() {
				established, err := handler.Handle(context.Background(), server)
				outcomes <- handleOutcome{established: established, err: err}
			}()
			setDeadline(t, client)
			if _, err := client.Write(test.bytes); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("client Write() error = %v", err)
			}
			var one [1]byte
			if _, err := client.Read(one[:]); !errors.Is(err, io.EOF) {
				t.Fatalf("client Read() error = %v, want EOF without failure Frame", err)
			}
			outcome := <-outcomes
			if !errors.Is(outcome.err, test.want) {
				t.Fatalf("Handle() error = %v, want %v", outcome.err, test.want)
			}
			var handleErr *HandleError
			if !errors.As(outcome.err, &handleErr) || handleErr.FailureSent() || handleErr.Code() != protocolv1.ErrorCode_ERROR_CODE_OK {
				t.Fatalf("HandleError = %#v, want direct close without protocol result", handleErr)
			}
			if err := client.Close(); err != nil {
				t.Fatalf("client Close() error = %v", err)
			}
		})
	}
}

func TestHandleRejectsUnknownAndInvalidRequestAsProtocolError(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*protocolv1.ConnectorAuthRequest)
	}{
		{
			name: "unknown field",
			mutate: func(request *protocolv1.ConnectorAuthRequest) {
				request.ProtoReflect().SetUnknown([]byte{0x50, 0x01})
			},
		},
		{name: "invalid connector", mutate: func(request *protocolv1.ConnectorAuthRequest) { request.ConnectorId = "con_invalid" }},
		{name: "missing hostname", mutate: func(request *protocolv1.ConnectorAuthRequest) { request.Hostname = "" }},
		{name: "invalid version range", mutate: func(request *protocolv1.ConnectorAuthRequest) { request.MinProtocol = 2; request.MaxProtocol = 1 }},
		{name: "duplicate capability", mutate: func(request *protocolv1.ConnectorAuthRequest) { request.Capabilities = []string{"tcp", "tcp"} }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest(testConnectorID)
			test.mutate(request)
			handler := testHandler(t, serverruntime.NewRegistry(), successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x21}, 64)))
			response, outcome := exchange(t, handler, request)
			assertFailure(t, response, outcome.err, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, 0)
		})
	}
}

func TestHandleConnectionTokenByteLimit(t *testing.T) {
	t.Run("8192 bytes reaches verifier", func(t *testing.T) {
		request := validRequest(testConnectorID)
		request.ConnectionToken = strings.Repeat("x", maxConnectionTokenBytes)
		verifierCalled := false
		verifier := verifierFunc(func(_ context.Context, token string) (application.VerifiedConnectionToken, error) {
			verifierCalled = true
			if len(token) != maxConnectionTokenBytes {
				t.Fatalf("Verify() token bytes = %d, want %d", len(token), maxConnectionTokenBytes)
			}
			return application.VerifiedConnectionToken{
				TunnelID: testTunnelID, TokenID: "tok_01J00000000000000000000000", TokenVersion: 1,
			}, nil
		})
		handler := testHandler(t, serverruntime.NewRegistry(), verifier, bytes.NewReader(bytes.Repeat([]byte{0x22}, 64)))
		response, outcome := exchange(t, handler, request)
		if outcome.err != nil || response.GetSuccess() == nil || !verifierCalled {
			t.Fatalf("Handle() response=%+v error=%v verifier_called=%t", response, outcome.err, verifierCalled)
		}
	})

	t.Run("8193 bytes is rejected before verifier", func(t *testing.T) {
		request := validRequest(testConnectorID)
		request.ConnectionToken = strings.Repeat("x", maxConnectionTokenBytes+1)
		verifierCalled := false
		verifier := verifierFunc(func(context.Context, string) (application.VerifiedConnectionToken, error) {
			verifierCalled = true
			return application.VerifiedConnectionToken{}, nil
		})
		handler := testHandler(t, serverruntime.NewRegistry(), verifier, bytes.NewReader(bytes.Repeat([]byte{0x23}, 64)))
		response, outcome := exchange(t, handler, request)
		assertFailure(t, response, outcome.err, protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, 0)
		if verifierCalled {
			t.Fatal("oversized token reached verifier")
		}
	})
}

func TestValidateRequestRejectsInvalidUTF8ConnectionToken(t *testing.T) {
	request := validRequest(testConnectorID)
	request.ConnectionToken = string([]byte{0xff})
	if err := validateRequest(request); err == nil {
		t.Fatal("validateRequest() accepted invalid UTF-8 connection_token")
	}
}

func TestHandleAuthenticationFailureMappingAndRetryRules(t *testing.T) {
	tests := []struct {
		name      string
		verifyErr error
		wantCode  protocolv1.ErrorCode
		wantRetry uint32
	}{
		{name: "token invalid", verifyErr: application.ErrConnectionTokenInvalid, wantCode: protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID},
		{name: "identity mismatch", verifyErr: application.ErrConnectionTokenIdentityMismatch, wantCode: protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID},
		{name: "secret mismatch", verifyErr: application.ErrConnectionTokenSecretMismatch, wantCode: protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID},
		{name: "token revoked", verifyErr: application.ErrConnectionTokenInactive, wantCode: protocolv1.ErrorCode_ERROR_CODE_TOKEN_REVOKED},
		{name: "tunnel revoked", verifyErr: application.ErrConnectionTokenTunnelRevoked, wantCode: protocolv1.ErrorCode_ERROR_CODE_TUNNEL_REVOKED},
		{name: "capacity", verifyErr: ErrAuthenticationCapacity, wantCode: protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED, wantRetry: 1_500},
		{name: "repository unavailable", verifyErr: errors.New("repository unavailable"), wantCode: protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, wantRetry: 1_500},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verifier := verifierFunc(func(context.Context, string) (application.VerifiedConnectionToken, error) {
				return application.VerifiedConnectionToken{}, test.verifyErr
			})
			handler := testHandler(t, serverruntime.NewRegistry(), verifier, bytes.NewReader(bytes.Repeat([]byte{0x31}, 64)))
			response, outcome := exchange(t, handler, validRequest(testConnectorID))
			assertFailure(t, response, outcome.err, test.wantCode, test.wantRetry)
			if strings.Contains(outcome.err.Error(), testToken) || strings.Contains(outcome.err.Error(), "repository unavailable") {
				t.Fatalf("HandleError leaked a Credential or underlying detail: %v", outcome.err)
			}
		})
	}
}

func TestHandleRejectsUnsupportedVersionAfterValidToken(t *testing.T) {
	request := validRequest(testConnectorID)
	request.MinProtocol = 2
	request.MaxProtocol = 3
	handler := testHandler(t, serverruntime.NewRegistry(), successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x41}, 64)))
	response, outcome := exchange(t, handler, request)
	assertFailure(t, response, outcome.err, protocolv1.ErrorCode_ERROR_CODE_VERSION_UNSUPPORTED, 0)
}

func TestHandleRejectsRuntimeRevokedTunnel(t *testing.T) {
	registry := serverruntime.NewRegistry()
	if err := registry.RevokeTunnel(testTunnelID); err != nil {
		t.Fatalf("RevokeTunnel() error = %v", err)
	}
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x45}, 64)))
	response, outcome := exchange(t, handler, validRequest(testConnectorID))
	assertFailure(t, response, outcome.err, protocolv1.ErrorCode_ERROR_CODE_TUNNEL_REVOKED, 0)
}

func TestHandleCSPRNGFailureReturnsRetryableInternalError(t *testing.T) {
	handler := testHandler(t, serverruntime.NewRegistry(), successfulVerifier(), errorReader{err: errTestRandom})
	response, outcome := exchange(t, handler, validRequest(testConnectorID))
	assertFailure(t, response, outcome.err, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, 1_500)
	if !errors.Is(outcome.err, errTestRandom) {
		t.Fatalf("Handle() error = %v, want wrapped CSPRNG error", outcome.err)
	}
	if strings.Contains(outcome.err.Error(), errTestRandom.Error()) {
		t.Fatalf("HandleError leaked random source detail: %v", outcome.err)
	}
}

func TestHandleReconnectGenerationAndMultipleConnectors(t *testing.T) {
	registry := serverruntime.NewRegistry()
	handler := testHandler(t, registry, successfulVerifier(), bytes.NewReader(bytes.Repeat([]byte{0x51}, 256)))

	_, first := exchange(t, handler, validRequest(testConnectorID))
	_, reconnected := exchange(t, handler, validRequest(testConnectorID))
	_, other := exchange(t, handler, validRequest(testConnectorIDTwo))
	if first.err != nil || reconnected.err != nil || other.err != nil {
		t.Fatalf("Handle() errors = %v, %v, %v", first.err, reconnected.err, other.err)
	}
	if reconnected.established.Session.Generation != first.established.Session.Generation+1 {
		t.Fatalf("same Connector generations = %d, %d, want increment", first.established.Session.Generation, reconnected.established.Session.Generation)
	}
	if other.established.Session.Generation != 1 {
		t.Fatalf("other Connector generation = %d, want independent generation 1", other.established.Session.Generation)
	}
	currentFirst, foundFirst := registry.Current(testTunnelID, testConnectorID)
	currentSecond, foundSecond := registry.Current(testTunnelID, testConnectorIDTwo)
	if !foundFirst || !foundSecond || currentFirst != reconnected.established.Session || currentSecond != other.established.Session {
		t.Fatalf("Current() = (%#v,%v), (%#v,%v), want both Connectors", currentFirst, foundFirst, currentSecond, foundSecond)
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	valid := testOptions()
	tests := []struct {
		name   string
		mutate func(*Options)
	}{
		{name: "read timeout", mutate: func(options *Options) { options.ReadTimeout = 0 }},
		{name: "write timeout", mutate: func(options *Options) { options.WriteTimeout = 0 }},
		{name: "auth frame above protocol", mutate: func(options *Options) { options.MaxFrameBytes = frame.MaxAuthFrameSize + 1 }},
		{name: "heartbeat sub millisecond", mutate: func(options *Options) { options.HeartbeatInterval = time.Microsecond }},
		{name: "retry negative", mutate: func(options *Options) { options.RetryAfter = -time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			if _, err := New(successfulVerifier(), serverruntime.NewRegistry(), options); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("New() error = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func successfulVerifier() TokenVerifier {
	return verifierFunc(func(_ context.Context, token string) (application.VerifiedConnectionToken, error) {
		if token != testToken {
			return application.VerifiedConnectionToken{}, application.ErrConnectionTokenInvalid
		}
		return application.VerifiedConnectionToken{TunnelID: testTunnelID, TokenID: "tok_01J00000000000000000000000", TokenVersion: 1, DesiredRevision: 7}, nil
	})
}

func validRequest(connectorID string) *protocolv1.ConnectorAuthRequest {
	return &protocolv1.ConnectorAuthRequest{
		ConnectionToken: testToken, ConnectorId: connectorID, Hostname: "connector.example.test",
		Version: "v0.1.0", Os: "linux", Arch: "amd64", MinProtocol: 1, MaxProtocol: 1,
		Capabilities: []string{"tcp"},
	}
}

func testHandler(t *testing.T, registry *serverruntime.Registry, verifier TokenVerifier, random io.Reader) *Handler {
	t.Helper()
	handler, err := newHandler(verifier, registry, testOptions(), random, time.Now)
	if err != nil {
		t.Fatalf("newHandler() error = %v", err)
	}
	return handler
}

func testOptions() Options {
	return Options{
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		HeartbeatInterval: 10 * time.Second, RetryAfter: 1500 * time.Millisecond,
	}
}

func exchange(t *testing.T, handler *Handler, request *protocolv1.ConnectorAuthRequest) (*protocolv1.ConnectorAuthResult, handleOutcome) {
	t.Helper()
	server, client := net.Pipe()
	outcomes := make(chan handleOutcome, 1)
	go func() {
		established, err := handler.Handle(context.Background(), server)
		outcomes <- handleOutcome{established: established, err: err}
	}()
	setDeadline(t, client)
	if err := frame.WriteAuth(client, request); err != nil {
		t.Fatalf("WriteAuth(request) error = %v", err)
	}
	response := &protocolv1.ConnectorAuthResult{}
	if err := frame.ReadAuth(client, response); err != nil {
		t.Fatalf("ReadAuth(response) error = %v", err)
	}
	outcome := <-outcomes
	if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("server Close() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("client Close() error = %v", err)
	}
	return response, outcome
}

func assertFailure(t *testing.T, response *protocolv1.ConnectorAuthResult, err error, code protocolv1.ErrorCode, retryAfter uint32) {
	t.Helper()
	failure := response.GetFailure()
	if failure == nil || failure.GetErrorCode() != code || failure.GetRetryAfterMs() != retryAfter {
		t.Fatalf("ConnectorAuthFailure = %#v, want code=%s retry_after_ms=%d", failure, code, retryAfter)
	}
	var handleErr *HandleError
	if !errors.As(err, &handleErr) || !handleErr.FailureSent() || handleErr.Code() != code {
		t.Fatalf("Handle() error = %#v, want sent HandleError code=%s", err, code)
	}
}

func setDeadline(t *testing.T, connection net.Conn) {
	t.Helper()
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}
}

func frameLength(length uint64) []byte {
	var encoded [binary.MaxVarintLen64]byte
	size := binary.PutUvarint(encoded[:], length)
	return append([]byte(nil), encoded[:size]...)
}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

type failingWriteConn struct {
	reader           io.Reader
	writeDeadlineErr error
	writeErr         error
	mu               sync.Mutex
	closed           bool
}

type blockingWriteConn struct {
	reader       io.Reader
	writeStarted chan struct{}
	allowWrite   chan struct{}
	startOnce    sync.Once
	mu           sync.Mutex
	written      bytes.Buffer
	closed       bool
}

type deadlineObservedConn struct {
	net.Conn
	readOnce         sync.Once
	writeOnce        sync.Once
	readDeadlineSet  chan struct{}
	writeDeadlineSet chan struct{}
}

func newDeadlineObservedConn(connection net.Conn) *deadlineObservedConn {
	return &deadlineObservedConn{
		Conn: connection, readDeadlineSet: make(chan struct{}), writeDeadlineSet: make(chan struct{}),
	}
}

func (connection *deadlineObservedConn) SetReadDeadline(deadline time.Time) error {
	connection.readOnce.Do(func() { close(connection.readDeadlineSet) })
	return connection.Conn.SetReadDeadline(deadline)
}

func (connection *deadlineObservedConn) SetWriteDeadline(deadline time.Time) error {
	connection.writeOnce.Do(func() { close(connection.writeDeadlineSet) })
	return connection.Conn.SetWriteDeadline(deadline)
}

func newBlockingWriteConn(reader io.Reader) *blockingWriteConn {
	return &blockingWriteConn{reader: reader, writeStarted: make(chan struct{}), allowWrite: make(chan struct{})}
}

func (connection *blockingWriteConn) Read(buffer []byte) (int, error) {
	return connection.reader.Read(buffer)
}

func (connection *blockingWriteConn) Write(buffer []byte) (int, error) {
	connection.startOnce.Do(func() {
		close(connection.writeStarted)
		<-connection.allowWrite
	})
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.written.Write(buffer)
}

func (connection *blockingWriteConn) Close() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.closed = true
	return nil
}

func (connection *blockingWriteConn) LocalAddr() net.Addr              { return testAddr("local") }
func (connection *blockingWriteConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (connection *blockingWriteConn) SetDeadline(time.Time) error      { return nil }
func (connection *blockingWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (connection *blockingWriteConn) SetWriteDeadline(time.Time) error { return nil }

func (connection *blockingWriteConn) writtenBytes() []byte {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return append([]byte(nil), connection.written.Bytes()...)
}

var (
	errSetAuthWriteDeadline = errors.New("set auth write deadline failed")
	errClearAuthDeadline    = errors.New("clear auth deadline failed")
)

type clearDeadlineFailConn struct {
	net.Conn
	onClear func()
	mu      sync.Mutex
	closed  bool
}

func (connection *clearDeadlineFailConn) SetDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		if connection.onClear != nil {
			connection.onClear()
		}
		return errClearAuthDeadline
	}
	return connection.Conn.SetDeadline(deadline)
}

func (connection *clearDeadlineFailConn) Close() error {
	connection.mu.Lock()
	connection.closed = true
	connection.mu.Unlock()
	return connection.Conn.Close()
}

func (connection *clearDeadlineFailConn) isClosed() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.closed
}

func (connection *failingWriteConn) Read(buffer []byte) (int, error) {
	return connection.reader.Read(buffer)
}

func (connection *failingWriteConn) Write([]byte) (int, error) {
	return 0, connection.writeErr
}

func (connection *failingWriteConn) Close() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.closed = true
	return nil
}

func (connection *failingWriteConn) LocalAddr() net.Addr  { return testAddr("local") }
func (connection *failingWriteConn) RemoteAddr() net.Addr { return testAddr("remote") }
func (connection *failingWriteConn) SetDeadline(time.Time) error {
	return nil
}
func (connection *failingWriteConn) SetReadDeadline(time.Time) error {
	return nil
}
func (connection *failingWriteConn) SetWriteDeadline(time.Time) error {
	return connection.writeDeadlineErr
}

func (connection *failingWriteConn) isClosed() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return connection.closed
}

type testAddr string

func (address testAddr) Network() string { return "test" }
func (address testAddr) String() string  { return string(address) }
