package managementapi

import (
	"context"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lifei6671/xtunnel/internal/repository"
	"github.com/oapi-codegen/nullable"
)

const securityAuditPageResource = "security_audit_events"

func (api *managementStrictAPI) ListSecurityAuditEvents(
	ctx context.Context,
	request ListSecurityAuditEventsRequestObject,
) (ListSecurityAuditEventsResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil || api.handler.securityAudits == nil {
		return ListSecurityAuditEvents500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext),
		))}, nil
	}
	query, scope, emptyRange, err := securityAuditQuery(request.Params, requestContext.request.URL.Query())
	if err != nil {
		return ListSecurityAuditEvents400JSONResponse{BadRequestJSONResponse(apiError(
			APIErrorCodeINVALIDREQUEST, "请求参数无效", requestContext.requestID,
		))}, nil
	}
	if request.Params.PageToken != nil {
		if emptyRange {
			return ListSecurityAuditEvents400JSONResponse{BadRequestJSONResponse(apiError(
				APIErrorCodeINVALIDPAGETOKEN, "page_token 无效", requestContext.requestID,
			))}, nil
		}
		payload, decodeErr := api.handler.pageTokens.decodeAudit(*request.Params.PageToken, scope)
		if decodeErr != nil {
			failure := paginationFailure(decodeErr)
			return ListSecurityAuditEvents400JSONResponse{BadRequestJSONResponse(apiError(
				failure.code, failure.message, requestContext.requestID,
			))}, nil
		}
		query.After = &repository.SecurityAuditEventCursor{OccurredAt: payload.OccurredAt, EventID: payload.LastID}
	}
	if emptyRange {
		return ListSecurityAuditEvents200JSONResponse(SecurityAuditEventList{Items: []SecurityAuditEvent{}}), nil
	}
	page, err := api.handler.securityAudits.Query(ctx, query)
	if errors.Is(err, repository.ErrInvalidSecurityAuditEventQuery) {
		return ListSecurityAuditEvents400JSONResponse{BadRequestJSONResponse(apiError(
			APIErrorCodeINVALIDREQUEST, "请求参数无效", requestContext.requestID,
		))}, nil
	}
	if err != nil {
		api.handler.logInternalError(ctx, requestContext.requestID, "management_security_audit_query_failed", err)
		return ListSecurityAuditEvents500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID,
		))}, nil
	}
	items := make([]SecurityAuditEvent, 0, len(page.Events))
	for _, event := range page.Events {
		items = append(items, securityAuditEventResponse(event))
	}
	response := SecurityAuditEventList{Items: items}
	if page.Next != nil {
		next, encodeErr := api.handler.pageTokens.encodeAudit(scope, page.Next.OccurredAt, page.Next.EventID)
		if encodeErr != nil {
			api.handler.logInternalError(ctx, requestContext.requestID, "management_security_audit_cursor_failed", encodeErr)
			return ListSecurityAuditEvents500JSONResponse{InternalErrorJSONResponse(apiError(
				APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID,
			))}, nil
		}
		response.NextPageToken = &next
	}
	return ListSecurityAuditEvents200JSONResponse(response), nil
}

func securityAuditQuery(params ListSecurityAuditEventsParams, rawQuery url.Values) (
	repository.SecurityAuditEventQuery,
	pageTokenScope,
	bool,
	error,
) {
	query := repository.SecurityAuditEventQuery{Limit: defaultPageSize}
	if params.PageSize != nil {
		query.Limit = *params.PageSize
	}
	if params.Action != nil {
		query.Action = string(*params.Action)
	}
	if params.Result != nil {
		query.Result = string(*params.Result)
	}
	if params.ResourceType != nil {
		query.ResourceType = string(*params.ResourceType)
	}
	if params.ResourceId != nil {
		if !utf8.ValidString(*params.ResourceId) || utf8.RuneCountInString(*params.ResourceId) < 1 ||
			utf8.RuneCountInString(*params.ResourceId) > 256 {
			return repository.SecurityAuditEventQuery{}, pageTokenScope{}, false, repository.ErrInvalidSecurityAuditEventQuery
		}
		query.ResourceID = *params.ResourceId
	}
	var fromFilter, toFilter string
	if params.OccurredFrom != nil {
		if !validSecurityAuditDateTimeParameter(rawQuery, "occurred_from") {
			return repository.SecurityAuditEventQuery{}, pageTokenScope{}, false, repository.ErrInvalidSecurityAuditEventQuery
		}
		value, err := securityAuditBoundary(*params.OccurredFrom)
		if err != nil {
			return repository.SecurityAuditEventQuery{}, pageTokenScope{}, false, err
		}
		query.OccurredFrom = &value
		fromFilter = strconv.FormatInt(value, 10)
	}
	if params.OccurredTo != nil {
		if !validSecurityAuditDateTimeParameter(rawQuery, "occurred_to") {
			return repository.SecurityAuditEventQuery{}, pageTokenScope{}, false, repository.ErrInvalidSecurityAuditEventQuery
		}
		value, err := securityAuditBoundary(*params.OccurredTo)
		if err != nil {
			return repository.SecurityAuditEventQuery{}, pageTokenScope{}, false, err
		}
		query.OccurredTo = &value
		toFilter = strconv.FormatInt(value, 10)
	}
	if params.OccurredFrom != nil && params.OccurredTo != nil && !params.OccurredFrom.Before(*params.OccurredTo) {
		return repository.SecurityAuditEventQuery{}, pageTokenScope{}, false, repository.ErrInvalidSecurityAuditEventQuery
	}
	scope := pageTokenScope{
		resource: securityAuditPageResource,
		filter:   pageFilter(query.Action, query.Result, query.ResourceType, query.ResourceID, fromFilter, toFilter),
	}
	emptyRange := query.OccurredFrom != nil && query.OccurredTo != nil && *query.OccurredFrom >= *query.OccurredTo
	if !emptyRange {
		if err := query.Validate(); err != nil {
			return repository.SecurityAuditEventQuery{}, pageTokenScope{}, false, err
		}
	}
	return query, scope, emptyRange, nil
}

// securityAuditBoundary 将 RFC3339 时刻向上取整到 SQLite 的 Unix 秒精度，
// 从而同时保持 from-inclusive 与 to-exclusive 的真实时间语义。
func securityAuditBoundary(value time.Time) (int64, error) {
	_, offset := value.Zone()
	if offset != 0 {
		return 0, repository.ErrInvalidSecurityAuditEventQuery
	}
	seconds := value.Unix()
	if value.Nanosecond() != 0 {
		seconds++
	}
	return seconds, nil
}

func validSecurityAuditDateTimeParameter(rawQuery url.Values, name string) bool {
	values, exists := rawQuery[name]
	if !exists || len(values) != 1 || !strings.HasSuffix(values[0], "Z") {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, values[0])
	return err == nil
}

func securityAuditEventResponse(event repository.SecurityAuditEvent) SecurityAuditEvent {
	return SecurityAuditEvent{
		EventId:           event.EventID,
		OperationId:       event.OperationID,
		Event:             SecurityAuditEventEvent(event.Event),
		Action:            SecurityAuditAction(event.Action),
		ActorType:         SecurityAuditEventActorType(event.ActorType),
		ActorId:           auditNullable[AdminID](event.ActorID),
		SourceIp:          auditNullable[string](event.SourceIP),
		ResourceType:      SecurityAuditResourceType(event.ResourceType),
		ResourceId:        event.ResourceID,
		Result:            SecurityAuditResult(event.Result),
		ErrorCode:         auditNullable[string](event.ErrorCode),
		RequestId:         auditNullable[string](event.RequestID),
		TraceId:           auditNullable[string](event.TraceID),
		BeforeStateDigest: auditDigest(event.BeforeStateDigest),
		AfterStateDigest:  auditDigest(event.AfterStateDigest),
		OccurredAt:        time.Unix(event.OccurredAt, 0).UTC(),
	}
}

func auditNullable[T ~string](value string) nullable.Nullable[T] {
	if value == "" {
		return nullable.NewNullNullable[T]()
	}
	return nullable.NewNullableWithValue(T(value))
}

func auditDigest(value []byte) nullable.Nullable[string] {
	if len(value) == 0 {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(hex.EncodeToString(value))
}
