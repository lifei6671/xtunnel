package token

import (
	"errors"
	"strings"
	"testing"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

func TestEncodeAndParseRoundTrip(t *testing.T) {
	text, err := Encode(testToken())
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if !strings.HasPrefix(text, Prefix) || strings.Contains(text, "=") {
		t.Fatalf("Encode() = %q, want canonical unpadded xta_ text", text)
	}
	parsed, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := parsed.GetEndpoint().GetHost(); got != "gateway.example.test" {
		t.Fatalf("Parse() endpoint host = %q", got)
	}
}

// TestEncodeAndParseIPv6GatewayEndpoint 锁定 GatewayEndpoint 支持 IPv6 字面量。
func TestEncodeAndParseIPv6GatewayEndpoint(t *testing.T) {
	connectionToken := testToken()
	connectionToken.Endpoint.Host = "2001:db8::1"

	text, err := Encode(connectionToken)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	parsed, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := parsed.GetEndpoint().GetHost(); got != "2001:db8::1" {
		t.Fatalf("Parse() endpoint host = %q, want IPv6 literal", got)
	}
}

func TestParseRejectsTamperingBeforeUse(t *testing.T) {
	text, err := Encode(testToken())
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	tampered := text[:len(text)-1] + "A"
	if _, err := Parse(tampered); !errors.Is(err, ErrIntegrity) && !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse() error = %v, want integrity or malformed error", err)
	}
	if _, err := Parse(strings.Replace(text, Prefix, "xta=", 1)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse() prefix error = %v, want ErrMalformed", err)
	}
}

// TestEncodeRejectsLegacyAgentIdentity 锁定 Token 的 Credential 所有者是 Tunnel，
// 防止旧 ag_ 身份被误当成新增 Connector 的独立 Token 身份继续写入线协议。
func TestEncodeRejectsLegacyAgentIdentity(t *testing.T) {
	connectionToken := testToken()
	connectionToken.TunnelId = "ag_01J00000000000000000000000"

	if _, err := Encode(connectionToken); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Encode() error = %v, want ErrMalformed", err)
	}
}

func TestEncodeRejectsGatewayHostWhitespaceAndControlCharacters(t *testing.T) {
	for name, host := range map[string]string{
		"internal whitespace": "gateway .example.test",
		"control character":   "gateway\x00.example.test",
	} {
		t.Run(name, func(t *testing.T) {
			connectionToken := testToken()
			connectionToken.Endpoint.Host = host

			if _, err := Encode(connectionToken); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Encode() error = %v, want ErrMalformed", err)
			}
		})
	}
}

func testToken() *protocolv1.ConnectionToken {
	return &protocolv1.ConnectionToken{
		FormatVersion: FormatVersionV1,
		Endpoint:      &protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 7443},
		TlsTrust: &protocolv1.TlsTrustDescriptor{
			Mode: &protocolv1.TlsTrustDescriptor_PinnedSpkiSha256{
				PinnedSpkiSha256: &protocolv1.PinnedSPKITrust{SpkiSha256: bytes32(0x22)},
			},
		},
		TunnelId:             "tun_01J00000000000000000000000",
		TokenId:              "tok_01J00000000000000000000000",
		TokenVersion:         1,
		AuthenticationSecret: bytes32(0x11),
	}
}

func bytes32(value byte) []byte {
	result := make([]byte, 32)
	for index := range result {
		result[index] = value
	}
	return result
}
