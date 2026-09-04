//go:build windows

package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestReadAdminPasswordWindowsRejectsNonConsole(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pipe, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	defer writer.Close()
	for name, input := range map[string]*os.File{"file": file, "pipe": pipe} {
		t.Run(name, func(t *testing.T) {
			password, err := readAdminPasswordFromTTY(input, io.Discard)
			if err == nil || password != "" {
				t.Fatal("non-console input must fail without returning a password")
			}
		})
	}
}

func TestReadAdminPasswordWindowsConsole(t *testing.T) {
	const childMarker = "XTUNNEL_TEST_ADMIN_PASSWORD_CONSOLE"
	if os.Getenv(childMarker) == "1" {
		testReadAdminPasswordWindowsConsole(t)
		return
	}
	// 使用独立隐藏 Console，输入事件和 mode 变更不会进入用户的终端。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadAdminPasswordWindowsConsole$", "-test.timeout=25s", "-test.v")
	cmd.Env = append(os.Environ(), childMarker+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE, HideWindow: true}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated native console test: %v\n%s", err, output)
	}
}

func testReadAdminPasswordWindowsConsole(t *testing.T) {
	input, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	handle := windows.Handle(input.Fd())
	var initialMode uint32
	if err := windows.GetConsoleMode(handle, &initialMode); err != nil {
		t.Fatal(err)
	}
	// 显式启用回显作为前置状态，确保测试确实验证关闭回显及恢复。
	initialMode |= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
	if err := windows.SetConsoleMode(handle, initialMode); err != nil {
		t.Fatal(err)
	}
	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		t.Fatal(err)
	}
	generatedPassword := hex.EncodeToString(randomBytes)
	promptFailure := errors.New("test prompt failure")
	for _, tc := range []struct {
		name     string
		password string
		writeErr error
		wantErr  string
	}{
		{name: "password", password: generatedPassword},
		{name: "unicode", password: generatedPassword + "中文\U0001F600"},
		{name: "empty", wantErr: "must not be empty"},
		{name: "prompt failure", writeErr: promptFailure, wantErr: "write password prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := adminPasswordConsoleScreen(t, windows.Handle(output.Fd()))
			prompt := &adminPasswordConsolePrompt{t: t, handle: handle, input: tc.password + "\r", err: tc.writeErr}
			password, err := readAdminPasswordFromTTY(input, prompt)
			if tc.wantErr == "" {
				if err != nil || password != tc.password {
					t.Fatalf("console password read failed or returned different bytes: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) || password != "" {
				t.Fatalf("expected %q error and no password, got error: %v", tc.wantErr, err)
			}
			if tc.writeErr != nil && !errors.Is(err, tc.writeErr) {
				t.Fatal("prompt error chain was not preserved")
			}
			if !prompt.called {
				t.Fatal("password prompt was not written")
			}
			var restoredMode uint32
			if err := windows.GetConsoleMode(handle, &restoredMode); err != nil {
				t.Fatal(err)
			}
			if restoredMode != initialMode {
				t.Fatalf("console mode not restored: got %#x, want %#x", restoredMode, initialMode)
			}
			after := adminPasswordConsoleScreen(t, windows.Handle(output.Fd()))
			if after != before {
				t.Fatal("console screen content changed despite password echo being disabled")
			}
		})
	}
}

func adminPasswordConsoleScreen(t *testing.T, handle windows.Handle) string {
	t.Helper()
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(handle, &info); err != nil {
		t.Fatal(err)
	}
	chars := make([]uint16, int(info.Size.X)*int(info.Size.Y))
	var read uint32
	readScreen := windows.NewLazySystemDLL("kernel32.dll").NewProc("ReadConsoleOutputCharacterW")
	ok, _, err := readScreen.Call(uintptr(handle), uintptr(unsafe.Pointer(&chars[0])), uintptr(len(chars)), 0, uintptr(unsafe.Pointer(&read)))
	if ok == 0 {
		t.Fatalf("read native console screen: %v", err)
	}
	if read != uint32(len(chars)) {
		t.Fatal("native console screen was only partially read")
	}
	return string(utf16.Decode(chars))
}

type adminPasswordConsolePrompt struct {
	t      *testing.T
	handle windows.Handle
	input  string
	err    error
	called bool
}

func (p *adminPasswordConsolePrompt) Write(data []byte) (int, error) {
	if string(data) != "Admin password: " {
		return len(data), nil
	}
	p.called = true
	var mode uint32
	if err := windows.GetConsoleMode(p.handle, &mode); err != nil {
		p.t.Fatal(err)
	}
	if mode&windows.ENABLE_ECHO_INPUT != 0 {
		p.t.Fatal("console echo remains enabled when prompting for password")
	}
	if p.err != nil {
		return 0, p.err
	}
	// INPUT_RECORD 的 KEY_EVENT 布局固定为 20 字节；仅测试绑定缺少的系统 API。
	type keyInputRecord struct {
		eventType uint16
		padding   uint16
		keyDown   int32
		repeat    uint16
		keyCode   uint16
		scanCode  uint16
		char      uint16
		control   uint32
	}
	chars := utf16.Encode([]rune(p.input))
	records := make([]keyInputRecord, len(chars))
	for i, char := range chars {
		records[i] = keyInputRecord{eventType: 1, keyDown: 1, repeat: 1, char: char}
		if char == '\r' {
			records[i].keyCode = 0x0d
		}
	}
	var written uint32
	writeInput := windows.NewLazySystemDLL("kernel32.dll").NewProc("WriteConsoleInputW")
	ok, _, err := writeInput.Call(uintptr(p.handle), uintptr(unsafe.Pointer(&records[0])), uintptr(len(records)), uintptr(unsafe.Pointer(&written)))
	if ok == 0 {
		p.t.Fatalf("write native console input: %v", err)
	}
	if written != uint32(len(records)) {
		p.t.Fatal("native console input was only partially written")
	}
	return len(data), nil
}
