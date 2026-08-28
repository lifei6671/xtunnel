package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
)

const testConnectorThree = "con_01J00000000000000000000002"

func TestProxyConnectionIDFailureDoesNotAcquireConnectorOrWork(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	session := fixture.sessionsByConnector[testConnectorID]
	fixture.registerWork(t, session, nil)
	pool, exists := fixture.sessions.Pool(session)
	if !exists {
		t.Fatal("Session Pool does not exist before connection ID failure")
	}
	before := pool.Snapshot()
	wantErr := errors.New("connection ID random source failed")
	fixture.proxy.newConnectionID = func() (string, error) { return "", wantErr }

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	err := fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Proxy.Serve() error = %v, want injected connection ID failure", err)
	}
	after := pool.Snapshot()
	if after != before {
		t.Fatalf("Work Pool after connection ID failure = %#v, want unchanged %#v", after, before)
	}
	if snapshot := fixture.limits.Snapshot(); snapshot.PendingOpens != 0 || snapshot.ActiveTotal != 0 {
		t.Fatalf("limits after connection ID failure = %#v, want no Pending/Active lease", snapshot)
	}
	fixture.close(t)
}

func TestProxyReselectsAlternateConnectorWhenOnlyWorkFailsPreRaw(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
	failedRequest := make(chan *protocolv1.OpenRequest, 1)
	failedConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	failedResult := make(chan error, 1)
	go func() {
		defer failedConnection.Close()
		request := &protocolv1.OpenRequest{}
		if err := frame.ReadWork(failedConnection, request); err != nil {
			failedResult <- err
			return
		}
		failedRequest <- request
		failedResult <- nil
	}()

	alternateRequests := make(chan *protocolv1.OpenRequest, 1)
	alternatePayload := make(chan []byte, 1)
	alternateConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorTwo], nil)
	alternateResult := make(chan error, 1)
	go func() {
		alternateResult <- runRecordingEchoConnector(alternateConnection, alternateRequests, alternatePayload)
	}()

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	payload := []byte("single-work-pre-raw-failover")
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("write public payload: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite: %v", err)
	}
	echoed, err := io.ReadAll(publicClient)
	if err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("failover echo = %q, %v, want %q", echoed, err, payload)
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() waited for same-Pool replenishment instead of using alternate")
	}
	first := <-failedRequest
	alternate := <-alternateRequests
	if alternate.GetConnectionId() != first.GetConnectionId() {
		t.Fatalf("alternate connection_id = %q, want %q", alternate.GetConnectionId(), first.GetConnectionId())
	}
	if got := <-alternatePayload; !bytes.Equal(got, payload) {
		t.Fatalf("alternate RAW payload = %q, want %q", got, payload)
	}
	if err := <-failedResult; err != nil {
		t.Fatalf("failed WorkConn script error = %v", err)
	}
	if err := <-alternateResult; err != nil {
		t.Fatalf("alternate echo error = %v", err)
	}
	fixture.close(t)
}

func TestProxyTriesNextAlternateAfterFirstAlternateLosesIdleRace(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo, testConnectorThree)
	failedRequests := make(chan *protocolv1.OpenRequest, 1)
	failedConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	failedResult := make(chan error, 1)
	go func() {
		defer failedConnection.Close()
		request := &protocolv1.OpenRequest{}
		if err := frame.ReadWork(failedConnection, request); err != nil {
			failedResult <- err
			return
		}
		failedRequests <- request
		failedResult <- nil
	}()
	fixture.registerWork(t, fixture.sessionsByConnector[testConnectorTwo], nil)

	alternateRequests := make(chan *protocolv1.OpenRequest, 1)
	alternatePayload := make(chan []byte, 1)
	alternateConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorThree], nil)
	alternateResult := make(chan error, 1)
	go func() {
		alternateResult <- runRecordingEchoConnector(alternateConnection, alternateRequests, alternatePayload)
	}()
	contentionObserved := make(chan struct{}, 1)
	fixture.proxy.afterAlternateAcquire = func(session serverruntime.Session) {
		if session.ConnectorID != testConnectorTwo {
			return
		}
		pool, exists := fixture.sessions.Pool(session)
		if !exists {
			t.Errorf("first alternate Pool disappeared before controlled contention")
			return
		}
		work, acquired, err := pool.TryAcquire()
		if err != nil || !acquired {
			t.Errorf("controlled contention TryAcquire() = %v, %v", acquired, err)
			return
		}
		_ = work.Close()
		contentionObserved <- struct{}{}
	}

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	payload := []byte("next-alternate-after-contention")
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("write public payload: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite: %v", err)
	}
	echoed, err := io.ReadAll(publicClient)
	if err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("failover echo = %q, %v, want %q", echoed, err, payload)
	}
	select {
	case <-contentionObserved:
	case <-time.After(testTimeout):
		t.Fatal("first alternate contention was not exercised")
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not continue to the third Connector")
	}
	firstRequest := <-failedRequests
	if request := <-alternateRequests; request.GetConnectionId() != firstRequest.GetConnectionId() {
		t.Fatalf("third Connector connection_id = %q, want %q", request.GetConnectionId(), firstRequest.GetConnectionId())
	}
	if got := <-alternatePayload; !bytes.Equal(got, payload) {
		t.Fatalf("third Connector RAW payload = %q, want %q", got, payload)
	}
	if err := <-failedResult; err != nil {
		t.Fatalf("failed WorkConn script error = %v", err)
	}
	if err := <-alternateResult; err != nil {
		t.Fatalf("third Connector echo error = %v", err)
	}
	fixture.close(t)
}

func TestProxyReselectsAlternateConnectorAfterTwoPreRawTransportFailures(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
	failedRequests := make(chan *protocolv1.OpenRequest, 2)
	failedResults := make([]<-chan error, 0, 2)
	for range 2 {
		connection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
		result := make(chan error, 1)
		failedResults = append(failedResults, result)
		go func() {
			defer connection.Close()
			request := &protocolv1.OpenRequest{}
			if err := frame.ReadWork(connection, request); err != nil {
				result <- err
				return
			}
			failedRequests <- request
			result <- nil
		}()
	}

	alternateRequests := make(chan *protocolv1.OpenRequest, 1)
	alternatePayload := make(chan []byte, 1)
	alternateConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorTwo], nil)
	alternateResult := make(chan error, 1)
	go func() {
		alternateResult <- runRecordingEchoConnector(alternateConnection, alternateRequests, alternatePayload)
	}()

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	payload := bytes.Repeat([]byte("pre-raw-failover-"), 64)
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("write public payload: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite: %v", err)
	}
	echoed, err := io.ReadAll(publicClient)
	if err != nil {
		t.Fatalf("read failover echo: %v", err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatalf("failover echo = %d bytes, want %d", len(echoed), len(payload))
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not finish after alternate Connector echo")
	}

	requests := make([]*protocolv1.OpenRequest, 0, 3)
	for range 2 {
		select {
		case request := <-failedRequests:
			requests = append(requests, request)
		case <-time.After(testTimeout):
			t.Fatal("failed Connector did not receive both OpenRequests")
		}
	}
	select {
	case request := <-alternateRequests:
		requests = append(requests, request)
	case <-time.After(testTimeout):
		t.Fatal("alternate Connector did not receive OpenRequest")
	}
	for index, request := range requests[1:] {
		if request.GetConnectionId() != requests[0].GetConnectionId() {
			t.Fatalf("OpenRequest %d connection_id = %q, want %q", index+1, request.GetConnectionId(), requests[0].GetConnectionId())
		}
	}
	select {
	case got := <-alternatePayload:
		if !bytes.Equal(got, payload) {
			t.Fatalf("alternate payload changed: got=%d want=%d", len(got), len(payload))
		}
	case <-time.After(testTimeout):
		t.Fatal("alternate Connector did not receive RAW payload")
	}
	for index, result := range failedResults {
		if err := <-result; err != nil {
			t.Fatalf("failed WorkConn %d script error = %v", index, err)
		}
	}
	if err := <-alternateResult; err != nil {
		t.Fatalf("alternate echo error = %v", err)
	}
	fixture.close(t)
}

func TestProxyReselectsAlternateConnectorImmediatelyOnOpenDraining(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
	firstSession := fixture.sessionsByConnector[testConnectorID]
	drainingRequests := make(chan *protocolv1.OpenRequest, 1)
	drainingConnection := fixture.registerWork(t, firstSession, nil)
	drainingResult := make(chan error, 1)
	go func() {
		defer drainingConnection.Close()
		request := &protocolv1.OpenRequest{}
		if err := frame.ReadWork(drainingConnection, request); err != nil {
			drainingResult <- err
			return
		}
		drainingRequests <- request
		drainingResult <- frame.WriteWork(drainingConnection, &protocolv1.OpenResponse{
			ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_ERROR,
			ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OPEN_DRAINING,
		})
	}()
	// 首 Connector 仍有第二个 IDLE；OPEN_DRAINING 必须直接跨 Connector，不能消费它。
	fixture.registerWork(t, firstSession, nil)

	alternateRequests := make(chan *protocolv1.OpenRequest, 1)
	alternatePayload := make(chan []byte, 1)
	alternateConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorTwo], nil)
	alternateResult := make(chan error, 1)
	go func() {
		alternateResult <- runRecordingEchoConnector(alternateConnection, alternateRequests, alternatePayload)
	}()

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	payload := []byte("open-draining-failover")
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("write public payload: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite: %v", err)
	}
	echoed, err := io.ReadAll(publicClient)
	if err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("failover echo = %q, %v, want %q", echoed, err, payload)
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not finish after OPEN_DRAINING failover")
	}

	firstRequest := <-drainingRequests
	alternateRequest := <-alternateRequests
	if alternateRequest.GetConnectionId() != firstRequest.GetConnectionId() {
		t.Fatalf("alternate connection_id = %q, want %q", alternateRequest.GetConnectionId(), firstRequest.GetConnectionId())
	}
	firstPool, exists := fixture.sessions.Pool(firstSession)
	if !exists || firstPool.Snapshot().Idle != 1 {
		t.Fatalf("first Connector Pool = %#v, %v, want untouched second IDLE", firstPool, exists)
	}
	if got := <-alternatePayload; !bytes.Equal(got, payload) {
		t.Fatalf("alternate RAW payload = %q, want %q", got, payload)
	}
	if err := <-drainingResult; err != nil {
		t.Fatalf("OPEN_DRAINING script error = %v", err)
	}
	if err := <-alternateResult; err != nil {
		t.Fatalf("alternate echo error = %v", err)
	}
	fixture.close(t)
}

func TestProxyReturnsAfterPreRawFailuresWhenNoAlternateIdleExists(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
	failedResults := make([]<-chan error, 0, 2)
	for range 2 {
		connection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
		result := make(chan error, 1)
		failedResults = append(failedResults, result)
		go func() {
			defer connection.Close()
			request := &protocolv1.OpenRequest{}
			result <- frame.ReadWork(connection, request)
		}()
	}

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	result := make(chan error, 1)
	go func() {
		result <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	select {
	case err := <-result:
		if !errors.Is(err, serveropen.ErrPreRAWTransport) || !errors.Is(err, serverruntime.ErrNoAvailableConnector) {
			t.Fatalf("Proxy.Serve() error = %v, want transport failure joined with no alternate", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() created a Pending Group instead of returning without alternate IDLE")
	}
	for index, failedResult := range failedResults {
		if err := <-failedResult; err != nil {
			t.Fatalf("failed WorkConn %d script error = %v", index, err)
		}
	}
	fixture.proxy.pendingMu.Lock()
	pendingGroups := len(fixture.proxy.pendingGroups)
	fixture.proxy.pendingMu.Unlock()
	if pendingGroups != 0 {
		t.Fatalf("pending groups after alternate miss = %d, want 0", pendingGroups)
	}
	if snapshot := fixture.limits.Snapshot(); snapshot.PendingOpens != 0 || snapshot.ActiveTotal != 0 {
		t.Fatalf("limits after alternate miss = %#v, want no Pending/Active lease", snapshot)
	}
	fixture.close(t)
}

func TestProxyDoesNotReselectAfterNonRetryableOpenFailure(t *testing.T) {
	tests := []struct {
		name     string
		wrap     func(net.Conn) net.Conn
		response func(*protocolv1.OpenRequest) *protocolv1.OpenResponse
		want     error
	}{
		{
			name: "protocol mismatch",
			response: func(*protocolv1.OpenRequest) *protocolv1.OpenResponse {
				return &protocolv1.OpenResponse{
					ConnectionId: "conn_01J00000000000000000000001",
					Status:       protocolv1.OpenStatus_OPEN_STATUS_OK, ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
				}
			},
			want: serveropen.ErrProtocol,
		},
		{
			name: "origin rejection",
			response: func(request *protocolv1.OpenRequest) *protocolv1.OpenResponse {
				return &protocolv1.OpenResponse{
					ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_ERROR,
					ErrorCode: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT,
				}
			},
			want: serveropen.ErrRejected,
		},
		{
			name: "RAW committed",
			wrap: func(connection net.Conn) net.Conn {
				return &failTunnelClearDeadlineConn{Conn: connection}
			},
			response: func(request *protocolv1.OpenRequest) *protocolv1.OpenResponse {
				return &protocolv1.OpenResponse{
					ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
					ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
				}
			},
			want: serveropen.ErrRawCommitted,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
			firstConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], test.wrap)
			firstResult := make(chan error, 1)
			go func() {
				defer firstConnection.Close()
				request := &protocolv1.OpenRequest{}
				if err := frame.ReadWork(firstConnection, request); err != nil {
					firstResult <- err
					return
				}
				firstResult <- frame.WriteWork(firstConnection, test.response(request))
			}()
			fixture.registerWork(t, fixture.sessionsByConnector[testConnectorTwo], nil)

			serverPeer, publicClient := tcpPair(t)
			defer publicClient.Close()
			result := make(chan error, 1)
			go func() {
				result <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
			}()
			select {
			case err := <-result:
				if !errors.Is(err, test.want) {
					t.Fatalf("Proxy.Serve() error = %v, want %v", err, test.want)
				}
			case <-time.After(testTimeout):
				t.Fatal("Proxy.Serve() did not return after non-retryable OPEN failure")
			}
			if err := <-firstResult; err != nil {
				t.Fatalf("first Connector script error = %v", err)
			}
			alternateSession := fixture.sessionsByConnector[testConnectorTwo]
			alternatePool, exists := fixture.sessions.Pool(alternateSession)
			if !exists || alternatePool.Snapshot().Idle != 1 {
				t.Fatalf("alternate Pool = %#v, %v, want untouched IDLE WorkConn", alternatePool, exists)
			}
			fixture.close(t)
		})
	}
}

func TestProxyDoesNotReselectAfterContextCancellation(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
	firstConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	requestRead := make(chan struct{})
	firstResult := make(chan error, 1)
	go func() {
		defer firstConnection.Close()
		request := &protocolv1.OpenRequest{}
		if err := frame.ReadWork(firstConnection, request); err != nil {
			firstResult <- err
			return
		}
		close(requestRead)
		_, err := io.Copy(io.Discard, firstConnection)
		firstResult <- err
	}()
	fixture.registerWork(t, fixture.sessionsByConnector[testConnectorTwo], nil)

	ctx, cancel := context.WithCancel(context.Background())
	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	result := make(chan error, 1)
	go func() {
		result <- fixture.proxy.Serve(ctx, testTCPDialRequest(), serverPeer)
	}()
	select {
	case <-requestRead:
		cancel()
	case <-time.After(testTimeout):
		t.Fatal("first Connector did not receive OpenRequest before cancellation")
	}
	select {
	case err := <-result:
		if err == nil || ctx.Err() == nil {
			t.Fatalf("Proxy.Serve() error = %v, want cancellation failure", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not return after Context cancellation")
	}
	if err := <-firstResult; err != nil {
		t.Fatalf("first Connector cancellation script error = %v", err)
	}
	alternateSession := fixture.sessionsByConnector[testConnectorTwo]
	alternatePool, exists := fixture.sessions.Pool(alternateSession)
	if !exists || alternatePool.Snapshot().Idle != 1 {
		t.Fatalf("alternate Pool = %#v, %v, want untouched IDLE after cancellation", alternatePool, exists)
	}
	fixture.close(t)
}

func TestProxyStopsAlternateReselectWhenContextCanceledAfterLease(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
	failedConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	failedResult := make(chan error, 1)
	go func() {
		defer failedConnection.Close()
		request := &protocolv1.OpenRequest{}
		failedResult <- frame.ReadWork(failedConnection, request)
	}()
	alternateSession := fixture.sessionsByConnector[testConnectorTwo]
	fixture.registerWork(t, alternateSession, nil)

	ctx, cancel := context.WithCancel(context.Background())
	fixture.proxy.afterAlternateAcquire = func(session serverruntime.Session) {
		if session == alternateSession {
			cancel()
		}
	}
	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	result := make(chan error, 1)
	go func() {
		result <- fixture.proxy.Serve(ctx, testTCPDialRequest(), serverPeer)
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Proxy.Serve() error = %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not stop after controlled alternate cancellation")
	}
	if err := <-failedResult; err != nil {
		t.Fatalf("failed WorkConn script error = %v", err)
	}
	alternatePool, exists := fixture.sessions.Pool(alternateSession)
	if !exists || alternatePool.Snapshot().Idle != 1 {
		t.Fatalf("alternate Pool = %#v, %v, want untouched IDLE after cancellation", alternatePool, exists)
	}
	fixture.close(t)
}

func TestProxyUsesRemainingConnectorAfterControlCrash(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
	fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	alternateRequests := make(chan *protocolv1.OpenRequest, 1)
	alternatePayload := make(chan []byte, 1)
	alternateConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorTwo], nil)
	alternateResult := make(chan error, 1)
	go func() {
		alternateResult <- runRecordingEchoConnector(alternateConnection, alternateRequests, alternatePayload)
	}()
	fixture.disconnectConnector(t, 0, testConnectorID)

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	payload := []byte("new-public-connection-after-control-crash")
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("write public payload: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite: %v", err)
	}
	echoed, err := io.ReadAll(publicClient)
	if err != nil || !bytes.Equal(echoed, payload) {
		t.Fatalf("post-crash echo = %q, %v, want %q", echoed, err, payload)
	}
	select {
	case err := <-proxyResult:
		if err != nil {
			t.Fatalf("Proxy.Serve() error = %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not use remaining Connector after Control crash")
	}
	select {
	case request := <-alternateRequests:
		if request.GetConnectionId() == "" {
			t.Fatal("remaining Connector received empty connection_id")
		}
	case <-time.After(testTimeout):
		t.Fatal("remaining Connector did not receive OpenRequest")
	}
	if got := <-alternatePayload; !bytes.Equal(got, payload) {
		t.Fatalf("remaining Connector RAW payload = %q, want %q", got, payload)
	}
	if err := <-alternateResult; err != nil {
		t.Fatalf("remaining Connector echo error = %v", err)
	}
	fixture.close(t)
}

func TestProxyDoesNotReplayAfterRawBusinessBytes(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID, testConnectorTwo)
	firstConnection := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	firstPayload := make(chan []byte, 1)
	firstResult := make(chan error, 1)
	payload := []byte("business-bytes-committed-to-first-connector")
	go func() {
		defer firstConnection.Close()
		request := &protocolv1.OpenRequest{}
		if err := frame.ReadWork(firstConnection, request); err != nil {
			firstResult <- err
			return
		}
		if err := frame.WriteWork(firstConnection, &protocolv1.OpenResponse{
			ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
			ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
		}); err != nil {
			firstResult <- err
			return
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(firstConnection, got); err != nil {
			firstResult <- err
			return
		}
		firstPayload <- got
		firstResult <- nil
	}()
	alternateSession := fixture.sessionsByConnector[testConnectorTwo]
	fixture.registerWork(t, alternateSession, nil)

	serverPeer, publicClient := tcpPair(t)
	defer publicClient.Close()
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- fixture.proxy.Serve(context.Background(), testTCPDialRequest(), serverPeer)
	}()
	if _, err := publicClient.Write(payload); err != nil {
		t.Fatalf("write public payload: %v", err)
	}
	if err := publicClient.CloseWrite(); err != nil {
		t.Fatalf("public CloseWrite: %v", err)
	}
	select {
	case got := <-firstPayload:
		if !bytes.Equal(got, payload) {
			t.Fatalf("first Connector RAW payload = %q, want %q", got, payload)
		}
	case <-time.After(testTimeout):
		t.Fatal("first Connector did not receive committed RAW business bytes")
	}
	select {
	case <-proxyResult:
	case <-time.After(testTimeout):
		t.Fatal("Proxy.Serve() did not finish after RAW transport failure")
	}
	if err := <-firstResult; err != nil {
		t.Fatalf("first Connector RAW script error = %v", err)
	}
	alternatePool, exists := fixture.sessions.Pool(alternateSession)
	if !exists || alternatePool.Snapshot().Idle != 1 {
		t.Fatalf("alternate Pool = %#v, %v, want untouched IDLE after RAW commit", alternatePool, exists)
	}
	fixture.close(t)
}

type failoverFixture struct {
	registry            *serverruntime.Registry
	sessions            *sessionruntime.Manager
	proxy               *Proxy
	limits              *serverlimits.Manager
	sessionsByConnector map[string]serverruntime.Session
	controlPeers        []net.Conn
	controlResults      []<-chan error
	agentWorks          []net.Conn
}

func newFailoverFixture(t *testing.T, connectorIDs ...string) *failoverFixture {
	t.Helper()
	return newFailoverFixtureWithLimits(t, newLimitManager(t, 16), connectorIDs...)
}

func newFailoverFixtureWithLimits(
	t *testing.T,
	limits *serverlimits.Manager,
	connectorIDs ...string,
) *failoverFixture {
	t.Helper()
	registry := serverruntime.NewRegistryWithLimits(limits)
	sessions, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 16, NormalCapacity: 32, InboundCapacity: 16,
		WriteTimeout: time.Second, MaxReplayEntries: 128,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, LimitManager: limits,
		SnapshotProvider: tunnelSnapshotProvider{},
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
	startSessionManager(t, sessions)
	fixture := &failoverFixture{
		registry: registry, sessions: sessions, limits: limits,
		sessionsByConnector: make(map[string]serverruntime.Session, len(connectorIDs)),
	}
	for _, connectorID := range connectorIDs {
		pending, reserveErr := registry.ReserveAuthenticated(testTunnelID, connectorID)
		if reserveErr != nil {
			t.Fatalf("ReserveAuthenticated(%s) error = %v", connectorID, reserveErr)
		}
		session, commitErr := registry.CommitAuthenticated(pending)
		if commitErr != nil {
			t.Fatalf("CommitAuthenticated(%s) error = %v", connectorID, commitErr)
		}
		fixture.sessionsByConnector[connectorID] = session
		controlServer, controlAgent := net.Pipe()
		fixture.controlPeers = append(fixture.controlPeers, controlAgent)
		established := establishedControl(t, session)
		result := make(chan error, 1)
		fixture.controlResults = append(fixture.controlResults, result)
		go func() { result <- sessions.Serve(context.Background(), controlServer, &established) }()
		readDemand(t, controlAgent)
	}
	openHandler, err := serveropen.NewHandler(serveropen.Options{HandshakeTimeout: time.Second, WriteTimeout: time.Second, ReadTimeout: time.Second})
	if err != nil {
		t.Fatalf("open.NewHandler() error = %v", err)
	}
	fixture.proxy, err = NewProxy(Options{
		Registry: registry, Sessions: sessions, OpenHandler: openHandler,
		AcquireTimeout: time.Second, LimitManager: limits,
	})
	if err != nil {
		t.Fatalf("NewProxy() error = %v", err)
	}
	return fixture
}

func (fixture *failoverFixture) registerWork(t *testing.T, session serverruntime.Session, wrap func(net.Conn) net.Conn) net.Conn {
	t.Helper()
	serverWork, agentWork := tcpPair(t)
	var registeredConnection net.Conn = serverWork
	if wrap != nil {
		registeredConnection = wrap(serverWork)
	}
	workID, err := identity.NewWorkID()
	if err != nil {
		_ = serverWork.Close()
		_ = agentWork.Close()
		t.Fatalf("NewWorkID() error = %v", err)
	}
	if _, err := fixture.sessions.RegisterIdle(
		registeredConnection,
		authenticatedIdleWithWorkID(t, session, workID),
	); err != nil {
		_ = serverWork.Close()
		_ = agentWork.Close()
		t.Fatalf("RegisterIdle(%s) error = %v", workID, err)
	}
	fixture.agentWorks = append(fixture.agentWorks, agentWork)
	return agentWork
}

func (fixture *failoverFixture) close(t *testing.T) {
	t.Helper()
	for _, peer := range fixture.controlPeers {
		if peer != nil {
			_ = peer.Close()
		}
	}
	for index, result := range fixture.controlResults {
		if result == nil {
			continue
		}
		select {
		case <-result:
		case <-time.After(testTimeout):
			t.Fatalf("Control Session %d did not finish", index)
		}
	}
	for _, connection := range fixture.agentWorks {
		_ = connection.Close()
	}
	waitForSnapshot(t, fixture.limits, func(snapshot serverlimits.Snapshot) bool {
		return snapshot.Connectors == 0 && snapshot.WorkTotal == 0 && snapshot.PendingOpens == 0 && snapshot.ActiveTotal == 0
	})
	fixture.proxy.pendingMu.Lock()
	pendingGroups := len(fixture.proxy.pendingGroups)
	fixture.proxy.pendingMu.Unlock()
	if pendingGroups != 0 {
		t.Fatalf("pending groups after failover cleanup = %d, want 0", pendingGroups)
	}
}

func (fixture *failoverFixture) disconnectConnector(t *testing.T, index int, connectorID string) {
	t.Helper()
	if index < 0 || index >= len(fixture.controlPeers) || fixture.controlPeers[index] == nil || fixture.controlResults[index] == nil {
		t.Fatalf("invalid Control Session index %d", index)
	}
	_ = fixture.controlPeers[index].Close()
	select {
	case <-fixture.controlResults[index]:
	case <-time.After(testTimeout):
		t.Fatalf("Control Session %d did not finish after crash", index)
	}
	fixture.controlPeers[index] = nil
	fixture.controlResults[index] = nil
	waitFor(t, func() bool {
		_, exists := fixture.registry.Current(testTunnelID, connectorID)
		return !exists
	})
	if _, exists := fixture.sessions.Pool(fixture.sessionsByConnector[connectorID]); exists {
		t.Fatalf("crashed Connector %s still has a selectable Pool", connectorID)
	}
}

func runRecordingEchoConnector(
	connection net.Conn,
	requests chan<- *protocolv1.OpenRequest,
	payloads chan<- []byte,
) error {
	defer connection.Close()
	request := &protocolv1.OpenRequest{}
	if err := frame.ReadWork(connection, request); err != nil {
		return err
	}
	requests <- request
	if err := frame.WriteWork(connection, &protocolv1.OpenResponse{
		ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
		ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK,
	}); err != nil {
		return err
	}
	payload, err := io.ReadAll(connection)
	if err != nil {
		return err
	}
	payloads <- payload
	if _, err := connection.Write(payload); err != nil {
		return err
	}
	if closeWriter, ok := connection.(interface{ CloseWrite() error }); ok {
		return closeWriter.CloseWrite()
	}
	return nil
}

type failTunnelClearDeadlineConn struct{ net.Conn }

func (connection *failTunnelClearDeadlineConn) SetDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return io.ErrClosedPipe
	}
	return connection.Conn.SetDeadline(deadline)
}
