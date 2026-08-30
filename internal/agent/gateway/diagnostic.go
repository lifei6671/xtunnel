package gateway

import (
	"context"
	"net/netip"
	"time"

	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
	servergateway "github.com/lifei6671/xtunnel/internal/server/gateway"
)

const certificateWarningWindow = 30 * 24 * time.Hour

// DiagnosticStatus 是连接诊断单个阶段的稳定结论。
type DiagnosticStatus string

const (
	DiagnosticPass    DiagnosticStatus = "PASS"
	DiagnosticWarning DiagnosticStatus = "WARNING"
	DiagnosticFail    DiagnosticStatus = "FAIL"
)

// DiagnosticSummary 是一次无副作用 Precheck 的稳定汇总结论。
type DiagnosticSummary string

const (
	DiagnosticReady         DiagnosticSummary = "READY"
	DiagnosticReadyDegraded DiagnosticSummary = "READY_DEGRADED"
	DiagnosticNotReady      DiagnosticSummary = "NOT_READY"
)

// DiagnosticStep 描述一个不含 Endpoint、Token 或底层错误文本的诊断阶段。
type DiagnosticStep struct {
	Stage   string
	Status  DiagnosticStatus
	Message string
}

// DiagnosticResult 包含按执行顺序排列的阶段结果和汇总结论。
type DiagnosticResult struct {
	Steps   []DiagnosticStep
	Summary DiagnosticSummary
}

// Diagnose 使用生产 Token Parser、TCP Dialer、TLS/Trust Builder 和精确 ALPN
// 路径验证 Control 与 Work 接入。它只进行 Precheck，不发送 Auth 或 Snapshot 消息。
func Diagnose(ctx context.Context, connectionTokenText string) DiagnosticResult {
	return diagnose(ctx, connectionTokenText, dialDependencies{}, time.Now)
}

func diagnose(
	ctx context.Context,
	connectionTokenText string,
	dependencies dialDependencies,
	now func() time.Time,
) DiagnosticResult {
	diagnosticContext, cancel := context.WithTimeout(ctx, gatewayDialTimeout)
	defer cancel()

	result := DiagnosticResult{Summary: DiagnosticReady}
	connectionToken, err := connectiontoken.Parse(connectionTokenText)
	if err != nil {
		result.append("TOKEN", DiagnosticFail, "connection token is invalid")
		result.Summary = DiagnosticNotReady
		return result
	}
	result.append("TOKEN", DiagnosticPass, "connection token is valid")
	if _, err := netip.ParseAddr(connectionToken.GetEndpoint().GetHost()); err == nil {
		result.append("ENDPOINT", DiagnosticPass, "IP endpoint is valid")
	} else {
		result.append("ENDPOINT", DiagnosticPass, "hostname endpoint is valid")
	}

	checks := []struct {
		prefix string
		alpn   string
	}{
		{prefix: "CONTROL", alpn: servergateway.ControlALPN},
		{prefix: "WORK", alpn: servergateway.WorkALPN},
	}
	for _, check := range checks {
		observer := func(observation phaseResult) {
			result.appendObservation(check.prefix, observation, now())
		}
		connection, err := dialParsedToken(diagnosticContext, connectionToken, check.alpn, observer, dependencies)
		if connection != nil {
			// Precheck 不发送应用层消息；Close 的唯一目的为释放本次探测资源。
			// 写 Deadline 继承同一个总预算，避免 close_notify 把 10 秒上限拖长。
			if deadline, ok := diagnosticContext.Deadline(); ok {
				_ = connection.SetWriteDeadline(deadline)
			}
			_ = connection.Close()
		}
		if err != nil {
			result.Summary = DiagnosticNotReady
			return result
		}
	}
	return result
}

func (result *DiagnosticResult) appendObservation(prefix string, observation phaseResult, now time.Time) {
	stage := prefix + "_"
	switch observation.phase {
	case dialPhaseDNS:
		stage += "DNS"
		if !observation.passed {
			result.append(stage, DiagnosticFail, "gateway hostname resolution failed")
		} else if observation.literal {
			result.append(stage, DiagnosticPass, "IP literal does not require DNS lookup")
		} else {
			result.append(stage, DiagnosticPass, "gateway hostname resolution completed")
		}
	case dialPhaseTCP:
		stage += "TCP"
		result.appendPassedOrFailed(stage, observation.passed, "TCP connection established", "TCP connection failed")
	case dialPhaseTLS:
		stage += "TLS"
		result.appendPassedOrFailed(stage, observation.passed, "TLS 1.3 handshake completed", "TLS handshake failed")
	case dialPhaseTrust:
		stage += "TRUST"
		if !observation.passed {
			result.append(stage, DiagnosticFail, "gateway certificate verification failed")
			return
		}
		message := "public CA certificate verified"
		if observation.trust == "pinned_spki" {
			message = "pinned SPKI certificate verified"
		}
		if !observation.notAfter.After(now.Add(certificateWarningWindow)) {
			result.append(stage, DiagnosticWarning, message+"; certificate expires within 30 days")
			if result.Summary == DiagnosticReady {
				result.Summary = DiagnosticReadyDegraded
			}
			return
		}
		result.append(stage, DiagnosticPass, message)
	case dialPhaseALPN:
		stage += "ALPN"
		result.appendPassedOrFailed(stage, observation.passed, prefix+" ALPN negotiated", prefix+" ALPN negotiation failed")
	}
}

func (result *DiagnosticResult) appendPassedOrFailed(stage string, passed bool, passMessage, failMessage string) {
	if passed {
		result.append(stage, DiagnosticPass, passMessage)
		return
	}
	result.append(stage, DiagnosticFail, failMessage)
}

func (result *DiagnosticResult) append(stage string, status DiagnosticStatus, message string) {
	result.Steps = append(result.Steps, DiagnosticStep{Stage: stage, Status: status, Message: message})
}
