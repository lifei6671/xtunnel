// Package bootstrap 负责装配并运行 Server 进程。
package bootstrap

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/logging"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
)

// Execute 把操作系统输入和信号接入 Server 生命周期，并返回进程退出码。
func Execute(program string, args, environ []string, stderr io.Writer) int {
	return executeWithRun(program, args, environ, stderr, run)
}

func executeWithRun(
	program string,
	args, environ []string,
	stderr io.Writer,
	runner func(context.Context, string, []string, []string, io.Writer) error,
) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	if isAdminCommand(args) {
		err = runAdminCommand(ctx, program, args, environ, stderr)
	} else if isGatewayCommand(args) {
		err = runGatewayCommand(ctx, program, args[1:], environ, stderr)
	} else {
		err = runner(ctx, program, args, environ, stderr)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "%s: %v\n", program, err)
		return 1
	}
	return 0
}

// run 完成 Server 的配置、日志、Web 资源、External Lock 和 SQLite 初始化，并保持前台运行直到收到退出信号。
// 后续任务会在 SQLite 已就绪且 External Lock 仍被持有时继续接入 PKI 和 Listener。
func run(ctx context.Context, program string, args, environ []string, stderr io.Writer) error {
	return runWithStorageAndBootstrap(ctx, program, args, environ, stderr, func(ctx context.Context, dataDir string) (storage, error) {
		return openServerStorage(ctx, dataDir, externallock.RuntimeDirectory)
	}, func(ctx context.Context, config serverconfig.Config, resources storage) (io.Closer, error) {
		return openGatewayAndBootstrap(ctx, config, resources)
	})
}

func runWithStorage(ctx context.Context, program string, args, environ []string, stderr io.Writer, openStorage func(context.Context, string) (storage, error)) error {
	return runWithStorageAndBootstrap(ctx, program, args, environ, stderr, openStorage, nil)
}

func runWithStorageAndBootstrap(
	ctx context.Context,
	program string,
	args, environ []string,
	stderr io.Writer,
	openStorage func(context.Context, string) (storage, error),
	openBootstrap func(context.Context, serverconfig.Config, storage) (io.Closer, error),
) error {
	options, err := parseConfigOptions(program, args, environ, stderr)
	if err != nil {
		return err
	}
	config, err := serverconfig.Load(options)
	if err != nil {
		return fmt.Errorf("load server config: %w", err)
	}
	logger, err := logging.New(stderr, logging.Options{
		Level:     config.Logging.Level,
		Format:    config.Logging.Format,
		Component: "server",
	})
	if err != nil {
		return fmt.Errorf("initialize server logging: %w", err)
	}
	if err := validateEmbeddedWeb(); err != nil {
		return fmt.Errorf("initialize embedded web: %w", err)
	}
	resources, err := openStorage(ctx, config.Server.DataDir)
	if err != nil {
		return fmt.Errorf("initialize server storage: %w", err)
	}
	var bootstrapSocket io.Closer
	if openBootstrap != nil {
		bootstrapSocket, err = openBootstrap(ctx, config, resources)
		if err != nil {
			return errors.Join(fmt.Errorf("initialize admin bootstrap socket: %w", err), resources.Close())
		}
	}

	logger.InfoContext(ctx, "process_started")
	var runtimeErr error
	if source, ok := bootstrapSocket.(interface{ RuntimeErrors() <-chan error }); ok {
		select {
		case runtimeErr = <-source.RuntimeErrors():
		case <-ctx.Done():
			// 信号退出与运行时失败可能同时发生；已排队的具体错误优先返回。
			select {
			case runtimeErr = <-source.RuntimeErrors():
			default:
			}
		}
	} else {
		<-ctx.Done()
	}
	if bootstrapSocket != nil {
		if err := bootstrapSocket.Close(); err != nil {
			return errors.Join(runtimeErr, fmt.Errorf("close admin bootstrap socket: %w", err), resources.Close())
		}
	}
	closeErr := resources.Close()
	logger.Info("process_stopped")
	if runtimeErr != nil {
		if closeErr != nil {
			return errors.Join(runtimeErr, fmt.Errorf("close server storage: %w", closeErr))
		}
		return runtimeErr
	}
	if closeErr != nil {
		return fmt.Errorf("close server storage: %w", closeErr)
	}
	return nil
}

func parseConfigOptions(program string, args, environ []string, stderr io.Writer) (baseconfig.Options, error) {
	flags := flag.NewFlagSet(program, flag.ContinueOnError)
	flags.SetOutput(stderr)

	var configPath string
	overrides := make(configOverrides)
	flags.StringVar(&configPath, "config", "", "YAML configuration file")
	flags.Var(overrides, "set", "override one Schema path with path=value; may be repeated")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s [--config path] [--set path=value]...\n", program)
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return baseconfig.Options{}, fmt.Errorf("parse command line: %w", err)
	}
	if flags.NArg() != 0 {
		return baseconfig.Options{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	var yamlData []byte
	if configPath != "" {
		var err error
		yamlData, err = os.ReadFile(configPath)
		if err != nil {
			return baseconfig.Options{}, fmt.Errorf("read config file %q: %w", configPath, err)
		}
	}
	return baseconfig.Options{YAML: yamlData, Environment: environ, CLI: overrides}, nil
}

type configOverrides map[string]string

func (configOverrides) String() string {
	return ""
}

func (values configOverrides) Set(raw string) error {
	path, value, ok := strings.Cut(raw, "=")
	if !ok || path == "" {
		return errors.New("expected path=value")
	}
	values[path] = value
	return nil
}
