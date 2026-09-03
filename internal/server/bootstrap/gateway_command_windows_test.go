//go:build windows

package bootstrap

import (
	"testing"

	"github.com/lifei6671/xtunnel/internal/server/pathprofile"
)

// The rotation command must resolve the profile before datadir.Resolve: the
// Windows config sentinel "auto" is not itself a filesystem path.
func TestGatewayRotateKeyUsesForegroundProfileForAutomaticDataDirectory(t *testing.T) {
	want, err := pathprofile.Resolve(pathprofile.AutomaticDataDir)
	if err != nil {
		t.Fatalf("pathprofile.Resolve(auto) error = %v", err)
	}
	got, err := gatewayRotationDataDirectory(pathprofile.AutomaticDataDir)
	if err != nil {
		t.Fatalf("gatewayRotationDataDirectory(auto) error = %v", err)
	}
	if got != want.DataDir || got == pathprofile.AutomaticDataDir {
		t.Fatalf("gatewayRotationDataDirectory(auto) = %q, want %q", got, want.DataDir)
	}
}
