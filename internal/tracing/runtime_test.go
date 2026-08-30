package tracing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestNewConfiguration(t *testing.T) {
	clearExporterEnvironment(t)

	tests := []struct {
		name    string
		config  Config
		wantErr string
	}{
		{
			name:    "missing service name",
			config:  Config{ServiceVersion: "v0.1.0"},
			wantErr: "service name is required",
		},
		{
			name:    "missing service version",
			config:  Config{ServiceName: "xtunnel-server"},
			wantErr: "service version is required",
		},
		{
			name: "negative shutdown timeout",
			config: Config{
				ServiceName:     "xtunnel-server",
				ServiceVersion:  "v0.1.0",
				ShutdownTimeout: -time.Second,
			},
			wantErr: "shutdown timeout must not be negative",
		},
		{
			name: "provider and exporter",
			config: Config{
				ServiceName:      "xtunnel-server",
				ServiceVersion:   "v0.1.0",
				TracerProvider:   sdktrace.NewTracerProvider(),
				Exporter:         tracetest.NewInMemoryExporter(),
				ProviderShutdown: func(context.Context) error { return nil },
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "provider without shutdown owner",
			config: Config{
				ServiceName:    "xtunnel-server",
				ServiceVersion: "v0.1.0",
				TracerProvider: sdktrace.NewTracerProvider(),
			},
			wantErr: "requires shutdown ownership",
		},
		{
			name: "shutdown without provider",
			config: Config{
				ServiceName:      "xtunnel-server",
				ServiceVersion:   "v0.1.0",
				ProviderShutdown: func(context.Context) error { return nil },
			},
			wantErr: "provider shutdown requires a tracer provider",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(context.Background(), test.config)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestNewDisabledWithoutEndpoint(t *testing.T) {
	clearExporterEnvironment(t)

	runtime, err := New(context.Background(), Config{
		ServiceName:    "xtunnel-server",
		ServiceVersion: "v0.1.0",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if runtime.Enabled() {
		t.Fatal("Enabled() = true, want false")
	}

	_, span := runtime.Tracer("test").Start(context.Background(), "disabled")
	if span.IsRecording() {
		t.Fatal("disabled runtime span is recording")
	}
	span.End()
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestNewRejectsInvalidEnvironmentEndpoint(t *testing.T) {
	tests := []struct {
		name           string
		tracesEndpoint string
		baseEndpoint   string
	}{
		{
			name:           "trace endpoint has unsupported scheme",
			tracesEndpoint: "grpc://collector.example:4317",
			baseEndpoint:   "https://collector.example:4318",
		},
		{
			name:         "base endpoint is relative",
			baseEndpoint: "collector.example:4318",
		},
		{
			name:           "trace endpoint is malformed",
			tracesEndpoint: "://invalid-secret-sentinel",
		},
		{
			name:           "plain HTTP is not allowed off loopback",
			tracesEndpoint: "http://collector.example:4318/v1/traces",
		},
		{
			name:           "endpoint user info is forbidden",
			tracesEndpoint: "https://user:invalid-secret-sentinel@collector.example:4318/v1/traces",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearExporterEnvironment(t)
			t.Setenv(tracesEndpointEnv, test.tracesEndpoint)
			t.Setenv(baseEndpointEnv, test.baseEndpoint)

			_, err := New(context.Background(), Config{
				ServiceName:    "xtunnel-server",
				ServiceVersion: "v0.1.0",
			})
			if err == nil || !strings.Contains(err.Error(), "invalid OTLP HTTP endpoint") {
				t.Fatalf("New() error = %v, want invalid endpoint error", err)
			}
			if strings.Contains(err.Error(), "invalid-secret-sentinel") || strings.Contains(err.Error(), "collector.example") {
				t.Fatalf("New() error exposes configured endpoint: %v", err)
			}
		})
	}
}

func TestValidateEndpointAllowsSecureAndLoopbackHTTP(t *testing.T) {
	for _, endpoint := range []string{
		"https://collector.example:4318/v1/traces",
		"http://localhost:4318/v1/traces",
		"http://127.0.0.1:4318/v1/traces",
		"http://[::1]:4318/v1/traces",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if err := validateEndpoint(endpoint); err != nil {
				t.Fatalf("validateEndpoint() error = %v", err)
			}
		})
	}
}

func TestNewEnablesConfiguredHTTPExporter(t *testing.T) {
	clearExporterEnvironment(t)
	t.Setenv(tracesEndpointEnv, "http://127.0.0.1:4318/v1/traces")

	runtime, err := New(context.Background(), Config{
		ServiceName:    "xtunnel-server",
		ServiceVersion: "v0.1.0",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !runtime.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestNewRejectsMalformedHeadersWithoutExposingValues(t *testing.T) {
	tests := []struct {
		name        string
		headerEnv   string
		headerValue string
	}{
		{
			name:        "base header lacks separator",
			headerEnv:   baseHeadersEnv,
			headerValue: "invalid-secret-sentinel",
		},
		{
			name:        "trace header has invalid escape",
			headerEnv:   tracesHeadersEnv,
			headerValue: "authorization=%ZZinvalid-secret-sentinel",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearExporterEnvironment(t)
			t.Setenv(tracesEndpointEnv, "https://collector.example:4318/v1/traces")
			t.Setenv(test.headerEnv, test.headerValue)

			_, err := New(context.Background(), Config{
				ServiceName: "xtunnel-server", ServiceVersion: "v0.1.0",
			})
			if err == nil || !strings.Contains(err.Error(), "invalid OTLP header environment") {
				t.Fatalf("New() error = %v, want invalid header error", err)
			}
			if strings.Contains(err.Error(), "invalid-secret-sentinel") || strings.Contains(err.Error(), "authorization") {
				t.Fatalf("New() error exposes configured header: %v", err)
			}
		})
	}
}

func TestNewValidatesBothEndpointsBeforeSelectingTraceEndpoint(t *testing.T) {
	clearExporterEnvironment(t)
	t.Setenv(tracesEndpointEnv, "https://collector.example:4318/v1/traces")
	t.Setenv(baseEndpointEnv, "://invalid-secret-sentinel")

	_, err := New(context.Background(), Config{
		ServiceName: "xtunnel-server", ServiceVersion: "v0.1.0",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid OTLP HTTP endpoint") {
		t.Fatalf("New() error = %v, want invalid endpoint error", err)
	}
	if strings.Contains(err.Error(), "invalid-secret-sentinel") {
		t.Fatalf("New() error exposes unused base endpoint: %v", err)
	}
}

func TestReadExporterEnvironmentSelectsEndpoint(t *testing.T) {
	tests := []struct {
		name          string
		baseEndpoint  string
		traceEndpoint string
		wantEndpoint  string
	}{
		{
			name:         "base endpoint appends traces path",
			baseEndpoint: "https://collector.example:4318/custom",
			wantEndpoint: "https://collector.example:4318/custom/v1/traces",
		},
		{
			name:          "trace endpoint overrides base",
			baseEndpoint:  "https://collector.example:4318/base",
			traceEndpoint: "https://traces.example:4318/explicit",
			wantEndpoint:  "https://traces.example:4318/explicit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearExporterEnvironment(t)
			t.Setenv(baseEndpointEnv, test.baseEndpoint)
			t.Setenv(tracesEndpointEnv, test.traceEndpoint)

			environment, configured, err := readExporterEnvironment()
			if err != nil {
				t.Fatalf("readExporterEnvironment() error = %v", err)
			}
			if !configured || environment.endpoint != test.wantEndpoint {
				t.Fatalf("readExporterEnvironment() = configured %t endpoint %q, want true %q", configured, environment.endpoint, test.wantEndpoint)
			}
		})
	}
}

func TestNewValidatesInsecureEnvironment(t *testing.T) {
	tests := []struct {
		name        string
		insecureEnv string
		endpoint    string
		wantErr     bool
	}{
		{
			name:        "base insecure cannot downgrade HTTPS",
			insecureEnv: "OTEL_EXPORTER_OTLP_INSECURE",
			endpoint:    "https://collector.example:4318/v1/traces",
			wantErr:     true,
		},
		{
			name:        "trace insecure cannot downgrade HTTPS",
			insecureEnv: "OTEL_EXPORTER_OTLP_TRACES_INSECURE",
			endpoint:    "https://collector.example:4318/v1/traces",
			wantErr:     true,
		},
		{
			name:        "loopback HTTP permits explicit insecure mode",
			insecureEnv: "OTEL_EXPORTER_OTLP_TRACES_INSECURE",
			endpoint:    "http://127.0.0.1:4318/v1/traces",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearExporterEnvironment(t)
			t.Setenv(tracesEndpointEnv, test.endpoint)
			t.Setenv(test.insecureEnv, "true")

			runtime, err := New(context.Background(), Config{
				ServiceName: "xtunnel-server", ServiceVersion: "v0.1.0",
			})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "insecure mode conflicts") {
					t.Fatalf("New() error = %v, want secure endpoint conflict", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if err := runtime.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown() error = %v", err)
			}
		})
	}
}

func TestExporterTimeoutEnvironmentRejectsOverflow(t *testing.T) {
	clearExporterEnvironment(t)
	t.Setenv(tracesEndpointEnv, "https://collector.example:4318/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "9223372036854775807")

	_, _, err := readExporterEnvironment()
	if err == nil || !strings.Contains(err.Error(), "invalid OTLP timeout environment") {
		t.Fatalf("readExporterEnvironment() error = %v, want timeout error", err)
	}
}

func TestReadExporterEnvironmentRejectsUnsupportedSettingsWithoutExposingValues(t *testing.T) {
	tests := []struct {
		name      string
		envName   string
		envValue  string
		wantError string
	}{
		{
			name: "gRPC protocol", envName: "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", envValue: "grpc",
			wantError: "protocol must be http/protobuf",
		},
		{
			name: "file TLS", envName: "OTEL_EXPORTER_OTLP_CLIENT_KEY", envValue: "invalid-secret-sentinel.pem",
			wantError: "file-based TLS configuration is not supported",
		},
		{
			name: "compression", envName: "OTEL_EXPORTER_OTLP_COMPRESSION", envValue: "invalid-secret-sentinel",
			wantError: "invalid OTLP compression environment",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearExporterEnvironment(t)
			t.Setenv(tracesEndpointEnv, "https://collector.example:4318/v1/traces")
			t.Setenv(test.envName, test.envValue)

			_, _, err := readExporterEnvironment()
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("readExporterEnvironment() error = %v, want containing %q", err, test.wantError)
			}
			if strings.Contains(err.Error(), "invalid-secret-sentinel") {
				t.Fatalf("readExporterEnvironment() error exposes configured value: %v", err)
			}
		})
	}
}

func clearExporterEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		baseEndpointEnv, tracesEndpointEnv,
		baseHeadersEnv, tracesHeadersEnv,
		"OTEL_EXPORTER_OTLP_INSECURE", "OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
		"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION",
		"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
	} {
		t.Setenv(name, "")
	}
}

func TestNewWithInjectedExporter(t *testing.T) {
	exporter := &recordingExporter{}
	runtime, err := New(context.Background(), Config{
		ServiceName:    "xtunnel-server",
		ServiceVersion: "v0.1.0",
		Exporter:       exporter,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if !runtime.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}

	ctx, span := runtime.Tracer("test").Start(context.Background(), "test.span")
	if TraceID(ctx) == "" {
		t.Fatal("TraceID() is empty for recording span")
	}
	span.End()
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	spans := exporter.Spans()
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	if spans[0].Name() != "test.span" {
		t.Errorf("span name = %q, want %q", spans[0].Name(), "test.span")
	}
	attributes := spans[0].Resource().Attributes()
	assertAttribute(t, attributes, "service.name", "xtunnel-server")
	assertAttribute(t, attributes, "service.version", "v0.1.0")
}

func TestExporterFailureReportsStableSignalWithoutReturningRawError(t *testing.T) {
	var reports atomic.Int32
	exporter := &failingExporter{exportErr: errors.New("collector invalid-secret-sentinel")}
	runtime, err := New(context.Background(), Config{
		ServiceName: "xtunnel-server", ServiceVersion: "v0.1.0", Exporter: exporter,
		ReportExportFailure: func() { reports.Add(1) },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for range 2 {
		_, span := runtime.Tracer("test").Start(context.Background(), "failed.export")
		span.End()
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := reports.Load(); got != 1 {
		t.Fatalf("export failure reports = %d, want exactly one", got)
	}
}

func TestExporterShutdownSanitizesError(t *testing.T) {
	runtime, err := New(context.Background(), Config{
		ServiceName: "xtunnel-agent", ServiceVersion: "v0.1.0",
		Exporter: &failingExporter{shutdownErr: errors.New("invalid-secret-sentinel")},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = runtime.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "trace exporter shutdown failed") {
		t.Fatalf("Shutdown() error = %v, want sanitized exporter failure", err)
	}
	if strings.Contains(err.Error(), "invalid-secret-sentinel") {
		t.Fatalf("Shutdown() error leaked exporter detail: %v", err)
	}
}

type recordingExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

type failingExporter struct {
	exportErr   error
	shutdownErr error
}

func (exporter *failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return exporter.exportErr
}

func (exporter *failingExporter) Shutdown(context.Context) error {
	return exporter.shutdownErr
}

func (e *recordingExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, spans...)
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error {
	return nil
}

func (e *recordingExporter) Spans() []sdktrace.ReadOnlySpan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), e.spans...)
}

func TestTraceContextInjectExtract(t *testing.T) {
	traceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	spanID := trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}
	state, err := trace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatalf("ParseTraceState() error = %v", err)
	}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
		TraceState: state,
	})
	runtime := disabledRuntime(propagation.TraceContext{}, defaultShutdownTimeout)
	carrier := propagation.MapCarrier{}

	runtime.Inject(trace.ContextWithSpanContext(context.Background(), spanContext), carrier)
	if got, want := carrier.Get("traceparent"), "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01"; got != want {
		t.Errorf("traceparent = %q, want %q", got, want)
	}
	if got, want := carrier.Get("tracestate"), "vendor=value"; got != want {
		t.Errorf("tracestate = %q, want %q", got, want)
	}

	extracted := trace.SpanContextFromContext(runtime.Extract(context.Background(), carrier))
	if !extracted.IsRemote() {
		t.Error("extracted SpanContext is not remote")
	}
	if extracted.TraceID() != traceID || extracted.SpanID() != spanID {
		t.Errorf("extracted SpanContext = %s/%s, want %s/%s", extracted.TraceID(), extracted.SpanID(), traceID, spanID)
	}
	if extracted.TraceState().String() != "vendor=value" {
		t.Errorf("extracted tracestate = %q, want %q", extracted.TraceState(), "vendor=value")
	}
}

func TestTraceContextRejectsInvalidParent(t *testing.T) {
	runtime := disabledRuntime(propagation.TraceContext{}, defaultShutdownTimeout)
	tests := []struct {
		name   string
		parent string
	}{
		{name: "missing", parent: ""},
		{name: "short trace id", parent: "00-0102-1112131415161718-01"},
		{name: "zero trace id", parent: "00-00000000000000000000000000000000-1112131415161718-01"},
		{name: "unsupported version", parent: "ff-0102030405060708090a0b0c0d0e0f10-1112131415161718-01"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := runtime.Extract(context.Background(), propagation.MapCarrier{"traceparent": test.parent})
			if got := trace.SpanContextFromContext(ctx); got.IsValid() {
				t.Fatalf("extracted SpanContext = %v, want invalid", got)
			}
		})
	}
}

func TestTraceID(t *testing.T) {
	validTraceID := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "no span context", ctx: context.Background()},
		{
			name: "valid span context",
			ctx: trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: validTraceID,
				SpanID:  trace.SpanID{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
			})),
			want: validTraceID.String(),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := TraceID(test.ctx); got != test.want {
				t.Errorf("TraceID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestShutdownHasDeadlineAndRunsOnce(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	var calls atomic.Int32
	runtime, err := New(context.Background(), Config{
		ServiceName:     "xtunnel-agent",
		ServiceVersion:  "v0.1.0",
		TracerProvider:  provider,
		ShutdownTimeout: 20 * time.Millisecond,
		ProviderShutdown: func(ctx context.Context) error {
			calls.Add(1)
			if _, ok := ctx.Deadline(); !ok {
				return errors.New("shutdown context has no deadline")
			}
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	err = runtime.Shutdown(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline exceeded", err)
	}
	if secondErr := runtime.Shutdown(context.Background()); !errors.Is(secondErr, context.DeadlineExceeded) {
		t.Fatalf("second Shutdown() error = %v, want cached deadline exceeded", secondErr)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("shutdown calls = %d, want 1", got)
	}
}

func assertAttribute(t *testing.T, attributes []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, value := range attributes {
		if string(value.Key) == key {
			if got := value.Value.AsString(); got != want {
				t.Errorf("resource attribute %q = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Errorf("resource attribute %q is missing", key)
}
