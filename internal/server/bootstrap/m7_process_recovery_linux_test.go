//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	agentbootstrap "github.com/lifei6671/xtunnel/internal/agent/bootstrap"
	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	"github.com/lifei6671/xtunnel/internal/tracing"
)

const m7ProcessRoleEnvironment = "GO_WANT_M7_PROCESS_RECOVERY_ROLE"
const m7ProcessRuntimeEnvironment = "GO_WANT_M7_PROCESS_RECOVERY_RUNTIME"

// TestM7ProcessRecoveryAfterSIGKILL 从真实 Server 与 Agent 进程贯穿 Token 认证、
// Snapshot、WorkConn 和 Origin 数据面。测试分别 SIGKILL Agent 与 Server，并要求
// 新 Agent、重启 Server 以及原 Agent 的重连都恢复同一个持久化 Tunnel。
func TestM7ProcessRecoveryAfterSIGKILL(t *testing.T) {
	if role := os.Getenv(m7ProcessRoleEnvironment); role != "" {
		args := m7ProcessArguments()
		switch role {
		case "server":
			runtimeDir := os.Getenv(m7ProcessRuntimeEnvironment)
			startedAt := time.Now()
			runner := func(ctx context.Context, options baseconfig.Options, stderr io.Writer) error {
				return runWithStorageAndBootstrapOptions(ctx, options, stderr, func(ctx context.Context, dataDir string) (storage, error) {
					return openServerStorage(ctx, dataDir, runtimeDir)
				}, func(ctx context.Context, config serverconfig.Config, resources storage, logger *slog.Logger, traceRuntime *tracing.Runtime) (io.Closer, error) {
					return openGatewayAndBootstrapWithStartedAtTracing(ctx, config, resources, logger, startedAt, runtimeDir, func(context.Context, string, string, *sqlite.Store, func() error, func(error)) (io.Closer, error) {
						return nil, nil
					}, traceRuntime)
				})
			}
			os.Exit(executeWithRun("xtunnel-server", args, os.Environ(), os.Stderr, runner))
		case "agent":
			os.Exit(agentbootstrap.Execute("xtunnel-agent", args, os.Environ(), os.Stdout, os.Stderr))
		default:
			os.Exit(2)
		}
	}

	ctx := context.Background()
	origin := startM7AlphaEchoOrigin(t)
	reserved, ports := reserveM7AlphaPorts(t, 1)
	gatewayReservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve process-recovery Gateway: %v", err)
	}
	gatewayAddress := gatewayReservation.Addr()

	dataDir := t.TempDir()
	resources, err := openServerStorage(ctx, dataDir, newRuntimeDirectory(t))
	if err != nil {
		t.Fatalf("open process-recovery storage: %v", err)
	}
	shard := seedM7AlphaShards(t, ctx, resources, origin.listener.Addr(), ports)[0]
	if err := resources.database.CreateFirstAdmin(ctx, "admin", "m7 process recovery gate password"); err != nil {
		t.Fatalf("create process-recovery Admin: %v", err)
	}
	if _, err := gateway.LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, time.Now()); err != nil {
		t.Fatalf("create process-recovery Gateway identity: %v", err)
	}
	token := issueM7AlphaToken(t, ctx, resources, gatewayAddress, shard.tunnelID)
	if err := resources.Close(); err != nil {
		t.Fatalf("close seeded process-recovery storage: %v", err)
	}
	if err := reserved[0].Close(); err != nil {
		t.Fatalf("release process-recovery public port: %v", err)
	}
	if err := gatewayReservation.Close(); err != nil {
		t.Fatalf("release process-recovery Gateway port: %v", err)
	}
	reserveAddress := func(name string) string {
		listener, listenErr := net.Listen("tcp4", "127.0.0.1:0")
		if listenErr != nil {
			t.Fatalf("reserve process-recovery %s port: %v", name, listenErr)
		}
		address := listener.Addr().String()
		if closeErr := listener.Close(); closeErr != nil {
			t.Fatalf("release process-recovery %s port: %v", name, closeErr)
		}
		return address
	}
	managementAddress := reserveAddress("Management")
	httpAddress := reserveAddress("HTTP ingress")
	metricsAddress := reserveAddress("Metrics")

	configPath := writeConfig(t, fmt.Sprintf(`
management:
  listen: %q
  public_url: "https://admin.example.test"
http_ingress:
  listen: %q
agent_gateway:
  listen: %q
  public_hostname: "gateway.example.test"
tcp_ingress:
  bind: "127.0.0.1"
  min_port: %d
  max_port: %d
connector_runtime:
  heartbeat_interval: 1s
  heartbeat_timeout: 30s
metrics:
  listen: %q
limits:
  max_tunnels: 16
  max_connectors: 8
  max_connectors_per_tunnel: 4
  max_services_per_tunnel: 1000
  max_health_targets_per_tunnel: 2000
  max_health_targets_global: 50000
  max_tunnel_snapshot_bytes: 786432
  max_active_connections: 32
  max_connections_per_tunnel: 16
  max_connections_per_service: 16
  max_connections_per_source_ip: 8
  max_open_rate_per_source_ip: 50
  max_open_burst_per_source_ip: 100
  max_http_requests_per_source_ip_per_second: 100
  max_pending_tls_handshakes: 64
  max_pending_auth: 64
  max_replay_entries_per_session: 32
  max_work_connections: 64
  max_idle_work_connections: 32
  max_connecting_work_connections: 16
  max_pending_opens: 16
  max_control_frame_bytes: 1048576
  max_http_header_bytes: 65536
  max_http_body_bytes: 2147483648
`, managementAddress, httpAddress, gatewayAddress.String(), ports[0], ports[0], metricsAddress))
	serverArgs := []string{"--config", configPath, "--set", "server.data_dir=" + dataDir}
	agentArgs := []string{"run", "--token", token}
	t.Setenv(m7ProcessRuntimeEnvironment, newRuntimeDirectory(t))

	processes := make([]*m7Process, 0, 4)
	server := startM7Process(t, "server", serverArgs)
	processes = append(processes, server)
	waitM7ProcessPort(t, server, gatewayAddress.String(), 10*time.Second)
	agent := startM7Process(t, "agent", agentArgs)
	processes = append(processes, agent)
	waitM7ProcessEcho(t, server, agent, shard.address, "before-agent-kill", 15*time.Second)
	waitM7ProcessQuiescent(t, metricsAddress, 10*time.Second)
	agent.killAndRequire(t, syscall.SIGKILL)

	agent = startM7Process(t, "agent", agentArgs)
	processes = append(processes, agent)
	waitM7ProcessEcho(t, server, agent, shard.address, "after-agent-restart", 15*time.Second)
	waitM7ProcessQuiescent(t, metricsAddress, 10*time.Second)
	server.killAndRequire(t, syscall.SIGKILL)

	if err := sqlite.ValidateBackupDatabase(ctx, filepath.Join(dataDir, "xtunnel.db"), sqlite.CurrentSchemaVersion()); err != nil {
		t.Fatalf("validate SQLite after Server SIGKILL: %v", err)
	}
	store, err := sqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("open SQLite after Server SIGKILL: %v", err)
	}
	if err := store.ReadConsistent(ctx, func(view repository.RepositoryView) error {
		_, readErr := view.Tunnels().Get(ctx, shard.tunnelID)
		return readErr
	}); err != nil {
		_ = store.Close()
		t.Fatalf("read persisted Tunnel after Server SIGKILL: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close SQLite recovery inspection: %v", err)
	}

	server = startM7Process(t, "server", serverArgs)
	processes = append(processes, server)
	waitM7ProcessPort(t, server, gatewayAddress.String(), 10*time.Second)
	waitM7ProcessEcho(t, server, agent, shard.address, "after-server-restart", 20*time.Second)
	waitM7ProcessQuiescent(t, metricsAddress, 10*time.Second)
	agent.stop(t)
	server.stop(t)
	for _, process := range processes {
		assertM7LogsSafe(t, "process-recovery child", process.output.String(), token)
	}
}

func waitM7ProcessQuiescent(t *testing.T, metricsAddress string, timeout time.Duration) {
	t.Helper()
	want := map[string]float64{
		"xtunnel_connectors_online":           1,
		"xtunnel_control_sessions_online":     1,
		"xtunnel_active_connections":          0,
		"xtunnel_tcp_idle_work_connections":   8,
		"xtunnel_tcp_active_work_connections": 0,
	}
	deadline := time.Now().Add(timeout)
	last := make(map[string]float64)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + metricsAddress + "/metrics")
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			closeErr := response.Body.Close()
			if readErr == nil && closeErr == nil && response.StatusCode == http.StatusOK {
				last = parseM7Metrics(string(body), want)
				matched := true
				for name, value := range want {
					if last[name] != value {
						matched = false
						break
					}
				}
				if matched {
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process resources did not quiesce: got=%v want=%v", last, want)
}

func parseM7Metrics(content string, wanted map[string]float64) map[string]float64 {
	result := make(map[string]float64, len(wanted))
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if _, ok := wanted[fields[0]]; !ok {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err == nil {
			result[fields[0]] = value
		}
	}
	return result
}

func m7ProcessArguments() []string {
	for index, argument := range os.Args {
		if argument == "--" {
			return os.Args[index+1:]
		}
	}
	return nil
}

type m7Process struct {
	command *exec.Cmd
	done    chan error
	output  m7ProcessOutput
	exited  bool
}

type m7ProcessOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (output *m7ProcessOutput) Write(content []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.Write(content)
}

func (output *m7ProcessOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}

func startM7Process(t *testing.T, role string, args []string) *m7Process {
	t.Helper()
	commandArgs := append([]string{"-test.run=^TestM7ProcessRecoveryAfterSIGKILL$", "--"}, args...)
	process := &m7Process{command: exec.Command(os.Args[0], commandArgs...), done: make(chan error, 1)}
	process.command.Env = append(os.Environ(), m7ProcessRoleEnvironment+"="+role)
	process.command.Stdout = &process.output
	process.command.Stderr = &process.output
	if err := process.command.Start(); err != nil {
		t.Fatalf("start %s process: %v", role, err)
	}
	go func() { process.done <- process.command.Wait() }()
	t.Cleanup(func() {
		if process.exited {
			return
		}
		killErr := process.command.Process.Kill()
		select {
		case <-process.done:
			process.exited = true
		case <-time.After(5 * time.Second):
			t.Errorf("process cleanup timed out after Kill; kill error: %v", killErr)
		}
	})
	return process
}

func (process *m7Process) killAndRequire(t *testing.T, signal syscall.Signal) {
	t.Helper()
	if err := process.command.Process.Signal(signal); err != nil {
		t.Fatalf("signal process with %s: %v", signal, err)
	}
	select {
	case err := <-process.done:
		process.exited = true
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("process exit after %s = %v, want ExitError\n%s", signal, err, process.output.String())
		}
		status, ok := exitError.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != signal {
			t.Fatalf("process wait status after %s = %v\n%s", signal, exitError.Sys(), process.output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("process did not exit after %s", signal)
	}
}

func (process *m7Process) stop(t *testing.T) {
	t.Helper()
	if process.exited {
		return
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case err := <-process.done:
		process.exited = true
		if err != nil {
			t.Fatalf("process graceful exit: %v\n%s", err, process.output.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
}

func waitM7ProcessPort(t *testing.T, process *m7Process, address string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp4", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		select {
		case exitErr := <-process.done:
			process.exited = true
			t.Fatalf("process exited before listening: %v\n%s", exitErr, process.output.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process did not listen on %s\n%s", address, process.output.String())
}

func waitM7ProcessEcho(t *testing.T, server, agent *m7Process, address, phase string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	payload := []byte("m7-process-recovery-" + phase)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp4", address, 300*time.Millisecond)
		if err == nil {
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			_, writeErr := connection.Write(payload)
			echoed := make([]byte, len(payload))
			_, readErr := io.ReadFull(connection, echoed)
			_ = connection.Close()
			if writeErr == nil && readErr == nil && bytes.Equal(echoed, payload) {
				return
			}
		}
		for _, process := range []*m7Process{server, agent} {
			select {
			case exitErr := <-process.done:
				process.exited = true
				t.Fatalf("process exited during %s: %v\n%s", phase, exitErr, process.output.String())
			default:
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("data plane did not recover during %s\nserver=%s\nagent=%s", phase, server.output.String(), agent.output.String())
}
