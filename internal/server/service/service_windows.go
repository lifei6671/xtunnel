//go:build windows

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsServiceName    = "XTunnelServer"
	windowsManagedMarker  = "Managed by xtunnel-server service install"
	windowsServiceAccount = `NT AUTHORITY\LocalService`
	windowsStateLimit     = 30 * time.Second
)

type windowsInstallPaths struct{ binary, config, root, data, runtime string }

func resolveWindowsInstallPaths() (windowsInstallPaths, error) {
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return windowsInstallPaths{}, err
	}
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return windowsInstallPaths{}, err
	}
	root := filepath.Join(pd, "XTunnel", "Server")
	return windowsInstallPaths{filepath.Join(pf, "XTunnel", "xtunnel-server.exe"), filepath.Join(pd, "XTunnel", "server.yaml"), root, filepath.Join(root, "data"), filepath.Join(root, "runtime")}, nil
}

// FixedConfigPath returns the SCM-owned absolute configuration path.
func FixedConfigPath() (string, error) { p, err := resolveWindowsInstallPaths(); return p.config, err }

// ReadConfig validates and reads the managed configuration using one no-follow handle.
func ReadConfig() ([]byte, error) {
	p, err := resolveWindowsInstallPaths()
	if err != nil {
		return nil, err
	}
	return winsecurity.ReadServiceFile(p.config, winsecurity.ServiceConfig)
}

// validateRuntimeService 仅请求查询权限，LocalService 不需要管理 SCM 的权限。
func validateRuntimeService() (resultErr error) {
	p, err := resolveWindowsInstallPaths()
	if err != nil {
		return err
	}
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseServiceHandle(manager)) }()
	name, err := windows.UTF16PtrFromString(windowsServiceName)
	if err != nil {
		return err
	}
	handle, err := windows.OpenService(manager, name, windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return err
	}
	s := &mgr.Service{Name: windowsServiceName, Handle: handle}
	defer func() { resultErr = errors.Join(resultErr, s.Close()) }()
	return validateManagedService(s, p)
}
func expectedWindowsConfig(p windowsInstallPaths) mgr.Config {
	return mgr.Config{ServiceType: windows.SERVICE_WIN32_OWN_PROCESS, StartType: mgr.StartAutomatic, ErrorControl: mgr.ErrorNormal, BinaryPathName: syscall.EscapeArg(p.binary) + " --config " + syscall.EscapeArg(p.config), ServiceStartName: windowsServiceAccount, DisplayName: "XTunnel Server", Description: windowsManagedMarker, SidType: windows.SERVICE_SID_TYPE_UNRESTRICTED}
}
func isExpectedManagedWindowsService(c mgr.Config, p windowsInstallPaths) bool {
	w := expectedWindowsConfig(p)
	return c.ServiceType == w.ServiceType && c.StartType == w.StartType && c.ErrorControl == w.ErrorControl && strings.EqualFold(c.BinaryPathName, w.BinaryPathName) && strings.EqualFold(c.ServiceStartName, w.ServiceStartName) && c.DisplayName == w.DisplayName && c.Description == w.Description && c.SidType == w.SidType && len(c.Dependencies) == 0 && !c.DelayedAutoStart && c.LoadOrderGroup == ""
}
func expectedRecovery() []mgr.RecoveryAction {
	return []mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.ServiceRestart, Delay: 5 * time.Second}, {Type: mgr.NoAction}}
}
func validateManagedService(s *mgr.Service, p windowsInstallPaths) error {
	c, err := s.Config()
	if err != nil {
		return err
	}
	if !isExpectedManagedWindowsService(c, p) {
		return errors.New("XTunnelServer SCM configuration is unmanaged or modified")
	}
	actions, err := s.RecoveryActions()
	if err != nil {
		return err
	}
	period, err := s.ResetPeriod()
	if err != nil {
		return err
	}
	noncrash, err := s.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return err
	}
	command, err := s.RecoveryCommand()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actions, expectedRecovery()) || period != 86400 || !noncrash || command != "" {
		return errors.New("XTunnelServer recovery policy is modified")
	}
	return nil
}
func requireAdministrator() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("Server service management requires an elevated Administrator terminal")
	}
	return nil
}

// RequireStopped verifies the managed SCM identity before offline maintenance opens storage.
func RequireStopped() (resultErr error) {
	if err := requireAdministrator(); err != nil {
		return err
	}
	p, err := resolveWindowsInstallPaths()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, m.Disconnect()) }()
	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.Close()) }()
	if err := validateManagedService(s, p); err != nil {
		return err
	}
	status, err := s.Query()
	if err != nil {
		return err
	}
	if status.State != svc.Stopped {
		return errors.New("XTunnelServer must be stopped before offline maintenance")
	}
	return requireManagedWindowsEventSource(registryWindowsEventSourceStore{})
}
func checkNTFS(path string) error {
	root, err := windows.UTF16PtrFromString(filepath.VolumeName(path) + `\`)
	if err != nil {
		return err
	}
	if windows.GetDriveType(root) != windows.DRIVE_FIXED {
		return errors.New("Server service paths require local fixed volumes")
	}
	var filesystem [32]uint16
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil {
		return err
	}
	if windows.UTF16ToString(filesystem[:]) != "NTFS" {
		return errors.New("Server service paths require NTFS")
	}
	return nil
}

// preflightObjects is read-only; missing leaves are admitted only for CREATE_NEW publication.
func preflightObjects(p windowsInstallPaths) error {
	for _, item := range []struct {
		path string
		kind winsecurity.ServiceObjectKind
		dir  bool
	}{{p.binary, winsecurity.ServiceBinary, false}, {p.config, winsecurity.ServiceConfig, false}, {p.root, winsecurity.ServiceConfig, true}, {p.data, winsecurity.ServiceData, true}, {p.runtime, winsecurity.ServiceData, true}} {
		if err := checkNTFS(item.path); err != nil {
			return err
		}
		if err := winsecurity.ValidateServiceAncestors(item.path); err != nil {
			return err
		}
		if _, err := os.Lstat(item.path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := winsecurity.ValidateServiceObject(item.path, item.kind, item.dir); err != nil {
			return err
		}
	}
	return nil
}
func install(ctx context.Context, source string) (resultErr error) {
	if err := requireAdministrator(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := resolveWindowsInstallPaths()
	if err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	info, err := input.Stat()
	if err != nil {
		return errors.Join(err, input.Close())
	}
	if !info.Mode().IsRegular() {
		return errors.Join(errors.New("configuration source must be a regular file"), input.Close())
	}
	content, err := io.ReadAll(input)
	closeErr := input.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	configuration, err := serverconfig.LoadService(baseconfig.Options{YAML: content})
	if err != nil {
		return fmt.Errorf("validate service configuration: %w", err)
	}
	if configuration.AgentGateway.TLS.Mode == gateway.PublicMode {
		// 复用 Gateway 的只读身份入口：同 Handle 验证外部权限并解析密钥对，
		// 安装器绝不接管外部证书，也不创建尚未安装的 Data 对象。
		if _, err := gateway.LoadPublicIdentity(configuration.AgentGateway.TLS.CertFile, configuration.AgentGateway.TLS.KeyFile); err != nil {
			return fmt.Errorf("validate service public TLS identity: %w", err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.EqualFold(filepath.Clean(executable), p.binary) {
		return errors.New("service installation must use an external deployment executable")
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, m.Disconnect()) }()
	s, err := m.OpenService(windowsServiceName)
	if err == nil {
		defer func() { resultErr = errors.Join(resultErr, s.Close()) }()
		if err := validateManagedService(s, p); err != nil {
			return err
		}
		return errors.New("XTunnelServer is already installed; use a maintenance window for upgrades")
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	exists, managed, err := (registryWindowsEventSourceStore{}).inspect(windowsEventLogSource)
	if err != nil {
		return err
	}
	if exists && !managed {
		return errors.New("refusing unmanaged XTunnelServer Event Source")
	}
	if err := preflightObjects(p); err != nil {
		return err
	}
	// 无 SCM Marker 时不能把残留 Binary 当作本产品版本接管；安装失败保留现场供维护。
	if _, err := os.Lstat(p.binary); err == nil {
		return errors.New("installed Server binary exists without managed SCM identity")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	configExists := false
	if old, err := winsecurity.ReadServiceFile(p.config, winsecurity.ServiceConfig); err == nil {
		configExists = true
		if !bytes.Equal(old, content) {
			return errors.New("retained Server configuration differs from install source")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, shared := range []string{filepath.Dir(p.binary), filepath.Dir(p.config)} {
		if _, err := os.Lstat(shared); errors.Is(err, os.ErrNotExist) {
			if err := winsecurity.CreateServiceSharedDirectory(shared); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	for _, item := range []struct {
		path string
		kind winsecurity.ServiceObjectKind
	}{{p.root, winsecurity.ServiceConfig}, {p.data, winsecurity.ServiceData}, {p.runtime, winsecurity.ServiceData}} {
		if _, err := os.Lstat(item.path); errors.Is(err, os.ErrNotExist) {
			if err := winsecurity.CreateServiceDirectory(item.path, item.kind); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	if !configExists {
		if err := winsecurity.CreateServiceFile(p.config, winsecurity.ServiceConfig, content); err != nil {
			return err
		}
	}
	if err := winsecurity.CreateServiceFile(p.binary, winsecurity.ServiceBinary, binary); err != nil {
		return err
	}
	if _, err := ensureWindowsEventSource(registryWindowsEventSourceStore{}); err != nil {
		return err
	}
	s, err = m.CreateService(windowsServiceName, p.binary, expectedWindowsConfig(p), "--config", p.config)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.Close()) }()
	if err := s.SetRecoveryActions(expectedRecovery(), 86400); err != nil {
		return err
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}
	return waitWindowsService(ctx, s, svc.Running)
}
func waitWindowsService(ctx context.Context, s *mgr.Service, target svc.State) error {
	ctx, cancel := context.WithTimeout(ctx, windowsStateLimit)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := s.Query()
		if err != nil {
			return err
		}
		if state.State == target {
			return nil
		}
		if target == svc.Running && state.State == svc.Stopped {
			return errors.New("Server service stopped during startup")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func uninstall(ctx context.Context) (resultErr error) {
	if err := requireAdministrator(); err != nil {
		return err
	}
	p, err := resolveWindowsInstallPaths()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.EqualFold(filepath.Clean(executable), p.binary) {
		return errors.New("service uninstall requires an external deployment executable")
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, m.Disconnect()) }()
	s, err := m.OpenService(windowsServiceName)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, s.Close()) }()
	if err := validateManagedService(s, p); err != nil {
		return err
	}
	if err := requireManagedWindowsEventSource(registryWindowsEventSourceStore{}); err != nil {
		return err
	}
	for _, path := range []string{p.binary, p.config, p.root, p.data, p.runtime} {
		if _, err := os.Lstat(path); err != nil {
			return fmt.Errorf("inspect complete managed Server installation: %w", err)
		}
	}
	if err := preflightObjects(p); err != nil {
		return err
	}
	status, err := s.Query()
	if err != nil {
		return err
	}
	if status.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
		if err := waitWindowsService(ctx, s, svc.Stopped); err != nil {
			return err
		}
	}
	// 已停止且所有受管身份通过后才删除；配置、数据和 Runtime 锁文件原样保留。
	if err := winsecurity.DeleteServiceFile(p.binary, winsecurity.ServiceBinary); err != nil {
		return err
	}
	if err := s.Delete(); err != nil {
		return err
	}
	return removeManagedWindowsEventSource(registryWindowsEventSourceStore{})
}
