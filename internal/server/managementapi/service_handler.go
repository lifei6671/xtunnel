package managementapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/oapi-codegen/nullable"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/healthbudget"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
	serversnapshot "github.com/lifei6671/xtunnel/internal/server/snapshot"
	"github.com/lifei6671/xtunnel/internal/tcpport"
)

func (api *managementStrictAPI) ListServices(ctx context.Context, request ListServicesRequestObject) (ListServicesResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if !validate.ValidID(request.Params.TunnelId, "tun_") {
		return ListServices400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "Tunnel ID 无效", requestIDFromContext(requestContext)))}, nil
	}
	if failure := validateListRequest(request.Params.PageSize, request.Params.PageToken); failure != nil {
		return ListServices400JSONResponse{BadRequestJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	if request.Params.Status != nil && !request.Params.Status.Valid() {
		return ListServices400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "status 无效", requestIDFromContext(requestContext)))}, nil
	}
	views, err := api.handler.services.List(ctx, request.Params.TunnelId)
	if err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		if failure.status == 404 {
			return ListServices404JSONResponse{NotFoundJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
		}
		return ListServices500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	items := make([]Service, 0, len(views))
	for _, view := range views {
		if request.Params.Status != nil && ServiceStatus(view.Status) != *request.Params.Status ||
			request.Params.Enabled != nil && view.Service.Enabled != *request.Params.Enabled {
			continue
		}
		item, err := serviceResponse(view)
		if err != nil {
			api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_service_projection_failed", err)
			return ListServices500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
		}
		items = append(items, item)
	}
	return ListServices200JSONResponse(ServiceList{Items: items}), nil
}

func (api *managementStrictAPI) CreateService(ctx context.Context, request CreateServiceRequestObject) (CreateServiceResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if request.Body == nil || !validate.ValidID(request.Body.TunnelId, "tun_") {
		return CreateService400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "Service 请求无效", requestIDFromContext(requestContext)))}, nil
	}
	expectedTunnelVersion, err := parseTunnelIfMatch(request.Params.IfMatch)
	if err != nil {
		return CreateService400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDIFMATCH, "If-Match 无效", requestIDFromContext(requestContext)))}, nil
	}
	input, err := createServiceInput(*request.Body, expectedTunnelVersion)
	if err != nil {
		return CreateService422JSONResponse{ValidationFailedJSONResponse(apiError(APIErrorCodeVALIDATIONFAILED, err.Error(), requestIDFromContext(requestContext)))}, nil
	}
	result, err := api.handler.services.Create(ctx, input)
	if err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		return createServiceFailure(failure, requestIDFromContext(requestContext)), nil
	}
	view, err := api.handler.services.Get(ctx, result.Service.ID)
	if err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		return CreateService500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	body, err := serviceResponse(view)
	if err != nil {
		api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_service_projection_failed", err)
		return CreateService500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
	}
	etag := serviceETag(view)
	location := "/api/v1/services/" + view.Service.ID
	return CreateService201JSONResponse{Body: body, Headers: CreateService201ResponseHeaders{ETag: &etag, Location: &location}}, nil
}

func (api *managementStrictAPI) GetService(ctx context.Context, request GetServiceRequestObject) (GetServiceResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if !validate.ValidID(request.ServiceId, "svc_") {
		return GetService404JSONResponse{NotFoundJSONResponse(apiError(APIErrorCodeRESOURCENOTFOUND, "Service 不存在", requestIDFromContext(requestContext)))}, nil
	}
	view, err := api.handler.services.Get(ctx, request.ServiceId)
	if err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		if failure.status == 404 {
			return GetService404JSONResponse{NotFoundJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
		}
		return GetService500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	body, err := serviceResponse(view)
	if err != nil {
		api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_service_projection_failed", err)
		return GetService500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
	}
	etag := serviceETag(view)
	return GetService200JSONResponse{ServiceOKJSONResponse{Body: body, Headers: ServiceOKResponseHeaders{ETag: &etag}}}, nil
}

func (api *managementStrictAPI) UpdateService(ctx context.Context, request UpdateServiceRequestObject) (UpdateServiceResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if !validate.ValidID(request.ServiceId, "svc_") {
		return UpdateService400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "Service ID 无效", requestIDFromContext(requestContext)))}, nil
	}
	identity, err := parseServiceIfMatch(request.Params.IfMatch, request.ServiceId)
	if err != nil {
		return UpdateService400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDIFMATCH, "If-Match 无效", requestIDFromContext(requestContext)))}, nil
	}
	if request.Body == nil {
		return UpdateService422JSONResponse{ValidationFailedJSONResponse(apiError(APIErrorCodeVALIDATIONFAILED, "Service 更新至少需要一个字段", requestIDFromContext(requestContext)))}, nil
	}
	input, err := updateServiceInput(request.ServiceId, identity, *request.Body)
	if err != nil {
		return UpdateService422JSONResponse{ValidationFailedJSONResponse(apiError(APIErrorCodeVALIDATIONFAILED, err.Error(), requestIDFromContext(requestContext)))}, nil
	}
	if _, err := api.handler.services.Update(ctx, input); err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		return updateServiceFailure(failure, requestIDFromContext(requestContext)), nil
	}
	return api.updatedServiceResponse(ctx, requestContext, request.ServiceId)
}

func (api *managementStrictAPI) DeleteService(ctx context.Context, request DeleteServiceRequestObject) (DeleteServiceResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if !validate.ValidID(request.ServiceId, "svc_") {
		return DeleteService400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "Service ID 无效", requestIDFromContext(requestContext)))}, nil
	}
	identity, err := parseServiceIfMatch(request.Params.IfMatch, request.ServiceId)
	if err != nil {
		return DeleteService400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDIFMATCH, "If-Match 无效", requestIDFromContext(requestContext)))}, nil
	}
	_, err = api.handler.services.Delete(ctx, application.DeleteServiceInput{
		TunnelID: identity.tunnelID, ServiceID: request.ServiceId,
		ExpectedTunnelVersion: identity.tunnelVersion, ExpectedServiceVersion: identity.serviceVersion,
	})
	if err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		return deleteServiceFailure(failure, requestIDFromContext(requestContext)), nil
	}
	return DeleteService204Response{}, nil
}

func (api *managementStrictAPI) EnableService(ctx context.Context, request EnableServiceRequestObject) (EnableServiceResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	identity, failure := validateServiceMutationIdentity(request.ServiceId, request.Params.IfMatch)
	if failure != nil {
		return enableServiceFailure(*failure, requestIDFromContext(requestContext)), nil
	}
	enabled := true
	_, err := api.handler.services.Update(ctx, application.UpdateServiceAPIInput{
		TunnelID: identity.tunnelID, ServiceID: request.ServiceId,
		ExpectedTunnelVersion: identity.tunnelVersion, ExpectedServiceVersion: identity.serviceVersion,
		Enabled: &enabled,
	})
	if err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		return enableServiceFailure(failure, requestIDFromContext(requestContext)), nil
	}
	response, projectionFailure := api.serviceOK(ctx, requestContext, request.ServiceId)
	if projectionFailure != nil {
		return EnableService500JSONResponse{InternalErrorJSONResponse(projectionFailure.response(requestIDFromContext(requestContext)))}, nil
	}
	return EnableService200JSONResponse{response}, nil
}

func (api *managementStrictAPI) DisableService(ctx context.Context, request DisableServiceRequestObject) (DisableServiceResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	identity, failure := validateServiceMutationIdentity(request.ServiceId, request.Params.IfMatch)
	if failure != nil {
		return disableServiceFailure(*failure, requestIDFromContext(requestContext)), nil
	}
	enabled := false
	_, err := api.handler.services.Update(ctx, application.UpdateServiceAPIInput{
		TunnelID: identity.tunnelID, ServiceID: request.ServiceId,
		ExpectedTunnelVersion: identity.tunnelVersion, ExpectedServiceVersion: identity.serviceVersion,
		Enabled: &enabled,
	})
	if err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		return disableServiceFailure(failure, requestIDFromContext(requestContext)), nil
	}
	response, projectionFailure := api.serviceOK(ctx, requestContext, request.ServiceId)
	if projectionFailure != nil {
		return DisableService500JSONResponse{InternalErrorJSONResponse(projectionFailure.response(requestIDFromContext(requestContext)))}, nil
	}
	return DisableService200JSONResponse{response}, nil
}

func (api *managementStrictAPI) updatedServiceResponse(ctx context.Context, requestContext *managementRequestContext, serviceID string) (UpdateServiceResponseObject, error) {
	response, failure := api.serviceOK(ctx, requestContext, serviceID)
	if failure != nil {
		return UpdateService500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	return UpdateService200JSONResponse{response}, nil
}

func (api *managementStrictAPI) serviceOK(ctx context.Context, requestContext *managementRequestContext, serviceID string) (ServiceOKJSONResponse, *managementFailure) {
	view, err := api.handler.services.Get(ctx, serviceID)
	if err != nil {
		failure := api.handler.mapServiceError(ctx, requestContext, err)
		return ServiceOKJSONResponse{}, &failure
	}
	body, err := serviceResponse(view)
	if err != nil {
		api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_service_projection_failed", err)
		failure := managementFailure{status: 500, code: APIErrorCodeINTERNALERROR, message: "服务器内部错误"}
		return ServiceOKJSONResponse{}, &failure
	}
	etag := serviceETag(view)
	return ServiceOKJSONResponse{Body: body, Headers: ServiceOKResponseHeaders{ETag: &etag}}, nil
}

type serviceMutationIdentity struct {
	tunnelID       string
	serviceVersion int64
	tunnelVersion  int64
}

func serviceETag(view application.ServiceView) string {
	return fmt.Sprintf("\"service:%s:%s:%d:%d\"", view.Service.ID, view.Service.TunnelID, view.Service.Version, view.TunnelVersion)
}

func parseServiceIfMatch(value, serviceID string) (serviceMutationIdentity, error) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' || strings.HasPrefix(value, "W/") {
		return serviceMutationIdentity{}, errors.New("invalid Service ETag")
	}
	parts := strings.Split(value[1:len(value)-1], ":")
	if len(parts) != 5 || parts[0] != "service" || parts[1] != serviceID || !validate.ValidID(parts[2], "tun_") {
		return serviceMutationIdentity{}, errors.New("invalid Service ETag")
	}
	serviceVersion, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || serviceVersion < 1 {
		return serviceMutationIdentity{}, errors.New("invalid Service ETag")
	}
	tunnelVersion, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil || tunnelVersion < 1 {
		return serviceMutationIdentity{}, errors.New("invalid Service ETag")
	}
	return serviceMutationIdentity{tunnelID: parts[2], serviceVersion: serviceVersion, tunnelVersion: tunnelVersion}, nil
}

func validateServiceMutationIdentity(serviceID, ifMatch string) (serviceMutationIdentity, *managementFailure) {
	if !validate.ValidID(serviceID, "svc_") {
		return serviceMutationIdentity{}, &managementFailure{status: 400, code: APIErrorCodeINVALIDREQUEST, message: "Service ID 无效"}
	}
	identity, err := parseServiceIfMatch(ifMatch, serviceID)
	if err != nil {
		return serviceMutationIdentity{}, &managementFailure{status: 400, code: APIErrorCodeINVALIDIFMATCH, message: "If-Match 无效"}
	}
	return identity, nil
}

func createServiceInput(body CreateServiceRequest, expectedTunnelVersion int64) (application.CreateServiceAPIInput, error) {
	origin, err := serviceOriginInput(body.Origin, body.ProxyOptions)
	if err != nil {
		return application.CreateServiceAPIInput{}, err
	}
	health, err := createServiceHealth(body.Health)
	if err != nil {
		return application.CreateServiceAPIInput{}, err
	}
	exposure, err := createServiceExposure(body.Exposure)
	if err != nil {
		return application.CreateServiceAPIInput{}, err
	}
	return application.CreateServiceAPIInput{Service: application.CreateServiceInput{
		TunnelID: body.TunnelId, ExpectedTunnelVersion: expectedTunnelVersion,
		Name: body.Name, Origin: origin, Health: health, Enabled: body.Enabled,
	}, Exposure: exposure}, nil
}

func serviceOriginInput(value OriginInput, proxy *ProxyOptionsInput) (application.ServiceOriginInput, error) {
	discriminator, err := value.Discriminator()
	if err != nil {
		return application.ServiceOriginInput{}, errors.New("origin.scheme 无效")
	}
	var result application.ServiceOriginInput
	switch discriminator {
	case "http":
		var input HTTPOriginInput
		if err := strictUnionDecode(value, &input); err != nil || input.Scheme != HTTPOriginInputSchemeHttp ||
			input.Port < 1 || input.Port > math.MaxUint16 || !validUint32(input.ConnectTimeoutMs, false) {
			return result, errors.New("origin 无效")
		}
		result.Scheme, result.Host, result.Port = repository.OriginSchemeHTTP, input.Host, uint32(input.Port)
		result.ConnectTimeoutMS, result.HTTPHost = uint32Pointer(input.ConnectTimeoutMs), stringValue(input.HttpHostHeader)
	case "https":
		var input HTTPSOriginInput
		if err := strictUnionDecode(value, &input); err != nil || input.Scheme != HTTPSOriginInputSchemeHttps ||
			input.Port < 1 || input.Port > math.MaxUint16 || !validUint32(input.ConnectTimeoutMs, false) {
			return result, errors.New("origin 无效")
		}
		result.Scheme, result.Host, result.Port = repository.OriginSchemeHTTPS, input.Host, uint32(input.Port)
		result.ConnectTimeoutMS, result.TLSVerify = uint32Pointer(input.ConnectTimeoutMs), input.TlsVerify
		result.TLSServerName, result.HTTPHost = stringValue(input.TlsServerName), stringValue(input.HttpHostHeader)
	case "tcp":
		var input TCPOriginInput
		if err := strictUnionDecode(value, &input); err != nil || input.Scheme != TCPOriginInputSchemeTcp ||
			input.Port < 1 || input.Port > math.MaxUint16 || !validUint32(input.ConnectTimeoutMs, false) {
			return result, errors.New("origin 无效")
		}
		result.Scheme, result.Host, result.Port = repository.OriginSchemeTCP, input.Host, uint32(input.Port)
		result.ConnectTimeoutMS = uint32Pointer(input.ConnectTimeoutMs)
	default:
		return result, errors.New("origin.scheme 无效")
	}
	if proxy != nil {
		if !validUint32(proxy.TcpKeepaliveIntervalMs, true) || !validUint32(proxy.HttpIdleConnectionTimeoutMs, false) ||
			!validUint32(proxy.HttpMaxIdleConnections, false) {
			return result, errors.New("proxy_options 无效")
		}
		result.Connection = &application.ServiceConnectionOptionsInput{
			DisableHappyEyeballs:   proxy.DisableHappyEyeballs,
			TCPKeepAliveIntervalMS: uint32Pointer(proxy.TcpKeepaliveIntervalMs),
		}
		if proxy.DisableChunkedEncoding != nil || proxy.HttpIdleConnectionTimeoutMs != nil || proxy.HttpMaxIdleConnections != nil {
			result.HTTPProxy = &application.ServiceHTTPProxyOptionsInput{
				DisableChunkedEncoding:  proxy.DisableChunkedEncoding,
				IdleConnectionTimeoutMS: uint32Pointer(proxy.HttpIdleConnectionTimeoutMs),
				MaxIdleConnections:      uint32Pointer(proxy.HttpMaxIdleConnections),
			}
		}
	}
	return result, nil
}

func createServiceHealth(value nullable.Nullable[HealthCheckInput]) (*application.ServiceHealthInput, error) {
	if !value.IsSpecified() || value.IsNull() {
		return nil, nil
	}
	input, err := value.Get()
	if err != nil {
		return nil, errors.New("health 无效")
	}
	discriminator, err := input.Discriminator()
	if err != nil {
		return nil, errors.New("health.type 无效")
	}
	switch discriminator {
	case "TCP":
		var value TCPHealthCheckInput
		if err := strictUnionDecode(input, &value); err != nil || value.Type != TCPHealthCheckInputTypeTCP ||
			!validHealthNumbers(value.IntervalMs, value.TimeoutMs, nil, nil, value.FailureThreshold, value.SuccessThreshold) {
			return nil, errors.New("health 无效")
		}
		return &application.ServiceHealthInput{Type: repository.HealthTypeTCP,
			IntervalMS: uint32Pointer(value.IntervalMs), TimeoutMS: uint32Pointer(value.TimeoutMs),
			FailureThreshold: uint32Pointer(value.FailureThreshold), SuccessThreshold: uint32Pointer(value.SuccessThreshold)}, nil
	case "HTTP":
		var value HTTPHealthCheckInput
		if err := strictUnionDecode(input, &value); err != nil || value.Type != HTTPHealthCheckInputTypeHTTP ||
			!validHealthNumbers(value.IntervalMs, value.TimeoutMs, value.ExpectedStatusMin, value.ExpectedStatusMax, value.FailureThreshold, value.SuccessThreshold) {
			return nil, errors.New("health 无效")
		}
		return &application.ServiceHealthInput{Type: repository.HealthTypeHTTP, Path: value.Path,
			IntervalMS: uint32Pointer(value.IntervalMs), TimeoutMS: uint32Pointer(value.TimeoutMs),
			ExpectedStatusMin: uint32Pointer(value.ExpectedStatusMin), ExpectedStatusMax: uint32Pointer(value.ExpectedStatusMax),
			FailureThreshold: uint32Pointer(value.FailureThreshold), SuccessThreshold: uint32Pointer(value.SuccessThreshold)}, nil
	default:
		return nil, errors.New("health.type 无效")
	}
}

func createServiceExposure(value ExposureInput) (application.ServiceExposureInput, error) {
	discriminator, err := value.Discriminator()
	if err != nil {
		return application.ServiceExposureInput{}, errors.New("exposure.type 无效")
	}
	switch discriminator {
	case "http":
		var input HTTPExposureInput
		if err := strictUnionDecode(value, &input); err != nil || input.Type != HTTPExposureInputTypeHttp {
			return application.ServiceExposureInput{}, errors.New("exposure 无效")
		}
		return application.ServiceExposureInput{Type: application.ServiceExposureHTTP,
			Hostname: input.Hostname, PathPrefix: input.PathPrefix, PreserveHost: input.PreserveHost}, nil
	case "tcp":
		var input TCPExposureInput
		if err := strictUnionDecode(value, &input); err != nil || input.Type != TCPExposureInputTypeTcp ||
			input.PublicPort != nil && (*input.PublicPort < 1 || *input.PublicPort > math.MaxUint16) {
			return application.ServiceExposureInput{}, errors.New("exposure 无效")
		}
		return application.ServiceExposureInput{Type: application.ServiceExposureTCP, PublicPort: uint16Pointer(input.PublicPort)}, nil
	default:
		return application.ServiceExposureInput{}, errors.New("exposure.type 无效")
	}
}

func updateServiceInput(serviceID string, identity serviceMutationIdentity, body UpdateServiceRequest) (application.UpdateServiceAPIInput, error) {
	if body.Origin != nil && (!validUint32(body.Origin.Port, false) || !validUint32(body.Origin.ConnectTimeoutMs, false)) {
		return application.UpdateServiceAPIInput{}, errors.New("origin 无效")
	}
	if body.ProxyOptions != nil && (!validUint32(body.ProxyOptions.TcpKeepaliveIntervalMs, true) ||
		!validUint32(body.ProxyOptions.HttpIdleConnectionTimeoutMs, false) || !validUint32(body.ProxyOptions.HttpMaxIdleConnections, false)) {
		return application.UpdateServiceAPIInput{}, errors.New("proxy_options 无效")
	}
	result := application.UpdateServiceAPIInput{
		TunnelID: identity.tunnelID, ServiceID: serviceID,
		ExpectedTunnelVersion: identity.tunnelVersion, ExpectedServiceVersion: identity.serviceVersion,
		Name: body.Name,
	}
	if body.Origin != nil {
		result.Origin = &application.ServiceOriginPatchInput{
			Scheme: originSchemePointer(body.Origin.Scheme), Host: body.Origin.Host, Port: uint32Pointer(body.Origin.Port),
			ConnectTimeoutMS: uint32Pointer(body.Origin.ConnectTimeoutMs), TLSVerify: body.Origin.TlsVerify,
			TLSServerName: body.Origin.TlsServerName, HTTPHost: body.Origin.HttpHostHeader,
		}
	}
	if body.ProxyOptions != nil {
		result.ProxyOptions = &application.ServiceProxyOptionsPatchInput{
			DisableChunkedEncoding:  body.ProxyOptions.DisableChunkedEncoding,
			DisableHappyEyeballs:    body.ProxyOptions.DisableHappyEyeballs,
			IdleConnectionTimeoutMS: uint32Pointer(body.ProxyOptions.HttpIdleConnectionTimeoutMs),
			MaxIdleConnections:      uint32Pointer(body.ProxyOptions.HttpMaxIdleConnections),
			TCPKeepAliveIntervalMS:  uint32Pointer(body.ProxyOptions.TcpKeepaliveIntervalMs),
		}
	}
	result.HealthSet = body.Health.IsSpecified()
	if result.HealthSet && !body.Health.IsNull() {
		patch, err := body.Health.Get()
		if err != nil {
			return result, errors.New("health 无效")
		}
		if !validHealthNumbers(patch.IntervalMs, patch.TimeoutMs, patch.ExpectedStatusMin, patch.ExpectedStatusMax,
			patch.FailureThreshold, patch.SuccessThreshold) {
			return result, errors.New("health 无效")
		}
		result.Health = &application.ServiceHealthPatchInput{
			Type: healthTypePointer(patch.Type), Path: patch.Path,
			IntervalMS: uint32Pointer(patch.IntervalMs), TimeoutMS: uint32Pointer(patch.TimeoutMs),
			ExpectedStatusMin: uint32Pointer(patch.ExpectedStatusMin), ExpectedStatusMax: uint32Pointer(patch.ExpectedStatusMax),
			FailureThreshold: uint32Pointer(patch.FailureThreshold), SuccessThreshold: uint32Pointer(patch.SuccessThreshold),
		}
	}
	result.ExposureSet = body.Exposure.IsSpecified()
	if result.ExposureSet && !body.Exposure.IsNull() {
		patch, err := body.Exposure.Get()
		if err != nil {
			return result, errors.New("exposure 无效")
		}
		if patch.PublicPort != nil && (*patch.PublicPort < 1 || *patch.PublicPort > math.MaxUint16) {
			return result, errors.New("exposure 无效")
		}
		result.Exposure = &application.ServiceExposurePatchInput{
			Type: exposureTypePointer(patch.Type), Hostname: patch.Hostname, PathPrefix: patch.PathPrefix,
			PreserveHost: patch.PreserveHost, PublicPort: uint16Pointer(patch.PublicPort),
		}
	}
	return result, nil
}

func serviceResponse(view application.ServiceView) (Service, error) {
	origin, err := serviceOriginResponse(view.Service)
	if err != nil {
		return Service{}, err
	}
	proxy, err := serviceProxyResponse(view.Service)
	if err != nil {
		return Service{}, err
	}
	health, err := serviceHealthResponse(view.Service.Health)
	if err != nil {
		return Service{}, err
	}
	exposure, err := serviceExposureResponse(view.Exposure)
	if err != nil {
		return Service{}, err
	}
	if view.HealthyConnectors > math.MaxInt || view.ActiveConnections > math.MaxInt {
		return Service{}, errors.New("service counters exceed int")
	}
	applyFailure := nullable.NewNullNullable[ApplyFailure]()
	// OpenAPI 规定只有 APPLY_FAILED 才能返回详情；DISABLED 的优先级更高，
	// 即使 Runtime 仍保留同 revision 的失败记录也必须投影为 null。
	if ServiceStatus(view.Status) == ServiceStatusAPPLYFAILED && view.ApplyFailure != nil {
		applyFailure = nullable.NewNullableWithValue(ApplyFailure{ErrorCode: view.ApplyFailure.ErrorCode, FailedAt: view.ApplyFailure.FailedAt.UTC()})
	}
	return Service{
		Id: view.Service.ID, TunnelId: view.Service.TunnelID, Name: view.Service.Name,
		RequiredRevision: view.Service.RequiredRevision, Origin: origin, ProxyOptions: proxy,
		Health: health, Exposure: exposure, Enabled: view.Service.Enabled, Version: view.Service.Version,
		Status: ServiceStatus(view.Status), ApplyFailure: applyFailure,
		HealthyConnectors: int(view.HealthyConnectors), ActiveConnections: int(view.ActiveConnections),
		Usage: unavailableServiceUsage(), CreatedAt: time.Unix(view.Service.CreatedAt, 0).UTC(), UpdatedAt: time.Unix(view.Service.UpdatedAt, 0).UTC(),
	}, nil
}

func serviceOriginResponse(service repository.Service) (Origin, error) {
	var result Origin
	switch service.OriginScheme {
	case repository.OriginSchemeHTTP:
		value := HTTPOrigin{Scheme: HTTPOriginSchemeHttp, Host: service.OriginHost, Port: int(service.OriginPort),
			ConnectTimeoutMs: int(service.ConnectTimeoutMS), HttpHostHeader: optionalString(service.OriginHTTPHost)}
		return result, result.FromHTTPOrigin(value)
	case repository.OriginSchemeHTTPS:
		value := HTTPSOrigin{Scheme: HTTPSOriginSchemeHttps, Host: service.OriginHost, Port: int(service.OriginPort),
			ConnectTimeoutMs: int(service.ConnectTimeoutMS), TlsVerify: service.TLSVerify,
			TlsServerName: optionalString(service.TLSServerName), HttpHostHeader: optionalString(service.OriginHTTPHost)}
		return result, result.FromHTTPSOrigin(value)
	case repository.OriginSchemeTCP:
		value := TCPOrigin{Scheme: TCPOriginSchemeTcp, Host: service.OriginHost, Port: int(service.OriginPort), ConnectTimeoutMs: int(service.ConnectTimeoutMS)}
		return result, result.FromTCPOrigin(value)
	default:
		return result, repository.ErrInvalidService
	}
}

func serviceProxyResponse(service repository.Service) (ProxyOptions, error) {
	options := service.ProxyOptions.WithDefaults()
	var result ProxyOptions
	if service.OriginScheme == repository.OriginSchemeTCP {
		return result, result.FromTCPProxyOptions(TCPProxyOptions{
			DisableHappyEyeballs: options.DisableHappyEyeballs, TcpKeepaliveIntervalMs: int(options.TCPKeepAliveIntervalMS),
		})
	}
	return result, result.FromHTTPProxyOptions(HTTPProxyOptions{
		DisableChunkedEncoding: options.DisableChunkedEncoding, DisableHappyEyeballs: options.DisableHappyEyeballs,
		HttpIdleConnectionTimeoutMs: int(options.HTTPIdleConnectionTimeoutMS), HttpMaxIdleConnections: int(options.HTTPMaxIdleConnections),
		TcpKeepaliveIntervalMs: int(options.TCPKeepAliveIntervalMS),
	})
}

func serviceHealthResponse(value *repository.HealthCheck) (nullable.Nullable[HealthCheck], error) {
	if value == nil {
		return nullable.NewNullNullable[HealthCheck](), nil
	}
	var health HealthCheck
	switch value.Type {
	case repository.HealthTypeTCP:
		err := health.FromTCPHealthCheck(TCPHealthCheck{Type: TCPHealthCheckTypeTCP,
			IntervalMs: int(value.IntervalMS), TimeoutMs: int(value.TimeoutMS),
			FailureThreshold: int(value.FailureThreshold), SuccessThreshold: int(value.SuccessThreshold)})
		return nullable.NewNullableWithValue(health), err
	case repository.HealthTypeHTTP:
		err := health.FromHTTPHealthCheck(HTTPHealthCheck{Type: HTTPHealthCheckTypeHTTP, Path: value.Path,
			IntervalMs: int(value.IntervalMS), TimeoutMs: int(value.TimeoutMS),
			ExpectedStatusMin: int(value.ExpectedStatusMin), ExpectedStatusMax: int(value.ExpectedStatusMax),
			FailureThreshold: int(value.FailureThreshold), SuccessThreshold: int(value.SuccessThreshold)})
		return nullable.NewNullableWithValue(health), err
	default:
		return nil, repository.ErrInvalidService
	}
}

func serviceExposureResponse(value repository.ServiceExposure) (nullable.Nullable[Exposure], error) {
	var exposure Exposure
	switch {
	case value.HTTP != nil && value.TCP != nil:
		return nil, repository.ErrInvalidRoute
	case value.HTTP != nil:
		err := exposure.FromHTTPExposure(HTTPExposure{Type: HTTPExposureTypeHttp, Hostname: value.HTTP.Hostname,
			PathPrefix: value.HTTP.PathPrefix, PreserveHost: value.HTTP.PreserveHost})
		return nullable.NewNullableWithValue(exposure), err
	case value.TCP != nil:
		err := exposure.FromTCPExposure(TCPExposure{Type: TCPExposureTypeTcp, PublicPort: int(value.TCP.PublicPort)})
		return nullable.NewNullableWithValue(exposure), err
	default:
		return nullable.NewNullNullable[Exposure](), nil
	}
}

func unavailableServiceUsage() UsageSummary {
	return UsageSummary{Availability: UsageSummaryAvailabilityUNAVAILABLE,
		ConnectionsToday: nullable.NewNullNullable[int64](), IngressBytesToday: nullable.NewNullNullable[int64](), EgressBytesToday: nullable.NewNullNullable[int64]()}
}

func (handler *ManagementHandler) mapServiceError(ctx context.Context, requestContext *managementRequestContext, err error) managementFailure {
	requestID := requestIDFromContext(requestContext)
	switch {
	case errors.Is(err, repository.ErrVersionConflict):
		return managementFailure{status: 412, code: APIErrorCodeRESOURCEVERSIONCONFLICT, message: "资源版本已变化"}
	case errors.Is(err, application.ErrServiceManagementUnavailable), errors.Is(err, application.ErrRouteManagementUnavailable), errors.Is(err, repository.ErrNotFound):
		return managementFailure{status: 404, code: APIErrorCodeRESOURCENOTFOUND, message: "Service 不存在"}
	case errors.Is(err, application.ErrServiceManagementTunnelRevoked):
		return managementFailure{status: 409, code: APIErrorCodeTUNNELREVOKED, message: "Tunnel 已撤销"}
	case errors.Is(err, tcpport.ErrPortUnavailable), errors.Is(err, tcpport.ErrPoolExhausted):
		return managementFailure{status: 409, code: APIErrorCodeTCPPORTUNAVAILABLE, message: "TCP 公网端口不可用"}
	case errors.Is(err, application.ErrServiceExposureConflict):
		return managementFailure{status: 409, code: APIErrorCodeROUTECONFLICT, message: "公网 Exposure 已被其他 Service 使用"}
	case errors.Is(err, serversnapshot.ErrServiceLimit), errors.Is(err, healthbudget.ErrTargetCapacity):
		return managementFailure{status: 422, code: APIErrorCodeTUNNELSERVICELIMIT, message: "Tunnel 的 Service 数量已达到上限"}
	case errors.Is(err, serversnapshot.ErrSnapshotTooLarge):
		return managementFailure{status: 422, code: APIErrorCodeSNAPSHOTTOOLARGE, message: "Tunnel Snapshot 超过大小限制"}
	case errors.Is(err, application.ErrServiceRuntimeConvergence), errors.Is(err, application.ErrRouteRuntimeConvergence):
		return managementFailure{status: 409, code: APIErrorCodeRUNTIMECONVERGENCEFAILED, message: "持久化状态已提交，但运行态尚未完全收敛"}
	case errors.Is(err, application.ErrServiceManagementInput), errors.Is(err, application.ErrRouteManagementInput),
		errors.Is(err, repository.ErrInvalidService), errors.Is(err, repository.ErrInvalidRoute):
		return managementFailure{status: 422, code: APIErrorCodeVALIDATIONFAILED, message: "请求内容不符合 Service 约束"}
	default:
		handler.logInternalError(ctx, requestID, "management_service_operation_failed", err)
		return managementFailure{status: 500, code: APIErrorCodeINTERNALERROR, message: "服务器内部错误"}
	}
}

func createServiceFailure(f managementFailure, requestID string) CreateServiceResponseObject {
	switch f.status {
	case 404:
		return CreateService404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return CreateService409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	case 412:
		return CreateService412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	case 422:
		return CreateService422JSONResponse{ValidationFailedJSONResponse(f.response(requestID))}
	default:
		return CreateService500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func updateServiceFailure(f managementFailure, requestID string) UpdateServiceResponseObject {
	switch f.status {
	case 404:
		return UpdateService404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return UpdateService409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	case 412:
		return UpdateService412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	case 422:
		return UpdateService422JSONResponse{ValidationFailedJSONResponse(f.response(requestID))}
	default:
		return UpdateService500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func deleteServiceFailure(f managementFailure, requestID string) DeleteServiceResponseObject {
	switch f.status {
	case 404:
		return DeleteService404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 412:
		return DeleteService412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	default:
		return DeleteService500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func enableServiceFailure(f managementFailure, requestID string) EnableServiceResponseObject {
	switch f.status {
	case 400:
		return EnableService400JSONResponse{BadRequestJSONResponse(f.response(requestID))}
	case 404:
		return EnableService404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return EnableService409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	case 412:
		return EnableService412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	case 422:
		return EnableService422JSONResponse{ValidationFailedJSONResponse(f.response(requestID))}
	default:
		return EnableService500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func disableServiceFailure(f managementFailure, requestID string) DisableServiceResponseObject {
	switch f.status {
	case 400:
		return DisableService400JSONResponse{BadRequestJSONResponse(f.response(requestID))}
	case 404:
		return DisableService404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return DisableService409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	case 412:
		return DisableService412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	case 422:
		return DisableService422JSONResponse{ValidationFailedJSONResponse(f.response(requestID))}
	default:
		return DisableService500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func uint32Pointer(value *int) *uint32 {
	if value == nil {
		return nil
	}
	converted := uint32(*value)
	return &converted
}

func uint16Pointer(value *int) *uint16 {
	if value == nil {
		return nil
	}
	converted := uint16(*value)
	return &converted
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func originSchemePointer(value *OriginPatchScheme) *repository.OriginScheme {
	if value == nil {
		return nil
	}
	converted := repository.OriginScheme(*value)
	return &converted
}

func healthTypePointer(value *HealthCheckPatchType) *repository.HealthType {
	if value == nil {
		return nil
	}
	converted := repository.HealthType(*value)
	return &converted
}

func exposureTypePointer(value *ExposurePatchType) *application.ServiceExposureType {
	if value == nil {
		return nil
	}
	converted := application.ServiceExposureType(*value)
	return &converted
}

func strictUnionDecode(value any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func validUint32(value *int, allowZero bool) bool {
	if value == nil {
		return true
	}
	if *value < 0 || uint64(*value) > math.MaxUint32 {
		return false
	}
	return allowZero || *value > 0
}

func validHealthNumbers(values ...*int) bool {
	for _, value := range values {
		if !validUint32(value, false) {
			return false
		}
	}
	return true
}
