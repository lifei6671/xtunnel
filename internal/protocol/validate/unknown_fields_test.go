package validate

import (
	"errors"
	"testing"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// unknownField 是未在 Protocol v1 声明的 field 99，值为 1。
// 测试直接保留原始 wire bytes，确保校验器不会通过 discard 掩盖协议扩展。
var unknownField = []byte{0x98, 0x06, 0x01}

func TestRejectUnknownFieldsRejectsProtocolV1MessagesAtAnyDepth(t *testing.T) {
	tests := []struct {
		name    string
		message proto.Message
	}{
		{
			name:    "Auth 自身未知字段",
			message: &protocolv1.AgentAuthRequest{},
		},
		{
			name: "Auth 嵌套未知字段",
			message: &protocolv1.AgentAuthResult{
				Result: &protocolv1.AgentAuthResult_Success{
					Success: &protocolv1.AgentAuthSuccess{},
				},
			},
		},
		{
			name: "ControlEnvelope 自身未知字段",
			message: &protocolv1.ControlEnvelope{
				Payload: &protocolv1.ControlEnvelope_Heartbeat{
					Heartbeat: &protocolv1.Heartbeat{},
				},
			},
		},
		{
			name: "ControlEnvelope 内的 Snapshot 深层未知字段",
			message: &protocolv1.ControlEnvelope{
				Payload: &protocolv1.ControlEnvelope_ConfigSnapshot{
					ConfigSnapshot: &protocolv1.AgentSnapshot{
						Bindings: []*protocolv1.TunnelBindingConfig{{
							Health: &protocolv1.HealthCheckConfig{},
						}},
					},
				},
			},
		},
		{
			name: "AgentSnapshot 自身未知字段",
			message: &protocolv1.AgentSnapshot{
				Bindings: []*protocolv1.TunnelBindingConfig{{
					Health: &protocolv1.HealthCheckConfig{},
				}},
			},
		},
		{
			name:    "Work 自身未知字段",
			message: &protocolv1.WorkHello{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := unknownFieldTarget(t, test.message)
			target.SetUnknown(append([]byte(nil), unknownField...))

			if err := RejectUnknownFields(test.message); !errors.Is(err, ErrUnknownFields) {
				t.Fatalf("RejectUnknownFields() error = %v, want ErrUnknownFields", err)
			}
			if got := target.GetUnknown(); string(got) != string(unknownField) {
				t.Fatalf("校验后未知字段被改写: got %x, want %x", got, unknownField)
			}
		})
	}
}

func TestRejectUnknownFieldsAcceptsFullyKnownNestedMessage(t *testing.T) {
	message := &protocolv1.ControlEnvelope{
		Payload: &protocolv1.ControlEnvelope_ConfigSnapshot{
			ConfigSnapshot: &protocolv1.AgentSnapshot{
				Bindings: []*protocolv1.TunnelBindingConfig{{
					Health: &protocolv1.HealthCheckConfig{},
				}},
			},
		},
	}

	if err := RejectUnknownFields(message); err != nil {
		t.Fatalf("RejectUnknownFields() error = %v, want nil", err)
	}
}

func TestValidateID(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		prefix string
		wantOK bool
	}{
		{name: "合法", value: "work_01J00000000000000000000000", prefix: "work_", wantOK: true},
		{name: "错误前缀", value: "conn_01J00000000000000000000000", prefix: "work_"},
		{name: "小写 ULID", value: "work_01j00000000000000000000000", prefix: "work_"},
		{name: "错误长度", value: "work_01J", prefix: "work_"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateID(test.value, test.prefix)
			if (err == nil) != test.wantOK {
				t.Fatalf("ValidateID(%q, %q) error = %v, wantOK=%t", test.value, test.prefix, err, test.wantOK)
			}
		})
	}
}

func unknownFieldTarget(t *testing.T, message proto.Message) protoreflect.Message {
	t.Helper()

	switch typed := message.(type) {
	case *protocolv1.AgentAuthRequest:
		return typed.ProtoReflect()
	case *protocolv1.AgentAuthResult:
		return typed.GetSuccess().ProtoReflect()
	case *protocolv1.ControlEnvelope:
		if snapshot := typed.GetConfigSnapshot(); snapshot != nil {
			return snapshot.GetBindings()[0].GetHealth().ProtoReflect()
		}
		return typed.ProtoReflect()
	case *protocolv1.AgentSnapshot:
		return typed.ProtoReflect()
	case *protocolv1.WorkHello:
		return typed.ProtoReflect()
	default:
		t.Fatalf("未覆盖的测试消息类型 %T", message)
		return nil
	}
}
