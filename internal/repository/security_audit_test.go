package repository

import (
	"errors"
	"strings"
	"testing"
)

const (
	testAuditEventID = "evt_01J00000000000000000000000"
	testOperationID  = "op_01J00000000000000000000000"
)

func TestSecurityAuditEventValidate(t *testing.T) {
	valid := testSecurityAuditEvent()
	tests := []struct {
		name   string
		mutate func(*SecurityAuditEvent)
	}{
		{name: "event ID", mutate: func(event *SecurityAuditEvent) { event.EventID = "evt_invalid" }},
		{name: "operation ID", mutate: func(event *SecurityAuditEvent) { event.OperationID = "op_invalid" }},
		{name: "event enum", mutate: func(event *SecurityAuditEvent) { event.Event = "UNKNOWN" }},
		{name: "action enum", mutate: func(event *SecurityAuditEvent) { event.Action = "UNKNOWN" }},
		{name: "actor enum", mutate: func(event *SecurityAuditEvent) { event.ActorType = "REMOTE" }},
		{name: "actor whitespace", mutate: func(event *SecurityAuditEvent) { event.ActorID = " operator " }},
		{name: "offline actor ID", mutate: func(event *SecurityAuditEvent) { event.ActorID = "operator" }},
		{name: "offline source IP", mutate: func(event *SecurityAuditEvent) { event.SourceIP = "127.0.0.1" }},
		{name: "resource enum", mutate: func(event *SecurityAuditEvent) { event.ResourceType = "TOKEN" }},
		{name: "resource empty", mutate: func(event *SecurityAuditEvent) { event.ResourceID = "" }},
		{name: "resource too long", mutate: func(event *SecurityAuditEvent) { event.ResourceID = strings.Repeat("x", maxAuditResourceIDBytes+1) }},
		{name: "success error code", mutate: func(event *SecurityAuditEvent) { event.ErrorCode = "UNEXPECTED" }},
		{name: "digest length", mutate: func(event *SecurityAuditEvent) { event.AfterStateDigest = []byte{1} }},
		{name: "occurred at", mutate: func(event *SecurityAuditEvent) { event.OccurredAt = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := valid
			event.BeforeStateDigest = append([]byte(nil), valid.BeforeStateDigest...)
			event.AfterStateDigest = append([]byte(nil), valid.AfterStateDigest...)
			test.mutate(&event)
			if err := event.Validate(); !errors.Is(err, ErrInvalidSecurityAuditEvent) {
				t.Fatalf("Validate() error = %v, want ErrInvalidSecurityAuditEvent", err)
			}
		})
	}

	failed := valid
	failed.Result = SecurityAuditResultFailed
	failed.ErrorCode = "GATEWAY_ROTATION_FAILED"
	if err := failed.Validate(); err != nil {
		t.Fatalf("failed event Validate() error = %v", err)
	}
	failed.ErrorCode = ""
	if err := failed.Validate(); !errors.Is(err, ErrInvalidSecurityAuditEvent) {
		t.Fatalf("failed event without error code = %v", err)
	}
}

func testSecurityAuditEvent() SecurityAuditEvent {
	return SecurityAuditEvent{
		EventID:           testAuditEventID,
		OperationID:       testOperationID,
		Event:             SecurityAuditEventOperationResult,
		Action:            SecurityAuditActionGatewayKeyRotate,
		ActorType:         SecurityAuditActorLocalOperator,
		ResourceType:      SecurityAuditResourceGatewayIdentity,
		ResourceID:        "gateway.example.test",
		Result:            SecurityAuditResultSucceeded,
		BeforeStateDigest: make([]byte, auditDigestBytes),
		AfterStateDigest:  make([]byte, auditDigestBytes),
		OccurredAt:        1,
	}
}
