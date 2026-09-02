//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/connector"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

const m7ReleasePreviousSchemaVersion = 10

// TestM7ReleaseBackupMigrationRestoreAgentReconnect 串联 M7-09 的状态恢复主路径：
// 上一版 Schema 的离线备份先经正常 Server 启动前向迁移，再由维护命令原样恢复；
// 恢复后的数据库仍由下一次正常启动迁移，Agent 不保存 revision 或业务配置，只用
// 原 Connection Token 建立新 Session、重新取得完整 Snapshot，并恢复公网数据面。
func TestM7ReleaseBackupMigrationRestoreAgentReconnect(t *testing.T) {
	requireRootTest(t)
	if sqlite.CurrentSchemaVersion() != m7ReleasePreviousSchemaVersion+1 {
		t.Fatalf(
			"M7 release fixture covers schema %d -> %d, current schema is %d; update the explicit previous-release fixture",
			m7ReleasePreviousSchemaVersion,
			m7ReleasePreviousSchemaVersion+1,
			sqlite.CurrentSchemaVersion(),
		)
	}

	ctx := context.Background()
	runtimeDir := newShortRuntimeDirectory(t)
	dataDir := t.TempDir()
	httpOrigin := httptest.NewServer(nil)
	t.Cleanup(httpOrigin.Close)
	tcpOrigin := startProductGateTCPOrigin(t)

	gatewayAddress, _ := reserveProductGateTCPPort(t)
	publicAddress, publicPort := reserveProductGateTCPPort(t)
	for publicAddress == gatewayAddress {
		publicAddress, publicPort = reserveProductGateTCPPort(t)
	}
	gatewayTCPAddress, err := net.ResolveTCPAddr("tcp4", gatewayAddress)
	if err != nil {
		t.Fatalf("resolve M7 release Gateway address: %v", err)
	}

	resources := initializeBackupCommandData(t, ctx, dataDir, runtimeDir)
	seedProductGateDesiredState(
		t, ctx, resources, httpOrigin.Listener.Addr(), tcpOrigin.listener.Addr(), publicPort,
	)
	if err := resources.database.CreateFirstAdmin(ctx, "admin", "m7 release backup restore password"); err != nil {
		_ = resources.Close()
		t.Fatalf("create M7 release Admin: %v", err)
	}
	connectionToken := issueProductGateToken(t, ctx, resources, gatewayTCPAddress)
	if err := resources.Close(); err != nil {
		t.Fatalf("close M7 release fixture storage: %v", err)
	}

	downgradeM7ReleaseFixture(t, dataDir)
	configPath := writeConfig(
		t,
		"management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: gateway.example.test\n",
	)
	archivePath := filepath.Join(t.TempDir(), "m7-release-backup.tar")
	commonArgs := []string{"--config", configPath, "--set", "server.data_dir=" + dataDir}
	var logs bytes.Buffer
	if err := runBackupCreate(
		ctx,
		"xtunnel-server",
		append([]string{"--output", archivePath}, commonArgs...),
		nil,
		&logs,
		runtimeDir,
	); err != nil {
		t.Fatalf("create M7 release schema-%d backup: %v", m7ReleasePreviousSchemaVersion, err)
	}
	assertM7ReleaseMaintenanceLog(t, logs.String(), "backup_create_completed")

	// 同一个 Agent runtime 贯穿三个 Server 生命周期。第一次启动证明恢复点可以由
	// 当前二进制迁移并正常提供数据面；随后离线推进到 revision 2，让 Agent 在新
	// Session 明确观察更高基线，避免 Restore 后的 revision 1 只是首次配置。
	server := startM7ReleaseServer(t, dataDir, runtimeDir, gatewayAddress, publicPort, "after migration")
	defer server.closeForCleanup(t)
	agent := startM7ReleaseAgent(t, connectionToken)
	defer agent.closeForCleanup(t)
	waitM7ReleaseAgentReady(t, agent, server.runtime, 1, 1, "after migration")
	assertM7ReleaseDataPlane(t, server.runtime, publicAddress, tcpOrigin, "after migration")
	server.close(t)

	advanceM7ReleaseDesiredRevision(t, dataDir)
	server = startM7ReleaseServer(t, dataDir, runtimeDir, gatewayAddress, publicPort, "at revision 2")
	defer server.closeForCleanup(t)
	waitM7ReleaseAgentReady(t, agent, server.runtime, 2, 2, "at revision 2")
	assertM7ReleaseDataPlane(t, server.runtime, publicAddress, tcpOrigin, "at revision 2")
	server.close(t)

	logs.Reset()
	if err := runBackupRestore(
		ctx,
		"xtunnel-server",
		append([]string{"--input", archivePath}, commonArgs...),
		nil,
		&logs,
		runtimeDir,
	); err != nil {
		t.Fatalf("restore M7 release schema-%d backup: %v", m7ReleasePreviousSchemaVersion, err)
	}
	assertM7ReleaseMaintenanceLog(t, logs.String(), "backup_restore_completed")
	assertM7ReleaseSchemaVersion(t, dataDir, m7ReleasePreviousSchemaVersion)

	server = startM7ReleaseServer(t, dataDir, runtimeDir, gatewayAddress, publicPort, "after restore")
	defer server.closeForCleanup(t)
	waitM7ReleaseAgentReady(t, agent, server.runtime, 1, 1, "after restore")
	assertM7ReleaseDataPlane(t, server.runtime, publicAddress, tcpOrigin, "after restore")
	server.close(t)
	agent.close(t)
}

// downgradeM7ReleaseFixture 从当前生产 Schema 精确撤销 v11 的独占对象，构造仓库内
// 可复现的 v10 发布输入。显式版本 Gate 会在新增 Migration 时要求维护者重新审阅，
// 禁止把任意删表伪装成一份仍受支持的历史发布数据库。
func downgradeM7ReleaseFixture(t *testing.T, dataDir string) {
	t.Helper()
	database := openGatewayAuditDatabase(t, dataDir)
	for _, statement := range []string{
		"DROP TABLE usage_minutes",
		"DROP TABLE usage_hours",
		"DROP TABLE usage_days",
		"DELETE FROM schema_migrations WHERE version = 11",
		"PRAGMA auto_vacuum = NONE",
		"VACUUM",
	} {
		if err := database.Exec(statement).Error; err != nil {
			closeGatewayAuditDatabase(t, database)
			t.Fatalf("construct M7 release schema-%d fixture with %q: %v", m7ReleasePreviousSchemaVersion, statement, err)
		}
	}
	closeGatewayAuditDatabase(t, database)
	assertM7ReleaseSchemaVersion(t, dataDir, m7ReleasePreviousSchemaVersion)
}

// advanceM7ReleaseDesiredRevision 在 Server 离线且 External Lock 已释放后，用生产
// Repository 事务把一个 Service 与 Tunnel Snapshot 一起推进到 revision 2。
// Restore 会完整替换这次写入，使同一 Agent 必须在新 Session 接受较低 revision。
func advanceM7ReleaseDesiredRevision(t *testing.T, dataDir string) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("open M7 release storage for revision advance: %v", err)
	}
	updateErr := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		service, err := transaction.Services().Get(context.Background(), productGateTunnelID, productGateTCPServiceID)
		if err != nil {
			return err
		}
		service.RequiredRevision = 2
		service.UpdatedAt = 2
		if _, err := transaction.Services().Update(context.Background(), service, service.Version); err != nil {
			return err
		}
		_, err = transaction.Tunnels().AdvanceDesiredRevision(context.Background(), productGateTunnelID, 1, 1, 2)
		return err
	})
	closeErr := store.Close()
	if err := errors.Join(updateErr, closeErr); err != nil {
		t.Fatalf("advance M7 release Desired Revision: %v", err)
	}
}

type m7ReleaseServer struct {
	runtime   *gatewayBootstrapCloser
	resources *serverStorage
	closer    io.Closer
	cancel    context.CancelFunc

	closeOnce sync.Once
	closeErr  error
}

func startM7ReleaseServer(
	t *testing.T,
	dataDir string,
	runtimeDir string,
	gatewayAddress string,
	publicPort uint16,
	phase string,
) *m7ReleaseServer {
	t.Helper()
	serverContext, cancelServer := context.WithCancel(context.Background())
	resources, err := openServerStorage(serverContext, dataDir, runtimeDir)
	if err != nil {
		cancelServer()
		t.Fatalf("open M7 release Server storage %s: %v", phase, err)
	}
	assertM7ReleaseSchemaVersion(t, dataDir, sqlite.CurrentSchemaVersion())

	config := gatewayLifecycleTestConfig(dataDir, gatewayAddress)
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)
	config.Limits.MaxPendingTLSHandshakes = 16
	config.Limits.MaxPendingAuth = 16
	closer, err := openGatewayAndBootstrapWith(
		serverContext,
		config,
		resources,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
		runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("existing Admin unexpectedly opened Bootstrap Socket")
		},
	)
	if err != nil {
		cancelServer()
		t.Fatalf("start M7 release Server runtime %s: %v", phase, errors.Join(err, resources.Close()))
	}
	runtime, ok := closer.(*gatewayBootstrapCloser)
	if !ok {
		cancelServer()
		t.Fatalf("M7 release Server runtime %s has unexpected type: %v", phase, errors.Join(closer.Close(), resources.Close()))
	}
	return &m7ReleaseServer{
		runtime: runtime, resources: resources, closer: closer, cancel: cancelServer,
	}
}

func (server *m7ReleaseServer) stop() error {
	server.closeOnce.Do(func() {
		server.closeErr = errors.Join(server.closer.Close(), server.resources.Close())
		server.cancel()
	})
	return server.closeErr
}

func (server *m7ReleaseServer) close(t *testing.T) {
	t.Helper()
	if err := server.stop(); err != nil {
		t.Fatalf("close M7 release Server: %v", err)
	}
}

func (server *m7ReleaseServer) closeForCleanup(t *testing.T) {
	t.Helper()
	if err := server.stop(); err != nil {
		t.Errorf("cleanup M7 release Server: %v", err)
	}
}

type m7ReleaseAgent struct {
	connectorID string
	cancel      context.CancelFunc
	done        chan struct{}

	resultMu sync.Mutex
	result   error
	stopOnce sync.Once
}

func startM7ReleaseAgent(t *testing.T, connectionToken string) *m7ReleaseAgent {
	t.Helper()
	config, err := connector.HostConfig(connectionToken, "v0.1.0-m7-release")
	if err != nil {
		t.Fatalf("build M7 release Agent config: %v", err)
	}
	config.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	runtime, err := connector.New(config)
	if err != nil {
		t.Fatalf("construct M7 release Agent runtime: %v", err)
	}
	agentContext, cancelAgent := context.WithCancel(context.Background())
	agent := &m7ReleaseAgent{
		connectorID: config.Connector.ID(), cancel: cancelAgent, done: make(chan struct{}),
	}
	go func() {
		runErr := runtime.Run(agentContext)
		agent.resultMu.Lock()
		agent.result = runErr
		agent.resultMu.Unlock()
		close(agent.done)
	}()
	return agent
}

func (agent *m7ReleaseAgent) runResult() error {
	agent.resultMu.Lock()
	defer agent.resultMu.Unlock()
	return agent.result
}

func (agent *m7ReleaseAgent) stop() error {
	agent.stopOnce.Do(agent.cancel)
	select {
	case <-agent.done:
		runErr := agent.runResult()
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			return runErr
		}
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("M7 release Agent did not stop after cancellation")
	}
}

func (agent *m7ReleaseAgent) close(t *testing.T) {
	t.Helper()
	if err := agent.stop(); err != nil {
		t.Fatalf("close M7 release Agent: %v", err)
	}
}

func (agent *m7ReleaseAgent) closeForCleanup(t *testing.T) {
	t.Helper()
	if err := agent.stop(); err != nil {
		t.Errorf("cleanup M7 release Agent: %v", err)
	}
}

func waitM7ReleaseAgentReady(
	t *testing.T,
	agent *m7ReleaseAgent,
	runtime *gatewayBootstrapCloser,
	wantRevision uint64,
	wantTCPRevision uint64,
	phase string,
) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, snapshot := range runtime.sessions.RuntimeStatusSnapshots() {
			httpService, hasHTTP := snapshot.Config.Services[productGateHTTPServiceID]
			tcpService, hasTCP := snapshot.Config.Services[productGateTCPServiceID]
			if snapshot.TunnelID == productGateTunnelID && snapshot.ConnectorID == agent.connectorID &&
				snapshot.CurrentControlSession && snapshot.Config.ConfigReady && snapshot.Config.HasObserved &&
				snapshot.Config.ObservedRevision == wantRevision && hasHTTP && hasTCP &&
				httpService.Enabled && httpService.RequiredRevision == 1 &&
				tcpService.Enabled && tcpService.RequiredRevision == wantTCPRevision && snapshot.WorkPool.Idle >= 1 {
				return
			}
		}
		select {
		case <-agent.done:
			t.Fatalf("M7 release Agent exited before ready %s: %v", phase, agent.runResult())
		case <-deadline.C:
			t.Fatalf(
				"M7 release Agent %s did not reconnect as Connector %s with full revision %d Snapshot",
				phase,
				agent.connectorID,
				wantRevision,
			)
		case <-ticker.C:
		}
	}
}

func assertM7ReleaseDataPlane(
	t *testing.T,
	runtime *gatewayBootstrapCloser,
	publicAddress string,
	tcpOrigin *productGateTCPOrigin,
	phase string,
) {
	t.Helper()
	connection := dialProductGateTCP(t, publicAddress, "127.0.0.1")
	echoDone := startProductGateOriginEcho(tcpOrigin.next(t, phase))
	assertProductGateRoundTrip(t, connection, []byte("m7-release-"+phase), phase)
	finishProductGateTCP(t, connection, echoDone, phase)
	waitForProductGateNoActiveWork(t, runtime)
}

func assertM7ReleaseSchemaVersion(t *testing.T, dataDir string, want int) {
	t.Helper()
	version, err := sqlite.InspectSchemaVersion(context.Background(), filepath.Join(dataDir, "xtunnel.db"))
	if err != nil {
		t.Fatalf("inspect M7 release SQLite schema: %v", err)
	}
	if version != want {
		t.Fatalf("M7 release SQLite schema = %d, want %d", version, want)
	}
}

func assertM7ReleaseMaintenanceLog(t *testing.T, output, event string) {
	t.Helper()
	if !strings.Contains(output, `"event":"`+event+`"`) || !strings.Contains(output, `"mode":"offline"`) {
		t.Fatalf("M7 release maintenance log for %s = %s", event, output)
	}
}
