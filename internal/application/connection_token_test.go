package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/token"
	"github.com/lifei6671/xtunnel/internal/repository"
	repositorysqlite "github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

const applicationTestTunnelID = "tun_01J00000000000000000000000"

func TestConnectionTokenIssueCurrentAndVerify(t *testing.T) {
	dataDir := t.TempDir()
	store := openApplicationStore(t, dataDir)
	protector := testTokenProtector(t, 0x51)
	service := NewConnectionTokenService(store, protector)
	seedApplicationTunnel(t, store)

	issued, err := service.Issue(context.Background(), testIssueInput())
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if issued.TunnelID != applicationTestTunnelID || issued.TokenVersion != 1 {
		t.Fatalf("Issue() identity = Tunnel %q version=%d", issued.TunnelID, issued.TokenVersion)
	}
	parsed, err := token.Parse(issued.Token)
	if err != nil {
		t.Fatalf("Parse(issued token) error = %v", err)
	}

	// 两次获取模拟为同一 Tunnel 添加两个 Connector；它们必须得到完全相同的 Token。
	for connectorIndex := 0; connectorIndex < 2; connectorIndex++ {
		current, err := service.Current(context.Background(), applicationTestTunnelID)
		if err != nil {
			t.Fatalf("Current() for connector %d error = %v", connectorIndex, err)
		}
		if current.Token != issued.Token || current.TokenID != issued.TokenID || current.TokenVersion != issued.TokenVersion {
			t.Fatalf("Current() for connector %d changed credential", connectorIndex)
		}
	}
	if _, err := service.Issue(context.Background(), testIssueInput()); !errors.Is(err, ErrConnectionTokenAlreadyActive) {
		t.Fatalf("second Issue() error = %v, want ErrConnectionTokenAlreadyActive", err)
	}
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		_, err := transaction.TunnelTokens().GetByTunnelVersion(context.Background(), applicationTestTunnelID, 2)
		if !errors.Is(err, repository.ErrNotFound) {
			return errors.New("Current or repeated Issue unexpectedly created version 2")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	verified, err := service.Verify(context.Background(), issued.Token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if verified.TunnelID != applicationTestTunnelID || verified.TokenID != issued.TokenID || verified.TokenVersion != 1 || verified.DesiredRevision != 0 {
		t.Fatalf("Verify() = %#v", verified)
	}

	// 关闭 SQLite 让 WAL 收敛，再扫描全部数据库文件，确保明文 Token 与 Secret 未落盘。
	if err := store.Close(); err != nil {
		t.Fatalf("Store.Close() error = %v", err)
	}
	assertSensitiveBytesAbsent(t, dataDir, []byte(issued.Token), parsed.GetAuthenticationSecret())
}

func TestConnectionTokenIssueSerializesConcurrentCreation(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	service := NewConnectionTokenService(store, testTokenProtector(t, 0x62))

	const workers = 8
	var successes atomic.Int32
	var unexpectedMu sync.Mutex
	var unexpected []error
	var wait sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := service.Issue(context.Background(), testIssueInput())
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrConnectionTokenAlreadyActive):
			default:
				unexpectedMu.Lock()
				unexpected = append(unexpected, err)
				unexpectedMu.Unlock()
			}
		}()
	}
	close(start)
	wait.Wait()
	if successes.Load() != 1 || len(unexpected) != 0 {
		t.Fatalf("concurrent Issue() successes=%d unexpected=%v, want 1 and none", successes.Load(), unexpected)
	}
}

func TestConnectionTokenCurrentFailsClosedWithWrongProtector(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	issuer := NewConnectionTokenService(store, testTokenProtector(t, 0x73))
	if _, err := issuer.Issue(context.Background(), testIssueInput()); err != nil {
		t.Fatal(err)
	}

	readerWithWrongKey := NewConnectionTokenService(store, testTokenProtector(t, 0x74))
	if _, err := readerWithWrongKey.Current(context.Background(), applicationTestTunnelID); !errors.Is(err, ErrConnectionTokenUnavailable) {
		t.Fatalf("Current() error = %v, want ErrConnectionTokenUnavailable", err)
	}
}

func TestConnectionTokenRejectsInvalidAndUnknownCredentials(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	seedApplicationTunnel(t, store)
	service := NewConnectionTokenService(store, testTokenProtector(t, 0x85))

	if _, err := service.Current(context.Background(), applicationTestTunnelID); !errors.Is(err, ErrConnectionTokenUnavailable) {
		t.Fatalf("Current() without token error = %v", err)
	}
	if _, err := service.Verify(context.Background(), "xta_invalid"); !errors.Is(err, ErrConnectionTokenInvalid) {
		t.Fatalf("Verify(invalid) error = %v", err)
	}
	invalidInput := testIssueInput()
	invalidInput.TunnelID = "tun_invalid"
	if _, err := service.Issue(context.Background(), invalidInput); !errors.Is(err, ErrConnectionTokenInput) {
		t.Fatalf("Issue(invalid) error = %v", err)
	}
}

func TestConnectionTokenVerifyRejectsRevokedTunnel(t *testing.T) {
	store := openApplicationStore(t, t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	protector := testTokenProtector(t, 0x96)
	service := NewConnectionTokenService(store, protector)

	secret := bytes.Repeat([]byte{0x37}, sha256.Size)
	const tokenID = "tok_01J00000000000000000000000"
	encoded, err := token.Encode(&protocolv1.ConnectionToken{
		FormatVersion:        token.FormatVersionV1,
		Endpoint:             testIssueInput().Endpoint,
		TlsTrust:             testIssueInput().TLSTrust,
		TunnelId:             applicationTestTunnelID,
		TokenId:              tokenID,
		TokenVersion:         1,
		AuthenticationSecret: secret,
	})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	ciphertext, err := protector.Seal([]byte(encoded), TokenProtectionContext{
		TunnelID: applicationTestTunnelID, TokenID: tokenID, Version: 1,
	})
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	revokedAt := int64(2)
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		if err := transaction.Tunnels().Create(context.Background(), repository.Tunnel{
			ID: applicationTestTunnelID, Name: "revoked", Version: 1, RevokedAt: &revokedAt, CreatedAt: 1, UpdatedAt: 2,
		}); err != nil {
			return err
		}
		return transaction.TunnelTokens().Create(context.Background(), repository.TunnelToken{
			ID: tokenID, TunnelID: applicationTestTunnelID, SecretHash: sha256.Sum256(secret),
			TokenCiphertext: ciphertext, Version: 1, Status: repository.TunnelTokenStatusActive, CreatedAt: 1,
		})
	}); err != nil {
		t.Fatalf("seed revoked Tunnel credential error = %v", err)
	}

	if _, err := service.Verify(context.Background(), encoded); !errors.Is(err, ErrConnectionTokenTunnelRevoked) {
		t.Fatalf("Verify() error = %v, want ErrConnectionTokenTunnelRevoked", err)
	}

	forged, err := token.Encode(&protocolv1.ConnectionToken{
		FormatVersion:        token.FormatVersionV1,
		Endpoint:             testIssueInput().Endpoint,
		TlsTrust:             testIssueInput().TLSTrust,
		TunnelId:             applicationTestTunnelID,
		TokenId:              tokenID,
		TokenVersion:         1,
		AuthenticationSecret: bytes.Repeat([]byte{0x38}, sha256.Size),
	})
	if err != nil {
		t.Fatalf("Encode(forged token) error = %v", err)
	}
	if _, err := service.Verify(context.Background(), forged); !errors.Is(err, ErrConnectionTokenSecretMismatch) {
		t.Fatalf("Verify(forged token) error = %v, want ErrConnectionTokenSecretMismatch", err)
	}
}

func openApplicationStore(t *testing.T, dataDir string) *repositorysqlite.Store {
	t.Helper()
	store, err := repositorysqlite.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	return store
}

func seedApplicationTunnel(t *testing.T, store repository.Store) {
	t.Helper()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), repository.Tunnel{
			ID: applicationTestTunnelID, Name: "production", Version: 1, CreatedAt: 1, UpdatedAt: 1,
		})
	}); err != nil {
		t.Fatalf("create test Tunnel error = %v", err)
	}
}

func testTokenProtector(t *testing.T, fill byte) TokenProtector {
	t.Helper()
	protector, err := NewAES256GCMTokenProtector(bytes.Repeat([]byte{fill}, aes256KeyBytes))
	if err != nil {
		t.Fatalf("NewAES256GCMTokenProtector() error = %v", err)
	}
	return protector
}

func testIssueInput() IssueConnectionTokenInput {
	return IssueConnectionTokenInput{
		TunnelID: applicationTestTunnelID,
		Endpoint: &protocolv1.GatewayEndpoint{Host: "gateway.example.com", Port: 7443},
		TLSTrust: &protocolv1.TlsTrustDescriptor{Mode: &protocolv1.TlsTrustDescriptor_PublicCa{
			PublicCa: &protocolv1.PublicCATrust{},
		}},
	}
}

func assertSensitiveBytesAbsent(t *testing.T, dataDir string, values ...[]byte) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dataDir, "xtunnel.db*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read SQLite file %s error = %v", filepath.Base(path), err)
		}
		for _, value := range values {
			if bytes.Contains(data, value) {
				t.Fatalf("SQLite file %s contains sensitive plaintext", filepath.Base(path))
			}
		}
	}
}
