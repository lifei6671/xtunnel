package controlsession

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/safego"
)

func TestOwnerWritesSingleOrderedStreamWithPriorityAndCoalescing(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	signaled := &writeSignalConn{Conn: server, started: make(chan struct{})}
	owner := mustOwner(t, signaled, state.EndpointServer, ownerOptions(2, 3, 2, time.Second))
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	firstSnapshot := snapshotEnvelope(testTunnelID, 1)
	if err := owner.Enqueue(firstSnapshot); err != nil {
		t.Fatalf("Enqueue(first snapshot) error = %v", err)
	}
	waitSignal(t, signaled.started)

	secondSnapshot := snapshotEnvelope(testTunnelID, 2)
	latestSnapshot := snapshotEnvelope(testTunnelID, 3)
	latestSnapshot.GetConfigSnapshot().Services = []*protocolv1.ServiceConfig{{ServiceId: testServiceID, OriginHost: "origin-v3"}}
	messages := []*protocolv1.ControlEnvelope{
		secondSnapshot,
		latestSnapshot,
		workDemandEnvelope(1, 10),
		workDemandEnvelope(2, 20),
		errorEnvelope(protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR),
	}
	for _, message := range messages {
		if err := owner.Enqueue(message); err != nil {
			t.Fatalf("Enqueue(%T) error = %v", message.GetPayload(), err)
		}
	}
	latestSnapshot.GetConfigSnapshot().Services[0].OriginHost = "mutated"

	setConnectionDeadline(t, peer)
	frames := make([]*protocolv1.ControlEnvelope, 4)
	frames[0] = &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(peer, frames[0]); err != nil {
		t.Fatalf("ReadControl(frame 0) error = %v", err)
	}
	if err := frame.WriteControl(peer, configAckEnvelope(1)); err != nil {
		t.Fatalf("WriteControl(ConfigAck 1) error = %v", err)
	}
	for index := 1; index < 3; index++ {
		frames[index] = &protocolv1.ControlEnvelope{}
		if err := frame.ReadControl(peer, frames[index]); err != nil {
			t.Fatalf("ReadControl(frame %d) error = %v", index, err)
		}
	}
	if err := frame.WriteControl(peer, configAckEnvelope(3)); err != nil {
		t.Fatalf("WriteControl(ConfigAck 3) error = %v", err)
	}
	frames[3] = &protocolv1.ControlEnvelope{}
	if err := frame.ReadControl(peer, frames[3]); err != nil {
		t.Fatalf("ReadControl(frame 3) error = %v; owner error = %v", err, owner.Wait())
	}
	if frames[0].GetConfigSnapshot().GetRevision() != 1 {
		t.Fatalf("first frame = %#v, want already in-flight snapshot revision 1", frames[0])
	}
	if frames[1].GetError() == nil {
		t.Fatalf("second frame = %#v, want queued high-priority Error", frames[1])
	}
	if snapshot := frames[2].GetConfigSnapshot(); snapshot.GetRevision() != 3 || snapshot.GetServices()[0].GetOriginHost() != "origin-v3" {
		t.Fatalf("third frame snapshot = %#v, want immutable coalesced revision 3", snapshot)
	}
	if demand := frames[3].GetWorkDemand(); demand.GetDemandGeneration() != 2 || demand.GetDesiredNonActive() != 20 {
		t.Fatalf("fourth frame work demand = %#v, want coalesced generation 2", demand)
	}

	if err := peer.Close(); err != nil {
		t.Fatalf("peer Close() error = %v", err)
	}
	if err := owner.Wait(); err != nil {
		t.Fatalf("Wait() after clean EOF error = %v", err)
	}
	if phase := owner.control.Phase(); phase != state.ControlClosed {
		t.Fatalf("control phase = %v, want CLOSED", phase)
	}
}

func TestOwnerReadsFragmentedAndCoalescedFramesInOrder(t *testing.T) {
	server, peer := net.Pipe()
	owner := mustOwner(t, server, state.EndpointServer, ownerOptions(1, 1, 4, time.Second))
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	setConnectionDeadline(t, peer)
	first := encodeControlFrames(t, heartbeatEnvelope(1))
	combined := encodeControlFrames(t, heartbeatEnvelope(2), heartbeatEnvelope(3))

	writeResult := make(chan error, 1)
	go func() {
		for _, octet := range first {
			if _, err := peer.Write([]byte{octet}); err != nil {
				writeResult <- err
				return
			}
		}
		if _, err := peer.Write(combined); err != nil {
			writeResult <- err
			return
		}
		writeResult <- peer.Close()
	}()

	for expected := uint64(1); expected <= 3; expected++ {
		select {
		case inbound, ok := <-owner.Inbound():
			if !ok {
				t.Fatalf("Inbound() closed before heartbeat %d", expected)
			}
			if timestamp := inbound.Envelope.GetHeartbeat().GetTimestampMs(); timestamp != expected {
				t.Fatalf("inbound timestamp = %d, want %d", timestamp, expected)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for heartbeat %d", expected)
		}
	}
	if err := <-writeResult; err != nil {
		t.Fatalf("fragmented/combined peer write error = %v", err)
	}
	if err := owner.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if _, ok := <-owner.Inbound(); ok {
		t.Fatal("Inbound() remained open after all internal goroutines exited")
	}
}

func TestOwnerRejectsIllegalInboundDirection(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	owner := mustOwner(t, server, state.EndpointServer, ownerOptions(1, 1, 1, time.Second))
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	writeErr := sendEnvelope(peer, snapshotEnvelope(testTunnelID, 1))
	if err := owner.Wait(); !errors.Is(err, ErrControlProtocol) || !errors.Is(err, state.ErrIllegalDirection) {
		t.Fatalf("Wait() error = %v, want protocol illegal direction", err)
	}
	allowPeerCloseError(t, <-writeErr)
}

func TestOwnerRejectsUnknownMalformedAndOversizedFrames(t *testing.T) {
	unknown := heartbeatEnvelope(1)
	unknown.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	var oversized [binary.MaxVarintLen64]byte
	oversizedLength := binary.PutUvarint(oversized[:], frame.MaxControlFrameSize+1)
	tests := []struct {
		name  string
		bytes []byte
		want  error
	}{
		{name: "unknown fields", bytes: encodeControlFrames(t, unknown), want: validate.ErrUnknownFields},
		{name: "malformed protobuf", bytes: []byte{0x01, 0xff}, want: frame.ErrMalformedMessage},
		{name: "oversized frame", bytes: append([]byte(nil), oversized[:oversizedLength]...), want: frame.ErrFrameTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, peer := net.Pipe()
			defer peer.Close()
			owner := mustOwner(t, server, state.EndpointServer, ownerOptions(1, 1, 1, time.Second))
			if err := owner.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			writeErr := sendRaw(peer, test.bytes)
			if err := owner.Wait(); !errors.Is(err, ErrControlProtocol) || !errors.Is(err, test.want) {
				t.Fatalf("Wait() error = %v, want ErrControlProtocol and %v", err, test.want)
			}
			allowPeerCloseError(t, <-writeErr)
		})
	}
}

func TestOwnerWriteTimeoutClosesBlockedSession(t *testing.T) {
	agent, peer := net.Pipe()
	defer peer.Close()
	owner := mustOwner(t, agent, state.EndpointAgent, ownerOptions(1, 1, 1, 30*time.Millisecond))
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := owner.Enqueue(heartbeatEnvelope(1)); err != nil {
		t.Fatalf("Enqueue(heartbeat) error = %v", err)
	}
	if err := owner.Wait(); !errors.Is(err, ErrControlWrite) {
		t.Fatalf("Wait() error = %v, want ErrControlWrite", err)
	}
	if phase := owner.control.Phase(); phase != state.ControlClosed {
		t.Fatalf("control phase = %v, want CLOSED", phase)
	}
}

func TestOwnerOutboxFullTriggersSingleShutdown(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	signaled := &writeSignalConn{Conn: server, started: make(chan struct{})}
	owner := mustOwner(t, signaled, state.EndpointServer, ownerOptions(1, 1, 1, time.Second))
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := owner.Enqueue(snapshotEnvelope(testTunnelID, 1)); err != nil {
		t.Fatalf("Enqueue(in-flight snapshot) error = %v", err)
	}
	waitSignal(t, signaled.started)
	if err := owner.Enqueue(snapshotEnvelope(testTunnelID, 2)); err != nil {
		t.Fatalf("Enqueue(queued snapshot) error = %v", err)
	}
	if err := owner.Enqueue(snapshotEnvelope(testTunnelIDTwo, 1)); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("Enqueue(full outbox) error = %v, want ErrOutboxFull", err)
	}
	if err := owner.Enqueue(errorEnvelope(protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR)); !errors.Is(err, ErrOwnerClosed) {
		t.Fatalf("Enqueue(after fatal) error = %v, want ErrOwnerClosed", err)
	}
	if err := owner.Wait(); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("Wait() error = %v, want ErrOutboxFull", err)
	}
}

func TestOwnerInboundCapacityDoesNotBlockStateOwner(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	owner := mustOwner(t, server, state.EndpointServer, ownerOptions(1, 1, 1, time.Second))
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	writeErr := sendRaw(peer, encodeControlFrames(t, heartbeatEnvelope(1), heartbeatEnvelope(2)))
	if err := owner.Wait(); !errors.Is(err, ErrInboundQueueFull) {
		t.Fatalf("Wait() error = %v, want ErrInboundQueueFull", err)
	}
	allowPeerCloseError(t, <-writeErr)
}

func TestOwnerCancellationAndCleanEOFBothExitAllLoops(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		server, peer := net.Pipe()
		defer peer.Close()
		ctx, cancel := context.WithCancel(context.Background())
		owner := mustOwner(t, server, state.EndpointServer, ownerOptions(1, 1, 1, time.Second))
		if err := owner.Start(ctx); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		cancel()
		if err := owner.Wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait() error = %v, want context.Canceled", err)
		}
		select {
		case <-owner.Done():
		default:
			t.Fatal("Done() not closed after Wait returned")
		}
	})

	t.Run("clean EOF", func(t *testing.T) {
		server, peer := net.Pipe()
		owner := mustOwner(t, server, state.EndpointServer, ownerOptions(1, 1, 1, time.Second))
		if err := owner.Start(context.Background()); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		if err := peer.Close(); err != nil {
			t.Fatalf("peer Close() error = %v", err)
		}
		if err := owner.Wait(); err != nil {
			t.Fatalf("Wait() clean EOF error = %v", err)
		}
	})
}

func TestOwnerLoopPanicsUseSingleShutdownPath(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    state.Endpoint
		connection  func(net.Conn) net.Conn
		beforeStart func(*Owner)
		afterStart  func(*Owner) error
	}{
		{
			name:     "read loop",
			endpoint: state.EndpointServer,
			connection: func(connection net.Conn) net.Conn {
				return &panicReadConn{Conn: connection}
			},
			afterStart: func(*Owner) error { return nil },
		},
		{
			name:     "write loop",
			endpoint: state.EndpointAgent,
			connection: func(connection net.Conn) net.Conn {
				return &panicWriteConn{Conn: connection}
			},
			afterStart: func(owner *Owner) error {
				return owner.Enqueue(heartbeatEnvelope(1))
			},
		},
		{
			name:       "owner loop",
			endpoint:   state.EndpointServer,
			connection: func(connection net.Conn) net.Conn { return connection },
			beforeStart: func(owner *Owner) {
				// 破坏同 package 内部不变量，确定性验证中央 Owner 自身 panic 时仍能
				// 执行唯一关闭路径，而不是向已经退出的 fatal 消费者发送错误。
				owner.outbox = nil
			},
			afterStart: func(*Owner) error { return nil },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, peer := net.Pipe()
			defer peer.Close()
			owner := mustOwner(t, test.connection(server), test.endpoint, ownerOptions(1, 1, 1, time.Second))
			if test.beforeStart != nil {
				test.beforeStart(owner)
			}
			if err := owner.Start(context.Background()); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if err := test.afterStart(owner); err != nil {
				t.Fatalf("prepare panic path: %v", err)
			}

			select {
			case <-owner.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("Owner panic path did not close Done")
			}
			if err := owner.Wait(); !errors.Is(err, safego.ErrPanic) {
				t.Fatalf("Wait() error = %v, want safego.ErrPanic", err)
			}
			if phase := owner.control.Phase(); phase != state.ControlClosed {
				t.Fatalf("control phase = %v, want CLOSED", phase)
			}
		})
	}
}

func TestNewOwnerRejectsControlFrameLimitAboveProtocol(t *testing.T) {
	serverConnection, peer := net.Pipe()
	defer serverConnection.Close()
	defer peer.Close()
	control := establishedControl(t, state.EndpointServer)
	_, err := NewOwner(serverConnection, control, Options{
		ProtocolVersion: testProtocolVersion, HighPriorityCapacity: 1, NormalCapacity: 1,
		InboundCapacity: 1, WriteTimeout: time.Second, MaxFrameBytes: frame.MaxControlFrameSize + 1,
	})
	if !errors.Is(err, ErrInvalidOwnerOptions) {
		t.Fatalf("NewOwner() error = %v, want ErrInvalidOwnerOptions", err)
	}
}

func TestOwnerShutdownReportsCloseErrorAndPreservesOrder(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	closeFailure := errors.New("injected close failure")
	observed := &shutdownOrderConn{Conn: server, closeErr: closeFailure}
	ctx, cancel := context.WithCancel(context.Background())
	owner := mustOwner(t, observed, state.EndpointServer, ownerOptions(1, 1, 1, time.Second))
	if err := owner.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	cancel()
	if err := owner.Wait(); !errors.Is(err, context.Canceled) || !errors.Is(err, closeFailure) {
		t.Fatalf("Wait() error = %v, want cancellation joined with close error", err)
	}
	if events := observed.Events(); len(events) != 2 || events[0] != "deadline" || events[1] != "close" {
		t.Fatalf("shutdown events = %v, want [deadline close]", events)
	}
}

func TestOwnerLifecycleAndConstructorValidation(t *testing.T) {
	control, err := state.NewControl(state.EndpointServer, testProtocolVersion)
	if err != nil {
		t.Fatalf("NewControl() error = %v", err)
	}
	server, peer := net.Pipe()
	defer server.Close()
	defer peer.Close()
	if _, err := NewOwner(server, control, ownerOptions(1, 1, 1, time.Second)); !errors.Is(err, ErrInvalidOwnerOptions) {
		t.Fatalf("NewOwner(AUTH control) error = %v, want ErrInvalidOwnerOptions", err)
	}

	established := establishedControl(t, state.EndpointServer)
	owner, err := NewOwner(server, established, ownerOptions(1, 1, 1, time.Second))
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}
	if err := owner.Enqueue(errorEnvelope(protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR)); !errors.Is(err, ErrOwnerNotRunning) {
		t.Fatalf("Enqueue(before Start) error = %v, want ErrOwnerNotRunning", err)
	}
	if err := owner.Wait(); !errors.Is(err, ErrOwnerNotRunning) {
		t.Fatalf("Wait(before Start) error = %v, want ErrOwnerNotRunning", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := owner.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := owner.Start(ctx); !errors.Is(err, ErrOwnerAlreadyStarted) {
		t.Fatalf("second Start() error = %v, want ErrOwnerAlreadyStarted", err)
	}
	cancel()
	if err := owner.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want context.Canceled", err)
	}
	if err := owner.Enqueue(errorEnvelope(protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR)); !errors.Is(err, ErrOwnerClosed) {
		t.Fatalf("Enqueue(after close) error = %v, want ErrOwnerClosed", err)
	}
}

func mustOwner(t *testing.T, connection net.Conn, endpoint state.Endpoint, options Options) *Owner {
	t.Helper()
	owner, err := NewOwner(connection, establishedControl(t, endpoint), options)
	if err != nil {
		t.Fatalf("NewOwner() error = %v", err)
	}
	return owner
}

type panicReadConn struct {
	net.Conn
}

func (*panicReadConn) Read([]byte) (int, error) {
	panic("injected read panic")
}

type panicWriteConn struct {
	net.Conn
}

func (*panicWriteConn) Write([]byte) (int, error) {
	panic("injected write panic")
}

func establishedControl(t *testing.T, endpoint state.Endpoint) *state.Control {
	t.Helper()
	control, err := state.NewControl(endpoint, testProtocolVersion)
	if err != nil {
		t.Fatalf("NewControl() error = %v", err)
	}
	result := &protocolv1.ConnectorAuthResult{Result: &protocolv1.ConnectorAuthResult_Success{
		Success: &protocolv1.ConnectorAuthSuccess{SessionSecret: make([]byte, 32)},
	}}
	if endpoint == state.EndpointServer {
		if _, err := control.AcceptOutbound(result); err != nil {
			t.Fatalf("AcceptOutbound(auth success) error = %v", err)
		}
		if err := control.CommitAuthSuccessAfterFlush(result); err != nil {
			t.Fatalf("CommitAuthSuccessAfterFlush() error = %v", err)
		}
	} else {
		if _, err := control.AcceptInbound(result); err != nil {
			t.Fatalf("AcceptInbound(auth success) error = %v", err)
		}
		if err := control.CommitAuthSuccessAfterDecode(result); err != nil {
			t.Fatalf("CommitAuthSuccessAfterDecode() error = %v", err)
		}
	}
	return control
}

func ownerOptions(high, normal, inbound int, writeTimeout time.Duration) Options {
	return Options{
		ProtocolVersion: testProtocolVersion, HighPriorityCapacity: high,
		NormalCapacity: normal, InboundCapacity: inbound, WriteTimeout: writeTimeout,
	}
}

func encodeControlFrames(t *testing.T, envelopes ...*protocolv1.ControlEnvelope) []byte {
	t.Helper()
	var encoded bytes.Buffer
	for _, envelope := range envelopes {
		if err := frame.WriteControl(&encoded, envelope); err != nil {
			t.Fatalf("WriteControl(buffer) error = %v", err)
		}
	}
	return encoded.Bytes()
}

func sendEnvelope(connection net.Conn, envelope *protocolv1.ControlEnvelope) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- frame.WriteControl(connection, envelope)
	}()
	return result
}

func sendRaw(connection net.Conn, raw []byte) <-chan error {
	result := make(chan error, 1)
	go func() {
		_, err := connection.Write(raw)
		result <- err
	}()
	return result
}

func allowPeerCloseError(t *testing.T, err error) {
	t.Helper()
	if err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("peer write error = %v", err)
	}
}

func setConnectionDeadline(t *testing.T, connection net.Conn) {
	t.Helper()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for write loop")
	}
}

type writeSignalConn struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (connection *writeSignalConn) Write(payload []byte) (int, error) {
	connection.once.Do(func() { close(connection.started) })
	return connection.Conn.Write(payload)
}

type shutdownOrderConn struct {
	net.Conn
	mu       sync.Mutex
	events   []string
	closeErr error
}

func (connection *shutdownOrderConn) SetDeadline(deadline time.Time) error {
	connection.mu.Lock()
	connection.events = append(connection.events, "deadline")
	connection.mu.Unlock()
	return connection.Conn.SetDeadline(deadline)
}

func (connection *shutdownOrderConn) Close() error {
	connection.mu.Lock()
	connection.events = append(connection.events, "close")
	connection.mu.Unlock()
	return errors.Join(connection.Conn.Close(), connection.closeErr)
}

func (connection *shutdownOrderConn) Events() []string {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return append([]string(nil), connection.events...)
}
