package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	secretHashBytes        = 32
	minimumCiphertextBytes = 29 // AES-GCM nonce(12) + tag(16) + 至少 1 字节明文。
)

var (
	// ErrInvalidTunnel 表示 Tunnel 的持久化字段不符合领域约束。
	ErrInvalidTunnel = errors.New("tunnel is invalid")
	// ErrInvalidTunnelToken 表示 Token 元数据或密文不符合领域约束。
	// 错误中绝不能携带 Secret Hash、Token 密文或完整 Connection Token。
	ErrInvalidTunnelToken = errors.New("tunnel token is invalid")
	// ErrNotFound 表示 Repository 未找到指定的持久化对象。
	ErrNotFound = errors.New("repository record not found")
	// ErrVersionConflict 表示聚合版本已变化，调用方必须拒绝覆盖并重新读取。
	ErrVersionConflict = errors.New("repository aggregate version conflicts")
	// ErrTunnelTokenStateConflict 表示 Token 已不处于调用方声明的旧状态。
	ErrTunnelTokenStateConflict = errors.New("tunnel token state conflicts")
	// ErrPostCommitCleanup 表示权威事务已经 COMMIT，但连接级清理失败。
	// 调用方不得把它当作回滚；存在提交后副作用时必须继续执行并合并返回该错误。
	ErrPostCommitCleanup = errors.New("repository transaction committed but cleanup failed")
)

// Tunnel 是 Management 平面创建的隧道聚合，也是 Connection Token 的所有者。
//
// Connector、Instance、Session 与实时在线状态全部属于运行态，不能伪聚合到本结构。
type Tunnel struct {
	// ID 固定为 tun_ 加 26 位大写 Crockford ULID。
	ID string
	// Name 是 Management 平面展示的隧道名称。
	Name string
	// Version 是 Tunnel Aggregate 的乐观锁版本，和 TokenVersion 不是同一概念。
	Version int64
	// DesiredRevision 是 Server 期望 Connector 应用的配置版本。
	DesiredRevision int64
	// RevokedAt 是 Tunnel 被强制撤销的 UTC Unix 秒；nil 表示未撤销。
	RevokedAt *int64
	// FirstAuthenticatedAt 是任意 Connector 首次成功完成 Control 认证的 UTC Unix 秒。
	// nil 表示从未成功认证；一旦设置就不得清空或改写。
	FirstAuthenticatedAt *int64
	// CreatedAt、UpdatedAt 使用 UTC Unix 秒。
	CreatedAt int64
	UpdatedAt int64
}

// TunnelTokenStatus 是 Tunnel Credential 的生命周期状态。
type TunnelTokenStatus string

const (
	// TunnelTokenStatusActive 允许任意 Connector 使用该 Token 建立新 Session。
	TunnelTokenStatusActive TunnelTokenStatus = "ACTIVE"
	// TunnelTokenStatusRevokedForNewSession 禁止新 Session，但不主动中断既有 Session。
	TunnelTokenStatusRevokedForNewSession TunnelTokenStatus = "REVOKED_FOR_NEW_SESSION"
	// TunnelTokenStatusRevoked 表示 Token 已被完全撤销。
	TunnelTokenStatusRevoked TunnelTokenStatus = "REVOKED"
)

// TunnelToken 保存认证所需摘要和可重复取回完整 Token 所需密文。
//
// SecretHash 用于热路径认证；TokenCiphertext 只能交给持有主密钥的 Application Service
// 解密。二者均不得进入错误、日志或 Management 普通查询结果。
type TunnelToken struct {
	// ID 固定为 tok_ 加 26 位大写 Crockford ULID。
	ID string
	// TunnelID 是所属 Tunnel 的不可变外键。
	TunnelID string
	// SecretHash 是认证 Secret 的 SHA-256 摘要，固定为 32 字节。
	SecretHash [secretHashBytes]byte
	// TokenCiphertext 是带随机 nonce 的完整 Connection Token 密文。
	TokenCiphertext []byte
	// Version 是同一 Tunnel 下单调递增的 Credential 代次，从 1 开始。
	Version int64
	// Status 仅描述 Credential 生命周期。
	Status TunnelTokenStatus
	// CreatedAt 是签发时刻的 UTC Unix 秒。
	CreatedAt int64
	// RevokedAt 是撤销时刻的 UTC Unix 秒；ACTIVE 必须为 nil。
	RevokedAt *int64
}

// Validate 检查 Tunnel 的全部持久化不变量。
func (tunnel Tunnel) Validate() error {
	if !validate.ValidID(tunnel.ID, "tun_") || strings.TrimSpace(tunnel.Name) == "" ||
		tunnel.Version < 1 || tunnel.DesiredRevision < 0 || tunnel.CreatedAt <= 0 || tunnel.UpdatedAt <= 0 ||
		(tunnel.RevokedAt != nil && *tunnel.RevokedAt <= 0) ||
		(tunnel.FirstAuthenticatedAt != nil && *tunnel.FirstAuthenticatedAt <= 0) {
		return ErrInvalidTunnel
	}
	return nil
}

// Validate 检查 Token 元数据的全部持久化不变量。
func (token TunnelToken) Validate() error {
	if !validate.ValidID(token.ID, "tok_") || !validate.ValidID(token.TunnelID, "tun_") ||
		len(token.TokenCiphertext) < minimumCiphertextBytes || token.Version < 1 || token.CreatedAt <= 0 {
		return ErrInvalidTunnelToken
	}
	switch token.Status {
	case TunnelTokenStatusActive:
		if token.RevokedAt != nil {
			return ErrInvalidTunnelToken
		}
	case TunnelTokenStatusRevokedForNewSession, TunnelTokenStatusRevoked:
		if token.RevokedAt == nil || *token.RevokedAt <= 0 {
			return ErrInvalidTunnelToken
		}
	default:
		return ErrInvalidTunnelToken
	}
	return nil
}

// TunnelRepository 定义 Tunnel 的最小持久化边界。
// Repository 实现不得自行开启或提交事务。
type TunnelRepository interface {
	Create(context.Context, Tunnel) error
	Get(context.Context, string) (Tunnel, error)
	Count(context.Context) (int64, error)
	List(context.Context) ([]Tunnel, error)
	UpdateName(context.Context, string, string, int64, int64) (Tunnel, error)
	Delete(context.Context, string, int64) error
	AdvanceVersion(context.Context, string, int64, int64) (Tunnel, error)
	AdvanceDesiredRevision(context.Context, string, int64, int64, int64) (Tunnel, error)
	MarkFirstAuthenticated(context.Context, string, int64) error
	Revoke(context.Context, string, int64, int64) (Tunnel, error)
}

// TunnelTokenRepository 定义 Tunnel Credential 的敏感持久化边界。
type TunnelTokenRepository interface {
	Create(context.Context, TunnelToken) error
	GetByIdentity(context.Context, string, string, int64) (TunnelToken, error)
	GetActiveByTunnel(context.Context, string) (TunnelToken, error)
	GetByTunnelVersion(context.Context, string, int64) (TunnelToken, error)
	TransitionStatus(context.Context, string, string, int64, TunnelTokenStatus, TunnelTokenStatus, int64) error
	RevokeAll(context.Context, string, int64) error
}

// RepositoryView 是一次 Repository 访问中共享的只读视图。
// 认证热路径只需要读取 Tunnel/Token，不能为了复用接口获取 SQLite 写锁。
type RepositoryView interface {
	Tunnels() TunnelRepository
	TunnelTokens() TunnelTokenRepository
	Services() ServiceRepository
	Routes() RouteRepository
}

// TxStore 是一次写事务内共享的 Repository 视图。
type TxStore interface {
	RepositoryView
	SecurityAuditEvents() SecurityAuditEventRepository
}

// Store 定义跨表不变量使用的事务边界。
// SQLite 实现必须以 BEGIN IMMEDIATE 获取写入权，避免 ACTIVE Token 并发写丢失。
type Store interface {
	Read(context.Context, func(RepositoryView) error) error
	WithTx(context.Context, func(TxStore) error) error
	WithDurableTx(context.Context, func(TxStore) error) error
}
