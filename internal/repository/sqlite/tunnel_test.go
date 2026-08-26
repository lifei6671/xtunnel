package sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	repositoryTestTunnelID = "tun_01J00000000000000000000000"
	repositoryTestTokenID  = "tok_01J00000000000000000000000"
)

func TestTunnelRepositoriesShareBeginImmediateTransaction(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	tunnel := testTunnel()
	token := testTunnelToken()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), tunnel); err != nil {
			return err
		}
		if err := transaction.TunnelTokens().Create(context.Background(), token); err != nil {
			return err
		}
		gotTunnel, err := transaction.Tunnels().Get(context.Background(), tunnel.ID)
		if err != nil {
			return err
		}
		if gotTunnel.Name != tunnel.Name || gotTunnel.Version != tunnel.Version {
			return errors.New("tunnel readback differs")
		}
		gotToken, err := transaction.TunnelTokens().GetByIdentity(context.Background(), token.TunnelID, token.ID, token.Version)
		if err != nil {
			return err
		}
		if gotToken.SecretHash != token.SecretHash || string(gotToken.TokenCiphertext) != string(token.TokenCiphertext) {
			return errors.New("tunnel token readback differs")
		}
		active, err := transaction.TunnelTokens().GetActiveByTunnel(context.Background(), token.TunnelID)
		if err != nil || active.ID != token.ID {
			return errors.New("active tunnel token readback differs")
		}
		versioned, err := transaction.TunnelTokens().GetByTunnelVersion(context.Background(), token.TunnelID, token.Version)
		if err != nil || versioned.ID != token.ID {
			return errors.New("versioned tunnel token readback differs")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithTx() error = %v", err)
	}
}

func TestTunnelRepositoryListReturnsNonNilEmptySlice(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		tunnels, err := view.Tunnels().List(context.Background())
		if err != nil {
			return err
		}
		if tunnels == nil || len(tunnels) != 0 {
			t.Fatalf("List() = %#v, want non-nil empty slice", tunnels)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read(List) error = %v", err)
	}
}

func TestTunnelRepositoryListUsesStableOrderAndMapsEveryField(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	revokedAt := int64(31)
	want := []repository.Tunnel{
		{ID: "tun_01J00000000000000000000001", Name: "first", Version: 2, DesiredRevision: 11, CreatedAt: 21, UpdatedAt: 22},
		{ID: "tun_01J00000000000000000000002", Name: "second", Version: 3, DesiredRevision: 12, RevokedAt: &revokedAt, CreatedAt: 23, UpdatedAt: 31},
		{ID: "tun_01J00000000000000000000003", Name: "third", Version: 4, DesiredRevision: 13, CreatedAt: 24, UpdatedAt: 25},
	}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		for _, index := range []int{2, 0, 1} {
			if err := transaction.Tunnels().Create(context.Background(), want[index]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed Tunnels error = %v", err)
	}

	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		got, err := view.Tunnels().List(context.Background())
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("List() = %#v, want %#v", got, want)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read(List) error = %v", err)
	}
}

func TestTunnelRepositoryListPropagatesCancellation(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = store.Read(ctx, func(view repository.RepositoryView) error {
		_, err := view.Tunnels().List(ctx)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("List(canceled) error = %v, want context.Canceled", err)
	}
}

func TestTunnelRepositoryListRejectsDamagedStoredRow(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	const damagedID = "tun_01J00000000000000000000009"
	if err := store.database.Connection(func(connection *gorm.DB) error {
		if err := connection.Exec("PRAGMA ignore_check_constraints = ON").Error; err != nil {
			return err
		}
		createErr := connection.Create(&tunnelRecord{
			ID: damagedID, Name: " ", Version: 1, CreatedAt: 1, UpdatedAt: 1,
		}).Error
		restoreErr := connection.Exec("PRAGMA ignore_check_constraints = OFF").Error
		return errors.Join(createErr, restoreErr)
	}); err != nil {
		t.Fatalf("seed damaged Tunnel row error = %v", err)
	}

	err = store.Read(context.Background(), func(view repository.RepositoryView) error {
		_, err := view.Tunnels().List(context.Background())
		return err
	})
	if !errors.Is(err, repository.ErrInvalidTunnel) || !strings.Contains(err.Error(), damagedID) {
		t.Fatalf("List(damaged) error = %v, want contextual ErrInvalidTunnel", err)
	}
}

func TestHasTunnelTokensOnlyReportsRowPresence(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	exists, err := store.HasTunnelTokens(context.Background())
	if err != nil {
		t.Fatalf("HasTunnelTokens() on empty table error = %v", err)
	}
	if exists {
		t.Fatal("HasTunnelTokens() = true before a Token row exists")
	}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), testTunnel()); err != nil {
			return err
		}
		return transaction.TunnelTokens().Create(context.Background(), testTunnelToken())
	}); err != nil {
		t.Fatalf("seed Tunnel Token error = %v", err)
	}
	exists, err = store.HasTunnelTokens(context.Background())
	if err != nil {
		t.Fatalf("HasTunnelTokens() with row error = %v", err)
	}
	if !exists {
		t.Fatal("HasTunnelTokens() = false after a Token row exists")
	}
}

func TestTunnelRepositoriesRollBackWholeTransaction(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sentinel := errors.New("rollback requested")
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), testTunnel()); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("WithTx() error = %v, want rollback sentinel", err)
	}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		_, err := transaction.Tunnels().Get(context.Background(), repositoryTestTunnelID)
		if !errors.Is(err, repository.ErrNotFound) {
			return errors.New("rolled-back Tunnel remained visible")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceDesiredRevisionUsesIndependentCAS(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), testTunnel()); err != nil {
			return err
		}
		updated, err := transaction.Tunnels().AdvanceDesiredRevision(
			context.Background(), repositoryTestTunnelID, 1, 0, 2,
		)
		if err != nil {
			return err
		}
		if updated.Version != 1 || updated.DesiredRevision != 1 || updated.UpdatedAt != 2 {
			return errors.New("desired revision CAS changed the wrong Tunnel fields")
		}
		return nil
	}); err != nil {
		t.Fatalf("advance desired revision error = %v", err)
	}

	tests := []struct {
		name             string
		expectedVersion  int64
		expectedRevision int64
	}{
		{name: "旧 Tunnel Version", expectedVersion: 2, expectedRevision: 1},
		{name: "旧 Desired Revision", expectedVersion: 1, expectedRevision: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
				_, err := transaction.Tunnels().AdvanceDesiredRevision(
					context.Background(), repositoryTestTunnelID,
					test.expectedVersion, test.expectedRevision, 3,
				)
				return err
			})
			if !errors.Is(err, repository.ErrVersionConflict) {
				t.Fatalf("AdvanceDesiredRevision() error = %v, want ErrVersionConflict", err)
			}
		})
	}
}

func TestAdvanceDesiredRevisionRejectsRevokedTunnel(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), testTunnel()); err != nil {
			return err
		}
		_, err := transaction.Tunnels().Revoke(context.Background(), repositoryTestTunnelID, 1, 2)
		return err
	}); err != nil {
		t.Fatalf("revoke Tunnel error = %v", err)
	}

	err = store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		_, err := transaction.Tunnels().AdvanceDesiredRevision(
			context.Background(), repositoryTestTunnelID, 2, 0, 3,
		)
		return err
	})
	if !errors.Is(err, repository.ErrVersionConflict) {
		t.Fatalf("AdvanceDesiredRevision(revoked) error = %v, want ErrVersionConflict", err)
	}
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		tunnel, err := view.Tunnels().Get(context.Background(), repositoryTestTunnelID)
		if err != nil {
			return err
		}
		if tunnel.Version != 2 || tunnel.DesiredRevision != 0 || tunnel.RevokedAt == nil || *tunnel.RevokedAt != 2 {
			return errors.New("rejected desired revision CAS changed the revoked Tunnel")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdvanceDesiredRevisionConcurrentCASCommitsOnce(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		t.Fatalf("seed Tunnel error = %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- store.WithTx(context.Background(), func(transaction repository.TxStore) error {
				_, err := transaction.Tunnels().AdvanceDesiredRevision(
					context.Background(), repositoryTestTunnelID, 1, 0, 2,
				)
				return err
			})
		}()
	}
	close(start)

	var succeeded, conflicted int
	for range 2 {
		switch err := <-results; {
		case err == nil:
			succeeded++
		case errors.Is(err, repository.ErrVersionConflict):
			conflicted++
		default:
			t.Fatalf("concurrent AdvanceDesiredRevision() error = %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent CAS results = succeeded:%d conflicted:%d, want 1/1", succeeded, conflicted)
	}

	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		tunnel, err := view.Tunnels().Get(context.Background(), repositoryTestTunnelID)
		if err != nil {
			return err
		}
		if tunnel.Version != 1 || tunnel.DesiredRevision != 1 {
			return errors.New("concurrent desired revision CAS committed more than once")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWithTxRollsBackAfterRequestContextCanceled(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	err = store.WithTx(ctx, func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(ctx, testTunnel()); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithTx() error = %v, want context.Canceled", err)
	}

	// 上一个请求取消后，独立清理 Context 必须已释放 BEGIN IMMEDIATE 写锁；
	// 下一笔事务既不能看到半提交行，也不能收到嵌套事务或数据库锁错误。
	next := testTunnel()
	next.ID = "tun_01J00000000000000000000001"
	next.Name = "next"
	nextContext, nextCancel := context.WithTimeout(context.Background(), time.Second)
	defer nextCancel()
	if err := store.WithTx(nextContext, func(transaction repository.TxStore) error {
		if _, err := transaction.Tunnels().Get(nextContext, repositoryTestTunnelID); !errors.Is(err, repository.ErrNotFound) {
			return errors.New("canceled transaction left a visible Tunnel")
		}
		return transaction.Tunnels().Create(nextContext, next)
	}); err != nil {
		t.Fatalf("next WithTx() after canceled request error = %v", err)
	}
}

func TestReadDoesNotAcquireSQLiteWriteLock(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	}); err != nil {
		t.Fatalf("seed Tunnel error = %v", err)
	}

	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- store.WithTx(context.Background(), func(repository.TxStore) error {
			close(writeStarted)
			<-releaseWrite
			return nil
		})
	}()
	<-writeStarted

	readContext, cancelRead := context.WithTimeout(context.Background(), time.Second)
	defer cancelRead()
	if err := store.Read(readContext, func(view repository.RepositoryView) error {
		_, err := view.Tunnels().Get(readContext, repositoryTestTunnelID)
		return err
	}); err != nil {
		close(releaseWrite)
		<-writeDone
		t.Fatalf("Read() was blocked by an unrelated BEGIN IMMEDIATE: %v", err)
	}
	close(releaseWrite)
	if err := <-writeDone; err != nil {
		t.Fatalf("writer transaction error = %v", err)
	}

	if err := store.Read(context.Background(), nil); err == nil {
		t.Fatal("Read(nil) error = nil")
	}
}

func TestTunnelTokenSchemaConstraints(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.database.Exec(
		"INSERT INTO tunnels(id, name, version, desired_revision, created_at, updated_at) VALUES (?, ?, 1, 0, 1, 1)",
		"tun_Z1J00000000000000000000000", "invalid",
	).Error; err == nil {
		t.Fatal("tunnels accepted an ID whose first ULID character exceeds 7")
	}
	if err := store.database.Exec(
		"INSERT INTO tunnels(id, name, version, desired_revision, revoked_at, created_at, updated_at) VALUES (?, ?, 1, 0, -1, 1, 1)",
		"tun_01J00000000000000000000001", "invalid-revocation",
	).Error; err == nil {
		t.Fatal("tunnels accepted a non-positive revoked_at")
	}
	if err := store.database.Exec(
		"INSERT INTO tunnels(id, name, version, desired_revision, created_at, updated_at) VALUES (?, ?, 1, 0, 1, 1)",
		repositoryTestTunnelID, "office",
	).Error; err != nil {
		t.Fatalf("insert parent tunnel error = %v", err)
	}

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{name: "错误 Token ID", sql: "INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at) VALUES (?, ?, zeroblob(32), zeroblob(29), 1, 'ACTIVE', 1)", args: []any{"tok_invalid", repositoryTestTunnelID}},
		{name: "Token ID 首字符超出 ULID 范围", sql: "INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at) VALUES (?, ?, zeroblob(32), zeroblob(29), 1, 'ACTIVE', 1)", args: []any{"tok_Z1J00000000000000000000000", repositoryTestTunnelID}},
		{name: "错误 Hash 长度", sql: "INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at) VALUES (?, ?, zeroblob(31), zeroblob(29), 1, 'ACTIVE', 1)", args: []any{repositoryTestTokenID, repositoryTestTunnelID}},
		{name: "错误密文长度", sql: "INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at) VALUES (?, ?, zeroblob(32), zeroblob(28), 1, 'ACTIVE', 1)", args: []any{repositoryTestTokenID, repositoryTestTunnelID}},
		{name: "不存在的 Tunnel", sql: "INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at) VALUES (?, ?, zeroblob(32), zeroblob(29), 1, 'ACTIVE', 1)", args: []any{repositoryTestTokenID, "tun_01J00000000000000000000001"}},
		{name: "撤销时间为零", sql: "INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at, revoked_at) VALUES (?, ?, zeroblob(32), zeroblob(29), 1, 'REVOKED', 1, 0)", args: []any{repositoryTestTokenID, repositoryTestTunnelID}},
		{name: "撤销时间为负数", sql: "INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at, revoked_at) VALUES (?, ?, zeroblob(32), zeroblob(29), 1, 'REVOKED', 1, -1)", args: []any{repositoryTestTokenID, repositoryTestTunnelID}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.database.Exec(test.sql, test.args...).Error; err == nil {
				t.Fatal("tunnel_tokens accepted an invalid row")
			}
		})
	}

	if err := store.database.Exec(
		"INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at) VALUES (?, ?, zeroblob(32), zeroblob(29), 1, 'ACTIVE', 1)",
		repositoryTestTokenID, repositoryTestTunnelID,
	).Error; err != nil {
		t.Fatalf("insert active token error = %v", err)
	}
	if err := store.database.Exec(
		"INSERT INTO tunnel_tokens(id, tunnel_id, secret_hash, token_ciphertext, version, status, created_at) VALUES (?, ?, X'1111111111111111111111111111111111111111111111111111111111111111', zeroblob(29), 2, 'ACTIVE', 1)",
		"tok_01J00000000000000000000001", repositoryTestTunnelID,
	).Error; err == nil {
		t.Fatal("tunnel_tokens accepted two ACTIVE tokens for one Tunnel")
	}

	invalid := testTunnelToken()
	invalid.ID = "tok_invalid"
	invalid.SecretHash[0] = 0xAB
	invalid.TokenCiphertext = []byte{0xCD}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.TunnelTokens().Create(context.Background(), invalid)
	}); !errors.Is(err, repository.ErrInvalidTunnelToken) || strings.Contains(err.Error(), "ab") || strings.Contains(err.Error(), "cd") {
		t.Fatalf("invalid token error = %v, want safe ErrInvalidTunnelToken", err)
	}
}

func TestTunnelTokenRevokeAllPreservesOriginalRevokedAt(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), testTunnel()); err != nil {
			return err
		}
		first := testTunnelToken()
		if err := transaction.TunnelTokens().Create(context.Background(), first); err != nil {
			return err
		}
		if err := transaction.TunnelTokens().TransitionStatus(
			context.Background(), first.TunnelID, first.ID, first.Version,
			repository.TunnelTokenStatusActive, repository.TunnelTokenStatusRevokedForNewSession, 2,
		); err != nil {
			return err
		}
		second := testTunnelToken()
		second.ID = "tok_01J00000000000000000000001"
		second.Version = 2
		second.SecretHash[0] ^= 0xFF
		if err := transaction.TunnelTokens().Create(context.Background(), second); err != nil {
			return err
		}
		return transaction.TunnelTokens().RevokeAll(context.Background(), first.TunnelID, 3)
	}); err != nil {
		t.Fatalf("seed and revoke all error = %v", err)
	}
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		first, err := view.TunnelTokens().GetByTunnelVersion(context.Background(), repositoryTestTunnelID, 1)
		if err != nil {
			return err
		}
		second, err := view.TunnelTokens().GetByTunnelVersion(context.Background(), repositoryTestTunnelID, 2)
		if err != nil {
			return err
		}
		if first.Status != repository.TunnelTokenStatusRevoked || first.RevokedAt == nil || *first.RevokedAt != 2 {
			return errors.New("rotated Token lost its original revoked_at")
		}
		if second.Status != repository.TunnelTokenStatusRevoked || second.RevokedAt == nil || *second.RevokedAt != 3 {
			return errors.New("active Token did not receive Tunnel revoke time")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTunnelDomainMigrationUpgradesAndRollsBackFailedNextVersion(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:1], testNow); err != nil {
		t.Fatalf("run version 1 migration error = %v", err)
	}
	var before int64
	if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tunnels'").Scan(&before).Error; err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("tunnels table exists before version 2 migration: %d", before)
	}

	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("upgrade migration error = %v", err)
	}
	var tunnelCount, tokenCount, indexCount int64
	if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tunnels'").Scan(&tunnelCount).Error; err != nil {
		t.Fatalf("inspect tunnels table error = %v", err)
	}
	if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tunnel_tokens'").Scan(&tokenCount).Error; err != nil {
		t.Fatalf("inspect tunnel_tokens table error = %v", err)
	}
	if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'one_active_token_per_tunnel'").Scan(&indexCount).Error; err != nil {
		t.Fatalf("inspect active token index error = %v", err)
	}
	if tunnelCount != 1 || tokenCount != 1 || indexCount != 1 {
		t.Fatalf("upgraded schema = tunnels:%d tokens:%d index:%d", tunnelCount, tokenCount, indexCount)
	}

	available := append([]migration{}, productionMigrations...)
	available = append(available, migration{version: 6, statements: []string{
		"CREATE TABLE interrupted_tunnel_migration (id INTEGER PRIMARY KEY)",
		"THIS IS NOT VALID SQL",
	}})
	if err := runMigrations(context.Background(), database, available, testNow); err == nil {
		t.Fatal("run failed next migration error = nil")
	}
	var interruptedCount, versionCount int64
	if err := database.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'interrupted_tunnel_migration'").Scan(&interruptedCount).Error; err != nil {
		t.Fatalf("inspect rolled-back table error = %v", err)
	}
	if err := database.Table("schema_migrations").Count(&versionCount).Error; err != nil {
		t.Fatalf("count migration versions error = %v", err)
	}
	if interruptedCount != 0 || versionCount != 5 {
		t.Fatalf("failed migration rollback = table:%d versions:%d", interruptedCount, versionCount)
	}
}

func testTunnel() repository.Tunnel {
	return repository.Tunnel{ID: repositoryTestTunnelID, Name: "office", Version: 1, CreatedAt: 1, UpdatedAt: 1}
}

func testTunnelToken() repository.TunnelToken {
	var secretHash [32]byte
	for index := range secretHash {
		secretHash[index] = byte(index + 1)
	}
	return repository.TunnelToken{
		ID:              repositoryTestTokenID,
		TunnelID:        repositoryTestTunnelID,
		SecretHash:      secretHash,
		TokenCiphertext: []byte("encrypted-token-that-is-long-enough"),
		Version:         1,
		Status:          repository.TunnelTokenStatusActive,
		CreatedAt:       1,
	}
}
