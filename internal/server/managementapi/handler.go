package managementapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/lifei6671/xtunnel/internal/identity"
	"github.com/lifei6671/xtunnel/internal/repository"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	webui "github.com/lifei6671/xtunnel/web"
)

const (
	adminSessionCookieName               = "xtunnel_admin_session"
	managementLoginMaxBody               = 64 << 10
	managementTunnelMaxBody              = 64 << 10
	loginPasswordVerificationConcurrency = 4
	loginBusyRetryAfterSeconds           = 1
)

type requestContextKey struct{}

type managementRequestContext struct {
	requestID string
	metadata  requestMetadata
	request   *http.Request
}

type authenticatedManagementRequestKey struct{}

type authenticatedManagementRequest struct {
	Session application.AdminAuthSession
	Audit   application.SecurityAuditContext
}

type managementValidationError struct{ message string }

func (err *managementValidationError) Error() string { return err.message }

// HandlerOptions 是 Management Handler 的固定生产依赖。认证状态只来自 SQLite，
// Host、Origin 与 Client IP 只来自同一份 Management Security Policy。
type HandlerOptions struct {
	Management      serverconfig.Management
	Store           repository.AdminAuthenticationStore
	Tunnels         *application.TunnelManagementService
	Credentials     *application.CredentialLifecycleService
	TunnelLifecycle *application.TunnelLifecycleService
	Logger          *slog.Logger
}

// ManagementHandler 拥有 Management API、内嵌 Web 和 Login 失败限流状态。
type ManagementHandler struct {
	auth                      *application.AdminAuthenticationService
	tunnels                   *application.TunnelManagementService
	credentials               *application.CredentialLifecycleService
	tunnelLifecycle           *application.TunnelLifecycleService
	security                  *managementSecurityPolicy
	limiter                   *loginFailureLimiter
	passwordVerificationSlots chan struct{}
	logger                    *slog.Logger
	api                       http.Handler
	web                       http.Handler
	index                     []byte
}

// NewHandler 构造同源 Management Handler。未知 API 在对应里程碑实现前稳定失败，
// 不能落入 SPA 或调用尚未实现的生成接口。
func NewHandler(options HandlerOptions) (*ManagementHandler, error) {
	if options.Store == nil || options.Logger == nil {
		return nil, errors.New("management store and logger are required")
	}
	security, err := newManagementSecurityPolicy(
		options.Management.PublicURL,
		options.Management.AllowedHosts,
		options.Management.TrustedProxies,
	)
	if err != nil {
		return nil, fmt.Errorf("construct management request security policy: %w", err)
	}
	assets, err := fs.Sub(webui.Dist, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded management web: %w", err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read embedded management index: %w", err)
	}
	handler := &ManagementHandler{
		auth:                      application.NewAdminAuthenticationService(options.Store),
		tunnels:                   options.Tunnels,
		credentials:               options.Credentials,
		tunnelLifecycle:           options.TunnelLifecycle,
		security:                  security,
		limiter:                   newLoginFailureLimiter(time.Now),
		passwordVerificationSlots: make(chan struct{}, loginPasswordVerificationConcurrency),
		logger:                    options.Logger,
		web:                       http.FileServer(http.FS(assets)),
		index:                     index,
	}
	strictAPI := NewStrictHandlerWithOptions(
		&managementStrictAPI{handler: handler},
		[]StrictMiddlewareFunc{handler.securityMiddleware},
		StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  handler.writeGeneratedRequestError,
			ResponseErrorHandlerFunc: handler.writeGeneratedResponseError,
		},
	)
	handler.api = HandlerWithOptions(strictAPI, StdHTTPServerOptions{
		BaseURL:          "/api/v1",
		ErrorHandlerFunc: handler.writeGeneratedRequestError,
	})
	return handler, nil
}

// ServeHTTP 先生成 Request ID 并完成可信代理、Host 和 Client IP 规范化，再路由到
// API 或 Web。后续认证与限流只消费这份不可变请求元数据。
func (handler *ManagementHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	requestID, err := identity.NewRequestID()
	if err != nil {
		handler.logger.ErrorContext(request.Context(), "management_request_id_failed", "error", err)
		http.Error(writer, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	metadata, err := handler.security.metadata(request)
	if err != nil {
		handler.writeError(writer, request, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST, "请求 Host 或代理元数据无效", requestID)
		return
	}
	ctx := &managementRequestContext{requestID: requestID, metadata: metadata, request: request}
	request = request.WithContext(context.WithValue(request.Context(), requestContextKey{}, ctx))
	ctx.request = request

	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/auth/login":
		if err := handler.prepareLoginRequest(writer, request); err != nil {
			handler.writeError(writer, request, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST, err.Error(), requestID)
			return
		}
		handler.api.ServeHTTP(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/api/v1/auth/me":
		handler.api.ServeHTTP(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/auth/logout":
		handler.api.ServeHTTP(writer, request)
	case handler.tunnelAPIReady() && isTunnelAPIPath(request.URL.Path):
		if err := handler.prepareTunnelRequest(writer, request); err != nil {
			var validationErr *managementValidationError
			if errors.As(err, &validationErr) {
				handler.writeError(writer, request, http.StatusUnprocessableEntity, APIErrorCodeVALIDATIONFAILED, validationErr.Error(), requestID)
				return
			}
			handler.writeError(writer, request, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST, err.Error(), requestID)
			return
		}
		handler.api.ServeHTTP(writer, request)
	case request.URL.Path == "/api/v1" || strings.HasPrefix(request.URL.Path, "/api/v1/auth/"):
		handler.api.ServeHTTP(writer, request)
	case strings.HasPrefix(request.URL.Path, "/api/v1/"):
		handler.writeError(writer, request, http.StatusInternalServerError, APIErrorCodeINTERNALERROR, "接口尚未实现", requestID)
	default:
		handler.serveWeb(writer, request)
	}
}

func (handler *ManagementHandler) prepareLoginRequest(writer http.ResponseWriter, request *http.Request) error {
	contentType, present, err := managementHeader(request.Header, "Content-Type")
	if err != nil || !present {
		return errors.New("Content-Type 必须为 application/json")
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		return errors.New("Content-Type 必须为 application/json")
	}
	request.Body = http.MaxBytesReader(writer, request.Body, managementLoginMaxBody)
	bodyBytes, err := io.ReadAll(request.Body)
	if err != nil {
		return errors.New("登录请求体无效")
	}
	decoder := json.NewDecoder(bytes.NewReader(bodyBytes))
	decoder.DisallowUnknownFields()
	var body LoginRequest
	if err := decoder.Decode(&body); err != nil {
		return errors.New("登录请求体无效")
	}
	if err := ensureJSONEOF(decoder); err != nil || strings.TrimSpace(body.Username) == "" || body.Password == nil || *body.Password == "" {
		return errors.New("登录请求体无效")
	}
	request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return nil
}

func (handler *ManagementHandler) tunnelAPIReady() bool {
	return handler.tunnels != nil && handler.credentials != nil && handler.tunnelLifecycle != nil
}

func isTunnelAPIPath(path string) bool {
	return path == "/api/v1/tunnels" || strings.HasPrefix(path, "/api/v1/tunnels/")
}

// prepareTunnelRequest 在生成 Decoder 前收紧有 Body 的两个 Tunnel Operation。
// 生成物负责最终绑定；这里补足 unknown field、尾随 JSON、媒体类型与 Merge Patch
// null/省略的边界，随后恢复 Body 供生成 Handler 读取。
func (handler *ManagementHandler) prepareTunnelRequest(writer http.ResponseWriter, request *http.Request) error {
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/api/v1/tunnels":
		var body CreateTunnelRequest
		if err := prepareManagementJSON(writer, request, managementTunnelMaxBody, "application/json", &body); err != nil {
			return err
		}
		if strings.TrimSpace(body.Name) == "" {
			return &managementValidationError{message: "Tunnel 名称不能为空"}
		}
	case request.Method == http.MethodPatch && isSingleTunnelResourcePath(request.URL.Path):
		var body struct {
			Name json.RawMessage `json:"name"`
		}
		if err := prepareManagementJSON(writer, request, managementTunnelMaxBody, "application/merge-patch+json", &body); err != nil {
			return err
		}
		if len(body.Name) == 0 || bytes.Equal(bytes.TrimSpace(body.Name), []byte("null")) {
			return &managementValidationError{message: "Tunnel 更新必须提供非空 name"}
		}
		var name string
		if err := json.Unmarshal(body.Name, &name); err != nil || strings.TrimSpace(name) == "" {
			return &managementValidationError{message: "Tunnel 更新必须提供非空 name"}
		}
	}
	return nil
}

func isSingleTunnelResourcePath(path string) bool {
	const prefix = "/api/v1/tunnels/"
	return strings.HasPrefix(path, prefix) && !strings.Contains(strings.TrimPrefix(path, prefix), "/")
}

func prepareManagementJSON(
	writer http.ResponseWriter,
	request *http.Request,
	maxBody int64,
	expectedMediaType string,
	target any,
) error {
	contentType, present, err := managementHeader(request.Header, "Content-Type")
	if err != nil || !present {
		return fmt.Errorf("Content-Type 必须为 %s", expectedMediaType)
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != expectedMediaType {
		return fmt.Errorf("Content-Type 必须为 %s", expectedMediaType)
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxBody)
	bodyBytes, err := io.ReadAll(request.Body)
	if err != nil {
		return errors.New("请求体无效")
	}
	decoder := json.NewDecoder(bytes.NewReader(bodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("请求体无效")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return errors.New("请求体无效")
	}
	request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return nil
}

// securityMiddleware 对所有已放行的 Management Operation 只认证一次。Mutation
// 在调用 Application owner 前统一校验同源 Origin 与 CSRF；Reveal GET 只写审计，
// 仍遵循冻结 OpenAPI，不额外要求 CSRF。
func (handler *ManagementHandler) securityMiddleware(next StrictHandlerFunc, operationID string) StrictHandlerFunc {
	return func(ctx context.Context, writer http.ResponseWriter, request *http.Request, body any) (any, error) {
		if operationID == "Login" {
			return next(ctx, writer, request, body)
		}
		requestContext := managementRequestContextFrom(ctx)
		if requestContext == nil {
			handler.writeError(writer, request, http.StatusInternalServerError, APIErrorCodeINTERNALERROR, "服务器内部错误", "")
			return nil, nil
		}
		token, ok := singleAdminSessionCookie(request)
		if !ok {
			handler.writeError(writer, request, http.StatusUnauthorized, APIErrorCodeSESSIONEXPIRED, "管理员会话无效或已过期", requestContext.requestID)
			return nil, nil
		}
		session, err := handler.auth.Authenticate(ctx, token)
		if errors.Is(err, application.ErrAdminSessionExpired) {
			handler.writeError(writer, request, http.StatusUnauthorized, APIErrorCodeSESSIONEXPIRED, "管理员会话无效或已过期", requestContext.requestID)
			return nil, nil
		}
		if err != nil {
			handler.logInternalError(ctx, requestContext.requestID, "management_session_lookup_failed", err)
			handler.writeError(writer, request, http.StatusInternalServerError, APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID)
			return nil, nil
		}
		if isStateChangingOperation(operationID) {
			origin, present, err := managementHeader(request.Header, "Origin")
			if err != nil || !present || !handler.security.allowsOrigin(origin) {
				handler.writeError(writer, request, http.StatusForbidden, APIErrorCodeORIGINNOTALLOWED, "请求 Origin 不被允许", requestContext.requestID)
				return nil, nil
			}
			provided, present, err := managementHeader(request.Header, "X-XTunnel-CSRF")
			if err != nil || !present || !validCSRF(provided, session.CSRFToken) {
				handler.writeError(writer, request, http.StatusForbidden, APIErrorCodeCSRFINVALID, "CSRF Token 无效", requestContext.requestID)
				return nil, nil
			}
		}
		authenticated := &authenticatedManagementRequest{
			Session: session,
			Audit: application.SecurityAuditContext{
				ActorID: session.Admin.ID, SourceIP: requestContext.metadata.clientIP.String(),
				RequestID: requestContext.requestID,
			},
		}
		return next(context.WithValue(ctx, authenticatedManagementRequestKey{}, authenticated), writer, request, body)
	}
}

func isStateChangingOperation(operationID string) bool {
	switch operationID {
	case "Logout", "CreateTunnel", "UpdateTunnel", "DeleteTunnel", "RevokeTunnel", "RotateTunnelToken", "RevokeTunnelToken":
		return true
	default:
		return false
	}
}

func authenticatedManagementRequestFrom(ctx context.Context) (*authenticatedManagementRequest, bool) {
	value, ok := ctx.Value(authenticatedManagementRequestKey{}).(*authenticatedManagementRequest)
	return value, ok && value != nil
}

// managementStrictAPI 实现已经进入当前里程碑的生成 Operation；其余 Operation
// 仍由 ManagementHandler 的 API Fence 稳定拒绝，不能调用嵌入的 nil 接口。
type managementStrictAPI struct {
	StrictServerInterface
	handler *ManagementHandler
}

func (api *managementStrictAPI) Login(ctx context.Context, request LoginRequestObject) (LoginResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil || request.Body == nil || request.Body.Password == nil {
		return Login500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext)))}, nil
	}
	if !api.handler.security.allowsOrigin(request.Params.Origin) {
		return Login403JSONResponse{ForbiddenJSONResponse(apiError(APIErrorCodeORIGINNOTALLOWED, "请求 Origin 不被允许", requestContext.requestID))}, nil
	}
	username := request.Body.Username
	if retryAfter := api.handler.limiter.RetryAfter(requestContext.metadata.clientIP, username); retryAfter > 0 {
		return Login429JSONResponse{RateLimitedJSONResponse{
			Body:    apiError(APIErrorCodeRATELIMITED, "登录尝试过于频繁", requestContext.requestID),
			Headers: RateLimitedResponseHeaders{RetryAfter: &retryAfter},
		}}, nil
	}
	if !api.handler.tryAcquirePasswordVerification() {
		retryAfter := loginBusyRetryAfterSeconds
		return Login429JSONResponse{RateLimitedJSONResponse{
			Body:    apiError(APIErrorCodeRATELIMITED, "登录验证资源正忙", requestContext.requestID),
			Headers: RateLimitedResponseHeaders{RetryAfter: &retryAfter},
		}}, nil
	}
	defer api.handler.releasePasswordVerification()

	session, err := api.handler.auth.Login(ctx, username, *request.Body.Password)
	switch {
	case errors.Is(err, application.ErrAdminSetupRequired):
		response := SetupRequiredErrorResponse{}
		response.Error.Code = APIErrorCodeSETUPREQUIRED
		response.Error.Message = "请先通过本机命令创建管理员"
		response.Error.RequestId = requestContext.requestID
		return Login409JSONResponse{SetupRequiredJSONResponse(response)}, nil
	case errors.Is(err, application.ErrAdminAuthenticationFailed):
		api.handler.limiter.RecordFailure(requestContext.metadata.clientIP, username)
		return Login401JSONResponse{UnauthorizedJSONResponse(apiError(APIErrorCodeAUTHENTICATIONFAILED, "用户名或密码错误", requestContext.requestID))}, nil
	case err != nil:
		api.handler.logInternalError(ctx, requestContext.requestID, "management_login_failed", err)
		return Login500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID))}, nil
	}
	api.handler.limiter.RecordSuccess(requestContext.metadata.clientIP, username)
	cookie := adminSessionCookie(session.SessionToken, session.ExpiresAt)
	cacheControl, pragma, setCookie := "no-store", "no-cache", cookie.String()
	return Login200JSONResponse{
		Body: authSessionResponse(session),
		Headers: Login200ResponseHeaders{
			CacheControl: &cacheControl,
			Pragma:       &pragma,
			SetCookie:    &setCookie,
		},
	}, nil
}

func (handler *ManagementHandler) tryAcquirePasswordVerification() bool {
	select {
	case handler.passwordVerificationSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (handler *ManagementHandler) releasePasswordVerification() {
	<-handler.passwordVerificationSlots
}

func (api *managementStrictAPI) GetCurrentAdmin(ctx context.Context, _ GetCurrentAdminRequestObject) (GetCurrentAdminResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil {
		return GetCurrentAdmin500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", ""))}, nil
	}
	authenticated, ok := authenticatedManagementRequestFrom(ctx)
	if !ok {
		return GetCurrentAdmin500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID))}, nil
	}
	cacheControl, pragma := "no-store", "no-cache"
	return GetCurrentAdmin200JSONResponse{
		Body: authSessionResponse(authenticated.Session),
		Headers: GetCurrentAdmin200ResponseHeaders{
			CacheControl: &cacheControl,
			Pragma:       &pragma,
		},
	}, nil
}

func (api *managementStrictAPI) Logout(ctx context.Context, _ LogoutRequestObject) (LogoutResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil {
		return Logout500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", ""))}, nil
	}
	authenticated, ok := authenticatedManagementRequestFrom(ctx)
	if !ok {
		return Logout500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID))}, nil
	}
	if err := api.handler.auth.Logout(ctx, authenticated.Session.SessionID); err != nil {
		if errors.Is(err, application.ErrAdminSessionExpired) {
			return Logout401JSONResponse{UnauthorizedJSONResponse(apiError(APIErrorCodeSESSIONEXPIRED, "管理员会话无效或已过期", requestContext.requestID))}, nil
		}
		api.handler.logInternalError(ctx, requestContext.requestID, "management_logout_failed", err)
		return Logout500JSONResponse{InternalErrorJSONResponse(apiError(APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID))}, nil
	}
	cookie := expiredAdminSessionCookie().String()
	return Logout204Response{Headers: Logout204ResponseHeaders{SetCookie: &cookie}}, nil
}

func authSessionResponse(session application.AdminAuthSession) AuthSession {
	return AuthSession{
		Admin:     Admin{Id: session.Admin.ID, Username: session.Admin.Username},
		CsrfToken: session.CSRFToken,
		ExpiresAt: session.ExpiresAt,
	}
}

func managementRequestContextFrom(ctx context.Context) *managementRequestContext {
	value, _ := ctx.Value(requestContextKey{}).(*managementRequestContext)
	return value
}

func requestIDFromContext(value *managementRequestContext) string {
	if value == nil {
		return ""
	}
	return value.requestID
}

func apiError(code APIErrorCode, message, requestID string) ErrorResponse {
	return ErrorResponse{Error: APIError{Code: code, Message: message, RequestId: requestID}}
}

func (handler *ManagementHandler) writeGeneratedRequestError(writer http.ResponseWriter, request *http.Request, err error) {
	requestContext := managementRequestContextFrom(request.Context())
	var requiredHeader *RequiredHeaderError
	if errors.As(err, &requiredHeader) && strings.EqualFold(requiredHeader.ParamName, "If-Match") {
		handler.writeError(writer, request, http.StatusPreconditionRequired, APIErrorCodePRECONDITIONREQUIRED, "请求缺少 If-Match", requestIDFromContext(requestContext))
		return
	}
	if errors.As(err, &requiredHeader) && strings.EqualFold(requiredHeader.ParamName, "Origin") {
		handler.writeError(writer, request, http.StatusForbidden, APIErrorCodeORIGINNOTALLOWED, "请求 Origin 不被允许", requestIDFromContext(requestContext))
		return
	}
	var invalidFormat *InvalidParamFormatError
	var tooMany *TooManyValuesForParamError
	if (errors.As(err, &invalidFormat) && strings.EqualFold(invalidFormat.ParamName, "If-Match")) ||
		(errors.As(err, &tooMany) && strings.EqualFold(tooMany.ParamName, "If-Match")) {
		handler.writeError(writer, request, http.StatusBadRequest, APIErrorCodeINVALIDIFMATCH, "If-Match 无效", requestIDFromContext(requestContext))
		return
	}
	handler.writeError(writer, request, http.StatusBadRequest, APIErrorCodeINVALIDREQUEST, "请求参数无效", requestIDFromContext(requestContext))
}

func (handler *ManagementHandler) writeGeneratedResponseError(writer http.ResponseWriter, request *http.Request, err error) {
	requestContext := managementRequestContextFrom(request.Context())
	requestID := requestIDFromContext(requestContext)
	handler.logInternalError(request.Context(), requestID, "management_response_failed", err)
	handler.writeError(writer, request, http.StatusInternalServerError, APIErrorCodeINTERNALERROR, "服务器内部错误", requestID)
}

func (handler *ManagementHandler) logInternalError(ctx context.Context, requestID, event string, err error) {
	handler.logger.ErrorContext(ctx, event, "request_id", requestID, "error", err)
}

func (handler *ManagementHandler) writeError(writer http.ResponseWriter, request *http.Request, status int, code APIErrorCode, message, requestID string) {
	writer.Header().Set("Cache-Control", "no-store")
	handler.writeJSON(writer, status, ErrorResponse{Error: APIError{
		Code: code, Message: message, RequestId: requestID,
	}})
}

func (handler *ManagementHandler) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		handler.logger.Error("management_response_encode_failed", "error", err)
	}
}

func expiredAdminSessionCookie() *http.Cookie {
	cookie := adminSessionCookie("", time.Unix(1, 0).UTC())
	cookie.MaxAge = -1
	return cookie
}

func (handler *ManagementHandler) serveWeb(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if strings.HasPrefix(request.URL.Path, "/assets/") {
		handler.web.ServeHTTP(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodGet {
		_, _ = writer.Write(handler.index)
	}
}

func adminSessionCookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: adminSessionCookieName, Value: value, Path: "/api/v1",
		Expires: expires, MaxAge: 12 * 60 * 60,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}
}

func singleAdminSessionCookie(request *http.Request) (string, bool) {
	var value string
	count := 0
	for _, cookie := range request.Cookies() {
		if cookie.Name == adminSessionCookieName {
			value = cookie.Value
			count++
		}
	}
	return value, count == 1 && value != ""
}

func validCSRF(provided, expected string) bool {
	providedBytes, err := base64.RawURLEncoding.DecodeString(provided)
	if err != nil || len(providedBytes) != 32 {
		return false
	}
	expectedBytes, err := base64.RawURLEncoding.DecodeString(expected)
	if err != nil || len(expectedBytes) != 32 {
		return false
	}
	return subtle.ConstantTimeCompare(providedBytes, expectedBytes) == 1
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}
