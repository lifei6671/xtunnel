// Package bootstrap 负责装配并运行 Agent 进程。
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/lifei6671/xtunnel/internal/agent/connector"
	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
	"github.com/lifei6671/xtunnel/internal/agent/service"
	"github.com/lifei6671/xtunnel/internal/buildinfo"
	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/tracing"
)

const (
	maxTokenBytes         = 8192
	tokenEnvironment      = "XTUNNEL_TOKEN"
	credentialsDirectory  = "CREDENTIALS_DIRECTORY"
	systemdCredentialName = "xtunnel-agent.token"
)

type lifecycleRunner func(context.Context, string, io.Writer) error
type diagnosticRunner func(context.Context, string) agentgateway.DiagnosticResult

type serviceOperations interface {
	Install(context.Context, string) error
	Uninstall(context.Context) (service.UninstallResult, error)
}

type platformServiceOperations struct{}

var (
	errCLIHelp            = errors.New("CLI help requested")
	errDiagnosticNotReady = errors.New("connectivity diagnostic reported NOT_READY")
)

func (platformServiceOperations) Install(ctx context.Context, token string) error {
	return service.Install(ctx, token)
}

func (platformServiceOperations) Uninstall(ctx context.Context) (service.UninstallResult, error) {
	return service.Uninstall(ctx)
}

// Execute 把操作系统输入和信号接入 Agent 生命周期，并返回进程退出码。
func Execute(program string, args, environ []string, stdout, stderr io.Writer) int {
	return executeProgram(program, args, environ, stdout, stderr, agentgateway.Diagnose)
}

func executeProgram(
	program string,
	args, environ []string,
	stdout, stderr io.Writer,
	diagnose diagnosticRunner,
) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := executeWithDiagnostic(ctx, program, args, environ, stdout, stderr, platformServiceOperations{}, diagnose); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", program, err)
		return 1
	}
	return 0
}

func execute(
	ctx context.Context,
	program string,
	args, environ []string,
	stdout, stderr io.Writer,
	services serviceOperations,
) error {
	return executeWithDiagnostic(ctx, program, args, environ, stdout, stderr, services, agentgateway.Diagnose)
}

func executeWithDiagnostic(
	ctx context.Context,
	program string,
	args, environ []string,
	stdout, stderr io.Writer,
	services serviceOperations,
	diagnose diagnosticRunner,
) error {
	command := newAgentCommand(program, environ, stdout, stderr, services, diagnose)
	err := command.Run(ctx, append([]string{program}, args...))
	if errors.Is(err, errCLIHelp) {
		return nil
	}
	return err
}

func agentHelpBefore(ctx context.Context, command *cli.Command) (context.Context, error) {
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
		return ctx, fmt.Errorf("show Agent command help: %w", err)
	}
	return ctx, errCLIHelp
}

// newAgentCommand 由一棵 urfave/cli 命令树统一声明 Agent 的公开命令面。
// Token 的来源选择和校验仍由 bootstrap 持有，避免 CLI 框架读取环境变量后改变安全优先级。
func newAgentCommand(
	program string,
	environ []string,
	stdout, stderr io.Writer,
	services serviceOperations,
	diagnose diagnosticRunner,
) *cli.Command {
	var runToken string
	var diagnoseToken string
	var installToken string
	stopOnFirstArgument := 1

	command := &cli.Command{
		Name:         program,
		Usage:        "run and manage XTunnel Agent",
		UsageText:    program + " <command> [options]",
		Writer:       stdout,
		ErrWriter:    stderr,
		HideVersion:  true,
		StopOnNthArg: &stopOnFirstArgument,
		Before:       agentHelpBefore,
		OnUsageError: func(_ context.Context, _ *cli.Command, err error, _ bool) error {
			return err
		},
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Action: func(_ context.Context, current *cli.Command) error {
			if current.NArg() != 0 {
				return errors.New("unknown command")
			}
			writer := current.Writer
			current.Writer = stderr
			defer func() { current.Writer = writer }()
			if err := cli.ShowRootCommandHelp(current); err != nil {
				return fmt.Errorf("show Agent command help: %w", err)
			}
			return errors.New("command is required")
		},
	}
	command.Commands = []*cli.Command{
		{
			Name:            "run",
			Usage:           "run the Agent in the foreground",
			UsageText:       program + " run [--token string]",
			HideHelpCommand: true,
			StopOnNthArg:    &stopOnFirstArgument,
			Before:          agentHelpBefore,
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "token", Usage: "Agent Token", Destination: &runToken, HideDefault: true},
			},
			OnUsageError: command.OnUsageError,
			Action: func(ctx context.Context, current *cli.Command) error {
				if current.NArg() != 0 {
					return errors.New("unexpected positional arguments")
				}
				return runWithTokenSource(
					ctx,
					stderr,
					runLifecycle,
					func() (string, error) {
						return resolveTokenSource(runToken, current.IsSet("token"), environ)
					},
					func() (string, bool, error) {
						return resolveTokenOverrideSource(runToken, current.IsSet("token"), environ)
					},
				)
			},
		},
		{
			Name:            "diagnose",
			Usage:           "run a non-authenticating Gateway connectivity precheck",
			UsageText:       program + " diagnose [--token string]",
			HideHelpCommand: true,
			StopOnNthArg:    &stopOnFirstArgument,
			Before:          agentHelpBefore,
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "token", Usage: "Agent Token", Destination: &diagnoseToken, HideDefault: true},
			},
			OnUsageError: command.OnUsageError,
			Action: func(ctx context.Context, current *cli.Command) error {
				if current.NArg() != 0 {
					return errors.New("diagnose does not accept positional arguments")
				}
				if diagnose == nil {
					return errors.New("Agent diagnostic runner must not be nil")
				}
				token, err := resolveTokenSource(diagnoseToken, current.IsSet("token"), environ)
				if err != nil {
					return fmt.Errorf("load agent token: %w", err)
				}
				result := diagnose(ctx, token)
				if err := writeDiagnosticResult(stdout, result); err != nil {
					return err
				}
				switch result.Summary {
				case agentgateway.DiagnosticReady, agentgateway.DiagnosticReadyDegraded:
					return nil
				case agentgateway.DiagnosticNotReady:
					return errDiagnosticNotReady
				default:
					return errors.New("connectivity diagnostic returned an invalid summary")
				}
			},
		},
		{
			Name:         "service",
			Usage:        "install or uninstall the native Agent service",
			UsageText:    program + " service <command> [options]",
			StopOnNthArg: &stopOnFirstArgument,
			Before:       agentHelpBefore,
			OnUsageError: command.OnUsageError,
			Action: func(_ context.Context, current *cli.Command) error {
				if current.NArg() == 0 {
					writer := current.Writer
					current.Writer = stderr
					defer func() { current.Writer = writer }()
					if err := cli.ShowSubcommandHelp(current); err != nil {
						return fmt.Errorf("show Agent service help: %w", err)
					}
					return errors.New("service command is required")
				}
				return errors.New("unknown service command")
			},
			Commands: []*cli.Command{
				{
					Name:            "install",
					Usage:           "install and start the native Agent service",
					UsageText:       program + " service install --token string",
					HideHelpCommand: true,
					StopOnNthArg:    &stopOnFirstArgument,
					Before:          agentHelpBefore,
					OnUsageError:    command.OnUsageError,
					Flags: []cli.Flag{
						&cli.StringFlag{Name: "token", Usage: "Agent Token", Destination: &installToken, HideDefault: true},
					},
					Action: func(ctx context.Context, current *cli.Command) error {
						if current.NArg() != 0 {
							return errors.New("service install does not accept positional arguments")
						}
						if !current.IsSet("token") {
							return errors.New("service install requires --token")
						}
						validatedToken, err := validateToken(installToken)
						if err != nil {
							return fmt.Errorf("validate service install Token: %w", err)
						}
						if err := services.Install(ctx, validatedToken); err != nil {
							return fmt.Errorf("install Agent service: %w", err)
						}
						fmt.Fprintf(stdout, "installed and started %s\n", service.Name())
						return nil
					},
				},
				{
					Name:            "uninstall",
					Usage:           "uninstall the native Agent service",
					UsageText:       program + " service uninstall",
					HideHelpCommand: true,
					StopOnNthArg:    &stopOnFirstArgument,
					Before:          agentHelpBefore,
					OnUsageError:    command.OnUsageError,
					Action: func(ctx context.Context, current *cli.Command) error {
						if current.NArg() != 0 {
							return errors.New("service uninstall does not accept arguments")
						}
						result, err := services.Uninstall(ctx)
						if err != nil {
							return fmt.Errorf("uninstall Agent service: %w", err)
						}
						if result.BinaryRemovalPendingReboot {
							fmt.Fprintf(stdout, "uninstalled %s; service registration was removed, the running binary will be deleted after the next reboot, and the credential was preserved\n", service.Name())
							return nil
						}
						fmt.Fprintf(stdout, "uninstalled %s; credential and service identity were preserved\n", service.Name())
						return nil
					},
				},
			},
		},
	}
	return command
}

func writeDiagnosticResult(writer io.Writer, result agentgateway.DiagnosticResult) error {
	for _, step := range result.Steps {
		if _, err := fmt.Fprintf(writer, "%s %s %s\n", step.Status, step.Stage, step.Message); err != nil {
			return fmt.Errorf("write connectivity diagnostic step: %w", err)
		}
	}
	if _, err := fmt.Fprintln(writer, result.Summary); err != nil {
		return fmt.Errorf("write connectivity diagnostic summary: %w", err)
	}
	return nil
}

// run 解析唯一的 Tunnel Token 来源，并把同一个 Token 交给进程内唯一 Connector。
func run(ctx context.Context, program string, args, environ []string, stderr io.Writer) error {
	return runWithLifecycle(ctx, program, args, environ, stderr, runLifecycle)
}

func runWithLifecycle(
	ctx context.Context,
	program string,
	args, environ []string,
	stderr io.Writer,
	lifecycle lifecycleRunner,
) error {
	return runWithTokenSource(ctx, stderr, lifecycle, func() (string, error) {
		return resolveToken(program, args, environ, stderr)
	}, func() (string, bool, error) {
		return resolveTokenOverride(program, args, environ, stderr)
	})
}

func runWithTokenSource(
	ctx context.Context,
	stderr io.Writer,
	lifecycle lifecycleRunner,
	resolve func() (string, error),
	resolveServiceOverride func() (string, bool, error),
) error {
	if lifecycle == nil {
		return errors.New("Agent lifecycle runner must not be nil")
	}
	handled, err := service.RunIfManagedService(resolveServiceOverride, func(serviceContext context.Context, token string, writer io.Writer) error {
		validatedToken, err := validateToken(token)
		if err != nil {
			return fmt.Errorf("validate Windows service credential: %w", err)
		}
		return lifecycle(serviceContext, validatedToken, writer)
	})
	if err != nil {
		return fmt.Errorf("run Agent as native service: %w", err)
	}
	if handled {
		return nil
	}
	token, err := resolve()
	if err != nil {
		return fmt.Errorf("load agent token: %w", err)
	}
	return lifecycle(ctx, token, stderr)
}

func runLifecycle(ctx context.Context, token string, stderr io.Writer) (resultErr error) {
	logger, err := logging.New(stderr, logging.Options{
		Level:     "info",
		Format:    "json",
		Component: "agent",
	})
	if err != nil {
		return fmt.Errorf("initialize agent logging: %w", err)
	}
	traceRuntime, err := tracing.New(ctx, tracing.Config{
		ServiceName:    "xtunnel-agent",
		ServiceVersion: buildinfo.Version(),
		ReportExportFailure: func() {
			logger.Warn("tracing_export_failed", logging.ErrorCodeKey, "EXPORT_FAILED")
		},
	})
	if err != nil {
		return fmt.Errorf("initialize agent tracing: %w", err)
	}
	// Connector.Run 已经完成 Session、WorkConn、Origin 与健康 owner 的 Drain/Wait；
	// 最后以不继承进程取消的有界 Context Flush，避免 Collector 影响退出上限。
	defer func() {
		resultErr = errors.Join(resultErr, traceRuntime.Shutdown(context.WithoutCancel(ctx)))
	}()

	logger.InfoContext(ctx, "process_started")
	defer logger.Info("process_stopped")

	config, err := connector.HostConfig(token, buildinfo.Version())
	if err != nil {
		return fmt.Errorf("create ephemeral Connector identity: %w", err)
	}
	config.Logger = logger
	config.Tracing = traceRuntime
	runtime, err := connector.New(config)
	if err != nil {
		return fmt.Errorf("initialize Connector runtime: %w", err)
	}
	if err := runtime.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("run Connector lifecycle: %w", err)
	}
	return nil
}

func resolveToken(program string, args, environ []string, stderr io.Writer) (string, error) {
	cliToken, cliTokenSet, err := parseTokenFlag(program, args, stderr)
	if err != nil {
		return "", err
	}
	return resolveTokenSource(cliToken, cliTokenSet, environ)
}

func resolveTokenOverride(
	program string,
	args, environ []string,
	stderr io.Writer,
) (string, bool, error) {
	cliToken, cliTokenSet, err := parseTokenFlag(program, args, stderr)
	if err != nil {
		return "", false, err
	}
	return resolveTokenOverrideSource(cliToken, cliTokenSet, environ)
}

func parseTokenFlag(program string, args []string, stderr io.Writer) (string, bool, error) {
	var cliToken string
	var cliTokenSet bool
	parsed := false
	stopOnFirstArgument := 1
	command := &cli.Command{
		Name:         program,
		UsageText:    program + " [--token string]",
		Writer:       stderr,
		ErrWriter:    stderr,
		HideVersion:  true,
		StopOnNthArg: &stopOnFirstArgument,
		Before:       agentHelpBefore,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "token", Usage: "Agent Token", Destination: &cliToken, HideDefault: true},
		},
		OnUsageError:   func(_ context.Context, _ *cli.Command, err error, _ bool) error { return err },
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
		Action: func(_ context.Context, current *cli.Command) error {
			if current.NArg() != 0 {
				return errors.New("unexpected positional arguments")
			}
			parsed = true
			cliTokenSet = current.IsSet("token")
			return nil
		},
	}
	if err := command.Run(context.Background(), append([]string{program}, args...)); err != nil {
		return "", false, fmt.Errorf("parse command line: %w", err)
	}
	if !parsed {
		return "", false, errCLIHelp
	}
	return cliToken, cliTokenSet, nil
}

func resolveTokenSource(cliToken string, cliTokenSet bool, environ []string) (string, error) {
	token, found, err := resolveTokenOverrideSource(cliToken, cliTokenSet, environ)
	if err != nil || found {
		return token, err
	}

	directory, ok := lookupEnvironment(environ, credentialsDirectory)
	if !ok {
		return "", errors.New("token is required: use --token, XTUNNEL_TOKEN, or the systemd credential")
	}
	if directory == "" {
		return "", errors.New("CREDENTIALS_DIRECTORY must not be empty")
	}
	credential, err := os.ReadFile(filepath.Join(directory, systemdCredentialName))
	if err != nil {
		return "", fmt.Errorf("read systemd credential %q: %w", systemdCredentialName, err)
	}
	return validateToken(string(credential))
}

func resolveTokenOverrideSource(cliToken string, cliTokenSet bool, environ []string) (string, bool, error) {
	if cliTokenSet {
		token, err := validateToken(cliToken)
		return token, true, err
	}
	if environmentToken, ok := lookupEnvironment(environ, tokenEnvironment); ok {
		token, err := validateToken(environmentToken)
		return token, true, err
	}
	return "", false, nil
}

func lookupEnvironment(environ []string, name string) (string, bool) {
	prefix := name + "="
	for index := len(environ) - 1; index >= 0; index-- {
		if strings.HasPrefix(environ[index], prefix) {
			return strings.TrimPrefix(environ[index], prefix), true
		}
	}
	return "", false
}

func validateToken(token string) (string, error) {
	if token == "" {
		return "", errors.New("token must not be empty")
	}
	if len(token) > maxTokenBytes {
		return "", fmt.Errorf("token must not exceed %d bytes", maxTokenBytes)
	}
	if strings.TrimSpace(token) != token {
		return "", errors.New("token must not contain leading or trailing whitespace")
	}
	if !strings.HasPrefix(token, "xta_") {
		return "", errors.New("token must start with xta_")
	}
	return token, nil
}
