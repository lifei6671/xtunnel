package managementapi

import (
	"context"

	"github.com/oapi-codegen/nullable"
)

func (api *managementStrictAPI) GetSystemInfo(
	ctx context.Context,
	_ GetSystemInfoRequestObject,
) (GetSystemInfoResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil || api.handler.system == nil {
		return GetSystemInfo500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext),
		))}, nil
	}
	info, err := api.handler.system.Info(ctx)
	if err != nil {
		api.handler.logInternalError(ctx, requestContext.requestID, "management_system_info_failed", err)
		return GetSystemInfo500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID,
		))}, nil
	}
	return GetSystemInfo200JSONResponse(SystemInfo{
		Version: info.Version, GoVersion: info.GoVersion,
		ProtocolVersion: SystemInfoProtocolVersion(info.ProtocolVersion),
		Os:              info.OS, Arch: info.Arch, StartedAt: info.StartedAt.UTC(),
		UptimeSeconds: info.UptimeSeconds,
	}), nil
}

func (api *managementStrictAPI) GetSystemHealth(
	ctx context.Context,
	_ GetSystemHealthRequestObject,
) (GetSystemHealthResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil || api.handler.system == nil {
		return GetSystemHealth500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext),
		))}, nil
	}
	health, err := api.handler.system.Health(ctx)
	if err != nil {
		api.handler.logInternalError(ctx, requestContext.requestID, "management_system_health_failed", err)
		return GetSystemHealth500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID,
		))}, nil
	}
	checks := make([]HealthCheckResult, 0, len(health.Checks))
	for _, check := range health.Checks {
		message := nullable.NewNullNullable[string]()
		if check.Message != nil {
			message = nullable.NewNullableWithValue(*check.Message)
		}
		checks = append(checks, HealthCheckResult{
			Name: check.Name, Status: HealthCheckResultStatus(check.Status), Message: message,
		})
	}
	return GetSystemHealth200JSONResponse(SystemHealth{
		Status: SystemHealthStatus(health.Status), Checks: checks, CheckedAt: health.CheckedAt.UTC(),
	}), nil
}

func (api *managementStrictAPI) GetSystemConfig(
	ctx context.Context,
	_ GetSystemConfigRequestObject,
) (GetSystemConfigResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil || api.handler.system == nil {
		return GetSystemConfig500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext),
		))}, nil
	}
	config, err := api.handler.system.Config(ctx)
	if err != nil {
		api.handler.logInternalError(ctx, requestContext.requestID, "management_system_config_failed", err)
		return GetSystemConfig500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID,
		))}, nil
	}
	response := SystemConfig{
		ChangesRequireRestart: SystemConfigChangesRequireRestart(config.ChangesRequireRestart),
		Limits: PublicLimits{
			MaxTunnels:                          config.Limits.MaxTunnels,
			MaxConnectors:                       config.Limits.MaxConnectors,
			MaxConnectorsPerTunnel:              config.Limits.MaxConnectorsPerTunnel,
			MaxServicesPerTunnel:                config.Limits.MaxServicesPerTunnel,
			MaxActiveConnections:                config.Limits.MaxActiveConnections,
			MaxConnectionsPerTunnel:             config.Limits.MaxConnectionsPerTunnel,
			MaxConnectionsPerService:            config.Limits.MaxConnectionsPerService,
			MaxConnectionsPerSourceIp:           config.Limits.MaxConnectionsPerSourceIP,
			MaxOpenRatePerSourceIp:              config.Limits.MaxOpenRatePerSourceIP,
			MaxOpenBurstPerSourceIp:             config.Limits.MaxOpenBurstPerSourceIP,
			MaxHttpRequestsPerSourceIpPerSecond: config.Limits.MaxHTTPRequestsPerSourceIPPerSecond,
			MaxHttpHeaderBytes:                  config.Limits.MaxHTTPHeaderBytes,
			MaxHttpBodyBytes:                    config.Limits.MaxHTTPBodyBytes,
		},
	}
	response.Management.PublicUrl = config.Management.PublicURL
	response.AgentGateway.PublicHostname = config.AgentGateway.PublicHostname
	response.AgentGateway.TlsMode = SystemConfigAgentGatewayTlsMode(config.AgentGateway.TLSMode)
	response.TcpIngress.MinPort = config.TCPIngress.MinPort
	response.TcpIngress.MaxPort = config.TCPIngress.MaxPort
	response.Logging.Level = SystemConfigLoggingLevel(config.Logging.Level)
	return GetSystemConfig200JSONResponse(response), nil
}
