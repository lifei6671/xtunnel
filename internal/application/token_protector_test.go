package application

import (
	"bytes"
	"errors"
	"testing"
)

func TestAES256GCMTokenProtectorRoundTripAndRandomNonce(t *testing.T) {
	protector, err := NewAES256GCMTokenProtector(bytes.Repeat([]byte{0x31}, aes256KeyBytes))
	if err != nil {
		t.Fatalf("NewAES256GCMTokenProtector() error = %v", err)
	}
	context := TokenProtectionContext{
		TunnelID: "tun_01J00000000000000000000000",
		TokenID:  "tok_01J00000000000000000000000",
		Version:  1,
	}
	plaintext := []byte("xta_test-connection-token")
	first, err := protector.Seal(plaintext, context)
	if err != nil {
		t.Fatalf("first Seal() error = %v", err)
	}
	second, err := protector.Seal(plaintext, context)
	if err != nil {
		t.Fatalf("second Seal() error = %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("Seal() reused nonce for identical plaintext")
	}
	opened, err := protector.Open(first, context)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("Open() = %q, want original plaintext", opened)
	}
}

func TestAES256GCMTokenProtectorRejectsTamperingAndRowReplacement(t *testing.T) {
	protector, err := NewAES256GCMTokenProtector(bytes.Repeat([]byte{0x42}, aes256KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	original := TokenProtectionContext{
		TunnelID: "tun_01J00000000000000000000000",
		TokenID:  "tok_01J00000000000000000000000",
		Version:  1,
	}
	ciphertext, err := protector.Seal([]byte("xta_sensitive-token"), original)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		ciphertext []byte
		context    TokenProtectionContext
	}{
		{name: "密文篡改", ciphertext: tamperedCopy(ciphertext), context: original},
		{name: "跨 Tunnel 替换", ciphertext: ciphertext, context: TokenProtectionContext{TunnelID: "tun_01J00000000000000000000001", TokenID: original.TokenID, Version: 1}},
		{name: "跨 Token 替换", ciphertext: ciphertext, context: TokenProtectionContext{TunnelID: original.TunnelID, TokenID: "tok_01J00000000000000000000001", Version: 1}},
		{name: "跨版本替换", ciphertext: ciphertext, context: TokenProtectionContext{TunnelID: original.TunnelID, TokenID: original.TokenID, Version: 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := protector.Open(test.ciphertext, test.context); !errors.Is(err, ErrTokenProtection) {
				t.Fatalf("Open() error = %v, want ErrTokenProtection", err)
			}
		})
	}
}

func TestAES256GCMTokenProtectorRejectsInvalidInput(t *testing.T) {
	if _, err := NewAES256GCMTokenProtector(make([]byte, aes256KeyBytes-1)); !errors.Is(err, ErrTokenProtectorKey) {
		t.Fatalf("short key error = %v, want ErrTokenProtectorKey", err)
	}
	protector, err := NewAES256GCMTokenProtector(make([]byte, aes256KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protector.Seal(nil, TokenProtectionContext{}); !errors.Is(err, ErrTokenProtection) {
		t.Fatalf("Seal(nil) error = %v, want ErrTokenProtection", err)
	}
	if _, err := protector.Open([]byte("short"), TokenProtectionContext{}); !errors.Is(err, ErrTokenProtection) {
		t.Fatalf("Open(short) error = %v, want ErrTokenProtection", err)
	}
}

func tamperedCopy(ciphertext []byte) []byte {
	result := append([]byte(nil), ciphertext...)
	result[len(result)-1] ^= 0x01
	return result
}
