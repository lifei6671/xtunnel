package configruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	protocolvalidate "github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/safego"
)

const (
	testTunnelID   = "tun_01J00000000000000000000000"
	testTunnelID2  = "tun_01J00000000000000000000001"
	testServiceID  = "svc_01J00000000000000000000000"
	testServiceID2 = "svc_01J00000000000000000000001"
)

func TestNewRejectsInvalidConfig(t *testing.T) {
	valid := testConfig(&fakeBuilder{})
	if _, err := New(nil, valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(nil) error = %v, want ErrInvalidConfig", err)
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "protocol version", mutate: func(config *Config) { config.ProtocolVersion = 2 }},
		{name: "service limit", mutate: func(config *Config) { config.MaxServices = 0 }},
		{name: "service limit above absolute", mutate: func(config *Config) { config.MaxServices = MaxServicesPerTunnel + 1 }},
		{name: "snapshot limit zero", mutate: func(config *Config) { config.MaxSnapshotBytes = 0 }},
		{name: "snapshot limit above absolute", mutate: func(config *Config) { config.MaxSnapshotBytes = MaxSnapshotSize + 1 }},
		{name: "frame limit zero", mutate: func(config *Config) { config.MaxControlFrameBytes = 0 }},
		{name: "frame limit above absolute", mutate: func(config *Config) { config.MaxControlFrameBytes = (1 << 20) + 1 }},
		{name: "retire timeout", mutate: func(config *Config) { config.RetireTimeout = 0 }},
		{name: "nil builder", mutate: func(config *Config) { config.Builder = nil }},
		{name: "typed nil builder", mutate: func(config *Config) { var builder *fakeBuilder; config.Builder = builder }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			if _, err := New(context.Background(), config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestApplyPublishesWholeTupleBeforeAckAndActivatesGateAfterAck(t *testing.T) {
	oldResources := &fakeResources{retired: make(chan struct{})}
	newResources := &fakeResources{retired: make(chan struct{})}
	builder := &fakeBuilder{resources: []Resources{oldResources, newResources}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)

	firstSink := &fakeSink{}
	if err := session.Apply(context.Background(), testSnapshot(1, testServiceID), firstSink); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	firstGate := builder.gate(0)
	if !firstGate.Active() {
		t.Fatal("first Gate is inactive after APPLIED Ack")
	}

	secondSnapshot := testSnapshot(2, testServiceID2)
	original := proto.Clone(secondSnapshot).(*protocolv1.TunnelSnapshot)
	secondSink := &fakeSink{onEnqueue: func(envelope *protocolv1.ControlEnvelope) error {
		view, ok := manager.Current()
		if !ok || view.Revision != 2 || view.Snapshot.GetRevision() != 2 {
			t.Fatalf("Current() during Ack = (%+v, %v), want complete revision 2 tuple", view, ok)
		}
		if view.Acked || builder.gate(1).Active() {
			t.Fatal("Candidate Gate activated before APPLIED Ack enqueue returned")
		}
		select {
		case <-oldResources.retired:
			t.Fatal("old Resources retired before APPLIED Ack enqueue returned")
		default:
		}
		ack := envelope.GetConfigAck()
		if ack.GetObservedRevision() != 2 || ack.GetApplyStatus() != protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED ||
			ack.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_OK {
			t.Fatalf("APPLIED Ack = %+v", ack)
		}
		return nil
	}}
	if err := session.Apply(context.Background(), secondSnapshot, secondSink); err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	if !builder.gate(1).Active() || firstGate.Active() {
		t.Fatalf("Gate states after publish: old=%v new=%v", firstGate.Active(), builder.gate(1).Active())
	}
	waitClosed(t, oldResources.retired, "old Resources retirement")
	if oldResources.retireCalls.Load() != 1 {
		t.Fatalf("old Resources Retire calls = %d, want 1", oldResources.retireCalls.Load())
	}

	secondSnapshot.Revision = 99
	secondSnapshot.Services[0].ServiceId = testServiceID
	view, ok := manager.Current()
	if !ok || view.Revision != 2 || view.Snapshot.GetServices()[0].GetServiceId() != testServiceID2 {
		t.Fatalf("Current changed after caller mutation: %+v", view)
	}
	if !proto.Equal(original, view.Snapshot) {
		t.Fatalf("owned Snapshot = %v, want %v", view.Snapshot, original)
	}
	encoded, err := deterministic.MarshalSnapshot(original)
	if err != nil {
		t.Fatal(err)
	}
	if view.Digest != sha256.Sum256(encoded) {
		t.Fatalf("Digest = %x, want %x", view.Digest, sha256.Sum256(encoded))
	}
	view.Snapshot.Revision = 100
	again, _ := manager.Current()
	if again.Revision != 2 || again.Snapshot.GetRevision() != 2 {
		t.Fatal("Current returned an aliased Snapshot")
	}
	closeManager(t, manager)
	if newResources.retireCalls.Load() != 1 {
		t.Fatalf("current Resources Retire calls = %d, want 1", newResources.retireCalls.Load())
	}
}

func TestApplySortsOwnedSnapshotWithoutMutatingInput(t *testing.T) {
	builder := &fakeBuilder{}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	snapshot := &protocolv1.TunnelSnapshot{TunnelId: testTunnelID, Revision: 3, Services: []*protocolv1.ServiceConfig{
		{ServiceId: testServiceID2, RequiredRevision: 3},
		{ServiceId: testServiceID, RequiredRevision: 2},
	}}
	if err := session.Apply(context.Background(), snapshot, &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Services[0].GetServiceId(); got != testServiceID2 {
		t.Fatalf("input order changed, first = %q", got)
	}
	built := builder.snapshot(0)
	if built.Services[0].GetServiceId() != testServiceID || built.Services[1].GetServiceId() != testServiceID2 {
		t.Fatalf("Builder input was not sorted: %v", built.Services)
	}
	closeManager(t, manager)
}

func TestBuilderCannotMutateManagerOwnedSnapshot(t *testing.T) {
	var retained *protocolv1.TunnelSnapshot
	builder := &fakeBuilder{inputHook: func(snapshot *protocolv1.TunnelSnapshot) {
		retained = snapshot
		snapshot.Revision = 99
		snapshot.Services[0].ServiceId = testServiceID2
	}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	if err := session.Apply(context.Background(), testSnapshot(2, testServiceID), &fakeSink{}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	view, ok := manager.Current()
	if !ok || view.Revision != 2 || view.Snapshot.GetRevision() != 2 ||
		view.Snapshot.GetServices()[0].GetServiceId() != testServiceID {
		t.Fatalf("Current() after Builder mutation = (%+v, %v)", view, ok)
	}
	retained.Revision = 100
	retained.Services[0].ServiceId = testServiceID2
	again, _ := manager.Current()
	if again.Revision != 2 || again.Snapshot.GetRevision() != 2 ||
		again.Snapshot.GetServices()[0].GetServiceId() != testServiceID {
		t.Fatalf("Current() changed after retained Builder input mutation: %+v", again)
	}
	closeManager(t, manager)
}

func TestApplyRejectsInvalidSnapshotsBeforeBuild(t *testing.T) {
	tests := []struct {
		name     string
		snapshot func() *protocolv1.TunnelSnapshot
		config   func(*Config)
	}{
		{name: "nil", snapshot: func() *protocolv1.TunnelSnapshot { return nil }},
		{name: "wrong tunnel", snapshot: func() *protocolv1.TunnelSnapshot {
			value := testSnapshot(1, testServiceID)
			value.TunnelId = testTunnelID2
			return value
		}},
		{name: "invalid tunnel", snapshot: func() *protocolv1.TunnelSnapshot {
			value := testSnapshot(1, testServiceID)
			value.TunnelId = "tun_bad"
			return value
		}},
		{name: "nil service", snapshot: func() *protocolv1.TunnelSnapshot {
			value := testSnapshot(1, testServiceID)
			value.Services[0] = nil
			return value
		}},
		{name: "invalid service", snapshot: func() *protocolv1.TunnelSnapshot { return testSnapshot(1, "svc_bad") }},
		{name: "duplicate service", snapshot: func() *protocolv1.TunnelSnapshot {
			value := testSnapshot(1, testServiceID)
			value.Services = append(value.Services, proto.Clone(value.Services[0]).(*protocolv1.ServiceConfig))
			return value
		}},
		{name: "future required revision", snapshot: func() *protocolv1.TunnelSnapshot {
			value := testSnapshot(1, testServiceID)
			value.Services[0].RequiredRevision = 2
			return value
		}},
		{name: "service count", snapshot: func() *protocolv1.TunnelSnapshot {
			value := testSnapshot(1, testServiceID)
			value.Services = append(value.Services, &protocolv1.ServiceConfig{ServiceId: testServiceID2, RequiredRevision: 1})
			return value
		}, config: func(config *Config) { config.MaxServices = 1 }},
		{name: "snapshot bytes", snapshot: func() *protocolv1.TunnelSnapshot { return testSnapshot(1, testServiceID) }, config: func(config *Config) { config.MaxSnapshotBytes = 1 }},
		{name: "envelope bytes", snapshot: func() *protocolv1.TunnelSnapshot { return testSnapshot(1, testServiceID) }, config: func(config *Config) { config.MaxControlFrameBytes = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder := &fakeBuilder{}
			config := testConfig(builder)
			if test.config != nil {
				test.config(&config)
			}
			manager := newTestManager(t, config)
			session := newTestSession(t, manager, testTunnelID)
			sink := &fakeSink{}
			err := session.Apply(context.Background(), test.snapshot(), sink)
			if !errors.Is(err, ErrConfigRejected) || builder.calls.Load() != 0 {
				t.Fatalf("Apply() = %v, Build calls=%d, want rejected before Build", err, builder.calls.Load())
			}
			ack := sink.onlyAck(t)
			if ack.GetObservedRevision() != 0 || ack.GetApplyStatus() != protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED ||
				ack.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR {
				t.Fatalf("REJECTED Ack = %+v", ack)
			}
			if _, ok := manager.Current(); ok {
				t.Fatal("invalid Snapshot was published")
			}
			closeManager(t, manager)
		})
	}
}

func TestUnknownFieldIsFatalWithoutAck(t *testing.T) {
	builder := &fakeBuilder{}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	snapshot := testSnapshot(1, testServiceID)
	snapshot.Services[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
	sink := &fakeSink{}
	err := session.Apply(context.Background(), snapshot, sink)
	if !errors.Is(err, ErrProtocolViolation) || !errors.Is(err, protocolvalidate.ErrUnknownFields) {
		t.Fatalf("Apply() error = %v", err)
	}
	if sink.count() != 0 || builder.calls.Load() != 0 {
		t.Fatalf("unknown field emitted Ack or called Builder: acks=%d builds=%d", sink.count(), builder.calls.Load())
	}
	closeManager(t, manager)
}

func TestBuildAndStartFailuresAbortThenRejectAtObservedRevision(t *testing.T) {
	buildFailure := errors.New("build failed")
	startFailure := errors.New("start failed")
	tests := []struct {
		name      string
		configure func(*fakeBuilder, *fakeCandidate)
	}{
		{name: "build", configure: func(builder *fakeBuilder, candidate *fakeCandidate) {
			builder.buildErrors = []error{nil, buildFailure}
			builder.candidates = []*fakeCandidate{nil, candidate}
		}},
		{name: "start", configure: func(builder *fakeBuilder, candidate *fakeCandidate) {
			builder.startErrors = []error{nil, startFailure}
			builder.candidates = []*fakeCandidate{nil, candidate}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := &fakeCandidate{resources: &fakeResources{retired: make(chan struct{})}}
			builder := &fakeBuilder{}
			test.configure(builder, candidate)
			manager := newTestManager(t, testConfig(builder))
			session := newTestSession(t, manager, testTunnelID)
			if err := session.Apply(context.Background(), testSnapshot(7, testServiceID), &fakeSink{}); err != nil {
				t.Fatal(err)
			}
			before, _ := manager.Current()
			sink := &fakeSink{}
			err := session.Apply(context.Background(), testSnapshot(8, testServiceID2), sink)
			if !errors.Is(err, ErrConfigRejected) {
				t.Fatalf("Apply() error = %v, want ErrConfigRejected", err)
			}
			if candidate.abortCalls.Load() != 1 {
				t.Fatalf("Abort calls = %d, want 1", candidate.abortCalls.Load())
			}
			after, _ := manager.Current()
			if after.Revision != before.Revision || after.Digest != before.Digest || !proto.Equal(after.Snapshot, before.Snapshot) {
				t.Fatalf("current changed after rejection: before=%+v after=%+v", before, after)
			}
			ack := sink.onlyAck(t)
			if ack.GetObservedRevision() != 7 || ack.GetApplyStatus() != protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_REJECTED ||
				ack.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR {
				t.Fatalf("REJECTED Ack = %+v", ack)
			}
			closeManager(t, manager)
		})
	}
}

func TestAbortFailureDoesNotAckOrPublish(t *testing.T) {
	abortFailure := errors.New("abort failed")
	candidate := &fakeCandidate{abortErr: abortFailure, resources: &fakeResources{retired: make(chan struct{})}}
	builder := &fakeBuilder{buildErrors: []error{errors.New("build failed")}, candidates: []*fakeCandidate{candidate}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	sink := &fakeSink{}
	err := session.Apply(context.Background(), testSnapshot(1, testServiceID), sink)
	if !errors.Is(err, ErrCandidateCleanup) || !errors.Is(err, abortFailure) {
		t.Fatalf("Apply() error = %v", err)
	}
	if sink.count() != 0 {
		t.Fatal("cleanup failure emitted REJECTED Ack")
	}
	if _, ok := manager.Current(); ok {
		t.Fatal("failed Candidate was published")
	}
	closeManager(t, manager)
}

func TestAckFailureKeepsNewGateInactiveAndDefersAllOldRetirement(t *testing.T) {
	resources := []*fakeResources{
		{retired: make(chan struct{})}, {retired: make(chan struct{})}, {retired: make(chan struct{})},
	}
	builder := &fakeBuilder{resources: []Resources{resources[0], resources[1], resources[2]}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	if err := session.Apply(context.Background(), testSnapshot(1, testServiceID), &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	ackFailure := errors.New("outbox full")
	if err := session.Apply(context.Background(), testSnapshot(2, testServiceID2), &fakeSink{err: ackFailure}); !errors.Is(err, ErrAckEnqueue) || !errors.Is(err, ackFailure) || errors.Is(err, ErrConfigRejected) {
		t.Fatalf("second Apply() error = %v", err)
	}
	if builder.gate(1).Active() {
		t.Fatal("Ack-failed Candidate Gate became active")
	}
	view, _ := manager.Current()
	if view.Revision != 2 || view.Acked {
		t.Fatalf("current after Ack failure = %+v", view)
	}
	assertNotClosed(t, resources[0].retired, "revision 1 retired after Ack failure")
	if revision, _, ok := session.Observed(); !ok || revision != 1 {
		t.Fatalf("Observed() after Ack failure = (%d, %v), want revision 1", revision, ok)
	}

	if err := session.Apply(context.Background(), testSnapshot(3, testServiceID), &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, resources[0].retired, "revision 1 retirement")
	waitClosed(t, resources[1].retired, "revision 2 retirement")
	if resources[0].retireCalls.Load() != 1 || resources[1].retireCalls.Load() != 1 {
		t.Fatalf("pending Retire calls = (%d, %d), want exactly once",
			resources[0].retireCalls.Load(), resources[1].retireCalls.Load())
	}
	if !builder.gate(2).Active() {
		t.Fatal("revision 3 Gate inactive after Ack")
	}
	closeManager(t, manager)
	if resources[2].retireCalls.Load() != 1 {
		t.Fatalf("current Retire calls = %d, want 1", resources[2].retireCalls.Load())
	}
}

func TestCancellationAfterStartAbortsWithoutPublishingOrAck(t *testing.T) {
	applyContext, cancelApply := context.WithCancel(context.Background())
	candidate := &fakeCandidate{
		resources: &fakeResources{retired: make(chan struct{})},
		startHook: cancelApply,
	}
	builder := &fakeBuilder{candidates: []*fakeCandidate{candidate}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	sink := &fakeSink{}
	err := session.Apply(applyContext, testSnapshot(1, testServiceID), sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply() error = %v, want context cancellation", err)
	}
	if candidate.startCalls.Load() != 1 || candidate.abortCalls.Load() != 1 {
		t.Fatalf("Candidate calls: Start=%d Abort=%d, want exactly once", candidate.startCalls.Load(), candidate.abortCalls.Load())
	}
	if sink.count() != 0 {
		t.Fatal("canceled Candidate emitted Ack")
	}
	if _, ok := manager.Current(); ok {
		t.Fatal("canceled Candidate was published")
	}
	closeManager(t, manager)
}

func TestCancellationReturnedByBuildOrStartDoesNotRejectSnapshot(t *testing.T) {
	tests := []struct {
		name      string
		configure func(context.CancelFunc, *fakeBuilder, *fakeCandidate)
	}{
		{name: "build", configure: func(cancel context.CancelFunc, builder *fakeBuilder, candidate *fakeCandidate) {
			builder.buildHook = func(int) { cancel() }
			builder.buildErrors = []error{context.Canceled}
			builder.candidates = []*fakeCandidate{candidate}
		}},
		{name: "start", configure: func(cancel context.CancelFunc, builder *fakeBuilder, candidate *fakeCandidate) {
			candidate.startHook = cancel
			builder.startErrors = []error{context.Canceled}
			builder.candidates = []*fakeCandidate{candidate}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applyContext, cancelApply := context.WithCancel(context.Background())
			candidate := &fakeCandidate{resources: &fakeResources{retired: make(chan struct{})}}
			builder := &fakeBuilder{}
			test.configure(cancelApply, builder, candidate)
			manager := newTestManager(t, testConfig(builder))
			session := newTestSession(t, manager, testTunnelID)
			sink := &fakeSink{}

			err := session.Apply(applyContext, testSnapshot(1, testServiceID), sink)
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrConfigRejected) {
				t.Fatalf("Apply() error = %v, want cancellation without ConfigRejected", err)
			}
			if candidate.abortCalls.Load() != 1 {
				t.Fatalf("Abort calls = %d, want 1", candidate.abortCalls.Load())
			}
			if sink.count() != 0 {
				t.Fatal("canceled Apply emitted ConfigAck")
			}
			if _, exists := manager.Current(); exists {
				t.Fatal("canceled Candidate was published")
			}
			closeManager(t, manager)
		})
	}
}

func TestManagerParentCancellationDuringStartDoesNotRejectSnapshot(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	candidate := &fakeCandidate{
		resources: &fakeResources{retired: make(chan struct{})},
		startErr:  context.Canceled,
		startContextHook: func(ctx context.Context) {
			cancelParent()
			<-ctx.Done()
		},
	}
	builder := &fakeBuilder{candidates: []*fakeCandidate{candidate}}
	manager, err := New(parent, testConfig(builder))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	session := newTestSession(t, manager, testTunnelID)
	sink := &fakeSink{}

	err = session.Apply(context.Background(), testSnapshot(1, testServiceID), sink)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrConfigRejected) {
		t.Fatalf("Apply() error = %v, want owner cancellation without ConfigRejected", err)
	}
	if candidate.abortCalls.Load() != 1 {
		t.Fatalf("Abort calls = %d, want 1", candidate.abortCalls.Load())
	}
	if sink.count() != 0 {
		t.Fatal("owner-canceled Apply emitted ConfigAck")
	}
	if _, exists := manager.Current(); exists {
		t.Fatal("owner-canceled Candidate was published")
	}
	closeManager(t, manager)
}

func TestPublishedCandidateLifetimeBelongsToManager(t *testing.T) {
	stopped := make(chan struct{})
	candidate := &fakeCandidate{
		resources: &fakeResources{retired: make(chan struct{})},
		startContextHook: func(ctx context.Context) {
			go func() {
				<-ctx.Done()
				close(stopped)
			}()
		},
	}
	builder := &fakeBuilder{candidates: []*fakeCandidate{candidate}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	if err := session.Apply(context.Background(), testSnapshot(1, testServiceID), &fakeSink{}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertNotClosed(t, stopped, "published Candidate stopped when Apply returned")
	closeManager(t, manager)
	waitClosed(t, stopped, "published Candidate lifetime cancellation")
}

func TestCloseWinsBeforePublishAndAbortsCandidate(t *testing.T) {
	runtimeEntered := make(chan struct{})
	releaseRuntime := make(chan struct{})
	candidate := &fakeCandidate{
		resources: &fakeResources{retired: make(chan struct{})},
		runtimeHook: func() {
			close(runtimeEntered)
			<-releaseRuntime
		},
	}
	builder := &fakeBuilder{candidates: []*fakeCandidate{candidate}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	sink := &fakeSink{}
	applyResult := make(chan error, 1)
	go func() {
		applyResult <- session.Apply(context.Background(), testSnapshot(1, testServiceID), sink)
	}()
	waitClosed(t, runtimeEntered, "Candidate Runtime boundary")
	closeResult := make(chan error, 1)
	go func() { closeResult <- manager.Close(context.Background()) }()
	for {
		if _, err := manager.NewSession(testTunnelID); errors.Is(err, ErrClosed) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseRuntime)
	if err := <-applyResult; !errors.Is(err, ErrClosed) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply() error = %v, want Close cancellation", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if candidate.abortCalls.Load() != 1 || sink.count() != 0 {
		t.Fatalf("Close race calls: Abort=%d Ack=%d", candidate.abortCalls.Load(), sink.count())
	}
	if _, ok := manager.Current(); ok {
		t.Fatal("Candidate published after Close won commit fence")
	}
}

func TestRetireTimeoutAndCloseReturnCleanupErrors(t *testing.T) {
	timedOut := &fakeResources{retired: make(chan struct{}), retire: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	current := &fakeResources{retired: make(chan struct{})}
	builder := &fakeBuilder{resources: []Resources{timedOut, current}}
	config := testConfig(builder)
	config.RetireTimeout = 20 * time.Millisecond
	manager := newTestManager(t, config)
	session := newTestSession(t, manager, testTunnelID)
	if err := session.Apply(context.Background(), testSnapshot(1, testServiceID), &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Apply(context.Background(), testSnapshot(2, testServiceID2), &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, timedOut.retired, "timed-out retire completion")
	err := manager.Close(context.Background())
	if !errors.Is(err, ErrRetire) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want recorded retire deadline", err)
	}
	waitClosed(t, current.retired, "current retirement on Close")
	if _, ok := manager.Current(); ok {
		t.Fatal("Current remains published after Close")
	}
	if _, err := manager.NewSession(testTunnelID); !errors.Is(err, ErrClosed) {
		t.Fatalf("NewSession after Close error = %v", err)
	}
}

func TestRetirePanicIsReturnedByCloseWithoutBlocking(t *testing.T) {
	panicked := &fakeResources{retired: make(chan struct{}), retire: func(context.Context) error {
		panic("retire panic must not escape its goroutine")
	}}
	current := &fakeResources{retired: make(chan struct{})}
	manager := newTestManager(t, testConfig(&fakeBuilder{resources: []Resources{panicked, current}}))
	session := newTestSession(t, manager, testTunnelID)
	if err := session.Apply(context.Background(), testSnapshot(1, testServiceID), &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Apply(context.Background(), testSnapshot(2, testServiceID2), &fakeSink{}); err != nil {
		t.Fatal(err)
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- manager.Close(context.Background()) }()
	select {
	case err := <-closeResult:
		if !errors.Is(err, ErrRetire) || !errors.Is(err, safego.ErrPanic) {
			t.Fatalf("Close() error = %v, want ErrRetire and safego.ErrPanic", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() blocked after Resources.Retire panic")
	}
	waitClosed(t, current.retired, "current retirement after peer panic")
	manager.statesMu.Lock()
	retiring := len(manager.retiring)
	manager.statesMu.Unlock()
	if retiring != 0 {
		t.Fatalf("retiring resources = %d, want 0", retiring)
	}
}

func TestCloseDeadlineStillWaitsForOwnedRetireGoroutines(t *testing.T) {
	firstStarted := make(chan struct{})
	firstFinished := make(chan struct{})
	first := &fakeResources{retired: make(chan struct{}), retire: func(ctx context.Context) error {
		close(firstStarted)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		close(firstFinished)
		return nil
	}}
	current := &fakeResources{retired: make(chan struct{}), retire: func(ctx context.Context) error {
		if ctx.Err() == nil {
			return errors.New("Close did not cancel manager-owned retire context")
		}
		return nil
	}}
	builder := &fakeBuilder{resources: []Resources{first, current}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)
	if err := session.Apply(context.Background(), testSnapshot(1, testServiceID), &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Apply(context.Background(), testSnapshot(2, testServiceID2), &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, firstStarted, "first retire start")
	closeContext, cancelClose := context.WithCancel(context.Background())
	cancelClose()
	err := manager.Close(closeContext)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want caller cancellation", err)
	}
	waitClosed(t, firstFinished, "first retire finish before Close return")
	waitClosed(t, first.retired, "first Retire return")
	waitClosed(t, current.retired, "current Retire return")
}

func TestNewSessionResetsObservedAndAcceptsLowerRevision(t *testing.T) {
	builder := &fakeBuilder{}
	manager := newTestManager(t, testConfig(builder))
	first := newTestSession(t, manager, testTunnelID)
	if err := first.Apply(context.Background(), testSnapshot(10, testServiceID), &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	second := newTestSession(t, manager, testTunnelID)
	if _, _, ok := second.Observed(); ok {
		t.Fatal("new Session inherited observed baseline")
	}
	if err := second.Apply(context.Background(), testSnapshot(2, testServiceID2), &fakeSink{}); err != nil {
		t.Fatalf("new Session lower revision Apply() error = %v", err)
	}
	if revision, _, ok := second.Observed(); !ok || revision != 2 {
		t.Fatalf("second Observed() = (%d, %v)", revision, ok)
	}
	if revision, _, ok := first.Observed(); !ok || revision != 10 {
		t.Fatalf("first Session baseline changed = (%d, %v)", revision, ok)
	}
	closeManager(t, manager)
}

func TestApplyRevisionAndDigestSemanticsAvoidDuplicateBuild(t *testing.T) {
	firstResources := &fakeResources{retired: make(chan struct{})}
	secondResources := &fakeResources{retired: make(chan struct{})}
	builder := &fakeBuilder{resources: []Resources{firstResources, secondResources}}
	manager := newTestManager(t, testConfig(builder))
	session := newTestSession(t, manager, testTunnelID)

	original := testSnapshot(10, testServiceID)
	if err := session.Apply(context.Background(), original, &fakeSink{}); err != nil {
		t.Fatal(err)
	}
	before, ok := manager.Current()
	if !ok {
		t.Fatal("Current() missing after initial Apply")
	}

	duplicateSink := &fakeSink{}
	if err := session.Apply(context.Background(), proto.Clone(original).(*protocolv1.TunnelSnapshot), duplicateSink); err != nil {
		t.Fatalf("idempotent Apply() error = %v", err)
	}
	duplicateAck := duplicateSink.onlyAck(t)
	if duplicateAck.GetObservedRevision() != 10 ||
		duplicateAck.GetApplyStatus() != protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED ||
		duplicateAck.GetErrorCode() != protocolv1.ErrorCode_ERROR_CODE_OK {
		t.Fatalf("idempotent ConfigAck = %#v", duplicateAck)
	}
	if builder.calls.Load() != 1 || firstResources.retireCalls.Load() != 0 {
		t.Fatalf("idempotent Apply work: builds=%d retires=%d, want 1 and 0", builder.calls.Load(), firstResources.retireCalls.Load())
	}

	for _, test := range []struct {
		name     string
		snapshot *protocolv1.TunnelSnapshot
	}{
		{name: "same revision different digest", snapshot: testSnapshot(10, testServiceID2)},
		{name: "older revision", snapshot: testSnapshot(9, testServiceID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink := &fakeSink{}
			if err := session.Apply(context.Background(), test.snapshot, sink); !errors.Is(err, ErrProtocolViolation) {
				t.Fatalf("Apply() error = %v, want ErrProtocolViolation", err)
			}
			if sink.count() != 0 || builder.calls.Load() != 1 {
				t.Fatalf("protocol violation side effects: acks=%d builds=%d", sink.count(), builder.calls.Load())
			}
			after, current := manager.Current()
			if !current || after.Revision != before.Revision || after.Digest != before.Digest || !after.Acked {
				t.Fatalf("Current() changed after protocol violation: before=%+v after=%+v current=%v", before, after, current)
			}
		})
	}

	ackFailure := errors.New("outbox unavailable")
	if err := session.Apply(context.Background(), original, &fakeSink{err: ackFailure}); !errors.Is(err, ErrAckEnqueue) || !errors.Is(err, ackFailure) {
		t.Fatalf("idempotent Ack failure = %v", err)
	}
	if builder.calls.Load() != 1 || firstResources.retireCalls.Load() != 0 {
		t.Fatalf("failed idempotent Ack work: builds=%d retires=%d", builder.calls.Load(), firstResources.retireCalls.Load())
	}

	if err := session.Apply(context.Background(), testSnapshot(11, testServiceID2), &fakeSink{}); err != nil {
		t.Fatalf("higher revision Apply() error = %v", err)
	}
	waitClosed(t, firstResources.retired, "revision 10 retirement")
	if builder.calls.Load() != 2 || firstResources.retireCalls.Load() != 1 {
		t.Fatalf("higher revision work: builds=%d retires=%d, want 2 and 1", builder.calls.Load(), firstResources.retireCalls.Load())
	}
	if revision, _, observed := session.Observed(); !observed || revision != 11 {
		t.Fatalf("Observed() after higher revision = (%d, %v)", revision, observed)
	}

	closeManager(t, manager)
	if secondResources.retireCalls.Load() != 1 {
		t.Fatalf("current Resources Retire calls = %d, want 1", secondResources.retireCalls.Load())
	}
}

func TestNewSessionReusesMatchingCurrentWithoutBuildOrRetire(t *testing.T) {
	resources := &fakeResources{retired: make(chan struct{})}
	builder := &fakeBuilder{resources: []Resources{resources}}
	manager := newTestManager(t, testConfig(builder))
	first := newTestSession(t, manager, testTunnelID)
	snapshot := testSnapshot(10, testServiceID)
	if err := first.Apply(context.Background(), snapshot, &fakeSink{}); err != nil {
		t.Fatal(err)
	}

	second := newTestSession(t, manager, testTunnelID)
	sink := &fakeSink{}
	if err := second.Apply(context.Background(), proto.Clone(snapshot).(*protocolv1.TunnelSnapshot), sink); err != nil {
		t.Fatalf("new Session matching Apply() error = %v", err)
	}
	ack := sink.onlyAck(t)
	if ack.GetObservedRevision() != 10 || ack.GetApplyStatus() != protocolv1.ConfigApplyStatus_CONFIG_APPLY_STATUS_APPLIED {
		t.Fatalf("new Session ConfigAck = %#v", ack)
	}
	if revision, _, observed := second.Observed(); !observed || revision != 10 {
		t.Fatalf("second Observed() = (%d, %v)", revision, observed)
	}
	if builder.calls.Load() != 1 || resources.retireCalls.Load() != 0 {
		t.Fatalf("matching current reuse work: builds=%d retires=%d", builder.calls.Load(), resources.retireCalls.Load())
	}

	closeManager(t, manager)
}

func TestNewSessionReusesAckFailedCurrentAndRetiresPendingOnce(t *testing.T) {
	oldResources := &fakeResources{retired: make(chan struct{})}
	currentResources := &fakeResources{retired: make(chan struct{})}
	builder := &fakeBuilder{resources: []Resources{oldResources, currentResources}}
	manager := newTestManager(t, testConfig(builder))
	first := newTestSession(t, manager, testTunnelID)
	if err := first.Apply(context.Background(), testSnapshot(1, testServiceID), &fakeSink{}); err != nil {
		t.Fatal(err)
	}

	ackFailure := errors.New("outbox unavailable")
	revisionTwo := testSnapshot(2, testServiceID2)
	if err := first.Apply(context.Background(), revisionTwo, &fakeSink{err: ackFailure}); !errors.Is(err, ErrAckEnqueue) || !errors.Is(err, ackFailure) {
		t.Fatalf("revision 2 Ack failure = %v", err)
	}
	if builder.gate(1).Active() {
		t.Fatal("Ack-failed current Gate became active")
	}
	assertNotClosed(t, oldResources.retired, "old Resources retired before replacement Session Ack")

	second := newTestSession(t, manager, testTunnelID)
	if err := second.Apply(context.Background(), proto.Clone(revisionTwo).(*protocolv1.TunnelSnapshot), &fakeSink{}); err != nil {
		t.Fatalf("new Session retry Apply() error = %v", err)
	}
	if builder.calls.Load() != 2 {
		t.Fatalf("Build calls = %d, want 2 without rebuilding Ack-failed current", builder.calls.Load())
	}
	if !builder.gate(1).Active() {
		t.Fatal("reused current Gate inactive after APPLIED Ack")
	}
	waitClosed(t, oldResources.retired, "old Resources retirement after replacement Session Ack")
	if oldResources.retireCalls.Load() != 1 {
		t.Fatalf("old Resources Retire calls = %d, want 1", oldResources.retireCalls.Load())
	}
	if revision, _, observed := second.Observed(); !observed || revision != 2 {
		t.Fatalf("second Observed() = (%d, %v)", revision, observed)
	}

	closeManager(t, manager)
	if currentResources.retireCalls.Load() != 1 {
		t.Fatalf("current Resources Retire calls = %d, want 1", currentResources.retireCalls.Load())
	}
}

func TestConcurrentApplyFailsFastAndCurrentReadersSeeWholeTuples(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	builder := &fakeBuilder{buildHook: func(call int) {
		if call == 0 {
			close(entered)
			<-release
		}
	}}
	manager := newTestManager(t, testConfig(builder))
	first := newTestSession(t, manager, testTunnelID)
	second := newTestSession(t, manager, testTunnelID)
	firstResult := make(chan error, 1)
	go func() { firstResult <- first.Apply(context.Background(), testSnapshot(1, testServiceID), &fakeSink{}) }()
	waitClosed(t, entered, "first Build entry")
	if err := second.Apply(context.Background(), testSnapshot(2, testServiceID2), &fakeSink{}); !errors.Is(err, ErrConcurrentApply) {
		t.Fatalf("concurrent Apply() error = %v", err)
	}
	close(release)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}

	stopReaders := make(chan struct{})
	readerErrors := make(chan error, 8)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				view, ok := manager.Current()
				if !ok {
					continue
				}
				encoded, err := deterministic.MarshalSnapshot(view.Snapshot)
				if err != nil {
					readerErrors <- err
					return
				}
				if view.Revision != view.Snapshot.GetRevision() || view.Digest != sha256.Sum256(encoded) {
					readerErrors <- fmt.Errorf("torn tuple: revision=%d snapshot=%d digest=%x", view.Revision, view.Snapshot.GetRevision(), view.Digest)
					return
				}
			}
		}()
	}
	for revision := uint64(2); revision <= 30; revision++ {
		serviceID := testServiceID
		if revision%2 == 0 {
			serviceID = testServiceID2
		}
		if err := first.Apply(context.Background(), testSnapshot(revision, serviceID), &fakeSink{}); err != nil {
			t.Fatal(err)
		}
	}
	close(stopReaders)
	readers.Wait()
	close(readerErrors)
	for err := range readerErrors {
		t.Fatal(err)
	}
	closeManager(t, manager)
}

func testConfig(builder Builder) Config {
	return Config{
		ProtocolVersion: 1, MaxServices: 10, MaxSnapshotBytes: MaxSnapshotSize,
		MaxControlFrameBytes: 1 << 20, RetireTimeout: time.Second, Builder: builder,
	}
}

func testSnapshot(revision uint64, serviceID string) *protocolv1.TunnelSnapshot {
	return &protocolv1.TunnelSnapshot{
		TunnelId: testTunnelID, Revision: revision,
		Services: []*protocolv1.ServiceConfig{{ServiceId: serviceID, RequiredRevision: revision}},
	}
}

func newTestManager(t *testing.T, config Config) *Manager {
	t.Helper()
	manager, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

func newTestSession(t *testing.T, manager *Manager, tunnelID string) *Session {
	t.Helper()
	session, err := manager.NewSession(tunnelID)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	return session
}

func closeManager(t *testing.T, manager *Manager) {
	t.Helper()
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func waitClosed(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func assertNotClosed(t *testing.T, channel <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-channel:
		t.Fatal(description)
	case <-time.After(20 * time.Millisecond):
	}
}

type fakeBuilder struct {
	mu          sync.Mutex
	calls       atomic.Int32
	resources   []Resources
	buildErrors []error
	startErrors []error
	candidates  []*fakeCandidate
	buildHook   func(int)
	inputHook   func(*protocolv1.TunnelSnapshot)
	gates       []Gate
	snapshots   []*protocolv1.TunnelSnapshot
}

func (builder *fakeBuilder) Build(_ context.Context, snapshot *protocolv1.TunnelSnapshot, gate Gate) (Candidate, error) {
	call := int(builder.calls.Add(1) - 1)
	if builder.inputHook != nil {
		builder.inputHook(snapshot)
	}
	if builder.buildHook != nil {
		builder.buildHook(call)
	}
	builder.mu.Lock()
	defer builder.mu.Unlock()
	builder.gates = append(builder.gates, gate)
	builder.snapshots = append(builder.snapshots, proto.Clone(snapshot).(*protocolv1.TunnelSnapshot))
	var candidate *fakeCandidate
	if call < len(builder.candidates) && builder.candidates[call] != nil {
		candidate = builder.candidates[call]
	} else {
		resources := Resources(&fakeResources{retired: make(chan struct{})})
		if call < len(builder.resources) {
			resources = builder.resources[call]
		}
		candidate = &fakeCandidate{resources: resources}
	}
	candidate.gate = gate
	if call < len(builder.startErrors) {
		candidate.startErr = builder.startErrors[call]
	}
	if call < len(builder.buildErrors) {
		return candidate, builder.buildErrors[call]
	}
	return candidate, nil
}

func (builder *fakeBuilder) gate(index int) Gate {
	builder.mu.Lock()
	defer builder.mu.Unlock()
	return builder.gates[index]
}

func (builder *fakeBuilder) snapshot(index int) *protocolv1.TunnelSnapshot {
	builder.mu.Lock()
	defer builder.mu.Unlock()
	return proto.Clone(builder.snapshots[index]).(*protocolv1.TunnelSnapshot)
}

type fakeCandidate struct {
	gate             Gate
	resources        Resources
	startErr         error
	startHook        func()
	startContextHook func(context.Context)
	runtimeHook      func()
	abortErr         error
	startCalls       atomic.Int32
	abortCalls       atomic.Int32
}

func (candidate *fakeCandidate) Start(ctx context.Context) error {
	candidate.startCalls.Add(1)
	if candidate.gate.Active() {
		return errors.New("Gate active during Candidate Start")
	}
	if candidate.startHook != nil {
		candidate.startHook()
	}
	if candidate.startContextHook != nil {
		candidate.startContextHook(ctx)
	}
	return candidate.startErr
}

func (candidate *fakeCandidate) Abort(context.Context) error {
	candidate.abortCalls.Add(1)
	return candidate.abortErr
}

func (candidate *fakeCandidate) Runtime() Resources {
	if candidate.runtimeHook != nil {
		candidate.runtimeHook()
	}
	return candidate.resources
}

type fakeResources struct {
	once        sync.Once
	retireCalls atomic.Int32
	retired     chan struct{}
	retire      func(context.Context) error
}

func (resources *fakeResources) Retire(ctx context.Context) error {
	resources.retireCalls.Add(1)
	var err error
	resources.once.Do(func() {
		if resources.retire != nil {
			err = resources.retire(ctx)
		}
		close(resources.retired)
	})
	return err
}

type fakeSink struct {
	mu        sync.Mutex
	envelopes []*protocolv1.ControlEnvelope
	err       error
	onEnqueue func(*protocolv1.ControlEnvelope) error
}

func (sink *fakeSink) Enqueue(envelope *protocolv1.ControlEnvelope) error {
	sink.mu.Lock()
	sink.envelopes = append(sink.envelopes, proto.Clone(envelope).(*protocolv1.ControlEnvelope))
	sink.mu.Unlock()
	if sink.onEnqueue != nil {
		return sink.onEnqueue(envelope)
	}
	return sink.err
}

func (sink *fakeSink) count() int {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return len(sink.envelopes)
}

func (sink *fakeSink) onlyAck(t *testing.T) *protocolv1.ConfigAck {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.envelopes) != 1 || sink.envelopes[0].GetConfigAck() == nil {
		t.Fatalf("Ack envelopes = %v, want exactly one ConfigAck", sink.envelopes)
	}
	return sink.envelopes[0].GetConfigAck()
}
