package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/migrations"
	"gorm.io/gorm"
)

// migration 描述一个不可拆分、只向前执行的数据库版本。statements 必须按声明顺序
// 在同一事务内完成，最后才记录版本，避免部分 DDL 被误认为已应用。
type migration struct {
	version    int
	before     func(context.Context, *gorm.DB) error
	prepare    func(context.Context, *gorm.DB) error
	statements []string
}

// productionMigrations 是当前二进制唯一认可的连续迁移链；数组顺序即版本顺序。
var productionMigrations = []migration{
	{
		version: 1,
		statements: []string{
			migrations.SchemaMigrations,
		},
	},
	{
		version: 2,
		statements: []string{
			migrations.TunnelDomain,
		},
	},
	{
		version: 3,
		statements: []string{
			migrations.SecurityAuditEvents,
		},
	},
	{
		version: 4,
		statements: []string{
			migrations.CredentialLifecycleAudit,
		},
	},
	{
		version: 5,
		statements: []string{
			migrations.ServiceDomain,
		},
	},
	{
		version: 6,
		statements: []string{
			migrations.TunnelFirstAuthentication,
		},
	},
	{
		version: 7,
		statements: []string{
			migrations.RouteDomain,
		},
	},
	{
		version: 8,
		statements: []string{
			migrations.ServiceProxyOptions,
		},
	},
	{
		version: 9,
		prepare: func(ctx context.Context, transaction *gorm.DB) error {
			return migrateLegacyAdminIDs(ctx, transaction, identity.NewAdminID)
		},
		statements: []string{
			migrations.AdminSessions,
		},
	},
	{
		version: 10,
		prepare: migrateServiceExposureUniqueness,
		statements: []string{
			migrations.ServiceExposure,
		},
	},
	{
		version: 11,
		before:  enableIncrementalVacuum,
		statements: []string{
			migrations.UsageAggregation,
		},
	},
}

// enableIncrementalVacuum 在 v11 事务开始前一次性转换数据库文件格式。
// VACUUM 不能在事务内执行；转换先于版本记录且可安全重跑，失败时 v11 仍未应用。
func enableIncrementalVacuum(ctx context.Context, database *gorm.DB) error {
	return database.WithContext(ctx).Connection(func(connection *gorm.DB) error {
		var mode int
		if err := connection.Raw("PRAGMA auto_vacuum").Scan(&mode).Error; err != nil {
			return fmt.Errorf("read SQLite auto_vacuum mode: %w", err)
		}
		if mode == 2 {
			return nil
		}
		if err := connection.Exec("PRAGMA auto_vacuum = INCREMENTAL").Error; err != nil {
			return fmt.Errorf("enable SQLite incremental auto_vacuum: %w", err)
		}
		if err := connection.Exec("VACUUM").Error; err != nil {
			return fmt.Errorf("convert SQLite database for incremental vacuum: %w", err)
		}
		if err := connection.Raw("PRAGMA auto_vacuum").Scan(&mode).Error; err != nil {
			return fmt.Errorf("verify SQLite auto_vacuum mode: %w", err)
		}
		if mode != 2 {
			return fmt.Errorf("SQLite auto_vacuum mode is %d, want incremental", mode)
		}
		return nil
	})
}

// migrateServiceExposureUniqueness 在建立唯一索引和跨表触发器前检查历史数据。
// 任一重复都会阻止升级，避免启动后把多个入口静默解释为某一个 Exposure。
func migrateServiceExposureUniqueness(ctx context.Context, transaction *gorm.DB) error {
	checks := []struct {
		name  string
		query string
	}{
		{
			name:  "HTTP",
			query: "SELECT COUNT(*) FROM (SELECT service_id FROM http_routes GROUP BY service_id HAVING COUNT(*) > 1)",
		},
		{
			name:  "TCP",
			query: "SELECT COUNT(*) FROM (SELECT service_id FROM tcp_routes GROUP BY service_id HAVING COUNT(*) > 1)",
		},
		{
			name: "cross-type",
			query: `SELECT COUNT(*) FROM (
				SELECT http_routes.service_id FROM http_routes
				INNER JOIN tcp_routes ON tcp_routes.service_id = http_routes.service_id
				GROUP BY http_routes.service_id
			)`,
		},
	}
	for _, check := range checks {
		var duplicateServices int64
		if err := transaction.WithContext(ctx).Raw(check.query).Scan(&duplicateServices).Error; err != nil {
			return fmt.Errorf("inspect %s service exposure uniqueness: %w", check.name, err)
		}
		if duplicateServices != 0 {
			return fmt.Errorf("%s service exposure uniqueness violated by %d service(s)", check.name, duplicateServices)
		}
	}
	return nil
}

// migrate 使用生产迁移集合把数据库推进到当前二进制支持的最新版本。
func migrate(ctx context.Context, database *gorm.DB) error {
	return runMigrations(ctx, database, productionMigrations, time.Now)
}

// runMigrations 先验证代码侧与数据库侧版本都从 1 连续递增，再逐版本事务提交。
// 数据库比二进制更新或存在版本空洞时立即失败，禁止猜测、跳过或自动降级。
func runMigrations(ctx context.Context, database *gorm.DB, available []migration, now func() time.Time) error {
	for index, candidate := range available {
		want := index + 1
		if candidate.version != want {
			return fmt.Errorf("available migration version %d is not contiguous, want %d", candidate.version, want)
		}
	}

	applied, tableExists, err := appliedVersions(ctx, database)
	if err != nil {
		return err
	}
	if tableExists && len(applied) == 0 {
		return fmt.Errorf("schema_migrations exists without an applied version")
	}
	for index, version := range applied {
		want := index + 1
		if version != want {
			return fmt.Errorf("schema_migrations has non-contiguous version %d, want %d", version, want)
		}
	}
	if len(applied) > len(available) {
		return fmt.Errorf("database schema version %d is newer than supported version %d", applied[len(applied)-1], len(available))
	}

	for _, next := range available[len(applied):] {
		if next.before != nil {
			if err := next.before(ctx, database); err != nil {
				return fmt.Errorf("prepare migration %d outside transaction: %w", next.version, err)
			}
		}
		if err := database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
			if next.prepare != nil {
				if err := next.prepare(ctx, transaction); err != nil {
					return fmt.Errorf("prepare migration %d: %w", next.version, err)
				}
			}
			for _, statement := range next.statements {
				if err := transaction.Exec(statement).Error; err != nil {
					return fmt.Errorf("execute migration %d: %w", next.version, err)
				}
			}
			if err := transaction.Exec(
				"INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
				next.version,
				now().UTC().Unix(),
			).Error; err != nil {
				return fmt.Errorf("record migration %d: %w", next.version, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// migrateLegacyAdminIDs 在 admin_sessions 外键建立前修正开发期遗留的 UUID 主键。
// 整个转换由 Migration 事务拥有；任一损坏 ID 或写入失败都会连同 v9 DDL 一起回滚。
func migrateLegacyAdminIDs(
	ctx context.Context,
	transaction *gorm.DB,
	newAdminID func() (string, error),
) error {
	var admins []AdminUser
	if err := transaction.WithContext(ctx).Order(AdminUserColumns.CreatedAt + " ASC").
		Order(AdminUserColumns.ID + " ASC").Find(&admins).Error; err != nil {
		return fmt.Errorf("read admin identifiers: %w", err)
	}
	for index, admin := range admins {
		if identity.ValidAdminID(admin.ID) {
			continue
		}
		legacy, err := uuid.Parse(admin.ID)
		if err != nil || legacy.String() != admin.ID {
			return fmt.Errorf("admin identifier at row %d is neither adm_ ULID nor canonical UUID", index+1)
		}
		replacement, err := newAdminID()
		if err != nil {
			return fmt.Errorf("generate replacement admin identifier at row %d: %w", index+1, err)
		}
		if !identity.ValidAdminID(replacement) {
			return fmt.Errorf("generated replacement admin identifier at row %d is invalid", index+1)
		}
		result := transaction.WithContext(ctx).Model(&AdminUser{}).
			Where(AdminUserColumns.ID+" = ?", admin.ID).
			Update(AdminUserColumns.ID, replacement)
		if result.Error != nil {
			return fmt.Errorf("replace legacy admin identifier at row %d: %w", index+1, result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("replace legacy admin identifier at row %d affected %d rows, want 1", index+1, result.RowsAffected)
		}
	}
	return nil
}

// appliedVersions 区分“迁移表尚不存在”和“迁移表存在但没有版本”两种状态；后者
// 表示数据库初始化被破坏，调用方必须阻止启动而不是把它当成新库重新执行。
func appliedVersions(ctx context.Context, database *gorm.DB) ([]int, bool, error) {
	var tableCount int64
	if err := database.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'",
	).Scan(&tableCount).Error; err != nil {
		return nil, false, fmt.Errorf("inspect schema_migrations table: %w", err)
	}
	if tableCount == 0 {
		return nil, false, nil
	}

	var versions []int
	if err := database.WithContext(ctx).Table("schema_migrations").Order("version ASC").Pluck("version", &versions).Error; err != nil {
		return nil, true, fmt.Errorf("read applied migrations: %w", err)
	}
	return versions, true, nil
}
