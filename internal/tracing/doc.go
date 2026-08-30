// Package tracing owns XTunnel's process-local OpenTelemetry trace runtime.
//
// Runtime objects deliberately do not mutate OpenTelemetry global state. The
// Server and Agent bootstrap paths own their respective Runtime and pass its
// provider and propagator into the components that participate in a trace.
package tracing
