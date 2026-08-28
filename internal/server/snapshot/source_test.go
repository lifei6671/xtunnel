package snapshot

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

const sourceServiceID = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAY"

func TestSourceCurrentBuildsFullSnapshotFromOneConsistentView(t *testing.T) {
	service := sourceTestService("old.internal", 3)
	reader := &sourceTestReader{view: sourceTestView{
		tunnels: sourceTestTunnelRepository{get: func(context.Context, string) (repository.Tunnel, error) {
			return sourceTestTunnel(3), nil
		}},
		services: sourceTestServiceRepository{list: func(context.Context, string) ([]repository.Service, error) {
			return []repository.Service{service}, nil
		}},
	}}
	source, err := NewSource(reader, sourceTestBuilder(t))
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}

	result, err := source.Current(context.Background(), testTunnelID)
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("ReadConsistent() calls = %d, want 1", reader.calls)
	}
	if result.Snapshot.GetTunnelId() != testTunnelID || result.Snapshot.GetRevision() != 3 || len(result.Snapshot.GetServices()) != 1 {
		t.Fatalf("Current() Snapshot = %#v", result.Snapshot)
	}
	gotService := result.Snapshot.GetServices()[0]
	if gotService.GetServiceId() != sourceServiceID || gotService.GetOriginHost() != "old.internal" ||
		gotService.GetRequiredRevision() != 3 {
		t.Fatalf("Current() Service = %#v", gotService)
	}
	if len(result.DeterministicBytes) == 0 {
		t.Fatal("Current() deterministic bytes are empty")
	}
}

func TestSourceCurrentRejectsUnavailableOrInvalidState(t *testing.T) {
	sentinel := errors.New("read failed")
	revokedAt := int64(5)
	tests := []struct {
		name       string
		tunnel     repository.Tunnel
		services   []repository.Service
		readerErr  error
		tunnelErr  error
		serviceErr error
		wantErr    error
	}{
		{name: "consistent reader", readerErr: sentinel, wantErr: sentinel},
		{name: "service read", serviceErr: sentinel, wantErr: sentinel},
		{name: "tunnel read", tunnelErr: repository.ErrNotFound, wantErr: repository.ErrNotFound},
		{name: "invalid stored tunnel", tunnel: repository.Tunnel{ID: testTunnelID}, wantErr: ErrInvalidSnapshot},
		{name: "revoked tunnel", tunnel: func() repository.Tunnel {
			tunnel := sourceTestTunnel(3)
			tunnel.RevokedAt = &revokedAt
			return tunnel
		}(), wantErr: ErrTunnelRevoked},
		{name: "invalid service state", tunnel: sourceTestTunnel(3), services: []repository.Service{func() repository.Service {
			service := sourceTestService("old.internal", 4)
			return service
		}()}, wantErr: ErrInvalidSnapshot},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &sourceTestReader{err: test.readerErr, view: sourceTestView{
				tunnels: sourceTestTunnelRepository{get: func(context.Context, string) (repository.Tunnel, error) {
					return test.tunnel, test.tunnelErr
				}},
				services: sourceTestServiceRepository{list: func(context.Context, string) ([]repository.Service, error) {
					return test.services, test.serviceErr
				}},
			}}
			source, err := NewSource(reader, sourceTestBuilder(t))
			if err != nil {
				t.Fatalf("NewSource() error = %v", err)
			}
			result, err := source.Current(context.Background(), testTunnelID)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Current() error = %v, want %v", err, test.wantErr)
			}
			if result.Snapshot != nil || result.DeterministicBytes != nil {
				t.Fatalf("Current() failure result = %#v, want zero", result)
			}
		})
	}
}

func TestSourceRejectsInvalidConstructionAndInput(t *testing.T) {
	reader := &sourceTestReader{}
	builder := sourceTestBuilder(t)
	if source, err := NewSource(nil, builder); source != nil || !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("NewSource(nil, builder) = (%#v, %v)", source, err)
	}
	if source, err := NewSource(reader, nil); source != nil || !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("NewSource(reader, nil) = (%#v, %v)", source, err)
	}

	source, err := NewSource(reader, builder)
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	for _, test := range []struct {
		name     string
		ctx      context.Context
		tunnelID string
	}{
		{name: "nil context", tunnelID: testTunnelID},
		{name: "invalid tunnel ID", ctx: context.Background(), tunnelID: "tun_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := source.Current(test.ctx, test.tunnelID); !errors.Is(err, ErrInvalidSource) {
				t.Fatalf("Current() error = %v, want ErrInvalidSource", err)
			}
		})
	}
	if reader.calls != 0 {
		t.Fatalf("ReadConsistent() calls for invalid input = %d, want 0", reader.calls)
	}

	var nilSource *Source
	if _, err := nilSource.Current(context.Background(), testTunnelID); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil Source.Current() error = %v, want ErrInvalidSource", err)
	}
}

func TestSourceCurrentReadsOnlyCompleteSQLiteState(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(ctx, sourceTestTunnel(1)); err != nil {
			return err
		}
		return transaction.Services().Create(ctx, sourceTestService("old.internal", 1))
	}); err != nil {
		t.Fatalf("seed old state error = %v", err)
	}

	servicesRead := make(chan struct{})
	resumeRead := make(chan struct{})
	reader := &sourcePausingReader{store: store, servicesRead: servicesRead, resumeRead: resumeRead}
	source, err := NewSource(reader, sourceTestBuilder(t))
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}

	type currentResult struct {
		result Result
		err    error
	}
	currentDone := make(chan currentResult, 1)
	go func() {
		result, err := source.Current(ctx, testTunnelID)
		currentDone <- currentResult{result: result, err: err}
	}()

	select {
	case <-servicesRead:
	case <-time.After(5 * time.Second):
		close(resumeRead)
		t.Fatal("Current() did not reach the paused Services read")
	}
	writeErr := store.WithTx(ctx, func(transaction repository.TxStore) error {
		service, err := transaction.Services().Get(ctx, testTunnelID, sourceServiceID)
		if err != nil {
			return err
		}
		service.OriginHost = "new.internal"
		service.RequiredRevision = 2
		service.UpdatedAt = 2
		if _, err := transaction.Services().Update(ctx, service, service.Version); err != nil {
			return err
		}
		_, err = transaction.Tunnels().AdvanceDesiredRevision(ctx, testTunnelID, 1, 1, 2)
		return err
	})
	close(resumeRead)
	if writeErr != nil {
		t.Fatalf("commit new state error = %v", writeErr)
	}

	var old currentResult
	select {
	case old = <-currentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Current() did not finish after resuming the consistent read")
	}
	if old.err != nil {
		t.Fatalf("Current(paused) error = %v", old.err)
	}
	assertSourceTuple(t, old.result, 1, "old.internal", 1)

	current, err := source.Current(ctx, testTunnelID)
	if err != nil {
		t.Fatalf("Current(after commit) error = %v", err)
	}
	assertSourceTuple(t, current, 2, "new.internal", 2)
	if reflect.DeepEqual(old.result.DeterministicBytes, current.DeterministicBytes) {
		t.Fatal("old and new complete SQLite states produced identical Snapshot bytes")
	}
}

func assertSourceTuple(t *testing.T, result Result, revision uint64, host string, requiredRevision uint64) {
	t.Helper()
	if result.Snapshot.GetRevision() != revision || len(result.Snapshot.GetServices()) != 1 {
		t.Fatalf("Snapshot tuple = revision:%d services:%d, want %d/1", result.Snapshot.GetRevision(), len(result.Snapshot.GetServices()), revision)
	}
	service := result.Snapshot.GetServices()[0]
	if service.GetOriginHost() != host || service.GetRequiredRevision() != requiredRevision {
		t.Fatalf("Snapshot tuple = revision:%d host:%q required_revision:%d, want %d/%q/%d",
			result.Snapshot.GetRevision(), service.GetOriginHost(), service.GetRequiredRevision(), revision, host, requiredRevision)
	}
}

func sourceTestBuilder(t *testing.T) *Builder {
	t.Helper()
	return newTestBuilder(t, Config{
		ProtocolVersion:      1,
		MaxServices:          MaxServicesPerTunnel,
		MaxSnapshotBytes:     MaxTunnelSnapshotSize,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
}

func sourceTestTunnel(revision int64) repository.Tunnel {
	return repository.Tunnel{
		ID: testTunnelID, Name: "source-test", Version: 1, DesiredRevision: revision, CreatedAt: 1, UpdatedAt: 1,
	}
}

func sourceTestService(host string, requiredRevision int64) repository.Service {
	return repository.Service{
		ID: sourceServiceID, TunnelID: testTunnelID, Name: "source-test", RequiredRevision: requiredRevision,
		OriginScheme: repository.OriginSchemeTCP, OriginHost: host, OriginPort: 8080,
		ConnectTimeoutMS: 5_000, Enabled: true, Version: 1, CreatedAt: 1, UpdatedAt: 1,
	}
}

type sourceTestReader struct {
	view  repository.RepositoryView
	err   error
	calls int
}

func (reader *sourceTestReader) ReadConsistent(_ context.Context, fn func(repository.RepositoryView) error) error {
	reader.calls++
	if reader.err != nil {
		return reader.err
	}
	return fn(reader.view)
}

type sourceTestView struct {
	repository.RepositoryView
	tunnels  repository.TunnelRepository
	services repository.ServiceRepository
}

func (view sourceTestView) Tunnels() repository.TunnelRepository   { return view.tunnels }
func (view sourceTestView) Services() repository.ServiceRepository { return view.services }

func (sourceTestView) Routes() repository.RouteRepository { return nil }

type sourceTestTunnelRepository struct {
	repository.TunnelRepository
	get func(context.Context, string) (repository.Tunnel, error)
}

func (repo sourceTestTunnelRepository) Get(ctx context.Context, id string) (repository.Tunnel, error) {
	return repo.get(ctx, id)
}

type sourceTestServiceRepository struct {
	repository.ServiceRepository
	list func(context.Context, string) ([]repository.Service, error)
}

func (repo sourceTestServiceRepository) ListByTunnel(ctx context.Context, tunnelID string) ([]repository.Service, error) {
	return repo.list(ctx, tunnelID)
}

type sourcePausingReader struct {
	store        *sqlite.Store
	servicesRead chan struct{}
	resumeRead   <-chan struct{}
	once         sync.Once
}

func (reader *sourcePausingReader) ReadConsistent(ctx context.Context, fn func(repository.RepositoryView) error) error {
	return reader.store.ReadConsistent(ctx, func(view repository.RepositoryView) error {
		return fn(sourcePausingView{
			RepositoryView: view,
			services: sourcePausingServiceRepository{
				ServiceRepository: view.Services(),
				reader:            reader,
			},
		})
	})
}

type sourcePausingView struct {
	repository.RepositoryView
	services repository.ServiceRepository
}

func (view sourcePausingView) Services() repository.ServiceRepository { return view.services }

func (sourcePausingView) Routes() repository.RouteRepository { return nil }

type sourcePausingServiceRepository struct {
	repository.ServiceRepository
	reader *sourcePausingReader
}

func (repo sourcePausingServiceRepository) ListByTunnel(ctx context.Context, tunnelID string) ([]repository.Service, error) {
	services, err := repo.ServiceRepository.ListByTunnel(ctx, tunnelID)
	if err != nil {
		return nil, err
	}
	repo.reader.once.Do(func() { close(repo.reader.servicesRead) })
	select {
	case <-repo.reader.resumeRead:
		return services, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
