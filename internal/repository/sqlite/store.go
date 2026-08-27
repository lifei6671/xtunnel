// Package sqlite 提供基于 GORM 的 XTunnel SQLite 持久化基座。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	libsqlite "github.com/libtnb/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	databaseFilename   = "xtunnel.db"
	maxOpenConnections = 4
)

// Store 持有 GORM 和其底层 database/sql 连接池。
// 后续业务 Repository 将在本包内复用同一个 GORM 事务作用域。
type Store struct {
	database  *gorm.DB
	pool      *sql.DB
	writeGate *writeGate
}

// Open 打开固定的数据目录数据库、校验逐连接 PRAGMA，并执行显式 Migration。
func Open(ctx context.Context, dataDir string) (*Store, error) {
	databasePath := filepath.Join(dataDir, databaseFilename)
	database, err := gorm.Open(libsqlite.Open(databaseDSN(databasePath)), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open SQLite database %q: %w", databasePath, err)
	}

	pool, err := database.DB()
	if err != nil {
		return nil, fmt.Errorf("get SQLite connection pool: %w", err)
	}
	pool.SetMaxOpenConns(maxOpenConnections)
	pool.SetMaxIdleConns(maxOpenConnections)

	store := &Store{database: database, pool: pool, writeGate: newWriteGate()}
	if err := store.verifyConnection(ctx); err != nil {
		return nil, closeAfterError(store, err)
	}
	if err := migrate(ctx, database); err != nil {
		return nil, closeAfterError(store, err)
	}
	return store, nil
}

// Close 关闭 GORM 使用的底层连接池。
func (store *Store) Close() error {
	if err := store.pool.Close(); err != nil {
		return fmt.Errorf("close SQLite connection pool: %w", err)
	}
	return nil
}

// HasTunnelTokens 返回数据库是否已经保存过 Tunnel Credential。
//
// Server Bootstrap 用它判断 Token 保护主密钥能否首次生成：空表允许创建密钥；
// 表中已有密文但密钥缺失时必须快速失败。查询只读取 EXISTS 布尔值，不接触
// secret_hash、token_ciphertext 或其他敏感列。
func (store *Store) HasTunnelTokens(ctx context.Context) (bool, error) {
	var exists bool
	if err := store.database.WithContext(ctx).Raw(
		"SELECT EXISTS(SELECT 1 FROM " + TunnelTokenTable + " LIMIT 1)",
	).Scan(&exists).Error; err != nil {
		return false, fmt.Errorf("inspect tunnel token presence: %w", err)
	}
	return exists, nil
}

// MarkFirstAuthenticated 在完整 AUTH Success Frame 写出后耐久记录 Tunnel 的首次
// 成功认证事实。已设置的 Tunnel 只走普通只读视图，避免每次重连争抢 SQLite 写锁；
// 只有首次写入才使用 FULL 提交，事务内的 NULL 条件负责收敛并发首次认证。
func (store *Store) MarkFirstAuthenticated(ctx context.Context, tunnelID string, authenticatedAt int64) error {
	if !validate.ValidID(tunnelID, "tun_") || authenticatedAt <= 0 {
		return repository.ErrInvalidTunnel
	}
	var alreadyAuthenticated bool
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		tunnel, err := view.Tunnels().Get(ctx, tunnelID)
		if err != nil {
			return err
		}
		alreadyAuthenticated = tunnel.FirstAuthenticatedAt != nil
		return nil
	}); err != nil {
		return err
	}
	if alreadyAuthenticated {
		return nil
	}

	return store.WithDurableTx(ctx, func(transaction repository.TxStore) error {
		return transaction.Tunnels().MarkFirstAuthenticated(ctx, tunnelID, authenticatedAt)
	})
}

// verifyConnection 在独占连接上验证 DSN 已设置的 SQLite 连接级约束。连接池中的
// 每个新连接都必须满足 foreign_keys、busy_timeout 与 WAL 语义，不能依赖进程环境。
func (store *Store) verifyConnection(ctx context.Context) (resultErr error) {
	connection, err := store.pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire SQLite connection for startup check: %w", err)
	}
	defer func() {
		if err := connection.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close SQLite startup-check connection: %w", err))
		}
	}()

	checks := []struct {
		pragma string
		want   string
	}{
		{pragma: "foreign_keys", want: "1"},
		{pragma: "busy_timeout", want: "5000"},
		{pragma: "journal_mode", want: "wal"},
		{pragma: "synchronous", want: "1"},
	}
	for _, check := range checks {
		var value string
		if err := connection.QueryRowContext(ctx, "PRAGMA "+check.pragma).Scan(&value); err != nil {
			return fmt.Errorf("query SQLite PRAGMA %s: %w", check.pragma, err)
		}
		if strings.ToLower(value) != check.want {
			return fmt.Errorf("SQLite PRAGMA %s is %q, want %q", check.pragma, value, check.want)
		}
	}
	return nil
}

// databaseDSN 为运行时读写连接构造固定的 SQLite URI 参数。
func databaseDSN(databasePath string) string {
	slashPath := filepath.ToSlash(databasePath)
	// 直接拼成 file:<escaped-path>，避免 Windows 盘符被 net/url 误写成 URI authority。
	escapedPath := (&url.URL{Path: slashPath}).EscapedPath()
	query := make(url.Values)
	query.Set("mode", "rwc")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + escapedPath + "?" + query.Encode()
}

// closeAfterError 在初始化中途失败时保留首因并合并数据库关闭错误。
func closeAfterError(store *Store, cause error) error {
	if err := store.Close(); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
