package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"

	"google.golang.org/protobuf/proto"

	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/token"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
)

var (
	ErrCredentialLifecycleInput       = errors.New("credential lifecycle input is invalid")
	ErrCredentialLifecycleUnavailable = errors.New("credential lifecycle is unavailable")
)

// SecurityAuditContext 是已认证管理面传入的安全主体与关联信息。
type SecurityAuditContext struct {
	ActorID   string
	SourceIP  string
	RequestID string
	TraceID   string
}

// CredentialMutationInput 使用 Tunnel aggregate version 实施精确 If-Match 语义。
type CredentialMutationInput struct {
	TunnelID        string
	ExpectedVersion int64
	Audit           SecurityAuditContext
}

// CredentialMutationResult 同时返回新的 Tunnel ETag 版本和 Credential 结果。
// Revoke 不返回 Token 文本；Rotate 的 Token 字段必须按 Secret 处理。
type CredentialMutationResult struct {
	TunnelVersion int64
	Credential    ConnectionTokenResult
}

// CredentialLifecycleService 是 Management Reveal/Rotate/Revoke 的唯一审计入口。
type CredentialLifecycleService struct {
	tokens              *ConnectionTokenService
	audit               *SecurityAuditWriter
	newAuditEventID     func() (string, error)
	newAuditOperationID func() (string, error)
}

func NewCredentialLifecycleService(tokens *ConnectionTokenService, audit *SecurityAuditWriter) *CredentialLifecycleService {
	return &CredentialLifecycleService{
		tokens: tokens, audit: audit,
		newAuditEventID: identity.NewAuditEventID, newAuditOperationID: identity.NewOperationID,
	}
}

// Reveal 在返回当前逐字节稳定 Token 前耐久提交安全审计。
func (service *CredentialLifecycleService) Reveal(
	ctx context.Context,
	tunnelID string,
	auditContext SecurityAuditContext,
) (ConnectionTokenResult, error) {
	if !service.valid(ctx) || !validate.ValidID(tunnelID, "tun_") {
		return ConnectionTokenResult{}, ErrCredentialLifecycleInput
	}
	event, err := service.newAuditEvent(repository.SecurityAuditActionTokenReveal, repository.SecurityAuditResourceTunnelToken, tunnelID, auditContext)
	if err != nil {
		return ConnectionTokenResult{}, err
	}
	var result ConnectionTokenResult
	transactionErr := service.tokens.store.WithDurableTx(ctx, func(transaction repository.TxStore) error {
		var err error
		result, _, err = service.tokens.currentFrom(ctx, transaction, tunnelID)
		if err != nil {
			return err
		}
		return service.audit.appendTo(ctx, transaction, event)
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return ConnectionTokenResult{}, transactionErr
	}
	service.audit.logCommitted(ctx, event)
	return result, transactionErr
}

// Rotate 复用当前 Endpoint/TLS Trust，撤销旧 Token 的新认证并签发下一版本。
// 既有 Session 和 ActiveWork 不属于本服务，不会被关闭或改写。
func (service *CredentialLifecycleService) Rotate(ctx context.Context, input CredentialMutationInput) (CredentialMutationResult, error) {
	if !service.validMutation(ctx, input) {
		return CredentialMutationResult{}, ErrCredentialLifecycleInput
	}
	current, metadata, err := service.tokens.current(ctx, input.TunnelID)
	if err != nil {
		return CredentialMutationResult{}, err
	}
	if metadata.Version == math.MaxInt64 {
		return CredentialMutationResult{}, ErrCredentialLifecycleUnavailable
	}
	parsed, err := token.Parse(current.Token)
	if err != nil {
		return CredentialMutationResult{}, ErrConnectionTokenUnavailable
	}
	defer clear(parsed.AuthenticationSecret)

	issuedAt := service.tokens.now().UTC()
	createdAt := issuedAt.Unix()
	if createdAt <= 0 {
		return CredentialMutationResult{}, ErrCredentialLifecycleInput
	}
	secret, err := randomSecret(service.tokens.random)
	if err != nil {
		return CredentialMutationResult{}, err
	}
	defer clear(secret[:])
	tokenID, err := newTokenID(issuedAt, service.tokens.random)
	if err != nil {
		return CredentialMutationResult{}, err
	}
	nextVersion := metadata.Version + 1
	rotated := &protocolv1.ConnectionToken{
		FormatVersion: token.FormatVersionV1,
		Endpoint:      proto.Clone(parsed.GetEndpoint()).(*protocolv1.GatewayEndpoint),
		TlsTrust:      proto.Clone(parsed.GetTlsTrust()).(*protocolv1.TlsTrustDescriptor),
		TunnelId:      input.TunnelID, TokenId: tokenID, TokenVersion: uint64(nextVersion), AuthenticationSecret: secret[:],
	}
	encoded, err := token.Encode(rotated)
	if err != nil {
		return CredentialMutationResult{}, ErrConnectionTokenUnavailable
	}
	ciphertext, err := service.tokens.protector.Seal([]byte(encoded), TokenProtectionContext{
		TunnelID: input.TunnelID, TokenID: tokenID, Version: nextVersion,
	})
	if err != nil {
		return CredentialMutationResult{}, fmt.Errorf("%w: seal", ErrConnectionTokenUnavailable)
	}
	defer clear(ciphertext)
	candidate := repository.TunnelToken{
		ID: tokenID, TunnelID: input.TunnelID, SecretHash: sha256.Sum256(secret[:]), TokenCiphertext: ciphertext,
		Version: nextVersion, Status: repository.TunnelTokenStatusActive, CreatedAt: createdAt,
	}
	event, err := service.newAuditEvent(repository.SecurityAuditActionTokenRotate, repository.SecurityAuditResourceTunnelToken, input.TunnelID, input.Audit)
	if err != nil {
		return CredentialMutationResult{}, err
	}

	var tunnelVersion int64
	transactionErr := service.tokens.store.WithDurableTx(ctx, func(transaction repository.TxStore) error {
		if err := checkCredentialMutationTunnel(ctx, transaction, input.TunnelID, input.ExpectedVersion); err != nil {
			return err
		}
		active, err := transaction.TunnelTokens().GetActiveByTunnel(ctx, input.TunnelID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ErrCredentialLifecycleUnavailable
			}
			return fmt.Errorf("load active token for rotation: %w", err)
		}
		if active.ID != metadata.ID || active.Version != metadata.Version {
			return repository.ErrVersionConflict
		}
		if err := transaction.TunnelTokens().TransitionStatus(
			ctx, active.TunnelID, active.ID, active.Version,
			repository.TunnelTokenStatusActive, repository.TunnelTokenStatusRevokedForNewSession, createdAt,
		); err != nil {
			return err
		}
		if err := transaction.TunnelTokens().Create(ctx, candidate); err != nil {
			return fmt.Errorf("create rotated connection token: %w", err)
		}
		updated, err := transaction.Tunnels().AdvanceVersion(ctx, input.TunnelID, input.ExpectedVersion, createdAt)
		if err != nil {
			return err
		}
		tunnelVersion = updated.Version
		return service.audit.appendTo(ctx, transaction, event)
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return CredentialMutationResult{}, transactionErr
	}
	service.audit.logCommitted(ctx, event)
	return CredentialMutationResult{
		TunnelVersion: tunnelVersion,
		Credential:    ConnectionTokenResult{TunnelID: input.TunnelID, TokenID: tokenID, TokenVersion: nextVersion, Token: encoded},
	}, transactionErr
}

// Revoke 禁止当前 Credential 建立新 Session，但不关闭任何既有 Session/ActiveWork。
func (service *CredentialLifecycleService) Revoke(ctx context.Context, input CredentialMutationInput) (CredentialMutationResult, error) {
	if !service.validMutation(ctx, input) {
		return CredentialMutationResult{}, ErrCredentialLifecycleInput
	}
	revokedAt := service.tokens.now().UTC().Unix()
	if revokedAt <= 0 {
		return CredentialMutationResult{}, ErrCredentialLifecycleInput
	}
	event, err := service.newAuditEvent(repository.SecurityAuditActionTokenRevoke, repository.SecurityAuditResourceTunnelToken, input.TunnelID, input.Audit)
	if err != nil {
		return CredentialMutationResult{}, err
	}
	var result CredentialMutationResult
	transactionErr := service.tokens.store.WithDurableTx(ctx, func(transaction repository.TxStore) error {
		if err := checkCredentialMutationTunnel(ctx, transaction, input.TunnelID, input.ExpectedVersion); err != nil {
			return err
		}
		active, err := transaction.TunnelTokens().GetActiveByTunnel(ctx, input.TunnelID)
		if err != nil {
			if errors.Is(err, repository.ErrNotFound) {
				return ErrCredentialLifecycleUnavailable
			}
			return fmt.Errorf("load active token for revocation: %w", err)
		}
		if err := transaction.TunnelTokens().TransitionStatus(
			ctx, active.TunnelID, active.ID, active.Version,
			repository.TunnelTokenStatusActive, repository.TunnelTokenStatusRevoked, revokedAt,
		); err != nil {
			return err
		}
		updated, err := transaction.Tunnels().AdvanceVersion(ctx, input.TunnelID, input.ExpectedVersion, revokedAt)
		if err != nil {
			return err
		}
		result.TunnelVersion = updated.Version
		result.Credential = ConnectionTokenResult{TunnelID: input.TunnelID, TokenID: active.ID, TokenVersion: active.Version}
		return service.audit.appendTo(ctx, transaction, event)
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return CredentialMutationResult{}, transactionErr
	}
	service.audit.logCommitted(ctx, event)
	return result, transactionErr
}

func (service *CredentialLifecycleService) valid(ctx context.Context) bool {
	return service != nil && service.tokens != nil && service.tokens.store != nil && service.tokens.protector != nil &&
		service.tokens.random != nil && service.tokens.now != nil && service.audit != nil && service.audit.valid(ctx) &&
		service.newAuditEventID != nil && service.newAuditOperationID != nil
}

func (service *CredentialLifecycleService) validMutation(ctx context.Context, input CredentialMutationInput) bool {
	return service.valid(ctx) && validate.ValidID(input.TunnelID, "tun_") && input.ExpectedVersion >= 1
}

func (service *CredentialLifecycleService) newAuditEvent(
	action, resourceType, tunnelID string,
	auditContext SecurityAuditContext,
) (repository.SecurityAuditEvent, error) {
	return newAdminAuditEvent(
		service.newAuditEventID, service.newAuditOperationID, service.tokens.now().UTC().Unix(),
		action, resourceType, tunnelID, auditContext,
	)
}

func checkCredentialMutationTunnel(
	ctx context.Context,
	transaction repository.RepositoryView,
	tunnelID string,
	expectedVersion int64,
) error {
	tunnelRecord, err := transaction.Tunnels().Get(ctx, tunnelID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrConnectionTokenTunnelUnavailable
		}
		return fmt.Errorf("load tunnel for credential mutation: %w", err)
	}
	if tunnelRecord.RevokedAt != nil {
		return ErrConnectionTokenTunnelRevoked
	}
	if tunnelRecord.Version != expectedVersion {
		return repository.ErrVersionConflict
	}
	return nil
}
