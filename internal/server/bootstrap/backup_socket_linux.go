//go:build linux

package bootstrap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/safego"
	"golang.org/x/sys/unix"
)

const (
	// backupSocketProtocolVersion 冻结本机 JSONL 协议版本；消息上限约束
	// Scanner/Reader 的内存占用，握手时限覆盖授权、Acquire 和 Release ACK。
	backupSocketProtocolVersion = 1
	backupSocketMessageLimit    = 4 * 1024
	backupSocketHandshake       = 5 * time.Second
)

const (
	// Backup Socket 是两阶段会话：客户端 acquire、服务端 granted，完成捕获后
	// 客户端 release、服务端 released。rejected 只表示本次会话不可继续。
	backupSocketActionAcquire  = "acquire"
	backupSocketActionRelease  = "release"
	backupSocketStatusGranted  = "granted"
	backupSocketStatusReleased = "released"
	backupSocketStatusRejected = "rejected"
)

// backupSocketMessage 是一行一个 JSON 对象的本机协议帧。
// DataTargetHash 在 acquire/granted/released 中绑定稳定 data target，防止 CLI
// 连接到其他实例的 Socket；release 必须省略该字段，由现有连接身份确定租约。
type backupSocketMessage struct {
	Version        int    `json:"version"`
	DataTargetHash string `json:"data_target_hash,omitempty"`
	Action         string `json:"action,omitempty"`
	Status         string `json:"status,omitempty"`
}

// backupSocketPath 为每个稳定 data target 派生唯一 Socket 路径，使同机多实例
// 的维护屏障互不串线。
func backupSocketPath(runtimeDir, targetHash string) string {
	return filepath.Join(runtimeDir, "backup-"+targetHash+".sock")
}

// onlineBackupLease 是 CLI 侧的单连接租约 owner。
//
// readResponse goroutine 是 connection 的唯一 Reader，并把唯一的终态响应写入
// response/readErr 后关闭 done。Close 是唯一 Writer/关闭者；once 保证
// BeforePublish 与外层 defer/显式清理重复调用时仍只发送一次 release。
type onlineBackupLease struct {
	connection net.Conn
	targetHash string
	done       chan struct{}
	readMu     sync.Mutex
	response   backupSocketMessage
	readErr    error
	once       sync.Once
	result     error
}

// acquireOnlineBackupBarrier 只在持久 Backup Socket 存在时连接运行中的 Server。
// handled=false 是唯一允许调用方转入离线 External Lock 路径的条件；Socket
// 存在后的 Lstat、连接、SO_PEERCRED、协议或 target hash 失败都返回 handled=true，
// 禁止把运行中 Server 的异常误判成离线。握手完成后清除 Deadline，由租约生命周期
// 和显式 Close 负责解除阻塞。
func acquireOnlineBackupBarrier(ctx context.Context, runtimeDir, targetHash string) (backupLease, bool, error) {
	path := backupSocketPath(runtimeDir, targetHash)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("inspect backup barrier socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return nil, true, fmt.Errorf("backup barrier path %q is not a Unix socket", path)
	}

	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, true, fmt.Errorf("connect to running backup barrier socket: %w", err)
	}
	fail := func(cause error) (backupLease, bool, error) {
		return nil, true, errors.Join(cause, connection.Close())
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return fail(errors.New("backup barrier connection is not a Unix connection"))
	}
	if err := requireBackupServerPeer(unixConnection, runtimeDir); err != nil {
		return fail(err)
	}
	if err := connection.SetDeadline(time.Now().Add(backupSocketHandshake)); err != nil {
		return fail(fmt.Errorf("set backup barrier handshake deadline: %w", err))
	}
	if err := writeBackupSocketMessage(connection, backupSocketMessage{
		Version: backupSocketProtocolVersion, DataTargetHash: targetHash, Action: backupSocketActionAcquire,
	}); err != nil {
		return fail(fmt.Errorf("request online backup barrier: %w", err))
	}
	response, err := readBackupSocketMessage(bufio.NewReader(connection))
	if err != nil {
		return fail(fmt.Errorf("read online backup barrier response: %w", err))
	}
	if response.Version != backupSocketProtocolVersion || response.Status != backupSocketStatusGranted || response.DataTargetHash != targetHash {
		return fail(errors.New("running Server rejected the online backup barrier"))
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return fail(fmt.Errorf("clear backup barrier handshake deadline: %w", err))
	}
	lease := &onlineBackupLease{connection: connection, targetHash: targetHash, done: make(chan struct{})}
	go lease.readResponse()
	return lease, true, nil
}

// BindContext 把 Server 端 EOF、Shutdown 或任意提前响应转换为 Create 的取消。
// 单独的 Reader 始终是 Socket 唯一读者，Close 只等待它保存的 release ACK。
// 监视 goroutine 由返回的 cancel 或 done 收敛，调用方必须在 Create 返回后调用
// cancel，避免父 Context 长期存活时遗留等待者。
func (lease *onlineBackupLease) BindContext(parent context.Context) (context.Context, context.CancelFunc) {
	leaseContext, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-lease.done:
			cancel()
		case <-leaseContext.Done():
		}
	}()
	return leaseContext, cancel
}

// readResponse 执行连接上的唯一阻塞读取。它先在锁内发布响应，再关闭 done，
// 因而等待方观察到 done 后一定能读取完整结果。
func (lease *onlineBackupLease) readResponse() {
	response, err := readBackupSocketMessage(bufio.NewReader(lease.connection))
	lease.readMu.Lock()
	lease.response = response
	lease.readErr = err
	lease.readMu.Unlock()
	close(lease.done)
}

// Close 发送显式 release，并等待唯一 Reader 收到与 target 绑定的 released ACK；
// 该 ACK 是 durableops BeforePublish 的成功条件。若调用方崩溃或连接断开，Server
// 端 EOF 路径仍会通过 defer 释放租约，但本次 CLI 操作不会被报告为成功。
// 所有写入、协议校验与连接关闭错误都汇总到稳定结果，重复 Close 返回同一结果。
func (lease *onlineBackupLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.once.Do(func() {
		select {
		case <-lease.done:
			lease.readMu.Lock()
			readErr := lease.readErr
			lease.readMu.Unlock()
			if readErr == nil {
				readErr = errors.New("unexpected backup barrier response before release")
			}
			lease.result = errors.Join(fmt.Errorf("online backup barrier was lost before release: %w", readErr), lease.connection.Close())
			return
		default:
		}
		if err := lease.connection.SetDeadline(time.Now().Add(backupSocketHandshake)); err != nil {
			lease.result = errors.Join(fmt.Errorf("set backup barrier release deadline: %w", err), lease.connection.Close())
			return
		}
		if err := writeBackupSocketMessage(lease.connection, backupSocketMessage{
			Version: backupSocketProtocolVersion, Action: backupSocketActionRelease,
		}); err != nil {
			lease.result = errors.Join(fmt.Errorf("release online backup barrier: %w", err), lease.connection.Close())
			return
		}
		<-lease.done
		lease.readMu.Lock()
		response, err := lease.response, lease.readErr
		lease.readMu.Unlock()
		if err != nil {
			lease.result = errors.Join(fmt.Errorf("read backup barrier release response: %w", err), lease.connection.Close())
			return
		}
		if response.Version != backupSocketProtocolVersion || response.Status != backupSocketStatusReleased || response.DataTargetHash != lease.targetHash {
			lease.result = errors.Join(errors.New("running Server rejected the backup barrier release"), lease.connection.Close())
			return
		}
		if err := lease.connection.Close(); err != nil {
			lease.result = fmt.Errorf("close backup barrier socket: %w", err)
		}
	})
	return lease.result
}

// backupBarrierSocket 是运行中 Server 的持久 Socket owner。
//
// serve goroutine 独占 Accept；每个已接收连接由一个 handle goroutine 独占读写，
// 且该连接持有的所有 Barrier 都在 handle 返回前释放。context watcher 负责把上游
// 取消转换成 stop。Close/运行时错误只触发一次 stop，主动关闭 Listener 和全部连接
// 以解除 Accept/Read/Write，再由 wait 等待所有 goroutine 退出。
type backupBarrierSocket struct {
	listener           *net.UnixListener
	path               string
	targetHash         string
	store              *sqlite.Store
	authorize          func(*net.UnixConn) error
	reportRuntimeError func(error)

	// stopOnce/done 固定停止转换；wait 覆盖 accept、context watcher 和所有请求。
	stopOnce sync.Once
	done     chan struct{}
	wait     sync.WaitGroup
	// connMu 保护活动连接集合与 stopping；锁内只复制/登记状态，不执行 Close。
	connMu   sync.Mutex
	conns    map[*net.UnixConn]struct{}
	stopping bool
	// errMu 保护首个运行时错误和停止阶段聚合错误；Close 最终统一返回两者。
	errMu   sync.Mutex
	err     error
	stopErr error
}

// openBackupBarrierSocket 使用生产 root peer authorizer 创建目标 Socket。
// 返回对象归 Server 生命周期所有，启动成功后必须由 Close 等待完全退出。
func openBackupBarrierSocket(
	ctx context.Context,
	runtimeDir, targetHash string,
	store *sqlite.Store,
	reportRuntimeError func(error),
) (*backupBarrierSocket, error) {
	return openBackupBarrierSocketWith(ctx, runtimeDir, targetHash, store, requireRootBootstrapPeer, reportRuntimeError)
}

// openBackupBarrierSocketWith 完成 Socket 发布和 goroutine 装配。
// authorize 参数仅用于注入 peer 认证策略/测试；生产入口固定使用 SO_PEERCRED。
// 旧路径只有确认为 Unix Socket 才可清理，非 Socket 文件会快速失败，避免覆盖
// runtimeDir 中的非本服务对象。权限设为 0600 后才启动接收循环。
func openBackupBarrierSocketWith(
	ctx context.Context,
	runtimeDir, targetHash string,
	store *sqlite.Store,
	authorize func(*net.UnixConn) error,
	reportRuntimeError func(error),
) (*backupBarrierSocket, error) {
	if store == nil {
		return nil, errors.New("backup barrier SQLite Store is required")
	}
	if authorize == nil {
		return nil, errors.New("backup barrier peer authorizer is required")
	}
	path := backupSocketPath(runtimeDir, targetHash)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("backup barrier path %q is not a Unix socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale backup barrier socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect backup barrier socket: %w", err)
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on backup barrier socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, errors.Join(
			fmt.Errorf("set backup barrier socket permissions: %w", err),
			listener.Close(), os.Remove(path),
		)
	}

	socket := &backupBarrierSocket{
		listener: listener, path: path, targetHash: targetHash, store: store,
		authorize: authorize, reportRuntimeError: reportRuntimeError, done: make(chan struct{}),
		conns: make(map[*net.UnixConn]struct{}),
	}
	socket.wait.Add(2)
	safego.Go(socket.handlePanic("backup barrier accept loop"), socket.wait.Done, func() { socket.serve(ctx) })
	safego.Go(socket.handlePanic("backup barrier context watcher"), socket.wait.Done, func() {
		select {
		case <-ctx.Done():
			socket.stop()
		case <-socket.done:
		}
	})
	return socket, nil
}

// serve 是 Listener 的唯一 Accept owner。连接先登记到 conns 再启动请求 goroutine，
// 保证并发 stop 能取得并关闭每个已接收 FD；stopping 后接到的连接立即关闭。
func (socket *backupBarrierSocket) serve(ctx context.Context) {
	for {
		connection, err := socket.listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			socket.recordRuntimeError(fmt.Errorf("accept backup barrier connection: %w", err))
			return
		}
		socket.connMu.Lock()
		if socket.stopping {
			socket.connMu.Unlock()
			_ = connection.Close()
			return
		}
		socket.conns[connection] = struct{}{}
		socket.connMu.Unlock()
		socket.wait.Add(1)
		safego.Go(socket.handlePanic("backup barrier request"), socket.wait.Done, func() {
			defer socket.removeConnection(connection)
			socket.handle(ctx, connection)
		})
	}
}

// handle 管理一个连接的完整 Lease 生命周期。
//
// 它先在握手 Deadline 内完成 SO_PEERCRED 授权、严格 acquire 解码和写屏障获取，
// 只有屏障真实持有后才返回 granted。此后清除 Deadline，连接本身拥有该 Barrier；
// 正常 release、EOF、协议错误、Server stop 或 panic 最终都经 defer Release 收敛。
// 显式 release 在发送 released ACK 前先释放屏障，使 ACK 精确表示 Server 已恢复写入。
func (socket *backupBarrierSocket) handle(ctx context.Context, connection *net.UnixConn) {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(backupSocketHandshake)); err != nil {
		return
	}
	if err := socket.authorize(connection); err != nil {
		_ = writeBackupSocketMessage(connection, backupSocketMessage{
			Version: backupSocketProtocolVersion,
			Status:  backupSocketStatusRejected,
		})
		return
	}
	scanner := bufio.NewScanner(connection)
	scanner.Buffer(make([]byte, 1024), backupSocketMessageLimit)
	request, err := scanBackupSocketMessage(scanner)
	if err != nil || request.Version != backupSocketProtocolVersion || request.Action != backupSocketActionAcquire || request.DataTargetHash != socket.targetHash {
		_ = writeBackupSocketMessage(connection, backupSocketMessage{
			Version: backupSocketProtocolVersion,
			Status:  backupSocketStatusRejected,
		})
		return
	}
	acquireContext, cancelAcquire := context.WithTimeout(ctx, backupSocketHandshake)
	barrier, err := socket.store.AcquireBackupBarrier(acquireContext)
	cancelAcquire()
	if err != nil {
		_ = writeBackupSocketMessage(connection, backupSocketMessage{
			Version: backupSocketProtocolVersion,
			Status:  backupSocketStatusRejected,
		})
		return
	}
	defer barrier.Release()
	if err := writeBackupSocketMessage(connection, backupSocketMessage{
		Version:        backupSocketProtocolVersion,
		DataTargetHash: socket.targetHash,
		Status:         backupSocketStatusGranted,
	}); err != nil {
		return
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return
	}
	release, err := scanBackupSocketMessage(scanner)
	if err != nil || release.Version != backupSocketProtocolVersion || release.Action != backupSocketActionRelease || release.DataTargetHash != "" {
		return
	}
	barrier.Release()
	_ = connection.SetDeadline(time.Now().Add(backupSocketHandshake))
	_ = writeBackupSocketMessage(connection, backupSocketMessage{
		Version:        backupSocketProtocolVersion,
		DataTargetHash: socket.targetHash,
		Status:         backupSocketStatusReleased,
	})
}

// removeConnection 在请求 goroutine 退出时撤销 FD 登记；连接关闭由 handle 的
// defer 负责，避免集合管理与 IO 生命周期出现两个 owner。
func (socket *backupBarrierSocket) removeConnection(connection *net.UnixConn) {
	socket.connMu.Lock()
	delete(socket.conns, connection)
	socket.connMu.Unlock()
}

// stop 执行 exactly-once 停止：先关闭 done/Listener 阻止新入口，再在锁外关闭
// 活动连接解除请求 IO，最后移除 Socket 路径。这里只发出停止信号，不等待 goroutine，
// 从 request goroutine 的运行时错误路径调用也不会自锁；等待统一留给 Close。
func (socket *backupBarrierSocket) stop() {
	socket.stopOnce.Do(func() {
		close(socket.done)
		stopErr := socket.listener.Close()
		socket.connMu.Lock()
		socket.stopping = true
		connections := make([]*net.UnixConn, 0, len(socket.conns))
		for connection := range socket.conns {
			connections = append(connections, connection)
		}
		socket.connMu.Unlock()
		for _, connection := range connections {
			if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				stopErr = errors.Join(stopErr, err)
			}
		}
		if err := os.Remove(socket.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			stopErr = errors.Join(stopErr, err)
		}
		socket.errMu.Lock()
		socket.stopErr = stopErr
		socket.errMu.Unlock()
	})
}

// Close 先停止新租约并主动解除现有 Socket IO，再等待所有 Barrier Release 完成。
// 返回值聚合首个运行时错误与 Listener、连接、路径清理错误，不会用后续错误覆盖根因。
func (socket *backupBarrierSocket) Close() error {
	if socket == nil {
		return nil
	}
	socket.stop()
	socket.wait.Wait()
	socket.errMu.Lock()
	defer socket.errMu.Unlock()
	return errors.Join(socket.err, socket.stopErr)
}

// requireBackupServerPeer 在 CLI 侧通过 SO_PEERCRED 验证对端 UID 与 runtimeDir
// owner 一致。Unix Socket 路径和 0600 权限不足以抵御目录替换或错误实例，因此
// peer 身份与 granted 中的 target hash 必须同时成立。
func requireBackupServerPeer(connection *net.UnixConn, runtimeDir string) error {
	runtimeInfo, err := os.Lstat(runtimeDir)
	if err != nil {
		return fmt.Errorf("inspect backup runtime directory owner: %w", err)
	}
	owner, ok := runtimeInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("backup runtime directory owner is unavailable")
	}
	raw, err := connection.SyscallConn()
	if err != nil {
		return fmt.Errorf("access backup server peer file descriptor: %w", err)
	}
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			peerErr = err
			return
		}
		if credentials.Uid != owner.Uid {
			peerErr = fmt.Errorf("backup server peer uid %d does not match runtime directory uid %d", credentials.Uid, owner.Uid)
		}
	}); err != nil {
		return fmt.Errorf("inspect backup server peer credentials: %w", err)
	}
	if peerErr != nil {
		return fmt.Errorf("authorize backup server peer: %w", peerErr)
	}
	return nil
}

// handlePanic 把 safego 捕获的 goroutine panic 转成带操作上下文的运行时错误，
// 交由统一错误 owner 触发 Socket 停止。
func (socket *backupBarrierSocket) handlePanic(operation string) func(error) {
	return func(err error) {
		socket.recordRuntimeError(fmt.Errorf("%s: %w", operation, err))
	}
}

// recordRuntimeError 只保留并上报首个运行时错误。首错触发 stop，使其余 goroutine
// 通过 Listener/连接关闭快速退出；后续清理错误由 stopErr 单独聚合并在 Close 返回。
func (socket *backupBarrierSocket) recordRuntimeError(err error) {
	socket.errMu.Lock()
	first := socket.err == nil
	if first {
		socket.err = err
	}
	report := socket.reportRuntimeError
	socket.errMu.Unlock()
	if !first {
		return
	}
	socket.stop()
	if report != nil {
		report(err)
	}
}

// scanBackupSocketMessage 读取服务端连接中的一个有界 JSONL 帧；Scanner 上限由
// handle 在读取前设置，EOF 与语法错误均交给会话 owner 决定是否释放租约。
func scanBackupSocketMessage(scanner *bufio.Scanner) (backupSocketMessage, error) {
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return backupSocketMessage{}, err
		}
		return backupSocketMessage{}, io.EOF
	}
	return decodeBackupSocketMessage(scanner.Bytes())
}

// readBackupSocketMessage 是 CLI 唯一 Reader 使用的有界帧读取器。
// ReadSlice 保留换行边界，并在 JSON 解码前拒绝超过协议上限的响应。
func readBackupSocketMessage(reader *bufio.Reader) (backupSocketMessage, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return backupSocketMessage{}, err
	}
	if len(line) > backupSocketMessageLimit {
		return backupSocketMessage{}, errors.New("backup barrier message exceeds limit")
	}
	return decodeBackupSocketMessage(bytes.TrimSuffix(line, []byte{'\n'}))
}

// decodeBackupSocketMessage 严格拒绝未知字段和同一行尾随 JSON，避免新旧协议双方
// 对同一帧产生不同解释；动作、状态和 target 的阶段语义由各会话 owner 校验。
func decodeBackupSocketMessage(encoded []byte) (backupSocketMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var message backupSocketMessage
	if err := decoder.Decode(&message); err != nil {
		return backupSocketMessage{}, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return backupSocketMessage{}, errors.New("backup barrier message contains trailing JSON")
		}
		return backupSocketMessage{}, err
	}
	return message, nil
}

// writeBackupSocketMessage 把一个协议对象编码为单行 JSON，并由调用方设置的连接
// Deadline 约束阻塞时长。每个连接只有其会话 owner 写入，因此无需额外 Writer 锁。
func writeBackupSocketMessage(writer io.Writer, message backupSocketMessage) error {
	return json.NewEncoder(writer).Encode(message)
}
