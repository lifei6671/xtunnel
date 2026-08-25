//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsServiceName        = "XTunnelAgent"
	windowsServiceDisplayName = "XTunnel Agent"
	windowsManagedMarker      = "Managed by xtunnel-agent service install"
	windowsServiceAccount     = `NT AUTHORITY\LocalService`
	windowsCredentialFile     = "agent.token.dpapi"
	windowsStateLimit         = 30 * time.Second
	windowsRecoveryDelay      = 5 * time.Second
)

func platformName() string {
	return windowsServiceName
}

type windowsInstallPaths struct {
	binary              string
	credentialDirectory string
	credential          string
}

func resolveWindowsInstallPaths() (windowsInstallPaths, error) {
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return windowsInstallPaths{}, fmt.Errorf("resolve Program Files: %w", err)
	}
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return windowsInstallPaths{}, fmt.Errorf("resolve ProgramData: %w", err)
	}
	if programFiles == "" || programData == "" {
		return windowsInstallPaths{}, errors.New("Windows install directories must not be empty")
	}
	credentialDirectory := filepath.Join(programData, "XTunnel", "credentials")
	return windowsInstallPaths{
		binary:              filepath.Join(programFiles, "XTunnel", "xtunnel-agent.exe"),
		credentialDirectory: credentialDirectory,
		credential:          filepath.Join(credentialDirectory, windowsCredentialFile),
	}, nil
}

func expectedWindowsImagePath(binary string) string {
	return syscall.EscapeArg(binary) + " run"
}

func expectedWindowsConfig(binary string) mgr.Config {
	return mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		BinaryPathName:   expectedWindowsImagePath(binary),
		ServiceStartName: windowsServiceAccount,
		DisplayName:      windowsServiceDisplayName,
		Description:      windowsManagedMarker,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}
}

func isExpectedManagedWindowsService(config mgr.Config, binary string) bool {
	want := expectedWindowsConfig(binary)
	return config.Description == want.Description &&
		strings.EqualFold(config.BinaryPathName, want.BinaryPathName) &&
		strings.EqualFold(config.ServiceStartName, want.ServiceStartName) &&
		config.DisplayName == want.DisplayName &&
		config.ServiceType == want.ServiceType &&
		config.StartType == want.StartType &&
		config.ErrorControl == want.ErrorControl &&
		config.SidType == want.SidType
}

func requireAdministrator() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("service management must run from an elevated Administrator terminal")
	}
	return nil
}

func replaceFile(source, destination string) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		sourcePointer,
		destinationPointer,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func install(ctx context.Context, token string) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	paths, err := resolveWindowsInstallPaths()
	if err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Disconnect()

	installedService, err := manager.OpenService(windowsServiceName)
	serviceExists := err == nil
	if err != nil && !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return fmt.Errorf("inspect Windows service: %w", err)
	}
	if serviceExists {
		defer installedService.Close()
		config, err := installedService.Config()
		if err != nil {
			return fmt.Errorf("inspect Windows service configuration: %w", err)
		}
		if !isExpectedManagedWindowsService(config, paths.binary) {
			return errors.New("refusing to overwrite an unmanaged or modified XTunnelAgent service")
		}
		if err := stopWindowsService(ctx, installedService); err != nil {
			return err
		}
	}

	if err := prepareWindowsInstallDirectories(paths); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	if !sameWindowsPath(executable, paths.binary) {
		if err := atomicCopyFile(paths.binary, executable, 0o755, -1, -1); err != nil {
			return fmt.Errorf("install Agent binary: %w", err)
		}
	}
	protectedToken, err := protectWindowsCredential([]byte(token))
	if err != nil {
		return fmt.Errorf("protect Agent credential with DPAPI: %w", err)
	}
	if err := atomicWriteBytes(paths.credential, protectedToken, 0o600, -1, -1); err != nil {
		return fmt.Errorf("install DPAPI credential: %w", err)
	}
	if err := applyWindowsCredentialACL(paths.credential, false); err != nil {
		return fmt.Errorf("protect DPAPI credential ACL: %w", err)
	}

	if !serviceExists {
		installedService, err = manager.CreateService(
			windowsServiceName,
			paths.binary,
			expectedWindowsConfig(paths.binary),
			"run",
		)
		if err != nil {
			return fmt.Errorf("create Windows service: %w", err)
		}
		defer installedService.Close()
	} else if err := installedService.UpdateConfig(expectedWindowsConfig(paths.binary)); err != nil {
		return fmt.Errorf("update Windows service configuration: %w", err)
	}
	if err := installedService.SetRecoveryActions(
		[]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: windowsRecoveryDelay}},
		24*60*60,
	); err != nil {
		return fmt.Errorf("configure Windows service recovery: %w", err)
	}
	if err := installedService.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("enable Windows service recovery for non-crash failures: %w", err)
	}
	if err := startWindowsService(ctx, installedService); err != nil {
		return err
	}
	return nil
}

func uninstall(ctx context.Context) (UninstallResult, error) {
	if err := requireAdministrator(); err != nil {
		return UninstallResult{}, err
	}
	paths, err := resolveWindowsInstallPaths()
	if err != nil {
		return UninstallResult{}, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return UninstallResult{}, fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Disconnect()

	installedService, err := manager.OpenService(windowsServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return UninstallResult{}, nil
	}
	if err != nil {
		return UninstallResult{}, fmt.Errorf("open Windows service: %w", err)
	}
	defer installedService.Close()
	config, err := installedService.Config()
	if err != nil {
		return UninstallResult{}, fmt.Errorf("inspect Windows service configuration: %w", err)
	}
	if !isExpectedManagedWindowsService(config, paths.binary) {
		return UninstallResult{}, errors.New("refusing to remove an unmanaged or modified XTunnelAgent service")
	}
	if err := stopWindowsService(ctx, installedService); err != nil {
		return UninstallResult{}, err
	}
	if err := installedService.Delete(); err != nil {
		return UninstallResult{}, fmt.Errorf("delete Windows service: %w", err)
	}
	scheduledForReboot, err := removeWindowsInstalledBinary(paths.binary)
	if err != nil {
		return UninstallResult{}, err
	}
	return UninstallResult{BinaryRemovalPendingReboot: scheduledForReboot}, nil
}

func removeWindowsInstalledBinary(path string) (scheduledForReboot bool, err error) {
	currentExecutable, executableErr := os.Executable()
	return removeWindowsBinary(
		path,
		currentExecutable,
		executableErr,
		os.Remove,
		func(path string) error {
			pathPointer, err := windows.UTF16PtrFromString(path)
			if err != nil {
				return err
			}
			return windows.MoveFileEx(pathPointer, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
		},
	)
}

func removeWindowsBinary(
	path string,
	currentExecutable string,
	currentExecutableErr error,
	remove func(string) error,
	delayUntilReboot func(string) error,
) (bool, error) {
	err := remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if currentExecutableErr == nil &&
		sameWindowsPath(currentExecutable, path) &&
		(errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)) {
		if delayErr := delayUntilReboot(path); delayErr != nil {
			return false, fmt.Errorf("schedule running Agent binary removal at reboot: %w", delayErr)
		}
		return true, nil
	}
	return false, fmt.Errorf("remove installed Agent binary: %w", err)
}

func prepareWindowsInstallDirectories(paths windowsInstallPaths) error {
	if err := os.MkdirAll(filepath.Dir(paths.binary), 0o755); err != nil {
		return fmt.Errorf("create Agent binary directory: %w", err)
	}
	if err := os.MkdirAll(paths.credentialDirectory, 0o700); err != nil {
		return fmt.Errorf("create Agent credential directory: %w", err)
	}
	if err := applyWindowsCredentialACL(paths.credentialDirectory, true); err != nil {
		return fmt.Errorf("protect Agent credential directory ACL: %w", err)
	}
	return nil
}

func sameWindowsPath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && strings.EqualFold(filepath.Clean(leftAbsolute), filepath.Clean(rightAbsolute))
}

func protectWindowsCredential(plainText []byte) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(plainText))}
	if len(plainText) > 0 {
		input.Data = &plainText[0]
	}
	var output windows.DataBlob
	if err := windows.CryptProtectData(
		&input,
		nil,
		nil,
		0,
		nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE|windows.CRYPTPROTECT_UI_FORBIDDEN,
		&output,
	); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	protected := append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...)
	runtime.KeepAlive(plainText)
	return protected, nil
}

func unprotectWindowsCredential(protected []byte) ([]byte, error) {
	if len(protected) == 0 {
		return nil, errors.New("DPAPI credential must not be empty")
	}
	input := windows.DataBlob{Size: uint32(len(protected)), Data: &protected[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(
		&input,
		nil,
		nil,
		0,
		nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&output,
	); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	plainText := append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...)
	runtime.KeepAlive(protected)
	return plainText, nil
}

func applyWindowsCredentialACL(path string, directory bool) error {
	descriptorText := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;LS)"
	if directory {
		descriptorText = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;LS)"
	}
	descriptor, err := windows.SecurityDescriptorFromString(descriptorText)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
	runtime.KeepAlive(descriptor)
	return err
}

func loadWindowsCredential() (string, error) {
	paths, err := resolveWindowsInstallPaths()
	if err != nil {
		return "", err
	}
	return loadWindowsCredentialFile(paths.credential)
}

func loadWindowsCredentialFile(credentialPath string) (string, error) {
	protected, err := os.ReadFile(credentialPath)
	if err != nil {
		return "", fmt.Errorf("read DPAPI credential: %w", err)
	}
	plainText, err := unprotectWindowsCredential(protected)
	if err != nil {
		return "", fmt.Errorf("decrypt DPAPI credential: %w", err)
	}
	return string(plainText), nil
}

func waitForWindowsServiceState(ctx context.Context, installedService *mgr.Service, target svc.State) error {
	waitContext, cancel := context.WithTimeout(ctx, windowsStateLimit)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := installedService.Query()
		if err != nil {
			return fmt.Errorf("query Windows service state: %w", err)
		}
		if status.State == target {
			return nil
		}
		select {
		case <-waitContext.Done():
			return fmt.Errorf("wait for Windows service state %d: %w", target, waitContext.Err())
		case <-ticker.C:
		}
	}
}

func stopWindowsService(ctx context.Context, installedService *mgr.Service) error {
	status, err := installedService.Query()
	if err != nil {
		return fmt.Errorf("query Windows service before stop: %w", err)
	}
	if status.State == svc.Stopped {
		return nil
	}
	if status.State != svc.StopPending {
		if _, err := installedService.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("stop Windows service: %w", err)
		}
	}
	if err := waitForWindowsServiceState(ctx, installedService, svc.Stopped); err != nil {
		return fmt.Errorf("verify Windows service stopped: %w", err)
	}
	return nil
}

func startWindowsService(ctx context.Context, installedService *mgr.Service) error {
	if err := installedService.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start Windows service: %w", err)
	}
	if err := waitForWindowsServiceState(ctx, installedService, svc.Running); err != nil {
		return fmt.Errorf("verify Windows service running: %w", err)
	}
	return nil
}

type windowsServiceHandler struct {
	callback func(context.Context, string) error
	load     func() (string, error)
	stopWait time.Duration
	err      error
}

func (handler *windowsServiceHandler) Execute(
	_ []string,
	requests <-chan svc.ChangeRequest,
	changes chan<- svc.Status,
) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	token, err := handler.load()
	if err != nil {
		handler.err = err
		return true, 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- handler.callback(ctx, token)
	}()
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("Agent runtime exited before a service stop request")
			}
			handler.err = err
			changes <- svc.Status{State: svc.StopPending}
			return true, 1
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				select {
				case handler.err = <-done:
					if handler.err != nil {
						return true, 1
					}
					return false, 0
				case <-time.After(handler.stopWait):
					handler.err = errors.New("Agent runtime did not stop before the Windows service deadline")
					return true, 1
				}
			}
		}
	}
}

func runIfManagedService(callback func(context.Context, string) error) (bool, error) {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return false, fmt.Errorf("detect Windows service context: %w", err)
	}
	if !isService {
		return false, nil
	}
	handler := &windowsServiceHandler{
		callback: callback,
		load:     loadWindowsCredential,
		stopWait: windowsStateLimit,
	}
	if err := svc.Run(windowsServiceName, handler); err != nil {
		return true, fmt.Errorf("run Windows service dispatcher: %w", err)
	}
	return true, handler.err
}
