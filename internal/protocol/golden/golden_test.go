package golden

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/token"
)

func TestProtocolV1GoldenVectors(t *testing.T) {
	tokenText, err := token.Encode(goldenToken())
	if err != nil {
		t.Fatalf("编码 Token 失败: %v", err)
	}
	assertGoldenText(t, "connection-token-v1.txt", tokenText)
	secondaryToken := goldenToken()
	secondaryToken.TokenId = "tok_01J00000000000000000000001"
	secondaryToken.AuthenticationSecret = bytes32(0x12)
	secondaryTokenText, err := token.Encode(secondaryToken)
	if err != nil {
		t.Fatalf("编码第二个部署 Smoke Token 失败: %v", err)
	}
	assertGoldenText(t, "connection-token-v1-secondary.txt", secondaryTokenText)

	sessionSecret := bytes32(0x11)
	assertGoldenText(t, "work-hello-session-secret-v1.hex", hex.EncodeToString(sessionSecret))
	workHello := &protocolv1.WorkHello{
		TunnelId:      "tun_01J00000000000000000000000",
		ConnectorId:   "con_01J00000000000000000000000",
		SessionId:     "sess_01J00000000000000000000000",
		WorkId:        "work_01J00000000000000000000000",
		BudgetLeaseId: "lease_01J00000000000000000000000",
		Nonce:         bytes32(0x42),
	}
	workPayload, err := deterministic.WorkHelloBytesWithoutMAC(workHello)
	if err != nil {
		t.Fatalf("编码无 MAC WorkHello 失败: %v", err)
	}
	assertGoldenText(t, "work-hello-without-mac-v1.hex", hex.EncodeToString(workPayload))
	hmacInput := append([]byte(deterministic.WorkMACDomain), workPayload...)
	assertGoldenText(t, "work-hello-hmac-input-v1.hex", hex.EncodeToString(hmacInput))
	mac, err := deterministic.ComputeWorkHelloMAC(sessionSecret, workHello)
	if err != nil {
		t.Fatalf("计算 WorkHello MAC 失败: %v", err)
	}
	assertGoldenText(t, "work-hello-mac-v1.hex", hex.EncodeToString(mac))
	workHello.Mac = mac
	workBytes, err := deterministic.Marshal(workHello)
	if err != nil {
		t.Fatalf("编码 WorkHello 失败: %v", err)
	}
	assertGoldenText(t, "work-hello-v1.hex", hex.EncodeToString(workBytes))

	snapshot := &protocolv1.TunnelSnapshot{
		TunnelId: "tun_01J00000000000000000000000",
		Revision: 7,
		Services: []*protocolv1.ServiceConfig{
			{
				ServiceId: "svc_01J00000000000000000000001", OriginScheme: "http", OriginHost: "127.0.0.1",
				OriginPort: 8080, Enabled: true, RequiredRevision: 7,
				OriginConnectionOptions: &protocolv1.OriginConnectionOptions{TcpKeepaliveIntervalMs: 30_000},
				HttpProxyOptions: &protocolv1.HTTPProxyOptions{
					IdleConnectionTimeoutMs: 90_000,
					MaxIdleConnections:      100,
				},
			},
			{
				ServiceId: "svc_01J00000000000000000000000", OriginScheme: "tcp", OriginHost: "::1",
				OriginPort: 22, Enabled: true, RequiredRevision: 7,
				OriginConnectionOptions: &protocolv1.OriginConnectionOptions{DisableHappyEyeballs: true},
			},
		},
	}
	snapshotBytes, err := deterministic.MarshalSnapshot(snapshot)
	if err != nil {
		t.Fatalf("编码 Snapshot 失败: %v", err)
	}
	assertGoldenText(t, "snapshot-v1.hex", hex.EncodeToString(snapshotBytes))
	snapshotHash := sha256.Sum256(snapshotBytes)
	assertGoldenText(t, "snapshot-v1.sha256", hex.EncodeToString(snapshotHash[:]))
	assertGoldenText(t, "snapshot-v1.size", strconv.Itoa(len(snapshotBytes)))

	// ConfigAck 的 Revision 必须与固定字节关联，防止后续字段调整让确认消息
	// 意外丢失已观测版本号。
	configAck := &protocolv1.ConfigAck{
		ObservedRevision: 7,
		ApplyStatus:      protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED,
	}
	configAckBytes, err := deterministic.Marshal(configAck)
	if err != nil {
		t.Fatalf("编码 ConfigAck 失败: %v", err)
	}
	assertGoldenText(t, "config-ack-v1.hex", hex.EncodeToString(configAckBytes))
}

// assertGoldenText 只比较已经人工冻结的 Fixture，测试绝不改写 Golden 文件。
func assertGoldenText(t *testing.T, name, actual string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "tests", "golden", "protocol-v1", name)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 Golden %s 失败: %v", name, err)
	}
	if want := strings.TrimSpace(string(content)); actual != want {
		t.Fatalf("Golden %s 不匹配:\nactual=%s\nwant=%s", name, actual, want)
	}
}

func goldenToken() *protocolv1.ConnectionToken {
	return &protocolv1.ConnectionToken{
		FormatVersion: token.FormatVersionV1,
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
