package reconnect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lifei6671/xtunnel/internal/agent/controlauth"
	agentgateway "github.com/lifei6671/xtunnel/internal/agent/gateway"
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
