//go:build windows

package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestWindowsManagedConfigRejectsDrift(t *testing.T) {
	p := windowsInstallPaths{binary: `C:\Program Files\XTunnel\xtunnel-server.exe`, config: `C:\ProgramData\XTunnel\server.yaml`}
	expected := expectedWindowsConfig(p)
	if !isExpectedManagedWindowsService(expected, p) {
		t.Fatal("expected config rejected")
	}
	for _, test := range []struct {
		name   string
		change func(*mgr.Config)
	}{
		{"marker", func(c *mgr.Config) { c.Description += " changed" }},
		{"image arguments", func(c *mgr.Config) { c.BinaryPathName += " --set logging.level=debug" }},
		{"account", func(c *mgr.Config) { c.ServiceStartName = "LocalSystem" }},
		{"sid", func(c *mgr.Config) { c.SidType = 0 }},
		{"dependencies", func(c *mgr.Config) { c.Dependencies = []string{"foreign"} }},
		{"delayed", func(c *mgr.Config) { c.DelayedAutoStart = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := expected
			test.change(&c)
			if isExpectedManagedWindowsService(c, p) {
				t.Fatal("modified service accepted")
			}
		})
	}
	actions := expectedRecovery()
	if len(actions) != 3 || actions[0].Type != mgr.ServiceRestart || actions[1].Type != mgr.ServiceRestart || actions[2].Type != mgr.NoAction {
		t.Fatalf("unbounded recovery: %+v", actions)
	}
}

func TestWindowsServiceReadinessAndStopJoinOwner(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(commandName(command), func(t *testing.T) {
			log := &fakeWindowsEventLogger{}
			startup := make(chan struct{})
			cancelled := make(chan struct{})
			release := make(chan struct{})
			joined := make(chan struct{})
			handler := &windowsServiceHandler{open: func() (*windowsEventLogWriter, error) {
				return openWindowsEventLogWriter("test", func(string) (windowsEventLogger, error) { return log, nil })
			}, callback: func(ctx context.Context, _ io.Writer, ready func()) error {
				<-startup
				ready()
				ready()
				<-ctx.Done()
				close(cancelled)
				<-release
				close(joined)
				return nil
			}}
			requests := make(chan svc.ChangeRequest, 1)
			changes := make(chan svc.Status, 16)
			done := make(chan uint32, 1)
			go func() { _, code := handler.Execute(nil, requests, changes); done <- code }()
			if status := <-changes; status.State != svc.StartPending {
				t.Fatalf("first state %v", status.State)
			}
			select {
			case status := <-changes:
				t.Fatalf("reported readiness before callback: %+v", status)
			default:
			}
			close(startup)
			waitState(t, changes, svc.Running)
			requests <- svc.ChangeRequest{Cmd: command}
			waitState(t, changes, svc.StopPending)
			<-cancelled
			select {
			case <-done:
				t.Fatal("Execute exited without joining owner")
			default:
			}
			close(release)
			select {
			case code := <-done:
				if code != 0 || handler.err != nil {
					t.Fatalf("stop %d %v", code, handler.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("owner did not converge")
			}
			select {
			case <-joined:
			default:
				t.Fatal("callback owner not joined")
			}
		})
	}
}
func commandName(command svc.Cmd) string {
	if command == svc.Stop {
		return "stop"
	}
	return "shutdown"
}
func waitState(t *testing.T, changes <-chan svc.Status, want svc.State) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case status := <-changes:
			if status.State == want {
				return
			}
		case <-timer.C:
			t.Fatalf("missing state %v", want)
		}
	}
}

func TestWindowsServiceFailureExit(t *testing.T) {
	for _, test := range []struct {
		name      string
		callback  func(context.Context, io.Writer, func()) error
		openError error
		want      string
	}{
		{name: "callback error", callback: func(context.Context, io.Writer, func()) error { return errors.New("failed startup") }, want: "failed startup"},
		{name: "callback panic", callback: func(context.Context, io.Writer, func()) error { panic("runtime panic") }, want: "panic"},
		{name: "unexpected return", callback: func(context.Context, io.Writer, func()) error { return nil }, want: "before SCM stop"},
		{name: "event open", openError: errors.New("event source denied"), want: "event source denied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := &windowsServiceHandler{callback: test.callback, open: func() (*windowsEventLogWriter, error) {
				if test.openError != nil {
					return nil, test.openError
				}
				return openWindowsEventLogWriter("test", func(string) (windowsEventLogger, error) { return &fakeWindowsEventLogger{}, nil })
			}}
			specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 16))
			if !specific || code == 0 || handler.err == nil || !strings.Contains(handler.err.Error(), test.want) {
				t.Fatalf("failure exit %v %d %v", specific, code, handler.err)
			}
		})
	}
}
