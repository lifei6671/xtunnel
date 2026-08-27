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

	"github.com/google/uuid"
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
		now := time.Now().UTC().Unix()
		admin := AdminUser{
			ID:           uuid.NewString(),
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

// hashPassword 使用每次独立随机 Salt 和冻结的 Argon2id 参数生成 PHC 字符串。
// 返回值只用于持久化，明文密码与派生中间值不得进入日志或错误文本。
func hashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read password salt: %w", err)
	}
	parallelism := argon2Parallelism()
	key := argon2.IDKey([]byte(password), salt, argon2Iterations, argon2MemoryKiB, parallelism, argon2KeyBytes)
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
		if !ok {
			return false, errors.New("invalid Argon2id hash parameters")
		}
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil || parsed == 0 {
			return false, errors.New("invalid Argon2id hash parameters")
		}
		parameters[name] = parsed
	}
	if len(parameters) != 3 || parameters["m"] == 0 || parameters["t"] == 0 || parameters["p"] == 0 || parameters["p"] > 255 {
		return false, errors.New("invalid Argon2id hash parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, errors.New("invalid Argon2id password salt")
	}
	wantKey, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(wantKey) == 0 {
		return false, errors.New("invalid Argon2id password key")
	}
	actualKey := argon2.IDKey([]byte(password), salt, uint32(parameters["t"]), uint32(parameters["m"]), uint8(parameters["p"]), uint32(len(wantKey)))
	return subtle.ConstantTimeCompare(actualKey, wantKey) == 1, nil
}

// argon2Parallelism 把本机可用 CPU 数钳制到冻结上限，避免小机器得到零并行度，
// 也避免大机器因核心数增长而让同一配置产生不可控的资源放大。
func argon2Parallelism() uint8 {
	return uint8(min(4, max(1, runtime.NumCPU())))
}
