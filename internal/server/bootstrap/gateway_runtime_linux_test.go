//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libsqlite "github.com/libtnb/sqlite"
	"gorm.io/gorm"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
)

const gatewayLockHelperEnvironment = "XTUNNEL_GATEWAY_ROTATE_LOCK_HELPER"

func TestServerStartupReconcilesGatewayRotationAuditBeforeBootstrap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	store, err := sqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	now := time.Date(2026, time.August, 26, 0, 0, 0, 0, time.UTC)
	if _, err := gateway.LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	audit := gateway.RotationAuditMetadata{
		EventID: "evt_01J00000000000000000000021", OperationID: "op_01J00000000000000000000021",
		OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
	}
	if _, err := gateway.RotatePinnedIdentity(dataDir, "gateway.example.test", now.Add(time.Hour), audit); err != nil {
		t.Fatalf("RotatePinnedIdentity() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}
	configPath := writeConfig(t, "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: gateway.example.test\n")
	bootstrapCalled := false
	err = runWithStorageAndBootstrap(
		ctx,
		"xtunnel-server",
		[]string{"--config", configPath, "--set", "server.data_dir=" + dataDir},
		nil,
		&bytes.Buffer{},
		func(ctx context.Context, dataDir string) (storage, error) {
			return openServerStorage(ctx, dataDir, runtimeDir)
		},
		func(context.Context, serverconfig.Config, storage, *slog.Logger) (io.Closer, error) {
			bootstrapCalled = true
			if _, exists, err := gateway.PendingRotationAuditEvent(dataDir); err != nil || exists {
				t.Fatalf("PendingRotationAuditEvent() in bootstrap = exists %t, error %v", exists, err)
			}
			database := openGatewayAuditDatabase(t, dataDir)
			defer closeGatewayAuditDatabase(t, database)
			var count int64
			if err := database.Table(sqlite.SecurityAuditEventTable).Count(&count).Error; err != nil {
				t.Fatalf("count reconciled audit events error = %v", err)
			}
			if count != 1 {
				t.Fatalf("audit event count before bootstrap = %d, want 1", count)
			}
			cancel()
			return nil, nil
		},
	)
	if err != nil {
		t.Fatalf("runWithStorageAndBootstrap() error = %v", err)
	}
	if !bootstrapCalled {
		t.Fatal("bootstrap callback was not called")
	}
}

// TestFirstAdminCreationStartsGateway 验证首个管理员提交成功之后才真正开始监听 Gateway。
// 测试使用允许任意本机连接的测试授权器，避免把运行时 root 对等校验误当作生命周期结论。
func TestFirstAdminCreationStartsGateway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	resources, err := openServerStorage(ctx, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage() error = %v", err)
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Errorf("server storage Close() error = %v", err)
		}
	})
	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	closer, err := openGatewayAndBootstrapWith(
		ctx,
		config,
		resources,
		slog.Default(),
		runtimeDir,
		func(ctx context.Context, runtimeDir, targetHash string, store *sqlite.Store, afterCreate func() error, reportRuntimeError func(error)) (io.Closer, error) {
			return openAdminBootstrapSocketWithRuntime(ctx, runtimeDir, targetHash, store, func(*net.UnixConn) error { return nil }, afterCreate, reportRuntimeError)
		},
	)
	if err != nil {
		t.Fatalf("openGatewayAndBootstrapWith() error = %v", err)
	}
	gatewayCloser, ok := closer.(*gatewayBootstrapCloser)
	if !ok {
		t.Fatalf("gateway closer type = %T, want *gatewayBootstrapCloser", closer)
	}
	t.Cleanup(func() {
		if err := closer.Close(); err != nil {
			t.Errorf("gateway Bootstrap Closer Close() error = %v", err)
		}
	})
	if address := gatewayCloser.gateway.Addr(); address != nil {
		t.Fatalf("gateway address before first admin = %v, want nil", address)
	}
	if actual := gatewayCloser.tcpIngress.Actual(); len(actual) != 0 {
		t.Fatalf("TCP listeners before first admin = %v, want none", actual)
	}
	if gatewayCloser.routes == nil || gatewayCloser.routes.Current() == nil {
		t.Fatal("immutable route snapshot was not loaded before first admin bootstrap")
	}
	if generation := gatewayCloser.routes.Current().Generation(); generation != 0 {
		t.Fatalf("initial route snapshot generation = %d, want 0", generation)
	}

	socketPath := filepath.Join(runtimeDir, adminBootstrapSocketName)
	handled, err := requestAdminBootstrap(ctx, socketPath, resources.targetHash, "admin", "gateway lifecycle password")
	if !handled || err != nil {
		t.Fatalf("requestAdminBootstrap() = handled %t, error %v", handled, err)
	}
	address := gatewayCloser.gateway.Addr()
	if address == nil {
		t.Fatal("gateway did not start after first admin creation")
	}
	connection, err := net.DialTimeout("tcp", address.String(), time.Second)
	if err != nil {
		t.Fatalf("dial started gateway %q error = %v", address, err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close gateway test connection error = %v", err)
	}
}

// TestTCPIngressWaitsForFirstAdminAndRestoresAfterRestart 从真实 SQLite Desired State
// 穿过 Bootstrap Gate 和生产 Listener 接线，证明 SETUP_REQUIRED 不会提前占用端口，
// 首个 Admin 提交后开始监听，完整关闭并重开后又能从同一持久化 Route 恢复。
func TestTCPIngressWaitsForFirstAdminAndRestoresAfterRestart(t *testing.T) {
	ctx := context.Background()
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()

	portProbe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP Route port error = %v", err)
	}
	publicPort := uint16(portProbe.Addr().(*net.TCPAddr).Port)
	publicAddress := portProbe.Addr().String()
	if err := portProbe.Close(); err != nil {
		t.Fatalf("release TCP Route port error = %v", err)
	}

	config := gatewayLifecycleTestConfig(dataDir, "127.0.0.1:0")
	config.TCPIngress.MinPort = int(publicPort)
	config.TCPIngress.MaxPort = int(publicPort)

	resources, err := openServerStorage(ctx, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage() error = %v", err)
	}
	serverResources := resources
	const tunnelID = "tun_01J00000000000000000000050"
	const serviceID = "svc_01J00000000000000000000050"
	if err := serverResources.database.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(ctx, repository.Tunnel{
			ID: tunnelID, Name: "tcp bootstrap", Version: 1, DesiredRevision: 1,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		if err := transaction.Services().Create(ctx, repository.Service{
			ID: serviceID, TunnelID: tunnelID, Name: "tcp origin", RequiredRevision: 1,
			OriginScheme: repository.OriginSchemeTCP, OriginHost: "127.0.0.1", OriginPort: 22,
			ConnectTimeoutMS: 5_000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		if err := transaction.Routes().CreateTCP(ctx, repository.TCPRoute{
			ID: "tcp-bootstrap", ServiceID: serviceID, PublicPort: publicPort,
			Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		_, err := transaction.Routes().AdvanceGeneration(ctx, 0)
		return err
	}); err != nil {
		_ = resources.Close()
		t.Fatalf("seed Bootstrap TCP Desired State error = %v", err)
	}

	closer, err := openGatewayAndBootstrapWith(
		ctx, config, resources, slog.Default(), runtimeDir,
		func(ctx context.Context, runtimeDir, targetHash string, store *sqlite.Store, afterCreate func() error, reportRuntimeError func(error)) (io.Closer, error) {
			return openAdminBootstrapSocketWithRuntime(ctx, runtimeDir, targetHash, store, func(*net.UnixConn) error { return nil }, afterCreate, reportRuntimeError)
		},
	)
	if err != nil {
		_ = resources.Close()
		t.Fatalf("openGatewayAndBootstrapWith() before first Admin error = %v", err)
	}
	firstRuntime := closer.(*gatewayBootstrapCloser)
	if actual := firstRuntime.tcpIngress.Actual(); len(actual) != 0 {
		t.Fatalf("TCP listeners before first Admin = %+v, want none", actual)
	}
	available, err := net.Listen("tcp4", publicAddress)
	if err != nil {
		t.Fatalf("TCP Route port was occupied during SETUP_REQUIRED: %v", err)
	}
	if err := available.Close(); err != nil {
		t.Fatalf("close SETUP_REQUIRED port probe error = %v", err)
	}
	if handled, err := requestAdminBootstrap(ctx, filepath.Join(runtimeDir, adminBootstrapSocketName), serverResources.targetHash, "admin", "tcp bootstrap password"); !handled || err != nil {
		t.Fatalf("requestAdminBootstrap() = handled %t, error %v", handled, err)
	}
	if actual := firstRuntime.tcpIngress.Actual(); len(actual) != 1 || actual[0].Route.ID != "tcp-bootstrap" || actual[0].Address != publicAddress {
		t.Fatalf("TCP listeners after first Admin = %+v", actual)
	}
	if conflict, err := net.Listen("tcp4", publicAddress); err == nil {
		_ = conflict.Close()
		t.Fatal("TCP Route port remained bindable after first Admin")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close first Bootstrap runtime error = %v", err)
	}
	if err := resources.Close(); err != nil {
		t.Fatalf("close first Server storage error = %v", err)
	}

	restartedResources, err := openServerStorage(ctx, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("reopen Server storage error = %v", err)
	}
	bootstrapOpened := false
	restartedCloser, err := openGatewayAndBootstrapWith(
		ctx, config, restartedResources, slog.Default(), runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			bootstrapOpened = true
			return nil, nil
		},
	)
	if err != nil {
		_ = restartedResources.Close()
		t.Fatalf("openGatewayAndBootstrapWith() after restart error = %v", err)
	}
	if bootstrapOpened {
		t.Fatal("existing Admin restart reopened first-admin Bootstrap Socket")
	}
	restartedRuntime := restartedCloser.(*gatewayBootstrapCloser)
	if actual := restartedRuntime.tcpIngress.Actual(); len(actual) != 1 || actual[0].Route.ID != "tcp-bootstrap" || actual[0].Address != publicAddress {
		t.Fatalf("restored TCP listeners = %+v", actual)
	}
	if err := restartedCloser.Close(); err != nil {
		t.Fatalf("close restarted Bootstrap runtime error = %v", err)
	}
	if err := restartedResources.Close(); err != nil {
		t.Fatalf("close restarted Server storage error = %v", err)
	}
}

// TestFirstAdminGatewayStartFailureStopsBootstrapAndExitsRun 锁定“Admin 事务已提交，
// Gateway 绑定失败”的不可回滚边界：当前进程必须停止 Bootstrap 并返回错误，
// 重启后则应识别已有 Admin，直接启动 Gateway。
func TestFirstAdminGatewayStartFailureStopsBootstrapAndExitsRun(t *testing.T) {
	runContext, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	blockedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve blocked gateway address error = %v", err)
	}
	blockedAddress := blockedListener.Addr().String()
	listenerClosed := false
	t.Cleanup(func() {
		if !listenerClosed {
			_ = blockedListener.Close()
		}
	})

	target, err := datadir.Resolve(dataDir)
	if err != nil {
		t.Fatalf("datadir.Resolve() error = %v", err)
	}
	configPath := writeConfig(t, "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: gateway.example.test\n")
	gatewayConfig := gatewayLifecycleTestConfig(dataDir, blockedAddress)
	runDone := make(chan error, 1)
	go func() {
		runDone <- runWithStorageAndBootstrap(
			runContext,
			"xtunnel-server",
			[]string{"--config", configPath, "--set", "server.data_dir=" + dataDir},
			nil,
			&bytes.Buffer{},
			func(ctx context.Context, dataDir string) (storage, error) {
				return openServerStorage(ctx, dataDir, runtimeDir)
			},
			func(ctx context.Context, _ serverconfig.Config, resources storage, logger *slog.Logger) (io.Closer, error) {
				return openGatewayAndBootstrapWith(
					ctx,
					gatewayConfig,
					resources,
					logger,
					runtimeDir,
					func(ctx context.Context, runtimeDir, targetHash string, store *sqlite.Store, afterCreate func() error, reportRuntimeError func(error)) (io.Closer, error) {
						return openAdminBootstrapSocketWithRuntime(ctx, runtimeDir, targetHash, store, func(*net.UnixConn) error { return nil }, afterCreate, reportRuntimeError)
					},
				)
			},
		)
	}()

	socketPath := filepath.Join(runtimeDir, adminBootstrapSocketName)
	waitForFile(t, socketPath, 2*time.Second)
	handled, requestErr := requestAdminBootstrap(
		runContext,
		socketPath,
		target.Hash,
		"admin",
		"gateway failure lifecycle password",
	)
	if !handled || requestErr == nil {
		t.Fatalf("requestAdminBootstrap() = handled %t, error %v; want handled rejection", handled, requestErr)
	}
	select {
	case runErr := <-runDone:
		if runErr == nil || !strings.Contains(runErr.Error(), "start agent gateway after HTTP ingress") {
			t.Fatalf("runWithStorageAndBootstrap() error = %v, want gateway startup failure", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWithStorageAndBootstrap() did not exit after Gateway startup failure")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Bootstrap Socket remained after Gateway startup failure: os.Lstat() error = %v", err)
	}

	if err := blockedListener.Close(); err != nil {
		t.Fatalf("release blocked gateway address error = %v", err)
	}
	listenerClosed = true

	restartContext, cancelRestart := context.WithCancel(context.Background())
	defer cancelRestart()
	restartedResources, err := openServerStorage(restartContext, dataDir, runtimeDir)
	if err != nil {
		t.Fatalf("openServerStorage() after failed first start error = %v", err)
	}
	restartedCloser, err := openGatewayAndBootstrapWith(
		restartContext,
		gatewayLifecycleTestConfig(dataDir, blockedAddress),
		restartedResources,
		slog.Default(),
		runtimeDir,
		func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
			return nil, errors.New("unexpected Bootstrap Socket after Admin was committed")
		},
	)
	if err != nil {
		_ = restartedResources.Close()
		t.Fatalf("openGatewayAndBootstrapWith() restart error = %v", err)
	}
	restartedGateway := restartedCloser.(*gatewayBootstrapCloser)
	if restartedGateway.gateway.Addr() == nil {
		t.Fatal("restart with existing Admin did not start Gateway")
	}
	if err := restartedCloser.Close(); err != nil {
		t.Errorf("restart gateway closer Close() error = %v", err)
	}
	if err := restartedResources.Close(); err != nil {
		t.Errorf("restart server storage Close() error = %v", err)
	}
}

func gatewayLifecycleTestConfig(dataDir, listen string) serverconfig.Config {
	return serverconfig.Config{
		Server:      serverconfig.Server{DataDir: dataDir},
		Management:  serverconfig.Management{Listen: "127.0.0.1:0"},
		HTTPIngress: serverconfig.HTTPIngress{Listen: "127.0.0.1:0"},
		AgentGateway: serverconfig.AgentGateway{
			Listen:         listen,
			PublicHostname: "gateway.example.test",
			TLS:            serverconfig.AgentGatewayTLS{Mode: gateway.PinnedMode},
		},
		TCPIngress: serverconfig.TCPIngress{Bind: "127.0.0.1", MinPort: 10000, MaxPort: 60000},
		Transport: serverconfig.Transport{TCP: serverconfig.TransportTCP{
			WorkAcquireTimeout: baseconfig.Duration{Duration: 2 * time.Second},
		}},
		Control: serverconfig.Control{
			HighPriorityQueue: 8, NormalQueue: 8,
			WriteTimeout: baseconfig.Duration{Duration: time.Second},
		},
		ConnectorRuntime: serverconfig.ConnectorRuntime{
			HeartbeatInterval: baseconfig.Duration{Duration: time.Second},
			HeartbeatTimeout:  baseconfig.Duration{Duration: 3 * time.Second},
		},
		Limits: serverconfig.Limits{
			MaxConnectors:                       8,
			MaxConnectorsPerTunnel:              4,
			MaxHealthTargetsPerTunnel:           2_000,
			MaxHealthTargetsGlobal:              50_000,
			MaxServicesPerTunnel:                1_000,
			MaxTunnelSnapshotBytes:              768 << 10,
			MaxPendingTLSHandshakes:             1,
			MaxPendingAuth:                      2,
			MaxReplayEntriesPerSession:          32,
			MaxWorkConnections:                  64,
			MaxIdleWorkConnections:              32,
			MaxConnectingWorkConnections:        16,
			MaxPendingOpens:                     16,
			MaxActiveConnections:                32,
			MaxConnectionsPerTunnel:             16,
			MaxConnectionsPerService:            16,
			MaxConnectionsPerSourceIP:           8,
			MaxOpenRatePerSourceIP:              50,
			MaxOpenBurstPerSourceIP:             100,
			MaxHTTPRequestsPerSourceIPPerSecond: 100,
			MaxControlFrameBytes:                1 << 20,
			MaxHTTPHeaderBytes:                  64 << 10,
			MaxHTTPBodyBytes:                    2 << 30,
		},
	}
}

// TestGatewayRotateKeyExternalLock 覆盖维护命令的真实跨进程 External Lock 互斥，
// 并确认冲突路径不会改变原有身份，释放锁后才允许完成离线换钥。
func TestGatewayRotateKeyExternalLock(t *testing.T) {
	if os.Getenv(gatewayLockHelperEnvironment) == "1" {
		runGatewayLockHelper(t)
		return
	}

	runtimeDir := newRuntimeDirectory(t)
	dataDir := t.TempDir()
	store, err := sqlite.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}
	before, err := gateway.LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, time.Now())
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	target, err := datadir.Resolve(dataDir)
	if err != nil {
		t.Fatalf("datadir.Resolve() error = %v", err)
	}
	readyPath := filepath.Join(t.TempDir(), "lock-ready")
	releasePath := filepath.Join(t.TempDir(), "lock-release")
	helper := exec.Command(os.Args[0], "-test.run=^TestGatewayRotateKeyExternalLock$")
	helper.Env = append(os.Environ(),
		gatewayLockHelperEnvironment+"=1",
		"XTUNNEL_GATEWAY_ROTATE_LOCK_RUNTIME_DIR="+runtimeDir,
		"XTUNNEL_GATEWAY_ROTATE_LOCK_TARGET_HASH="+target.Hash,
		"XTUNNEL_GATEWAY_ROTATE_LOCK_READY="+readyPath,
		"XTUNNEL_GATEWAY_ROTATE_LOCK_RELEASE="+releasePath,
	)
	var helperOutput bytes.Buffer
	helper.Stdout = &helperOutput
	helper.Stderr = &helperOutput
	if err := helper.Start(); err != nil {
		t.Fatalf("start external lock helper error = %v", err)
	}
	helperFinished := false
	t.Cleanup(func() {
		if helperFinished {
			return
		}
		if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil && !errors.Is(err, os.ErrExist) {
			t.Errorf("release external lock helper error = %v", err)
		}
		if err := helper.Wait(); err != nil {
			t.Errorf("external lock helper error = %v; output: %s", err, helperOutput.String())
		}
	})
	waitForFile(t, readyPath, 2*time.Second)

	configPath := writeConfig(t, "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: gateway.example.test\n")
	commandArgs := []string{"--maintenance", "--config", configPath, "--set", "server.data_dir=" + dataDir}
	startedAt := time.Now()
	err = runGatewayRotateKey(context.Background(), "xtunnel-server", commandArgs, nil, &bytes.Buffer{}, runtimeDir, time.Now())
	if !errors.Is(err, externallock.ErrAlreadyLocked) {
		t.Fatalf("runGatewayRotateKey() under external lock error = %v, want ErrAlreadyLocked", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= time.Second {
		t.Fatalf("runGatewayRotateKey() lock conflict took %s, want fast failure", elapsed)
	}
	afterConflict, err := gateway.LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after lock conflict error = %v", err)
	}
	if afterConflict.SPKIHash() != before.SPKIHash() {
		t.Fatal("lock-conflicted gateway rotation changed the pinned identity")
	}

	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("release external lock helper error = %v", err)
	}
	helperErr := helper.Wait()
	helperFinished = true
	if helperErr != nil {
		t.Fatalf("external lock helper error = %v; output: %s", helperErr, helperOutput.String())
	}
	// 进程已回收，Cleanup 不再重复 Wait，只保留路径清理由 TempDir 负责。

	rotationOutput := &bytes.Buffer{}
	rotationTime := time.Now().Add(time.Second)
	if err := runGatewayRotateKey(context.Background(), "xtunnel-server", commandArgs, nil, rotationOutput, runtimeDir, rotationTime); err != nil {
		t.Fatalf("runGatewayRotateKey() after lock release error = %v", err)
	}
	afterRotation, err := gateway.LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after successful rotation error = %v", err)
	}
	if afterRotation.SPKIHash() == before.SPKIHash() {
		t.Fatal("successful gateway rotation did not change pinned identity")
	}
	database := openGatewayAuditDatabase(t, dataDir)
	var audit struct {
		EventID           string
		OperationID       string
		Action            string
		ActorType         string
		ResourceID        string
		Result            string
		BeforeStateDigest []byte
		AfterStateDigest  []byte
		OccurredAt        int64
	}
	if err := database.Table(sqlite.SecurityAuditEventTable).Take(&audit).Error; err != nil {
		t.Fatalf("read gateway rotation audit event error = %v", err)
	}
	beforeDigest := before.SPKIHash()
	afterDigest := afterRotation.SPKIHash()
	if audit.EventID == "" || audit.OperationID == "" || audit.Action != repository.SecurityAuditActionGatewayKeyRotate ||
		audit.ActorType != repository.SecurityAuditActorLocalOperator || audit.ResourceID != "gateway.example.test" ||
		audit.Result != repository.SecurityAuditResultSucceeded || audit.OccurredAt != rotationTime.UTC().Unix() ||
		!bytes.Equal(audit.BeforeStateDigest, beforeDigest[:]) || !bytes.Equal(audit.AfterStateDigest, afterDigest[:]) {
		t.Fatalf("gateway rotation audit event = %#v", audit)
	}
	if !strings.Contains(rotationOutput.String(), `"event":"security_audit_event"`) {
		t.Fatalf("gateway rotation output %q does not contain structured security log", rotationOutput.String())
	}

	if err := database.Exec(`
		CREATE TRIGGER reject_gateway_rotation_audit
		BEFORE INSERT ON security_audit_events
		BEGIN
			SELECT RAISE(ABORT, 'injected audit failure');
		END;
	`).Error; err != nil {
		t.Fatalf("create injected audit failure trigger error = %v", err)
	}
	closeGatewayAuditDatabase(t, database)
	beforeAuditFailure := afterRotation
	auditFailureOutput := &bytes.Buffer{}
	err = runGatewayRotateKey(context.Background(), "xtunnel-server", commandArgs, nil, auditFailureOutput, runtimeDir, rotationTime.Add(time.Second))
	if !errors.Is(err, errGatewayRotationAuditAfterCommit) {
		t.Fatalf("runGatewayRotateKey() audit failure error = %v, want errGatewayRotationAuditAfterCommit", err)
	}
	if !strings.Contains(auditFailureOutput.String(), `"error_code":"AUDIT_WRITE_FAILED_AFTER_COMMIT"`) {
		t.Fatalf("gateway rotation audit failure output = %q", auditFailureOutput.String())
	}
	if strings.Contains(auditFailureOutput.String(), `"event":"security_audit_event"`) {
		t.Fatalf("failed gateway rotation audit emitted success event: %q", auditFailureOutput.String())
	}
	afterAuditFailure, err := gateway.LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after audit failure error = %v", err)
	}
	if afterAuditFailure.SPKIHash() == beforeAuditFailure.SPKIHash() {
		t.Fatal("audit failure incorrectly reported that gateway rotation did not commit")
	}
	if _, exists, err := gateway.PendingRotationAuditEvent(dataDir); err != nil || !exists {
		t.Fatalf("PendingRotationAuditEvent() after audit failure = exists %t, error %v", exists, err)
	}
	database = openGatewayAuditDatabase(t, dataDir)
	var count int64
	if err := database.Table(sqlite.SecurityAuditEventTable).Count(&count).Error; err != nil {
		t.Fatalf("count audit events after injected failure error = %v", err)
	}
	if count != 1 {
		t.Fatalf("audit event count after injected failure = %d, want 1", count)
	}
	if err := database.Exec(`DROP TRIGGER reject_gateway_rotation_audit`).Error; err != nil {
		t.Fatalf("drop injected audit failure trigger error = %v", err)
	}
	closeGatewayAuditDatabase(t, database)

	recoveryOutput := &bytes.Buffer{}
	if err := runGatewayRotateKey(context.Background(), "xtunnel-server", commandArgs, nil, recoveryOutput, runtimeDir, rotationTime.Add(2*time.Second)); err != nil {
		t.Fatalf("runGatewayRotateKey() audit recovery error = %v", err)
	}
	if !strings.Contains(recoveryOutput.String(), `"event":"gateway_rotation_audit_reconciled"`) ||
		!strings.Contains(recoveryOutput.String(), `"rotation_performed":false`) {
		t.Fatalf("gateway rotation audit recovery output = %q", recoveryOutput.String())
	}
	afterRecovery, err := gateway.LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after audit recovery error = %v", err)
	}
	if afterRecovery.SPKIHash() != afterAuditFailure.SPKIHash() {
		t.Fatal("audit recovery unexpectedly performed another gateway rotation")
	}
	if _, exists, err := gateway.PendingRotationAuditEvent(dataDir); err != nil || exists {
		t.Fatalf("PendingRotationAuditEvent() after recovery = exists %t, error %v", exists, err)
	}
	database = openGatewayAuditDatabase(t, dataDir)
	defer closeGatewayAuditDatabase(t, database)
	if err := database.Table(sqlite.SecurityAuditEventTable).Count(&count).Error; err != nil {
		t.Fatalf("count audit events after recovery error = %v", err)
	}
	if count != 2 {
		t.Fatalf("audit event count after recovery = %d, want 2", count)
	}
}

func openGatewayAuditDatabase(t *testing.T, dataDir string) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(libsqlite.Open(filepath.Join(dataDir, "xtunnel.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open gateway audit database error = %v", err)
	}
	return database
}

func closeGatewayAuditDatabase(t *testing.T, database *gorm.DB) {
	t.Helper()
	pool, err := database.DB()
	if err != nil {
		t.Fatalf("get gateway audit database pool error = %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close gateway audit database error = %v", err)
	}
}

// runGatewayLockHelper 在独立测试进程中持有锁，通过受控临时文件与父进程同步。
func runGatewayLockHelper(t *testing.T) {
	t.Helper()
	runtimeDir := os.Getenv("XTUNNEL_GATEWAY_ROTATE_LOCK_RUNTIME_DIR")
	targetHash := os.Getenv("XTUNNEL_GATEWAY_ROTATE_LOCK_TARGET_HASH")
	readyPath := os.Getenv("XTUNNEL_GATEWAY_ROTATE_LOCK_READY")
	releasePath := os.Getenv("XTUNNEL_GATEWAY_ROTATE_LOCK_RELEASE")
	lock, err := externallock.Acquire(runtimeDir, targetHash)
	if err != nil {
		t.Fatalf("external lock helper Acquire() error = %v", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Errorf("external lock helper Close() error = %v", err)
		}
	}()
	if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
		t.Fatalf("external lock helper write ready error = %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(releasePath); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("external lock helper inspect release file error = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("external lock helper timed out waiting for release")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForFile 在有限时间内等待辅助进程完成就绪信号，防止测试无限阻塞。
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect helper ready signal error = %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("external lock helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
