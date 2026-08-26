package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
)

func TestParseConfigOptions(t *testing.T) {
	configPath := writeConfig(t, "logging:\n  level: warn\n")
	options, err := parseConfigOptions(
		"xtunnel-server",
		[]string{"--config", configPath, "--set", "logging.level=error", "--set", "logging.level=debug"},
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
	if len(options.Environment) != 1 || options.Environment[0] != "OTHER=value" {
		t.Fatalf("Environment = %#v", options.Environment)
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
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseConfigOptions() error = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(stderr.String(), "--config") || !strings.Contains(stderr.String(), "--set") {
		t.Fatalf("help output = %q", stderr.String())
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
	dataDir := t.TempDir()
	var stderr bytes.Buffer
	resources := &fakeStorage{}
	go func() {
		done <- runWithStorage(ctx, "xtunnel-server", []string{
			"--config", configPath,
			"--set", "server.data_dir=" + dataDir,
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
	err := runWithStorage(ctx, "xtunnel-server", []string{
		"--config", configPath,
		"--set", "server.data_dir=" + t.TempDir(),
	}, nil, &bytes.Buffer{}, func(context.Context, string) (storage, error) {
		return &fakeStorage{closeErr: wantErr}, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("runWithStorage() error = %v, want close error", err)
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
	err := runWithStorageAndBootstrap(
		ctx,
		"xtunnel-server",
		[]string{"--config", configPath, "--set", "server.data_dir=" + t.TempDir()},
		nil,
		&stderr,
		func(context.Context, string) (storage, error) { return resources, nil },
		func(_ context.Context, _ serverconfig.Config, _ storage, logger *slog.Logger) (io.Closer, error) {
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

func (storage *fakeStorage) Close() error {
	storage.closed = true
	return storage.closeErr
}
