package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/tokenkey"
)

type storage interface {
	Close() error
}

type serverStorage struct {
	database        *sqlite.Store
	lock            *externallock.Lock
	targetHash      string
	databaseExisted bool
	tokenMasterKey  tokenkey.Key
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
	_, databaseStatErr := os.Stat(filepath.Join(target.Path, "xtunnel.db"))
	databaseExisted := databaseStatErr == nil
	if databaseStatErr != nil && !errors.Is(databaseStatErr, os.ErrNotExist) {
		return failAfterLock(fmt.Errorf("inspect Server database before opening SQLite: %w", databaseStatErr))
	}
	database, err := sqlite.Open(ctx, target.Path)
	if err != nil {
		return failAfterLock(fmt.Errorf("initialize SQLite: %w", err))
	}
	failAfterDatabase := func(cause error) (*serverStorage, error) {
		return nil, errors.Join(cause, database.Close(), lock.Close())
	}
	hasTunnelTokens, err := database.HasTunnelTokens(ctx)
	if err != nil {
		return failAfterDatabase(fmt.Errorf("inspect Tunnel Token state before loading master key: %w", err))
	}
	// 完整 Tunnel Token 需要在后续“添加 Connector”时可重复取回，因此主密钥
	// 独立于 Gateway TLS 私钥并随 Server Storage 一起加载。只有空 Token 表允许
	// 首次生成；已有密文时丢失或损坏密钥必须阻止启动，不能静默制造新密钥。
	tokenMasterKey, err := tokenkey.LoadOrCreate(target.Path, hasTunnelTokens)
	if err != nil {
		return failAfterDatabase(fmt.Errorf("load Tunnel Token master key: %w", err))
	}
	return &serverStorage{
		database: database, lock: lock, targetHash: target.Hash, databaseExisted: databaseExisted,
		tokenMasterKey: tokenMasterKey,
	}, nil
}

// Close 先关闭 SQLite，再释放覆盖整个数据目录的 External Lock。
func (storage *serverStorage) Close() error {
	databaseErr := storage.database.Close()
	lockErr := storage.lock.Close()
	return errors.Join(databaseErr, lockErr)
}
