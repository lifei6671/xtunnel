package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"

	libsqlite "github.com/libtnb/sqlite"

	"github.com/lifei6671/xtunnel/internal/repository"
)

// TunnelTokensSchemaVersion 是 tunnel_tokens 首次成为持久化 Schema 的版本。
// 更早的合法数据库没有该表，维护校验只能按其实际 Schema 检查，不能为了读取
// Token 而在候选目录内运行 Migration；向前迁移仍只属于正常 Server 启动流程。
const TunnelTokensSchemaVersion = 2

// CurrentSchemaVersion 返回当前二进制支持的最高连续 Migration 版本。版本来自唯一
// migration 列表而非独立常量，避免备份 Manifest 与运行时迁移能力发生漂移。
func CurrentSchemaVersion() int {
	return len(productionMigrations)
}

// ValidateBackupDatabase 以 immutable 只读方式校验恢复候选数据库的页结构与精确
// Schema 版本。它绝不运行 Migration，也不创建 sidecar，避免维护命令在目录切换
// 前改变已由 Manifest 摘要约束的归档内容。
//
// expectedSchemaVersion 来自已验证 Manifest，必须与数据库内连续 migration 记录
// 精确相等；这里只接受 PRAGMA quick_check 唯一一行 "ok"，任何额外诊断行都视为
// 页结构不可信并快速失败。
func ValidateBackupDatabase(ctx context.Context, databasePath string, expectedSchemaVersion int) (resultErr error) {
	pool, err := sql.Open(libsqlite.DriverName, immutableDatabaseDSN(databasePath))
	if err != nil {
		return fmt.Errorf("open SQLite database for integrity validation: %w", err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close SQLite integrity validation pool: %w", err))
		}
	}()
	version, err := inspectSchemaVersion(ctx, pool)
	if err != nil {
		return err
	}
	if version != expectedSchemaVersion {
		return fmt.Errorf("SQLite schema version is %d, want %d", version, expectedSchemaVersion)
	}
	rows, err := pool.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("run SQLite quick_check: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close SQLite quick_check rows: %w", err))
		}
	}()
	checks := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("scan SQLite quick_check result: %w", err)
		}
		checks++
		if result != "ok" {
			return fmt.Errorf("SQLite quick_check failed: %s", result)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate SQLite quick_check results: %w", err)
	}
	if checks != 1 {
		return fmt.Errorf("SQLite quick_check returned %d rows, want 1", checks)
	}
	return nil
}

// ValidateBackupTunnelTokens 以 immutable 只读视图遍历备份中的全部 Token 行。
// validate 负责使用同一归档中的主密钥解密，并核对明文身份、版本和 secret hash；
// 本函数只负责稳定地还原 Repository 元数据，不会迁移候选数据库。
//
// 敏感字段只在当前行存活：完成领域转换后立即 clear 扫描缓冲区，回调返回后再
// clear 传出的密文副本。错误只带行号和操作上下文，不拼接密文、摘要、明文或密钥。
func ValidateBackupTunnelTokens(
	ctx context.Context,
	databasePath string,
	validate func(repository.TunnelToken) error,
) (resultErr error) {
	if validate == nil {
		return errors.New("Tunnel Token backup validator is nil")
	}
	pool, err := sql.Open(libsqlite.DriverName, immutableDatabaseDSN(databasePath))
	if err != nil {
		return fmt.Errorf("open SQLite database for Tunnel Token validation: %w", err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close SQLite Tunnel Token validation pool: %w", err))
		}
	}()
	rows, err := pool.QueryContext(ctx, `
		SELECT id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at, revoked_at
		FROM tunnel_tokens
		ORDER BY tunnel_id, version`)
	if err != nil {
		return fmt.Errorf("read SQLite Tunnel Token rows: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close SQLite Tunnel Token rows: %w", err))
		}
	}()
	// 行号只用于定位损坏记录，不使用 Token/Tunnel ID，避免校验错误成为敏感
	// 元数据的旁路输出。ORDER BY 使同一归档的失败位置可重复。
	rowNumber := 0
	for rows.Next() {
		rowNumber++
		var record tunnelTokenRecord
		if err := rows.Scan(
			&record.ID, &record.TunnelID, &record.SecretHash, &record.TokenCiphertext,
			&record.Version, &record.Status, &record.CreatedAt, &record.RevokedAt,
		); err != nil {
			return fmt.Errorf("scan SQLite Tunnel Token row %d: %w", rowNumber, err)
		}
		if len(record.SecretHash) != sha256.Size {
			clear(record.TokenCiphertext)
			return fmt.Errorf("SQLite Tunnel Token row %d has invalid secret hash length", rowNumber)
		}
		metadata := record.toDomain()
		clear(record.SecretHash)
		clear(record.TokenCiphertext)
		if err := validate(metadata); err != nil {
			clear(metadata.TokenCiphertext)
			return fmt.Errorf("validate SQLite Tunnel Token row %d: %w", rowNumber, err)
		}
		clear(metadata.TokenCiphertext)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate SQLite Tunnel Token rows: %w", err)
	}
	return nil
}

// InspectSchemaVersion 以只读模式检查数据库版本，绝不创建表或执行 Migration。
// 尚无 schema_migrations 的空数据库返回 0；非连续或高于当前二进制的版本会失败，
// 因为维护命令不能猜测缺失 Migration，也不能降级解释未来二进制写出的 Schema。
func InspectSchemaVersion(ctx context.Context, databasePath string) (version int, resultErr error) {
	pool, err := sql.Open(libsqlite.DriverName, readOnlyDatabaseDSN(databasePath))
	if err != nil {
		return 0, fmt.Errorf("open SQLite database for schema inspection: %w", err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close SQLite schema inspection pool: %w", err))
		}
	}()
	return inspectSchemaVersion(ctx, pool)
}

// inspectSchemaVersion 在调用方提供的只读池上验证 migration 序列必须严格为
// 1..N。它与运行时 migrate 分离，确保备份检查不会获得写能力或修复候选状态。
func inspectSchemaVersion(ctx context.Context, pool *sql.DB) (version int, resultErr error) {
	var tableCount int
	if err := pool.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'",
	).Scan(&tableCount); err != nil {
		return 0, fmt.Errorf("inspect schema_migrations table: %w", err)
	}
	if tableCount == 0 {
		return 0, nil
	}

	rows, err := pool.QueryContext(ctx, "SELECT version FROM schema_migrations ORDER BY version ASC")
	if err != nil {
		return 0, fmt.Errorf("read applied SQLite schema versions: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close applied SQLite schema versions: %w", err))
		}
	}()

	// 从 1 开始逐项比较，而不是只取 MAX(version)，防止缺口或重复记录被误判为
	// 一个已完整应用的 Schema 版本。
	want := 1
	for rows.Next() {
		var applied int
		if err := rows.Scan(&applied); err != nil {
			return 0, fmt.Errorf("scan applied SQLite schema version: %w", err)
		}
		if applied != want {
			return 0, fmt.Errorf("schema_migrations has non-contiguous version %d, want %d", applied, want)
		}
		version = applied
		want++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate applied SQLite schema versions: %w", err)
	}
	if version == 0 {
		return 0, errors.New("schema_migrations exists without an applied version")
	}
	if version > CurrentSchemaVersion() {
		return 0, fmt.Errorf(
			"database schema version %d is newer than supported version %d",
			version,
			CurrentSchemaVersion(),
		)
	}
	return version, nil
}
