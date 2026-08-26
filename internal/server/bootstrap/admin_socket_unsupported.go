//go:build !linux

package bootstrap

import (
	"context"
	"errors"
	"io"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

func requestAdminBootstrap(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func openAdminBootstrapSocket(context.Context, string, string, *sqlite.Store) (io.Closer, error) {
	return nil, errors.New("admin bootstrap socket is only supported on Linux in XTunnel V0.1")
}

func openAdminBootstrapSocketAfter(context.Context, string, string, *sqlite.Store, func() error) (io.Closer, error) {
	return nil, errors.New("admin bootstrap socket is only supported on Linux in XTunnel V0.1")
}
