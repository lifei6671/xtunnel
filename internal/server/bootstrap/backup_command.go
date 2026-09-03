package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/durableops"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
)

// backupCommandOptions 汇总 backup 子命令解析后的归档路径和统一配置输入。
// path 在 create 中表示输出，在 restore 中表示输入；两种情况都必须是绝对路径。
type backupCommandOptions struct {
	path   string
	config baseconfig.Options
}

// backupLease 表示运行中 Server 授予 CLI 的在线写屏障租约。
// BindContext 把服务端提前断开传播为备份取消，Close 则完成显式 Release/ACK；
// 两者共同保证 CLI 不会在失去屏障后仍把归档报告为成功。
type backupLease interface {
	io.Closer
	BindContext(context.Context) (context.Context, context.CancelFunc)
}

// runBackupCreate 创建与固定 data target 绑定的备份归档。
//
// 运行中的 Server 通过 Backup Socket 授予写屏障，CLI 必须持有租约直到归档、
// 输出文件及其父目录全部落盘，并在发布成功前收到 Release ACK。Socket 不存在是
// 唯一可转入离线路径的情况；离线路径先独占 External Lock 并恢复遗留 Restore，
// 避免同时运行的 Server 或未收敛的交换事务污染快照。
func runBackupCreate(
	ctx context.Context,
	program string,
	args, environ []string,
	stderr io.Writer,
	runtimeDir string,
) (resultErr error) {
	options, err := parseBackupCommandOptions(program, "create", args, environ, stderr)
	if err != nil {
		return err
	}
	return runBackupCreateWithOptions(ctx, options, stderr, runtimeDir)
}

func runBackupCreateWithOptions(
	ctx context.Context,
	options backupCommandOptions,
	stderr io.Writer,
	runtimeDir string,
) (resultErr error) {
	if err := requireBackupMaintenanceRoot(); err != nil {
		return err
	}
	config, logger, target, err := loadBackupCommand(configAndLogInput{options: options.config, stderr: stderr})
	if err != nil {
		return err
	}
	tlsMode, err := durableTLSMode(config.AgentGateway.TLS.Mode)
	if err != nil {
		return err
	}

	// create 固定一次 SQLite 源 inode，再把 SQLite Backup API 与 durableops 的
	// 文件快照装配在同一生命周期内。在线路径传入 beforePublish=lease.Close，
	// 因而 Release ACK 是归档对调用方可见为成功之前的最后一道承诺条件。
	create := func(createContext context.Context, beforePublish func() error, validateCanonical func() error) (result durableops.Manifest, resultErr error) {
		if err := validateCanonical(); err != nil {
			return durableops.Manifest{}, fmt.Errorf("validate canonical server data directory for backup: %w", err)
		}
		databasePath := filepath.Join(target.Path, "xtunnel.db")
		source, err := sqlite.OpenBackupSource(databasePath)
		if err != nil {
			return durableops.Manifest{}, fmt.Errorf("open fixed SQLite backup source: %w", err)
		}
		defer func() {
			if err := source.Close(); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		return durableops.Create(createContext, durableops.CreateOptions{
			DataDir: target.Path, TLSMode: tlsMode, OutputPath: options.path,
			BeforePublish: beforePublish,
			BackupDatabase: func(ctx context.Context, destination string) (int, error) {
				sourceVersion, err := source.InspectSchemaVersion(ctx)
				if err != nil {
					return 0, fmt.Errorf("inspect source database schema before backup: %w", err)
				}
				if sourceVersion < 1 {
					return 0, errors.New("source database has no applied schema version")
				}
				if err := source.BackupSQLite(ctx, destination); err != nil {
					return 0, err
				}
				if err := sqlite.ValidateBackupDatabase(ctx, destination, sourceVersion); err != nil {
					return 0, fmt.Errorf("validate captured database: %w", err)
				}
				return sourceVersion, nil
			},
		})
	}

	lease, handled, err := acquireOnlineBackupBarrier(ctx, runtimeDir, target.Hash)
	if err != nil {
		return err
	}
	if handled {
		// Socket Reader 会在 Server Shutdown、EOF 或异常提前响应时取消
		// leaseContext，使仍在进行的 SQLite/文件捕获尽快失败收敛。
		leaseContext, cancelLeaseContext := lease.BindContext(ctx)
		manifest, createErr := create(leaseContext, lease.Close, func() error {
			return datadir.ValidateCanonical(target)
		})
		cancelLeaseContext()
		releaseErr := lease.Close()
		if createErr != nil || releaseErr != nil {
			return errors.Join(createErr, releaseErr)
		}
		logBackupCompletion(ctx, logger, "backup_create_completed", target.Hash, manifest, "online")
		return nil
	}

	// handled=false 已证明固定目标的 Socket 路径不存在；External Lock 再确认
	// 没有 Server 持有相同 target，防止“离线”判断与 Server 启动发生竞态。
	lock, err := externallock.Acquire(runtimeDir, target.Hash)
	if err != nil {
		return fmt.Errorf("acquire Server external lock for offline backup: %w", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close offline backup external lock: %w", err))
		}
	}()
	parentGuard, err := datadir.PinParent(target)
	if err != nil {
		return fmt.Errorf("pin stable data parent for offline backup: %w", err)
	}
	defer func() {
		if err := parentGuard.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close offline backup data parent guard: %w", err))
		}
	}()
	if _, err := durableops.RecoverPendingRestore(ctx, target); err != nil {
		return fmt.Errorf("recover pending Restore before offline backup: %w", err)
	}
	manifest, err := create(ctx, nil, parentGuard.ValidateCanonical)
	if err != nil {
		return err
	}
	logBackupCompletion(ctx, logger, "backup_create_completed", target.Hash, manifest, "offline")
	return nil
}

// runBackupRestore 在 Server 离线且持有目标 External Lock 时执行恢复。
// Restore 会改换整个 data leaf，因此 V0.1 不提供在线恢复：先收敛上次崩溃遗留
// 的 Journal，再执行新的交换，最后重新验证 canonical target 才记录成功事件。
func runBackupRestore(
	ctx context.Context,
	program string,
	args, environ []string,
	stderr io.Writer,
	runtimeDir string,
) (resultErr error) {
	options, err := parseBackupCommandOptions(program, "restore", args, environ, stderr)
	if err != nil {
		return err
	}
	return runBackupRestoreWithOptions(ctx, options, stderr, runtimeDir)
}

func runBackupRestoreWithOptions(
	ctx context.Context,
	options backupCommandOptions,
	stderr io.Writer,
	runtimeDir string,
) (resultErr error) {
	if err := requireBackupMaintenanceRoot(); err != nil {
		return err
	}
	config, logger, target, err := loadBackupCommand(configAndLogInput{options: options.config, stderr: stderr})
	if err != nil {
		return err
	}
	tlsMode, err := durableTLSMode(config.AgentGateway.TLS.Mode)
	if err != nil {
		return err
	}
	lock, err := externallock.Acquire(runtimeDir, target.Hash)
	if err != nil {
		return fmt.Errorf("acquire Server external lock for restore: %w", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close Restore external lock: %w", err))
		}
	}()
	parentGuard, err := datadir.PinParent(target)
	if err != nil {
		return fmt.Errorf("pin stable data parent for Restore: %w", err)
	}
	defer func() {
		if err := parentGuard.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close Restore data parent guard: %w", err))
		}
	}()
	if _, err := durableops.RecoverPendingRestore(ctx, target); err != nil {
		return fmt.Errorf("recover previous pending Restore: %w", err)
	}
	manifest, err := durableops.Restore(ctx, target, options.path, sqlite.CurrentSchemaVersion(), tlsMode)
	if err != nil {
		return err
	}
	if err := parentGuard.ValidateCanonical(); err != nil {
		return fmt.Errorf("validate restored canonical server data directory: %w", err)
	}
	logBackupCompletion(ctx, logger, "backup_restore_completed", target.Hash, manifest, "offline")
	return nil
}

// configAndLogInput 把配置来源和结构化日志目标作为一组传入，避免维护命令
// 绕过 Server 的统一 Schema、环境覆盖及脱敏日志初始化路径。
type configAndLogInput struct {
	options baseconfig.Options
	stderr  io.Writer
}

// loadBackupCommand 使用与 Server 启动相同的配置权威解析稳定 data target，
// 同时只返回 target hash 等可安全写入维护日志的身份信息。
func loadBackupCommand(input configAndLogInput) (serverconfig.Config, *slog.Logger, datadir.Target, error) {
	config, err := serverconfig.Load(input.options)
	if err != nil {
		return serverconfig.Config{}, nil, datadir.Target{}, fmt.Errorf("load server config: %w", err)
	}
	logger, err := logging.New(input.stderr, logging.Options{
		Level: config.Logging.Level, Format: config.Logging.Format, Component: "server",
	})
	if err != nil {
		return serverconfig.Config{}, nil, datadir.Target{}, fmt.Errorf("initialize backup maintenance logging: %w", err)
	}
	target, err := datadir.Resolve(config.Server.DataDir)
	if err != nil {
		return serverconfig.Config{}, nil, datadir.Target{}, fmt.Errorf("resolve stable server data target: %w", err)
	}
	return config, logger, target, nil
}

// parseBackupCommandOptions 解析 create/restore 的共同参数边界。
// 归档不允许 stdout、相对路径或位置参数，防止二进制数据混入日志，也避免恢复时
// 因工作目录变化选择错误文件；配置仍严格复用 YAML、环境变量和 --set 优先级。
func parseBackupCommandOptions(
	program, operation string,
	args, environ []string,
	stderr io.Writer,
) (backupCommandOptions, error) {
	var options backupCommandOptions
	parsed := false
	command := newBackupOperationCommand(program, operation, environ, stderr, func(_ context.Context, parsedOptions backupCommandOptions) error {
		parsed = true
		options = parsedOptions
		return nil
	})
	command.Writer = stderr
	command.ErrWriter = stderr
	command.HideVersion = true
	command.ExitErrHandler = ignoreCLIExitError
	if err := command.Run(context.Background(), append([]string{program}, args...)); err != nil {
		return backupCommandOptions{}, fmt.Errorf("parse backup %s command: %w", operation, err)
	}
	if !parsed {
		return backupCommandOptions{}, errServerCLIHelp
	}
	return options, nil
}

// durableTLSMode 把配置 Schema 的 TLS 字符串收窄为 durableops 已冻结的枚举；
// 未知值必须快速失败，不能猜测应否捕获 pinned identity 文件。
func durableTLSMode(mode string) (durableops.TLSMode, error) {
	switch mode {
	case string(durableops.TLSModePinned):
		return durableops.TLSModePinned, nil
	case string(durableops.TLSModePublic):
		return durableops.TLSModePublic, nil
	default:
		return "", fmt.Errorf("unsupported gateway TLS mode %q", mode)
	}
}

// logBackupCompletion 只记录稳定 target hash、Manifest 摘要和非敏感元数据。
// 归档绝对路径、Token Key、证书内容和 Manifest 文件明细均不得进入运行日志。
func logBackupCompletion(
	ctx context.Context,
	logger *slog.Logger,
	event, targetHash string,
	manifest durableops.Manifest,
	mode string,
) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		// Manifest 已经由 durableops 成功编码；这里若发生内部不变量错误，
		// 不用可能含路径的回退字段掩盖它。
		logger.ErrorContext(ctx, "backup_operation_log_failed", "error_code", "MANIFEST_ENCODE_FAILED")
		return
	}
	digest := sha256.Sum256(encoded)
	logger.InfoContext(ctx, event,
		"target_hash", targetHash,
		"manifest_sha256", hex.EncodeToString(digest[:]),
		"schema_version", manifest.SchemaVersion,
		"mode", mode,
	)
}
