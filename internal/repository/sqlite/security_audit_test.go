package sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/migrations"
)

const (
	sqliteAuditEventID = "evt_01J00000000000000000000000"
	sqliteOperationID  = "op_01J00000000000000000000000"
)

func TestSecurityAuditAppendIsIdempotentAndConflictsFail(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	event := sqliteSecurityAuditEvent()
	for attempt := range 2 {
		if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
			return transaction.SecurityAuditEvents().Append(context.Background(), event)
		}); err != nil {
			t.Fatalf("Append() attempt %d error = %v", attempt+1, err)
		}
	}
	var count int64
	if err := store.database.Table(SecurityAuditEventTable).Count(&count).Error; err != nil {
		t.Fatalf("count security audit events error = %v", err)
	}
	if count != 1 {
		t.Fatalf("security audit event count = %d, want 1", count)
	}

	conflicts := []struct {
		name  string
		event repository.SecurityAuditEvent
	}{
		{name: "same event id different operation id", event: func() repository.SecurityAuditEvent {
			conflict := event
			conflict.OperationID = "op_01J00000000000000000000001"
			return conflict
		}()},
		{name: "different event id same operation id", event: func() repository.SecurityAuditEvent {
			conflict := event
			conflict.EventID = "evt_01J00000000000000000000001"
			return conflict
		}()},
	}
	for _, test := range conflicts {
		t.Run(test.name, func(t *testing.T) {
			err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
				return transaction.SecurityAuditEvents().Append(context.Background(), test.event)
			})
			if !errors.Is(err, repository.ErrSecurityAuditConflict) {
				t.Fatalf("conflicting Append() error = %v, want ErrSecurityAuditConflict", err)
			}
		})
	}
}

func TestSecurityAuditConcurrentReplayCreatesOneEvent(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	event := sqliteSecurityAuditEvent()
	const workers = 8
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsByWorker <- store.WithTx(context.Background(), func(transaction repository.TxStore) error {
				return transaction.SecurityAuditEvents().Append(context.Background(), event)
			})
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent Append() error = %v", err)
		}
	}
	var count int64
	if err := store.database.Table(SecurityAuditEventTable).Count(&count).Error; err != nil {
		t.Fatalf("count security audit events error = %v", err)
	}
	if count != 1 {
		t.Fatalf("security audit event count = %d, want 1", count)
	}
}

func TestDurableTransactionUsesFullSynchronousMode(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	var synchronous int
	if err := store.WithDurableTx(context.Background(), func(transaction repository.TxStore) error {
		sqliteTransaction, ok := transaction.(*transactionStore)
		if !ok {
			t.Fatalf("transaction type = %T, want *transactionStore", transaction)
		}
		return sqliteTransaction.database.Raw("PRAGMA synchronous").Scan(&synchronous).Error
	}); err != nil {
		t.Fatalf("WithDurableTx() error = %v", err)
	}
	if synchronous != 2 {
		t.Fatalf("durable transaction synchronous mode = %d, want 2 (FULL)", synchronous)
	}
	store.pool.SetMaxOpenConns(1)
	store.pool.SetMaxIdleConns(1)
	var restored int
	if err := store.database.Connection(func(connection *gorm.DB) error {
		return connection.Raw("PRAGMA synchronous").Scan(&restored).Error
	}); err != nil {
		t.Fatalf("inspect restored synchronous mode error = %v", err)
	}
	if restored != 1 {
		t.Fatalf("restored synchronous mode = %d, want 1 (NORMAL)", restored)
	}
}

func TestDurableTransactionMarksCleanupFailureAfterCommit(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	restoreErr := errors.New("injected synchronous restore failure")
	if err := store.database.Callback().Raw().Before("gorm:raw").Register("test:fail-normal-restore", func(database *gorm.DB) {
		if strings.TrimSpace(database.Statement.SQL.String()) == "PRAGMA synchronous = NORMAL" {
			database.AddError(restoreErr)
		}
	}); err != nil {
		t.Fatalf("register restore failure callback: %v", err)
	}
	callbackErr := errors.New("injected transaction callback failure")
	err = store.WithDurableTx(context.Background(), func(repository.TxStore) error { return callbackErr })
	if !errors.Is(err, callbackErr) || !errors.Is(err, restoreErr) || errors.Is(err, repository.ErrPostCommitCleanup) {
		t.Fatalf("pre-commit cleanup error = %v, must not carry the post-commit marker", err)
	}

	err = store.WithDurableTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.Tunnels().Create(context.Background(), testTunnel())
	})
	if !errors.Is(err, repository.ErrPostCommitCleanup) || !errors.Is(err, restoreErr) {
		t.Fatalf("WithDurableTx() error = %v, want post-commit cleanup and injected causes", err)
	}
	if err := store.Read(context.Background(), func(view repository.RepositoryView) error {
		_, err := view.Tunnels().Get(context.Background(), repositoryTestTunnelID)
		return err
	}); err != nil {
		t.Fatalf("committed Tunnel missing after cleanup failure: %v", err)
	}
}

func TestSecurityAuditMigrationEnforcesAppendOnlyAndEnums(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	event := sqliteSecurityAuditEvent()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		return transaction.SecurityAuditEvents().Append(context.Background(), event)
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := store.database.Table(SecurityAuditEventTable).
		Where(SecurityAuditEventColumns.EventID+" = ?", event.EventID).
		Update(SecurityAuditEventColumns.ResourceID, "changed.example.test").Error; err == nil {
		t.Fatal("security audit table accepted UPDATE")
	}
	if err := store.database.Table(SecurityAuditEventTable).
		Where(SecurityAuditEventColumns.EventID+" = ?", event.EventID).
		Delete(&securityAuditEventRecord{}).Error; err == nil {
		t.Fatal("security audit table accepted DELETE")
	}
	if err := store.database.Exec(`
		INSERT INTO security_audit_events(
			event_id, operation_id, event, action, actor_type, resource_type, result, occurred_at
		) VALUES (?, ?, 'SECURITY_OPERATION_RESULT', 'UNKNOWN', 'LOCAL_OPERATOR', 'GATEWAY_IDENTITY', 'SUCCEEDED', 1)`,
		"evt_01J00000000000000000000001", "op_01J00000000000000000000001",
	).Error; err == nil {
		t.Fatal("security audit table accepted an unknown action")
	}
	invalidManagement := []struct {
		name         string
		eventID      string
		operationID  string
		actorType    string
		actorID      any
		resourceType string
		resourceID   string
	}{
		{
			name: "local actor", eventID: "evt_01J00000000000000000000005", operationID: "op_01J00000000000000000000005",
			actorType: repository.SecurityAuditActorLocalOperator, actorID: nil,
			resourceType: repository.SecurityAuditResourceTunnelToken, resourceID: repositoryTestTunnelID,
		},
		{
			name: "missing admin identity", eventID: "evt_01J00000000000000000000006", operationID: "op_01J00000000000000000000006",
			actorType: repository.SecurityAuditActorAdmin, actorID: nil,
			resourceType: repository.SecurityAuditResourceTunnelToken, resourceID: repositoryTestTunnelID,
		},
		{
			name: "token id resource", eventID: "evt_01J00000000000000000000007", operationID: "op_01J00000000000000000000007",
			actorType: repository.SecurityAuditActorAdmin, actorID: "adm_01J00000000000000000000000",
			resourceType: repository.SecurityAuditResourceTunnelToken, resourceID: repositoryTestTokenID,
		},
	}
	for _, test := range invalidManagement {
		t.Run(test.name, func(t *testing.T) {
			if err := store.database.Exec(`
				INSERT INTO security_audit_events(
					event_id, operation_id, event, action, actor_type, actor_id,
					resource_type, resource_id, result, occurred_at
				) VALUES (?, ?, 'SECURITY_OPERATION_RESULT', 'CONNECTION_TOKEN_ROTATE', ?, ?, ?, ?, 'SUCCEEDED', 1)`,
				test.eventID, test.operationID, test.actorType, test.actorID, test.resourceType, test.resourceID,
			).Error; err == nil {
				t.Fatal("security audit table accepted invalid management subject")
			}
		})
	}
}

func TestSecurityAuditMigrationMeasuresTextInUTF8Bytes(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	tooLong := strings.Repeat("界", 86)
	err = store.database.Exec(`
		INSERT INTO security_audit_events(
			event_id, operation_id, event, action, actor_type,
			resource_type, resource_id, result, occurred_at
		) VALUES (?, ?, 'SECURITY_OPERATION_RESULT', 'GATEWAY_KEY_ROTATE', 'LOCAL_OPERATOR',
			'GATEWAY_IDENTITY', ?, 'SUCCEEDED', 1)`,
		"evt_01J00000000000000000000002", "op_01J00000000000000000000002", tooLong,
	).Error
	if err == nil {
		t.Fatal("security audit table accepted a 258-byte resource_id")
	}
}

func TestCredentialLifecycleAuditMigrationPreservesV3Evidence(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:3], testNow); err != nil {
		t.Fatalf("run v3 migrations error = %v", err)
	}
	event := sqliteSecurityAuditEvent()
	if err := database.Create(securityAuditEventRecordFromDomain(event)).Error; err != nil {
		t.Fatalf("seed v3 audit evidence error = %v", err)
	}
	if err := runMigrations(context.Background(), database, productionMigrations, testNow); err != nil {
		t.Fatalf("upgrade to v4 error = %v", err)
	}

	var preserved securityAuditEventRecord
	if err := database.Where(SecurityAuditEventColumns.EventID+" = ?", event.EventID).Take(&preserved).Error; err != nil {
		t.Fatalf("read preserved v3 evidence error = %v", err)
	}
	if !preserved.equal(securityAuditEventRecordFromDomain(event)) {
		t.Fatal("v4 migration changed existing v3 evidence")
	}

	management := event
	management.EventID = "evt_01J00000000000000000000003"
	management.OperationID = "op_01J00000000000000000000003"
	management.Action = repository.SecurityAuditActionTokenRotate
	management.ActorType = repository.SecurityAuditActorAdmin
	management.ActorID = "adm_01J00000000000000000000000"
	management.SourceIP = "127.0.0.1"
	management.ResourceType = repository.SecurityAuditResourceTunnelToken
	management.ResourceID = repositoryTestTunnelID
	if err := database.Create(securityAuditEventRecordFromDomain(management)).Error; err != nil {
		t.Fatalf("insert v4 management evidence error = %v", err)
	}
	if err := database.Model(&securityAuditEventRecord{}).
		Where(SecurityAuditEventColumns.EventID+" = ?", management.EventID).
		Update(SecurityAuditEventColumns.Result, repository.SecurityAuditResultFailed).Error; err == nil {
		t.Fatal("v4 migration did not restore append-only UPDATE trigger")
	}
}

func TestCredentialLifecycleAuditMigrationRollsBackInterruptedRebuild(t *testing.T) {
	database := openUnmigratedDatabase(t)
	if err := runMigrations(context.Background(), database, productionMigrations[:3], testNow); err != nil {
		t.Fatalf("run v3 migrations error = %v", err)
	}
	event := sqliteSecurityAuditEvent()
	if err := database.Create(securityAuditEventRecordFromDomain(event)).Error; err != nil {
		t.Fatalf("seed v3 audit evidence error = %v", err)
	}
	interrupted := append([]migration{}, productionMigrations[:3]...)
	interrupted = append(interrupted, migration{version: 4, statements: []string{
		migrations.CredentialLifecycleAudit,
		"THIS IS NOT VALID SQL",
	}})
	if err := runMigrations(context.Background(), database, interrupted, testNow); err == nil {
		t.Fatal("interrupted v4 migration error = nil")
	}

	var versions []int
	if err := database.Table("schema_migrations").Order("version").Pluck("version", &versions).Error; err != nil {
		t.Fatalf("read versions after rollback error = %v", err)
	}
	if len(versions) != 3 || versions[2] != 3 {
		t.Fatalf("versions after rollback = %#v, want [1 2 3]", versions)
	}
	var preserved securityAuditEventRecord
	if err := database.Where(SecurityAuditEventColumns.EventID+" = ?", event.EventID).Take(&preserved).Error; err != nil {
		t.Fatalf("v3 evidence missing after rollback: %v", err)
	}
	if err := database.Model(&securityAuditEventRecord{}).
		Where(SecurityAuditEventColumns.EventID+" = ?", event.EventID).
		Update(SecurityAuditEventColumns.Result, repository.SecurityAuditResultFailed).Error; err == nil {
		t.Fatal("interrupted rebuild did not restore v3 append-only trigger")
	}
	management := event
	management.EventID = "evt_01J00000000000000000000004"
	management.OperationID = "op_01J00000000000000000000004"
	management.Action = repository.SecurityAuditActionTunnelRevoke
	management.ActorType = repository.SecurityAuditActorAdmin
	management.ActorID = "adm_01J00000000000000000000000"
	management.ResourceType = repository.SecurityAuditResourceTunnel
	management.ResourceID = repositoryTestTunnelID
	if err := database.Create(securityAuditEventRecordFromDomain(management)).Error; err == nil {
		t.Fatal("rolled-back v3 schema accepted an M2 audit action")
	}
}

func sqliteSecurityAuditEvent() repository.SecurityAuditEvent {
	return repository.SecurityAuditEvent{
		EventID: sqliteAuditEventID, OperationID: sqliteOperationID,
		Event:             repository.SecurityAuditEventOperationResult,
		Action:            repository.SecurityAuditActionGatewayKeyRotate,
		ActorType:         repository.SecurityAuditActorLocalOperator,
		ResourceType:      repository.SecurityAuditResourceGatewayIdentity,
		ResourceID:        "gateway.example.test",
		Result:            repository.SecurityAuditResultSucceeded,
		BeforeStateDigest: make([]byte, 32), AfterStateDigest: make([]byte, 32), OccurredAt: 1,
	}
}
