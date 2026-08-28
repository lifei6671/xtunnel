package bootstrap

import (
	"strings"
	"testing"

	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
)

func TestOpenGatewayLifecycleRejectsNilHealthBudgetBeforeInspectingResources(t *testing.T) {
	server, sessions, tunnelProxy, err := openGatewayLifecycle(serverconfig.Config{}, nil, nil, nil, nil)
	if server != nil || sessions != nil || tunnelProxy != nil || err == nil ||
		!strings.Contains(err.Error(), "health target budget manager is required") {
		t.Fatalf(
			"openGatewayLifecycle(nil budget) = (%T, %T, %T, %v), want nil results and budget error",
			server, sessions, tunnelProxy, err,
		)
	}
}
