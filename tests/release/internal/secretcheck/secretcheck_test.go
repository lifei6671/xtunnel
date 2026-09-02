package secretcheck

import (
	"bytes"
	"testing"
)

func TestBytes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "ordinary field names", content: "token password cookie private_key"},
		{name: "short placeholder", content: "xta_example"},
		{name: "connection token", content: "xta_0123456789abcdefghijklmnop", wantErr: true},
		{name: "bearer authorization", content: "Authorization: Bearer credential", wantErr: true},
		{name: "basic authorization", content: "Authorization: Basic credential", wantErr: true},
		{name: "cookie header", content: "Cookie: session-value", wantErr: true},
		{name: "JSON password", content: `{"password":"credential"}`, wantErr: true},
		{name: "PEM private key", content: "-----BEGIN PRIVATE KEY-----", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Bytes("fixture", []byte(test.content))
			if (err != nil) != test.wantErr {
				t.Fatalf("Bytes() error = %v, want error %t", err, test.wantErr)
			}
		})
	}
	t.Run("UTF-16LE registry text", func(t *testing.T) {
		plain := []byte("Authorization: Basic credential")
		encoded := make([]byte, 0, len(plain)*2+2)
		encoded = append(encoded, 0xff, 0xfe)
		for _, value := range plain {
			encoded = append(encoded, value, 0)
		}
		if err := Bytes("fixture", encoded); err == nil {
			t.Fatal("Bytes() accepted a UTF-16LE credential")
		}
	})
	t.Run("stream boundary", func(t *testing.T) {
		content := append(bytes.Repeat([]byte{'x'}, scanChunkBytes-8), []byte("Authorization: Bearer credential")...)
		if err := Reader("fixture", bytes.NewReader(content)); err == nil {
			t.Fatal("Reader() accepted a credential split across chunks")
		}
	})
}
