package reconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
	"github.com/lifei6671/xtunnel/internal/protocol/frame"
	protocolv1 "github.com/lifei6671/xtunnel/internal/protocol/gen"
	connectiontoken "github.com/lifei6671/xtunnel/internal/protocol/token"
)

func TestRunProcessCancellationDrainsCurrentSessionWithoutReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	session := &fakeSession{}
	starter := &successfulStarter{session: session, onStart: cancel}
	handlerCalls := 0
	err := Run(ctx, starter, func(processContext context.Context, got *fakeSession) error {
		handlerCalls++
		if got != session {
			t.Fatal("Handler 未收到当前 Session")
		}
		<-processContext.Done()
		return processContext.Err()
	}, Options{
		InitialBackoff: time.Second, MaximumBackoff: 4 * time.Second, StableAfter: time.Minute,
		JitterFraction: 0,
		Sleep: func(context.Context, time.Duration) error {
			t.Fatal("进程取消后不应进入退避或发起新一代")
			return nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if starter.calls != 1 || handlerCalls != 1 || session.closeCalls.Load() != 1 || session.waitCalls.Load() != 1 {
		t.Fatalf("lifecycle calls = start:%d handler:%d close:%d wait:%d",
			starter.calls, handlerCalls, session.closeCalls.Load(), session.waitCalls.Load())
	}
}

func TestRunUsesCappedExponentialBackoffAndJitter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	starter := &failingStarter{err: errors.New("network unavailable")}
	var delays []time.Duration
	err := Run(ctx, starter, func(context.Context, *fakeSession) error { return nil }, Options{
		InitialBackoff: time.Second, MaximumBackoff: 4 * time.Second, StableAfter: time.Minute,
		JitterFraction: 0.2, RandomUnit: func() (float64, error) { return 0.5, nil },
		Sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			if len(delays) == 4 {
				cancel()
				return context.Canceled
			}
			return nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for index := range want {
		if delays[index] != want[index] {
			t.Fatalf("delay[%d] = %s, want %s", index, delays[index], want[index])
		}
	}
}

func TestRunHonorsRetryAfterAsMinimum(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	starter := &failingStarter{err: &controlauth.Failure{
		Code:  protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED,
		Class: controlauth.FailureRetryable, RetryAfter: 7 * time.Second,
	}}
	var got time.Duration
	err := Run(ctx, starter, func(context.Context, *fakeSession) error { return nil }, Options{
		InitialBackoff: time.Second, MaximumBackoff: 30 * time.Second, StableAfter: time.Minute,
		JitterFraction: 0.2, RandomUnit: func() (float64, error) { return 0, nil },
		Sleep: func(_ context.Context, delay time.Duration) error {
			got = delay
			cancel()
			return context.Canceled
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if got != 7*time.Second {
		t.Fatalf("retry delay = %s, want retry_after 7s", got)
	}
}

func TestRunStopsOnPermanentAuthenticationAndPinErrors(t *testing.T) {
	for name, failure := range map[string]error{
		"token": &controlauth.Failure{
			Code:  protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID,
			Class: controlauth.FailurePermanent,
		},
		"pin":             agentgateway.ErrPinnedCertificate,
		"malformed token": connectiontoken.ErrMalformed,
		"tampered token":  connectiontoken.ErrIntegrity,
	} {
		t.Run(name, func(t *testing.T) {
			starter := &failingStarter{err: failure}
			err := Run(context.Background(), starter, func(context.Context, *fakeSession) error { return nil }, Options{
				InitialBackoff: time.Second, MaximumBackoff: 30 * time.Second, StableAfter: time.Minute,
				JitterFraction: 0.2,
				Sleep: func(context.Context, time.Duration) error {
					t.Fatal("Sleep called for permanent error")
					return nil
				},
			})
			if !errors.Is(err, failure) {
				t.Fatalf("Run() error = %v, want %v", err, failure)
			}
			if starter.calls != 1 {
				t.Fatalf("Start calls = %d, want 1", starter.calls)
			}
		})
	}
}

func TestRunRetriesWrappedAuthTransportTruncation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	authTransportError := fmt.Errorf("read connector auth result: %w",
		errors.Join(frame.ErrTruncatedFrame, syscall.ECONNRESET))
	if !errors.Is(authTransportError, frame.ErrTruncatedFrame) ||
		!errors.Is(authTransportError, syscall.ECONNRESET) ||
		errors.Is(authTransportError, controlauth.ErrProtocol) {
		t.Fatalf("auth transport error identity = %v, want truncated frame + connection reset without protocol violation",
			authTransportError)
	}

	starter := &failingStarter{err: authTransportError}
	randomCalls := 0
	sleepCalls := 0
	var gotDelay time.Duration
	err := Run(ctx, starter, func(context.Context, *fakeSession) error { return nil }, Options{
		InitialBackoff: 2 * time.Second, MaximumBackoff: 30 * time.Second, StableAfter: time.Minute,
		JitterFraction: 0.2,
		RandomUnit: func() (float64, error) {
			randomCalls++
			return 0.5, nil
		},
		Sleep: func(sleepContext context.Context, delay time.Duration) error {
			sleepCalls++
			gotDelay = delay
			cancel()
			return sleepContext.Err()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if starter.calls != 1 || randomCalls != 1 || sleepCalls != 1 {
		t.Fatalf("calls = start:%d random:%d sleep:%d, want 1/1/1",
			starter.calls, randomCalls, sleepCalls)
	}
	if gotDelay != 2*time.Second {
		t.Fatalf("retry delay = %s, want 2s", gotDelay)
	}
}

func TestRunReconnectScaleSpreadsRetryDelays(t *testing.T) {
	for _, connectorCount := range []int{100, 500, 1000, 5000} {
		t.Run(fmt.Sprintf("connectors_%d", connectorCount), func(t *testing.T) {
			results := runReconnectScale(t, connectorCount, errors.New("network unavailable"))

			const bucketCount = 20
			const lowerBound = 8 * time.Second
			const upperBound = 12 * time.Second
			buckets := make([]int, bucketCount)
			uniqueDelays := make(map[time.Duration]struct{}, connectorCount)
			for connector, got := range results {
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("connector %d Run() error = %v, want context.Canceled", connector, got.err)
				}
				if got.startCalls != 1 || got.randomCalls != 1 || got.sleepCalls != 1 {
					t.Fatalf("connector %d calls = start:%d random:%d sleep:%d, want 1/1/1",
						connector, got.startCalls, got.randomCalls, got.sleepCalls)
				}
				if got.delay < lowerBound || got.delay >= upperBound {
					t.Fatalf("connector %d delay = %s, want [%s,%s)", connector, got.delay, lowerBound, upperBound)
				}
				bucket := int((got.delay - lowerBound) * bucketCount / (upperBound - lowerBound))
				buckets[bucket]++
				uniqueDelays[got.delay] = struct{}{}
			}

			if len(uniqueDelays) != connectorCount {
				t.Fatalf("unique delays = %d, want %d", len(uniqueDelays), connectorCount)
			}
			for bucket, count := range buckets {
				if count == 0 || count > connectorCount/10 {
					t.Fatalf("bucket %d count = %d, want 1..%d; buckets = %v",
						bucket, count, connectorCount/10, buckets)
				}
			}
		})
	}
}

func TestRunReconnectScaleStopsPermanentErrors(t *testing.T) {
	for _, connectorCount := range []int{100, 500, 1000, 5000} {
		t.Run(fmt.Sprintf("connectors_%d", connectorCount), func(t *testing.T) {
			failure := &controlauth.Failure{
				Code:  protocolv1.ErrorCode_ERROR_CODE_TOKEN_INVALID,
				Class: controlauth.FailurePermanent,
			}
			results := runReconnectScale(t, connectorCount, failure)

			for connector, got := range results {
				if !errors.Is(got.err, failure) {
					t.Fatalf("connector %d Run() error = %v, want permanent failure", connector, got.err)
				}
				if got.startCalls != 1 || got.randomCalls != 0 || got.sleepCalls != 0 {
					t.Fatalf("connector %d calls = start:%d random:%d sleep:%d, want 1/0/0",
						connector, got.startCalls, got.randomCalls, got.sleepCalls)
				}
			}
		})
	}
}

func TestRunReconnectScaleHonorsRetryAfterMinimum(t *testing.T) {
	for _, connectorCount := range []int{100, 500, 1000, 5000} {
		t.Run(fmt.Sprintf("connectors_%d", connectorCount), func(t *testing.T) {
			const retryAfter = 9 * time.Second
			failure := &controlauth.Failure{
				Code:       protocolv1.ErrorCode_ERROR_CODE_SESSION_RESOURCE_EXHAUSTED,
				Class:      controlauth.FailureRetryable,
				RetryAfter: retryAfter,
			}
			results := runReconnectScale(t, connectorCount, failure)

			clamped := 0
			aboveMinimum := 0
			for connector, got := range results {
				if !errors.Is(got.err, context.Canceled) {
					t.Fatalf("connector %d Run() error = %v, want context.Canceled", connector, got.err)
				}
				if got.startCalls != 1 || got.randomCalls != 1 || got.sleepCalls != 1 {
					t.Fatalf("connector %d calls = start:%d random:%d sleep:%d, want 1/1/1",
						connector, got.startCalls, got.randomCalls, got.sleepCalls)
				}
				if got.delay < retryAfter {
					t.Fatalf("connector %d delay = %s, below retry_after %s", connector, got.delay, retryAfter)
				}
				if got.delay == retryAfter {
					clamped++
				} else {
					aboveMinimum++
				}
			}
			if clamped == 0 || aboveMinimum == 0 {
				t.Fatalf("retry_after distribution = clamped:%d above:%d, want both", clamped, aboveMinimum)
			}
		})
	}
}

type reconnectScaleResult struct {
	delay       time.Duration
	err         error
	startCalls  int
	randomCalls int
	sleepCalls  int
}

func runReconnectScale(t *testing.T, connectorCount int, starterError error) []reconnectScaleResult {
	t.Helper()
	results := make([]reconnectScaleResult, connectorCount)
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(connectorCount)

	// 每个 Worker 只写自己的结果槽，并拥有独立 Context。起跑门闩制造同批并发，
	// Sleep 主动取消且 WaitGroup 等待全部退出，因此测试不会依赖真实时间或遗留 goroutine。
	for connector := range connectorCount {
		go func() {
			defer workers.Done()
			<-start

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			starter := &failingStarter{err: starterError}
			unit := float64(connector) / float64(connectorCount)
			var got reconnectScaleResult
			got.err = Run(ctx, starter, func(context.Context, *fakeSession) error { return nil }, Options{
				InitialBackoff: 10 * time.Second, MaximumBackoff: 30 * time.Second,
				StableAfter: time.Minute, JitterFraction: 0.2,
				RandomUnit: func() (float64, error) {
					got.randomCalls++
					return unit, nil
				},
				Sleep: func(ctx context.Context, delay time.Duration) error {
					got.sleepCalls++
					got.delay = delay
					cancel()
					return ctx.Err()
				},
			})
			got.startCalls = starter.calls
			results[connector] = got
		}()
	}

	close(start)
	workers.Wait()
	return results
}

func TestJitterBoundsAndInvalidRandom(t *testing.T) {
	low, err := jitter(10*time.Second, 0.2, func() (float64, error) { return 0, nil })
	if err != nil || low != 8*time.Second {
		t.Fatalf("low jitter = %s, %v", low, err)
	}
	high, err := jitter(10*time.Second, 0.2, func() (float64, error) { return 0.999999, nil })
	if err != nil || high < 11999990000*time.Nanosecond || high >= 12*time.Second {
		t.Fatalf("high jitter = %s, %v", high, err)
	}
	if _, err := jitter(time.Second, 0.2, func() (float64, error) { return 1, nil }); err == nil {
		t.Fatal("jitter accepted random unit 1")
	}
}

type failingStarter struct {
	err   error
	calls int
}

func (starter *failingStarter) StartDetached(context.Context) (*fakeSession, error) {
	starter.calls++
	return nil, starter.err
}

type successfulStarter struct {
	session *fakeSession
	onStart func()
	calls   int
}

func (starter *successfulStarter) StartDetached(context.Context) (*fakeSession, error) {
	starter.calls++
	if starter.onStart != nil {
		starter.onStart()
	}
	return starter.session, nil
}

type fakeSession struct {
	closeCalls atomic.Int32
	waitCalls  atomic.Int32
}

func (session *fakeSession) Close() {
	session.closeCalls.Add(1)
}

func (session *fakeSession) Wait() error {
	session.waitCalls.Add(1)
	if session.closeCalls.Load() == 0 {
		return errors.New("Wait 在 Close 前调用")
	}
	return nil
}

// stagedFailure 以固定阶段模拟 Runner，不把底层错误文本用于日志分类。
type stagedFailure struct{ cause error }

func (failure stagedFailure) Error() string        { return failure.cause.Error() }
func (failure stagedFailure) Unwrap() error        { return failure.cause }
func (failure stagedFailure) FailureStage() string { return "authentication" }

func TestRunLogsFailureStageAndRetryWithoutUnknownErrorText(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprintf("permanent_%v", permanent), func(t *testing.T) {
			var output bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("private authentication payload must not be logged")
			if permanent {
				cause = errors.Join(controlauth.ErrProtocol, cause)
			}
			starter := &failingStarter{err: stagedFailure{cause: cause}}
			err := Run(ctx, starter, func(context.Context, *fakeSession) error { return nil }, Options{
				Logger:         slog.New(slog.NewJSONHandler(&output, nil)),
				InitialBackoff: time.Second, MaximumBackoff: time.Second, StableAfter: time.Minute,
				RandomUnit: func() (float64, error) { return .5, nil },
				Sleep:      func(context.Context, time.Duration) error { cancel(); return context.Canceled },
			})
			if err == nil || starter.calls != 1 {
				t.Fatal("expected one failed attempt")
			}
			if strings.Contains(output.String(), "private authentication payload") {
				t.Fatal("unknown error text leaked")
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record["msg"] != "agent_server_connection_failed" || record["stage"] != "authentication" ||
				record["attempt"] != float64(1) || record["retryable"] != !permanent || record["error"] == "" {
				t.Fatalf("unexpected failure record: %#v", record)
			}
			if permanent {
				if record["level"] != "ERROR" || record["retry_delay_ms"] != nil {
					t.Fatalf("permanent failure: %#v", record)
				}
			} else if record["level"] != "WARN" || record["retry_delay_ms"] != float64(1000) {
				t.Fatalf("retry failure: %#v", record)
			}
		})
	}
}

func TestRunLogsEstablishedThenSessionEOFAndRetry(t *testing.T) {
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Run(ctx, &successfulStarter{session: &fakeSession{}}, func(context.Context, *fakeSession) error { return io.EOF }, Options{
		Logger:         slog.New(slog.NewJSONHandler(&output, nil)),
		InitialBackoff: time.Second, MaximumBackoff: time.Second, StableAfter: time.Minute,
		RandomUnit: func() (float64, error) { return .5, nil },
		Sleep:      func(context.Context, time.Duration) error { cancel(); return context.Canceled },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var connected, failed map[string]any
	if err := decoder.Decode(&connected); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&failed); err != nil {
		t.Fatal(err)
	}
	if connected["msg"] != "agent_server_connected" || connected["stage"] != "established" ||
		failed["msg"] != "agent_server_connection_failed" || failed["stage"] != "session" ||
		failed["retry_delay_ms"] != float64(1000) || failed["error"] == "" {
		t.Fatalf("connection lifecycle: %#v %#v", connected, failed)
	}
}
