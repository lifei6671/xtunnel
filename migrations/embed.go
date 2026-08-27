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

// CredentialLifecycleAudit 扩展 M2 管理面 Credential 生命周期审计，同时保留 v3 证据。
//
//go:embed 000004_credential_lifecycle_audit.sql
var CredentialLifecycleAudit string

// ServiceDomain 包含 M3-01 的 Service 聚合与 Tunnel 直接引用前向 Migration。
//
//go:embed 000005_services.sql
var ServiceDomain string

// TunnelFirstAuthentication 包含 M3-11 跨 Server 重启保留的首次认证事实。
//
//go:embed 000006_tunnel_first_authentication.sql
var TunnelFirstAuthentication string
