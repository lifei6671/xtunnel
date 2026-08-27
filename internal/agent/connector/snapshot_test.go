package connector

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	agenthealth "github.com/lifei6671/xtunnel/internal/agent/health"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

func TestSnapshotCandidateCoordinatesChildLifecycle(t *testing.T) {
	recorder := &lifecycleRecorder{}
	origin := newOrderedOriginCandidate(recorder, "origin")
	health := newOrderedCandidate(recorder, "health")
	builder := snapshotBuilder{
		origin: orderedBuilder{recorder: recorder, candidate: origin},
		health: orderedHealthPreparer{recorder: recorder, candidate: health, wantDialer: origin},
	}

	candidate, err := builder.Build(context.Background(), &protocolv1.TunnelSnapshot{}, inactiveGate{})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	resources := candidate.Runtime()
	if resources == nil {
		t.Fatal("Runtime() returned nil Resources")
	}
	if err := resources.Retire(context.Background()); err != nil {
		t.Fatalf("Retire() error = %v", err)
	}

	want := []string{
		"origin.build", "health.prepare",
		"origin.start", "health.start",
		"origin.runtime", "health.runtime",
		"health.retire", "origin.retire",
	}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle order = %v, want %v", got, want)
	}
}

func TestSnapshotCandidateAbortsChildrenInReverseOrderAndJoinsErrors(t *testing.T) {
	recorder := &lifecycleRecorder{}
	originErr := errors.New("origin abort failed")
	healthErr := errors.New("health abort failed")
	candidate := &snapshotCandidate{
		origin: &orderedCandidate{recorder: recorder, name: "origin", abortErr: originErr},
		health: &orderedCandidate{recorder: recorder, name: "health", abortErr: healthErr},
	}

	err := candidate.Abort(context.Background())
	if !errors.Is(err, healthErr) || !errors.Is(err, originErr) {
		t.Fatalf("Abort() error = %v, want both child errors", err)
	}
	if got, want := recorder.snapshot(), []string{"health.abort", "origin.abort"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Abort order = %v, want %v", got, want)
	}
}

func TestSnapshotResourcesRetireChildrenInReverseOrderAndJoinsErrors(t *testing.T) {
	recorder := &lifecycleRecorder{}
	originErr := errors.New("origin retire failed")
	healthErr := errors.New("health retire failed")
	resources := &snapshotResources{
		origin: &orderedResources{recorder: recorder, name: "origin", err: originErr},
		health: &orderedResources{recorder: recorder, name: "health", err: healthErr},
	}

	err := resources.Retire(context.Background())
	if !errors.Is(err, healthErr) || !errors.Is(err, originErr) {
		t.Fatalf("Retire() error = %v, want both child errors", err)
	}
	if got, want := recorder.snapshot(), []string{"health.retire", "origin.retire"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Retire order = %v, want %v", got, want)
	}
}

func TestSnapshotCandidateHealthStartFailureIsCleanedUpInReverseOrder(t *testing.T) {
	recorder := &lifecycleRecorder{}
	startErr := errors.New("health start failed")
	candidate := &snapshotCandidate{
		origin: newOrderedCandidate(recorder, "origin"),
		health: &orderedCandidate{recorder: recorder, name: "health", startErr: startErr},
	}

	if err := candidate.Start(context.Background()); !errors.Is(err, startErr) {
		t.Fatalf("Start() error = %v, want health failure", err)
	}
	if err := candidate.Abort(context.Background()); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}
	want := []string{"origin.start", "health.start", "health.abort", "origin.abort"}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle order = %v, want %v", got, want)
	}
}

func TestSnapshotBuilderRejectsOriginCandidateWithoutScopedDialer(t *testing.T) {
	recorder := &lifecycleRecorder{}
	origin := newOrderedCandidate(recorder, "origin")
	health := orderedHealthPreparer{recorder: recorder, candidate: newOrderedCandidate(recorder, "health")}
	builder := snapshotBuilder{origin: orderedBuilder{recorder: recorder, candidate: origin}, health: health}

	candidate, err := builder.Build(context.Background(), &protocolv1.TunnelSnapshot{}, inactiveGate{})
	if !errors.Is(err, ErrInvalidConfig) || candidate == nil {
		t.Fatalf("Build() = (%T, %v), want cleanup Candidate and ErrInvalidConfig", candidate, err)
	}
	if abortErr := candidate.Abort(context.Background()); abortErr != nil {
		t.Fatalf("Abort() error = %v", abortErr)
	}
	if got, want := recorder.snapshot(), []string{"origin.build", "origin.abort"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle order = %v, want %v", got, want)
	}
}

func TestSnapshotBuilderHealthPrepareFailureRetainsBothCandidatesForAbort(t *testing.T) {
	recorder := &lifecycleRecorder{}
	origin := newOrderedOriginCandidate(recorder, "origin")
	health := newOrderedCandidate(recorder, "health")
	prepareErr := errors.New("health prepare failed")
	builder := snapshotBuilder{
		origin: orderedBuilder{recorder: recorder, candidate: origin},
		health: orderedHealthPreparer{recorder: recorder, candidate: health, prepareErr: prepareErr},
	}

	candidate, err := builder.Build(context.Background(), &protocolv1.TunnelSnapshot{}, inactiveGate{})
	if !errors.Is(err, prepareErr) || candidate == nil {
		t.Fatalf("Build() = (%T, %v), want cleanup Candidate and prepare error", candidate, err)
	}
	if abortErr := candidate.Abort(context.Background()); abortErr != nil {
		t.Fatalf("Abort() error = %v", abortErr)
	}
	want := []string{"origin.build", "health.prepare", "health.abort", "origin.abort"}
	if got := recorder.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle order = %v, want %v", got, want)
	}
}

func TestSnapshotCandidateRuntimeRequiresBothResources(t *testing.T) {
	for _, test := range []struct {
		name      string
		originNil bool
		healthNil bool
	}{
		{name: "origin", originNil: true},
		{name: "health", healthNil: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := &snapshotCandidate{
				origin: &orderedCandidate{name: "origin", runtimeNil: test.originNil},
				health: &orderedCandidate{name: "health", runtimeNil: test.healthNil},
			}
			if resources := candidate.Runtime(); resources != nil {
				t.Fatalf("Runtime() = %T, want nil", resources)
			}
		})
	}
}

type inactiveGate struct{}

func (inactiveGate) Active() bool { return false }

type lifecycleRecorder struct {
	mu     sync.Mutex
	events []string
}

func (recorder *lifecycleRecorder) add(event string) {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
}

func (recorder *lifecycleRecorder) snapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.events...)
}

type orderedBuilder struct {
	recorder  *lifecycleRecorder
	candidate configruntime.Candidate
	err       error
}

func (builder orderedBuilder) Build(context.Context, *protocolv1.TunnelSnapshot, configruntime.Gate) (configruntime.Candidate, error) {
	builder.recorder.add("origin.build")
	return builder.candidate, builder.err
}

type orderedHealthPreparer struct {
	recorder   *lifecycleRecorder
	candidate  configruntime.Candidate
	prepareErr error
	wantDialer agenthealth.OriginDialer
}

func (preparer orderedHealthPreparer) Prepare(
	_ context.Context,
	_ *protocolv1.TunnelSnapshot,
	_ configruntime.Gate,
	dialer agenthealth.OriginDialer,
) (configruntime.Candidate, error) {
	preparer.recorder.add("health.prepare")
	if preparer.wantDialer != nil && dialer != preparer.wantDialer {
		return preparer.candidate, errors.New("health received the wrong scoped Origin dialer")
	}
	return preparer.candidate, preparer.prepareErr
}

type orderedCandidate struct {
	recorder   *lifecycleRecorder
	name       string
	startErr   error
	abortErr   error
	retireErr  error
	runtimeNil bool
}

func newOrderedCandidate(recorder *lifecycleRecorder, name string) *orderedCandidate {
	return &orderedCandidate{recorder: recorder, name: name}
}

func (candidate *orderedCandidate) Start(context.Context) error {
	candidate.recorder.add(candidate.name + ".start")
	return candidate.startErr
}

func (candidate *orderedCandidate) Abort(context.Context) error {
	candidate.recorder.add(candidate.name + ".abort")
	return candidate.abortErr
}

func (candidate *orderedCandidate) Runtime() configruntime.Resources {
	candidate.recorder.add(candidate.name + ".runtime")
	if candidate.runtimeNil {
		return nil
	}
	return &orderedResources{recorder: candidate.recorder, name: candidate.name, err: candidate.retireErr}
}

type orderedOriginCandidate struct {
	*orderedCandidate
}

func newOrderedOriginCandidate(recorder *lifecycleRecorder, name string) *orderedOriginCandidate {
	return &orderedOriginCandidate{orderedCandidate: newOrderedCandidate(recorder, name)}
}

func (*orderedOriginCandidate) DialOrigin(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
	return nil, protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, errors.New("test dialer")
}

type orderedResources struct {
	recorder *lifecycleRecorder
	name     string
	err      error
}

func (resources *orderedResources) Retire(context.Context) error {
	resources.recorder.add(resources.name + ".retire")
	return resources.err
}
