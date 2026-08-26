package bootstrap

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
)

// runGatewayCommand 只提供离线身份轮换；在线轮换会破坏旧 Pin，因此不属于 V0.1。
func runGatewayCommand(ctx context.Context, program string, args, environ []string, stderr io.Writer) error {
	if len(args) < 1 || args[0] != "rotate-key" {
		return errors.New("expected gateway rotate-key --maintenance")
	}
	return runGatewayRotateKey(ctx, program, args[1:], environ, stderr, externallock.RuntimeDirectory, time.Now())
}

func runGatewayRotateKey(
	ctx context.Context,
	program string,
	args, environ []string,
	stderr io.Writer,
	runtimeDir string,
	now time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	flags := flag.NewFlagSet(program+" gateway rotate-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var maintenance bool
	var configPath string
	overrides := make(configOverrides)
	flags.BoolVar(&maintenance, "maintenance", false, "require offline maintenance mode")
	flags.StringVar(&configPath, "config", "", "YAML configuration file")
	flags.Var(overrides, "set", "override one Schema path with path=value; may be repeated")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s gateway rotate-key --maintenance [--config path] [--set path=value]...\n", program)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse gateway rotate-key command: %w", err)
	}
	if !maintenance {
		return errors.New("gateway rotate-key requires --maintenance")
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("gateway rotate-key does not accept positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	var yamlData []byte
	if configPath != "" {
		var err error
		yamlData, err = os.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("read config file %q: %w", configPath, err)
		}
	}
	config, err := serverconfig.Load(baseconfig.Options{YAML: yamlData, Environment: environ, CLI: overrides})
	if err != nil {
		return fmt.Errorf("load server config: %w", err)
	}
	if config.AgentGateway.TLS.Mode != gateway.PinnedMode {
		return gateway.ErrPublicRotation
	}
	target, err := datadir.Resolve(config.Server.DataDir)
	if err != nil {
		return fmt.Errorf("resolve stable server data target: %w", err)
	}
	lock, err := externallock.Acquire(runtimeDir, target.Hash)
	if err != nil {
		return fmt.Errorf("acquire server external lock for gateway rotation: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := datadir.CheckPendingRestore(target); err != nil {
		return fmt.Errorf("check pending restore journal before gateway rotation: %w", err)
	}
	if err := datadir.ValidateCanonical(target); err != nil {
		return fmt.Errorf("validate canonical server data directory before gateway rotation: %w", err)
	}
	if _, err := os.Stat(target.Path + "/xtunnel.db"); err != nil {
		return fmt.Errorf("inspect initialized Server database before gateway rotation: %w", err)
	}
	if _, err := gateway.RotatePinnedIdentity(target.Path, config.AgentGateway.PublicHostname, now); err != nil {
		return fmt.Errorf("rotate gateway pinned TLS identity: %w", err)
	}
	return nil
}

func isGatewayCommand(args []string) bool {
	return len(args) != 0 && args[0] == "gateway"
}
