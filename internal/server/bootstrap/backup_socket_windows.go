//go:build windows

package bootstrap

import (
	"context"
	"errors"
	"io"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

func acquireOnlineBackupBarrier(context.Context, string, string) (backupLease, bool, error) {
	return nil, false, errors.New("Server backup maintenance is only supported on Linux in XTunnel V0.1")
}

// Windows Preview 的业务启动不发布 Backup Barrier；显式备份命令仍快速失败。
func openBackupBarrierSocket(context.Context, string, string, *sqlite.Store, func(error)) (io.Closer, error) {
	return nil, nil
}
