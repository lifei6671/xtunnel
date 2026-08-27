//go:build !windows

package bootstrap

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
)

func TestProcessExitsOnSIGTERM(t *testing.T) {
	if os.Getenv("GO_WANT_XTUNNEL_SERVER_HELPER_PROCESS") == "1" {
		for index, arg := range os.Args {
			if arg == "--" {
				runner := func(ctx context.Context, options baseconfig.Options, stderr io.Writer) error {
					return runWithStorageAndBootstrapOptions(ctx, options, stderr, func(context.Context, string) (storage, error) {
						return &fakeStorage{}, nil
					}, nil)
				}
				os.Exit(executeWithRun("xtunnel-server", os.Args[index+1:], os.Environ(), os.Stderr, runner))
			}
		}
		os.Exit(2)
	}

	configPath := writeConfig(t, `
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
`)
	args := []string{
		"-test.run=^TestProcessExitsOnSIGTERM$", "--",
		"--config", configPath,
		"--set", "server.data_dir=" + t.TempDir(),
	}
	command := exec.Command(os.Args[0], args...)
	command.Env = append(os.Environ(), "GO_WANT_XTUNNEL_SERVER_HELPER_PROCESS=1")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("command.Start() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	exited := false
	t.Cleanup(func() {
		if exited {
			return
		}
		_ = command.Process.Kill()
		<-done
	})

	select {
	case err := <-done:
		exited = true
		t.Fatalf("process exited before signal: %v\n%s", err, output.String())
	case <-time.After(100 * time.Millisecond):
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Process.Signal(SIGTERM) error = %v", err)
	}

	select {
	case err := <-done:
		exited = true
		if err != nil {
			t.Fatalf("process exit error = %v\n%s", err, output.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after SIGTERM")
	}
}
