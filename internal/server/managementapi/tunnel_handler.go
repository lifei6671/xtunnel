package managementapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oapi-codegen/nullable"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	"github.com/lifei6671/xtunnel/internal/repository"
)

type managementFailure struct {
	status  int
	code    APIErrorCode
	message string
	details *APIError_Details
}

func (api *managementStrictAPI) ListTunnels(ctx context.Context, request ListTunnelsRequestObject) (ListTunnelsResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if failure := validateListRequest(request.Params.PageSize, request.Params.PageToken); failure != nil {
		return ListTunnels400JSONResponse{BadRequestJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	if request.Params.Status != nil && !request.Params.Status.Valid() {
		return ListTunnels400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "status 无效", requestIDFromContext(requestContext)))}, nil
	}
	views, err := api.handler.tunnels.List(ctx)
	if err != nil {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return ListTunnels500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	filtered := make([]application.TunnelView, 0, len(views))
	for _, view := range views {
		if request.Params.Status != nil && string(*request.Params.Status) != string(view.Status) {
			continue
		}
		filtered = append(filtered, view)
	}
	statusFilter := ""
	if request.Params.Status != nil {
		statusFilter = string(*request.Params.Status)
	}
	page, nextPageToken, err := paginateManagementItems(
		api.handler.pageTokens, filtered, request.Params.PageSize, request.Params.PageToken,
		pageTokenScope{resource: "tunnels", idPrefix: "tun_", filter: pageFilter("status", statusFilter)},
		func(view application.TunnelView) string { return view.Tunnel.ID },
	)
	if err != nil {
		failure := paginationFailure(err)
		if failure.status == http.StatusInternalServerError {
			api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_tunnel_pagination_failed", err)
			return ListTunnels500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
		}
		return ListTunnels400JSONResponse{BadRequestJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	items := make([]Tunnel, 0, len(page))
	for _, view := range page {
		items = append(items, tunnelResponse(view))
	}
	return ListTunnels200JSONResponse(TunnelList{Items: items, NextPageToken: nextPageToken}), nil
}

func (api *managementStrictAPI) CreateTunnel(ctx context.Context, request CreateTunnelRequestObject) (CreateTunnelResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil || request.Body == nil {
		return CreateTunnel500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
	}
	result, err := api.handler.tunnels.Create(ctx, application.CreateTunnelInput{Name: request.Body.Name})
	if err != nil && !errors.Is(err, repository.ErrPostCommitCleanup) {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		switch failure.status {
		case 422:
			return CreateTunnel422JSONResponse{ValidationFailedJSONResponse(failure.response(requestContext.requestID))}, nil
		case 409:
			return CreateTunnel409JSONResponse{ConflictJSONResponse(failure.response(requestContext.requestID))}, nil
		case 429:
			return CreateTunnel429JSONResponse{RateLimitedJSONResponse{Body: failure.response(requestContext.requestID)}}, nil
		default:
			return CreateTunnel500JSONResponse{InternalErrorJSONResponse(failure.response(requestContext.requestID))}, nil
		}
	}
	if err != nil {
		api.handler.logInternalError(ctx, requestContext.requestID, "management_tunnel_create_post_commit_cleanup_failed", err)
	}
	view, viewErr := api.handler.tunnels.Get(ctx, result.Tunnel.ID)
	if viewErr != nil {
		failure := api.handler.mapTunnelError(ctx, requestContext, viewErr)
		return CreateTunnel500JSONResponse{InternalErrorJSONResponse(failure.response(requestContext.requestID))}, nil
	}
	// Representation 与 ETag 必须来自同一次投影读取；否则并发 PATCH/Rotate
	// 可能让响应体已是新版本，而 Header 仍停留在 Create 的线性化版本。
	etag := tunnelETag(view.Tunnel.Version)
	location := "/api/v1/tunnels/" + result.Tunnel.ID
	cacheControl, pragma := secretCacheHeaders()
	return CreateTunnel201JSONResponse{
		Body: TunnelCredentialResponse{Tunnel: tunnelResponse(view), Credential: connectionCredential(result.Credential)},
		Headers: CreateTunnel201ResponseHeaders{
			CacheControl: &cacheControl, ETag: &etag, Location: &location, Pragma: &pragma,
		},
	}, nil
}

func (api *managementStrictAPI) GetTunnel(ctx context.Context, request GetTunnelRequestObject) (GetTunnelResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if !validate.ValidID(request.TunnelId, "tun_") {
		return GetTunnel404JSONResponse{NotFoundJSONResponse(apiError(APIErrorCodeRESOURCENOTFOUND, "Tunnel 不存在", requestIDFromContext(requestContext)))}, nil
	}
	view, err := api.handler.tunnels.Get(ctx, request.TunnelId)
	if err != nil {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		switch failure.status {
		case 404:
			return GetTunnel404JSONResponse{NotFoundJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
		default:
			return GetTunnel500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
		}
	}
	etag := tunnelETag(view.Tunnel.Version)
	return GetTunnel200JSONResponse{Body: tunnelResponse(view), Headers: GetTunnel200ResponseHeaders{ETag: &etag}}, nil
}

func (api *managementStrictAPI) UpdateTunnel(ctx context.Context, request UpdateTunnelRequestObject) (UpdateTunnelResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if !validate.ValidID(request.TunnelId, "tun_") {
		return UpdateTunnel400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "Tunnel ID 无效", requestIDFromContext(requestContext)))}, nil
	}
	expectedVersion, err := parseTunnelIfMatch(request.Params.IfMatch)
	if err != nil {
		return UpdateTunnel400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDIFMATCH, "If-Match 无效", requestIDFromContext(requestContext)))}, nil
	}
	if request.Body == nil || request.Body.Name == nil || strings.TrimSpace(*request.Body.Name) == "" {
		return UpdateTunnel422JSONResponse{ValidationFailedJSONResponse(apiError(APIErrorCodeVALIDATIONFAILED, "Tunnel 名称不能为空", requestIDFromContext(requestContext)))}, nil
	}
	view, err := api.handler.tunnels.Get(ctx, request.TunnelId)
	if err != nil {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return updateTunnelFailure(failure, requestIDFromContext(requestContext)), nil
	}
	updated, err := api.handler.tunnels.Update(ctx, application.UpdateTunnelInput{
		TunnelID: request.TunnelId, ExpectedVersion: expectedVersion, Name: *request.Body.Name,
	})
	if err != nil {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return updateTunnelFailure(failure, requestIDFromContext(requestContext)), nil
	}
	// 运行态字段可以保持写前快照，但持久字段和 ETag 必须绑定本次 CAS 的提交结果，
	// 不能在解锁后重新 GET 并误返回后一个管理员的版本。
	view.Tunnel = updated
	etag := tunnelETag(view.Tunnel.Version)
	return UpdateTunnel200JSONResponse{TunnelOKJSONResponse{Body: tunnelResponse(view), Headers: TunnelOKResponseHeaders{ETag: &etag}}}, nil
}

func (api *managementStrictAPI) DeleteTunnel(ctx context.Context, request DeleteTunnelRequestObject) (DeleteTunnelResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if !validate.ValidID(request.TunnelId, "tun_") {
		return DeleteTunnel400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "Tunnel ID 无效", requestIDFromContext(requestContext)))}, nil
	}
	expectedVersion, err := parseTunnelIfMatch(request.Params.IfMatch)
	if err != nil {
		return DeleteTunnel400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDIFMATCH, "If-Match 无效", requestIDFromContext(requestContext)))}, nil
	}
	result, err := api.handler.tunnels.Delete(ctx, application.DeleteTunnelInput{TunnelID: request.TunnelId, ExpectedVersion: expectedVersion})
	if err != nil && !ignorableTunnelPostCommitCleanup(err, result.Deleted) {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return deleteTunnelFailure(failure, requestIDFromContext(requestContext)), nil
	}
	if err != nil {
		api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_tunnel_delete_post_commit_cleanup_failed", err)
	}
	return DeleteTunnel204Response{}, nil
}

func (api *managementStrictAPI) ListTunnelConnectors(ctx context.Context, request ListTunnelConnectorsRequestObject) (ListTunnelConnectorsResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if !validate.ValidID(request.TunnelId, "tun_") {
		return ListTunnelConnectors400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "Tunnel ID 无效", requestIDFromContext(requestContext)))}, nil
	}
	if failure := validateListRequest(request.Params.PageSize, request.Params.PageToken); failure != nil {
		return ListTunnelConnectors400JSONResponse{BadRequestJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	if request.Params.Status != nil && !request.Params.Status.Valid() {
		return ListTunnelConnectors400JSONResponse{BadRequestJSONResponse(apiError(APIErrorCodeINVALIDREQUEST, "status 无效", requestIDFromContext(requestContext)))}, nil
	}
	views, err := api.handler.tunnels.ListConnectors(ctx, request.TunnelId)
	if err != nil {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		switch failure.status {
		case 404:
			return ListTunnelConnectors404JSONResponse{NotFoundJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
		default:
			return ListTunnelConnectors500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
		}
	}
	filtered := make([]application.ConnectorView, 0, len(views))
	for _, view := range views {
		if request.Params.Status != nil && string(*request.Params.Status) != string(view.Status) {
			continue
		}
		filtered = append(filtered, view)
	}
	statusFilter := ""
	if request.Params.Status != nil {
		statusFilter = string(*request.Params.Status)
	}
	page, nextPageToken, err := paginateManagementItems(
		api.handler.pageTokens, filtered, request.Params.PageSize, request.Params.PageToken,
		pageTokenScope{
			resource: "connectors", idPrefix: "con_",
			filter: pageFilter("tunnel_id", request.TunnelId, "status", statusFilter),
		},
		func(view application.ConnectorView) string { return view.ID },
	)
	if err != nil {
		failure := paginationFailure(err)
		if failure.status == http.StatusInternalServerError {
			api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_connector_pagination_failed", err)
			return ListTunnelConnectors500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
		}
		return ListTunnelConnectors400JSONResponse{BadRequestJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	items := make([]Connector, 0, len(page))
	for _, view := range page {
		item, err := connectorResponse(view)
		if err != nil {
			api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_connector_projection_failed", err)
			return ListTunnelConnectors500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
		}
		items = append(items, item)
	}
	return ListTunnelConnectors200JSONResponse(ConnectorList{Items: items, NextPageToken: nextPageToken}), nil
}

func (api *managementStrictAPI) RevealTunnelToken(ctx context.Context, request RevealTunnelTokenRequestObject) (RevealTunnelTokenResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	authenticated, ok := authenticatedManagementRequestFrom(ctx)
	if !ok {
		return RevealTunnelToken500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
	}
	if !validate.ValidID(request.TunnelId, "tun_") {
		return RevealTunnelToken404JSONResponse{NotFoundJSONResponse(apiError(APIErrorCodeRESOURCENOTFOUND, "Tunnel 不存在", requestIDFromContext(requestContext)))}, nil
	}
	result, err := api.handler.credentials.Reveal(ctx, request.TunnelId, authenticated.Audit)
	if err != nil && !errors.Is(err, repository.ErrPostCommitCleanup) {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return revealTunnelTokenFailure(failure, requestIDFromContext(requestContext)), nil
	}
	if err != nil {
		api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_token_reveal_post_commit_cleanup_failed", err)
	}
	cacheControl, pragma := secretCacheHeaders()
	return RevealTunnelToken200JSONResponse{SecretCredentialOKJSONResponse{
		Body:    connectionCredential(result),
		Headers: SecretCredentialOKResponseHeaders{CacheControl: &cacheControl, Pragma: &pragma},
	}}, nil
}

func (api *managementStrictAPI) RotateTunnelToken(ctx context.Context, request RotateTunnelTokenRequestObject) (RotateTunnelTokenResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	authenticated, ok := authenticatedManagementRequestFrom(ctx)
	if !ok {
		return RotateTunnelToken500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
	}
	expectedVersion, failure := validateMutationIdentity(request.TunnelId, request.Params.IfMatch)
	if failure != nil {
		return RotateTunnelToken400JSONResponse{BadRequestJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	result, err := api.handler.credentials.Rotate(ctx, application.CredentialMutationInput{
		TunnelID: request.TunnelId, ExpectedVersion: expectedVersion, Audit: authenticated.Audit,
	})
	if err != nil && !errors.Is(err, repository.ErrPostCommitCleanup) {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return rotateTunnelTokenFailure(failure, requestIDFromContext(requestContext)), nil
	}
	if err != nil {
		api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_token_rotate_post_commit_cleanup_failed", err)
	}
	etag := tunnelETag(result.TunnelVersion)
	cacheControl, pragma := secretCacheHeaders()
	return RotateTunnelToken200JSONResponse{
		Body: connectionCredential(result.Credential),
		Headers: RotateTunnelToken200ResponseHeaders{
			CacheControl: &cacheControl, ETag: &etag, Pragma: &pragma,
		},
	}, nil
}

func (api *managementStrictAPI) RevokeTunnelToken(ctx context.Context, request RevokeTunnelTokenRequestObject) (RevokeTunnelTokenResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	authenticated, ok := authenticatedManagementRequestFrom(ctx)
	if !ok {
		return RevokeTunnelToken500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
	}
	expectedVersion, failure := validateMutationIdentity(request.TunnelId, request.Params.IfMatch)
	if failure != nil {
		return RevokeTunnelToken400JSONResponse{BadRequestJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	result, err := api.handler.credentials.Revoke(ctx, application.CredentialMutationInput{
		TunnelID: request.TunnelId, ExpectedVersion: expectedVersion, Audit: authenticated.Audit,
	})
	if err != nil && !errors.Is(err, repository.ErrPostCommitCleanup) {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return revokeTunnelTokenFailure(failure, requestIDFromContext(requestContext)), nil
	}
	if err != nil {
		api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_token_revoke_post_commit_cleanup_failed", err)
	}
	etag := tunnelETag(result.TunnelVersion)
	return RevokeTunnelToken200JSONResponse{
		Body: ConnectionCredentialMetadata{
			TunnelId: result.Credential.TunnelID, TokenId: result.Credential.TokenID,
			TokenVersion: result.Credential.TokenVersion, Status: ConnectionCredentialMetadataStatusREVOKED,
		},
		Headers: RevokeTunnelToken200ResponseHeaders{ETag: &etag},
	}, nil
}

func (api *managementStrictAPI) RevokeTunnel(ctx context.Context, request RevokeTunnelRequestObject) (RevokeTunnelResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	authenticated, ok := authenticatedManagementRequestFrom(ctx)
	if !ok {
		return RevokeTunnel500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
	}
	expectedVersion, failure := validateMutationIdentity(request.TunnelId, request.Params.IfMatch)
	if failure != nil {
		return RevokeTunnel400JSONResponse{BadRequestJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	result, err := api.handler.tunnelLifecycle.Revoke(ctx, application.TunnelRevokeInput{
		TunnelID: request.TunnelId, ExpectedVersion: expectedVersion, Audit: authenticated.Audit,
	})
	if err != nil && !ignorableTunnelPostCommitCleanup(err, true) {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return revokeTunnelFailure(failure, requestIDFromContext(requestContext)), nil
	}
	if err != nil {
		api.handler.logInternalError(ctx, requestIDFromContext(requestContext), "management_tunnel_revoke_post_commit_cleanup_failed", err)
	}
	view, err := api.handler.tunnels.Get(ctx, result.TunnelID)
	if err != nil {
		failure := api.handler.mapTunnelError(ctx, requestContext, err)
		return RevokeTunnel500JSONResponse{InternalErrorJSONResponse(failure.response(requestIDFromContext(requestContext)))}, nil
	}
	// GET 投影可能观察到紧随撤销之后的并发名称更新，因此 ETag 必须与 Body
	// 使用同一个已读取版本，不能继续使用撤销事务返回的旧版本。
	etag := tunnelETag(view.Tunnel.Version)
	return RevokeTunnel200JSONResponse{TunnelOKJSONResponse{Body: tunnelResponse(view), Headers: TunnelOKResponseHeaders{ETag: &etag}}}, nil
}

func tunnelResponse(view application.TunnelView) Tunnel {
	firstAuthenticatedAt := nullable.NewNullNullable[DateTime]()
	if view.Tunnel.FirstAuthenticatedAt != nil {
		firstAuthenticatedAt = nullable.NewNullableWithValue[DateTime](time.Unix(*view.Tunnel.FirstAuthenticatedAt, 0).UTC())
	}
	revokedAt := nullable.NewNullNullable[DateTime]()
	if view.Tunnel.RevokedAt != nil {
		revokedAt = nullable.NewNullableWithValue[DateTime](time.Unix(*view.Tunnel.RevokedAt, 0).UTC())
	}
	lastSeenAt := nullable.NewNullNullable[DateTime]()
	if view.LastSeenAt != nil {
		lastSeenAt = nullable.NewNullableWithValue[DateTime](view.LastSeenAt.UTC())
	}
	return Tunnel{
		Id: view.Tunnel.ID, Name: view.Tunnel.Name, Version: view.Tunnel.Version,
		DesiredRevision: view.Tunnel.DesiredRevision, Status: TunnelStatus(view.Status),
		ConnectorsOnline: int(view.ConnectorsOnline), ServicesCount: int(view.ServicesCount),
		ActiveConnections: int(view.ActiveConnections), LastSeenAt: lastSeenAt,
		FirstAuthenticatedAt: firstAuthenticatedAt, RevokedAt: revokedAt,
		CreatedAt: time.Unix(view.Tunnel.CreatedAt, 0).UTC(), UpdatedAt: time.Unix(view.Tunnel.UpdatedAt, 0).UTC(),
	}
}

func connectorResponse(view application.ConnectorView) (Connector, error) {
	if view.ObservedRevision > math.MaxInt64 {
		return Connector{}, errors.New("connector observed revision exceeds int64")
	}
	return Connector{
		Id: view.ID, TunnelId: view.TunnelID, Hostname: view.Hostname,
		Os: view.OS, Arch: view.Arch, Version: view.Version, Status: ConnectorStatus(view.Status),
		IdleWorkConnections: int(view.IdleWorkConnections), ActiveConnections: int(view.ActiveConnections),
		ConnectedAt: view.ConnectedAt, LastHeartbeatAt: view.LastHeartbeatAt,
		ConfigReady: view.ConfigReady, ObservedRevision: int64(view.ObservedRevision),
	}, nil
}

func connectionCredential(result application.ConnectionTokenResult) ConnectionCredential {
	return ConnectionCredential{
		TunnelId: result.TunnelID, TokenId: result.TokenID, TokenVersion: result.TokenVersion,
		Status: ConnectionCredentialStatusACTIVE, ConnectionToken: result.Token,
		DeploymentCommands: deploymentCommands(result.Token),
	}
}

func deploymentCommands(token string) []DeploymentCommand {
	return []DeploymentCommand{
		{Environment: FOREGROUND, Command: "xtunnel-agent run --token '" + token + "'"},
		{Environment: CONTAINER, Command: "docker run --rm -e XTUNNEL_TOKEN='" + token + "' xtunnel-agent:v0.1.0"},
		{Environment: LINUXSYSTEMD, Command: "sudo xtunnel-agent service install --token '" + token + "'"},
		{Environment: WINDOWSSCM, Command: ".\\xtunnel-agent.exe service install --token '" + token + "'"},
	}
}

func secretCacheHeaders() (string, string) { return "no-store", "no-cache" }

// ignorableTunnelPostCommitCleanup 只允许忽略“事务已经提交、仅清理现场失败”的错误。
// Runtime 收敛失败即使与 ErrPostCommitCleanup 联合出现也必须返回给客户端，避免把
// 仍可能存活的 Session/Work 误报为删除或撤销成功。
func ignorableTunnelPostCommitCleanup(err error, committed bool) bool {
	return committed && errors.Is(err, repository.ErrPostCommitCleanup) &&
		!errors.Is(err, application.ErrTunnelRuntimeConvergence) &&
		!errors.Is(err, application.ErrTunnelManagementRuntimeConvergence)
}

func tunnelETag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

func parseTunnelIfMatch(value IfMatch) (int64, error) {
	raw := string(value)
	if len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, errors.New("invalid strong ETag")
	}
	digits := raw[1 : len(raw)-1]
	if digits == "" || digits[0] == '0' {
		return 0, errors.New("invalid strong ETag version")
	}
	for _, current := range digits {
		if current < '0' || current > '9' {
			return 0, errors.New("invalid strong ETag version")
		}
	}
	version, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || version < 1 {
		return 0, errors.New("invalid strong ETag version")
	}
	return version, nil
}

func validateMutationIdentity(tunnelID string, ifMatch IfMatch) (int64, *managementFailure) {
	if !validate.ValidID(tunnelID, "tun_") {
		return 0, &managementFailure{status: 400, code: APIErrorCodeINVALIDREQUEST, message: "Tunnel ID 无效"}
	}
	version, err := parseTunnelIfMatch(ifMatch)
	if err != nil {
		return 0, &managementFailure{status: 400, code: APIErrorCodeINVALIDIFMATCH, message: "If-Match 无效"}
	}
	return version, nil
}

func validateListRequest(pageSize *PageSize, pageToken *PageToken) *managementFailure {
	if pageSize != nil && (*pageSize < 1 || *pageSize > maximumPageSize) {
		return &managementFailure{status: 400, code: APIErrorCodeINVALIDREQUEST, message: "page_size 必须在 1 到 200 之间"}
	}
	return nil
}

func (failure managementFailure) response(requestID string) ErrorResponse {
	response := apiError(failure.code, failure.message, requestID)
	response.Error.Details = failure.details
	return response
}

func (handler *ManagementHandler) mapTunnelError(ctx context.Context, requestContext *managementRequestContext, err error) managementFailure {
	requestID := requestIDFromContext(requestContext)
	switch {
	case errors.Is(err, repository.ErrVersionConflict):
		return managementFailure{status: 412, code: APIErrorCodeRESOURCEVERSIONCONFLICT, message: "资源版本已变化"}
	case errors.Is(err, application.ErrTunnelManagementUnavailable), errors.Is(err, repository.ErrNotFound),
		errors.Is(err, application.ErrConnectionTokenTunnelUnavailable):
		return managementFailure{status: 404, code: APIErrorCodeRESOURCENOTFOUND, message: "Tunnel 不存在"}
	case errors.Is(err, application.ErrTunnelManagementInUse):
		failure := managementFailure{status: 409, code: APIErrorCodeTUNNELINUSE, message: "Tunnel 仍被 Service 引用"}
		var inUse *application.TunnelInUseError
		if errors.As(err, &inUse) {
			details := APIError_Details{}
			_ = details.FromTunnelInUseDetails(TunnelInUseDetails{
				Type: TunnelInUseDetailsTypeTUNNELINUSE, ServiceCount: inUse.ServiceCount,
				ReferencingServiceIds: inUse.ReferencingServiceIDs, ReferencesTruncated: inUse.ReferencesTruncated,
			})
			failure.details = &details
		}
		return failure
	case errors.Is(err, application.ErrTunnelManagementLimit):
		return managementFailure{status: 429, code: APIErrorCodeRATELIMITED, message: "Tunnel 数量已达到配置上限"}
	case errors.Is(err, application.ErrConnectionTokenTunnelRevoked):
		return managementFailure{status: 409, code: APIErrorCodeTUNNELREVOKED, message: "Tunnel 已撤销"}
	case errors.Is(err, application.ErrConnectionTokenUnavailable), errors.Is(err, application.ErrCredentialLifecycleUnavailable):
		return managementFailure{status: 409, code: APIErrorCodeCONNECTIONTOKENUNAVAILABLE, message: "当前 Connection Token 不可用"}
	case errors.Is(err, application.ErrTunnelRuntimeConvergence):
		return managementFailure{status: 409, code: APIErrorCodeRUNTIMECONVERGENCEFAILED, message: "持久化状态已提交，但运行态尚未完全收敛"}
	case errors.Is(err, application.ErrTunnelManagementRuntimeConvergence):
		return managementFailure{status: 500, code: APIErrorCodeRUNTIMECONVERGENCEFAILED, message: "Tunnel 已删除，但运行态尚未完全收敛"}
	case errors.Is(err, application.ErrTunnelManagementInput), errors.Is(err, application.ErrCredentialLifecycleInput),
		errors.Is(err, application.ErrTunnelLifecycleInput):
		return managementFailure{status: 422, code: APIErrorCodeVALIDATIONFAILED, message: "请求内容不符合 Tunnel 约束"}
	default:
		handler.logInternalError(ctx, requestID, "management_tunnel_operation_failed", err)
		return managementFailure{status: 500, code: APIErrorCodeINTERNALERROR, message: "服务器内部错误"}
	}
}

func updateTunnelFailure(f managementFailure, requestID string) UpdateTunnelResponseObject {
	switch f.status {
	case 404:
		return UpdateTunnel404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 412:
		return UpdateTunnel412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	case 422:
		return UpdateTunnel422JSONResponse{ValidationFailedJSONResponse(f.response(requestID))}
	default:
		return UpdateTunnel500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func deleteTunnelFailure(f managementFailure, requestID string) DeleteTunnelResponseObject {
	switch f.status {
	case 404:
		return DeleteTunnel404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return DeleteTunnel409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	case 412:
		return DeleteTunnel412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	default:
		return DeleteTunnel500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func revealTunnelTokenFailure(f managementFailure, requestID string) RevealTunnelTokenResponseObject {
	switch f.status {
	case 404:
		return RevealTunnelToken404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return RevealTunnelToken409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	default:
		return RevealTunnelToken500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func rotateTunnelTokenFailure(f managementFailure, requestID string) RotateTunnelTokenResponseObject {
	switch f.status {
	case 404:
		return RotateTunnelToken404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return RotateTunnelToken409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	case 412:
		return RotateTunnelToken412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	default:
		return RotateTunnelToken500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func revokeTunnelTokenFailure(f managementFailure, requestID string) RevokeTunnelTokenResponseObject {
	switch f.status {
	case 404:
		return RevokeTunnelToken404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return RevokeTunnelToken409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	case 412:
		return RevokeTunnelToken412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	default:
		return RevokeTunnelToken500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}

func revokeTunnelFailure(f managementFailure, requestID string) RevokeTunnelResponseObject {
	switch f.status {
	case 404:
		return RevokeTunnel404JSONResponse{NotFoundJSONResponse(f.response(requestID))}
	case 409:
		return RevokeTunnel409JSONResponse{ConflictJSONResponse(f.response(requestID))}
	case 412:
		return RevokeTunnel412JSONResponse{PreconditionFailedJSONResponse(f.response(requestID))}
	default:
		return RevokeTunnel500JSONResponse{InternalErrorJSONResponse(f.response(requestID))}
	}
}
