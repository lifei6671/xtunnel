package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfigOptions(t *testing.T) {
	configPath := writeConfig(t, "logging:\n  level: warn\n")
	options, err := parseConfigOptions(
		"xtunnel-agent",
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
			_, err := parseConfigOptions("xtunnel-agent", test.args, nil, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("parseConfigOptions() error = %v, want substring %q", err, test.match)
			}
		})
	}
}

func TestParseConfigOptionsHelp(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseConfigOptions("xtunnel-agent", []string{"--help"}, nil, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseConfigOptions() error = %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(stderr.String(), "--config") || !strings.Contains(stderr.String(), "--set") {
		t.Fatalf("help output = %q", stderr.String())
	}
}

func TestRunWaitsForContextCancellation(t *testing.T) {
	configPath := writeConfig(t, `
server:
  endpoint: tunnel.example.com:7443
  tls:
    server_pin: sha256:dGVzdC1waW4=
`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	dataDir := t.TempDir()
	var stderr bytes.Buffer
	go func() {
		done <- run(ctx, "xtunnel-agent", []string{
			"--config", configPath,
			"--set", "data_dir=" + dataDir,
			"--set", "auth.token_file=" + filepath.Join(dataDir, "token"),
		}, nil, &stderr)
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

	assertLifecycleLogs(t, stderr.String(), "agent")
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	err := run(context.Background(), "xtunnel-agent", nil, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "load agent config") {
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
