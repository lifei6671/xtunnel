package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/repository"
	repositorysqlite "github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/snapshot"
)

func TestGatewayStartupRejectsRevokedStoredSnapshotBeforeExistingAdminListener(t *testing.T) {
	ctx := context.Background()
	store, err := repositorysqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	const tunnelID = "tun_01J00000000000000000000020"
	const originCanary = "origin-secret-canary.internal"
	const tlsCanary = "tls-secret-canary.internal"
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(ctx, repository.Tunnel{
			ID: tunnelID, Name: "startup gate", Version: 1, DesiredRevision: 1,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		for index, serviceID := range []string{
			"svc_01J00000000000000000000020",
			"svc_01J00000000000000000000021",
		} {
			if err := transaction.Services().Create(ctx, repository.Service{
				ID: serviceID, TunnelID: tunnelID, Name: "origin",
				RequiredRevision: 1, OriginScheme: repository.OriginSchemeHTTPS,
				OriginHost: originCanary, OriginPort: uint32(8_080 + index),
				TLSVerify: true, TLSServerName: tlsCanary,
				ConnectTimeoutMS: 5_000, Enabled: true, Version: 1,
				CreatedAt: 1, UpdatedAt: 1,
			}); err != nil {
				return err
			}
		}
		_, err := transaction.Tunnels().Revoke(ctx, tunnelID, 1, 2)
		return err
	}); err != nil {
		t.Fatalf("seed stored Snapshot error = %v", err)
	}
	if err := store.CreateFirstAdmin(ctx, "admin", "startup gate password"); err != nil {
		t.Fatalf("CreateFirstAdmin() error = %v", err)
	}

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve Gateway address error = %v", err)
	}
	gatewayAddress := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatalf("release reserved Gateway address error = %v", err)
	}

	config := serverconfig.Config{
		AgentGateway: serverconfig.AgentGateway{Listen: gatewayAddress},
		Limits: serverconfig.Limits{
			MaxServicesPerTunnel:      1,
			MaxTunnelSnapshotBytes:    snapshot.MaxTunnelSnapshotSize,
			MaxControlFrameBytes:      1 << 20,
			MaxHealthTargetsPerTunnel: 2_000,
			MaxHealthTargetsGlobal:    50_000,
		},
	}
	bootstrapSocketOpened := false
	closer, err := openGatewayAndBootstrapWith(
		ctx,
		config,
		&serverStorage{database: store},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"",
		func(context.Context, string, string, *repositorysqlite.Store, func() error, func(error)) (io.Closer, error) {
			bootstrapSocketOpened = true
			return nil, nil
		},
	)
	if closer != nil || !errors.Is(err, snapshot.ErrServiceLimit) {
		t.Fatalf("openGatewayAndBootstrapWith() = (%T, %v), want (nil, ErrServiceLimit)", closer, err)
	}
	if strings.Contains(err.Error(), originCanary) || strings.Contains(err.Error(), tlsCanary) {
		t.Fatalf("startup Gate error leaked Origin configuration: %v", err)
	}
	if bootstrapSocketOpened {
		t.Fatal("invalid stored Snapshot opened Bootstrap Socket before failing")
	}
	rebound, err := net.Listen("tcp", gatewayAddress)
	if err != nil {
		t.Fatalf("invalid stored Snapshot started Gateway at %q: %v", gatewayAddress, err)
	}
	if err := rebound.Close(); err != nil {
		t.Errorf("close rebound Gateway listener error = %v", err)
	}
}

func TestGatewayStartupRejectsStoredSnapshotBeforeOpeningFirstAdminSocket(t *testing.T) {
	ctx := context.Background()
	store, err := repositorysqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	const tunnelID = "tun_01J00000000000000000000022"
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(ctx, repository.Tunnel{
			ID: tunnelID, Name: "first admin gate", Version: 1, DesiredRevision: 1,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			return err
		}
		for index, serviceID := range []string{
			"svc_01J00000000000000000000022",
			"svc_01J00000000000000000000023",
		} {
			if err := transaction.Services().Create(ctx, repository.Service{
				ID: serviceID, TunnelID: tunnelID, Name: "origin",
				RequiredRevision: 1, OriginScheme: repository.OriginSchemeHTTP,
				OriginHost: "127.0.0.1", OriginPort: uint32(8_082 + index),
				ConnectTimeoutMS: 5_000, Enabled: true, Version: 1,
				CreatedAt: 1, UpdatedAt: 1,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed stored Snapshot error = %v", err)
	}

	config := serverconfig.Config{Limits: serverconfig.Limits{
		MaxServicesPerTunnel:      1,
		MaxTunnelSnapshotBytes:    snapshot.MaxTunnelSnapshotSize,
		MaxControlFrameBytes:      1 << 20,
		MaxHealthTargetsPerTunnel: 2_000,
		MaxHealthTargetsGlobal:    50_000,
	}}
	bootstrapSocketOpened := false
	closer, err := openGatewayAndBootstrapWith(
		ctx,
		config,
		&serverStorage{database: store},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		"",
		func(context.Context, string, string, *repositorysqlite.Store, func() error, func(error)) (io.Closer, error) {
			bootstrapSocketOpened = true
			return nil, nil
		},
	)
	if closer != nil || !errors.Is(err, snapshot.ErrServiceLimit) {
		t.Fatalf("openGatewayAndBootstrapWith() = (%T, %v), want (nil, ErrServiceLimit)", closer, err)
	}
	if bootstrapSocketOpened {
		t.Fatal("invalid stored Snapshot opened first-admin Bootstrap Socket before failing")
	}
}
