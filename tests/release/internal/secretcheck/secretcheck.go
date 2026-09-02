// Package secretcheck owns the release-artifact secret-shape rules.
package secretcheck

import (
	"fmt"
	"io"
	"regexp"
)

const (
	scanChunkBytes = 64 * 1024
	scanOverlap    = 16 * 1024
)

var forbiddenShapes = []*regexp.Regexp{
	regexp.MustCompile(`xta_[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`(?i)authorization:[[:space:]]*(?:bearer|basic)[[:space:]]+[^[:space:]]+`),
	regexp.MustCompile(`(?i)(?:cookie|set-cookie):[[:space:]]*[^[:space:];]+`),
	regexp.MustCompile(`(?i)"(?:token|password|cookie|session_secret|private_key)"[[:space:]]*:[[:space:]]*"[^"[:space:]]+"`),
	regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----`),
}

// Bytes 拒绝发布面中可能承载真实凭据的稳定形状。普通的 token、password、cookie
// 单词会合法存在于错误码和帮助文本，不能仅凭字段名把发布二进制误判为泄漏。
func Bytes(source string, content []byte) error {
	for _, candidate := range [][]byte{content, collapseUTF16LE(content, 0), collapseUTF16LE(content, 1)} {
		for _, pattern := range forbiddenShapes {
			if pattern.Match(candidate) {
				return fmt.Errorf("%s contains forbidden secret shape rule %q", source, pattern.String())
			}
		}
	}
	return nil
}

func collapseUTF16LE(content []byte, offset int) []byte {
	result := make([]byte, 0, len(content)/2)
	for index := offset; index+1 < len(content); index += 2 {
		if content[index+1] == 0 && content[index] <= 0x7f {
			result = append(result, content[index])
		} else {
			result = append(result, ' ')
		}
	}
	return result
}

// Reader 以固定窗口扫描大 Binary、OCI Layer 和日志，避免把整个发布产物读入内存。
// 窗口保留 16 KiB 重叠；Connection Token 上限为 8192 bytes，其他固定头更短。
func Reader(source string, reader io.Reader) error {
	buffer := make([]byte, scanChunkBytes)
	tail := make([]byte, 0, scanOverlap)
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			window := make([]byte, 0, len(tail)+read)
			window = append(window, tail...)
			window = append(window, buffer[:read]...)
			if scanErr := Bytes(source, window); scanErr != nil {
				return scanErr
			}
			keep := min(len(window), scanOverlap)
			tail = append(tail[:0], window[len(window)-keep:]...)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("scan %s: %w", source, err)
		}
	}
}
