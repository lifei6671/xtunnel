package identity

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	tests := []struct {
		name    string
		prefix  string
		now     time.Time
		random  *bytes.Reader
		wantErr error
	}{
		{
			name:   "合法 ULID",
			prefix: connectorPrefix,
			now:    time.Date(2026, time.August, 25, 1, 2, 3, 456000000, time.UTC),
			random: bytes.NewReader(bytes.Repeat([]byte{0xAB}, 10)),
		},
		{
			name:    "随机源不足",
			prefix:  connectorPrefix,
			now:     time.Unix(1, 0),
			random:  bytes.NewReader(nil),
			wantErr: io.EOF,
		},
		{
			name:    "纪元前时间",
			prefix:  connectorPrefix,
			now:     time.Unix(-1, 0),
			random:  bytes.NewReader(bytes.Repeat([]byte{0xAB}, 10)),
			wantErr: errTimeOutsideULIDRange,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, err := newID(test.prefix, test.now, test.random)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("newID() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newID() error = %v", err)
			}
			if !ValidConnectorID(id) {
				t.Fatalf("newID() = %q, want con_ ULID", id)
			}
		})
	}
}

func TestConnectorAndSessionIdentity(t *testing.T) {
	serviceID, err := NewServiceID()
	if err != nil || !ValidServiceID(serviceID) {
		t.Fatalf("NewServiceID() = %q, %v, want svc_ ULID", serviceID, err)
	}
	adminID, err := NewAdminID()
	if err != nil || !ValidAdminID(adminID) {
		t.Fatalf("NewAdminID() = %q, %v, want adm_ ULID", adminID, err)
	}
	adminSessionID, err := NewAdminSessionID()
	if err != nil || !ValidAdminSessionID(adminSessionID) {
		t.Fatalf("NewAdminSessionID() = %q, %v, want ads_ ULID", adminSessionID, err)
	}

	connector, err := NewConnector()
	if err != nil {
		t.Fatalf("NewConnector() error = %v", err)
	}
	if !ValidConnectorID(connector.ID()) {
		t.Fatalf("Connector.ID() = %q, want con_ ULID", connector.ID())
	}
	if connector.ID() != connector.ID() {
		t.Fatal("Connector.ID() changed within one process identity")
	}
	restartedConnector, err := NewConnector()
	if err != nil {
		t.Fatalf("NewConnector(restart) error = %v", err)
	}
	if restartedConnector.ID() == connector.ID() {
		t.Fatal("NewConnector(restart) reused the previous process identity")
	}

	firstSession, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID(first) error = %v", err)
	}
	secondSession, err := NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID(second) error = %v", err)
	}
	if !ValidSessionID(firstSession) || !ValidSessionID(secondSession) {
		t.Fatalf("NewSessionID() = %q, %q, want sess_ ULID", firstSession, secondSession)
	}
	if firstSession == secondSession {
		t.Fatal("NewSessionID() generated duplicate Session ID")
	}

	firstWork, err := NewWorkID()
	if err != nil {
		t.Fatalf("NewWorkID(first) error = %v", err)
	}
	secondWork, err := NewWorkID()
	if err != nil {
		t.Fatalf("NewWorkID(second) error = %v", err)
	}
	if firstWork == secondWork || !validID(firstWork, workPrefix) || !validID(secondWork, workPrefix) {
		t.Fatalf("NewWorkID() = %q, %q, want distinct work_ ULIDs", firstWork, secondWork)
	}
	leaseID, err := NewLeaseID()
	if err != nil || !validID(leaseID, leasePrefix) {
		t.Fatalf("NewLeaseID() = %q, %v, want lease_ ULID", leaseID, err)
	}
	connectionID, err := NewConnectionID()
	if err != nil || !validID(connectionID, connectionPrefix) {
		t.Fatalf("NewConnectionID() = %q, %v, want conn_ ULID", connectionID, err)
	}
	drainID, err := NewDrainID()
	if err != nil || !validID(drainID, drainPrefix) {
		t.Fatalf("NewDrainID() = %q, %v, want drain_ ULID", drainID, err)
	}
	auditEventID, err := NewAuditEventID()
	if err != nil || !validID(auditEventID, auditEventPrefix) {
		t.Fatalf("NewAuditEventID() = %q, %v, want evt_ ULID", auditEventID, err)
	}
	operationID, err := NewOperationID()
	if err != nil || !validID(operationID, operationPrefix) {
		t.Fatalf("NewOperationID() = %q, %v, want op_ ULID", operationID, err)
	}
	requestID, err := NewRequestID()
	if err != nil || !validID(requestID, requestPrefix) {
		t.Fatalf("NewRequestID() = %q, %v, want req_ ULID", requestID, err)
	}
}

func TestValidateIDs(t *testing.T) {
	validBody := "01J00000000000000000000000"
	tests := []struct {
		name     string
		validate func(string) error
		value    string
		wantErr  error
	}{
		{name: "合法 Tunnel", validate: ValidateTunnelID, value: "tun_" + validBody},
		{name: "错误 Tunnel 前缀", validate: ValidateTunnelID, value: "con_" + validBody, wantErr: ErrInvalidTunnelID},
		{name: "合法 Service", validate: ValidateServiceID, value: "svc_" + validBody},
		{name: "错误 Service 前缀", validate: ValidateServiceID, value: "tun_" + validBody, wantErr: ErrInvalidServiceID},
		{name: "小写 Service", validate: ValidateServiceID, value: "svc_01j00000000000000000000000", wantErr: ErrInvalidServiceID},
		{name: "Service 首字符超出 ULID 范围", validate: ValidateServiceID, value: "svc_81J00000000000000000000000", wantErr: ErrInvalidServiceID},
		{name: "合法 Connector", validate: ValidateConnectorID, value: "con_" + validBody},
		{name: "错误 Connector 前缀", validate: ValidateConnectorID, value: "sess_" + validBody, wantErr: ErrInvalidConnectorID},
		{name: "小写 Connector", validate: ValidateConnectorID, value: "con_01j00000000000000000000000", wantErr: ErrInvalidConnectorID},
		{name: "非法 Crockford 字符", validate: ValidateConnectorID, value: "con_01I00000000000000000000000", wantErr: ErrInvalidConnectorID},
		{name: "超出 ULID 128 位范围", validate: ValidateConnectorID, value: "con_81J00000000000000000000000", wantErr: ErrInvalidConnectorID},
		{name: "合法 Session", validate: ValidateSessionID, value: "sess_" + validBody},
		{name: "错误 Session 长度", validate: ValidateSessionID, value: "sess_01J0000000000000000000000", wantErr: ErrInvalidSessionID},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.validate(test.value)
			if test.wantErr == nil && err != nil {
				t.Fatalf("validate(%q) error = %v", test.value, err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("validate(%q) error = %v, want %v", test.value, err, test.wantErr)
			}
		})
	}
}
