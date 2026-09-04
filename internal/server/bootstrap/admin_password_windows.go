//go:build windows

package bootstrap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func readAdminPasswordFromTTY(input *os.File, stderr io.Writer) (password string, resultErr error) {
	handle := windows.Handle(input.Fd())
	var originalMode uint32
	if err := windows.GetConsoleMode(handle, &originalMode); err != nil {
		return "", fmt.Errorf("admin password requires --password-file or an interactive Windows console: %w", err)
	}
	// 本调用独占控制台输入模式，只关闭密码回显并启用行编辑。os.File.Read
	// 使用 Go 的 ReadConsole UTF-16 解码路径，保留中文密码与控制台退格行为。
	// 无论读取或提示是否失败，都先恢复原模式；恢复失败时不继续创建管理员。
	mode := (originalMode | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT) &^ windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(handle, mode); err != nil {
		return "", fmt.Errorf("disable console password echo: %w", err)
	}
	defer func() {
		if err := windows.SetConsoleMode(handle, originalMode); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore console input mode: %w", err))
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
	password, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password from console: %w", err)
	}
	password = strings.TrimSuffix(password, "\n")
	password = strings.TrimSuffix(password, "\r")
	if password == "" {
		return "", errors.New("admin password must not be empty")
	}
	return password, nil
}
