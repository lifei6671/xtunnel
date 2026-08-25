package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

type tcpListenTarget struct {
	network string
	host    string
}

// listenTCP 在地址省略 Host 时分别创建 tcp4、tcp6 Socket。
// 两个原生 Socket 不依赖 IPv4-mapped IPv6，因此后续的来源地址、可信代理
// 和限流逻辑不会同时处理 IPv4 与映射后的 IPv6 两种表示。
func listenTCP(ctx context.Context, address string) ([]net.Listener, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("split TCP listen address %q: %w", address, err)
	}

	targets := tcpListenTargets(host)
	listeners := make([]net.Listener, 0, len(targets))
	for _, target := range targets {
		listenPort := port
		if port == "0" && len(listeners) != 0 {
			listenPort = strconv.Itoa(listeners[0].Addr().(*net.TCPAddr).Port)
		}
		listenAddress := net.JoinHostPort(target.host, listenPort)

		var config net.ListenConfig
		listener, listenErr := config.Listen(ctx, target.network, listenAddress)
		if listenErr != nil {
			closeErr := closeListeners(listeners)
			return nil, errors.Join(
				fmt.Errorf("listen on %s %s: %w", target.network, listenAddress, listenErr),
				closeErr,
			)
		}
		listeners = append(listeners, listener)
	}
	return listeners, nil
}

func tcpListenTargets(host string) []tcpListenTarget {
	switch host {
	case "":
		return []tcpListenTarget{
			{network: "tcp4", host: "0.0.0.0"},
			{network: "tcp6", host: "::"},
		}
	}

	address, err := netip.ParseAddr(host)
	if err != nil {
		// 监听主机名时保留标准库语义：解析结果至多选择一个本地地址。
		return []tcpListenTarget{{network: "tcp", host: host}}
	}
	if address.Is4() {
		return []tcpListenTarget{{network: "tcp4", host: host}}
	}
	return []tcpListenTarget{{network: "tcp6", host: host}}
}

func closeListeners(listeners []net.Listener) error {
	errorsToJoin := make([]error, 0, len(listeners))
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			errorsToJoin = append(errorsToJoin, err)
		}
	}
	return errors.Join(errorsToJoin...)
}
