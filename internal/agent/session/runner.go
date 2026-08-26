// Package session 装配 Agent 侧一条已经认证的 Connector Control Session。
//
// 本包只负责单次 Dial、AUTH 和 Control Owner 生命周期，不实现重连退避、WorkPool、
// Snapshot Apply 或 Bootstrap 接线。调用方应在进程启动时只创建一个 Connector，并在
// 同一个 Runner 的后续连接尝试中复用它；每次认证仍由 Server 签发新的 Session ID。
package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/state"
	"github.com/lifei6671/xtunnel/internal/safego"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

const (
	protocolVersionV1       uint32 = 1
	maxConnectionTokenBytes        = 8192
)

var (
	// ErrInvalidConfig 表示单次 Control Session 缺少必需的身份、Token 或有界 IO 参数。
	ErrInvalidConfig = errors.New("agent control session runner config is invalid")
	// ErrSessionActive 表示同一 Runner 已有一次连接尝试或存活 Session。
	ErrSessionActive = errors.New("agent control session runner already has an active session")
	// ErrSessionClosed 表示调用方在 Control Session 结束后索取当前 Work 认证材料。
	ErrSessionClosed = errors.New("agent control session is closed")
)

// Config 是一个 Agent 进程内全部 Control 连接尝试共享的固定输入。
type Config struct {
	// ConnectionToken 是 Tunnel 当前 Token；同一 Tunnel 的多个 Connector 复用同一 Token。
	ConnectionToken string
	// Connector 是进程启动时生成的内存身份，同一进程的全部重连必须复用该值。
	Connector identity.Connector
	// Hostname、Version、OS、Arch 和 Capabilities 是 Connector 自报运行信息。
	Hostname     string
	Version      string
	OS           string
	Arch         string
	Capabilities []string
	// AuthWriteTimeout 与 AuthReadTimeout 限制 AUTH 请求和响应的完整 Frame IO。
	AuthWriteTimeout time.Duration
	AuthReadTimeout  time.Duration
	// OwnerOptions 提供有界队列和已建立 Control Frame 的固定写超时。
	// ProtocolVersion 由认证结果覆盖，调用方无需也不能决定协商结果。
	OwnerOptions controlsession.Options
}

type dialFunc func(context.Context, string, string) (net.Conn, error)
type authenticateFunc func(context.Context, net.Conn, controlauth.Config) (*controlauth.Session, error)
type ownerFactory func(net.Conn, *state.Control, controlsession.Options) (*controlsession.Owner, error)

type dependencies struct {
	dial         dialFunc
	authenticate authenticateFunc
	newOwner     ownerFactory
}

// Runner 串行化同一 Connector 的 Control 连接尝试。
//
// Runner 不执行自动重连；Start 返回的 Session 结束后，M1-13 的重连器可以再次调用
// Start。内部 attemptGeneration 只保护 Agent 本地旧清理不会释放新尝试；Server 侧
// session_generation fencing 仍由认证 Registry 独立负责。
type Runner struct {
	config       Config
	dependencies dependencies

	lifecycleMu       sync.Mutex
	busy              bool
	attemptGeneration uint64
}

// Session 是已经完成 AUTH 并由 Control Owner 独占连接的单次运行态。
// 业务 goroutine 只能通过 Enqueue 和 Inbound 与 Control Session 交互。
type Session struct {
	owner  *controlsession.Owner
	cancel context.CancelFunc

	authMu         sync.Mutex
	authentication controlauth.Session
	closed         bool

	done   chan struct{}
	result error
}

// NewRunner 创建生产 Runner。它始终只使用 Connection Token 内的 Gateway Endpoint、
// TLS Trust 和固定 Control ALPN 建连，不接受额外的地址或信任来源。
func NewRunner(config Config) (*Runner, error) {
	return newRunner(config, dependencies{
		dial: func(ctx context.Context, token, alpn string) (net.Conn, error) {
			return agentgateway.DialContext(ctx, token, alpn)
		},
		authenticate: controlauth.Authenticate,
		newOwner:     controlsession.NewOwner,
	})
}

func newRunner(config Config, dependencies dependencies) (*Runner, error) {
	if len(config.ConnectionToken) == 0 || len(config.ConnectionToken) > maxConnectionTokenBytes ||
		config.AuthWriteTimeout <= 0 || config.AuthReadTimeout <= 0 ||
		config.OwnerOptions.HighPriorityCapacity <= 0 || config.OwnerOptions.NormalCapacity <= 0 ||
		config.OwnerOptions.InboundCapacity <= 0 || config.OwnerOptions.WriteTimeout <= 0 ||
		dependencies.dial == nil || dependencies.authenticate == nil || dependencies.newOwner == nil {
		return nil, ErrInvalidConfig
	}
	if err := identity.ValidateConnectorID(config.Connector.ID()); err != nil {
		return nil, fmt.Errorf("%w: connector id: %v", ErrInvalidConfig, err)
	}
	// Capabilities 在 Runner 构造时取得独立副本，避免调用方在后续重连间修改认证语义。
	config.Capabilities = append([]string(nil), config.Capabilities...)
	return &Runner{config: config, dependencies: dependencies}, nil
}

// Start 完成一次 Control TLS 拨号、同步 AUTH 和 Owner 启动，然后立即返回 Session。
//
// Start 成功后，连接和协议状态的所有权已经转移给 Owner；Context 取消会由 Owner 按
// cancel -> deadline(now) -> close -> wait 的固定顺序结束全部内部 goroutine。Start
// 失败时连接仍属于 Runner，本方法会在返回前关闭它并清理 Session Secret。
func (runner *Runner) Start(ctx context.Context) (*Session, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	lifetimeContext, cancelLifetime := context.WithCancel(ctx)
	return runner.start(ctx, lifetimeContext, cancelLifetime)
}

// StartDetached 使用 ctx 限制 Dial/AUTH，但已建立 Session 不再继承其取消信号。
//
// 该入口只供进程级重连器使用：SIGTERM 后 Agent 仍需在存活的 Control socket 上完成
// DrainRequest/DrainAck 两阶段握手。调用方取得 Session 后必须最终调用 Close 并 Wait；
// Start 失败则由本方法自行取消尚未移交的生命周期。
func (runner *Runner) StartDetached(ctx context.Context) (*Session, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	lifetimeContext, cancelLifetime := context.WithCancel(context.WithoutCancel(ctx))
	return runner.start(ctx, lifetimeContext, cancelLifetime)
}

func (runner *Runner) start(
	attemptContext context.Context,
	lifetimeContext context.Context,
	cancelLifetime context.CancelFunc,
) (*Session, error) {
	if err := attemptContext.Err(); err != nil {
		cancelLifetime()
		return nil, err
	}

	generation, err := runner.reserveAttempt()
	if err != nil {
		cancelLifetime()
		return nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			cancelLifetime()
			runner.releaseAttempt(generation)
		}
	}()

	connection, err := runner.dependencies.dial(attemptContext, runner.config.ConnectionToken, servergateway.ControlALPN)
	if err != nil {
		return nil, fmt.Errorf("dial connector control session: %w", err)
	}

	authentication, err := runner.dependencies.authenticate(attemptContext, connection, controlauth.Config{
		ConnectionToken: runner.config.ConnectionToken,
		Connector:       runner.config.Connector,
		Hostname:        runner.config.Hostname,
		Version:         runner.config.Version,
		OS:              runner.config.OS,
		Arch:            runner.config.Arch,
		MinProtocol:     protocolVersionV1,
		MaxProtocol:     protocolVersionV1,
		Capabilities:    runner.config.Capabilities,
		WriteTimeout:    runner.config.AuthWriteTimeout,
		ReadTimeout:     runner.config.AuthReadTimeout,
	})
	if err != nil {
		return nil, closeUnownedConnection(connection, fmt.Errorf("authenticate connector control session: %w", err))
	}
	// Detached 仅从成功建连之后生效；若进程在 Dial/AUTH 期间已经取消，不能发布一条
	// 调用方尚未来得及进入 Drain 流程的半存活 Session。
	if err := attemptContext.Err(); err != nil {
		return nil, cleanupAuthenticatedConnection(connection, authentication, err)
	}

	ownerOptions := runner.config.OwnerOptions
	// AUTH Success 是协议版本的唯一权威；禁止让本地配置覆盖协商结果。
	ownerOptions.ProtocolVersion = authentication.ProtocolVersion
	owner, err := runner.dependencies.newOwner(connection, authentication.Control, ownerOptions)
	if err != nil {
		return nil, cleanupAuthenticatedConnection(connection, authentication,
			fmt.Errorf("create connector control session owner: %w", err))
	}
	if err := owner.Start(lifetimeContext); err != nil {
		return nil, cleanupAuthenticatedConnection(connection, authentication,
			fmt.Errorf("start connector control session owner: %w", err))
	}

	session := &Session{
		owner:          owner,
		cancel:         cancelLifetime,
		authentication: *authentication,
		done:           make(chan struct{}),
	}
	// Control 指针已经独占移交给 Owner，Session 只保留 WorkHello 所需身份与 Secret。
	session.authentication.Control = nil
	// 结构体复制会产生第二份固定数组；保留 Session 内受锁保护的一份后立即擦除临时值。
	clear(authentication.SessionSecret[:])
	succeeded = true
	session.startCompletionObserver(runner, generation)
	return session, nil
}

// Enqueue 把消息深拷贝后交给有界、可合并的 Control Outbox。
func (session *Session) Enqueue(envelope *protocolv1.ControlEnvelope) error {
	return session.owner.Enqueue(envelope)
}

// Inbound 返回已经由唯一状态 Owner 校验过的只读入站事件流。
func (session *Session) Inbound() <-chan controlsession.Inbound {
	return session.owner.Inbound()
}

// Done 在 Owner 及其全部内部 goroutine 退出、敏感认证材料清理后关闭。
func (session *Session) Done() <-chan struct{} {
	return session.done
}

// Wait 等待当前 Control Session 完整退出并返回唯一终止原因。
func (session *Session) Wait() error {
	<-session.done
	return session.result
}

// Close 请求结束当前 Control Session；可并发、重复调用。
// Close 只广播取消，Wait 才是连接关闭、goroutine 回收和 Secret 清理的完成点。
func (session *Session) Close() {
	if session == nil || session.cancel == nil {
		return
	}
	session.cancel()
}

// WorkAuthSession 返回仅供当前 Session 创建 WorkHello 的认证材料副本。
//
// 返回值不携带 Control 状态指针，调用方不能绕过 Owner 修改协议状态。副本包含
// Session Secret，只能按值传给 workauth.Authenticate，禁止持久化、记录或缓存；
// Control Session 结束后本方法快速失败，避免旧 Session 为新 WorkConn 签名。
func (session *Session) WorkAuthSession() (controlauth.Session, error) {
	session.authMu.Lock()
	defer session.authMu.Unlock()
	if session.closed {
		return controlauth.Session{}, ErrSessionClosed
	}
	return session.authentication, nil
}

func (session *Session) startCompletionObserver(runner *Runner, generation uint64) {
	safego.Go(func(err error) {
		session.result = fmt.Errorf("observe connector control session completion: %w", err)
	}, func() {
		session.finishCompletion(runner, generation)
	}, func() {
		session.result = session.owner.Wait()
	})
}

func (session *Session) finishCompletion(runner *Runner, generation uint64) {
	session.cancel()

	session.authMu.Lock()
	clear(session.authentication.SessionSecret[:])
	session.closed = true
	session.authMu.Unlock()

	// 先按 generation 释放 Runner，再关闭 Done。这样 Wait 返回后，调用方可以立即
	// 发起下一次连接，同时旧 Session 的迟到清理永远不会覆盖新尝试。
	runner.releaseAttempt(generation)
	close(session.done)
}

func (runner *Runner) reserveAttempt() (uint64, error) {
	runner.lifecycleMu.Lock()
	defer runner.lifecycleMu.Unlock()
	if runner.busy {
		return 0, ErrSessionActive
	}
	runner.busy = true
	runner.attemptGeneration++
	return runner.attemptGeneration, nil
}

func (runner *Runner) releaseAttempt(generation uint64) {
	runner.lifecycleMu.Lock()
	defer runner.lifecycleMu.Unlock()
	if runner.attemptGeneration == generation {
		runner.busy = false
	}
}

func cleanupAuthenticatedConnection(connection net.Conn, authentication *controlauth.Session, cause error) error {
	if authentication != nil {
		clear(authentication.SessionSecret[:])
		if authentication.Control != nil {
			authentication.Control.Close()
		}
	}
	return closeUnownedConnection(connection, cause)
}

func closeUnownedConnection(connection net.Conn, cause error) error {
	if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return errors.Join(cause, fmt.Errorf("close unowned connector control connection: %w", err))
	}
	return cause
}
