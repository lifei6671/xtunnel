//go:build linux

package bootstrap

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestListenTCPBindsIPv4AndIPv6(t *testing.T) {
	listeners, err := listenTCP(context.Background(), ":0")
	if err != nil {
		t.Fatalf("listenTCP() error = %v", err)
	}
	t.Cleanup(func() {
		if err := closeListeners(listeners); err != nil {
			t.Errorf("closeListeners() error = %v", err)
		}
	})

	if len(listeners) != 2 {
		t.Fatalf("len(listeners) = %d, want 2", len(listeners))
	}
	ipv4Port := listeners[0].Addr().(*net.TCPAddr).Port
	ipv6Port := listeners[1].Addr().(*net.TCPAddr).Port
	if ipv4Port != ipv6Port {
		t.Fatalf("listener ports = tcp4:%d tcp6:%d, want the same port", ipv4Port, ipv6Port)
	}
	assertListenerAccepts(t, listeners[0], "tcp4", "127.0.0.1")
	assertListenerAccepts(t, listeners[1], "tcp6", "::1")
}

func TestListenTCPCleansUpIPv4WhenIPv6BindFails(t *testing.T) {
	blocker, err := net.Listen("tcp6", "[::]:0")
	if err != nil {
		t.Fatalf("reserve IPv6 port: %v", err)
	}
	t.Cleanup(func() {
		if err := blocker.Close(); err != nil {
			t.Errorf("blocker.Close() error = %v", err)
		}
	})

	port := blocker.Addr().(*net.TCPAddr).Port
	_, err = listenTCP(context.Background(), net.JoinHostPort("", strconv.Itoa(port)))
	if err == nil || !strings.Contains(err.Error(), "tcp6") {
		t.Fatalf("listenTCP() error = %v, want IPv6 bind failure", err)
	}

	// 第二个 Socket 绑定失败后，第一个 IPv4 Socket 必须已经释放。
	reopened, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("IPv4 port was not released: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("reopened.Close() error = %v", err)
	}
}

func TestListenTCPUsesIPv6OnlySocketForIPv6Family(t *testing.T) {
	listeners, err := listenTCP(context.Background(), ":0")
	if err != nil {
		t.Fatalf("listenTCP() error = %v", err)
	}
	t.Cleanup(func() {
		if err := closeListeners(listeners); err != nil {
			t.Errorf("closeListeners() error = %v", err)
		}
	})

	ipv6Listener := listeners[1].(*net.TCPListener)
	rawConnection, err := ipv6Listener.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn() error = %v", err)
	}
	var option int
	var optionErr error
	if err := rawConnection.Control(func(fileDescriptor uintptr) {
		option, optionErr = unix.GetsockoptInt(int(fileDescriptor), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
	}); err != nil {
		t.Fatalf("Control() error = %v", err)
	}
	if optionErr != nil {
		t.Fatalf("GetsockoptInt(IPV6_V6ONLY) error = %v", optionErr)
	}
	if option != 1 {
		t.Fatalf("IPV6_V6ONLY = %d, want 1", option)
	}
}

func assertListenerAccepts(t *testing.T, listener net.Listener, network, host string) {
	t.Helper()
	tcpListener := listener.(*net.TCPListener)
	if err := tcpListener.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetDeadline() error = %v", err)
	}

	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			err = connection.Close()
		}
		accepted <- err
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	connection, err := net.DialTimeout(network, net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		t.Fatalf("DialTimeout(%s) error = %v", network, err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("connection.Close() error = %v", err)
	}
	if err := <-accepted; err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
}
