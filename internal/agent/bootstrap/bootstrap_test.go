package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
	"github.com/lifei6671/xtunnel/internal/agent/service"
	"github.com/lifei6671/xtunnel/internal/logging"
)

func TestMissingTokenHintMatchesPlatform(t *testing.T) {
	_, err := resolveTokenSource("", false, nil)
	if err == nil || !strings.Contains(err.Error(), "--token") || !strings.Contains(err.Error(), "XTUNNEL_TOKEN") {
		t.Fatalf("missing token hint = %v", err)
	}
	if strings.Contains(err.Error(), "systemd") != (runtime.GOOS == "linux") {
		t.Fatalf("missing token hint does not match %s: %v", runtime.GOOS, err)
	}
}

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
		{
			name: "maximum size token is accepted",
			args: []string{"--token", "xta_" + strings.Repeat("x", maxTokenBytes-len("xta_"))},
			want: "xta_" + strings.Repeat("x", maxTokenBytes-len("xta_")),
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

func TestResolveTokenOverrideDoesNotReadServiceCredential(t *testing.T) {
	tests := []struct {
		name        string
		cliToken    string
		cliTokenSet bool
		environ     []string
		want        string
		wantFound   bool
	}{
		{
			name:        "CLI wins over environment",
			cliToken:    "xta_cli_override_secret",
			cliTokenSet: true,
			environ:     []string{"XTUNNEL_TOKEN=xta_environment_secret"},
			want:        "xta_cli_override_secret",
			wantFound:   true,
		},
		{
			name:      "environment override",
			environ:   []string{"XTUNNEL_TOKEN=xta_environment_secret"},
			want:      "xta_environment_secret",
			wantFound: true,
		},
		{
			name:      "service credential remains lazy fallback",
			environ:   []string{"CREDENTIALS_DIRECTORY=missing-service-credential"},
			wantFound: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, found, err := resolveTokenOverrideSource(test.cliToken, test.cliTokenSet, test.environ)
			if err != nil {
				t.Fatalf("resolveTokenOverrideSource() error = %v", err)
			}
			if got != test.want || found != test.wantFound {
				t.Fatalf("resolveTokenOverrideSource() = (%q, %t), want (%q, %t)", got, found, test.want, test.wantFound)
			}
		})
	}
}

func TestResolveTokenOverrideRejectsInvalidHighPrioritySourceWithoutLeaking(t *testing.T) {
	const secret = "invalid_cli_override_secret"
	_, found, err := resolveTokenOverrideSource(
		secret,
		true,
		[]string{"XTUNNEL_TOKEN=xta_environment_secret"},
	)
	if !found || err == nil || !strings.Contains(err.Error(), "must start with xta_") {
		t.Fatalf("resolveTokenOverrideSource() = found %t, error %v", found, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("override validation error leaked Token")
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
	if !errors.Is(err, errCLIHelp) {
		t.Fatalf("resolveToken() error = %v, want errCLIHelp", err)
	}
	output := stderr.String()
	if !strings.Contains(output, "--token string") {
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
	var receivedToken string
	go func() {
		done <- runWithLifecycle(ctx, "xtunnel-agent", []string{"--token", token}, nil, &stderr,
			func(ctx context.Context, currentToken string, writer io.Writer) error {
				receivedToken = currentToken
				logger, err := logging.New(writer, logging.Options{Level: "info", Format: "json", Component: "agent"})
				if err != nil {
					return err
				}
				logger.InfoContext(ctx, "process_started")
				<-ctx.Done()
				logger.Info("process_stopped")
				return nil
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

	if strings.Contains(stderr.String(), token) {
		t.Fatal("lifecycle logs leaked Token")
	}
	if receivedToken != token {
		t.Fatalf("lifecycle token = %q, want resolved token", receivedToken)
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

func TestExecuteDiagnoseUsesTokenSourcePrecedenceAndStableOutput(t *testing.T) {
	credentialDirectory := writeCredential(t, "xta_diagnostic_credential_secret")
	tests := []struct {
		name    string
		args    []string
		environ []string
		want    string
	}{
		{
			name: "CLI wins",
			args: []string{"diagnose", "--token", "xta_diagnostic_cli_secret"},
			environ: []string{
				"XTUNNEL_TOKEN=xta_diagnostic_environment_secret",
				"CREDENTIALS_DIRECTORY=" + credentialDirectory,
			},
			want: "xta_diagnostic_cli_secret",
		},
		{
			name: "environment wins",
			args: []string{"diagnose"},
			environ: []string{
				"XTUNNEL_TOKEN=xta_diagnostic_environment_secret",
				"CREDENTIALS_DIRECTORY=" + credentialDirectory,
			},
			want: "xta_diagnostic_environment_secret",
		},
		{
			name:    "systemd credential fallback",
			args:    []string{"diagnose"},
			environ: []string{"CREDENTIALS_DIRECTORY=" + credentialDirectory},
			want:    "xta_diagnostic_credential_secret",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var receivedToken string
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := executeProgram("xtunnel-agent", test.args, test.environ, &stdout, &stderr,
				func(_ context.Context, token string) agentgateway.DiagnosticResult {
					receivedToken = token
					return agentgateway.DiagnosticResult{
						Steps: []agentgateway.DiagnosticStep{{
							Stage: "TOKEN", Status: agentgateway.DiagnosticPass, Message: "connection token is valid",
						}},
						Summary: agentgateway.DiagnosticReady,
					}
				})
			if exitCode != 0 {
				t.Fatalf("executeProgram() = %d, want 0; stderr = %q", exitCode, stderr.String())
			}
			if receivedToken != test.want {
				t.Fatalf("diagnostic runner received wrong Token source")
			}
			if stdout.String() != "PASS TOKEN connection token is valid\nREADY\n" || stderr.Len() != 0 {
				t.Fatalf("diagnose output = stdout %q stderr %q", stdout.String(), stderr.String())
			}
			for _, secret := range []string{
				"xta_diagnostic_cli_secret",
				"xta_diagnostic_environment_secret",
				"xta_diagnostic_credential_secret",
			} {
				if strings.Contains(stdout.String()+stderr.String(), secret) {
					t.Fatalf("diagnose output leaked Token %q", secret)
				}
			}
		})
	}
}

func TestExecuteDiagnoseSummaryExitCodesAndSensitiveBoundary(t *testing.T) {
	const token = "xta_diagnostic_sensitive_token"
	tests := []struct {
		name        string
		summary     agentgateway.DiagnosticSummary
		status      agentgateway.DiagnosticStatus
		wantCode    int
		wantStderr  bool
		wantSummary string
	}{
		{
			name:    "ready",
			summary: agentgateway.DiagnosticReady, status: agentgateway.DiagnosticPass,
			wantCode: 0, wantSummary: "READY\n",
		},
		{
			name:    "ready degraded",
			summary: agentgateway.DiagnosticReadyDegraded, status: agentgateway.DiagnosticWarning,
			wantCode: 0, wantSummary: "READY_DEGRADED\n",
		},
		{
			name:    "not ready",
			summary: agentgateway.DiagnosticNotReady, status: agentgateway.DiagnosticFail,
			wantCode: 1, wantStderr: true, wantSummary: "NOT_READY\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := executeProgram("xtunnel-agent", []string{"diagnose", "--token", token}, nil, &stdout, &stderr,
				func(context.Context, string) agentgateway.DiagnosticResult {
					return agentgateway.DiagnosticResult{
						Steps: []agentgateway.DiagnosticStep{{
							Stage: "CONTROL_TCP", Status: test.status, Message: "fixed diagnostic message",
						}},
						Summary: test.summary,
					}
				})
			if exitCode != test.wantCode {
				t.Fatalf("executeProgram() = %d, want %d", exitCode, test.wantCode)
			}
			if !strings.HasSuffix(stdout.String(), test.wantSummary) {
				t.Fatalf("stdout = %q, want suffix %q", stdout.String(), test.wantSummary)
			}
			if (stderr.Len() != 0) != test.wantStderr {
				t.Fatalf("stderr = %q, want present %t", stderr.String(), test.wantStderr)
			}
			if strings.Contains(stdout.String()+stderr.String(), token) {
				t.Fatal("diagnose result leaked Token")
			}
		})
	}
}

func TestExecuteDiagnoseMalformedTokenUsesPublicPathWithoutLeak(t *testing.T) {
	const token = "xta_malformed_diagnostic_sensitive_sentinel"
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := Execute("xtunnel-agent", []string{"diagnose", "--token", token}, nil, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("Execute() = %d, want 1", exitCode)
	}
	if stdout.String() != "FAIL TOKEN connection token is invalid\nNOT_READY\n" {
		t.Fatalf("stdout = %q, want stable malformed Token result", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Fatal("public diagnose path leaked malformed Token")
	}
}

func TestExecuteDiagnoseHelpAndInvalidArgumentsDoNotRun(t *testing.T) {
	var calls atomic.Int32
	runner := func(context.Context, string) agentgateway.DiagnosticResult {
		calls.Add(1)
		return agentgateway.DiagnosticResult{Summary: agentgateway.DiagnosticReady}
	}
	tests := []struct {
		name     string
		args     []string
		wantCode int
		contains string
	}{
		{name: "root help", args: []string{"--help"}, contains: "diagnose"},
		{name: "diagnose help", args: []string{"diagnose", "--help"}, contains: "diagnose [--token string]"},
		{name: "unknown flag", args: []string{"diagnose", "--endpoint", "secret.example"}, wantCode: 1, contains: "flag provided but not defined"},
		{name: "positional", args: []string{"diagnose", "xta_positional_secret"}, wantCode: 1, contains: "does not accept positional arguments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			gotCode := executeProgram("xtunnel-agent", test.args, nil, &stdout, &stderr, runner)
			if gotCode != test.wantCode {
				t.Fatalf("executeProgram() = %d, want %d", gotCode, test.wantCode)
			}
			if !strings.Contains(stdout.String()+stderr.String(), test.contains) {
				t.Fatalf("output = stdout %q stderr %q, want %q", stdout.String(), stderr.String(), test.contains)
			}
			if strings.Contains(stdout.String()+stderr.String(), "xta_positional_secret") {
				t.Fatal("invalid argument output leaked positional Token")
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("help/invalid arguments invoked diagnostic runner %d times", calls.Load())
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

func TestExecuteServiceInstallRejectsInvalidInputWithoutLeakingToken(t *testing.T) {
	const token = "xta_service_install_secret"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing Token", args: []string{"service", "install"}, want: "requires --token"},
		{name: "unknown flag", args: []string{"service", "install", "--config", token}, want: "flag provided but not defined"},
		{name: "positional argument", args: []string{"service", "install", "--token", token, "extra"}, want: "does not accept positional"},
		{name: "invalid Token", args: []string{"service", "install", "--token", "wrong_" + token}, want: "must start with xta_"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			services := &fakeServiceOperations{}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := execute(context.Background(), "xtunnel-agent", test.args, nil, &stdout, &stderr, services)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("execute(service install) error = %v, want substring %q", err, test.want)
			}
			if services.installCalls != 0 {
				t.Fatal("invalid service install input invoked service installation")
			}
			if strings.Contains(err.Error()+stdout.String()+stderr.String(), token) {
				t.Fatal("invalid service install input leaked Token")
			}
		})
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
			if !strings.Contains(stdout.String()+stderr.String(), "USAGE") {
				t.Fatalf("help output missing Usage: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestExecuteExplicitFalseHelpDoesNotRunActions(t *testing.T) {
	tests := [][]string{
		{"run", "--help=false", "--token", "xta_runtime_secret"},
		{"service", "install", "--help=false", "--token", "xta_install_secret"},
		{"service", "uninstall", "--help=false"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args[:2], "_"), func(t *testing.T) {
			services := &fakeServiceOperations{}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			if err := execute(context.Background(), "xtunnel-agent", args, nil, &stdout, &stderr, services); err != nil {
				t.Fatalf("execute() error = %v", err)
			}
			if services.installCalls != 0 || services.uninstallCalls != 0 {
				t.Fatal("explicit false help invoked service operation")
			}
			if !strings.Contains(stdout.String()+stderr.String(), "USAGE") {
				t.Fatalf("help output missing Usage: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestExecuteLeafHelpSubcommandRemainsPositionalError(t *testing.T) {
	tests := [][]string{
		{"run", "help"},
		{"service", "install", "help"},
		{"service", "uninstall", "help"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			services := &fakeServiceOperations{}
			err := execute(context.Background(), "xtunnel-agent", args, nil, &bytes.Buffer{}, &bytes.Buffer{}, services)
			if err == nil {
				t.Fatal("execute() error = nil, want positional argument error")
			}
			if services.installCalls != 0 || services.uninstallCalls != 0 {
				t.Fatal("leaf help subcommand invoked service operation")
			}
		})
	}
}

func TestExecuteHelpAndMissingCommandStreams(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout bool
		wantStderr bool
	}{
		{name: "root help", args: []string{"--help"}, wantCode: 0, wantStdout: true},
		{name: "run help", args: []string{"run", "--help"}, wantCode: 0, wantStdout: true},
		{name: "missing command", wantCode: 1, wantStderr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			gotCode := Execute("xtunnel-agent", test.args, nil, &stdout, &stderr)
			if gotCode != test.wantCode {
				t.Fatalf("Execute() = %d, want %d", gotCode, test.wantCode)
			}
			if (stdout.Len() != 0) != test.wantStdout {
				t.Fatalf("stdout presence = %t, want %t; output = %q", stdout.Len() != 0, test.wantStdout, stdout.String())
			}
			if (stderr.Len() != 0) != test.wantStderr {
				t.Fatalf("stderr presence = %t, want %t; output = %q", stderr.Len() != 0, test.wantStderr, stderr.String())
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
