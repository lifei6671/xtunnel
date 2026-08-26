package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
)

func TestReadConsistentCommitsReadOnlyView(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		t.Fatalf("seed Tunnel error = %v", err)
	}

	if err := store.ReadConsistent(context.Background(), func(view repository.RepositoryView) error {
		tunnel, err := view.Tunnels().Get(context.Background(), repositoryTestTunnelID)
		if err != nil {
			return err
		}
		services, err := view.Services().ListByTunnel(context.Background(), repositoryTestTunnelID)
		if err != nil {
			return err
		}
		if tunnel.DesiredRevision != 0 || len(services) != 0 {
			return errors.New("consistent view returned unexpected state")
		}
		return nil
	}); err != nil {
		t.Fatalf("ReadConsistent() error = %v", err)
	}
	if err := store.ReadConsistent(context.Background(), nil); err == nil {
		t.Fatal("ReadConsistent(nil) error = nil")
	}
}

func TestReadConsistentRollsBackAfterRequestContextCanceled(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		t.Fatalf("seed Tunnel error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	err = store.ReadConsistent(ctx, func(view repository.RepositoryView) error {
		if _, err := view.Tunnels().Get(ctx, repositoryTestTunnelID); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadConsistent() error = %v, want context.Canceled", err)
	}

	// 独立 Rollback Context 必须先释放已取消请求持有的只读事务和连接。
	nextContext, nextCancel := context.WithTimeout(context.Background(), time.Second)
	defer nextCancel()
	if err := store.WithTx(nextContext, func(transaction repository.TxStore) error {
		_, err := transaction.Tunnels().AdvanceVersion(nextContext, repositoryTestTunnelID, 1, 2)
		return err
	}); err != nil {
		t.Fatalf("WithTx() after canceled consistent read error = %v", err)
	}
}
