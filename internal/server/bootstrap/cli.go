package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	"github.com/lifei6671/xtunnel/internal/server/externallock"
)

var errServerCLIHelp = errors.New("Server CLI help requested")

type configFlagValues struct {
	path      string
	overrides []string
}

func (values *configFlagValues) flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:        "config",
			Usage:       "YAML configuration file",
			Destination: &values.path,
			Local:       true,
		},
		&cli.StringSliceFlag{
			Name:        "set",
			Usage:       "override one Schema path with path=value; may be repeated",
			Destination: &values.overrides,
			HideDefault: true,
			Local:       true,
		},
	}
}

// options 只把 CLI 输入转换为配置加载器接受的来源集合。Schema 字段解释、默认值和
// 优先级仍由 config.Load 持有；--set 在此逐项解析以保留“最后一次赋值生效”语义。
func (values *configFlagValues) options(environ []string) (baseconfig.Options, error) {
	overrides := make(configOverrides)
	for _, raw := range values.overrides {
		if err := overrides.Set(raw); err != nil {
			return baseconfig.Options{}, err
		}
	}

	var yamlData []byte
	if values.path != "" {
		var err error
		yamlData, err = os.ReadFile(values.path)
		if err != nil {
			return baseconfig.Options{}, fmt.Errorf("read config file %q: %w", values.path, err)
		}
	}
	return baseconfig.Options{YAML: yamlData, Environment: environ, CLI: overrides}, nil
}

func passthroughUsageError(_ context.Context, _ *cli.Command, err error, _ bool) error {
	return err
}

func ignoreCLIExitError(context.Context, *cli.Command, error) {}

func serverHelpBefore(ctx context.Context, command *cli.Command) (context.Context, error) {
	if !command.IsSet("help") || command.Bool("help") {
		return ctx, nil
	}
	var err error
	if command.Root() == command {
		err = cli.ShowRootCommandHelp(command)
	} else {
		err = cli.ShowSubcommandHelp(command)
	}
	if err != nil {
		return ctx, fmt.Errorf("show Server command help: %w", err)
	}
	return ctx, errServerCLIHelp
}

func stopOnFirstArgument() *int {
	value := 1
	return &value
}

// newServerCommand 使用单一命令树声明常驻 Server 与所有维护子命令。每个 Action
// 只接收已经解析的类型化参数，再进入原有的配置、安全和资源生命周期 owner。
func newServerCommand(
	program string,
	args []string,
	environ []string,
	stderr io.Writer,
	runner func(context.Context, baseconfig.Options, io.Writer) error,
) *cli.Command {
	rootConfig := &configFlagValues{}
	command := &cli.Command{
		Name:                      program,
		Usage:                     "run and maintain XTunnel Server",
		UsageText:                 program + " [--config path] [--set path=value]...",
		Flags:                     rootConfig.flags(),
		Writer:                    stderr,
		ErrWriter:                 stderr,
		HideVersion:               true,
		HideHelpCommand:           true,
		DisableSliceFlagSeparator: true,
		StopOnNthArg:              stopOnFirstArgument(),
		Before:                    serverHelpBefore,
		OnUsageError:              passthroughUsageError,
		ExitErrHandler:            ignoreCLIExitError,
		Action: func(ctx context.Context, current *cli.Command) error {
			if current.NArg() != 0 {
				return fmt.Errorf("unexpected positional arguments: %s", strings.Join(current.Args().Slice(), " "))
			}
			options, err := rootConfig.options(environ)
			if err != nil {
				return err
			}
			return runner(ctx, options, stderr)
		},
	}
	admin := &cli.Command{
		Name:         "admin",
		Usage:        "manage the initial administrator",
		UsageText:    program + " admin <command> [options]",
		OnUsageError: passthroughUsageError,
		Before:       serverHelpBefore,
		Action: func(_ context.Context, _ *cli.Command) error {
			return errors.New("expected admin create")
		},
	}
	admin.Commands = []*cli.Command{newAdminCreateCommand(program, environ, stderr, func(ctx context.Context, options adminCreateOptions) error {
		return runAdminCreateWithOptions(ctx, options, stderr, externallock.RuntimeDirectory)
	})}

	gateway := &cli.Command{
		Name:         "gateway",
		Usage:        "maintain the Agent Gateway identity",
		UsageText:    program + " gateway <command> [options]",
		OnUsageError: passthroughUsageError,
		Before:       serverHelpBefore,
		Action: func(_ context.Context, _ *cli.Command) error {
			return errors.New("expected gateway rotate-key --maintenance")
		},
	}
	gateway.Commands = []*cli.Command{newGatewayRotateKeyCommand(program, environ, stderr, func(ctx context.Context, options baseconfig.Options) error {
		return runGatewayRotateKeyWithOptions(ctx, options, stderr, externallock.RuntimeDirectory, time.Now())
	})}

	backup := &cli.Command{
		Name:         "backup",
		Usage:        "create or restore a durable Server backup",
		UsageText:    program + " backup <command> [options]",
		OnUsageError: passthroughUsageError,
		Before:       serverHelpBefore,
		Action: func(_ context.Context, current *cli.Command) error {
			if current.NArg() == 0 {
				return errors.New("expected backup create or backup restore")
			}
			return fmt.Errorf("unknown backup command %q", current.Args().First())
		},
	}
	backup.Commands = []*cli.Command{
		newBackupOperationCommand(program, "create", environ, stderr, func(ctx context.Context, options backupCommandOptions) error {
			return runBackupCreateWithOptions(ctx, options, stderr, externallock.RuntimeDirectory)
		}),
		newBackupOperationCommand(program, "restore", environ, stderr, func(ctx context.Context, options backupCommandOptions) error {
			return runBackupRestoreWithOptions(ctx, options, stderr, externallock.RuntimeDirectory)
		}),
	}

	if len(args) == 0 || args[0] == "admin" || args[0] == "gateway" || args[0] == "backup" || args[0] == "-h" || args[0] == "--help" {
		command.Commands = []*cli.Command{admin, gateway, backup}
	}
	return command
}

func parseServerConfigOptions(program string, args, environ []string, stderr io.Writer) (baseconfig.Options, error) {
	values := &configFlagValues{}
	var options baseconfig.Options
	parsed := false
	command := &cli.Command{
		Name:                      program,
		UsageText:                 program + " [--config path] [--set path=value]...",
		Flags:                     values.flags(),
		Writer:                    stderr,
		ErrWriter:                 stderr,
		HideVersion:               true,
		DisableSliceFlagSeparator: true,
		StopOnNthArg:              stopOnFirstArgument(),
		Before:                    serverHelpBefore,
		OnUsageError:              passthroughUsageError,
		ExitErrHandler:            ignoreCLIExitError,
		Action: func(_ context.Context, current *cli.Command) error {
			if current.NArg() != 0 {
				return fmt.Errorf("unexpected positional arguments: %s", strings.Join(current.Args().Slice(), " "))
			}
			parsed = true
			var err error
			options, err = values.options(environ)
			return err
		},
	}
	if err := command.Run(context.Background(), append([]string{program}, args...)); err != nil {
		return baseconfig.Options{}, fmt.Errorf("parse command line: %w", err)
	}
	if !parsed {
		return baseconfig.Options{}, errServerCLIHelp
	}
	return options, nil
}

func newAdminCreateCommand(
	program string,
	environ []string,
	stderr io.Writer,
	action func(context.Context, adminCreateOptions) error,
) *cli.Command {
	configValues := &configFlagValues{}
	var username string
	var passwordFile string
	return &cli.Command{
		Name:                      "create",
		Usage:                     "create the first administrator",
		UsageText:                 program + " admin create --username name [--password-file path] [--config path] [--set path=value]...",
		DisableSliceFlagSeparator: true,
		StopOnNthArg:              stopOnFirstArgument(),
		Before:                    serverHelpBefore,
		OnUsageError:              passthroughUsageError,
		Flags: append([]cli.Flag{
			&cli.StringFlag{Name: "username", Usage: "first administrator username", Destination: &username},
			&cli.StringFlag{Name: "password-file", Usage: "file containing the first administrator password", Destination: &passwordFile},
		}, configValues.flags()...),
		Action: func(ctx context.Context, current *cli.Command) error {
			if current.NArg() != 0 {
				return fmt.Errorf("unexpected positional arguments: %s", strings.Join(current.Args().Slice(), " "))
			}
			if strings.TrimSpace(username) == "" {
				return errors.New("admin username must not be empty")
			}
			config, err := configValues.options(environ)
			if err != nil {
				return err
			}
			return action(ctx, adminCreateOptions{username: username, passwordFile: passwordFile, config: config})
		},
	}
}

func newGatewayRotateKeyCommand(
	program string,
	environ []string,
	stderr io.Writer,
	action func(context.Context, baseconfig.Options) error,
) *cli.Command {
	configValues := &configFlagValues{}
	var maintenance bool
	return &cli.Command{
		Name:                      "rotate-key",
		Usage:                     "rotate the pinned Gateway identity while offline",
		UsageText:                 program + " gateway rotate-key --maintenance [--config path] [--set path=value]...",
		DisableSliceFlagSeparator: true,
		StopOnNthArg:              stopOnFirstArgument(),
		Before:                    serverHelpBefore,
		OnUsageError:              passthroughUsageError,
		Flags: append([]cli.Flag{
			&cli.BoolFlag{Name: "maintenance", Usage: "require offline maintenance mode", Destination: &maintenance},
		}, configValues.flags()...),
		Action: func(ctx context.Context, current *cli.Command) error {
			if !maintenance {
				return errors.New("gateway rotate-key requires --maintenance")
			}
			if current.NArg() != 0 {
				return fmt.Errorf("gateway rotate-key does not accept positional arguments: %s", strings.Join(current.Args().Slice(), " "))
			}
			options, err := configValues.options(environ)
			if err != nil {
				return err
			}
			return action(ctx, options)
		},
	}
}

func newBackupOperationCommand(
	program, operation string,
	environ []string,
	stderr io.Writer,
	action func(context.Context, backupCommandOptions) error,
) *cli.Command {
	configValues := &configFlagValues{}
	pathFlag := "output"
	if operation == "restore" {
		pathFlag = "input"
	}
	var archivePath string
	return &cli.Command{
		Name:                      operation,
		Usage:                     operation + " a durable Server backup",
		UsageText:                 fmt.Sprintf("%s backup %s --%s path [--config path] [--set path=value]...", program, operation, pathFlag),
		DisableSliceFlagSeparator: true,
		StopOnNthArg:              stopOnFirstArgument(),
		Before:                    serverHelpBefore,
		OnUsageError:              passthroughUsageError,
		Flags: append([]cli.Flag{
			&cli.StringFlag{Name: pathFlag, Usage: "absolute backup archive path", Destination: &archivePath},
		}, configValues.flags()...),
		Action: func(ctx context.Context, current *cli.Command) error {
			if current.NArg() != 0 {
				return fmt.Errorf("backup %s does not accept positional arguments: %s", operation, strings.Join(current.Args().Slice(), " "))
			}
			if archivePath == "" || archivePath == "-" || !filepath.IsAbs(archivePath) {
				return fmt.Errorf("backup %s --%s must be an absolute non-stdout path", operation, pathFlag)
			}
			config, err := configValues.options(environ)
			if err != nil {
				return err
			}
			return action(ctx, backupCommandOptions{path: archivePath, config: config})
		},
	}
}
