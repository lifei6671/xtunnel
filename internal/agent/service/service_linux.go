//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const (
	minimumSystemdVersion = 249
	externalCommandLimit  = 30 * time.Second
	serviceUser           = "xtunnel-agent"
)

func platformName() string {
	return UnitName
}

func runIfManagedService(func(context.Context, string, io.Writer) error) (bool, error) {
	return false, nil
}

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}

func install(ctx context.Context, token string) error {
	if os.Geteuid() != 0 {
		return errors.New("service install must run as root")
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return errors.New("systemctl is required")
	}
	useradd, err := exec.LookPath("useradd")
	if err != nil {
		return errors.New("useradd is required")
	}
	if err := requireSupportedSystemd(ctx, systemctl, runExternal); err != nil {
		return err
	}

	exists, managed, err := inspectManagedUnit(UnitPath)
	if err != nil {
		return err
	}
	if exists && !managed {
		return errors.New("refusing to overwrite an unmanaged xtunnel-agent.service")
	}
	if err := ensureServiceUser(ctx, useradd); err != nil {
		return err
	}
	if err := prepareInstallDirectories(); err != nil {
		return err
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	publications := []linuxInstallPublication{
		{
			path:  BinaryPath,
			label: "install Agent binary",
			publish: func() error {
				return atomicCopyFile(BinaryPath, executable, 0o755, 0, 0)
			},
		},
		{
			path:  CredentialSourcePath,
			label: "install Agent Credential Source",
			publish: func() error {
				return atomicWriteBytes(CredentialSourcePath, []byte(token), 0o600, 0, 0)
			},
		},
		{
			path:  UnitPath,
			label: "install systemd unit",
			publish: func() error {
				return atomicWriteBytes(UnitPath, unitFile, 0o644, 0, 0)
			},
		},
	}
	rollbackContext := context.WithoutCancel(ctx)
	return applyLinuxInstall(
		publications,
		func() (serviceActivationProgress, error) {
			return activateManagedServiceTracked(ctx, systemctl, runExternal)
		},
		func(progress serviceActivationProgress) error {
			return rollbackLinuxActivationBeforeRestore(rollbackContext, systemctl, exists, progress)
		},
		func(progress serviceActivationProgress) error {
			return rollbackLinuxActivationAfterRestore(rollbackContext, systemctl, exists, progress)
		},
	)
}

type linuxInstallPublication struct {
	path    string
	label   string
	publish func() error
}

type linuxFileSnapshot struct {
	path       string
	backupPath string
	existed    bool
}

type linuxInstallTransaction struct {
	snapshots []linuxFileSnapshot
}

func beginLinuxInstallTransaction(paths []string) (*linuxInstallTransaction, error) {
	transaction := &linuxInstallTransaction{}
	for _, path := range paths {
		snapshot := linuxFileSnapshot{path: path}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			transaction.snapshots = append(transaction.snapshots, snapshot)
			continue
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("inspect existing %s: %w", path, err), transaction.discard())
		}
		if !info.Mode().IsRegular() {
			return nil, errors.Join(fmt.Errorf("existing install target %s is not a regular file", path), transaction.discard())
		}
		backup, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".rollback-*")
		if err != nil {
			return nil, errors.Join(fmt.Errorf("create rollback path for %s: %w", path, err), transaction.discard())
		}
		backupPath := backup.Name()
		if err := backup.Close(); err != nil {
			_ = os.Remove(backupPath)
			return nil, errors.Join(fmt.Errorf("close rollback path for %s: %w", path, err), transaction.discard())
		}
		if err := os.Remove(backupPath); err != nil {
			return nil, errors.Join(fmt.Errorf("prepare rollback path for %s: %w", path, err), transaction.discard())
		}
		if err := os.Link(path, backupPath); err != nil {
			return nil, errors.Join(fmt.Errorf("snapshot existing %s: %w", path, err), transaction.discard())
		}
		snapshot.existed = true
		snapshot.backupPath = backupPath
		transaction.snapshots = append(transaction.snapshots, snapshot)
	}
	return transaction, nil
}

func (transaction *linuxInstallTransaction) rollback() error {
	var rollbackErrors []error
	directories := make(map[string]struct{})
	for _, snapshot := range transaction.snapshots {
		directories[filepath.Dir(snapshot.path)] = struct{}{}
		if snapshot.existed {
			targetInfo, targetErr := os.Stat(snapshot.path)
			backupInfo, backupErr := os.Stat(snapshot.backupPath)
			if targetErr == nil && backupErr == nil && os.SameFile(targetInfo, backupInfo) {
				if err := os.Remove(snapshot.backupPath); err != nil {
					rollbackErrors = append(rollbackErrors, fmt.Errorf("remove unchanged rollback snapshot for %s: %w", snapshot.path, err))
				}
				continue
			}
			if err := os.Rename(snapshot.backupPath, snapshot.path); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore %s: %w", snapshot.path, err))
			}
			continue
		}
		if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("remove newly installed %s: %w", snapshot.path, err))
		}
	}
	for directory := range directories {
		if err := syncDirectory(directory); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("sync rollback directory %s: %w", directory, err))
		}
	}
	return errors.Join(rollbackErrors...)
}

func (transaction *linuxInstallTransaction) discard() error {
	var discardErrors []error
	directories := make(map[string]struct{})
	for _, snapshot := range transaction.snapshots {
		if !snapshot.existed {
			continue
		}
		directories[filepath.Dir(snapshot.path)] = struct{}{}
		if err := os.Remove(snapshot.backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			discardErrors = append(discardErrors, fmt.Errorf("remove rollback snapshot for %s: %w", snapshot.path, err))
		}
	}
	for directory := range directories {
		if err := syncDirectory(directory); err != nil {
			discardErrors = append(discardErrors, fmt.Errorf("sync snapshot directory %s: %w", directory, err))
		}
	}
	return errors.Join(discardErrors...)
}

func applyLinuxInstall(
	publications []linuxInstallPublication,
	activate func() (serviceActivationProgress, error),
	beforeRestore func(serviceActivationProgress) error,
	afterRestore func(serviceActivationProgress) error,
) error {
	paths := make([]string, 0, len(publications))
	for _, publication := range publications {
		paths = append(paths, publication.path)
	}
	transaction, err := beginLinuxInstallTransaction(paths)
	if err != nil {
		return err
	}
	for _, publication := range publications {
		if err := publication.publish(); err != nil {
			return errors.Join(
				fmt.Errorf("%s: %w", publication.label, err),
				wrapLinuxInstallRollbackError(transaction.rollback()),
			)
		}
	}
	progress, err := activate()
	if err != nil {
		beforeRestoreErr := beforeRestore(progress)
		rollbackErr := transaction.rollback()
		var afterRestoreErr error
		if rollbackErr == nil {
			afterRestoreErr = afterRestore(progress)
		}
		return errors.Join(
			err,
			wrapLinuxInstallRecoveryError("prepare service rollback", beforeRestoreErr),
			wrapLinuxInstallRollbackError(rollbackErr),
			wrapLinuxInstallRecoveryError("restore previous service state", afterRestoreErr),
		)
	}
	if err := transaction.discard(); err != nil {
		return fmt.Errorf("remove install rollback snapshots: %w", err)
	}
	return nil
}

func wrapLinuxInstallRollbackError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("roll back Agent install files: %w", err)
}

func wrapLinuxInstallRecoveryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func rollbackLinuxActivationBeforeRestore(
	ctx context.Context,
	systemctl string,
	unitPreviouslyExisted bool,
	progress serviceActivationProgress,
) error {
	if unitPreviouslyExisted || !progress.enableAttempted {
		return nil
	}
	if _, err := runExternal(ctx, systemctl, "disable", "--now", UnitName); err != nil {
		return fmt.Errorf("disable partially installed Agent service: %w", err)
	}
	return nil
}

func rollbackLinuxActivationAfterRestore(
	ctx context.Context,
	systemctl string,
	unitPreviouslyExisted bool,
	progress serviceActivationProgress,
) error {
	if _, err := runExternal(ctx, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("reload restored systemd unit: %w", err)
	}
	if !unitPreviouslyExisted || !progress.restartAttempted {
		return nil
	}
	if _, err := runExternal(ctx, systemctl, "restart", UnitName); err != nil {
		return fmt.Errorf("restart restored Agent service: %w", err)
	}
	if _, err := runExternal(ctx, systemctl, "is-active", "--quiet", UnitName); err != nil {
		return fmt.Errorf("verify restored Agent service is active: %w", err)
	}
	return nil
}

func uninstall(ctx context.Context) (UninstallResult, error) {
	if os.Geteuid() != 0 {
		return UninstallResult{}, errors.New("service uninstall must run as root")
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return UninstallResult{}, errors.New("systemctl is required")
	}
	if err := requireSupportedSystemd(ctx, systemctl, runExternal); err != nil {
		return UninstallResult{}, err
	}
	exists, managed, err := inspectManagedUnit(UnitPath)
	if err != nil {
		return UninstallResult{}, err
	}
	if exists && !managed {
		return UninstallResult{}, errors.New("refusing to remove an unmanaged xtunnel-agent.service")
	}
	if exists {
		if _, err := runExternal(ctx, systemctl, "disable", "--now", UnitName); err != nil {
			return UninstallResult{}, fmt.Errorf("disable and stop Agent service: %w", err)
		}
		if err := os.Remove(UnitPath); err != nil {
			return UninstallResult{}, fmt.Errorf("remove managed systemd unit: %w", err)
		}
		if err := os.Remove(BinaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return UninstallResult{}, fmt.Errorf("remove installed Agent binary: %w", err)
		}
		if err := syncDirectory(filepath.Dir(UnitPath)); err != nil {
			return UninstallResult{}, fmt.Errorf("sync systemd unit directory: %w", err)
		}
		if err := syncDirectory(filepath.Dir(BinaryPath)); err != nil {
			return UninstallResult{}, fmt.Errorf("sync binary directory: %w", err)
		}
	}
	if _, err := runExternal(ctx, systemctl, "daemon-reload"); err != nil {
		return UninstallResult{}, fmt.Errorf("reload systemd units: %w", err)
	}
	return UninstallResult{}, nil
}

// install 和 uninstall 都必须在触碰 Unit、Binary、Credential 或服务身份前确认
// 当前 systemd 处于支持矩阵内，避免旧版本只执行部分服务生命周期操作。
func requireSupportedSystemd(
	ctx context.Context,
	systemctl string,
	run func(context.Context, string, ...string) (string, error),
) error {
	versionOutput, err := run(ctx, systemctl, "show", "--property=Version", "--value")
	if err != nil {
		return fmt.Errorf("query running systemd: %w", err)
	}
	version, err := parseSystemdVersion(versionOutput)
	if err != nil {
		return err
	}
	if version < minimumSystemdVersion {
		return fmt.Errorf("systemd %d or newer is required; found %d", minimumSystemdVersion, version)
	}
	return nil
}

func ensureServiceUser(ctx context.Context, useradd string) error {
	if _, err := user.Lookup(serviceUser); err == nil {
		return nil
	} else if _, ok := err.(user.UnknownUserError); !ok {
		return fmt.Errorf("look up service user: %w", err)
	}
	if _, err := runExternal(
		ctx,
		useradd,
		"--system",
		"--user-group",
		"--home-dir", "/nonexistent",
		"--shell", "/usr/sbin/nologin",
		serviceUser,
	); err != nil {
		return fmt.Errorf("create service user: %w", err)
	}
	return nil
}

func prepareInstallDirectories() error {
	for _, directory := range []string{filepath.Dir(BinaryPath), filepath.Dir(UnitPath)} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create install directory %s: %w", directory, err)
		}
	}
	if err := os.MkdirAll(CredentialDirectory, 0o700); err != nil {
		return fmt.Errorf("create Credential Source directory: %w", err)
	}
	if err := os.Chmod(CredentialDirectory, 0o700); err != nil {
		return fmt.Errorf("set Credential Source directory mode: %w", err)
	}
	if err := os.Chown(CredentialDirectory, 0, 0); err != nil {
		return fmt.Errorf("set Credential Source directory owner: %w", err)
	}
	return nil
}

func runExternal(ctx context.Context, path string, args ...string) (string, error) {
	commandContext, cancel := context.WithTimeout(ctx, externalCommandLimit)
	defer cancel()
	output, err := exec.CommandContext(commandContext, path, args...).CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(output)), nil
	}
	name := filepath.Base(path)
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%s timed out", name)
	}
	if errors.Is(commandContext.Err(), context.Canceled) {
		return "", fmt.Errorf("%s canceled", name)
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		return "", fmt.Errorf("%s failed: %w", name, err)
	}
	return "", fmt.Errorf("%s failed: %s: %w", name, message, err)
}
