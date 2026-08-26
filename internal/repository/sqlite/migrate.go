package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/lifei6671/xtunnel/migrations"
	"gorm.io/gorm"
)

type migration struct {
	version    int
	statements []string
}

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
}

func migrate(ctx context.Context, database *gorm.DB) error {
	return runMigrations(ctx, database, productionMigrations, time.Now)
}

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
		if err := database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
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
