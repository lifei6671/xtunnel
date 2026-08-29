package application

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lifei6671/xtunnel/internal/repository"
)

func TestSecurityAuditQueryServiceDelegatesReadOnlyQuery(t *testing.T) {
	wantEvent := repository.SecurityAuditEvent{
		EventID:      "evt_01J00000000000000000000000",
		OperationID:  "op_01J00000000000000000000000",
		Event:        repository.SecurityAuditEventOperationResult,
		Action:       repository.SecurityAuditActionGatewayKeyRotate,
		ActorType:    repository.SecurityAuditActorLocalOperator,
		ResourceType: repository.SecurityAuditResourceGatewayIdentity,
		ResourceID:   "gateway.example.test",
		Result:       repository.SecurityAuditResultSucceeded,
		OccurredAt:   10,
	}
	wantPage := repository.SecurityAuditEventPage{
		Events: []repository.SecurityAuditEvent{wantEvent},
		Next: &repository.SecurityAuditEventCursor{
			OccurredAt: wantEvent.OccurredAt,
			EventID:    wantEvent.EventID,
		},
	}
	store := &securityAuditQueryStoreStub{page: wantPage}
	service := NewSecurityAuditQueryService(store)
	query := repository.SecurityAuditEventQuery{
		Action: repository.SecurityAuditActionGatewayKeyRotate,
		Limit:  50,
	}

	got, err := service.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if store.calls != 1 || !reflect.DeepEqual(store.query, query) {
		t.Fatalf("store calls = %d, query = %#v, want one call with %#v", store.calls, store.query, query)
	}
	if !reflect.DeepEqual(got, wantPage) {
		t.Fatalf("Query() page = %#v, want %#v", got, wantPage)
	}
}

func TestSecurityAuditQueryServiceRejectsInvalidInputBeforeStore(t *testing.T) {
	validQuery := repository.SecurityAuditEventQuery{Limit: 1}
	tests := []struct {
		name    string
		service *SecurityAuditQueryService
		ctx     context.Context
		query   repository.SecurityAuditEventQuery
		wantErr error
	}{
		{
			name:    "nil service",
			service: nil,
			ctx:     context.Background(),
			query:   validQuery,
			wantErr: ErrSecurityAuditQueryServiceInput,
		},
		{
			name:    "nil store",
			service: NewSecurityAuditQueryService(nil),
			ctx:     context.Background(),
			query:   validQuery,
			wantErr: ErrSecurityAuditQueryServiceInput,
		},
		{
			name:    "nil context",
			service: NewSecurityAuditQueryService(&securityAuditQueryStoreStub{}),
			query:   validQuery,
			wantErr: ErrSecurityAuditQueryServiceInput,
		},
		{
			name:    "invalid repository query",
			service: NewSecurityAuditQueryService(&securityAuditQueryStoreStub{}),
			ctx:     context.Background(),
			query:   repository.SecurityAuditEventQuery{},
			wantErr: repository.ErrInvalidSecurityAuditEventQuery,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.service.Query(test.ctx, test.query)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Query() error = %v, want %v", err, test.wantErr)
			}
			if test.service != nil {
				if store, ok := test.service.store.(*securityAuditQueryStoreStub); ok && store.calls != 0 {
					t.Fatalf("store calls = %d, want 0", store.calls)
				}
			}
		})
	}
}

func TestSecurityAuditQueryServicePreservesStoreErrorCause(t *testing.T) {
	queryErr := errors.New("injected query failure")
	store := &securityAuditQueryStoreStub{err: queryErr}
	service := NewSecurityAuditQueryService(store)
	_, err := service.Query(context.Background(), repository.SecurityAuditEventQuery{Limit: 1})
	if !errors.Is(err, queryErr) {
		t.Fatalf("Query() error = %v, want injected cause", err)
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d, want 1", store.calls)
	}
}

type securityAuditQueryStoreStub struct {
	page  repository.SecurityAuditEventPage
	err   error
	query repository.SecurityAuditEventQuery
	calls int
}

func (store *securityAuditQueryStoreStub) QuerySecurityAuditEvents(
	_ context.Context,
	query repository.SecurityAuditEventQuery,
) (repository.SecurityAuditEventPage, error) {
	store.calls++
	store.query = query
	return store.page, store.err
}
