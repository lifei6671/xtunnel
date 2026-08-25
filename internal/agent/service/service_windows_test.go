//go:build windows

package service

import (
	"context"
	"strings"
	"testing"
	"time"

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

func TestWindowsServiceHandlerCancelsOnStop(t *testing.T) {
	const token = "xta_windows_service_secret"
	started := make(chan struct{})
	handler := &windowsServiceHandler{
		load:     func() (string, error) { return token, nil },
		stopWait: time.Second,
		callback: func(ctx context.Context, got string) error {
			if got != token {
				t.Errorf("callback Token was not loaded from protected credential")
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
		callback: func(context.Context, string) error {
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
		callback: func(context.Context, string) error {
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
