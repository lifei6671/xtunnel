// Package connector 装配一个 Agent 进程内唯一的 ephemeral Connector 生命周期。
package connector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	"github.com/lifei6671/xtunnel/internal/agent/open"
	"github.com/lifei6671/xtunnel/internal/agent/reconnect"
	agentsession "github.com/lifei6671/xtunnel/internal/agent/session"
	agentworkpool "github.com/lifei6671/xtunnel/internal/agent/workpool"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/safego"
)

const (
	controlAuthTimeout = 10 * time.Second
	workAuthTimeout    = 10 * time.Second
	openReadTimeout    = 10 * time.Second
	openWriteTimeout   = 10 * time.Second
	originDialTimeout  = 10 * time.Second

	controlHighQueue    = 32
	controlNormalQueue  = 128
	controlInboundQueue = 128
	controlWriteTimeout = 5 * time.Second

	reconnectInitial  = time.Second
	reconnectMaximum  = 30 * time.Second
	reconnectStable   = 30 * time.Second
	reconnectJitter   = 0.20
	agentDrainTimeout = 30 * time.Second
)

var (
	// ErrInvalidConfig 表示 Connector 缺少 Token、身份或 Origin 解析器。
	ErrInvalidConfig = errors.New("agent connector runtime config is invalid")
	// ErrUnsupportedControlMessage 表示 M1 Agent 收到当前方向不应处理的消息。
	ErrUnsupportedControlMessage = errors.New("agent connector received unsupported control message")
	// ErrServiceConfigNotObserved 表示本代 Session 尚未成功 Ack 任何完整 Snapshot。
	ErrServiceConfigNotObserved = errors.New("service configuration has not been observed by connector")
)

// Config 是进程启动时冻结、并在全部重连代次复用的 Connector 输入。
type Config struct {
	ConnectionToken string
	Connector       identity.Connector
	Hostname        string
	Version         string
	OS              string
	Arch            string
	OriginDialer    open.OriginDialer
}

// Runtime 持有一个进程内固定 Connector 身份及其可重连 Control Runner。
type Runtime struct {
	token              string
	origin             open.OriginDialer
	runControlSessions func(context.Context, reconnect.SessionHandler[*agentsession.Session]) error
	newConfigManager   func(context.Context) (*configruntime.Manager, error)
	newWorkPool        func(agentworkpool.Config) (workPool, error)
	newDrainID         func() (string, error)
	drainTimeout       time.Duration

	retiredMu    sync.Mutex
	retiredNext  uint64
	retiredPools map[uint64]retiredPool
	retiredWait  sync.WaitGroup
	retiredErr   error
}

type retiredPool struct {
	cancel context.CancelFunc
}

type workPool interface {
	Start(context.Context) error
	ApplyDemand(*protocolv1.WorkDemand) (agentworkpool.DemandResult, error)
	BeginDrain() error
	CompleteDrain(context.Context) error
	Wait() error
	Done() <-chan struct{}
}

type establishedSession interface {
	Enqueue(*protocolv1.ControlEnvelope) error
	Inbound() <-chan controlsession.Inbound
	Done() <-chan struct{}
}

type snapshotBuilder struct{}

type snapshotCandidate struct{}

type snapshotResources struct{}

// New 创建生产 Connector Runtime，但不会立即建立网络连接。
func New(config Config) (*Runtime, error) {
	if config.ConnectionToken == "" || config.Connector.ID() == "" || config.Hostname == "" ||
		config.Version == "" || config.OS == "" || config.Arch == "" || config.OriginDialer == nil {
		return nil, ErrInvalidConfig
	}
	runner, err := agentsession.NewRunner(agentsession.Config{
		ConnectionToken:  config.ConnectionToken,
		Connector:        config.Connector,
		Hostname:         config.Hostname,
		Version:          config.Version,
		OS:               config.OS,
		Arch:             config.Arch,
		Capabilities:     []string{"tcp"},
		AuthWriteTimeout: controlAuthTimeout,
		AuthReadTimeout:  controlAuthTimeout,
		OwnerOptions: controlsession.Options{
			HighPriorityCapacity: controlHighQueue,
			NormalCapacity:       controlNormalQueue,
			InboundCapacity:      controlInboundQueue,
			WriteTimeout:         controlWriteTimeout,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create connector control runner: %w", err)
	}
	sharedBudget := agentworkpool.NewBudget()
	return &Runtime{
		token:  config.ConnectionToken,
		origin: config.OriginDialer,
		runControlSessions: func(ctx context.Context, handler reconnect.SessionHandler[*agentsession.Session]) error {
			return reconnect.Run(ctx, runner, handler, reconnect.Options{
				InitialBackoff: reconnectInitial,
				MaximumBackoff: reconnectMaximum,
				StableAfter:    reconnectStable,
				JitterFraction: reconnectJitter,
			})
		},
		newConfigManager: func(parent context.Context) (*configruntime.Manager, error) {
			return configruntime.New(parent, configruntime.Config{
				ProtocolVersion:      1,
				MaxServices:          configruntime.MaxServicesPerTunnel,
				MaxSnapshotBytes:     configruntime.MaxSnapshotSize,
				MaxControlFrameBytes: int(frame.MaxControlFrameSize),
				RetireTimeout:        agentDrainTimeout,
				Builder:              snapshotBuilder{},
			})
		},
		newWorkPool: func(config agentworkpool.Config) (workPool, error) {
			return agentworkpool.NewWithBudget(config, sharedBudget)
		},
		newDrainID:   identity.NewDrainID,
		drainTimeout: agentDrainTimeout,
		retiredPools: make(map[uint64]retiredPool),
	}, nil
}

// Run 持续运行同一个 Connector 的多代 Control Session，直到进程 Context 取消，
// 或 Token、Pin、ALPN、协议版本等永久错误要求管理员介入。
func (runtime *Runtime) Run(ctx context.Context) error {
	if runtime == nil || ctx == nil || runtime.runControlSessions == nil || runtime.newConfigManager == nil ||
		runtime.drainTimeout <= 0 {
		return ErrInvalidConfig
	}
	managerParent, cancelManagerParent := context.WithCancel(context.WithoutCancel(ctx))
	manager, err := runtime.newConfigManager(managerParent)
	if err != nil {
		cancelManagerParent()
		return fmt.Errorf("create Agent config runtime: %w", err)
	}
	defer cancelManagerParent()

	shutdownStarted := make(chan time.Time, 1)
	stopShutdownClock := context.AfterFunc(ctx, func() { shutdownStarted <- time.Now() })
	runErr := runtime.runControlSessions(ctx, func(sessionContext context.Context, session *agentsession.Session) error {
		return runtime.handleSession(sessionContext, manager, session)
	})
	closeTimeout := runtime.drainTimeout
	if stopShutdownClock() {
		if ctx.Err() == nil {
			runtime.shutdownRetiredPools()
			return errors.Join(runErr, runtime.retiredError(), closeConfigManager(ctx, manager, closeTimeout))
		}
		shutdownStarted <- time.Now()
	}
	shutdownDeadline := (<-shutdownStarted).Add(runtime.drainTimeout)
	remaining := time.Until(shutdownDeadline)
	if remaining < 0 {
		remaining = 0
	}
	runtime.drainRetiredPools(remaining)
	closeTimeout = time.Until(shutdownDeadline)
	if closeTimeout < 0 {
		closeTimeout = 0
	}
	return errors.Join(runErr, runtime.retiredError(), closeConfigManager(ctx, manager, closeTimeout))
}

func (runtime *Runtime) handleSession(
	ctx context.Context,
	manager *configruntime.Manager,
	session *agentsession.Session,
) (resultErr error) {
	authentication, err := session.WorkAuthSession()
	if err != nil {
		return fmt.Errorf("copy current Work authentication: %w", err)
	}
	heartbeatInterval := authentication.HeartbeatInterval
	configSession, err := manager.NewSession(authentication.TunnelID)
	if err != nil {
		clear(authentication.SessionSecret[:])
		return fmt.Errorf("create Agent config session: %w", err)
	}

	openHandler, err := open.NewHandler(open.Options{
		ReadTimeout: openReadTimeout, WriteTimeout: openWriteTimeout,
		ConnectTimeout: originDialTimeout, Dialer: runtime.origin,
	})
	if err != nil {
		clear(authentication.SessionSecret[:])
		return fmt.Errorf("create Agent OPEN handler: %w", err)
	}
	pool, err := runtime.newWorkPool(agentworkpool.Config{
		ConnectionToken:  runtime.token,
		Session:          authentication,
		SessionDone:      session.Done(),
		Handler:          openHandler,
		AuthWriteTimeout: workAuthTimeout,
		AuthReadTimeout:  workAuthTimeout,
	})
	// WorkPool.New 已取得自己的固定数组副本；本地临时认证副本必须立即擦除。
	clear(authentication.SessionSecret[:])
	if err != nil {
		return fmt.Errorf("create Agent WorkPool: %w", err)
	}

	// WorkPool 与已建立 Control Session 一样不能直接继承进程取消；否则 SIGTERM 会
	// 在 DrainRequest 写出前关闭全部 WorkConn。真正终止由下面的排空流程或 SessionDone 驱动。
	poolContext, cancelPool := context.WithCancel(context.WithoutCancel(ctx))
	if err := pool.Start(poolContext); err != nil {
		cancelPool()
		return fmt.Errorf("start Agent WorkPool: %w", err)
	}
	defer func() {
		// 普通 Control 断开只清理本代非 ACTIVE WorkConn；旧 ACTIVE 必须允许自然
		// 结束，且 Wait 不能阻塞下一代重连。业务错误或进程退出仍强制取消整个 Pool。
		preserveActive := resultErr == nil && ctx.Err() == nil
		if !preserveActive {
			cancelPool()
		}
		if waitErr := pool.Wait(); waitErr != nil && !errors.Is(waitErr, context.Canceled) {
			resultErr = errors.Join(resultErr, fmt.Errorf("wait Agent WorkPool: %w", waitErr))
		}
		if preserveActive {
			// 旧 ACTIVE 不阻塞下一代重连，但必须登记到 Runtime；进程关停时允许其
			// 在同一固定排空窗口内结束，Deadline 后再统一取消并等待 Pool.Done。
			runtime.retainPool(pool, cancelPool)
		}
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	return runtime.runEstablished(ctx, session, configSession, pool, ticker)
}

func (runtime *Runtime) runEstablished(
	ctx context.Context,
	session establishedSession,
	configSession *configruntime.Session,
	pool workPool,
	ticker *time.Ticker,
) error {
	for {
		select {
		case <-ctx.Done():
			return runtime.drain(ctx, session, configSession, pool, ticker)
		case <-session.Done():
			return nil
		case inbound, ok := <-session.Inbound():
			if !ok {
				return nil
			}
			if err := applyInbound(ctx, session, configSession, pool, inbound); err != nil {
				return err
			}
		case now := <-ticker.C:
			if err := session.Enqueue(heartbeatEnvelope(now, observedRevision(configSession))); err != nil {
				// Owner 已经关闭时，Session.Done 可能尚未完成 Secret 清理和广播。
				// 这是普通 Control 代际结束，不得因 select 时序误杀旧 ACTIVE。
				if errors.Is(err, controlsession.ErrOwnerClosed) {
					return nil
				}
				return fmt.Errorf("enqueue Agent heartbeat: %w", err)
			}
		}
	}
}

func (runtime *Runtime) retainPool(pool workPool, cancel context.CancelFunc) {
	if runtime == nil || pool == nil || cancel == nil {
		return
	}
	runtime.retiredMu.Lock()
	if runtime.retiredPools == nil {
		runtime.retiredPools = make(map[uint64]retiredPool)
	}
	runtime.retiredNext++
	id := runtime.retiredNext
	runtime.retiredPools[id] = retiredPool{cancel: cancel}
	runtime.retiredWait.Add(1)
	runtime.retiredMu.Unlock()

	safego.Go(func(err error) {
		cancel()
		runtime.recordRetiredError(fmt.Errorf("observe retired Agent WorkPool: %w", err))
	}, func() {
		runtime.retiredMu.Lock()
		delete(runtime.retiredPools, id)
		runtime.retiredMu.Unlock()
		runtime.retiredWait.Done()
	}, func() {
		<-pool.Done()
		if err := pool.Wait(); errors.Is(err, safego.ErrPanic) {
			runtime.recordRetiredError(err)
		}
	})
}

func (runtime *Runtime) drainRetiredPools(timeout time.Duration) {
	if runtime == nil {
		return
	}
	done := make(chan struct{})
	safego.Go(func(err error) {
		runtime.recordRetiredError(fmt.Errorf("wait for retired Agent WorkPools: %w", err))
	}, func() {
		close(done)
	}, func() {
		runtime.retiredWait.Wait()
	})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		runtime.shutdownRetiredPools()
	}
}

func (runtime *Runtime) recordRetiredError(err error) {
	if err == nil {
		return
	}
	runtime.retiredMu.Lock()
	runtime.retiredErr = errors.Join(runtime.retiredErr, err)
	runtime.retiredMu.Unlock()
}

func (runtime *Runtime) retiredError() error {
	runtime.retiredMu.Lock()
	defer runtime.retiredMu.Unlock()
	return runtime.retiredErr
}

func (runtime *Runtime) shutdownRetiredPools() {
	if runtime == nil {
		return
	}
	runtime.retiredMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(runtime.retiredPools))
	for _, retired := range runtime.retiredPools {
		cancels = append(cancels, retired.cancel)
	}
	runtime.retiredMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	runtime.retiredWait.Wait()
}

func (runtime *Runtime) drain(
	processContext context.Context,
	session establishedSession,
	configSession *configruntime.Session,
	pool workPool,
	ticker *time.Ticker,
) error {
	if err := pool.BeginDrain(); err != nil && !errors.Is(err, agentworkpool.ErrPoolClosed) {
		return errors.Join(processContext.Err(), fmt.Errorf("begin Agent WorkPool drain: %w", err))
	}
	drainID, err := runtime.newDrainID()
	if err != nil {
		return errors.Join(processContext.Err(), fmt.Errorf("generate Agent drain id: %w", err))
	}

	// processContext 已经取消，Deadline 必须从 WithoutCancel 派生并使用本机单调时钟；
	// 禁止与 Server 比较绝对时间，也禁止把用户可变配置引入冻结的 V0.1 基线。
	drainContext, cancelDrain := context.WithTimeout(context.WithoutCancel(processContext), runtime.drainTimeout)
	defer cancelDrain()
	if err := session.Enqueue(drainEnvelope(drainID, runtime.drainTimeout)); err != nil {
		return errors.Join(processContext.Err(), fmt.Errorf("enqueue Agent DrainRequest: %w", err))
	}

	for {
		select {
		case <-drainContext.Done():
			// 外层 defer 会取消 Pool 并 Close socket；这里只返回 Deadline，避免在两处
			// 并发执行强制关闭。仅等待 Context 不足以解除阻塞网络 IO。
			return errors.Join(processContext.Err(), drainContext.Err())
		case <-session.Done():
			return processContext.Err()
		case inbound, ok := <-session.Inbound():
			if !ok {
				return processContext.Err()
			}
			ack := inbound.Envelope.GetDrainAck()
			if ack == nil || ack.GetDrainId() != drainID {
				// 旧代或错误 ID 的 Ack 已由协议 Owner 做过方向/字段校验，但不能完成
				// 本次两阶段握手；其他合法消息继续按既有业务路径消费。
				if err := applyInbound(drainContext, session, configSession, pool, inbound); err != nil {
					return errors.Join(processContext.Err(), err)
				}
				continue
			}
			if err := pool.CompleteDrain(drainContext); err != nil {
				return errors.Join(processContext.Err(), fmt.Errorf("complete Agent WorkPool drain: %w", err))
			}
			return processContext.Err()
		case now := <-ticker.C:
			// Heartbeat 在 DRAINING 仍为合法方向；握手期间继续发送，避免长连接被误判失联。
			if err := session.Enqueue(heartbeatEnvelope(now, observedRevision(configSession))); err != nil {
				return errors.Join(processContext.Err(), fmt.Errorf("enqueue draining heartbeat: %w", err))
			}
		}
	}
}

func drainEnvelope(drainID string, timeout time.Duration) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: 1,
		Payload: &protocolv1.ControlEnvelope_DrainRequest{DrainRequest: &protocolv1.DrainRequest{
			DrainId: drainID, DrainTimeoutMs: uint32(timeout / time.Millisecond),
		}},
	}
}

func applyInbound(
	ctx context.Context,
	session establishedSession,
	configSession *configruntime.Session,
	pool workPool,
	inbound controlsession.Inbound,
) error {
	if inbound.Envelope == nil {
		return ErrUnsupportedControlMessage
	}
	switch payload := inbound.Envelope.GetPayload().(type) {
	case *protocolv1.ControlEnvelope_WorkDemand:
		if _, _, observed := configSession.Observed(); !observed {
			return ErrServiceConfigNotObserved
		}
		if _, err := pool.ApplyDemand(payload.WorkDemand); err != nil {
			return fmt.Errorf("apply WorkDemand: %w", err)
		}
		return nil
	case *protocolv1.ControlEnvelope_ConfigSnapshot:
		if err := configSession.Apply(ctx, payload.ConfigSnapshot, session); err != nil {
			if errors.Is(err, configruntime.ErrConfigRejected) {
				return nil
			}
			return fmt.Errorf("apply Agent config snapshot: %w", err)
		}
		return nil
	case *protocolv1.ControlEnvelope_DrainAck:
		// DrainAck 只确认 Agent 主动发出的关闭请求；基线重连循环会在进程
		// Context 取消后结束，收到 Ack 本身不创建新的网络或状态转换。
		return nil
	case *protocolv1.ControlEnvelope_Error:
		return fmt.Errorf("%w: server error code=%s", ErrUnsupportedControlMessage, payload.Error.GetErrorCode())
	default:
		return fmt.Errorf("%w: %T", ErrUnsupportedControlMessage, payload)
	}
}

func heartbeatEnvelope(now time.Time, observedRevision uint64) *protocolv1.ControlEnvelope {
	timestamp := now.UnixMilli()
	if timestamp < 0 {
		timestamp = 0
	}
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: 1,
		Payload: &protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{
			TimestampMs:      uint64(timestamp),
			ObservedRevision: observedRevision,
		}},
	}
}

func observedRevision(session *configruntime.Session) uint64 {
	revision, _, observed := session.Observed()
	if !observed {
		return 0
	}
	return revision
}

func (snapshotBuilder) Build(ctx context.Context, _ *protocolv1.TunnelSnapshot, _ configruntime.Gate) (configruntime.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return snapshotCandidate{}, nil
}

func (snapshotCandidate) Start(ctx context.Context) error {
	return ctx.Err()
}

func (snapshotCandidate) Abort(context.Context) error {
	return nil
}

func (snapshotCandidate) Runtime() configruntime.Resources {
	return snapshotResources{}
}

func (snapshotResources) Retire(context.Context) error {
	return nil
}

func closeConfigManager(parent context.Context, manager *configruntime.Manager, timeout time.Duration) error {
	closeContext, cancelClose := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancelClose()
	if err := manager.Close(closeContext); err != nil {
		return fmt.Errorf("close Agent config runtime: %w", err)
	}
	return nil
}

// HostConfig 使用进程当前主机与 Go 运行时信息构造无本地状态的生产配置。
// Connector 在此函数每次调用时新建，因此调用方必须只在单个进程生命周期调用一次。
func HostConfig(connectionToken, version string, origin open.OriginDialer) (Config, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return Config{}, fmt.Errorf("read Agent hostname: %w", err)
	}
	connectorIdentity, err := identity.NewConnector()
	if err != nil {
		return Config{}, err
	}
	return Config{
		ConnectionToken: connectionToken,
		Connector:       connectorIdentity,
		Hostname:        hostname,
		Version:         version,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		OriginDialer:    origin,
	}, nil
}

// UnobservedOriginDialer 是 M3 Snapshot 尚未 Apply 时的安全边界。
// 它不会猜测 localhost、环境变量或其他地址，避免把未知 service_id 错接到本机服务。
func UnobservedOriginDialer(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
	return nil, protocolv1.ErrorCode_ERROR_CODE_SERVICE_CONFIG_NOT_OBSERVED, ErrServiceConfigNotObserved
}

// 保证静态函数可直接作为生产 OPEN Handler 的 OriginDialer 使用。
var _ open.OriginDialer = open.OriginDialerFunc(UnobservedOriginDialer)

// 编译期确保生产 WorkPool Handler 仍与 OPEN Handler 契约一致。
var _ agentworkpool.Handler = (*open.Handler)(nil)
