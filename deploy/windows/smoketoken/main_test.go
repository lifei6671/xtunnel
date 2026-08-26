package main

import (
	"testing"

	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
)

func TestNewSmokeTokenUsesProductionConnectionTokenContract(t *testing.T) {
	first, err := newSmokeToken()
	if err != nil {
		t.Fatalf("newSmokeToken() error = %v", err)
	}
	second, err := newSmokeToken()
	if err != nil {
		t.Fatalf("second newSmokeToken() error = %v", err)
	}
	if first == second {
		t.Fatal("两次 Smoke Token 生成结果相同，未使用独立随机 Secret")
	}

	parsed, err := connectiontoken.Parse(first)
	if err != nil {
		t.Fatalf("生产 Parse 拒绝 Smoke Token: %v", err)
	}
	if parsed.GetEndpoint().GetHost() != "127.0.0.1" || parsed.GetEndpoint().GetPort() != 1 {
		t.Fatalf("Smoke Endpoint = %#v，want 127.0.0.1:1", parsed.GetEndpoint())
	}
	if parsed.GetTlsTrust().GetPublicCa() == nil {
		t.Fatal("Smoke Token 未使用 Public CA 信任描述")
	}
}
