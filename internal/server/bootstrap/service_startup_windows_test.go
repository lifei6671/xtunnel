//go:build windows

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/service"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// TestMain 的 SCM 分支仅供一次性 CI 使用固定测试配置调查启动失败。
// 生产二进制不包含该诊断，普通测试仍由 m.Run 执行全部原有用例。
func TestMain(m *testing.M) {
	managed, err := svc.IsWindowsService()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !managed {
		os.Exit(m.Run())
	}
	handled, err := service.RunIfService(func(ctx context.Context, writer io.Writer, ready func()) error {
		startupErr := runService(ctx, writer, ready, os.Args[1:], os.Environ())
		if startupErr == nil {
			return nil
		}
		// 在回调返回、SCM 宣告 Stopped 之前记录具体失败；不依赖停止后的 IO。
		// 调查 Runner 没有真实凭据，原产品 JSON 日志与 Secret 规则保持原样。
		log, openErr := eventlog.Open("XTunnelServer")
		if openErr != nil {
			return errors.Join(startupErr, fmt.Errorf("open diagnostic Event Log: %w", openErr))
		}
		writeErr := log.Error(3, "XTunnelServer startup diagnostic: "+startupErr.Error())
		closeErr := log.Close()
		return errors.Join(startupErr, writeErr, closeErr)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !handled {
		os.Exit(1)
	}
	os.Exit(0)
}
