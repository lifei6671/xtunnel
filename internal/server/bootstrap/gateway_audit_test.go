package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libsqlite "github.com/libtnb/sqlite"
	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
	"github.com/lifei6671/xtunnel/internal/server/gateway"
)

func TestReconcileGatewayRotationAuditPersistsBeforeClearingJournal(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	now := time.Date(2026, time.August, 26, 0, 0, 0, 0, time.UTC)
	before, err := gateway.LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now)
	if err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	audit := gateway.RotationAuditMetadata{
		EventID: "evt_01J00000000000000000000011", OperationID: "op_01J00000000000000000000011",
		OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
	}
	after, err := gateway.RotatePinnedIdentity(dataDir, "gateway.example.test", now.Add(time.Hour), audit)
	if err != nil {
		t.Fatalf("RotatePinnedIdentity() error = %v", err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	reconciled, err := reconcileGatewayRotationAudit(ctx, dataDir, store, logger)
	if err != nil || !reconciled {
		t.Fatalf("reconcileGatewayRotationAudit() = %t, %v", reconciled, err)
	}
	if _, exists, err := gateway.PendingRotationAuditEvent(dataDir); err != nil || exists {
		t.Fatalf("PendingRotationAuditEvent() after commit = exists %t, error %v", exists, err)
	}
	if !strings.Contains(output.String(), `"msg":"security_audit_event"`) {
		t.Fatalf("security audit output = %q", output.String())
	}
	database := openGatewayAuditTestDatabase(t, dataDir)
	defer closeGatewayAuditTestDatabase(t, database)
	var record struct {
		EventID           string
		OperationID       string
		BeforeStateDigest []byte
		AfterStateDigest  []byte
	}
	if err := database.Table(sqlite.SecurityAuditEventTable).Take(&record).Error; err != nil {
		t.Fatalf("read security audit event error = %v", err)
	}
	beforeDigest := before.SPKIHash()
	afterDigest := after.SPKIHash()
	if record.EventID != audit.EventID || record.OperationID != audit.OperationID ||
		!bytes.Equal(record.BeforeStateDigest, beforeDigest[:]) || !bytes.Equal(record.AfterStateDigest, afterDigest[:]) {
		t.Fatalf("security audit record = %#v", record)
	}
	reconciled, err = reconcileGatewayRotationAudit(ctx, dataDir, store, logger)
	if err != nil || reconciled {
		t.Fatalf("second reconcileGatewayRotationAudit() = %t, %v", reconciled, err)
	}
}

func TestReconcileGatewayRotationAuditRetriesAfterDatabaseFailure(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	now := time.Date(2026, time.August, 26, 0, 0, 0, 0, time.UTC)
	if _, err := gateway.LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	audit := gateway.RotationAuditMetadata{
		EventID: "evt_01J00000000000000000000012", OperationID: "op_01J00000000000000000000012",
		OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
	}
	if _, err := gateway.RotatePinnedIdentity(dataDir, "gateway.example.test", now.Add(time.Hour), audit); err != nil {
		t.Fatalf("RotatePinnedIdentity() error = %v", err)
	}
	database := openGatewayAuditTestDatabase(t, dataDir)
	if err := database.Exec(`
		CREATE TRIGGER reject_gateway_rotation_audit
		BEFORE INSERT ON security_audit_events
		BEGIN
			SELECT RAISE(ABORT, 'injected audit failure');
		END;
	`).Error; err != nil {
		t.Fatalf("create audit failure trigger error = %v", err)
	}
	closeGatewayAuditTestDatabase(t, database)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	if reconciled, err := reconcileGatewayRotationAudit(ctx, dataDir, store, logger); err == nil || reconciled {
		t.Fatalf("failed reconcileGatewayRotationAudit() = %t, %v", reconciled, err)
	}
	if strings.Contains(output.String(), `"msg":"security_audit_event"`) {
		t.Fatalf("failed audit append emitted success log: %q", output.String())
	}
	if _, exists, err := gateway.PendingRotationAuditEvent(dataDir); err != nil || !exists {
		t.Fatalf("PendingRotationAuditEvent() after failure = exists %t, error %v", exists, err)
	}
	database = openGatewayAuditTestDatabase(t, dataDir)
	if err := database.Exec(`DROP TRIGGER reject_gateway_rotation_audit`).Error; err != nil {
		t.Fatalf("drop audit failure trigger error = %v", err)
	}
	closeGatewayAuditTestDatabase(t, database)
	if reconciled, err := reconcileGatewayRotationAudit(ctx, dataDir, store, logger); err != nil || !reconciled {
		t.Fatalf("retried reconcileGatewayRotationAudit() = %t, %v", reconciled, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "pki", "agent-gateway.rotation.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rotation journal remains after retry: %v", err)
	}
}

func TestReconcileGatewayRotationAuditTreatsPostUnlinkSyncFailureAsWarning(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := sqlite.Open(ctx, dataDir)
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	now := time.Date(2026, time.August, 26, 0, 0, 0, 0, time.UTC)
	if _, err := gateway.LoadOrCreatePinnedIdentity(dataDir, "gateway.example.test", true, now); err != nil {
		t.Fatalf("LoadOrCreatePinnedIdentity() error = %v", err)
	}
	audit := gateway.RotationAuditMetadata{
		EventID: "evt_01J00000000000000000000013", OperationID: "op_01J00000000000000000000013",
		OccurredAt: now.Add(time.Hour).Unix(), ResourceID: "gateway.example.test",
	}
	if _, err := gateway.RotatePinnedIdentity(dataDir, audit.ResourceID, now.Add(time.Hour), audit); err != nil {
		t.Fatalf("RotatePinnedIdentity() error = %v", err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	injected := errors.New("injected directory sync failure")
	reconciled, err := reconcileGatewayRotationAuditWith(
		ctx, dataDir, store, logger,
		func(dataDir, eventID, operationID string) error {
			if err := gateway.CompleteRotationAudit(dataDir, eventID, operationID); err != nil {
				return err
			}
			return errors.Join(gateway.ErrRotationAuditCleanupUncertain, injected)
		},
	)
	if err != nil || !reconciled {
		t.Fatalf("reconcileGatewayRotationAuditWith() = %t, %v", reconciled, err)
	}
	if !strings.Contains(output.String(), `"msg":"gateway_rotation_audit_cleanup_uncertain"`) ||
		!strings.Contains(output.String(), `"error_code":"AUDIT_JOURNAL_DIRECTORY_SYNC_FAILED"`) {
		t.Fatalf("cleanup warning output = %q", output.String())
	}
	if _, exists, err := gateway.PendingRotationAuditEvent(dataDir); err != nil || exists {
		t.Fatalf("PendingRotationAuditEvent() after uncertain cleanup = exists %t, error %v", exists, err)
	}
}

func openGatewayAuditTestDatabase(t *testing.T, dataDir string) *gorm.DB {
	t.Helper()
	database, err := gorm.Open(libsqlite.Open(filepath.Join(dataDir, "xtunnel.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open gateway audit database error = %v", err)
	}
	return database
}

func closeGatewayAuditTestDatabase(t *testing.T, database *gorm.DB) {
	t.Helper()
	pool, err := database.DB()
	if err != nil {
		t.Fatalf("get gateway audit database pool error = %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close gateway audit database error = %v", err)
	}
}
