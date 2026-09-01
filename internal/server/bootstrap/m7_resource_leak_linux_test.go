//go:build linux

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

const (
	m7LeakEpochsEnvironment      = "XTUNNEL_M7_07_EPOCHS"
	m7LeakConnectionsEnvironment = "XTUNNEL_M7_07_CONNECTIONS"

	// 固定累计预算只吸收 testing、TLS、SQLite 与 Go Runtime 在预热后的有限缓存抖动。
	// Full 使用 1 个预热加 3 个等量测量 epoch；累计增长超过预算即失败，不再要求
	// “每一步也超过阈值”，避免稳定的小泄漏被逐步阈值掩盖。
	m7LeakHeapAllocAllowance   = 1 << 20
	m7LeakHeapObjectsAllowance = 3_000
)

// TestM7ResourceLeak 是 M7-07 的 Linux 产品级入口。每个分区先完成一次预热 epoch，
// 再用等量 epoch 检查真实连接 churn、Cancel、Reconnect 与 Drain 后的 FD、goroutine
// 和 GC 后 live heap。RSS 只记录诊断趋势；Go allocator 不保证把已释放 arena 立即归还 OS。
func TestM7ResourceLeak(t *testing.T) {
	epochs, connections := m7LeakConfig(t)

	t.Run("connection churn and cancel drain", func(t *testing.T) {
		m7RunLeakEpochs(t, epochs, func(t *testing.T) {
			m7ExerciseConnectionChurn(t, connections)
		})
	})
	t.Run("reconnect", func(t *testing.T) {
		m7RunLeakEpochs(t, epochs, func(t *testing.T) {
			t.Setenv(m7ChaosConnectorsEnvironment, "1")
			TestM7ReconnectStorm(t)
		})
	})
	t.Run("drain", func(t *testing.T) {
		m7RunLeakEpochs(t, epochs, TestM7GracefulShutdownChaos)
	})
}

type m7LeakSample struct {
	FDs         int
	Goroutines  int
	RSSKiB      int64
	HeapAlloc   uint64
	HeapObjects uint64
	HeapInuse   uint64
	StackInuse  uint64
	NumGC       uint32
	CapturedAt  time.Time
}

func m7LeakConfig(t *testing.T) (int, int) {
	t.Helper()
	epochsText := os.Getenv(m7LeakEpochsEnvironment)
	connectionsText := os.Getenv(m7LeakConnectionsEnvironment)
	if epochsText == "" || connectionsText == "" {
		t.Skipf("set %s and %s through tests/leak/run-m7-07.sh", m7LeakEpochsEnvironment, m7LeakConnectionsEnvironment)
	}
	epochs, err := strconv.Atoi(epochsText)
	if err != nil || epochs < 2 || epochs > 8 {
		t.Fatalf("%s=%q must be an integer from 2 through 8", m7LeakEpochsEnvironment, epochsText)
	}
	connections, err := strconv.Atoi(connectionsText)
	if err != nil || connections < 1 || connections > 1_000 {
		t.Fatalf("%s=%q must be an integer from 1 through 1000", m7LeakConnectionsEnvironment, connectionsText)
	}
	return epochs, connections
}

// m7RunLeakEpochs 把业务 owner 的精确归零断言留在各 workload 内部，并在子测试
// Cleanup 全部完成后才采样进程资源。首轮用于初始化库级缓存，后续轮次不得形成持续增长。
func m7RunLeakEpochs(t *testing.T, epochs int, workload func(*testing.T)) {
	t.Helper()
	samples := make([]m7LeakSample, 0, epochs)
	for epoch := 1; epoch <= epochs; epoch++ {
		name := fmt.Sprintf("epoch_%02d", epoch)
		if !t.Run(name, workload) {
			t.Fatalf("%s failed", name)
		}

		var sample m7LeakSample
		if epoch == 1 {
			sample = m7ReadStableLeakSample(t)
		} else {
			sample = m7WaitForLeakBaseline(t, samples[0])
		}
		samples = append(samples, sample)
		retainedAlloc := int64(sample.HeapAlloc) - int64(samples[0].HeapAlloc)
		retainedObjects := int64(sample.HeapObjects) - int64(samples[0].HeapObjects)
		t.Logf("M7-07 epoch=%d fd=%d goroutines=%d rss_kib=%d heap_alloc=%d heap_objects=%d heap_inuse=%d stack_inuse=%d num_gc=%d",
			epoch, sample.FDs, sample.Goroutines, sample.RSSKiB, sample.HeapAlloc,
			sample.HeapObjects, sample.HeapInuse, sample.StackInuse, sample.NumGC)
		t.Logf("M7-07 epoch=%d retained_heap_alloc=%d retained_heap_objects=%d",
			epoch, retainedAlloc, retainedObjects)
	}
	m7AssertHeapPlateau(t, samples)
}

func m7ExerciseConnectionChurn(t *testing.T, connections int) {
	t.Helper()
	baseline := m7MustReadShutdownResources(t)
	fixture := newM7ShutdownFixture(t, nil)
	wantIdle := uint32(8)
	waitForProductGateIdleWork(t, fixture.runtime, wantIdle)

	for index := 0; index < connections; index++ {
		operation := fmt.Sprintf("M7-07 churn %d/%d", index+1, connections)
		public := dialProductGateTCP(t, fixture.publicTCP, "127.0.0.1")
		originDone := startProductGateOriginEcho(fixture.tcpOrigin.next(t, operation))
		payload := []byte(fmt.Sprintf("m7-07-churn-%06d", index))
		assertProductGateRoundTrip(t, public, payload, operation)
		finishProductGateTCP(t, public, originDone, operation)
		if err := public.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("close %s public connection: %v", operation, err)
		}
	}

	waitForProductGateNoActiveWork(t, fixture.runtime)
	waitForProductGateIdleWork(t, fixture.runtime, wantIdle)
	if limits := fixture.runtime.limits.Snapshot(); limits.PendingOpens != 0 || limits.ActiveTotal != 0 ||
		len(limits.ActiveByTunnel) != 0 || len(limits.ActiveByService) != 0 || len(limits.ActiveBySource) != 0 {
		t.Fatalf("M7-07 churn left active or pending limits: %+v", limits)
	}

	// Agent Context Cancel 进入冻结的两阶段 Drain：先撤下新 Work，再等待既有 ACTIVE；
	// 本场景 ACTIVE 已精确归零，因此 Run 必须有界退出，不能遗留 Control/Work owner。
	fixture.agent.beginDrain()
	if err := fixture.agent.wait(10 * time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("M7-07 Agent cancel/drain error: %v", err)
	}
	if err := fixture.closeServer(); err != nil {
		t.Fatalf("close M7-07 Server after connection churn: %v", err)
	}
	fixture.cleanup(t)
	m7AssertShutdownQuiescent(t, fixture, baseline)
}

func m7ReadLeakSample(t *testing.T) m7LeakSample {
	t.Helper()
	// 两轮 GC 会清空前一轮 sync.Pool，并把终态比较约束到 live heap；不调用
	// debug.FreeOSMemory，避免把测试主动 scavenging 描述成产品自然归还 RSS。
	runtime.GC()
	runtime.GC()
	runtime.Gosched()
	resources, err := m7ReadResources()
	if err != nil {
		t.Fatalf("read M7-07 process resources: %v", err)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return m7LeakSample{
		FDs: resources.FDs, Goroutines: resources.Goroutines, RSSKiB: resources.RSSKiB,
		HeapAlloc: memory.HeapAlloc, HeapObjects: memory.HeapObjects,
		HeapInuse: memory.HeapInuse, StackInuse: memory.StackInuse,
		NumGC: memory.NumGC, CapturedAt: time.Now(),
	}
}

func m7ReadStableLeakSample(t *testing.T) m7LeakSample {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var previous m7LeakSample
	stable := 0
	for {
		current := m7ReadLeakSample(t)
		if stable > 0 && current.FDs == previous.FDs && current.Goroutines == previous.Goroutines {
			stable++
		} else {
			stable = 1
		}
		if stable >= 3 {
			return current
		}
		previous = current
		select {
		case <-deadline.C:
			t.Fatalf("M7-07 warmed resources did not produce three stable samples: final=%+v fd_targets=%v",
				current, m7LeakFDTargets())
		case <-ticker.C:
		}
	}
}

func m7WaitForLeakBaseline(t *testing.T, baseline m7LeakSample) m7LeakSample {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var final m7LeakSample
	stable := 0
	for {
		final = m7ReadLeakSample(t)
		if final.FDs <= baseline.FDs && final.Goroutines <= baseline.Goroutines {
			stable++
			if stable >= 3 {
				return final
			}
		} else {
			stable = 0
		}
		select {
		case <-deadline.C:
			t.Fatalf("M7-07 process resources did not return to warmed baseline: baseline=%+v final=%+v fd_targets=%v",
				baseline, final, m7LeakFDTargets())
		case <-ticker.C:
		}
	}
}

func m7AssertHeapPlateau(t *testing.T, samples []m7LeakSample) {
	t.Helper()
	if err := m7HeapPlateauError(samples); err != nil {
		t.Fatal(err)
	}
}

func m7HeapPlateauError(samples []m7LeakSample) error {
	if len(samples) < 2 {
		return errors.New("M7-07 heap plateau requires one warm-up and at least one measured epoch")
	}
	warmed := samples[0]
	final := samples[len(samples)-1]
	if final.HeapAlloc > warmed.HeapAlloc+m7LeakHeapAllocAllowance {
		return fmt.Errorf("M7-07 HeapAlloc exceeded fixed warmed allowance: warmed=%d final=%d allowance=%d samples=%+v",
			warmed.HeapAlloc, final.HeapAlloc, m7LeakHeapAllocAllowance, samples)
	}
	if final.HeapObjects > warmed.HeapObjects+m7LeakHeapObjectsAllowance {
		return fmt.Errorf("M7-07 HeapObjects exceeded fixed warmed allowance: warmed=%d final=%d allowance=%d samples=%+v",
			warmed.HeapObjects, final.HeapObjects, m7LeakHeapObjectsAllowance, samples)
	}
	return nil
}

func m7LeakFDTargets() map[string]string {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	targets := make(map[string]string, len(entries))
	for _, entry := range entries {
		if target, readErr := os.Readlink("/proc/self/fd/" + entry.Name()); readErr == nil {
			targets[entry.Name()] = target
		}
	}
	return targets
}

func TestM7HeapPlateauRejectsSmallSteadyRetainedGrowth(t *testing.T) {
	warmed := m7LeakSample{HeapAlloc: 2 << 20, HeapObjects: 9_000}
	samples := []m7LeakSample{warmed}
	for epoch := 1; epoch <= 3; epoch++ {
		samples = append(samples, m7LeakSample{
			HeapAlloc:   warmed.HeapAlloc + uint64(epoch)*(400<<10),
			HeapObjects: warmed.HeapObjects + uint64(epoch)*100,
		})
	}
	if err := m7HeapPlateauError(samples); err == nil {
		t.Fatal("small steady retained HeapAlloc growth passed the cumulative M7-07 oracle")
	}
}
