package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/tcpport"
)

var (
	// ErrRouteManagementInput 表示 TCP Route Mutation 输入不完整或端口策略不允许。
	ErrRouteManagementInput = errors.New("route management input is invalid")
	// ErrRouteManagementUnavailable 表示目标 Route 或 Service 不存在。
	ErrRouteManagementUnavailable = errors.New("route management resource is unavailable")
	// ErrRouteRuntimeConvergence 表示 Desired State 已提交但运行态收敛通知失败。
	ErrRouteRuntimeConvergence = errors.New("route mutation committed but runtime convergence failed")
)

// RouteReconcileNotifier 在 Route 事务提交后唤醒唯一 Route Snapshot owner。
type RouteReconcileNotifier interface {
	MarkDirty(generation uint64)
}

// CreateTCPRouteInput 创建一个 TCP Route。PublicPort=0 表示从逻辑端口池自动分配；
// 该未指定态只存在于写入输入，提交到 SQLite 的 Route 始终包含具体端口。
type CreateTCPRouteInput struct {
	ID                     string
	TunnelID               string
	ServiceID              string
	ExpectedTunnelVersion  int64
	ExpectedServiceVersion int64
	PublicPort             uint16
	Enabled                bool
}

// UpdateTCPRouteInput 全量替换 TCP Route 的可变字段。PublicPort=0 表示重新自动分配。
type UpdateTCPRouteInput struct {
	ID                     string
	TunnelID               string
	ServiceID              string
	ExpectedTunnelVersion  int64
	ExpectedServiceVersion int64
	PublicPort             uint16
	Enabled                bool
}

// DeleteTCPRouteInput 使用所属 Service 与 Tunnel 的版本 fencing 删除 Route。
type DeleteTCPRouteInput struct {
	ID                     string
	TunnelID               string
	ServiceID              string
	ExpectedTunnelVersion  int64
	ExpectedServiceVersion int64
}

// TCPRouteMutationResult 返回持久化后的具体端口和新全局 Generation。
type TCPRouteMutationResult struct {
	Route          repository.TCPRoute
	Service        repository.Service
	TunnelVersion  int64
	TunnelRevision int64
	Generation     uint64
	Changed        bool
}

// RouteManagementService 是 TCP Route Desired State 的单事务写入入口。
type RouteManagementService struct {
	owner         *ServiceManagementService
	policy        tcpport.Policy
	routeNotifier RouteReconcileNotifier
	now           func() time.Time
}

// NewRouteManagementService 创建使用系统时钟的生产 Route 写协调器。
func NewRouteManagementService(
	owner *ServiceManagementService,
	policy tcpport.Policy,
	routeNotifier RouteReconcileNotifier,
) *RouteManagementService {
	return &RouteManagementService{
		owner: owner, policy: policy, routeNotifier: routeNotifier, now: time.Now,
	}
}

// CreateTCP 在事务前拒绝可预测的范围、保留端口和重复端口错误，再在
// BEGIN IMMEDIATE 内重做完整裁决并一次提交 Route 与新 Generation。
func (service *RouteManagementService) CreateTCP(
	ctx context.Context,
	input CreateTCPRouteInput,
) (TCPRouteMutationResult, error) {
	if !service.valid(ctx) || strings.TrimSpace(input.ID) == "" || !validServiceMutationInput(
		input.TunnelID, input.ServiceID, input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
	) {
		return TCPRouteMutationResult{}, ErrRouteManagementInput
	}
	unlockMutation := service.owner.lockTunnelMutation(input.TunnelID)
	mutationLocked := true
	defer func() {
		if mutationLocked {
			unlockMutation()
		}
	}()
	if _, err := service.preflightTCP(ctx, input.PublicPort, ""); err != nil {
		return TCPRouteMutationResult{}, err
	}
	now, err := service.timestamp()
	if err != nil {
		return TCPRouteMutationResult{}, err
	}

	var result TCPRouteMutationResult
	var reservation *healthbudget.ConfigurationLease
	defer func() {
		if reservation != nil && !reservation.Release() {
			panic("health target budget configuration release invariant violated")
		}
	}()
	transactionErr := service.owner.store.WithTx(ctx, func(transaction repository.TxStore) error {
		state, err := transaction.Routes().LoadDesiredState(ctx)
		if err != nil {
			return fmt.Errorf("load TCP route candidate: %w", err)
		}
		tunnel, currentService, nextRevision, err := loadRouteMutationAggregates(
			ctx, transaction, input.TunnelID, input.ServiceID,
			input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
		)
		if err != nil {
			return err
		}
		reservation, err = service.reserveRouteSnapshot(
			ctx, transaction, input.TunnelID, currentService, nextRevision,
		)
		if err != nil {
			return err
		}
		port, err := service.resolvePort(input.PublicPort, state.TCPRoutes, "")
		if err != nil {
			return err
		}
		route := repository.TCPRoute{
			ID: input.ID, ServiceID: input.ServiceID, PublicPort: port, Enabled: input.Enabled,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := transaction.Routes().CreateTCP(ctx, route); err != nil {
			return fmt.Errorf("create TCP route: %w", err)
		}
		updatedService, updatedTunnel, err := advanceRouteAggregates(
			ctx, transaction, currentService, tunnel, nextRevision, now,
		)
		if err != nil {
			return err
		}
		generation, err := transaction.Routes().AdvanceGeneration(ctx, state.Generation)
		if err != nil {
			return err
		}
		result = routeMutationResult(route, updatedService, updatedTunnel, generation, true)
		return nil
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return TCPRouteMutationResult{}, transactionErr
	}
	if !reservation.Commit() {
		panic("health target budget configuration commit invariant violated")
	}
	reservation = nil
	unlockMutation()
	mutationLocked = false
	return service.finishMutation(input.TunnelID, result, transactionErr)
}

// UpdateTCP 原子替换 Route；同端口更新由 Listener Manager 复用现有 Socket。
func (service *RouteManagementService) UpdateTCP(
	ctx context.Context,
	input UpdateTCPRouteInput,
) (TCPRouteMutationResult, error) {
	if !service.valid(ctx) || strings.TrimSpace(input.ID) == "" || !validServiceMutationInput(
		input.TunnelID, input.ServiceID, input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
	) {
		return TCPRouteMutationResult{}, ErrRouteManagementInput
	}
	unlockMutation := service.owner.lockTunnelMutation(input.TunnelID)
	mutationLocked := true
	defer func() {
		if mutationLocked {
			unlockMutation()
		}
	}()
	if _, err := service.preflightTCP(ctx, input.PublicPort, input.ID); err != nil {
		return TCPRouteMutationResult{}, err
	}
	now, err := service.timestamp()
	if err != nil {
		return TCPRouteMutationResult{}, err
	}

	var result TCPRouteMutationResult
	var reservation *healthbudget.ConfigurationLease
	defer func() {
		if reservation != nil && !reservation.Release() {
			panic("health target budget configuration release invariant violated")
		}
	}()
	transactionErr := service.owner.store.WithTx(ctx, func(transaction repository.TxStore) error {
		state, err := transaction.Routes().LoadDesiredState(ctx)
		if err != nil {
			return fmt.Errorf("load TCP route candidate: %w", err)
		}
		tunnel, currentService, nextRevision, err := loadRouteMutationAggregates(
			ctx, transaction, input.TunnelID, input.ServiceID,
			input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
		)
		if err != nil {
			return err
		}
		current, exists := desiredTCPRoute(state.TCPRoutes, input.ID)
		if !exists || current.ServiceID != input.ServiceID {
			return ErrRouteManagementUnavailable
		}
		port, err := service.resolvePort(input.PublicPort, state.TCPRoutes, input.ID)
		if err != nil {
			return err
		}
		candidate := current
		candidate.ServiceID = input.ServiceID
		candidate.PublicPort = port
		candidate.Enabled = input.Enabled
		if candidate.ServiceID == current.ServiceID && candidate.PublicPort == current.PublicPort && candidate.Enabled == current.Enabled {
			result = TCPRouteMutationResult{
				Route: current, Service: currentService, TunnelVersion: tunnel.Version,
				TunnelRevision: tunnel.DesiredRevision, Generation: state.Generation, Changed: false,
			}
			return nil
		}
		reservation, err = service.reserveRouteSnapshot(
			ctx, transaction, input.TunnelID, currentService, nextRevision,
		)
		if err != nil {
			return err
		}
		candidate.UpdatedAt = now
		if err := transaction.Routes().UpdateTCP(ctx, candidate); err != nil {
			return fmt.Errorf("update TCP route: %w", err)
		}
		updatedService, updatedTunnel, err := advanceRouteAggregates(
			ctx, transaction, currentService, tunnel, nextRevision, now,
		)
		if err != nil {
			return err
		}
		generation, err := transaction.Routes().AdvanceGeneration(ctx, state.Generation)
		if err != nil {
			return err
		}
		result = routeMutationResult(candidate, updatedService, updatedTunnel, generation, true)
		return nil
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return TCPRouteMutationResult{}, transactionErr
	}
	if !result.Changed {
		unlockMutation()
		mutationLocked = false
		return result, transactionErr
	}
	if !reservation.Commit() {
		panic("health target budget configuration commit invariant violated")
	}
	reservation = nil
	unlockMutation()
	mutationLocked = false
	return service.finishMutation(input.TunnelID, result, transactionErr)
}

// DeleteTCP 删除 Route 后才释放其逻辑端口，并推进一次全局 Generation。
func (service *RouteManagementService) DeleteTCP(
	ctx context.Context,
	input DeleteTCPRouteInput,
) (TCPRouteMutationResult, error) {
	if !service.valid(ctx) || strings.TrimSpace(input.ID) == "" || !validServiceMutationInput(
		input.TunnelID, input.ServiceID, input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
	) {
		return TCPRouteMutationResult{}, ErrRouteManagementInput
	}
	unlockMutation := service.owner.lockTunnelMutation(input.TunnelID)
	mutationLocked := true
	defer func() {
		if mutationLocked {
			unlockMutation()
		}
	}()
	now, err := service.timestamp()
	if err != nil {
		return TCPRouteMutationResult{}, err
	}
	var result TCPRouteMutationResult
	var reservation *healthbudget.ConfigurationLease
	defer func() {
		if reservation != nil && !reservation.Release() {
			panic("health target budget configuration release invariant violated")
		}
	}()
	transactionErr := service.owner.store.WithTx(ctx, func(transaction repository.TxStore) error {
		state, err := transaction.Routes().LoadDesiredState(ctx)
		if err != nil {
			return fmt.Errorf("load TCP route candidate: %w", err)
		}
		tunnel, currentService, nextRevision, err := loadRouteMutationAggregates(
			ctx, transaction, input.TunnelID, input.ServiceID,
			input.ExpectedTunnelVersion, input.ExpectedServiceVersion,
		)
		if err != nil {
			return err
		}
		current, exists := desiredTCPRoute(state.TCPRoutes, input.ID)
		if !exists || current.ServiceID != input.ServiceID {
			return ErrRouteManagementUnavailable
		}
		reservation, err = service.reserveRouteSnapshot(
			ctx, transaction, input.TunnelID, currentService, nextRevision,
		)
		if err != nil {
			return err
		}
		if err := transaction.Routes().DeleteTCP(ctx, input.ID); err != nil {
			return fmt.Errorf("delete TCP route: %w", err)
		}
		updatedService, updatedTunnel, err := advanceRouteAggregates(
			ctx, transaction, currentService, tunnel, nextRevision, now,
		)
		if err != nil {
			return err
		}
		generation, err := transaction.Routes().AdvanceGeneration(ctx, state.Generation)
		if err != nil {
			return err
		}
		result = routeMutationResult(current, updatedService, updatedTunnel, generation, true)
		return nil
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return TCPRouteMutationResult{}, transactionErr
	}
	if !reservation.Commit() {
		panic("health target budget configuration commit invariant violated")
	}
	reservation = nil
	unlockMutation()
	mutationLocked = false
	return service.finishMutation(input.TunnelID, result, transactionErr)
}

func (service *RouteManagementService) valid(ctx context.Context) bool {
	return service != nil && ctx != nil && service.owner != nil && service.owner.valid(ctx) &&
		service.routeNotifier != nil && service.now != nil
}

// preflightTCP 保证当前快照中可预测的端口错误在进入写事务前返回。事务内仍会
// 重做相同检查，防止并发提交绕过唯一性；全局 Generation 只由事务内部推进，
// 不作为互不相关 Route 写入之间的客户端并发锁。
func (service *RouteManagementService) preflightTCP(
	ctx context.Context,
	requestedPort uint16,
	excludeRouteID string,
) (uint16, error) {
	var port uint16
	err := service.owner.store.Read(ctx, func(view repository.RepositoryView) error {
		routes, err := view.Routes().ListTCP(ctx)
		if err != nil {
			return err
		}
		port, err = service.resolvePort(requestedPort, routes, excludeRouteID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return port, nil
}

func (service *RouteManagementService) resolvePort(
	requested uint16,
	routes []repository.TCPRoute,
	excludeRouteID string,
) (uint16, error) {
	used := make(map[uint16]struct{}, len(routes))
	for _, route := range routes {
		if route.ID != excludeRouteID {
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

func (service *RouteManagementService) timestamp() (int64, error) {
	now := service.now().UTC().Unix()
	if now <= 0 {
		return 0, ErrRouteManagementInput
	}
	return now, nil
}

func desiredTCPRoute(routes []repository.TCPRoute, id string) (repository.TCPRoute, bool) {
	for _, route := range routes {
		if route.ID == id {
			return route, true
		}
	}
	return repository.TCPRoute{}, false
}

func loadRouteMutationAggregates(
	ctx context.Context,
	transaction repository.TxStore,
	tunnelID, serviceID string,
	expectedTunnelVersion, expectedServiceVersion int64,
) (repository.Tunnel, repository.Service, int64, error) {
	tunnel, err := loadServiceMutationTunnel(ctx, transaction, tunnelID, expectedTunnelVersion)
	if err != nil {
		if errors.Is(err, ErrServiceManagementUnavailable) {
			return repository.Tunnel{}, repository.Service{}, 0, ErrRouteManagementUnavailable
		}
		return repository.Tunnel{}, repository.Service{}, 0, err
	}
	current, err := loadServiceForMutation(ctx, transaction, tunnelID, serviceID)
	if err != nil {
		if errors.Is(err, ErrServiceManagementUnavailable) {
			return repository.Tunnel{}, repository.Service{}, 0, ErrRouteManagementUnavailable
		}
		return repository.Tunnel{}, repository.Service{}, 0, err
	}
	if current.Version != expectedServiceVersion {
		return repository.Tunnel{}, repository.Service{}, 0, repository.ErrVersionConflict
	}
	nextRevision, err := nextServiceRevision(tunnel.DesiredRevision)
	if err != nil {
		return repository.Tunnel{}, repository.Service{}, 0, err
	}
	return tunnel, current, nextRevision, nil
}

func advanceRouteAggregates(
	ctx context.Context,
	transaction repository.TxStore,
	current repository.Service,
	tunnel repository.Tunnel,
	nextRevision, now int64,
) (repository.Service, repository.Tunnel, error) {
	candidate := current
	candidate.RequiredRevision = nextRevision
	candidate.UpdatedAt = now
	updatedService, err := transaction.Services().Update(ctx, candidate, current.Version)
	if err != nil {
		return repository.Service{}, repository.Tunnel{}, err
	}
	updatedTunnel, err := transaction.Tunnels().AdvanceDesiredRevision(
		ctx, tunnel.ID, tunnel.Version, tunnel.DesiredRevision, now,
	)
	if err != nil {
		return repository.Service{}, repository.Tunnel{}, err
	}
	return updatedService, updatedTunnel, nil
}

func routeMutationResult(
	route repository.TCPRoute,
	service repository.Service,
	tunnel repository.Tunnel,
	generation uint64,
	changed bool,
) TCPRouteMutationResult {
	return TCPRouteMutationResult{
		Route: route, Service: service, TunnelVersion: tunnel.Version,
		TunnelRevision: tunnel.DesiredRevision, Generation: generation, Changed: changed,
	}
}

func (service *RouteManagementService) finishMutation(
	tunnelID string,
	result TCPRouteMutationResult,
	transactionErr error,
) (TCPRouteMutationResult, error) {
	service.routeNotifier.MarkDirty(result.Generation)
	if err := service.owner.notifier.MarkDirty(tunnelID); err != nil {
		return result, errors.Join(
			transactionErr,
			fmt.Errorf("%w: mark Tunnel Snapshot dirty: %w", ErrRouteRuntimeConvergence, err),
		)
	}
	return result, transactionErr
}

func (service *RouteManagementService) reserveRouteSnapshot(
	ctx context.Context,
	transaction repository.TxStore,
	tunnelID string,
	current repository.Service,
	nextRevision int64,
) (*healthbudget.ConfigurationLease, error) {
	services, err := transaction.Services().ListByTunnel(ctx, tunnelID)
	if err != nil {
		return nil, fmt.Errorf("list route snapshot candidate services: %w", err)
	}
	for index := range services {
		if services[index].ID == current.ID {
			services[index].RequiredRevision = nextRevision
			break
		}
	}
	if err := service.owner.gate.Validate(tunnelID, nextRevision, services); err != nil {
		return nil, fmt.Errorf("validate route snapshot candidate: %w", err)
	}
	reservation, err := service.owner.budget.ReserveConfiguration(
		tunnelID, uint64(nextRevision), healthEnabledServiceCount(services),
	)
	if err != nil {
		return nil, fmt.Errorf("reserve route health target budget: %w", err)
	}
	return reservation, nil
}
