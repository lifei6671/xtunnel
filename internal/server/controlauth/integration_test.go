package controlauth_test

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	agentcontrolauth "github.com/lifei6671/xtunnel/internal/agent/controlauth"
	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/identity"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
	repositorysqlite "github.com/lifei6671/xtunnel/internal/repository/sqlite"
	servercontrolauth "github.com/lifei6671/xtunnel/internal/server/controlauth"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

const integrationTunnelID = "tun_01J00000000000000000000000"

// TestConnectorAuthenticationWithPersistentTunnelToken 穿过真实 SQLite Repository、
// AES-GCM Token 密文、Token Verify、Server AUTH 与 Agent AUTH，证明同一 Tunnel 当前
// Token 可以被三个独立 Connector 复用，而不会为新增 Connector 签发新 Credential。
func TestConnectorAuthenticationWithPersistentTunnelToken(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := repositorysqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	storeClosed := false
	t.Cleanup(func() {
		if !storeClosed {
			_ = store.Close()
		}
	})
	if err := store.WithTx(ctx, func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(ctx, repository.Tunnel{
			ID: integrationTunnelID, Name: "integration", Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err != nil {
		t.Fatalf("create Tunnel error = %v", err)
	}

	protector, err := application.NewAES256GCMTokenProtector(bytes.Repeat([]byte{0x6a}, 32))
	if err != nil {
		t.Fatalf("NewAES256GCMTokenProtector() error = %v", err)
	}
	tokenService := application.NewConnectionTokenService(store, protector)
	issued, err := tokenService.Issue(ctx, application.IssueConnectionTokenInput{
		TunnelID: integrationTunnelID,
		Endpoint: &protocolv1.GatewayEndpoint{Host: "gateway.example.test", Port: 7443},
		TLSTrust: &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{
			PublicCa: &protocolv1.PublicCATrust{},
		}},
	})
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	current, err := tokenService.Current(ctx, integrationTunnelID)
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current.Token != issued.Token {
		t.Fatal("Current() changed the Tunnel Token before adding another Connector")
	}

	registry := serverruntime.NewRegistry()
	serverHandler, err := servercontrolauth.New(tokenService, registry, servercontrolauth.Options{
		AuthenticationRecorder:    store,
		TunnelAdmissionController: integrationAdmission{},
		ReadTimeout:               2 * time.Second, WriteTimeout: 2 * time.Second,
		HeartbeatInterval: 10 * time.Second, RetryAfter: time.Second,
	})
	if err != nil {
		t.Fatalf("controlauth.New() error = %v", err)
	}

	const connectorCount = 3
	connectorIDs := make(map[string]struct{}, connectorCount)
	sessionIDs := make(map[string]struct{}, connectorCount)
	for index := range connectorCount {
		connector, err := identity.NewConnector()
		if err != nil {
			t.Fatalf("NewConnector(%d) error = %v", index, err)
		}
		serverSession, agentSession := authenticatePair(t, serverHandler, current.Token, connector)
		if serverSession.Session.TunnelID != integrationTunnelID || serverSession.Session.ConnectorID != connector.ID() {
			t.Fatalf("authenticated Session(%d) = %#v, want Tunnel %q Connector %q", index, serverSession.Session, integrationTunnelID, connector.ID())
		}
		if agentSession.SessionID != serverSession.Session.SessionID ||
			!bytes.Equal(agentSession.SessionSecret[:], serverSession.SessionSecret[:]) {
			t.Fatalf("Agent and Server Session(%d) identity or Secret mismatch", index)
		}
		if _, exists := connectorIDs[serverSession.Session.ConnectorID]; exists {
			t.Fatalf("Connector ID %q was reused", serverSession.Session.ConnectorID)
		}
		connectorIDs[serverSession.Session.ConnectorID] = struct{}{}
		if _, exists := sessionIDs[serverSession.Session.SessionID]; exists {
			t.Fatalf("Session ID %q was reused", serverSession.Session.SessionID)
		}
		sessionIDs[serverSession.Session.SessionID] = struct{}{}
		if currentSession, exists := registry.Current(integrationTunnelID, connector.ID()); !exists || currentSession != serverSession.Session {
			t.Fatalf("Current(%d) = %#v, %v, want %#v", index, currentSession, exists, serverSession.Session)
		}
		clear(agentSession.SessionSecret[:])
		clear(serverSession.SessionSecret[:])
	}
	afterConnectors, err := tokenService.Current(ctx, integrationTunnelID)
	if err != nil {
		t.Fatalf("Current(after Connectors) error = %v", err)
	}
	if afterConnectors.Token != issued.Token || afterConnectors.TokenID != issued.TokenID ||
		afterConnectors.TokenVersion != issued.TokenVersion {
		t.Fatalf("Current(after Connectors) changed Credential identity: got=%s/v%d want=%s/v%d",
			afterConnectors.TokenID, afterConnectors.TokenVersion, issued.TokenID, issued.TokenVersion)
	}
	var persistedFirstAuthenticatedAt *int64
	if err := store.Read(ctx, func(view repository.RepositoryView) error {
		tunnel, readErr := view.Tunnels().Get(ctx, integrationTunnelID)
		if readErr != nil {
			return readErr
		}
		persistedFirstAuthenticatedAt = tunnel.FirstAuthenticatedAt
		return nil
	}); err != nil {
		t.Fatalf("read authenticated Tunnel error = %v", err)
	}
	if persistedFirstAuthenticatedAt == nil {
		t.Fatal("successful authentication did not persist first_authenticated_at")
	}
	firstAuthenticatedAt := *persistedFirstAuthenticatedAt
	if err := store.Close(); err != nil {
		t.Fatalf("close SQLite before restart error = %v", err)
	}
	storeClosed = true

	reopened, err := repositorysqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open(after restart) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var reopenedFirstAuthenticatedAt *int64
	if err := reopened.Read(ctx, func(view repository.RepositoryView) error {
		tunnel, readErr := view.Tunnels().Get(ctx, integrationTunnelID)
		if readErr != nil {
			return readErr
		}
		reopenedFirstAuthenticatedAt = tunnel.FirstAuthenticatedAt
		return nil
	}); err != nil {
		t.Fatalf("read authenticated Tunnel after restart error = %v", err)
	}
	if reopenedFirstAuthenticatedAt == nil || *reopenedFirstAuthenticatedAt != firstAuthenticatedAt {
		t.Fatalf("FirstAuthenticatedAt after restart = %v, want %d", reopenedFirstAuthenticatedAt, firstAuthenticatedAt)
	}
}

type integrationAdmission struct{}

func (integrationAdmission) BeginTunnelAdmission(string) (func(), error) {
	return func() {}, nil
}

func authenticatePair(
	t *testing.T,
	handler *servercontrolauth.Handler,
	connectionToken string,
	connector identity.Connector,
) (servercontrolauth.Established, *agentcontrolauth.Session) {
	t.Helper()
	serverConnection, agentConnection := net.Pipe()
	serverResults := make(chan struct {
		established servercontrolauth.Established
		err         error
	}, 1)
	go func() {
		established, err := handler.Handle(context.Background(), serverConnection)
		serverResults <- struct {
			established servercontrolauth.Established
			err         error
		}{established: established, err: err}
	}()

	agentSession, agentErr := agentcontrolauth.Authenticate(context.Background(), agentConnection, agentcontrolauth.Config{
		ConnectionToken: connectionToken,
		Connector:       connector,
		Hostname:        "connector.example.test",
		Version:         "v0.1.0",
		OS:              "linux",
		Arch:            "amd64",
		MinProtocol:     1,
		MaxProtocol:     1,
		Capabilities:    []string{"tcp"},
		WriteTimeout:    2 * time.Second,
		ReadTimeout:     2 * time.Second,
	})
	serverResult := <-serverResults
	if agentErr != nil || serverResult.err != nil {
		_ = agentConnection.Close()
		_ = serverConnection.Close()
		t.Fatalf("Authenticate() errors: agent=%v server=%v", agentErr, serverResult.err)
	}
	if err := agentConnection.Close(); err != nil {
		t.Fatalf("close Agent connection error = %v", err)
	}
	if err := serverConnection.Close(); err != nil {
		t.Fatalf("close Server connection error = %v", err)
	}
	return serverResult.established, agentSession
}
