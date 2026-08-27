package application

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	defaultServiceConnectTimeoutMS uint32 = 5_000
	defaultHealthIntervalMS        uint32 = 10_000
	defaultHealthTimeoutMS         uint32 = 2_000
	defaultHealthPath                     = "/health"
	defaultHealthStatusMin         uint32 = 200
	defaultHealthStatusMax         uint32 = 399
	defaultHealthFailureThreshold  uint32 = 3
	defaultHealthSuccessThreshold  uint32 = 2
)

var (
	// ErrServiceManagementInput 表示 Service Mutation 输入不完整或不符合领域约束。
	ErrServiceManagementInput = errors.New("service management input is invalid")
	// ErrServiceManagementUnavailable 表示指定 Tunnel 或 Service 不存在。
	ErrServiceManagementUnavailable = errors.New("service management resource is unavailable")
	// ErrServiceManagementTunnelRevoked 表示所属 Tunnel 已被永久撤销。
	ErrServiceManagementTunnelRevoked = errors.New("service management tunnel is revoked")
	// ErrServiceManagementRevisionExhausted 表示 Tunnel Desired Revision 已无法继续递增。
	ErrServiceManagementRevisionExhausted = errors.New("service management revision is exhausted")
	// ErrServiceRuntimeConvergence 表示 Service Desired State 已提交，但运行态收敛通知失败。
	ErrServiceRuntimeConvergence = errors.New("service mutation committed but runtime convergence failed")
)

// TunnelSnapshotGate 在事务提交前校验受影响 Tunnel 的完整 Candidate Service 集合。
type TunnelSnapshotGate interface {
	Validate(tunnelID string, revision int64, services []repository.Service) error
}

// SnapshotReconcileNotifier 在 Service Desired State 提交后标记受影响 Tunnel 待收敛。
type SnapshotReconcileNotifier interface {
	MarkDirty(tunnelID string) error
}

// ServiceOriginInput 是 Create 或 Update 提交的完整 Origin 配置。
type ServiceOriginInput struct {
	Scheme           repository.OriginScheme
	Host             string
	Port             uint32
	TLSVerify        *bool
	TLSServerName    string
	HTTPHost         string
	ConnectTimeoutMS *uint32
}

// ServiceHealthInput 是启用 Health 时提交的策略。nil 表示未提供或 Disabled。
// Type 是必填判别字段，其余 nil 字段由 Application Service 补全冻结默认值。
type ServiceHealthInput struct {
	Type              repository.HealthType
	Path              *string
	IntervalMS        *uint32
	TimeoutMS         *uint32
	ExpectedStatusMin *uint32
	ExpectedStatusMax *uint32
	FailureThreshold  *uint32
	SuccessThreshold  *uint32
}

// CreateServiceInput 使用所属 Tunnel 当前 ETag 创建 Service。
type CreateServiceInput struct {
	TunnelID              string
	ExpectedTunnelVersion int64
	Name                  string
	Origin                ServiceOriginInput
	Health                *ServiceHealthInput
	Enabled               *bool
}

// UpdateServiceInput 使用 Service 与 Tunnel 两个精确版本执行 PATCH。
// Origin 非 nil 时替换完整 Origin；DisableHealth 用于显式落库 Disabled。
type UpdateServiceInput struct {
	TunnelID               string
	ServiceID              string
	ExpectedTunnelVersion  int64
	ExpectedServiceVersion int64
	Name                   *string
	Origin                 *ServiceOriginInput
	Health                 *ServiceHealthInput
	DisableHealth          bool
	Enabled                *bool
}

// DeleteServiceInput 使用 Service 与 Tunnel 两个精确版本删除 Service。
type DeleteServiceInput struct {
	TunnelID               string
	ServiceID              string
	ExpectedTunnelVersion  int64
	ExpectedServiceVersion int64
}

// ServiceMutationResult 返回提交后的 Service 与 Tunnel 版本状态。
// Delete 返回删除前的 Service；Changed=false 仅表示 Update 是完整 no-op。
type ServiceMutationResult struct {
	Service        repository.Service
	TunnelVersion  int64
	TunnelRevision int64
	Changed        bool
}

// ServiceManagementService 是 Service Desired State Mutation 的唯一事务入口。
type ServiceManagementService struct {
	store        repository.Store
	gate         TunnelSnapshotGate
	notifier     SnapshotReconcileNotifier
	newServiceID func() (string, error)
	now          func() time.Time
}

// NewServiceManagementService 返回使用 CSPRNG Service ID 与系统时钟的生产服务。
func NewServiceManagementService(
	store repository.Store,
	gate TunnelSnapshotGate,
	notifier SnapshotReconcileNotifier,
) *ServiceManagementService {
	return &ServiceManagementService{
		store: store, gate: gate, notifier: notifier, newServiceID: identity.NewServiceID, now: time.Now,
	}
}

// Create 创建 Service，并在同一事务内验证完整 Candidate 后推进一次 Tunnel Revision。
func (service *ServiceManagementService) Create(ctx context.Context, input CreateServiceInput) (ServiceMutationResult, error) {
	if !service.valid(ctx) || !validTunnelMutationInput(input.TunnelID, input.ExpectedTunnelVersion) {
		return ServiceMutationResult{}, ErrServiceManagementInput
	}
	serviceID, err := service.newServiceID()
	if err != nil {
		return ServiceMutationResult{}, fmt.Errorf("generate service identifier: %w", err)
	}
	now, err := service.timestamp()
	if err != nil {
		return ServiceMutationResult{}, err
	}
	candidate, err := newServiceCandidate(serviceID, input, now)
	if err != nil {
		return ServiceMutationResult{}, err
	}

	var result ServiceMutationResult
	if err := service.store.WithTx(ctx, func(transaction repository.TxStore) error {
		tunnel, err := loadServiceMutationTunnel(ctx, transaction, input.TunnelID, input.ExpectedTunnelVersion)
		if err != nil {
			return err
		}
		nextRevision, err := nextServiceRevision(tunnel.DesiredRevision)
		if err != nil {
			return err
		}
		candidate.RequiredRevision = nextRevision
		if err := transaction.Services().Create(ctx, candidate); err != nil {
			return fmt.Errorf("create service: %w", err)
		}
		services, err := transaction.Services().ListByTunnel(ctx, input.TunnelID)
		if err != nil {
			return fmt.Errorf("list service snapshot candidate: %w", err)
		}
		if err := service.gate.Validate(input.TunnelID, nextRevision, services); err != nil {
			return fmt.Errorf("validate service snapshot candidate: %w", err)
		}
		updatedTunnel, err := transaction.Tunnels().AdvanceDesiredRevision(
			ctx, input.TunnelID, input.ExpectedTunnelVersion, tunnel.DesiredRevision, now,
		)
		if err != nil {
			return err
		}
		result = serviceMutationResult(candidate, updatedTunnel, true)
		return nil
	}); err != nil {
		return ServiceMutationResult{}, err
	}
	return service.notifySnapshotReconcile(input.TunnelID, result)
}

// Update 应用 Service PATCH。只有影响 Agent Snapshot 的字段才推进 Tunnel Revision；
// 仅 Name 变化只推进 Service Version，完整 no-op 不执行任何写入。
func (service *ServiceManagementService) Update(ctx context.Context, input UpdateServiceInput) (ServiceMutationResult, error) {
	if !service.valid(ctx) || !validServiceMutationInput(
		input.TunnelID, input.ServiceID, input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
	) || (input.Health != nil && input.DisableHealth) {
		return ServiceMutationResult{}, ErrServiceManagementInput
	}

	var result ServiceMutationResult
	var notifyReconcile bool
	if err := service.store.WithTx(ctx, func(transaction repository.TxStore) error {
		tunnel, err := loadServiceMutationTunnel(ctx, transaction, input.TunnelID, input.ExpectedTunnelVersion)
		if err != nil {
			return err
		}
		current, err := loadServiceForMutation(ctx, transaction, input.TunnelID, input.ServiceID)
		if err != nil {
			return err
		}
		if current.Version != input.ExpectedServiceVersion {
			return repository.ErrVersionConflict
		}

		candidate, snapshotChanged, changed, err := applyServiceUpdate(current, input)
		if err != nil {
			return err
		}
		if !changed {
			result = serviceMutationResult(current, tunnel, false)
			return nil
		}
		now, err := service.timestamp()
		if err != nil {
			return err
		}
		candidate.UpdatedAt = now
		if snapshotChanged {
			nextRevision, err := nextServiceRevision(tunnel.DesiredRevision)
			if err != nil {
				return err
			}
			candidate.RequiredRevision = nextRevision
		}
		updated, err := transaction.Services().Update(ctx, candidate, input.ExpectedServiceVersion)
		if err != nil {
			return err
		}
		if !snapshotChanged {
			result = serviceMutationResult(updated, tunnel, true)
			return nil
		}
		services, err := transaction.Services().ListByTunnel(ctx, input.TunnelID)
		if err != nil {
			return fmt.Errorf("list service snapshot candidate: %w", err)
		}
		if err := service.gate.Validate(input.TunnelID, candidate.RequiredRevision, services); err != nil {
			return fmt.Errorf("validate service snapshot candidate: %w", err)
		}
		updatedTunnel, err := transaction.Tunnels().AdvanceDesiredRevision(
			ctx, input.TunnelID, input.ExpectedTunnelVersion, tunnel.DesiredRevision, now,
		)
		if err != nil {
			return err
		}
		result = serviceMutationResult(updated, updatedTunnel, true)
		notifyReconcile = true
		return nil
	}); err != nil {
		return ServiceMutationResult{}, err
	}
	if notifyReconcile {
		return service.notifySnapshotReconcile(input.TunnelID, result)
	}
	return result, nil
}

// Delete 删除 Service，并以删除后的完整 Candidate 推进一次 Tunnel Revision。
func (service *ServiceManagementService) Delete(ctx context.Context, input DeleteServiceInput) (ServiceMutationResult, error) {
	if !service.valid(ctx) || !validServiceMutationInput(
		input.TunnelID, input.ServiceID, input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
	) {
		return ServiceMutationResult{}, ErrServiceManagementInput
	}

	var result ServiceMutationResult
	if err := service.store.WithTx(ctx, func(transaction repository.TxStore) error {
		tunnel, err := loadServiceMutationTunnel(ctx, transaction, input.TunnelID, input.ExpectedTunnelVersion)
		if err != nil {
			return err
		}
		current, err := loadServiceForMutation(ctx, transaction, input.TunnelID, input.ServiceID)
		if err != nil {
			return err
		}
		if current.Version != input.ExpectedServiceVersion {
			return repository.ErrVersionConflict
		}
		nextRevision, err := nextServiceRevision(tunnel.DesiredRevision)
		if err != nil {
			return err
		}
		if err := transaction.Services().Delete(ctx, input.TunnelID, input.ServiceID, input.ExpectedServiceVersion); err != nil {
			return err
		}
		services, err := transaction.Services().ListByTunnel(ctx, input.TunnelID)
		if err != nil {
			return fmt.Errorf("list service snapshot candidate: %w", err)
		}
		if err := service.gate.Validate(input.TunnelID, nextRevision, services); err != nil {
			return fmt.Errorf("validate service snapshot candidate: %w", err)
		}
		now, err := service.timestamp()
		if err != nil {
			return err
		}
		updatedTunnel, err := transaction.Tunnels().AdvanceDesiredRevision(
			ctx, input.TunnelID, input.ExpectedTunnelVersion, tunnel.DesiredRevision, now,
		)
		if err != nil {
			return err
		}
		result = serviceMutationResult(current, updatedTunnel, true)
		return nil
	}); err != nil {
		return ServiceMutationResult{}, err
	}
	return service.notifySnapshotReconcile(input.TunnelID, result)
}

func (service *ServiceManagementService) valid(ctx context.Context) bool {
	return service != nil && ctx != nil && service.store != nil && service.gate != nil && service.notifier != nil &&
		service.newServiceID != nil && service.now != nil
}

func (service *ServiceManagementService) notifySnapshotReconcile(
	tunnelID string,
	result ServiceMutationResult,
) (ServiceMutationResult, error) {
	if err := service.notifier.MarkDirty(tunnelID); err != nil {
		return result, fmt.Errorf("%w: mark Tunnel Snapshot dirty: %w", ErrServiceRuntimeConvergence, err)
	}
	return result, nil
}

func (service *ServiceManagementService) timestamp() (int64, error) {
	now := service.now().UTC().Unix()
	if now <= 0 {
		return 0, ErrServiceManagementInput
	}
	return now, nil
}

func validTunnelMutationInput(tunnelID string, expectedTunnelVersion int64) bool {
	return identity.ValidTunnelID(tunnelID) && expectedTunnelVersion >= 1
}

func validServiceMutationInput(tunnelID, serviceID string, expectedTunnelVersion, expectedServiceVersion int64) bool {
	return validTunnelMutationInput(tunnelID, expectedTunnelVersion) && identity.ValidServiceID(serviceID) &&
		expectedServiceVersion >= 1
}

func loadServiceMutationTunnel(
	ctx context.Context,
	transaction repository.RepositoryView,
	tunnelID string,
	expectedVersion int64,
) (repository.Tunnel, error) {
	tunnel, err := transaction.Tunnels().Get(ctx, tunnelID)
	if errors.Is(err, repository.ErrNotFound) {
		return repository.Tunnel{}, ErrServiceManagementUnavailable
	}
	if err != nil {
		return repository.Tunnel{}, fmt.Errorf("load tunnel for service mutation: %w", err)
	}
	if tunnel.RevokedAt != nil {
		return repository.Tunnel{}, ErrServiceManagementTunnelRevoked
	}
	if tunnel.Version != expectedVersion {
		return repository.Tunnel{}, repository.ErrVersionConflict
	}
	return tunnel, nil
}

func loadServiceForMutation(
	ctx context.Context,
	transaction repository.RepositoryView,
	tunnelID, serviceID string,
) (repository.Service, error) {
	service, err := transaction.Services().Get(ctx, tunnelID, serviceID)
	if errors.Is(err, repository.ErrNotFound) {
		return repository.Service{}, ErrServiceManagementUnavailable
	}
	if err != nil {
		return repository.Service{}, fmt.Errorf("load service for mutation: %w", err)
	}
	return service, nil
}

func nextServiceRevision(current int64) (int64, error) {
	if current < 0 || current == math.MaxInt64 {
		return 0, ErrServiceManagementRevisionExhausted
	}
	return current + 1, nil
}

func newServiceCandidate(serviceID string, input CreateServiceInput, now int64) (repository.Service, error) {
	origin := serviceOrigin(input.Origin)
	health, err := serviceHealth(input.Health)
	if err != nil {
		return repository.Service{}, err
	}
	candidate := repository.Service{
		ID: serviceID, TunnelID: input.TunnelID, Name: input.Name,
		OriginScheme: origin.Scheme, OriginHost: origin.Host, OriginPort: origin.Port,
		TLSVerify: origin.TLSVerify, TLSServerName: origin.TLSServerName,
		OriginHTTPHost: origin.HTTPHost, ConnectTimeoutMS: origin.ConnectTimeoutMS,
		Health: health, Enabled: boolValue(input.Enabled, true), Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := candidate.Validate(); err != nil {
		return repository.Service{}, fmt.Errorf("%w: service fields", ErrServiceManagementInput)
	}
	return candidate, nil
}

type normalizedServiceOrigin struct {
	Scheme           repository.OriginScheme
	Host             string
	Port             uint32
	TLSVerify        bool
	TLSServerName    string
	HTTPHost         string
	ConnectTimeoutMS uint32
}

func serviceOrigin(input ServiceOriginInput) normalizedServiceOrigin {
	return normalizedServiceOrigin{
		Scheme: input.Scheme, Host: input.Host, Port: input.Port,
		TLSVerify: boolValue(input.TLSVerify, true), TLSServerName: input.TLSServerName,
		HTTPHost: input.HTTPHost, ConnectTimeoutMS: uint32Value(input.ConnectTimeoutMS, defaultServiceConnectTimeoutMS),
	}
}

func serviceHealth(input *ServiceHealthInput) (*repository.HealthCheck, error) {
	if input == nil {
		return nil, nil
	}
	health := &repository.HealthCheck{
		Type:             input.Type,
		IntervalMS:       uint32Value(input.IntervalMS, defaultHealthIntervalMS),
		TimeoutMS:        uint32Value(input.TimeoutMS, defaultHealthTimeoutMS),
		FailureThreshold: uint32Value(input.FailureThreshold, defaultHealthFailureThreshold),
		SuccessThreshold: uint32Value(input.SuccessThreshold, defaultHealthSuccessThreshold),
	}
	switch input.Type {
	case repository.HealthTypeTCP:
		if input.Path != nil || input.ExpectedStatusMin != nil || input.ExpectedStatusMax != nil {
			return nil, fmt.Errorf("%w: TCP health contains HTTP fields", ErrServiceManagementInput)
		}
	case repository.HealthTypeHTTP:
		health.Path = stringValue(input.Path, defaultHealthPath)
		health.ExpectedStatusMin = uint32Value(input.ExpectedStatusMin, defaultHealthStatusMin)
		health.ExpectedStatusMax = uint32Value(input.ExpectedStatusMax, defaultHealthStatusMax)
	default:
		return nil, fmt.Errorf("%w: health type", ErrServiceManagementInput)
	}
	return health, nil
}

func applyServiceUpdate(
	current repository.Service,
	input UpdateServiceInput,
) (repository.Service, bool, bool, error) {
	candidate := current
	if input.Name != nil {
		candidate.Name = *input.Name
	}
	if input.Origin != nil {
		origin := serviceOrigin(*input.Origin)
		candidate.OriginScheme = origin.Scheme
		candidate.OriginHost = origin.Host
		candidate.OriginPort = origin.Port
		candidate.TLSVerify = origin.TLSVerify
		candidate.TLSServerName = origin.TLSServerName
		candidate.OriginHTTPHost = origin.HTTPHost
		candidate.ConnectTimeoutMS = origin.ConnectTimeoutMS
	}
	if input.DisableHealth {
		candidate.Health = nil
	} else if input.Health != nil {
		health, err := serviceHealth(input.Health)
		if err != nil {
			return repository.Service{}, false, false, err
		}
		candidate.Health = health
	}
	if input.Enabled != nil {
		candidate.Enabled = *input.Enabled
	}
	if err := candidate.Validate(); err != nil {
		return repository.Service{}, false, false, fmt.Errorf("%w: service fields", ErrServiceManagementInput)
	}

	snapshotChanged := !sameServiceSnapshot(current, candidate)
	changed := snapshotChanged || current.Name != candidate.Name
	return candidate, snapshotChanged, changed, nil
}

func sameServiceSnapshot(left, right repository.Service) bool {
	return left.OriginScheme == right.OriginScheme && left.OriginHost == right.OriginHost &&
		left.OriginPort == right.OriginPort && left.TLSVerify == right.TLSVerify &&
		left.TLSServerName == right.TLSServerName && left.OriginHTTPHost == right.OriginHTTPHost &&
		left.ConnectTimeoutMS == right.ConnectTimeoutMS && left.Enabled == right.Enabled &&
		sameServiceHealth(left.Health, right.Health)
}

func sameServiceHealth(left, right *repository.HealthCheck) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func serviceMutationResult(service repository.Service, tunnel repository.Tunnel, changed bool) ServiceMutationResult {
	return ServiceMutationResult{
		Service: service, TunnelVersion: tunnel.Version, TunnelRevision: tunnel.DesiredRevision, Changed: changed,
	}
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func uint32Value(value *uint32, fallback uint32) uint32 {
	if value == nil {
		return fallback
	}
	return *value
}

func stringValue(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}
