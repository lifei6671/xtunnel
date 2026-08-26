package open

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	serverworkauth "github.com/lifei6671/xtunnel/internal/server/workauth"
)

const (
	testTunnelID     = "tun_01J00000000000000000000000"
	testConnectorID  = "con_01J00000000000000000000000"
	testSessionID    = "sess_01J00000000000000000000000"
	testWorkID       = "work_01J00000000000000000000000"
	testConnectionID = "conn_01J00000000000000000000000"
	testServiceID    = "svc_01J00000000000000000000000"
	testTimeout      = 2 * time.Second
)

func TestHandlePreservesRawBytesFollowingOpenOKFrame(t *testing.T) {
	serverConnection, agentConnection := net.Pipe()
	defer agentConnection.Close()
	handler := newTestHandler(t)
	idle := serverIdle(t)
	result := make(chan struct {
		active *Active
		err    error
	}, 1)
	go func() {
		active, err := handler.Handle(context.Background(), serverConnection, idle, validOpenRequest())
		result <- struct {
			active *Active
			err    error
		}{active: active, err: err}
	}()

	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(agentConnection, request); err != nil {
		t.Fatalf("read OpenRequest: %v", err)
	}
	if request.GetServiceId() != testServiceID || request.GetConnectionId() != testConnectionID {
		t.Fatalf("OpenRequest = %#v", request)
	}
	rawPayload := []byte("raw-after-open-ok-in-the-same-write")
	response := &protocolv1.OpenResponse{
		ConnectionId: testConnectionID, Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	var encoded bytes.Buffer
	if err := frame.WriteWork(&encoded, response); err != nil {
		t.Fatalf("encode OpenResponse: %v", err)
	}
	encoded.Write(rawPayload)
	writeDone := make(chan error, 1)
	go func() {
		_, err := agentConnection.Write(encoded.Bytes())
		writeDone <- err
	}()

	var active *Active
	select {
	case completed := <-result:
		if completed.err != nil {
			t.Fatalf("Handle() error = %v", completed.err)
		}
		active = completed.active
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not return after OPEN_OK")
	}
	defer active.Connection.Close()
	gotRaw := make([]byte, len(rawPayload))
	if _, err := io.ReadFull(active.Connection, gotRaw); err != nil {
		t.Fatalf("read preserved RAW: %v", err)
	}
	if !bytes.Equal(gotRaw, rawPayload) {
		t.Fatalf("RAW = %q, want %q", gotRaw, rawPayload)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write combined OpenResponse+RAW: %v", err)
	}
	if active.Identity.WorkID != testWorkID || active.Response.GetStatus() != protocolv1.OpenStatus_OPEN_STATUS_OK {
		t.Fatalf("Active = %#v", active)
	}
}

func TestHandleReturnsAgentOpenErrorAndClosesWork(t *testing.T) {
	serverConnection, agentConnection := net.Pipe()
	defer agentConnection.Close()
	handler := newTestHandler(t)
	idle := serverIdle(t)
	result := make(chan error, 1)
	go func() {
		_, err := handler.Handle(context.Background(), serverConnection, idle, validOpenRequest())
		result <- err
	}()
	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(agentConnection, request); err != nil {
		t.Fatalf("read OpenRequest: %v", err)
	}
	response := &protocolv1.OpenResponse{
		ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_ERROR,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT,
	}
	if err := frame.WriteWork(agentConnection, response); err != nil {
		t.Fatalf("write OpenResponse: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("Handle() error = %v, want ErrRejected", err)
		}
		var rejected *Rejected
		if !errors.As(err, &rejected) || rejected.Code != protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT {
			t.Fatalf("Rejected = %#v", rejected)
		}
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not finish after OPEN_ERROR")
	}
	if idle.State.Phase() != state.WorkClosed {
		t.Fatalf("Work phase = %v, want closed", idle.State.Phase())
	}
}

func TestHandleRejectsMismatchedConnectionID(t *testing.T) {
	serverConnection, agentConnection := net.Pipe()
	defer agentConnection.Close()
	handler := newTestHandler(t)
	idle := serverIdle(t)
	result := make(chan error, 1)
	go func() {
		_, err := handler.Handle(context.Background(), serverConnection, idle, validOpenRequest())
		result <- err
	}()
	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(agentConnection, request); err != nil {
		t.Fatalf("read OpenRequest: %v", err)
	}
	response := &protocolv1.OpenResponse{
		ConnectionId: "conn_01J00000000000000000000001",
		Status:       protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode:    protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	if err := frame.WriteWork(agentConnection, response); err != nil {
		t.Fatalf("write mismatched OpenResponse: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Handle() error = %v, want ErrProtocol", err)
		}
		if errors.Is(err, ErrPreRAWTransport) || errors.Is(err, ErrRawCommitted) {
			t.Fatalf("Handle() error = %v, mismatched connection ID must not be retryable", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not reject mismatched connection ID")
	}
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	handler, err := NewHandler(Options{WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	return handler
}

func TestNewHandlerRejectsFrameLimitAboveProtocol(t *testing.T) {
	if _, err := NewHandler(Options{
		WriteTimeout: time.Second, ReadTimeout: time.Second, MaxFrameBytes: frame.MaxWorkFrameSize + 1,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("NewHandler() error = %v, want ErrInvalidInput", err)
	}
}

func serverIdle(t *testing.T) serverworkauth.Idle {
	t.Helper()
	workState, err := state.NewWork(state.EndpointServer)
	if err != nil {
		t.Fatalf("NewWork() error = %v", err)
	}
	hello := &protocolv1.WorkHello{
		TunnelId: testTunnelID, ConnectorId: testConnectorID, SessionId: testSessionID,
		WorkId: testWorkID, Nonce: make([]byte, 32), Mac: make([]byte, 32),
		BudgetLeaseId: "lease_01J00000000000000000000000",
	}
	if err := workState.AcceptInbound(hello); err != nil {
		t.Fatalf("AcceptInbound(WorkHello) error = %v", err)
	}
	ready := &protocolv1.WorkReady{
		WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	if err := workState.AcceptOutbound(ready); err != nil {
		t.Fatalf("AcceptOutbound(WorkReady) error = %v", err)
	}
	return serverworkauth.Idle{
		TunnelID: testTunnelID, ConnectorID: testConnectorID, SessionID: testSessionID,
		WorkID: testWorkID, State: workState,
	}
}

func validOpenRequest() *protocolv1.OpenRequest {
	return &protocolv1.OpenRequest{
		ProtocolVersion: 1, ConnectionId: testConnectionID, ServiceId: testServiceID,
		IngressType: protocolv1.IngressType_INGRESS_TYPE_TCP,
	}
}
