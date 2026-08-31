//go:build windows

package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/safego"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func TestWindowsServiceConfigContract(t *testing.T) {
	const binary = `C:\Program Files\XTunnel\xtunnel-agent.exe`
	config := expectedWindowsConfig(binary)
	if config.BinaryPathName != `"C:\Program Files\XTunnel\xtunnel-agent.exe" run` {
		t.Fatalf("BinaryPathName = %q", config.BinaryPathName)
	}
	if config.Description != windowsManagedMarker ||
		config.ServiceStartName != windowsServiceAccount ||
		config.DisplayName != windowsServiceDisplayName {
		t.Fatalf("unexpected managed service config: %#v", config)
	}
	if config.StartType == 0 || config.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
		t.Fatalf("service config is not automatic/restricted: %#v", config)
	}
	if !isExpectedManagedWindowsService(config, binary) {
		t.Fatal("expected managed config was rejected")
	}
	config.Description = "foreign"
	if isExpectedManagedWindowsService(config, binary) {
		t.Fatal("foreign service marker was accepted")
	}
}

func TestWindowsServiceStoppedBeforeTargetReportsExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		status svc.Status
		target svc.State
		want   string
	}{
		{
			name: "启动失败",
			status: svc.Status{
				State:                   svc.Stopped,
				Win32ExitCode:           1066,
				ServiceSpecificExitCode: 1,
				ProcessId:               42,
			},
			target: svc.Running,
			want:   "win32_exit_code=1066 service_specific_exit_code=1 process_id=42",
		},
		{
			name:   "仍在启动",
			status: svc.Status{State: svc.StartPending},
			target: svc.Running,
		},
		{
			name:   "正常停止",
			status: svc.Status{State: svc.Stopped},
			target: svc.Stopped,
		},
		{
			name:   "尚未记录退出码",
			status: svc.Status{State: svc.Stopped},
			target: svc.Running,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := windowsServiceStoppedBeforeTarget(test.status, test.target)
			if test.want == "" {
				if err != nil {
					t.Fatalf("windowsServiceStoppedBeforeTarget() error = %v，want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("windowsServiceStoppedBeforeTarget() error = %v，want substring %q", err, test.want)
			}
		})
	}
}

func TestWindowsDPAPIRoundTrip(t *testing.T) {
	plainText := []byte("xta_dpapi_round_trip_secret")
	protected, err := protectWindowsCredential(plainText)
	if err != nil {
		t.Fatalf("protectWindowsCredential() error = %v", err)
	}
	if len(protected) == 0 || strings.Contains(string(protected), string(plainText)) {
		t.Fatal("DPAPI output is empty or exposes plaintext")
	}
	got, err := unprotectWindowsCredential(protected)
	if err != nil {
		t.Fatalf("unprotectWindowsCredential() error = %v", err)
	}
	if string(got) != string(plainText) {
		t.Fatal("DPAPI round trip changed plaintext")
	}
}

func TestLoadWindowsCredentialFile(t *testing.T) {
	const token = "xta_dpapi_file_secret"
	path := filepath.Join(t.TempDir(), "agent.token.dpapi")
	protected, err := protectWindowsCredential([]byte(token))
	if err != nil {
		t.Fatalf("protectWindowsCredential() error = %v", err)
	}
	if err := os.WriteFile(path, protected, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	got, err := loadWindowsCredentialFile(path)
	if err != nil || got != token {
		t.Fatalf("loadWindowsCredentialFile() = %q, %v; want Token", got, err)
	}

	if _, err := loadWindowsCredentialFile(filepath.Join(t.TempDir(), "missing")); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("missing credential error = %v, want non-secret read failure", err)
	}
	if err := os.WriteFile(path, []byte("not a DPAPI credential"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(corrupt credential) error = %v", err)
	}
	if _, err := loadWindowsCredentialFile(path); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("corrupt credential error = %v, want non-secret decrypt failure", err)
	}
}

func TestResolveWindowsServiceTokenPrecedence(t *testing.T) {
	tests := []struct {
		name             string
		resolveOverride  func() (string, bool, error)
		credentialToken  string
		credentialErr    error
		want             string
		wantErr          string
		wantCredentialIO bool
	}{
		{
			name: "override wins without credential IO",
			resolveOverride: func() (string, bool, error) {
				return "xta_override_secret", true, nil
			},
			credentialErr: errors.New("corrupt lower-priority credential"),
			want:          "xta_override_secret",
		},
		{
			name: "credential fallback",
			resolveOverride: func() (string, bool, error) {
				return "", false, nil
			},
			credentialToken:  "xta_credential_secret",
			want:             "xta_credential_secret",
			wantCredentialIO: true,
		},
		{
			name: "invalid override fails without credential IO",
			resolveOverride: func() (string, bool, error) {
				return "", true, errors.New("invalid override")
			},
			credentialToken: "xta_credential_secret",
			wantErr:         "resolve Windows service token override: invalid override",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credentialRead := false
			token, err := resolveWindowsServiceToken(test.resolveOverride, func() (string, error) {
				credentialRead = true
				return test.credentialToken, test.credentialErr
			})
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("resolveWindowsServiceToken() error = %v", err)
				}
			} else if err == nil || err.Error() != test.wantErr {
				t.Fatalf("resolveWindowsServiceToken() error = %v, want %q", err, test.wantErr)
			}
			if token != test.want {
				t.Fatalf("resolveWindowsServiceToken() = %q, want %q", token, test.want)
			}
			if credentialRead != test.wantCredentialIO {
				t.Fatalf("credential read = %t, want %t", credentialRead, test.wantCredentialIO)
			}
		})
	}
}

func TestWindowsServiceHandlerCancelsOnStop(t *testing.T) {
	const token = "xta_windows_service_secret"
	started := make(chan struct{})
	handler := &windowsServiceHandler{
		load:     func() (string, error) { return token, nil },
		stopWait: time.Second,
		writer:   io.Discard,
		logger:   newTestWindowsServiceLogger(t),
		callback: func(ctx context.Context, got string, writer io.Writer) error {
			if got != token {
				t.Errorf("callback Token was not loaded from protected credential")
			}
			if writer != io.Discard {
				t.Error("callback did not receive the managed service log writer")
			}
			close(started)
			<-ctx.Done()
			return nil
		},
	}
	requests := make(chan svc.ChangeRequest)
	changes := make(chan svc.Status)
	done := make(chan struct{})
	go func() {
		handler.Execute(nil, requests, changes)
		close(done)
	}()

	if state := receiveWindowsServiceState(t, changes); state != svc.StartPending {
		t.Fatalf("first state = %d, want StartPending", state)
	}
	if state := receiveWindowsServiceState(t, changes); state != svc.Running {
		t.Fatalf("second state = %d, want Running", state)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("service callback did not start")
	}
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	if state := receiveWindowsServiceState(t, changes); state != svc.StopPending {
		t.Fatalf("stop state = %d, want StopPending", state)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service handler did not stop after SCM Stop")
	}
	if handler.err != nil {
		t.Fatalf("handler error = %v", handler.err)
	}
}

func TestWindowsServiceHandlerFailsWhenRuntimeDoesNotStop(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	handler := &windowsServiceHandler{
		load:     func() (string, error) { return "xta_timeout_secret", nil },
		stopWait: 20 * time.Millisecond,
		writer:   io.Discard,
		logger:   newTestWindowsServiceLogger(t),
		callback: func(context.Context, string, io.Writer) error {
			<-release
			return nil
		},
	}
	requests := make(chan svc.ChangeRequest)
	changes := make(chan svc.Status)
	result := make(chan struct {
		serviceSpecific bool
		exitCode        uint32
	}, 1)
	go func() {
		serviceSpecific, exitCode := handler.Execute(nil, requests, changes)
		result <- struct {
			serviceSpecific bool
			exitCode        uint32
		}{serviceSpecific, exitCode}
	}()
	receiveWindowsServiceState(t, changes)
	receiveWindowsServiceState(t, changes)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	receiveWindowsServiceState(t, changes)
	select {
	case got := <-result:
		if !got.serviceSpecific || got.exitCode == 0 || handler.err == nil {
			t.Fatalf("Execute() = (%v, %d), error=%v; want service-specific failure", got.serviceSpecific, got.exitCode, handler.err)
		}
	case <-time.After(time.Second):
		t.Fatal("service handler did not enforce bounded stop")
	}
}

func TestWindowsServiceHandlerReportsRuntimeFailure(t *testing.T) {
	fail := make(chan struct{})
	handler := &windowsServiceHandler{
		load:     func() (string, error) { return "xta_failure_secret", nil },
		stopWait: time.Second,
		writer:   io.Discard,
		logger:   newTestWindowsServiceLogger(t),
		callback: func(context.Context, string, io.Writer) error {
			<-fail
			return context.Canceled
		},
	}
	requests := make(chan svc.ChangeRequest)
	changes := make(chan svc.Status)
	result := make(chan struct {
		serviceSpecific bool
		exitCode        uint32
	}, 1)
	go func() {
		serviceSpecific, exitCode := handler.Execute(nil, requests, changes)
		result <- struct {
			serviceSpecific bool
			exitCode        uint32
		}{serviceSpecific, exitCode}
	}()
	receiveWindowsServiceState(t, changes)
	receiveWindowsServiceState(t, changes)
	close(fail)
	receiveWindowsServiceState(t, changes)
	select {
	case got := <-result:
		if !got.serviceSpecific || got.exitCode == 0 || handler.err == nil {
			t.Fatalf("Execute() = (%v, %d), error=%v; want service-specific runtime failure", got.serviceSpecific, got.exitCode, handler.err)
		}
	case <-time.After(time.Second):
		t.Fatal("service handler did not propagate runtime failure")
	}
}

func TestWindowsServiceHandlerReportsRuntimePanic(t *testing.T) {
	handler := &windowsServiceHandler{
		load:     func() (string, error) { return "xta_panic_secret", nil },
		stopWait: time.Second,
		writer:   io.Discard,
		logger:   newTestWindowsServiceLogger(t),
		callback: func(context.Context, string, io.Writer) error {
			panic("service callback panic must not escape its goroutine")
		},
	}
	requests := make(chan svc.ChangeRequest)
	changes := make(chan svc.Status)
	result := make(chan struct {
		serviceSpecific bool
		exitCode        uint32
	}, 1)
	go func() {
		serviceSpecific, exitCode := handler.Execute(nil, requests, changes)
		result <- struct {
			serviceSpecific bool
			exitCode        uint32
		}{serviceSpecific, exitCode}
	}()
	if state := receiveWindowsServiceState(t, changes); state != svc.StartPending {
		t.Fatalf("first state = %d, want StartPending", state)
	}
	if state := receiveWindowsServiceState(t, changes); state != svc.Running {
		t.Fatalf("second state = %d, want Running", state)
	}
	if state := receiveWindowsServiceState(t, changes); state != svc.StopPending {
		t.Fatalf("third state = %d, want StopPending", state)
	}
	select {
	case got := <-result:
		if !got.serviceSpecific || got.exitCode == 0 || !errors.Is(handler.err, safego.ErrPanic) {
			t.Fatalf("Execute() = (%v, %d), error=%v; want safego panic failure", got.serviceSpecific, got.exitCode, handler.err)
		}
	case <-time.After(time.Second):
		t.Fatal("service handler did not report callback panic")
	}
}

func TestWindowsServiceHandlerFailsBeforeCredentialLoadWhenEventLogWriteFails(t *testing.T) {
	writeFailure := errors.New("event log unavailable")
	logger, err := logging.New(errorWriter{err: writeFailure}, logging.Options{
		Level: "info", Format: "json", Component: "agent",
	})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	loadCalled := false
	handler := &windowsServiceHandler{
		load: func() (string, error) {
			loadCalled = true
			return "xta_must_not_be_loaded", nil
		},
		stopWait: time.Second,
		writer:   errorWriter{err: writeFailure},
		logger:   logger,
		callback: func(context.Context, string, io.Writer) error {
			t.Fatal("callback ran after Event Log failure")
			return nil
		},
	}
	changes := make(chan svc.Status, 1)
	serviceSpecific, exitCode := handler.Execute(nil, make(chan svc.ChangeRequest), changes)
	if !serviceSpecific || exitCode == 0 || !errors.Is(handler.err, writeFailure) {
		t.Fatalf("Execute() = (%t, %d), error=%v; want Event Log failure", serviceSpecific, exitCode, handler.err)
	}
	if loadCalled {
		t.Fatal("credential was loaded after Event Log failure")
	}
}

func TestWindowsServiceHandlerStopsWhenRuntimeSlogWriteFails(t *testing.T) {
	writeFailure := errors.New("runtime Event Log write failed")
	eventLogger := &fakeWindowsEventLogger{writeErr: writeFailure, failAt: 3}
	eventWriter, err := openWindowsEventLogWriter(windowsEventLogSource, func(string) (windowsEventLogger, error) {
		return eventLogger, nil
	})
	if err != nil {
		t.Fatalf("openWindowsEventLogWriter() error = %v", err)
	}
	serviceLogger, err := logging.New(eventWriter, logging.Options{
		Level: "info", Format: "json", Component: "agent",
	})
	if err != nil {
		t.Fatalf("logging.New(service) error = %v", err)
	}
	emitRuntimeLog := make(chan struct{})
	handler := &windowsServiceHandler{
		load:     func() (string, error) { return "xta_runtime_log_failure", nil },
		stopWait: time.Second,
		writer:   eventWriter,
		logger:   serviceLogger,
		failures: eventWriter.Failures(),
		callback: func(ctx context.Context, _ string, writer io.Writer) error {
			runtimeLogger, err := logging.New(writer, logging.Options{
				Level: "info", Format: "json", Component: "agent",
			})
			if err != nil {
				return err
			}
			<-emitRuntimeLog
			runtimeLogger.Info("runtime_log_after_running")
			<-ctx.Done()
			return nil
		},
	}
	requests := make(chan svc.ChangeRequest)
	changes := make(chan svc.Status)
	result := make(chan struct {
		serviceSpecific bool
		exitCode        uint32
	}, 1)
	go func() {
		serviceSpecific, exitCode := handler.Execute(nil, requests, changes)
		result <- struct {
			serviceSpecific bool
			exitCode        uint32
		}{serviceSpecific, exitCode}
	}()
	if state := receiveWindowsServiceState(t, changes); state != svc.StartPending {
		t.Fatalf("first state = %d, want StartPending", state)
	}
	if state := receiveWindowsServiceState(t, changes); state != svc.Running {
		t.Fatalf("second state = %d, want Running", state)
	}
	close(emitRuntimeLog)
	if state := receiveWindowsServiceState(t, changes); state != svc.StopPending {
		t.Fatalf("third state = %d, want StopPending", state)
	}
	select {
	case got := <-result:
		if !got.serviceSpecific || got.exitCode == 0 || !errors.Is(handler.err, writeFailure) {
			t.Fatalf("Execute() = (%t, %d), error=%v; want Event Log service failure", got.serviceSpecific, got.exitCode, handler.err)
		}
	case <-time.After(time.Second):
		t.Fatal("service handler did not stop after runtime Event Log failure")
	}
}

func TestRemoveWindowsBinarySchedulesRunningExecutableForReboot(t *testing.T) {
	const binary = `C:\Program Files\XTunnel\xtunnel-agent.exe`
	delayed := ""
	scheduled, err := removeWindowsBinary(
		binary,
		strings.ToUpper(binary),
		nil,
		func(string) error { return windows.ERROR_SHARING_VIOLATION },
		func(path string) error { delayed = path; return nil },
	)
	if err != nil || !scheduled || delayed != binary {
		t.Fatalf("removeWindowsBinary() = (%v, %v), delayed=%q", scheduled, err, delayed)
	}
}

func receiveWindowsServiceState(t *testing.T, changes <-chan svc.Status) svc.State {
	t.Helper()
	select {
	case status := <-changes:
		return status.State
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Windows service state")
		return 0
	}
}

func newTestWindowsServiceLogger(t *testing.T) *slog.Logger {
	t.Helper()
	logger, err := logging.New(io.Discard, logging.Options{Level: "info", Format: "json", Component: "agent"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	return logger
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) {
	return 0, writer.err
}
