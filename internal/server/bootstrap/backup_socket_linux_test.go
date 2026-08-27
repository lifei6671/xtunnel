//go:build linux

package bootstrap

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

func TestBackupBarrierSocketOwnsLeaseUntilClientRelease(t *testing.T) {
	runtimeDir := newShortRuntimeDirectory(t)
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const targetHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	socket, err := openBackupBarrierSocketWith(
		context.Background(), runtimeDir, targetHash, store,
		func(*net.UnixConn) error { return nil }, nil,
	)
	if err != nil {
		t.Fatalf("openBackupBarrierSocketWith() error = %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	info, err := os.Lstat(backupSocketPath(runtimeDir, targetHash))
	if err != nil {
		t.Fatalf("Lstat(socket) error = %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want Unix Socket 0600", info.Mode())
	}

	lease, handled, err := acquireOnlineBackupBarrier(context.Background(), runtimeDir, targetHash)
	if err != nil || !handled {
		t.Fatalf("acquireOnlineBackupBarrier() = handled:%t error:%v", handled, err)
	}
	written := make(chan error, 1)
	go func() {
		written <- store.WithTx(context.Background(), func(repository.TxStore) error { return nil })
	}()
	select {
	case err := <-written:
		t.Fatalf("write completed while online barrier held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("online lease Close() error = %v", err)
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("write after barrier release error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write did not resume after online barrier release")
	}
}

func TestBackupBarrierSocketCloseReleasesDisconnectedLease(t *testing.T) {
	runtimeDir := newShortRuntimeDirectory(t)
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const targetHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	socket, err := openBackupBarrierSocketWith(
		context.Background(), runtimeDir, targetHash, store,
		func(*net.UnixConn) error { return nil }, nil,
	)
	if err != nil {
		t.Fatalf("openBackupBarrierSocketWith() error = %v", err)
	}
	lease, handled, err := acquireOnlineBackupBarrier(context.Background(), runtimeDir, targetHash)
	if err != nil || !handled {
		t.Fatalf("acquireOnlineBackupBarrier() = handled:%t error:%v", handled, err)
	}
	leaseContext, cancelLeaseContext := lease.BindContext(context.Background())
	defer cancelLeaseContext()
	if err := socket.Close(); err != nil {
		t.Fatalf("backup socket Close() error = %v", err)
	}
	select {
	case <-leaseContext.Done():
	case <-time.After(time.Second):
		t.Fatal("online lease context was not canceled after Server shutdown")
	}
	if _, err := os.Lstat(backupSocketPath(runtimeDir, targetHash)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed socket path remains or stat failed: %v", err)
	}
	_ = lease.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := store.WithTx(ctx, func(repository.TxStore) error { return nil }); err != nil {
		t.Fatalf("write after socket shutdown error = %v", err)
	}
}

func newShortRuntimeDirectory(t *testing.T) string {
	t.Helper()
	runtimeDir, err := os.MkdirTemp("/tmp", "xtb-")
	if err != nil {
		t.Fatalf("os.MkdirTemp(/tmp) error = %v", err)
	}
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatalf("os.Chmod(runtimeDir) error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(runtimeDir); err != nil {
			t.Errorf("os.RemoveAll(runtimeDir) error = %v", err)
		}
	})
	return runtimeDir
}

func TestAcquireOnlineBackupBarrierOnlyFallsBackWhenSocketAbsent(t *testing.T) {
	runtimeDir := t.TempDir()
	const targetHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	lease, handled, err := acquireOnlineBackupBarrier(context.Background(), runtimeDir, targetHash)
	if err != nil || handled || lease != nil {
		t.Fatalf("absent socket = lease:%v handled:%t error:%v", lease, handled, err)
	}
	path := backupSocketPath(runtimeDir, targetHash)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write non-socket path error = %v", err)
	}
	if _, handled, err := acquireOnlineBackupBarrier(context.Background(), runtimeDir, targetHash); err == nil || !handled {
		t.Fatalf("non-socket path = handled:%t error:%v, want handled error", handled, err)
	}
	if filepath.Dir(path) != runtimeDir {
		t.Fatalf("socket path escaped runtime dir: %q", path)
	}
}

func TestAcquireOnlineBackupBarrierRejectsUnboundGrant(t *testing.T) {
	runtimeDir := newShortRuntimeDirectory(t)
	const targetHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: backupSocketPath(runtimeDir, targetHash), Net: "unix"})
	if err != nil {
		t.Fatalf("net.ListenUnix(fake backup server) error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	served := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			served <- err
			return
		}
		defer connection.Close()
		scanner := bufio.NewScanner(connection)
		if _, err := scanBackupSocketMessage(scanner); err != nil {
			served <- err
			return
		}
		served <- writeBackupSocketMessage(connection, backupSocketMessage{
			Version: backupSocketProtocolVersion, DataTargetHash: "wrong-target", Status: backupSocketStatusGranted,
		})
	}()
	if _, handled, err := acquireOnlineBackupBarrier(context.Background(), runtimeDir, targetHash); err == nil || !handled {
		t.Fatalf("unbound grant = handled:%t error:%v", handled, err)
	}
	if err := <-served; err != nil {
		t.Fatalf("fake backup server error = %v", err)
	}
}

func TestAcquireOnlineBackupBarrierAuthenticatesServerUID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("runtime owner mismatch setup requires root")
	}
	runtimeDir := newShortRuntimeDirectory(t)
	const targetHash = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: backupSocketPath(runtimeDir, targetHash), Net: "unix"})
	if err != nil {
		t.Fatalf("net.ListenUnix(fake backup server) error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chown(runtimeDir, 65534, 65534); err != nil {
		t.Fatalf("os.Chown(runtimeDir) error = %v", err)
	}
	accepted := make(chan struct{})
	go func() {
		connection, err := listener.AcceptUnix()
		if err == nil {
			_ = connection.Close()
		}
		close(accepted)
	}()
	if _, handled, err := acquireOnlineBackupBarrier(context.Background(), runtimeDir, targetHash); err == nil || !handled || !strings.Contains(err.Error(), "peer uid") {
		t.Fatalf("mismatched server uid = handled:%t error:%v", handled, err)
	}
	<-accepted
}
