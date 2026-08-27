//go:build !linux

package bootstrap

import (
	"context"
	"errors"
	"io"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

// acquireOnlineBackupBarrier 在非 Linux 平台快速失败，不返回 handled=false，
// 从而禁止调用方误入缺少 Linux 锁与 peer credential 保障的离线路径。
func acquireOnlineBackupBarrier(context.Context, string, string) (backupLease, bool, error) {
	return nil, false, errors.New("Server backup maintenance is only supported on Linux in XTunnel V0.1")
}

// openBackupBarrierSocket 在非 Linux 平台不发布占位 Socket；V0.1 不允许以弱化
// peer 身份或资源生命周期语义的兼容实现启动 Server。
func openBackupBarrierSocket(context.Context, string, string, *sqlite.Store, func(error)) (io.Closer, error) {
	return nil, errors.New("Server backup maintenance is only supported on Linux in XTunnel V0.1")
}
