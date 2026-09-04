package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v3"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/pathprofile"
	"github.com/lifei6671/xtunnel/internal/tracing"
)

func TestParseConfigOptions(t *testing.T) {
	configPath := writeConfig(t, "logging:\n  level: warn\n")
	options, err := parseConfigOptions(
		"xtunnel-server",
		[]string{
			"--config", configPath,
			"--set", "logging.level=error",
			"--set", "logging.level=debug",
			"--set", "management.public_url=https://admin.example.com/path?a=b,c=d",
		},
		[]string{"OTHER=value"},
		&bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("parseConfigOptions() error = %v", err)
	}
	if string(options.YAML) != "logging:\n  level: warn\n" {
		t.Fatalf("YAML = %q", options.YAML)
	}
	if options.CLI["logging.level"] != "debug" {
		t.Fatalf("CLI logging.level = %q, want last override", options.CLI["logging.level"])
	}
	if options.CLI["management.public_url"] != "https://admin.example.com/path?a=b,c=d" {
		t.Fatalf("CLI management.public_url = %q, want comma and equals preserved", options.CLI["management.public_url"])
	}
	if len(options.Environment) != 1 || options.Environment[0] != "OTHER=value" {
		t.Fatalf("Environment = %#v", options.Environment)
	}
}

func TestServerCLIParsesRootOptionsAndRejectsCommandsAfterFlags(t *testing.T) {
	t.Run("root options", func(t *testing.T) {
		called := false
		var stderr bytes.Buffer
		exitCode := executeWithRun(
			"xtunnel-server",
			[]string{"--set", "logging.level=debug", "--set", "logging.level=warn"},
			[]string{"OTHER=value"},
			&stderr,
			func(_ context.Context, options baseconfig.Options, _ io.Writer) error {
				called = true
				if options.CLI["logging.level"] != "warn" {
					t.Fatalf("CLI logging.level = %q, want last override", options.CLI["logging.level"])
				}
				return nil
			},
		)
		if exitCode != 0 || !called {
			t.Fatalf("executeWithRun() = %d, called = %t, stderr = %q", exitCode, called, stderr.String())
		}
	})

	t.Run("subcommand must be first", func(t *testing.T) {
		var stderr bytes.Buffer
		exitCode := executeWithRun(
			"xtunnel-server",
			[]string{"--set", "logging.level=debug", "backup", "create", "--help"},
			nil,
			&stderr,
			func(context.Context, baseconfig.Options, io.Writer) error {
				t.Fatal("misplaced backup command invoked Server runner")
				return nil
			},
		)
		if exitCode != 1 || !strings.Contains(stderr.String(), "unexpected positional arguments") {
			t.Fatalf("executeWithRun() = %d, stderr = %q", exitCode, stderr.String())
		}
	})

	t.Run("help", func(t *testing.T) {
		var stderr bytes.Buffer
		exitCode := executeWithRun("xtunnel-server", []string{"--help"}, nil, &stderr, func(context.Context, baseconfig.Options, io.Writer) error {
			t.Fatal("help invoked Server runner")
			return nil
		})
		if exitCode != 0 || !strings.Contains(stderr.String(), "COMMANDS:") || !strings.Contains(stderr.String(), "backup") {
			t.Fatalf("executeWithRun(--help) = %d, stderr = %q", exitCode, stderr.String())
		}
	})

	t.Run("explicit false help", func(t *testing.T) {
		var stderr bytes.Buffer
		exitCode := executeWithRun("xtunnel-server", []string{"--help=false", "--config", filepath.Join(t.TempDir(), "missing.yaml")}, nil, &stderr, func(context.Context, baseconfig.Options, io.Writer) error {
			t.Fatal("explicit false help invoked Server runner")
			return nil
		})
		if exitCode != 0 || !strings.Contains(stderr.String(), "USAGE") {
			t.Fatalf("executeWithRun(--help=false) = %d, stderr = %q", exitCode, stderr.String())
		}
	})
}

func TestServerCLIServiceInstallAndUninstall(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "server.yaml")
	services := &fakeServerServiceOperations{}
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "install", args: []string{"service", "install", "--config", configPath}},
		{name: "uninstall", args: []string{"service", "uninstall"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			command := newServerCommandWithServices(
				"xtunnel-server",
				test.args,
				nil,
				&output,
				func(context.Context, baseconfig.Options, io.Writer) error {
					t.Fatal("service command invoked Server runner")
					return nil
				},
				services,
			)
			if err := command.Run(context.Background(), append([]string{"xtunnel-server"}, test.args...)); err != nil {
				t.Fatalf("command.Run() error = %v", err)
			}
			if !strings.Contains(output.String(), "xtunnel-server.service") {
				t.Fatalf("output = %q", output.String())
			}
		})
	}
	if services.installCalls != 1 || services.configSource != configPath {
		t.Fatalf("install calls = %d, config = %q", services.installCalls, services.configSource)
	}
	if services.uninstallCalls != 1 {
		t.Fatalf("uninstall calls = %d", services.uninstallCalls)
	}
}

func TestServerCLIServiceRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		match string
	}{
		{name: "missing config", args: []string{"service", "install"}, match: "requires --config"},
		{name: "install positional", args: []string{"service", "install", "extra"}, match: "does not accept positional"},
		{name: "uninstall positional", args: []string{"service", "uninstall", "extra"}, match: "does not accept arguments"},
		{name: "unknown service command", args: []string{"service", "replace"}, match: "unknown service command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			services := &fakeServerServiceOperations{}
			command := newServerCommandWithServices(
				"xtunnel-server", test.args, nil, &bytes.Buffer{},
				func(context.Context, baseconfig.Options, io.Writer) error {
					t.Fatal("invalid service command invoked Server runner")
					return nil
				},
				services,
			)
			err := command.Run(context.Background(), append([]string{"xtunnel-server"}, test.args...))
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("command.Run() error = %v, want substring %q", err, test.match)
			}
			if services.installCalls != 0 || services.uninstallCalls != 0 {
				t.Fatalf("service calls = install %d uninstall %d", services.installCalls, services.uninstallCalls)
			}
		})
	}
}

func TestServerCLIInit(t *testing.T) {
	configPath := writeConfig(t, "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: gateway.example.test\n")
	var initialized baseconfig.Options
	called := false
	args := []string{"init", "--config", configPath, "--set", "logging.level=debug"}
	var output bytes.Buffer
	command := newServerCommandWithServicesAndInitializer(
		"xtunnel-server", args, nil, &output,
		func(context.Context, baseconfig.Options, io.Writer) error {
			t.Fatal("server init invoked the Server runner")
			return nil
		},
		&fakeServerServiceOperations{},
		func(_ context.Context, options baseconfig.Options) error {
			called = true
			initialized = options
			return nil
		},
	)
	if err := command.Run(context.Background(), append([]string{"xtunnel-server"}, args...)); err != nil {
		t.Fatalf("command.Run() error = %v", err)
	}
	if !called || string(initialized.YAML) == "" || initialized.CLI["logging.level"] != "debug" {
		t.Fatalf("initializer options = %#v, called = %t", initialized, called)
	}
	if !strings.Contains(output.String(), "initialized Server data and runtime directories") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestServerCLIInitRejectsInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  []string
		match string
	}{
		{name: "missing config", args: []string{"init"}, match: "requires --config"},
		{name: "positional", args: []string{"init", "extra"}, match: "does not accept positional"},
		{name: "invalid set", args: []string{"init", "--config", filepath.Join(t.TempDir(), "server.yaml"), "--set", "logging.level"}, match: "expected path=value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			command := newServerCommandWithServicesAndInitializer(
				"xtunnel-server", test.args, nil, &bytes.Buffer{},
				func(context.Context, baseconfig.Options, io.Writer) error { return nil },
				&fakeServerServiceOperations{},
				func(context.Context, baseconfig.Options) error {
					called = true
					return nil
				},
			)
			err := command.Run(context.Background(), append([]string{"xtunnel-server"}, test.args...))
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("command.Run() error = %v, want substring %q", err, test.match)
			}
			if called {
				t.Fatal("invalid server init invoked initializer")
			}
		})
	}
}

func TestParseConfigOptionsRejectsInvalidCommandLine(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		match string
	}{
		{name: "unknown flag", args: []string{"--unknown"}, match: "flag provided but not defined"},
		{name: "positional argument", args: []string{"extra"}, match: "unexpected positional arguments"},
		{name: "invalid set", args: []string{"--set", "logging.level"}, match: "expected path=value"},
		{name: "missing file", args: []string{"--config", filepath.Join(t.TempDir(), "missing.yaml")}, match: "read config file"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseConfigOptions("xtunnel-server", test.args, nil, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("parseConfigOptions() error = %v, want substring %q", err, test.match)
			}
		})
	}
}

func TestParseConfigOptionsHelp(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseConfigOptions("xtunnel-server", []string{"--help"}, nil, &stderr)
	if !errors.Is(err, errServerCLIHelp) {
		t.Fatalf("parseConfigOptions() error = %v, want errServerCLIHelp", err)
	}
	if !strings.Contains(stderr.String(), "--config") || !strings.Contains(stderr.String(), "--set") {
		t.Fatalf("help output = %q", stderr.String())
	}
}

func TestServerExplicitFalseHelpDoesNotRunMaintenanceActions(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		newCommand func(*bool, io.Writer) *cli.Command
	}{
		{
			name: "admin create",
			args: []string{"--help=false", "--username", "admin"},
			newCommand: func(called *bool, output io.Writer) *cli.Command {
				return newAdminCreateCommand("xtunnel-server", nil, output, func(context.Context, adminCreateOptions) error {
					*called = true
					return nil
				})
			},
		},
		{
			name: "gateway rotate-key",
			args: []string{"--help=false", "--maintenance"},
			newCommand: func(called *bool, output io.Writer) *cli.Command {
				return newGatewayRotateKeyCommand("xtunnel-server", nil, output, func(context.Context, baseconfig.Options) error {
					*called = true
					return nil
				})
			},
		},
		{
			name: "backup create",
			args: []string{"--help=false", "--output", filepath.Join(t.TempDir(), "backup.tar")},
			newCommand: func(called *bool, output io.Writer) *cli.Command {
				return newBackupOperationCommand("xtunnel-server", "create", nil, output, func(context.Context, backupCommandOptions) error {
					*called = true
					return nil
				})
			},
		},
		{
			name: "backup restore",
			args: []string{"--help=false", "--input", filepath.Join(t.TempDir(), "backup.tar")},
			newCommand: func(called *bool, output io.Writer) *cli.Command {
				return newBackupOperationCommand("xtunnel-server", "restore", nil, output, func(context.Context, backupCommandOptions) error {
					*called = true
					return nil
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			var output bytes.Buffer
			command := test.newCommand(&called, &output)
			command.Writer = &output
			command.ErrWriter = &output
			command.HideVersion = true
			command.ExitErrHandler = ignoreCLIExitError
			err := command.Run(context.Background(), append([]string{command.Name}, test.args...))
			if !errors.Is(err, errServerCLIHelp) {
				t.Fatalf("command.Run() error = %v, want errServerCLIHelp", err)
			}
			if called {
				t.Fatal("explicit false help invoked maintenance action")
			}
			if !strings.Contains(output.String(), "USAGE") {
				t.Fatalf("help output = %q, want Usage", output.String())
			}
		})
	}
}

func TestRunWaitsForContextCancellation(t *testing.T) {
	configPath := writeConfig(t, `
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	configuredDataDir, dataDir := newBootstrapLifecycleTestDataDir(t)
	var stderr bytes.Buffer
	resources := &fakeStorage{}
	go func() {
		done <- runWithStorage(ctx, "xtunnel-server", []string{
			"--config", configPath,
			"--set", "server.data_dir=" + configuredDataDir,
		}, nil, &stderr, func(_ context.Context, gotDataDir string) (storage, error) {
			if gotDataDir != dataDir {
				t.Errorf("storage dataDir = %q, want %q", gotDataDir, dataDir)
			}
			return resources, nil
		})
	}()

	select {
	case err := <-done:
		t.Fatalf("run() returned before cancellation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run() did not return after cancellation")
	}
	if !resources.closed {
		t.Fatal("runWithStorage() returned without closing storage")
	}

	assertLifecycleLogs(t, stderr.String(), "server")
}

func TestRunReturnsStorageCloseError(t *testing.T) {
	configPath := writeConfig(t, `
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wantErr := errors.New("close failed")
	configuredDataDir, _ := newBootstrapLifecycleTestDataDir(t)
	err := runWithStorage(ctx, "xtunnel-server", []string{
		"--config", configPath,
		"--set", "server.data_dir=" + configuredDataDir,
	}, nil, &bytes.Buffer{}, func(context.Context, string) (storage, error) {
		return &fakeStorage{closeErr: wantErr}, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runWithStorage() error = %v, want close error", err)
	}
}

func TestRunClosesStorageWhenBootstrapInitializationFails(t *testing.T) {
	configPath := writeConfig(t, `
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
`)
	resources := &fakeStorage{}
	wantErr := errors.New("startup snapshot gate failed")
	configuredDataDir, _ := newBootstrapLifecycleTestDataDir(t)
	err := runWithStorageAndBootstrap(
		context.Background(),
		"xtunnel-server",
		[]string{"--config", configPath, "--set", "server.data_dir=" + configuredDataDir},
		nil,
		&bytes.Buffer{},
		func(context.Context, string) (storage, error) { return resources, nil },
		func(context.Context, serverconfig.Config, storage, *slog.Logger, *tracing.Runtime) (io.Closer, error) {
			return nil, wantErr
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runWithStorageAndBootstrap() error = %v, want startup failure", err)
	}
	if !resources.closed {
		t.Fatal("runWithStorageAndBootstrap() did not close storage after startup failure")
	}
}

func TestRunPassesProductionLoggerToBootstrap(t *testing.T) {
	configPath := writeConfig(t, `
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resources := &fakeStorage{}
	bootstrapCloser := &fakeStorage{}
	var stderr bytes.Buffer
	configuredDataDir, _ := newBootstrapLifecycleTestDataDir(t)
	err := runWithStorageAndBootstrap(
		ctx,
		"xtunnel-server",
		[]string{"--config", configPath, "--set", "server.data_dir=" + configuredDataDir},
		nil,
		&stderr,
		func(context.Context, string) (storage, error) { return resources, nil },
		func(_ context.Context, _ serverconfig.Config, _ storage, logger *slog.Logger, _ *tracing.Runtime) (io.Closer, error) {
			if logger == nil {
				t.Fatal("bootstrap received a nil production Logger")
			}
			logger.Info("connector_connected")
			cancel()
			return bootstrapCloser, nil
		},
	)
	if err != nil {
		t.Fatalf("runWithStorageAndBootstrap() error = %v", err)
	}
	if !resources.closed || !bootstrapCloser.closed {
		t.Fatalf("bootstrap close state = resources %t bootstrap %t", resources.closed, bootstrapCloser.closed)
	}
	if !strings.Contains(stderr.String(), `"component":"server"`) ||
		!strings.Contains(stderr.String(), `"event":"connector_connected"`) {
		t.Fatalf("bootstrap production Logger output = %s", stderr.String())
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	err := run(context.Background(), "xtunnel-server", nil, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "load server config") {
		t.Fatalf("run() error = %v, want config error", err)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	return path
}

// newBootstrapLifecycleTestDataDir 为只使用 fake storage 的生命周期测试准备
// 兼容当前平台 Profile 的配置值；Windows 不创建或写入真实用户目录。
func newBootstrapLifecycleTestDataDir(t *testing.T) (configuredDataDir, resolvedDataDir string) {
	t.Helper()
	configuredDataDir = t.TempDir()
	if runtime.GOOS == "windows" {
		configuredDataDir = pathprofile.AutomaticDataDir
	}
	profile, err := pathprofile.Resolve(configuredDataDir)
	if err != nil {
		t.Fatalf("pathprofile.Resolve(%q) error = %v", configuredDataDir, err)
	}
	return configuredDataDir, profile.DataDir
}

func assertLifecycleLogs(t *testing.T, output, component string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 2 {
		t.Fatalf("lifecycle log lines = %d, want 2; output = %q", len(lines), output)
	}
	wantEvents := []string{"process_started", "process_stopped"}
	for index, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("json.Unmarshal(log line %d) error = %v", index, err)
		}
		if record["component"] != component || record["event"] != wantEvents[index] {
			t.Fatalf("log line %d = %#v", index, record)
		}
	}
}

type fakeStorage struct {
	closed   bool
	closeErr error
}

type fakeServerServiceOperations struct {
	installCalls   int
	uninstallCalls int
	configSource   string
}

func (operations *fakeServerServiceOperations) Install(_ context.Context, configSource string) error {
	operations.installCalls++
	operations.configSource = configSource
	return nil
}

func (operations *fakeServerServiceOperations) Uninstall(context.Context) error {
	operations.uninstallCalls++
	return nil
}

func (storage *fakeStorage) Close() error {
	storage.closed = true
	return storage.closeErr
}
