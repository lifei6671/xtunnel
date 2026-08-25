package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
)

const (
	adminBootstrapSocketName = "admin-bootstrap.sock"
	adminBootstrapNetwork    = "unix"

	adminBootstrapStatusCreated            = "created"
	adminBootstrapStatusAlreadyInitialized = "already_initialized"
	adminBootstrapStatusRejected           = "rejected"
)

type adminCreateOptions struct {
	username     string
	passwordFile string
	config       baseconfig.Options
}

func runAdminCreate(ctx context.Context, program string, args, environ []string, stderr io.Writer) error {
	return runAdminCreateWithRuntimeDir(ctx, program, args, environ, stderr, externallock.RuntimeDirectory)
}

func runAdminCreateWithRuntimeDir(ctx context.Context, program string, args, environ []string, stderr io.Writer, runtimeDir string) (resultErr error) {
	options, err := parseAdminCreateOptions(program, args, environ, stderr)
	if err != nil {
		return err
	}
	config, err := serverconfig.Load(options.config)
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
	flags := flag.NewFlagSet(program+" admin create", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var options adminCreateOptions
	overrides := make(configOverrides)
	var configPath string
	flags.StringVar(&options.username, "username", "", "first administrator username")
	flags.StringVar(&options.passwordFile, "password-file", "", "file containing the first administrator password")
	flags.StringVar(&configPath, "config", "", "YAML configuration file")
	flags.Var(overrides, "set", "override one Schema path with path=value; may be repeated")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s admin create --username name [--password-file path] [--config path] [--set path=value]...\n", program)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return adminCreateOptions{}, fmt.Errorf("parse admin create command line: %w", err)
	}
	if flags.NArg() != 0 {
		return adminCreateOptions{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(options.username) == "" {
		return adminCreateOptions{}, errors.New("admin username must not be empty")
	}

	var yamlData []byte
	if configPath != "" {
		var err error
		yamlData, err = os.ReadFile(configPath)
		if err != nil {
			return adminCreateOptions{}, fmt.Errorf("read config file %q: %w", configPath, err)
		}
	}
	options.config = baseconfig.Options{YAML: yamlData, Environment: environ, CLI: overrides}
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

func isAdminCommand(args []string) bool {
	return len(args) != 0 && args[0] == "admin"
}

func runAdminCommand(ctx context.Context, program string, args, environ []string, stderr io.Writer) error {
	if len(args) < 2 || args[1] != "create" {
		return errors.New("expected admin create")
	}
	return runAdminCreate(ctx, program, args[2:], environ, stderr)
}
