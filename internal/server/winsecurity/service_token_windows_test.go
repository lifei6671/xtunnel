//go:build windows

package winsecurity

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	_ "modernc.org/sqlite"
)

// TestServiceTokenIsolation 只在显式开启的隔离管理员 CI 中操作 SCM；普通 go test
// 不产生服务或固定路径副作用。它必须紧跟真实 Server uninstall，使用保留的数据。
func TestServiceTokenIsolation(t *testing.T) {
	if os.Getenv("XTUNNEL_SERVER_SCM_SECURITY_TEST") != "1" {
		t.Skip("requires isolated SCM security runner")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("SCM security runner is not elevated")
	}
	manager, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Disconnect()
	for _, name := range []string{xtunnelServerServiceName, "XTunnelServerSecurityProbe"} {
		service, err := manager.OpenService(name)
		if err == nil {
			service.Close()
			t.Fatalf("service %s must be absent before token test", name)
		}
		if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			t.Fatal(err)
		}
	}
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "XTunnel", "Server", "data")
	if err := ValidateServiceObject(data, ServiceData, true); err != nil {
		t.Fatal(err)
	}
	for _, path := range tokenProofPaths(data, "unused")[:3] {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("real Server evidence missing: %s: %v", filepath.Base(path), err)
		}
	}
	locks, err := filepath.Glob(filepath.Join(base, "XTunnel", "Server", "runtime", "server-lock-*.lock"))
	if err != nil || len(locks) != 1 {
		t.Fatalf("expected one real Server external lock, got %d: %v", len(locks), err)
	}
	temporary := t.TempDir()
	descriptor, err := windows.SecurityDescriptorFromString("O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;LS)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(temporary, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(temporary, "security-helper.exe")
	if err := os.WriteFile(helper, content, 0600); err != nil {
		t.Fatal(err)
	}
	proof := "scm-security-" + filepath.Base(temporary)
	for _, path := range tokenProofPaths(data, proof)[3:] {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("proof target already exists or inaccessible: %s", path)
		}
	}
	t.Cleanup(func() {
		// 两个 helper 的 SCM Stop/退出等待先完成，随后仅清理本轮命名的测试文件。
		for _, path := range tokenProofPaths(data, proof)[3:] {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove proof object: %v", err)
			}
		}
	})
	for index, name := range []string{xtunnelServerServiceName, "XTunnelServerSecurityProbe"} {
		report := filepath.Join(temporary, fmt.Sprintf("result-%d", index))
		service, err := manager.CreateService(name, helper, mgr.Config{StartType: mgr.StartManual, ServiceStartName: `NT AUTHORITY\LocalService`, SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED}, "-test.run=^TestServiceTokenHelper$", "--", "token-helper", name, data, proof, report, locks[0])
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, controlErr := service.Control(svc.Stop)
			if controlErr != nil && !errors.Is(controlErr, windows.ERROR_SERVICE_NOT_ACTIVE) {
				t.Errorf("stop helper: %v", controlErr)
			}
			deadline := time.Now().Add(15 * time.Second)
			for {
				status, err := service.Query()
				if err != nil {
					t.Errorf("query helper: %v", err)
					break
				}
				if status.State == svc.Stopped {
					break
				}
				if time.Now().After(deadline) {
					t.Error("helper did not stop")
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if err := service.Delete(); err != nil {
				t.Errorf("delete helper: %v", err)
			}
			if err := service.Close(); err != nil {
				t.Errorf("close helper: %v", err)
			}
		})
		if err := service.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			result, err := os.ReadFile(report)
			if err == nil {
				if string(result) != "PASS" {
					t.Fatalf("%s: %s", name, result)
				}
				break
			}
			if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
				t.Fatalf("%s report: %v", name, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("%s actual SCM LocalService token: PASS", name)
	}
}

func tokenProofPaths(data, proof string) []string {
	return []string{filepath.Join(data, "xtunnel.db"), filepath.Join(data, "credentials", "tunnel-token.key"), filepath.Join(data, "pki", "agent-gateway.key"), filepath.Join(data, proof+".key"), filepath.Join(data, proof+".db"), filepath.Join(data, proof+".db-wal"), filepath.Join(data, proof+".db-shm")}
}

func TestServiceTokenHelper(t *testing.T) {
	args := flag.Args()
	if len(args) != 6 || args[0] != "token-helper" {
		t.Skip("SCM helper entry only")
	}
	if err := svc.Run(args[1], &tokenProbeHandler{data: args[2], proof: args[3], report: args[4], lock: args[5], own: args[1] == xtunnelServerServiceName}); err != nil {
		t.Fatal(err)
	}
}

type tokenProbeHandler struct {
	data, proof, report, lock string
	own                       bool
}

func (probe *tokenProbeHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending}
	database, err := probe.check()
	if database != nil {
		defer database.Close()
	}
	result := "PASS"
	if err != nil {
		result = err.Error()
	}
	if writeErr := os.WriteFile(probe.report+".tmp", []byte(result), 0600); writeErr != nil {
		return false, 1
	}
	if err := os.Rename(probe.report+".tmp", probe.report); err != nil {
		return false, 1
	}
	if err != nil {
		return false, 1
	}
	statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for request := range requests {
		if request.Cmd == svc.Stop || request.Cmd == svc.Shutdown {
			statuses <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 1
}

func (probe *tokenProbeHandler) check() (*sql.DB, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user.User.Sid.String() != "S-1-5-19" {
		return nil, errors.New("SCM helper is not LocalService")
	}
	var database *sql.DB
	if probe.own {
		security, err := NewFileSecurityForPath(probe.data)
		if err != nil {
			return nil, err
		}
		if err := PublishForegroundFile(probe.data, probe.proof+".key", []byte("service-token-proof"), security); err != nil {
			return nil, err
		}
		database, err = sql.Open("sqlite", filepath.Join(probe.data, probe.proof+".db"))
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := database.ExecContext(ctx, "PRAGMA journal_mode=WAL; CREATE TABLE proof (id INTEGER); INSERT INTO proof VALUES (1)"); err != nil {
			return database, err
		}
	} else if err := validateServiceStorageToken(); err == nil {
		return nil, errors.New("foreign service selected XTunnelServer storage policy")
	}
	for _, path := range append(tokenProofPaths(probe.data, probe.proof), probe.lock) {
		pointer, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return database, err
		}
		for _, access := range []uint32{windows.FILE_READ_DATA, windows.WRITE_DAC, windows.WRITE_OWNER} {
			handle, openErr := windows.CreateFile(pointer, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
			allowed := probe.own && access == windows.FILE_READ_DATA
			if openErr == nil {
				windows.CloseHandle(handle)
				if !allowed {
					return database, fmt.Errorf("unexpected access 0x%x to %s", access, filepath.Base(path))
				}
			} else if allowed || !errors.Is(openErr, windows.ERROR_ACCESS_DENIED) {
				return database, fmt.Errorf("access 0x%x to %s: %w", access, filepath.Base(path), openErr)
			}
		}
		if probe.own && (strings.HasSuffix(path, ".key") || strings.HasSuffix(path, ".lock")) {
			security, err := NewFileSecurityForPath(filepath.Dir(path))
			if err != nil {
				return database, err
			}
			handle, err := openFileNoFollow(path)
			if err != nil {
				return database, err
			}
			validation := security.ValidateFile(handle)
			closeErr := windows.CloseHandle(handle)
			if err := errors.Join(validation, closeErr); err != nil {
				return database, err
			}
		}
	}
	return database, nil
}
