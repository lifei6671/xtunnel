package sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/repository"
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
