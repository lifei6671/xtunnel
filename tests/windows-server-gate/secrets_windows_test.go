//go:build windows

package windowsservergate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

// 输出只留在有界内存。复制进程输出的 goroutine 共享同一锁；上限溢出记为失败，
// 不允许丢弃未检查日志后仍产出产品通过报告。
type boundedOutput struct {
	mu       sync.Mutex
	data     []byte
	overflow bool
}

const outputLimit = 16 << 20

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := outputLimit - len(b.data)
	if len(p) > remaining {
		b.overflow = true
		b.data = append(b.data, p[:remaining]...)
	} else {
		b.data = append(b.data, p...)
	}
	return len(p), nil
}
func (b *boundedOutput) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data), b.overflow
}

type secretAudit struct {
	output boundedOutput
	values []string
}

// 文本表面使用与候选 Artifact 检查一致的已冻结形状；Binary 仅检查本轮精确值，
// 其通用字节形状由 Artifact 验收负责，避免把编译进程序的正则常量当成凭据。
var secretTextPatterns = []*regexp.Regexp{
	regexp.MustCompile(`xta_[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`(?i)authorization:[[:space:]]*(?:bearer|basic)[[:space:]]+[^[:space:]]+`),
	regexp.MustCompile(`(?i)(?:cookie|set-cookie):[[:space:]]*[^[:space:];]+`),
	regexp.MustCompile(`(?i)"(?:token|password|cookie|session_secret|private_key)"[[:space:]]*:[[:space:]]*"[^"[:space:]]+"`),
	regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----`),
}

func wideBytes(value string) []byte {
	units := utf16.Encode([]rune(value))
	wide := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(wide[2*i:], u)
	}
	return wide
}
func containsSecret(data []byte, values []string, text bool) bool {
	for _, value := range values {
		if value != "" && (bytes.Contains(data, []byte(value)) || bytes.Contains(data, wideBytes(value))) {
			return true
		}
	}
	if text {
		for _, pattern := range secretTextPatterns {
			if pattern.Match(data) {
				return true
			}
		}
		// SCM 注册值使用 UTF-16LE；同时检查两种字节对齐，保持与 Artifact 形状检查一致。
		for offset := 0; offset < 2; offset++ {
			var decoded []uint16
			for i := offset; i+1 < len(data); i += 2 {
				decoded = append(decoded, binary.LittleEndian.Uint16(data[i:]))
			}
			decodedBytes := []byte(string(utf16.Decode(decoded)))
			for _, pattern := range secretTextPatterns {
				if pattern.Match(decodedBytes) {
					return true
				}
			}
		}
	}
	return false
}
func (a *secretAudit) scan(t *testing.T, surface string, data []byte, text bool) {
	t.Helper()
	if containsSecret(data, a.values, text) {
		t.Errorf("secret leak detected in %s", surface)
	}
}
func (a *secretAudit) loadMasterKey(t *testing.T, root string) {
	data, err := winsecurity.ReadForegroundFile(filepath.Join(root, "data", "credentials"), "tunnel-token.key")
	must(t, err, "read owned master key for leak checks")
	a.values = append(a.values, string(data), hex.EncodeToString(data), base64.StdEncoding.EncodeToString(data), base64.RawStdEncoding.EncodeToString(data))
	clear(data)
}
func (a *secretAudit) scanFile(t *testing.T, surface, path string, text bool) {
	t.Helper()
	f, err := os.Open(path)
	must(t, err, "open secret scan target")
	data, readErr := io.ReadAll(io.LimitReader(f, 256<<20))
	closeErr := f.Close()
	must(t, readErr, "read secret scan target")
	must(t, closeErr, "close secret scan target")
	if len(data) == 256<<20 {
		t.Fatal("secret scan file limit exceeded")
	}
	a.scan(t, surface, data, text)
	clear(data)
}
func (a *secretAudit) scanRegistry(t *testing.T, path string) {
	t.Helper()
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.READ)
	must(t, err, "open SCM registry scan")
	defer func() {
		if err := key.Close(); err != nil {
			t.Error("close registry scan", err)
		}
	}()
	names, err := key.ReadValueNames(-1)
	must(t, err, "enumerate SCM values")
	for _, name := range names {
		size, _, err := key.GetValue(name, nil)
		must(t, err, "size SCM value")
		if size > 1<<20 {
			t.Fatal("SCM registry value limit exceeded")
		}
		data := make([]byte, size)
		_, _, err = key.GetValue(name, data)
		must(t, err, "read SCM value")
		a.scan(t, "SCM registry", data, true)
		clear(data)
	}
	children, err := key.ReadSubKeyNames(-1)
	must(t, err, "enumerate SCM subkeys")
	for _, child := range children {
		a.scanRegistry(t, path+`\`+child)
	}
}
func (a *secretAudit) scanEvents(t *testing.T, began time.Time) {
	t.Helper()
	// Provider 和 UTC 起点均由验收 owner 确定；输出仅进入内存，不提交事件内容。
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'; $events=@(Get-WinEvent -FilterHashtable @{LogName='Application';ProviderName='XTunnelServer';StartTime=[DateTime]::Parse('%s').ToUniversalTime()} -ErrorAction SilentlyContinue); if ($events.Count -eq 0) { throw 'No Server events for secret scan' }; $events | ForEach-Object { $_.Properties | ForEach-Object { [string]$_.Value } } | ConvertTo-Json -Compress`, began.UTC().Format(time.RFC3339Nano))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	command.WaitDelay = 2 * time.Second
	var capture boundedOutput
	command.Stdout = &capture
	command.Stderr = &capture
	if command.Run() != nil {
		t.Fatal("Server Event Log secret scan failed")
	}
	data, overflow := capture.snapshot()
	if overflow {
		t.Fatal("Server Event Log scan limit exceeded")
	}
	a.scan(t, "Server Event Log", data, true)
	clear(data)
}

func TestBoundedSecretCapture(t *testing.T) {
	var capture boundedOutput
	payload := bytes.Repeat([]byte{'x'}, outputLimit+1)
	n, err := capture.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatal("capture must drain producer")
	}
	data, overflow := capture.snapshot()
	if !overflow || len(data) != outputLimit {
		t.Fatal("capture must mark overflow")
	}
}

func TestSecretSurfaceDetection(t *testing.T) {
	value := "test-only-exact-sensitive-value"
	for _, test := range []struct {
		name string
		data []byte
		want bool
	}{
		{"ASCII exact", []byte("prefix " + value + " suffix"), true},
		{"UTF16 exact", wideBytes(value), true},
		{"PEM shape", []byte("-----BEGIN PRIVATE KEY-----"), true},
		{"UTF16 PEM", wideBytes("-----BEGIN RSA PRIVATE KEY-----"), true},
		{"Bearer shape", []byte("Authorization: Bearer opaque-test-value"), true},
		{"clean", []byte(`{"event":"process_started","status":"ok"}`), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if containsSecret(test.data, []string{value}, true) != test.want {
				t.Fatal("secret classification mismatch")
			}
		})
	}
}
