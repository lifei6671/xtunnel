package route

import (
	"errors"
	"testing"

	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	testTunnelID  = "tun_01J00000000000000000000000"
	testServiceID = "svc_01J00000000000000000000000"
)

func TestBuildSnapshotBuildsCompleteJoinedViewAndProtectsImmutability(t *testing.T) {
	state := validDesiredState(7)
	state.HTTPRoutes = append(state.HTTPRoutes, repository.HTTPRoute{
		ID: "http-admin", ServiceID: testServiceID, Hostname: "app.example.com",
		PathPrefix: "/admin", PreserveHost: false, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
	})

	snapshot, err := buildSnapshot(state)
	if err != nil {
		t.Fatalf("buildSnapshot() error = %v", err)
	}
	if got := snapshot.Generation(); got != 7 {
		t.Fatalf("Generation() = %d, want 7", got)
	}

	hostRoutes, ok := snapshot.HTTP("app.example.com")
	if !ok {
		t.Fatal("HTTP(app.example.com) not found")
	}
	routes := hostRoutes.Routes()
	if len(routes) != 2 {
		t.Fatalf("len(Routes()) = %d, want 2", len(routes))
	}
	if routes[0].ID != "http-admin" || routes[1].ID != "http-main" {
		t.Fatalf("Routes() IDs = [%q %q], want stable [http-admin http-main]", routes[0].ID, routes[1].ID)
	}
	if routes[1].TunnelID != testTunnelID || routes[1].RequiredRevision != 3 || !routes[1].PreserveHost ||
		routes[1].OriginScheme != repository.OriginSchemeHTTP || routes[1].OriginHost != "127.0.0.1" ||
		routes[1].OriginPort != 8080 ||
		routes[1].OriginHTTPHost != "origin.example" || routes[1].ProxyOptions.IdleConnectionTimeoutMS != 90_000 ||
		routes[1].ProxyOptions.MaxIdleConnections != 100 || routes[1].ProxyOptions.DisableChunkedEncoding {
		t.Fatalf("joined HTTP route = %+v, want tunnel=%q revision=3 preserve_host=true", routes[1], testTunnelID)
	}

	tcpRoute, ok := snapshot.TCP(8443)
	if !ok {
		t.Fatal("TCP(8443) not found")
	}
	if tcpRoute.ServiceID != testServiceID || tcpRoute.TunnelID != testTunnelID || tcpRoute.RequiredRevision != 3 {
		t.Fatalf("TCP(8443) = %+v, want joined service/tunnel/revision", tcpRoute)
	}
	tcpRoutes := snapshot.TCPRoutes()
	if len(tcpRoutes) != 1 || tcpRoutes[0] != tcpRoute {
		t.Fatalf("TCPRoutes() = %+v, want [%+v]", tcpRoutes, tcpRoute)
	}
	tcpRoutes[0].ServiceID = "mutated"
	if againTCP, _ := snapshot.TCP(8443); againTCP.ServiceID != testServiceID {
		t.Fatalf("TCPRoutes() result mutated snapshot: %+v", againTCP)
	}
	tunnel, ok := snapshot.Tunnel(testTunnelID)
	if !ok || tunnel.DesiredRevision != 4 {
		t.Fatalf("Tunnel(%q) = %+v, %v, want desired_revision=4", testTunnelID, tunnel, ok)
	}

	// 修改调用方得到的副本和最初的 Source 输入，均不能反向污染已发布候选。
	routes[0].PathPrefix = "/mutated"
	state.HTTPRoutes[0].PathPrefix = "/source-mutated"
	again, _ := snapshot.HTTP("app.example.com")
	if got := again.Routes()[0].PathPrefix; got != "/admin" {
		t.Fatalf("immutable HTTP route path = %q, want /admin", got)
	}
}

func TestBuildSnapshotRejectsIncompleteState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*repository.RouteDesiredState)
	}{
		{
			name: "Service 引用未知 Tunnel",
			mutate: func(state *repository.RouteDesiredState) {
				state.Services[0].TunnelID = "tun_01J00000000000000000000001"
			},
		},
		{
			name: "HTTP Route 引用未知 Service",
			mutate: func(state *repository.RouteDesiredState) {
				state.HTTPRoutes[0].ServiceID = "svc_01J00000000000000000000001"
			},
		},
		{
			name: "重复 HTTP 匹配键",
			mutate: func(state *repository.RouteDesiredState) {
				duplicate := state.HTTPRoutes[0]
				duplicate.ID = "http-duplicate"
				state.HTTPRoutes = append(state.HTTPRoutes, duplicate)
			},
		},
		{
			name: "相同 HTTP ID 使用不同匹配键",
			mutate: func(state *repository.RouteDesiredState) {
				duplicate := state.HTTPRoutes[0]
				duplicate.Hostname = "other.example.com"
				state.HTTPRoutes = append(state.HTTPRoutes, duplicate)
			},
		},
		{
			name: "重复 TCP 公开端口",
			mutate: func(state *repository.RouteDesiredState) {
				duplicate := state.TCPRoutes[0]
				duplicate.ID = "tcp-duplicate"
				state.TCPRoutes = append(state.TCPRoutes, duplicate)
			},
		},
		{
			name: "相同 TCP ID 使用不同公开端口",
			mutate: func(state *repository.RouteDesiredState) {
				duplicate := state.TCPRoutes[0]
				duplicate.PublicPort = 9443
				state.TCPRoutes = append(state.TCPRoutes, duplicate)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := validDesiredState(1)
			test.mutate(&state)
			_, err := buildSnapshot(state)
			if !errors.Is(err, ErrInvalidDesiredState) {
				t.Fatalf("buildSnapshot() error = %v, want ErrInvalidDesiredState", err)
			}
		})
	}
}

func TestBuildSnapshotCopiesHTTPProxyPolicyByValue(t *testing.T) {
	state := validDesiredState(8)
	state.Services[0].ProxyOptions = repository.ServiceProxyOptions{
		DisableChunkedEncoding:      true,
		DisableHappyEyeballs:        true,
		HTTPIdleConnectionTimeoutMS: 45_000,
		HTTPMaxIdleConnections:      8,
		TCPKeepAliveIntervalMS:      0,
	}

	snapshot, err := buildSnapshot(state)
	if err != nil {
		t.Fatalf("buildSnapshot() error = %v", err)
	}
	hostRoutes, ok := snapshot.HTTP("app.example.com")
	if !ok {
		t.Fatal("HTTP(app.example.com) not found")
	}
	routes := hostRoutes.Routes()
	if len(routes) != 1 || routes[0].OriginHTTPHost != "origin.example" ||
		!routes[0].ProxyOptions.DisableChunkedEncoding ||
		routes[0].ProxyOptions.IdleConnectionTimeoutMS != 45_000 ||
		routes[0].ProxyOptions.MaxIdleConnections != 8 {
		t.Fatalf("HTTP proxy policy = %+v", routes)
	}

	state.Services[0].OriginHost = "127.0.0.2"
	state.Services[0].OriginPort = 9090
	state.Services[0].OriginHTTPHost = "mutated.example"
	state.Services[0].ProxyOptions.HTTPMaxIdleConnections = 99
	again, _ := snapshot.HTTP("app.example.com")
	got := again.Routes()[0]
	if got.OriginHost != "127.0.0.1" || got.OriginPort != 8080 ||
		got.OriginHTTPHost != "origin.example" || got.ProxyOptions.MaxIdleConnections != 8 {
		t.Fatalf("published HTTP proxy policy changed with source mutation: %+v", got)
	}
}

func TestBuildSnapshotOmitsDisabledOrRevokedRoutesWithoutDroppingAssociationData(t *testing.T) {
	state := validDesiredState(2)
	state.HTTPRoutes[0].Enabled = false
	state.Services[0].Enabled = false

	snapshot, err := buildSnapshot(state)
	if err != nil {
		t.Fatalf("buildSnapshot() error = %v", err)
	}
	if _, ok := snapshot.HTTP("app.example.com"); ok {
		t.Fatal("disabled HTTP route unexpectedly published")
	}
	if _, ok := snapshot.TCP(8443); ok {
		t.Fatal("route to disabled Service unexpectedly published")
	}
	if _, ok := snapshot.Tunnel(testTunnelID); !ok {
		t.Fatal("Tunnel association data was dropped with disabled routes")
	}

	state = validDesiredState(3)
	revokedAt := int64(2)
	state.Tunnels[0].RevokedAt = &revokedAt
	snapshot, err = buildSnapshot(state)
	if err != nil {
		t.Fatalf("buildSnapshot(revoked) error = %v", err)
	}
	if _, ok := snapshot.HTTP("app.example.com"); ok {
		t.Fatal("route to revoked Tunnel unexpectedly published")
	}
	if _, ok := snapshot.TCP(8443); ok {
		t.Fatal("TCP route to revoked Tunnel unexpectedly published")
	}
}

func validDesiredState(generation uint64) repository.RouteDesiredState {
	return repository.RouteDesiredState{
		Generation: generation,
		Tunnels: []repository.Tunnel{{
			ID: testTunnelID, Name: "office", Version: 1, DesiredRevision: 4,
			CreatedAt: 1, UpdatedAt: 1,
		}},
		Services: []repository.Service{{
			ID: testServiceID, TunnelID: testTunnelID, Name: "web", RequiredRevision: 3,
			OriginScheme: repository.OriginSchemeHTTP, OriginHost: "127.0.0.1", OriginPort: 8080,
			OriginHTTPHost: "origin.example", ConnectTimeoutMS: 5_000, Enabled: true,
			Version: 1, CreatedAt: 1, UpdatedAt: 1,
		}},
		HTTPRoutes: []repository.HTTPRoute{{
			ID: "http-main", ServiceID: testServiceID, Hostname: "app.example.com",
			PathPrefix: "/", PreserveHost: true, Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		}},
		TCPRoutes: []repository.TCPRoute{{
			ID: "tcp-main", ServiceID: testServiceID, PublicPort: 8443,
			Enabled: true, CreatedAt: 1, UpdatedAt: 1,
		}},
	}
}
