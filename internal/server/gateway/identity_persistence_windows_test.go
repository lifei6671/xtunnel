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

// TestLoadPinnedIdentityRejectsUnprotectedDataDirectoryWithManagedPKI 确认
// 受管 pki/key/cert 不能脱离受保护 data root 独立被信任。根目录边界失效时，
// 加载必须在 PEM 解析前拒绝，且不得接管、修复或替换任何现有身份对象。
func TestLoadPinnedIdentityRejectsUnprotectedDataDirectoryWithManagedPKI(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "unmanaged-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(unprotected data directory) error = %v", err)
	}
	paths := identityPaths(dataDir)
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(PKI) error = %v", err)
	}
	identity, err := newSelfSignedCertificate("gateway.example.test", time.Date(2026, time.September, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("newSelfSignedCertificate() error = %v", err)
	}
	key, certificate, err := pinnedIdentityPEM(identity)
	if err != nil {
		t.Fatalf("pinnedIdentityPEM() error = %v", err)
	}
	fileSecurity, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	if err := winsecurity.PublishForegroundFile(paths.directory, keyFileName, key, fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(key) error = %v", err)
	}
	if err := winsecurity.PublishForegroundFile(paths.directory, certFileName, certificate, fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(certificate) error = %v", err)
	}
	dataInfoBefore, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("os.Stat(unprotected data directory before rejection) error = %v", err)
	}
	directoryInfoBefore, err := os.Stat(paths.directory)
	if err != nil {
		t.Fatalf("os.Stat(PKI before rejection) error = %v", err)
	}
	keyInfoBefore, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(key before rejection) error = %v", err)
	}
	certificateInfoBefore, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(certificate before rejection) error = %v", err)
	}

	if _, err := LoadPinnedIdentity(dataDir); err == nil {
		t.Fatal("LoadPinnedIdentity() accepted a managed PKI below an unprotected data directory")
	} else if errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("LoadPinnedIdentity() error = %v, want data-directory security rejection", err)
	}
	keyAfter, err := os.ReadFile(paths.key)
	if err != nil {
		t.Fatalf("os.ReadFile(key after rejection) error = %v", err)
	}
	certificateAfter, err := os.ReadFile(paths.cert)
	if err != nil {
		t.Fatalf("os.ReadFile(certificate after rejection) error = %v", err)
	}
	if !bytes.Equal(keyAfter, key) || !bytes.Equal(certificateAfter, certificate) {
		t.Fatal("LoadPinnedIdentity() changed a managed identity file below the unprotected data directory")
	}
	dataInfoAfter, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("os.Stat(unprotected data directory after rejection) error = %v", err)
	}
	directoryInfoAfter, err := os.Stat(paths.directory)
	if err != nil {
		t.Fatalf("os.Stat(PKI after rejection) error = %v", err)
	}
	keyInfoAfter, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(key after rejection) error = %v", err)
	}
	certificateInfoAfter, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(certificate after rejection) error = %v", err)
	}
	if !os.SameFile(dataInfoBefore, dataInfoAfter) || !os.SameFile(directoryInfoBefore, directoryInfoAfter) ||
		!os.SameFile(keyInfoBefore, keyInfoAfter) || !os.SameFile(certificateInfoBefore, certificateInfoAfter) {
		t.Fatal("LoadPinnedIdentity() replaced an object below the unprotected data directory")
	}
}

// TestRotationJournalReadsRejectUnprotectedDataDirectory 确认 Journal 的所有消费
// 路径都不能只信任受保护 pki 与 Journal 文件；根目录失效时不得恢复、读取审计
// 证据或删除 Journal，避免未受保护根目录决定后续身份状态。
func TestRotationJournalReadsRejectUnprotectedDataDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "unmanaged-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(unprotected data directory) error = %v", err)
	}
	paths := identityPaths(dataDir)
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(PKI) error = %v", err)
	}
	audit := RotationAuditMetadata{
		EventID: "evt_01K00000000000000000000024", OperationID: "op_01K00000000000000000000024",
		OccurredAt: time.Date(2026, time.September, 4, 1, 0, 0, 0, time.UTC).Unix(), ResourceID: "gateway.example.test",
	}
	journal := rotationJournal{
		Version: 2, KeyTemporary: paths.keyTemp, CertificateTemporary: paths.certTemp,
		Audit: &rotationAuditJournal{
			EventID: audit.EventID, OperationID: audit.OperationID,
			OccurredAt: audit.OccurredAt, ResourceID: audit.ResourceID,
		},
	}
	if err := writeJournal(paths.journal, journal); err != nil {
		t.Fatalf("writeJournal() error = %v", err)
	}
	journalBefore, err := os.ReadFile(paths.journal)
	if err != nil {
		t.Fatalf("os.ReadFile(rotation journal before rejection) error = %v", err)
	}
	journalInfoBefore, err := os.Stat(paths.journal)
	if err != nil {
		t.Fatalf("os.Stat(rotation journal before rejection) error = %v", err)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "recover", call: func() error { return RecoverRotation(dataDir) }},
		{name: "pending audit event", call: func() error {
			_, _, err := PendingRotationAuditEvent(dataDir)
			return err
		}},
		{name: "complete audit", call: func() error {
			return CompleteRotationAudit(dataDir, audit.EventID, audit.OperationID)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("rotation journal consumer accepted an unprotected data directory")
			}
			journalAfter, err := os.ReadFile(paths.journal)
			if err != nil {
				t.Fatalf("os.ReadFile(rotation journal after rejection) error = %v", err)
			}
			if !bytes.Equal(journalAfter, journalBefore) {
				t.Fatal("rotation journal consumer changed journal bytes after data-directory rejection")
			}
			journalInfoAfter, err := os.Stat(paths.journal)
			if err != nil {
				t.Fatalf("os.Stat(rotation journal after rejection) error = %v", err)
			}
			if !os.SameFile(journalInfoBefore, journalInfoAfter) {
				t.Fatal("rotation journal consumer replaced the journal after data-directory rejection")
			}
		})
	}
}

// TestRenewPinnedIdentityRejectsUnprotectedDataDirectory 确认续签在发布新证书
// 前重新验证 data root。后台周期可能在初始加载之后才执行，不能只依赖早先的
// 身份读取校验；拒绝后原 key/cert 都必须保持原字节和对象身份。
func TestRenewPinnedIdentityRejectsUnprotectedDataDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "unmanaged-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(unprotected data directory) error = %v", err)
	}
	paths := identityPaths(dataDir)
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(PKI) error = %v", err)
	}
	issuedAt := time.Date(2025, time.September, 4, 0, 0, 0, 0, time.UTC)
	certificate, err := newSelfSignedCertificate("gateway.example.test", issuedAt)
	if err != nil {
		t.Fatalf("newSelfSignedCertificate() error = %v", err)
	}
	key, certificatePEM, err := pinnedIdentityPEM(certificate)
	if err != nil {
		t.Fatalf("pinnedIdentityPEM() error = %v", err)
	}
	fileSecurity, err := winsecurity.NewForegroundFileSecurity()
	if err != nil {
		t.Fatalf("NewForegroundFileSecurity() error = %v", err)
	}
	if err := winsecurity.PublishForegroundFile(paths.directory, keyFileName, key, fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(key) error = %v", err)
	}
	if err := winsecurity.PublishForegroundFile(paths.directory, certFileName, certificatePEM, fileSecurity); err != nil {
		t.Fatalf("PublishForegroundFile(certificate) error = %v", err)
	}
	keyBefore, err := os.ReadFile(paths.key)
	if err != nil {
		t.Fatalf("os.ReadFile(key before rejection) error = %v", err)
	}
	certificateBefore, err := os.ReadFile(paths.cert)
	if err != nil {
		t.Fatalf("os.ReadFile(certificate before rejection) error = %v", err)
	}
	keyInfoBefore, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(key before rejection) error = %v", err)
	}
	certificateInfoBefore, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(certificate before rejection) error = %v", err)
	}

	if _, err := renewPinnedIdentity(paths, "gateway.example.test", Identity{Certificate: certificate}, issuedAt.Add(380*24*time.Hour)); err == nil {
		t.Fatal("renewPinnedIdentity() accepted an unprotected data directory")
	}
	keyAfter, err := os.ReadFile(paths.key)
	if err != nil {
		t.Fatalf("os.ReadFile(key after rejection) error = %v", err)
	}
	certificateAfter, err := os.ReadFile(paths.cert)
	if err != nil {
		t.Fatalf("os.ReadFile(certificate after rejection) error = %v", err)
	}
	if !bytes.Equal(keyAfter, keyBefore) || !bytes.Equal(certificateAfter, certificateBefore) {
		t.Fatal("renewPinnedIdentity() changed identity bytes after data-directory rejection")
	}
	keyInfoAfter, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(key after rejection) error = %v", err)
	}
	certificateInfoAfter, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(certificate after rejection) error = %v", err)
	}
	if !os.SameFile(keyInfoBefore, keyInfoAfter) || !os.SameFile(certificateInfoBefore, certificateInfoAfter) {
		t.Fatal("renewPinnedIdentity() replaced an identity object after data-directory rejection")
	}
}

// TestWritePinnedIdentityRejectsUnprotectedDataDirectory 确认首次身份发布在
// 生成并写入 key/cert 前重新验证 data root。即使 pki 已是受管目录，普通根目录
// 也不得被作为长期身份的发布位置，失败后不能创建或替换任何身份文件。
func TestWritePinnedIdentityRejectsUnprotectedDataDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "unmanaged-data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("os.Mkdir(unprotected data directory) error = %v", err)
	}
	paths := identityPaths(dataDir)
	directorySecurity, err := winsecurity.NewForegroundDirectorySecurity()
	if err != nil {
		t.Fatalf("NewForegroundDirectorySecurity() error = %v", err)
	}
	if err := winsecurity.CreateForegroundDirectory(paths.directory, directorySecurity); err != nil {
		t.Fatalf("CreateForegroundDirectory(PKI) error = %v", err)
	}
	certificate, err := newSelfSignedCertificate("gateway.example.test", time.Date(2026, time.September, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("newSelfSignedCertificate() error = %v", err)
	}
	dataInfoBefore, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("os.Stat(data directory before rejection) error = %v", err)
	}
	directoryInfoBefore, err := os.Stat(paths.directory)
	if err != nil {
		t.Fatalf("os.Stat(PKI before rejection) error = %v", err)
	}

	if err := writePinnedIdentity(dataDir, paths.key, paths.cert, certificate); err == nil {
		t.Fatal("writePinnedIdentity() accepted an unprotected data directory")
	}
	for _, path := range []string{paths.key, paths.cert} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("os.Stat(%q) error = %v, want absent identity file", path, err)
		}
	}
	dataInfoAfter, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("os.Stat(data directory after rejection) error = %v", err)
	}
	directoryInfoAfter, err := os.Stat(paths.directory)
	if err != nil {
		t.Fatalf("os.Stat(PKI after rejection) error = %v", err)
	}
	if !os.SameFile(dataInfoBefore, dataInfoAfter) || !os.SameFile(directoryInfoBefore, directoryInfoAfter) {
		t.Fatal("writePinnedIdentity() replaced an existing directory after data-directory rejection")
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

// TestLoadPinnedIdentityRejectsUnprotectedPrivateKey 确认既有受管 PKI 目录中的
// 私钥若被替换成普通文件，读取路径会在解析 PEM 前通过 no-follow/DACL 验证拒绝。
// 失败不得修复、替换或删除该对象，避免把已部署给 Agent 的 SPKI 身份静默改写。
func TestLoadPinnedIdentityRejectsUnprotectedPrivateKey(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	now := time.Date(2026, time.September, 4, 0, 0, 0, 0, time.UTC)
	if _, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	paths := identityPaths(dataDir)
	key, err := winsecurity.ReadForegroundFile(paths.directory, keyFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(key) error = %v", err)
	}
	if err := os.Remove(paths.key); err != nil {
		t.Fatalf("os.Remove(protected key) error = %v", err)
	}
	if err := os.WriteFile(paths.key, key, 0o600); err != nil {
		t.Fatalf("os.WriteFile(unprotected key) error = %v", err)
	}
	before, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(unprotected key before rejection) error = %v", err)
	}

	if _, err := LoadPinnedIdentity(dataDir); err == nil {
		t.Fatal("LoadPinnedIdentity() accepted an unprotected private key")
	} else if errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("LoadPinnedIdentity() error = %v, want security rejection for existing private key", err)
	}
	after, err := os.ReadFile(paths.key)
	if err != nil {
		t.Fatalf("os.ReadFile(unprotected key after rejection) error = %v", err)
	}
	if !bytes.Equal(after, key) {
		t.Fatal("LoadPinnedIdentity() changed the unprotected private key bytes")
	}
	afterInfo, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(unprotected key after rejection) error = %v", err)
	}
	if !os.SameFile(before, afterInfo) {
		t.Fatal("LoadPinnedIdentity() replaced the unprotected private key")
	}
}

// TestLoadPinnedIdentityRejectsUnprotectedCertificate 确认既有受管 PKI 目录中的
// 证书若被替换成普通文件，读取路径会在解析 PEM 前通过 no-follow/DACL 验证拒绝。
// 拒绝后不得修复、替换或删除该对象，避免启动路径把不受信任的证书静默写回受管状态。
func TestLoadPinnedIdentityRejectsUnprotectedCertificate(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	now := time.Date(2026, time.September, 4, 0, 0, 0, 0, time.UTC)
	if _, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	paths := identityPaths(dataDir)
	certificate, err := winsecurity.ReadForegroundFile(paths.directory, certFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(certificate) error = %v", err)
	}
	if err := os.Remove(paths.cert); err != nil {
		t.Fatalf("os.Remove(protected certificate) error = %v", err)
	}
	if err := os.WriteFile(paths.cert, certificate, 0o600); err != nil {
		t.Fatalf("os.WriteFile(unprotected certificate) error = %v", err)
	}
	before, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(unprotected certificate before rejection) error = %v", err)
	}

	if _, err := LoadPinnedIdentity(dataDir); err == nil {
		t.Fatal("LoadPinnedIdentity() accepted an unprotected certificate")
	} else if errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("LoadPinnedIdentity() error = %v, want security rejection for existing certificate", err)
	}
	after, err := os.ReadFile(paths.cert)
	if err != nil {
		t.Fatalf("os.ReadFile(unprotected certificate after rejection) error = %v", err)
	}
	if !bytes.Equal(after, certificate) {
		t.Fatal("LoadPinnedIdentity() changed the unprotected certificate bytes")
	}
	afterInfo, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(unprotected certificate after rejection) error = %v", err)
	}
	if !os.SameFile(before, afterInfo) {
		t.Fatal("LoadPinnedIdentity() replaced the unprotected certificate")
	}
}

// TestLoadPinnedIdentityRejectsReplacedPKIDirectory 确认既有受管 PKI 目录即使
// 携带原有且有效的 PEM 内容，被普通目录整体替换后也必须在读取前拒绝。失败路径
// 不得接管、重建或改写该目录，避免父目录安全边界降级后继续加载长期身份。
func TestLoadPinnedIdentityRejectsReplacedPKIDirectory(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	now := time.Date(2026, time.September, 4, 0, 0, 0, 0, time.UTC)
	if _, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	paths := identityPaths(dataDir)
	key, err := winsecurity.ReadForegroundFile(paths.directory, keyFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(key) error = %v", err)
	}
	certificate, err := winsecurity.ReadForegroundFile(paths.directory, certFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(certificate) error = %v", err)
	}
	if err := os.Remove(paths.key); err != nil {
		t.Fatalf("os.Remove(protected key) error = %v", err)
	}
	if err := os.Remove(paths.cert); err != nil {
		t.Fatalf("os.Remove(protected certificate) error = %v", err)
	}
	if err := os.Remove(paths.directory); err != nil {
		t.Fatalf("os.Remove(protected PKI directory) error = %v", err)
	}
	if err := os.Mkdir(paths.directory, 0o700); err != nil {
		t.Fatalf("os.Mkdir(unprotected PKI directory) error = %v", err)
	}
	if err := os.WriteFile(paths.key, key, 0o600); err != nil {
		t.Fatalf("os.WriteFile(unprotected key) error = %v", err)
	}
	if err := os.WriteFile(paths.cert, certificate, 0o600); err != nil {
		t.Fatalf("os.WriteFile(unprotected certificate) error = %v", err)
	}
	before, err := os.Stat(paths.directory)
	if err != nil {
		t.Fatalf("os.Stat(unprotected PKI directory before rejection) error = %v", err)
	}

	if _, err := LoadPinnedIdentity(dataDir); err == nil {
		t.Fatal("LoadPinnedIdentity() accepted a replaced PKI directory")
	} else if errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("LoadPinnedIdentity() error = %v, want security rejection for existing PKI directory", err)
	}
	keyAfter, err := os.ReadFile(paths.key)
	if err != nil {
		t.Fatalf("os.ReadFile(unprotected key after rejection) error = %v", err)
	}
	certificateAfter, err := os.ReadFile(paths.cert)
	if err != nil {
		t.Fatalf("os.ReadFile(unprotected certificate after rejection) error = %v", err)
	}
	if !bytes.Equal(keyAfter, key) || !bytes.Equal(certificateAfter, certificate) {
		t.Fatal("LoadPinnedIdentity() changed the contents of the replaced PKI directory")
	}
	after, err := os.Stat(paths.directory)
	if err != nil {
		t.Fatalf("os.Stat(unprotected PKI directory after rejection) error = %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("LoadPinnedIdentity() replaced the unprotected PKI directory")
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

// TestRecoverPinnedRotationRejectsUnprotectedTemporaryPrivateKey 确认已发布
// Journal 的恢复路径仍会验证临时私钥。临时文件被普通文件替换时，恢复不得开始
// 正式文件替换，也不得清理 Journal 或接管该临时对象。
func TestRecoverPinnedRotationRejectsUnprotectedTemporaryPrivateKey(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	now := time.Date(2026, time.September, 4, 0, 0, 0, 0, time.UTC)
	before, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	paths := identityPaths(dataDir)
	keyBefore, err := winsecurity.ReadForegroundFile(paths.directory, keyFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(original key) error = %v", err)
	}
	certificateBefore, err := winsecurity.ReadForegroundFile(paths.directory, certFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(original certificate) error = %v", err)
	}
	keyInfoBefore, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(original key before rejection) error = %v", err)
	}
	certificateInfoBefore, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(original certificate before rejection) error = %v", err)
	}
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
			EventID: "evt_01K00000000000000000000023", OperationID: "op_01K00000000000000000000023",
			OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
			BeforeStateDigest: before.SPKIHash(), AfterStateDigest: replacementSPKIHash(t, replacement),
		},
	}
	if err := writeJournal(paths.journal, journal); err != nil {
		t.Fatalf("writeJournal() error = %v", err)
	}
	journalBefore, err := winsecurity.ReadForegroundFile(paths.directory, journalFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(rotation journal) error = %v", err)
	}
	journalInfoBefore, err := os.Stat(paths.journal)
	if err != nil {
		t.Fatalf("os.Stat(rotation journal before rejection) error = %v", err)
	}
	temporaryKey, err := winsecurity.ReadForegroundFile(paths.directory, keyTempFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(temporary key) error = %v", err)
	}
	if err := os.Remove(paths.keyTemp); err != nil {
		t.Fatalf("os.Remove(protected temporary key) error = %v", err)
	}
	if err := os.WriteFile(paths.keyTemp, temporaryKey, 0o600); err != nil {
		t.Fatalf("os.WriteFile(unprotected temporary key) error = %v", err)
	}
	temporaryBefore, err := os.Stat(paths.keyTemp)
	if err != nil {
		t.Fatalf("os.Stat(unprotected temporary key before rejection) error = %v", err)
	}

	if err := RecoverRotation(dataDir); err == nil {
		t.Fatal("RecoverRotation() accepted an unprotected temporary private key")
	}
	temporaryAfter, err := os.ReadFile(paths.keyTemp)
	if err != nil {
		t.Fatalf("os.ReadFile(unprotected temporary key after rejection) error = %v", err)
	}
	if !bytes.Equal(temporaryAfter, temporaryKey) {
		t.Fatal("RecoverRotation() changed the unprotected temporary private key")
	}
	temporaryAfterInfo, err := os.Stat(paths.keyTemp)
	if err != nil {
		t.Fatalf("os.Stat(unprotected temporary key after rejection) error = %v", err)
	}
	if !os.SameFile(temporaryBefore, temporaryAfterInfo) {
		t.Fatal("RecoverRotation() replaced the unprotected temporary private key")
	}
	loaded, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity(original identity) error = %v", err)
	}
	if loaded.SPKIHash() != before.SPKIHash() {
		t.Fatal("RecoverRotation() changed the pinned identity before rejecting the temporary key")
	}
	keyAfter, err := winsecurity.ReadForegroundFile(paths.directory, keyFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(original key after rejection) error = %v", err)
	}
	certificateAfter, err := winsecurity.ReadForegroundFile(paths.directory, certFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(original certificate after rejection) error = %v", err)
	}
	if !bytes.Equal(keyAfter, keyBefore) || !bytes.Equal(certificateAfter, certificateBefore) {
		t.Fatal("RecoverRotation() changed the original pinned identity files")
	}
	keyInfoAfter, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(original key after rejection) error = %v", err)
	}
	certificateInfoAfter, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(original certificate after rejection) error = %v", err)
	}
	if !os.SameFile(keyInfoBefore, keyInfoAfter) || !os.SameFile(certificateInfoBefore, certificateInfoAfter) {
		t.Fatal("RecoverRotation() replaced the original pinned identity files")
	}
	journalAfter, err := winsecurity.ReadForegroundFile(paths.directory, journalFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(rotation journal after rejection) error = %v", err)
	}
	if !bytes.Equal(journalAfter, journalBefore) {
		t.Fatal("RecoverRotation() changed the rotation journal")
	}
	journalInfoAfter, err := os.Stat(paths.journal)
	if err != nil {
		t.Fatalf("os.Stat(rotation journal after rejection) error = %v", err)
	}
	if !os.SameFile(journalInfoBefore, journalInfoAfter) {
		t.Fatal("RecoverRotation() replaced the rotation journal")
	}
}

// TestRecoverPinnedRotationRejectsUnprotectedTemporaryCertificateBeforeReplacement
// 确认恢复会先验证整组临时对象。临时证书被普通文件替换时，正式私钥和证书都不得
// 先行替换，Journal 与所有临时对象必须保留，供受管修复后重新执行恢复。
func TestRecoverPinnedRotationRejectsUnprotectedTemporaryCertificateBeforeReplacement(t *testing.T) {
	dataDir := newProtectedGatewayDataDir(t)
	now := time.Date(2026, time.September, 4, 0, 0, 0, 0, time.UTC)
	before, err := LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	paths := identityPaths(dataDir)
	keyBefore, err := winsecurity.ReadForegroundFile(paths.directory, keyFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(original key) error = %v", err)
	}
	certificateBefore, err := winsecurity.ReadForegroundFile(paths.directory, certFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(original certificate) error = %v", err)
	}
	keyInfoBefore, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(original key before rejection) error = %v", err)
	}
	certificateInfoBefore, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(original certificate before rejection) error = %v", err)
	}
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
			EventID: "evt_01K00000000000000000000024", OperationID: "op_01K00000000000000000000024",
			OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
			BeforeStateDigest: before.SPKIHash(), AfterStateDigest: replacementSPKIHash(t, replacement),
		},
	}
	if err := writeJournal(paths.journal, journal); err != nil {
		t.Fatalf("writeJournal() error = %v", err)
	}
	journalBefore, err := winsecurity.ReadForegroundFile(paths.directory, journalFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(rotation journal) error = %v", err)
	}
	journalInfoBefore, err := os.Stat(paths.journal)
	if err != nil {
		t.Fatalf("os.Stat(rotation journal before rejection) error = %v", err)
	}
	temporaryKey, err := winsecurity.ReadForegroundFile(paths.directory, keyTempFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(temporary key) error = %v", err)
	}
	temporaryKeyInfoBefore, err := os.Stat(paths.keyTemp)
	if err != nil {
		t.Fatalf("os.Stat(temporary key before rejection) error = %v", err)
	}
	temporaryCertificate, err := winsecurity.ReadForegroundFile(paths.directory, certTempFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(temporary certificate) error = %v", err)
	}
	if err := os.Remove(paths.certTemp); err != nil {
		t.Fatalf("os.Remove(protected temporary certificate) error = %v", err)
	}
	if err := os.WriteFile(paths.certTemp, temporaryCertificate, 0o600); err != nil {
		t.Fatalf("os.WriteFile(unprotected temporary certificate) error = %v", err)
	}
	temporaryInfoBefore, err := os.Stat(paths.certTemp)
	if err != nil {
		t.Fatalf("os.Stat(unprotected temporary certificate before rejection) error = %v", err)
	}

	if err := RecoverRotation(dataDir); err == nil {
		t.Fatal("RecoverRotation() accepted an unprotected temporary certificate")
	}
	loaded, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		t.Fatalf("LoadPinnedIdentity(original identity) error = %v", err)
	}
	if loaded.SPKIHash() != before.SPKIHash() {
		t.Fatal("RecoverRotation() replaced the private key before rejecting the temporary certificate")
	}
	keyAfter, err := winsecurity.ReadForegroundFile(paths.directory, keyFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(original key after rejection) error = %v", err)
	}
	certificateAfter, err := winsecurity.ReadForegroundFile(paths.directory, certFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(original certificate after rejection) error = %v", err)
	}
	if !bytes.Equal(keyAfter, keyBefore) || !bytes.Equal(certificateAfter, certificateBefore) {
		t.Fatal("RecoverRotation() changed the original pinned identity files")
	}
	keyInfoAfter, err := os.Stat(paths.key)
	if err != nil {
		t.Fatalf("os.Stat(original key after rejection) error = %v", err)
	}
	certificateInfoAfter, err := os.Stat(paths.cert)
	if err != nil {
		t.Fatalf("os.Stat(original certificate after rejection) error = %v", err)
	}
	if !os.SameFile(keyInfoBefore, keyInfoAfter) || !os.SameFile(certificateInfoBefore, certificateInfoAfter) {
		t.Fatal("RecoverRotation() replaced the original pinned identity files")
	}
	journalAfter, err := winsecurity.ReadForegroundFile(paths.directory, journalFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(rotation journal after rejection) error = %v", err)
	}
	if !bytes.Equal(journalAfter, journalBefore) {
		t.Fatal("RecoverRotation() changed the rotation journal")
	}
	journalInfoAfter, err := os.Stat(paths.journal)
	if err != nil {
		t.Fatalf("os.Stat(rotation journal after rejection) error = %v", err)
	}
	if !os.SameFile(journalInfoBefore, journalInfoAfter) {
		t.Fatal("RecoverRotation() replaced the rotation journal")
	}
	temporaryKeyAfter, err := winsecurity.ReadForegroundFile(paths.directory, keyTempFileName)
	if err != nil {
		t.Fatalf("ReadForegroundFile(temporary key after rejection) error = %v", err)
	}
	if !bytes.Equal(temporaryKeyAfter, temporaryKey) {
		t.Fatal("RecoverRotation() changed the protected temporary private key")
	}
	temporaryKeyInfoAfter, err := os.Stat(paths.keyTemp)
	if err != nil {
		t.Fatalf("os.Stat(temporary key after rejection) error = %v", err)
	}
	if !os.SameFile(temporaryKeyInfoBefore, temporaryKeyInfoAfter) {
		t.Fatal("RecoverRotation() replaced the protected temporary private key")
	}
	temporaryCertificateAfter, err := os.ReadFile(paths.certTemp)
	if err != nil {
		t.Fatalf("os.ReadFile(unprotected temporary certificate after rejection) error = %v", err)
	}
	if !bytes.Equal(temporaryCertificateAfter, temporaryCertificate) {
		t.Fatal("RecoverRotation() changed the unprotected temporary certificate")
	}
	temporaryInfoAfter, err := os.Stat(paths.certTemp)
	if err != nil {
		t.Fatalf("os.Stat(unprotected temporary certificate after rejection) error = %v", err)
	}
	if !os.SameFile(temporaryInfoBefore, temporaryInfoAfter) {
		t.Fatal("RecoverRotation() replaced the unprotected temporary certificate")
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
