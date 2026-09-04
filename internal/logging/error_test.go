package logging

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
)

func TestErrorDetailPreservesCauseWithoutSensitiveContext(t *testing.T) {
	const sensitive = "private-auth-material-canary"
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"wrapped EOF", fmt.Errorf("%s: %w", sensitive, io.EOF), "peer closed connection (EOF)"},
		{"partial message", errors.Join(errors.New(sensitive), io.ErrUnexpectedEOF), "peer closed connection before the message was complete"},
		{"canceled", fmt.Errorf("%s: %w", sensitive, context.Canceled), "operation canceled"},
		{"deadline", fmt.Errorf("%s: %w", sensitive, context.DeadlineExceeded), "operation timed out"},
		{"closed", net.ErrClosed, "network connection already closed"},
		{"DNS", &net.DNSError{Err: sensitive, Name: sensitive, Server: sensitive, IsNotFound: true}, "DNS name not found"},
		{"TLS record", tls.RecordHeaderError{Msg: sensitive}, "invalid TLS record received"},
		{"joined socket failure", errors.Join(io.EOF, &net.OpError{Op: sensitive, Net: sensitive, Err: syscall.ECONNRESET}), fmt.Sprintf("system error %d: %s", uintptr(syscall.ECONNRESET), syscall.ECONNRESET.Error())},
		{"unknown", errors.New(sensitive), "connection attempt failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail := ErrorDetail(test.err, "connection attempt failed")
			if detail != test.want {
				t.Fatalf("detail = %q, want %q", detail, test.want)
			}
			var output bytes.Buffer
			logger, err := New(&output, Options{Level: LevelInfo, Format: "json", Component: "agent"})
			if err != nil {
				t.Fatal(err)
			}
			logger.Warn(EventAgentServerConnectionFailed, "error", detail, "stage", "connect")
			if strings.Contains(output.String(), sensitive) {
				t.Fatal("error logging disclosed sensitive context")
			}
			record := decodeRecord(t, output.String())
			if record["error"] != test.want || record["stage"] != "connect" {
				t.Fatal("error log did not preserve the diagnostic cause and stage")
			}
		})
	}
}
