// Package migrations 保存 XTunnel 显式、只向前执行的数据库 Migration。
package migrations

import (
	_ "embed"
)

// SchemaMigrations 包含随 Server Binary 发布的首个 SQL Migration 和 V0.1 初始 Schema。
//
//go:embed 000001_schema_migrations.sql
var SchemaMigrations string

// TunnelDomain 包含 M1-01 的 Tunnel 与 Connection Token 前向 Migration。
//
//go:embed 000002_tunnels.sql
var TunnelDomain string

// SecurityAuditEvents 包含 M1-04 的 append-only 最小安全审计事件契约。
//
//go:embed 000003_security_audit_events.sql
var SecurityAuditEvents string
