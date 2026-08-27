package health

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
)

const maximumResponseHeaderBytes = 64 << 10

// checkTarget 对单个 Service 执行一次健康探测，并把结果归一化为 Scheduler 可消费的
// observation。连接目标、TLS 等拨号细节由 Snapshot 绑定的 OriginDialer 负责；这里仅
// 判断 TCP 是否成功建立，或 HTTP 响应头是否符合当前 Health Policy。
func checkTarget(ctx context.Context, spec targetSpec, dialer OriginDialer) observation {
	started := time.Now()
	// DialOrigin 会复用正常业务连接的 Origin 规则，因此 DNS、连接和 TLS 错误都属于
	// Origin 失败，并保留拨号层给出的稳定错误码。
	connection, code, err := dialer.DialOrigin(ctx, spec.serviceID)
	if err != nil {
		return observation{latency: time.Since(started), originCode: code, failure: FailureOrigin}
	}
	// 成功但没有返回连接违反 Dialer 契约。这里快速失败，避免后续读写触发空指针。
	if connection == nil {
		return observation{
			latency: time.Since(started), originCode: protocolv1.ErrorCode_ERROR_CODE_INTERNAL_ERROR,
			failure: FailureOrigin,
		}
	}
	// net.Conn 的 Read/Write 不会因为 Context 取消而自动返回。除设置 Deadline 外，
	// 还要在提前取消时主动关闭连接，保证 Worker 能及时退出且不会遗留 FD。
	defer connection.Close()
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = connection.SetDeadline(time.Now())
		_ = connection.Close()
	})
	defer stopCancellation()
	if deadline, exists := ctx.Deadline(); exists {
		if err := connection.SetDeadline(deadline); err != nil {
			return observation{latency: time.Since(started), originCode: protocolv1.ErrorCode_ERROR_CODE_ORIGIN_RESET, failure: FailureOrigin}
		}
	}
	// TCP 健康检查只关心完整的 Origin 拨号链是否建立成功，不发送应用层数据。
	if spec.healthType == protocolv1.HealthType_HEALTH_TYPE_TCP {
		return observation{success: true, latency: time.Since(started), originCode: protocolv1.ErrorCode_ERROR_CODE_OK}
	}

	// HTTP 路径在编译 Snapshot 时已校验；这里仍按请求目标解析，防止内部测试或未来
	// 调用方传入无法编码为 HTTP request-target 的值。
	parsed, err := url.ParseRequestURI(spec.path)
	if err != nil {
		return observation{latency: time.Since(started), failure: FailureHTTPProtocol}
	}
	// Request.Host 才是 Go 写出 Host 请求头的正式入口；Close=true 明确要求服务端
	// 响应后关闭连接，因为健康探测不会把这条连接放回连接池复用。
	request := &http.Request{
		Method: http.MethodGet, URL: parsed, Host: spec.hostHeader, Header: make(http.Header),
		Close: true, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}
	if err := request.Write(connection); err != nil {
		return ioFailure(ctx, started)
	}
	// 健康检查不消费响应体，只允许读取有限大小的响应头，避免异常 Origin 用超大
	// Header 长时间占用 Worker 或持续消耗内存。
	limited := io.LimitReader(connection, maximumResponseHeaderBytes+1)
	response, err := http.ReadResponse(bufio.NewReader(limited), request)
	if err != nil {
		// 超时属于 Origin 可用性问题；其他无法解析的响应则属于 HTTP 协议问题。
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ioFailure(ctx, started)
		}
		return observation{latency: time.Since(started), failure: FailureHTTPProtocol}
	}
	// Health 只读取响应头。先关闭 Socket，确保 Body.Close 不会尝试排空未知长度的
	// 响应体；健康结果已经确定，清理 Close 错误不应反向覆盖它。
	_ = connection.Close()
	_ = response.Body.Close()
	// 能读到合法 HTTP 响应不等于健康；最终还要应用 Snapshot 下发的状态码范围。
	if response.StatusCode < spec.minimumStatus || response.StatusCode > spec.maximumStatus {
		return observation{latency: time.Since(started), failure: FailureHTTPStatus}
	}
	return observation{success: true, latency: time.Since(started), originCode: protocolv1.ErrorCode_ERROR_CODE_OK}
}

// ioFailure 把请求读写失败统一映射为 Wire ErrorCode：只有 Context 的截止时间耗尽
// 才报告超时，其余连接中断、主动取消或 Socket 错误均按 Origin Reset 处理。
func ioFailure(ctx context.Context, started time.Time) observation {
	code := protocolv1.ErrorCode_ERROR_CODE_ORIGIN_RESET
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		code = protocolv1.ErrorCode_ERROR_CODE_ORIGIN_TIMEOUT
	}
	return observation{latency: time.Since(started), originCode: code, failure: FailureOrigin}
}
