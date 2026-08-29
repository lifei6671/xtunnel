package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	adminSessionAbsoluteTTL = 12 * time.Hour
	adminSessionIdleTTL     = 30 * time.Minute
	adminSessionTouchAfter  = time.Minute
	adminTokenBytes         = 32
	adminSessionCleanupSize = 128
)

var (
	// ErrAdminSetupRequired 表示数据库尚未通过本机 Bootstrap 创建首个管理员。
	ErrAdminSetupRequired = errors.New("admin setup is required")
	// ErrAdminAuthenticationFailed 对外统一表示用户名或密码错误。
	ErrAdminAuthenticationFailed = errors.New("admin authentication failed")
	// ErrAdminSessionExpired 对外统一表示 Cookie 缺失、无效或 Session 已过期。
	ErrAdminSessionExpired = errors.New("admin session expired")
)

// AdminAuthSession 是 Handler 可以安全返回的认证结果。
// SessionToken 只允许写入 HttpOnly Cookie，CSRFToken 只允许写入响应 Body。
type AdminAuthSession struct {
	SessionID    string
	Admin        repository.AdminUser
	SessionToken string
	CSRFToken    string
	ExpiresAt    time.Time
}

// AdminAuthenticationService 是 Admin 密码与持久化 Session 生命周期的唯一 Application Owner。
type AdminAuthenticationService struct {
	store        repository.AdminAuthenticationStore
	now          func() time.Time
	random       io.Reader
	newSessionID func() (string, error)
}

// NewAdminAuthenticationService 创建使用生产时钟和 CSPRNG 的认证服务。
func NewAdminAuthenticationService(store repository.AdminAuthenticationStore) *AdminAuthenticationService {
	return &AdminAuthenticationService{
		store:        store,
		now:          time.Now,
		random:       rand.Reader,
		newSessionID: identity.NewAdminSessionID,
	}
}

// Login 校验密码，随后在一个 Repository 事务中创建 Session 并记录成功登录时间。
func (service *AdminAuthenticationService) Login(ctx context.Context, username, password string) (AdminAuthSession, error) {
	hasAdmin, err := service.store.HasAdmin(ctx)
	if err != nil {
		return AdminAuthSession{}, fmt.Errorf("check admin setup state: %w", err)
	}
	if !hasAdmin {
		return AdminAuthSession{}, ErrAdminSetupRequired
	}

	admin, err := service.store.VerifyAdminCredentials(ctx, username, password)
	if errors.Is(err, repository.ErrInvalidAdminCredentials) {
		return AdminAuthSession{}, ErrAdminAuthenticationFailed
	}
	if err != nil {
		return AdminAuthSession{}, fmt.Errorf("verify admin credentials: %w", err)
	}
	now := service.now().UTC()
	if _, err := service.store.DeleteExpiredAdminSessions(
		ctx,
		now.Unix(),
		now.Add(-adminSessionIdleTTL).Unix(),
		adminSessionCleanupSize,
	); err != nil {
		return AdminAuthSession{}, fmt.Errorf("clean expired admin sessions: %w", err)
	}
	sessionID, err := service.newSessionID()
	if err != nil {
		return AdminAuthSession{}, fmt.Errorf("generate admin session identifier: %w", err)
	}
	sessionToken, sessionBytes, err := service.randomToken()
	if err != nil {
		return AdminAuthSession{}, fmt.Errorf("generate admin session token: %w", err)
	}
	defer clear(sessionBytes[:])
	csrfToken, csrfBytes, err := service.randomToken()
	if err != nil {
		return AdminAuthSession{}, fmt.Errorf("generate admin csrf token: %w", err)
	}
	defer clear(csrfBytes[:])

	expiresAt := now.Add(adminSessionAbsoluteTTL)
	session := repository.AdminSession{
		ID:         sessionID,
		UserID:     admin.ID,
		TokenHash:  sha256.Sum256(sessionBytes[:]),
		CSRFToken:  csrfBytes,
		ExpiresAt:  expiresAt.Unix(),
		CreatedAt:  now.Unix(),
		LastSeenAt: now.Unix(),
	}
	defer clear(session.CSRFToken[:])
	if err := service.store.CreateAdminSession(ctx, session, now.Unix()); err != nil {
		return AdminAuthSession{}, fmt.Errorf("persist admin session: %w", err)
	}
	return AdminAuthSession{
		SessionID: sessionID, Admin: admin, SessionToken: sessionToken,
		CSRFToken: csrfToken, ExpiresAt: expiresAt,
	}, nil
}

// Authenticate 校验 Cookie Token 的固定形状、绝对期限和空闲期限，并按需推进最后访问时间。
func (service *AdminAuthenticationService) Authenticate(ctx context.Context, token string) (AdminAuthSession, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != adminTokenBytes {
		return AdminAuthSession{}, ErrAdminSessionExpired
	}
	now := service.now().UTC()
	session, err := service.store.GetAdminSessionByTokenHash(
		ctx,
		sha256.Sum256(raw),
		now.Unix(),
		now.Add(-adminSessionIdleTTL).Unix(),
	)
	if errors.Is(err, repository.ErrAdminSessionNotFound) || errors.Is(err, repository.ErrAdminSessionExpired) {
		return AdminAuthSession{}, ErrAdminSessionExpired
	}
	if err != nil {
		return AdminAuthSession{}, fmt.Errorf("load admin session: %w", err)
	}
	if session.LastSeenAt <= now.Add(-adminSessionTouchAfter).Unix() {
		if err := service.store.TouchAdminSession(ctx, session.ID, now.Unix()); err != nil {
			if errors.Is(err, repository.ErrAdminSessionNotFound) || errors.Is(err, repository.ErrAdminSessionExpired) {
				return AdminAuthSession{}, ErrAdminSessionExpired
			}
			return AdminAuthSession{}, fmt.Errorf("touch admin session: %w", err)
		}
	}
	return AdminAuthSession{
		SessionID: session.ID,
		Admin:     session.Admin,
		CSRFToken: base64.RawURLEncoding.EncodeToString(session.CSRFToken[:]),
		ExpiresAt: time.Unix(session.ExpiresAt, 0).UTC(),
	}, nil
}

// Logout 删除已经通过认证和 CSRF 校验的当前 Session。
func (service *AdminAuthenticationService) Logout(ctx context.Context, sessionID string) error {
	if err := service.store.DeleteAdminSession(ctx, sessionID); err != nil {
		if errors.Is(err, repository.ErrAdminSessionNotFound) {
			return ErrAdminSessionExpired
		}
		return fmt.Errorf("delete admin session: %w", err)
	}
	return nil
}

func (service *AdminAuthenticationService) randomToken() (string, [adminTokenBytes]byte, error) {
	var raw [adminTokenBytes]byte
	if _, err := io.ReadFull(service.random, raw[:]); err != nil {
		return "", raw, err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), raw, nil
}
