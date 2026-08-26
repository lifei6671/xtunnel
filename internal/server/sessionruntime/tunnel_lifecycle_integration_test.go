package sessionruntime_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/logging"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
	"github.com/lifei6671/xtunnel/internal/server/sessionruntime"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
)

const (
	lifecycleTunnelID    = "tun_01J00000000000000000000071"
	lifecycleConnectorID = "con_01J00000000000000000000071"
	lifecycleAdminID     = "adm_01J00000000000000000000071"
)

func TestTunnelLifecycleDurableCommitConvergesSameSessionManager(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(ctx, repository.Tunnel{
			ID: lifecycleTunnelID, Name: "lifecycle-integration", Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err != nil {
		t.Fatalf("create Tunnel error = %v", err)
	}

	registry := serverruntime.NewRegistry()
	manager, err := sessionruntime.New(registry, sessionruntime.Options{
		HighPriorityCapacity: 8, NormalCapacity: 8, InboundCapacity: 8,
		WriteTimeout: time.Second, MaxReplayEntries: 8,
		MaxWorkTotal: 64, MaxWorkConnecting: 16, HeartbeatTimeout: 5 * time.Second,
		SnapshotProvider: lifecycleSnapshotProvider{},
	})
	if err != nil {
		t.Fatalf("sessionruntime.New() error = %v", err)
	}
	logger, err := logging.New(io.Discard, logging.Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	revoker := &durableCheckingManagerRevoker{store: store, manager: manager}
	service := application.NewTunnelLifecycleService(
		store,
		application.NewSecurityAuditWriter(store, logger),
		revoker,
	)

	result, err := service.Revoke(ctx, application.TunnelRevokeInput{
		TunnelID: lifecycleTunnelID, ExpectedVersion: 1,
		Audit: application.SecurityAuditContext{ActorID: lifecycleAdminID},
	})
	if err != nil {
		t.Fatalf("TunnelLifecycleService.Revoke() error = %v", err)
	}
	if result.TunnelVersion != 2 || !revoker.observedDurableCommit {
		t.Fatalf("Revoke() result = %#v durable_before_runtime=%t", result, revoker.observedDurableCommit)
	}
	if _, err := registry.ReserveAuthenticated(lifecycleTunnelID, lifecycleConnectorID); !errors.Is(err, serverruntime.ErrTunnelRuntimeRevoked) {
		t.Fatalf("ReserveAuthenticated() after durable revoke error = %v, want ErrTunnelRuntimeRevoked", err)
	}
}

type lifecycleSnapshotProvider struct{}

func (lifecycleSnapshotProvider) Current(_ context.Context, tunnelID string) (serversnapshot.Result, error) {
	return serversnapshot.Result{Snapshot: &protocolv1.TunnelSnapshot{TunnelId: tunnelID}}, nil
}

type durableCheckingManagerRevoker struct {
	store                 repository.Store
	manager               *sessionruntime.Manager
	observedDurableCommit bool
}

func (revoker *durableCheckingManagerRevoker) RevokeTunnel(tunnelID string) error {
	err := revoker.store.Read(context.Background(), func(view repository.RepositoryView) error {
		tunnel, err := view.Tunnels().Get(context.Background(), tunnelID)
		if err != nil {
			return err
		}
		revoker.observedDurableCommit = tunnel.RevokedAt != nil && tunnel.Version == 2
		return nil
	})
	if err != nil {
		return err
	}
	return revoker.manager.RevokeTunnel(tunnelID)
}
