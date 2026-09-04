package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
)

const (
	adminBootstrapSocketName = "admin-bootstrap.sock"
	adminBootstrapNetwork    = "unix"

	adminBootstrapStatusCreated            = "created"
	adminBootstrapStatusAlreadyInitialized = "already_initialized"
	adminBootstrapStatusRejected           = "rejected"
)

type adminCreateOptions struct {
	username       string
	passwordFile   string
	config         baseconfig.Options
	serviceProfile bool
}

func runAdminCreateWithRuntimeDir(ctx context.Context, program string, args, environ []string, stderr io.Writer, runtimeDir string) (resultErr error) {
	options, err := parseAdminCreateOptions(program, args, environ, stderr)
	if err != nil {
		return err
	}
	return runAdminCreateWithOptions(ctx, options, stderr, runtimeDir)
}

func runAdminCreateWithOptions(ctx context.Context, options adminCreateOptions, stderr io.Writer, runtimeDir string) (resultErr error) {
	config, err := loadProfileConfig(options.config, options.serviceProfile)
	if err != nil {
		return fmt.Errorf("load server config: %w", err)
	}
	target, err := datadir.Resolve(config.Server.DataDir)
	if err != nil {
		return fmt.Errorf("resolve stable server data target: %w", err)
	}
	password, err := readAdminPassword(options.passwordFile, stderr)
	if err != nil {
		return err
	}

	socketPath := filepath.Join(runtimeDir, adminBootstrapSocketName)
	handled, err := requestAdminBootstrap(ctx, socketPath, target.Hash, options.username, password)
	if handled {
		return err
	}
	if err != nil {
		return err
	}

	storage, err := openServerStorage(ctx, config.Server.DataDir, runtimeDir)
	if err != nil {
		return fmt.Errorf("initialize offline admin storage: %w", err)
	}
	defer func() {
		closeErr := storage.Close()
		if resultErr == nil && closeErr != nil {
			resultErr = fmt.Errorf("close offline admin storage: %w", closeErr)
		}
	}()
	if err := storage.database.CreateFirstAdmin(ctx, options.username, password); err != nil {
		return err
	}
	return nil
}

func parseAdminCreateOptions(program string, args, environ []string, stderr io.Writer) (adminCreateOptions, error) {
	var options adminCreateOptions
	parsed := false
	command := newAdminCreateCommand(program, environ, stderr, func(_ context.Context, parsedOptions adminCreateOptions) error {
		parsed = true
		options = parsedOptions
		return nil
	})
	command.Writer = stderr
	command.ErrWriter = stderr
	command.HideVersion = true
	command.ExitErrHandler = ignoreCLIExitError
	if err := command.Run(context.Background(), append([]string{program}, args...)); err != nil {
		return adminCreateOptions{}, fmt.Errorf("parse admin create command line: %w", err)
	}
	if !parsed {
		return adminCreateOptions{}, errServerCLIHelp
	}
	return options, nil
}

func readAdminPassword(passwordFile string, stderr io.Writer) (string, error) {
	if passwordFile == "" {
		return readAdminPasswordFromTTY(os.Stdin, stderr)
	}
	data, err := os.ReadFile(passwordFile)
	if err != nil {
		return "", fmt.Errorf("read admin password file: %w", err)
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	data = bytes.TrimSuffix(data, []byte("\r"))
	if len(data) == 0 {
		return "", errors.New("admin password must not be empty")
	}
	return string(data), nil
}
