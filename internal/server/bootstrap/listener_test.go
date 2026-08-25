package bootstrap

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestListenTCPKeepsExplicitAddressFamily(t *testing.T) {
	tests := []struct {
		name    string
		address string
		network string
	}{
		{name: "IPv4 wildcard", address: "0.0.0.0:7443", network: "tcp4"},
		{name: "IPv4 loopback", address: "127.0.0.1:7443", network: "tcp4"},
		{name: "IPv6 wildcard", address: "[::]:7443", network: "tcp6"},
		{name: "IPv6 loopback", address: "[::1]:7443", network: "tcp6"},
		{name: "IPv6 with zone", address: "[fe80::1%lo]:7443", network: "tcp6"},
		{name: "hostname", address: "localhost:7443", network: "tcp"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host, _, err := net.SplitHostPort(test.address)
			if err != nil {
				t.Fatalf("net.SplitHostPort() error = %v", err)
			}
			targets := tcpListenTargets(host)
			if len(targets) != 1 || targets[0].network != test.network {
				t.Fatalf("tcpListenTargets(%q) = %#v, want one %s target", host, targets, test.network)
			}
		})
	}
}

func TestListenTCPRejectsUnbracketedIPv6Address(t *testing.T) {
	_, err := listenTCP(context.Background(), "::1")
	if err == nil || !strings.Contains(err.Error(), "too many colons") {
		t.Fatalf("listenTCP() error = %v, want bracketed IPv6 error", err)
	}
}

func TestCloseListenersPreservesEveryError(t *testing.T) {
	firstErr := errors.New("first close failed")
	secondErr := errors.New("second close failed")
	err := closeListeners([]net.Listener{
		closeErrorListener{err: firstErr},
		closeErrorListener{err: secondErr},
	})
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("closeListeners() error = %v, want both close errors", err)
	}
}

type closeErrorListener struct {
	net.Listener
	err error
}

func (listener closeErrorListener) Close() error {
	return listener.err
}
