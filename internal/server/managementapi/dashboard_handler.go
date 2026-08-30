package managementapi

import (
	"context"
	"errors"

	"github.com/lifei6671/xtunnel/internal/application"
	"github.com/oapi-codegen/nullable"
)

func (api *managementStrictAPI) GetDashboard(
	ctx context.Context,
	_ GetDashboardRequestObject,
) (GetDashboardResponseObject, error) {
	requestContext := managementRequestContextFrom(ctx)
	if requestContext == nil || api.handler.dashboard == nil {
		return GetDashboard500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestIDFromContext(requestContext),
		))}, nil
	}
	snapshot, err := api.handler.dashboard.Snapshot(ctx)
	if err != nil {
		api.handler.logInternalError(ctx, requestContext.requestID, "management_dashboard_failed", err)
		return GetDashboard500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID,
		))}, nil
	}
	response, err := dashboardResponse(snapshot)
	if err != nil {
		api.handler.logInternalError(ctx, requestContext.requestID, "management_dashboard_projection_failed", err)
		return GetDashboard500JSONResponse{InternalErrorJSONResponse(apiError(
			APIErrorCodeINTERNALERROR, "服务器内部错误", requestContext.requestID,
		))}, nil
	}
	return GetDashboard200JSONResponse(response), nil
}

func dashboardResponse(snapshot application.DashboardSnapshot) (Dashboard, error) {
	counts := make([]int, 0, 8)
	for _, value := range []uint64{
		snapshot.Counts.TunnelsTotal, snapshot.Counts.TunnelsOnline, snapshot.Counts.TunnelsOffline,
		snapshot.Counts.ConnectorsOnline, snapshot.Counts.ServicesTotal, snapshot.Counts.ServicesReady,
		snapshot.Counts.ServicesError, snapshot.Counts.ActiveConnections,
	} {
		converted, ok := managementCount(value)
		if !ok {
			return Dashboard{}, errors.New("dashboard count exceeds API integer range")
		}
		counts = append(counts, converted)
	}
	traffic := UsageSummary{
		Availability:      UsageSummaryAvailability(snapshot.Traffic.Availability),
		ConnectionsToday:  nullableInt64(snapshot.Traffic.ConnectionsToday),
		IngressBytesToday: nullableInt64(snapshot.Traffic.IngressBytesToday),
		EgressBytesToday:  nullableInt64(snapshot.Traffic.EgressBytesToday),
	}
	recentErrors := make([]RecentError, 0, len(snapshot.RecentErrors.Items))
	if len(snapshot.RecentErrors.Items) > 20 {
		return Dashboard{}, errors.New("dashboard recent errors exceed API item limit")
	}
	for _, item := range snapshot.RecentErrors.Items {
		code := RecentErrorCode(item.Code)
		if !code.Valid() || item.OccurredAt.IsZero() {
			return Dashboard{}, errors.New("dashboard recent error projection is invalid")
		}
		requestID := nullable.NewNullNullable[RequestID]()
		if item.RequestID != nil {
			requestID = nullable.NewNullableWithValue[RequestID](*item.RequestID)
		}
		recentErrors = append(recentErrors, RecentError{
			Code: code, Message: item.Message,
			OccurredAt: item.OccurredAt.UTC(), RequestId: requestID,
		})
	}
	return Dashboard{
		ServerStatus: DashboardServerStatus(snapshot.ServerStatus),
		Counts: DashboardCounts{
			TunnelsTotal: counts[0], TunnelsOnline: counts[1], TunnelsOffline: counts[2],
			ConnectorsOnline: counts[3], ServicesTotal: counts[4], ServicesReady: counts[5],
			ServicesError: counts[6], ActiveConnections: counts[7],
		},
		Traffic: traffic,
		RecentErrors: RecentErrorsSummary{
			Availability: RecentErrorsSummaryAvailability(snapshot.RecentErrors.Availability),
			Items:        recentErrors,
		},
		GeneratedAt: snapshot.GeneratedAt.UTC(),
	}, nil
}

func managementCount(value uint64) (int, bool) {
	maximum := uint64(^uint(0) >> 1)
	if value > maximum {
		return 0, false
	}
	return int(value), true
}

func nullableInt64(value *int64) nullable.Nullable[int64] {
	if value == nil {
		return nullable.NewNullNullable[int64]()
	}
	return nullable.NewNullableWithValue(*value)
}
