// Package originconfig 定义 Server Desired State 与 Agent Snapshot 共用的 Origin 语义校验。
package originconfig

import (
	"errors"
	"net/netip"
	"strings"
)

const maximumPort = 65_535

// ErrInvalid 表示 Origin 字段不能被 Server 与 Agent 无歧义地共同解释。
var ErrInvalid = errors.New("origin configuration is invalid")

// Fields 是 HTTP、HTTPS 与 TCP Origin 的完整配置字段。
type Fields struct {
	Scheme           string
	Host             string
	Port             uint32
	ConnectTimeoutMS uint32
	TLSVerify        bool
	TLSServerName    string
	HTTPHostHeader   string
}

// Validate 拒绝 Agent 无法无歧义解析的 Origin。Host 必须已经是规范 ASCII DNS
// 名或规范 IP Literal；本函数不解析 DNS，也不阻止 Loopback 或私网地址。
func Validate(fields Fields) error {
	if fields.Port < 1 || fields.Port > maximumPort || fields.ConnectTimeoutMS == 0 ||
		!ValidHost(fields.Host) || (fields.TLSServerName != "" && !ValidHost(fields.TLSServerName)) ||
		!validHTTPHostHeader(fields.HTTPHostHeader) {
		return ErrInvalid
	}
	switch fields.Scheme {
	case "http":
		if fields.TLSServerName != "" {
			return ErrInvalid
		}
	case "https":
		return nil
	case "tcp":
		if fields.TLSServerName != "" || fields.HTTPHostHeader != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// ValidHost 接受规范 IPv4/IPv6 Literal 或小写 ASCII DNS Hostname。
// V0.1 不接受 IPv6 Zone、URI、内嵌端口、尾点、Unicode 或大小写混合形式。
func ValidHost(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	if address, err := netip.ParseAddr(value); err == nil {
		return address.Zone() == "" && address.String() == value
	}
	if strings.ContainsAny(value, "[]/:\\") || strings.HasSuffix(value, ".") || value != strings.ToLower(value) ||
		len(value) > 253 || allNumericDNS(value) {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func allNumericDNS(value string) bool {
	for _, character := range value {
		if character != '.' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validHTTPHostHeader(value string) bool {
	if value == "" {
		return true
	}
	if strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character <= 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
