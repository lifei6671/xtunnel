package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/token"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
)

const initialConnectionTokenVersion int64 = 1

var (
	// ErrConnectionTokenInput 表示请求缺少连接描述、Tunnel 身份或 Token 保护器。
	ErrConnectionTokenInput = errors.New("connection token input is invalid")
	// ErrConnectionTokenRandom 表示 CSPRNG 未能产生签发所需随机字节。
	ErrConnectionTokenRandom = errors.New("connection token random generation failed")
	// ErrConnectionTokenTunnelUnavailable 表示 Tunnel 不存在或已撤销。
	ErrConnectionTokenTunnelUnavailable = errors.New("connection token tunnel is unavailable")
	// ErrConnectionTokenTunnelRevoked 表示 Token 所属 Tunnel 已被管理员撤销。
	// Auth Handler 依赖该错误返回稳定的 TUNNEL_REVOKED，而不能把它降级成普通 Token 错误。
	ErrConnectionTokenTunnelRevoked = errors.New("connection token tunnel is revoked")
	// ErrConnectionTokenAlreadyActive 表示 Tunnel 已有供全部 Connector 共用的 ACTIVE Token。
	ErrConnectionTokenAlreadyActive = errors.New("connection token already active")
	// ErrConnectionTokenVersionExists 表示初始第 1 代 Token 已存在，不能覆盖。
	ErrConnectionTokenVersionExists = errors.New("connection token version already exists")
	// ErrConnectionTokenInvalid 表示文本 Token 无法通过冻结协议的严格解析。
	ErrConnectionTokenInvalid = errors.New("connection token is invalid")
	// ErrConnectionTokenIdentityMismatch 表示 Token 与持久化行身份不完全一致。
	ErrConnectionTokenIdentityMismatch = errors.New("connection token identity mismatch")
	// ErrConnectionTokenSecretMismatch 表示认证 Secret 的摘要不匹配。
	ErrConnectionTokenSecretMismatch = errors.New("connection token secret mismatch")
	// ErrConnectionTokenInactive 表示 Credential 不再允许建立新的 Session。
	ErrConnectionTokenInactive = errors.New("connection token is inactive")
	// ErrConnectionTokenUnavailable 表示当前 ACTIVE Token 缺失或密文无法安全恢复。
	ErrConnectionTokenUnavailable = errors.New("current connection token is unavailable")
)

// IssueConnectionTokenInput 接收首次签发使用的当前 Gateway 连接描述。
type IssueConnectionTokenInput struct {
	TunnelID string
	Endpoint *protocolv1.GatewayEndpoint
	TLSTrust *protocolv1.TlsTrustDescriptor
}

// ConnectionTokenResult 返回 Tunnel 当前 Credential；调用方不得记录 Token 字段。
// 首次签发与后续添加 Connector 都使用同一结果形状。
type ConnectionTokenResult struct {
	TunnelID     string
	TokenID      string
	TokenVersion int64
	Token        string
}

// VerifiedConnectionToken 是认证后可安全使用的 Credential 身份，不包含 Secret。
type VerifiedConnectionToken struct {
	TunnelID     string
	TokenID      string
	TokenVersion int64
	// DesiredRevision 是认证事务读取到的 Tunnel 当前期望配置版本。
	// Auth Success 使用该值下发首份 Snapshot 基线，不能在处理器中写死为零。
	DesiredRevision int64
}

// ConnectionTokenService 管理 Tunnel 首次签发、重复获取与认证。
// 添加 Connector 必须调用 Current 复用 Token，不能调用 Issue 创建新版本。
type ConnectionTokenService struct {
	store     repository.Store
	protector TokenProtector
	random    io.Reader
	now       func() time.Time
}

// NewConnectionTokenService 返回使用 crypto/rand.Reader 的生产 Token 服务。
// protector 的 32 字节主密钥生命周期由 Server Bootstrap 管理。
func NewConnectionTokenService(store repository.Store, protector TokenProtector) *ConnectionTokenService {
	return newConnectionTokenService(store, protector, rand.Reader, time.Now)
}

func newConnectionTokenService(store repository.Store, protector TokenProtector, random io.Reader, now func() time.Time) *ConnectionTokenService {
	return &ConnectionTokenService{store: store, protector: protector, random: random, now: now}
}

// Issue 为既有 Tunnel 创建唯一的第 1 代 ACTIVE Connection Token。
// 完整 Token 经保护器加密后入库，使后续 Connector 可获取相同 Token；认证 Secret
// 仍单独保存 SHA-256 摘要，认证热路径不需要解密。
func (service *ConnectionTokenService) Issue(ctx context.Context, input IssueConnectionTokenInput) (ConnectionTokenResult, error) {
	if service == nil || service.store == nil || service.protector == nil || service.random == nil || service.now == nil ||
		!validate.ValidID(input.TunnelID, "tun_") || input.Endpoint == nil || input.TLSTrust == nil {
		return ConnectionTokenResult{}, ErrConnectionTokenInput
	}

	secret, err := randomSecret(service.random)
	if err != nil {
		return ConnectionTokenResult{}, err
	}
	defer clear(secret[:])
	issuedAt := service.now().UTC()
	tokenID, err := newTokenID(issuedAt, service.random)
	if err != nil {
		return ConnectionTokenResult{}, err
	}
	connectionToken := &protocolv1.ConnectionToken{
		FormatVersion:        token.FormatVersionV1,
		Endpoint:             input.Endpoint,
		TlsTrust:             input.TLSTrust,
		TunnelId:             input.TunnelID,
		TokenId:              tokenID,
		TokenVersion:         uint64(initialConnectionTokenVersion),
		AuthenticationSecret: secret[:],
	}
	encoded, err := token.Encode(connectionToken)
	if err != nil {
		return ConnectionTokenResult{}, fmt.Errorf("%w: connection description", ErrConnectionTokenInput)
	}

	protectionContext := TokenProtectionContext{TunnelID: input.TunnelID, TokenID: tokenID, Version: initialConnectionTokenVersion}
	ciphertext, err := service.protector.Seal([]byte(encoded), protectionContext)
	if err != nil {
		return ConnectionTokenResult{}, fmt.Errorf("%w: seal", ErrConnectionTokenUnavailable)
	}
	defer clear(ciphertext)
	createdAt := issuedAt.Unix()
	if createdAt <= 0 {
		return ConnectionTokenResult{}, ErrConnectionTokenInput
	}
	metadata := repository.TunnelToken{
		ID: tokenID, TunnelID: input.TunnelID, SecretHash: sha256.Sum256(secret[:]),
		TokenCiphertext: ciphertext, Version: initialConnectionTokenVersion,
		Status: repository.TunnelTokenStatusActive, CreatedAt: createdAt,
	}
	if err := service.store.WithTx(ctx, func(transaction repository.TxStore) error {
		tunnelRecord, err := transaction.Tunnels().Get(ctx, input.TunnelID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ErrConnectionTokenTunnelUnavailable
			}
			return fmt.Errorf("load tunnel for connection token: %w", err)
		}
		if tunnelRecord.RevokedAt != nil {
			return ErrConnectionTokenTunnelUnavailable
		}
		if _, err := transaction.TunnelTokens().GetActiveByTunnel(ctx, input.TunnelID); err == nil {
			return ErrConnectionTokenAlreadyActive
		} else if !errors.Is(err, repository.ErrNotFound) {
			return fmt.Errorf("check active connection token: %w", err)
		}
		if _, err := transaction.TunnelTokens().GetByTunnelVersion(ctx, input.TunnelID, initialConnectionTokenVersion); err == nil {
			return ErrConnectionTokenVersionExists
		} else if !errors.Is(err, repository.ErrNotFound) {
			return fmt.Errorf("check initial connection token version: %w", err)
		}
		if err := transaction.TunnelTokens().Create(ctx, metadata); err != nil {
			return fmt.Errorf("create connection token metadata: %w", err)
		}
		return nil
	}); err != nil {
		return ConnectionTokenResult{}, err
	}
	return ConnectionTokenResult{TunnelID: input.TunnelID, TokenID: tokenID, TokenVersion: initialConnectionTokenVersion, Token: encoded}, nil
}

// Current 解密并返回 Tunnel 当前 ACTIVE Token。
// Management 新增 Connector 时必须调用本方法，因此返回字节与首次 Issue 完全相同，
// 且不会创建数据库行、递增 Credential 版本或改变既有 Connector 的认证材料。
func (service *ConnectionTokenService) Current(ctx context.Context, tunnelID string) (ConnectionTokenResult, error) {
	if service == nil || service.store == nil || service.protector == nil || !validate.ValidID(tunnelID, "tun_") {
		return ConnectionTokenResult{}, ErrConnectionTokenInput
	}
	var metadata repository.TunnelToken
	if err := service.store.Read(ctx, func(transaction repository.RepositoryView) error {
		var err error
		metadata, err = transaction.TunnelTokens().GetActiveByTunnel(ctx, tunnelID)
		if errors.Is(err, repository.ErrNotFound) {
			return ErrConnectionTokenUnavailable
		}
		if err != nil {
			return fmt.Errorf("load current connection token: %w", err)
		}
		return nil
	}); err != nil {
		return ConnectionTokenResult{}, err
	}

	protectionContext := TokenProtectionContext{TunnelID: metadata.TunnelID, TokenID: metadata.ID, Version: metadata.Version}
	plaintext, err := service.protector.Open(metadata.TokenCiphertext, protectionContext)
	if err != nil {
		return ConnectionTokenResult{}, ErrConnectionTokenUnavailable
	}
	defer clear(plaintext)
	parsed, err := token.Parse(string(plaintext))
	if err != nil || parsed.GetTunnelId() != metadata.TunnelID || parsed.GetTokenId() != metadata.ID ||
		parsed.GetTokenVersion() != uint64(metadata.Version) {
		return ConnectionTokenResult{}, ErrConnectionTokenUnavailable
	}
	secretHash := sha256.Sum256(parsed.GetAuthenticationSecret())
	if subtle.ConstantTimeCompare(metadata.SecretHash[:], secretHash[:]) != 1 {
		return ConnectionTokenResult{}, ErrConnectionTokenUnavailable
	}
	return ConnectionTokenResult{
		TunnelID: metadata.TunnelID, TokenID: metadata.ID, TokenVersion: metadata.Version, Token: string(plaintext),
	}, nil
}

// Verify 严格解析 Token 后按 Tunnel、Token ID、代次和 Secret 摘要精确核验。
func (service *ConnectionTokenService) Verify(ctx context.Context, encoded string) (VerifiedConnectionToken, error) {
	if service == nil || service.store == nil {
		return VerifiedConnectionToken{}, ErrConnectionTokenInput
	}
	parsed, err := token.Parse(encoded)
	if err != nil || parsed.GetTokenVersion() > uint64(1<<63-1) {
		return VerifiedConnectionToken{}, fmt.Errorf("%w: parse", ErrConnectionTokenInvalid)
	}
	identity := VerifiedConnectionToken{
		TunnelID: parsed.GetTunnelId(), TokenID: parsed.GetTokenId(), TokenVersion: int64(parsed.GetTokenVersion()),
	}
	secretHash := sha256.Sum256(parsed.GetAuthenticationSecret())
	if err := service.store.Read(ctx, func(transaction repository.RepositoryView) error {
		metadata, err := transaction.TunnelTokens().GetByIdentity(ctx, identity.TunnelID, identity.TokenID, identity.TokenVersion)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ErrConnectionTokenIdentityMismatch
			}
			return fmt.Errorf("load connection token metadata: %w", err)
		}
		if metadata.TunnelID != identity.TunnelID || metadata.ID != identity.TokenID || metadata.Version != identity.TokenVersion {
			return ErrConnectionTokenIdentityMismatch
		}
		if subtle.ConstantTimeCompare(metadata.SecretHash[:], secretHash[:]) != 1 {
			return ErrConnectionTokenSecretMismatch
		}
		if metadata.Status != repository.TunnelTokenStatusActive {
			return ErrConnectionTokenInactive
		}
		tunnelRecord, err := transaction.Tunnels().Get(ctx, identity.TunnelID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ErrConnectionTokenIdentityMismatch
			}
			return fmt.Errorf("load connection token tunnel: %w", err)
		}
		if tunnelRecord.RevokedAt != nil {
			return ErrConnectionTokenTunnelRevoked
		}
		identity.DesiredRevision = tunnelRecord.DesiredRevision
		return nil
	}); err != nil {
		return VerifiedConnectionToken{}, err
	}
	return identity, nil
}

func randomSecret(random io.Reader) ([sha256.Size]byte, error) {
	var secret [sha256.Size]byte
	if _, err := io.ReadFull(random, secret[:]); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("%w: %v", ErrConnectionTokenRandom, err)
	}
	return secret, nil
}

// newTokenID 构造 48 位毫秒时间戳加 80 位 CSPRNG 熵的 ULID。
func newTokenID(now time.Time, random io.Reader) (string, error) {
	milliseconds := now.UTC().UnixMilli()
	if milliseconds < 0 || milliseconds >= 1<<48 {
		return "", ErrConnectionTokenInput
	}
	var raw [16]byte
	for index := 5; index >= 0; index-- {
		raw[index] = byte(milliseconds)
		milliseconds >>= 8
	}
	if _, err := io.ReadFull(random, raw[6:]); err != nil {
		return "", fmt.Errorf("%w: %v", ErrConnectionTokenRandom, err)
	}
	return "tok_" + encodeULID(raw), nil
}

// encodeULID 将 128 位 ULID 编码为 26 个大写 Crockford Base32 字符。
func encodeULID(raw [16]byte) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var encoded [26]byte
	for outputIndex := range encoded {
		var value byte
		for bit := 0; bit < 5; bit++ {
			position := outputIndex*5 + bit
			value <<= 1
			if position < 2 {
				continue
			}
			rawPosition := position - 2
			value |= (raw[rawPosition/8] >> (7 - rawPosition%8)) & 1
		}
		encoded[outputIndex] = alphabet[value]
	}
	return string(encoded[:])
}
