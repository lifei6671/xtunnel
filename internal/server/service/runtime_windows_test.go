//go:build windows

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
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

// 硬期限触发后 callback 仍拥有资源收敛。SCM 必须等待它完成，并保留
// DeadlineExceeded 的失败语义与事件，而不能把“进程已经退出”改写成正常停止。
func TestWindowsServiceStopRetainsDrainDeadline(t *testing.T) {
	log := &fakeWindowsEventLogger{}
	cancelled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseOwner := func() { releaseOnce.Do(func() { close(release) }) }
	joined := make(chan struct{})
	handler := &windowsServiceHandler{
		open: func() (*windowsEventLogWriter, error) {
			return openWindowsEventLogWriter("test", func(string) (windowsEventLogger, error) { return log, nil })
		},
		callback: func(ctx context.Context, _ io.Writer, ready func()) error {
			ready()
			<-ctx.Done()
			close(cancelled)
			<-release
			close(joined)
			return context.DeadlineExceeded
		},
	}
	requests := make(chan svc.ChangeRequest, 1)
	changes := make(chan svc.Status, 16)
	type exit struct {
		specific bool
		code     uint32
	}
	done := make(chan exit, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		specific, code := handler.Execute(nil, requests, changes)
		done <- exit{specific, code}
	}()
	t.Cleanup(func() {
		releaseOwner()
		close(requests)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("SCM test owner cleanup did not finish")
		}
	})
	waitState(t, changes, svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	waitState(t, changes, svc.StopPending)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("SCM stop did not cancel callback")
	}
	select {
	case <-done:
		t.Fatal("SCM exited before callback owner completed")
	default:
	}
	releaseOwner()
	select {
	case result := <-done:
		if !result.specific || result.code != 1 || !errors.Is(handler.err, context.DeadlineExceeded) {
			t.Fatalf("drain failure result: specific=%v code=%d err=%v", result.specific, result.code, handler.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SCM did not join callback owner")
	}
	select {
	case <-joined:
	default:
		t.Fatal("callback owner was not joined")
	}
	failed := 0
	for _, entry := range log.records {
		var event map[string]any
		if err := json.Unmarshal([]byte(entry.message), &event); err != nil {
			t.Fatal(err)
		}
		if event["event"] == "windows_service_failed" && event["error_code"] == "RUNTIME_FAILED" {
			failed++
		}
		if event["event"] == "windows_service_stopped" {
			t.Fatal("drain failure reported normal service stop")
		}
	}
	if failed != 1 {
		t.Fatalf("RUNTIME_FAILED event count=%d want=1", failed)
	}
}
