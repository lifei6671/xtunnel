package identity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	tunnelPrefix     = "tun_"
	connectorPrefix  = "con_"
	sessionPrefix    = "sess_"
	workPrefix       = "work_"
	leasePrefix      = "lease_"
	connectionPrefix = "conn_"
	drainPrefix      = "drain_"
	auditEventPrefix = "evt_"
	operationPrefix  = "op_"
	ulidLength       = 26
	maxULIDMillis    = (1 << 48) - 1

	crockfordBase32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

var (
	// ErrInvalidTunnelID 表示 Tunnel ID 不符合 tun_ 加 ULID 的固定格式。
	ErrInvalidTunnelID = errors.New("tunnel identifier is invalid")

	// ErrInvalidConnectorID 表示 Connector ID 不符合 con_ 加 ULID 的固定格式。
	ErrInvalidConnectorID = errors.New("connector identifier is invalid")

	// ErrInvalidSessionID 表示 Session ID 不符合 sess_ 加 ULID 的固定格式。
	ErrInvalidSessionID = errors.New("session identifier is invalid")

	errTimeOutsideULIDRange = errors.New("identifier time is outside ULID range")
)

// Connector 是 xtunnel-agent 进程启动时生成的一次性内存身份。
//
// 一个 Connector 在同一进程的全部重连间保持不变；新进程必须重新创建 Connector，
// 因此本类型不提供任何持久化、导入或恢复接口。
type Connector struct {
	id string
}

// NewConnector 使用 CSPRNG 生成当前 xtunnel-agent 进程唯一的 Connector。
func NewConnector() (Connector, error) {
	id, err := newID(connectorPrefix, time.Now(), rand.Reader)
	if err != nil {
		return Connector{}, fmt.Errorf("generate connector identifier: %w", err)
	}
	return Connector{id: id}, nil
}

// ID 返回本进程在所有重连中复用的 Connector ID。
func (connector Connector) ID() string {
	return connector.id
}

// NewSessionID 使用 CSPRNG 生成仅属于一次已认证连接的 Session ID。
// 调用方只能在认证已经成功后调用本函数；认证处理器将在 M1-05 接入该边界。
func NewSessionID() (string, error) {
	id, err := newID(sessionPrefix, time.Now(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate session identifier: %w", err)
	}
	return id, nil
}

// NewWorkID 使用与其他运行时身份相同的 ULID CSPRNG 工厂生成一次 WorkConn 身份。
// Work ID 不通过替换 Session ID 前缀拼装；独立入口让类型语义在生成点就保持正确，
// 同时继续复用唯一的 ULID 时间与随机编码实现。
func NewWorkID() (string, error) {
	id, err := newID(workPrefix, time.Now(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate work identifier: %w", err)
	}
	return id, nil
}

// NewLeaseID 为一次 Server WorkDemand Budget Lease 生成独立身份。
func NewLeaseID() (string, error) {
	id, err := newID(leasePrefix, time.Now(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate budget lease identifier: %w", err)
	}
	return id, nil
}

// NewConnectionID 为一次公网业务连接生成贯穿 OPEN 与 ActiveWork 的身份。
func NewConnectionID() (string, error) {
	id, err := newID(connectionPrefix, time.Now(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate connection identifier: %w", err)
	}
	return id, nil
}

// NewDrainID 为一次 Control Session 优雅排空握手生成独立身份。
// 同一个握手只能复用这一个 ID；超时或重试不得重新生成后伪装成新请求。
func NewDrainID() (string, error) {
	id, err := newID(drainPrefix, time.Now(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate drain identifier: %w", err)
	}
	return id, nil
}

// NewAuditEventID 为一条不可变安全审计证据生成独立身份。
func NewAuditEventID() (string, error) {
	id, err := newID(auditEventPrefix, time.Now(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate security audit event identifier: %w", err)
	}
	return id, nil
}

// NewOperationID 为一次安全操作生成跨审计写入重试复用的身份。
func NewOperationID() (string, error) {
	id, err := newID(operationPrefix, time.Now(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate security operation identifier: %w", err)
	}
	return id, nil
}

// ValidateTunnelID 校验 tun_ 前缀和 26 位大写 Crockford ULID。
func ValidateTunnelID(value string) error {
	if !validID(value, tunnelPrefix) {
		return ErrInvalidTunnelID
	}
	return nil
}

// ValidateConnectorID 校验 con_ 前缀和 26 位大写 Crockford ULID。
func ValidateConnectorID(value string) error {
	if !validID(value, connectorPrefix) {
		return ErrInvalidConnectorID
	}
	return nil
}

// ValidateSessionID 校验 sess_ 前缀和 26 位大写 Crockford ULID。
func ValidateSessionID(value string) error {
	if !validID(value, sessionPrefix) {
		return ErrInvalidSessionID
	}
	return nil
}

// ValidTunnelID 返回 value 是否为合法 Tunnel ID。
func ValidTunnelID(value string) bool {
	return validID(value, tunnelPrefix)
}

// ValidConnectorID 返回 value 是否为合法 Connector ID。
func ValidConnectorID(value string) bool {
	return validID(value, connectorPrefix)
}

// ValidSessionID 返回 value 是否为合法 Session ID。
func ValidSessionID(value string) bool {
	return validID(value, sessionPrefix)
}

func validID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+ulidLength {
		return false
	}
	// ULID 承载 128 位数据，首个 Base32 字符的高两位必须为零。
	if !strings.ContainsRune("01234567", rune(value[len(prefix)])) {
		return false
	}
	for _, character := range value[len(prefix):] {
		if !strings.ContainsRune(crockfordBase32, character) {
			return false
		}
	}
	return true
}

// newID 将 48 位 UTC Unix 毫秒和 80 位 CSPRNG 随机数编码为标准 ULID。
// 它只供身份工厂及测试使用，避免任何调用方选择可预测的随机源。
func newID(prefix string, now time.Time, random io.Reader) (string, error) {
	milliseconds := now.UTC().UnixMilli()
	if milliseconds < 0 || milliseconds > maxULIDMillis {
		return "", errTimeOutsideULIDRange
	}

	var raw [16]byte
	for index := 5; index >= 0; index-- {
		raw[index] = byte(milliseconds)
		milliseconds >>= 8
	}
	if _, err := io.ReadFull(random, raw[6:]); err != nil {
		return "", fmt.Errorf("read identifier randomness: %w", err)
	}

	var encoded [ulidLength]byte
	for index := range encoded {
		var value byte
		for bit := 0; bit < 5; bit++ {
			value <<= 1
			sourceBit := index*5 + bit - 2
			if sourceBit < 0 {
				continue
			}
			value |= (raw[sourceBit/8] >> (7 - sourceBit%8)) & 1
		}
		encoded[index] = crockfordBase32[value]
	}
	return prefix + string(encoded[:]), nil
}
