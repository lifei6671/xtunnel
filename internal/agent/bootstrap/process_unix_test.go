//go:build !windows

package bootstrap

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessExitsOnSIGTERM(t *testing.T) {
	if os.Getenv("GO_WANT_XTUNNEL_AGENT_HELPER_PROCESS") == "1" {
		for index, arg := range os.Args {
			if arg == "--" {
				os.Exit(Execute("xtunnel-agent", os.Args[index+1:], os.Environ(), os.Stdout, os.Stderr))
			}
		}
		os.Exit(2)
	}

	const token = "xta_process_secret"
	args := []string{
		"-test.run=^TestProcessExitsOnSIGTERM$", "--",
		"run", "--token", token,
	}
	command := exec.Command(os.Args[0], args...)
	command.Env = append(os.Environ(), "GO_WANT_XTUNNEL_AGENT_HELPER_PROCESS=1")
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
	if strings.Contains(output.String(), token) {
		t.Fatal("process output leaked Token")
	}
}
