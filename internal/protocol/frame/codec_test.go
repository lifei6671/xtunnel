package frame

import (
	"bytes"
	"errors"
	"io"
	"testing"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"google.golang.org/protobuf/proto"
)

// TestPayloadLimits 覆盖三种传输层各自的 Frame 上限，精确上限可写入和读取，
// 多一个字节必须在分配或写入前拒绝。
func TestPayloadLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		limit uint64
	}{
		{name: "auth", limit: MaxAuthFrameSize},
		{name: "control", limit: MaxControlFrameSize},
		{name: "work", limit: MaxWorkFrameSize},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			payload := bytes.Repeat([]byte{0x5a}, int(test.limit))
			var encoded bytes.Buffer
			if err := WritePayload(&encoded, payload, test.limit); err != nil {
				t.Fatalf("WritePayload() error = %v", err)
			}
			decoded, err := ReadPayload(&encoded, test.limit)
			if err != nil {
				t.Fatalf("ReadPayload() error = %v", err)
			}
			if !bytes.Equal(decoded, payload) {
				t.Fatal("ReadPayload() payload changed")
			}

			overLimit := append(payload, 0)
			if err := WritePayload(io.Discard, overLimit, test.limit); !errors.Is(err, ErrFrameTooLarge) {
				t.Fatalf("WritePayload() error = %v, want ErrFrameTooLarge", err)
			}
		})
	}
}

// TestReadPayloadFragmentedUVarint 确保两字节长度前缀即使被拆成单字节 Read，
// 仍会严格还原当前 Frame，而不会依赖 bufio 等可能预读的数据结构。
func TestReadPayloadFragmentedUVarint(t *testing.T) {
	payload := bytes.Repeat([]byte{0xa5}, 300)
	var encoded bytes.Buffer
	if err := WritePayload(&encoded, payload, MaxWorkFrameSize); err != nil {
		t.Fatalf("WritePayload() error = %v", err)
	}

	decoded, err := ReadPayload(&singleByteReader{reader: bytes.NewReader(encoded.Bytes())}, MaxWorkFrameSize)
	if err != nil {
		t.Fatalf("ReadPayload() error = %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatal("ReadPayload() payload changed after fragmented reads")
	}
}

// TestReadPayloadLeavesFollowingBytes 验证读取一个结构化 Frame 后，第二帧和随后的
// RAW 字节仍完全留在底层 Reader；这是 OPEN_OK 切入 RAW 时零丢失的基础保证。
func TestReadPayloadLeavesFollowingBytes(t *testing.T) {
	first := []byte("first")
	second := []byte("second")
	raw := []byte("raw-first-bytes")

	var stream bytes.Buffer
	if err := WritePayload(&stream, first, MaxWorkFrameSize); err != nil {
		t.Fatalf("write first frame: %v", err)
	}
	if err := WritePayload(&stream, second, MaxWorkFrameSize); err != nil {
		t.Fatalf("write second frame: %v", err)
	}
	if _, err := stream.Write(raw); err != nil {
		t.Fatalf("write raw bytes: %v", err)
	}

	reader := bytes.NewReader(stream.Bytes())
	decoded, err := ReadPayload(reader, MaxWorkFrameSize)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if !bytes.Equal(decoded, first) {
		t.Fatalf("first payload = %q, want %q", decoded, first)
	}

	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining stream: %v", err)
	}
	var expected bytes.Buffer
	if err := WritePayload(&expected, second, MaxWorkFrameSize); err != nil {
		t.Fatalf("encode expected second frame: %v", err)
	}
	if _, err := expected.Write(raw); err != nil {
		t.Fatalf("append expected raw bytes: %v", err)
	}
	if !bytes.Equal(remaining, expected.Bytes()) {
		t.Fatalf("remaining bytes = %x, want %x", remaining, expected.Bytes())
	}
}

// TestReadPayloadRejectsMalformedInput 覆盖 EOF、截断、非规范 UVarint、溢出和声明超限，
// 这些错误都必须在调用方进入 Protobuf 或业务状态机之前可识别。
func TestReadPayloadRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		limit uint64
		want  error
	}{
		{name: "empty stream", input: nil, limit: MaxAuthFrameSize, want: io.EOF},
		{name: "truncated length", input: []byte{0x80}, limit: MaxAuthFrameSize, want: ErrTruncatedFrame},
		{name: "non canonical length", input: []byte{0x81, 0x00, 0x00}, limit: MaxAuthFrameSize, want: ErrInvalidLength},
		{name: "overflow length", input: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}, limit: MaxAuthFrameSize, want: ErrInvalidLength},
		{name: "truncated payload", input: []byte{0x03, 0x01, 0x02}, limit: MaxAuthFrameSize, want: ErrTruncatedFrame},
		{name: "announced payload over limit", input: []byte{0x81, 0x01}, limit: 128, want: ErrFrameTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadPayload(bytes.NewReader(test.input), test.limit)
			if !errors.Is(err, test.want) {
				t.Fatalf("ReadPayload() error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestReadMessageRejectsMalformedProtobuf 确保长度合法但 Protobuf wire 格式损坏时，
// 错误不会被当作空消息接受。
func TestReadMessageRejectsMalformedProtobuf(t *testing.T) {
	// 第一字节是长度 1，payload 0xff 是带 continuation bit 的不完整 field key。
	err := ReadAuth(bytes.NewReader([]byte{0x01, 0xff}), &protocolv1.AgentAuthRequest{})
	if !errors.Is(err, ErrMalformedMessage) {
		t.Fatalf("ReadAuth() error = %v, want ErrMalformedMessage", err)
	}
}

// TestTransportWrappers 验证 AUTH、Control 与 Work 入口使用各自上限并可完成
// Protobuf 往返；消息状态和方向约束由后续 Protocol State Test 负责。
func TestTransportWrappers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		write func(io.Writer, proto.Message) error
		read  func(io.Reader, proto.Message) error
		sent  proto.Message
		recv  proto.Message
	}{
		{
			name:  "auth",
			write: WriteAuth,
			read:  ReadAuth,
			sent:  &protocolv1.AgentAuthRequest{ConnectionToken: "xta_example"},
			recv:  &protocolv1.AgentAuthRequest{},
		},
		{
			name: "control",
			write: func(writer io.Writer, message proto.Message) error {
				return WriteControl(writer, message.(*protocolv1.ControlEnvelope))
			},
			read: func(reader io.Reader, message proto.Message) error {
				return ReadControl(reader, message.(*protocolv1.ControlEnvelope))
			},
			sent: &protocolv1.ControlEnvelope{ProtocolVersion: 1},
			recv: &protocolv1.ControlEnvelope{},
		},
		{
			name:  "work",
			write: WriteWork,
			read:  ReadWork,
			sent:  &protocolv1.WorkHello{AgentId: "ag_example"},
			recv:  &protocolv1.WorkHello{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var encoded bytes.Buffer
			if err := test.write(&encoded, test.sent); err != nil {
				t.Fatalf("write wrapper error = %v", err)
			}
			if err := test.read(&encoded, test.recv); err != nil {
				t.Fatalf("read wrapper error = %v", err)
			}
			if !proto.Equal(test.recv, test.sent) {
				t.Fatalf("decoded message = %v, want %v", test.recv, test.sent)
			}
		})
	}
}

// singleByteReader 模拟网络把每个字节独立送达的场景，迫使解码器处理分片前缀与 payload。
type singleByteReader struct {
	reader io.Reader
}

func (reader *singleByteReader) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	return reader.reader.Read(data[:1])
}
