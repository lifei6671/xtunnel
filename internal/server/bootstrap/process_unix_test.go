//go:build !windows

package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
						// ExtraFiles 的首个文件固定映射为 fd 3。只有完成 signal handler
						// 注册并进入真实启动路径后才通知父进程，避免用固定 sleep 猜测时序。
						ready := os.NewFile(3, "sigterm-ready")
						if ready == nil {
							return nil, errors.New("open SIGTERM readiness pipe")
						}
						if _, err := ready.Write([]byte{1}); err != nil {
							_ = ready.Close()
							return nil, fmt.Errorf("write SIGTERM readiness: %w", err)
						}
						if err := ready.Close(); err != nil {
							return nil, fmt.Errorf("close SIGTERM readiness: %w", err)
						}
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
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	t.Cleanup(func() { _ = readyReader.Close() })
	command.ExtraFiles = []*os.File{readyWriter}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		_ = readyWriter.Close()
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
	if err := readyWriter.Close(); err != nil {
		t.Fatalf("close parent readiness writer error = %v", err)
	}

	ready := make(chan error, 1)
	go func() {
		var marker [1]byte
		_, err := io.ReadFull(readyReader, marker[:])
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			// 子进程仍可能写入 command 的输出管道；交由 Cleanup 完成
			// Kill+Wait 后再释放，避免失败路径并发读取 bytes.Buffer。
			t.Fatalf("wait for process readiness error = %v", err)
		}
	case err := <-done:
		exited = true
		t.Fatalf("process exited before readiness: %v\n%s", err, output.String())
	case <-time.After(2 * time.Second):
		t.Fatal("process did not report SIGTERM readiness")
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
