package controlsession

import (
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"google.golang.org/protobuf/proto"
)

const (
	testProtocolVersion = uint32(1)
	testTunnelID        = "tun_01J00000000000000000000000"
	testTunnelIDTwo     = "tun_01J00000000000000000000001"
	testServiceID       = "svc_01J00000000000000000000000"
	testServiceIDTwo    = "svc_01J00000000000000000000001"
)

func TestNewOutboxRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name                   string
		protocol, high, normal int
		want                   error
	}{
		{name: "协议版本为零", protocol: 0, high: 1, normal: 1, want: ErrInvalidOutboxProtocol},
		{name: "高优先容量为零", protocol: 1, high: 0, normal: 1, want: ErrInvalidOutboxCapacity},
		{name: "普通容量为负数", protocol: 1, high: 1, normal: -1, want: ErrInvalidOutboxCapacity},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewOutbox(uint32(test.protocol), test.high, test.normal); !errors.Is(err, test.want) {
				t.Fatalf("NewOutbox() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOutboxHighPriorityFIFOHeartbeatCoalescingAndCapacity(t *testing.T) {
	outbox := mustOutbox(t, 2, 2)
	heartbeat := heartbeatEnvelope(1)
	if err := outbox.Enqueue(heartbeat); err != nil {
		t.Fatalf("Enqueue(heartbeat) error = %v", err)
	}
	if err := outbox.Enqueue(errorEnvelope(protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR)); err != nil {
		t.Fatalf("Enqueue(error) error = %v", err)
	}
	newHeartbeat := heartbeatEnvelope(2)
	if err := outbox.Enqueue(newHeartbeat); err != nil {
		t.Fatalf("Enqueue(replacement heartbeat) error = %v", err)
	}
	// 入队后修改调用方消息，不能影响 Outbox 已取得的副本。
	newHeartbeat.GetHeartbeat().TimestampMs = 99
	if err := outbox.Enqueue(drainRequestEnvelope("drain-1")); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("Enqueue(full high priority) error = %v, want ErrOutboxFull", err)
	}

	first := mustDequeue(t, outbox)
	if timestamp := first.GetHeartbeat().GetTimestampMs(); timestamp != 2 {
		t.Fatalf("first heartbeat timestamp = %d, want latest 2", timestamp)
	}
	second := mustDequeue(t, outbox)
	if second.GetError().GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR {
		t.Fatalf("second payload = %#v, want queued Error", second.GetPayload())
	}
	assertEmpty(t, outbox)
}

func TestOutboxHighPriorityTypesAndPriorityOverNormal(t *testing.T) {
	outbox := mustOutbox(t, 5, 1)
	if err := outbox.Enqueue(snapshotEnvelope(testTunnelID, 1)); err != nil {
		t.Fatalf("Enqueue(snapshot) error = %v", err)
	}
	high := []*protocolv1.ControlEnvelope{
		drainRequestEnvelope("drain-1"),
		drainAckEnvelope("drain-1"),
		configAckEnvelope(7),
		errorEnvelope(protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR),
		heartbeatEnvelope(8),
	}
	for _, envelope := range high {
		if err := outbox.Enqueue(envelope); err != nil {
			t.Fatalf("Enqueue(%T) error = %v", envelope.GetPayload(), err)
		}
	}
	for index, expected := range high {
		actual := mustDequeue(t, outbox)
		if reflect.TypeOf(actual.GetPayload()) != reflect.TypeOf(expected.GetPayload()) {
			t.Fatalf("high dequeue %d payload = %T, want %T", index, actual.GetPayload(), expected.GetPayload())
		}
	}
	if snapshot := mustDequeue(t, outbox).GetConfigSnapshot(); snapshot.GetTunnelId() != testTunnelID {
		t.Fatalf("normal payload after high = %#v, want snapshot", snapshot)
	}
}

func TestOutboxSnapshotKeepsHighestRevisionAndOwnsInput(t *testing.T) {
	outbox := mustOutbox(t, 1, 2)
	first := snapshotEnvelope(testTunnelID, 2)
	first.GetConfigSnapshot().Services = []*protocolv1.ServiceConfig{{
		ServiceId: testServiceID, OriginHost: "origin-v2",
	}}
	if err := outbox.Enqueue(first); err != nil {
		t.Fatalf("Enqueue(first snapshot) error = %v", err)
	}
	first.GetConfigSnapshot().Services[0].OriginHost = "mutated"

	older := snapshotEnvelope(testTunnelID, 1)
	older.GetConfigSnapshot().Services = []*protocolv1.ServiceConfig{{ServiceId: testServiceID, OriginHost: "origin-v1"}}
	if err := outbox.Enqueue(older); err != nil {
		t.Fatalf("Enqueue(older snapshot) error = %v", err)
	}
	higher := snapshotEnvelope(testTunnelID, 3)
	higher.GetConfigSnapshot().Services = []*protocolv1.ServiceConfig{{ServiceId: testServiceID, OriginHost: "origin-v3"}}
	if err := outbox.Enqueue(higher); err != nil {
		t.Fatalf("Enqueue(higher snapshot) error = %v", err)
	}
	higher.GetConfigSnapshot().Services[0].OriginHost = "mutated-after-enqueue"
	if err := outbox.Enqueue(snapshotEnvelope(testTunnelIDTwo, 1)); err != nil {
		t.Fatalf("Enqueue(second tunnel snapshot) error = %v", err)
	}

	got := mustDequeue(t, outbox).GetConfigSnapshot()
	if got.GetRevision() != 3 || got.GetServices()[0].GetOriginHost() != "origin-v3" {
		t.Fatalf("coalesced snapshot = %#v, want immutable revision 3", got)
	}
	if got := mustDequeue(t, outbox).GetConfigSnapshot(); got.GetTunnelId() != testTunnelIDTwo {
		t.Fatalf("second snapshot tunnel = %q, want %q", got.GetTunnelId(), testTunnelIDTwo)
	}
}

func TestOutboxWorkDemandKeepsHighestGenerationWithinSession(t *testing.T) {
	outbox := mustOutbox(t, 1, 1)
	if err := outbox.Enqueue(workDemandEnvelope(2, 20)); err != nil {
		t.Fatalf("Enqueue(work demand 2) error = %v", err)
	}
	if err := outbox.Enqueue(workDemandEnvelope(1, 10)); err != nil {
		t.Fatalf("Enqueue(older work demand) error = %v", err)
	}
	if err := outbox.Enqueue(snapshotEnvelope(testTunnelID, 1)); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("Enqueue(full normal queue) error = %v, want ErrOutboxFull", err)
	}
	if got := mustDequeue(t, outbox).GetWorkDemand(); got.GetDemandGeneration() != 2 || got.GetDesiredNonActive() != 20 {
		t.Fatalf("coalesced work demand = %#v, want generation 2", got)
	}
}

func TestOutboxHealthAccumulatorFreezesImmutableIncreasingBatches(t *testing.T) {
	outbox := mustOutbox(t, 1, 3)
	first := healthEnvelope(
		&protocolv1.ServiceHealth{ServiceId: testServiceID, CheckedAtMs: 1},
		&protocolv1.ServiceHealth{ServiceId: testServiceIDTwo, CheckedAtMs: 2},
	)
	if err := outbox.Enqueue(first); err != nil {
		t.Fatalf("Enqueue(first health) error = %v", err)
	}
	first.GetServiceHealthBatch().Items[0].CheckedAtMs = 99
	if err := outbox.Enqueue(healthEnvelope(&protocolv1.ServiceHealth{ServiceId: testServiceID, CheckedAtMs: 3})); err != nil {
		t.Fatalf("Enqueue(updated health) error = %v", err)
	}

	firstBatch := mustDequeue(t, outbox).GetServiceHealthBatch()
	if firstBatch.GetGeneration() != 1 || len(firstBatch.GetItems()) != 2 {
		t.Fatalf("first health batch = %#v, want generation 1 with two services", firstBatch)
	}
	if firstBatch.GetItems()[0].GetServiceId() != testServiceID || firstBatch.GetItems()[0].GetCheckedAtMs() != 3 {
		t.Fatalf("first service health = %#v, want latest queued observation", firstBatch.GetItems()[0])
	}
	firstBatch.Items[0].CheckedAtMs = 100

	if err := outbox.Enqueue(healthEnvelope(&protocolv1.ServiceHealth{ServiceId: testServiceID, CheckedAtMs: 4})); err != nil {
		t.Fatalf("Enqueue(second batch health) error = %v", err)
	}
	secondBatch := mustDequeue(t, outbox).GetServiceHealthBatch()
	if secondBatch.GetGeneration() != 2 || secondBatch.GetItems()[0].GetCheckedAtMs() != 4 {
		t.Fatalf("second health batch = %#v, want immutable generation 2", secondBatch)
	}
}

func TestOutboxHealthBatchSplitsAtConfiguredFrameLimit(t *testing.T) {
	first := &protocolv1.ServiceHealth{ServiceId: testServiceID, ErrorCode: "origin_timeout"}
	second := &protocolv1.ServiceHealth{ServiceId: testServiceIDTwo, ErrorCode: "origin_refused"}
	maximumGenerationSingle := healthEnvelope(first)
	maximumGenerationSingle.GetServiceHealthBatch().Generation = math.MaxUint64
	limit := uint64(proto.Size(maximumGenerationSingle))
	outbox, err := newOutbox(testProtocolVersion, 1, 2, limit)
	if err != nil {
		t.Fatalf("newOutbox() error = %v", err)
	}
	if err := outbox.Enqueue(healthEnvelope(first, second)); err != nil {
		t.Fatalf("Enqueue(two health items) error = %v", err)
	}

	for generation, serviceID := range []string{testServiceID, testServiceIDTwo} {
		envelope := mustDequeue(t, outbox)
		batch := envelope.GetServiceHealthBatch()
		if batch.GetGeneration() != uint64(generation+1) || len(batch.GetItems()) != 1 ||
			batch.GetItems()[0].GetServiceId() != serviceID {
			t.Fatalf("batch %d = %#v, want one ordered item %s", generation+1, batch, serviceID)
		}
		if size := uint64(proto.Size(envelope)); size > limit {
			t.Fatalf("batch %d size = %d, limit = %d", generation+1, size, limit)
		}
	}
	assertEmpty(t, outbox)
}

func TestOutboxRejectsSingleHealthItemOverConfiguredFrameLimit(t *testing.T) {
	item := &protocolv1.ServiceHealth{ServiceId: testServiceID, ErrorCode: "origin_timeout"}
	maximumGenerationSingle := healthEnvelope(item)
	maximumGenerationSingle.GetServiceHealthBatch().Generation = math.MaxUint64
	limit := uint64(proto.Size(maximumGenerationSingle) - 1)
	outbox, err := newOutbox(testProtocolVersion, 1, 1, limit)
	if err != nil {
		t.Fatalf("newOutbox() error = %v", err)
	}
	if err := outbox.Enqueue(healthEnvelope(item)); !errors.Is(err, ErrOutboxMessageTooLarge) {
		t.Fatalf("Enqueue(single oversized health item) error = %v, want ErrOutboxMessageTooLarge", err)
	}
	assertEmpty(t, outbox)
}

func TestOutboxHealthCapacityFailureIsAtomic(t *testing.T) {
	outbox := mustOutbox(t, 1, 2)
	if err := outbox.Enqueue(snapshotEnvelope(testTunnelID, 1)); err != nil {
		t.Fatalf("Enqueue(snapshot) error = %v", err)
	}
	batch := healthEnvelope(
		&protocolv1.ServiceHealth{ServiceId: testServiceID},
		&protocolv1.ServiceHealth{ServiceId: testServiceIDTwo},
	)
	if err := outbox.Enqueue(batch); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("Enqueue(oversized health accumulator) error = %v, want ErrOutboxFull", err)
	}
	if got := mustDequeue(t, outbox).GetConfigSnapshot(); got == nil {
		t.Fatal("capacity failure removed the existing snapshot")
	}
	assertEmpty(t, outbox)
}

func TestOutboxRejectsInvalidUnknownAndPrefrozenMessages(t *testing.T) {
	outbox := mustOutbox(t, 1, 1)
	unknown := heartbeatEnvelope(1)
	unknown.ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	tests := []struct {
		name    string
		message *protocolv1.ControlEnvelope
		want    error
	}{
		{name: "nil", message: nil, want: ErrInvalidOutboxMessage},
		{name: "wrong protocol", message: &protocolv1.ControlEnvelope{ProtocolVersion: 2, Payload: &protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{}}}, want: ErrInvalidOutboxMessage},
		{name: "missing payload", message: &protocolv1.ControlEnvelope{ProtocolVersion: 1}, want: ErrUnsupportedOutboxMessage},
		{name: "typed nil payload", message: &protocolv1.ControlEnvelope{ProtocolVersion: 1, Payload: &protocolv1.ControlEnvelope_Heartbeat{}}, want: ErrInvalidOutboxMessage},
		{name: "unknown field", message: unknown, want: ErrUnsupportedOutboxMessage},
		{name: "invalid tunnel key", message: snapshotEnvelope("tun-invalid", 1), want: ErrInvalidOutboxMessage},
		{name: "prefrozen health generation", message: healthEnvelope(&protocolv1.ServiceHealth{ServiceId: testServiceID}), want: ErrInvalidOutboxMessage},
		{name: "invalid health key", message: healthEnvelope(&protocolv1.ServiceHealth{ServiceId: "svc-invalid"}), want: ErrInvalidOutboxMessage},
	}
	// 只有本用例模拟调用方提前分配 generation。
	tests[6].message.GetServiceHealthBatch().Generation = 1
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := outbox.Enqueue(test.message); !errors.Is(err, test.want) {
				t.Fatalf("Enqueue() error = %v, want %v", err, test.want)
			}
		})
	}
	if err := outbox.Enqueue(unknown); !errors.Is(err, validate.ErrUnknownFields) {
		t.Fatalf("Enqueue(unknown field) error = %v, want wrapped validate.ErrUnknownFields", err)
	}
}

func TestOutboxConcurrentCoalescing(t *testing.T) {
	outbox := mustOutbox(t, 1, 2)
	const workers = 64
	var wait sync.WaitGroup
	errorsSeen := make(chan error, workers*3)
	for index := 1; index <= workers; index++ {
		wait.Add(1)
		go func(generation uint64) {
			defer wait.Done()
			messages := []*protocolv1.ControlEnvelope{
				heartbeatEnvelope(generation),
				workDemandEnvelope(generation, uint32(generation)),
				healthEnvelope(&protocolv1.ServiceHealth{ServiceId: testServiceID, CheckedAtMs: generation}),
			}
			for _, message := range messages {
				if err := outbox.Enqueue(message); err != nil {
					errorsSeen <- err
				}
			}
		}(uint64(index))
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent Enqueue() error = %v", err)
	}

	var heartbeatCount, workCount, healthCount int
	for {
		envelope, ok := outbox.Dequeue()
		if !ok {
			break
		}
		switch envelope.GetPayload().(type) {
		case *protocolv1.ControlEnvelope_Heartbeat:
			heartbeatCount++
		case *protocolv1.ControlEnvelope_WorkDemand:
			workCount++
			if envelope.GetWorkDemand().GetDemandGeneration() != workers {
				t.Fatalf("work generation = %d, want %d", envelope.GetWorkDemand().GetDemandGeneration(), workers)
			}
		case *protocolv1.ControlEnvelope_ServiceHealthBatch:
			healthCount++
		default:
			t.Fatalf("unexpected dequeued payload %T", envelope.GetPayload())
		}
	}
	if heartbeatCount != 1 || workCount != 1 || healthCount != 1 {
		t.Fatalf("coalesced counts = heartbeat:%d work:%d health:%d, want 1 each", heartbeatCount, workCount, healthCount)
	}
}

func mustOutbox(t *testing.T, high, normal int) *Outbox {
	t.Helper()
	outbox, err := NewOutbox(testProtocolVersion, high, normal)
	if err != nil {
		t.Fatalf("NewOutbox() error = %v", err)
	}
	return outbox
}

func mustDequeue(t *testing.T, outbox *Outbox) *protocolv1.ControlEnvelope {
	t.Helper()
	envelope, ok := outbox.Dequeue()
	if !ok {
		t.Fatal("Dequeue() returned empty outbox")
	}
	return envelope
}

func assertEmpty(t *testing.T, outbox *Outbox) {
	t.Helper()
	if envelope, ok := outbox.Dequeue(); ok {
		t.Fatalf("Dequeue() = %#v, true, want empty", envelope)
	}
}

func heartbeatEnvelope(timestamp uint64) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocolVersion,
		Payload:         &protocolv1.ControlEnvelope_Heartbeat{Heartbeat: &protocolv1.Heartbeat{TimestampMs: timestamp}},
	}
}

func errorEnvelope(code protocolv1.ErrorCode) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocolVersion,
		Payload:         &protocolv1.ControlEnvelope_Error{Error: &protocolv1.Error{ErrorCode: code}},
	}
}

func drainRequestEnvelope(id string) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocolVersion,
		Payload:         &protocolv1.ControlEnvelope_DrainRequest{DrainRequest: &protocolv1.DrainRequest{DrainId: id, DrainTimeoutMs: 1}},
	}
}

func drainAckEnvelope(id string) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocolVersion,
		Payload:         &protocolv1.ControlEnvelope_DrainAck{DrainAck: &protocolv1.DrainAck{DrainId: id}},
	}
}

func configAckEnvelope(revision uint64) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocolVersion,
		Payload:         &protocolv1.ControlEnvelope_ConfigAck{ConfigAck: &protocolv1.ConfigAck{ObservedRevision: revision}},
	}
}

func snapshotEnvelope(tunnelID string, revision uint64) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocolVersion,
		Payload: &protocolv1.ControlEnvelope_ConfigSnapshot{ConfigSnapshot: &protocolv1.TunnelSnapshot{
			TunnelId: tunnelID, Revision: revision,
		}},
	}
}

func workDemandEnvelope(generation uint64, desired uint32) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocolVersion,
		Payload: &protocolv1.ControlEnvelope_WorkDemand{WorkDemand: &protocolv1.WorkDemand{
			DemandGeneration: generation, DesiredNonActive: desired,
		}},
	}
}

func healthEnvelope(items ...*protocolv1.ServiceHealth) *protocolv1.ControlEnvelope {
	return &protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocolVersion,
		Payload: &protocolv1.ControlEnvelope_ServiceHealthBatch{ServiceHealthBatch: &protocolv1.ServiceHealthBatch{
			Items: items,
		}},
	}
}
