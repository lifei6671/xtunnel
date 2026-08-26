package open

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/workauth"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	testWorkID       = "work_01J00000000000000000000000"
	testConnectionID = "conn_01J00000000000000000000000"
	testServiceID    = "svc_01J00000000000000000000000"
	testTimeout      = 2 * time.Second
)

func TestHandlePreservesRawBytesFollowingOpenRequestFrame(t *testing.T) {
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
	ready := idleReady(t)
	result := make(chan error, 1)
	transitions := make([]state.WorkPhase, 0, 2)
	go func() {
		result <- handler.HandleObserved(
			context.Background(), agentConnection, ready,
			func(target state.WorkPhase, commit func() error) error {
				if err := commit(); err != nil {
					return err
				}
				transitions = append(transitions, target)
				return nil
			},
		)
	}()

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

func TestHandleEnforcesOriginConnectTimeout(t *testing.T) {
	agentConnection, serverConnection := net.Pipe()
	defer serverConnection.Close()
	handler, err := NewHandler(Options{
		ReadTimeout: time.Second, WriteTimeout: time.Second, ConnectTimeout: 20 * time.Millisecond,
		Dialer: OriginDialerFunc(func(ctx context.Context, _ string) (net.Conn, protocolv1.ErrorCode, error) {
			<-ctx.Done()
			return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT, ctx.Err()
		}),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	ready := idleReady(t)
	result := make(chan error, 1)
	go func() { result <- handler.Handle(context.Background(), agentConnection, ready) }()
	if err := frame.WriteWork(serverConnection, validOpenRequest()); err != nil {
		t.Fatalf("write OpenRequest: %v", err)
	}
	response := &protocolv1.OpenResponse{}
	if err := frame.ReadWork(serverConnection, response); err != nil {
		t.Fatalf("read timeout OpenResponse: %v", err)
	}
	if response.GetStatus() != protocolv1.OpenStatus_OPEN_STATUS_ERROR ||
		response.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT {
		t.Fatalf("timeout OpenResponse = %#v", response)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrOrigin) {
			t.Fatalf("Handle() error = %v, want ErrOrigin", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not finish after Origin timeout")
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

func newTestHandler(t *testing.T, dialer OriginDialer, rawProxy RawProxy) *Handler {
	t.Helper()
	handler, err := NewHandler(Options{
		ReadTimeout: time.Second, WriteTimeout: time.Second, ConnectTimeout: time.Second,
		Dialer: dialer, Proxy: rawProxy,
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
