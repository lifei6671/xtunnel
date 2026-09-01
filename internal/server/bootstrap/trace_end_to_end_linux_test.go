//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/connector"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	internaltracing "github.com/lifei6671/xtunnel/internal/tracing"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const traceGateUntrustedTraceparent = "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01"

// TestOpenTelemetryTraceEndToEnd 通过生产 Bootstrap、Gateway、Token-only Agent、
// HTTP Tunnel 和 Origin Socket 验证 M6-03 的跨进程 Trace。两个进程使用独立
// TracerProvider，共享的线程安全 Recorder 只替代外部 Collector，不替代任何协议
// 或数据面组件；公网 traceparent 必须被本地 Root 隔离。
func TestOpenTelemetryTraceEndToEnd(t *testing.T) {
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)

	recorder := tracetest.NewSpanRecorder()
	serverTracing := newTraceGateRuntime(t, recorder, "xtunnel-server")
	agentTracing := newTraceGateRuntime(t, recorder, "xtunnel-agent")
	serverLogs := new(traceGateLockedBuffer)
	agentLogs := new(traceGateLockedBuffer)
	serverLogger := newTraceGateLogger(t, serverLogs, "server")
	agentLogger := newTraceGateLogger(t, agentLogs, "agent")

	httpOrigin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "trace-gate-ok")
	}))
	t.Cleanup(httpOrigin.Close)
	tcpOrigin := startProductGateTCPOrigin(t)
	_, publicPort := reserveProductGateTCPPort(t)
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("open Trace Gate Server storage: %v", err)
	}
	resourcesClosed := false
	t.Cleanup(func() {
		if !resourcesClosed {
			if err := resources.Close(); err != nil {
				t.Errorf("close Trace Gate Server storage: %v", err)
			}
		}
	})
	seedProductGateDesiredState(
		t, serverContext, resources, httpOrigin.Listener.Addr(), tcpOrigin.listener.Addr(), publicPort,
	)
	if err := resources.database.CreateFirstAdmin(serverContext, "admin", "trace gate password"); err != nil {
		t.Fatalf("create Trace Gate Admin: %v", err)
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)
	config.Limits.MaxWorkConnections = 1
	config.Limits.MaxIdleWorkConnections = 1
	config.Limits.MaxConnectingWorkConnections = 1
	config.Limits.MaxPendingOpens = 1
	config.Limits.MaxActiveConnections = 2
	config.Limits.MaxConnectionsPerTunnel = 2
	config.Limits.MaxConnectionsPerService = 2
	config.Limits.MaxConnectionsPerSourceIP = 2
	config.Limits.MaxPendingTLSHandshakes = 16
	config.Limits.MaxPendingAuth = 16

	closer, err := openGatewayAndBootstrapWithStartedAtTracing(
		serverContext,
		config,
		resources,
		serverLogger,
		time.Now(),
		runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened Trace Gate Bootstrap Socket")
		},
		serverTracing,
	)
	if err != nil {
		t.Fatalf("start Trace Gate Server runtime: %v", err)
	}
	serverRuntime := closer.(*gatewayBootstrapCloser)
	runtimeClosed := false
	t.Cleanup(func() {
		if !runtimeClosed {
			if err := closer.Close(); err != nil {
				t.Errorf("close Trace Gate Server runtime: %v", err)
			}
		}
	})

	issuedToken := issueProductGateToken(t, serverContext, resources, serverRuntime.gateway.Addr())
	stopAgent := startTraceGateAgent(t, issuedToken, serverRuntime, agentLogger, agentTracing)
	t.Cleanup(stopAgent)

	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"http://"+serverRuntime.httpIngress.Addr().String()+"/gate/trace", nil,
	)
	if err != nil {
		t.Fatalf("construct Trace Gate public request: %v", err)
	}
	request.Host = productGatePublicHost
	request.Header.Set("traceparent", traceGateUntrustedTraceparent)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("execute Trace Gate public request: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	transport.CloseIdleConnections()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || string(body) != "trace-gate-ok" {
		t.Fatalf(
			"Trace Gate response = status %d body %q, read %v close %v",
			response.StatusCode, body, readErr, closeErr,
		)
	}

	// HTTP/1.1 Origin 可能保留空闲 keep-alive；先由测试关闭自己拥有的 Origin
	// 连接，再等待 Agent 侧 Handler 完成 ACTIVE 收敛并排空 Agent。第五个 Span
	// 必须随该 WorkConn 的自然关闭而结束，而不是依赖 30 秒强关补齐。
	httpOrigin.CloseClientConnections()
	waitForProductGateNoActiveWork(t, serverRuntime)
	stopAgent()
	spans := waitForTraceGateSpans(t, recorder)
	traceID := assertTraceGateSpanChain(t, spans)
	assertTraceGateLog(t, serverLogs, logging.EventTunnelConnectionOpened, traceID)
	assertTraceGateLog(t, serverLogs, logging.EventHTTPIngressRequestCompleted, traceID)
	assertTraceGateLog(t, agentLogs, logging.EventAgentConnectionOpened, traceID)

	if err := closer.Close(); err != nil {
		t.Fatalf("close Trace Gate Server runtime: %v", err)
	}
	runtimeClosed = true
	if err := resources.Close(); err != nil {
		t.Fatalf("close Trace Gate Server storage: %v", err)
	}
	resourcesClosed = true
}

func newTraceGateRuntime(
	t *testing.T,
	recorder sdktrace.SpanProcessor,
	serviceName string,
) *internaltracing.Runtime {
	t.Helper()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	runtime, err := internaltracing.New(t.Context(), internaltracing.Config{
		ServiceName: serviceName, ServiceVersion: "v0.1.0-m6-trace-gate",
		TracerProvider: provider, ProviderShutdown: provider.Shutdown,
	})
	if err != nil {
		t.Fatalf("create %s Trace Gate runtime: %v", serviceName, err)
	}
	t.Cleanup(func() {
		if err := runtime.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown %s Trace Gate runtime: %v", serviceName, err)
		}
	})
	return runtime
}

func newTraceGateLogger(t *testing.T, output io.Writer, component string) *slog.Logger {
	t.Helper()
	logger, err := logging.New(output, logging.Options{
		Level: logging.LevelDebug, Format: "json", Component: component,
	})
	if err != nil {
		t.Fatalf("create %s Trace Gate logger: %v", component, err)
	}
	return logger
}

func startTraceGateAgent(
	t *testing.T,
	token string,
	runtime *gatewayBootstrapCloser,
	logger *slog.Logger,
	traceRuntime *internaltracing.Runtime,
) func() {
	t.Helper()
	agentConfig, err := connector.HostConfig(token, "v0.1.0-m6-trace-gate")
	if err != nil {
		t.Fatalf("build Trace Gate Agent config: %v", err)
	}
	agentConfig.Logger = logger
	agentConfig.Tracing = traceRuntime
	agentRuntime, err := connector.New(agentConfig)
	if err != nil {
		t.Fatalf("construct Trace Gate Agent runtime: %v", err)
	}
	agentContext, cancelAgent := context.WithCancel(context.Background())
	agentDone := make(chan error, 1)
	go func() { agentDone <- agentRuntime.Run(agentContext) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancelAgent()
			select {
			case runErr := <-agentDone:
				if runErr != nil && !errors.Is(runErr, context.Canceled) {
					t.Errorf("stop Trace Gate Agent runtime: %v", runErr)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("Trace Gate Agent runtime did not stop after cancellation")
			}
		})
	}

	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			httpService, hasHTTP := snapshot.Config.Services[productGateHTTPServiceID]
			if snapshot.TunnelID == productGateTunnelID && snapshot.CurrentControlSession &&
				snapshot.Config.ConfigReady && snapshot.Config.HasObserved &&
				snapshot.Config.ObservedRevision == 1 && hasHTTP && httpService.Enabled &&
				httpService.RequiredRevision == 1 && snapshot.WorkPool.Idle >= 1 {
				return stop
			}
		}
		select {
		case runErr := <-agentDone:
			cancelAgent()
			t.Fatalf("Trace Gate Agent exited before ready: %v", runErr)
		case <-deadline.C:
			cancelAgent()
			select {
			case <-agentDone:
			case <-time.After(5 * time.Second):
				t.Error("Trace Gate Agent did not stop after readiness timeout")
			}
			t.Fatal("Trace Gate Agent did not publish a ready HTTP Snapshot and IDLE Work")
		case <-ticker.C:
		}
	}
}

func waitForTraceGateSpans(t *testing.T, recorder *tracetest.SpanRecorder) map[string]sdktrace.ReadOnlySpan {
	t.Helper()
	wanted := map[string]struct{}{
		"ingress.Accept": {}, "tunnel.DialContext": {}, "transport.Acquire": {},
		"origin.Dial": {}, "proxy.Bidirectional": {},
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		spans := make(map[string]sdktrace.ReadOnlySpan, len(wanted))
		duplicates := make(map[string]int)
		for _, span := range recorder.Ended() {
			if _, ok := wanted[span.Name()]; !ok {
				continue
			}
			duplicates[span.Name()]++
			spans[span.Name()] = span
		}
		if len(spans) == len(wanted) {
			for name, count := range duplicates {
				if count != 1 {
					t.Fatalf("Trace Gate span %q count = %d, want 1", name, count)
				}
			}
			return spans
		}
		select {
		case <-deadline.C:
			t.Fatalf("Trace Gate ended spans = %v, want five-span chain", duplicates)
		case <-ticker.C:
		}
	}
}

func assertTraceGateSpanChain(t *testing.T, spans map[string]sdktrace.ReadOnlySpan) string {
	t.Helper()
	ingress := spans["ingress.Accept"]
	tunnelSpan := spans["tunnel.DialContext"]
	acquire := spans["transport.Acquire"]
	origin := spans["origin.Dial"]
	proxy := spans["proxy.Bidirectional"]

	traceID := ingress.SpanContext().TraceID()
	if !traceID.IsValid() {
		t.Fatal("Trace Gate ingress TraceID is invalid")
	}
	untrustedTraceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	if err != nil {
		t.Fatalf("parse Trace Gate untrusted TraceID fixture: %v", err)
	}
	if traceID == untrustedTraceID {
		t.Fatal("Trace Gate public traceparent became the local root")
	}
	if ingress.Parent().IsValid() {
		t.Fatalf("Trace Gate ingress parent = %s, want local root", ingress.Parent().SpanID())
	}
	assertTraceGateLocalParent(t, tunnelSpan, ingress)
	assertTraceGateLocalParent(t, acquire, tunnelSpan)
	assertTraceGateRemoteParent(t, origin, acquire)
	assertTraceGateLocalParent(t, proxy, origin)
	for name, span := range spans {
		if span.SpanContext().TraceID() != traceID {
			t.Errorf("Trace Gate span %q TraceID = %s, want %s", name, span.SpanContext().TraceID(), traceID)
		}
	}
	return traceID.String()
}

func assertTraceGateLocalParent(t *testing.T, child, parent sdktrace.ReadOnlySpan) {
	t.Helper()
	got := child.Parent()
	want := parent.SpanContext()
	if got.TraceID() != want.TraceID() || got.SpanID() != want.SpanID() || got.IsRemote() {
		t.Errorf(
			"Trace Gate span %q parent = %s/%s remote=%t, want %s/%s remote=false",
			child.Name(), got.TraceID(), got.SpanID(), got.IsRemote(), want.TraceID(), want.SpanID(),
		)
	}
}

func assertTraceGateRemoteParent(t *testing.T, child, parent sdktrace.ReadOnlySpan) {
	t.Helper()
	got := child.Parent()
	want := parent.SpanContext()
	if got.TraceID() != want.TraceID() || got.SpanID() != want.SpanID() || !got.IsRemote() {
		t.Errorf(
			"Trace Gate span %q parent = %s/%s remote=%t, want %s/%s remote=true",
			child.Name(), got.TraceID(), got.SpanID(), got.IsRemote(), want.TraceID(), want.SpanID(),
		)
	}
}

func assertTraceGateLog(t *testing.T, output *traceGateLockedBuffer, event, traceID string) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output.snapshot()))
	for decoder.More() {
		entry := make(map[string]any)
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("decode Trace Gate %s log: %v", event, err)
		}
		if entry[logging.EventKey] != event {
			continue
		}
		if entry[logging.TraceIDKey] != traceID {
			t.Fatalf("Trace Gate %s trace_id = %v, want %s", event, entry[logging.TraceIDKey], traceID)
		}
		return
	}
	t.Fatalf("Trace Gate log missing event %q", event)
}

type traceGateLockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *traceGateLockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *traceGateLockedBuffer) snapshot() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return bytes.Clone(buffer.buffer.Bytes())
}
