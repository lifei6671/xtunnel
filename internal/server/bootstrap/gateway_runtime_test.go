package bootstrap

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	servertcpingress "github.com/lifei6671/xtunnel/internal/server/tcpingress"
)

func TestNewTCPIngressManagerRequiresSourceLimitManager(t *testing.T) {
	manager, err := newTCPIngressManager(serverconfig.Config{}, nil, nil, nil, nil, nil)
	if manager != nil || err == nil {
		t.Fatalf("newTCPIngressManager() = %#v, %v, want a required limiter error", manager, err)
	}
	if errors.Is(err, servertcpingress.ErrInvalidOptions) {
		t.Fatalf("newTCPIngressManager() error = %v, want failure before listener construction", err)
	}
}

func TestReservedTCPPortsUsesOnlyConcreteIngressPorts(t *testing.T) {
	config := serverconfig.Config{}
	config.Management.Listen = "127.0.0.1:0"
	config.HTTPIngress.Listen = "127.0.0.1:8080"
	config.AgentGateway.Listen = "127.0.0.1:8443"

	ports, err := reservedTCPPorts(config)
	if err != nil {
		t.Fatalf("reservedTCPPorts() error = %v", err)
	}
	slices.Sort(ports)
	if want := []uint16{80, 443, 8080, 8443}; !reflect.DeepEqual(ports, want) {
		t.Fatalf("reservedTCPPorts() = %v, want %v", ports, want)
	}
}

func TestDefaultTCPRangeIsIncludedInServerFDBudget(t *testing.T) {
	config := serverconfig.Config{}
	config.TCPIngress = serverconfig.TCPIngress{MinPort: 10000, MaxPort: 60000}
	config.Limits = serverconfig.Limits{
		MaxWorkConnections: 60000, MaxActiveConnections: 20000,
		MaxPendingOpens: 1024, MaxConnectors: 5000,
		MaxPendingTLSHandshakes: 512, MaxPendingAuth: 512,
	}

	tcpListeners := tcpListenerFDReserve(config.TCPIngress)
	if tcpListeners != 50002 {
		t.Fatalf("TCP listener FD reserve = %d, want 50002", tcpListeners)
	}
	budget, err := serverFDBudget(config, tcpListeners)
	if err != nil {
		t.Fatalf("serverFDBudget() error = %v", err)
	}
	if budget.Listeners != 50006 {
		t.Fatalf("listener FD budget = %d, want 50006", budget.Listeners)
	}
	total := budget.WorkConnections + budget.PublicActiveConnections + budget.PendingOpenConnections +
		budget.ConnectorControls + budget.PendingTLSHandshakes + budget.PendingAuth + budget.Listeners +
		budget.SQLite + budget.Management + budget.Metrics + budget.SafetyMargin
	if total != 137192 {
		t.Fatalf("default Server FD budget = %d, want 137192", total)
	}
}
