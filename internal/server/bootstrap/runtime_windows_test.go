//go:build windows

package bootstrap

import (
	"path/filepath"
	"testing"

	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
	"github.com/lifei6671/xtunnel/internal/server/pathprofile"
	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

func TestRuntimeDirectoryForOptionsUsesForegroundProfile(t *testing.T) {
	profile, err := pathprofile.Resolve(pathprofile.AutomaticDataDir)
	if err != nil {
		t.Fatalf("pathprofile.Resolve(auto) error = %v", err)
	}
	runtimeDir, err := runtimeDirectoryForOptions(baseconfig.Options{YAML: []byte(`
server:
  data_dir: auto
management:
  public_url: https://admin.example.com
agent_gateway:
  public_hostname: tunnel.example.com
`)})
	if err != nil {
		t.Fatalf("runtimeDirectoryForOptions() error = %v", err)
	}
	if runtimeDir != profile.RuntimeDir {
		t.Fatalf("runtimeDirectoryForOptions() = %q, want %q", runtimeDir, profile.RuntimeDir)
	}
}

func TestLoadGatewayIdentityUsesResolvedStorageDataDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "managed-data")
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(dataDir, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(%q) error = %v", dataDir, err)
	}
	_, err = loadGatewayIdentity(serverconfig.Config{
		Server: serverconfig.Server{DataDir: pathprofile.AutomaticDataDir},
		AgentGateway: serverconfig.AgentGateway{
			Listen:         "127.0.0.1:8443",
			PublicHostname: "gateway.example.test",
			TLS:            serverconfig.AgentGatewayTLS{Mode: gateway.PinnedMode},
		},
	}, &serverStorage{dataDir: dataDir})
	if err != nil {
		t.Fatalf("loadGatewayIdentity() error = %v", err)
	}
	if _, err := winsecurity.ReadForegroundFile(filepath.Join(dataDir, "pki"), "agent-gateway.key"); err != nil {
		t.Fatalf("ReadForegroundFile(pinned key) error = %v", err)
	}
}
