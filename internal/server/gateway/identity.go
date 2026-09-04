// Package gateway 提供 Agent Gateway 的 TLS 身份、握手边界和离线轮换能力。
package gateway

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lifei6671/xtunnel/internal/protocol/validate"
)

const (
	// PinnedMode 是 Server 自管、由 Agent SPKI Pin 校验的 TLS 身份模式。
	PinnedMode = "pinned"
	// PublicMode 是由外部 CA 签发、由 Agent 系统根证书校验的 TLS 身份模式。
	PublicMode = "public"

	pkiDirectoryName = "pki"
	keyFileName      = "agent-gateway.key"
	certFileName     = "agent-gateway.crt"
	journalFileName  = "agent-gateway.rotation.json"
	keyTempFileName  = "agent-gateway.key.rotate"
	certTempFileName = "agent-gateway.crt.rotate"

	// pinnedRenewalWindow 是 pinned 自签证书允许自动续签的剩余有效期边界。
	// 到达边界时必须立即续签，避免下一次正常启动错过 30 天告警窗口。
	pinnedRenewalWindow = 30 * 24 * time.Hour
)

var (
	// ErrIdentityMissing 表示已有 Server 数据目录缺少其必须保持不变的 pinned 身份。
	ErrIdentityMissing = errors.New("gateway pinned TLS identity is missing")
	// ErrPinnedIdentityMismatch 表示私钥与证书不是同一对，不能安全启动 Gateway。
	ErrPinnedIdentityMismatch = errors.New("gateway pinned TLS key and certificate do not match")
	// ErrPublicRotation 表示 public 模式的私钥由外部证书管理系统负责，不能使用本命令轮换。
	ErrPublicRotation = errors.New("gateway key rotation is only available in pinned TLS mode")
	// ErrRotationAuditPending 表示上一次离线换钥已经进入提交阶段，但权威审计尚未确认落库。
	ErrRotationAuditPending = errors.New("gateway key rotation has a pending security audit event")
	// ErrRotationAuditCleanupUncertain 表示事件已耐久提交且 Journal 已删除，但目录同步失败。
	ErrRotationAuditCleanupUncertain = errors.New("gateway rotation audit cleanup durability is uncertain")
)

// RotationAuditMetadata 是离线换钥开始前必须持久化的审计标识。
type RotationAuditMetadata struct {
	EventID     string
	OperationID string
	OccurredAt  int64
	ResourceID  string
}

// PendingRotationAudit 是身份文件已提交后等待写入权威审计库的不可变证据。
type PendingRotationAudit struct {
	RotationAuditMetadata
	BeforeStateDigest [sha256.Size]byte
	AfterStateDigest  [sha256.Size]byte
}

// Identity 是已经校验完毕、可供 Gateway 构造 TLS 配置的服务端身份。
type Identity struct {
	Certificate tlsCertificate

	// pinnedRenewal 只在由 Server 自管的 pinned 身份中存在，保存续签所需的
	// 本地文件和主机名；public 身份没有该来源，不能由 Gateway 自动续签。
	pinnedRenewal *pinnedRenewalSource
}

type pinnedRenewalSource struct {
	paths    identityFilePaths
	hostname string
}

// tlsCertificate 避免在身份文件逻辑中泄漏可修改的 TLS 配置。
// 实际 Gateway 创建时会转换为 crypto/tls.Certificate。
type tlsCertificate struct {
	certificate [][]byte
	privateKey  crypto.PrivateKey
	leaf        *x509.Certificate
}

// CertificateChain 返回 TLS 握手使用的叶子和链证书 DER 字节副本。
func (identity Identity) CertificateChain() [][]byte {
	chain := make([][]byte, len(identity.Certificate.certificate))
	for index, item := range identity.Certificate.certificate {
		chain[index] = append([]byte(nil), item...)
	}
	return chain
}

// PrivateKey 返回仅由 Gateway 内部使用的私钥对象。
func (identity Identity) PrivateKey() crypto.PrivateKey {
	return identity.Certificate.privateKey
}

// Leaf 返回已解析的叶子证书。
func (identity Identity) Leaf() *x509.Certificate {
	return identity.Certificate.leaf
}

// SPKIHash 返回固定的 SHA-256 SubjectPublicKeyInfo 摘要，供后续 Token 签发使用。
func (identity Identity) SPKIHash() [sha256.Size]byte {
	return sha256.Sum256(identity.Certificate.leaf.RawSubjectPublicKeyInfo)
}

// LoadPinnedIdentity 读取已经存在的 pinned 身份；不会静默补建缺失文件。
func LoadPinnedIdentity(dataDir string) (Identity, error) {
	paths := identityPaths(dataDir)
	if err := validatePinnedIdentityFiles(dataDir, paths.key, paths.cert); err != nil {
		return Identity{}, err
	}
	certificate, err := loadKeyPair(paths.key, paths.cert)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Certificate: certificate}, nil
}

// LoadPublicIdentity 读取外部证书管理系统提供的 public TLS 身份。平台实现负责
// 在解析 PEM 前验证外部文件边界；它绝不接管、复制或修复 operator-owned 对象。
func LoadPublicIdentity(certPath, keyPath string) (Identity, error) {
	certPEM, keyPEM, err := readPublicIdentityFiles(certPath, keyPath)
	if err != nil {
		return Identity{}, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Identity{}, fmt.Errorf("load public gateway certificate: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return Identity{}, errors.New("public gateway certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Identity{}, fmt.Errorf("parse public gateway leaf certificate: %w", err)
	}
	return Identity{Certificate: tlsCertificate{certificate: pair.Certificate, privateKey: pair.PrivateKey, leaf: leaf}}, nil
}

// LoadOrCreatePinnedIdentity 只允许在全新数据目录中首次生成 pinned 身份。
// 对已有数据目录，任何缺失都必须快速失败，以避免 Agent 的既有 Pin 被静默替换。
func LoadOrCreatePinnedIdentity(dataDir, hostname string, mayCreate bool, now time.Time) (Identity, error) {
	paths := identityPaths(dataDir)
	if err := RecoverRotation(dataDir); err != nil {
		return Identity{}, err
	}
	identity, err := LoadPinnedIdentity(dataDir)
	if err == nil {
		identity = withPinnedRenewalSource(identity, paths, hostname)
		// 续签只替换证书且复用原私钥，因此不会改变 Agent Token 中的 SPKI Pin。
		identity, err = renewPinnedIdentity(paths, hostname, identity, now)
		if err != nil {
			return Identity{}, err
		}
		return identity, nil
	}
	if !errors.Is(err, ErrIdentityMissing) || !mayCreate {
		return Identity{}, err
	}
	if hostname == "" {
		return Identity{}, errors.New("gateway public hostname must not be empty")
	}
	if err := createPinnedIdentityDirectory(dataDir, paths.directory); err != nil {
		return Identity{}, fmt.Errorf("create gateway PKI directory: %w", err)
	}
	certificate, err := newSelfSignedCertificate(hostname, now)
	if err != nil {
		return Identity{}, err
	}
	if err := writePinnedIdentity(dataDir, paths.key, paths.cert, certificate); err != nil {
		return Identity{}, err
	}
	return withPinnedRenewalSource(Identity{Certificate: certificate}, paths, hostname), nil
}

// RotatePinnedIdentity 在已持有 Server External Lock 的离线维护窗口内轮换 SPKI。
// Journal 在两个原子替换之间保持存在，使崩溃后的启动能够完成同一组替换而非加载错配文件。
func RotatePinnedIdentity(dataDir, hostname string, now time.Time, audit RotationAuditMetadata) (Identity, error) {
	return rotatePinnedIdentity(dataDir, hostname, now, audit, defaultRotationFileOps())
}

func rotatePinnedIdentity(
	dataDir, hostname string,
	now time.Time,
	audit RotationAuditMetadata,
	fileOps rotationFileOps,
) (Identity, error) {
	if hostname == "" {
		return Identity{}, errors.New("gateway public hostname must not be empty")
	}
	if err := validateRotationAuditMetadata(audit); err != nil || audit.ResourceID != hostname {
		return Identity{}, errors.New("gateway rotation audit metadata is invalid")
	}
	paths := identityPaths(dataDir)
	if err := recoverRotation(dataDir, fileOps); err != nil {
		return Identity{}, err
	}
	if _, exists, err := PendingRotationAuditEvent(dataDir); err != nil {
		return Identity{}, err
	} else if exists {
		return Identity{}, ErrRotationAuditPending
	}
	before, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		return Identity{}, fmt.Errorf("load existing gateway identity before rotation: %w", err)
	}
	certificate, err := newSelfSignedCertificate(hostname, now)
	if err != nil {
		return Identity{}, err
	}
	if err := writeKeyPairWith(paths.keyTemp, paths.certTemp, certificate, fileOps.writeFileSync); err != nil {
		cleanupErr := rollbackRotationPreparation(paths, fileOps)
		return Identity{}, errors.Join(err, cleanupErr)
	}
	journal := rotationJournal{
		Version: 2, KeyTemporary: paths.keyTemp, CertificateTemporary: paths.certTemp,
		Audit: &rotationAuditJournal{
			EventID: audit.EventID, OperationID: audit.OperationID,
			OccurredAt: audit.OccurredAt, ResourceID: audit.ResourceID,
			BeforeStateDigest: before.SPKIHash(),
			AfterStateDigest:  sha256.Sum256(certificate.leaf.RawSubjectPublicKeyInfo),
		},
	}
	if err := writeJournalWith(paths.journal, journal, fileOps); err != nil {
		cleanupErr := rollbackRotationPreparation(paths, fileOps)
		return Identity{}, errors.Join(err, cleanupErr)
	}
	if err := completeRotationWith(paths, journal, false, fileOps); err != nil {
		return Identity{}, err
	}
	identity, err := LoadPinnedIdentity(dataDir)
	if err != nil {
		return Identity{}, err
	}
	if identity.SPKIHash() != journal.Audit.AfterStateDigest {
		return Identity{}, errors.New("rotated gateway identity does not match the journal after-state")
	}
	return identity, nil
}

// RecoverRotation 完成已经落盘的轮换 Journal；没有 Journal 时绝不修改身份文件。
func RecoverRotation(dataDir string) error {
	return recoverRotation(dataDir, defaultRotationFileOps())
}

func recoverRotation(dataDir string, fileOps rotationFileOps) error {
	paths := identityPaths(dataDir)
	journal, exists, err := readJournal(dataDir, paths.journal)
	if err != nil || !exists {
		return err
	}
	if journal.KeyTemporary != paths.keyTemp || journal.CertificateTemporary != paths.certTemp {
		return errors.New("gateway rotation journal is invalid")
	}
	switch journal.Version {
	case 1:
		return completeRotationWith(paths, journal, true, fileOps)
	case 2:
		if err := validateRotationAuditJournal(journal.Audit); err != nil {
			return err
		}
		if err := completeRotationWith(paths, journal, false, fileOps); err != nil {
			return err
		}
		identity, err := LoadPinnedIdentity(dataDir)
		if err != nil {
			return err
		}
		if identity.SPKIHash() != journal.Audit.AfterStateDigest {
			return errors.New("recovered gateway identity does not match the journal after-state")
		}
		return nil
	default:
		return errors.New("gateway rotation journal is invalid")
	}
}

// PendingRotationAuditEvent 返回已经完成或可恢复完成的 v2 换钥审计证据。
func PendingRotationAuditEvent(dataDir string) (PendingRotationAudit, bool, error) {
	journal, exists, err := readJournal(dataDir, identityPaths(dataDir).journal)
	if err != nil || !exists {
		return PendingRotationAudit{}, false, err
	}
	if journal.Version != 2 {
		return PendingRotationAudit{}, false, nil
	}
	if err := validateRotationAuditJournal(journal.Audit); err != nil {
		return PendingRotationAudit{}, true, err
	}
	return PendingRotationAudit{
		RotationAuditMetadata: RotationAuditMetadata{
			EventID: journal.Audit.EventID, OperationID: journal.Audit.OperationID,
			OccurredAt: journal.Audit.OccurredAt, ResourceID: journal.Audit.ResourceID,
		},
		BeforeStateDigest: journal.Audit.BeforeStateDigest,
		AfterStateDigest:  journal.Audit.AfterStateDigest,
	}, true, nil
}

// CompleteRotationAudit 删除已经幂等落库的换钥 Journal。调用方必须持有 External Lock。
func CompleteRotationAudit(dataDir, eventID, operationID string) error {
	return completeRotationAudit(dataDir, eventID, operationID, defaultRotationFileOps().syncDirectory)
}

func completeRotationAudit(
	dataDir, eventID, operationID string,
	syncPKIDirectory func(string) error,
) error {
	paths := identityPaths(dataDir)
	journal, exists, err := readJournal(dataDir, paths.journal)
	if err != nil {
		return err
	}
	if !exists || journal.Version != 2 || journal.Audit == nil ||
		journal.Audit.EventID != eventID || journal.Audit.OperationID != operationID {
		return errors.New("gateway rotation audit journal does not match the committed event")
	}
	if err := removePinnedIdentityFile(paths.journal); err != nil {
		return fmt.Errorf("remove gateway rotation audit journal: %w", err)
	}
	if err := syncPKIDirectory(paths.directory); err != nil {
		return errors.Join(
			ErrRotationAuditCleanupUncertain,
			fmt.Errorf("sync completed gateway rotation audit: %w", err),
		)
	}
	return nil
}

type identityFilePaths struct {
	directory string
	key       string
	cert      string
	journal   string
	keyTemp   string
	certTemp  string
}

func identityPaths(dataDir string) identityFilePaths {
	directory := filepath.Join(dataDir, pkiDirectoryName)
	return identityFilePaths{
		directory: directory,
		key:       filepath.Join(directory, keyFileName),
		cert:      filepath.Join(directory, certFileName),
		journal:   filepath.Join(directory, journalFileName),
		keyTemp:   filepath.Join(directory, keyTempFileName),
		certTemp:  filepath.Join(directory, certTempFileName),
	}
}

func newSelfSignedCertificate(hostname string, now time.Time) (tlsCertificate, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsCertificate{}, fmt.Errorf("generate gateway private key: %w", err)
	}
	return newSelfSignedCertificateWithPrivateKey(hostname, now, privateKey)
}

// newSelfSignedCertificateWithPrivateKey 使用给定私钥签发自签证书。
// 续签路径只能调用该函数，禁止重新生成私钥而改变已经部署给 Agent 的 SPKI Pin。
func newSelfSignedCertificateWithPrivateKey(hostname string, now time.Time, privateKey *ecdsa.PrivateKey) (tlsCertificate, error) {
	if hostname == "" {
		return tlsCertificate{}, errors.New("gateway public hostname must not be empty")
	}
	if privateKey == nil {
		return tlsCertificate{}, errors.New("gateway private key must not be nil")
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tlsCertificate{}, fmt.Errorf("generate gateway certificate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkixName(hostname),
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(397 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{hostname},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tlsCertificate{}, fmt.Errorf("create gateway self-signed certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tlsCertificate{}, fmt.Errorf("parse generated gateway certificate: %w", err)
	}
	return tlsCertificate{certificate: [][]byte{der}, privateKey: privateKey, leaf: leaf}, nil
}

// renewPinnedIdentity 在证书剩余有效期不超过 30 天时原子替换证书文件。
// 私钥文件不会参与续签，也不会写入轮换 Journal；Journal 仅用于显式离线 SPKI 轮换。
func renewPinnedIdentity(paths identityFilePaths, hostname string, identity Identity, now time.Time) (Identity, error) {
	leaf := identity.Leaf()
	if now.Before(leaf.NotBefore) {
		return Identity{}, fmt.Errorf(
			"gateway pinned certificate is not yet valid: current time %s is before NotBefore %s",
			now.UTC().Format(time.RFC3339),
			leaf.NotBefore.UTC().Format(time.RFC3339),
		)
	}
	if leaf.NotAfter.Sub(now) > pinnedRenewalWindow {
		return identity, nil
	}
	privateKey, ok := identity.PrivateKey().(*ecdsa.PrivateKey)
	if !ok {
		return Identity{}, errors.New("gateway pinned private key is not ECDSA P-256")
	}
	certificate, err := newSelfSignedCertificateWithPrivateKey(hostname, now, privateKey)
	if err != nil {
		return Identity{}, fmt.Errorf("renew gateway pinned certificate: %w", err)
	}
	if err := replacePinnedCertificate(filepath.Dir(paths.directory), paths.directory, paths.cert, certificate); err != nil {
		return Identity{}, fmt.Errorf("atomically renew gateway pinned certificate: %w", err)
	}
	return withPinnedRenewalSource(Identity{Certificate: certificate}, paths, hostname), nil
}

// renewIfNecessary 只允许带有 pinned 续签来源的身份执行自动续签。
// public 模式没有来源且其证书生命周期由外部证书管理系统负责。
func (identity Identity) renewIfNecessary(now time.Time) (Identity, error) {
	if identity.pinnedRenewal == nil {
		return identity, nil
	}
	return renewPinnedIdentity(
		identity.pinnedRenewal.paths,
		identity.pinnedRenewal.hostname,
		identity,
		now,
	)
}

func withPinnedRenewalSource(identity Identity, paths identityFilePaths, hostname string) Identity {
	identity.pinnedRenewal = &pinnedRenewalSource{paths: paths, hostname: hostname}
	return identity
}

// replaceCertificateAtomically 先将新证书完整落盘到 PKI 目录中的临时文件，
// 再以同目录 rename 替换旧证书。私钥未变，即使进程在两个步骤之间退出也不会形成错配。
func replaceCertificateAtomically(directory, certPath string, certificate tlsCertificate) (resultErr error) {
	temporary, err := os.CreateTemp(directory, certFileName+".renew-*")
	if err != nil {
		return fmt.Errorf("create gateway certificate renewal temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err := os.Remove(temporaryPath); resultErr == nil && err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = fmt.Errorf("remove gateway certificate renewal temporary file: %w", err)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return fmt.Errorf("set gateway certificate renewal temporary file permissions: %w", errors.Join(err, closeErr))
		}
		return fmt.Errorf("set gateway certificate renewal temporary file permissions: %w", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.certificate[0]})
	if _, err := temporary.Write(certificatePEM); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return fmt.Errorf("write gateway certificate renewal temporary file: %w", errors.Join(err, closeErr))
		}
		return fmt.Errorf("write gateway certificate renewal temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return fmt.Errorf("sync gateway certificate renewal temporary file: %w", errors.Join(err, closeErr))
		}
		return fmt.Errorf("sync gateway certificate renewal temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close gateway certificate renewal temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, certPath); err != nil {
		return fmt.Errorf("replace gateway certificate with renewal: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync renewed gateway certificate directory: %w", err)
	}
	return nil
}

// pkixName 保持证书 Subject 的构造集中，避免把 hostname 误用为 URL 或文件路径。
func pkixName(hostname string) pkix.Name {
	return pkix.Name{CommonName: hostname}
}

func loadKeyPair(keyPath, certPath string) (tlsCertificate, error) {
	keyPEM, err := readPinnedIdentityFile(keyPath)
	if err != nil {
		return tlsCertificate{}, fmt.Errorf("read gateway private key: %w", err)
	}
	certPEM, err := readPinnedIdentityFile(certPath)
	if err != nil {
		return tlsCertificate{}, fmt.Errorf("read gateway certificate: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		return tlsCertificate{}, errors.New("gateway private key PEM is invalid")
	}
	privateKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return tlsCertificate{}, fmt.Errorf("parse gateway private key: %w", err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return tlsCertificate{}, errors.New("gateway certificate PEM is invalid")
	}
	leaf, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return tlsCertificate{}, fmt.Errorf("parse gateway certificate: %w", err)
	}
	publicKey, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.X.Cmp(privateKey.PublicKey.X) != 0 || publicKey.Y.Cmp(privateKey.PublicKey.Y) != 0 {
		return tlsCertificate{}, ErrPinnedIdentityMismatch
	}
	return tlsCertificate{certificate: [][]byte{append([]byte(nil), certBlock.Bytes...)}, privateKey: privateKey, leaf: leaf}, nil
}

func writeIdentity(keyPath, certPath string, certificate tlsCertificate) error {
	if err := writeKeyPair(keyPath, certPath, certificate); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(keyPath)); err != nil {
		return fmt.Errorf("sync gateway PKI directory: %w", err)
	}
	return nil
}

func writeKeyPair(keyPath, certPath string, certificate tlsCertificate) error {
	return writeKeyPairWith(keyPath, certPath, certificate, writeFileSync)
}

func writeKeyPairWith(
	keyPath, certPath string,
	certificate tlsCertificate,
	writeSyncedFile func(string, []byte, os.FileMode) error,
) error {
	keyPEM, certPEM, err := pinnedIdentityPEM(certificate)
	if err != nil {
		return err
	}
	if err := writeSyncedFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write gateway private key: %w", err)
	}
	if err := writeSyncedFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write gateway certificate: %w", err)
	}
	return nil
}

func pinnedIdentityPEM(certificate tlsCertificate) ([]byte, []byte, error) {
	privateKey, ok := certificate.privateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("gateway private key is not ECDSA P-256")
	}
	keyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal gateway private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.certificate[0]})
	return keyPEM, certPEM, nil
}

type rotationJournal struct {
	Version              int                   `json:"version"`
	KeyTemporary         string                `json:"key_temporary"`
	CertificateTemporary string                `json:"certificate_temporary"`
	Audit                *rotationAuditJournal `json:"audit,omitempty"`
}

type rotationAuditJournal struct {
	EventID           string            `json:"event_id"`
	OperationID       string            `json:"operation_id"`
	OccurredAt        int64             `json:"occurred_at"`
	ResourceID        string            `json:"resource_id"`
	BeforeStateDigest [sha256.Size]byte `json:"before_state_digest"`
	AfterStateDigest  [sha256.Size]byte `json:"after_state_digest"`
}

// rotationFileOps 只承载离线换钥的文件系统提交点。调用链按值传递该组操作，
// 使失败注入不会污染并发测试或其他 Gateway 身份生命周期。
type rotationFileOps struct {
	writeFileSync func(string, []byte, os.FileMode) error
	rename        func(string, string) error
	remove        func(string) error
	syncDirectory func(string) error
}

// rollbackRotationPreparation 只在 Journal 发布返回错误、身份 rename 尚未开始时调用。
// 这些路径均由当前操作精确拥有，因此可以逐个删除；禁止扫描前缀猜测其他操作的产物。
func rollbackRotationPreparation(paths identityFilePaths, fileOps rotationFileOps) error {
	var cleanupErr error
	removed := false
	for _, path := range []string{paths.journal, paths.keyTemp, paths.certTemp} {
		if err := fileOps.remove(path); err == nil {
			removed = true
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove failed gateway rotation preparation %q: %w", filepath.Base(path), err))
		}
	}
	if removed {
		if err := fileOps.syncDirectory(paths.directory); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync gateway rotation preparation rollback: %w", err))
		}
	}
	return cleanupErr
}

func validateRotationAuditJournal(audit *rotationAuditJournal) error {
	if audit == nil || validateRotationAuditMetadata(RotationAuditMetadata{
		EventID: audit.EventID, OperationID: audit.OperationID,
		OccurredAt: audit.OccurredAt, ResourceID: audit.ResourceID,
	}) != nil {
		return errors.New("gateway rotation audit journal is invalid")
	}
	return nil
}

func validateRotationAuditMetadata(audit RotationAuditMetadata) error {
	if !validate.ValidID(audit.EventID, "evt_") || !validate.ValidID(audit.OperationID, "op_") ||
		audit.OccurredAt <= 0 || len(audit.ResourceID) == 0 || len(audit.ResourceID) > 256 ||
		strings.TrimSpace(audit.ResourceID) != audit.ResourceID {
		return errors.New("gateway rotation audit metadata is invalid")
	}
	return nil
}

func writeJournal(path string, journal rotationJournal) error {
	return writeJournalWith(path, journal, defaultRotationFileOps())
}

func writeJournalWith(path string, journal rotationJournal, fileOps rotationFileOps) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("marshal gateway rotation journal: %w", err)
	}
	if err := fileOps.writeFileSync(path, data, 0o600); err != nil {
		return fmt.Errorf("write gateway rotation journal: %w", err)
	}
	if err := fileOps.syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync gateway rotation journal directory: %w", err)
	}
	return nil
}

// readJournal 在解析未决轮换状态前先验证 Data Directory。Journal 可能决定
// 恢复替换或删除自身，不能只因 pki 目录和 Journal 文件受保护就跳过其根边界。
func readJournal(dataDir, path string) (rotationJournal, bool, error) {
	if err := validatePinnedIdentityDataDirectory(dataDir); err != nil {
		return rotationJournal{}, true, fmt.Errorf("validate server data directory before reading gateway rotation journal: %w", err)
	}
	data, err := readPinnedIdentityFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rotationJournal{}, false, nil
	}
	if err != nil {
		return rotationJournal{}, true, fmt.Errorf("read gateway rotation journal: %w", err)
	}
	var journal rotationJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return rotationJournal{}, true, fmt.Errorf("parse gateway rotation journal: %w", err)
	}
	return journal, true, nil
}

func completeRotation(paths identityFilePaths, journal rotationJournal, removeJournal bool) error {
	return completeRotationWith(paths, journal, removeJournal, defaultRotationFileOps())
}

func completeRotationWith(
	paths identityFilePaths,
	journal rotationJournal,
	removeJournal bool,
	fileOps rotationFileOps,
) error {
	replacements := []struct{ temporary, destination string }{
		{journal.KeyTemporary, paths.key},
		{journal.CertificateTemporary, paths.cert},
	}
	// 先完整验证所有已存在的临时对象，再开始正式替换。这样某个临时文件的
	// Protected DACL/no-follow 校验失败时，不会留下“私钥已替换、证书未替换”的
	// 部分轮换状态；实际替换仍由平台的安全发布原语在提交点再次验证。
	temporaryExists := make([]bool, len(replacements))
	for index, replacement := range replacements {
		if exists, err := rotationTemporaryExists(replacement.temporary); err == nil && exists {
			temporaryExists[index] = true
		} else if err != nil {
			return fmt.Errorf("inspect gateway identity replacement: %w", err)
		}
	}
	for index, replacement := range replacements {
		if temporaryExists[index] {
			if err := fileOps.rename(replacement.temporary, replacement.destination); err != nil {
				return fmt.Errorf("atomically replace gateway identity file: %w", err)
			}
			if err := fileOps.syncDirectory(paths.directory); err != nil {
				return fmt.Errorf("sync gateway identity replacement: %w", err)
			}
		}
	}
	if _, err := LoadPinnedIdentity(filepath.Dir(paths.directory)); err != nil {
		return fmt.Errorf("validate recovered gateway identity: %w", err)
	}
	if removeJournal {
		if err := fileOps.remove(paths.journal); err != nil {
			return fmt.Errorf("remove gateway rotation journal: %w", err)
		}
		if err := fileOps.syncDirectory(paths.directory); err != nil {
			return fmt.Errorf("sync completed gateway rotation: %w", err)
		}
	}
	return nil
}
