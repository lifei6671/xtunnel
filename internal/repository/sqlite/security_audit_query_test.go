package sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/lifei6671/xtunnel/internal/repository"
)

func TestQuerySecurityAuditEventsFiltersAndTimeBoundaries(t *testing.T) {
	store := openSecurityAuditQueryStore(t)
	events := securityAuditQueryFixtures()
	seedSecurityAuditQueryEvents(t, store, events)
	from200, to400, from400, to401 := int64(200), int64(400), int64(400), int64(401)

	tests := []struct {
		name  string
		query repository.SecurityAuditEventQuery
		want  []string
	}{
		{
			name:  "action",
			query: repository.SecurityAuditEventQuery{Action: repository.SecurityAuditActionTokenRotate, Limit: 10},
			want:  []string{events[2].EventID},
		},
		{
			name:  "result",
			query: repository.SecurityAuditEventQuery{Result: repository.SecurityAuditResultFailed, Limit: 10},
			want:  []string{events[4].EventID, events[2].EventID},
		},
		{
			name: "resource type",
			query: repository.SecurityAuditEventQuery{
				ResourceType: repository.SecurityAuditResourceTunnelToken,
				Limit:        10,
			},
			want: []string{events[4].EventID, events[2].EventID, events[1].EventID},
		},
		{
			name:  "resource id",
			query: repository.SecurityAuditEventQuery{ResourceID: events[1].ResourceID, Limit: 10},
			want:  []string{events[2].EventID, events[1].EventID},
		},
		{
			name: "from inclusive and to exclusive",
			query: repository.SecurityAuditEventQuery{
				OccurredFrom: &from200,
				OccurredTo:   &to400,
				Limit:        10,
			},
			want: []string{events[3].EventID, events[2].EventID, events[1].EventID},
		},
		{
			name: "combined filters",
			query: repository.SecurityAuditEventQuery{
				Action:       repository.SecurityAuditActionTokenRevoke,
				Result:       repository.SecurityAuditResultFailed,
				ResourceType: repository.SecurityAuditResourceTunnelToken,
				ResourceID:   events[4].ResourceID,
				OccurredFrom: &from400,
				OccurredTo:   &to401,
				Limit:        10,
			},
			want: []string{events[4].EventID},
		},
		{
			name:  "no results",
			query: repository.SecurityAuditEventQuery{ResourceID: "missing.example.test", Limit: 10},
			want:  []string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page, err := store.QuerySecurityAuditEvents(context.Background(), test.query)
			if err != nil {
				t.Fatalf("QuerySecurityAuditEvents() error = %v", err)
			}
			if page.Next != nil {
				t.Fatalf("Next = %#v, want nil", page.Next)
			}
			if got := securityAuditEventIDs(page.Events); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("event IDs = %#v, want %#v", got, test.want)
			}
			if page.Events == nil {
				t.Fatal("Events = nil, want a stable empty slice")
			}
		})
	}
}

func TestQuerySecurityAuditEventsUsesStableDescendingKeyset(t *testing.T) {
	store := openSecurityAuditQueryStore(t)
	events := securityAuditQueryFixtures()
	seedSecurityAuditQueryEvents(t, store, events)

	wantPages := [][]string{
		{events[4].EventID, events[3].EventID},
		{events[2].EventID, events[1].EventID},
		{events[0].EventID},
	}
	var after *repository.SecurityAuditEventCursor
	seen := make(map[string]bool, len(events))
	for index, want := range wantPages {
		page, err := store.QuerySecurityAuditEvents(context.Background(), repository.SecurityAuditEventQuery{
			After: after,
			Limit: 2,
		})
		if err != nil {
			t.Fatalf("page %d QuerySecurityAuditEvents() error = %v", index+1, err)
		}
		got := securityAuditEventIDs(page.Events)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("page %d IDs = %#v, want %#v", index+1, got, want)
		}
		for _, eventID := range got {
			if seen[eventID] {
				t.Fatalf("page %d repeated event %q", index+1, eventID)
			}
			seen[eventID] = true
		}
		if index < len(wantPages)-1 {
			last := page.Events[len(page.Events)-1]
			if page.Next == nil || page.Next.OccurredAt != last.OccurredAt || page.Next.EventID != last.EventID {
				t.Fatalf("page %d Next = %#v, want key of %#v", index+1, page.Next, last)
			}
		} else if page.Next != nil {
			t.Fatalf("last page Next = %#v, want nil", page.Next)
		}
		after = page.Next
	}
	if len(seen) != len(events) {
		t.Fatalf("unique events across pages = %d, want %d", len(seen), len(events))
	}
}

func TestQuerySecurityAuditEventsAppliesFiltersToEveryKeysetBranch(t *testing.T) {
	store := openSecurityAuditQueryStore(t)
	events := securityAuditQueryFixtures()
	seedSecurityAuditQueryEvents(t, store, events)

	first, err := store.QuerySecurityAuditEvents(context.Background(), repository.SecurityAuditEventQuery{
		Result: repository.SecurityAuditResultFailed,
		Limit:  1,
	})
	if err != nil {
		t.Fatalf("first QuerySecurityAuditEvents() error = %v", err)
	}
	if got, want := securityAuditEventIDs(first.Events), []string{events[4].EventID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first page IDs = %#v, want %#v", got, want)
	}
	if first.Next == nil {
		t.Fatal("first page Next = nil, want continuation key")
	}

	second, err := store.QuerySecurityAuditEvents(context.Background(), repository.SecurityAuditEventQuery{
		Result: repository.SecurityAuditResultFailed,
		After:  first.Next,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("second QuerySecurityAuditEvents() error = %v", err)
	}
	if got, want := securityAuditEventIDs(second.Events), []string{events[2].EventID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second page IDs = %#v, want %#v", got, want)
	}
}

func TestQuerySecurityAuditEventsRejectsInvalidBoundaries(t *testing.T) {
	store := openSecurityAuditQueryStore(t)
	zero, one, two := int64(0), int64(1), int64(2)
	tests := []struct {
		name  string
		query repository.SecurityAuditEventQuery
	}{
		{name: "zero limit", query: repository.SecurityAuditEventQuery{}},
		{name: "limit above maximum", query: repository.SecurityAuditEventQuery{Limit: 201}},
		{name: "unknown action", query: repository.SecurityAuditEventQuery{Action: "UNKNOWN", Limit: 1}},
		{name: "unknown result", query: repository.SecurityAuditEventQuery{Result: "UNKNOWN", Limit: 1}},
		{name: "unknown resource type", query: repository.SecurityAuditEventQuery{ResourceType: "UNKNOWN", Limit: 1}},
		{name: "long resource id", query: repository.SecurityAuditEventQuery{ResourceID: strings.Repeat("x", 257), Limit: 1}},
		{name: "equal time bounds", query: repository.SecurityAuditEventQuery{OccurredFrom: &one, OccurredTo: &one, Limit: 1}},
		{name: "reversed time bounds", query: repository.SecurityAuditEventQuery{OccurredFrom: &two, OccurredTo: &one, Limit: 1}},
		{name: "zero cursor time", query: repository.SecurityAuditEventQuery{
			After: &repository.SecurityAuditEventCursor{EventID: sqliteAuditEventID}, Limit: 1,
		}},
		{name: "invalid cursor event id", query: repository.SecurityAuditEventQuery{
			After: &repository.SecurityAuditEventCursor{OccurredAt: 1, EventID: "invalid"}, Limit: 1,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.QuerySecurityAuditEvents(context.Background(), test.query)
			if !errors.Is(err, repository.ErrInvalidSecurityAuditEventQuery) {
				t.Fatalf("QuerySecurityAuditEvents() error = %v, want ErrInvalidSecurityAuditEventQuery", err)
			}
		})
	}
	if _, err := store.QuerySecurityAuditEvents(nil, repository.SecurityAuditEventQuery{Limit: 1}); !errors.Is(err, repository.ErrInvalidSecurityAuditEventQuery) {
		t.Fatalf("nil context error = %v, want ErrInvalidSecurityAuditEventQuery", err)
	}
	negative := int64(-1)
	for _, query := range []repository.SecurityAuditEventQuery{
		{ResourceID: " spaced ", Limit: 1},
		{OccurredFrom: &zero, Limit: 1},
		{OccurredTo: &zero, Limit: 1},
		{OccurredFrom: &negative, Limit: 1},
	} {
		if _, err := store.QuerySecurityAuditEvents(context.Background(), query); err != nil {
			t.Fatalf("valid query %#v error = %v", query, err)
		}
	}
}

func TestQuerySecurityAuditEventsPropagatesCancellationAndDatabaseErrors(t *testing.T) {
	t.Run("canceled context", func(t *testing.T) {
		store := openSecurityAuditQueryStore(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := store.QuerySecurityAuditEvents(ctx, repository.SecurityAuditEventQuery{Limit: 1})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("QuerySecurityAuditEvents() error = %v, want context.Canceled", err)
		}
	})

	t.Run("database query failure", func(t *testing.T) {
		store := openSecurityAuditQueryStore(t)
		queryErr := errors.New("injected security audit query failure")
		if err := store.database.Callback().Query().Before("gorm:query").Register(
			"test:fail-security-audit-query",
			func(database *gorm.DB) { database.AddError(queryErr) },
		); err != nil {
			t.Fatalf("register query callback error = %v", err)
		}
		_, err := store.QuerySecurityAuditEvents(context.Background(), repository.SecurityAuditEventQuery{Limit: 1})
		if !errors.Is(err, queryErr) {
			t.Fatalf("QuerySecurityAuditEvents() error = %v, want injected error", err)
		}
	})
}

func TestQuerySecurityAuditEventsReturnsIndependentDigests(t *testing.T) {
	store := openSecurityAuditQueryStore(t)
	event := securityAuditQueryFixtures()[0]
	event.BeforeStateDigest = []byte(strings.Repeat("a", 32))
	event.AfterStateDigest = []byte(strings.Repeat("b", 32))
	seedSecurityAuditQueryEvents(t, store, []repository.SecurityAuditEvent{event})

	first, err := store.QuerySecurityAuditEvents(context.Background(), repository.SecurityAuditEventQuery{Limit: 1})
	if err != nil {
		t.Fatalf("first QuerySecurityAuditEvents() error = %v", err)
	}
	first.Events[0].BeforeStateDigest[0] = 'z'
	second, err := store.QuerySecurityAuditEvents(context.Background(), repository.SecurityAuditEventQuery{Limit: 1})
	if err != nil {
		t.Fatalf("second QuerySecurityAuditEvents() error = %v", err)
	}
	if second.Events[0].BeforeStateDigest[0] != 'a' || second.Events[0].AfterStateDigest[0] != 'b' {
		t.Fatalf("persisted digests changed after caller mutation: before=%q after=%q",
			second.Events[0].BeforeStateDigest, second.Events[0].AfterStateDigest)
	}
}

func openSecurityAuditQueryStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	return store
}

func seedSecurityAuditQueryEvents(t *testing.T, store *Store, events []repository.SecurityAuditEvent) {
	t.Helper()
	if err := store.WithTx(context.Background(), func(transaction repository.TxStore) error {
		for _, event := range events {
			if err := transaction.SecurityAuditEvents().Append(context.Background(), event); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed security audit events error = %v", err)
	}
}

func securityAuditQueryFixtures() []repository.SecurityAuditEvent {
	firstTunnelID := "tun_01J00000000000000000000001"
	secondTunnelID := "tun_01J00000000000000000000002"
	adminID := "adm_01J00000000000000000000000"
	events := []repository.SecurityAuditEvent{
		securityAuditQueryGatewayEvent('0', 100, "gateway-a"),
		securityAuditQueryAdminEvent('1', 200, repository.SecurityAuditActionTokenReveal,
			repository.SecurityAuditResourceTunnelToken, firstTunnelID, repository.SecurityAuditResultSucceeded),
		securityAuditQueryAdminEvent('2', 200, repository.SecurityAuditActionTokenRotate,
			repository.SecurityAuditResourceTunnelToken, firstTunnelID, repository.SecurityAuditResultFailed),
		securityAuditQueryAdminEvent('3', 300, repository.SecurityAuditActionTunnelRevoke,
			repository.SecurityAuditResourceTunnel, secondTunnelID, repository.SecurityAuditResultSucceeded),
		securityAuditQueryAdminEvent('4', 400, repository.SecurityAuditActionTokenRevoke,
			repository.SecurityAuditResourceTunnelToken, secondTunnelID, repository.SecurityAuditResultFailed),
	}
	for index := 1; index < len(events); index++ {
		events[index].ActorID = adminID
	}
	return events
}

func securityAuditQueryGatewayEvent(suffix byte, occurredAt int64, resourceID string) repository.SecurityAuditEvent {
	return repository.SecurityAuditEvent{
		EventID:      securityAuditQueryID("evt_", suffix),
		OperationID:  securityAuditQueryID("op_", suffix),
		Event:        repository.SecurityAuditEventOperationResult,
		Action:       repository.SecurityAuditActionGatewayKeyRotate,
		ActorType:    repository.SecurityAuditActorLocalOperator,
		ResourceType: repository.SecurityAuditResourceGatewayIdentity,
		ResourceID:   resourceID,
		Result:       repository.SecurityAuditResultSucceeded,
		OccurredAt:   occurredAt,
	}
}

func securityAuditQueryAdminEvent(
	suffix byte,
	occurredAt int64,
	action string,
	resourceType string,
	resourceID string,
	result string,
) repository.SecurityAuditEvent {
	event := repository.SecurityAuditEvent{
		EventID:      securityAuditQueryID("evt_", suffix),
		OperationID:  securityAuditQueryID("op_", suffix),
		Event:        repository.SecurityAuditEventOperationResult,
		Action:       action,
		ActorType:    repository.SecurityAuditActorAdmin,
		SourceIP:     "127.0.0.1",
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Result:       result,
		RequestID:    "req-1",
		TraceID:      "trace-1",
		OccurredAt:   occurredAt,
	}
	if result == repository.SecurityAuditResultFailed {
		event.ErrorCode = "TEST_FAILURE"
	}
	return event
}

func securityAuditQueryID(prefix string, suffix byte) string {
	return prefix + "01J0000000000000000000000" + string(suffix)
}

func securityAuditEventIDs(events []repository.SecurityAuditEvent) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.EventID)
	}
	return ids
}
