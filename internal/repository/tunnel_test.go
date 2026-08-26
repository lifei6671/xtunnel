package repository

import (
	"errors"
	"testing"
)

const (
	testTunnelID = "tun_01J00000000000000000000000"
	testTokenID  = "tok_01J00000000000000000000000"
)

func TestTunnelValidate(t *testing.T) {
	valid := Tunnel{ID: testTunnelID, Name: "office", Version: 1, CreatedAt: 1, UpdatedAt: 1}
	tests := []struct {
		name   string
		mutate func(*Tunnel)
	}{
		{name: "合法", mutate: func(*Tunnel) {}},
		{name: "错误 ID", mutate: func(tunnel *Tunnel) { tunnel.ID = "tun_invalid" }},
		{name: "ID 首字符超出 ULID 范围", mutate: func(tunnel *Tunnel) { tunnel.ID = "tun_Z1J00000000000000000000000" }},
		{name: "空白名称", mutate: func(tunnel *Tunnel) { tunnel.Name = " \t" }},
		{name: "无效版本", mutate: func(tunnel *Tunnel) { tunnel.Version = 0 }},
		{name: "负期望版本", mutate: func(tunnel *Tunnel) { tunnel.DesiredRevision = -1 }},
		{name: "无效撤销时间", mutate: func(tunnel *Tunnel) { value := int64(-1); tunnel.RevokedAt = &value }},
		{name: "无效创建时间", mutate: func(tunnel *Tunnel) { tunnel.CreatedAt = 0 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			err := candidate.Validate()
			if test.name == "合法" && err != nil {
				t.Fatalf("Tunnel.Validate() error = %v", err)
			}
			if test.name != "合法" && !errors.Is(err, ErrInvalidTunnel) {
				t.Fatalf("Tunnel.Validate() error = %v, want ErrInvalidTunnel", err)
			}
		})
	}
}

func TestTunnelTokenValidate(t *testing.T) {
	valid := TunnelToken{
		ID:              testTokenID,
		TunnelID:        testTunnelID,
		TokenCiphertext: []byte("encrypted-token-that-is-long-enough"),
		Version:         1,
		Status:          TunnelTokenStatusActive,
		CreatedAt:       1,
	}
	tests := []struct {
		name   string
		mutate func(*TunnelToken)
	}{
		{name: "合法 ACTIVE", mutate: func(*TunnelToken) {}},
		{name: "错误 Token ID", mutate: func(token *TunnelToken) { token.ID = "tok_invalid" }},
		{name: "Token ID 首字符超出 ULID 范围", mutate: func(token *TunnelToken) { token.ID = "tok_Z1J00000000000000000000000" }},
		{name: "错误 Tunnel ID", mutate: func(token *TunnelToken) { token.TunnelID = "tun_invalid" }},
		{name: "空密文", mutate: func(token *TunnelToken) { token.TokenCiphertext = nil }},
		{name: "零代次", mutate: func(token *TunnelToken) { token.Version = 0 }},
		{name: "ACTIVE 有撤销时刻", mutate: func(token *TunnelToken) { value := int64(2); token.RevokedAt = &value }},
		{name: "撤销状态无撤销时刻", mutate: func(token *TunnelToken) { token.Status = TunnelTokenStatusRevoked }},
		{name: "未知状态", mutate: func(token *TunnelToken) { token.Status = "UNKNOWN" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			err := candidate.Validate()
			if test.name == "合法 ACTIVE" && err != nil {
				t.Fatalf("TunnelToken.Validate() error = %v", err)
			}
			if test.name != "合法 ACTIVE" && !errors.Is(err, ErrInvalidTunnelToken) {
				t.Fatalf("TunnelToken.Validate() error = %v, want ErrInvalidTunnelToken", err)
			}
		})
	}
}
