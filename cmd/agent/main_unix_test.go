//go:build !windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestProcessExitsOnSIGTERM(t *testing.T) {
	if os.Getenv("XTUNNEL_AGENT_TEST_HELPER") == "1" {
		for index, arg := range os.Args {
			if arg == "--" {
				os.Exit(execute("xtunnel-agent", os.Args[index+1:], os.Environ(), os.Stderr))
			}
		}
		os.Exit(2)
	}

	configPath := writeConfig(t, `
server:
  endpoint: tunnel.example.com:7443
  tls:
    server_pin: sha256:dGVzdC1waW4=
`)
	dataDir := t.TempDir()
	args := []string{
		"-test.run=^TestProcessExitsOnSIGTERM$", "--",
		"--config", configPath,
		"--set", "data_dir=" + dataDir,
		"--set", "auth.token_file=" + filepath.Join(dataDir, "token"),
	}
	command := exec.Command(os.Args[0], args...)
	command.Env = append(os.Environ(), "XTUNNEL_AGENT_TEST_HELPER=1")
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
