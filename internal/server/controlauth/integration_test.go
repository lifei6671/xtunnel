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
// Token 可以被两个独立 Connector 复用，而不会为第二个 Connector 签发新 Credential。
func TestConnectorAuthenticationWithPersistentTunnelToken(t *testing.T) {
	ctx := context.Background()
	store, err := repositorysqlite.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
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
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		HeartbeatInterval: 10 * time.Second, RetryAfter: time.Second,
	})
	if err != nil {
		t.Fatalf("controlauth.New() error = %v", err)
	}

	firstConnector, err := identity.NewConnector()
	if err != nil {
		t.Fatalf("NewConnector(first) error = %v", err)
	}
	secondConnector, err := identity.NewConnector()
	if err != nil {
		t.Fatalf("NewConnector(second) error = %v", err)
	}
	firstServer, firstAgent := authenticatePair(t, serverHandler, issued.Token, firstConnector)
	secondServer, secondAgent := authenticatePair(t, serverHandler, current.Token, secondConnector)

	if firstServer.Session.TunnelID != integrationTunnelID || secondServer.Session.TunnelID != integrationTunnelID ||
		firstServer.Session.ConnectorID == secondServer.Session.ConnectorID {
		t.Fatalf("authenticated Sessions = %#v, %#v, want same Tunnel and different Connectors", firstServer.Session, secondServer.Session)
	}
	if firstAgent.SessionID != firstServer.Session.SessionID || secondAgent.SessionID != secondServer.Session.SessionID ||
		!bytes.Equal(firstAgent.SessionSecret[:], firstServer.SessionSecret[:]) ||
		!bytes.Equal(secondAgent.SessionSecret[:], secondServer.SessionSecret[:]) {
		t.Fatal("Agent and Server did not commit identical Session identities and Secrets")
	}
	if _, exists := registry.Current(integrationTunnelID, firstConnector.ID()); !exists {
		t.Fatal("first Connector is absent from the Tunnel runtime Registry")
	}
	if _, exists := registry.Current(integrationTunnelID, secondConnector.ID()); !exists {
		t.Fatal("second Connector is absent from the Tunnel runtime Registry")
	}
	clear(firstAgent.SessionSecret[:])
	clear(secondAgent.SessionSecret[:])
	clear(firstServer.SessionSecret[:])
	clear(secondServer.SessionSecret[:])
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
