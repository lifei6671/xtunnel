//go:build windows

package windowsservergate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// candidateProcess 的 Wait 只有一个 owner；正常停止等待退出，失败清理主动 Kill
// 并等待相同 done，确保测试不会把仍持有 Socket/SQLite 的子进程留在 Runner。
type candidateProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
	stderr  boundedOutput
}

func childEnvironment(extra []string) []string {
	var result []string
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(strings.ToUpper(key), "XTUNNEL_") || strings.HasPrefix(strings.ToUpper(key), "OTEL_") {
			continue
		}
		result = append(result, e)
	}
	return append(result, extra...)
}
func quietCommand(ctx context.Context, binary string, args, extra []string, output ...io.Writer) error {
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = childEnvironment(extra)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if len(output) > 0 {
		command.Stdout = output[0]
		command.Stderr = output[0]
	}
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	command.WaitDelay = 2 * time.Second
	return command.Run()
}
func runCLI(t *testing.T, audit *secretAudit, binary, label string, args []string, success bool, rejection ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = childEnvironment(nil)
	command.Stdout = &audit.output
	var stderr boundedOutput
	command.Stderr = io.MultiWriter(&stderr, &audit.output)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	command.WaitDelay = 2 * time.Second
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s exceeded command deadline", label)
	}
	if success {
		must(t, err, label)
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() == 0 {
			t.Fatalf("%s did not reject", label)
		}
		if len(rejection) != 1 || !containsRejection(&stderr, rejection[0]) {
			t.Fatalf("%s rejected for an unexpected reason", label)
		}
	}
}
func startCandidate(t *testing.T, audit *secretAudit, binary string, args, extra []string) *candidateProcess {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = childEnvironment(extra)
	command.Stdout = &audit.output
	p := &candidateProcess{command: command, done: make(chan struct{})}
	command.Stderr = io.MultiWriter(&p.stderr, &audit.output)
	// 独立隐藏 Console 提供真实 Ctrl-Break 生命周期；信号辅助进程只附着此 Console，
	// 不向测试 Runner 或其他候选进程广播停止。
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
	must(t, command.Start(), "start candidate")
	go func() { p.err = command.Wait(); close(p.done) }()
	return p
}
func (p *candidateProcess) stop(t *testing.T, hardDeadline bool) {
	t.Helper()
	select {
	case <-p.done:
		must(t, p.err, "candidate exit")
		return
	default:
	}
	exe, err := os.Executable()
	must(t, err, "signal helper executable")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = quietCommand(ctx, exe, []string{"-test.run=^TestConsoleSignalHelper$"}, []string{"XTUNNEL_WINDOWS_SERVER_GATE=1", "XTUNNEL_GATE_SIGNAL_PID=" + strconv.Itoa(p.command.Process.Pid)})
	if err != nil {
		p.cleanup(t)
		t.Fatal("candidate Ctrl-Break signal failed", err)
	}
	select {
	case <-p.done:
		_, code := p.exitStatus()
		if !expectedExitCode(code, hardDeadline) {
			t.Fatalf("candidate stop exit_code=%d hard_deadline=%t", code, hardDeadline)
		}
		if hardDeadline {
			data, overflow := p.stderr.snapshot()
			valid := matchesDeadlineError(data, p.command.Path, overflow)
			clear(data)
			if !valid {
				t.Fatal("active stop did not return the exact expected deadline error")
			}
		} else {
			must(t, p.err, "candidate graceful exit")
		}
	case <-time.After(35 * time.Second):
		p.cleanup(t)
		t.Fatal("candidate exceeded process stop bound")
	}
}
func (p *candidateProcess) cleanup(t *testing.T) {
	t.Helper()
	if t.Failed() {
		exited, code := p.exitStatus()
		t.Logf("candidate state before cleanup: pid=%d exited=%t exit_code=%d", p.command.Process.Pid, exited, code)
	}
	select {
	case <-p.done:
		return
	default:
	}
	if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Error("kill owned candidate", err)
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Error("owned candidate did not exit after kill")
	}
}

// TestConsoleSignalHelper 仅由已启动的候选进程 owner 调用。这里使用 Windows
// Console 事件，不能把 TerminateProcess 当作前台 graceful shutdown 的通过证据。
func TestConsoleSignalHelper(t *testing.T) {
	raw := os.Getenv("XTUNNEL_GATE_SIGNAL_PID")
	if raw == "" {
		t.Skip("console signal helper only")
	}
	if os.Getenv("XTUNNEL_WINDOWS_SERVER_GATE") != "1" {
		t.Fatal("console helper requires explicit gate")
	}
	pid, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || pid == 0 {
		t.Fatal("invalid owned candidate PID")
	}
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	free := kernel.NewProc("FreeConsole")
	attach := kernel.NewProc("AttachConsole")
	set := kernel.NewProc("SetConsoleCtrlHandler")
	// 无 Console 时 FreeConsole 也是允许的；后续 AttachConsole 必须明确成功。
	free.Call()
	if ok, _, e := attach.Call(uintptr(pid)); ok == 0 {
		t.Fatal("attach owned console", e)
	}
	handler := windows.NewCallback(func(uint32) uintptr { return 1 })
	if ok, _, e := set.Call(handler, 1); ok == 0 {
		t.Fatal("install helper console handler", e)
	}
	must(t, windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0), "deliver Ctrl-Break")
}

func serviceProcessHandle(t *testing.T, s *mgr.Service) windows.Handle {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		state, err := s.Query()
		must(t, err, "query SCM readiness")
		if state.State == svc.Running && state.ProcessId != 0 {
			h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, state.ProcessId)
			must(t, err, "pin SCM process")
			return h
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("SCM did not become ready")
	return 0
}
func stopService(t *testing.T, s *mgr.Service, h windows.Handle, hardDeadline bool) {
	t.Helper()
	began := time.Now()
	_, err := s.Control(svc.Stop)
	must(t, err, "request SCM stop")
	for time.Since(began) < 35*time.Second {
		state, e := s.Query()
		must(t, e, "query stopping SCM")
		wait, e := windows.WaitForSingleObject(h, 0)
		must(t, e, "query SCM process exit")
		if state.State == svc.Stopped && wait == windows.WAIT_OBJECT_0 {
			var code uint32
			must(t, windows.GetExitCodeProcess(h, &code), "SCM exit code")
			if !expectedServiceExit(state, code, hardDeadline) {
				t.Fatalf("SCM stop status mismatch: process_exit=%d win32_exit=%d service_exit=%d hard_deadline=%t", code, state.Win32ExitCode, state.ServiceSpecificExitCode, hardDeadline)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("SCM process exceeded 35-second final convergence bound")
}
func stopServiceCleanup(t *testing.T, s *mgr.Service, h windows.Handle) {
	t.Helper()
	if h == 0 {
		return
	}
	_, err := s.Control(svc.Stop)
	if err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		t.Error("cleanup SCM stop", err)
	}
	wait, e := windows.WaitForSingleObject(h, 35000)
	if e != nil || wait != windows.WAIT_OBJECT_0 {
		if err := windows.TerminateProcess(h, 1); err != nil {
			t.Error("terminate owned SCM process", err)
		}
		wait, e = windows.WaitForSingleObject(h, 10000)
		if e != nil || wait != windows.WAIT_OBJECT_0 {
			t.Error("owned SCM process did not exit")
		}
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Error("close cleanup SCM pin", err)
	}
}

// cleanupFailedInstall 只处理测试启动前确认不存在、且现在匹配本轮固定安装身份的服务。
// 进程先收敛；完整身份不足导致正式卸载拒绝时保留现场并报告失败，不绕过产品校验删除。
func cleanupFailedInstall(t *testing.T, audit *secretAudit, manager *mgr.Mgr, binary string, paths productPaths) {
	s, err := manager.OpenService("XTunnelServer")
	if err != nil {
		if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			t.Error("inspect partial installation", err)
		} else {
			for _, path := range []string{paths.root, paths.config, paths.binary} {
				if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
					t.Error("partial installation retained before SCM creation")
					break
				}
			}
		}
		return
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error("close partial installation", err)
		}
	}()
	cfg, err := s.Config()
	if err != nil {
		t.Error("inspect partial SCM identity", err)
		return
	}
	wantBinary := syscall.EscapeArg(paths.binary) + " --config " + syscall.EscapeArg(paths.config)
	if !strings.EqualFold(cfg.BinaryPathName, wantBinary) || !strings.EqualFold(cfg.ServiceStartName, `NT AUTHORITY\LocalService`) || cfg.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED || cfg.Description != "Managed by xtunnel-server service install" {
		t.Error("partial service identity differs; preserving objects")
		return
	}
	state, err := s.Query()
	if err != nil {
		t.Error("inspect partial service process", err)
		return
	}
	if state.ProcessId != 0 {
		h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, state.ProcessId)
		if err != nil {
			t.Error("pin partial service process", err)
			return
		}
		stopServiceCleanup(t, s, h)
	}
	rootInfo, rootErr := os.Lstat(paths.root)
	configInfo, configErr := os.Lstat(paths.config)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := quietCommand(ctx, binary, []string{"service", "uninstall"}, nil, &audit.output); err != nil {
		t.Error("partial installation retained: managed uninstall rejected", err)
		return
	}
	// 正式卸载成功证明对象权限/Marker；再比较捕获身份，仅回收本轮专用根及配置。
	for _, item := range []struct {
		path string
		info os.FileInfo
		err  error
		tree bool
	}{{paths.root, rootInfo, rootErr, true}, {paths.config, configInfo, configErr, false}} {
		now, err := os.Lstat(item.path)
		if item.err != nil || err != nil || !os.SameFile(item.info, now) {
			t.Error("partial object identity changed; preserving it")
			continue
		}
		if item.tree {
			err = os.RemoveAll(item.path)
		} else {
			err = os.Remove(item.path)
		}
		if err != nil {
			t.Error("remove owned partial object", err)
		}
	}
}

func containsRejection(output *boundedOutput, expected string) bool {
	data, overflow := output.snapshot()
	return !overflow && strings.Contains(string(data), expected)
}

// done 的关闭发布唯一 Wait owner 的 ProcessState；未退出时不读取并发写入状态。
func (p *candidateProcess) exitStatus() (bool, int) {
	select {
	case <-p.done:
		if p.command.ProcessState != nil {
			return true, p.command.ProcessState.ExitCode()
		}
		return true, -1
	default:
		return false, -1
	}
}

func expectedExitCode(code int, hardDeadline bool) bool {
	if hardDeadline {
		return code == 1
	}
	return code == 0
}
func expectedServiceExit(state svc.Status, code uint32, hardDeadline bool) bool {
	if state.State != svc.Stopped || !expectedExitCode(int(code), hardDeadline) {
		return false
	}
	if hardDeadline {
		return state.Win32ExitCode == uint32(windows.ERROR_SERVICE_SPECIFIC_ERROR) && state.ServiceSpecificExitCode == 1
	}
	return state.Win32ExitCode == 0 && state.ServiceSpecificExitCode == 0
}

var deadlineCloseLine = regexp.MustCompile(`^close active work conn_[0-9A-HJKMNP-TV-Z]{26}: close WorkConn: close WorkConn: tls: failed to send closeNotify alert \(but connection was closed anyway\): write tcp ([^ ]+)->([^ ]+): i/o timeout$`)

// 只识别本次候选的完整 stderr；结构化日志之后必须恰有 deadline 主错误，
// 以及至多一条已确认的 loopback TLS closeNotify 超时。其他错误、重复和截断均拒绝。
func matchesDeadlineError(data []byte, program string, overflow bool) bool {
	if overflow {
		return false
	}
	mainError := program + ": close admin bootstrap socket: context deadline exceeded"
	found, closeFound := false, false
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == mainError {
			if found {
				return false
			}
			found = true
			continue
		}
		if !found {
			var record struct {
				Event     string `json:"event"`
				Component string `json:"component"`
				Level     string `json:"level"`
			}
			if json.Unmarshal([]byte(line), &record) != nil || record.Event == "" || record.Component != "server" || record.Level == "" {
				return false
			}
			continue
		}
		match := deadlineCloseLine.FindStringSubmatch(line)
		if closeFound || match == nil {
			return false
		}
		for _, endpoint := range match[1:] {
			address, err := netip.ParseAddrPort(endpoint)
			if err != nil || !address.Addr().IsLoopback() || address.Port() == 0 {
				return false
			}
		}
		closeFound = true
	}
	return found
}
func TestDeadlineExitContract(t *testing.T) {
	for _, test := range []struct {
		name       string
		code       int
		hard, want bool
	}{
		{"ordinary zero", 0, false, true}, {"ordinary failure", 1, false, false}, {"deadline failure", 1, true, true}, {"deadline zero", 0, true, false}, {"other failure", 2, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if expectedExitCode(test.code, test.hard) != test.want {
				t.Fatal("exit classification mismatch")
			}
		})
	}
	state := svc.Status{State: svc.Stopped, Win32ExitCode: uint32(windows.ERROR_SERVICE_SPECIFIC_ERROR), ServiceSpecificExitCode: 1}
	if !expectedServiceExit(state, 1, true) {
		t.Fatal("expected SCM deadline status rejected")
	}
	for _, mutate := range []func(*svc.Status){func(s *svc.Status) { s.State = svc.Running }, func(s *svc.Status) { s.Win32ExitCode = 0 }, func(s *svc.Status) { s.ServiceSpecificExitCode = 2 }} {
		changed := state
		mutate(&changed)
		if expectedServiceExit(changed, 1, true) {
			t.Fatal("unrelated SCM failure accepted")
		}
	}
}
func TestDeadlineErrorParser(t *testing.T) {
	program := `C:\candidate\server.exe`
	mainError := program + ": close admin bootstrap socket: context deadline exceeded\n"
	closeLine := "close active work conn_01M1P0EJ3Y25KAST8HRYD7WV6H: close WorkConn: close WorkConn: tls: failed to send closeNotify alert (but connection was closed anyway): write tcp 127.0.0.1:60495->127.0.0.1:60589: i/o timeout\n"
	for _, test := range []struct {
		name, body     string
		overflow, want bool
	}{
		{"deadline", mainError, false, true}, {"expected TLS close", mainError + closeLine, false, true},
		{"structured log", `{"event":"process_started","component":"server","level":"info"}` + "\n" + mainError, false, true},
		{"duplicate deadline", mainError + mainError, false, false}, {"extra error", mainError + "close SQLite: failure\n", false, false},
		{"foreign program", strings.ReplaceAll(mainError, program, "other.exe"), false, false},
		{"remote TLS", mainError + strings.ReplaceAll(closeLine, "127.0.0.1:60589", "192.0.2.1:60589"), false, false},
		{"duplicate TLS", mainError + closeLine + closeLine, false, false}, {"TLS only", closeLine, false, false},
		{"overflow", mainError, true, false}, {"malformed prefix", "unrelated failure\n" + mainError, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if matchesDeadlineError([]byte(test.body), program, test.overflow) != test.want {
				t.Fatal("deadline error classification mismatch")
			}
		})
	}
}
