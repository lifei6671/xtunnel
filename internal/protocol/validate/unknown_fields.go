// Package validate 提供 Protocol v1 结构化消息的共享校验。
package validate

import (
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ErrUnknownFields 表示 Protocol v1 消息包含协议未声明的字段。
//
// 调用方必须在业务、HMAC 或 Revision 校验前使用 errors.Is 判断该错误，
// 并将会话按 PROTOCOL_ERROR 处理。该校验绝不丢弃或改写未知字段。
var (
	// ErrUnknownFields 表示 Protocol v1 消息包含协议未声明的字段。
	//
	// 调用方必须在业务、HMAC 或 Revision 校验前使用 errors.Is 判断该错误，
	// 并将会话按 PROTOCOL_ERROR 处理。该校验绝不丢弃或改写未知字段。
	ErrUnknownFields = errors.New("protocol message contains unknown fields")

	// ErrInvalidID 表示协议标识符不符合冻结的前缀 ULID 格式。
	ErrInvalidID = errors.New("protocol identifier is invalid")
)

const crockfordBase32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ValidateID 校验 Protocol v1 的类型前缀与 26 位大写 Crockford ULID Body。
func ValidateID(value, prefix string) error {
	if !ValidID(value, prefix) {
		return ErrInvalidID
	}
	return nil
}

// ValidID 返回 value 是否符合 Protocol v1 的类型前缀 ULID 格式。
func ValidID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+26 {
		return false
	}
	// ULID 用 26 个 Base32 字符承载 128 位数据，首字符只有低 3 位可用。
	// 若允许 8..Z，多个字符串会表示超出 128 位范围的非标准值，并让各模块
	// 对同一 ID 得出不同结论，因此共享校验必须先锁定首字符为 0..7。
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

// RejectUnknownFields 递归检查 message 及其全部已出现的子消息。
//
// Protocol v1 禁止在既有消息中扩展字段：只要任意层保存了未知字段，
// 本函数就返回 ErrUnknownFields。重复消息、map 的 message value 与单个
// message 字段都经由 protobuf v2 reflection 统一遍历，避免遗漏嵌套字段。
func RejectUnknownFields(message proto.Message) error {
	return rejectUnknownFields(message.ProtoReflect())
}

func rejectUnknownFields(message protoreflect.Message) error {
	if len(message.GetUnknown()) != 0 {
		return ErrUnknownFields
	}

	var rejected bool
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		rejected = valueContainsUnknownFields(field, value)
		return !rejected
	})
	if rejected {
		return ErrUnknownFields
	}

	return nil
}

func valueContainsUnknownFields(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
	if field.IsMap() {
		mapValue := field.MapValue()
		if !isMessageKind(mapValue.Kind()) {
			return false
		}

		var rejected bool
		value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
			rejected = rejectUnknownFields(item.Message()) != nil
			return !rejected
		})
		return rejected
	}

	if !isMessageKind(field.Kind()) {
		return false
	}

	if field.IsList() {
		list := value.List()
		for index := 0; index < list.Len(); index++ {
			if rejectUnknownFields(list.Get(index).Message()) != nil {
				return true
			}
		}
		return false
	}

	return rejectUnknownFields(value.Message()) != nil
}

func isMessageKind(kind protoreflect.Kind) bool {
	return kind == protoreflect.MessageKind || kind == protoreflect.GroupKind
}
