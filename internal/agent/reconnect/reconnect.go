// Package reconnect 实现 Agent Control Session 的有抖动指数退避。
package reconnect

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
	"github.com/lifei6671/xtunnel/internal/logging"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
)

var (
	// ErrInvalidOptions 表示重连器缺少 Starter/Handler 或退避参数不合法。
	ErrInvalidOptions = errors.New("agent reconnect options are invalid")
)

// Session 是重连器结束一代网络生命周期所需的最小能力。
type Session interface {
	Close()
	Wait() error
}

// Starter 由同一个 Connector Runner 满足；每次 StartDetached 只创建一条新
// Control Session。泛型保留具体 Session 类型，避免重连层反向依赖 Connector 业务接口。
type Starter[S Session] interface {
	StartDetached(context.Context) (S, error)
}

// SessionHandler 在一条已认证 Session 存活期间运行 Heartbeat、WorkPool 等业务。
// 返回后 Run 仍会等待 Session.Done，禁止把旧 Session 的后台 goroutine泄漏到下一代。
type SessionHandler[S Session] func(context.Context, S) error

// Options 固定指数退避、稳定窗口和可测试依赖。
type Options struct {
	Logger         *slog.Logger
	InitialBackoff time.Duration
	MaximumBackoff time.Duration
	StableAfter    time.Duration
	JitterFraction float64
	RandomUnit     func() (float64, error)
	Sleep          func(context.Context, time.Duration) error
}

// Run 持续建立并运行 Control Session，直到 Context 取消或遇到永久认证/信任错误。
func Run[S Session](ctx context.Context, starter Starter[S], handler SessionHandler[S], options Options) error {
	if ctx == nil || starter == nil || handler == nil || options.InitialBackoff <= 0 ||
		options.MaximumBackoff < options.InitialBackoff || options.StableAfter <= 0 ||
		options.JitterFraction < 0 || options.JitterFraction > 1 {
		return ErrInvalidOptions
	}
	if options.RandomUnit == nil {
		options.RandomUnit = cryptoRandomUnit
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}

	backoff := options.InitialBackoff
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Dial/AUTH 仍由进程 Context 取消；成功后的 Session 则由 Starter 脱离该
		// 取消链。这样 SIGTERM 能先由 Handler 在仍可写的 Control socket 上完成
		// Drain，再由本循环统一 Close/Wait，且旧代退出前绝不会启动新一代。
		attempt++
		stage := "connect"
		session, err := starter.StartDetached(ctx)
		var retryAfter time.Duration
		if err == nil {
			stage = "session"
			if options.Logger != nil {
				options.Logger.InfoContext(ctx, logging.EventAgentServerConnected, "stage", "established", "attempt", attempt)
			}
			startedAt := time.Now()
			handlerErr := handler(ctx, session)
			session.Close()
			sessionErr := session.Wait()
			err = errors.Join(handlerErr, sessionErr)
			if time.Since(startedAt) >= options.StableAfter {
				// 只有稳定运行达到窗口才重置退避；短连接成功不能制造重连风暴。
				backoff = options.InitialBackoff
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		// 阶段只来自 Runner 的类型化固定标记，未知错误使用当前生命周期阶段。
		var staged interface{ FailureStage() string }
		if errors.As(err, &staged) {
			stage = staged.FailureStage()
		}
		permanent, suggested := classify(err)
		fallback := "control connection ended; reconnect scheduled"
		switch stage {
		case "dial":
			fallback = "unable to establish connection to server gateway"
		case "authentication":
			fallback = "server control authentication failed"
		case "session_setup":
			fallback = "unable to initialize authenticated control session"
		}
		var authFailure *controlauth.Failure
		if errors.As(err, &authFailure) {
			fallback = "server rejected control authentication: " + authFailure.Code.String()
		} else if errors.Is(err, agentgateway.ErrPinnedCertificate) {
			fallback = "server certificate does not match the connection token pin"
		} else if errors.Is(err, agentgateway.ErrUnsupportedALPN) {
			fallback = "server did not negotiate the required control protocol"
		} else if errors.Is(err, controlauth.ErrProtocol) {
			fallback = "control authentication protocol violation"
		}
		detail := logging.ErrorDetail(err, fallback)
		if permanent {
			if options.Logger != nil {
				options.Logger.ErrorContext(ctx, logging.EventAgentServerConnectionFailed,
					"stage", stage, "attempt", attempt, "retryable", false,
					"error", detail)
			}
			return err
		}
		retryAfter = suggested
		delay, jitterErr := jitter(backoff, options.JitterFraction, options.RandomUnit)
		if jitterErr != nil {
			return fmt.Errorf("generate reconnect jitter: %w", jitterErr)
		}
		if retryAfter > delay {
			delay = retryAfter
		}
		if options.Logger != nil {
			options.Logger.WarnContext(ctx, logging.EventAgentServerConnectionFailed,
				"stage", stage, "attempt", attempt, "retryable", true, "retry_delay_ms", delay.Milliseconds(),
				"error", detail)
		}
		if err := options.Sleep(ctx, delay); err != nil {
			return err
		}
		if backoff >= options.MaximumBackoff/2 {
			backoff = options.MaximumBackoff
		} else {
			backoff *= 2
		}
	}
}

func classify(err error) (permanent bool, retryAfter time.Duration) {
	var failure *controlauth.Failure
	if errors.As(err, &failure) {
		return !failure.Retryable(), failure.RetryAfter
	}
	// Pin、ALPN 与本地协议错误在 Token/版本不变时不会自行恢复，禁止快速重试。
	if errors.Is(err, agentgateway.ErrPinnedCertificate) || errors.Is(err, agentgateway.ErrUnsupportedALPN) ||
		errors.Is(err, connectiontoken.ErrMalformed) || errors.Is(err, connectiontoken.ErrIntegrity) ||
		errors.Is(err, controlauth.ErrProtocol) || errors.Is(err, controlauth.ErrInvalidInput) {
		return true, 0
	}
	return false, 0
}

func jitter(base time.Duration, fraction float64, randomUnit func() (float64, error)) (time.Duration, error) {
	unit, err := randomUnit()
	if err != nil {
		return 0, err
	}
	if math.IsNaN(unit) || unit < 0 || unit >= 1 {
		return 0, errors.New("random unit must be in [0,1)")
	}
	factor := 1 + (unit*2-1)*fraction
	return time.Duration(float64(base) * factor), nil
}

func cryptoRandomUnit() (float64, error) {
	var bytes [8]byte
	if _, err := cryptorand.Read(bytes[:]); err != nil {
		return 0, err
	}
	// 取最高 53 位，精确映射到 float64 可表示的 [0,1) 网格。
	value := binary.BigEndian.Uint64(bytes[:]) >> 11
	return float64(value) / float64(uint64(1)<<53), nil
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
