package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serveropen "github.com/lifei6671/xtunnel/internal/server/open"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

func TestProxyMetricsRecordsSuccessfulOriginConnectLatency(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	metrics := &recordingTunnelMetrics{}
	usage := &recordingTunnelUsage{}
	fixture.proxy.options.Metrics = metrics
	fixture.proxy.options.Usage = usage
	agent := fixture.registerWork(t, fixture.sessionsByConnector[testConnectorID], nil)
	agentResult := make(chan error, 1)
	go func() {
		request := &protocolv1.OpenRequest{}
		if err := frame.ReadWork(agent, request); err != nil {
			agentResult <- err
			return
		}
		if err := frame.WriteWork(agent, &protocolv1.OpenResponse{
			ConnectionId: request.GetConnectionId(), Status: protocolv1.OpenStatus_OPEN_STATUS_OK,
			ErrorCode: protocolv1.ErrorCode_ERROR_CODE_OK, OriginConnectLatencyMs: 250,
		}); err != nil {
			agentResult <- err
			return
		}
		_, err := io.Copy(io.Discard, agent)
		agentResult <- err
	}()

	connection, err := fixture.proxy.Dial(
		context.Background(), testHTTPDialRequest(0, testDialClientAddr),
	)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitDialAgent(t, agentResult)
	snapshot := assertSingleOpenMetric(t, metrics, protocolv1.ErrorCode_ERROR_CODE_OK)
	if len(snapshot.originConnect) != 1 || snapshot.originConnect[0] != 250*time.Millisecond {
		t.Fatalf("origin connect metrics = %v, want [250ms]", snapshot.originConnect)
	}
	if len(snapshot.originErrors) != 0 {
		t.Fatalf("successful OPEN emitted origin error metrics: %v", snapshot.originErrors)
	}
	usageSnapshot := usage.snapshot()
	if usageSnapshot.successfulOpens != 1 || usageSnapshot.failedOpens != 0 {
		t.Fatalf("usage OPEN snapshot = %#v, want one successful OPEN", usageSnapshot)
	}
}

func TestProxyUsageRecordsFinalOpenFailureOnce(t *testing.T) {
	fixture := newFailoverFixture(t, testConnectorID)
	defer cleanupDialFixture(t, fixture)
	usage := &recordingTunnelUsage{}
	diagnostics := &recordingTunnelDiagnostics{}
	fixture.proxy.options.Usage = usage
	fixture.proxy.options.Diagnostics = diagnostics

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := fixture.proxy.Dial(ctx, testHTTPDialRequest(0, testDialClientAddr)); err == nil {
		t.Fatal("Dial() error = nil, want final OPEN failure")
	}
	usageSnapshot := usage.snapshot()
	if usageSnapshot.failedOpens != 1 || usageSnapshot.successfulOpens != 0 {
		t.Fatalf("usage OPEN snapshot = %#v, want one final failure", usageSnapshot)
	}
	observations := diagnostics.snapshot()
	if len(observations) != 1 || observations[0].code != protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR ||
		observations[0].requestID != testHTTPDialRequest(0, testDialClientAddr).RequestID {
		t.Fatalf("diagnostic OPEN observations = %#v, want one final failure with request_id", observations)
	}
}

func TestMeteredConnectionUsageDirectionsAndErrors(t *testing.T) {
	usageErr := errors.New("usage capacity")
	usage := &recordingTunnelUsage{egressErr: usageErr, ingressErr: usageErr}
	connection := &meteredConnection{
		Conn: &byteTestConn{readData: []byte("origin")}, usage: usage,
		tunnelID: testTunnelID, serviceID: testServiceID,
	}

	buffer := make([]byte, len("origin"))
	read, err := connection.Read(buffer)
	if read != len("origin") || string(buffer) != "origin" || !errors.Is(err, usageErr) {
		t.Fatalf("Read() = (%d, %q, %v), want delivered egress plus usage error", read, buffer, err)
	}
	written, err := connection.Write([]byte("public"))
	if written != len("public") || !errors.Is(err, usageErr) {
		t.Fatalf("Write() = (%d, %v), want delivered ingress plus usage error", written, err)
	}
	usageSnapshot := usage.snapshot()
	if usageSnapshot.egressBytes != uint64(len("origin")) || usageSnapshot.ingressBytes != uint64(len("public")) {
		t.Fatalf("usage byte snapshot = %#v", usageSnapshot)
	}
}

type openMetricObservation struct {
	duration time.Duration
	code     protocolv1.ErrorCode
}

type diagnosticOpenObservation struct {
	code      protocolv1.ErrorCode
	requestID string
}

type recordingTunnelDiagnostics struct {
	mu           sync.Mutex
	observations []diagnosticOpenObservation
}

func (diagnostics *recordingTunnelDiagnostics) ObserveOpen(code protocolv1.ErrorCode, requestID string) {
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	diagnostics.observations = append(diagnostics.observations, diagnosticOpenObservation{code: code, requestID: requestID})
}

func (diagnostics *recordingTunnelDiagnostics) snapshot() []diagnosticOpenObservation {
	diagnostics.mu.Lock()
	defer diagnostics.mu.Unlock()
	return append([]diagnosticOpenObservation(nil), diagnostics.observations...)
}

type recordingTunnelMetrics struct {
	mu sync.Mutex

	opens         []openMetricObservation
	originErrors  []protocolv1.ErrorCode
	originConnect []time.Duration
	ingressBytes  uint64
	egressBytes   uint64
}

type tunnelUsageSnapshot struct {
	successfulOpens uint64
	failedOpens     uint64
	ingressBytes    uint64
	egressBytes     uint64
}

type recordingTunnelUsage struct {
	mu sync.Mutex

	tunnelUsageSnapshot
	ingressErr error
	egressErr  error
	openErr    error
}

func (usage *recordingTunnelUsage) ObserveOpen(_, _ string, success bool) error {
	usage.mu.Lock()
	defer usage.mu.Unlock()
	if success {
		usage.successfulOpens++
	} else {
		usage.failedOpens++
	}
	return usage.openErr
}

func (usage *recordingTunnelUsage) AddIngressBytes(_, _ string, count uint64) error {
	usage.mu.Lock()
	defer usage.mu.Unlock()
	usage.ingressBytes += count
	return usage.ingressErr
}

func (usage *recordingTunnelUsage) AddEgressBytes(_, _ string, count uint64) error {
	usage.mu.Lock()
	defer usage.mu.Unlock()
	usage.egressBytes += count
	return usage.egressErr
}

func (usage *recordingTunnelUsage) snapshot() tunnelUsageSnapshot {
	usage.mu.Lock()
	defer usage.mu.Unlock()
	return usage.tunnelUsageSnapshot
}

type byteTestConn struct {
	net.Conn
	readData []byte
}

func (connection *byteTestConn) Read(buffer []byte) (int, error) {
	read := copy(buffer, connection.readData)
	connection.readData = connection.readData[read:]
	return read, io.EOF
}

func (*byteTestConn) Write(buffer []byte) (int, error) {
	return len(buffer), nil
}

func (metrics *recordingTunnelMetrics) ObserveOpen(duration time.Duration, code protocolv1.ErrorCode) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.opens = append(metrics.opens, openMetricObservation{duration: duration, code: code})
}

func (metrics *recordingTunnelMetrics) ObserveOriginError(code protocolv1.ErrorCode) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.originErrors = append(metrics.originErrors, code)
}

func (metrics *recordingTunnelMetrics) ObserveOriginConnect(duration time.Duration) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.originConnect = append(metrics.originConnect, duration)
}

func (metrics *recordingTunnelMetrics) AddIngressBytes(count uint64) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.ingressBytes += count
}

func (metrics *recordingTunnelMetrics) AddEgressBytes(count uint64) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.egressBytes += count
}

type tunnelMetricsSnapshot struct {
	opens         []openMetricObservation
	originErrors  []protocolv1.ErrorCode
	originConnect []time.Duration
	ingressBytes  uint64
	egressBytes   uint64
}

func (metrics *recordingTunnelMetrics) snapshot() tunnelMetricsSnapshot {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	return tunnelMetricsSnapshot{
		opens:         append([]openMetricObservation(nil), metrics.opens...),
		originErrors:  append([]protocolv1.ErrorCode(nil), metrics.originErrors...),
		originConnect: append([]time.Duration(nil), metrics.originConnect...),
		ingressBytes:  metrics.ingressBytes,
		egressBytes:   metrics.egressBytes,
	}
}

func assertSingleOpenMetric(t *testing.T, metrics *recordingTunnelMetrics, want protocolv1.ErrorCode) tunnelMetricsSnapshot {
	t.Helper()
	snapshot := metrics.snapshot()
	if len(snapshot.opens) != 1 {
		t.Fatalf("OPEN metric count = %d, want exactly 1; snapshot=%#v", len(snapshot.opens), snapshot)
	}
	if snapshot.opens[0].code != want {
		t.Fatalf("OPEN metric code = %s, want %s", snapshot.opens[0].code, want)
	}
	if snapshot.opens[0].duration < 0 {
		t.Fatalf("OPEN duration = %s, want non-negative", snapshot.opens[0].duration)
	}
	return snapshot
}

func TestTunnelMetricErrorCodeIsFinite(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want protocolv1.ErrorCode
	}{
		{name: "success", want: protocolv1.ErrorCode_ERROR_CODE_OK},
		{
			name: "public origin rejection",
			err:  &serveropen.Rejected{Code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT},
			want: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT,
		},
		{
			name: "unknown public code",
			err:  &serveropen.Rejected{Code: protocolv1.ErrorCode(0x7fffffff)},
			want: protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR,
		},
		{name: "protocol", err: serveropen.ErrProtocol, want: protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR},
		{name: "capacity", err: serverlimits.ErrPendingOpenCapacity, want: protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED},
		{name: "offline", err: serverruntime.ErrNoAvailableConnector, want: protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE},
		{name: "cancel", err: context.Canceled, want: protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR},
		{name: "internal", err: errors.New("private failure"), want: protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := tunnelMetricErrorCode(test.err); got != test.want {
				t.Fatalf("tunnelMetricErrorCode() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestOriginErrorCodeUsesOnlyProtocolOriginRange(t *testing.T) {
	tests := []struct {
		code protocolv1.ErrorCode
		want bool
	}{
		{code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED, want: true},
		{code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT, want: true},
		{code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE, want: true},
		{code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_RESET, want: true},
		{code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TLS_ERROR, want: true},
		{code: protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, want: false},
		{code: protocolv1.ErrorCode_ERROR_CODE_OK, want: false},
	}
	for _, test := range tests {
		if got := isOriginErrorCode(test.code); got != test.want {
			t.Errorf("isOriginErrorCode(%s) = %v, want %v", test.code, got, test.want)
		}
	}
}
