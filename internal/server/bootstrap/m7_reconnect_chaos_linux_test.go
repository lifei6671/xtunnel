//go:build linux

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentconnector "github.com/lifei6671/xtunnel/internal/agent/connector"
	agentreconnect "github.com/lifei6671/xtunnel/internal/agent/reconnect"
	agentsession "github.com/lifei6671/xtunnel/internal/agent/session"
	"github.com/lifei6671/xtunnel/internal/controlsession"
	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"golang.org/x/sys/unix"
)

const (
	m7ChaosConnectorsEnvironment = "XTUNNEL_M7_02_CONNECTORS"
	m7FullWorkTarget             = 8
	m7ChaosDeadline              = 5 * time.Minute
	m7ChaosStaggerWindow         = 2 * time.Second
)

// TestM7ReconnectStorm 是 M7-02 的 Linux 产品级重连风暴入口。测试只在显式给出
// XTUNNEL_M7_02_CONNECTORS 时运行；1 只供开发 smoke，100/500/1000 是完整
// Connector Runtime 档，5000 是不创建 WorkConn 的 Control-only 容量档。
func TestM7ReconnectStorm(t *testing.T) {
	connectorCount := m7ChaosConnectorCount(t)
	controlOnly := connectorCount == 5000
	baseline, err := m7ReadResources()
	if err != nil {
		t.Fatalf("read M7-02 resource baseline: %v", err)
	}

	serverContext, cancelServer := context.WithCancel(context.Background())
	httpOrigin := httptest.NewServer(http.NotFoundHandler())
	tcpOrigin := startProductGateTCPOrigin(t)
	gatewayAddress, publicAddress, publicPort := m7ReservePorts(t)
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("open M7-02 Server storage: %v", err)
	}
	seedProductGateDesiredState(
		t, serverContext, resources, httpOrigin.Listener.Addr(), tcpOrigin.listener.Addr(), publicPort,
	)
	if err := resources.database.CreateFirstAdmin(serverContext, "admin", "m7 reconnect chaos password"); err != nil {
		t.Fatalf("create M7-02 Admin: %v", err)
	}

	config := m7ChaosConfig(dataDir, gatewayAddress, publicPort, connectorCount)
	softLimit, requiredFDs := m7EnsureFDLimit(t, config, connectorCount, controlOnly)
	var (
		currentRuntime atomic.Pointer[gatewayBootstrapCloser]
		agents         *m7AgentGroup
		cleanupOnce    sync.Once
	)
	cleanup := func() {
		cleanupOnce.Do(func() {
			if agents != nil {
				if stopErr := agents.stop(time.Minute); stopErr != nil {
					t.Errorf("stop M7-02 Agents: %v", stopErr)
				}
			}
			if server := currentRuntime.Swap(nil); server != nil {
				if closeErr := server.Close(); closeErr != nil {
					t.Errorf("close M7-02 Server runtime: %v", closeErr)
				}
			}
			cancelServer()
			if resources != nil {
				if closeErr := resources.Close(); closeErr != nil {
					t.Errorf("close M7-02 Server storage: %v", closeErr)
				}
			}
		})
	}
	t.Cleanup(cleanup)
	t.Cleanup(httpOrigin.Close)

	server, err := m7OpenServer(serverContext, config, resources, runtimeDir, controlOnly)
	if err != nil {
		t.Fatalf("start initial M7-02 Server runtime: %v", err)
	}
	currentRuntime.Store(server)
	token := issueProductGateToken(t, serverContext, resources, server.gateway.Addr())
	agents, err = m7StartAgents(token, connectorCount, controlOnly)
	if err != nil {
		t.Fatalf("construct M7-02 Agents: %v", err)
	}

	startupAt := time.Now()
	startupSampler, err := m7StartSampler(&currentRuntime, &agents.started, gatewayAddress)
	if err != nil {
		t.Fatalf("start initial M7-02 resource sampler: %v", err)
	}
	m7CleanupSampler(t, startupSampler, controlOnly)
	var startupProbe *m7DataPlaneProbe
	if !controlOnly {
		startupProbe = m7StartDataPlaneProbe(startupAt, publicAddress, tcpOrigin)
		m7CleanupDataPlaneProbe(t, startupProbe)
	}
	agents.start()
	startup, err := m7WaitPhase(startupAt, server, agents, controlOnly)
	if err != nil {
		t.Fatalf("wait initial M7-02 readiness: %v", err)
	}
	if !controlOnly {
		startup.firstSuccess, startup.dataPlaneAttempts, err = startupProbe.await()
		if err != nil {
			t.Fatalf("exercise initial M7-02 data plane: %v", err)
		}
	}
	startup.resources, err = startupSampler.stop(controlOnly)
	if err != nil {
		t.Fatalf("sample initial M7-02 resources: %v", err)
	}

	restartAt := time.Now()
	if err := server.Close(); err != nil {
		t.Fatalf("stop initial M7-02 Server runtime: %v", err)
	}
	currentRuntime.Store(nil)
	// Restart 恢复计时从关闭前开始，但 Pending/资源采样只在旧 Server 已完全排空后启动。
	// 否则 Shutdown 先移除 Current 状态、后关闭旧 Socket 的窗口会把旧 Control 误算为新 AUTH。
	recoverySampler, err := m7StartSampler(&currentRuntime, &agents.started, gatewayAddress)
	if err != nil {
		t.Fatalf("start recovered M7-02 resource sampler: %v", err)
	}
	m7CleanupSampler(t, recoverySampler, controlOnly)
	if err := resources.Close(); err != nil {
		t.Fatalf("close initial M7-02 Server storage: %v", err)
	}
	resources = nil
	resources, err = openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("reopen M7-02 Server storage: %v", err)
	}
	server, err = m7OpenServer(serverContext, config, resources, runtimeDir, controlOnly)
	if err != nil {
		t.Fatalf("restart M7-02 Server on %s: %v", gatewayAddress, err)
	}
	currentRuntime.Store(server)
	var recoveryProbe *m7DataPlaneProbe
	if !controlOnly {
		recoveryProbe = m7StartDataPlaneProbe(restartAt, publicAddress, tcpOrigin)
		m7CleanupDataPlaneProbe(t, recoveryProbe)
	}
	recovery, err := m7WaitPhase(restartAt, server, agents, controlOnly)
	if err != nil {
		t.Fatalf("wait M7-02 recovery: %v", err)
	}
	if !controlOnly {
		recovery.firstSuccess, recovery.dataPlaneAttempts, err = recoveryProbe.await()
		if err != nil {
			t.Fatalf("exercise recovered M7-02 data plane: %v", err)
		}
	}
	recovery.resources, err = recoverySampler.stop(controlOnly)
	if err != nil {
		t.Fatalf("sample recovered M7-02 resources: %v", err)
	}
	generationResets, err := m7AssertRestartFencing(startup.sessions, recovery.sessions)
	if err != nil {
		t.Fatal(err)
	}

	result := m7ChaosResult{
		Connectors: connectorCount, Mode: m7Mode(controlOnly), GatewayAddress: gatewayAddress,
		ConfiguredPendingTLS:  config.Limits.MaxPendingTLSHandshakes,
		ConfiguredPendingAuth: config.Limits.MaxPendingAuth,
		SoftFDLimit:           softLimit, RequiredFDs: requiredFDs,
		Startup:          m7PhaseResult(startup, false, controlOnly),
		Recovery:         m7PhaseResult(recovery, true, controlOnly),
		GenerationResets: generationResets,
		RetryAfterScope:  "not injected; covered by internal/agent/reconnect scale tests",
		Baseline:         baseline,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode M7-02 result: %v", err)
	}
	t.Logf("m7_02_result=%s", encoded)
}

func m7ChaosConnectorCount(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(m7ChaosConnectorsEnvironment))
	if raw == "" {
		t.Skipf("set %s to 100, 500, 1000 or 5000 (1 is development smoke)", m7ChaosConnectorsEnvironment)
	}
	count, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", m7ChaosConnectorsEnvironment, raw, err)
	}
	switch count {
	case 1, 100, 500, 1000, 5000:
		return count
	default:
		t.Fatalf("%s=%d is unsupported; use 100, 500, 1000 or 5000 (1 is development smoke)",
			m7ChaosConnectorsEnvironment, count)
		return 0
	}
}

func m7ChaosConfig(dataDir, gatewayAddress string, publicPort uint16, connectors int) serverconfig.Config {
	config := gatewayLifecycleTestConfig(dataDir, gatewayAddress)
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)
	config.ConnectorRuntime.HeartbeatInterval.Duration = 30 * time.Second
	config.ConnectorRuntime.HeartbeatTimeout.Duration = 5 * time.Minute
	config.Limits.MaxConnectors = connectors
	config.Limits.MaxConnectorsPerTunnel = connectors
	config.Limits.MaxHealthTargetsPerTunnel = max(config.Limits.MaxHealthTargetsPerTunnel, connectors)
	config.Limits.MaxHealthTargetsGlobal = max(config.Limits.MaxHealthTargetsGlobal, connectors)
	config.Limits.MaxWorkConnections = connectors * m7FullWorkTarget
	config.Limits.MaxIdleWorkConnections = connectors * m7FullWorkTarget
	config.Limits.MaxConnectingWorkConnections = connectors * m7FullWorkTarget
	config.Limits.MaxPendingTLSHandshakes = 512
	config.Limits.MaxPendingAuth = 512
	return config
}

func m7OpenServer(
	ctx context.Context,
	config serverconfig.Config,
	resources *serverStorage,
	runtimeDir string,
	controlOnly bool,
) (*gatewayBootstrapCloser, error) {
	closer, err := openGatewayAndBootstrapWith(
		ctx, config, resources, slog.New(slog.NewJSONHandler(io.Discard, nil)), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened M7-02 Bootstrap Socket")
		},
	)
	if err != nil {
		return nil, err
	}
	runtime := closer.(*gatewayBootstrapCloser)
	if controlOnly {
		runtime.drainTimeout = 5 * time.Second
	}
	return runtime, nil
}

func m7ReservePorts(t *testing.T) (gatewayAddress, publicAddress string, publicPort uint16) {
	t.Helper()
	gatewayListener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("reserve M7-02 Gateway port: %v", err)
	}
	publicListener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		_ = gatewayListener.Close()
		t.Fatalf("reserve M7-02 public port: %v", err)
	}
	gatewayAddress = gatewayListener.Addr().String()
	publicAddress = publicListener.Addr().String()
	publicPort = uint16(publicListener.Addr().(*net.TCPAddr).Port)
	if err := errors.Join(gatewayListener.Close(), publicListener.Close()); err != nil {
		t.Fatalf("release M7-02 reserved ports: %v", err)
	}
	return gatewayAddress, publicAddress, publicPort
}

type m7AgentGroup struct {
	ctx       context.Context
	cancel    context.CancelFunc
	runs      []func(context.Context) error
	expected  map[string]struct{}
	results   chan error
	started   atomic.Int64
	wait      sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
	stopErr   error
}

func m7StartAgents(token string, count int, controlOnly bool) (*m7AgentGroup, error) {
	ctx, cancel := context.WithCancel(context.Background())
	group := &m7AgentGroup{
		ctx: ctx, cancel: cancel, runs: make([]func(context.Context) error, 0, count),
		expected: make(map[string]struct{}, count), results: make(chan error, count),
	}
	for index := range count {
		if controlOnly {
			connectorIdentity, err := identity.NewConnector()
			if err != nil {
				cancel()
				return nil, fmt.Errorf("create control-only Connector %d: %w", index, err)
			}
			runner, err := m7SessionRunner(token, connectorIdentity, index)
			if err != nil {
				cancel()
				return nil, fmt.Errorf("create control-only Runner %d: %w", index, err)
			}
			group.expected[connectorIdentity.ID()] = struct{}{}
			group.runs = append(group.runs, func(ctx context.Context) error {
				return agentreconnect.Run(ctx, runner, m7ControlOnlySession, agentreconnect.Options{
					InitialBackoff: time.Second, MaximumBackoff: 30 * time.Second,
					StableAfter: 30 * time.Second, JitterFraction: 0.20,
				})
			})
			continue
		}
		config, err := agentconnector.HostConfig(token, "v0.1.0-m7-02-chaos")
		if err != nil {
			cancel()
			return nil, fmt.Errorf("build full Connector %d config: %w", index, err)
		}
		config.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
		runtime, err := agentconnector.New(config)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("create full Connector %d: %w", index, err)
		}
		group.expected[config.Connector.ID()] = struct{}{}
		group.runs = append(group.runs, runtime.Run)
	}
	return group, nil
}

// start 为每个 Agent 建立明确 owner。所有 goroutine 都由 group.ctx 停止并由 stop 等待；
// 两秒固定窗口只错开首次启动，后续重连仍由生产 reconnect 的 Jitter/Backoff 决定。
func (group *m7AgentGroup) start() {
	group.startOnce.Do(func() {
		for index, run := range group.runs {
			delay := time.Duration(index) * m7ChaosStaggerWindow / time.Duration(len(group.runs))
			group.wait.Add(1)
			go func() {
				defer group.wait.Done()
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-group.ctx.Done():
					group.results <- group.ctx.Err()
					return
				case <-timer.C:
				}
				group.started.Add(1)
				group.results <- run(group.ctx)
			}()
		}
	})
}

func (group *m7AgentGroup) stop(timeout time.Duration) error {
	group.stopOnce.Do(func() {
		group.cancel()
		done := make(chan struct{})
		go func() {
			group.wait.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			group.stopErr = errors.New("Agent owners did not stop before timeout")
			return
		}
		close(group.results)
		for err := range group.results {
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
				group.stopErr = errors.Join(group.stopErr, err)
			}
		}
	})
	return group.stopErr
}

func m7SessionRunner(token string, connector identity.Connector, index int) (*agentsession.Runner, error) {
	return agentsession.NewRunner(agentsession.Config{
		ConnectionToken: token, Connector: connector,
		Hostname: fmt.Sprintf("m7-control-%05d.test", index), Version: "v0.1.0-m7-02-control",
		OS: "linux", Arch: "test", Capabilities: []string{"tcp"},
		AuthWriteTimeout: 10 * time.Second, AuthReadTimeout: 10 * time.Second,
		OwnerOptions: controlsession.Options{
			HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
			WriteTimeout: 5 * time.Second,
		},
	})
}

func m7ControlOnlySession(ctx context.Context, session *agentsession.Session) error {
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	var observedRevision uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.Done():
			return nil
		case inbound, ok := <-session.Inbound():
			if !ok {
				return nil
			}
			snapshot := inbound.Envelope.GetConfigSnapshot()
			if snapshot == nil {
				continue
			}
			observedRevision = snapshot.GetRevision()
			if err := session.Enqueue(&protocolv1.ControlEnvelope{
				ProtocolVersion: inbound.Envelope.GetProtocolVersion(),
				Payload: &protocolv1.ControlEnvelope_ConfigAck{ConfigAck: &protocolv1.ConfigAck{
					ObservedRevision: observedRevision,
					ApplyStatus:      protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED,
					ErrorCode:        protocolv1.ErrorCode_ERROR_CODE_OK,
				}},
			}); err != nil {
				return fmt.Errorf("enqueue control-only Snapshot Ack: %w", err)
			}
		case now := <-heartbeat.C:
			if err := session.Enqueue(&protocolv1.ControlEnvelope{
				ProtocolVersion: 1,
				Payload: &protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{
					TimestampMs: uint64(now.UnixMilli()), ObservedRevision: observedRevision,
				}},
			}); err != nil {
				return fmt.Errorf("enqueue control-only Heartbeat: %w", err)
			}
		}
	}
}

type m7PhaseObservation struct {
	control, configReady, workReady map[string]time.Duration
	sessionIDs                      map[string]string
	sessions                        map[string]serverruntime.Session
	firstSuccess                    time.Duration
	dataPlaneAttempts               int
	resources                       m7ResourceObservation
}

func m7WaitPhase(
	startedAt time.Time,
	server *gatewayBootstrapCloser,
	agents *m7AgentGroup,
	controlOnly bool,
) (m7PhaseObservation, error) {
	phase := m7PhaseObservation{
		control:     make(map[string]time.Duration, len(agents.expected)),
		configReady: make(map[string]time.Duration, len(agents.expected)),
		workReady:   make(map[string]time.Duration, len(agents.expected)),
		sessionIDs:  make(map[string]string, len(agents.expected)),
		sessions:    make(map[string]serverruntime.Session, len(agents.expected)),
	}
	deadline := time.NewTimer(m7ChaosDeadline)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		now := time.Since(startedAt)
		currentCounts := make(map[string]int, len(agents.expected))
		for _, snapshot := range server.sessions.RuntimeStatusSnapshots() {
			if snapshot.TunnelID != productGateTunnelID || !snapshot.CurrentControlSession {
				continue
			}
			if _, expected := agents.expected[snapshot.ConnectorID]; !expected {
				return phase, fmt.Errorf("unexpected current Connector %q", snapshot.ConnectorID)
			}
			currentCounts[snapshot.ConnectorID]++
			if phase.sessionIDs[snapshot.ConnectorID] != snapshot.Session.SessionID {
				// 同一恢复阶段内也可能再次断线。时延必须归属最终 Current Session，
				// 不能保留已被替换代次的首次就绪时间冒充恢复尾部。
				phase.sessionIDs[snapshot.ConnectorID] = snapshot.Session.SessionID
				delete(phase.control, snapshot.ConnectorID)
				delete(phase.configReady, snapshot.ConnectorID)
				delete(phase.workReady, snapshot.ConnectorID)
			}
			phase.sessions[snapshot.ConnectorID] = snapshot.Session
			if _, seen := phase.control[snapshot.ConnectorID]; !seen {
				phase.control[snapshot.ConnectorID] = now
			}
			tcpService, hasTCP := snapshot.Config.Services[productGateTCPServiceID]
			httpService, hasHTTP := snapshot.Config.Services[productGateHTTPServiceID]
			configReady := snapshot.Config.ConfigReady && snapshot.Config.HasObserved &&
				snapshot.Config.ObservedRevision == 1 && hasTCP && hasHTTP &&
				tcpService.Enabled && httpService.Enabled
			if configReady {
				if _, seen := phase.configReady[snapshot.ConnectorID]; !seen {
					phase.configReady[snapshot.ConnectorID] = now
				}
				if !controlOnly && snapshot.WorkPool.Idle >= m7FullWorkTarget &&
					snapshot.WorkPool.Connecting == 0 && snapshot.WorkPool.Opening == 0 &&
					snapshot.WorkPool.Active == 0 {
					if _, seen := phase.workReady[snapshot.ConnectorID]; !seen {
						phase.workReady[snapshot.ConnectorID] = now
					}
				}
			}
		}
		for connectorID, count := range currentCounts {
			if count != 1 {
				return phase, fmt.Errorf("Connector %q has %d Current Sessions, want exactly one", connectorID, count)
			}
		}
		ready := len(phase.control) == len(agents.expected) &&
			len(phase.configReady) == len(agents.expected)
		if !controlOnly {
			ready = ready && len(phase.workReady) == len(agents.expected)
		}
		if ready && len(currentCounts) == len(agents.expected) {
			return phase, nil
		}
		select {
		case agentErr := <-agents.results:
			return phase, fmt.Errorf("Agent owner exited before phase readiness: %w", agentErr)
		case <-deadline.C:
			return phase, fmt.Errorf("readiness timeout: current=%d config=%d work=%d want=%d",
				len(phase.control), len(phase.configReady), len(phase.workReady), len(agents.expected))
		case <-ticker.C:
		}
	}
}

type m7DataPlaneResult struct {
	firstSuccess time.Duration
	attempts     int
	err          error
}

type m7DataPlaneProbe struct {
	cancel  context.CancelFunc
	done    chan m7DataPlaneResult
	once    sync.Once
	awaited atomic.Bool
	result  m7DataPlaneResult
}

func m7StartDataPlaneProbe(
	startedAt time.Time,
	publicAddress string,
	origin *productGateTCPOrigin,
) *m7DataPlaneProbe {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan m7DataPlaneResult, 1)
	go func() {
		firstSuccess, attempts, err := m7ExerciseDataPlane(ctx, startedAt, publicAddress, origin)
		done <- m7DataPlaneResult{firstSuccess: firstSuccess, attempts: attempts, err: err}
	}()
	return &m7DataPlaneProbe{cancel: cancel, done: done}
}

func (probe *m7DataPlaneProbe) await() (time.Duration, int, error) {
	probe.awaited.Store(true)
	probe.once.Do(func() {
		probe.result = <-probe.done
		probe.cancel()
	})
	return probe.result.firstSuccess, probe.result.attempts, probe.result.err
}

func m7CleanupDataPlaneProbe(t *testing.T, probe *m7DataPlaneProbe) {
	t.Helper()
	t.Cleanup(func() {
		if probe.awaited.Load() {
			return
		}
		probe.cancel()
		_, _, err := probe.await()
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("stop M7-02 data-plane probe: %v", err)
		}
	})
}

func m7ExerciseDataPlane(
	ctx context.Context,
	startedAt time.Time,
	publicAddress string,
	origin *productGateTCPOrigin,
) (time.Duration, int, error) {
	const attemptTimeout = 3 * time.Second
	payload := []byte("m7-02-reconnect-success")
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return 0, attempt - 1, fmt.Errorf("no successful TCP round trip: %w", err)
		}
		dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.31")}}
		connection, err := dialer.DialContext(ctx, "tcp4", publicAddress)
		if err != nil {
			retry := time.NewTimer(20 * time.Millisecond)
			select {
			case <-ctx.Done():
				if !retry.Stop() {
					<-retry.C
				}
				return 0, attempt, fmt.Errorf("no successful TCP round trip: %w", ctx.Err())
			case <-retry.C:
			}
			continue
		}
		if err := connection.SetDeadline(time.Now().Add(attemptTimeout)); err != nil {
			_ = connection.Close()
			return 0, attempt, fmt.Errorf("set public connection deadline: %w", err)
		}
		if _, err := connection.Write(payload); err != nil {
			_ = connection.Close()
			continue
		}

		var originPeer net.Conn
		select {
		case originPeer = <-origin.peers:
		case err := <-origin.done:
			_ = connection.Close()
			return 0, attempt, fmt.Errorf("TCP Origin stopped: %w", err)
		case <-time.After(attemptTimeout):
			_ = connection.Close()
			continue
		case <-ctx.Done():
			_ = connection.Close()
			return 0, attempt, fmt.Errorf("no successful TCP round trip: %w", ctx.Err())
		}
		// 固定探测 Payload 直接在 Probe owner 内完成 Origin 读写；两端 Deadline 保证
		// Server 未传播 Close 时也会有界退出，不创建无法等待的 Echo goroutine。
		if err := originPeer.SetDeadline(time.Now().Add(attemptTimeout)); err != nil {
			_ = originPeer.Close()
			_ = connection.Close()
			return 0, attempt, fmt.Errorf("set M7-02 Origin connection deadline: %w", err)
		}
		originPayload := make([]byte, len(payload))
		if _, err := io.ReadFull(originPeer, originPayload); err != nil {
			_ = originPeer.Close()
			_ = connection.Close()
			continue
		}
		if string(originPayload) != string(payload) {
			_ = originPeer.Close()
			_ = connection.Close()
			continue
		}
		if _, err := originPeer.Write(originPayload); err != nil {
			_ = originPeer.Close()
			_ = connection.Close()
			continue
		}
		echoed := make([]byte, len(payload))
		_, readErr := io.ReadFull(connection, echoed)
		businessSucceeded := readErr == nil && string(echoed) == string(payload)
		var firstSuccess time.Duration
		if businessSucceeded {
			// T_first_success 在公网侧收到完整业务响应时截取；后续资源回收失败仍让
			// 本次尝试失败，但 Close 延迟不能污染首个业务成功时刻。
			firstSuccess = time.Since(startedAt)
		}
		if closeErr := errors.Join(originPeer.Close(), connection.Close()); closeErr != nil {
			return firstSuccess, attempt, fmt.Errorf("close M7-02 data-plane probe connections: %w", closeErr)
		}
		if businessSucceeded {
			return firstSuccess, attempt, nil
		}
	}
}

func m7AssertRestartFencing(
	before, after map[string]serverruntime.Session,
) (generationResets int, resultErr error) {
	if len(before) != len(after) {
		return 0, fmt.Errorf("M7-02 Session set changed across restart: before=%d after=%d", len(before), len(after))
	}
	oldSessionIDs := make(map[string]struct{}, len(before))
	for connectorID, oldSession := range before {
		oldSessionIDs[oldSession.SessionID] = struct{}{}
		newSession, ok := after[connectorID]
		if !ok {
			resultErr = errors.Join(resultErr, fmt.Errorf("Connector %q missing after restart", connectorID))
			continue
		}
		if newSession.SessionID == oldSession.SessionID {
			resultErr = errors.Join(resultErr, fmt.Errorf("Connector %q reused old Session ID", connectorID))
		}
		if newSession.Generation == 0 {
			resultErr = errors.Join(resultErr, fmt.Errorf("Connector %q published zero generation", connectorID))
		}
		if newSession.Generation <= oldSession.Generation {
			// Server 进程级 Runtime 已重建，generation 合法地从 1 重新开始。
			generationResets++
		}
	}
	for connectorID, current := range after {
		if _, polluted := oldSessionIDs[current.SessionID]; polluted {
			resultErr = errors.Join(resultErr, fmt.Errorf("Connector %q retained an old Session after restart", connectorID))
		}
	}
	return generationResets, resultErr
}

type m7ResourceSample struct {
	FDs        int   `json:"fds"`
	RSSKiB     int64 `json:"rss_kib"`
	Goroutines int   `json:"goroutines"`
}

type m7ResourceObservation struct {
	Peak                         m7ResourceSample `json:"peak"`
	CPUTimeMS                    int64            `json:"cpu_time_ms"`
	PeakClientInFlight           int64            `json:"peak_client_inflight"`
	PeakGatewayNoncurrent        int              `json:"peak_gateway_noncurrent_connections"`
	PeakPendingTLSAuthUpperBound *int             `json:"peak_pending_tls_auth_upper_bound,omitempty"`
	PendingSemantics             string           `json:"pending_measurement"`
}

type m7Sampler struct {
	cancel  context.CancelFunc
	done    chan m7SamplerResult
	once    sync.Once
	stopped atomic.Bool
	result  m7SamplerResult
}

type m7SamplerResult struct {
	observation m7ResourceObservation
	err         error
}

func m7StartSampler(
	runtimeRef *atomic.Pointer[gatewayBootstrapCloser],
	started *atomic.Int64,
	gatewayAddress string,
) (*m7Sampler, error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan m7SamplerResult, 1)
	_, portText, err := net.SplitHostPort(gatewayAddress)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("split M7-02 Gateway address %q: %w", gatewayAddress, err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("parse M7-02 Gateway port %q: %w", portText, err)
	}
	go func() {
		observation := m7ResourceObservation{}
		cpuStart, err := m7CPUTime()
		if err != nil {
			done <- m7SamplerResult{err: err}
			return
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			current := 0
			if server := runtimeRef.Load(); server != nil {
				for _, snapshot := range server.sessions.RuntimeStatusSnapshots() {
					if snapshot.TunnelID == productGateTunnelID && snapshot.CurrentControlSession {
						current++
					}
				}
			}
			sample, err := m7ReadResources()
			if err != nil {
				done <- m7SamplerResult{err: err}
				return
			}
			observation.Peak.FDs = max(observation.Peak.FDs, sample.FDs)
			observation.Peak.RSSKiB = max(observation.Peak.RSSKiB, sample.RSSKiB)
			observation.Peak.Goroutines = max(observation.Peak.Goroutines, sample.Goroutines)
			observation.PeakClientInFlight = max(observation.PeakClientInFlight, started.Load()-int64(current))
			gatewayConnections, err := m7GatewayConnections(uint16(port))
			if err != nil {
				done <- m7SamplerResult{err: err}
				return
			}
			observation.PeakGatewayNoncurrent = max(observation.PeakGatewayNoncurrent, gatewayConnections-current)
			select {
			case <-ctx.Done():
				cpuEnd, err := m7CPUTime()
				if err != nil {
					done <- m7SamplerResult{err: err}
					return
				}
				observation.CPUTimeMS = (cpuEnd - cpuStart).Milliseconds()
				done <- m7SamplerResult{observation: observation}
				return
			case <-ticker.C:
			}
		}
	}()
	return &m7Sampler{cancel: cancel, done: done}, nil
}

func (sampler *m7Sampler) stop(controlOnly bool) (m7ResourceObservation, error) {
	sampler.stopped.Store(true)
	sampler.once.Do(func() {
		sampler.cancel()
		sampler.result = <-sampler.done
		if sampler.result.err != nil {
			return
		}
		if controlOnly {
			peak := sampler.result.observation.PeakGatewayNoncurrent
			sampler.result.observation.PeakPendingTLSAuthUpperBound = &peak
			sampler.result.observation.PendingSemantics = "upper bound: IPv4 SYN_RECV+ESTABLISHED minus published Current Control; includes post-auth pre-publication connections"
		} else {
			sampler.result.observation.PendingSemantics = "mixed with WorkConn; not interpreted as Pending TLS/Auth"
		}
	})
	return sampler.result.observation, sampler.result.err
}

func m7CleanupSampler(t *testing.T, sampler *m7Sampler, controlOnly bool) {
	t.Helper()
	t.Cleanup(func() {
		if sampler.stopped.Load() {
			return
		}
		if _, err := sampler.stop(controlOnly); err != nil {
			t.Errorf("stop M7-02 resource sampler: %v", err)
		}
	})
}

func m7ReadResources() (m7ResourceSample, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return m7ResourceSample{}, fmt.Errorf("read M7-02 process file descriptors: %w", err)
	}
	rssKiB, err := m7RSSKiB()
	if err != nil {
		return m7ResourceSample{}, err
	}
	return m7ResourceSample{FDs: len(entries), RSSKiB: rssKiB, Goroutines: runtime.NumGoroutine()}, nil
}

func m7RSSKiB() (int64, error) {
	contents, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("read M7-02 process status: %w", err)
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse M7-02 VmRSS value %q: %w", fields[1], err)
			}
			return value, nil
		}
	}
	return 0, errors.New("read M7-02 process status: VmRSS is missing")
}

func m7CPUTime() (time.Duration, error) {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0, fmt.Errorf("read M7-02 process CPU usage: %w", err)
	}
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second +
		time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond, nil
}

func m7GatewayConnections(port uint16) (int, error) {
	// Gateway 显式监听 127.0.0.1，仅读取 IPv4 表，避免 tcp/tcp6 双栈映射重复计数。
	return m7GatewayConnectionsFile("/proc/net/tcp", port)
}

func m7GatewayConnectionsFile(path string, port uint16) (int, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read M7-02 Gateway connections from %s: %w", path, err)
	}
	wantPort := fmt.Sprintf("%04X", port)
	count := 0
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		local := strings.Split(fields[1], ":")
		if len(local) == 2 && local[1] == wantPort && (fields[3] == "01" || fields[3] == "03") {
			count++
		}
	}
	return count, nil
}

func m7EnsureFDLimit(t *testing.T, config serverconfig.Config, connectors int, controlOnly bool) (uint64, uint64) {
	t.Helper()
	budget, err := serverFDBudget(config, tcpListenerFDReserve(config.TCPIngress))
	if err != nil {
		t.Fatalf("calculate M7-02 Server FD budget: %v", err)
	}
	required := budget.WorkConnections + budget.PublicActiveConnections + budget.PendingOpenConnections +
		budget.ConnectorControls + budget.PendingTLSHandshakes + budget.PendingAuth + budget.Listeners +
		budget.SQLite + budget.Management + budget.Metrics + budget.SafetyMargin
	clientConnections := uint64(connectors)
	if !controlOnly {
		clientConnections += uint64(connectors * m7FullWorkTarget)
	}
	required += clientConnections
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("read M7-02 RLIMIT_NOFILE: %v", err)
	}
	if limit.Cur >= required {
		return limit.Cur, required
	}
	if limit.Max < required {
		t.Fatalf("M7-02 requires RLIMIT_NOFILE >= %d, hard limit is %d", required, limit.Max)
	}
	original := limit
	limit.Cur = required
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatalf("raise M7-02 RLIMIT_NOFILE to %d: %v", required, err)
	}
	t.Cleanup(func() {
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
			t.Errorf("restore M7-02 RLIMIT_NOFILE: %v", err)
		}
	})
	return required, required
}

type m7Distribution struct {
	MinMS int64 `json:"min_ms"`
	P50MS int64 `json:"p50_ms"`
	P95MS int64 `json:"p95_ms"`
	P99MS int64 `json:"p99_ms"`
	MaxMS int64 `json:"max_ms"`
}

type m7StructuredPhase struct {
	TControlInitialMS   *m7Distribution       `json:"t_control_initial_ms,omitempty"`
	TControlReconnectMS *m7Distribution       `json:"t_control_reconnect_ms,omitempty"`
	TConfigReadyMS      m7Distribution        `json:"t_config_ready_ms"`
	TWorkPoolReadyMS    *m7Distribution       `json:"t_workpool_ready_ms"`
	TFirstSuccessMS     *int64                `json:"t_first_success_ms"`
	DataPlaneAttempts   *int                  `json:"data_plane_attempts"`
	WorkPoolStatus      string                `json:"workpool_status"`
	FirstSuccessStatus  string                `json:"first_success_status"`
	Resources           m7ResourceObservation `json:"resources"`
}

type m7ChaosResult struct {
	Connectors            int               `json:"connectors"`
	Mode                  string            `json:"mode"`
	GatewayAddress        string            `json:"gateway_address"`
	ConfiguredPendingTLS  int               `json:"configured_max_pending_tls"`
	ConfiguredPendingAuth int               `json:"configured_max_pending_auth"`
	SoftFDLimit           uint64            `json:"soft_fd_limit"`
	RequiredFDs           uint64            `json:"required_fds"`
	Startup               m7StructuredPhase `json:"startup"`
	Recovery              m7StructuredPhase `json:"recovery"`
	GenerationResets      int               `json:"generation_resets"`
	RetryAfterScope       string            `json:"retry_after_scope"`
	Baseline              m7ResourceSample  `json:"baseline"`
}

func m7PhaseResult(phase m7PhaseObservation, reconnect, controlOnly bool) m7StructuredPhase {
	control := m7Durations(phase.control)
	result := m7StructuredPhase{
		TConfigReadyMS: m7Durations(phase.configReady),
		WorkPoolStatus: "N/A: control-only tier", FirstSuccessStatus: "N/A: control-only tier",
		Resources: phase.resources,
	}
	if reconnect {
		result.TControlReconnectMS = &control
	} else {
		result.TControlInitialMS = &control
	}
	if !controlOnly {
		work := m7Durations(phase.workReady)
		result.TWorkPoolReadyMS = &work
		first := phase.firstSuccess.Milliseconds()
		result.TFirstSuccessMS = &first
		attempts := phase.dataPlaneAttempts
		result.DataPlaneAttempts = &attempts
		result.WorkPoolStatus = "READY"
		result.FirstSuccessStatus = "SUCCEEDED"
	}
	return result
}

func m7Durations(values map[string]time.Duration) m7Distribution {
	durations := make([]int64, 0, len(values))
	for _, value := range values {
		durations = append(durations, value.Milliseconds())
	}
	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })
	if len(durations) == 0 {
		return m7Distribution{}
	}
	percentile := func(value int) int64 {
		index := (len(durations) - 1) * value / 100
		return durations[index]
	}
	return m7Distribution{
		MinMS: durations[0], P50MS: percentile(50), P95MS: percentile(95),
		P99MS: percentile(99), MaxMS: durations[len(durations)-1],
	}
}

func m7Mode(controlOnly bool) string {
	if controlOnly {
		return "control-only"
	}
	return "full-runtime"
}
