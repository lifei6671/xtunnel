// Package main 为仓库内各平台的部署 Smoke 生成一次性 Connection Token。
//
// Smoke 必须复用生产编码器，不能在 Shell 或 PowerShell 中复制 Protobuf、HMAC
// 或 Base64URL 契约；否则 Protocol 演进后，部署测试会再次生成生产代码必然拒绝的旧格式。
package main

import (
	cryptorand "crypto/rand"
	"fmt"
	"os"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
)

const (
	smokeTunnelID = "tun_01J00000000000000000000000"
	smokeTokenID  = "tok_01J00000000000000000000000"
)

func main() {
	token, err := newSmokeToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "generate deployment smoke Connection Token: %v\n", err)
		os.Exit(1)
	}
	// stdout 只承载机器可读的 Token，调用脚本会直接捕获且不会把它写入日志。
	fmt.Print(token)
}

func newSmokeToken() (string, error) {
	secret := make([]byte, 32)
	if _, err := cryptorand.Read(secret); err != nil {
		return "", fmt.Errorf("generate authentication secret: %w", err)
	}
	return connectiontoken.Encode(&protocolv1.ConnectionToken{
		FormatVersion: connectiontoken.FormatVersionV1,
		// 本机 TCP 1 端口用于制造可重试的连接拒绝。Smoke 验证的是进程与服务
		// 生命周期；无需依赖真实 Server，也不能把永久认证错误伪装成健康运行。
		Endpoint: &protocolv1.GatewayEndpoint{Host: "127.0.0.1", Port: 1},
		TlsTrust: &protocolv1.TlsTrustDescriptor{
			Mode: &protocolv1.TlsTrustDescriptor_PublicCa{
				PublicCa: &protocolv1.PublicCATrust{},
			},
		},
		TunnelId:             smokeTunnelID,
		TokenId:              smokeTokenID,
		TokenVersion:         1,
		AuthenticationSecret: secret,
	})
}
