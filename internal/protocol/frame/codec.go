// Package frame 提供 XTunnel Protocol v1 的有界 Protobuf 分帧编解码。
//
// 每个结构化消息均使用“最短 UVarint 长度 + Protobuf payload”编码。读取器只按
// 当前帧长度读取，绝不引入可能吞掉下一帧或 Work RAW 首包的缓冲预读。
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

const (
	// MaxControlFrameSize 是 ControlEnvelope 完整 Protobuf payload 的最大字节数。
	MaxControlFrameSize uint64 = 1 << 20

	// MaxAuthFrameSize 是 AUTH 阶段裸消息 Protobuf payload 的最大字节数。
	MaxAuthFrameSize uint64 = 64 << 10

	// MaxWorkFrameSize 是 WorkConn 进入 RAW 前裸消息 Protobuf payload 的最大字节数。
	MaxWorkFrameSize uint64 = 64 << 10
)

var (
	// ErrInvalidLength 表示长度前缀不是最短、无溢出的 UVarint。
	ErrInvalidLength = errors.New("frame: invalid UVarint length")

	// ErrTruncatedFrame 表示长度前缀或当前帧 payload 在读取完成前结束。
	ErrTruncatedFrame = errors.New("frame: truncated frame")

	// ErrFrameTooLarge 表示当前帧宣称或待写入的 payload 超过所属传输层限制。
	ErrFrameTooLarge = errors.New("frame: payload exceeds frame size limit")

	// ErrMalformedMessage 表示完整 payload 不能解码为预期的 Protobuf 消息。
	ErrMalformedMessage = errors.New("frame: malformed protobuf message")

	// ErrNilMessage 表示调用方没有提供用于编解码的 Protobuf 消息。
	ErrNilMessage = errors.New("frame: nil protobuf message")
)

// ReadPayload 读取一个完整帧的 Protobuf payload。
//
// 它逐字节读取 UVarint 前缀，再用 io.ReadFull 精确读取已声明的 payload 长度。因此，
// 返回时底层 Reader 恰好停在下一帧（或 RAW 数据）的第一个字节。
func ReadPayload(reader io.Reader, limit uint64) ([]byte, error) {
	length, err := readLength(reader)
	if err != nil {
		return nil, err
	}
	if length > limit {
		return nil, fmt.Errorf("%w: length=%d limit=%d", ErrFrameTooLarge, length, limit)
	}
	// make 的长度参数受本机 int 表示范围限制；即使调用方错误传入超大 limit，
	// 也必须作为协议长度错误返回，不能让长度转换发生溢出。
	if length > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("%w: length=%d exceeds native allocation range", ErrFrameTooLarge, length)
	}

	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, fmt.Errorf("%w: payload: %w", ErrTruncatedFrame, err)
	}
	return payload, nil
}

// WritePayload 将一个 Protobuf payload 作为完整帧写入 writer。
//
// 写入顺序固定为最短 UVarint 长度前缀后接 payload；短写会返回 io.ErrShortWrite，
// 不会被误报为成功。
func WritePayload(writer io.Writer, payload []byte, limit uint64) error {
	length := uint64(len(payload))
	if length > limit {
		return fmt.Errorf("%w: length=%d limit=%d", ErrFrameTooLarge, length, limit)
	}

	var prefix [binary.MaxVarintLen64]byte
	prefixLength := binary.PutUvarint(prefix[:], length)
	if err := writeAll(writer, prefix[:prefixLength]); err != nil {
		return fmt.Errorf("write frame length: %w", err)
	}
	if err := writeAll(writer, payload); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}
	return nil
}

// ReadMessage 读取一帧并将其 payload 解码到 message。
func ReadMessage(reader io.Reader, message proto.Message, limit uint64) error {
	if message == nil {
		return ErrNilMessage
	}
	payload, err := ReadPayload(reader, limit)
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(payload, message); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedMessage, err)
	}
	return nil
}

// WriteMessage 将 message 序列化为 Protobuf payload 后写入一个完整帧。
func WriteMessage(writer io.Writer, message proto.Message, limit uint64) error {
	if message == nil {
		return ErrNilMessage
	}
	payload, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal protobuf message: %w", err)
	}
	return WritePayload(writer, payload, limit)
}

// ReadAuth 读取 AUTH 阶段的裸 ConnectorAuthRequest 或 ConnectorAuthResult。
func ReadAuth(reader io.Reader, message proto.Message) error {
	return ReadAuthLimit(reader, message, MaxAuthFrameSize)
}

// WriteAuth 写入 AUTH 阶段的裸 ConnectorAuthRequest 或 ConnectorAuthResult。
func WriteAuth(writer io.Writer, message proto.Message) error {
	return WriteAuthLimit(writer, message, MaxAuthFrameSize)
}

// ReadControl 读取 ESTABLISHED 或 DRAINING 阶段的 ControlEnvelope。
func ReadControl(reader io.Reader, envelope *protocolv1.ControlEnvelope) error {
	return ReadControlLimit(reader, envelope, MaxControlFrameSize)
}

// WriteControl 写入 ESTABLISHED 或 DRAINING 阶段的 ControlEnvelope。
func WriteControl(writer io.Writer, envelope *protocolv1.ControlEnvelope) error {
	return WriteControlLimit(writer, envelope, MaxControlFrameSize)
}

// ReadWork 读取 WorkConn 在 RAW 前、由状态唯一确定的裸 Work 消息。
func ReadWork(reader io.Reader, message proto.Message) error {
	return ReadWorkLimit(reader, message, MaxWorkFrameSize)
}

// WriteWork 写入 WorkConn 在 RAW 前、由状态唯一确定的裸 Work 消息。
func WriteWork(writer io.Writer, message proto.Message) error {
	return WriteWorkLimit(writer, message, MaxWorkFrameSize)
}

// ReadAuthLimit 使用 Server 配置收紧 AUTH Frame 上限。limit 不能超过 Protocol v1
// 的绝对上限；调用方传入更大值也不能扩大冻结协议的内存分配边界。
func ReadAuthLimit(reader io.Reader, message proto.Message, limit uint64) error {
	return readWithinProtocolLimit(reader, message, limit, MaxAuthFrameSize)
}

// WriteAuthLimit 使用与读取侧一致的 AUTH Frame 有效上限。
func WriteAuthLimit(writer io.Writer, message proto.Message, limit uint64) error {
	return writeWithinProtocolLimit(writer, message, limit, MaxAuthFrameSize)
}

// ReadControlLimit 使用 Server 配置收紧 Control Frame 上限。
func ReadControlLimit(reader io.Reader, envelope *protocolv1.ControlEnvelope, limit uint64) error {
	return readWithinProtocolLimit(reader, envelope, limit, MaxControlFrameSize)
}

// WriteControlLimit 使用与读取侧一致的 Control Frame 有效上限。
func WriteControlLimit(writer io.Writer, envelope *protocolv1.ControlEnvelope, limit uint64) error {
	return writeWithinProtocolLimit(writer, envelope, limit, MaxControlFrameSize)
}

// ReadWorkLimit 使用 Server 配置收紧 Work/OPEN Frame 上限。
func ReadWorkLimit(reader io.Reader, message proto.Message, limit uint64) error {
	return readWithinProtocolLimit(reader, message, limit, MaxWorkFrameSize)
}

// WriteWorkLimit 使用与读取侧一致的 Work/OPEN Frame 有效上限。
func WriteWorkLimit(writer io.Writer, message proto.Message, limit uint64) error {
	return writeWithinProtocolLimit(writer, message, limit, MaxWorkFrameSize)
}

func readWithinProtocolLimit(reader io.Reader, message proto.Message, configured, absolute uint64) error {
	if configured == 0 || configured > absolute {
		return fmt.Errorf("%w: configured limit=%d absolute=%d", ErrFrameTooLarge, configured, absolute)
	}
	return ReadMessage(reader, message, configured)
}

func writeWithinProtocolLimit(writer io.Writer, message proto.Message, configured, absolute uint64) error {
	if configured == 0 || configured > absolute {
		return fmt.Errorf("%w: configured limit=%d absolute=%d", ErrFrameTooLarge, configured, absolute)
	}
	return WriteMessage(writer, message, configured)
}

// readLength 读取并校验最短 UVarint 长度前缀，且不会触碰 payload 的任何字节。
func readLength(reader io.Reader) (uint64, error) {
	var value uint64
	var one [1]byte

	for index := range binary.MaxVarintLen64 {
		if _, err := io.ReadFull(reader, one[:]); err != nil {
			if index == 0 && errors.Is(err, io.EOF) {
				return 0, io.EOF
			}
			return 0, fmt.Errorf("%w: length: %w", ErrTruncatedFrame, err)
		}

		byteValue := one[0]
		if byteValue < 0x80 {
			// 第十个字节只能为 0 或 1，否则 uint64 已溢出。
			if index == binary.MaxVarintLen64-1 && byteValue > 1 {
				return 0, ErrInvalidLength
			}
			value |= uint64(byteValue) << (7 * index)

			// 同一长度只允许最短编码，避免多种字节序列表达同一 Frame。
			var canonical [binary.MaxVarintLen64]byte
			if binary.PutUvarint(canonical[:], value) != index+1 {
				return 0, ErrInvalidLength
			}
			return value, nil
		}

		// 第十个字节仍有 continuation bit 时，UVarint 必然溢出。
		if index == binary.MaxVarintLen64-1 {
			return 0, ErrInvalidLength
		}
		value |= uint64(byteValue&0x7f) << (7 * index)
	}

	return 0, ErrInvalidLength
}

// writeAll 处理 io.Writer 合法返回的短写，直到完整帧字节已写入或出现错误。
func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
