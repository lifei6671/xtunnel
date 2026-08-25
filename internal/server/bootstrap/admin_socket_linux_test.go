//go:build linux

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/datadir"
)

func TestAdminBootstrapSocketCreatesOneAdminAndRemovesSocket(t *testing.T) {
	runtimeDir := newRuntimeDirectory(t)
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	const targetHash = "7d830b5ffe2a129fe0c742d3bd2406cdfda72abb777b434b22a41d8e16f80d15"
	socket, err := openAdminBootstrapSocketWith(context.Background(), runtimeDir, targetHash, store, func(*net.UnixConn) error { return nil })
	if err != nil {
		t.Fatalf("openAdminBootstrapSocketWith() error = %v", err)
	}
	t.Cleanup(func() {
		if err := socket.Close(); err != nil {
			t.Errorf("admin Bootstrap Socket Close() error = %v", err)
		}
	})

	socketPath := filepath.Join(runtimeDir, adminBootstrapSocketName)
	info, err := os.Lstat(socketPath)
	if err != nil {
		t.Fatalf("os.Lstat(socket) error = %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want Socket 0600", info.Mode())
	}
	handled, err := requestAdminBootstrap(context.Background(), socketPath, targetHash, "admin", "socket password")
	if !handled || err != nil {
		t.Fatalf("requestAdminBootstrap() = handled %t, error %v", handled, err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful Bootstrap Socket was not removed: %v", err)
	}
	if err := store.CreateFirstAdmin(context.Background(), "another", "another password"); !errors.Is(err, sqlite.ErrAdminAlreadyExists) {
		t.Fatalf("second CreateFirstAdmin() error = %v, want ErrAdminAlreadyExists", err)
	}
}

func TestAdminCreateRejectsBootstrapSocketForDifferentDataTarget(t *testing.T) {
	runtimeDir := newRuntimeDirectory(t)
	parent := t.TempDir()
	serverDataDir := filepath.Join(parent, "server-data")
	commandDataDir := filepath.Join(parent, "command-data")
	for _, path := range []string{serverDataDir, commandDataDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("os.Mkdir(%q) error = %v", path, err)
		}
	}
	serverTarget, err := datadir.Resolve(serverDataDir)
	if err != nil {
		t.Fatalf("datadir.Resolve(serverDataDir) error = %v", err)
	}
	store, err := sqlite.Open(context.Background(), serverDataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	socket, err := openAdminBootstrapSocketWith(context.Background(), runtimeDir, serverTarget.Hash, store, func(*net.UnixConn) error { return nil })
	if err != nil {
		t.Fatalf("openAdminBootstrapSocketWith() error = %v", err)
	}
	t.Cleanup(func() {
		if err := socket.Close(); err != nil {
			t.Errorf("admin Bootstrap Socket Close() error = %v", err)
		}
	})

	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("target-bound password\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(passwordFile) error = %v", err)
	}
	configPath := writeConfig(t, "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: tunnel.example.com\n")
	args := []string{"--config", configPath, "--set", "server.data_dir=" + commandDataDir, "--username", "admin", "--password-file", passwordFile}
	err = runAdminCreateWithRuntimeDir(context.Background(), "xtunnel-server", args, nil, &bytes.Buffer{}, runtimeDir)
	if err == nil || err.Error() != "running admin bootstrap rejected the request" {
		t.Fatalf("runAdminCreateWithRuntimeDir() error = %v, want target mismatch rejection", err)
	}
	hasAdmin, err := store.HasAdmin(context.Background())
	if err != nil {
		t.Fatalf("Store.HasAdmin() error = %v", err)
	}
	if hasAdmin {
		t.Fatal("mismatched Bootstrap request created an admin in the running Server data target")
	}
	if _, err := os.Stat(filepath.Join(commandDataDir, "xtunnel.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched Bootstrap request touched command data target: os.Stat() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(runtimeDir, adminBootstrapSocketName)); err != nil {
		t.Fatalf("target mismatch removed the running Server Bootstrap Socket: %v", err)
	}
}

func TestAdminBootstrapSocketStopsAfterCreateWhenClientDisconnects(t *testing.T) {
	runtimeDir := newRuntimeDirectory(t)
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	const targetHash = "4ecbb687ae7110116073efdc1a57bd0b3540f23673a060184b2f9ce4f29b3671"
	socket, err := openAdminBootstrapSocketWith(context.Background(), runtimeDir, targetHash, store, func(*net.UnixConn) error { return nil })
	if err != nil {
		t.Fatalf("openAdminBootstrapSocketWith() error = %v", err)
	}
	t.Cleanup(func() {
		if err := socket.Close(); err != nil {
			t.Errorf("admin Bootstrap Socket Close() error = %v", err)
		}
	})

	socketPath := filepath.Join(runtimeDir, adminBootstrapSocketName)
	connection, err := net.DialUnix(adminBootstrapNetwork, nil, &net.UnixAddr{Name: socketPath, Net: adminBootstrapNetwork})
	if err != nil {
		t.Fatalf("net.DialUnix() error = %v", err)
	}
	if err := json.NewEncoder(connection).Encode(adminBootstrapRequest{
		DataTargetHash: targetHash,
		Username:       "admin",
		Password:       "disconnected client password",
	}); err != nil {
		_ = connection.Close()
		t.Fatalf("encode admin Bootstrap request error = %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("client Close() error = %v", err)
	}

	deadline := time.Now().Add(adminBootstrapSocketTimeout)
	for {
		_, pathErr := os.Lstat(socketPath)
		if errors.Is(pathErr, os.ErrNotExist) {
			break
		}
		if pathErr != nil {
			t.Fatalf("os.Lstat(socket) error = %v", pathErr)
		}
		if time.Now().After(deadline) {
			t.Fatal("Bootstrap Socket remained after admin creation with a disconnected client")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hasAdmin, err := store.HasAdmin(context.Background())
	if err != nil {
		t.Fatalf("Store.HasAdmin() error = %v", err)
	}
	if !hasAdmin {
		t.Fatal("admin was not committed before the Bootstrap Socket stopped")
	}
}

func TestAdminBootstrapSocketRequiresRootPeer(t *testing.T) {
	runtimeDir := newRuntimeDirectory(t)
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	const targetHash = "8af9ccd5d28f30d94285f5655fdf21456f87d5fe998c7fd64ca4f3e260ced532"
	socket, err := openAdminBootstrapSocket(context.Background(), runtimeDir, targetHash, store)
	if err != nil {
		t.Fatalf("openAdminBootstrapSocket() error = %v", err)
	}
	t.Cleanup(func() {
		if err := socket.Close(); err != nil {
			t.Errorf("admin Bootstrap Socket Close() error = %v", err)
		}
	})

	handled, err := requestAdminBootstrap(context.Background(), filepath.Join(runtimeDir, adminBootstrapSocketName), targetHash, "admin", "root peer password")
	if !handled {
		t.Fatal("requestAdminBootstrap() did not use the running Bootstrap Socket")
	}
	hasAdmin, hasAdminErr := store.HasAdmin(context.Background())
	if hasAdminErr != nil {
		t.Fatalf("HasAdmin() error = %v", hasAdminErr)
	}
	if os.Geteuid() == 0 {
		if err != nil || !hasAdmin {
			t.Fatalf("root requestAdminBootstrap() = error %v, hasAdmin %t; want success", err, hasAdmin)
		}
		return
	}
	if err == nil || hasAdmin {
		t.Fatalf("non-root requestAdminBootstrap() = error %v, hasAdmin %t; want rejection without write", err, hasAdmin)
	}
	if _, statErr := os.Lstat(filepath.Join(runtimeDir, adminBootstrapSocketName)); statErr != nil {
		t.Fatalf("non-root rejection removed Bootstrap Socket: %v", statErr)
	}
}

func TestAdminBootstrapSocketRejectedPeerDoesNotCreateAdmin(t *testing.T) {
	runtimeDir := newRuntimeDirectory(t)
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	const targetHash = "0d44849d1513411ad283287d996410ad007549cd85caab9cae8ece933ce93f47"
	socket, err := openAdminBootstrapSocketWith(context.Background(), runtimeDir, targetHash, store, func(*net.UnixConn) error {
		return errors.New("test peer rejection")
	})
	if err != nil {
		t.Fatalf("openAdminBootstrapSocketWith() error = %v", err)
	}
	t.Cleanup(func() {
		if err := socket.Close(); err != nil {
			t.Errorf("admin Bootstrap Socket Close() error = %v", err)
		}
	})

	handled, err := requestAdminBootstrap(context.Background(), filepath.Join(runtimeDir, adminBootstrapSocketName), targetHash, "admin", "rejected peer password")
	if !handled || err == nil {
		t.Fatalf("requestAdminBootstrap() = handled %t, error %v; want handled rejection", handled, err)
	}
	hasAdmin, err := store.HasAdmin(context.Background())
	if err != nil {
		t.Fatalf("HasAdmin() error = %v", err)
	}
	if hasAdmin {
		t.Fatal("rejected Bootstrap peer created an admin")
	}
	if _, err := os.Lstat(filepath.Join(runtimeDir, adminBootstrapSocketName)); err != nil {
		t.Fatalf("rejected Bootstrap peer removed socket: %v", err)
	}
}

func TestAdminBootstrapSocketCloseStopsContextWatcher(t *testing.T) {
	runtimeDir := newRuntimeDirectory(t)
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	socket, err := openAdminBootstrapSocketWith(context.Background(), runtimeDir, "close-watcher", store, func(*net.UnixConn) error { return nil })
	if err != nil {
		t.Fatalf("openAdminBootstrapSocketWith() error = %v", err)
	}
	if err := socket.Close(); err != nil {
		t.Fatalf("admin Bootstrap Socket Close() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(runtimeDir, adminBootstrapSocketName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("admin Bootstrap Socket Close() left socket path: %v", err)
	}
	select {
	case <-socket.done:
	default:
		t.Fatal("Bootstrap Socket Close() did not stop its context watcher")
	}
}

func TestAdminCreateOfflineUsesExternalLockAndRejectsDuplicate(t *testing.T) {
	runtimeDir := newRuntimeDirectory(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(dataDir) error = %v", err)
	}
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("offline password\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(passwordFile) error = %v", err)
	}
	configPath := writeConfig(t, "management:\n  public_url: https://admin.example.com\nagent_gateway:\n  public_hostname: tunnel.example.com\n")
	args := []string{"--config", configPath, "--set", "server.data_dir=" + dataDir, "--username", "admin", "--password-file", passwordFile}
	if err := runAdminCreateWithRuntimeDir(context.Background(), "xtunnel-server", args, nil, &bytes.Buffer{}, runtimeDir); err != nil {
		t.Fatalf("offline runAdminCreateWithRuntimeDir() error = %v", err)
	}
	if err := runAdminCreateWithRuntimeDir(context.Background(), "xtunnel-server", args, nil, &bytes.Buffer{}, runtimeDir); !errors.Is(err, sqlite.ErrAdminAlreadyExists) {
		t.Fatalf("duplicate offline runAdminCreateWithRuntimeDir() error = %v, want ErrAdminAlreadyExists", err)
	}
}

func newRuntimeDirectory(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("os.Mkdir(runtimeDir) error = %v", err)
	}
	return path
}
