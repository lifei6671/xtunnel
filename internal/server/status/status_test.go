package status

import (
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/repository"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

func TestCalculateTunnel(t *testing.T) {
	tests := []struct {
		name  string
		input TunnelInput
		want  TunnelStatus
	}{
		{name: "revoked 优先于当前 Connector", input: TunnelInput{Revoked: true, EverAuthenticated: true, ConnectorStatuses: []ConnectorStatus{ConnectorStatusOnline}}, want: TunnelStatusRevoked},
		{name: "从未认证保持 pending", input: TunnelInput{}, want: TunnelStatusPending},
		{name: "曾认证但无当前 Connector 为 offline", input: TunnelInput{EverAuthenticated: true}, want: TunnelStatusOffline},
		{name: "当前 Connector 本身证明已经认证", input: TunnelInput{ConnectorStatuses: []ConnectorStatus{ConnectorStatusOnline}}, want: TunnelStatusOnline},
		{name: "任一 Connector online 则 online", input: TunnelInput{EverAuthenticated: true, ConnectorStatuses: []ConnectorStatus{ConnectorStatusDegraded, ConnectorStatusOnline, ConnectorStatusDraining}}, want: TunnelStatusOnline},
		{name: "全部 Connector 不可接受新 Work 则 degraded", input: TunnelInput{EverAuthenticated: true, ConnectorStatuses: []ConnectorStatus{ConnectorStatusDegraded, ConnectorStatusDraining}}, want: TunnelStatusDegraded},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CalculateTunnel(test.input); got != test.want {
				t.Fatalf("CalculateTunnel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTunnelInputFromRepositoryUsesDurableAuthenticationFact(t *testing.T) {
	authenticatedAt := int64(10)
	revokedAt := int64(20)
	tests := []struct {
		name   string
		tunnel repository.Tunnel
		want   TunnelStatus
	}{
		{name: "未认证", tunnel: repository.Tunnel{}, want: TunnelStatusPending},
		{name: "重启后无 Runtime", tunnel: repository.Tunnel{FirstAuthenticatedAt: &authenticatedAt}, want: TunnelStatusOffline},
		{name: "撤销优先", tunnel: repository.Tunnel{FirstAuthenticatedAt: &authenticatedAt, RevokedAt: &revokedAt}, want: TunnelStatusRevoked},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CalculateTunnel(TunnelInputFromRepository(test.tunnel, nil)); got != test.want {
				t.Fatalf("CalculateTunnel(TunnelInputFromRepository()) = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCalculateConnector(t *testing.T) {
	tests := []struct {
		name        string
		input       ConnectorInput
		want        ConnectorStatus
		wantPresent bool
	}{
		{name: "Control Session 关闭后无状态", input: ConnectorInput{HeartbeatFresh: true, TransportAcceptsWork: true}, wantPresent: false},
		{name: "Heartbeat 过期后无状态", input: ConnectorInput{CurrentControlSession: true, TransportAcceptsWork: true}, wantPresent: false},
		{name: "Drain 优先于 Transport", input: ConnectorInput{CurrentControlSession: true, HeartbeatFresh: true, ConfigReady: true, Draining: true, TransportAcceptsWork: true}, want: ConnectorStatusDraining, wantPresent: true},
		{name: "首次配置未完成时为 degraded", input: ConnectorInput{CurrentControlSession: true, HeartbeatFresh: true, TransportAcceptsWork: true}, want: ConnectorStatusDegraded, wantPresent: true},
		{name: "配置就绪且 Transport 可接受新 Work 为 online", input: ConnectorInput{CurrentControlSession: true, HeartbeatFresh: true, ConfigReady: true, TransportAcceptsWork: true}, want: ConnectorStatusOnline, wantPresent: true},
		{name: "Transport 持续不可接受新 Work 为 degraded", input: ConnectorInput{CurrentControlSession: true, HeartbeatFresh: true}, want: ConnectorStatusDegraded, wantPresent: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, present := CalculateConnector(test.input)
			if got != test.want || present != test.wantPresent {
				t.Fatalf("CalculateConnector() = (%q, %t), want (%q, %t)", got, present, test.want, test.wantPresent)
			}
		})
	}
}

func TestCalculateServicePriority(t *testing.T) {
	ready := ServiceInput{
		Enabled:          true,
		RequiredRevision: 7,
		HealthEnabled:    true,
		Connectors: []ServiceConnector{{
			Current: true, ControlLive: true, HeartbeatFresh: true, ConfigReady: true,
			HasObserved: true, ObservedRevision: 7, HealthRevision: 7, HealthHealthy: true,
			HealthFresh: true, TransportAcceptsWork: true, CapacityAvailable: true,
		}},
	}
	current := ServiceConnector{Current: true, ControlLive: true, HeartbeatFresh: true}
	synced := current
	synced.ConfigReady = true
	synced.HasObserved = true
	synced.ObservedRevision = 7
	healthy := synced
	healthy.HealthRevision = 7
	healthy.HealthHealthy = true
	healthy.HealthFresh = true
	available := synced
	available.TransportAcceptsWork = true
	available.CapacityAvailable = true
	tests := []struct {
		name  string
		input ServiceInput
		want  ServiceStatus
	}{
		{name: "disabled 覆盖全部失败", input: ServiceInput{ApplyFailure: &ApplyFailure{}}, want: ServiceStatusDisabled},
		{name: "当前 apply failed 覆盖 Tunnel 离线", input: ServiceInput{Enabled: true, RequiredRevision: 7, ApplyFailure: &ApplyFailure{RequiredRevision: 7, ErrorCode: "LISTEN_FAILED", FailedAt: time.Unix(1, 0)}}, want: ServiceStatusApplyFailed},
		{name: "旧 apply failed 不覆盖 Tunnel 离线", input: ServiceInput{Enabled: true, RequiredRevision: 8, ApplyFailure: &ApplyFailure{RequiredRevision: 7}}, want: ServiceStatusTunnelOffline},
		{name: "Tunnel 离线覆盖配置未同步", input: ServiceInput{Enabled: true}, want: ServiceStatusTunnelOffline},
		{name: "Tombstone 不算已连接", input: ServiceInput{Enabled: true, Connectors: []ServiceConnector{{Current: true, Tombstone: true, ControlLive: true, HeartbeatFresh: true}}}, want: ServiceStatusTunnelOffline},
		{name: "首次配置未完成时 syncing", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{current}}, want: ServiceStatusConfigSyncing},
		{name: "配置未同步覆盖 Origin 不健康", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{{Current: true, ControlLive: true, HeartbeatFresh: true, ConfigReady: true, HasObserved: true, ObservedRevision: 6}}}, want: ServiceStatusConfigSyncing},
		{name: "旧 Revision Health 不覆盖配置同步", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{{Current: true, ControlLive: true, HeartbeatFresh: true, ConfigReady: true, HasObserved: true, ObservedRevision: 6, HealthRevision: 7, HealthHealthy: true, HealthFresh: true, TransportAcceptsWork: true, CapacityAvailable: true}}}, want: ServiceStatusConfigSyncing},
		{name: "Origin 不健康覆盖无容量", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{synced}}, want: ServiceStatusOriginUnhealthy},
		{name: "Health Revision 必须精确匹配", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{{Current: true, ControlLive: true, HeartbeatFresh: true, ConfigReady: true, HasObserved: true, ObservedRevision: 8, HealthRevision: 6, HealthHealthy: true, HealthFresh: true, TransportAcceptsWork: true, CapacityAvailable: true}}}, want: ServiceStatusOriginUnhealthy},
		{name: "过期 Health 不可放行", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{{Current: true, ControlLive: true, HeartbeatFresh: true, ConfigReady: true, HasObserved: true, ObservedRevision: 7, HealthRevision: 7, HealthHealthy: true, TransportAcceptsWork: true, CapacityAvailable: true}}}, want: ServiceStatusOriginUnhealthy},
		{name: "Health disabled 跳过 Origin 状态", input: ServiceInput{Enabled: true, RequiredRevision: 7, Connectors: []ServiceConnector{synced}}, want: ServiceStatusNoCapacity},
		{name: "Health disabled 且有容量即可 READY", input: ServiceInput{Enabled: true, RequiredRevision: 7, Connectors: []ServiceConnector{available}}, want: ServiceStatusReady},
		{name: "无运行容量", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{healthy}}, want: ServiceStatusNoCapacity},
		{name: "Draining 即使有 Capacity 也不可 READY", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{func() ServiceConnector {
			connector := healthy
			connector.Draining = true
			connector.TransportAcceptsWork = true
			connector.CapacityAvailable = true
			return connector
		}()}}, want: ServiceStatusNoCapacity},
		{name: "不同 Connector 的健康与容量不能拼成 READY", input: ServiceInput{Enabled: true, RequiredRevision: 7, HealthEnabled: true, Connectors: []ServiceConnector{
			func() ServiceConnector { connector := healthy; return connector }(),
			func() ServiceConnector {
				connector := synced
				connector.TransportAcceptsWork = true
				connector.CapacityAvailable = true
				return connector
			}(),
		}}, want: ServiceStatusNoCapacity},
		{name: "全部门禁通过", input: ready, want: ServiceStatusReady},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CalculateService(test.input); got != test.want {
				t.Fatalf("CalculateService() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOriginHealthDoesNotAffectTunnelOrConnector(t *testing.T) {
	connector, present := CalculateConnector(ConnectorInput{
		CurrentControlSession: true,
		HeartbeatFresh:        true,
		ConfigReady:           true,
		TransportAcceptsWork:  true,
	})
	if !present || connector != ConnectorStatusOnline {
		t.Fatalf("CalculateConnector() = (%q, %t), want (%q, true)", connector, present, ConnectorStatusOnline)
	}
	if got := CalculateTunnel(TunnelInput{
		EverAuthenticated: true,
		ConnectorStatuses: []ConnectorStatus{connector},
	}); got != TunnelStatusOnline {
		t.Fatalf("CalculateTunnel() = %q, want %q", got, TunnelStatusOnline)
	}
	if got := CalculateService(ServiceInput{
		Enabled: true, RequiredRevision: 7, HealthEnabled: true,
		Connectors: []ServiceConnector{{Current: true, ControlLive: true, HeartbeatFresh: true, ConfigReady: true, HasObserved: true, ObservedRevision: 7, TransportAcceptsWork: true, CapacityAvailable: true}},
	}); got != ServiceStatusOriginUnhealthy {
		t.Fatalf("CalculateService() = %q, want %q", got, ServiceStatusOriginUnhealthy)
	}
}

func TestRuntimeSnapshotConversionsPreserveConnectorIdentity(t *testing.T) {
	now := time.Unix(100, 0)
	snapshot := serverruntime.SessionStatusSnapshot{
		CurrentControlSession: true,
		HeartbeatFresh:        true,
		Config: serverruntime.SessionEligibility{
			ConfigReady: true, HasObserved: true, ObservedRevision: 7,
			Services: map[string]serverruntime.ServiceEligibility{
				"svc_current": {
					RequiredRevision: 7, Enabled: true, HealthRevision: 7,
					HealthHealthy: true, HealthyUntil: now.Add(time.Second),
				},
			},
		},
		WorkPool: serverruntime.ConnectorWorkPoolSnapshot{Idle: 1, Total: 1},
	}
	connector, present := CalculateConnector(ConnectorInputFromRuntime(snapshot))
	if !present || connector != ConnectorStatusOnline {
		t.Fatalf("runtime Connector status = (%q, %t), want ONLINE", connector, present)
	}
	service := ServiceConnectorFromRuntime(snapshot, "svc_current", 7, now)
	if !service.Current || !service.ConfigReady || !service.HasObserved ||
		!service.HealthHealthy || !service.HealthFresh || !service.CapacityAvailable {
		t.Fatalf("runtime Service connector = %#v", service)
	}
	if got := CalculateService(ServiceInput{
		Enabled: true, RequiredRevision: 7, HealthEnabled: true,
		Connectors: []ServiceConnector{service},
	}); got != ServiceStatusReady {
		t.Fatalf("CalculateService(runtime snapshot) = %q, want READY", got)
	}

	wrongRevision := ServiceConnectorFromRuntime(snapshot, "svc_current", 8, now)
	if wrongRevision.HasObserved || wrongRevision.HealthHealthy || wrongRevision.HealthFresh {
		t.Fatalf("different RequiredRevision inherited status: %#v", wrongRevision)
	}
	if got := CalculateService(ServiceInput{
		Enabled: true, RequiredRevision: 8, HealthEnabled: true,
		Connectors: []ServiceConnector{wrongRevision},
	}); got != ServiceStatusConfigSyncing {
		t.Fatalf("CalculateService(different required revision) = %q, want CONFIG_SYNCING", got)
	}
}

func TestRuntimeSnapshotConversionsFailClosedForSyncDrainStaleHealthAndNoCapacity(t *testing.T) {
	now := time.Unix(100, 0)
	tests := []struct {
		name     string
		snapshot serverruntime.SessionStatusSnapshot
		want     ServiceStatus
	}{
		{
			name: "Config Ack 前保持 syncing",
			snapshot: serverruntime.SessionStatusSnapshot{
				CurrentControlSession: true, HeartbeatFresh: true,
			},
			want: ServiceStatusConfigSyncing,
		},
		{
			name:     "过期 Health 不放行",
			snapshot: runtimeServiceSnapshot(now.Add(-time.Second), 1, false),
			want:     ServiceStatusOriginUnhealthy,
		},
		{
			name:     "没有 Idle Work 为无容量",
			snapshot: runtimeServiceSnapshot(now.Add(time.Second), 0, false),
			want:     ServiceStatusNoCapacity,
		},
		{
			name:     "Drain 即使有 Idle Work 也无容量",
			snapshot: runtimeServiceSnapshot(now.Add(time.Second), 1, true),
			want:     ServiceStatusNoCapacity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := ServiceConnectorFromRuntime(test.snapshot, "svc_current", 7, now)
			if got := CalculateService(ServiceInput{
				Enabled: true, RequiredRevision: 7, HealthEnabled: true,
				Connectors: []ServiceConnector{input},
			}); got != test.want {
				t.Fatalf("CalculateService() = %q, want %q; input=%#v", got, test.want, input)
			}
		})
	}
}

func TestRuntimeSnapshotConversionUsesLifecycleDrainBeforePoolDrain(t *testing.T) {
	snapshot := serverruntime.SessionStatusSnapshot{
		CurrentControlSession: true,
		HeartbeatFresh:        true,
		LifecycleStatus:       serverruntime.ConnectorStatusDraining,
		Config:                serverruntime.SessionEligibility{ConfigReady: true},
	}
	connector, present := CalculateConnector(ConnectorInputFromRuntime(snapshot))
	if !present || connector != ConnectorStatusDraining {
		t.Fatalf("CalculateConnector(lifecycle drain) = (%q, %t), want DRAINING", connector, present)
	}
	service := ServiceConnectorFromRuntime(snapshot, "svc_current", 7, time.Unix(1, 0))
	if !service.Draining {
		t.Fatalf("ServiceConnectorFromRuntime(lifecycle drain) = %#v", service)
	}
}

func runtimeServiceSnapshot(healthyUntil time.Time, idle uint32, draining bool) serverruntime.SessionStatusSnapshot {
	return serverruntime.SessionStatusSnapshot{
		CurrentControlSession: true, HeartbeatFresh: true,
		Config: serverruntime.SessionEligibility{
			ConfigReady: true, HasObserved: true, ObservedRevision: 7,
			Services: map[string]serverruntime.ServiceEligibility{
				"svc_current": {
					RequiredRevision: 7, Enabled: true, HealthRevision: 7,
					HealthHealthy: true, HealthyUntil: healthyUntil,
				},
			},
		},
		WorkPool: serverruntime.ConnectorWorkPoolSnapshot{Idle: idle, Total: idle, Draining: draining},
	}
}

func TestStatusWireValues(t *testing.T) {
	tunnel := map[TunnelStatus]string{
		TunnelStatusPending: "PENDING", TunnelStatusOnline: "ONLINE", TunnelStatusDegraded: "DEGRADED",
		TunnelStatusOffline: "OFFLINE", TunnelStatusRevoked: "REVOKED",
	}
	connector := map[ConnectorStatus]string{
		ConnectorStatusOnline: "ONLINE", ConnectorStatusDegraded: "DEGRADED", ConnectorStatusDraining: "DRAINING",
	}
	service := map[ServiceStatus]string{
		ServiceStatusDisabled: "DISABLED", ServiceStatusApplyFailed: "APPLY_FAILED",
		ServiceStatusTunnelOffline: "TUNNEL_OFFLINE", ServiceStatusConfigSyncing: "CONFIG_SYNCING",
		ServiceStatusOriginUnhealthy: "ORIGIN_UNHEALTHY", ServiceStatusNoCapacity: "NO_CAPACITY",
		ServiceStatusReady: "READY",
	}
	for got, want := range tunnel {
		if string(got) != want {
			t.Fatalf("TunnelStatus = %q, want %q", got, want)
		}
	}
	for got, want := range connector {
		if string(got) != want {
			t.Fatalf("ConnectorStatus = %q, want %q", got, want)
		}
	}
	for got, want := range service {
		if string(got) != want {
			t.Fatalf("ServiceStatus = %q, want %q", got, want)
		}
	}
}
