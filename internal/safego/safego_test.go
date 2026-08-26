package safego

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestGoRunsFunctionWithoutReportingPanic(t *testing.T) {
	completed := make(chan struct{})
	reported := make(chan error, 1)

	Go(func(err error) {
		reported <- err
	}, nil, func() {
		close(completed)
	})

	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("function did not complete")
	}
	select {
	case err := <-reported:
		t.Fatalf("unexpected panic report: %v", err)
	default:
	}
}

func TestGoRecoversPanicAndReportsStack(t *testing.T) {
	reported := make(chan error, 1)
	Go(func(err error) {
		reported <- err
	}, nil, panicFromTestGo)

	select {
	case err := <-reported:
		if !errors.Is(err, ErrPanic) {
			t.Fatalf("panic error = %v, want errors.Is ErrPanic", err)
		}
		var panicErr *PanicError
		if !errors.As(err, &panicErr) {
			t.Fatalf("panic error type = %T, want *PanicError", err)
		}
		if !bytes.Contains(panicErr.Stack(), []byte("panicFromTestGo")) {
			t.Fatalf("panic stack does not contain panic source: %s", panicErr.Stack())
		}
	case <-time.After(time.Second):
		t.Fatal("panic was not reported")
	}
}

func TestGoRecoversPanicFromReporter(t *testing.T) {
	reporterStarted := make(chan struct{})
	Go(func(error) {
		close(reporterStarted)
		panic("reporter failed")
	}, nil, func() {
		panic("worker failed")
	})

	select {
	case <-reporterStarted:
	case <-time.After(time.Second):
		t.Fatal("panic reporter did not run")
	}
}

func TestGoRejectsNilCallbacks(t *testing.T) {
	tests := []struct {
		name     string
		onPanic  func(error)
		function func()
	}{
		{name: "nil panic reporter", function: func() {}},
		{name: "nil function", onPanic: func(error) {}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Go did not reject nil callback")
				}
			}()
			Go(test.onPanic, nil, test.function)
		})
	}
}

func TestGoReportsPanicBeforeDone(t *testing.T) {
	reported := make(chan struct{})
	done := make(chan struct{})
	Go(func(error) {
		close(reported)
	}, func() {
		select {
		case <-reported:
		case <-time.After(time.Second):
			t.Error("onDone ran before panic was reported")
		}
		close(done)
	}, func() {
		panic("worker failed")
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onDone did not run")
	}
}

func TestGoRecoversPanicFromDone(t *testing.T) {
	reported := make(chan error, 1)
	Go(func(err error) {
		reported <- err
	}, func() {
		panic("done failed")
	}, func() {})

	select {
	case err := <-reported:
		if !errors.Is(err, ErrPanic) {
			t.Fatalf("done panic error = %v, want ErrPanic", err)
		}
	case <-time.After(time.Second):
		t.Fatal("done panic was not reported")
	}
}

func panicFromTestGo() {
	panic("secret value must not enter the error")
}
