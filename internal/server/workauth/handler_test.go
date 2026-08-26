package workauth

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
)

const handlerTestTimeout = time.Second

func TestHandlerSuccessFlushesReadyAndReturnsIdleConnection(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	handler := newTestHandler(t, ResolverFunc(func(sessionID string) (*SessionAuthenticator, bool) {
		if sessionID != serverSessionID {
			return nil, false
		}
		return authenticator, true
	}), 80*time.Millisecond, 80*time.Millisecond)
	serverConnection, agentConnection := net.Pipe()
	defer agentConnection.Close()

	result := make(chan handleResult, 1)
	go func() {
		idle, err := handler.Handle(context.Background(), serverConnection)
		result <- handleResult{idle: idle, err: err}
	}()
	hello := goldenHello(t)
	if err := frame.WriteWork(agentConnection, hello); err != nil {
		t.Fatalf("WriteWork(hello) error = %v", err)
	}
	ready := &protocolv1.WorkReady{}
	if err := frame.ReadWork(agentConnection, ready); err != nil {
		t.Fatalf("ReadWork(ready) error = %v", err)
	}
	got := receiveHandleResult(t, result)
	if got.err != nil {
		t.Fatalf("Handle() error = %v", got.err)
	}
	if ready.GetWorkId() != serverWorkID || ready.GetStatus() != protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY ||
		ready.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("WorkReady = %#v", ready)
	}
	if got.idle.TunnelID != serverTunnelID || got.idle.ConnectorID != serverConnectorID ||
		got.idle.SessionID != serverSessionID || got.idle.WorkID != serverWorkID ||
		got.idle.State == nil || got.idle.State.Phase() != state.WorkIdle {
		t.Fatalf("Idle = %#v", got.idle)
	}

	// 等待超过认证 IO Deadline 后仍能传输字节，证明成功交接前已清理临时 Deadline。
	time.Sleep(120 * time.Millisecond)
	if err := agentConnection.SetWriteDeadline(time.Now().Add(handlerTestTimeout)); err != nil {
		t.Fatal(err)
	}
	writeResult := make(chan error, 1)
	go func() {
		_, err := agentConnection.Write([]byte{0x7a})
		writeResult <- err
	}()
	var one [1]byte
	if _, err := io.ReadFull(serverConnection, one[:]); err != nil {
		t.Fatalf("post-auth read error = %v", err)
	}
	if err := <-writeResult; err != nil || one[0] != 0x7a {
		t.Fatalf("post-auth write error = %v byte=%x", err, one[0])
	}
	_ = serverConnection.Close()
}

func TestHandlerUnknownSessionAndBadMACHaveSamePublicResult(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 2, time.Minute); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		resolver Resolver
		hello    func(*testing.T) *protocolv1.WorkHello
	}{
		{
			name: "unknown session",
			resolver: ResolverFunc(func(string) (*SessionAuthenticator, bool) {
				return nil, false
			}),
			hello: goldenHello,
		},
		{
			name: "bad MAC",
			resolver: ResolverFunc(func(string) (*SessionAuthenticator, bool) {
				return authenticator, true
			}),
			hello: func(t *testing.T) *protocolv1.WorkHello {
				hello := goldenHello(t)
				hello.Mac = filledBytes(0xff, sessionSecretSize)
				return hello
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t, test.resolver, handlerTestTimeout, handlerTestTimeout)
			ready, handleErr := exchangeWork(t, handler, test.hello(t))
			if ready.GetWorkId() != serverWorkID || ready.GetStatus() != protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED ||
				ready.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID {
				t.Fatalf("WorkReady = %#v", ready)
			}
			assertHandleError(t, handleErr, protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID, true)
			var decision *DecisionError
			if !errors.As(handleErr, &decision) || decision.Reason != ReasonSessionInvalid {
				t.Fatalf("Handle() error = %v, want session-invalid decision", handleErr)
			}
		})
	}
}

func TestHandlerMapsBudgetRejectionAndDoesNotConsumeOnFailure(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	handler := newTestHandler(t, ResolverFunc(func(string) (*SessionAuthenticator, bool) {
		return authenticator, true
	}), handlerTestTimeout, handlerTestTimeout)
	if ready, err := exchangeWork(t, handler, goldenHello(t)); err != nil ||
		ready.GetStatus() != protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY {
		t.Fatalf("first exchange ready=%#v err=%v", ready, err)
	}
	second := signedHello(t, serverOtherWorkID, serverLeaseID, 0x43, 0x11)
	ready, err := exchangeWork(t, handler, second)
	if ready.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED {
		t.Fatalf("budget WorkReady = %#v", ready)
	}
	assertHandleError(t, err, protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED, true)
	if authenticator.leases[serverLeaseID].remaining != 0 || len(authenticator.replayIndex) != 1 {
		t.Fatal("budget rejection mutated slot or replay state")
	}
}

func TestHandlerClosesUnsafeFramesWithoutWorkReadyOrResolverLookup(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	var resolverCalls atomic.Int32
	resolver := ResolverFunc(func(string) (*SessionAuthenticator, bool) {
		resolverCalls.Add(1)
		return authenticator, true
	})
	tests := []struct {
		name      string
		write     func(*testing.T, net.Conn)
		wantCode  protocolv1.ErrorCode
		wantCause error
	}{
		{
			name: "unknown field",
			write: func(t *testing.T, connection net.Conn) {
				hello := goldenHello(t)
				hello.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
				if err := frame.WriteWork(connection, hello); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR,
		},
		{
			name: "invalid identifier",
			write: func(t *testing.T, connection net.Conn) {
				hello := goldenHello(t)
				hello.SessionId = "sess_invalid"
				if err := frame.WriteWork(connection, hello); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR,
		},
		{
			name: "malformed protobuf",
			write: func(t *testing.T, connection net.Conn) {
				if err := frame.WritePayload(connection, []byte{0x0f}, frame.MaxWorkFrameSize); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: protocolv1.ErrorCode_ERROR_CODE_OK, wantCause: frame.ErrMalformedMessage,
		},
		{
			name: "oversize",
			write: func(t *testing.T, connection net.Conn) {
				var prefix [binary.MaxVarintLen64]byte
				size := binary.PutUvarint(prefix[:], frame.MaxWorkFrameSize+1)
				if _, err := connection.Write(prefix[:size]); err != nil {
					t.Fatal(err)
				}
			},
			wantCode: protocolv1.ErrorCode_ERROR_CODE_OK, wantCause: frame.ErrFrameTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := resolverCalls.Load()
			handler := newTestHandler(t, resolver, handlerTestTimeout, handlerTestTimeout)
			serverConnection, agentConnection := net.Pipe()
			result := make(chan handleResult, 1)
			go func() {
				idle, err := handler.Handle(context.Background(), serverConnection)
				result <- handleResult{idle: idle, err: err}
			}()
			test.write(t, agentConnection)
			got := receiveHandleResult(t, result)
			assertHandleError(t, got.err, test.wantCode, false)
			if test.wantCause != nil && !errors.Is(got.err, test.wantCause) {
				t.Fatalf("Handle() error = %v, want cause %v", got.err, test.wantCause)
			}
			if resolverCalls.Load() != before {
				t.Fatal("unsafe WorkHello reached resolver")
			}
			if err := frame.ReadWork(agentConnection, &protocolv1.WorkReady{}); !errors.Is(err, io.EOF) {
				t.Fatalf("ReadWork() error = %v, want EOF without response", err)
			}
			_ = agentConnection.Close()
		})
	}
}

func TestHandlerReadTimeoutAndContextCancellation(t *testing.T) {
	resolver := ResolverFunc(func(string) (*SessionAuthenticator, bool) { return nil, false })
	tests := []struct {
		name      string
		readLimit time.Duration
		cancel    bool
		want      error
	}{
		{name: "timeout", readLimit: 35 * time.Millisecond, want: ErrReadTimeout},
		{name: "context cancellation", readLimit: handlerTestTimeout, cancel: true, want: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t, resolver, test.readLimit, handlerTestTimeout)
			serverConnection, agentConnection := net.Pipe()
			defer agentConnection.Close()
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan handleResult, 1)
			go func() {
				idle, err := handler.Handle(ctx, serverConnection)
				result <- handleResult{idle: idle, err: err}
			}()
			if test.cancel {
				time.Sleep(10 * time.Millisecond)
				cancel()
			} else {
				defer cancel()
			}
			got := receiveHandleResult(t, result)
			if !errors.Is(got.err, test.want) {
				t.Fatalf("Handle() error = %v, want %v", got.err, test.want)
			}
			assertHandleError(t, got.err, protocolv1.ErrorCode_ERROR_CODE_OK, false)
		})
	}
}

func TestHandlerWriteTimeoutAndCancellationKeepReplayConsumed(t *testing.T) {
	tests := []struct {
		name       string
		writeLimit time.Duration
		cancel     bool
		want       error
	}{
		{name: "timeout", writeLimit: 35 * time.Millisecond, want: ErrWriteTimeout},
		{name: "context cancellation", writeLimit: handlerTestTimeout, cancel: true, want: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator, _ := newTestAuthenticator(t, 8)
			if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
				t.Fatal(err)
			}
			handler := newTestHandler(t, ResolverFunc(func(string) (*SessionAuthenticator, bool) {
				return authenticator, true
			}), handlerTestTimeout, test.writeLimit)
			serverConnection, agentConnection := net.Pipe()
			defer agentConnection.Close()
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan handleResult, 1)
			go func() {
				idle, err := handler.Handle(ctx, serverConnection)
				result <- handleResult{idle: idle, err: err}
			}()
			if err := frame.WriteWork(agentConnection, goldenHello(t)); err != nil {
				t.Fatal(err)
			}
			if test.cancel {
				time.Sleep(10 * time.Millisecond)
				cancel()
			} else {
				defer cancel()
			}
			got := receiveHandleResult(t, result)
			if !errors.Is(got.err, test.want) {
				t.Fatalf("Handle() error = %v, want %v", got.err, test.want)
			}
			assertHandleError(t, got.err, protocolv1.ErrorCode_ERROR_CODE_OK, false)
			if authenticator.leases[serverLeaseID].remaining != 0 || len(authenticator.replayIndex) != 1 {
				t.Fatal("failed READY write rolled back an authenticated WorkHello")
			}
		})
	}
}

func TestHandlerClosesWhenSuccessDeadlineCleanupFails(t *testing.T) {
	authenticator, _ := newTestAuthenticator(t, 8)
	if err := authenticator.GrantLease(serverLeaseID, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	handler := newTestHandler(t, ResolverFunc(func(string) (*SessionAuthenticator, bool) {
		return authenticator, true
	}), handlerTestTimeout, handlerTestTimeout)
	serverPipe, agentConnection := net.Pipe()
	serverConnection := &clearDeadlineFailureConn{Conn: serverPipe}
	defer agentConnection.Close()
	result := make(chan handleResult, 1)
	go func() {
		idle, err := handler.Handle(context.Background(), serverConnection)
		result <- handleResult{idle: idle, err: err}
	}()
	if err := frame.WriteWork(agentConnection, goldenHello(t)); err != nil {
		t.Fatal(err)
	}
	ready := &protocolv1.WorkReady{}
	if err := frame.ReadWork(agentConnection, ready); err != nil {
		t.Fatal(err)
	}
	if ready.GetStatus() != protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY {
		t.Fatalf("WorkReady = %#v", ready)
	}
	got := receiveHandleResult(t, result)
	assertHandleError(t, got.err, protocolv1.ErrorCode_ERROR_CODE_OK, true)
	if got.idle.State != nil {
		t.Fatal("cleanup failure returned an IDLE handoff")
	}
}

func TestNewHandlerAndHandleValidateInputs(t *testing.T) {
	validResolver := ResolverFunc(func(string) (*SessionAuthenticator, bool) { return nil, false })
	for _, test := range []struct {
		name     string
		resolver Resolver
		options  HandlerOptions
	}{
		{name: "nil resolver", options: HandlerOptions{ReadTimeout: time.Second, WriteTimeout: time.Second}},
		{name: "nil resolver function", resolver: ResolverFunc(nil), options: HandlerOptions{ReadTimeout: time.Second, WriteTimeout: time.Second}},
		{name: "zero read", resolver: validResolver, options: HandlerOptions{WriteTimeout: time.Second}},
		{name: "zero write", resolver: validResolver, options: HandlerOptions{ReadTimeout: time.Second}},
		{name: "work frame above protocol", resolver: validResolver, options: HandlerOptions{
			ReadTimeout: time.Second, WriteTimeout: time.Second, MaxFrameBytes: frame.MaxWorkFrameSize + 1,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewHandler(test.resolver, test.options); !errors.Is(err, ErrInvalidHandlerOptions) {
				t.Fatalf("NewHandler() error = %v", err)
			}
		})
	}
	handler := newTestHandler(t, validResolver, time.Second, time.Second)
	if _, err := handler.Handle(nil, nil); !errors.Is(err, ErrInvalidHandlerOptions) {
		t.Fatalf("Handle() error = %v", err)
	}
}

type handleResult struct {
	idle Idle
	err  error
}

func newTestHandler(t *testing.T, resolver Resolver, readTimeout, writeTimeout time.Duration) *Handler {
	t.Helper()
	handler, err := NewHandler(resolver, HandlerOptions{ReadTimeout: readTimeout, WriteTimeout: writeTimeout})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func exchangeWork(t *testing.T, handler *Handler, hello *protocolv1.WorkHello) (*protocolv1.WorkReady, error) {
	t.Helper()
	serverConnection, agentConnection := net.Pipe()
	defer agentConnection.Close()
	result := make(chan handleResult, 1)
	go func() {
		idle, err := handler.Handle(context.Background(), serverConnection)
		result <- handleResult{idle: idle, err: err}
	}()
	if err := frame.WriteWork(agentConnection, hello); err != nil {
		t.Fatalf("WriteWork(hello) error = %v", err)
	}
	ready := &protocolv1.WorkReady{}
	if err := frame.ReadWork(agentConnection, ready); err != nil {
		t.Fatalf("ReadWork(ready) error = %v", err)
	}
	got := receiveHandleResult(t, result)
	if got.err == nil {
		_ = serverConnection.Close()
	} else {
		var one [1]byte
		if _, readErr := agentConnection.Read(one[:]); !errors.Is(readErr, io.EOF) {
			t.Fatalf("rejected connection read error = %v, want EOF", readErr)
		}
	}
	return ready, got.err
}

func receiveHandleResult(t *testing.T, result <-chan handleResult) handleResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * handlerTestTimeout):
		t.Fatal("Handle() did not return")
		return handleResult{}
	}
}

func assertHandleError(t *testing.T, err error, code protocolv1.ErrorCode, responseSent bool) {
	t.Helper()
	var handleErr *HandleError
	if !errors.As(err, &handleErr) {
		t.Fatalf("error = %v, want *HandleError", err)
	}
	if handleErr.Code() != code || handleErr.ResponseSent() != responseSent {
		t.Fatalf("HandleError code=%s responseSent=%t, want code=%s responseSent=%t", handleErr.Code(), handleErr.ResponseSent(), code, responseSent)
	}
}

type clearDeadlineFailureConn struct {
	net.Conn
}

func (connection *clearDeadlineFailureConn) SetDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return errors.New("test clear deadline failure")
	}
	return connection.Conn.SetDeadline(deadline)
}
