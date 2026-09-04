package secretcheck

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestWindowsServerBinaryClassification(t *testing.T) {
	const public = "xta_sha1PortbitsTypePref"
	cases := []struct {
		name, content string
		wantErr       bool
	}{
		{"exact", public, false},
		{"delimited", "\x00" + public + "\x00", false},
		{"two public matches", public + " " + public, false},
		{"changed character", strings.Replace(public, "sha1", "sha2", 1), true},
		{"longer", public + "A", true},
		{"second token", public + " xta_" + strings.Repeat("z", 20), true},
		{"authorization", public + " Authorization: Bearer credential", true},
		{"cookie", public + " Cookie: credential", true},
		{"json", public + ` {"password":"credential"}`, true},
		{"private key", public + " -----BEGIN PRIVATE KEY-----", true},
	}
	for _, suffix := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-" {
		cases = append(cases, struct {
			name, content string
			wantErr       bool
		}{"suffix " + string(suffix), public + string(suffix), true})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			for _, short := range []bool{false, true} {
				var reader io.Reader = strings.NewReader(test.content)
				if short {
					reader = iotest.OneByteReader(reader)
				}
				if err := WindowsServerBinaryReader("explicit Server purpose", reader); (err != nil) != test.wantErr {
					t.Fatalf("short=%t: error=%v, wantErr=%t", short, err, test.wantErr)
				}
			}
		})
	}
	for _, offset := range []int{0, 1} {
		encoded := make([]byte, offset)
		for _, b := range []byte(public) {
			encoded = append(encoded, b, 0)
		}
		if err := WindowsServerBinaryReader("Server", bytes.NewReader(encoded)); err == nil {
			t.Fatalf("accepted UTF16 alignment %d", offset)
		}
	}
	if err := Bytes("xtunnel-server-windows-amd64.exe", []byte(public)); err == nil {
		t.Fatal("generic Bytes accepted public shape")
	}
	if err := Reader("xtunnel-server-windows-amd64.exe", strings.NewReader(public)); err == nil {
		t.Fatal("generic Reader inferred purpose from filename")
	}
}

func TestWindowsServerBinaryStreamingBoundaries(t *testing.T) {
	const public = "xta_sha1PortbitsTypePref"
	// 覆盖公开匹配结束恰在窗口末尾，后续字符必须由 overlap 重扫后拒绝。
	for split := 1; split <= len(public); split++ {
		prefix := strings.Repeat(" ", scanChunkBytes-split)
		for _, suffix := range []string{"", " ", "A"} {
			content := prefix + public + suffix
			if err := WindowsServerBinaryReader("Server", strings.NewReader(content)); (err != nil) != (suffix == "A") {
				t.Fatalf("split=%d suffix length=%d: %v", split, len(suffix), err)
			}
		}
	}
	// Reader 可以在最后一批数据同时返回 EOF；扫描必须先于 EOF 成功返回。
	for _, suffix := range []string{"", "A"} {
		reader := iotest.DataErrReader(strings.NewReader(public + suffix))
		if err := WindowsServerBinaryReader("Server", reader); (err != nil) != (suffix == "A") {
			t.Fatalf("data+EOF: %v", err)
		}
	}
	errRead := errors.New("read failure")
	if err := WindowsServerBinaryReader("Server", iotest.ErrReader(errRead)); !errors.Is(err, errRead) {
		t.Fatalf("lost reader error: %v", err)
	}
}
