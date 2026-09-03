//go:build !windows

package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadOrCreatePinnedIdentityKeepsSPKIAndPrivateKeyPermissions(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	created, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(create) error = %v", err)
	}
	loaded, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", false, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(load) error = %v", err)
	}
	if created.SPKIHash() != loaded.SPKIHash() {
		t.Fatal("pinned identity SPKI changed during ordinary restart")
	}
	info, err := os.Stat(filepath.Join(dataDir, pkiDirectoryName, keyFileName))
	if err != nil {
		t.Fatalf("os.Stat(private key) error = %v", err)
	}
	if runtime.GOOS == "linux" && info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreatePinnedIdentityRejectsExistingDataWithoutIdentity(t *testing.T) {
	_, err := LoadOrCreatePinnedIdentity(t.TempDir(), "gateway.example.test", false, time.Now())
	if !errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v, want ErrIdentityMissing", err)
	}
}

func TestLoadOrCreatePinnedIdentityRenewsOnlyAtThirtyDayBoundary(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	created, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(create) error = %v", err)
	}
	keyPath := filepath.Join(dataDir, pkiDirectoryName, keyFileName)
	privateKeyBefore, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("os.ReadFile(private key before renewal) error = %v", err)
	}

	// 还剩 31 天时必须继续使用原证书，避免无意义的重复写入。
	notRenewed, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", false, createdAt.Add(366*24*time.Hour))
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(before threshold) error = %v", err)
	}
	if !bytes.Equal(created.Leaf().Raw, notRenewed.Leaf().Raw) {
		t.Fatal("pinned certificate renewed before the 30-day boundary")
	}

	// 剩余有效期恰好 30 天时也必须续签，防止正常启动错过既定窗口。
	renewedAt := createdAt.Add(367 * 24 * time.Hour)
	renewed, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", false, renewedAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(at threshold) error = %v", err)
	}
	if bytes.Equal(created.Leaf().Raw, renewed.Leaf().Raw) {
		t.Fatal("pinned certificate was not renewed at the 30-day boundary")
	}
	if created.SPKIHash() != renewed.SPKIHash() {
		t.Fatal("pinned certificate renewal changed the SPKI")
	}
	privateKeyAfter, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("os.ReadFile(private key after renewal) error = %v", err)
	}
	if !bytes.Equal(privateKeyBefore, privateKeyAfter) {
		t.Fatal("pinned certificate renewal rewrote the private key")
	}
	if want := renewedAt.Add(397 * 24 * time.Hour); !renewed.Leaf().NotAfter.Equal(want) {
		t.Fatalf("renewed certificate NotAfter = %s, want %s", renewed.Leaf().NotAfter, want)
	}
	persisted, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after renewal error = %v", err)
	}
	if !bytes.Equal(persisted.Leaf().Raw, renewed.Leaf().Raw) {
		t.Fatal("renewed certificate was not atomically persisted")
	}

	// Gateway 的 TLS 配置必须直接使用已经热加载到内存的新证书。
	server, err := NewServer(ServerOptions{
		Listen:                  "127.0.0.1:0",
		Identity:                renewed,
		MaxPendingTLSHandshakes: 1,
	})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	certificate, err := server.tlsConfig().GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("TLS GetCertificate() error = %v", err)
	}
	if !bytes.Equal(certificate.Certificate[0], renewed.Leaf().Raw) {
		t.Fatal("Gateway TLS config did not load the renewed certificate")
	}
}

func TestLoadOrCreatePinnedIdentityRenewsExpiredCertificateWithSameSPKI(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	created, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(create) error = %v", err)
	}

	renewedAt := createdAt.Add(398 * 24 * time.Hour)
	renewed, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", false, renewedAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(expired) error = %v", err)
	}
	if bytes.Equal(created.Leaf().Raw, renewed.Leaf().Raw) {
		t.Fatal("expired pinned certificate was not renewed")
	}
	if created.SPKIHash() != renewed.SPKIHash() {
		t.Fatal("expired pinned certificate renewal changed the SPKI")
	}
	if !renewed.Leaf().NotAfter.Equal(renewedAt.Add(397 * 24 * time.Hour)) {
		t.Fatalf("renewed certificate NotAfter = %s, want %s", renewed.Leaf().NotAfter, renewedAt.Add(397*24*time.Hour))
	}
}

func TestLoadOrCreatePinnedIdentityRejectsClockBeforeCertificateNotBefore(t *testing.T) {
	dataDir := t.TempDir()
	createdAt := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	created, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, createdAt)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(create) error = %v", err)
	}

	rolledBackAt := createdAt.Add(-2 * time.Minute)
	_, err = LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", false, rolledBackAt)
	if err == nil {
		t.Fatal("LoadOrCreatePinnedIdentity(clock rollback) error = nil")
	}
	if !strings.Contains(err.Error(), rolledBackAt.Format(time.RFC3339)) ||
		!strings.Contains(err.Error(), created.Leaf().NotBefore.Format(time.RFC3339)) {
		t.Fatalf("LoadOrCreatePinnedIdentity(clock rollback) error = %q, want current time and NotBefore", err)
	}

	persisted, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after clock rollback error = %v", err)
	}
	if !bytes.Equal(persisted.Leaf().Raw, created.Leaf().Raw) {
		t.Fatal("clock rollback changed the persisted pinned certificate")
	}
}

func TestRenewPinnedIdentityPropagatesFailureWithoutChangingExistingIdentity(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	original, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity(create) error = %v", err)
	}

	// 指向不存在的 PKI 目录会使临时文件创建失败；该失败必须明确返回，且不能改写原身份。
	missingPaths := identityPaths(filepath.Join(t.TempDir(), "missing-data-dir"))
	if _, err := renewPinnedIdentity(missingPaths, "gateway.example.test", original, now.Add(367*24*time.Hour)); err == nil {
		t.Fatal("renewPinnedIdentity() error = nil, want temporary file creation failure")
	}
	loaded, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() after renewal failure error = %v", err)
	}
	if !bytes.Equal(loaded.Leaf().Raw, original.Leaf().Raw) || loaded.SPKIHash() != original.SPKIHash() {
		t.Fatal("renewal failure changed the existing pinned identity")
	}
}

func TestRotatePinnedIdentityKeepsJournalUntilAuditCommit(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	before, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	audit := RotationAuditMetadata{
		EventID: "evt_01K00000000000000000000000", OperationID: "op_01K00000000000000000000000",
		OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
	}
	after, err := RotatePinnedIdentity(dataDir, "gateway.example.test", now.Add(time.Hour), audit)
	if err != nil {
		t.Fatalf("RotatePinnedIdentity() error = %v", err)
	}
	if before.SPKIHash() == after.SPKIHash() {
		t.Fatal("RotatePinnedIdentity() did not change SPKI")
	}
	pending, exists, err := PendingRotationAuditEvent(dataDir)
	if err != nil || !exists {
		t.Fatalf("PendingRotationAuditEvent() = %#v, %t, %v", pending, exists, err)
	}
	if pending.EventID != audit.EventID || pending.OperationID != audit.OperationID ||
		pending.BeforeStateDigest != before.SPKIHash() || pending.AfterStateDigest != after.SPKIHash() {
		t.Fatalf("pending rotation audit = %#v", pending)
	}
	if _, err := RotatePinnedIdentity(dataDir, "gateway.example.test", now.Add(2*time.Hour), audit); !errors.Is(err, ErrRotationAuditPending) {
		t.Fatalf("second RotatePinnedIdentity() error = %v, want ErrRotationAuditPending", err)
	}
	if err := CompleteRotationAudit(dataDir, audit.EventID, audit.OperationID); err != nil {
		t.Fatalf("CompleteRotationAudit() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, pkiDirectoryName, journalFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rotation journal remains after audit commit: %v", err)
	}
}

func TestRotatePinnedIdentityRejectsInvalidAuditBeforeMutation(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	before, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	valid := RotationAuditMetadata{
		EventID: "evt_01K00000000000000000000010", OperationID: "op_01K00000000000000000000010",
		OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
	}
	tests := []struct {
		name     string
		hostname string
		audit    RotationAuditMetadata
	}{
		{name: "invalid event id", hostname: valid.ResourceID, audit: func() RotationAuditMetadata {
			value := valid
			value.EventID = "evt_invalid"
			return value
		}()},
		{name: "invalid operation id", hostname: valid.ResourceID, audit: func() RotationAuditMetadata {
			value := valid
			value.OperationID = "op_invalid"
			return value
		}()},
		{name: "oversize resource", hostname: strings.Repeat("a", 257), audit: func() RotationAuditMetadata {
			value := valid
			value.ResourceID = strings.Repeat("a", 257)
			return value
		}()},
		{name: "resource whitespace", hostname: " gateway.example.test", audit: func() RotationAuditMetadata {
			value := valid
			value.ResourceID = " gateway.example.test"
			return value
		}()},
		{name: "resource mismatch", hostname: "other.example.test", audit: valid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RotatePinnedIdentity(dataDir, test.hostname, now.Add(time.Hour), test.audit); err == nil {
				t.Fatal("RotatePinnedIdentity() error = nil, want invalid audit metadata")
			}
			after, err := LoadPinnedIdentity(dataDir)
			if err != nil {
				t.Fatalf("LoadPinnedIdentity() error = %v", err)
			}
			if after.SPKIHash() != before.SPKIHash() {
				t.Fatal("invalid audit metadata changed the pinned identity")
			}
			if _, err := os.Stat(filepath.Join(dataDir, pkiDirectoryName, journalFileName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid audit metadata created a rotation journal: %v", err)
			}
		})
	}
}

func TestCompleteRotationAuditReportsUncertainDirectorySyncAfterRemovingJournal(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	if _, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	audit := RotationAuditMetadata{
		EventID: "evt_01K00000000000000000000011", OperationID: "op_01K00000000000000000000011",
		OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
	}
	if _, err := RotatePinnedIdentity(dataDir, audit.ResourceID, now.Add(time.Hour), audit); err != nil {
		t.Fatalf("RotatePinnedIdentity() error = %v", err)
	}
	injected := errors.New("injected directory sync failure")
	err := completeRotationAudit(dataDir, audit.EventID, audit.OperationID, func(string) error { return injected })
	if !errors.Is(err, ErrRotationAuditCleanupUncertain) || !errors.Is(err, injected) {
		t.Fatalf("completeRotationAudit() error = %v, want cleanup uncertainty and injected error", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, pkiDirectoryName, journalFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rotation journal remains after successful unlink: %v", err)
	}
}

func TestRecoverAuditedRotationKeepsEvidenceAfterPartialReplacement(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	before, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	paths := identityPaths(dataDir)
	replacement, err := newSelfSignedCertificate("gateway.example.test", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("newSelfSignedCertificate() error = %v", err)
	}
	if err := writeKeyPair(paths.keyTemp, paths.certTemp, replacement); err != nil {
		t.Fatalf("writeKeyPair() error = %v", err)
	}
	journal := rotationJournal{
		Version: 2, KeyTemporary: paths.keyTemp, CertificateTemporary: paths.certTemp,
		Audit: &rotationAuditJournal{
			EventID: "evt_01K00000000000000000000001", OperationID: "op_01K00000000000000000000001",
			OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
			BeforeStateDigest: before.SPKIHash(),
			AfterStateDigest:  sha256.Sum256(replacement.leaf.RawSubjectPublicKeyInfo),
		},
	}
	if err := writeJournal(paths.journal, journal); err != nil {
		t.Fatalf("writeJournal() error = %v", err)
	}
	if err := os.Rename(paths.keyTemp, paths.key); err != nil {
		t.Fatalf("os.Rename(key) error = %v", err)
	}
	if err := RecoverRotation(dataDir); err != nil {
		t.Fatalf("RecoverRotation() error = %v", err)
	}
	pending, exists, err := PendingRotationAuditEvent(dataDir)
	if err != nil || !exists {
		t.Fatalf("PendingRotationAuditEvent() = %#v, %t, %v", pending, exists, err)
	}
	loaded, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() error = %v", err)
	}
	if loaded.SPKIHash() != pending.AfterStateDigest {
		t.Fatal("recovered identity does not match the pending audit after-state")
	}
}

func TestRecoverRotationCompletesTemporaryIdentity(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	if _, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	paths := identityPaths(dataDir)
	replacement, err := newSelfSignedCertificate("gateway.example.test", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("newSelfSignedCertificate() error = %v", err)
	}
	if err := writeKeyPair(paths.keyTemp, paths.certTemp, replacement); err != nil {
		t.Fatalf("writeKeyPair() error = %v", err)
	}
	journal := rotationJournal{Version: 1, KeyTemporary: paths.keyTemp, CertificateTemporary: paths.certTemp}
	if err := writeJournal(paths.journal, journal); err != nil {
		t.Fatalf("writeJournal() error = %v", err)
	}
	if err := os.Rename(paths.keyTemp, paths.key); err != nil {
		t.Fatalf("os.Rename(key) error = %v", err)
	}
	if err := RecoverRotation(dataDir); err != nil {
		t.Fatalf("RecoverRotation() error = %v", err)
	}
	identity, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity() error = %v", err)
	}
	want := sha256.Sum256(replacement.leaf.RawSubjectPublicKeyInfo)
	if identity.SPKIHash() != want {
		t.Fatal("recovered identity did not use the journal replacement")
	}
}
