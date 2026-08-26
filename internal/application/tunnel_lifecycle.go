package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
)

var (
	ErrTunnelLifecycleInput     = errors.New("tunnel lifecycle input is invalid")
	ErrTunnelRuntimeConvergence = errors.New("tunnel revoke committed but runtime convergence failed")
)

const tunnelRuntimeConvergenceErrorCode = "RUNTIME_CONVERGENCE_FAILED"

// TunnelRuntimeRevoker 是持久化 Application Service 唯一依赖的运行时关闭端口。
// 实现必须自行关闭该 Tunnel 全 generation Session/ActiveWork，并保持幂等。
type TunnelRuntimeRevoker interface {
	RevokeTunnel(string) error
}

type TunnelRevokeInput struct {
	TunnelID        string
	ExpectedVersion int64
	Audit           SecurityAuditContext
}

type TunnelRevokeResult struct {
	TunnelID       string
	TunnelVersion  int64
	AlreadyRevoked bool
}

// TunnelLifecycleService 先耐久提交权威吊销状态和审计，再收敛内存 Runtime。
type TunnelLifecycleService struct {
	store               repository.Store
	audit               *SecurityAuditWriter
	runtime             TunnelRuntimeRevoker
	now                 func() time.Time
	newAuditEventID     func() (string, error)
	newAuditOperationID func() (string, error)
}

func NewTunnelLifecycleService(
	store repository.Store,
	audit *SecurityAuditWriter,
	runtime TunnelRuntimeRevoker,
) *TunnelLifecycleService {
	return &TunnelLifecycleService{
		store: store, audit: audit, runtime: runtime, now: time.Now,
		newAuditEventID: identity.NewAuditEventID, newAuditOperationID: identity.NewOperationID,
	}
}

// Revoke 对已经撤销且版本仍匹配的 Tunnel 不重复递增版本，但仍调用 Runtime 收敛。
func (service *TunnelLifecycleService) Revoke(ctx context.Context, input TunnelRevokeInput) (TunnelRevokeResult, error) {
	if !service.valid(ctx) || !validate.ValidID(input.TunnelID, "tun_") || input.ExpectedVersion < 1 {
		return TunnelRevokeResult{}, ErrTunnelLifecycleInput
	}
	revokedAt := service.now().UTC().Unix()
	if revokedAt <= 0 {
		return TunnelRevokeResult{}, ErrTunnelLifecycleInput
	}
	event, err := newAdminAuditEvent(
		service.newAuditEventID, service.newAuditOperationID, revokedAt,
		repository.SecurityAuditActionTunnelRevoke, repository.SecurityAuditResourceTunnel, input.TunnelID, input.Audit,
	)
	if err != nil {
		return TunnelRevokeResult{}, err
	}
	failureEvent, err := newAdminAuditEvent(
		service.newAuditEventID, service.newAuditOperationID, revokedAt,
		repository.SecurityAuditActionTunnelRevoke, repository.SecurityAuditResourceTunnel, input.TunnelID, input.Audit,
	)
	if err != nil {
		return TunnelRevokeResult{}, err
	}
	failureEvent.Result = repository.SecurityAuditResultFailed
	failureEvent.ErrorCode = tunnelRuntimeConvergenceErrorCode
	if err := failureEvent.Validate(); err != nil {
		return TunnelRevokeResult{}, err
	}
	result := TunnelRevokeResult{TunnelID: input.TunnelID}
	transactionErr := service.store.WithDurableTx(ctx, func(transaction repository.TxStore) error {
		tunnelRecord, err := transaction.Tunnels().Get(ctx, input.TunnelID)
		if err != nil {
			return err
		}
		if tunnelRecord.Version != input.ExpectedVersion {
			return repository.ErrVersionConflict
		}
		if tunnelRecord.RevokedAt != nil {
			result.TunnelVersion = tunnelRecord.Version
			result.AlreadyRevoked = true
			return service.audit.appendTo(ctx, transaction, event)
		}
		updated, err := transaction.Tunnels().Revoke(ctx, input.TunnelID, input.ExpectedVersion, revokedAt)
		if err != nil {
			return err
		}
		if err := transaction.TunnelTokens().RevokeAll(ctx, input.TunnelID, revokedAt); err != nil {
			return err
		}
		result.TunnelVersion = updated.Version
		return service.audit.appendTo(ctx, transaction, event)
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return TunnelRevokeResult{}, transactionErr
	}
	service.audit.logCommitted(ctx, event)
	runtimeErr := service.runtime.RevokeTunnel(input.TunnelID)
	if runtimeErr == nil {
		return result, transactionErr
	}
	convergenceErr := errors.Join(ErrTunnelRuntimeConvergence, fmt.Errorf("revoke tunnel runtime: %w", runtimeErr))
	if err := service.audit.Append(ctx, failureEvent); err != nil {
		convergenceErr = errors.Join(convergenceErr, fmt.Errorf("append tunnel runtime convergence failure audit: %w", err))
	}
	return result, errors.Join(transactionErr, convergenceErr)
}

func (service *TunnelLifecycleService) valid(ctx context.Context) bool {
	return service != nil && service.store != nil && service.audit != nil && service.audit.valid(ctx) && service.runtime != nil &&
		service.now != nil && service.newAuditEventID != nil && service.newAuditOperationID != nil
}

func newAdminAuditEvent(
	newEventID, newOperationID func() (string, error),
	occurredAt int64,
	action, resourceType, tunnelID string,
	auditContext SecurityAuditContext,
) (repository.SecurityAuditEvent, error) {
	eventID, err := newEventID()
	if err != nil {
		return repository.SecurityAuditEvent{}, fmt.Errorf("generate security audit event identifier: %w", err)
	}
	operationID, err := newOperationID()
	if err != nil {
		return repository.SecurityAuditEvent{}, fmt.Errorf("generate security audit operation identifier: %w", err)
	}
	event := repository.SecurityAuditEvent{
		EventID: eventID, OperationID: operationID, Event: repository.SecurityAuditEventOperationResult,
		Action: action, ActorType: repository.SecurityAuditActorAdmin, ActorID: auditContext.ActorID,
		SourceIP: auditContext.SourceIP, ResourceType: resourceType, ResourceID: tunnelID,
		Result: repository.SecurityAuditResultSucceeded, RequestID: auditContext.RequestID,
		TraceID: auditContext.TraceID, OccurredAt: occurredAt,
	}
	if err := event.Validate(); err != nil {
		return repository.SecurityAuditEvent{}, err
	}
	return event, nil
}
