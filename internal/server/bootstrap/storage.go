package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
)

type storage interface {
	Close() error
}

type serverStorage struct {
	database *sqlite.Store
	lock     *externallock.Lock
}

// openServerStorage 按冻结顺序取得外部锁、校验数据目录并打开数据库。
// 关键阶段保持显式排列，避免未来接入 PKI/Listener 时破坏锁的先后关系。
func openServerStorage(ctx context.Context, dataDir, runtimeDir string) (*serverStorage, error) {
	target, err := datadir.Resolve(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve stable server data target: %w", err)
	}
	lock, err := externallock.Acquire(runtimeDir, target.Hash)
	if err != nil {
		return nil, fmt.Errorf("acquire server external lock: %w", err)
	}
	failAfterLock := func(cause error) (*serverStorage, error) {
		return nil, errors.Join(cause, lock.Close())
	}

	if err := datadir.CheckPendingRestore(target); err != nil {
		return failAfterLock(fmt.Errorf("check pending restore journal: %w", err))
	}
	if err := datadir.ValidateCanonical(target); err != nil {
		return failAfterLock(fmt.Errorf("validate canonical server data directory: %w", err))
	}
	database, err := sqlite.Open(ctx, target.Path)
	if err != nil {
		return failAfterLock(fmt.Errorf("initialize SQLite: %w", err))
	}
	return &serverStorage{database: database, lock: lock}, nil
}

// Close 先关闭 SQLite，再释放覆盖整个数据目录的 External Lock。
func (storage *serverStorage) Close() error {
	databaseErr := storage.database.Close()
	lockErr := storage.lock.Close()
	return errors.Join(databaseErr, lockErr)
}
