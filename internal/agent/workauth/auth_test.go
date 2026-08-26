package workauth

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	testTunnelID    = "tun_01J00000000000000000000000"
	testConnectorID = "con_01J00000000000000000000000"
	testSessionID   = "sess_01J00000000000000000000000"
	testWorkID      = "work_01J00000000000000000000000"
	otherWorkID     = "work_01J00000000000000000000001"
	testLeaseID     = "lease_01J00000000000000000000000"
)

func TestAuthenticateMatchesFrozenWorkHelloGoldenVector(t *testing.T) {
	config := testConfig(0x11)
	nonce := bytes.Repeat([]byte{0x42}, nonceSize)
	ready, err, hello := exchange(t, config, fixedWorkID(testWorkID), bytes.NewReader(nonce), func(connection net.Conn, hello *protocolv1.WorkHello) error {
		return frame.WriteWork(connection, &protocolv1.WorkReady{
			WorkId: hello.GetWorkId(), Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY,
		})
	})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if ready.WorkID != testWorkID || ready.State == nil || ready.State.Phase() != state.WorkIdle {
		t.Fatalf("Authenticate() Ready = %#v, want WorkIdle", ready)
	}
	if hello.GetTunnelId() != testTunnelID || hello.GetConnectorId() != testConnectorID ||
		hello.GetSessionId() != testSessionID || hello.GetBudgetLeaseId() != testLeaseID ||
		hello.GetWorkId() != testWorkID || !bytes.Equal(hello.GetNonce(), nonce) {
		t.Fatalf("WorkHello = %#v", hello)
	}
	wantMAC, err := deterministic.ComputeWorkHelloMAC(config.Session.SessionSecret[:], hello)
	if err != nil {
		t.Fatalf("ComputeWorkHelloMAC() error = %v", err)
	}
	if !bytes.Equal(hello.GetMac(), wantMAC) {
		t.Fatalf("WorkHello MAC = %x, want %x", hello.GetMac(), wantMAC)
	}
	wire, err := deterministic.Marshal(hello)
	if err != nil {
		t.Fatalf("deterministic.Marshal() error = %v", err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "golden", "protocol-v1", "work-hello-v1.hex"))
	if err != nil {
		t.Fatalf("read WorkHello golden fixture: %v", err)
	}
	if got, want := hex.EncodeToString(wire), strings.TrimSpace(string(fixture)); got != want {
		t.Fatalf("WorkHello golden mismatch\ngot:  %s\nwant: %s", got, want)
	}
}

func TestGenerateWorkIDProducesDistinctValidatedIDs(t *testing.T) {
	first, err := generateWorkID()
	if err != nil {
		t.Fatalf("generateWorkID(first) error = %v", err)
	}
	second, err := generateWorkID()
	if err != nil {
		t.Fatalf("generateWorkID(second) error = %v", err)
	}
	if first == second || validate.ValidateID(first, "work_") != nil || validate.ValidateID(second, "work_") != nil {
		t.Fatalf("generated Work IDs = %q, %q", first, second)
	}
}

func TestAuthenticateRejectsCSPRNGFailuresBeforeNetworkIO(t *testing.T) {
	tests := []struct {
		name   string
		workID func() (string, error)
		random io.Reader
	}{
		{name: "work id source", workID: func() (string, error) { return "", errRandomFailed }, random: bytes.NewReader(make([]byte, nonceSize))},
		{name: "invalid generated work id", workID: fixedWorkID("work_invalid"), random: bytes.NewReader(make([]byte, nonceSize))},
		{name: "nonce source", workID: fixedWorkID(testWorkID), random: errReader{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			_, err := authenticate(context.Background(), client, testConfig(0x11), test.workID, test.random)
			if !errors.Is(err, ErrRandomSource) {
				t.Fatalf("authenticate() error = %v, want ErrRandomSource", err)
			}
		})
	}
}

func TestAuthenticateRejectsInvalidSessionAndLeaseInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "Tunnel ID", mutate: func(config *Config) { config.Session.TunnelID = "tun_invalid" }},
		{name: "Connector ID", mutate: func(config *Config) { config.Session.ConnectorID = "con_invalid" }},
		{name: "Session ID", mutate: func(config *Config) { config.Session.SessionID = "sess_invalid" }},
		{name: "Budget Lease ID", mutate: func(config *Config) { config.BudgetLeaseID = "lease_invalid" }},
		{name: "write timeout", mutate: func(config *Config) { config.WriteTimeout = 0 }},
		{name: "read timeout", mutate: func(config *Config) { config.ReadTimeout = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(0x11)
			test.mutate(&config)
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			_, err := Authenticate(context.Background(), client, config)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Authenticate() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestAuthenticateRejectsWrongWorkIDAndInvalidReadyCombinations(t *testing.T) {
	tests := []struct {
		name     string
		response *protocolv1.WorkReady
	}{
		{name: "wrong Work ID", response: &protocolv1.WorkReady{WorkId: otherWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY}},
		{name: "invalid Work ID", response: &protocolv1.WorkReady{WorkId: "work_invalid", Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY}},
		{name: "READY carries error", response: &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY, ErrorCode: protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID}},
		{name: "REJECTED carries OK", response: &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED}},
		{name: "REJECTED carries unknown code", response: &protocolv1.WorkReady{WorkId: testWorkID, Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED, ErrorCode: protocolv1.ErrorCode(99_999)}},
		{name: "status unspecified", response: &protocolv1.WorkReady{WorkId: testWorkID}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err, _ := exchange(t, testConfig(0x11), fixedWorkID(testWorkID), bytes.NewReader(make([]byte, nonceSize)), func(connection net.Conn, _ *protocolv1.WorkHello) error {
				return frame.WriteWork(connection, test.response)
			})
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Authenticate() error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestAuthenticateReturnsTypedRejectedErrorWithClosedState(t *testing.T) {
	_, err, _ := exchange(t, testConfig(0x11), fixedWorkID(testWorkID), bytes.NewReader(make([]byte, nonceSize)), func(connection net.Conn, hello *protocolv1.WorkHello) error {
		return frame.WriteWork(connection, &protocolv1.WorkReady{
			WorkId: hello.GetWorkId(), Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_REJECTED,
			ErrorCode: protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED,
		})
	})
	var rejected *Rejected
	if !errors.As(err, &rejected) {
		t.Fatalf("Authenticate() error = %v, want *Rejected", err)
	}
	if rejected.WorkID != testWorkID || rejected.Code != protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED ||
		rejected.State == nil || rejected.State.Phase() != state.WorkClosed {
		t.Fatalf("Rejected = %#v", rejected)
	}
}

func TestAuthenticateRejectsMalformedUnknownAndOversizedReady(t *testing.T) {
	tests := []struct {
		name    string
		handler func(net.Conn, *protocolv1.WorkHello) error
	}{
		{name: "malformed", handler: func(connection net.Conn, _ *protocolv1.WorkHello) error {
			return frame.WritePayload(connection, []byte{0xff}, frame.MaxWorkFrameSize)
		}},
		{name: "unknown field", handler: func(connection net.Conn, hello *protocolv1.WorkHello) error {
			response := &protocolv1.WorkReady{WorkId: hello.GetWorkId(), Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY}
			response.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
			return frame.WriteWork(connection, response)
		}},
		{name: "oversized", handler: func(connection net.Conn, _ *protocolv1.WorkHello) error {
			var prefix [binary.MaxVarintLen64]byte
			length := binary.PutUvarint(prefix[:], frame.MaxWorkFrameSize+1)
			_, err := connection.Write(prefix[:length])
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err, _ := exchange(t, testConfig(0x11), fixedWorkID(testWorkID), bytes.NewReader(make([]byte, nonceSize)), test.handler)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Authenticate() error = %v, want ErrProtocol", err)
			}
		})
	}
}

func TestAuthenticateWriteAndReadTimeouts(t *testing.T) {
	t.Run("write timeout", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		config := testConfig(0x11)
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
		readResult := make(chan error, 1)
		go func() {
			readResult <- frame.ReadWork(server, &protocolv1.WorkHello{})
		}()
		config := testConfig(0x11)
		config.ReadTimeout = 120 * time.Millisecond
		_, err := Authenticate(context.Background(), client, config)
		if !errors.Is(err, ErrReadTimeout) {
			t.Fatalf("Authenticate() error = %v, want ErrReadTimeout", err)
		}
		if err := <-readResult; err != nil {
			t.Fatalf("server ReadWork() error = %v", err)
		}
	})
}

func TestAuthenticateContextCancellationInterruptsBlockedIO(t *testing.T) {
	tests := []struct {
		name      string
		readHello bool
	}{
		{name: "write", readHello: false},
		{name: "read", readHello: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			var readResult chan error
			if test.readHello {
				readResult = make(chan error, 1)
				go func() { readResult <- frame.ReadWork(server, &protocolv1.WorkHello{}) }()
			}
			ctx, cancel := context.WithCancel(context.Background())
			timer := time.AfterFunc(80*time.Millisecond, cancel)
			defer timer.Stop()
			config := testConfig(0x11)
			config.WriteTimeout = 5 * time.Second
			config.ReadTimeout = 5 * time.Second
			_, err := Authenticate(ctx, client, config)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Authenticate() error = %v, want context.Canceled", err)
			}
			if readResult != nil {
				if err := <-readResult; err != nil {
					t.Fatalf("server ReadWork() error = %v", err)
				}
			}
		})
	}
}

func TestAuthenticateClearsTemporaryDeadlinesBeforeIdleHandoff(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	serverResult := make(chan error, 1)
	go func() {
		hello := &protocolv1.WorkHello{}
		if err := frame.ReadWork(server, hello); err != nil {
			serverResult <- err
			return
		}
		if err := frame.WriteWork(server, &protocolv1.WorkReady{WorkId: hello.GetWorkId(), Status: protocolv1.WorkReadyStatus_WORK_READY_STATUS_READY}); err != nil {
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
	config := testConfig(0x11)
	config.WriteTimeout = 120 * time.Millisecond
	config.ReadTimeout = 120 * time.Millisecond
	ready, err := Authenticate(context.Background(), client, config)
	if err != nil || ready.State.Phase() != state.WorkIdle {
		t.Fatalf("Authenticate() = (%#v, %v)", ready, err)
	}
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

func exchange(
	t *testing.T,
	config Config,
	newWorkID func() (string, error),
	random io.Reader,
	handler func(net.Conn, *protocolv1.WorkHello) error,
) (*Ready, error, *protocolv1.WorkHello) {
	t.Helper()
	client, server := net.Pipe()
	serverResult := make(chan error, 1)
	helloResult := make(chan *protocolv1.WorkHello, 1)
	go func() {
		hello := &protocolv1.WorkHello{}
		if err := frame.ReadWork(server, hello); err != nil {
			serverResult <- err
			return
		}
		helloResult <- hello
		serverResult <- handler(server, hello)
	}()
	ready, err := authenticate(context.Background(), client, config, newWorkID, random)
	_ = client.Close()
	_ = server.Close()
	serverErr := <-serverResult
	if serverErr != nil && !errors.Is(serverErr, net.ErrClosed) {
		t.Fatalf("server exchange error = %v", serverErr)
	}
	return ready, err, <-helloResult
}

func testConfig(secretByte byte) Config {
	var secret [32]byte
	for index := range secret {
		secret[index] = secretByte
	}
	return Config{
		Session: controlauth.Session{
			TunnelID: testTunnelID, ConnectorID: testConnectorID, SessionID: testSessionID,
			SessionSecret: secret,
		},
		BudgetLeaseID: testLeaseID,
		WriteTimeout:  time.Second,
		ReadTimeout:   time.Second,
	}
}

func fixedWorkID(value string) func() (string, error) {
	return func() (string, error) { return value, nil }
}

var errRandomFailed = errors.New("random failed")

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errRandomFailed
}

func TestRejectedErrorStringContainsNoSensitiveMaterial(t *testing.T) {
	rejected := &Rejected{WorkID: testWorkID, Code: protocolv1.ErrorCode_ERROR_CODE_SESSION_INVALID}
	want := "agent work auth rejected: work_id=" + testWorkID + " code=ERROR_CODE_SESSION_INVALID"
	if rejected.Error() != want {
		t.Fatalf("Rejected.Error() = %q, want %q", rejected.Error(), want)
	}
}

func TestIdentityValidationUsedByWorkAuth(t *testing.T) {
	if !identity.ValidTunnelID(testTunnelID) || !identity.ValidConnectorID(testConnectorID) || !identity.ValidSessionID(testSessionID) {
		t.Fatal("test fixture must remain valid under shared identity validation")
	}
}
