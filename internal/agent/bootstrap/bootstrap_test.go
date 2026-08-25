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

	"github.com/lifei6671/xtunnel/internal/agent/service"
)

func TestResolveTokenSourcesAndPrecedence(t *testing.T) {
	credentialDirectory := writeCredential(t, "xta_credential_secret")
	tests := []struct {
		name    string
		args    []string
		environ []string
		want    string
	}{
		{
			name: "separate CLI argument wins",
			args: []string{"--token", "xta_cli_secret"},
			environ: []string{
				"XTUNNEL_TOKEN=xta_environment_secret",
				"CREDENTIALS_DIRECTORY=" + credentialDirectory,
			},
			want: "xta_cli_secret",
		},
		{
			name: "equals CLI argument wins",
			args: []string{"--token=xta_equals_secret"},
			environ: []string{
				"XTUNNEL_TOKEN=xta_environment_secret",
				"CREDENTIALS_DIRECTORY=" + credentialDirectory,
			},
			want: "xta_equals_secret",
		},
		{
			name: "environment wins over credential",
			environ: []string{
				"XTUNNEL_TOKEN=xta_environment_secret",
				"CREDENTIALS_DIRECTORY=" + credentialDirectory,
			},
			want: "xta_environment_secret",
		},
		{
			name:    "systemd credential fallback",
			environ: []string{"CREDENTIALS_DIRECTORY=" + credentialDirectory},
			want:    "xta_credential_secret",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got, err := resolveToken("xtunnel-agent", test.args, test.environ, &stderr)
			if err != nil {
				t.Fatalf("resolveToken() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("resolveToken() = %q, want selected source", got)
			}
			if strings.Contains(stderr.String(), got) {
				t.Fatalf("stderr leaked selected Token")
			}
		})
	}
}

func TestResolveTokenRejectsMissingOrInvalidTokenWithoutLeaking(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		environ []string
		secret  string
		match   string
	}{
		{name: "missing", match: "token is required"},
		{name: "empty CLI", args: []string{"--token="}, match: "must not be empty"},
		{name: "empty environment", environ: []string{"XTUNNEL_TOKEN="}, match: "must not be empty"},
		{name: "wrong prefix", args: []string{"--token", "wrong_prefix_secret"}, secret: "wrong_prefix_secret", match: "must start with xta_"},
		{name: "leading whitespace", args: []string{"--token", " xta_leading_secret"}, secret: "xta_leading_secret", match: "leading or trailing whitespace"},
		{name: "trailing whitespace", environ: []string{"XTUNNEL_TOKEN=xta_trailing_secret "}, secret: "xta_trailing_secret", match: "leading or trailing whitespace"},
		{
			name:   "too long",
			args:   []string{"--token", "xta_" + strings.Repeat("x", maxTokenBytes-len("xta_")+1)},
			secret: "xta_" + strings.Repeat("x", maxTokenBytes-len("xta_")+1),
			match:  "must not exceed 8192 bytes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			_, err := resolveToken("xtunnel-agent", test.args, test.environ, &stderr)
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("resolveToken() error = %v, want substring %q", err, test.match)
			}
			if test.secret != "" && (strings.Contains(err.Error(), test.secret) || strings.Contains(stderr.String(), test.secret)) {
				t.Fatalf("error path leaked Token")
			}
		})
	}
}

func TestResolveTokenRejectsInvalidSystemdCredentialWithoutLeaking(t *testing.T) {
	const secret = "xta_credential_secret"
	credentialDirectory := writeCredential(t, secret+"\n")
	var stderr bytes.Buffer
	_, err := resolveToken(
		"xtunnel-agent",
		nil,
		[]string{"CREDENTIALS_DIRECTORY=" + credentialDirectory},
		&stderr,
	)
	if err == nil || !strings.Contains(err.Error(), "leading or trailing whitespace") {
		t.Fatalf("resolveToken() error = %v, want whitespace error", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatal("credential error path leaked Token")
	}
}

func TestResolveTokenRejectsUnsupportedCommandLine(t *testing.T) {
	tests := []string{"--config", "--set", "--token-file", "--unknown"}
	for _, argument := range tests {
		t.Run(argument, func(t *testing.T) {
			_, err := resolveToken("xtunnel-agent", []string{argument}, nil, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("resolveToken() error = %v, want unknown flag error", err)
			}
		})
	}

	const positionalSecret = "xta_positional_secret"
	_, err := resolveToken("xtunnel-agent", []string{positionalSecret}, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
		t.Fatalf("resolveToken() error = %v, want positional argument error", err)
	}
	if strings.Contains(err.Error(), positionalSecret) {
		t.Fatal("positional argument error leaked Token")
	}
}

func TestResolveTokenHelp(t *testing.T) {
	var stderr bytes.Buffer
	_, err := resolveToken("xtunnel-agent", []string{"--help"}, nil, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("resolveToken() error = %v, want flag.ErrHelp", err)
	}
	output := stderr.String()
	if !strings.Contains(output, "--token STRING") || !strings.Contains(output, "-token string") {
		t.Fatalf("help output = %q, want Token usage", output)
	}
	for _, removed := range []string{"--config", "--set", "--token-file"} {
		if strings.Contains(output, removed) {
			t.Fatalf("help output still contains removed option %q", removed)
		}
	}
}

func TestRunWaitsForContextCancellationWithoutLeakingToken(t *testing.T) {
	const token = "xta_runtime_secret"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var stderr bytes.Buffer
	go func() {
		done <- run(ctx, "xtunnel-agent", []string{"--token", token}, nil, &stderr)
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

	if strings.Contains(stderr.String(), token) {
		t.Fatal("lifecycle logs leaked Token")
	}
	assertLifecycleLogs(t, stderr.String(), "agent")
}

func TestExecuteDoesNotLeakInvalidToken(t *testing.T) {
	const token = "invalid_runtime_secret"
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := Execute("xtunnel-agent", []string{"run", "--token", token}, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("Execute() = %d, want 1", exitCode)
	}
	if strings.Contains(stdout.String(), token) || strings.Contains(stderr.String(), token) {
		t.Fatal("process error leaked Token")
	}
}

func TestExecuteServiceCommands(t *testing.T) {
	const token = "xta_install_secret"
	services := &fakeServiceOperations{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := execute(
		context.Background(),
		"xtunnel-agent",
		[]string{"service", "install", "--token", token},
		[]string{"XTUNNEL_TOKEN=xta_environment_secret"},
		&stdout,
		&stderr,
		services,
	); err != nil {
		t.Fatalf("execute(service install) error = %v", err)
	}
	if services.installedToken != token || services.installCalls != 1 {
		t.Fatalf("service install calls = %d, token selected incorrectly", services.installCalls)
	}
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatal("service install output leaked Token")
	}

	stdout.Reset()
	stderr.Reset()
	if err := execute(
		context.Background(),
		"xtunnel-agent",
		[]string{"service", "uninstall"},
		nil,
		&stdout,
		&stderr,
		services,
	); err != nil {
		t.Fatalf("execute(service uninstall) error = %v", err)
	}
	if services.uninstallCalls != 1 {
		t.Fatalf("service uninstall calls = %d, want 1", services.uninstallCalls)
	}
}

func TestExecuteServiceUninstallReportsPendingReboot(t *testing.T) {
	services := &fakeServiceOperations{uninstallResult: service.UninstallResult{BinaryRemovalPendingReboot: true}}
	var stdout bytes.Buffer
	if err := execute(
		context.Background(),
		"xtunnel-agent",
		[]string{"service", "uninstall"},
		nil,
		&stdout,
		&bytes.Buffer{},
		services,
	); err != nil {
		t.Fatalf("execute(service uninstall) error = %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "deleted after the next reboot") || !strings.Contains(output, "credential was preserved") {
		t.Fatalf("pending-reboot output = %q", output)
	}
}

func TestExecuteRejectsLegacyAndInvalidRoutesWithoutLeaking(t *testing.T) {
	const token = "xta_route_secret"
	tests := []struct {
		name string
		args []string
	}{
		{name: "legacy root token", args: []string{"--token", token}},
		{name: "missing command"},
		{name: "unknown command", args: []string{"unknown", token}},
		{name: "missing service command", args: []string{"service"}},
		{name: "unknown service command", args: []string{"service", "unknown", token}},
		{name: "install requires explicit token", args: []string{"service", "install"}},
		{name: "uninstall rejects arguments", args: []string{"service", "uninstall", token}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			services := &fakeServiceOperations{}
			err := execute(
				context.Background(),
				"xtunnel-agent",
				test.args,
				[]string{"XTUNNEL_TOKEN=" + token},
				&stdout,
				&stderr,
				services,
			)
			if err == nil {
				t.Fatal("execute() error = nil, want route error")
			}
			if services.installCalls != 0 || services.uninstallCalls != 0 {
				t.Fatal("invalid route invoked service operation")
			}
			if strings.Contains(err.Error()+stdout.String()+stderr.String(), token) {
				t.Fatal("route error leaked Token")
			}
		})
	}
}

func TestExecuteHelpRoutes(t *testing.T) {
	tests := [][]string{
		{"--help"},
		{"run", "--help"},
		{"service", "--help"},
		{"service", "install", "--help"},
		{"service", "uninstall", "--help"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if exitCode := Execute("xtunnel-agent", args, nil, &stdout, &stderr); exitCode != 0 {
				t.Fatalf("Execute() = %d, want 0", exitCode)
			}
			if !strings.Contains(stdout.String()+stderr.String(), "Usage") {
				t.Fatalf("help output missing Usage: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

type fakeServiceOperations struct {
	installedToken  string
	installCalls    int
	uninstallCalls  int
	uninstallResult service.UninstallResult
}

func (service *fakeServiceOperations) Install(_ context.Context, stringToken string) error {
	service.installCalls++
	service.installedToken = stringToken
	return nil
}

func (operations *fakeServiceOperations) Uninstall(context.Context) (service.UninstallResult, error) {
	operations.uninstallCalls++
	return operations.uninstallResult, nil
}

func writeCredential(t *testing.T, token string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, systemdCredentialName)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	return directory
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
		if record["level"] != "info" {
			t.Fatalf("log line %d level = %#v, want info", index, record["level"])
		}
	}
}
