package application

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lifei6671/xtunnel/internal/logging"
	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/lifei6671/xtunnel/internal/repository/sqlite"
)

func TestSecurityAuditWriterPersistsBeforeStructuredLog(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	var output bytes.Buffer
	logger, err := logging.New(&output, logging.Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	writer := NewSecurityAuditWriter(store, logger)
	event := applicationSecurityAuditEvent()
	if err := writer.Append(context.Background(), event); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	logLine := output.String()
	for _, field := range []string{
		`"event":"security_audit_event"`,
		`"event_id":"` + event.EventID + `"`,
		`"operation_id":"` + event.OperationID + `"`,
		`"action":"GATEWAY_KEY_ROTATE"`,
		`"result":"SUCCEEDED"`,
	} {
		if !strings.Contains(logLine, field) {
			t.Fatalf("security log %q does not contain %q", logLine, field)
		}
	}
}

func TestSecurityAuditWriterLogsCommittedEventOnPostCommitCleanupFailure(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	cleanupErr := errors.New("injected security audit cleanup failure")
	recordingStore := &recordingAuditStore{Store: store}
	faultStore := &postCommitCleanupStore{Store: recordingStore, err: cleanupErr}
	output, writer := newTestAuditWriter(t, faultStore)
	event := applicationSecurityAuditEvent()

	err = writer.Append(context.Background(), event)
	if !errors.Is(err, repository.ErrPostCommitCleanup) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Append() error = %v, want post-commit cleanup causes", err)
	}
	events := recordingStore.snapshot()
	if len(events) != 1 || events[0].EventID != event.EventID || events[0].OperationID != event.OperationID {
		t.Fatalf("committed audit events = %#v, want the submitted event", events)
	}
	if !strings.Contains(output.String(), `"event":"security_audit_event"`) ||
		!strings.Contains(output.String(), `"event_id":"`+event.EventID+`"`) {
		t.Fatalf("committed audit event was not logged: %q", output.String())
	}
}

func TestSecurityAuditWriterRejectsInvalidInputWithoutLogging(t *testing.T) {
	var output bytes.Buffer
	logger, err := logging.New(&output, logging.Options{Level: "info", Format: "json", Component: "server"})
	if err != nil {
		t.Fatalf("logging.New() error = %v", err)
	}
	writer := NewSecurityAuditWriter(nil, logger)
	if err := writer.Append(context.Background(), applicationSecurityAuditEvent()); !errors.Is(err, ErrSecurityAuditWriterInput) {
		t.Fatalf("Append() error = %v, want ErrSecurityAuditWriterInput", err)
	}
	if output.Len() != 0 {
		t.Fatalf("invalid writer emitted log %q", output.String())
	}
}

func applicationSecurityAuditEvent() repository.SecurityAuditEvent {
	return repository.SecurityAuditEvent{
		EventID: "evt_01J00000000000000000000000", OperationID: "op_01J00000000000000000000000",
		Event:        repository.SecurityAuditEventOperationResult,
		Action:       repository.SecurityAuditActionGatewayKeyRotate,
		ActorType:    repository.SecurityAuditActorLocalOperator,
		ResourceType: repository.SecurityAuditResourceGatewayIdentity,
		ResourceID:   "gateway.example.test", Result: repository.SecurityAuditResultSucceeded,
		BeforeStateDigest: make([]byte, 32), AfterStateDigest: make([]byte, 32), OccurredAt: 1,
	}
}
