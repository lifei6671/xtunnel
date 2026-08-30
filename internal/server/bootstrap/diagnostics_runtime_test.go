package bootstrap

import (
	"errors"
	"testing"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	serverrecenterror "github.com/lifei6671/xtunnel/internal/server/recenterror"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

func TestDashboardCodeForProtocolUsesOnlyFrozenFiveCategories(t *testing.T) {
	tests := []struct {
		code protocolv1.ErrorCode
		want serverrecenterror.Code
		ok   bool
	}{
		{protocolv1.ErrorCode_ERROR_CODE_TUNNEL_OFFLINE, "", false},
		{protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED, serverrecenterror.CodeOriginDown, true},
		{protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT, serverrecenterror.CodeOriginDown, true},
		{protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE, serverrecenterror.CodeOriginDown, true},
		{protocolv1.ErrorCode_ERROR_CODE_ORIGIN_RESET, serverrecenterror.CodeOriginDown, true},
		{protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TLS_ERROR, serverrecenterror.CodeOriginDown, true},
		{protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED, serverrecenterror.CodeNoCapacity, true},
		{protocolv1.ErrorCode_ERROR_CODE_CONNECTOR_BUSY, serverrecenterror.CodeNoCapacity, true},
		{protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED, serverrecenterror.CodeNoCapacity, true},
		{protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED, serverrecenterror.CodeNoCapacity, true},
		{protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR, serverrecenterror.CodeProtocolError, true},
		{protocolv1.ErrorCode_ERROR_CODE_OPEN_DRAINING, "", false},
		{protocolv1.ErrorCode_ERROR_CODE_VERSION_UNSUPPORTED, "", false},
		{protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR, "", false},
		{protocolv1.ErrorCode_ERROR_CODE_OK, "", false},
	}
	for _, test := range tests {
		got, ok := dashboardCodeForProtocol(test.code)
		if got != test.want || ok != test.ok {
			t.Errorf("dashboardCodeForProtocol(%s) = (%q, %t), want (%q, %t)", test.code, got, ok, test.want, test.ok)
		}
	}
}

func TestServerDiagnosticsBridgePublishesFinalOpenWithRealRequestID(t *testing.T) {
	owner := serverrecenterror.NewOwner()
	requestID := "req_01J00000000000000000000000"
	now := time.Date(2026, time.August, 30, 13, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	bridge := &serverDiagnosticsBridge{
		owner: owner, now: func() time.Time { return now },
	}
	bridge.ObserveOpen(protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT, requestID)
	bridge.ObserveOpen(protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED, "req_secret")

	items := owner.Snapshot()
	if len(items) != 2 {
		t.Fatalf("Snapshot() = %+v, want two safe events", items)
	}
	byCode := map[serverrecenterror.Code]serverrecenterror.Item{items[0].Code: items[0], items[1].Code: items[1]}
	origin := byCode[serverrecenterror.CodeOriginDown]
	if origin.Code != serverrecenterror.CodeOriginDown ||
		!origin.OccurredAt.Equal(now.UTC()) || origin.OccurredAt.Location() != time.UTC ||
		origin.RequestID == nil || *origin.RequestID != requestID ||
		byCode[serverrecenterror.CodeNoCapacity].RequestID != nil {
		t.Fatalf("Snapshot() = %+v, want one safe Origin event", items)
	}
}

func TestServerDiagnosticsBridgeFiltersExpectedAndNonCurrentDisconnects(t *testing.T) {
	owner := serverrecenterror.NewOwner()
	now := time.Date(2026, time.August, 30, 5, 0, 0, 0, time.UTC)
	bridge := &serverDiagnosticsBridge{
		owner: owner, now: func() time.Time { return now },
	}
	for _, reason := range []string{"server_shutdown", "tunnel_revoked", "tunnel_deleted", "session_replaced"} {
		bridge.ObserveConnectorLifecycle(serverruntime.ConnectorLifecycleEvent{
			Name: serverruntime.ConnectorEventDisconnected, Reason: reason,
		})
	}
	bridge.ObserveConnectorLifecycle(serverruntime.ConnectorLifecycleEvent{
		Name: serverruntime.ConnectorEventDraining, Reason: "heartbeat_timeout",
	})
	if items := owner.Snapshot(); len(items) != 0 {
		t.Fatalf("expected lifecycle events produced diagnostics: %+v", items)
	}

	bridge.ObserveConnectorLifecycle(serverruntime.ConnectorLifecycleEvent{
		Name: serverruntime.ConnectorEventDisconnected, Reason: "heartbeat_timeout", TunnelBecameOffline: true,
		Snapshot: serverruntime.ConnectorSnapshot{Session: serverruntime.Session{TunnelID: "tun_test"}},
	})
	items := owner.Snapshot()
	if len(items) != 2 || items[0].RequestID != nil || items[1].RequestID != nil {
		t.Fatalf("unexpected disconnect Snapshot() = %+v", items)
	}
	codes := map[serverrecenterror.Code]bool{items[0].Code: true, items[1].Code: true}
	if !codes[serverrecenterror.CodeConnectorOffline] || !codes[serverrecenterror.CodeTunnelOffline] {
		t.Fatalf("unexpected disconnect codes = %+v, want Connector and Tunnel offline", codes)
	}
}

func TestServerDiagnosticsBridgeKeepsTunnelOnlineWhenAnotherConnectorRemains(t *testing.T) {
	owner := serverrecenterror.NewOwner()
	bridge := &serverDiagnosticsBridge{owner: owner, now: time.Now}
	bridge.ObserveConnectorLifecycle(serverruntime.ConnectorLifecycleEvent{
		Name: serverruntime.ConnectorEventDisconnected, Reason: "control_session_closed",
		Snapshot: serverruntime.ConnectorSnapshot{Session: serverruntime.Session{TunnelID: "tun_test"}},
	})
	items := owner.Snapshot()
	if len(items) != 1 || items[0].Code != serverrecenterror.CodeConnectorOffline {
		t.Fatalf("Snapshot() = %+v, want Connector Offline only", items)
	}
}

func TestServerDiagnosticsBridgeIgnoresGracefulDrainingDisconnect(t *testing.T) {
	owner := serverrecenterror.NewOwner()
	bridge := &serverDiagnosticsBridge{owner: owner, now: time.Now}
	bridge.ObserveConnectorLifecycle(serverruntime.ConnectorLifecycleEvent{
		Name: serverruntime.ConnectorEventDisconnected, Reason: "control_session_closed",
		Snapshot: serverruntime.ConnectorSnapshot{
			Status: serverruntime.ConnectorStatusDraining,
		},
		WasDraining:         true,
		TunnelBecameOffline: true,
	})
	if items := owner.Snapshot(); len(items) != 0 {
		t.Fatalf("graceful draining disconnect produced diagnostics: %+v", items)
	}
}

func TestServerDiagnosticsBridgeIgnoresDrainingActiveWorkTombstone(t *testing.T) {
	owner := serverrecenterror.NewOwner()
	bridge := &serverDiagnosticsBridge{owner: owner, now: time.Now}
	bridge.ObserveConnectorLifecycle(serverruntime.ConnectorLifecycleEvent{
		Name: serverruntime.ConnectorEventDisconnected, Reason: "control_session_closed",
		Snapshot: serverruntime.ConnectorSnapshot{
			Tombstone: true, ActiveWork: 1,
		},
		WasDraining:         true,
		TunnelBecameOffline: true,
	})
	if items := owner.Snapshot(); len(items) != 0 {
		t.Fatalf("Draining Active Work Tombstone produced diagnostics: %+v", items)
	}
}

func TestServerDiagnosticsBridgeReportsImpossibleProjectionFailure(t *testing.T) {
	want := errors.New("projection failed")
	reported := make(chan error, 1)
	bridge := &serverDiagnosticsBridge{owner: serverrecenterror.NewOwner(), reportError: func(err error) { reported <- err }}
	bridge.publish(serverrecenterror.Record{Code: serverrecenterror.Code(want.Error()), OccurredAt: time.Now()})
	select {
	case err := <-reported:
		if !errors.Is(err, serverrecenterror.ErrInvalidCode) {
			t.Fatalf("reported error = %v, want ErrInvalidCode", err)
		}
	default:
		t.Fatal("invalid internal projection was not reported")
	}
}
