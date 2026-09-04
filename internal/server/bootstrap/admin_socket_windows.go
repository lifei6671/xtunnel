//go:build windows

package bootstrap

import (
	"context"
	"errors"
	"io"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

// Windows 首个管理员通过已停止服务的离线事务创建，未初始化时运行时只保留
// Management/Metrics。这里不建立维护通道；重启后才重新读取管理员状态。
func openAdminBootstrapSocketAfter(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
	return nil, nil
}

func requestAdminBootstrap(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func openAdminBootstrapSocket(context.Context, string, string, *sqlite.Store) (io.Closer, error) {
	return nil, errors.New("Windows Server administrator creation requires offline maintenance")
}
