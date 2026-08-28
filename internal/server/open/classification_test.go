package open

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

func TestHandleClassifiesPreRawTransportFailures(t *testing.T) {
	tests := []struct {
		name     string
		readData []byte
		readErr  error
		writeErr error
	}{
		{name: "request write", writeErr: io.ErrClosedPipe},
		{name: "response EOF", readErr: io.EOF},
		{name: "response truncated", readData: []byte{0x03, 0x01}, readErr: io.EOF},
		{name: "response timeout", readErr: os.ErrDeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection := &scriptedOpenConn{
				readData: bytes.NewReader(test.readData), readErr: test.readErr, writeErr: test.writeErr,
			}
			_, err := newTestHandler(t).Handle(context.Background(), connection, serverIdle(t), validOpenRequest())
			if !errors.Is(err, ErrPreRAWTransport) {
				t.Fatalf("Handle() error = %v, want ErrPreRAWTransport", err)
			}
			if errors.Is(err, ErrProtocol) || errors.Is(err, ErrRawCommitted) {
				t.Fatalf("Handle() error = %v, want only pre-RAW transport classification", err)
			}
			if !connection.closed {
				t.Fatal("Handle() did not close failed pre-RAW WorkConn")
			}
		})
	}
}

func TestHandleCanceledContextDoesNotWriteOpenRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	connection := &scriptedOpenConn{readData: bytes.NewReader(nil), readErr: io.EOF}

	_, err := newTestHandler(t).Handle(ctx, connection, serverIdle(t), validOpenRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle() error = %v, want context.Canceled", err)
	}
	if connection.writes != 0 {
		t.Fatalf("Handle() writes = %d, want 0 after entry cancellation", connection.writes)
	}
	if !connection.closed {
		t.Fatal("Handle() did not close canceled WorkConn")
	}
}

func TestHandleClassifiesMalformedFramesAsProtocol(t *testing.T) {
	overLimit := make([]byte, binary.MaxVarintLen64)
	overLimit = overLimit[:binary.PutUvarint(overLimit, frame.MaxWorkFrameSize+1)]
	unknownResponse := &protocolv1.OpenResponse{
		ConnectionId: testConnectionID, Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}
	unknownResponse.ProtoReflect().SetUnknown([]byte{0x78, 0x01})
	var unknownFrame bytes.Buffer
	if err := frame.WriteWork(&unknownFrame, unknownResponse); err != nil {
		t.Fatalf("encode OpenResponse with unknown field: %v", err)
	}
	tests := []struct {
		name     string
		readData []byte
	}{
		{name: "non-canonical length", readData: []byte{0x81, 0x00, 0x00}},
		{name: "oversize response", readData: overLimit},
		{name: "malformed protobuf", readData: []byte{0x01, 0xff}},
		{name: "unknown field", readData: unknownFrame.Bytes()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection := &scriptedOpenConn{readData: bytes.NewReader(test.readData), readErr: io.EOF}
			_, err := newTestHandler(t).Handle(context.Background(), connection, serverIdle(t), validOpenRequest())
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Handle() error = %v, want ErrProtocol", err)
			}
			if errors.Is(err, ErrPreRAWTransport) || errors.Is(err, ErrRawCommitted) {
				t.Fatalf("Handle() error = %v, want only Protocol classification", err)
			}
		})
	}
}

func TestHandleClassifiesOversizeOpenRequestAsProtocol(t *testing.T) {
	handler, err := NewHandler(Options{
		HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second, MaxFrameBytes: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	connection := &scriptedOpenConn{readData: bytes.NewReader(nil), readErr: io.EOF}
	_, err = handler.Handle(context.Background(), connection, serverIdle(t), validOpenRequest())
	if !errors.Is(err, ErrProtocol) || errors.Is(err, ErrPreRAWTransport) {
		t.Fatalf("Handle() error = %v, want non-retryable oversize Protocol failure", err)
	}
}

func TestHandleClassifiesFailureAfterAcceptRawAsCommitted(t *testing.T) {
	serverConnection, agentConnection := net.Pipe()
	defer agentConnection.Close()
	committedConnection := &failClearDeadlineConn{Conn: serverConnection}
	result := make(chan error, 1)
	go func() {
		_, err := newTestHandler(t).Handle(context.Background(), committedConnection, serverIdle(t), validOpenRequest())
		result <- err
	}()

	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(agentConnection, request); err != nil {
		t.Fatalf("read OpenRequest: %v", err)
	}
	if err := frame.WriteWork(agentConnection, &protocolv1.OpenResponse{
		ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}); err != nil {
		t.Fatalf("write OpenResponse: %v", err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrRawCommitted) {
			t.Fatalf("Handle() error = %v, want ErrRawCommitted", err)
		}
		if errors.Is(err, ErrPreRAWTransport) {
			t.Fatalf("Handle() error = %v, must not permit retry after AcceptRaw", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Handle() did not return after committed deadline failure")
	}
}

type scriptedOpenConn struct {
	readData *bytes.Reader
	readErr  error
	writeErr error
	writes   int
	closed   bool
}

func (connection *scriptedOpenConn) Read(buffer []byte) (int, error) {
	if connection.readData != nil && connection.readData.Len() > 0 {
		return connection.readData.Read(buffer)
	}
	return 0, connection.readErr
}

func (connection *scriptedOpenConn) Write(buffer []byte) (int, error) {
	connection.writes++
	if connection.writeErr != nil {
		return 0, connection.writeErr
	}
	return len(buffer), nil
}

func (connection *scriptedOpenConn) Close() error                     { connection.closed = true; return nil }
func (connection *scriptedOpenConn) LocalAddr() net.Addr              { return openTestAddr("local") }
func (connection *scriptedOpenConn) RemoteAddr() net.Addr             { return openTestAddr("remote") }
func (connection *scriptedOpenConn) SetDeadline(time.Time) error      { return nil }
func (connection *scriptedOpenConn) SetReadDeadline(time.Time) error  { return nil }
func (connection *scriptedOpenConn) SetWriteDeadline(time.Time) error { return nil }

type failClearDeadlineConn struct{ net.Conn }

func (connection *failClearDeadlineConn) SetDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return io.ErrClosedPipe
	}
	return connection.Conn.SetDeadline(deadline)
}

type openTestAddr string

func (address openTestAddr) Network() string { return "test" }
func (address openTestAddr) String() string  { return string(address) }
