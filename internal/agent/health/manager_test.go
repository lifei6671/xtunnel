package health

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/configruntime"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/safego"
)

const testServiceID = "svc_01J00000000000000000000000"

type testGate struct{ active atomic.Bool }

func (gate *testGate) Active() bool { return gate.active.Load() }

type unusedDialer struct{}

func (*unusedDialer) DialOrigin(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
	return nil, protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE, errors.New("unused dialer")
}

func TestTargetStateTransitions(t *testing.T) {
	current := &target{
		spec:  targetSpec{serviceRevision: 7, failureThreshold: 2, successThreshold: 2},
		state: State{Status: protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN, ServiceRevision: 7},
	}
	now := time.Unix(100, 0)
	current.apply(now, observation{success: true})
	if current.state.Status != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY {
		t.Fatalf("UNKNOWN first success status = %s", current.state.Status)
	}
	current.apply(now, observation{failure: FailureOrigin})
	current.apply(now, observation{success: true})
	current.apply(now, observation{failure: FailureOrigin})
	if current.state.Status != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY {
		t.Fatalf("opposite result did not reset failure streak: %s", current.state.Status)
	}
	current.apply(now, observation{failure: FailureOrigin})
	if current.state.Status != protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY {
		t.Fatalf("failure threshold status = %s", current.state.Status)
	}
	current.apply(now, observation{success: true})
	current.apply(now, observation{failure: FailureOrigin})
	current.apply(now, observation{success: true})
	if current.state.Status != protocolv1.HealthStatus_HEALTH_STATUS_UNHEALTHY {
		t.Fatalf("opposite result did not reset success streak: %s", current.state.Status)
	}
	current.apply(now, observation{success: true})
	if current.state.Status != protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY {
		t.Fatalf("success threshold status = %s", current.state.Status)
	}
}

func TestTargetJitterAndBudgetMiss(t *testing.T) {
	now := time.Unix(100, 0)
	current := &target{
		spec:               targetSpec{interval: 10 * time.Second, serviceRevision: 3},
		state:              State{Status: protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY, ServiceRevision: 3},
		consecutiveFailure: 2,
	}
	current.scheduleInitial(now, .5)
	if got := current.due.Sub(now); got != 5*time.Second {
		t.Fatalf("initial jitter = %s", got)
	}
	current.scheduleNext(now, 0)
	if got := current.due.Sub(now); got != 8*time.Second {
		t.Fatalf("minimum interval jitter = %s", got)
	}
	current.scheduleNext(now, 1)
	if got := current.due.Sub(now); got != 12*time.Second {
		t.Fatalf("maximum interval jitter = %s", got)
	}
	if got := current.latestDue.Sub(now); got != 20*time.Second {
		t.Fatalf("maximum stale deadline = %s", got)
	}
	gate := &testGate{}
	gate.active.Store(true)
	manager := newManager(defaultOptions())
	scheduler := scheduler{
		manager: manager, active: &plan{gate: gate}, queue: targetHeap{current},
		nextPermit: current.latestDue.Add(time.Second),
	}
	// 持续限流可以把任务推迟到 due 之后，但不能越过 2*interval 的
	// stale deadline；到达绝对截止时间时必须直接 fail closed。
	scheduler.dispatch(current.latestDue)
	if current.state.Status != protocolv1.HealthStatus_HEALTH_STATUS_UNKNOWN ||
		current.state.Failure != FailureBudget ||
		current.state.OriginErrorCode != protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED ||
		current.consecutiveFailure != 0 {
		t.Fatalf("budget miss state = %#v, failures=%d", current.state, current.consecutiveFailure)
	}
}

func TestCandidateUnregisterCanRetryAfterCanceledSend(t *testing.T) {
	options := defaultOptions()
	options.workers = 1
	options.globalLimit = 1
	options.gatePoll = time.Millisecond
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := &testGate{}
	candidateValue := prepareCandidate(t, manager, testSnapshot(testServiceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP), gate)
	candidate := candidateValue.(*candidate)
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	resources := candidate.Runtime()
	if resources == nil {
		t.Fatal("Candidate.Runtime() = nil")
	}

	// 让 Owner 阻塞在 command response，并填满队列，确保已取消的首次
	// Retire 无法入队，而不是与可写 channel 随机竞争。
	blockResponse := make(chan error)
	manager.commands <- command{kind: commandValidate, plan: candidate.plan, response: blockResponse}
	waitFor(t, func() bool { return len(manager.commands) == 0 })
	for len(manager.commands) < cap(manager.commands) {
		manager.commands <- command{
			kind: commandValidate, plan: candidate.plan, response: make(chan error, 1),
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := resources.Retire(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Retire() error = %v, want context.Canceled", err)
	}
	<-blockResponse
	if err := resources.Retire(context.Background()); err != nil {
		t.Fatalf("second Retire() error = %v", err)
	}
	gate.active.Store(true)
	waitFor(t, func() bool {
		_, exists := manager.State(testServiceID)
		return !exists
	})
	shutdownManager(t, manager)
}

func TestManagerGatePreservesUnchangedState(t *testing.T) {
	var checks atomic.Int32
	options := defaultOptions()
	options.workers = 1
	options.globalLimit = 1
	options.rateInterval = 0
	options.gatePoll = time.Millisecond
	options.random = func() float64 { return 0 }
	options.checker = func(context.Context, targetSpec, OriginDialer) observation {
		checks.Add(1)
		return observation{success: true}
	}
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := &testGate{}
	candidate := prepareCandidate(t, manager, testSnapshot(testServiceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP), gate)
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := manager.State(testServiceID); exists {
		t.Fatal("unpublished plan exposed state")
	}
	gate.active.Store(true)
	waitFor(t, func() bool {
		state, exists := manager.State(testServiceID)
		return exists && state.Status == protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY
	})
	gate.active.Store(false)
	if _, exists := manager.State(testServiceID); exists {
		t.Fatal("inactive Gate did not hide state immediately")
	}
	nextGate := &testGate{}
	nextCandidate := prepareCandidate(t, manager, testSnapshot(testServiceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP), nextGate)
	if err := nextCandidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	nextGate.active.Store(true)
	waitFor(t, func() bool {
		state, exists := manager.State(testServiceID)
		return exists && state.Status == protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY
	})
	time.Sleep(20 * time.Millisecond)
	if got := checks.Load(); got != 1 {
		t.Fatalf("unchanged new plan restarted check: calls=%d", got)
	}
	shutdownManager(t, manager)
}

func TestManagerRejectsSameRevisionChangedFingerprint(t *testing.T) {
	options := defaultOptions()
	options.workers = 1
	options.globalLimit = 1
	options.gatePoll = time.Millisecond
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := prepareCandidate(t, manager, testSnapshot(testServiceID, 4, protocolv1.HealthType_HEALTH_TYPE_HTTP), &testGate{})
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed := testSnapshot(testServiceID, 4, protocolv1.HealthType_HEALTH_TYPE_HTTP)
	changed.Services[0].Health.Path = "/changed"
	if _, err := manager.Prepare(context.Background(), changed, &testGate{}, &unusedDialer{}); !errors.Is(err, configruntime.ErrProtocolViolation) {
		t.Fatalf("Prepare() error = %v, want ErrProtocolViolation", err)
	}
	shutdownManager(t, manager)
}

func TestManagerFencesOldResult(t *testing.T) {
	oldReleased := make(chan struct{})
	started := make(chan struct{}, 2)
	var calls atomic.Int32
	options := defaultOptions()
	options.workers = 2
	options.globalLimit = 2
	options.rateInterval = 0
	options.gatePoll = time.Millisecond
	options.random = func() float64 { return 0 }
	options.checker = func(context.Context, targetSpec, OriginDialer) observation {
		call := calls.Add(1)
		started <- struct{}{}
		if call == 1 {
			<-oldReleased
			return observation{success: true}
		}
		return observation{failure: FailureOrigin, originCode: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_RESET}
	}
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldGate := &testGate{}
	oldCandidate := prepareCandidate(t, manager, testSnapshot(testServiceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP), oldGate)
	if err := oldCandidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldGate.active.Store(true)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old check did not start")
	}
	oldGate.active.Store(false)
	newGate := &testGate{}
	newCandidate := prepareCandidate(t, manager, testSnapshot(testServiceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP), newGate)
	if err := newCandidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	newGate.active.Store(true)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("new check did not start")
	}
	waitFor(t, func() bool {
		state, exists := manager.State(testServiceID)
		return exists && state.Failure == FailureOrigin
	})
	close(oldReleased)
	time.Sleep(20 * time.Millisecond)
	state, _ := manager.State(testServiceID)
	if state.Status == protocolv1.HealthStatus_HEALTH_STATUS_HEALTHY || state.Failure != FailureOrigin {
		t.Fatalf("old result overwrote new state: %#v", state)
	}
	shutdownManager(t, manager)
}

func TestManagerEnforcesGlobalAndPerOriginConcurrency(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 16)
	options := defaultOptions()
	options.workers = 8
	options.globalLimit = 3
	options.perOriginLimit = 2
	options.rateInterval = 0
	options.gatePoll = time.Millisecond
	options.random = func() float64 { return 0 }
	options.checker = func(_ context.Context, spec targetSpec, _ OriginDialer) observation {
		started <- spec.originKey
		<-release
		return observation{success: true}
	}
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := &testGate{}
	snapshot := &protocolv1.TunnelSnapshot{}
	for index := 0; index < 6; index++ {
		serviceID := fmt.Sprintf("svc_01J0000000000000000000000%d", index)
		service := testService(serviceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP)
		if index >= 3 {
			service.OriginHost = fmt.Sprintf("origin-%d.test", index)
		}
		snapshot.Services = append(snapshot.Services, service)
	}
	candidate := prepareCandidate(t, manager, snapshot, gate)
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.active.Store(true)
	origins := make(map[string]int)
	for range 3 {
		select {
		case origin := <-started:
			origins[origin]++
		case <-time.After(time.Second):
			t.Fatal("expected three concurrent checks")
		}
	}
	select {
	case origin := <-started:
		t.Fatalf("global limit exceeded by %s", origin)
	case <-time.After(30 * time.Millisecond):
	}
	for origin, count := range origins {
		if count > 2 {
			t.Fatalf("per-origin limit exceeded for %s: %d", origin, count)
		}
	}
	close(release)
	shutdownManager(t, manager)
}

func TestManagerRateLimit(t *testing.T) {
	started := make(chan time.Time, 4)
	options := defaultOptions()
	options.workers = 4
	options.globalLimit = 4
	options.perOriginLimit = 4
	options.rateInterval = 25 * time.Millisecond
	options.gatePoll = time.Millisecond
	options.random = func() float64 { return 0 }
	options.checker = func(context.Context, targetSpec, OriginDialer) observation {
		started <- time.Now()
		return observation{success: true}
	}
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := &testGate{}
	snapshot := &protocolv1.TunnelSnapshot{}
	for index := 0; index < 3; index++ {
		snapshot.Services = append(snapshot.Services, testService(fmt.Sprintf("svc_01J0000000000000000000000%d", index), 1, protocolv1.HealthType_HEALTH_TYPE_TCP))
	}
	candidate := prepareCandidate(t, manager, snapshot, gate)
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.active.Store(true)
	values := make([]time.Time, 0, 3)
	for range 3 {
		select {
		case at := <-started:
			values = append(values, at)
		case <-time.After(time.Second):
			t.Fatal("rate-limited checks did not run")
		}
	}
	for index := 1; index < len(values); index++ {
		if gap := values[index].Sub(values[index-1]); gap < 18*time.Millisecond {
			t.Fatalf("rate gap[%d] = %s", index, gap)
		}
	}
	shutdownManager(t, manager)
}

func TestManagerWorkerPanicFailsOwner(t *testing.T) {
	options := defaultOptions()
	options.workers = 1
	options.globalLimit = 1
	options.rateInterval = 0
	options.gatePoll = time.Millisecond
	options.random = func() float64 { return 0 }
	options.checker = func(context.Context, targetSpec, OriginDialer) observation { panic("health checker bug") }
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := &testGate{}
	candidate := prepareCandidate(t, manager, testSnapshot(testServiceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP), gate)
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.active.Store(true)
	select {
	case <-manager.Done():
	case <-time.After(time.Second):
		t.Fatal("manager did not stop after worker panic")
	}
	if err := manager.Err(); !errors.Is(err, safego.ErrPanic) || !errors.Is(err, ErrScheduler) {
		t.Fatalf("Manager.Err() = %v", err)
	}
}

func TestManagerOwnerPanicCancelsWorkersBeforeWaiting(t *testing.T) {
	options := defaultOptions()
	options.workers = 1
	options.globalLimit = 1
	options.gatePoll = time.Millisecond
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	candidate, err := manager.Prepare(
		context.Background(), testSnapshot(testServiceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP),
		panicGate{}, &unusedDialer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.Start(context.Background()); err == nil {
		t.Fatal("Candidate.Start() unexpectedly succeeded")
	}
	select {
	case <-manager.Done():
	case <-time.After(time.Second):
		t.Fatal("owner panic deadlocked while waiting for workers")
	}
	if err := manager.Err(); !errors.Is(err, safego.ErrPanic) {
		t.Fatalf("Manager.Err() = %v", err)
	}
}

func TestShutdownDeadlineStillWaitsForOwnedChecker(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	options := defaultOptions()
	options.workers = 1
	options.globalLimit = 1
	options.rateInterval = 0
	options.gatePoll = time.Millisecond
	options.random = func() float64 { return 0 }
	options.checker = func(context.Context, targetSpec, OriginDialer) observation {
		started <- struct{}{}
		<-release
		return observation{success: true}
	}
	manager := newManager(options)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate := &testGate{}
	candidate := prepareCandidate(t, manager, testSnapshot(testServiceID, 1, protocolv1.HealthType_HEALTH_TYPE_TCP), gate)
	if err := candidate.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.active.Store(true)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("check did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	shutdownResult := make(chan error, 1)
	shutdownPanic := make(chan error, 1)
	safego.Go(func(err error) { shutdownPanic <- err }, nil, func() {
		shutdownResult <- manager.Shutdown(ctx)
	})
	<-ctx.Done()
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown() returned before checker exit: %v", err)
	case err := <-shutdownPanic:
		t.Fatalf("Shutdown goroutine panic: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-shutdownResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case err := <-shutdownPanic:
		t.Fatalf("Shutdown goroutine panic: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return after checker exited")
	}
	select {
	case <-manager.Done():
	default:
		t.Fatal("Shutdown returned before Manager.Done closed")
	}
}

type panicGate struct{}

func (panicGate) Active() bool { panic("gate bug") }

func TestHTTPCheckerUsesHostAndDoesNotReadBody(t *testing.T) {
	client, server := net.Pipe()
	dialer := &pipeDialer{connection: client}
	host := make(chan string, 1)
	serverDone := make(chan struct{})
	serverPanic := make(chan error, 1)
	safego.Go(func(err error) { serverPanic <- err }, func() { close(serverDone) }, func() {
		defer server.Close()
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			host <- "read-error"
			return
		}
		host <- request.Host
		_, _ = server.Write([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 1000000\r\n\r\n"))
		buffer := make([]byte, 1)
		_, _ = server.Read(buffer)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result := checkTarget(ctx, targetSpec{
		serviceID: testServiceID, healthType: protocolv1.HealthType_HEALTH_TYPE_HTTP,
		path: "/health?ready=1", hostHeader: "virtual.test", minimumStatus: 200, maximumStatus: 299,
	}, dialer)
	if !result.success {
		t.Fatalf("checkTarget() = %#v", result)
	}
	if got := <-host; got != "virtual.test" {
		t.Fatalf("Host = %q", got)
	}
	select {
	case <-serverDone:
	case err := <-serverPanic:
		t.Fatalf("server goroutine panic: %v", err)
	case <-time.After(time.Second):
		t.Fatal("server goroutine did not exit")
	}
}

func TestCheckerPassesThroughOriginErrorCode(t *testing.T) {
	dialer := errorDialer{
		code: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED,
		err:  errors.New("refused"),
	}
	result := checkTarget(context.Background(), targetSpec{serviceID: testServiceID}, dialer)
	if result.success || result.failure != FailureOrigin || result.originCode != dialer.code {
		t.Fatalf("checkTarget() = %#v", result)
	}
}

type errorDialer struct {
	code protocolv1.ErrorCode
	err  error
}

func (dialer errorDialer) DialOrigin(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
	return nil, dialer.code, dialer.err
}

func TestValidateHealthAndPath(t *testing.T) {
	tests := []struct {
		name   string
		health *protocolv1.HealthCheckConfig
	}{
		{name: "relative path", health: testHTTPHealth("health")},
		{name: "absolute URI", health: testHTTPHealth("http://origin.test/health")},
		{name: "header injection", health: testHTTPHealth("/health\r\nx: bad")},
		{name: "timeout equals interval", health: func() *protocolv1.HealthCheckConfig {
			value := testHTTPHealth("/health")
			value.TimeoutMs = value.IntervalMs
			return value
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateHealth(test.health); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("validateHealth() error = %v", err)
			}
		})
	}
}

type pipeDialer struct {
	mu         sync.Mutex
	connection net.Conn
}

func (dialer *pipeDialer) DialOrigin(context.Context, string) (net.Conn, protocolv1.ErrorCode, error) {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	connection := dialer.connection
	dialer.connection = nil
	return connection, protocolv1.ErrorCode_ERROR_CODE_OK, nil
}

func prepareCandidate(t *testing.T, manager *Manager, snapshot *protocolv1.TunnelSnapshot, gate *testGate) configruntime.Candidate {
	t.Helper()
	candidate, err := manager.Prepare(context.Background(), snapshot, gate, &unusedDialer{})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	return candidate
}

func testSnapshot(serviceID string, revision uint64, healthType protocolv1.HealthType) *protocolv1.TunnelSnapshot {
	return &protocolv1.TunnelSnapshot{Services: []*protocolv1.ServiceConfig{testService(serviceID, revision, healthType)}}
}

func testService(serviceID string, revision uint64, healthType protocolv1.HealthType) *protocolv1.ServiceConfig {
	health := &protocolv1.HealthCheckConfig{Type: healthType}
	switch healthType {
	case protocolv1.HealthType_HEALTH_TYPE_TCP:
		health.IntervalMs = 1_000
		health.TimeoutMs = 100
		health.FailureThreshold = 2
		health.SuccessThreshold = 2
	case protocolv1.HealthType_HEALTH_TYPE_HTTP:
		health = testHTTPHealth("/health")
	}
	return &protocolv1.ServiceConfig{
		ServiceId: serviceID, OriginScheme: "http", OriginHost: "origin.test", OriginPort: 8080,
		ConnectTimeoutMs: 500, TlsVerify: true, Health: health, Enabled: true, RequiredRevision: revision,
	}
}

func testHTTPHealth(path string) *protocolv1.HealthCheckConfig {
	return &protocolv1.HealthCheckConfig{
		Type: protocolv1.HealthType_HEALTH_TYPE_HTTP, Path: path, IntervalMs: 1_000, TimeoutMs: 100,
		ExpectedStatusMin: 200, ExpectedStatusMax: 399, FailureThreshold: 2, SuccessThreshold: 2,
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not reached")
}

func shutdownManager(t *testing.T, manager *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}
