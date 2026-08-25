// Package token 实现 Protocol v1 Connection Token 的冻结文本编码与严格解析。
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"google.golang.org/protobuf/proto"
)

const (
	// Prefix 是用户唯一可见的 Connection Token 固定前缀。
	Prefix = "xta_"

	// FormatVersionV1 是当前冻结的 Connection Token 编码版本。
	FormatVersionV1 uint32 = 1

	// IntegrityDomain 将 Token 完整性标签与其他 HMAC 输入隔离。
	IntegrityDomain = "xtunnel-connection-token-v1"
)

var (
	// ErrMalformed 表示文本编码、Protobuf 结构或字段语义不符合冻结契约。
	ErrMalformed = errors.New("connection token is malformed")

	// ErrIntegrity 表示 Token 的完整性标签无法通过常量时间校验。
	ErrIntegrity = errors.New("connection token integrity check failed")
)

// Encode 验证 Token 语义、重建完整性标签，并返回唯一规范的 xta_ 文本形式。
func Encode(connectionToken *protocolv1.ConnectionToken) (string, error) {
	if err := validateSemantics(connectionToken, false); err != nil {
		return "", err
	}

	copyToken := proto.Clone(connectionToken).(*protocolv1.ConnectionToken)
	copyToken.IntegrityTag = nil
	tag, err := integrityTag(copyToken)
	if err != nil {
		return "", err
	}
	copyToken.IntegrityTag = tag

	encoded, err := marshalDeterministic(copyToken)
	if err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(encoded), nil
}

// Parse 只接受规范的 xta_ 无填充 Base64URL 表示，并在任何 TLS 拨号前完成完整性与语义校验。
func Parse(text string) (*protocolv1.ConnectionToken, error) {
	if !strings.HasPrefix(text, Prefix) || len(text) == len(Prefix) {
		return nil, ErrMalformed
	}
	encodedText := strings.TrimPrefix(text, Prefix)
	if strings.ContainsAny(encodedText, "= \t\r\n") {
		return nil, ErrMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(encodedText)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encodedText {
		return nil, ErrMalformed
	}

	connectionToken := &protocolv1.ConnectionToken{}
	if err := proto.Unmarshal(raw, connectionToken); err != nil {
		return nil, fmt.Errorf("%w: protobuf", ErrMalformed)
	}
	if err := validateSemantics(connectionToken, true); err != nil {
		return nil, err
	}
	canonical, err := marshalDeterministic(connectionToken)
	if err != nil || !hmac.Equal(raw, canonical) {
		return nil, ErrMalformed
	}

	tag := append([]byte(nil), connectionToken.IntegrityTag...)
	connectionToken.IntegrityTag = nil
	wantTag, err := integrityTag(connectionToken)
	connectionToken.IntegrityTag = tag
	if err != nil {
		return nil, err
	}
	if !hmac.Equal(tag, wantTag) {
		return nil, ErrIntegrity
	}
	return connectionToken, nil
}

func integrityTag(connectionToken *protocolv1.ConnectionToken) ([]byte, error) {
	payload, err := marshalDeterministic(connectionToken)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, connectionToken.GetAuthenticationSecret())
	_, _ = mac.Write([]byte(IntegrityDomain))
	_, _ = mac.Write(payload)
	return mac.Sum(nil), nil
}

func marshalDeterministic(message proto.Message) ([]byte, error) {
	if err := validate.RejectUnknownFields(message); err != nil {
		return nil, fmt.Errorf("%w: unknown field", ErrMalformed)
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(message)
}

func validateSemantics(connectionToken *protocolv1.ConnectionToken, requireIntegrityTag bool) error {
	if connectionToken == nil || connectionToken.GetFormatVersion() != FormatVersionV1 ||
		!validate.ValidID(connectionToken.GetAgentId(), "ag_") || !validate.ValidID(connectionToken.GetTokenId(), "tok_") ||
		connectionToken.GetTokenVersion() == 0 || len(connectionToken.GetAuthenticationSecret()) != sha256.Size {
		return ErrMalformed
	}
	if requireIntegrityTag && len(connectionToken.GetIntegrityTag()) != sha256.Size {
		return ErrMalformed
	}
	if endpoint := connectionToken.GetEndpoint(); endpoint == nil || endpoint.GetPort() == 0 || endpoint.GetPort() > 65535 ||
		!validGatewayHost(endpoint.GetHost()) {
		return ErrMalformed
	}

	trust := connectionToken.GetTlsTrust()
	if trust == nil {
		return ErrMalformed
	}
	switch mode := trust.GetMode().(type) {
	case *protocolv1.TlsTrustDescriptor_PublicCa:
		if mode.PublicCa == nil {
			return ErrMalformed
		}
	case *protocolv1.TlsTrustDescriptor_PinnedSpkiSha256:
		if mode.PinnedSpkiSha256 == nil || len(mode.PinnedSpkiSha256.GetSpkiSha256()) != sha256.Size {
			return ErrMalformed
		}
	default:
		return ErrMalformed
	}
	return nil
}

func validGatewayHost(host string) bool {
	if host == "" || host != strings.TrimSpace(host) || strings.ContainsAny(host, "/\\") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return !strings.Contains(host, ":")
}
