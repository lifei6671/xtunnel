package snapshot

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
)

const (
	testTunnelID      = "tun_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	otherTunnelID     = "tun_01ARZ3NDEKTSV4RRFFQ69G5FAW"
	firstServiceID    = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAV"
	secondServiceID   = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAW"
	thirdServiceID    = "svc_01ARZ3NDEKTSV4RRFFQ69G5FAX"
	testProtocol      = uint32(1)
	testSnapshotLimit = MaxTunnelSnapshotSize
)

func TestBuilderBuildMapsAndSortsWithoutMutatingInput(t *testing.T) {
	tcpHealth := &repository.HealthCheck{
		Type:             repository.HealthTypeTCP,
		IntervalMS:       5_000,
		TimeoutMS:        1_000,
		FailureThreshold: 2,
		SuccessThreshold: 3,
	}
	httpHealth := &repository.HealthCheck{
		Type:              repository.HealthTypeHTTP,
		Path:              "/ready",
		IntervalMS:        10_000,
		TimeoutMS:         2_000,
		ExpectedStatusMin: 200,
		ExpectedStatusMax: 299,
		FailureThreshold:  4,
		SuccessThreshold:  5,
	}
	services := []repository.Service{
		validService(thirdServiceID, repository.OriginSchemeHTTPS, httpHealth),
		validService(firstServiceID, repository.OriginSchemeHTTP, nil),
		validService(secondServiceID, repository.OriginSchemeTCP, tcpHealth),
	}
	services[0].TLSVerify = true
	services[0].TLSServerName = "origin.internal"
	services[0].OriginHTTPHost = "public.example"
	services[1].OriginHTTPHost = "virtual.example"
	before := cloneServices(services)

	builder := newTestBuilder(t, Config{
		ProtocolVersion:      testProtocol,
		MaxServices:          len(services),
		MaxSnapshotBytes:     testSnapshotLimit,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
	result, err := builder.Build(testTunnelID, 9, services)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if !reflect.DeepEqual(services, before) {
		t.Fatalf("Build() mutated input\ngot:  %#v\nwant: %#v", services, before)
	}
	if result.Snapshot.GetTunnelId() != testTunnelID || result.Snapshot.GetRevision() != 9 {
		t.Fatalf("snapshot identity = (%q, %d)", result.Snapshot.GetTunnelId(), result.Snapshot.GetRevision())
	}
	gotIDs := []string{
		result.Snapshot.Services[0].GetServiceId(),
		result.Snapshot.Services[1].GetServiceId(),
		result.Snapshot.Services[2].GetServiceId(),
	}
	wantIDs := []string{firstServiceID, secondServiceID, thirdServiceID}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("service order = %v, want %v", gotIDs, wantIDs)
	}

	http := result.Snapshot.Services[0]
	if http.GetOriginScheme() != "http" || http.GetOriginHost() != "127.0.0.1" ||
		http.GetOriginPort() != 8080 || http.GetConnectTimeoutMs() != 5_000 ||
		http.GetTlsVerify() || http.GetTlsServerName() != "" ||
		http.GetOriginHttpHost() != "virtual.example" || !http.GetEnabled() ||
		http.GetRequiredRevision() != 3 || http.GetHealth() == nil ||
		http.GetHealth().GetType() != protocolv1.HealthType_HEALTH_TYPE_DISABLED {
		t.Fatalf("disabled-health HTTP mapping = %#v", http)
	}

	tcp := result.Snapshot.Services[1]
	if tcp.GetOriginScheme() != "tcp" || tcp.GetHealth().GetType() != protocolv1.HealthType_HEALTH_TYPE_TCP ||
		tcp.GetHealth().GetIntervalMs() != 5_000 || tcp.GetHealth().GetTimeoutMs() != 1_000 ||
		tcp.GetHealth().GetPath() != "" || tcp.GetHealth().GetExpectedStatusMin() != 0 ||
		tcp.GetHealth().GetExpectedStatusMax() != 0 || tcp.GetHealth().GetFailureThreshold() != 2 ||
		tcp.GetHealth().GetSuccessThreshold() != 3 {
		t.Fatalf("TCP mapping = %#v", tcp)
	}

	https := result.Snapshot.Services[2]
	if https.GetOriginScheme() != "https" || !https.GetTlsVerify() ||
		https.GetTlsServerName() != "origin.internal" || https.GetOriginHttpHost() != "public.example" ||
		https.GetHealth().GetType() != protocolv1.HealthType_HEALTH_TYPE_HTTP ||
		https.GetHealth().GetPath() != "/ready" || https.GetHealth().GetExpectedStatusMin() != 200 ||
		https.GetHealth().GetExpectedStatusMax() != 299 || https.GetHealth().GetFailureThreshold() != 4 ||
		https.GetHealth().GetSuccessThreshold() != 5 {
		t.Fatalf("HTTPS/HTTP-health mapping = %#v", https)
	}

	wantBytes, err := deterministic.MarshalSnapshot(result.Snapshot)
	if err != nil {
		t.Fatalf("MarshalSnapshot() error = %v", err)
	}
	if !reflect.DeepEqual(result.DeterministicBytes, wantBytes) {
		t.Fatalf("deterministic bytes do not describe returned Snapshot")
	}
	reversed := []repository.Service{services[2], services[1], services[0]}
	reversedResult, err := builder.Build(testTunnelID, 9, reversed)
	if err != nil {
		t.Fatalf("Build(reversed) error = %v", err)
	}
	if !reflect.DeepEqual(reversedResult.DeterministicBytes, result.DeterministicBytes) {
		t.Fatalf("deterministic bytes changed with input order")
	}
}

func TestBuilderServiceCountBoundary(t *testing.T) {
	services := []repository.Service{
		validService(firstServiceID, repository.OriginSchemeHTTP, nil),
		validService(secondServiceID, repository.OriginSchemeTCP, nil),
		validService(thirdServiceID, repository.OriginSchemeHTTPS, nil),
	}
	builder := newTestBuilder(t, Config{
		ProtocolVersion:      testProtocol,
		MaxServices:          2,
		MaxSnapshotBytes:     testSnapshotLimit,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})

	if _, err := builder.Build(testTunnelID, 3, services[:2]); err != nil {
		t.Fatalf("Build() at service limit error = %v", err)
	}
	if _, err := builder.Build(testTunnelID, 3, services); !errors.Is(err, ErrServiceLimit) {
		t.Fatalf("Build() over service limit error = %v, want ErrServiceLimit", err)
	}
}

func TestBuilderSerializedSizeBoundaries(t *testing.T) {
	services := []repository.Service{validService(firstServiceID, repository.OriginSchemeHTTPS, nil)}
	baseline := newTestBuilder(t, Config{
		ProtocolVersion:      testProtocol,
		MaxServices:          1,
		MaxSnapshotBytes:     testSnapshotLimit,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
	result, err := baseline.Build(testTunnelID, 3, services)
	if err != nil {
		t.Fatalf("baseline Build() error = %v", err)
	}
	envelopeBytes, err := deterministic.Marshal(&protocolv1.ControlEnvelope{
		ProtocolVersion: testProtocol,
		Payload: &protocolv1.ControlEnvelope_ConfigSnapshot{
			ConfigSnapshot: result.Snapshot,
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope error = %v", err)
	}

	tests := []struct {
		name          string
		snapshotLimit int
		frameLimit    int
		wantErr       error
	}{
		{name: "both exact", snapshotLimit: len(result.DeterministicBytes), frameLimit: len(envelopeBytes)},
		{name: "snapshot one byte over", snapshotLimit: len(result.DeterministicBytes) - 1, frameLimit: len(envelopeBytes), wantErr: ErrSnapshotTooLarge},
		{name: "envelope one byte over", snapshotLimit: len(result.DeterministicBytes), frameLimit: len(envelopeBytes) - 1, wantErr: ErrControlFrameTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder := newTestBuilder(t, Config{
				ProtocolVersion:      testProtocol,
				MaxServices:          1,
				MaxSnapshotBytes:     test.snapshotLimit,
				MaxControlFrameBytes: test.frameLimit,
			})
			_, err := builder.Build(testTunnelID, 3, services)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Build() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestBuilderRejectsInvalidSnapshotInputs(t *testing.T) {
	valid := validService(firstServiceID, repository.OriginSchemeHTTP, nil)
	tests := []struct {
		name     string
		tunnelID string
		revision int64
		services func() []repository.Service
	}{
		{name: "invalid tunnel", tunnelID: "tun_invalid", revision: 3, services: func() []repository.Service { return []repository.Service{valid} }},
		{name: "negative revision", tunnelID: testTunnelID, revision: -1, services: func() []repository.Service { return []repository.Service{valid} }},
		{name: "invalid service", tunnelID: testTunnelID, revision: 3, services: func() []repository.Service {
			service := valid
			service.Name = " "
			return []repository.Service{service}
		}},
		{name: "cross tunnel service", tunnelID: testTunnelID, revision: 3, services: func() []repository.Service {
			service := valid
			service.TunnelID = otherTunnelID
			return []repository.Service{service}
		}},
		{name: "future required revision", tunnelID: testTunnelID, revision: 2, services: func() []repository.Service { return []repository.Service{valid} }},
		{name: "duplicate service", tunnelID: testTunnelID, revision: 3, services: func() []repository.Service { return []repository.Service{valid, valid} }},
	}
	builder := newTestBuilder(t, Config{
		ProtocolVersion:      testProtocol,
		MaxServices:          10,
		MaxSnapshotBytes:     testSnapshotLimit,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := builder.Build(test.tunnelID, test.revision, test.services())
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("Build() error = %v, want ErrInvalidSnapshot", err)
			}
		})
	}
}

func TestBuilderRevisionConversionAndEmptySnapshot(t *testing.T) {
	builder := newTestBuilder(t, Config{
		ProtocolVersion:      testProtocol,
		MaxServices:          1,
		MaxSnapshotBytes:     testSnapshotLimit,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})

	empty, err := builder.Build(testTunnelID, 0, nil)
	if err != nil {
		t.Fatalf("Build(empty) error = %v", err)
	}
	if empty.Snapshot.GetRevision() != 0 || len(empty.Snapshot.GetServices()) != 0 {
		t.Fatalf("empty Snapshot = %#v", empty.Snapshot)
	}

	service := validService(firstServiceID, repository.OriginSchemeHTTP, nil)
	service.RequiredRevision = math.MaxInt64
	result, err := builder.Build(testTunnelID, math.MaxInt64, []repository.Service{service})
	if err != nil {
		t.Fatalf("Build(MaxInt64) error = %v", err)
	}
	if result.Snapshot.GetRevision() != uint64(math.MaxInt64) ||
		result.Snapshot.Services[0].GetRequiredRevision() != uint64(math.MaxInt64) {
		t.Fatalf("max revision mapping = (%d, %d)", result.Snapshot.GetRevision(), result.Snapshot.Services[0].GetRequiredRevision())
	}
}

func TestBuilderValidateAppliesSizeGates(t *testing.T) {
	service := validService(firstServiceID, repository.OriginSchemeHTTP, nil)
	baseline := newTestBuilder(t, Config{
		ProtocolVersion:      testProtocol,
		MaxServices:          1,
		MaxSnapshotBytes:     testSnapshotLimit,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
	result, err := baseline.Build(testTunnelID, 3, []repository.Service{service})
	if err != nil {
		t.Fatalf("baseline Build() error = %v", err)
	}
	builder := newTestBuilder(t, Config{
		ProtocolVersion:      testProtocol,
		MaxServices:          1,
		MaxSnapshotBytes:     len(result.DeterministicBytes) - 1,
		MaxControlFrameBytes: int(frame.MaxControlFrameSize),
	})
	if err := builder.Validate(testTunnelID, 3, []repository.Service{service}); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("Validate() error = %v, want ErrSnapshotTooLarge", err)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	valid := Config{
		ProtocolVersion:      testProtocol,
		MaxServices:          1,
		MaxSnapshotBytes:     1,
		MaxControlFrameBytes: 1,
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "protocol zero", mutate: func(config *Config) { config.ProtocolVersion = 0 }},
		{name: "protocol unsupported", mutate: func(config *Config) { config.ProtocolVersion = 2 }},
		{name: "services zero", mutate: func(config *Config) { config.MaxServices = 0 }},
		{name: "services above absolute", mutate: func(config *Config) { config.MaxServices = MaxServicesPerTunnel + 1 }},
		{name: "snapshot bytes zero", mutate: func(config *Config) { config.MaxSnapshotBytes = 0 }},
		{name: "snapshot above protocol hard limit", mutate: func(config *Config) { config.MaxSnapshotBytes = MaxTunnelSnapshotSize + 1 }},
		{name: "control bytes zero", mutate: func(config *Config) { config.MaxControlFrameBytes = 0 }},
		{name: "control above protocol hard limit", mutate: func(config *Config) { config.MaxControlFrameBytes = int(frame.MaxControlFrameSize) + 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			builder, err := New(config)
			if builder != nil || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() = (%#v, %v), want (nil, ErrInvalidConfig)", builder, err)
			}
		})
	}

	var builder *Builder
	if err := builder.Validate(testTunnelID, 0, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Builder.Validate() error = %v, want ErrInvalidConfig", err)
	}
}

func validService(id string, scheme repository.OriginScheme, health *repository.HealthCheck) repository.Service {
	return repository.Service{
		ID:               id,
		TunnelID:         testTunnelID,
		Name:             "origin",
		RequiredRevision: 3,
		OriginScheme:     scheme,
		OriginHost:       "127.0.0.1",
		OriginPort:       8080,
		ConnectTimeoutMS: 5_000,
		Health:           health,
		Enabled:          true,
		Version:          1,
		CreatedAt:        1,
		UpdatedAt:        1,
	}
}

func cloneServices(services []repository.Service) []repository.Service {
	cloned := make([]repository.Service, len(services))
	copy(cloned, services)
	for index := range cloned {
		if services[index].Health != nil {
			health := *services[index].Health
			cloned[index].Health = &health
		}
	}
	return cloned
}

func newTestBuilder(t *testing.T, config Config) *Builder {
	t.Helper()
	builder, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return builder
}
