package repository

import (
	"context"
	"errors"
)

var (
	// ErrInvalidAdminCredentials 对外统一表示用户名或密码不匹配，禁止泄露账户是否存在。
	ErrInvalidAdminCredentials = errors.New("admin credentials are invalid")
	// ErrAdminSessionNotFound 表示 Cookie 摘要没有命中持久化 Session。
	ErrAdminSessionNotFound = errors.New("admin session was not found")
	// ErrAdminSessionExpired 表示 Session 已超过绝对或空闲有效期。
	ErrAdminSessionExpired = errors.New("admin session has expired")
)

// AdminUser 是 Management 认证使用的管理员公开身份。
// Password Hash 属于 SQLite 内部实现，不得越过 Repository 边界。
type AdminUser struct {
	ID       string
	Username string
}

// AdminSession 保存 Admin Session 的持久化安全状态。
// TokenHash 是 Cookie 原文的 SHA-256，CSRFToken 是另一份独立随机值；二者都不得记录日志。
type AdminSession struct {
	ID         string
	UserID     string
	TokenHash  [32]byte
	CSRFToken  [32]byte
	ExpiresAt  int64
	CreatedAt  int64
	LastSeenAt int64
	Admin      AdminUser
}

// AdminAuthenticationStore 定义 Admin 登录与 Session 生命周期需要的最小持久化边界。
// 密码校验留在实现侧，避免 PHC Hash 进入 Application 或 HTTP Handler。
type AdminAuthenticationStore interface {
	HasAdmin(context.Context) (bool, error)
	VerifyAdminCredentials(context.Context, string, string) (AdminUser, error)
	CreateAdminSession(context.Context, AdminSession, int64) error
	GetAdminSessionByTokenHash(context.Context, [32]byte, int64, int64) (AdminSession, error)
	TouchAdminSession(context.Context, string, int64) error
	DeleteAdminSession(context.Context, string) error
	DeleteExpiredAdminSessions(context.Context, int64, int64, int) (int64, error)
}
