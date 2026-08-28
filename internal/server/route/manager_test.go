package route

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
)

type fakeSource struct {
	loadCalls       atomic.Int32
	generationCalls atomic.Int32
	load            func(context.Context, int) (repository.RouteDesiredState, error)
	generation      func(context.Context, int) (uint64, error)
}

func (source *fakeSource) LoadRouteDesiredState(ctx context.Context) (repository.RouteDesiredState, error) {
	call := int(source.loadCalls.Add(1))
	return source.load(ctx, call)
}

func (source *fakeSource) CurrentRouteGeneration(ctx context.Context) (uint64, error) {
	call := int(source.generationCalls.Add(1))
	return source.generation(ctx, call)
}

func TestManagerGenerationFenceDiscardsOldCandidate(t *testing.T) {
	secondLoadStarted := make(chan struct{})
	releaseSecondLoad := make(chan struct{})
	source := &fakeSource{
		load: func(ctx context.Context, call int) (repository.RouteDesiredState, error) {
			switch call {
			case 1:
				return validDesiredState(1), nil
			case 2:
				close(secondLoadStarted)
				select {
				case <-releaseSecondLoad:
					state := validDesiredState(2)
					state.HTTPRoutes[0].PathPrefix = "/new"
					return state, nil
				case <-ctx.Done():
					return repository.RouteDesiredState{}, ctx.Err()
				}
			default:
				return repository.RouteDesiredState{}, fmt.Errorf("unexpected load call %d", call)
			}
		},
		generation: func(_ context.Context, call int) (uint64, error) {
			if call == 1 {
				return 2, nil
			}
			return 2, nil
		},
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(t, manager, cancel)

	startResult := make(chan error, 1)
	go func() { startResult <- manager.Start(ctx) }()
	waitSignal(t, secondLoadStarted, "second generation load")
	if current := manager.Current(); current != nil {
		t.Fatalf("Current() = generation %d before latest candidate completed; old candidate was published", current.Generation())
	}
	close(releaseSecondLoad)
	if err := waitError(t, startResult, "manager start"); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got := manager.Current().Generation(); got != 2 {
		t.Fatalf("published generation = %d, want 2", got)
	}
	routes, _ := manager.Current().HTTP("app.example.com")
	if got := routes.Routes()[0].PathPrefix; got != "/new" {
		t.Fatalf("published route path = %q, want /new", got)
	}
	if got := source.loadCalls.Load(); got != 2 {
		t.Fatalf("LoadRouteDesiredState calls = %d, want 2", got)
	}
}

func TestManagerDirtyGenerationFencesNotificationAfterDatabaseRead(t *testing.T) {
	var manager *Manager
	source := &fakeSource{
		load: func(_ context.Context, call int) (repository.RouteDesiredState, error) {
			if call == 1 {
				return validDesiredState(1), nil
			}
			if call == 2 {
				return validDesiredState(2), nil
			}
			return repository.RouteDesiredState{}, fmt.Errorf("unexpected load call %d", call)
		},
		generation: func(_ context.Context, call int) (uint64, error) {
			if call == 1 {
				// 模拟事务已提交、generation 查询仍线性化在旧提交，但写入协调器已在
				// atomic.Store 前送达新代次通知。旧候选必须被内存 fence 拦下。
				manager.MarkDirty(2)
				return 1, nil
			}
			return 2, nil
		},
	}
	manager = newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	defer stopManager(t, manager, cancel)

	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got := manager.Current().Generation(); got != 2 {
		t.Fatalf("published generation = %d, want dirty-fenced generation 2", got)
	}
	if got := source.loadCalls.Load(); got != 2 {
		t.Fatalf("LoadRouteDesiredState calls = %d, want old candidate discarded then latest reload", got)
	}
}

func TestManagerDirtyBurstCoalescesDuringBuild(t *testing.T) {
	secondLoadStarted := make(chan struct{})
	releaseSecondLoad := make(chan struct{})
	source := &fakeSource{
		load: func(ctx context.Context, call int) (repository.RouteDesiredState, error) {
			if call == 1 {
				return validDesiredState(1), nil
			}
			if call != 2 {
				return repository.RouteDesiredState{}, fmt.Errorf("unexpected load call %d", call)
			}
			close(secondLoadStarted)
			select {
			case <-releaseSecondLoad:
				return validDesiredState(2), nil
			case <-ctx.Done():
				return repository.RouteDesiredState{}, ctx.Err()
			}
		},
		generation: func(_ context.Context, call int) (uint64, error) {
			if call == 1 {
				return 1, nil
			}
			return 2, nil
		},
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	defer stopManager(t, manager, cancel)

	manager.MarkDirty(2)
	waitSignal(t, secondLoadStarted, "dirty rebuild")
	for range 100 {
		manager.MarkDirty(2)
	}
	close(releaseSecondLoad)
	waitFor(t, "generation 2 publish", func() bool {
		current := manager.Current()
		return current != nil && current.Generation() == 2
	})
	// 给 owner 一个调度机会；若构建期间的 burst 未被合并，这里会出现第 3 次读取。
	time.Sleep(20 * time.Millisecond)
	if got := source.loadCalls.Load(); got != 2 {
		t.Fatalf("LoadRouteDesiredState calls = %d, want initial + one coalesced rebuild", got)
	}
}

func TestManagerBuildFailureKeepsOldSnapshotAndLaterDirtyConverges(t *testing.T) {
	failedBuildObserved := make(chan struct{})
	source := &fakeSource{
		load: func(_ context.Context, call int) (repository.RouteDesiredState, error) {
			switch call {
			case 1:
				return validDesiredState(1), nil
			case 2:
				state := validDesiredState(2)
				state.HTTPRoutes[0].Hostname = "new.example.com"
				duplicate := state.TCPRoutes[0]
				duplicate.ID = "tcp-duplicate"
				state.TCPRoutes = append(state.TCPRoutes, duplicate)
				close(failedBuildObserved)
				return state, nil
			case 3:
				state := validDesiredState(3)
				state.HTTPRoutes[0].Hostname = "recovered.example.com"
				return state, nil
			default:
				return repository.RouteDesiredState{}, fmt.Errorf("unexpected load call %d", call)
			}
		},
		generation: func(_ context.Context, call int) (uint64, error) {
			switch call {
			case 1:
				return 1, nil
			case 2:
				return 3, nil
			default:
				return 3, nil
			}
		},
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	defer stopManager(t, manager, cancel)

	old := manager.Current()
	manager.MarkDirty(2)
	waitSignal(t, failedBuildObserved, "failed build")
	waitFor(t, "LastError", func() bool { return manager.LastError() != nil })
	if current := manager.Current(); current != old || current.Generation() != 1 {
		t.Fatalf("Current() after failed build = %p generation %d, want old %p generation 1", current, current.Generation(), old)
	}
	if _, ok := manager.Current().HTTP("new.example.com"); ok {
		t.Fatal("partial HTTP candidate was published after later TCP validation failed")
	}

	manager.MarkDirty(3)
	waitFor(t, "generation 3 recovery", func() bool {
		current := manager.Current()
		return current != nil && current.Generation() == 3 && manager.LastError() == nil
	})
	if _, ok := manager.Current().HTTP("recovered.example.com"); !ok {
		t.Fatal("recovered full snapshot was not published")
	}
}

func TestManagerRejectsGenerationRollbackBelowPublishedSnapshot(t *testing.T) {
	source := &fakeSource{
		load: func(_ context.Context, call int) (repository.RouteDesiredState, error) {
			if call == 1 {
				return validDesiredState(3), nil
			}
			return validDesiredState(2), nil
		},
		generation: func(_ context.Context, call int) (uint64, error) {
			if call == 1 {
				return 3, nil
			}
			return 2, nil
		},
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	defer stopManager(t, manager, cancel)

	published := manager.Current()
	manager.MarkDirty(4)
	waitFor(t, "generation rollback error", func() bool {
		return errors.Is(manager.LastError(), ErrInvalidDesiredState)
	})
	if current := manager.Current(); current != published || current.Generation() != 3 {
		t.Fatalf("Current() after rollback = %p generation %d, want retained %p generation 3", current, current.Generation(), published)
	}
}

func TestManagerRetriesFailedGenerationWhenMarkedDirtyAgain(t *testing.T) {
	wantErr := errors.New("temporary SQLite read failure")
	source := &fakeSource{
		load: func(_ context.Context, call int) (repository.RouteDesiredState, error) {
			switch call {
			case 1:
				return validDesiredState(1), nil
			case 2:
				return repository.RouteDesiredState{}, wantErr
			case 3:
				return validDesiredState(2), nil
			default:
				return repository.RouteDesiredState{}, fmt.Errorf("unexpected load call %d", call)
			}
		},
		generation: func(_ context.Context, call int) (uint64, error) {
			if call == 1 {
				return 1, nil
			}
			return 2, nil
		},
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	defer stopManager(t, manager, cancel)

	manager.MarkDirty(2)
	waitFor(t, "temporary generation 2 failure", func() bool {
		return errors.Is(manager.LastError(), wantErr)
	})
	if got := manager.Current().Generation(); got != 1 {
		t.Fatalf("generation after temporary failure = %d, want retained 1", got)
	}

	manager.MarkDirty(2)
	waitFor(t, "same generation retry", func() bool {
		return manager.Current().Generation() == 2 && manager.LastError() == nil
	})
	if got := source.loadCalls.Load(); got != 3 {
		t.Fatalf("LoadRouteDesiredState calls = %d, want initial + failure + same-generation retry", got)
	}
}

func TestManagerPublishedGenerationBecomesDirtyNotificationFloor(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(3)
	source := &fakeSource{
		load: func(_ context.Context, _ int) (repository.RouteDesiredState, error) {
			return validDesiredState(generation.Load()), nil
		},
		generation: func(_ context.Context, _ int) (uint64, error) {
			return generation.Load(), nil
		},
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	defer stopManager(t, manager, cancel)

	manager.MarkDirty(2)
	manager.MarkDirty(3)
	if queued := len(manager.dirty); queued != 0 {
		t.Fatalf("stale/equal generation queued %d wakeups, want 0", queued)
	}
	if got := source.loadCalls.Load(); got != 1 {
		t.Fatalf("LoadRouteDesiredState calls after stale/equal notifications = %d, want initial load only", got)
	}

	generation.Store(4)
	manager.MarkDirty(4)
	waitFor(t, "generation above published floor", func() bool {
		return manager.Current().Generation() == 4
	})
	if got := source.loadCalls.Load(); got != 2 {
		t.Fatalf("LoadRouteDesiredState calls after generation 4 = %d, want one rebuild", got)
	}
}

func TestManagerPublishedQueriesNeverReadSource(t *testing.T) {
	source := &fakeSource{
		load: func(_ context.Context, _ int) (repository.RouteDesiredState, error) {
			return validDesiredState(1), nil
		},
		generation: func(_ context.Context, _ int) (uint64, error) { return 1, nil },
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	defer stopManager(t, manager, cancel)

	loads := source.loadCalls.Load()
	generationReads := source.generationCalls.Load()
	for range 100 {
		snapshot := manager.Current()
		_, _ = snapshot.HTTP("app.example.com")
		_, _ = snapshot.TCP(8443)
		_, _ = snapshot.Tunnel(testTunnelID)
	}
	if got := source.loadCalls.Load(); got != loads {
		t.Fatalf("hot-path queries caused Source loads: before=%d after=%d", loads, got)
	}
	if got := source.generationCalls.Load(); got != generationReads {
		t.Fatalf("hot-path queries caused generation reads: before=%d after=%d", generationReads, got)
	}
}

func TestManagerCancellationUnblocksSourceAndWaitsForOwner(t *testing.T) {
	secondLoadStarted := make(chan struct{})
	source := &fakeSource{
		load: func(ctx context.Context, call int) (repository.RouteDesiredState, error) {
			if call == 1 {
				return validDesiredState(1), nil
			}
			close(secondLoadStarted)
			<-ctx.Done()
			return repository.RouteDesiredState{}, ctx.Err()
		},
		generation: func(_ context.Context, _ int) (uint64, error) { return 1, nil },
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	manager.MarkDirty(2)
	waitSignal(t, secondLoadStarted, "blocking Source load")
	cancel()
	waitManager(t, manager)
	if got := manager.Current().Generation(); got != 1 {
		t.Fatalf("Current generation after cancellation = %d, want retained 1", got)
	}
}

func TestManagerInitialFailurePublishesNothingAndTerminates(t *testing.T) {
	wantErr := errors.New("database unavailable")
	source := &fakeSource{
		load: func(_ context.Context, _ int) (repository.RouteDesiredState, error) {
			return repository.RouteDesiredState{}, wantErr
		},
		generation: func(_ context.Context, _ int) (uint64, error) {
			t.Fatal("CurrentRouteGeneration called after load failure")
			return 0, nil
		},
	}
	manager := newTestManager(t, source)
	if err := manager.Start(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Start() error = %v, want wrapped database error", err)
	}
	if manager.Current() != nil {
		t.Fatal("Current() is non-nil after initial failure")
	}
	waitManager(t, manager)
}

func newTestManager(t *testing.T, source Source) *Manager {
	t.Helper()
	manager, err := NewManager(source)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager
}

func stopManager(t *testing.T, manager *Manager, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	waitManager(t, manager)
}

func waitManager(t *testing.T, manager *Manager) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		manager.Wait()
		close(done)
	}()
	waitSignal(t, done, "route manager exit")
}

func waitSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func waitError(t *testing.T, result <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		return nil
	}
}

func waitFor(t *testing.T, operation string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", operation)
}

// 并发读测试持续跨越多次原子发布，Race Detector 会验证 Snapshot 发布后没有写入，
// 同时具体断言读者最终都能观察到最后 generation，而不是只检查“没有崩溃”。
func TestManagerConcurrentReadersObserveAtomicPublications(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(1)
	source := &fakeSource{
		load: func(_ context.Context, _ int) (repository.RouteDesiredState, error) {
			return validDesiredState(generation.Load()), nil
		},
		generation: func(_ context.Context, _ int) (uint64, error) { return generation.Load(), nil },
	}
	manager := newTestManager(t, source)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start() error = %v", err)
	}
	defer stopManager(t, manager, cancel)

	const readers = 8
	var wait sync.WaitGroup
	wait.Add(readers)
	start := make(chan struct{})
	readErrors := make(chan error, readers)
	for range readers {
		go func() {
			defer wait.Done()
			<-start
			for range 5_000 {
				current := manager.Current()
				if current == nil {
					readErrors <- errors.New("atomic snapshot unexpectedly nil")
					return
				}
				host, httpOK := current.HTTP("app.example.com")
				tcp, tcpOK := current.TCP(8443)
				tunnel, tunnelOK := current.Tunnel(testTunnelID)
				if !httpOK || len(host.Routes()) != 1 || !tcpOK || !tunnelOK ||
					tcp.TunnelID != tunnel.TunnelID {
					readErrors <- fmt.Errorf("reader observed incomplete generation %d", current.Generation())
					return
				}
			}
		}()
	}
	close(start)
	for next := uint64(2); next <= 20; next++ {
		generation.Store(next)
		manager.MarkDirty(next)
		waitFor(t, fmt.Sprintf("generation %d", next), func() bool {
			return manager.Current().Generation() == next
		})
	}
	wait.Wait()
	close(readErrors)
	for err := range readErrors {
		t.Error(err)
	}
	if got := manager.Current().Generation(); got != 20 {
		t.Fatalf("final generation = %d, want 20", got)
	}
}
