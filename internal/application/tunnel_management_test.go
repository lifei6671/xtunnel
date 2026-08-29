package application

import (
	"context"
	"errors"
	"testing"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

type shortCiphertextProtector struct{}

func (shortCiphertextProtector) Seal([]byte, TokenProtectionContext) ([]byte, error) {
	return []byte{1}, nil
}

func (shortCiphertextProtector) Open([]byte, TokenProtectionContext) ([]byte, error) {
	return nil, ErrTokenProtection
}

type emptyTunnelRuntime struct{}

func (emptyTunnelRuntime) RuntimeStatusSnapshots() []serverruntime.SessionStatusSnapshot { return nil }
func (emptyTunnelRuntime) ConnectorSnapshots() []serverruntime.ConnectorSnapshot         { return nil }
func (emptyTunnelRuntime) DeleteTunnel(string) error                                     { return nil }

func TestTunnelManagementCreateRollsBackTunnelWhenTokenInsertFails(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	tokens := NewConnectionTokenService(store, shortCiphertextProtector{})
	service := NewTunnelManagementService(
		store, tokens, emptyTunnelRuntime{},
		&protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 443},
		&protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{PublicCa: &protocolv1.PublicCATrust{}}},
		1000,
	)
	const tunnelID = "tun_01J00000000000000000000000"
	service.newTunnelID = func() (string, error) { return tunnelID, nil }

	result, err := service.Create(context.Background(), CreateTunnelInput{Name: "atomic tunnel"})
	if err == nil || result != (CreateTunnelResult{}) {
		t.Fatalf("Create() returned a result after invalid Token metadata")
	}
	readErr := store.Read(context.Background(), func(view repository.RepositoryView) error {
		_, err := view.Tunnels().Get(context.Background(), tunnelID)
		return err
	})
	if !errors.Is(readErr, repository.ErrNotFound) {
		t.Fatalf("Tunnel survived failed Token insert: %v", readErr)
	}
}

func TestTunnelManagementCreateRejectsConfiguredTunnelLimit(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), repository.Tunnel{
			ID: "tun_01J00000000000000000000000", Name: "existing", Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err != nil {
		t.Fatalf("seed Tunnel error = %v", err)
	}

	service := NewTunnelManagementService(
		store, NewConnectionTokenService(store, shortCiphertextProtector{}), emptyTunnelRuntime{},
		&protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 443},
		&protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{PublicCa: &protocolv1.PublicCATrust{}}},
		1,
	)
	service.newTunnelID = func() (string, error) { return "tun_01J00000000000000000000001", nil }

	result, err := service.Create(context.Background(), CreateTunnelInput{Name: "over limit"})
	if !errors.Is(err, ErrTunnelManagementLimit) || result != (CreateTunnelResult{}) {
		t.Fatalf("Create() = (%#v, %v), want empty result and ErrTunnelManagementLimit", result, err)
	}
	readErr := store.Read(context.Background(), func(view repository.RepositoryView) error {
		count, err := view.Tunnels().Count(context.Background())
		if err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("Count() after rejected Create = %d, want 1", count)
		}
		return nil
	})
	if readErr != nil {
		t.Fatalf("Read(Count) error = %v", readErr)
	}
}
