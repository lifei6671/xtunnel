package recenterror

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOwnerProjectsFixedMessages(t *testing.T) {
	tests := []struct {
		code    Code
		message string
	}{
		{code: CodeTunnelOffline, message: messageTunnelOffline},
		{code: CodeConnectorOffline, message: messageConnectorOffline},
		{code: CodeOriginDown, message: messageOriginDown},
		{code: CodeNoCapacity, message: messageNoCapacity},
		{code: CodeProtocolError, message: messageProtocolError},
	}
	owner := NewOwner()
	requestID := "req_01K4Z3JMESEMR8E7Z8AC9PKYYJ"
	zone := time.FixedZone("UTC+8", 8*60*60)
	base := time.Date(2026, 8, 30, 18, 0, 0, 0, zone)
	for index, test := range tests {
		if err := owner.Publish(Record{
			Code: test.code, OccurredAt: base.Add(time.Duration(index) * time.Second), RequestID: &requestID,
		}); err != nil {
			t.Fatalf("Publish(%s) error = %v", test.code, err)
		}
	}

	items := owner.Snapshot()
	if len(items) != len(tests) {
		t.Fatalf("Snapshot() length = %d, want %d", len(items), len(tests))
	}
	wantMessages := make(map[Code]string, len(tests))
	for _, test := range tests {
		wantMessages[test.code] = test.message
	}
	for _, item := range items {
		if item.Message != wantMessages[item.Code] {
			t.Errorf("Snapshot() item %s message = %q, want %q", item.Code, item.Message, wantMessages[item.Code])
		}
		if item.OccurredAt.Location() != time.UTC {
			t.Errorf("Snapshot() item %s location = %v, want UTC", item.Code, item.OccurredAt.Location())
		}
		if item.RequestID == nil || *item.RequestID != requestID {
			t.Errorf("Snapshot() item %s request ID = %v, want %q", item.Code, item.RequestID, requestID)
		}
	}
}

func TestOwnerRejectsInvalidInputWithoutLeakingSentinel(t *testing.T) {
	const secret = "sensitive-origin-token-sentinel"
	owner := NewOwner()
	if err := owner.Publish(Record{Code: Code(secret), OccurredAt: time.Now()}); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("Publish(invalid code) error = %v, want ErrInvalidCode", err)
	}
	if err := owner.Publish(Record{Code: CodeOriginDown}); !errors.Is(err, ErrInvalidOccurredAt) {
		t.Fatalf("Publish(zero time) error = %v, want ErrInvalidOccurredAt", err)
	}
	items := owner.Snapshot()
	if items == nil || len(items) != 0 {
		t.Fatalf("Snapshot() = %+v, want non-nil empty slice", items)
	}
	if strings.Contains(fmt.Sprint(items), secret) {
		t.Fatal("Snapshot() contains rejected sensitive sentinel")
	}
}

func TestOwnerKeepsLatestPerCodeAndSortsNewestFirst(t *testing.T) {
	owner := NewOwner()
	base := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	publish := func(code Code, occurredAt time.Time) {
		t.Helper()
		if err := owner.Publish(Record{Code: code, OccurredAt: occurredAt}); err != nil {
			t.Fatalf("Publish(%s) error = %v", code, err)
		}
	}

	publish(CodeTunnelOffline, base.Add(time.Minute))
	publish(CodeTunnelOffline, base.Add(5*time.Minute))
	publish(CodeTunnelOffline, base.Add(2*time.Minute))
	publish(CodeConnectorOffline, base.Add(4*time.Minute))
	publish(CodeOriginDown, base.Add(3*time.Minute))
	publish(CodeNoCapacity, base.Add(2*time.Minute))
	publish(CodeProtocolError, base.Add(time.Minute))

	items := owner.Snapshot()
	if len(items) != slotCount {
		t.Fatalf("Snapshot() length = %d, want %d", len(items), slotCount)
	}
	wantCodes := []Code{CodeTunnelOffline, CodeConnectorOffline, CodeOriginDown, CodeNoCapacity, CodeProtocolError}
	for index, wantCode := range wantCodes {
		if items[index].Code != wantCode {
			t.Errorf("Snapshot()[%d].Code = %s, want %s", index, items[index].Code, wantCode)
		}
	}
	if !items[0].OccurredAt.Equal(base.Add(5 * time.Minute)) {
		t.Errorf("Tunnel latest occurred_at = %v, want %v", items[0].OccurredAt, base.Add(5*time.Minute))
	}
}

func TestOwnerSnapshotReturnsIndependentCopy(t *testing.T) {
	owner := NewOwner()
	requestID := "req_01K4Z3JMESEMR8E7Z8AC9PKYYJ"
	if err := owner.Publish(Record{Code: CodeProtocolError, OccurredAt: time.Now(), RequestID: &requestID}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	first := owner.Snapshot()
	first[0].Message = "mutated"
	*first[0].RequestID = "req_mutated"
	first = append(first, Item{Code: CodeOriginDown})

	second := owner.Snapshot()
	if len(second) != 1 || second[0].Message != messageProtocolError {
		t.Fatalf("second Snapshot() = %+v, want independent fixed projection", second)
	}
	if second[0].RequestID == nil || *second[0].RequestID != requestID {
		t.Fatalf("second Snapshot() request ID = %v, want %q", second[0].RequestID, requestID)
	}
}

func TestOwnerPreservesNullRequestID(t *testing.T) {
	owner := NewOwner()
	if err := owner.Publish(Record{Code: CodeNoCapacity, OccurredAt: time.Now()}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	items := owner.Snapshot()
	if len(items) != 1 || items[0].RequestID != nil {
		t.Fatalf("Snapshot() = %+v, want one item with nil RequestID", items)
	}
}

func TestOwnerConcurrentPublishAndSnapshot(t *testing.T) {
	owner := NewOwner()
	codes := []Code{CodeTunnelOffline, CodeConnectorOffline, CodeOriginDown, CodeNoCapacity, CodeProtocolError}
	base := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	const publishesPerCode = 200
	var wait sync.WaitGroup
	for _, code := range codes {
		for sequence := 0; sequence < publishesPerCode; sequence++ {
			wait.Add(1)
			go func(code Code, sequence int) {
				defer wait.Done()
				requestID := fmt.Sprintf("req_%s_%03d", code, sequence)
				if err := owner.Publish(Record{
					Code: code, OccurredAt: base.Add(time.Duration(sequence) * time.Nanosecond), RequestID: &requestID,
				}); err != nil {
					t.Errorf("Publish(%s, %d) error = %v", code, sequence, err)
				}
				_ = owner.Snapshot()
			}(code, sequence)
		}
	}
	wait.Wait()

	items := owner.Snapshot()
	if len(items) != slotCount {
		t.Fatalf("Snapshot() length = %d, want %d", len(items), slotCount)
	}
	wantTime := base.Add((publishesPerCode - 1) * time.Nanosecond)
	for _, item := range items {
		if !item.OccurredAt.Equal(wantTime) {
			t.Errorf("Snapshot() item %s occurred_at = %v, want %v", item.Code, item.OccurredAt, wantTime)
		}
		wantRequestID := fmt.Sprintf("req_%s_%03d", item.Code, publishesPerCode-1)
		if item.RequestID == nil || *item.RequestID != wantRequestID {
			t.Errorf("Snapshot() item %s request ID = %v, want %q", item.Code, item.RequestID, wantRequestID)
		}
	}
}
