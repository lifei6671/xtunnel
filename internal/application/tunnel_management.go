package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	serverstatus "github.com/lifei6671/xtunnel/internal/server/status"
)

const maximumTunnelInUseReferences = 200

var (
	// ErrTunnelManagementInput 表示 Tunnel 管理输入不满足领域约束。
	ErrTunnelManagementInput = errors.New("tunnel management input is invalid")
	// ErrTunnelManagementUnavailable 表示目标 Tunnel 不存在。
	ErrTunnelManagementUnavailable = errors.New("tunnel management resource is unavailable")
	// ErrTunnelManagementInUse 表示 Service 引用阻止 Tunnel 删除。
	ErrTunnelManagementInUse = errors.New("tunnel is referenced by services")
	// ErrTunnelManagementLimit 表示持久化 Tunnel 已达到配置硬上限。
	ErrTunnelManagementLimit = errors.New("tunnel limit reached")
	// ErrTunnelManagementRuntimeInitialization 表示创建已提交但 Health Budget 基线注册失败。
	ErrTunnelManagementRuntimeInitialization = errors.New("tunnel creation committed but runtime initialization failed")
	// ErrTunnelManagementRuntimeConvergence 表示删除已提交但 Runtime 清理失败。
	ErrTunnelManagementRuntimeConvergence = errors.New("tunnel deletion committed but runtime convergence failed")
)

// TunnelRuntimeOwner 只暴露 Management 查询和删除收敛需要的值型快照。
// 两类快照各自经过 generation fence，但不承诺跨方法原子一致；投影允许在并发
// replacement/revoke 时短暂最终一致。实现必须在返回前释放内部锁，Application
// 不会在 Runtime owner 锁内访问 SQLite。
type TunnelRuntimeOwner interface {
	RuntimeStatusSnapshots() []serverruntime.SessionStatusSnapshot
	ConnectorSnapshots() []serverruntime.ConnectorSnapshot
	DeleteTunnel(string) error
}

// TunnelHealthBudgetOwner 接收新建 Tunnel 的空配置基线，使首次 Service 变更与
// Connector Auth 不必等待进程重启后再由启动快照补注册。
type TunnelHealthBudgetOwner interface {
	InitializeTunnel(tunnelID string, revision, enabledCount uint64) error
}

// CreateTunnelInput 是公开 Create Tunnel 唯一允许提供的业务字段。
type CreateTunnelInput struct {
	Name string
}

// CreateTunnelResult 同时返回原子提交的 Tunnel 与首代 Credential。
type CreateTunnelResult struct {
	Tunnel     repository.Tunnel
	Credential ConnectionTokenResult
}

// UpdateTunnelInput 使用 Tunnel aggregate version 更新展示名称。
type UpdateTunnelInput struct {
	TunnelID        string
	ExpectedVersion int64
	Name            string
}

// DeleteTunnelInput 使用 Tunnel aggregate version 删除无引用 Tunnel。
type DeleteTunnelInput struct {
	TunnelID        string
	ExpectedVersion int64
}

// DeleteTunnelResult 显式保留已经提交的删除事实，供提交后清理错误使用。
type DeleteTunnelResult struct {
	TunnelID string
	Deleted  bool
}

// TunnelView 是持久化事实与两类独立 Runtime 快照在线性化边界外形成的只读投影。
// 两次快照之间只保证短暂最终一致，不能据此推导跨 owner 的原子运行态。
// 状态始终由 server/status 计算，Handler 只负责转换成 OpenAPI DTO。
type TunnelView struct {
	Tunnel            repository.Tunnel
	Status            serverstatus.TunnelStatus
	ConnectorsOnline  uint64
	ServicesCount     int64
	ActiveConnections uint64
	LastSeenAt        *time.Time
}

// ConnectorView 只描述当前、心跳新鲜的 Control Session。断连或过期项不会被
// 伪装为 OFFLINE；Connector 本身不进入 SQLite。
type ConnectorView struct {
	ID                  string
	TunnelID            string
	Hostname            string
	OS                  string
	Arch                string
	Version             string
	Status              serverstatus.ConnectorStatus
	IdleWorkConnections uint32
	ActiveConnections   uint32
	ConnectedAt         time.Time
	LastHeartbeatAt     time.Time
	ConfigReady         bool
	ObservedRevision    uint64
}

// TunnelInUseError 返回有界且稳定排序的 Service 引用，避免错误响应无限增长。
type TunnelInUseError struct {
	ServiceCount          int64
	ReferencingServiceIDs []string
	ReferencesTruncated   bool
}

func (err *TunnelInUseError) Error() string { return ErrTunnelManagementInUse.Error() }
func (err *TunnelInUseError) Unwrap() error { return ErrTunnelManagementInUse }

// TunnelManagementService 是 Tunnel CRUD 与只读状态投影的唯一 Application owner。
// Create 把 Tunnel 与首代 Credential 放入同一事务；Delete 在同一写事务检查引用。
type TunnelManagementService struct {
	store        repository.Store
	tokens       *ConnectionTokenService
	runtime      TunnelRuntimeOwner
	healthBudget TunnelHealthBudgetOwner
	endpoint     *protocolv1.GatewayEndpoint
	tlsTrust     *protocolv1.TlsTrustDescriptor
	maxTunnels   int
	newTunnelID  func() (string, error)
	now          func() time.Time
}

// NewTunnelManagementService 绑定唯一 Store、Runtime、Health Budget、Gateway 描述和 Tunnel 硬上限。
func NewTunnelManagementService(
	store repository.Store,
	tokens *ConnectionTokenService,
	runtime TunnelRuntimeOwner,
	healthBudget TunnelHealthBudgetOwner,
	endpoint *protocolv1.GatewayEndpoint,
	tlsTrust *protocolv1.TlsTrustDescriptor,
	maxTunnels int,
) *TunnelManagementService {
	return &TunnelManagementService{
		store: store, tokens: tokens, runtime: runtime, healthBudget: healthBudget, endpoint: endpoint, tlsTrust: tlsTrust,
		maxTunnels: maxTunnels, newTunnelID: identity.NewTunnelID, now: time.Now,
	}
}

// Create 在事务外生成和加密 Secret，再在一个写事务中同时持久化 Tunnel 与 Token。
func (service *TunnelManagementService) Create(ctx context.Context, input CreateTunnelInput) (CreateTunnelResult, error) {
	if !service.valid(ctx) || strings.TrimSpace(input.Name) == "" {
		return CreateTunnelResult{}, ErrTunnelManagementInput
	}
	tunnelID, err := service.newTunnelID()
	if err != nil {
		return CreateTunnelResult{}, fmt.Errorf("generate tunnel identifier: %w", err)
	}
	createdAt := service.now().UTC().Unix()
	if createdAt <= 0 {
		return CreateTunnelResult{}, ErrTunnelManagementInput
	}
	tunnelRecord := repository.Tunnel{
		ID: tunnelID, Name: input.Name, Version: 1, DesiredRevision: 0,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	if err := tunnelRecord.Validate(); err != nil {
		return CreateTunnelResult{}, ErrTunnelManagementInput
	}
	prepared, err := service.tokens.prepareIssue(IssueConnectionTokenInput{
		TunnelID: tunnelID, Endpoint: service.endpoint, TLSTrust: service.tlsTrust,
	})
	if err != nil {
		return CreateTunnelResult{}, err
	}
	defer clear(prepared.metadata.TokenCiphertext)
	result := CreateTunnelResult{Tunnel: tunnelRecord, Credential: prepared.result}
	transactionErr := service.store.WithTx(ctx, func(transaction repository.TxStore) error {
		existing, err := transaction.Tunnels().Count(ctx)
		if err != nil {
			return fmt.Errorf("count tunnels before create: %w", err)
		}
		if existing >= int64(service.maxTunnels) {
			return ErrTunnelManagementLimit
		}
		if err := transaction.Tunnels().Create(ctx, tunnelRecord); err != nil {
			return fmt.Errorf("create tunnel: %w", err)
		}
		return service.tokens.persistPrepared(ctx, transaction, prepared)
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return CreateTunnelResult{}, transactionErr
	}
	// SQLite 已提交后再发布进程内空基线。这样 Service 首次把 Desired Revision 从
	// 0 推到 1 时可以立即预留预算；存量 Tunnel 仍由启动快照负责初始化。
	if err := service.healthBudget.InitializeTunnel(tunnelID, 0, 0); err != nil {
		return result, errors.Join(
			transactionErr,
			ErrTunnelManagementRuntimeInitialization,
			fmt.Errorf("initialize created tunnel health budget: %w", err),
		)
	}
	return result, transactionErr
}

// Get 从一个持久化读视图和一组 Runtime 快照构造 Tunnel 投影。
func (service *TunnelManagementService) Get(ctx context.Context, tunnelID string) (TunnelView, error) {
	if !service.valid(ctx) || !validate.ValidID(tunnelID, "tun_") {
		return TunnelView{}, ErrTunnelManagementInput
	}
	statusSnapshots := service.runtime.RuntimeStatusSnapshots()
	connectorSnapshots := service.runtime.ConnectorSnapshots()
	var tunnelRecord repository.Tunnel
	var servicesCount int64
	if err := service.store.Read(ctx, func(view repository.RepositoryView) error {
		var err error
		tunnelRecord, err = view.Tunnels().Get(ctx, tunnelID)
		if err != nil {
			return err
		}
		servicesCount, err = view.Services().CountByTunnel(ctx, tunnelID)
		return err
	}); err != nil {
		return TunnelView{}, tunnelManagementReadError("get tunnel", err)
	}
	return projectTunnel(tunnelRecord, servicesCount, statusSnapshots, connectorSnapshots), nil
}

// List 一次获取 Runtime 快照并投影全部 Tunnel，避免逐 Tunnel 重复读取 owner。
func (service *TunnelManagementService) List(ctx context.Context) ([]TunnelView, error) {
	if !service.valid(ctx) {
		return nil, ErrTunnelManagementInput
	}
	statusSnapshots := service.runtime.RuntimeStatusSnapshots()
	connectorSnapshots := service.runtime.ConnectorSnapshots()
	var tunnelRecords []repository.Tunnel
	serviceCounts := make(map[string]int64)
	if err := service.store.Read(ctx, func(view repository.RepositoryView) error {
		var err error
		tunnelRecords, err = view.Tunnels().List(ctx)
		if err != nil {
			return err
		}
		for _, tunnelRecord := range tunnelRecords {
			count, err := view.Services().CountByTunnel(ctx, tunnelRecord.ID)
			if err != nil {
				return err
			}
			serviceCounts[tunnelRecord.ID] = count
		}
		return nil
	}); err != nil {
		return nil, tunnelManagementReadError("list tunnels", err)
	}
	result := make([]TunnelView, 0, len(tunnelRecords))
	for _, tunnelRecord := range tunnelRecords {
		result = append(result, projectTunnel(
			tunnelRecord, serviceCounts[tunnelRecord.ID], statusSnapshots, connectorSnapshots,
		))
	}
	return result, nil
}

// ListConnectors 返回指定 Tunnel 当前可展示的运行态 Connector，并保持 ID 升序。
func (service *TunnelManagementService) ListConnectors(ctx context.Context, tunnelID string) ([]ConnectorView, error) {
	if !service.valid(ctx) || !validate.ValidID(tunnelID, "tun_") {
		return nil, ErrTunnelManagementInput
	}
	if err := service.store.Read(ctx, func(view repository.RepositoryView) error {
		_, err := view.Tunnels().Get(ctx, tunnelID)
		return err
	}); err != nil {
		return nil, tunnelManagementReadError("get connector tunnel", err)
	}
	snapshots := service.runtime.RuntimeStatusSnapshots()
	result := make([]ConnectorView, 0)
	for _, snapshot := range snapshots {
		if snapshot.TunnelID != tunnelID {
			continue
		}
		connectorStatus, visible := serverstatus.CalculateConnector(serverstatus.ConnectorInputFromRuntime(snapshot))
		if !visible {
			continue
		}
		result = append(result, ConnectorView{
			ID: snapshot.ConnectorID, TunnelID: snapshot.TunnelID,
			Hostname: snapshot.Hostname, OS: snapshot.OS, Arch: snapshot.Arch, Version: snapshot.Version,
			Status: connectorStatus, IdleWorkConnections: snapshot.WorkPool.Idle,
			ActiveConnections: snapshot.WorkPool.Active, ConnectedAt: snapshot.ConnectedAt.UTC(),
			LastHeartbeatAt: snapshot.LastHeartbeatAt.UTC(), ConfigReady: snapshot.Config.ConfigReady,
			ObservedRevision: snapshot.Config.ObservedRevision,
		})
	}
	slices.SortFunc(result, func(left, right ConnectorView) int {
		return strings.Compare(left.ID, right.ID)
	})
	return result, nil
}

// Update 原子更新名称并推进 Tunnel aggregate version。
func (service *TunnelManagementService) Update(ctx context.Context, input UpdateTunnelInput) (repository.Tunnel, error) {
	if !service.valid(ctx) || !validate.ValidID(input.TunnelID, "tun_") || input.ExpectedVersion < 1 || strings.TrimSpace(input.Name) == "" {
		return repository.Tunnel{}, ErrTunnelManagementInput
	}
	updatedAt := service.now().UTC().Unix()
	if updatedAt <= 0 {
		return repository.Tunnel{}, ErrTunnelManagementInput
	}
	var result repository.Tunnel
	if err := service.store.WithTx(ctx, func(transaction repository.TxStore) error {
		var err error
		result, err = transaction.Tunnels().UpdateName(ctx, input.TunnelID, input.Name, input.ExpectedVersion, updatedAt)
		return err
	}); err != nil {
		return repository.Tunnel{}, tunnelManagementReadError("update tunnel", err)
	}
	return result, nil
}

// Delete 把版本校验、Service 引用检查和删除放在同一写事务。提交后才收敛
// Runtime；即使清理失败也返回 Deleted=true，调用方不得把已删除状态当作回滚。
func (service *TunnelManagementService) Delete(ctx context.Context, input DeleteTunnelInput) (DeleteTunnelResult, error) {
	if !service.valid(ctx) || !validate.ValidID(input.TunnelID, "tun_") || input.ExpectedVersion < 1 {
		return DeleteTunnelResult{}, ErrTunnelManagementInput
	}
	result := DeleteTunnelResult{TunnelID: input.TunnelID}
	transactionErr := service.store.WithTx(ctx, func(transaction repository.TxStore) error {
		tunnelRecord, err := transaction.Tunnels().Get(ctx, input.TunnelID)
		if err != nil {
			return err
		}
		if tunnelRecord.Version != input.ExpectedVersion {
			return repository.ErrVersionConflict
		}
		count, err := transaction.Services().CountByTunnel(ctx, input.TunnelID)
		if err != nil {
			return err
		}
		if count > 0 {
			services, err := transaction.Services().ListByTunnel(ctx, input.TunnelID)
			if err != nil {
				return err
			}
			limit := min(len(services), maximumTunnelInUseReferences)
			references := make([]string, 0, limit)
			for _, item := range services[:limit] {
				references = append(references, item.ID)
			}
			slices.Sort(references)
			return &TunnelInUseError{
				ServiceCount: count, ReferencingServiceIDs: references,
				ReferencesTruncated: int64(len(references)) < count,
			}
		}
		if err := transaction.Tunnels().Delete(ctx, input.TunnelID, input.ExpectedVersion); err != nil {
			return err
		}
		result.Deleted = true
		return nil
	})
	if transactionErr != nil && !errors.Is(transactionErr, repository.ErrPostCommitCleanup) {
		return DeleteTunnelResult{}, tunnelManagementReadError("delete tunnel", transactionErr)
	}
	// 持久化聚合已删除后必须使用删除专用收敛：它等待 AUTH/startup fence，
	// 清空 Session/Work 后摘除运行态墓碑。永久 Revoke fence 只属于撤销语义。
	runtimeErr := service.runtime.DeleteTunnel(input.TunnelID)
	if runtimeErr != nil {
		return result, errors.Join(
			transactionErr,
			ErrTunnelManagementRuntimeConvergence,
			fmt.Errorf("remove deleted tunnel runtime: %w", runtimeErr),
		)
	}
	return result, transactionErr
}

func (service *TunnelManagementService) valid(ctx context.Context) bool {
	return service != nil && ctx != nil && service.store != nil && service.tokens != nil && service.runtime != nil && service.healthBudget != nil &&
		service.endpoint != nil && service.tlsTrust != nil && service.maxTunnels > 0 && service.newTunnelID != nil && service.now != nil
}

func tunnelManagementReadError(operation string, err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return errors.Join(ErrTunnelManagementUnavailable, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func projectTunnel(
	tunnelRecord repository.Tunnel,
	servicesCount int64,
	statusSnapshots []serverruntime.SessionStatusSnapshot,
	connectorSnapshots []serverruntime.ConnectorSnapshot,
) TunnelView {
	statuses := make([]serverstatus.ConnectorStatus, 0)
	var connectorsOnline uint64
	for _, snapshot := range statusSnapshots {
		if snapshot.TunnelID != tunnelRecord.ID {
			continue
		}
		connectorStatus, visible := serverstatus.CalculateConnector(serverstatus.ConnectorInputFromRuntime(snapshot))
		if !visible {
			continue
		}
		statuses = append(statuses, connectorStatus)
		if connectorStatus == serverstatus.ConnectorStatusOnline {
			connectorsOnline++
		}
	}
	var activeConnections uint64
	var lastSeenAt *time.Time
	for _, snapshot := range connectorSnapshots {
		if snapshot.TunnelID != tunnelRecord.ID {
			continue
		}
		activeConnections += snapshot.ActiveWork
		if !snapshot.LastHeartbeatAt.IsZero() && (lastSeenAt == nil || snapshot.LastHeartbeatAt.After(*lastSeenAt)) {
			value := snapshot.LastHeartbeatAt.UTC()
			lastSeenAt = &value
		}
	}
	return TunnelView{
		Tunnel:           tunnelRecord,
		Status:           serverstatus.CalculateTunnel(serverstatus.TunnelInputFromRepository(tunnelRecord, statuses)),
		ConnectorsOnline: connectorsOnline, ServicesCount: servicesCount,
		ActiveConnections: activeConnections, LastSeenAt: lastSeenAt,
	}
}
