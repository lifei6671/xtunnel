//go:build windows

// windows-scm-gate 是仅供提升权限 CI 使用的隔离 Windows SCM 故障注入进程。
package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/service"
)

const runtimeFailureDelay = time.Second

func main() {
	os.Exit(run(os.Args))
}

func run(args []string) int {
	if len(args) != 3 {
		return 2
	}
	mode := args[1]
	if mode != "runtime-failure" && mode != "stop-timeout" {
		return 2
	}

	handled, err := service.RunIfManagedService(func(ctx context.Context, _ string, _ io.Writer) error {
		firstRun, err := claimFirstRun(args[2])
		if err != nil {
			return err
		}
		if !firstRun {
			<-ctx.Done()
			return nil
		}

		switch mode {
		case "runtime-failure":
			timer := time.NewTimer(runtimeFailureDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil
			case <-timer.C:
				return errors.New("injected Windows SCM runtime failure")
			}
		case "stop-timeout":
			// 首次进程故意忽略 SCM 取消，真实经过生产 Handler 的 30 秒停止上界。
			// Handler 返回后进程退出，SCM recovery 的第二个进程走上方可取消分支。
			select {}
		}
		return nil
	})
	if err != nil {
		return 1
	}
	if !handled {
		return 2
	}
	return 0
}

func claimFirstRun(path string) (bool, error) {
	marker, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := marker.Close(); err != nil {
		return false, err
	}
	return true, nil
}
