package bootstrap

import (
	"fmt"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	"github.com/lifei6671/xtunnel/internal/protocol/validate"
	serverrecenterror "github.com/lifei6671/xtunnel/internal/server/recenterror"
	serverruntime "github.com/lifei6671/xtunnel/internal/server/runtime"
)

// serverDiagnosticsBridge 是生产数据面与固定五槽 Latest Projection 之间的唯一映射。
// 它只接受最终逻辑 OPEN 和完成 generation fencing 的 Connector lifecycle；不解析
// 日志/Metric，也不接收底层 error、Origin 或资源标识。
type serverDiagnosticsBridge struct {
	owner       *serverrecenterror.Owner
	now         func() time.Time
	reportError func(error)
}

// ObserveOpen 每次公网逻辑 OPEN 最多发布一次。OK、取消、认证、配置同步、
// OPEN_DRAINING、VERSION_UNSUPPORTED 与 INTERNAL_ERROR 都不属于五类 Dashboard 诊断。
func (bridge *serverDiagnosticsBridge) ObserveOpen(code protocolv1.ErrorCode, requestID string) {
	if bridge == nil || bridge.owner == nil || bridge.now == nil {
		return
	}
	dashboardCode, observed := dashboardCodeForProtocol(code)
	if !observed {
		return
	}
	var requestIDValue *string
	if validate.ValidateID(requestID, "req_") == nil {
		cloned := requestID
		requestIDValue = &cloned
	}
	bridge.publish(serverrecenterror.Record{
		Code: dashboardCode, OccurredAt: bridge.now().UTC(), RequestID: requestIDValue,
	})
}

// ObserveConnectorLifecycle 只把非预期 Current Connector 断开投影为类别级诊断。
// Revoke、Delete、Server Shutdown 与 replacement 都是显式收敛动作，不得制造告警；
// 旧 generation 的 cleanup 已由 Registry 在产生事件前拒绝。
func (bridge *serverDiagnosticsBridge) ObserveConnectorLifecycle(event serverruntime.ConnectorLifecycleEvent) {
	if bridge == nil || bridge.owner == nil || bridge.now == nil ||
		event.Name != serverruntime.ConnectorEventDisconnected ||
		event.WasDraining ||
		expectedConnectorDisconnect(event.Reason) {
		return
	}
	bridge.publish(serverrecenterror.Record{
		Code: serverrecenterror.CodeConnectorOffline, OccurredAt: bridge.now().UTC(),
	})
	if event.TunnelBecameOffline {
		bridge.publish(serverrecenterror.Record{
			Code: serverrecenterror.CodeTunnelOffline, OccurredAt: bridge.now().UTC(),
		})
	}
}

func (bridge *serverDiagnosticsBridge) publish(record serverrecenterror.Record) {
	if err := bridge.owner.Publish(record); err != nil && bridge.reportError != nil {
		bridge.reportError(fmt.Errorf("publish recent error projection: %w", err))
	}
}

func dashboardCodeForProtocol(code protocolv1.ErrorCode) (serverrecenterror.Code, bool) {
	switch code {
	case protocolv1.ErrorCode_ERROR_CODE_ORIGIN_REFUSED,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_UNREACHABLE,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_RESET,
		protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TLS_ERROR:
		return serverrecenterror.CodeOriginDown, true
	case protocolv1.ErrorCode_ERROR_CODE_WORK_POOL_EXHAUSTED,
		protocolv1.ErrorCode_ERROR_CODE_CONNECTOR_BUSY,
		protocolv1.ErrorCode_ERROR_CODE_HEALTH_BUDGET_EXCEEDED,
		protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED:
		return serverrecenterror.CodeNoCapacity, true
	case protocolv1.ErrorCode_ERROR_CODE_PROTOCOL_ERROR:
		return serverrecenterror.CodeProtocolError, true
	default:
		return "", false
	}
}

func expectedConnectorDisconnect(reason string) bool {
	switch reason {
	case "server_shutdown", "tunnel_revoked", "tunnel_deleted", "session_replaced":
		return true
	default:
		return false
	}
}
