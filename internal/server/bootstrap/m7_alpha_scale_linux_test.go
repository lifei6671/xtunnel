//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/connector"
	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	serverlimits "github.com/lifei6671/xtunnel/internal/server/limits"
)

const m7AlphaConnectionsEnvironment = "XTUNNEL_M7_10_CONNECTIONS"

type m7AlphaShard struct {
	tunnelID string
	service  string
	address  string
	token    string
}

type m7AlphaConnectionResult struct {
	duration time.Duration
	err      error
}

type m7AlphaScaleResult struct {
	Connections         int              `json:"connections"`
	Shards              int              `json:"shards"`
	Succeeded           int              `json:"succeeded"`
	SuccessRate         float64          `json:"success_rate"`
	OpenP95MS           int64            `json:"open_p95_ms"`
	ScaleRoundTripP95MS int64            `json:"scale_round_trip_p95_ms"`
	DirectRTTP95Micros  int64            `json:"direct_rtt_p95_micros"`
	Baseline            m7ResourceSample `json:"baseline"`
	Peak                m7ResourceSample `json:"peak"`
	Settled             m7ResourceSample `json:"settled"`
}

// TestM7AlphaPublicConnectionGate 通过真实公网 TCP Listener、Tunnel Proxy、Gateway、
// Token-only Agent 与 Origin 执行 1000/5000 并发连接。每个 Tunnel 分片最多 50 条，
// 使冻结的 16 条并发建连预算能在 10 秒 Lease 内完成补池；结果按固定分母计算，
// 失败连接不会被丢弃。
func TestM7AlphaPublicConnectionGate(t *testing.T) {
	connections := m7AlphaConnectionCount(t)
	shardCount := (connections + 49) / 50
	origin := startM7AlphaEchoOrigin(t)
	directRTT := m7AlphaMeasureDirectRTT(t, origin.listener.Addr().String())
	if p95 := m7AlphaPercentile(directRTT, 95); p95 > time.Millisecond {
		t.Fatalf("M7-10 direct loopback RTT P95 = %v, want <= 1ms", p95)
	}

	reserved, ports := reserveM7AlphaPorts(t, shardCount)
	serverContext, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("open M7-10 Server storage: %v", err)
	}
	resourcesClosed := false
	defer func() {
		if !resourcesClosed {
			if closeErr := resources.Close(); closeErr != nil {
				t.Errorf("close M7-10 Server storage: %v", closeErr)
			}
		}
	}()
	shards := seedM7AlphaShards(t, serverContext, resources, origin.listener.Addr(), ports)
	if err := resources.database.CreateFirstAdmin(serverContext, "admin", "m7 alpha scale gate password"); err != nil {
		t.Fatalf("create M7-10 Admin: %v", err)
	}
	for _, listener := range reserved {
		if err := listener.Close(); err != nil {
			t.Fatalf("release M7-10 reserved port: %v", err)
		}
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.TCPIngress.MinPort = int(ports[0])
	config.TCPIngress.MaxPort = int(ports[len(ports)-1])
	config.ConnectorRuntime.HeartbeatInterval.Duration = time.Second
	config.ConnectorRuntime.HeartbeatTimeout.Duration = 5 * time.Minute
	config.Transport.TCP.WorkAcquireTimeout.Duration = 90 * time.Second
	config.Limits.MaxTunnels = max(config.Limits.MaxTunnels, shardCount)
	config.Limits.MaxConnectors = max(config.Limits.MaxConnectors, shardCount)
	config.Limits.MaxConnectorsPerTunnel = 1
	config.Limits.MaxWorkConnections = connections + shardCount*8
	config.Limits.MaxIdleWorkConnections = connections + shardCount*8
	config.Limits.MaxConnectingWorkConnections = min(connections, 1000)
	config.Limits.MaxPendingOpens = connections
	config.Limits.MaxActiveConnections = connections
	config.Limits.MaxConnectionsPerTunnel = 50
	config.Limits.MaxConnectionsPerService = 50
	config.Limits.MaxConnectionsPerSourceIP = 2
	config.Limits.MaxOpenRatePerSourceIP = 1000
	config.Limits.MaxOpenBurstPerSourceIP = 1000
	// 初始 8 条 WorkConn 之后，全部 Pending OPEN 可能同步触发补池；握手与认证
	// 预算覆盖完整固定分母，确保 Gate 测到数据面容量，而不是预认证限流。
	config.Limits.MaxPendingTLSHandshakes = max(config.Limits.MaxPendingTLSHandshakes, connections+shardCount*8)
	config.Limits.MaxPendingAuth = max(config.Limits.MaxPendingAuth, connections+shardCount*8)

	serverLogs := &bytes.Buffer{}
	closer, err := openGatewayAndBootstrapWith(
		serverContext, config, resources, slog.New(slog.NewJSONHandler(serverLogs, nil)), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened M7-10 Bootstrap Socket")
		},
	)
	if err != nil {
		t.Fatalf("start M7-10 Server runtime: %v", err)
	}
	serverRuntime := closer.(*gatewayBootstrapCloser)
	runtimeClosed := false
	defer func() {
		if !runtimeClosed {
			if closeErr := closer.Close(); closeErr != nil {
				t.Errorf("close M7-10 Server runtime: %v", closeErr)
			}
		}
	}()
	for index := range shards {
		shards[index].token = issueM7AlphaToken(t, serverContext, resources, serverRuntime.gateway.Addr(), shards[index].tunnelID)
	}
	agents := make([]*m7AlphaAgent, 0, len(shards))
	for _, shard := range shards {
		agents = append(agents, startM7AlphaAgent(t, shard, serverRuntime, serverLogs))
	}
	defer func() {
		for _, agent := range agents {
			agent.stop(t)
		}
	}()
	openDurations := m7AlphaMeasureTunnelOpen(t, shards)
	openP95 := m7AlphaPercentile(openDurations, 95)
	if openP95 > 200*time.Millisecond {
		t.Fatalf("M7-10 public Dial-to-Origin-echo P95 = %v, want <= 200ms", openP95)
	}
	m7AlphaWaitSettled(t, serverRuntime, shards, 30*time.Second)
	m7AlphaWaitHeartbeatWarmup(t, serverRuntime, shardCount, 30*time.Second)
	baseline, err := m7ReadResources()
	if err != nil {
		t.Fatalf("read warmed M7-10 baseline resources: %v", err)
	}

	results := make(chan m7AlphaConnectionResult, connections)
	release := make(chan struct{})
	var clients sync.WaitGroup
	var releaseOnce sync.Once
	releaseAll := func() {
		releaseOnce.Do(func() { close(release) })
		clients.Wait()
	}
	defer releaseAll()
	for index := range connections {
		shard := shards[index%len(shards)]
		// 每波为 Control 心跳、Work 认证和 Origin Dial 保留调度余量；所有成功
		// 连接一直保持到固定分母收齐，因此峰值仍是完整的 1000/5000 并发连接。
		wave := (index / len(shards)) / 4
		clients.Add(1)
		go func(index int, wave int) {
			defer clients.Done()
			timer := time.NewTimer(time.Duration(wave) * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-t.Context().Done():
				results <- m7AlphaConnectionResult{err: t.Context().Err()}
				return
			}
			startedAt := time.Now()
			localIP := net.IPv4(127, byte(index/64516+1), byte((index/254)%254+1), byte(index%254+1))
			dialer := net.Dialer{Timeout: 90 * time.Second, LocalAddr: &net.TCPAddr{IP: localIP}}
			connection, err := dialer.DialContext(t.Context(), "tcp4", shard.address)
			if err != nil {
				results <- m7AlphaConnectionResult{err: err}
				return
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(90 * time.Second)); err != nil {
				results <- m7AlphaConnectionResult{err: err}
				return
			}
			payload := []byte(fmt.Sprintf("m7a-%08d", index))
			if _, err := connection.Write(payload); err != nil {
				results <- m7AlphaConnectionResult{err: err}
				return
			}
			echoed := make([]byte, len(payload))
			if _, err := io.ReadFull(connection, echoed); err != nil || string(echoed) != string(payload) {
				results <- m7AlphaConnectionResult{err: errors.Join(err, errors.New("origin echo mismatch"))}
				return
			}
			results <- m7AlphaConnectionResult{duration: time.Since(startedAt)}
			<-release
		}(index, wave)
	}
	durations := make([]time.Duration, 0, connections)
	failures := make([]string, 0)
	peak := baseline
	for range connections {
		result := <-results
		if result.err != nil {
			if len(failures) < 10 {
				failures = append(failures, result.err.Error())
			}
		} else {
			durations = append(durations, result.duration)
		}
		current, readErr := m7ReadResources()
		if readErr != nil {
			t.Fatalf("sample M7-10 resources: %v", readErr)
		}
		peak.FDs = max(peak.FDs, current.FDs)
		peak.RSSKiB = max(peak.RSSKiB, current.RSSKiB)
		peak.Goroutines = max(peak.Goroutines, current.Goroutines)
	}
	succeeded := len(durations)
	wantSucceeded := connections
	if connections == 1000 {
		wantSucceeded = 999
	} else if connections == 5000 {
		wantSucceeded = 4975
	}
	if succeeded < wantSucceeded {
		releaseAll()
		t.Fatalf("M7-10 successful connections = %d/%d, want >= %d; limits=%+v sessions=%+v; first failures: %s", succeeded, connections, wantSucceeded, serverRuntime.limits.Snapshot(), serverRuntime.sessions.RuntimeStatusSnapshots(), strings.Join(failures, "; "))
	}
	limits := m7AlphaWaitActive(t, serverRuntime, uint64(succeeded), 10*time.Second)
	if limits.ActiveTotal != uint64(succeeded) || limits.PendingOpens != 0 || limits.ActiveTotal > uint64(connections) {
		releaseAll()
		t.Fatalf("M7-10 active limits = %+v, want active=%d pending=0", limits, succeeded)
	}
	scaleP95 := m7AlphaPercentile(durations, 95)
	releaseAll()
	m7AlphaWaitSettled(t, serverRuntime, shards, 30*time.Second)
	settled := m7AlphaWaitResources(t, baseline, 30*time.Second)
	for _, agent := range agents {
		agent.stop(t)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close M7-10 Server runtime: %v", err)
	}
	runtimeClosed = true
	if err := resources.Close(); err != nil {
		t.Fatalf("close M7-10 Server storage: %v", err)
	}
	resourcesClosed = true
	assertM7LogsSafe(t, "Server", serverLogs.String(), shardsTokens(shards)...)
	for _, agent := range agents {
		assertM7LogsSafe(t, "Agent", agent.logs.String(), shardsTokens(shards)...)
	}
	result := m7AlphaScaleResult{
		Connections: connections, Shards: shardCount, Succeeded: succeeded,
		SuccessRate: float64(succeeded) / float64(connections), OpenP95MS: openP95.Milliseconds(),
		ScaleRoundTripP95MS: scaleP95.Milliseconds(),
		DirectRTTP95Micros:  m7AlphaPercentile(directRTT, 95).Microseconds(),
		Baseline:            baseline, Peak: peak, Settled: settled,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode M7-10 scale result: %v", err)
	}
	t.Logf("m7_10_scale_result=%s", encoded)
}

func shardsTokens(shards []m7AlphaShard) []string {
	// Connector ID 与 Token 没有可逆关系；这里返回所有本轮 Token，使每个 Agent
	// 日志都必须对完整凭据集合保持安全，分片规模最多 100，扫描开销有界。
	tokens := make([]string, 0, len(shards))
	for _, shard := range shards {
		tokens = append(tokens, shard.token)
	}
	return tokens
}

func assertM7LogsSafe(t *testing.T, owner, content string, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if token != "" && strings.Contains(content, token) {
			t.Fatalf("%s log leaked a Connection Token", owner)
		}
	}
	lower := strings.ToLower(content)
	for _, forbidden := range []string{
		"authorization: bearer", `"authorization":"bearer`, `"cookie":`,
		`"authentication_secret":`, `"session_secret":`, "-----begin private key-----",
		"-----begin rsa private key-----", "-----begin ec private key-----",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("%s log contains forbidden secret shape %q", owner, forbidden)
		}
	}
}

func m7AlphaWaitHeartbeatWarmup(t *testing.T, runtime *gatewayBootstrapCloser, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		warmed := 0
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			if snapshot.CurrentControlSession && snapshot.LastHeartbeatAt.After(snapshot.ConnectedAt) {
				warmed++
			}
		}
		if warmed == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("M7-10 Control Sessions did not complete warm-up heartbeat: want=%d snapshots=%+v", want, runtime.sessions.RuntimeStatusSnapshots())
}

func m7AlphaWaitActive(t *testing.T, runtime *gatewayBootstrapCloser, want uint64, timeout time.Duration) serverlimits.Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current := runtime.limits.Snapshot()
		if current.ActiveTotal == want && current.PendingOpens == 0 {
			return current
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.limits.Snapshot()
}

func m7AlphaConnectionCount(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(m7AlphaConnectionsEnvironment))
	if raw == "" {
		return 10
	}
	value, err := strconv.Atoi(raw)
	if err != nil || (value != 10 && value != 1000 && value != 5000) {
		t.Fatalf("%s=%q must be 10, 1000, or 5000", m7AlphaConnectionsEnvironment, raw)
	}
	return value
}

type m7AlphaEchoOrigin struct {
	listener net.Listener
	done     chan error
	wait     sync.WaitGroup
}

func startM7AlphaEchoOrigin(t *testing.T) *m7AlphaEchoOrigin {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen M7-10 Origin: %v", err)
	}
	origin := &m7AlphaEchoOrigin{listener: listener, done: make(chan error, 1)}
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				if errors.Is(acceptErr, net.ErrClosed) {
					origin.done <- nil
				} else {
					origin.done <- acceptErr
				}
				return
			}
			origin.wait.Add(1)
			go func() {
				defer origin.wait.Done()
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-origin.done; err != nil {
			t.Errorf("close M7-10 Origin: %v", err)
		}
		origin.wait.Wait()
	})
	return origin
}

func reserveM7AlphaPorts(t *testing.T, count int) ([]net.Listener, []uint16) {
	t.Helper()
	listeners := make([]net.Listener, 0, count)
	ports := make([]uint16, 0, count)
	for range count {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve M7-10 public port: %v", err)
		}
		listeners = append(listeners, listener)
		ports = append(ports, uint16(listener.Addr().(*net.TCPAddr).Port))
	}
	sort.Slice(ports, func(left, right int) bool { return ports[left] < ports[right] })
	return listeners, ports
}

func seedM7AlphaShards(t *testing.T, ctx context.Context, resources *serverStorage, origin net.Addr, ports []uint16) []m7AlphaShard {
	t.Helper()
	host, port := productGateHostPort(t, origin)
	shards := make([]m7AlphaShard, 0, len(ports))
	err := resources.database.WithTx(ctx, func(transaction repository.TxStore) error {
		for index, publicPort := range ports {
			tunnelID, err := identity.NewTunnelID()
			if err != nil {
				return err
			}
			serviceID, err := identity.NewServiceID()
			if err != nil {
				return err
			}
			if err := transaction.Tunnels().Create(ctx, repository.Tunnel{ID: tunnelID, Name: fmt.Sprintf("m7-alpha-%d", index), Version: 1, DesiredRevision: 1, CreatedAt: 1, UpdatedAt: 1}); err != nil {
				return err
			}
			if err := transaction.Services().Create(ctx, repository.Service{ID: serviceID, TunnelID: tunnelID, Name: "scale-tcp", RequiredRevision: 1, OriginScheme: repository.OriginSchemeTCP, OriginHost: host, OriginPort: port, TLSVerify: true, ConnectTimeoutMS: 5000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1}); err != nil {
				return err
			}
			if err := transaction.Routes().CreateTCP(ctx, repository.TCPRoute{ID: fmt.Sprintf("m7-alpha-route-%d", index), ServiceID: serviceID, PublicPort: publicPort, Enabled: true, CreatedAt: 1, UpdatedAt: 1}); err != nil {
				return err
			}
			shards = append(shards, m7AlphaShard{tunnelID: tunnelID, service: serviceID, address: net.JoinHostPort("127.0.0.1", strconv.Itoa(int(publicPort)))})
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 0)
		return err
	})
	if err != nil {
		t.Fatalf("seed M7-10 shards: %v", err)
	}
	return shards
}

func issueM7AlphaToken(t *testing.T, ctx context.Context, resources *serverStorage, gatewayAddress net.Addr, tunnelID string) string {
	t.Helper()
	identityValue, err := gateway.LoadOrCreatePinnedIdentity(resources.dataDir, "gateway.example.test", false, time.Now())
	if err != nil {
		t.Fatalf("load M7-10 pinned identity: %v", err)
	}
	protector, err := application.NewAES256GCMTokenProtector(resources.tokenMasterKey[:])
	if err != nil {
		t.Fatalf("construct M7-10 Token protector: %v", err)
	}
	host, port := productGateHostPort(t, gatewayAddress)
	pin := identityValue.SPKIHash()
	issued, err := application.NewConnectionTokenService(resources.database, protector).Issue(ctx, application.IssueConnectionTokenInput{TunnelID: tunnelID, Endpoint: &protocolv1.GatewayEndpoint{Host: host, Port: port}, TLSTrust: &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PinnedSpkiSha256{PinnedSpkiSha256: &protocolv1.PinnedSPKITrust{SpkiSha256: pin[:]}}}})
	if err != nil {
		t.Fatalf("issue M7-10 Token: %v", err)
	}
	return issued.Token
}

type m7AlphaAgent struct {
	connectorID string
	logs        *bytes.Buffer
	cancel      context.CancelFunc
	done        chan error
	once        sync.Once
}

func startM7AlphaAgent(t *testing.T, shard m7AlphaShard, runtime *gatewayBootstrapCloser, serverLogs *bytes.Buffer) *m7AlphaAgent {
	t.Helper()
	config, err := connector.HostConfig(shard.token, "v0.1.0-m7-alpha-scale")
	if err != nil {
		t.Fatalf("build M7-10 Agent config: %v", err)
	}
	logs := &bytes.Buffer{}
	config.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	agentRuntime, err := connector.New(config)
	if err != nil {
		t.Fatalf("construct M7-10 Agent: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	agent := &m7AlphaAgent{connectorID: config.Connector.ID(), logs: logs, cancel: cancel, done: make(chan error, 1)}
	go func() { agent.done <- agentRuntime.Run(ctx) }()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			service, exists := snapshot.Config.Services[shard.service]
			if snapshot.TunnelID == shard.tunnelID && snapshot.ConnectorID == agent.connectorID && snapshot.CurrentControlSession && snapshot.Config.ConfigReady && exists && service.Enabled && snapshot.WorkPool.Idle >= 1 {
				return agent
			}
		}
		select {
		case runErr := <-agent.done:
			cancel()
			t.Fatalf("M7-10 Agent exited before ready: %v", runErr)
		case <-deadline.C:
			cancel()
			t.Fatalf("M7-10 Agent did not become ready: snapshots=%+v agent_logs=%s server_logs=%s", runtime.sessions.RuntimeStatusSnapshots(), agent.logs.String(), serverLogs.String())
		case <-ticker.C:
		}
	}
}

func (agent *m7AlphaAgent) stop(t *testing.T) {
	t.Helper()
	agent.once.Do(func() {
		agent.cancel()
		select {
		case err := <-agent.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("stop M7-10 Agent: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("M7-10 Agent did not stop")
		}
	})
}

func m7AlphaMeasureDirectRTT(t *testing.T, address string) []time.Duration {
	t.Helper()
	values := make([]time.Duration, 0, 100)
	for index := range 100 {
		startedAt := time.Now()
		connection, err := net.DialTimeout("tcp4", address, time.Second)
		if err != nil {
			t.Fatalf("dial M7-10 direct Origin: %v", err)
		}
		payload := []byte{byte(index)}
		if _, err := connection.Write(payload); err != nil {
			_ = connection.Close()
			t.Fatalf("write M7-10 direct Origin: %v", err)
		}
		echoed := make([]byte, 1)
		if _, err := io.ReadFull(connection, echoed); err != nil || echoed[0] != payload[0] {
			_ = connection.Close()
			t.Fatalf("read M7-10 direct Origin: %v", err)
		}
		values = append(values, time.Since(startedAt))
		if err := connection.Close(); err != nil {
			t.Fatalf("close M7-10 direct Origin: %v", err)
		}
	}
	return values
}

func m7AlphaMeasureTunnelOpen(t *testing.T, shards []m7AlphaShard) []time.Duration {
	t.Helper()
	values := make([]time.Duration, 0, 100)
	for index := range 100 {
		shard := shards[index%len(shards)]
		startedAt := time.Now()
		connection, err := net.DialTimeout("tcp4", shard.address, 5*time.Second)
		if err != nil {
			t.Fatalf("dial M7-10 public latency sample: %v", err)
		}
		if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			_ = connection.Close()
			t.Fatalf("set M7-10 public latency deadline: %v", err)
		}
		payload := []byte("m7-alpha-open")
		if _, err := connection.Write(payload); err != nil {
			_ = connection.Close()
			t.Fatalf("write M7-10 public latency sample: %v", err)
		}
		echoed := make([]byte, len(payload))
		if _, err := io.ReadFull(connection, echoed); err != nil || string(echoed) != string(payload) {
			_ = connection.Close()
			t.Fatalf("read M7-10 public latency sample: %v", err)
		}
		values = append(values, time.Since(startedAt))
		if err := connection.Close(); err != nil {
			t.Fatalf("close M7-10 public latency sample: %v", err)
		}
	}
	return values
}

func m7AlphaPercentile(values []time.Duration, percentile int) time.Duration {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left] < sorted[right] })
	index := (len(sorted)*percentile + 99) / 100
	if index < 1 {
		index = 1
	}
	return sorted[index-1]
}

func m7AlphaWaitSettled(t *testing.T, runtime *gatewayBootstrapCloser, shards []m7AlphaShard, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		limits := runtime.limits.Snapshot()
		activeWork := 0
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			for _, shard := range shards {
				if snapshot.TunnelID == shard.tunnelID && snapshot.CurrentControlSession {
					activeWork += int(snapshot.WorkPool.Active)
				}
			}
		}
		if limits.PendingOpens == 0 && limits.ActiveTotal == 0 && activeWork == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("M7-10 connections did not settle: %+v", runtime.limits.Snapshot())
}

func m7AlphaWaitResources(t *testing.T, baseline m7ResourceSample, timeout time.Duration) m7ResourceSample {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var current m7ResourceSample
	for time.Now().Before(deadline) {
		var err error
		current, err = m7ReadResources()
		if err != nil {
			t.Fatalf("read settled M7-10 resources: %v", err)
		}
		if current.FDs <= baseline.FDs+10 && current.Goroutines <= baseline.Goroutines+20 {
			return current
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("M7-10 resources did not settle: baseline=%+v current=%+v", baseline, current)
	return current
}
