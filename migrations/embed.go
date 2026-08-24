// Package migrations 保存 XTunnel 显式、只向前执行的数据库 Migration。
package migrations

import (
	_ "embed"
)

// SchemaMigrations 包含随 Server Binary 发布的首个 SQL Migration。
//
//go:embed 000001_schema_migrations.sql
var SchemaMigrations string
