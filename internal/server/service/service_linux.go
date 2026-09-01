//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	minimumSystemdVersion = 249
	externalCommandLimit  = 30 * time.Second
	serviceUser           = "xtunnel-server"
)

var legacyServerPaths = []string{
	"/var/lib/xtunnel/xtunnel.db",
	"/var/lib/xtunnel/xtunnel.db-wal",
	"/var/lib/xtunnel/xtunnel.db-shm",
	"/var/lib/xtunnel/credentials",
	"/var/lib/xtunnel/pki",
}

func install(ctx context.Context, configSource string) error {
	if os.Geteuid() != 0 {
		return errors.New("service install must run as root")
	}
	configInfo, err := os.Stat(configSource)
	if err != nil {
		return fmt.Errorf("inspect Server configuration source: %w", err)
	}
	if !configInfo.Mode().IsRegular() {
		return errors.New("service install --config must name an existing regular file")
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
	if err := rejectLegacyServerLayout(legacyServerPaths); err != nil {
		return err
	}
	exists, managed, err := inspectOwnedUnit(UnitPath)
	if err != nil {
		return err
	}
	if exists && !managed {
		return errors.New("refusing to overwrite an unmanaged xtunnel-server.service")
	}
	previousState, err := inspectPreviousServiceState(ctx, systemctl, exists, runExternal)
	if err != nil {
		return err
	}
	serviceIdentity, err := ensureServiceUser(ctx, useradd)
	if err != nil {
		return err
	}
	serviceGroup, err := user.LookupGroup(serviceUser)
	if err != nil {
		return fmt.Errorf("look up Server service group: %w", err)
	}
	uid, gid, err := numericIdentity(serviceIdentity.Uid, serviceGroup.Gid)
	if err != nil {
		return err
	}
	if err := prepareInstallDirectories(uid, gid); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}

	publications := []linuxInstallPublication{
		{
			path:  BinaryPath,
			label: "install Server binary",
			publish: func() error {
				return atomicCopyFile(BinaryPath, executable, 0o755, 0, 0)
			},
		},
		{
			path:  ConfigPath,
			label: "install Server configuration",
			publish: func() error {
				return atomicCopyFile(ConfigPath, configSource, 0o640, 0, gid)
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
			return activateManagedService(ctx, systemctl, runExternal)
		},
		func(progress serviceActivationProgress) error {
			return rollbackLinuxActivationBeforeRestore(rollbackContext, systemctl, previousState, progress, runExternal)
		},
		func(progress serviceActivationProgress) error {
			return rollbackLinuxActivationAfterRestore(rollbackContext, systemctl, previousState, progress, runExternal)
		},
	)
}

func uninstall(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("service uninstall must run as root")
	}
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return errors.New("systemctl is required")
	}
	if err := requireSupportedSystemd(ctx, systemctl, runExternal); err != nil {
		return err
	}
	exists, managed, err := inspectOwnedUnit(UnitPath)
	if err != nil {
		return err
	}
	if exists && !managed {
		return errors.New("refusing to remove an unmanaged xtunnel-server.service")
	}
	if exists {
		if _, err := runExternal(ctx, systemctl, "disable", "--now", UnitName); err != nil {
			return fmt.Errorf("disable and stop Server service: %w", err)
		}
		if err := os.Remove(UnitPath); err != nil {
			return fmt.Errorf("remove managed systemd unit: %w", err)
		}
		if err := os.Remove(BinaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove installed Server binary: %w", err)
		}
		if err := syncDirectory(filepath.Dir(UnitPath)); err != nil {
			return fmt.Errorf("sync systemd unit directory: %w", err)
		}
		if err := syncDirectory(filepath.Dir(BinaryPath)); err != nil {
			return fmt.Errorf("sync binary directory: %w", err)
		}
	}
	if _, err := runExternal(ctx, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd units: %w", err)
	}
	return nil
}

func rejectLegacyServerLayout(paths []string) error {
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("legacy Server data layout is not migrated automatically: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect legacy Server data layout %s: %w", path, err)
		}
	}
	return nil
}

func ensureServiceUser(ctx context.Context, useradd string) (*user.User, error) {
	identity, err := user.Lookup(serviceUser)
	if err == nil {
		return identity, nil
	}
	if _, ok := err.(user.UnknownUserError); !ok {
		return nil, fmt.Errorf("look up Server service user: %w", err)
	}
	if _, err := runExternal(
		ctx,
		useradd,
		"--system",
		"--user-group",
		"--home-dir", "/var/lib/xtunnel",
		"--shell", "/usr/sbin/nologin",
		serviceUser,
	); err != nil {
		return nil, fmt.Errorf("create Server service user: %w", err)
	}
	identity, err = user.Lookup(serviceUser)
	if err != nil {
		return nil, fmt.Errorf("look up created Server service user: %w", err)
	}
	return identity, nil
}

func numericIdentity(uidText, gidText string) (int, int, error) {
	uid, err := strconv.Atoi(uidText)
	if err != nil {
		return 0, 0, fmt.Errorf("parse Server service user ID: %w", err)
	}
	gid, err := strconv.Atoi(gidText)
	if err != nil {
		return 0, 0, fmt.Errorf("parse Server service group ID: %w", err)
	}
	return uid, gid, nil
}

// 目录在文件事务前建立：systemd 的 StateDirectory 只保证父目录，Server 的
// Canonical Data Target 必须由安装 owner 预先创建，并保持仅服务身份可访问。
func prepareInstallDirectories(uid, gid int) error {
	for _, directory := range []string{filepath.Dir(BinaryPath), filepath.Dir(UnitPath)} {
		if err := ensureInstallDirectory(directory, 0o755); err != nil {
			return fmt.Errorf("create install directory %s: %w", directory, err)
		}
	}
	configDirectory := filepath.Dir(ConfigPath)
	// Agent 先安装时可能用 root-only mode 创建 /etc/xtunnel。Server 配置属于
	// root:xtunnel-server 0640，父目录必须允许服务组穿透；Credential 子目录仍保持 0700。
	if err := secureInstallDirectory(configDirectory, 0o755, 0, 0); err != nil {
		return fmt.Errorf("prepare Server config directory: %w", err)
	}
	if err := secureInstallDirectory(DataDirectory, 0o700, uid, gid); err != nil {
		return fmt.Errorf("prepare Server data directory: %w", err)
	}
	return nil
}

func ensureInstallDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(path, mode)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("existing path is not a directory")
	}
	return nil
}

func secureInstallDirectory(path string, mode os.FileMode, uid, gid int) error {
	if err := ensureInstallDirectory(path, mode); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if uid >= 0 && gid >= 0 {
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

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

type serviceActivationProgress struct {
	enableAttempted  bool
	restartAttempted bool
}

type previousServiceState struct {
	unitExisted bool
	enablement  string
	active      bool
}

func inspectPreviousServiceState(
	ctx context.Context,
	systemctl string,
	unitExisted bool,
	run func(context.Context, string, ...string) (string, error),
) (previousServiceState, error) {
	state := previousServiceState{unitExisted: unitExisted}
	if !unitExisted {
		return state, nil
	}
	unitFileState, err := run(ctx, systemctl, "show", "--property=UnitFileState", "--value", UnitName)
	if err != nil {
		return state, fmt.Errorf("query previous Server enablement state: %w", err)
	}
	switch unitFileState {
	case "enabled", "enabled-runtime", "disabled":
		state.enablement = unitFileState
	default:
		return state, fmt.Errorf("unsupported previous Server UnitFileState %q", unitFileState)
	}
	activeState, err := run(ctx, systemctl, "show", "--property=ActiveState", "--value", UnitName)
	if err != nil {
		return state, fmt.Errorf("query previous Server active state: %w", err)
	}
	switch activeState {
	case "active":
		state.active = true
	case "inactive", "failed":
		state.active = false
	default:
		return state, fmt.Errorf("refusing to replace Server service in transitional ActiveState %q", activeState)
	}
	return state, nil
}

func activateManagedService(
	ctx context.Context,
	systemctl string,
	run func(context.Context, string, ...string) (string, error),
) (serviceActivationProgress, error) {
	var progress serviceActivationProgress
	if _, err := run(ctx, systemctl, "daemon-reload"); err != nil {
		return progress, fmt.Errorf("reload systemd units: %w", err)
	}
	progress.enableAttempted = true
	if _, err := run(ctx, systemctl, "enable", UnitName); err != nil {
		return progress, fmt.Errorf("enable Server service: %w", err)
	}
	progress.restartAttempted = true
	if _, err := run(ctx, systemctl, "restart", UnitName); err != nil {
		return progress, fmt.Errorf("restart Server service: %w", err)
	}
	if _, err := run(ctx, systemctl, "is-active", "--quiet", UnitName); err != nil {
		return progress, fmt.Errorf("verify Server service is active: %w", err)
	}
	return progress, nil
}

func rollbackLinuxActivationBeforeRestore(
	ctx context.Context,
	systemctl string,
	previous previousServiceState,
	progress serviceActivationProgress,
	run func(context.Context, string, ...string) (string, error),
) error {
	if !progress.enableAttempted {
		return nil
	}
	if !previous.unitExisted {
		if _, err := run(ctx, systemctl, "disable", "--now", UnitName); err != nil {
			return fmt.Errorf("disable partially installed Server service: %w", err)
		}
		return nil
	}
	if !progress.restartAttempted {
		return nil
	}
	if _, err := run(ctx, systemctl, "stop", UnitName); err != nil {
		return fmt.Errorf("stop failed Server upgrade before file restore: %w", err)
	}
	return nil
}

func restorePreviousEnablement(
	ctx context.Context,
	systemctl string,
	previous previousServiceState,
	run func(context.Context, string, ...string) (string, error),
) error {
	var args []string
	switch previous.enablement {
	case "enabled":
		args = []string{"enable", UnitName}
	case "enabled-runtime":
		// 本轮 install 使用持久 enable；先移除它及旧 runtime link，再精确恢复
		// runtime-only enablement，避免失败升级改变下次开机行为。
		if _, err := run(ctx, systemctl, "disable", UnitName); err != nil {
			return fmt.Errorf("remove persistent Server enablement before runtime restore: %w", err)
		}
		args = []string{"enable", "--runtime", UnitName}
	case "disabled":
		args = []string{"disable", UnitName}
	default:
		return fmt.Errorf("cannot restore unsupported Server enablement state %q", previous.enablement)
	}
	if _, err := run(ctx, systemctl, args...); err != nil {
		return fmt.Errorf("restore Server enablement state %s: %w", previous.enablement, err)
	}
	return nil
}

func restorePreviousActivity(
	ctx context.Context,
	systemctl string,
	previous previousServiceState,
	run func(context.Context, string, ...string) (string, error),
) error {
	if !previous.active {
		if _, err := run(ctx, systemctl, "stop", UnitName); err != nil {
			return fmt.Errorf("stop restored Server service: %w", err)
		}
		return nil
	}
	if _, err := run(ctx, systemctl, "restart", UnitName); err != nil {
		return fmt.Errorf("restart restored Server service: %w", err)
	}
	if _, err := run(ctx, systemctl, "is-active", "--quiet", UnitName); err != nil {
		return fmt.Errorf("verify restored Server service is active: %w", err)
	}
	return nil
}

func rollbackLinuxActivationAfterRestore(
	ctx context.Context,
	systemctl string,
	previous previousServiceState,
	progress serviceActivationProgress,
	run func(context.Context, string, ...string) (string, error),
) error {
	if _, err := run(ctx, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("reload restored systemd unit: %w", err)
	}
	if !previous.unitExisted {
		return nil
	}
	var restoreErrors []error
	if progress.enableAttempted {
		restoreErrors = append(restoreErrors, restorePreviousEnablement(ctx, systemctl, previous, run))
	}
	if progress.restartAttempted {
		restoreErrors = append(restoreErrors, restorePreviousActivity(ctx, systemctl, previous, run))
	}
	return errors.Join(restoreErrors...)
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

// 文件发布和服务激活属于一个逻辑事务。激活失败时先停掉本轮新建服务，再逆序
// 恢复文件，最后重新加载并恢复此前已存在的受管服务，避免部分升级被误报成功。
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
			return errors.Join(fmt.Errorf("%s: %w", publication.label, err), wrapRollbackError(transaction.rollback()))
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
			wrapRecoveryError("prepare service rollback", beforeRestoreErr),
			wrapRollbackError(rollbackErr),
			wrapRecoveryError("restore previous service state", afterRestoreErr),
		)
	}
	if err := transaction.discard(); err != nil {
		return fmt.Errorf("remove install rollback snapshots: %w", err)
	}
	return nil
}

func wrapRollbackError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("roll back Server install files: %w", err)
}

func wrapRecoveryError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
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
