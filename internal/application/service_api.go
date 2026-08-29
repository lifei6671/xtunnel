package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
	serverroute "github.com/lifei6671/xtunnel/internal/server/route"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
	"github.com/lifei6671/xtunnel/internal/tcpport"
)

var (
	// ErrServiceExposureConflict 表示 canonical HTTP Host+Path 已被其他 Service 占用。
	ErrServiceExposureConflict = errors.New("service exposure conflicts with an existing route")
)

// ServiceExposureType 是 Service 唯一公网入口的判别类型。
type ServiceExposureType string

const (
	ServiceExposureHTTP ServiceExposureType = "http"
	ServiceExposureTCP  ServiceExposureType = "tcp"
)

// ServiceExposureInput 创建一个完整 Nested Exposure。TCP 的 PublicPort=nil
// 表示在同一事务内从冻结端口池分配。
type ServiceExposureInput struct {
	Type         ServiceExposureType
	Hostname     string
	PathPrefix   *string
	PreserveHost *bool
	PublicPort   *uint16
}

// ServiceOriginPatchInput 保留 Origin Merge Patch 的字段 presence。
type ServiceOriginPatchInput struct {
	Scheme           *repository.OriginScheme
	Host             *string
	Port             *uint32
	ConnectTimeoutMS *uint32
	TLSVerify        *bool
	TLSServerName    *string
	HTTPHost         *string
}

// ServiceProxyOptionsPatchInput 保留 Proxy Options Merge Patch 的字段 presence。
type ServiceProxyOptionsPatchInput struct {
	DisableChunkedEncoding  *bool
	DisableHappyEyeballs    *bool
	IdleConnectionTimeoutMS *uint32
	MaxIdleConnections      *uint32
	TCPKeepAliveIntervalMS  *uint32
}

// ServiceHealthPatchInput 保留 Health Merge Patch 的字段 presence。
type ServiceHealthPatchInput struct {
	Type              *repository.HealthType
	Path              *string
	IntervalMS        *uint32
	TimeoutMS         *uint32
	ExpectedStatusMin *uint32
	ExpectedStatusMax *uint32
	FailureThreshold  *uint32
	SuccessThreshold  *uint32
}

// ServiceExposurePatchInput 保留 Exposure Merge Patch 的字段 presence。
type ServiceExposurePatchInput struct {
	Type         *ServiceExposureType
	Hostname     *string
	PathPrefix   *string
	PreserveHost *bool
	PublicPort   *uint16
}

// CreateServiceAPIInput 在父 Tunnel ETag 下原子创建 Service 与必需 Exposure。
type CreateServiceAPIInput struct {
	Service  CreateServiceInput
	Exposure ServiceExposureInput
}

// UpdateServiceAPIInput 在组合 ETag 下原子应用 Service 与 Exposure Merge Patch。
// HealthSet/ExposureSet 区分 omitted、null 与 value；Set=true 且指针 nil 表示删除。
type UpdateServiceAPIInput struct {
	TunnelID               string
	ServiceID              string
	ExpectedTunnelVersion  int64
	ExpectedServiceVersion int64
	Name                   *string
	Origin                 *ServiceOriginPatchInput
	ProxyOptions           *ServiceProxyOptionsPatchInput
	HealthSet              bool
	Health                 *ServiceHealthPatchInput
	ExposureSet            bool
	Exposure               *ServiceExposurePatchInput
	Enabled                *bool
}

// ServiceAPIMutationResult 返回复合事务已经提交的聚合版本。
type ServiceAPIMutationResult struct {
	Service        repository.Service
	Exposure       repository.ServiceExposure
	TunnelVersion  int64
	TunnelRevision int64
	Generation     uint64
	Changed        bool
	Deleted        bool
}

// ServiceView 是持久化 Desired State 与 Runtime 值型快照形成的只读投影。
type ServiceView struct {
	Service           repository.Service
	Exposure          repository.ServiceExposure
	TunnelVersion     int64
	Status            serverstatus.ServiceStatus
	ApplyFailure      *serverstatus.ApplyFailure
	HealthyConnectors uint64
	ActiveConnections uint64
}

// ServiceRuntimeOwner 只提供已完成 generation fencing 的 Session 值型快照。
type ServiceRuntimeOwner interface {
	RuntimeStatusSnapshots() []serverruntime.SessionStatusSnapshot
}

// ServiceLimitOwner 提供一个线性化时刻的连接计数深拷贝。
type ServiceLimitOwner interface {
	Snapshot() serverlimits.Snapshot
}

// ServiceApplyFailureOwner 提供当前 revision 的 Route Apply Failure。
type ServiceApplyFailureOwner interface {
	ServiceApplyFailure(serviceID string, requiredRevision uint64) *serverstatus.ApplyFailure
}

// ServiceAPIService 是 Management Service API 的复合 Application owner。
// Mutation 复用既有 per-Tunnel owner，在一笔 SQLite 事务内线性化 Service、
// Nested Exposure、Tunnel Revision 与 Route Generation；提交后才通知两个 Runtime owner。
type ServiceAPIService struct {
	owner         *ServiceManagementService
	policy        tcpport.Policy
	routeNotifier RouteReconcileNotifier
	runtime       ServiceRuntimeOwner
	limits        ServiceLimitOwner
	applyFailures ServiceApplyFailureOwner
	now           func() time.Time
}

// NewServiceAPIService 绑定 Service/Route 写 owner 与只读 Runtime 投影源。
func NewServiceAPIService(
	owner *ServiceManagementService,
	policy tcpport.Policy,
	routeNotifier RouteReconcileNotifier,
	runtime ServiceRuntimeOwner,
	limits ServiceLimitOwner,
	applyFailures ServiceApplyFailureOwner,
) *ServiceAPIService {
	return &ServiceAPIService{
		owner: owner, policy: policy, routeNotifier: routeNotifier,
		runtime: runtime, limits: limits, applyFailures: applyFailures, now: time.Now,
	}
}

// Create 原子创建 Service 与唯一 Exposure，并各推进一次 Tunnel Revision 和 Route Generation。
func (service *ServiceAPIService) Create(ctx context.Context, input CreateServiceAPIInput) (ServiceAPIMutationResult, error) {
	if !service.validMutation(ctx) || !validTunnelMutationInput(input.Service.TunnelID, input.Service.ExpectedTunnelVersion) {
		return ServiceAPIMutationResult{}, ErrServiceManagementInput
	}
	serviceID, err := service.owner.newServiceID()
	if err != nil {
		return ServiceAPIMutationResult{}, fmt.Errorf("generate service identifier: %w", err)
	}
	now, err := service.timestamp()
	if err != nil {
		return ServiceAPIMutationResult{}, err
	}
	candidate, err := newServiceCandidate(serviceID, input.Service, now)
	if err != nil {
		return ServiceAPIMutationResult{}, err
	}

	unlock := service.owner.lockTunnelMutation(input.Service.TunnelID)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	var result ServiceAPIMutationResult
	var reservation *healthbudget.ConfigurationLease
	defer releaseServiceReservation(&reservation)
	transactionErr := service.owner.store.WithTx(ctx, func(transaction repository.TxStore) error {
		tunnel, err := loadServiceMutationTunnel(ctx, transaction, input.Service.TunnelID, input.Service.ExpectedTunnelVersion)
		if err != nil {
			return err
		}
		nextRevision, err := nextServiceRevision(tunnel.DesiredRevision)
		if err != nil {
			return err
		}
		candidate.RequiredRevision = nextRevision
		services, err := transaction.Services().ListByTunnel(ctx, input.Service.TunnelID)
		if err != nil {
			return fmt.Errorf("list service snapshot candidate: %w", err)
		}
		services = append(services, candidate)
		if err := service.owner.gate.Validate(input.Service.TunnelID, nextRevision, services); err != nil {
			return fmt.Errorf("validate service snapshot candidate: %w", err)
		}
		reservation, err = service.owner.budget.ReserveConfiguration(
			input.Service.TunnelID, uint64(nextRevision), healthEnabledServiceCount(services),
		)
		if err != nil {
			return fmt.Errorf("reserve health target budget: %w", err)
		}
		state, err := transaction.Routes().LoadDesiredState(ctx)
		if err != nil {
			return fmt.Errorf("load route candidate: %w", err)
		}
		exposure, err := service.newExposure(input.Exposure, serviceID, now, state)
		if err != nil {
			return err
		}
		if err := transaction.Services().Create(ctx, candidate); err != nil {
			return fmt.Errorf("create service: %w", err)
		}
		if err := createServiceExposure(ctx, transaction.Routes(), exposure); err != nil {
			return err
		}
		updatedTunnel, err := transaction.Tunnels().AdvanceDesiredRevision(
			ctx, input.Service.TunnelID, input.Service.ExpectedTunnelVersion, tunnel.DesiredRevision, now,
		)
		if err != nil {
			return err
		}
		generation, err := transaction.Routes().AdvanceGeneration(ctx, state.Generation)
		if err != nil {
			return err
		}
		result = ServiceAPIMutationResult{
			Service: candidate, Exposure: exposure, TunnelVersion: updatedTunnel.Version,
			TunnelRevision: updatedTunnel.DesiredRevision, Generation: generation, Changed: true,
		}
		return nil
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return ServiceAPIMutationResult{}, transactionErr
	}
	commitServiceReservation(&reservation)
	unlock()
	locked = false
	return service.finishMutation(input.Service.TunnelID, result, true, true, transactionErr)
}

// Update 在锁内应用 Merge Patch。Exposure 切型先删除旧 Route 再创建新 Route，
// 整个过程对其他读写者不可见，且只推进一次聚合版本。
func (service *ServiceAPIService) Update(ctx context.Context, input UpdateServiceAPIInput) (ServiceAPIMutationResult, error) {
	if !service.validMutation(ctx) || !validServiceMutationInput(
		input.TunnelID, input.ServiceID, input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
	) || !serviceUpdateProvided(input) {
		return ServiceAPIMutationResult{}, ErrServiceManagementInput
	}
	unlock := service.owner.lockTunnelMutation(input.TunnelID)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	var result ServiceAPIMutationResult
	var routeChanged bool
	var routeSnapshotChanged bool
	var notifySnapshot bool
	var reservation *healthbudget.ConfigurationLease
	defer releaseServiceReservation(&reservation)
	transactionErr := service.owner.store.WithTx(ctx, func(transaction repository.TxStore) error {
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
		state, err := transaction.Routes().LoadDesiredState(ctx)
		if err != nil {
			return fmt.Errorf("load route candidate: %w", err)
		}
		currentExposure, err := exposureForService(state, input.ServiceID)
		if err != nil {
			return err
		}
		candidate, serviceSnapshotChanged, serviceChanged, err := applyServiceAPIPatch(current, input)
		if err != nil {
			return err
		}
		nextExposure := currentExposure
		if input.ExposureSet {
			nextExposure, routeChanged, err = service.applyExposurePatch(
				currentExposure, input.Exposure, input.ServiceID, state,
				service.timestampValue(),
			)
			if err != nil {
				return err
			}
		}
		changed := serviceChanged || routeChanged
		if !changed {
			result = ServiceAPIMutationResult{
				Service: current, Exposure: currentExposure, TunnelVersion: tunnel.Version,
				TunnelRevision: tunnel.DesiredRevision, Generation: state.Generation,
			}
			return nil
		}
		now, err := service.timestamp()
		if err != nil {
			return err
		}
		candidate.UpdatedAt = now
		snapshotChanged := serviceSnapshotChanged || routeChanged
		// Route Snapshot 同时缓存 Service 的启用状态、Origin、ProxyOptions、
		// RequiredRevision 与 Tunnel DesiredRevision。即使 Exposure 行未变化，
		// Service Snapshot 变化也必须用新 generation 唤醒 Route owner。
		routeSnapshotChanged = snapshotChanged
		if snapshotChanged {
			nextRevision, err := nextServiceRevision(tunnel.DesiredRevision)
			if err != nil {
				return err
			}
			candidate.RequiredRevision = nextRevision
			services, err := transaction.Services().ListByTunnel(ctx, input.TunnelID)
			if err != nil {
				return fmt.Errorf("list service snapshot candidate: %w", err)
			}
			for index := range services {
				if services[index].ID == candidate.ID {
					services[index] = candidate
					break
				}
			}
			if err := service.owner.gate.Validate(input.TunnelID, nextRevision, services); err != nil {
				return fmt.Errorf("validate service snapshot candidate: %w", err)
			}
			reservation, err = service.owner.budget.ReserveConfiguration(
				input.TunnelID, uint64(nextRevision), healthEnabledServiceCount(services),
			)
			if err != nil {
				return fmt.Errorf("reserve health target budget: %w", err)
			}
		}
		updated, err := transaction.Services().Update(ctx, candidate, input.ExpectedServiceVersion)
		if err != nil {
			return err
		}
		if routeChanged {
			if err := replaceServiceExposure(ctx, transaction.Routes(), currentExposure, nextExposure); err != nil {
				return err
			}
		}
		updatedTunnel := tunnel
		if snapshotChanged {
			updatedTunnel, err = transaction.Tunnels().AdvanceDesiredRevision(
				ctx, input.TunnelID, input.ExpectedTunnelVersion, tunnel.DesiredRevision, now,
			)
			if err != nil {
				return err
			}
			notifySnapshot = true
		}
		generation := state.Generation
		if routeSnapshotChanged {
			generation, err = transaction.Routes().AdvanceGeneration(ctx, state.Generation)
			if err != nil {
				return err
			}
		}
		result = ServiceAPIMutationResult{
			Service: updated, Exposure: nextExposure, TunnelVersion: updatedTunnel.Version,
			TunnelRevision: updatedTunnel.DesiredRevision, Generation: generation, Changed: true,
		}
		return nil
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return ServiceAPIMutationResult{}, transactionErr
	}
	if reservation != nil {
		commitServiceReservation(&reservation)
	}
	unlock()
	locked = false
	if !result.Changed {
		return result, transactionErr
	}
	return service.finishMutation(input.TunnelID, result, routeSnapshotChanged, notifySnapshot, transactionErr)
}

// Delete 先删除 Nested Exposure，再删除 Service，避免外键 RESTRICT 与孤立 Route。
func (service *ServiceAPIService) Delete(ctx context.Context, input DeleteServiceInput) (ServiceAPIMutationResult, error) {
	if !service.validMutation(ctx) || !validServiceMutationInput(
		input.TunnelID, input.ServiceID, input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
	) {
		return ServiceAPIMutationResult{}, ErrServiceManagementInput
	}
	unlock := service.owner.lockTunnelMutation(input.TunnelID)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	var result ServiceAPIMutationResult
	var routeChanged bool
	var reservation *healthbudget.ConfigurationLease
	defer releaseServiceReservation(&reservation)
	transactionErr := service.owner.store.WithTx(ctx, func(transaction repository.TxStore) error {
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
		state, err := transaction.Routes().LoadDesiredState(ctx)
		if err != nil {
			return fmt.Errorf("load route candidate: %w", err)
		}
		exposure, err := exposureForService(state, input.ServiceID)
		if err != nil {
			return err
		}
		nextRevision, err := nextServiceRevision(tunnel.DesiredRevision)
		if err != nil {
			return err
		}
		services, err := transaction.Services().ListByTunnel(ctx, input.TunnelID)
		if err != nil {
			return fmt.Errorf("list service snapshot candidate: %w", err)
		}
		services = slices.DeleteFunc(services, func(candidate repository.Service) bool { return candidate.ID == input.ServiceID })
		if err := service.owner.gate.Validate(input.TunnelID, nextRevision, services); err != nil {
			return fmt.Errorf("validate service snapshot candidate: %w", err)
		}
		reservation, err = service.owner.budget.ReserveConfiguration(
			input.TunnelID, uint64(nextRevision), healthEnabledServiceCount(services),
		)
		if err != nil {
			return fmt.Errorf("reserve health target budget: %w", err)
		}
		routeChanged = exposure.HTTP != nil || exposure.TCP != nil
		if routeChanged {
			if err := deleteServiceExposure(ctx, transaction.Routes(), exposure); err != nil {
				return err
			}
		}
		if err := transaction.Services().Delete(ctx, input.TunnelID, input.ServiceID, input.ExpectedServiceVersion); err != nil {
			return err
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
		// 删除 Service 会同时改变 Route Desired State 中的 Service 绑定和 Tunnel
		// DesiredRevision；即使 Exposure 已先被移除，也必须用新 generation 发布完整快照。
		generation, err := transaction.Routes().AdvanceGeneration(ctx, state.Generation)
		if err != nil {
			return err
		}
		result = ServiceAPIMutationResult{
			Service: current, Exposure: exposure, TunnelVersion: updatedTunnel.Version,
			TunnelRevision: updatedTunnel.DesiredRevision, Generation: generation,
			Changed: true, Deleted: true,
		}
		return nil
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return ServiceAPIMutationResult{}, transactionErr
	}
	commitServiceReservation(&reservation)
	unlock()
	locked = false
	return service.finishMutation(input.TunnelID, result, true, true, transactionErr)
}

// Get 按全局 Service ID 从一次一致性 Desired State 读取 Service、Exposure 和父 Tunnel。
func (service *ServiceAPIService) Get(ctx context.Context, serviceID string) (ServiceView, error) {
	if !service.validQuery(ctx) || strings.TrimSpace(serviceID) == "" {
		return ServiceView{}, ErrServiceManagementInput
	}
	views, err := service.readViews(ctx, "", serviceID)
	if err != nil {
		return ServiceView{}, err
	}
	if len(views) != 1 {
		return ServiceView{}, ErrServiceManagementUnavailable
	}
	return views[0], nil
}

// List 返回指定 Tunnel 的 Service，并按 ID 稳定排序。
func (service *ServiceAPIService) List(ctx context.Context, tunnelID string) ([]ServiceView, error) {
	if !service.validQuery(ctx) || strings.TrimSpace(tunnelID) == "" {
		return nil, ErrServiceManagementInput
	}
	return service.readViews(ctx, tunnelID, "")
}

func (service *ServiceAPIService) readViews(ctx context.Context, tunnelID, serviceID string) ([]ServiceView, error) {
	runtimeSnapshots := service.runtime.RuntimeStatusSnapshots()
	limitSnapshot := service.limits.Snapshot()
	var state repository.RouteDesiredState
	if err := service.owner.store.Read(ctx, func(view repository.RepositoryView) error {
		var err error
		state, err = view.Routes().LoadDesiredState(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read service desired state: %w", err)
	}
	tunnelVersions := make(map[string]int64, len(state.Tunnels))
	for _, tunnel := range state.Tunnels {
		tunnelVersions[tunnel.ID] = tunnel.Version
	}
	result := make([]ServiceView, 0)
	for _, record := range state.Services {
		if tunnelID != "" && record.TunnelID != tunnelID || serviceID != "" && record.ID != serviceID {
			continue
		}
		exposure, err := exposureForService(state, record.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, projectService(
			record, exposure, tunnelVersions[record.TunnelID], runtimeSnapshots,
			limitSnapshot, service.applyFailures, service.now().UTC(),
		))
	}
	if serviceID != "" && len(result) == 0 {
		return nil, ErrServiceManagementUnavailable
	}
	if tunnelID != "" {
		if _, exists := tunnelVersions[tunnelID]; !exists {
			return nil, ErrServiceManagementUnavailable
		}
	}
	slices.SortFunc(result, func(left, right ServiceView) int { return strings.Compare(left.Service.ID, right.Service.ID) })
	return result, nil
}

func projectService(
	record repository.Service,
	exposure repository.ServiceExposure,
	tunnelVersion int64,
	runtimeSnapshots []serverruntime.SessionStatusSnapshot,
	limitSnapshot serverlimits.Snapshot,
	applyFailures ServiceApplyFailureOwner,
	now time.Time,
) ServiceView {
	requiredRevision := uint64(record.RequiredRevision)
	connectors := make([]serverstatus.ServiceConnector, 0)
	var healthy uint64
	for _, snapshot := range runtimeSnapshots {
		if snapshot.TunnelID != record.TunnelID {
			continue
		}
		connector := serverstatus.ServiceConnectorFromRuntime(snapshot, record.ID, requiredRevision, now)
		connectors = append(connectors, connector)
		if serviceConnectorHealthy(connector, record.Health != nil, requiredRevision) {
			healthy++
		}
	}
	applyFailure := applyFailures.ServiceApplyFailure(record.ID, requiredRevision)
	active := limitSnapshot.ActiveByService[serverlimits.ConnectionService{TunnelID: record.TunnelID, ServiceID: record.ID}]
	status := serverstatus.CalculateService(serverstatus.ServiceInput{
		Enabled: record.Enabled, RequiredRevision: requiredRevision, HealthEnabled: record.Health != nil,
		ApplyFailure: applyFailure, Connectors: connectors,
	})
	if status != serverstatus.ServiceStatusApplyFailed {
		applyFailure = nil
	}
	return ServiceView{
		Service: record, Exposure: exposure, TunnelVersion: tunnelVersion,
		Status:       status,
		ApplyFailure: applyFailure, HealthyConnectors: healthy, ActiveConnections: active,
	}
}

func serviceConnectorHealthy(connector serverstatus.ServiceConnector, healthEnabled bool, requiredRevision uint64) bool {
	if !connector.Current || connector.Tombstone || !connector.ControlLive || !connector.HeartbeatFresh ||
		!connector.ConfigReady || !connector.HasObserved || connector.ObservedRevision < requiredRevision {
		return false
	}
	return !healthEnabled || connector.HealthRevision == requiredRevision && connector.HealthHealthy && connector.HealthFresh
}

func (service *ServiceAPIService) finishMutation(
	tunnelID string,
	result ServiceAPIMutationResult,
	routeChanged bool,
	snapshotChanged bool,
	transactionErr error,
) (ServiceAPIMutationResult, error) {
	if routeChanged {
		service.routeNotifier.MarkDirty(result.Generation)
	}
	if snapshotChanged {
		if err := service.owner.notifier.MarkDirty(tunnelID); err != nil {
			return result, errors.Join(
				transactionErr,
				fmt.Errorf("%w: mark Tunnel Snapshot dirty: %w", ErrServiceRuntimeConvergence, err),
			)
		}
	}
	return result, transactionErr
}

func (service *ServiceAPIService) validMutation(ctx context.Context) bool {
	return service != nil && ctx != nil && service.owner != nil && service.owner.valid(ctx) &&
		service.routeNotifier != nil && service.now != nil
}

func (service *ServiceAPIService) validQuery(ctx context.Context) bool {
	return service != nil && ctx != nil && service.owner != nil && service.owner.store != nil &&
		service.runtime != nil && service.limits != nil && service.applyFailures != nil && service.now != nil
}

func (service *ServiceAPIService) timestamp() (int64, error) {
	now := service.timestampValue()
	if now <= 0 {
		return 0, ErrServiceManagementInput
	}
	return now, nil
}

func (service *ServiceAPIService) timestampValue() int64 { return service.now().UTC().Unix() }

func releaseServiceReservation(reservation **healthbudget.ConfigurationLease) {
	if *reservation != nil && !(*reservation).Release() {
		panic("health target budget configuration release invariant violated")
	}
}

func commitServiceReservation(reservation **healthbudget.ConfigurationLease) {
	if *reservation == nil || !(*reservation).Commit() {
		panic("health target budget configuration commit invariant violated")
	}
	*reservation = nil
}

func serviceUpdateProvided(input UpdateServiceAPIInput) bool {
	return input.Name != nil || input.Origin != nil || input.ProxyOptions != nil || input.HealthSet || input.ExposureSet || input.Enabled != nil
}

func applyServiceAPIPatch(current repository.Service, input UpdateServiceAPIInput) (repository.Service, bool, bool, error) {
	candidate := current
	if input.Name != nil {
		candidate.Name = *input.Name
	}
	if input.Origin != nil {
		if err := applyOriginPatch(&candidate, *input.Origin); err != nil {
			return repository.Service{}, false, false, err
		}
	}
	if input.ProxyOptions != nil {
		applyProxyOptionsPatch(&candidate, *input.ProxyOptions)
	}
	if input.HealthSet {
		var err error
		candidate.Health, err = applyHealthPatch(candidate.Health, input.Health)
		if err != nil {
			return repository.Service{}, false, false, err
		}
	}
	if input.Enabled != nil {
		candidate.Enabled = *input.Enabled
	}
	if err := candidate.Validate(); err != nil {
		return repository.Service{}, false, false, fmt.Errorf("%w: service fields", ErrServiceManagementInput)
	}
	snapshotChanged := !sameServiceSnapshot(current, candidate)
	return candidate, snapshotChanged, snapshotChanged || current.Name != candidate.Name, nil
}

func applyOriginPatch(candidate *repository.Service, patch ServiceOriginPatchInput) error {
	targetScheme := candidate.OriginScheme
	if patch.Scheme != nil {
		targetScheme = *patch.Scheme
	}
	switch targetScheme {
	case repository.OriginSchemeHTTP:
		if patch.TLSVerify != nil || patch.TLSServerName != nil {
			return fmt.Errorf("%w: TLS fields require HTTPS origin", ErrServiceManagementInput)
		}
	case repository.OriginSchemeHTTPS:
	case repository.OriginSchemeTCP:
		if patch.TLSVerify != nil || patch.TLSServerName != nil || patch.HTTPHost != nil {
			return fmt.Errorf("%w: HTTP and TLS fields do not apply to TCP origin", ErrServiceManagementInput)
		}
	default:
		return fmt.Errorf("%w: origin scheme", ErrServiceManagementInput)
	}
	if patch.Scheme != nil && *patch.Scheme != candidate.OriginScheme {
		candidate.OriginScheme = *patch.Scheme
		candidate.TLSVerify = *patch.Scheme == repository.OriginSchemeHTTPS
		candidate.TLSServerName = ""
		candidate.OriginHTTPHost = ""
		candidate.ProxyOptions = (repository.ServiceProxyOptions{}).WithDefaults()
	}
	if patch.Host != nil {
		candidate.OriginHost = *patch.Host
	}
	if patch.Port != nil {
		candidate.OriginPort = *patch.Port
	}
	if patch.ConnectTimeoutMS != nil {
		candidate.ConnectTimeoutMS = *patch.ConnectTimeoutMS
	}
	if patch.TLSVerify != nil {
		candidate.TLSVerify = *patch.TLSVerify
	}
	if patch.TLSServerName != nil {
		candidate.TLSServerName = *patch.TLSServerName
	}
	if patch.HTTPHost != nil {
		candidate.OriginHTTPHost = *patch.HTTPHost
	}
	if candidate.OriginScheme != repository.OriginSchemeHTTPS {
		candidate.TLSVerify = false
		candidate.TLSServerName = ""
	}
	if candidate.OriginScheme == repository.OriginSchemeTCP {
		candidate.OriginHTTPHost = ""
	}
	return nil
}

func applyProxyOptionsPatch(candidate *repository.Service, patch ServiceProxyOptionsPatchInput) {
	options := candidate.ProxyOptions.WithDefaults()
	if patch.DisableChunkedEncoding != nil {
		options.DisableChunkedEncoding = *patch.DisableChunkedEncoding
	}
	if patch.DisableHappyEyeballs != nil {
		options.DisableHappyEyeballs = *patch.DisableHappyEyeballs
	}
	if patch.IdleConnectionTimeoutMS != nil {
		options.HTTPIdleConnectionTimeoutMS = *patch.IdleConnectionTimeoutMS
	}
	if patch.MaxIdleConnections != nil {
		options.HTTPMaxIdleConnections = *patch.MaxIdleConnections
	}
	if patch.TCPKeepAliveIntervalMS != nil {
		options.TCPKeepAliveIntervalMS = *patch.TCPKeepAliveIntervalMS
	}
	candidate.ProxyOptions = options
}

func applyHealthPatch(current *repository.HealthCheck, patch *ServiceHealthPatchInput) (*repository.HealthCheck, error) {
	if patch == nil {
		return nil, nil
	}
	var candidate repository.HealthCheck
	if current != nil {
		candidate = *current
	}
	if patch.Type != nil && (current == nil || *patch.Type != candidate.Type) {
		defaults, err := serviceHealth(&ServiceHealthInput{Type: *patch.Type})
		if err != nil {
			return nil, err
		}
		candidate = *defaults
	} else if current == nil {
		return nil, fmt.Errorf("%w: health type is required", ErrServiceManagementInput)
	}
	if patch.Path != nil {
		candidate.Path = *patch.Path
	}
	if patch.IntervalMS != nil {
		candidate.IntervalMS = *patch.IntervalMS
	}
	if patch.TimeoutMS != nil {
		candidate.TimeoutMS = *patch.TimeoutMS
	}
	if patch.ExpectedStatusMin != nil {
		candidate.ExpectedStatusMin = *patch.ExpectedStatusMin
	}
	if patch.ExpectedStatusMax != nil {
		candidate.ExpectedStatusMax = *patch.ExpectedStatusMax
	}
	if patch.FailureThreshold != nil {
		candidate.FailureThreshold = *patch.FailureThreshold
	}
	if patch.SuccessThreshold != nil {
		candidate.SuccessThreshold = *patch.SuccessThreshold
	}
	if candidate.Type == repository.HealthTypeTCP {
		candidate.Path = ""
		candidate.ExpectedStatusMin = 0
		candidate.ExpectedStatusMax = 0
	}
	return &candidate, nil
}

func (service *ServiceAPIService) newExposure(
	input ServiceExposureInput,
	serviceID string,
	now int64,
	state repository.RouteDesiredState,
) (repository.ServiceExposure, error) {
	switch input.Type {
	case ServiceExposureHTTP:
		if input.PublicPort != nil {
			return repository.ServiceExposure{}, fmt.Errorf("%w: HTTP exposure contains TCP fields", ErrServiceManagementInput)
		}
		hostname, path, err := canonicalHTTPExposure(input.Hostname, stringValue(input.PathPrefix, "/"))
		if err != nil {
			return repository.ServiceExposure{}, err
		}
		if err := ensureHTTPExposureAvailable(state.HTTPRoutes, hostname, path, ""); err != nil {
			return repository.ServiceExposure{}, err
		}
		route := repository.HTTPRoute{
			ID: serviceID, ServiceID: serviceID, Hostname: hostname, PathPrefix: path,
			PreserveHost: boolValue(input.PreserveHost, true), Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		}
		return repository.ServiceExposure{HTTP: &route}, nil
	case ServiceExposureTCP:
		if input.Hostname != "" || input.PathPrefix != nil || input.PreserveHost != nil {
			return repository.ServiceExposure{}, fmt.Errorf("%w: TCP exposure contains HTTP fields", ErrServiceManagementInput)
		}
		requested := uint16Value(input.PublicPort)
		port, err := service.resolvePort(requested, state.TCPRoutes, "")
		if err != nil {
			return repository.ServiceExposure{}, err
		}
		route := repository.TCPRoute{
			ID: serviceID, ServiceID: serviceID, PublicPort: port, Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		}
		return repository.ServiceExposure{TCP: &route}, nil
	default:
		return repository.ServiceExposure{}, fmt.Errorf("%w: exposure type", ErrServiceManagementInput)
	}
}

func (service *ServiceAPIService) applyExposurePatch(
	current repository.ServiceExposure,
	patch *ServiceExposurePatchInput,
	serviceID string,
	state repository.RouteDesiredState,
	now int64,
) (repository.ServiceExposure, bool, error) {
	if patch == nil {
		return repository.ServiceExposure{}, current.HTTP != nil || current.TCP != nil, nil
	}
	currentType := ServiceExposureType("")
	if current.HTTP != nil {
		currentType = ServiceExposureHTTP
	} else if current.TCP != nil {
		currentType = ServiceExposureTCP
	}
	targetType := currentType
	if patch.Type != nil {
		targetType = *patch.Type
	}
	if targetType == "" {
		return repository.ServiceExposure{}, false, fmt.Errorf("%w: exposure type is required", ErrServiceManagementInput)
	}
	if targetType != currentType {
		input := ServiceExposureInput{Type: targetType}
		if patch.Hostname != nil {
			input.Hostname = *patch.Hostname
		}
		input.PathPrefix = patch.PathPrefix
		input.PreserveHost = patch.PreserveHost
		input.PublicPort = patch.PublicPort
		next, err := service.newExposure(input, serviceID, now, state)
		return next, err == nil, err
	}
	switch targetType {
	case ServiceExposureHTTP:
		if patch.PublicPort != nil || current.HTTP == nil {
			return repository.ServiceExposure{}, false, fmt.Errorf("%w: HTTP exposure fields", ErrServiceManagementInput)
		}
		route := *current.HTTP
		if patch.Hostname != nil {
			route.Hostname = *patch.Hostname
		}
		if patch.PathPrefix != nil {
			route.PathPrefix = *patch.PathPrefix
		}
		if patch.PreserveHost != nil {
			route.PreserveHost = *patch.PreserveHost
		}
		hostname, path, err := canonicalHTTPExposure(route.Hostname, route.PathPrefix)
		if err != nil {
			return repository.ServiceExposure{}, false, err
		}
		if err := ensureHTTPExposureAvailable(state.HTTPRoutes, hostname, path, route.ID); err != nil {
			return repository.ServiceExposure{}, false, err
		}
		route.Hostname, route.PathPrefix, route.Enabled = hostname, path, true
		changed := *current.HTTP != route
		if changed {
			route.UpdatedAt = now
		}
		return repository.ServiceExposure{HTTP: &route}, changed, nil
	case ServiceExposureTCP:
		if patch.Hostname != nil || patch.PathPrefix != nil || patch.PreserveHost != nil || current.TCP == nil {
			return repository.ServiceExposure{}, false, fmt.Errorf("%w: TCP exposure fields", ErrServiceManagementInput)
		}
		route := *current.TCP
		if patch.PublicPort != nil {
			port, err := service.resolvePort(*patch.PublicPort, state.TCPRoutes, route.ID)
			if err != nil {
				return repository.ServiceExposure{}, false, err
			}
			route.PublicPort = port
		}
		route.Enabled = true
		changed := *current.TCP != route
		if changed {
			route.UpdatedAt = now
		}
		return repository.ServiceExposure{TCP: &route}, changed, nil
	default:
		return repository.ServiceExposure{}, false, fmt.Errorf("%w: exposure type", ErrServiceManagementInput)
	}
}

func (service *ServiceAPIService) resolvePort(requested uint16, routes []repository.TCPRoute, excludeID string) (uint16, error) {
	used := make(map[uint16]struct{}, len(routes))
	for _, route := range routes {
		if route.ID != excludeID {
			used[route.PublicPort] = struct{}{}
		}
	}
	if requested == 0 {
		port, err := service.policy.Allocate(used)
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrRouteManagementInput, err)
		}
		return port, nil
	}
	if err := service.policy.ValidateExplicit(requested, used); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrRouteManagementInput, err)
	}
	return requested, nil
}

func exposureForService(state repository.RouteDesiredState, serviceID string) (repository.ServiceExposure, error) {
	var result repository.ServiceExposure
	for index := range state.HTTPRoutes {
		if state.HTTPRoutes[index].ServiceID == serviceID {
			if result.HTTP != nil || result.TCP != nil {
				return repository.ServiceExposure{}, fmt.Errorf("load service exposure: %w", repository.ErrInvalidRoute)
			}
			route := state.HTTPRoutes[index]
			result.HTTP = &route
		}
	}
	for index := range state.TCPRoutes {
		if state.TCPRoutes[index].ServiceID == serviceID {
			if result.HTTP != nil || result.TCP != nil {
				return repository.ServiceExposure{}, fmt.Errorf("load service exposure: %w", repository.ErrInvalidRoute)
			}
			route := state.TCPRoutes[index]
			result.TCP = &route
		}
	}
	return result, nil
}

func canonicalHTTPExposure(hostname, path string) (string, string, error) {
	canonicalHostname, err := serverroute.CanonicalHostname(hostname)
	if err != nil {
		return "", "", fmt.Errorf("%w: exposure hostname: %w", ErrServiceManagementInput, err)
	}
	canonicalPath, err := serverroute.CanonicalPathPrefix(path)
	if err != nil {
		return "", "", fmt.Errorf("%w: exposure path: %w", ErrServiceManagementInput, err)
	}
	return canonicalHostname, canonicalPath, nil
}

func ensureHTTPExposureAvailable(routes []repository.HTTPRoute, hostname, path, excludeID string) error {
	for _, route := range routes {
		if route.ID != excludeID && route.Hostname == hostname && route.PathPrefix == path {
			return ErrServiceExposureConflict
		}
	}
	return nil
}

func createServiceExposure(ctx context.Context, routes repository.RouteRepository, exposure repository.ServiceExposure) error {
	switch {
	case exposure.HTTP != nil:
		if err := routes.CreateHTTP(ctx, *exposure.HTTP); err != nil {
			return fmt.Errorf("create HTTP exposure: %w", err)
		}
	case exposure.TCP != nil:
		if err := routes.CreateTCP(ctx, *exposure.TCP); err != nil {
			return fmt.Errorf("create TCP exposure: %w", err)
		}
	default:
		return fmt.Errorf("%w: exposure is required", ErrServiceManagementInput)
	}
	return nil
}

func deleteServiceExposure(ctx context.Context, routes repository.RouteRepository, exposure repository.ServiceExposure) error {
	switch {
	case exposure.HTTP != nil:
		if err := routes.DeleteHTTP(ctx, exposure.HTTP.ID); err != nil {
			return fmt.Errorf("delete HTTP exposure: %w", err)
		}
	case exposure.TCP != nil:
		if err := routes.DeleteTCP(ctx, exposure.TCP.ID); err != nil {
			return fmt.Errorf("delete TCP exposure: %w", err)
		}
	}
	return nil
}

func replaceServiceExposure(
	ctx context.Context,
	routes repository.RouteRepository,
	current repository.ServiceExposure,
	next repository.ServiceExposure,
) error {
	if current.HTTP != nil && next.HTTP != nil {
		if err := routes.UpdateHTTP(ctx, *next.HTTP); err != nil {
			return fmt.Errorf("update HTTP exposure: %w", err)
		}
		return nil
	}
	if current.TCP != nil && next.TCP != nil {
		if err := routes.UpdateTCP(ctx, *next.TCP); err != nil {
			return fmt.Errorf("update TCP exposure: %w", err)
		}
		return nil
	}
	if err := deleteServiceExposure(ctx, routes, current); err != nil {
		return err
	}
	if next.HTTP == nil && next.TCP == nil {
		return nil
	}
	return createServiceExposure(ctx, routes, next)
}

func uint16Value(value *uint16) uint16 {
	if value == nil {
		return 0
	}
	return *value
}
