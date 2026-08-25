//go:build linux

package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"golang.org/x/sys/unix"
)

const (
	adminBootstrapSocketTimeout = 5 * time.Second
	adminBootstrapResponseLimit = 4 * 1024
	adminBootstrapRequestLimit  = 64 * 1024
)

type adminBootstrapRequest struct {
	DataTargetHash string `json:"data_target_hash"`
	Username       string `json:"username"`
	Password       string `json:"password"`
}

type adminBootstrapResponse struct {
	Status string `json:"status"`
}

// requestAdminBootstrap 仅在 Socket 存在时向运行中的 Server 请求创建。
// handled 为 false 表示没有运行中的 Bootstrap Socket，调用方可改走离线路径。
func requestAdminBootstrap(ctx context.Context, socketPath, dataTargetHash, username, password string) (handled bool, resultErr error) {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("inspect admin bootstrap socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return true, fmt.Errorf("admin bootstrap path %q is not a Unix socket", socketPath)
	}

	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, adminBootstrapNetwork, socketPath)
	if err != nil {
		return true, fmt.Errorf("connect to running admin bootstrap socket: %w", err)
	}
	defer func() {
		if err := connection.Close(); resultErr == nil && err != nil {
			resultErr = fmt.Errorf("close admin bootstrap socket: %w", err)
		}
	}()
	if err := connection.SetDeadline(time.Now().Add(adminBootstrapSocketTimeout)); err != nil {
		return true, fmt.Errorf("set admin bootstrap socket deadline: %w", err)
	}
	if err := json.NewEncoder(connection).Encode(adminBootstrapRequest{
		DataTargetHash: dataTargetHash,
		Username:       username,
		Password:       password,
	}); err != nil {
		return true, fmt.Errorf("write admin bootstrap request: %w", err)
	}
	var response adminBootstrapResponse
	if err := json.NewDecoder(io.LimitReader(connection, adminBootstrapResponseLimit)).Decode(&response); err != nil {
		return true, fmt.Errorf("read admin bootstrap response: %w", err)
	}
	switch response.Status {
	case adminBootstrapStatusCreated:
		return true, nil
	case adminBootstrapStatusAlreadyInitialized:
		return true, sqlite.ErrAdminAlreadyExists
	default:
		return true, errors.New("running admin bootstrap rejected the request")
	}
}

type adminBootstrapSocket struct {
	listener   *net.UnixListener
	path       string
	store      *sqlite.Store
	targetHash string
	authorize  func(*net.UnixConn) error

	stopOnce sync.Once
	wait     sync.WaitGroup
}

func openAdminBootstrapSocket(ctx context.Context, runtimeDir, targetHash string, store *sqlite.Store) (*adminBootstrapSocket, error) {
	return openAdminBootstrapSocketWith(ctx, runtimeDir, targetHash, store, requireRootBootstrapPeer)
}

func openAdminBootstrapSocketWith(ctx context.Context, runtimeDir, targetHash string, store *sqlite.Store, authorize func(*net.UnixConn) error) (*adminBootstrapSocket, error) {
	path := runtimeDir + "/" + adminBootstrapSocketName
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("admin bootstrap path %q is not a Unix socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale admin bootstrap socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect admin bootstrap socket: %w", err)
	}

	listener, err := net.ListenUnix(adminBootstrapNetwork, &net.UnixAddr{Name: path, Net: adminBootstrapNetwork})
	if err != nil {
		return nil, fmt.Errorf("listen on admin bootstrap socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		closeErr := listener.Close()
		removeErr := os.Remove(path)
		return nil, errors.Join(fmt.Errorf("set admin bootstrap socket permissions: %w", err), closeErr, removeErr)
	}

	socket := &adminBootstrapSocket{listener: listener, path: path, store: store, targetHash: targetHash, authorize: authorize}
	socket.wait.Add(1)
	go socket.serve(ctx)
	return socket, nil
}

func (socket *adminBootstrapSocket) serve(ctx context.Context) {
	defer socket.wait.Done()
	go func() {
		<-ctx.Done()
		socket.stop()
	}()
	for {
		connection, err := socket.listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			return
		}
		socket.wait.Add(1)
		go func() {
			defer socket.wait.Done()
			socket.handle(ctx, connection)
		}()
	}
}

func (socket *adminBootstrapSocket) handle(ctx context.Context, connection *net.UnixConn) {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(adminBootstrapSocketTimeout)); err != nil {
		return
	}
	if err := socket.authorize(connection); err != nil {
		_ = json.NewEncoder(connection).Encode(adminBootstrapResponse{Status: adminBootstrapStatusRejected})
		return
	}
	var request adminBootstrapRequest
	decoder := json.NewDecoder(io.LimitReader(connection, adminBootstrapRequestLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(adminBootstrapResponse{Status: adminBootstrapStatusRejected})
		return
	}
	if request.DataTargetHash != socket.targetHash {
		_ = json.NewEncoder(connection).Encode(adminBootstrapResponse{Status: adminBootstrapStatusRejected})
		return
	}
	if err := socket.store.CreateFirstAdmin(ctx, request.Username, request.Password); err != nil {
		if errors.Is(err, sqlite.ErrAdminAlreadyExists) {
			_ = json.NewEncoder(connection).Encode(adminBootstrapResponse{Status: adminBootstrapStatusAlreadyInitialized})
			return
		}
		_ = json.NewEncoder(connection).Encode(adminBootstrapResponse{Status: adminBootstrapStatusRejected})
		return
	}
	socket.stop()
	_ = json.NewEncoder(connection).Encode(adminBootstrapResponse{Status: adminBootstrapStatusCreated})
}

func (socket *adminBootstrapSocket) stop() {
	socket.stopOnce.Do(func() {
		_ = socket.listener.Close()
		_ = os.Remove(socket.path)
	})
}

// Close 停止接收新的 Bootstrap 请求，并等待已接收请求完成。
func (socket *adminBootstrapSocket) Close() error {
	socket.stop()
	socket.wait.Wait()
	return nil
}

func requireRootBootstrapPeer(connection *net.UnixConn) error {
	raw, err := connection.SyscallConn()
	if err != nil {
		return fmt.Errorf("access bootstrap peer file descriptor: %w", err)
	}
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			peerErr = err
			return
		}
		if credentials.Uid != 0 {
			peerErr = fmt.Errorf("admin bootstrap peer uid %d is not root", credentials.Uid)
		}
	}); err != nil {
		return fmt.Errorf("inspect bootstrap peer credentials: %w", err)
	}
	if peerErr != nil {
		return fmt.Errorf("authorize bootstrap peer: %w", peerErr)
	}
	return nil
}
