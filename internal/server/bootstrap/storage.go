package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/durableops"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/tokenkey"
)

// storage 是 bootstrap 关闭路径需要的最小资源集合；生产实现仍由 serverStorage
// 统一拥有数据库、External Lock 和 Token 主密钥生命周期。
type storage interface {
	Close() error
}

// serverStorage 持有从启动成功到全部 Listener/Session 关闭后的进程级存储资源。
// tokenMasterKey 只驻留内存，不经日志或配置传播。
type serverStorage struct {
	database        *sqlite.Store
	lock            *externallock.Lock
	dataDir         string
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

	if _, err := durableops.RecoverPendingRestore(ctx, target); err != nil {
		return failAfterLock(fmt.Errorf("recover pending Restore Journal: %w", err))
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
		database: database, lock: lock, dataDir: target.Path, targetHash: target.Hash, databaseExisted: databaseExisted,
		tokenMasterKey: tokenMasterKey,
	}, nil
}

// Close 先关闭 SQLite，再释放覆盖整个数据目录的 External Lock。
func (storage *serverStorage) Close() error {
	databaseErr := storage.database.Close()
	lockErr := storage.lock.Close()
	return errors.Join(databaseErr, lockErr)
}
