//go:build windows

package windowsservergate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
)

// TestWindowsServerProductGate 只允许一次性提升权限 CI；所有业务通过候选进程的
// 管理 API 与真实 Socket 进入。普通全包测试不会安装服务或创建固定产品目录。
func TestWindowsServerProductGate(t *testing.T) {
	if os.Getenv("XTUNNEL_WINDOWS_SERVER_GATE") != "1" {
		t.Skip("requires isolated Windows Server candidate runner")
	}
	if runtime.GOARCH != "amd64" || !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("gate requires elevated native Windows amd64")
	}
	commit := os.Getenv("XTUNNEL_CANDIDATE_COMMIT")
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		t.Fatal("candidate commit must be an exact SHA")
	}
	server, agent := candidatePath(t, "XTUNNEL_SERVER_BINARY"), candidatePath(t, "XTUNNEL_AGENT_BINARY")
	report := os.Getenv("XTUNNEL_PRODUCT_REPORT")
	if !filepath.IsAbs(report) {
		t.Fatal("product report must be an absolute new path")
	}
	// 报告不进入仓库，也不覆盖既有证明。
	cwd, err := os.Getwd()
	must(t, err, "working directory")
	workspace := cwd
	for {
		if _, e := os.Stat(filepath.Join(workspace, "go.mod")); e == nil {
			break
		}
		parent := filepath.Dir(workspace)
		if parent == workspace {
			t.Fatal("cannot locate checkout root")
		}
		workspace = parent
	}
	if strings.HasPrefix(strings.ToLower(filepath.Clean(report)), strings.ToLower(workspace)+string(os.PathSeparator)) {
		t.Fatal("product report must be outside the checkout")
	}
	requireAbsent(t, report)
	before := hashFile(t, server)
	paths := gatePaths(t)
	for _, p := range []string{paths.foreground, paths.root, paths.config, paths.binary} {
		requireAbsent(t, p)
	}
	manager, err := mgr.Connect()
	must(t, err, "connect SCM")
	defer func() {
		if err := manager.Disconnect(); err != nil {
			t.Error("close SCM", err)
		}
	}()
	if existing, err := manager.OpenService("XTunnelServer"); err == nil {
		existing.Close()
		t.Fatal("existing XTunnelServer service")
	} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		t.Fatal("query existing service", err)
	}
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\EventLog\Application\XTunnelServer`, registry.READ)
	if err == nil {
		key.Close()
		t.Fatal("existing Server Event Source")
	}
	if !errors.Is(err, registry.ErrNotExist) {
		t.Fatal("query Event Source", err)
	}
	for _, mode := range []string{"foreground", "scm"} {
		if !t.Run(mode, func(t *testing.T) { runMode(t, mode, server, agent, "v0.1.0-ci."+commit, paths, manager) }) {
			return
		}
		if hashFile(t, server) != before {
			t.Fatal("candidate bytes changed during product gate")
		}
	}
	result := struct {
		Commit    string   `json:"commit"`
		ServerSHA string   `json:"server_sha256"`
		Version   string   `json:"version"`
		OS        string   `json:"os"`
		Arch      string   `json:"arch"`
		Modes     []string `json:"modes"`
	}{commit, before, "v0.1.0-ci." + commit, "windows", "amd64", []string{"foreground", "scm"}}
	content, err := json.Marshal(result)
	must(t, err, "encode product report")
	f, err := os.OpenFile(report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(t, err, "create product report")
	_, writeErr := f.Write(append(content, '\n'))
	syncErr := f.Sync()
	closeErr := f.Close()
	must(t, errors.Join(writeErr, syncErr, closeErr), "persist product report")
}

type productPaths struct{ foreground, root, config, binary string }

func gatePaths(t *testing.T) productPaths {
	t.Helper()
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	must(t, err, "LocalAppData")
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	must(t, err, "ProgramData")
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	must(t, err, "ProgramFiles")
	return productPaths{filepath.Join(local, "XTunnel", "Server"), filepath.Join(pd, "XTunnel", "Server"), filepath.Join(pd, "XTunnel", "server.yaml"), filepath.Join(pf, "XTunnel", "xtunnel-server.exe")}
}
func candidatePath(t *testing.T, key string) string {
	t.Helper()
	p := os.Getenv(key)
	if !filepath.IsAbs(p) {
		t.Fatalf("%s must be absolute", key)
	}
	i, e := os.Lstat(p)
	must(t, e, "candidate metadata")
	if !i.Mode().IsRegular() {
		t.Fatal("candidate must be a regular file")
	}
	return p
}
func hashFile(t *testing.T, p string) string {
	t.Helper()
	f, e := os.Open(p)
	must(t, e, "open candidate")
	h := sha256.New()
	_, e = io.Copy(h, f)
	must(t, errors.Join(e, f.Close()), "hash candidate")
	return hex.EncodeToString(h.Sum(nil))
}
func requireAbsent(t *testing.T, p string) {
	t.Helper()
	if _, e := os.Lstat(p); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("gate target must be absent: %s", p)
	}
}
func must(t *testing.T, err error, operation string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
}
func ownedTree(t *testing.T, p string) func() {
	t.Helper()
	identity, e := os.Lstat(p)
	must(t, e, "capture owned directory")
	return func() {
		now, e := os.Lstat(p)
		if e != nil || !os.SameFile(identity, now) || !now.IsDir() {
			t.Error("owned directory identity changed; preserving it")
			return
		}
		if e := os.RemoveAll(p); e != nil {
			t.Error("remove owned tree", e)
		}
	}
}
func reservePorts(t *testing.T, n int) []int {
	t.Helper()
	var listeners []net.Listener
	var ports []int
	for range n {
		l, e := net.Listen("tcp4", "127.0.0.1:0")
		must(t, e, "reserve port")
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		must(t, l.Close(), "release port reservation")
	}
	return ports
}

func runMode(t *testing.T, mode, server, agent, version string, paths productPaths, manager *mgr.Mgr) {
	beganMode := time.Now()
	audit := &secretAudit{}
	t.Cleanup(func() {
		data, overflow := audit.output.snapshot()
		defer clear(data)
		diagnostic, allowed := checkedDiagnostic(data, audit.values, overflow)
		if !allowed {
			t.Error("candidate diagnostics withheld: output overflow or secret check failed")
			return
		}
		if t.Failed() && diagnostic != "" {
			t.Logf("candidate diagnostics after complete secret scan (at most 8192 bytes):\n%s", diagnostic)
		}
	})
	private := filepath.Join(t.TempDir(), "private")
	security, err := winsecurity.NewForegroundDirectorySecurity()
	must(t, err, "private directory policy")
	must(t, winsecurity.CreateForegroundDirectory(private, security), "private test directory")
	ports := reservePorts(t, 5)
	config := productConfig(ports)
	configPath := filepath.Join(private, "server.yaml")
	must(t, os.WriteFile(configPath, []byte(config), 0600), "write test config")
	password := rand.Text() + rand.Text()
	audit.values = append(audit.values, password)
	passwordPath := filepath.Join(private, "password")
	must(t, os.WriteFile(passwordPath, []byte(password), 0600), "write private password")
	var foreground *candidateProcess
	var service *mgr.Service
	var serviceProcess windows.Handle
	root := paths.foreground
	// 清理顺序：先停止并 Wait 进程，再用正式 CLI 卸载 SCM，最后删除本轮捕获身份的目录。
	if mode == "scm" {
		managedCleanupRegistered := false
		// 安装器可在创建服务或启动后失败；先登记失败收敛，不能让 Fatal 越过 owner。
		t.Cleanup(func() {
			if !managedCleanupRegistered {
				cleanupFailedInstall(t, audit, manager, server, paths)
			}
		})
		runCLI(t, audit, server, "install", []string{"service", "install", "--config", configPath}, true)
		root = paths.root
		remove := ownedTree(t, root)
		service, err = manager.OpenService("XTunnelServer")
		must(t, err, "open installed service")
		t.Cleanup(func() {
			stopServiceCleanup(t, service, serviceProcess)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if e := quietCommand(ctx, server, []string{"service", "uninstall"}, nil, &audit.output); e != nil {
				t.Error("uninstall owned service", e)
				service.Close()
				return
			}
			if e := service.Close(); e != nil {
				t.Error("close owned service", e)
			}
			remove()
			if e := os.Remove(paths.config); e != nil {
				t.Error("remove owned service config", e)
			}
		})
		managedCleanupRegistered = true
		if hashFile(t, paths.binary) != hashFile(t, server) {
			t.Fatal("installed SCM binary does not match candidate")
		}
		actual, e := service.Config()
		must(t, e, "read installed SCM identity")
		if actual.ServiceStartName != `NT AUTHORITY\LocalService` || actual.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED {
			t.Fatal("installed SCM identity differs from LocalService + Service SID")
		}
		serviceProcess = serviceProcessHandle(t, service)
		stopService(t, service, serviceProcess)
		must(t, windows.CloseHandle(serviceProcess), "close stopped SCM process")
		serviceProcess = 0
		configPath = paths.config
	} else {
		initialized := false
		t.Cleanup(func() {
			if !initialized {
				if _, e := os.Lstat(root); !errors.Is(e, os.ErrNotExist) {
					t.Error("partial foreground init retained before ownership capture")
				}
			}
		})
		runCLI(t, audit, server, "init", []string{"init", "--config", configPath}, true)
		remove := ownedTree(t, root)
		t.Cleanup(func() {
			if foreground != nil {
				foreground.cleanup(t)
			}
			remove()
		})
		initialized = true
	}
	runCLI(t, audit, server, "create first admin", []string{"admin", "create", "--config", configPath, "--username", "gate-admin", "--password-file", passwordPath}, true)
	must(t, os.Remove(passwordPath), "remove password file")
	start := func() {
		if mode == "scm" {
			must(t, service.Start(), "SCM start")
			serviceProcess = serviceProcessHandle(t, service)
		} else {
			foreground = startCandidate(t, audit, server, []string{"--config", configPath}, nil)
		}
	}
	stop := func() {
		if mode == "scm" {
			stopService(t, service, serviceProcess)
			must(t, windows.CloseHandle(serviceProcess), "close stopped SCM process")
			serviceProcess = 0
		} else {
			foreground.stop(t)
			foreground = nil
		}
	}
	start()
	api := newManagement(t, ports[0], audit)
	api.waitReady(t, foreground)
	api.login(t, password)
	api.systemInfo(t, version)
	origin := startOrigins(t)
	tunnel, httpService, tcpService, token := api.configure(t, origin, ports[3])
	audit.values = append(audit.values, token)
	tokenEnv := []string{"XTUNNEL_TOKEN=" + token}
	agentProcess := startCandidate(t, audit, agent, []string{"run"}, tokenEnv)
	t.Cleanup(func() { agentProcess.cleanup(t) })
	api.waitRoutes(t, tunnel, httpService, tcpService)
	checkTraffic(t, ports[1], ports[3])
	// 普通前台候选进程竞争同一固定 Profile；SCM 维护命令必须在运行状态被拒绝。
	if mode == "foreground" {
		runCLI(t, audit, server, "second foreground server", []string{"--config", configPath}, false, "server data target is already locked")
	} else {
		must(t, os.WriteFile(passwordPath, []byte(password), 0600), "write maintenance password")
		runCLI(t, audit, server, "online service maintenance", []string{"admin", "create", "--config", configPath, "--username", "blocked", "--password-file", passwordPath}, false, "XTunnelServer must be stopped before offline maintenance")
		must(t, os.Remove(passwordPath), "remove maintenance password")
	}
	checkTraffic(t, ports[1], ports[3])
	audit.loadMasterKey(t, root)
	identity := persistentIdentity(t, root)
	stop()
	assertBindable(t, ports)
	start()
	api.waitReady(t, foreground)
	api.login(t, password)
	api.systemInfo(t, version)
	api.assertPersisted(t, tunnel, httpService, tcpService)
	if persistentIdentity(t, root) != identity {
		t.Fatal("restart replaced credential identity")
	}
	api.waitRoutes(t, tunnel, httpService, tcpService)
	checkTraffic(t, ports[1], ports[3])
	active := dialTCP(t, ports[3])
	defer active.Close()
	writeRead(t, active, []byte("active-stop-proof"))
	api.waitActive(t, tunnel)
	// 30 秒是生产 drain deadline；观察 socket 关闭容许 1 秒调度误差，进程收尾另限 35 秒。
	began := time.Now()
	readDone := make(chan struct{})
	var socketErr error
	var socketBytes int
	var socketElapsed time.Duration
	must(t, active.SetReadDeadline(began.Add(31*time.Second)), "active read deadline")
	go func() {
		defer close(readDone)
		var b [1]byte
		n, e := active.Read(b[:])
		socketElapsed = time.Since(began)
		socketBytes, socketErr = n, e
	}()
	t.Cleanup(func() {
		active.Close()
		select {
		case <-readDone:
		case <-time.After(2 * time.Second):
			t.Error("active-read owner did not exit")
		}
	})
	stop()
	processElapsed := time.Since(began)
	<-readDone
	if socketErr == nil || socketBytes != 0 {
		t.Fatal("active connection did not close")
	}
	if ne, ok := socketErr.(net.Error); ok && ne.Timeout() {
		t.Fatal("active socket exceeded 30-second drain plus scheduling allowance")
	}
	if socketElapsed > 31*time.Second || processElapsed > 35*time.Second {
		t.Fatal("active stop exceeded socket/process bounds")
	}
	t.Logf("active socket closed in %s; process exited in %s (30s drain, 1s socket scheduling allowance, 35s process bound)", socketElapsed, processElapsed)
	assertBindable(t, ports)
	start()
	api.waitReady(t, foreground)
	api.login(t, password)
	api.waitRoutes(t, tunnel, httpService, tcpService)
	checkTraffic(t, ports[1], ports[3])
	agentProcess.stop(t)
	stop()
	assertBindable(t, ports)
	audit.scanFile(t, "server binary", server, false)
	audit.scanFile(t, "agent binary", agent, false)
	audit.scanFile(t, "source config", filepath.Join(private, "server.yaml"), true)
	if mode == "scm" {
		audit.scanFile(t, "installed binary", paths.binary, false)
		audit.scanFile(t, "managed config", paths.config, true)
		audit.scanRegistry(t, `SYSTEM\CurrentControlSet\Services\XTunnelServer`)
		audit.scanEvents(t, beganMode)
	}
	t.Log("secret scans: candidate logs, binaries, config; SCM mode also registry and Server events")
	t.Log("candidate Management/CSRF/config/token/Gateway/HTTP/WebSocket/TCP/half-close/restart/active-stop: PASS")
}
func persistentIdentity(t *testing.T, root string) string {
	t.Helper()
	return hashFile(t, filepath.Join(root, "data", "credentials", "tunnel-token.key")) + hashFile(t, filepath.Join(root, "data", "pki", "agent-gateway.key"))
}
func assertBindable(t *testing.T, ports []int) {
	t.Helper()
	for _, p := range ports {
		l, e := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", p))
		must(t, e, "stopped listener release")
		must(t, l.Close(), "close listener proof")
	}
}

func productConfig(ports []int) string {
	return fmt.Sprintf("server:\n  data_dir: auto\nmanagement:\n  listen: '127.0.0.1:%d'\n  public_url: https://admin.gate.test\n  trusted_proxies: ['127.0.0.1/32']\nhttp_ingress:\n  listen: '127.0.0.1:%d'\nagent_gateway:\n  listen: '127.0.0.1:%d'\n  public_hostname: localhost\n  tls:\n    mode: pinned\ntcp_ingress:\n  bind: 127.0.0.1\n  min_port: %d\n  max_port: %d\nmetrics:\n  listen: '127.0.0.1:%d'\n", ports[0], ports[1], ports[2], ports[3], ports[3], ports[4])
}

// 配置机器契约校验不创建固定目录，可在普通测试中发现验收输入漂移。
func TestProductConfigContract(t *testing.T) {
	options := baseconfig.Options{YAML: []byte(productConfig([]int{19001, 19002, 19003, 19004, 19005}))}
	_, err := serverconfig.Load(options)
	must(t, err, "foreground config contract")
	_, err = serverconfig.LoadService(options)
	must(t, err, "SCM config contract")
}
