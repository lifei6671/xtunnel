package application

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/tcpport"
)

const routeManagementIDOne = "tcp-route-one"

func TestRouteManagementAutoAllocatesAndAdvancesAllFences(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	snapshotNotifier := &recordingSnapshotNotifier{}
	owner := newServiceManagementTestServiceWithNotifier(
		store, gate, snapshotNotifier, serviceManagementIDOne,
	)
	createdService, err := owner.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "tcp"),
	)
	if err != nil {
		t.Fatalf("create Service error = %v", err)
	}
	policy, err := tcpport.New(10000, 10003, []uint16{10001})
	if err != nil {
		t.Fatalf("tcpport.New() error = %v", err)
	}
	routeNotifier := &recordingRouteNotifier{}
	service := NewRouteManagementService(owner, policy, routeNotifier)
	service.now = func() time.Time { return time.Unix(200, 0) }

	result, err := service.CreateTCP(context.Background(), CreateTCPRouteInput{
		ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID,
		ServiceID:             createdService.Service.ID,
		ExpectedTunnelVersion: createdService.TunnelVersion, ExpectedServiceVersion: createdService.Service.Version,
		PublicPort: 0, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateTCP() error = %v", err)
	}
	if result.Route.PublicPort != 10000 || result.Generation != 1 || !result.Changed ||
		result.Service.Version != 2 || result.Service.RequiredRevision != 2 ||
		result.TunnelVersion != 1 || result.TunnelRevision != 2 {
		t.Fatalf("CreateTCP() result = %+v", result)
	}
	state, err := store.LoadRouteDesiredState(context.Background())
	if err != nil {
		t.Fatalf("LoadRouteDesiredState() error = %v", err)
	}
	if state.Generation != 1 || len(state.TCPRoutes) != 1 || state.TCPRoutes[0] != result.Route {
		t.Fatalf("stored Route state = %+v", state)
	}
	storedService := readServiceManagementService(t, store, serviceManagementTunnelID, serviceManagementIDOne)
	if storedService.Version != 2 || storedService.RequiredRevision != 2 {
		t.Fatalf("stored Service = %+v, want version/revision 2", storedService)
	}
	if got := routeNotifier.snapshot(); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("Route MarkDirty calls = %v, want [1]", got)
	}
	if _, err := service.CreateTCP(context.Background(), CreateTCPRouteInput{
		ID: "tcp-route-stale", TunnelID: serviceManagementTunnelID, ServiceID: createdService.Service.ID,
		ExpectedTunnelVersion:  createdService.TunnelVersion,
		ExpectedServiceVersion: createdService.Service.Version,
		PublicPort:             10002,
		Enabled:                true,
	}); !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("CreateTCP(stale parent versions) error = %v, want ErrVersionConflict", err)
	}
	if current, err := store.LoadRouteDesiredState(context.Background()); err != nil ||
		current.Generation != 1 || len(current.TCPRoutes) != 1 {
		t.Fatalf("state after stale mutation = %+v, %v", current, err)
	}
	if got := snapshotNotifier.snapshot(); !reflect.DeepEqual(got, []string{serviceManagementTunnelID, serviceManagementTunnelID}) {
		t.Fatalf("Snapshot MarkDirty calls = %v", got)
	}
}

func TestRouteManagementRejectsPredictablePortErrorsBeforeTransaction(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	owner := newServiceManagementTestService(
		store, &recordingSnapshotGate{}, serviceManagementIDOne,
	)
	createdService, err := owner.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "tcp"),
	)
	if err != nil {
		t.Fatalf("create Service error = %v", err)
	}
	policy, err := tcpport.New(10000, 10002, []uint16{10001})
	if err != nil {
		t.Fatalf("tcpport.New() error = %v", err)
	}
	routeNotifier := &recordingRouteNotifier{}
	service := NewRouteManagementService(owner, policy, routeNotifier)
	service.now = func() time.Time { return time.Unix(200, 0) }
	first, err := service.CreateTCP(context.Background(), CreateTCPRouteInput{
		ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID, ServiceID: serviceManagementIDOne,
		ExpectedTunnelVersion: createdService.TunnelVersion, ExpectedServiceVersion: createdService.Service.Version,
		PublicPort: 10000, Enabled: false,
	})
	if err != nil {
		t.Fatalf("CreateTCP(first) error = %v", err)
	}

	counting := &routeCountingStore{Store: store}
	owner.store = counting
	tests := []struct {
		name string
		port uint16
	}{
		{name: "disabled Route still occupies", port: 10000},
		{name: "reserved", port: 10001},
		{name: "outside range", port: 9999},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := counting.withTxCalls
			_, err := service.CreateTCP(context.Background(), CreateTCPRouteInput{
				ID: "tcp-route-invalid-" + test.name, TunnelID: serviceManagementTunnelID,
				ServiceID:             serviceManagementIDOne,
				ExpectedTunnelVersion: first.TunnelVersion, ExpectedServiceVersion: first.Service.Version,
				PublicPort: test.port, Enabled: true,
			})
			if !errors.Is(err, ErrRouteManagementInput) {
				t.Fatalf("CreateTCP() error = %v, want ErrRouteManagementInput", err)
			}
			if counting.withTxCalls != before {
				t.Fatalf("WithTx calls = %d, want unchanged %d", counting.withTxCalls, before)
			}
		})
	}
	if got := routeNotifier.snapshot(); !reflect.DeepEqual(got, []uint64{1}) {
		t.Fatalf("Route MarkDirty calls after rejected writes = %v, want [1]", got)
	}
	second, err := service.CreateTCP(context.Background(), CreateTCPRouteInput{
		ID: "tcp-route-two", TunnelID: serviceManagementTunnelID, ServiceID: serviceManagementIDOne,
		ExpectedTunnelVersion: first.TunnelVersion, ExpectedServiceVersion: first.Service.Version,
		PublicPort: 10002, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateTCP(second) error = %v", err)
	}
	before := counting.withTxCalls
	if _, err := service.CreateTCP(context.Background(), CreateTCPRouteInput{
		ID: "tcp-route-exhausted", TunnelID: serviceManagementTunnelID, ServiceID: serviceManagementIDOne,
		ExpectedTunnelVersion: second.TunnelVersion, ExpectedServiceVersion: second.Service.Version,
		Enabled: true,
	}); !errors.Is(err, ErrRouteManagementInput) || !errors.Is(err, tcpport.ErrPoolExhausted) {
		t.Fatalf("CreateTCP(exhausted) error = %v", err)
	}
	if counting.withTxCalls != before {
		t.Fatalf("exhausted pool WithTx calls = %d, want unchanged %d", counting.withTxCalls, before)
	}
}

func TestRouteManagementUpdateAndDeleteRemainAtomic(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	owner := newServiceManagementTestService(
		store, &recordingSnapshotGate{}, serviceManagementIDOne,
	)
	createdService, err := owner.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "tcp"),
	)
	if err != nil {
		t.Fatalf("create Service error = %v", err)
	}
	policy, err := tcpport.New(10000, 10003, []uint16{10001})
	if err != nil {
		t.Fatalf("tcpport.New() error = %v", err)
	}
	notifier := &recordingRouteNotifier{}
	service := NewRouteManagementService(owner, policy, notifier)
	service.now = func() time.Time { return time.Unix(200, 0) }
	created, err := service.CreateTCP(context.Background(), CreateTCPRouteInput{
		ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID, ServiceID: serviceManagementIDOne,
		ExpectedTunnelVersion: createdService.TunnelVersion, ExpectedServiceVersion: createdService.Service.Version,
		PublicPort: 10000, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateTCP() error = %v", err)
	}
	noOp, err := service.UpdateTCP(context.Background(), UpdateTCPRouteInput{
		ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID, ServiceID: serviceManagementIDOne,
		ExpectedTunnelVersion: created.TunnelVersion, ExpectedServiceVersion: created.Service.Version,
		PublicPort: 10000, Enabled: true,
	})
	if err != nil || noOp.Changed || noOp.Generation != 1 {
		t.Fatalf("UpdateTCP(no-op) result/error = %+v/%v", noOp, err)
	}
	updated, err := service.UpdateTCP(context.Background(), UpdateTCPRouteInput{
		ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID, ServiceID: serviceManagementIDOne,
		ExpectedTunnelVersion: created.TunnelVersion, ExpectedServiceVersion: created.Service.Version,
		PublicPort: 10002, Enabled: false,
	})
	if err != nil {
		t.Fatalf("UpdateTCP() error = %v", err)
	}
	if updated.Route.PublicPort != 10002 || updated.Route.Enabled || updated.Service.Version != 3 ||
		updated.TunnelRevision != 3 || updated.Generation != 2 {
		t.Fatalf("UpdateTCP() result = %+v", updated)
	}
	deleted, err := service.DeleteTCP(context.Background(), DeleteTCPRouteInput{
		ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID, ServiceID: serviceManagementIDOne,
		ExpectedTunnelVersion: updated.TunnelVersion, ExpectedServiceVersion: updated.Service.Version,
	})
	if err != nil {
		t.Fatalf("DeleteTCP() error = %v", err)
	}
	if deleted.Service.Version != 4 || deleted.TunnelRevision != 4 || deleted.Generation != 3 {
		t.Fatalf("DeleteTCP() result = %+v", deleted)
	}
	state, err := store.LoadRouteDesiredState(context.Background())
	if err != nil {
		t.Fatalf("LoadRouteDesiredState() error = %v", err)
	}
	if state.Generation != 3 || len(state.TCPRoutes) != 0 || state.Services[0].RequiredRevision != 4 {
		t.Fatalf("final Route state = %+v", state)
	}
	if got := notifier.snapshot(); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("Route MarkDirty calls = %v, want [1 2 3]", got)
	}
}

func TestRouteManagementGateFailureRollsBackAllFencesAndBudget(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	gate := &recordingSnapshotGate{}
	owner := newServiceManagementTestService(store, gate, serviceManagementIDOne)
	createdService, err := owner.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "tcp"),
	)
	if err != nil {
		t.Fatalf("create Service error = %v", err)
	}
	policy, err := tcpport.New(10000, 10002, nil)
	if err != nil {
		t.Fatalf("tcpport.New() error = %v", err)
	}
	notifier := &recordingRouteNotifier{}
	service := NewRouteManagementService(owner, policy, notifier)
	gateErr := errors.New("injected route snapshot rejection")
	gate.setError(gateErr)
	input := CreateTCPRouteInput{
		ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID, ServiceID: createdService.Service.ID,
		ExpectedTunnelVersion:  createdService.TunnelVersion,
		ExpectedServiceVersion: createdService.Service.Version,
		Enabled:                true,
	}
	if _, err := service.CreateTCP(context.Background(), input); !errors.Is(err, gateErr) {
		t.Fatalf("CreateTCP(rejected) error = %v, want gate error", err)
	}
	state, err := store.LoadRouteDesiredState(context.Background())
	if err != nil {
		t.Fatalf("LoadRouteDesiredState() error = %v", err)
	}
	if state.Generation != 0 || len(state.TCPRoutes) != 0 || state.Services[0].Version != 1 ||
		state.Tunnels[0].DesiredRevision != 1 {
		t.Fatalf("state after rejected mutation = %+v", state)
	}
	if len(notifier.snapshot()) != 0 {
		t.Fatal("rejected mutation emitted Route dirty notification")
	}
	gate.setError(nil)
	if _, err := service.CreateTCP(context.Background(), input); err != nil {
		t.Fatalf("CreateTCP(after rollback) error = %v", err)
	}
}

func TestRouteManagementCommittedCleanupAndNotifierFailuresRemainObservable(t *testing.T) {
	tests := []struct {
		name            string
		postCommitError error
		notifierError   error
		wantError       error
	}{
		{
			name: "post-commit cleanup", postCommitError: errors.New("injected cleanup failure"),
			wantError: repository.ErrPostCommitCleanup,
		},
		{
			name: "Tunnel Snapshot notifier", notifierError: errors.New("injected notifier failure"),
			wantError: ErrRouteRuntimeConvergence,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openServiceManagementStore(t)
			seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
			tunnelNotifier := &recordingSnapshotNotifier{}
			owner := newServiceManagementTestServiceWithNotifier(
				store, &recordingSnapshotGate{}, tunnelNotifier, serviceManagementIDOne,
			)
			createdService, err := owner.Create(
				context.Background(), validCreateServiceInput(serviceManagementTunnelID, "tcp"),
			)
			if err != nil {
				t.Fatalf("create Service error = %v", err)
			}
			if test.postCommitError != nil {
				owner.store = &serviceManagementPostCommitCleanupStore{Store: store, err: test.postCommitError}
			}
			if test.notifierError != nil {
				tunnelNotifier.mu.Lock()
				tunnelNotifier.err = test.notifierError
				tunnelNotifier.mu.Unlock()
			}
			policy, err := tcpport.New(10000, 10002, nil)
			if err != nil {
				t.Fatalf("tcpport.New() error = %v", err)
			}
			routeNotifier := &recordingRouteNotifier{}
			service := NewRouteManagementService(owner, policy, routeNotifier)
			result, err := service.CreateTCP(context.Background(), CreateTCPRouteInput{
				ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID, ServiceID: createdService.Service.ID,
				ExpectedTunnelVersion:  createdService.TunnelVersion,
				ExpectedServiceVersion: createdService.Service.Version,
				Enabled:                true,
			})
			if !errors.Is(err, test.wantError) {
				t.Fatalf("CreateTCP() error = %v, want %v", err, test.wantError)
			}
			if !result.Changed || result.Generation != 1 || result.Route.PublicPort != 10000 {
				t.Fatalf("CreateTCP() committed result = %+v", result)
			}
			state, loadErr := store.LoadRouteDesiredState(context.Background())
			if loadErr != nil || state.Generation != 1 || len(state.TCPRoutes) != 1 {
				t.Fatalf("committed state/error = %+v/%v", state, loadErr)
			}
			if got := routeNotifier.snapshot(); !reflect.DeepEqual(got, []uint64{1}) {
				t.Fatalf("Route dirty calls = %v, want [1]", got)
			}
		})
	}
}

func TestRouteManagementConcurrentAutoAllocationDoesNotConflictOnGlobalGeneration(t *testing.T) {
	store := openServiceManagementStore(t)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelID)
	seedServiceManagementTunnel(t, store, serviceManagementTunnelIDTwo)
	owner := newServiceManagementTestService(
		store, &recordingSnapshotGate{}, serviceManagementIDOne, serviceManagementIDTwo,
	)
	first, err := owner.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelID, "tcp-one"),
	)
	if err != nil {
		t.Fatalf("create first Service error = %v", err)
	}
	second, err := owner.Create(
		context.Background(), validCreateServiceInput(serviceManagementTunnelIDTwo, "tcp-two"),
	)
	if err != nil {
		t.Fatalf("create second Service error = %v", err)
	}
	policy, err := tcpport.New(10000, 10003, nil)
	if err != nil {
		t.Fatalf("tcpport.New() error = %v", err)
	}
	notifier := &recordingRouteNotifier{}
	service := NewRouteManagementService(owner, policy, notifier)
	service.now = func() time.Time { return time.Unix(200, 0) }

	inputs := []CreateTCPRouteInput{
		{
			ID: routeManagementIDOne, TunnelID: serviceManagementTunnelID, ServiceID: first.Service.ID,
			ExpectedTunnelVersion: first.TunnelVersion, ExpectedServiceVersion: first.Service.Version,
			Enabled: true,
		},
		{
			ID: "tcp-route-two", TunnelID: serviceManagementTunnelIDTwo, ServiceID: second.Service.ID,
			ExpectedTunnelVersion: second.TunnelVersion, ExpectedServiceVersion: second.Service.Version,
			Enabled: true,
		},
	}
	start := make(chan struct{})
	results := make(chan TCPRouteMutationResult, len(inputs))
	errorsChannel := make(chan error, len(inputs))
	var wait sync.WaitGroup
	for _, input := range inputs {
		input := input
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := service.CreateTCP(context.Background(), input)
			results <- result
			errorsChannel <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("concurrent CreateTCP() error = %v", err)
		}
	}
	var ports []int
	var generations []int
	for result := range results {
		ports = append(ports, int(result.Route.PublicPort))
		generations = append(generations, int(result.Generation))
	}
	sort.Ints(ports)
	sort.Ints(generations)
	if !reflect.DeepEqual(ports, []int{10000, 10001}) || !reflect.DeepEqual(generations, []int{1, 2}) {
		t.Fatalf("ports/generations = %v/%v, want [10000 10001]/[1 2]", ports, generations)
	}
}

type recordingRouteNotifier struct {
	mu     sync.Mutex
	values []uint64
}

func (notifier *recordingRouteNotifier) MarkDirty(generation uint64) {
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	notifier.values = append(notifier.values, generation)
}

func (notifier *recordingRouteNotifier) snapshot() []uint64 {
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	return append([]uint64(nil), notifier.values...)
}

type routeCountingStore struct {
	repository.Store
	withTxCalls int
}

func (store *routeCountingStore) WithTx(ctx context.Context, fn func(repository.TxStore) error) error {
	store.withTxCalls++
	return store.Store.WithTx(ctx, fn)
}
