// Package connector 装配一个 Agent 进程内唯一的 ephemeral Connector 生命周期。
package connector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	agenthealth "github.com/lifei6671/xtunnel/internal/agent/health"
	"github.com/lifei6671/xtunnel/internal/agent/open"
	agentorigin "github.com/lifei6671/xtunnel/internal/agent/origin"
	"github.com/lifei6671/xtunnel/internal/agent/reconnect"
	agentsession "github.com/lifei6671/xtunnel/internal/agent/session"
	agentworkpool "github.com/lifei6671/xtunnel/internal/agent/workpool"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/safego"
	"github.com/lifei6671/xtunnel/internal/tracing"
)

const (
	controlAuthTimeout = 10 * time.Second
	workAuthTimeout    = 10 * time.Second
	openReadTimeout    = 10 * time.Second
	openWriteTimeout   = 10 * time.Second
	controlHighQueue   = 32
	// Health Outbox 按 service_id 占槽。重连后的完整恢复必须能一次容纳单 Tunnel
	// 上限内的全部 Service，不能依赖 writeLoop 恰好在多个 Reporter 子批次之间抢先出队。
	controlNormalQueue  = configruntime.MaxServicesPerTunnel
	controlInboundQueue = 128
	controlWriteTimeout = 5 * time.Second

	reconnectInitial  = time.Second
	reconnectMaximum  = 30 * time.Second
	reconnectStable   = 30 * time.Second
	reconnectJitter   = 0.20
	agentDrainTimeout = 30 * time.Second
)

var (
	// ErrInvalidConfig 表示 Connector 缺少 Token 或进程身份。
	ErrInvalidConfig = errors.New("agent connector runtime config is invalid")
	// ErrUnsupportedControlMessage 表示 M1 Agent 收到当前方向不应处理的消息。
	ErrUnsupportedControlMessage = errors.New("agent connector received unsupported control message")
	// ErrServiceConfigNotObserved 表示本代 Session 尚未成功 Ack 任何完整 Snapshot。
	ErrServiceConfigNotObserved = agentorigin.ErrConfigNotObserved
	// ErrHealthStopped 表示中心 Health Scheduler 在 Connector Runtime 结束前意外退出。
	ErrHealthStopped = errors.New("agent health scheduler stopped unexpectedly")
)

// Config 是进程启动时冻结、并在全部重连代次复用的 Connector 输入。
type Config struct {
	ConnectionToken string
	Connector       identity.Connector
	Hostname        string
	Version         string
	OS              string
	Arch            string
	Logger          *slog.Logger
	Tracing         *tracing.Runtime
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
	health             healthRuntime
	logger             *slog.Logger
	tracing            *tracing.Runtime

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
	ReplaceHealth([]*protocolv1.ServiceHealth) error
	EnqueueConfigAckAndReplaceHealth(*protocolv1.ControlEnvelope, []*protocolv1.ServiceHealth) error
	Flush(context.Context) error
	Inbound() <-chan controlsession.Inbound
	Done() <-chan struct{}
}

type healthRuntime interface {
	Start(context.Context) error
	Snapshot() map[string]agenthealth.State
	Changed() <-chan struct{}
	Done() <-chan struct{}
	Err() error
	Shutdown(context.Context) error
}

// New 创建生产 Connector Runtime，但不会立即建立网络连接。
func New(config Config) (*Runtime, error) {
	if config.ConnectionToken == "" || config.Connector.ID() == "" || config.Hostname == "" ||
		config.Version == "" || config.OS == "" || config.Arch == "" || config.Logger == nil {
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
	originResolver := agentorigin.New()
	healthManager := agenthealth.New()
	return &Runtime{
		token:   config.ConnectionToken,
		origin:  originResolver,
		health:  healthManager,
		logger:  config.Logger,
		tracing: config.Tracing,
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
				Builder:              snapshotBuilder{origin: originResolver, health: healthManager},
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
	if runtime.health != nil {
		if startErr := runtime.health.Start(managerParent); startErr != nil {
			configErr, healthErr := closeConfigAndHealth(
				ctx, manager, runtime.health, time.Now().Add(runtime.drainTimeout),
			)
			return errors.Join(
				fmt.Errorf("start Agent health scheduler: %w", startErr),
				configErr,
				healthErr,
			)
		}
	}

	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var healthWatch sync.WaitGroup
	var healthWatchErr error
	if runtime.health != nil {
		healthWatch.Add(1)
		safego.Go(func(panicErr error) {
			healthWatchErr = fmt.Errorf("observe Agent health scheduler: %w", panicErr)
			cancelRun()
		}, healthWatch.Done, func() {
			select {
			case <-runtime.health.Done():
				healthErr := runtime.health.Err()
				if healthErr == nil {
					healthErr = ErrHealthStopped
				}
				healthWatchErr = fmt.Errorf("run Agent health scheduler: %w", healthErr)
				cancelRun()
			case <-runContext.Done():
			}
		})
	}

	shutdownStarted := make(chan time.Time, 1)
	stopShutdownClock := context.AfterFunc(ctx, func() { shutdownStarted <- time.Now() })
	runErr := runtime.runControlSessions(runContext, func(sessionContext context.Context, session *agentsession.Session) error {
		return runtime.handleSession(sessionContext, manager, session)
	})
	cancelRun()
	healthWatch.Wait()
	if runtime.health != nil && healthWatchErr == nil {
		select {
		case <-runtime.health.Done():
			healthErr := runtime.health.Err()
			if healthErr == nil {
				healthErr = ErrHealthStopped
			}
			healthWatchErr = fmt.Errorf("run Agent health scheduler: %w", healthErr)
		default:
		}
	}
	if stopShutdownClock() {
		if ctx.Err() == nil {
			runtime.shutdownRetiredPools()
			configErr, healthErr := closeConfigAndHealth(
				ctx, manager, runtime.health, time.Now().Add(runtime.drainTimeout),
			)
			return errors.Join(runErr, healthWatchErr, runtime.retiredError(),
				configErr, healthErr)
		}
		shutdownStarted <- time.Now()
	}
	shutdownDeadline := (<-shutdownStarted).Add(runtime.drainTimeout)
	remaining := time.Until(shutdownDeadline)
	if remaining < 0 {
		remaining = 0
	}
	runtime.drainRetiredPools(remaining)
	configErr, healthErr := closeConfigAndHealth(ctx, manager, runtime.health, shutdownDeadline)
	return errors.Join(runErr, healthWatchErr, runtime.retiredError(),
		configErr, healthErr)
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
		Dialer:  runtime.origin,
		Tracing: runtime.tracing,
		Logger: logging.WithCorrelationFields(runtime.logger, logging.Correlation{
			TunnelID: authentication.TunnelID, ConnectorID: authentication.ConnectorID,
			SessionID: authentication.SessionID,
		}),
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
		resultErr = runtime.finishSessionPool(ctx, pool, cancelPool, resultErr)
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	return runtime.runEstablished(ctx, session, configSession, pool, ticker)
}

func (runtime *Runtime) finishSessionPool(
	ctx context.Context,
	pool workPool,
	cancelPool context.CancelFunc,
	resultErr error,
) error {
	// 普通 Control 断开只清理本代非 ACTIVE WorkConn；旧 ACTIVE 必须允许自然
	// 结束，且 Wait 不能阻塞下一代重连。业务错误或进程退出则先取消整个 Pool，
	// 再等待 Done，防止 SessionDone 抢先把 ACTIVE 转为 detached 后 Agent 提前返回。
	preserveActive := resultErr == nil && ctx.Err() == nil
	if preserveActive {
		if waitErr := pool.Wait(); waitErr != nil && !errors.Is(waitErr, context.Canceled) {
			resultErr = errors.Join(resultErr, fmt.Errorf("wait Agent WorkPool: %w", waitErr))
		}
		// 旧 ACTIVE 不阻塞下一代重连，但必须登记到 Runtime；进程关停时允许其
		// 在同一固定排空窗口内结束，Deadline 后再统一取消并等待 Pool.Done。
		runtime.retainPool(pool, cancelPool)
		return resultErr
	}

	cancelPool()
	<-pool.Done()
	if waitErr := pool.Wait(); waitErr != nil && !errors.Is(waitErr, context.Canceled) {
		resultErr = errors.Join(resultErr, fmt.Errorf("wait Agent WorkPool: %w", waitErr))
	}
	return resultErr
}

func (runtime *Runtime) runEstablished(
	ctx context.Context,
	session establishedSession,
	configSession *configruntime.Session,
	pool workPool,
	ticker *time.Ticker,
) error {
	reporter := newHealthReporter(runtime.health, session)
	reportTicker := time.NewTicker(healthReportFlushInterval)
	defer reportTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return runtime.drain(ctx, session, configSession, pool, ticker, reporter, reportTicker)
		case <-session.Done():
			return nil
		case inbound, ok := <-session.Inbound():
			if !ok {
				return nil
			}
			if err := applyInboundAndReport(ctx, session, configSession, pool, inbound, reporter); err != nil {
				return err
			}
		case <-reporter.changed():
			if err := reporter.collectChanges(); err != nil {
				if errors.Is(err, controlsession.ErrOwnerClosed) {
					return nil
				}
				return fmt.Errorf("enqueue Agent health report: %w", err)
			}
		case <-reportTicker.C:
			if err := reporter.flush(); err != nil {
				if errors.Is(err, controlsession.ErrOwnerClosed) {
					return nil
				}
				return fmt.Errorf("flush Agent health report: %w", err)
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
	reporter *healthReporter,
	reportTicker *time.Ticker,
) error {
	// DrainRequest 可能让 Server 很快停止等待普通增量；先把最新权威快照合并进
	// pending 并完整提交，不能把最后一批健康结果留到下一次 Session。
	if err := reporter.collectChanges(); err != nil {
		return errors.Join(processContext.Err(), fmt.Errorf("collect Agent health before drain: %w", err))
	}
	if err := reporter.flush(); err != nil {
		return errors.Join(processContext.Err(), fmt.Errorf("flush Agent health before drain: %w", err))
	}
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
	if err := session.Flush(drainContext); err != nil {
		return errors.Join(processContext.Err(), fmt.Errorf("write Agent health before drain: %w", err))
	}
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
				if err := applyInboundAndReport(drainContext, session, configSession, pool, inbound, reporter); err != nil {
					return errors.Join(processContext.Err(), err)
				}
				continue
			}
			// CompleteDrain 会等待 ACTIVE 自然结束，不能同步阻塞 Control Owner；
			// 否则等待期间没有 Heartbeat，Server 会先按 Heartbeat Timeout 关闭
			// Session，绕过 Agent 自己的 Drain Deadline。该 goroutine 由本函数
			// 持有：任何退出分支都会取消 drainContext 并等待它返回。
			completeResult := make(chan error, 1)
			safego.Go(
				func(panicErr error) { completeResult <- panicErr },
				nil,
				func() { completeResult <- pool.CompleteDrain(drainContext) },
			)
			waitComplete := func(ownerErr error) error {
				cancelDrain()
				completeErr := <-completeResult
				if completeErr != nil {
					completeErr = fmt.Errorf("complete Agent WorkPool drain: %w", completeErr)
				}
				return errors.Join(processContext.Err(), ownerErr, completeErr)
			}
			for {
				select {
				case completeErr := <-completeResult:
					if completeErr != nil {
						return errors.Join(processContext.Err(), fmt.Errorf("complete Agent WorkPool drain: %w", completeErr))
					}
					return processContext.Err()
				case <-drainContext.Done():
					return waitComplete(nil)
				case <-session.Done():
					return waitComplete(nil)
				case now := <-ticker.C:
					if err := session.Enqueue(heartbeatEnvelope(now, observedRevision(configSession))); err != nil {
						return waitComplete(fmt.Errorf("enqueue draining heartbeat after ack: %w", err))
					}
				}
			}
		case <-reporter.changed():
			if err := reporter.collectChanges(); err != nil {
				return errors.Join(processContext.Err(), fmt.Errorf("enqueue draining health report: %w", err))
			}
		case <-reportTicker.C:
			if err := reporter.flush(); err != nil {
				return errors.Join(processContext.Err(), fmt.Errorf("flush draining health report: %w", err))
			}
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
	return applyInboundAndReport(ctx, session, configSession, pool, inbound, nil)
}

func applyInboundAndReport(
	ctx context.Context,
	session establishedSession,
	configSession *configruntime.Session,
	pool workPool,
	inbound controlsession.Inbound,
	reporter *healthReporter,
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
		sink := &configAckHealthSink{
			session: session, reporter: reporter, snapshot: payload.ConfigSnapshot,
		}
		if err := configSession.Apply(ctx, payload.ConfigSnapshot, sink); err != nil {
			if errors.Is(err, configruntime.ErrConfigRejected) {
				return nil
			}
			return fmt.Errorf("apply Agent config snapshot: %w", err)
		}
		if sink.committed {
			// Outbox 已在同一临界区提交 Ack 与完整集合；只有成功后
			// 才更新 Reporter 本地基线，使后续 Changed 仅发送新 Snapshot 的增量。
			reporter.commitFull(sink.full)
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

func closeConfigManager(parent context.Context, manager *configruntime.Manager, timeout time.Duration) error {
	closeContext, cancelClose := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancelClose()
	if err := manager.Close(closeContext); err != nil {
		return fmt.Errorf("close Agent config runtime: %w", err)
	}
	return nil
}

func shutdownHealth(parent context.Context, health healthRuntime, timeout time.Duration) error {
	if health == nil {
		return nil
	}
	shutdownContext, cancelShutdown := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancelShutdown()
	if err := health.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("shutdown Agent health scheduler: %w", err)
	}
	return nil
}

// closeConfigAndHealth 让两个 Owner 按固定顺序共享同一个绝对截止时间。
// Config Close 消耗的时间会从 Health Shutdown 的预算中扣除，避免串行清理
// 把进程的固定排空窗口隐式翻倍。
func closeConfigAndHealth(
	parent context.Context,
	manager *configruntime.Manager,
	health healthRuntime,
	deadline time.Time,
) (error, error) {
	configErr := closeConfigManager(parent, manager, remainingUntil(deadline))
	healthErr := shutdownHealth(parent, health, remainingUntil(deadline))
	return configErr, healthErr
}

func remainingUntil(deadline time.Time) time.Duration {
	remaining := time.Until(deadline)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// HostConfig 使用进程当前主机与 Go 运行时信息构造无本地状态的生产配置。
// Connector 在此函数每次调用时新建，因此调用方必须只在单个进程生命周期调用一次。
func HostConfig(connectionToken, version string) (Config, error) {
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
	}, nil
}

// 编译期确保生产 WorkPool Handler 仍与 OPEN Handler 契约一致。
var _ agentworkpool.Handler = (*open.Handler)(nil)
