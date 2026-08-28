// Package snapshot 构建并校验下发给 Tunnel 全部 Connector 的完整 Desired State。
package snapshot

import (
	"errors"
	"fmt"
	"sort"

	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/protocol/deterministic"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/repository"
)

var (
	// ErrInvalidConfig 表示 Builder 的固定协议版本或大小边界无效。
	ErrInvalidConfig = errors.New("snapshot builder config is invalid")
	// ErrInvalidSnapshot 表示输入不能构成合法的完整 TunnelSnapshot。
	ErrInvalidSnapshot = errors.New("tunnel snapshot is invalid")
	// ErrServiceLimit 表示完整 Service 集合超过配置的单 Tunnel 上限。
	ErrServiceLimit = errors.New("tunnel snapshot exceeds service limit")
	// ErrSnapshotTooLarge 表示 TunnelSnapshot 的确定性编码超过配置上限。
	ErrSnapshotTooLarge = errors.New("tunnel snapshot exceeds byte limit")
	// ErrControlFrameTooLarge 表示最终 ControlEnvelope protobuf payload 超过配置上限。
	ErrControlFrameTooLarge = errors.New("tunnel snapshot control envelope exceeds frame limit")
)

const (
	// MaxServicesPerTunnel 是 V0.1 单 Tunnel 完整 Snapshot 的绝对 Service 上限。
	MaxServicesPerTunnel = 1000
	// MaxTunnelSnapshotSize 是 Protocol v1 对确定性 TunnelSnapshot payload 的绝对业务上限。
	MaxTunnelSnapshotSize = 768 << 10
)

// Config 固定一个 Builder 对所有 Build 使用的协议版本和大小边界。
type Config struct {
	ProtocolVersion      uint32
	MaxServices          int
	MaxSnapshotBytes     int
	MaxControlFrameBytes int
}

// Result 包含按 service_id 排序的 Wire Snapshot 及其确定性编码。
type Result struct {
	Snapshot           *protocolv1.TunnelSnapshot
	DeterministicBytes []byte
}

// Builder 把持久化 Service 状态映射为冻结的 Protocol v1 Snapshot 契约。
type Builder struct {
	config Config
}

// New 校验协议与分配边界后返回不可变的 Builder。
func New(config Config) (*Builder, error) {
	if config.ProtocolVersion != 1 || config.MaxServices < 1 || config.MaxServices > MaxServicesPerTunnel || config.MaxSnapshotBytes < 1 ||
		config.MaxSnapshotBytes > MaxTunnelSnapshotSize || config.MaxControlFrameBytes < 1 ||
		uint64(config.MaxControlFrameBytes) > frame.MaxControlFrameSize {
		return nil, ErrInvalidConfig
	}
	return &Builder{config: config}, nil
}

// Validate 执行完整 Build 与大小检查，但不保留生成结果。
func (builder *Builder) Validate(tunnelID string, revision int64, services []repository.Service) error {
	_, err := builder.Build(tunnelID, revision, services)
	return err
}

// Build 校验完整 Service 集合、映射 Protocol v1，并执行 Snapshot 与 Envelope 两层大小 Gate。
// 返回的 Snapshot 独占按 service_id 排序的新切片，不会别名引用或重排输入 services。
func (builder *Builder) Build(tunnelID string, revision int64, services []repository.Service) (Result, error) {
	if builder == nil {
		return Result{}, ErrInvalidConfig
	}
	if !identity.ValidTunnelID(tunnelID) || revision < 0 {
		return Result{}, ErrInvalidSnapshot
	}
	if len(services) > builder.config.MaxServices {
		return Result{}, fmt.Errorf("%w: count=%d limit=%d", ErrServiceLimit, len(services), builder.config.MaxServices)
	}

	wireServices := make([]*protocolv1.ServiceConfig, 0, len(services))
	seen := make(map[string]struct{}, len(services))
	for index := range services {
		service := services[index]
		if err := service.Validate(); err != nil || service.TunnelID != tunnelID || service.RequiredRevision > revision {
			return Result{}, fmt.Errorf("%w: service at index %d", ErrInvalidSnapshot, index)
		}
		if _, exists := seen[service.ID]; exists {
			return Result{}, fmt.Errorf("%w: duplicate service at index %d", ErrInvalidSnapshot, index)
		}
		seen[service.ID] = struct{}{}
		wireServices = append(wireServices, mapService(service))
	}

	sort.SliceStable(wireServices, func(left, right int) bool {
		return wireServices[left].GetServiceId() < wireServices[right].GetServiceId()
	})
	snapshot := &protocolv1.TunnelSnapshot{
		TunnelId: tunnelID,
		Revision: uint64(revision),
		Services: wireServices,
	}
	snapshotBytes, err := deterministic.MarshalSnapshot(snapshot)
	if err != nil {
		return Result{}, fmt.Errorf("%w: deterministic encoding: %w", ErrInvalidSnapshot, err)
	}
	if len(snapshotBytes) > builder.config.MaxSnapshotBytes {
		return Result{}, fmt.Errorf("%w: bytes=%d limit=%d", ErrSnapshotTooLarge, len(snapshotBytes), builder.config.MaxSnapshotBytes)
	}

	envelope := &protocolv1.ControlEnvelope{
		ProtocolVersion: builder.config.ProtocolVersion,
		Payload: &protocolv1.ControlEnvelope_ConfigSnapshot{
			ConfigSnapshot: snapshot,
		},
	}
	envelopeBytes, err := deterministic.Marshal(envelope)
	if err != nil {
		return Result{}, fmt.Errorf("%w: deterministic envelope encoding: %w", ErrInvalidSnapshot, err)
	}
	if len(envelopeBytes) > builder.config.MaxControlFrameBytes {
		return Result{}, fmt.Errorf("%w: bytes=%d limit=%d", ErrControlFrameTooLarge, len(envelopeBytes), builder.config.MaxControlFrameBytes)
	}

	return Result{Snapshot: snapshot, DeterministicBytes: snapshotBytes}, nil
}

// mapService 把 Repository 中已经默认化、校验过的 Service 映射为完整 Wire 配置。
// 通用连接参数始终下发；HTTP Transport 参数只对 HTTP/HTTPS 出现，使 Agent 能把
// “消息缺失”识别为协议错误，而不是在运行时各自补一套可能漂移的默认值。
func mapService(service repository.Service) *protocolv1.ServiceConfig {
	proxyOptions := service.ProxyOptions.WithDefaults()
	wire := &protocolv1.ServiceConfig{
		ServiceId:        service.ID,
		OriginScheme:     string(service.OriginScheme),
		OriginHost:       service.OriginHost,
		OriginPort:       service.OriginPort,
		ConnectTimeoutMs: service.ConnectTimeoutMS,
		TlsVerify:        service.TLSVerify,
		TlsServerName:    service.TLSServerName,
		OriginHttpHost:   service.OriginHTTPHost,
		Health:           mapHealth(service.Health),
		Enabled:          service.Enabled,
		RequiredRevision: uint64(service.RequiredRevision),
		OriginConnectionOptions: &protocolv1.OriginConnectionOptions{
			DisableHappyEyeballs:   proxyOptions.DisableHappyEyeballs,
			TcpKeepaliveIntervalMs: proxyOptions.TCPKeepAliveIntervalMS,
		},
	}
	if service.OriginScheme == repository.OriginSchemeHTTP || service.OriginScheme == repository.OriginSchemeHTTPS {
		wire.HttpProxyOptions = &protocolv1.HTTPProxyOptions{
			DisableChunkedEncoding:  proxyOptions.DisableChunkedEncoding,
			IdleConnectionTimeoutMs: proxyOptions.HTTPIdleConnectionTimeoutMS,
			MaxIdleConnections:      proxyOptions.HTTPMaxIdleConnections,
		}
	}
	return wire
}

func mapHealth(health *repository.HealthCheck) *protocolv1.HealthCheckConfig {
	if health == nil {
		return &protocolv1.HealthCheckConfig{Type: protocolv1.HealthType_HEALTH_TYPE_DISABLED}
	}

	healthType := protocolv1.HealthType_HEALTH_TYPE_TCP
	if health.Type == repository.HealthTypeHTTP {
		healthType = protocolv1.HealthType_HEALTH_TYPE_HTTP
	}
	return &protocolv1.HealthCheckConfig{
		Type:              healthType,
		Path:              health.Path,
		IntervalMs:        health.IntervalMS,
		TimeoutMs:         health.TimeoutMS,
		ExpectedStatusMin: health.ExpectedStatusMin,
		ExpectedStatusMax: health.ExpectedStatusMax,
		FailureThreshold:  health.FailureThreshold,
		SuccessThreshold:  health.SuccessThreshold,
	}
}
