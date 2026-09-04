//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/safego"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// RunIfService 在 SCM 上下文先进入 Dispatcher；配置与业务资源仅在 Execute 回调内打开。
// callback 拥有全部运行资源，并负责取消后 30 秒排空、Force Close 和等待 owner 退出。
func RunIfService(callback func(context.Context, io.Writer, func()) error) (bool, error) {
	managed, err := svc.IsWindowsService()
	if err != nil {
		return false, fmt.Errorf("detect Server SCM context: %w", err)
	}
	if !managed {
		return false, nil
	}
	if err := validateServicePlatform(runtime.GOOS, runtime.GOARCH); err != nil {
		return true, err
	}
	handler := &windowsServiceHandler{callback: callback}
	if err := svc.Run(windowsServiceName, handler); err != nil {
		return true, fmt.Errorf("run Server service dispatcher: %w", err)
	}
	return true, handler.err
}

type windowsServiceHandler struct {
	callback func(context.Context, io.Writer, func()) error
	// open 在 dispatcher 内运行，测试可以注入无主机副作用的 Event Log。
	open func() (*windowsEventLogWriter, error)
	err  error
}

func (handler *windowsServiceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending, WaitHint: 30000}
	open := handler.open
	if open == nil {
		open = func() (*windowsEventLogWriter, error) {
			if err := validateRuntimeService(); err != nil {
				return nil, err
			}
			if err := requireManagedWindowsEventSource(registryWindowsEventSourceStore{}); err != nil {
				return nil, err
			}
			return openWindowsEventLogWriter(windowsEventLogSource, func(source string) (windowsEventLogger, error) { return eventlog.Open(source) })
		}
	}
	writer, err := open()
	if err != nil {
		handler.err = err
		return true, 1
	}
	logger, err := logging.New(writer, logging.Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		handler.err = errors.Join(err, writer.Close())
		return true, 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	event := func(level slog.Level, name, code string) error {
		record := slog.NewRecord(time.Now(), level, name, 0)
		if code != "" {
			record.AddAttrs(slog.String(logging.ErrorCodeKey, code))
		}
		return logger.Handler().Handle(ctx, record)
	}
	if err := event(slog.LevelInfo, logging.EventWindowsServiceStarting, ""); err != nil {
		handler.err = errors.Join(err, writer.Close())
		return true, 1
	}
	ready := make(chan struct{})
	var readyOnce sync.Once
	done := make(chan error, 1)
	// 唯一 callback goroutine 始终由 Execute 等待；Stop 不会遗弃资源 owner。
	safego.Go(func(err error) { done <- fmt.Errorf("Server service callback: %w", err) }, nil, func() { done <- handler.callback(ctx, writer, func() { readyOnce.Do(func() { close(ready) }) }) })
	state := svc.StartPending
	checkpoint := uint32(0)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var failure error
	stopping := false
	stop := func(err error) {
		failure = errors.Join(failure, err)
		if stopping {
			return
		}
		stopping = true
		ready = nil
		state = svc.StopPending
		checkpoint = 1
		changes <- svc.Status{State: state, CheckPoint: checkpoint, WaitHint: 30000}
		cancel()
	}
	for {
		select {
		case err := <-done:
			if !stopping && err == nil {
				err = errors.New("Server runtime exited before SCM stop")
			}
			failure = errors.Join(failure, err)
			select {
			case logErr := <-writer.Failures():
				failure = errors.Join(failure, logErr)
			default:
			}
			if failure != nil {
				failure = errors.Join(failure, event(slog.LevelError, logging.EventWindowsServiceFailed, "RUNTIME_FAILED"))
			} else {
				failure = event(slog.LevelInfo, logging.EventWindowsServiceStopped, "")
			}
			handler.err = errors.Join(failure, writer.Close())
			if handler.err != nil {
				return true, 1
			}
			return false, 0
		case <-ready:
			ready = nil
			if err := event(slog.LevelInfo, logging.EventWindowsServiceRunning, ""); err != nil {
				stop(err)
				continue
			}
			state = svc.Running
			changes <- svc.Status{State: state, Accepts: svc.AcceptStop | svc.AcceptShutdown}
		case err := <-writer.Failures():
			stop(err)
		case request, ok := <-requests:
			if !ok {
				requests = nil
				stop(errors.New("SCM control channel closed"))
				continue
			}
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				if !stopping {
					stop(event(slog.LevelInfo, logging.EventWindowsServiceStopRequested, ""))
				}
			}
		case <-ticker.C:
			if state != svc.Running {
				checkpoint++
				changes <- svc.Status{State: state, CheckPoint: checkpoint, WaitHint: 30000}
			}
		}
	}
}
