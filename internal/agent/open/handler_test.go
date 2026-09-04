package open

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/workauth"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	internaltracing "github.com/lifei6671/xtunnel/internal/tracing"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	testWorkID       = "work_01J00000000000000000000000"
	testConnectionID = "conn_01J00000000000000000000000"
	testServiceID    = "svc_01J00000000000000000000000"
	testTraceID      = "0102030405060708090a0b0c0d0e0f10"
	testTraceparent  = "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01"
	testTracestate   = "vendor=value"
	testTimeout      = 2 * time.Second
)

func TestHandlePreservesRawBytesFollowingOpenRequestFrameAfterIdle(t *testing.T) {
	agentConnection, serverConnection := net.Pipe()
	originConnection, originPeer := net.Pipe()
	defer serverConnection.Close()
	defer originPeer.Close()
	rawPayload := []byte("raw-data-immediately-after-open")
	rawReceived := make(chan []byte, 1)
	handler := newTestHandler(t, OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
		return originConnection, protocolv1.ErrorCode_ERROR_CODE_OK, nil
	}), func(_ context.Context, work, origin net.Conn) error {
		if origin != originConnection {
			return errors.New("unexpected origin connection")
		}
		buffer := make([]byte, len(rawPayload))
		if _, err := io.ReadFull(work, buffer); err != nil {
			return err
		}
		rawReceived <- buffer
		return nil
	})
	if err := agentConnection.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	handler.options.ReadTimeout = 60 * time.Millisecond
	reader := &firstReadConnection{Conn: agentConnection, started: make(chan struct{})}
	started := reader.started
	ready := idleReady(t)
	result := make(chan error, 1)
	transitions := make([]state.WorkPhase, 0, 2)
	go func() {
		result <- handler.HandleObserved(
			context.Background(), reader, ready,
			func(target state.WorkPhase, commit func() error) error {
				if err := commit(); err != nil {
					return err
				}
				transitions = append(transitions, target)
				return nil
			},
		)
	}()

	select {
	case <-started:
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not start waiting for OPEN")
	}
	select {
	case err := <-result:
		t.Fatalf("IDLE ended before OPEN: %v", err)
	case <-time.After(3 * handler.options.ReadTimeout):
	}
	if err := serverConnection.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}

	request := validOpenRequest()
	var encoded bytes.Buffer
	if err := frame.WriteWork(&encoded, request); err != nil {
		t.Fatalf("encode OpenRequest: %v", err)
	}
	encoded.Write(rawPayload)
	writeDone := make(chan error, 1)
	go func() {
		_, err := serverConnection.Write(encoded.Bytes())
		writeDone <- err
	}()
	response := &protocolv1.OpenResponse{}
	if err := frame.ReadWork(serverConnection, response); err != nil {
		t.Fatalf("read OpenResponse: %v", err)
	}
	if response.GetStatus() != protocolv1.OpenStatus_OPEN_STATUS_OK || response.GetConnectionId() != testConnectionID {
		t.Fatalf("OpenResponse = %#v, want OPEN_OK", response)
	}
	select {
	case got := <-rawReceived:
		if !bytes.Equal(got, rawPayload) {
			t.Fatalf("RAW = %q, want %q", got, rawPayload)
		}
	case <-time.After(testTimeout):
		t.Fatal("RAW bytes were not handed off after OpenRequest")
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write combined OpenRequest+RAW: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Handle() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not finish after RAW proxy returned")
	}
	if len(transitions) != 2 || transitions[0] != state.WorkOpening || transitions[1] != state.WorkActive {
		t.Fatalf("observed transitions = %v, want [OPENING ACTIVE]", transitions)
	}
}

func TestHandleReturnsExplicitOriginFailure(t *testing.T) {
	agentConnection, serverConnection := net.Pipe()
	defer serverConnection.Close()
	handler := newTestHandler(t, OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
		return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED, errors.New("fixture refused")
	}), nil)
	ready := idleReady(t)
	result := make(chan error, 1)
	go func() { result <- handler.Handle(context.Background(), agentConnection, ready) }()

	if err := frame.WriteWork(serverConnection, validOpenRequest()); err != nil {
		t.Fatalf("write OpenRequest: %v", err)
	}
	response := &protocolv1.OpenResponse{}
	if err := frame.ReadWork(serverConnection, response); err != nil {
		t.Fatalf("read failure OpenResponse: %v", err)
	}
	if response.GetStatus() != protocolv1.OpenStatus_OPEN_STATUS_ERROR ||
		response.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED {
		t.Fatalf("failure OpenResponse = %#v", response)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrOrigin) {
			t.Fatalf("Handle() error = %v, want ErrOrigin", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not close after origin failure")
	}
	if ready.State.Phase() != state.WorkClosed {
		t.Fatalf("Work phase = %v, want closed", ready.State.Phase())
	}
}

func TestHandleLogsOriginFailureWithValidatedCorrelation(t *testing.T) {
	const secret = "origin-dial-secret-must-not-be-logged"
	var output bytes.Buffer
	logger, err := logging.New(&output, logging.Options{
		Level: logging.LevelDebug, Format: "json", Component: "agent",
	})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	handler, err := NewHandler(Options{
		ReadTimeout: time.Second, WriteTimeout: time.Second, Logger: logger,
		Dialer: OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
			return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED, errors.New(secret)
		}),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	agentConnection, serverConnection := net.Pipe()
	defer serverConnection.Close()
	ready := idleReady(t)
	result := make(chan error, 1)
	go func() { result <- handler.Handle(context.Background(), agentConnection, ready) }()

	request := validOpenRequest()
	request.TraceId = testTraceID
	request.Traceparent = testTraceparent
	request.Tracestate = testTracestate
	if err := frame.WriteWork(serverConnection, request); err != nil {
		t.Fatalf("write OpenRequest: %v", err)
	}
	if err := frame.ReadWork(serverConnection, &protocolv1.OpenResponse{}); err != nil {
		t.Fatalf("read OpenResponse: %v", err)
	}
	if err := <-result; !errors.Is(err, ErrOrigin) {
		t.Fatalf("Handle() error = %v, want ErrOrigin", err)
	}

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("decode log %q: %v", output.String(), err)
	}
	if record[logging.EventKey] != logging.EventAgentOriginConnectionFailed ||
		record[logging.ErrorCodeKey] != "ORIGIN_REFUSED" ||
		record[logging.TraceIDKey] != request.TraceId ||
		record[logging.ServiceIDKey] != testServiceID ||
		record[logging.ConnectionIDKey] != testConnectionID {
		t.Fatalf("origin failure log = %#v", record)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("origin failure log leaked error detail: %q", output.String())
	}
}

func TestHandleRejectsUnknownOpenRequestWithoutDial(t *testing.T) {
	agentConnection, serverConnection := net.Pipe()
	defer serverConnection.Close()
	dialed := false
	handler := newTestHandler(t, OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
		dialed = true
		return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE, errors.New("must not run")
	}), nil)
	ready := idleReady(t)
	result := make(chan error, 1)
	go func() { result <- handler.Handle(context.Background(), agentConnection, ready) }()
	request := validOpenRequest()
	request.ProtoReflect().SetUnknown(protowire.AppendTag(nil, 100, protowire.VarintType))
	if err := frame.WriteWork(serverConnection, request); err != nil {
		t.Fatalf("write unknown OpenRequest: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Handle() error = %v, want ErrProtocol", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not reject unknown fields")
	}
	if dialed {
		t.Fatal("Origin Dialer was called for unknown-field OpenRequest")
	}
}

func TestHandleRejectsInvalidTraceContextWithoutDial(t *testing.T) {
	tests := []struct {
		name           string
		traceID        string
		traceparent    string
		tracestate     string
		existingParent bool
	}{
		{name: "trace id without parent", traceID: testTraceID},
		{name: "parent without trace id", traceparent: testTraceparent},
		{name: "tracestate without parent", tracestate: testTracestate},
		{
			name: "malformed parent does not reuse caller parent", traceID: testTraceID,
			traceparent: "00-invalid-parent-01", existingParent: true,
		},
		{
			name:        "mismatched trace id",
			traceID:     "101112131415161718191a1b1c1d1e1f",
			traceparent: testTraceparent,
		},
		{name: "invalid tracestate", traceID: testTraceID, traceparent: testTraceparent, tracestate: "bad state"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agentConnection, serverConnection := net.Pipe()
			defer serverConnection.Close()
			dialed := false
			handler := newTestHandler(t, OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
				dialed = true
				return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE, errors.New("must not run")
			}), nil)
			ready := idleReady(t)
			result := make(chan error, 1)
			handleContext := context.Background()
			if test.existingParent {
				traceID, err := trace.TraceIDFromHex(testTraceID)
				if err != nil {
					t.Fatalf("TraceIDFromHex() error = %v", err)
				}
				spanID, err := trace.SpanIDFromHex("1112131415161718")
				if err != nil {
					t.Fatalf("SpanIDFromHex() error = %v", err)
				}
				handleContext = trace.ContextWithRemoteSpanContext(handleContext, trace.NewSpanContext(trace.SpanContextConfig{
					TraceID: traceID, SpanID: spanID, Remote: true,
				}))
			}
			go func() { result <- handler.Handle(handleContext, agentConnection, ready) }()

			request := validOpenRequest()
			request.TraceId = test.traceID
			request.Traceparent = test.traceparent
			request.Tracestate = test.tracestate
			if err := frame.WriteWork(serverConnection, request); err != nil {
				t.Fatalf("write OpenRequest: %v", err)
			}
			select {
			case err := <-result:
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("Handle() error = %v, want ErrProtocol", err)
				}
			case <-time.After(testTimeout):
				t.Fatal("Handle() did not reject invalid trace context")
			}
			if dialed {
				t.Fatal("Origin Dialer was called for invalid trace context")
			}
			if ready.State.Phase() != state.WorkClosed {
				t.Fatalf("Work phase = %v, want closed", ready.State.Phase())
			}
		})
	}
}

func TestHandleRestoresRemoteParentAndCreatesStrictSpanChain(t *testing.T) {
	type contextKey struct{}
	traceRuntime, exporter := newTestTraceRuntime(t)
	agentConnection, serverConnection := net.Pipe()
	originConnection, originPeer := net.Pipe()
	defer serverConnection.Close()
	defer originPeer.Close()
	dialContext := make(chan trace.SpanContext, 1)
	dialPreserved := make(chan bool, 1)
	proxyContext := make(chan trace.SpanContext, 1)
	proxyPhase := make(chan state.WorkPhase, 1)
	ready := idleReady(t)
	handler, err := NewHandler(Options{
		ReadTimeout: time.Second, WriteTimeout: time.Second,
		Dialer: OriginDialerFunc(func(ctx context.Context, _ string) (net.Conn, protocolv1.ErrorCode, error) {
			dialContext <- trace.SpanContextFromContext(ctx)
			_, hasDeadline := ctx.Deadline()
			dialPreserved <- hasDeadline && ctx.Value(contextKey{}) == "preserved"
			return originConnection, protocolv1.ErrorCode_ERROR_CODE_OK, nil
		}),
		Proxy: func(ctx context.Context, _, _ net.Conn) error {
			proxyContext <- trace.SpanContextFromContext(ctx)
			proxyPhase <- ready.State.Phase()
			return nil
		},
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Tracing: traceRuntime,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	result := make(chan error, 1)
	handleContext, cancelHandle := context.WithTimeout(
		context.WithValue(context.Background(), contextKey{}, "preserved"), testTimeout,
	)
	defer cancelHandle()
	go func() { result <- handler.Handle(handleContext, agentConnection, ready) }()

	if err := frame.WriteWork(serverConnection, validTracedOpenRequest()); err != nil {
		t.Fatalf("write OpenRequest: %v", err)
	}
	response := &protocolv1.OpenResponse{}
	if err := frame.ReadWork(serverConnection, response); err != nil {
		t.Fatalf("read OpenResponse: %v", err)
	}
	if response.GetStatus() != protocolv1.OpenStatus_OPEN_STATUS_OK {
		t.Fatalf("OpenResponse status = %s, want OPEN_STATUS_OK", response.GetStatus())
	}
	if err := <-result; err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	dialSpanContext := <-dialContext
	if preserved := <-dialPreserved; !preserved {
		t.Fatal("restored trace context discarded caller cancellation, deadline, or values")
	}
	proxySpanContext := <-proxyContext
	if phase := <-proxyPhase; phase != state.WorkActive {
		t.Fatalf("Proxy phase = %v, want ACTIVE", phase)
	}
	wantTraceID, err := trace.TraceIDFromHex(testTraceID)
	if err != nil {
		t.Fatalf("TraceIDFromHex() error = %v", err)
	}
	if dialSpanContext.TraceID() != wantTraceID || proxySpanContext.TraceID() != wantTraceID {
		t.Fatalf("Span TraceIDs = %s/%s, want %s", dialSpanContext.TraceID(), proxySpanContext.TraceID(), wantTraceID)
	}

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported spans = %d, want 2", len(spans))
	}
	var originSpan, proxySpan tracetest.SpanStub
	for _, span := range spans {
		switch span.Name {
		case "origin.Dial":
			originSpan = span
		case "proxy.Bidirectional":
			proxySpan = span
		}
	}
	wantRemoteSpanID, err := trace.SpanIDFromHex("1112131415161718")
	if err != nil {
		t.Fatalf("SpanIDFromHex() error = %v", err)
	}
	if originSpan.Name == "" || !originSpan.Parent.IsRemote() || !originSpan.Parent.IsSampled() ||
		originSpan.Parent.SpanID() != wantRemoteSpanID {
		t.Fatalf("origin parent = %#v, want remote span %s", originSpan.Parent, wantRemoteSpanID)
	}
	if proxySpan.Name == "" || proxySpan.Parent.TraceID() != originSpan.SpanContext.TraceID() ||
		proxySpan.Parent.SpanID() != originSpan.SpanContext.SpanID() || proxySpan.Parent.IsRemote() {
		t.Fatalf("proxy parent = %#v, want local origin span %#v", proxySpan.Parent, originSpan.SpanContext)
	}
}

func TestHandleEndsTraceSpansExactlyOnceOnFailure(t *testing.T) {
	proxyFailure := errors.New("proxy failed")
	tests := []struct {
		name       string
		dialError  error
		proxyError error
		wantError  error
		wantSpans  int
	}{
		{name: "origin failure", dialError: errors.New("origin unavailable"), wantError: ErrOrigin, wantSpans: 1},
		{name: "proxy canceled", proxyError: context.Canceled, wantError: context.Canceled, wantSpans: 2},
		{name: "proxy failure", proxyError: proxyFailure, wantError: proxyFailure, wantSpans: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			traceRuntime, exporter := newTestTraceRuntime(t)
			agentConnection, serverConnection := net.Pipe()
			originConnection, originPeer := net.Pipe()
			defer serverConnection.Close()
			defer originPeer.Close()
			handler, err := NewHandler(Options{
				ReadTimeout: time.Second, WriteTimeout: time.Second,
				Dialer: OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
					if test.dialError != nil {
						return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE, test.dialError
					}
					return originConnection, protocolv1.ErrorCode_ERROR_CODE_OK, nil
				}),
				Proxy:   func(context.Context, net.Conn, net.Conn) error { return test.proxyError },
				Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
				Tracing: traceRuntime,
			})
			if err != nil {
				t.Fatalf("NewHandler() error = %v", err)
			}
			ready := idleReady(t)
			result := make(chan error, 1)
			go func() { result <- handler.Handle(context.Background(), agentConnection, ready) }()
			if err := frame.WriteWork(serverConnection, validTracedOpenRequest()); err != nil {
				t.Fatalf("write OpenRequest: %v", err)
			}
			if err := frame.ReadWork(serverConnection, &protocolv1.OpenResponse{}); err != nil {
				t.Fatalf("read OpenResponse: %v", err)
			}
			if err := <-result; !errors.Is(err, test.wantError) {
				t.Fatalf("Handle() error = %v, want %v", err, test.wantError)
			}
			if spans := exporter.GetSpans(); len(spans) != test.wantSpans {
				t.Fatalf("exported spans = %d, want %d", len(spans), test.wantSpans)
			} else {
				if test.dialError != nil {
					if len(spans[0].Attributes) != 1 || string(spans[0].Attributes[0].Key) != internaltracing.AttributeErrorCode ||
						spans[0].Attributes[0].Value.AsString() != "ORIGIN_UNREACHABLE" {
						t.Fatalf("origin failure attributes = %#v, want only bounded error_code", spans[0].Attributes)
					}
				}
				if test.proxyError != nil && !errors.Is(test.proxyError, context.Canceled) {
					proxyAttributes := spans[len(spans)-1].Attributes
					if len(proxyAttributes) != 1 || string(proxyAttributes[0].Key) != internaltracing.AttributeErrorCode ||
						proxyAttributes[0].Value.AsString() != "INTERNAL_ERROR" {
						t.Fatalf("proxy failure attributes = %#v, want only bounded error_code", proxyAttributes)
					}
				}
			}
		})
	}
}

func newTestTraceRuntime(t *testing.T) (*internaltracing.Runtime, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	runtime, err := internaltracing.New(context.Background(), internaltracing.Config{
		ServiceName: "xtunnel-agent", ServiceVersion: "test", TracerProvider: provider,
		ProviderShutdown: provider.Shutdown,
	})
	if err != nil {
		t.Fatalf("tracing.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Errorf("Tracing Shutdown() error = %v", err)
		}
	})
	return runtime, exporter
}

func newTestHandler(t *testing.T, dialer OriginDialer, rawProxy RawProxy) *Handler {
	t.Helper()
	handler, err := NewHandler(Options{
		ReadTimeout: time.Second, WriteTimeout: time.Second,
		Dialer: dialer, Proxy: rawProxy,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func idleReady(t *testing.T) *workauth.Ready {
	t.Helper()
	workState, err := state.NewWork(state.EndpointAgent)
	if err != nil {
		t.Fatalf("NewWork() error = %v", err)
	}
	hello := &protocolv1.WorkHello{
		TunnelId: "tun_01J00000000000000000000000", ConnectorId: "con_01J00000000000000000000000",
		SessionId: "sess_01J00000000000000000000000", WorkId: testWorkID,
		Nonce: make([]byte, 32), Mac: make([]byte, 32), BudgetLeaseId: "lease_01J00000000000000000000000",
	}
	if err := workState.AcceptOutbound(hello); err != nil {
		t.Fatalf("AcceptOutbound(WorkHello) error = %v", err)
	}
	ready := &protocolv1.WorkReady{
		WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	if err := workState.AcceptInbound(ready); err != nil {
		t.Fatalf("AcceptInbound(WorkReady) error = %v", err)
	}
	return &workauth.Ready{WorkID: testWorkID, State: workState}
}

func validOpenRequest() *protocolv1.OpenRequest {
	return &protocolv1.OpenRequest{
		ProtocolVersion: 1, ConnectionId: testConnectionID, ServiceId: testServiceID,
		IngressType: protocolv1.IngressType_INGRESS_TYPE_TCP,
	}
}

func validTracedOpenRequest() *protocolv1.OpenRequest {
	request := validOpenRequest()
	request.TraceId = testTraceID
	request.Traceparent = testTraceparent
	request.Tracestate = testTracestate
	return request
}

// firstReadConnection 观察首次读取的进入点，确保测试确实经历 IDLE 等待。
type firstReadConnection struct {
	net.Conn
	started chan struct{}
	reads   chan struct{}
}

func (connection *firstReadConnection) Read(buffer []byte) (int, error) {
	if connection.reads != nil {
		connection.reads <- struct{}{}
	}
	if connection.started != nil {
		close(connection.started)
		connection.started = nil
	}
	return connection.Conn.Read(buffer)
}

func TestHandleIdleCancellationAndPartialFrameTimeout(t *testing.T) {
	for _, scenario := range []string{"cancel idle", "cancel partial frame", "partial frame timeout"} {
		t.Run(scenario, func(t *testing.T) {
			agentConnection, serverConnection := net.Pipe()
			defer serverConnection.Close()
			if err := serverConnection.SetDeadline(time.Now().Add(testTimeout)); err != nil {
				t.Fatal(err)
			}
			dialed := false
			handler := newTestHandler(t, OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
				dialed = true
				return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED, errors.New("unexpected origin dial")
			}), nil)
			handler.options.ReadTimeout = 60 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := &firstReadConnection{Conn: agentConnection, started: make(chan struct{})}
			started := reader.started
			ready := idleReady(t)
			result := make(chan error, 1)
			go func() { result <- handler.Handle(ctx, reader, ready) }()
			select {
			case <-started:
			case <-time.After(testTimeout):
				t.Fatal("Handle() did not start waiting for OPEN")
			}
			if scenario != "cancel idle" {
				if _, err := serverConnection.Write([]byte{0x80}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "partial frame timeout" {
				cancel()
			}
			select {
			case err := <-result:
				want := context.Canceled
				if scenario == "partial frame timeout" {
					want = ErrProtocol
				}
				if !errors.Is(err, want) {
					t.Fatalf("Handle() error = %v, want %v", err, want)
				}
			case <-time.After(testTimeout):
				t.Fatal("Handle() did not close after cancellation or frame timeout")
			}
			if dialed || ready.State.Phase() != state.WorkClosed {
				t.Fatalf("dialed = %v, phase = %v; want false, CLOSED", dialed, ready.State.Phase())
			}
			if _, err := serverConnection.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("WorkConn read = %v, want EOF", err)
			}
		})
	}
}

func TestHandleDistinguishesIdleEOFAndTruncatedOpen(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial_%v", partial), func(t *testing.T) {
			agent, server := net.Pipe()
			defer server.Close()
			var output bytes.Buffer
			handler := newTestHandler(t, OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
				t.Error("unexpected origin dial")
				return nil, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, nil
			}), nil)
			handler.options.Logger = slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
			result := make(chan error, 1)
			ready := idleReady(t)
			reads := make(chan struct{}, 2)
			go func() {
				result <- handler.Handle(context.Background(), &firstReadConnection{Conn: agent, reads: reads}, ready)
			}()
			select {
			case <-reads:
			case <-time.After(testTimeout):
				t.Fatal("idle read did not start")
			}
			if partial {
				if _, err := server.Write([]byte{0x80}); err != nil {
					t.Fatal(err)
				}
				select {
				case <-reads:
				case <-time.After(testTimeout):
					t.Fatal("partial frame read did not start")
				}
			}
			if err := server.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if errors.Is(err, ErrProtocol) != partial {
					t.Fatalf("protocol classification = %v, want %v", err, partial)
				}
				if !partial && !errors.Is(err, io.EOF) {
					t.Fatalf("idle error = %v, want EOF", err)
				}
			case <-time.After(testTimeout):
				t.Fatal("handler did not close")
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if partial {
				if record["error_code"] != "PROTOCOL_ERROR" || record["stage"] != "open_request" {
					t.Fatalf("truncated frame: %#v", record)
				}
			} else if record["msg"] != logging.EventAgentConnectionClosed || record["level"] != "DEBUG" || record["stage"] != "idle_wait" {
				t.Fatalf("idle closure: %#v", record)
			}
		})
	}
}

// responseWriteFailureConnection 保留真实 OPEN 读取，仅注入响应写入时的网络关闭。
type responseWriteFailureConnection struct{ net.Conn }

func (connection responseWriteFailureConnection) Write([]byte) (int, error) {
	return 0, &net.OpError{Op: "write", Net: "tcp", Err: net.ErrClosed}
}

func TestHandleOpenResponseNetworkFailureRemainsVisible(t *testing.T) {
	agent, server := net.Pipe()
	origin, originPeer := net.Pipe()
	defer server.Close()
	defer originPeer.Close()
	var output bytes.Buffer
	handler := newTestHandler(t, OriginDialerFunc(func(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
		return origin, protocolv1.ErrorCode_ERROR_CODE_OK, nil
	}), func(context.Context, net.Conn, net.Conn) error {
		t.Error("RAW proxy must not start after failed OPEN response")
		return nil
	})
	handler.options.Logger = slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ready := idleReady(t)
	result := make(chan error, 1)
	go func() { result <- handler.Handle(context.Background(), responseWriteFailureConnection{agent}, ready) }()
	if err := server.SetDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	if err := frame.WriteWork(server, validOpenRequest()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("response error = %v, want network close", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("handler did not finish after response write failure")
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != logging.EventAgentConnectionFailed || record["level"] != "ERROR" || record["stage"] != "origin_dial" {
		t.Fatalf("response write failure log = %#v", record)
	}
	if ready.State.Phase() != state.WorkClosed {
		t.Fatalf("phase = %v, want CLOSED", ready.State.Phase())
	}
}
