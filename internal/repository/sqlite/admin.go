package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/repository"
	"golang.org/x/crypto/argon2"
	"gorm.io/gorm"
)

const (
	argon2MemoryKiB  uint32 = 64 * 1024
	argon2Iterations uint32 = 3
	argon2SaltBytes         = 16
	argon2KeyBytes          = 32
)

// ErrAdminAlreadyExists 表示数据库已经完成首次管理员初始化。
var ErrAdminAlreadyExists = errors.New("first admin is already initialized")

var _ repository.AdminAuthenticationStore = (*Store)(nil)

// AdminUser 映射 admin_users 表中的管理员身份记录。
// 密码哈希仅用于认证校验，调用方不得将其输出到日志、API 或错误消息。
type AdminUser struct {
	// ID 是不可变管理员标识。
	ID string `gorm:"column:id;primaryKey"`
	// Username 是全局唯一的管理员登录名。
	Username string `gorm:"column:username"`
	// PasswordHash 是 Argon2id PHC 格式的密码哈希，不保存密码明文。
	PasswordHash string `gorm:"column:password_hash"`
	// CreatedAt 是账户创建时刻的 UTC Unix 秒。
	CreatedAt int64 `gorm:"column:created_at"`
	// UpdatedAt 是最近资料更新时间的 UTC Unix 秒。
	UpdatedAt int64 `gorm:"column:updated_at"`
	// LastLoginAt 是最近一次成功登录的 UTC Unix 秒；nil 表示从未成功登录。
	LastLoginAt *int64 `gorm:"column:last_login_at"`
}

// adminSessionRecord 映射持久化 Session。Cookie 原文永不进入该结构；TokenHash
// 只保存 SHA-256，CSRFToken 保存 /auth/me 需要恢复的独立 32-byte 随机值。
type adminSessionRecord struct {
	ID         string `gorm:"column:id;primaryKey"`
	UserID     string `gorm:"column:user_id"`
	TokenHash  []byte `gorm:"column:token_hash"`
	CSRFToken  []byte `gorm:"column:csrf_token"`
	ExpiresAt  int64  `gorm:"column:expires_at"`
	CreatedAt  int64  `gorm:"column:created_at"`
	LastSeenAt int64  `gorm:"column:last_seen_at"`
}

func (adminSessionRecord) TableName() string {
	return AdminSessionTable
}

// TableName 返回 AdminUser 对应的固定表名。
func (AdminUser) TableName() string {
	return AdminUserTable
}

// HasAdmin 返回数据库中是否已存在管理员。
func (store *Store) HasAdmin(ctx context.Context) (bool, error) {
	var count int64
	if err := store.database.WithContext(ctx).Model(&AdminUser{}).Count(&count).Error; err != nil {
		return false, fmt.Errorf("count admin users: %w", err)
	}
	return count != 0, nil
}

// CreateFirstAdmin 以单条条件插入保证首次管理员只能创建一次。
func (store *Store) CreateFirstAdmin(ctx context.Context, username, password string) error {
	if strings.TrimSpace(username) == "" {
		return errors.New("admin username must not be empty")
	}
	if password == "" {
		return errors.New("admin password must not be empty")
	}

	// 首管初始化与其他写事务共用 Store 写租约。这样在线备份等待租约时，
	// 后到达的首管请求不能越过备份屏障修改数据库。
	lease, err := store.writeGate.acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire first-admin write lease: %w", err)
	}
	defer lease.Release()

	return store.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var count int64
		if err := transaction.Model(&AdminUser{}).Count(&count).Error; err != nil {
			return fmt.Errorf("count admin users for initialization: %w", err)
		}
		if count != 0 {
			return ErrAdminAlreadyExists
		}
		hash, err := hashPassword(password)
		if err != nil {
			return fmt.Errorf("hash admin password: %w", err)
		}
		adminID, err := identity.NewAdminID()
		if err != nil {
			return fmt.Errorf("generate first-admin identifier: %w", err)
		}
		now := time.Now().UTC().Unix()
		admin := AdminUser{
			ID:           adminID,
			Username:     username,
			PasswordHash: hash,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := transaction.Create(&admin).Error; err != nil {
			return fmt.Errorf("create first admin: %w", err)
		}
		return nil
	})
}

// VerifyAdminCredentials 在 SQLite 边界内读取 PHC 并校验密码，避免 Password Hash
// 泄露给 Application 或 Handler。用户名不存在与密码错误统一返回同一领域错误。
func (store *Store) VerifyAdminCredentials(ctx context.Context, username, password string) (repository.AdminUser, error) {
	if strings.TrimSpace(username) == "" || password == "" {
		return repository.AdminUser{}, repository.ErrInvalidAdminCredentials
	}
	var admin AdminUser
	err := store.database.WithContext(ctx).Where(AdminUserColumns.Username+" = ?", username).Take(&admin).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		consumeInvalidCredentialPassword(password)
		return repository.AdminUser{}, repository.ErrInvalidAdminCredentials
	}
	if err != nil {
		return repository.AdminUser{}, fmt.Errorf("read admin credentials: %w", err)
	}
	verified, err := verifyPassword(admin.PasswordHash, password)
	if err != nil {
		return repository.AdminUser{}, fmt.Errorf("verify stored admin password: %w", err)
	}
	if !verified {
		return repository.AdminUser{}, repository.ErrInvalidAdminCredentials
	}
	return repository.AdminUser{ID: admin.ID, Username: admin.Username}, nil
}

// CreateAdminSession 在一个写事务内持久化 Session 并记录成功登录时间。调用方只传
// Cookie Token 的 SHA-256；Cookie 原文不得进入 Repository。
func (store *Store) CreateAdminSession(ctx context.Context, session repository.AdminSession, loginAt int64) error {
	if !identity.ValidAdminSessionID(session.ID) || !identity.ValidAdminID(session.UserID) ||
		session.CreatedAt <= 0 || loginAt < session.CreatedAt || session.LastSeenAt != session.CreatedAt ||
		session.ExpiresAt <= session.CreatedAt || loginAt >= session.ExpiresAt {
		return errors.New("admin session is invalid")
	}
	record := adminSessionRecord{
		ID:         session.ID,
		UserID:     session.UserID,
		TokenHash:  append([]byte(nil), session.TokenHash[:]...),
		CSRFToken:  append([]byte(nil), session.CSRFToken[:]...),
		ExpiresAt:  session.ExpiresAt,
		CreatedAt:  session.CreatedAt,
		LastSeenAt: session.LastSeenAt,
	}
	defer clear(record.TokenHash)
	defer clear(record.CSRFToken)

	lease, err := store.writeGate.acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire admin-session write lease: %w", err)
	}
	defer lease.Release()

	return store.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		result := transaction.Model(&AdminUser{}).
			Where(AdminUserColumns.ID+" = ?", session.UserID).
			Update(AdminUserColumns.LastLoginAt, loginAt)
		if result.Error != nil {
			return fmt.Errorf("record successful admin login: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return repository.ErrInvalidAdminCredentials
		}
		if err := transaction.Create(&record).Error; err != nil {
			return fmt.Errorf("create admin session: %w", err)
		}
		return nil
	})
}

// GetAdminSessionByTokenHash 读取持久化 Session 与公开管理员身份。绝对或空闲边界
// 任一到期都返回 ErrAdminSessionExpired；本方法不隐式触碰时间或删除记录。
func (store *Store) GetAdminSessionByTokenHash(
	ctx context.Context,
	tokenHash [32]byte,
	now int64,
	idleCutoff int64,
) (repository.AdminSession, error) {
	if now <= 0 || idleCutoff < 0 || idleCutoff > now {
		return repository.AdminSession{}, repository.ErrAdminSessionNotFound
	}
	type sessionRow struct {
		ID            string `gorm:"column:id"`
		UserID        string `gorm:"column:user_id"`
		TokenHash     []byte `gorm:"column:token_hash"`
		CSRFToken     []byte `gorm:"column:csrf_token"`
		ExpiresAt     int64  `gorm:"column:expires_at"`
		CreatedAt     int64  `gorm:"column:created_at"`
		LastSeenAt    int64  `gorm:"column:last_seen_at"`
		AdminID       string `gorm:"column:admin_id"`
		AdminUsername string `gorm:"column:admin_username"`
	}
	var row sessionRow
	err := store.database.WithContext(ctx).
		Table(AdminSessionTable+" AS admin_session").
		Select("admin_session.*, admin_user.id AS admin_id, admin_user.username AS admin_username").
		Joins("JOIN "+AdminUserTable+" AS admin_user ON admin_user.id = admin_session.user_id").
		Where("admin_session."+AdminSessionColumns.TokenHash+" = ?", tokenHash[:]).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return repository.AdminSession{}, repository.ErrAdminSessionNotFound
	}
	if err != nil {
		return repository.AdminSession{}, fmt.Errorf("read admin session: %w", err)
	}
	defer clear(row.TokenHash)
	defer clear(row.CSRFToken)
	if now >= row.ExpiresAt || row.LastSeenAt <= idleCutoff {
		return repository.AdminSession{}, repository.ErrAdminSessionExpired
	}
	if len(row.TokenHash) != 32 || len(row.CSRFToken) != 32 || row.AdminID != row.UserID {
		return repository.AdminSession{}, errors.New("stored admin session is invalid")
	}
	var result repository.AdminSession
	result.ID = row.ID
	result.UserID = row.UserID
	copy(result.TokenHash[:], row.TokenHash)
	copy(result.CSRFToken[:], row.CSRFToken)
	result.ExpiresAt = row.ExpiresAt
	result.CreatedAt = row.CreatedAt
	result.LastSeenAt = row.LastSeenAt
	result.Admin = repository.AdminUser{ID: row.AdminID, Username: row.AdminUsername}
	return result, nil
}

// TouchAdminSession 单调推进最近活动时间，禁止旧请求把 last_seen_at 回退。
func (store *Store) TouchAdminSession(ctx context.Context, id string, seenAt int64) error {
	if !identity.ValidAdminSessionID(id) || seenAt <= 0 {
		return repository.ErrAdminSessionNotFound
	}
	lease, err := store.writeGate.acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire admin-session touch lease: %w", err)
	}
	defer lease.Release()

	result := store.database.WithContext(ctx).Model(&adminSessionRecord{}).
		Where(AdminSessionColumns.ID+" = ?", id).
		Where(AdminSessionColumns.ExpiresAt+" > ?", seenAt).
		Update(AdminSessionColumns.LastSeenAt,
			gorm.Expr("CASE WHEN "+AdminSessionColumns.LastSeenAt+" < ? THEN ? ELSE "+AdminSessionColumns.LastSeenAt+" END", seenAt, seenAt))
	if result.Error != nil {
		return fmt.Errorf("touch admin session: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var count int64
	if err := store.database.WithContext(ctx).Model(&adminSessionRecord{}).
		Where(AdminSessionColumns.ID+" = ?", id).Count(&count).Error; err != nil {
		return fmt.Errorf("inspect untouched admin session: %w", err)
	}
	if count == 0 {
		return repository.ErrAdminSessionNotFound
	}
	return repository.ErrAdminSessionExpired
}

// DeleteAdminSession 删除当前登录 Session；重复删除返回 NotFound，避免调用方把失效
// Cookie 当成仍已认证的登出成功。
func (store *Store) DeleteAdminSession(ctx context.Context, id string) error {
	if !identity.ValidAdminSessionID(id) {
		return repository.ErrAdminSessionNotFound
	}
	lease, err := store.writeGate.acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire admin-session delete lease: %w", err)
	}
	defer lease.Release()
	result := store.database.WithContext(ctx).Where(AdminSessionColumns.ID+" = ?", id).Delete(&adminSessionRecord{})
	if result.Error != nil {
		return fmt.Errorf("delete admin session: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return repository.ErrAdminSessionNotFound
	}
	return nil
}

// DeleteExpiredAdminSessions 使用两个索引化查询在单个写事务中有界清理绝对与空闲
// 过期记录；调度生命周期由上层 Server Owner 管理。
func (store *Store) DeleteExpiredAdminSessions(ctx context.Context, now, idleCutoff int64, limit int) (int64, error) {
	if now <= 0 || idleCutoff < 0 || idleCutoff > now || limit <= 0 {
		return 0, errors.New("admin session cleanup boundary is invalid")
	}
	lease, err := store.writeGate.acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("acquire admin-session cleanup lease: %w", err)
	}
	defer lease.Release()

	var deleted int64
	err = store.database.WithContext(ctx).Transaction(func(transaction *gorm.DB) error {
		var ids []string
		if err := transaction.Model(&adminSessionRecord{}).Select(AdminSessionColumns.ID).
			Where(AdminSessionColumns.ExpiresAt+" <= ?", now).
			Order(AdminSessionColumns.ExpiresAt+" ASC").Limit(limit).Pluck(AdminSessionColumns.ID, &ids).Error; err != nil {
			return fmt.Errorf("select absolutely expired admin sessions: %w", err)
		}
		remaining := limit - len(ids)
		if remaining > 0 {
			var idleIDs []string
			if err := transaction.Model(&adminSessionRecord{}).Select(AdminSessionColumns.ID).
				Where(AdminSessionColumns.ExpiresAt+" > ?", now).
				Where(AdminSessionColumns.LastSeenAt+" <= ?", idleCutoff).
				Order(AdminSessionColumns.LastSeenAt+" ASC").Limit(remaining).
				Pluck(AdminSessionColumns.ID, &idleIDs).Error; err != nil {
				return fmt.Errorf("select idle-expired admin sessions: %w", err)
			}
			ids = append(ids, idleIDs...)
		}
		if len(ids) == 0 {
			return nil
		}
		result := transaction.Where(AdminSessionColumns.ID+" IN ?", ids).Delete(&adminSessionRecord{})
		if result.Error != nil {
			return fmt.Errorf("delete expired admin sessions: %w", result.Error)
		}
		deleted = result.RowsAffected
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// hashPassword 使用每次独立随机 Salt 和冻结的 Argon2id 参数生成 PHC 字符串。
// 返回值只用于持久化，明文密码与派生中间值不得进入日志或错误文本。
func hashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read password salt: %w", err)
	}
	defer clear(salt)
	parallelism := argon2Parallelism()
	key := argon2.IDKey([]byte(password), salt, argon2Iterations, argon2MemoryKiB, parallelism, argon2KeyBytes)
	defer clear(key)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argon2MemoryKiB,
		argon2Iterations,
		parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// verifyPassword 严格解析本项目生成的 PHC 形状，并用常量时间比较验证派生值。
// 参数或编码漂移视为存储损坏，不回落到其他算法或弱参数。
func verifyPassword(encodedHash, password string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false, errors.New("invalid Argon2id hash format")
	}

	parameters := make(map[string]uint64, 3)
	for _, parameter := range strings.Split(parts[3], ",") {
		name, value, ok := strings.Cut(parameter, "=")
		if !ok || (name != "m" && name != "t" && name != "p") {
			return false, errors.New("invalid Argon2id hash parameters")
		}
		if _, duplicate := parameters[name]; duplicate {
			return false, errors.New("invalid Argon2id hash parameters")
		}
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil || parsed == 0 {
			return false, errors.New("invalid Argon2id hash parameters")
		}
		parameters[name] = parsed
	}
	if len(parameters) != 3 || parameters["m"] != uint64(argon2MemoryKiB) ||
		parameters["t"] != uint64(argon2Iterations) || parameters["p"] == 0 || parameters["p"] > 4 {
		return false, errors.New("invalid Argon2id hash parameters")
	}
	if len(parts[4]) != base64.RawStdEncoding.EncodedLen(argon2SaltBytes) ||
		len(parts[5]) != base64.RawStdEncoding.EncodedLen(argon2KeyBytes) {
		return false, errors.New("invalid Argon2id hash encoding length")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argon2SaltBytes {
		return false, errors.New("invalid Argon2id password salt")
	}
	defer clear(salt)
	wantKey, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(wantKey) != argon2KeyBytes {
		return false, errors.New("invalid Argon2id password key")
	}
	defer clear(wantKey)
	actualKey := argon2.IDKey([]byte(password), salt, uint32(parameters["t"]), uint32(parameters["m"]), uint8(parameters["p"]), uint32(len(wantKey)))
	defer clear(actualKey)
	return subtle.ConstantTimeCompare(actualKey, wantKey) == 1, nil
}

// consumeInvalidCredentialPassword 为不存在的用户名执行同量级 Argon2 工作，减少
// 仅凭响应时间判断账户是否存在的差异；结果不持久化且立即清零。
func consumeInvalidCredentialPassword(password string) {
	salt := make([]byte, argon2SaltBytes)
	defer clear(salt)
	key := argon2.IDKey([]byte(password), salt, argon2Iterations, argon2MemoryKiB, argon2Parallelism(), argon2KeyBytes)
	clear(key)
}

// argon2Parallelism 把本机可用 CPU 数钳制到冻结上限，避免小机器得到零并行度，
// 也避免大机器因核心数增长而让同一配置产生不可控的资源放大。
func argon2Parallelism() uint8 {
	return uint8(min(4, max(1, runtime.NumCPU())))
}
