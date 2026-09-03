package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/durableops"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	"github.com/lifei6671/xtunnel/internal/server/pathprofile"
)

var errGatewayRotationAuditAfterCommit = errors.New("gateway identity was rotated but its security audit event was not persisted")

// runGatewayRotateKey 在 External Lock 保护的离线维护窗口中轮换 pinned 身份。
// 文件替换先由 durable Journal 提交，再把对应安全审计写入 SQLite；若审计落库失败，
// Journal 保留供下次启动或重试收敛，绝不能假装整次操作未发生。
func runGatewayRotateKey(
	ctx context.Context,
	program string,
	args, environ []string,
	stderr io.Writer,
	runtimeDir string,
	now time.Time,
) (resultErr error) {
	options, err := parseGatewayRotateKeyOptions(program, args, environ, stderr)
	if err != nil {
		return err
	}
	return runGatewayRotateKeyWithOptions(ctx, options, stderr, runtimeDir, now)
}

func runGatewayRotateKeyWithOptions(
	ctx context.Context,
	options baseconfig.Options,
	stderr io.Writer,
	runtimeDir string,
	now time.Time,
) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := serverconfig.Load(options)
	if err != nil {
		return fmt.Errorf("load server config: %w", err)
	}
	if config.AgentGateway.TLS.Mode != gateway.PinnedMode {
		return gateway.ErrPublicRotation
	}
	logger, err := logging.New(stderr, logging.Options{
		Level: config.Logging.Level, Format: config.Logging.Format, Component: "server",
	})
	if err != nil {
		return fmt.Errorf("initialize gateway rotation logging: %w", err)
	}
	if now.UTC().Unix() <= 0 {
		return errors.New("gateway rotation time must be after the Unix epoch")
	}
	dataDir, err := gatewayRotationDataDirectory(config.Server.DataDir)
	if err != nil {
		return err
	}
	target, err := datadir.Resolve(dataDir)
	if err != nil {
		return fmt.Errorf("resolve stable server data target: %w", err)
	}
	lock, err := externallock.Acquire(runtimeDir, target.Hash)
	if err != nil {
		return fmt.Errorf("acquire server external lock for gateway rotation: %w", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close gateway rotation external lock: %w", err))
		}
	}()
	parentGuard, err := datadir.PinParent(target)
	if err != nil {
		return fmt.Errorf("pin stable data parent for gateway rotation: %w", err)
	}
	defer func() {
		if err := parentGuard.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close gateway rotation data parent guard: %w", err))
		}
	}()
	if _, err := durableops.RecoverPendingRestore(ctx, target); err != nil {
		return fmt.Errorf("recover pending Restore Journal before gateway rotation: %w", err)
	}
	if err := parentGuard.ValidateCanonical(); err != nil {
		return fmt.Errorf("validate canonical server data directory before gateway rotation: %w", err)
	}
	if _, err := os.Stat(target.Path + "/xtunnel.db"); err != nil {
		return fmt.Errorf("inspect initialized Server database before gateway rotation: %w", err)
	}
	store, err := sqlite.Open(ctx, target.Path)
	if err != nil {
		return fmt.Errorf("open Server database for gateway rotation audit: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close gateway rotation database: %w", err))
		}
	}()
	reconciled, err := reconcileGatewayRotationAudit(ctx, target.Path, store, logger)
	if err != nil {
		return fmt.Errorf("reconcile previous gateway rotation audit: %w", err)
	}
	if reconciled {
		logger.InfoContext(ctx, "gateway_rotation_audit_reconciled",
			"action", repository.SecurityAuditActionGatewayKeyRotate,
			"rotation_performed", false,
		)
		return nil
	}
	eventID, err := identity.NewAuditEventID()
	if err != nil {
		return err
	}
	operationID, err := identity.NewOperationID()
	if err != nil {
		return err
	}
	audit := gateway.RotationAuditMetadata{
		EventID: eventID, OperationID: operationID,
		OccurredAt: now.UTC().Unix(), ResourceID: config.AgentGateway.PublicHostname,
	}
	_, err = gateway.RotatePinnedIdentity(target.Path, config.AgentGateway.PublicHostname, now, audit)
	if err != nil {
		return fmt.Errorf("rotate gateway pinned TLS identity: %w", err)
	}
	reconciled, err = reconcileGatewayRotationAudit(ctx, target.Path, store, logger)
	if err != nil {
		logger.ErrorContext(ctx, "security_audit_write_failed_after_gateway_rotation",
			"operation_id", operationID,
			"action", repository.SecurityAuditActionGatewayKeyRotate,
			"error_code", "AUDIT_WRITE_FAILED_AFTER_COMMIT",
		)
		return errors.Join(errGatewayRotationAuditAfterCommit, err)
	}
	if !reconciled {
		return errors.Join(errGatewayRotationAuditAfterCommit, errors.New("gateway rotation audit journal disappeared before reconciliation"))
	}
	return nil
}

// gatewayRotationDataDirectory makes the maintenance command use the same
// foreground profile as normal startup. In particular, Windows "auto" is a
// configuration sentinel and must be expanded before Stable Target validation.
func gatewayRotationDataDirectory(configuredDataDir string) (string, error) {
	profile, err := pathprofile.Resolve(configuredDataDir)
	if err != nil {
		return "", fmt.Errorf("resolve server foreground path profile for gateway rotation: %w", err)
	}
	return profile.DataDir, nil
}

func parseGatewayRotateKeyOptions(program string, args, environ []string, stderr io.Writer) (baseconfig.Options, error) {
	var options baseconfig.Options
	parsed := false
	command := newGatewayRotateKeyCommand(program, environ, stderr, func(_ context.Context, parsedOptions baseconfig.Options) error {
		parsed = true
		options = parsedOptions
		return nil
	})
	command.Writer = stderr
	command.ErrWriter = stderr
	command.HideVersion = true
	command.ExitErrHandler = ignoreCLIExitError
	if err := command.Run(context.Background(), append([]string{program}, args...)); err != nil {
		return baseconfig.Options{}, fmt.Errorf("parse gateway rotate-key command: %w", err)
	}
	if !parsed {
		return baseconfig.Options{}, errServerCLIHelp
	}
	return options, nil
}
