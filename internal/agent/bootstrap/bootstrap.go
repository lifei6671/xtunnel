// Package bootstrap 负责装配并运行 Agent 进程。
package bootstrap

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/lifei6671/xtunnel/internal/agent/service"
	"github.com/lifei6671/xtunnel/internal/logging"
)

const (
	maxTokenBytes         = 8192
	tokenEnvironment      = "XTUNNEL_TOKEN"
	credentialsDirectory  = "CREDENTIALS_DIRECTORY"
	systemdCredentialName = "xtunnel-agent.token"
)

type serviceOperations interface {
	Install(context.Context, string) error
	Uninstall(context.Context) (service.UninstallResult, error)
}

type platformServiceOperations struct{}

func (platformServiceOperations) Install(ctx context.Context, token string) error {
	return service.Install(ctx, token)
}

func (platformServiceOperations) Uninstall(ctx context.Context) (service.UninstallResult, error) {
	return service.Uninstall(ctx)
}

// Execute 把操作系统输入和信号接入 Agent 生命周期，并返回进程退出码。
func Execute(program string, args, environ []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := execute(ctx, program, args, environ, stdout, stderr, platformServiceOperations{}); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
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
	if len(args) == 0 {
		printRootUsage(stderr, program)
		return errors.New("command is required")
	}
	switch args[0] {
	case "-h", "--help", "help":
		printRootUsage(stdout, program)
		return nil
	case "run":
		return run(ctx, program+" run", args[1:], environ, stderr)
	case "service":
		return executeService(ctx, program, args[1:], stdout, stderr, services)
	default:
		return errors.New("unknown command")
	}
}

func printRootUsage(writer io.Writer, program string) {
	fmt.Fprintf(writer, "Usage:\n  %s run [--token STRING]\n  %s service install --token STRING\n  %s service uninstall\n", program, program, program)
}

func executeService(
	ctx context.Context,
	program string,
	args []string,
	stdout, stderr io.Writer,
	services serviceOperations,
) error {
	if len(args) == 0 {
		printServiceUsage(stderr, program)
		return errors.New("service command is required")
	}
	switch args[0] {
	case "-h", "--help", "help":
		printServiceUsage(stdout, program)
		return nil
	case "install":
		return executeServiceInstall(ctx, program, args[1:], stdout, stderr, services)
	case "uninstall":
		if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
			fmt.Fprintf(stdout, "Usage: %s service uninstall\n", program)
			return nil
		}
		if len(args) != 1 {
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
	default:
		return errors.New("unknown service command")
	}
}

func printServiceUsage(writer io.Writer, program string) {
	fmt.Fprintf(writer, "Usage:\n  %s service install --token STRING\n  %s service uninstall\n", program, program)
}

func executeServiceInstall(
	ctx context.Context,
	program string,
	args []string,
	stdout, stderr io.Writer,
	services serviceOperations,
) error {
	flags := flag.NewFlagSet(program+" service install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var token string
	flags.StringVar(&token, "token", "", "Agent Token")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s service install --token STRING\n", program)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse service install command: %w", err)
	}
	if flags.NArg() != 0 {
		return errors.New("service install does not accept positional arguments")
	}
	tokenSet := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == "token" {
			tokenSet = true
		}
	})
	if !tokenSet {
		return errors.New("service install requires --token")
	}
	validatedToken, err := validateToken(token)
	if err != nil {
		return fmt.Errorf("validate service install Token: %w", err)
	}
	if err := services.Install(ctx, validatedToken); err != nil {
		return fmt.Errorf("install Agent service: %w", err)
	}
	fmt.Fprintf(stdout, "installed and started %s\n", service.Name())
	return nil
}

// run 完成 Agent 进程当前阶段的 Token 和日志初始化，并保持前台运行直到收到退出信号。
// 后续任务会在等待 Context 取消前依次接入身份、Control Session 和 WorkPool。
func run(ctx context.Context, program string, args, environ []string, stderr io.Writer) error {
	handled, err := service.RunIfManagedService(func(serviceContext context.Context, token string) error {
		if _, err := validateToken(token); err != nil {
			return fmt.Errorf("validate Windows service credential: %w", err)
		}
		return runLifecycle(serviceContext, stderr)
	})
	if err != nil {
		return fmt.Errorf("run Agent as native service: %w", err)
	}
	if handled {
		return nil
	}
	if _, err := resolveToken(program, args, environ, stderr); err != nil {
		return fmt.Errorf("load agent token: %w", err)
	}
	return runLifecycle(ctx, stderr)
}

func runLifecycle(ctx context.Context, stderr io.Writer) error {
	logger, err := logging.New(stderr, logging.Options{
		Level:     "info",
		Format:    "json",
		Component: "agent",
	})
	if err != nil {
		return fmt.Errorf("initialize agent logging: %w", err)
	}

	logger.InfoContext(ctx, "process_started")
	<-ctx.Done()
	logger.Info("process_stopped")
	return nil
}

func resolveToken(program string, args, environ []string, stderr io.Writer) (string, error) {
	flags := flag.NewFlagSet(program, flag.ContinueOnError)
	flags.SetOutput(stderr)

	var cliToken string
	flags.StringVar(&cliToken, "token", "", "Agent Token")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s [--token STRING]\n", program)
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return "", fmt.Errorf("parse command line: %w", err)
	}
	if flags.NArg() != 0 {
		return "", errors.New("unexpected positional arguments")
	}

	cliTokenSet := false
	flags.Visit(func(current *flag.Flag) {
		if current.Name == "token" {
			cliTokenSet = true
		}
	})
	if cliTokenSet {
		return validateToken(cliToken)
	}
	if token, ok := lookupEnvironment(environ, tokenEnvironment); ok {
		return validateToken(token)
	}

	directory, ok := lookupEnvironment(environ, credentialsDirectory)
	if !ok {
		return "", errors.New("token is required: use --token, XTUNNEL_TOKEN, or the systemd credential")
	}
	if directory == "" {
		return "", errors.New("CREDENTIALS_DIRECTORY must not be empty")
	}
	token, err := os.ReadFile(filepath.Join(directory, systemdCredentialName))
	if err != nil {
		return "", fmt.Errorf("read systemd credential %q: %w", systemdCredentialName, err)
	}
	return validateToken(string(token))
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
