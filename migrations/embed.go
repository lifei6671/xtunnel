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

// RouteDomain 包含 M4-01 的 HTTP/TCP Route Desired State 与全局 Generation。
//
//go:embed 000007_routes.sql
var RouteDomain string

// ServiceProxyOptions 包含 V0.1 Service 代理行为的类型化持久化列。
//
//go:embed 000008_service_proxy_options.sql
var ServiceProxyOptions string

// AdminSessions 包含 M5-03 的管理员 Session、CSRF 与过期索引前向 Migration。
//
//go:embed 000009_admin_sessions.sql
var AdminSessions string

// ServiceExposure 包含每个 Service 至多一个 HTTP/TCP 公网 Exposure 的数据库约束。
//
//go:embed 000010_service_exposure.sql
var ServiceExposure string

// UsageAggregation 包含 M6-04 minute/hour/day Usage 聚合表。
//
//go:embed 000011_usage_aggregation.sql
var UsageAggregation string
