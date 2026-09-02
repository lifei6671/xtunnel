//go:build linux

package bootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

const (
	m7NetworkLargeTransferBytes = int64(1 << 30)
	m7NetworkImpairedBytes      = int64(8 << 20)
	m7NetworkTransferTimeout    = 10 * time.Minute
	m7NetworkResetTimeout       = 20 * time.Second
)

// TestM7PrivilegedNetworkChaos 只在外部 Runner 显式启用时运行。Runner 负责把整个
// Test Binary 放进独立 network namespace，并在该 namespace 的 loopback 上配置
// netem/nftables；本测试只从真实 Public TCP Listener 穿过生产 Server、Gateway、
// Token-only Agent 和 Origin，通过字节数、SHA-256、Half-Close 与连接恢复断言结果。
func TestM7PrivilegedNetworkChaos(t *testing.T) {
	if os.Getenv("XTUNNEL_RUN_M7_NETWORK_CHAOS") != "1" {
		t.Skip("set XTUNNEL_RUN_M7_NETWORK_CHAOS=1 and use tests/chaos/run-m7-08.sh")
	}
	seed := m7NetworkSeed(t)

	t.Run("large_bidirectional_transfer", func(t *testing.T) {
		testM7NetworkTransfer(t, m7NetworkLargeTransferBytes, seed)
	})
	t.Run("impaired_bidirectional_transfer", func(t *testing.T) {
		testM7NetworkTransfer(t, m7NetworkImpairedBytes, seed)
	})
	t.Run("tcp_reset_and_recovery", func(t *testing.T) {
		testM7NetworkResetAndRecovery(t, seed)
	})
}

func testM7NetworkTransfer(t *testing.T, byteCount int64, seed uint64) {
	baseline := m7MustReadShutdownResources(t)
	fixture := newM7ShutdownFixture(t, nil)
	waitForProductGateIdleWork(t, fixture.runtime, 1)

	public := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.1")
	origin := fixture.tcpOrigin.next(t, "M7-08 transfer")
	for _, connection := range []net.Conn{public, origin} {
		if err := connection.SetDeadline(time.Now().Add(m7NetworkTransferTimeout)); err != nil {
			t.Fatalf("set M7-08 transfer deadline: %v", err)
		}
	}
	m7AwaitNetworkProfile(t)

	upload := m7TransferOneDirection(t, public, origin, byteCount, seed, "upload")
	download := m7TransferOneDirection(t, origin, public, byteCount, seed^0x9e3779b97f4a7c15, "download")
	if err := public.Close(); err != nil {
		t.Fatalf("close M7-08 public connection: %v", err)
	}
	if err := origin.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close M7-08 Origin connection: %v", err)
	}

	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
	t.Logf("M7-08 transfer: bytes_per_direction=%d seed=%d upload_sha256=%s download_sha256=%s lost=0 duplicate=0 half_close=true",
		byteCount, seed, upload, download)
}

func m7TransferOneDirection(
	t *testing.T,
	sender net.Conn,
	receiver net.Conn,
	byteCount int64,
	seed uint64,
	direction string,
) string {
	t.Helper()
	type sendResult struct {
		count  int64
		digest string
		err    error
	}
	sent := make(chan sendResult, 1)
	go func() {
		digest := sha256.New()
		count, copyErr := io.CopyN(io.MultiWriter(sender, digest), &m7PatternReader{seed: seed}, byteCount)
		closeErr := m7CloseWrite(sender)
		sent <- sendResult{count: count, digest: hex.EncodeToString(digest.Sum(nil)), err: errors.Join(copyErr, closeErr)}
	}()

	receivedDigest := sha256.New()
	receivedCount, receiveErr := io.Copy(receivedDigest, receiver)
	send := <-sent
	if send.err != nil {
		t.Fatalf("send M7-08 %s stream: %v", direction, send.err)
	}
	if receiveErr != nil {
		t.Fatalf("receive M7-08 %s stream: %v", direction, receiveErr)
	}
	if send.count != byteCount || receivedCount != byteCount {
		t.Fatalf("M7-08 %s byte count = sent %d received %d, want %d", direction, send.count, receivedCount, byteCount)
	}
	received := hex.EncodeToString(receivedDigest.Sum(nil))
	if send.digest != received {
		t.Fatalf("M7-08 %s SHA-256 = sent %s received %s", direction, send.digest, received)
	}
	return received
}

func m7CloseWrite(connection net.Conn) error {
	tcpConnection, ok := connection.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("M7-08 connection %T does not support TCP Half-Close", connection)
	}
	return tcpConnection.CloseWrite()
}

func testM7NetworkResetAndRecovery(t *testing.T, seed uint64) {
	readyPath := m7RequiredMarkerPath(t, "XTUNNEL_M7_RESET_READY_FILE")
	observedPath := m7RequiredMarkerPath(t, "XTUNNEL_M7_RESET_OBSERVED_FILE")
	releasePath := m7RequiredMarkerPath(t, "XTUNNEL_M7_RESET_RELEASE_FILE")
	baseline := m7MustReadShutdownResources(t)
	fixture := newM7ShutdownFixture(t, nil)
	waitForProductGateIdleWork(t, fixture.runtime, 1)

	public := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.1")
	if err := public.SetDeadline(time.Now().Add(m7NetworkResetTimeout)); err != nil {
		t.Fatalf("set M7-08 reset public deadline: %v", err)
	}
	origin := fixture.tcpOrigin.next(t, "M7-08 reset")
	originDone := startProductGateOriginEcho(origin)
	assertProductGateRoundTrip(t, public, []byte("before-reset"), "M7-08 reset prime")
	m7WaitForAgentActiveWork(t, fixture, 1)
	_, portText, err := net.SplitHostPort(fixture.publicTCP)
	if err != nil {
		t.Fatalf("split M7-08 public address: %v", err)
	}
	if err := m7WriteMarker(readyPath, portText+"\n"); err != nil {
		t.Fatalf("publish M7-08 reset-ready marker: %v", err)
	}

	resetErr := m7DriveUntilReset(public, seed)
	if resetErr == nil {
		t.Fatal("M7-08 active stream did not unblock after privileged TCP Reset injection")
	}
	if networkErr, ok := resetErr.(net.Error); ok && networkErr.Timeout() {
		t.Fatalf("M7-08 active stream timed out instead of observing TCP Reset: %v", resetErr)
	}
	t.Logf("M7-08 active stream unblocked: reset_error=%q", resetErr)
	_ = public.Close()
	if err := m7WriteMarker(observedPath, resetErr.Error()+"\n"); err != nil {
		t.Fatalf("publish M7-08 reset-observed marker: %v", err)
	}
	m7WaitForMarker(t, releasePath, m7NetworkResetTimeout)
	select {
	case <-originDone:
	case <-time.After(m7NetworkResetTimeout):
		t.Fatal("M7-08 Origin stream did not unblock after TCP Reset")
	}
	waitForProductGateNoActiveWork(t, fixture.runtime)
	waitForProductGateIdleWork(t, fixture.runtime, 1)

	recovered := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.2")
	var recoveredOrigin net.Conn
	select {
	case recoveredOrigin = <-fixture.tcpOrigin.peers:
	case originErr := <-fixture.tcpOrigin.done:
		t.Fatalf("M7-08 Origin stopped before recovery accept: %v", originErr)
	case <-time.After(10 * time.Second):
		t.Fatal("M7-08 Origin did not receive the recovery Agent connection")
	}
	recoveredDone := startProductGateOriginEcho(recoveredOrigin)
	assertProductGateRoundTrip(t, recovered, []byte("after-reset"), "M7-08 reset recovery")
	finishProductGateTCP(t, recovered, recoveredDone, "M7-08 reset recovery")
	if err := recovered.Close(); err != nil {
		t.Fatalf("close M7-08 recovered public connection: %v", err)
	}

	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
	t.Logf("M7-08 reset: seed=%d active_unblocked=true recovered=true lost=0 duplicate=0 reset_error=%q", seed, resetErr)
}

func m7DriveUntilReset(connection net.Conn, seed uint64) error {
	payload := make([]byte, 32<<10)
	reader := &m7PatternReader{seed: seed}
	if _, err := io.ReadFull(reader, payload); err != nil {
		return fmt.Errorf("generate M7-08 reset payload: %w", err)
	}
	echoed := make([]byte, len(payload))
	for {
		if _, err := connection.Write(payload); err != nil {
			return err
		}
		if _, err := io.ReadFull(connection, echoed); err != nil {
			return err
		}
		if !bytes.Equal(echoed, payload) {
			return errors.New("M7-08 reset stream returned modified bytes before reset")
		}
	}
}

func m7NetworkSeed(t *testing.T) uint64 {
	t.Helper()
	value := os.Getenv("XTUNNEL_M7_NETWORK_SEED")
	if value == "" {
		t.Fatal("XTUNNEL_M7_NETWORK_SEED is required")
	}
	seed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || seed == 0 {
		t.Fatalf("XTUNNEL_M7_NETWORK_SEED = %q, want a positive uint64", value)
	}
	return seed
}

func m7RequiredMarkerPath(t *testing.T, name string) string {
	t.Helper()
	path := os.Getenv(name)
	if path == "" {
		t.Fatalf("%s is required for the reset scenario", name)
	}
	return path
}

// m7AwaitNetworkProfile 让 Runner 只在公网连接、WorkConn 和 Origin 都已就绪后注入
// netem。这样 Loss/Jitter 验收的是生产数据流，不会因随机丢失建链 SYN 而把拨号超时
// 误判成传输完整性失败；直接运行单个 clean 测试时可不提供这组 marker。
func m7AwaitNetworkProfile(t *testing.T) {
	t.Helper()
	readyPath := os.Getenv("XTUNNEL_M7_PROFILE_READY_FILE")
	releasePath := os.Getenv("XTUNNEL_M7_PROFILE_RELEASE_FILE")
	if readyPath == "" && releasePath == "" {
		return
	}
	if readyPath == "" || releasePath == "" {
		t.Fatal("XTUNNEL_M7_PROFILE_READY_FILE and XTUNNEL_M7_PROFILE_RELEASE_FILE must be set together")
	}
	if err := m7WriteMarker(readyPath, "ready\n"); err != nil {
		t.Fatalf("publish M7-08 profile-ready marker: %v", err)
	}
	m7WaitForMarker(t, releasePath, m7NetworkResetTimeout)
}

func m7WriteMarker(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func m7WaitForMarker(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect M7-08 marker %s: %v", path, err)
		}
		select {
		case <-deadline.C:
			t.Fatalf("M7-08 marker did not appear within %s: %s", timeout, path)
		case <-ticker.C:
		}
	}
}

type m7PatternReader struct {
	seed     uint64
	pattern  []byte
	position int
}

func (reader *m7PatternReader) Read(buffer []byte) (int, error) {
	if reader.pattern == nil {
		reader.pattern = make([]byte, 32<<10)
		state := reader.seed
		for index := range reader.pattern {
			state ^= state >> 12
			state ^= state << 25
			state ^= state >> 27
			reader.pattern[index] = byte((state * 0x2545f4914f6cdd1d) >> 56)
		}
	}
	written := 0
	for written < len(buffer) {
		copied := copy(buffer[written:], reader.pattern[reader.position:])
		written += copied
		reader.position = (reader.position + copied) % len(reader.pattern)
	}
	return len(buffer), nil
}

var _ io.Reader = (*m7PatternReader)(nil)
