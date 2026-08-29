package buildinfo

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const expectedVersionEnvironment = "XTUNNEL_TEST_EXPECTED_BUILD_VERSION"

func TestVersionDefaultsToDevelopmentMarker(t *testing.T) {
	if expected := os.Getenv(expectedVersionEnvironment); expected != "" {
		t.Skip("linker injection subprocess")
	}
	if got := Version(); got != developmentVersion {
		t.Fatalf("Version() = %q, want %q", got, developmentVersion)
	}
}

func TestVersionAcceptsLinkerInjection(t *testing.T) {
	const injectedVersion = "v0.1.0-test.1+build"
	if expected := os.Getenv(expectedVersionEnvironment); expected != "" {
		if got := Version(); got != expected {
			t.Fatalf("Version() = %q, want injected %q", got, expected)
		}
		return
	}

	ldflags := "-X github.com/lifei6671/xtunnel/internal/buildinfo.version=" + injectedVersion
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-count=1", "-run", "^TestVersionAcceptsLinkerInjection$", "-ldflags", ldflags, ".")
	command.Env = append(os.Environ(), expectedVersionEnvironment+"="+injectedVersion)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go test with version linker injection failed: %v\n%s", err, strings.TrimSpace(string(output)))
	}
}
