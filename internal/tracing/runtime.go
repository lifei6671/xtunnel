package tracing

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/http/httpguts"
)

const (
	tracesEndpointEnv = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	baseEndpointEnv   = "OTEL_EXPORTER_OTLP_ENDPOINT"
	tracesHeadersEnv  = "OTEL_EXPORTER_OTLP_TRACES_HEADERS"
	baseHeadersEnv    = "OTEL_EXPORTER_OTLP_HEADERS"

	// AttributeErrorCode 是 Span 上唯一的有限错误分类，值必须来自已冻结的
	// Protocol/HTTP 错误码，不能写入底层错误文本。
	AttributeErrorCode = "xtunnel.error_code"
	// AttributeIngressType 只记录 HTTP/TCP 有限枚举，不包含公网地址或路由标识。
	AttributeIngressType = "xtunnel.ingress_type"

	defaultShutdownTimeout = 5 * time.Second
	batchTimeout           = 5 * time.Second
	exportTimeout          = 5 * time.Second
	maxQueueSize           = 2048
	maxExportBatchSize     = 512
)

// Config describes one process-local trace runtime. Exporter and TracerProvider
// are injection points for deterministic tests; production callers normally
// leave both nil so the runtime validates the supported OTLP/HTTP environment
// and passes an explicit, process-local configuration to the exporter.
type Config struct {
	ServiceName         string
	ServiceVersion      string
	Exporter            sdktrace.SpanExporter
	TracerProvider      trace.TracerProvider
	ProviderShutdown    func(context.Context) error
	ReportExportFailure func()
	ShutdownTimeout     time.Duration
}

// Runtime owns one tracer provider, W3C Trace Context propagator, and their
// finite shutdown lifecycle. It never installs either object globally.
type Runtime struct {
	enabled         bool
	provider        trace.TracerProvider
	propagator      propagation.TextMapPropagator
	shutdown        func(context.Context) error
	shutdownTimeout time.Duration

	shutdownOnce sync.Once
	shutdownErr  error
}

// New constructs a process-local trace runtime. When neither OTLP endpoint
// environment variable is configured, New returns a disabled no-op runtime.
func New(ctx context.Context, config Config) (*Runtime, error) {
	if strings.TrimSpace(config.ServiceName) == "" {
		return nil, errors.New("create tracing runtime: service name is required")
	}
	if strings.TrimSpace(config.ServiceVersion) == "" {
		return nil, errors.New("create tracing runtime: service version is required")
	}
	if config.ShutdownTimeout < 0 {
		return nil, errors.New("create tracing runtime: shutdown timeout must not be negative")
	}
	if config.TracerProvider != nil && config.Exporter != nil {
		return nil, errors.New("create tracing runtime: tracer provider and span exporter are mutually exclusive")
	}
	if config.TracerProvider != nil && config.ProviderShutdown == nil {
		return nil, errors.New("create tracing runtime: injected tracer provider requires shutdown ownership")
	}
	if config.TracerProvider == nil && config.ProviderShutdown != nil {
		return nil, errors.New("create tracing runtime: provider shutdown requires a tracer provider")
	}

	shutdownTimeout := config.ShutdownTimeout
	if shutdownTimeout == 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	propagator := propagation.TraceContext{}

	if config.TracerProvider != nil {
		return &Runtime{
			enabled:         true,
			provider:        config.TracerProvider,
			propagator:      propagator,
			shutdown:        config.ProviderShutdown,
			shutdownTimeout: shutdownTimeout,
		}, nil
	}

	exporter := config.Exporter
	if exporter == nil {
		environment, configured, err := readExporterEnvironment()
		if err != nil {
			return nil, err
		}
		if !configured {
			return disabledRuntime(propagator, shutdownTimeout), nil
		}
		options := []otlptracehttp.Option{
			otlptracehttp.WithEndpointURL(environment.endpoint),
			otlptracehttp.WithEncoding(otlptracehttp.EncodingProtobuf),
			otlptracehttp.WithTimeout(environment.timeout),
			otlptracehttp.WithHeaders(environment.headers),
		}
		if environment.gzip {
			options = append(options, otlptracehttp.WithCompression(otlptracehttp.GzipCompression))
		}

		exporter, err = otlptracehttp.New(ctx, options...)
		if err != nil {
			return nil, errors.New("create tracing runtime: create OTLP HTTP exporter")
		}
	}
	exporter = &safeExporter{next: exporter, reportFailure: config.ReportExportFailure}

	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(config.ServiceName),
		semconv.ServiceVersion(config.ServiceVersion),
	)
	// BatchSpanProcessor 使用有界队列且不启用 WithBlocking。Exporter 变慢或失败时，
	// 数据面只会丢弃超出队列容量的遥测，不会等待外部 Collector。
	processor := sdktrace.NewBatchSpanProcessor(
		exporter,
		sdktrace.WithMaxQueueSize(maxQueueSize),
		sdktrace.WithMaxExportBatchSize(maxExportBatchSize),
		sdktrace.WithBatchTimeout(batchTimeout),
		sdktrace.WithExportTimeout(exportTimeout),
	)
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(processor),
		// 父级采样决定跨 Server/Agent 保持一致；本地 Root 默认全采样，容量与
		// 比例采样在 M6 Gate 压测后再冻结，当前不增加第二套配置入口。
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)

	return &Runtime{
		enabled:         true,
		provider:        provider,
		propagator:      propagator,
		shutdown:        provider.Shutdown,
		shutdownTimeout: shutdownTimeout,
	}, nil
}

// safeExporter prevents raw Collector/network errors from reaching OTel's
// process-global fallback logger. The caller receives one stable signal while
// the BatchSpanProcessor keeps export failure off the data-plane path.
type safeExporter struct {
	next          sdktrace.SpanExporter
	reportFailure func()
	reportOnce    sync.Once
}

func (exporter *safeExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if err := exporter.next.ExportSpans(ctx, spans); err != nil {
		exporter.reportOnce.Do(func() {
			if exporter.reportFailure != nil {
				exporter.reportFailure()
			}
		})
	}
	return nil
}

func (exporter *safeExporter) Shutdown(ctx context.Context) error {
	if err := exporter.next.Shutdown(ctx); err != nil {
		return errors.New("trace exporter shutdown failed")
	}
	return nil
}

func disabledRuntime(propagator propagation.TextMapPropagator, shutdownTimeout time.Duration) *Runtime {
	return &Runtime{
		provider:        trace.NewNoopTracerProvider(),
		propagator:      propagator,
		shutdown:        func(context.Context) error { return nil },
		shutdownTimeout: shutdownTimeout,
	}
}

type exporterEnvironment struct {
	endpoint string
	headers  map[string]string
	timeout  time.Duration
	gzip     bool
}

func readExporterEnvironment() (exporterEnvironment, bool, error) {
	tracesEndpoint := environmentValue(tracesEndpointEnv)
	baseEndpoint := environmentValue(baseEndpointEnv)
	if tracesEndpoint == "" && baseEndpoint == "" {
		return exporterEnvironment{}, false, nil
	}
	for _, endpoint := range []string{baseEndpoint, tracesEndpoint} {
		if endpoint != "" {
			if err := validateEndpoint(endpoint); err != nil {
				return exporterEnvironment{}, false, err
			}
		}
	}

	effectiveEndpoint := tracesEndpoint
	if effectiveEndpoint == "" {
		effectiveEndpoint = baseEndpoint
		parsed, _ := url.Parse(effectiveEndpoint)
		parsed.Path = path.Join(parsed.Path, "/v1/traces")
		effectiveEndpoint = parsed.String()
	}
	parsedEffective, _ := url.Parse(effectiveEndpoint)
	if err := validateInsecureEnvironment(parsedEffective); err != nil {
		return exporterEnvironment{}, false, err
	}
	if err := validateFixedProtocolEnvironment(); err != nil {
		return exporterEnvironment{}, false, err
	}
	if err := rejectFileTLSConfiguration(); err != nil {
		return exporterEnvironment{}, false, err
	}

	baseHeaders, err := parseHeadersEnvironment(baseHeadersEnv)
	if err != nil {
		return exporterEnvironment{}, false, err
	}
	tracesHeaders, err := parseHeadersEnvironment(tracesHeadersEnv)
	if err != nil {
		return exporterEnvironment{}, false, err
	}
	headers := baseHeaders
	if environmentValue(tracesHeadersEnv) != "" {
		headers = tracesHeaders
	}

	timeout, err := exporterTimeoutEnvironment()
	if err != nil {
		return exporterEnvironment{}, false, err
	}
	gzip, err := exporterCompressionEnvironment()
	if err != nil {
		return exporterEnvironment{}, false, err
	}
	return exporterEnvironment{
		endpoint: effectiveEndpoint, headers: headers, timeout: timeout, gzip: gzip,
	}, true, nil
}

func environmentValue(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}

func validateInsecureEnvironment(endpoint *url.URL) error {
	base, baseSet, err := optionalBooleanEnvironment("OTEL_EXPORTER_OTLP_INSECURE")
	if err != nil {
		return err
	}
	traces, tracesSet, err := optionalBooleanEnvironment("OTEL_EXPORTER_OTLP_TRACES_INSECURE")
	if err != nil {
		return err
	}
	insecure := base
	if tracesSet {
		insecure = traces
	} else if !baseSet {
		return nil
	}
	if insecure && endpoint.Scheme != "http" {
		return errors.New("create tracing runtime: OTLP insecure mode conflicts with secure endpoint")
	}
	return nil
}

func optionalBooleanEnvironment(name string) (bool, bool, error) {
	value := environmentValue(name)
	if value == "" {
		return false, false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, false, errors.New("create tracing runtime: invalid OTLP boolean environment")
	}
	return parsed, true, nil
}

func validateFixedProtocolEnvironment() error {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"} {
		if value := environmentValue(name); value != "" && value != "http/protobuf" {
			return errors.New("create tracing runtime: OTLP traces protocol must be http/protobuf")
		}
	}
	return nil
}

func rejectFileTLSConfiguration() error {
	for _, name := range []string{
		"OTEL_EXPORTER_OTLP_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_CLIENT_KEY",
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE", "OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY",
	} {
		if environmentValue(name) != "" {
			return errors.New("create tracing runtime: OTLP file-based TLS configuration is not supported")
		}
	}
	return nil
}

func parseHeadersEnvironment(name string) (map[string]string, error) {
	raw := environmentValue(name)
	if raw == "" {
		return nil, nil
	}
	headers := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		key, encodedValue, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !found || !httpguts.ValidHeaderFieldName(key) || reservedExporterHeader(key) {
			return nil, errors.New("create tracing runtime: invalid OTLP header environment")
		}
		value, err := url.PathUnescape(encodedValue)
		if err != nil {
			return nil, errors.New("create tracing runtime: invalid OTLP header environment")
		}
		value = strings.TrimSpace(value)
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, errors.New("create tracing runtime: invalid OTLP header environment")
		}
		headers[key] = value
	}
	return headers, nil
}

func reservedExporterHeader(name string) bool {
	switch strings.ToLower(name) {
	case "content-length", "content-encoding", "content-type":
		return true
	default:
		return false
	}
}

func exporterTimeoutEnvironment() (time.Duration, error) {
	selected := exportTimeout
	const maxMilliseconds = uint64((1<<63 - 1) / int64(time.Millisecond))
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT"} {
		value := environmentValue(name)
		if value == "" {
			continue
		}
		milliseconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || milliseconds == 0 || milliseconds > maxMilliseconds {
			return 0, errors.New("create tracing runtime: invalid OTLP timeout environment")
		}
		selected = time.Duration(milliseconds) * time.Millisecond
	}
	return selected, nil
}

func exporterCompressionEnvironment() (bool, error) {
	gzip := false
	for _, name := range []string{"OTEL_EXPORTER_OTLP_COMPRESSION", "OTEL_EXPORTER_OTLP_TRACES_COMPRESSION"} {
		value := environmentValue(name)
		if value == "" {
			continue
		}
		switch value {
		case "none":
			gzip = false
		case "gzip":
			gzip = true
		default:
			return false, errors.New("create tracing runtime: invalid OTLP compression environment")
		}
	}
	return gzip, nil
}

func validateEndpoint(endpoint string) error {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil {
		// 配置值可能包含用户信息，错误中只报告类别，绝不回显 Endpoint。
		return errors.New("create tracing runtime: invalid OTLP HTTP endpoint: malformed URL")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("create tracing runtime: invalid OTLP HTTP endpoint")
	}
	if parsed.Scheme == "http" && !loopbackHost(parsed.Hostname()) {
		return errors.New("create tracing runtime: invalid OTLP HTTP endpoint: insecure transport is only allowed on loopback")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

// Enabled reports whether this runtime records and exports spans.
func (r *Runtime) Enabled() bool {
	return r.enabled
}

// TracerProvider returns the runtime-owned provider for explicit dependency
// injection into Server or Agent components.
func (r *Runtime) TracerProvider() trace.TracerProvider {
	return r.provider
}

// Tracer returns a tracer from the runtime-owned provider.
func (r *Runtime) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	return r.provider.Tracer(name, options...)
}

// Propagator returns the W3C Trace Context propagator used at process and Wire
// boundaries. It propagates traceparent and tracestate, but not baggage.
func (r *Runtime) Propagator() propagation.TextMapPropagator {
	return r.propagator
}

// Inject writes the active W3C Trace Context into carrier.
func (r *Runtime) Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	r.propagator.Inject(ctx, carrier)
}

// Extract restores a validated remote W3C Trace Context from carrier.
func (r *Runtime) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	return r.propagator.Extract(ctx, carrier)
}

// Shutdown flushes and releases trace resources exactly once. The runtime caps
// the caller's context with its own timeout so shutdown cannot wait forever.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.shutdownOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(ctx, r.shutdownTimeout)
		defer cancel()

		if err := r.shutdown(shutdownCtx); err != nil {
			r.shutdownErr = fmt.Errorf("shutdown tracing runtime: %w", err)
		}
	})
	return r.shutdownErr
}

// TraceID returns the active, valid OpenTelemetry TraceID or an empty string.
func TraceID(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}
