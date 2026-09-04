package bootstrap

import (
	"testing"

	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
)

func TestManagementGatewayConnectionDescriptionPublicAddress(t *testing.T) {
	for _, test := range []struct {
		value, host string
		port        uint32
	}{
		{"127.0.0.1", "127.0.0.1", 8443},
		{"127.0.0.1:9443", "127.0.0.1", 9443},
		{"[::1]:9443", "::1", 9443},
		{"gateway.example.test:443", "gateway.example.test", 443},
	} {
		t.Run(test.value, func(t *testing.T) {
			config := serverconfig.Config{AgentGateway: serverconfig.AgentGateway{
				Listen: ":8443", PublicHostname: test.value,
				TLS: serverconfig.AgentGatewayTLS{Mode: gateway.PublicMode},
			}}
			endpoint, trust, err := managementGatewayConnectionDescription(config, gateway.Identity{})
			if err != nil {
				t.Fatal(err)
			}
			if endpoint.GetHost() != test.host || endpoint.GetPort() != test.port || trust.GetPublicCa() == nil {
				t.Fatalf("description = %v, %v; want %s:%d with public CA", endpoint, trust, test.host, test.port)
			}
		})
	}
}
