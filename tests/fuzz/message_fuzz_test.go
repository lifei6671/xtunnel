package fuzz_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	protocolvalidate "github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const fuzzMessageLimit uint64 = 64 << 10

func FuzzControlEnvelope(f *testing.F) {
	seeds := [][]byte{
		framedControlSeed(f, &protocolv1.ControlEnvelope{}),
		framedControlSeed(f, &protocolv1.ControlEnvelope{
			ProtocolVersion: 1,
			Payload: &protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{
				TimestampMs: 1, ObservedRevision: 2,
			}},
		}),
		framedControlSeed(f, &protocolv1.ControlEnvelope{
			ProtocolVersion: 1,
			Payload: &protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: &protocolv1.TunnelSnapshot{
				TunnelId: "tun_00000000000000000000000000", Revision: 1,
			}},
		}),
		{0x03, 0x98, 0x06, 0x01},
		{0x02, 0x5a, 0x02},
		{0x80, 0x00},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > int(fuzzMessageLimit)+64 {
			return
		}
		message := &protocolv1.ControlEnvelope{}
		if err := frame.ReadControlLimit(bytes.NewReader(data), message, fuzzMessageLimit); err != nil {
			assertMessageReadError(t, err)
			return
		}
		if err := protocolvalidate.RejectUnknownFields(message); err != nil {
			return
		}
		assertMessageRoundTrip(t, message, func(reader *bytes.Reader, target proto.Message) error {
			return frame.ReadControlLimit(reader, target.(*protocolv1.ControlEnvelope), fuzzMessageLimit)
		})
	})
}

func FuzzWorkHello(f *testing.F) {
	seeds := [][]byte{
		framedWorkSeed(f, &protocolv1.WorkHello{}),
		framedWorkSeed(f, &protocolv1.WorkHello{
			TunnelId: "tun_00000000000000000000000000", ConnectorId: "con_00000000000000000000000000",
			SessionId: "ses_00000000000000000000000000", WorkId: "wrk_00000000000000000000000000",
			Nonce: bytes.Repeat([]byte{1}, 32), Mac: bytes.Repeat([]byte{2}, 32),
			BudgetLeaseId: "bdg_00000000000000000000000000",
		}),
		{0x02, 0x28, 0x01},
		{0x02, 0x32, 0x01},
		{0x02, 0x0a, 0x02},
		{0x80, 0x00},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > int(fuzzMessageLimit)+64 {
			return
		}
		message := &protocolv1.WorkHello{}
		if err := frame.ReadWorkLimit(bytes.NewReader(data), message, fuzzMessageLimit); err != nil {
			assertMessageReadError(t, err)
			return
		}
		if err := protocolvalidate.RejectUnknownFields(message); err != nil {
			return
		}
		assertMessageRoundTrip(t, message, func(reader *bytes.Reader, target proto.Message) error {
			return frame.ReadWorkLimit(reader, target, fuzzMessageLimit)
		})
	})
}

func framedControlSeed(f *testing.F, message *protocolv1.ControlEnvelope) []byte {
	f.Helper()
	var output bytes.Buffer
	if err := frame.WriteControlLimit(&output, message, fuzzMessageLimit); err != nil {
		f.Fatalf("WriteControlLimit(seed) error = %v", err)
	}
	return output.Bytes()
}

func framedWorkSeed(f *testing.F, message *protocolv1.WorkHello) []byte {
	f.Helper()
	var output bytes.Buffer
	if err := frame.WriteWorkLimit(&output, message, fuzzMessageLimit); err != nil {
		f.Fatalf("WriteWorkLimit(seed) error = %v", err)
	}
	return output.Bytes()
}

func assertMessageRoundTrip(t *testing.T, message proto.Message, read func(*bytes.Reader, proto.Message) error) {
	t.Helper()
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		t.Fatalf("deterministic marshal error = %v", err)
	}
	var framed bytes.Buffer
	if err := frame.WritePayload(&framed, payload, fuzzMessageLimit); err != nil {
		t.Fatalf("WritePayload(round trip) error = %v", err)
	}
	target := message.ProtoReflect().Type().New().Interface()
	if err := read(bytes.NewReader(framed.Bytes()), target); err != nil {
		t.Fatalf("reread error = %v", err)
	}
	if err := protocolvalidate.RejectUnknownFields(target); err != nil {
		t.Fatalf("reread unknown fields error = %v", err)
	}
	if !proto.Equal(message, target) {
		t.Fatalf("round trip mismatch: before=%v after=%v", message, target)
	}
}
