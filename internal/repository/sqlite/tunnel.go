package sqlite

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	// TunnelTable 是 tunnels 的固定表名。
	TunnelTable = "tunnels"
	// TunnelTokenTable 是 tunnel_tokens 的固定表名。
	TunnelTokenTable = "tunnel_tokens"

	// rollbackTimeout 独立约束事务清理，避免调用请求已取消时把未回滚连接放回池中。
	rollbackTimeout = 5 * time.Second
)

var (
	// 编译期确认 SQLite Store 暴露的是领域 Repository 的事务边界。
	_ repository.Store          = (*Store)(nil)
	_ repository.RepositoryView = (*transactionStore)(nil)
	_ repository.TxStore        = (*transactionStore)(nil)
)

// TunnelColumns 集中定义 tunnels 的列名，避免查询条件分散硬编码。
var TunnelColumns = struct {
	ID                   string
	Name                 string
	Version              string
	DesiredRevision      string
	RevokedAt            string
	FirstAuthenticatedAt string
	CreatedAt            string
	UpdatedAt            string
}{
	ID:                   "id",
	Name:                 "name",
	Version:              "version",
	DesiredRevision:      "desired_revision",
	RevokedAt:            "revoked_at",
	FirstAuthenticatedAt: "first_authenticated_at",
	CreatedAt:            "created_at",
	UpdatedAt:            "updated_at",
}

// TunnelTokenColumns 集中定义 tunnel_tokens 的列名，避免泄漏或误用敏感列。
var TunnelTokenColumns = struct {
	ID              string
	TunnelID        string
	SecretHash      string
	TokenCiphertext string
	Version         string
	Status          string
	CreatedAt       string
	RevokedAt       string
}{
	ID:              "id",
	TunnelID:        "tunnel_id",
	SecretHash:      "secret_hash",
	TokenCiphertext: "token_ciphertext",
	Version:         "version",
	Status:          "status",
	CreatedAt:       "created_at",
	RevokedAt:       "revoked_at",
}

// tunnelRecord 是 tunnels 表的内部 GORM 映射。
type tunnelRecord struct {
	ID                   string `gorm:"column:id;primaryKey"`
	Name                 string `gorm:"column:name"`
	Version              int64  `gorm:"column:version"`
	DesiredRevision      int64  `gorm:"column:desired_revision"`
	RevokedAt            *int64 `gorm:"column:revoked_at"`
	FirstAuthenticatedAt *int64 `gorm:"column:first_authenticated_at"`
	CreatedAt            int64  `gorm:"column:created_at"`
	UpdatedAt            int64  `gorm:"column:updated_at"`
}

// TableName 把 Tunnel 模型固定到 tunnels 表。
func (tunnelRecord) TableName() string { return TunnelTable }

// tunnelTokenRecord 是 tunnel_tokens 表的内部 GORM 映射。
// 敏感字节只在这一内部映射中进入数据库，错误路径绝不格式化其内容。
type tunnelTokenRecord struct {
	ID              string `gorm:"column:id;primaryKey"`
	TunnelID        string `gorm:"column:tunnel_id"`
	SecretHash      []byte `gorm:"column:secret_hash"`
	TokenCiphertext []byte `gorm:"column:token_ciphertext"`
	Version         int64  `gorm:"column:version"`
	Status          string `gorm:"column:status"`
	CreatedAt       int64  `gorm:"column:created_at"`
	RevokedAt       *int64 `gorm:"column:revoked_at"`
}

// TableName 把 Token 模型固定到 tunnel_tokens 表。
func (tunnelTokenRecord) TableName() string { return TunnelTokenTable }

// transactionStore 将同一个 BEGIN IMMEDIATE 事务连接交给各 Repository。
type transactionStore struct {
	database *gorm.DB
	readOnly bool
}

var errRepositoryWriteOutsideTransaction = errors.New("repository write requires a write transaction")

// Read 在普通 GORM 连接池视图上运行只读回调，不开启 SQLite 写事务。
// Connector Auth 和 Token Reveal 属于高频只读路径；若使用 BEGIN IMMEDIATE，
// 每次认证都会争抢全库写锁，导致多 Connector 重连被无谓串行化。
func (store *Store) Read(ctx context.Context, fn func(repository.RepositoryView) error) error {
	if fn == nil {
		return errors.New("repository read callback must not be nil")
	}
	return fn(&transactionStore{database: store.database.WithContext(ctx), readOnly: true})
}

// ReadConsistent 在同一个 SQLite 连接的普通只读事务中运行 fn。
//
// 普通 BEGIN 不抢占写锁；WAL 模式下首次读取固定快照，后续跨表查询只能看到
// 同一次提交前或提交后的完整状态，不能拼出不同提交的 Tunnel Revision 与 Services。
func (store *Store) ReadConsistent(ctx context.Context, fn func(repository.RepositoryView) error) error {
	if fn == nil {
		return errors.New("repository consistent read callback must not be nil")
	}

	return store.database.WithContext(ctx).Connection(func(connection *gorm.DB) (resultErr error) {
		committed := false
		if err := connection.Exec("BEGIN").Error; err != nil {
			return fmt.Errorf("begin consistent repository read: %w", err)
		}
		defer func() {
			if committed {
				return
			}
			// 回调可能因请求取消而返回；资源清理不能继承已取消的请求 Context，
			// 否则打开的只读事务会随连接回到池中并长期保留 WAL 快照。
			rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
			defer cancel()
			if err := connection.WithContext(rollbackContext).Exec("ROLLBACK").Error; err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("rollback consistent repository read: %w", err))
			}
		}()

		transaction := connection.Session(&gorm.Session{SkipDefaultTransaction: true})
		if err := fn(&transactionStore{database: transaction, readOnly: true}); err != nil {
			return err
		}
		if err := connection.Exec("COMMIT").Error; err != nil {
			return fmt.Errorf("commit consistent repository read: %w", err)
		}
		committed = true
		return nil
	})
}

// WithTx 使用 SQLite BEGIN IMMEDIATE 取得写入权后运行 fn。
//
// 这里不能改用 GORM 默认的 Begin：SQLite 的普通 BEGIN 是延迟事务，两个并发签发
// 可能同时读到“该 Tunnel 尚无 ACTIVE Token”，随后才在写入阶段竞争。业务要求同一
// Tunnel 的所有 Connector 共用唯一 ACTIVE Token，所以必须在检查与创建之前取得写锁。
// GORM 没有公开的 SQLite 事务模式参数，因此在独占连接上显式执行 BEGIN IMMEDIATE。
// 已经由外层开启事务后，还必须关闭 GORM Create 的默认事务，避免嵌套 BEGIN 破坏边界。
func (store *Store) WithTx(ctx context.Context, fn func(repository.TxStore) error) error {
	return store.withTx(ctx, false, fn)
}

// WithDurableTx 在当前连接上临时使用 synchronous=FULL，使成功 COMMIT 在返回前同步 WAL。
// 该路径只用于必须先于外部 Journal 清理完成耐久提交的低频安全操作。
func (store *Store) WithDurableTx(ctx context.Context, fn func(repository.TxStore) error) error {
	return store.withTx(ctx, true, fn)
}

// withTx 是全部 SQLite 写入的共同边界：先取得进程内 FIFO Lease，再在独占连接上
// BEGIN IMMEDIATE。durable 只临时提升本次事务的同步级别，提交或失败后都恢复默认值。
func (store *Store) withTx(ctx context.Context, durable bool, fn func(repository.TxStore) error) error {
	if fn == nil {
		return errors.New("repository transaction callback must not be nil")
	}
	lease, err := store.writeGate.acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire repository write lease: %w", err)
	}
	defer lease.Release()

	return store.database.WithContext(ctx).Connection(func(connection *gorm.DB) (resultErr error) {
		committed := false
		if durable {
			if err := connection.Exec("PRAGMA synchronous = FULL").Error; err != nil {
				return fmt.Errorf("enable durable SQLite transaction: %w", err)
			}
			defer func() {
				restoreContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
				defer cancel()
				if err := connection.WithContext(restoreContext).Exec("PRAGMA synchronous = NORMAL").Error; err != nil {
					restoreErr := fmt.Errorf("restore normal SQLite synchronous mode: %w", err)
					if committed {
						restoreErr = fmt.Errorf("%w: %w", repository.ErrPostCommitCleanup, restoreErr)
					}
					resultErr = errors.Join(resultErr, restoreErr)
				}
			}()
		}
		if err := connection.Exec("BEGIN IMMEDIATE").Error; err != nil {
			return fmt.Errorf("begin immediate repository transaction: %w", err)
		}

		defer func() {
			if committed {
				return
			}
			// fn 可能正是因为请求 ctx 已取消而返回。Rollback 是数据库资源清理，
			// 必须使用不继承取消信号、但仍有短超时的 Context；否则 raw
			// BEGIN IMMEDIATE 会随连接回到池中并继续占用写锁。
			rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
			defer cancel()
			if err := connection.WithContext(rollbackContext).Exec("ROLLBACK").Error; err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("rollback repository transaction: %w", err))
			}
		}()

		transaction := connection.Session(&gorm.Session{SkipDefaultTransaction: true})
		if err := fn(&transactionStore{database: transaction}); err != nil {
			return err
		}
		if err := connection.Exec("COMMIT").Error; err != nil {
			return fmt.Errorf("commit repository transaction: %w", err)
		}
		committed = true
		return nil
	})
}

// Tunnels 返回当前事务作用域的 Tunnel Repository。
func (store *transactionStore) Tunnels() repository.TunnelRepository {
	return tunnelRepository{database: store.database, readOnly: store.readOnly}
}

// TunnelTokens 返回当前事务作用域的 Tunnel Token Repository。
func (store *transactionStore) TunnelTokens() repository.TunnelTokenRepository {
	return tunnelTokenRepository{database: store.database, readOnly: store.readOnly}
}

// Services 返回当前只读视图或 BEGIN IMMEDIATE 事务作用域的 Service Repository。
func (store *transactionStore) Services() repository.ServiceRepository {
	return serviceRepository{database: store.database, readOnly: store.readOnly}
}

// tunnelRepository 绑定一个 RepositoryView；readOnly 防止读取回调绕过写事务。
type tunnelRepository struct {
	database *gorm.DB
	readOnly bool
}

// Create 在当前写事务中插入经过领域校验的 Tunnel。
func (store tunnelRepository) Create(ctx context.Context, tunnel repository.Tunnel) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if err := tunnel.Validate(); err != nil {
		return err
	}
	if err := store.database.WithContext(ctx).Create(tunnelRecordFromDomain(tunnel)).Error; err != nil {
		return fmt.Errorf("create tunnel: %w", err)
	}
	return nil
}

// Get 按稳定 Tunnel ID 读取聚合，并把缺失行归一为 repository.ErrNotFound。
func (store tunnelRepository) Get(ctx context.Context, id string) (repository.Tunnel, error) {
	if !validate.ValidID(id, "tun_") {
		return repository.Tunnel{}, repository.ErrInvalidTunnel
	}
	var record tunnelRecord
	if err := store.database.WithContext(ctx).Where(TunnelColumns.ID+" = ?", id).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.Tunnel{}, repository.ErrNotFound
		}
		return repository.Tunnel{}, fmt.Errorf("get tunnel: %w", err)
	}
	return record.toDomain(), nil
}

// List 按 Tunnel ID 升序返回当前视图中的全部持久化 Tunnel。
func (store tunnelRepository) List(ctx context.Context) ([]repository.Tunnel, error) {
	var records []tunnelRecord
	if err := store.database.WithContext(ctx).Model(&tunnelRecord{}).
		Order(TunnelColumns.ID + " ASC").
		Find(&records).Error; err != nil {
		return nil, fmt.Errorf("list tunnels: %w", err)
	}
	tunnels := make([]repository.Tunnel, 0, len(records))
	for _, record := range records {
		tunnel := record.toDomain()
		if err := tunnel.Validate(); err != nil {
			return nil, fmt.Errorf("list tunnels: stored tunnel %q is invalid: %w", record.ID, err)
		}
		tunnels = append(tunnels, tunnel)
	}
	return tunnels, nil
}

// AdvanceVersion 以 Tunnel aggregate version 为唯一 CAS 条件推进一次管理面变更。
func (store tunnelRepository) AdvanceVersion(ctx context.Context, id string, expectedVersion, updatedAt int64) (repository.Tunnel, error) {
	if store.readOnly {
		return repository.Tunnel{}, errRepositoryWriteOutsideTransaction
	}
	if !validate.ValidID(id, "tun_") || expectedVersion < 1 || expectedVersion == math.MaxInt64 || updatedAt <= 0 {
		return repository.Tunnel{}, repository.ErrInvalidTunnel
	}
	result := store.database.WithContext(ctx).Model(&tunnelRecord{}).
		Where(TunnelColumns.ID+" = ?", id).
		Where(TunnelColumns.Version+" = ?", expectedVersion).
		Updates(map[string]any{TunnelColumns.Version: expectedVersion + 1, TunnelColumns.UpdatedAt: updatedAt})
	if result.Error != nil {
		return repository.Tunnel{}, fmt.Errorf("advance tunnel version: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		if _, err := store.Get(ctx, id); err != nil {
			return repository.Tunnel{}, err
		}
		return repository.Tunnel{}, repository.ErrVersionConflict
	}
	return store.Get(ctx, id)
}

// AdvanceDesiredRevision 在不改变 Tunnel ETag Version 的前提下推进远端 Desired State。
// expectedVersion 用于拒绝基于旧 Tunnel Aggregate 的 Service 写入，expectedRevision 则
// 防止调用方重复发布或跳过 Revision。两者必须在同一个 BEGIN IMMEDIATE 事务内校验。
func (store tunnelRepository) AdvanceDesiredRevision(
	ctx context.Context,
	id string,
	expectedVersion, expectedRevision, updatedAt int64,
) (repository.Tunnel, error) {
	if store.readOnly {
		return repository.Tunnel{}, errRepositoryWriteOutsideTransaction
	}
	if !validate.ValidID(id, "tun_") || expectedVersion < 1 || expectedRevision < 0 ||
		expectedRevision == math.MaxInt64 || updatedAt <= 0 {
		return repository.Tunnel{}, repository.ErrInvalidTunnel
	}
	result := store.database.WithContext(ctx).Model(&tunnelRecord{}).
		Where(TunnelColumns.ID+" = ?", id).
		Where(TunnelColumns.Version+" = ?", expectedVersion).
		Where(TunnelColumns.DesiredRevision+" = ?", expectedRevision).
		Where(TunnelColumns.RevokedAt + " IS NULL").
		Updates(map[string]any{
			TunnelColumns.DesiredRevision: expectedRevision + 1,
			TunnelColumns.UpdatedAt:       updatedAt,
		})
	if result.Error != nil {
		return repository.Tunnel{}, fmt.Errorf("advance tunnel desired revision: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		if _, err := store.Get(ctx, id); err != nil {
			return repository.Tunnel{}, err
		}
		return repository.Tunnel{}, repository.ErrVersionConflict
	}
	return store.Get(ctx, id)
}

// MarkFirstAuthenticated 只写入第一次成功认证的时刻。重复调用保持原值，且不推进
// Management Aggregate Version/UpdatedAt；该字段是状态计算所需的单调历史事实，
// 不是一次用户发起的 Tunnel 配置变更。
func (store tunnelRepository) MarkFirstAuthenticated(ctx context.Context, id string, authenticatedAt int64) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if !validate.ValidID(id, "tun_") || authenticatedAt <= 0 {
		return repository.ErrInvalidTunnel
	}
	result := store.database.WithContext(ctx).Model(&tunnelRecord{}).
		Where(TunnelColumns.ID+" = ?", id).
		Where(TunnelColumns.FirstAuthenticatedAt+" IS NULL").
		UpdateColumn(TunnelColumns.FirstAuthenticatedAt, authenticatedAt)
	if result.Error != nil {
		return fmt.Errorf("mark tunnel first authenticated: %w", result.Error)
	}
	if result.RowsAffected > 0 {
		return nil
	}
	tunnel, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	if tunnel.FirstAuthenticatedAt == nil {
		return errors.New("mark tunnel first authenticated affected no row")
	}
	return nil
}

// Revoke 原子设置 Tunnel revoke tombstone 并推进 aggregate version。
func (store tunnelRepository) Revoke(ctx context.Context, id string, expectedVersion, revokedAt int64) (repository.Tunnel, error) {
	if store.readOnly {
		return repository.Tunnel{}, errRepositoryWriteOutsideTransaction
	}
	if !validate.ValidID(id, "tun_") || expectedVersion < 1 || expectedVersion == math.MaxInt64 || revokedAt <= 0 {
		return repository.Tunnel{}, repository.ErrInvalidTunnel
	}
	result := store.database.WithContext(ctx).Model(&tunnelRecord{}).
		Where(TunnelColumns.ID+" = ?", id).
		Where(TunnelColumns.Version+" = ?", expectedVersion).
		Where(TunnelColumns.RevokedAt + " IS NULL").
		Updates(map[string]any{
			TunnelColumns.Version: expectedVersion + 1, TunnelColumns.RevokedAt: revokedAt, TunnelColumns.UpdatedAt: revokedAt,
		})
	if result.Error != nil {
		return repository.Tunnel{}, fmt.Errorf("revoke tunnel: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		if _, err := store.Get(ctx, id); err != nil {
			return repository.Tunnel{}, err
		}
		return repository.Tunnel{}, repository.ErrVersionConflict
	}
	return store.Get(ctx, id)
}

// tunnelTokenRepository 绑定同一事务内的 Credential 读写视图。
type tunnelTokenRepository struct {
	database *gorm.DB
	readOnly bool
}

// Create 插入完整 Token 元数据；唯一性和 ACTIVE 单例约束由事务检查与数据库共同保证。
func (store tunnelTokenRepository) Create(ctx context.Context, token repository.TunnelToken) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if err := token.Validate(); err != nil {
		return err
	}
	if err := store.database.WithContext(ctx).Create(tunnelTokenRecordFromDomain(token)).Error; err != nil {
		return fmt.Errorf("create tunnel token: %w", err)
	}
	return nil
}

// GetByIdentity 按 Tunnel、Token ID 与 Version 的完整身份精确读取 Credential。
func (store tunnelTokenRepository) GetByIdentity(ctx context.Context, tunnelID, tokenID string, version int64) (repository.TunnelToken, error) {
	if !validate.ValidID(tunnelID, "tun_") || !validate.ValidID(tokenID, "tok_") || version < 1 {
		return repository.TunnelToken{}, repository.ErrInvalidTunnelToken
	}
	var record tunnelTokenRecord
	if err := store.database.WithContext(ctx).
		Where(TunnelTokenColumns.TunnelID+" = ?", tunnelID).
		Where(TunnelTokenColumns.ID+" = ?", tokenID).
		Where(TunnelTokenColumns.Version+" = ?", version).
		Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.TunnelToken{}, repository.ErrNotFound
		}
		return repository.TunnelToken{}, fmt.Errorf("get tunnel token by identity: %w", err)
	}
	return record.toDomain(), nil
}

// GetActiveByTunnel 返回指定 Tunnel 当前供全部 Connector 共用的唯一 ACTIVE Token。
func (store tunnelTokenRepository) GetActiveByTunnel(ctx context.Context, tunnelID string) (repository.TunnelToken, error) {
	if !validate.ValidID(tunnelID, "tun_") {
		return repository.TunnelToken{}, repository.ErrInvalidTunnelToken
	}
	var record tunnelTokenRecord
	if err := store.database.WithContext(ctx).
		Where(TunnelTokenColumns.TunnelID+" = ?", tunnelID).
		Where(TunnelTokenColumns.Status+" = ?", repository.TunnelTokenStatusActive).
		Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.TunnelToken{}, repository.ErrNotFound
		}
		return repository.TunnelToken{}, fmt.Errorf("get active tunnel token: %w", err)
	}
	return record.toDomain(), nil
}

// GetByTunnelVersion 读取指定 Tunnel 代次，供签发与轮换冲突检查使用。
func (store tunnelTokenRepository) GetByTunnelVersion(ctx context.Context, tunnelID string, version int64) (repository.TunnelToken, error) {
	if !validate.ValidID(tunnelID, "tun_") || version < 1 {
		return repository.TunnelToken{}, repository.ErrInvalidTunnelToken
	}
	var record tunnelTokenRecord
	if err := store.database.WithContext(ctx).
		Where(TunnelTokenColumns.TunnelID+" = ?", tunnelID).
		Where(TunnelTokenColumns.Version+" = ?", version).
		Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.TunnelToken{}, repository.ErrNotFound
		}
		return repository.TunnelToken{}, fmt.Errorf("get tunnel token by version: %w", err)
	}
	return record.toDomain(), nil
}

// TransitionStatus 只允许生命周期服务按精确 Token identity 和旧状态推进一次状态机。
func (store tunnelTokenRepository) TransitionStatus(
	ctx context.Context,
	tunnelID, tokenID string,
	version int64,
	from, to repository.TunnelTokenStatus,
	revokedAt int64,
) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if !validate.ValidID(tunnelID, "tun_") || !validate.ValidID(tokenID, "tok_") || version < 1 || revokedAt <= 0 ||
		from != repository.TunnelTokenStatusActive ||
		(to != repository.TunnelTokenStatusRevokedForNewSession && to != repository.TunnelTokenStatusRevoked) {
		return repository.ErrInvalidTunnelToken
	}
	result := store.database.WithContext(ctx).Model(&tunnelTokenRecord{}).
		Where(TunnelTokenColumns.TunnelID+" = ?", tunnelID).
		Where(TunnelTokenColumns.ID+" = ?", tokenID).
		Where(TunnelTokenColumns.Version+" = ?", version).
		Where(TunnelTokenColumns.Status+" = ?", from).
		Updates(map[string]any{TunnelTokenColumns.Status: string(to), TunnelTokenColumns.RevokedAt: revokedAt})
	if result.Error != nil {
		return fmt.Errorf("transition tunnel token status: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		if _, err := store.GetByIdentity(ctx, tunnelID, tokenID, version); err != nil {
			return err
		}
		return repository.ErrTunnelTokenStateConflict
	}
	return nil
}

// RevokeAll 使 Tunnel 的全部历史 Credential 永久拒绝新认证。
func (store tunnelTokenRepository) RevokeAll(ctx context.Context, tunnelID string, revokedAt int64) error {
	if store.readOnly {
		return errRepositoryWriteOutsideTransaction
	}
	if !validate.ValidID(tunnelID, "tun_") || revokedAt <= 0 {
		return repository.ErrInvalidTunnelToken
	}
	activeResult := store.database.WithContext(ctx).Model(&tunnelTokenRecord{}).
		Where(TunnelTokenColumns.TunnelID+" = ?", tunnelID).
		Where(TunnelTokenColumns.Status+" = ?", repository.TunnelTokenStatusActive).
		Updates(map[string]any{
			TunnelTokenColumns.Status: string(repository.TunnelTokenStatusRevoked), TunnelTokenColumns.RevokedAt: revokedAt,
		})
	if activeResult.Error != nil {
		return fmt.Errorf("revoke active tunnel tokens: %w", activeResult.Error)
	}
	// 已因 Rotate 禁止新认证的历史 Token 保留首次 revoked_at，只推进最终状态。
	rotatedResult := store.database.WithContext(ctx).Model(&tunnelTokenRecord{}).
		Where(TunnelTokenColumns.TunnelID+" = ?", tunnelID).
		Where(TunnelTokenColumns.Status+" = ?", repository.TunnelTokenStatusRevokedForNewSession).
		Update(TunnelTokenColumns.Status, string(repository.TunnelTokenStatusRevoked))
	if rotatedResult.Error != nil {
		return fmt.Errorf("revoke rotated tunnel tokens: %w", rotatedResult.Error)
	}
	return nil
}

// tunnelRecordFromDomain 把值型 Tunnel 聚合映射为持久化行。
func tunnelRecordFromDomain(tunnel repository.Tunnel) tunnelRecord {
	return tunnelRecord{
		ID:                   tunnel.ID,
		Name:                 tunnel.Name,
		Version:              tunnel.Version,
		DesiredRevision:      tunnel.DesiredRevision,
		RevokedAt:            tunnel.RevokedAt,
		FirstAuthenticatedAt: tunnel.FirstAuthenticatedAt,
		CreatedAt:            tunnel.CreatedAt,
		UpdatedAt:            tunnel.UpdatedAt,
	}
}

// toDomain 把 Tunnel 行还原为值型领域聚合。
func (record tunnelRecord) toDomain() repository.Tunnel {
	return repository.Tunnel{
		ID:                   record.ID,
		Name:                 record.Name,
		Version:              record.Version,
		DesiredRevision:      record.DesiredRevision,
		RevokedAt:            record.RevokedAt,
		FirstAuthenticatedAt: record.FirstAuthenticatedAt,
		CreatedAt:            record.CreatedAt,
		UpdatedAt:            record.UpdatedAt,
	}
}

// tunnelTokenRecordFromDomain 复制 Secret 摘要与密文，避免保留调用方切片别名。
func tunnelTokenRecordFromDomain(token repository.TunnelToken) tunnelTokenRecord {
	return tunnelTokenRecord{
		ID:              token.ID,
		TunnelID:        token.TunnelID,
		SecretHash:      append([]byte(nil), token.SecretHash[:]...),
		TokenCiphertext: append([]byte(nil), token.TokenCiphertext...),
		Version:         token.Version,
		Status:          string(token.Status),
		CreatedAt:       token.CreatedAt,
		RevokedAt:       token.RevokedAt,
	}
}

// toDomain 复制数据库字节到固定摘要与独立密文切片。
func (record tunnelTokenRecord) toDomain() repository.TunnelToken {
	var secretHash [32]byte
	copy(secretHash[:], record.SecretHash)
	return repository.TunnelToken{
		ID:              record.ID,
		TunnelID:        record.TunnelID,
		SecretHash:      secretHash,
		TokenCiphertext: append([]byte(nil), record.TokenCiphertext...),
		Version:         record.Version,
		Status:          repository.TunnelTokenStatus(record.Status),
		CreatedAt:       record.CreatedAt,
		RevokedAt:       record.RevokedAt,
	}
}
