//go:build windows

package gateway

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/server/winsecurity"
)

func TestPinnedIdentityUsesProtectedForegroundFiles(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	issuedAt := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	created, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, issuedAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(create) error = %v", err)
	}
	paths := identityPaths(dataDir)
	keyBefore, err := winsecurity.ReadForegroundFile(paths.directory, keyFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(key) error = %v", err)
	}
	certBefore, err := winsecurity.ReadForegroundFile(paths.directory, certFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(cert) error = %v", err)
	}
	loaded, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() error = %v", err)
	}
	if loaded.SPKIHash() != created.SPKIHash() {
		t.Fatal("protected pinned identity changed during ordinary load")
	}

	renewed, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", false, issuedAt.Add(367*24*time.Hour))
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(renew) error = %v", err)
	}
	keyAfter, err := winsecurity.ReadForegroundFile(paths.directory, keyFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(renewed key) error = %v", err)
	}
	certAfter, err := winsecurity.ReadForegroundFile(paths.directory, certFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(renewed cert) error = %v", err)
	}
	if !bytes.Equal(keyBefore, keyAfter) || renewed.SPKIHash() != created.SPKIHash() {
		t.Fatal("certificate renewal changed the protected pinned private key")
	}
	if bytes.Equal(certBefore, certAfter) {
		t.Fatal("certificate renewal did not publish a new protected certificate")
	}
}

func TestLoadOrCreatePinnedIdentityRejectsUnprotectedDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, time.Now()); err == nil {
		t.Fatal("LoadOrCreatePinnedIdentity() error = nil, want unprotected data directory rejection")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "pki")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprotected data directory created PKI: os.Stat() error = %v", err)
	}
}

func TestLoadOrCreatePinnedIdentityRejectsUnprotectedPKIDirectory(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	pkiDir := filepath.Join(dataDir, "pki")
	if err := os.Mkdir(pkiDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(%q) error = %v", pkiDir, err)
	}
	if _, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, time.Now()); err == nil {
		t.Fatal("LoadOrCreatePinnedIdentity() error = nil, want inherited PKI directory rejection")
	}
	if _, err := os.Stat(filepath.Join(pkiDir, keyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprotected PKI directory created private key: os.Stat() error = %v", err)
	}
}

func TestPinnedRotationUsesProtectedJournalAndAuditCleanup(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	before, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	audit := RotationAuditMetadata{
		EventID: "evt_01K00000000000000000000021", OperationID: "op_01K00000000000000000000021",
		OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
	}
	after, err := RotatePinnedIdentity(dataDir, audit.ResourceID, now.Add(time.Hour), audit)
	if err != nil {
		t.Fatalf("RotatePinnedIdentity() error = %v", err)
	}
	if before.SPKIHash() == after.SPKIHash() {
		t.Fatal("RotatePinnedIdentity() did not replace the pinned SPKI")
	}
	paths := identityPaths(dataDir)
	if _, err := winsecurity.ReadForegroundFile(paths.directory, journalFileName); err != nil {
		t.Fatalf("ReadForegroundFile(rotation journal) error = %v", err)
	}
	if err := CompleteRotationAudit(dataDir, audit.EventID, audit.OperationID); err != nil {
		t.Fatalf("CompleteRotationAudit() error = %v", err)
	}
	if _, err := winsecurity.ReadForegroundFile(paths.directory, journalFileName); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadForegroundFile(removed rotation journal) error = %v, want not exist", err)
	}
}

func TestRecoverPinnedRotationCompletesProtectedTemporaryIdentity(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	if _, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	paths := identityPaths(dataDir)
	replacement, err := newSelfSignedCertificate("gateway.example.test", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("newSelfSignedCertificate() error = %v", err)
	}
	fileOps := defaultRotationFileOps()
	if err := writeKeyPairWith(paths.keyTemp, paths.certTemp, replacement, fileOps.writeFileSync); err != nil {
		t.Fatalf("writeKeyPairWith(rotation candidates) error = %v", err)
	}
	journal := rotationJournal{Version: 2, KeyTemporary: paths.keyTemp, CertificateTemporary: paths.certTemp,
		Audit: &rotationAuditJournal{
			EventID: "evt_01K00000000000000000000022", OperationID: "op_01K00000000000000000000022",
			OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
			AfterStateDigest: replacementSPKIHash(t, replacement),
		},
	}
	if err := writeJournal(paths.journal, journal); err != nil {
		t.Fatalf("writeJournal() error = %v", err)
	}
	if err := fileOps.rename(paths.keyTemp, paths.key); err != nil {
		t.Fatalf("replace protected temporary key error = %v", err)
	}
	if err := RecoverRotation(dataDir); err != nil {
		t.Fatalf("RecoverRotation() error = %v", err)
	}
	loaded, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() error = %v", err)
	}
	if loaded.SPKIHash() != replacementSPKIHash(t, replacement) {
		t.Fatal("RecoverRotation() did not complete the protected replacement identity")
	}
}

func replacementSPKIHash(t *testing.T, identity tlsCertificate) [32]byte {
	t.Helper()
	if identity.leaf == nil {
		t.Fatal("replacement identity leaf is nil")
	}
	return sha256.Sum256(identity.leaf.RawSubjectPublicKeyInfo)
}

func newProtectedGatewayDataDir(t *testing.T) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "managed-data")
	security, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(dataDir, security); err != nil {
		t.Fatalf("CreateForegroundDirectory(%q) error = %v", dataDir, err)
	}
	return dataDir
}
