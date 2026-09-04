//go:build linux

package bootstrap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func readAdminPasswordFromTTY(input *os.File, stderr io.Writer) (password string, resultErr error) {
	info, err := input.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect password input: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return "", errors.New("admin password requires --password-file or an interactive TTY")
	}
	termios, err := unix.IoctlGetTermios(int(input.Fd()), unix.TCGETS)
	if err != nil {
		return "", fmt.Errorf("read TTY settings: %w", err)
	}
	withoutEcho := *termios
	withoutEcho.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(int(input.Fd()), unix.TCSETS, &withoutEcho); err != nil {
		return "", fmt.Errorf("disable TTY echo: %w", err)
	}
	defer func() {
		// 恢复失败时不能继续创建管理员；同时保留读取或提示输出的原始错误链。
		if err := unix.IoctlSetTermios(int(input.Fd()), unix.TCSETS, termios); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore TTY settings: %w", err))
		}
		if _, err := fmt.Fprintln(stderr); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("finish password prompt: %w", err))
		}
		if resultErr != nil {
			password = ""
		}
	}()

	if _, err := fmt.Fprint(stderr, "Admin password: "); err != nil {
		return "", fmt.Errorf("write password prompt: %w", err)
	}
	password, err = bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password from TTY: %w", err)
	}
	password = strings.TrimSuffix(password, "\n")
	password = strings.TrimSuffix(password, "\r")
	if password == "" {
		return "", errors.New("admin password must not be empty")
	}
	return password, nil
}
