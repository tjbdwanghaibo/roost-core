package app

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/clock"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// 业务时间只许前进（docs/feature/BUSINESS-TIME-MONOTONIC-2026-10-06.md）：同一套部署的业务时间
// （真实时间 + time.logic_offset）不能回到它已经到过的时刻。高水位存在单实例锁的协调存储里，
// App 在任何 Mod Init 之前检查：按新偏移算出的业务时间低于“高水位 − 1 分钟容差”就拒绝启动，
// 点名偏移和高水位；否则推进高水位，运行中定期推进。修前（没有守卫）偏移从 +24h 改回 0 照常启动。

const testBusinessTimeKey = testSingletonPrefix + businessTimeKeySuffix

// seedBusinessTime 写一个高水位，值的格式与 App 写的一样。
func seedBusinessTime(h *singletonHarness, at time.Time, writer string) {
	h.store.set(testBusinessTimeKey, strconv.FormatInt(at.UnixMilli(), 10)+"|"+writer, 0)
}

// businessTimeMarkOf 读高水位（不存在为零值）。
func businessTimeMarkOf(t *testing.T, h *singletonHarness) time.Time {
	t.Helper()
	mark, err := parseBusinessTimeMark(h.store.value(testBusinessTimeKey))
	if err != nil {
		t.Fatal(err)
	}
	if mark.raw == nil {
		return time.Time{}
	}
	return mark.time()
}

func withLogicOffset(t *testing.T, h *singletonHarness, offset string) {
	t.Helper()
	saved := clock.Offset()
	t.Cleanup(func() { clock.SetOffset(saved) })
	h.app.cfg.Set("time.logic_offset", offset)
}

func TestBusinessTimeMovingBackRefusesToStart(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	withLogicOffset(t, h, "0s")
	// 上一次运行在 +24h 偏移下，高水位走到了真实时间 + 24h。
	reached := time.Now().Add(24 * time.Hour)
	seedBusinessTime(h, reached, "24h0m0s|game:1")
	initialized := false
	h.svc.onInit = func(*Registry) error { initialized = true; return nil }

	result := h.start()
	select {
	case <-h.svc.served:
		t.Fatal("moving time.logic_offset from 24h back to 0 started the service; business time must only move forward")
	case err := <-result:
		if !errors.Is(err, ErrBusinessTimeMovedBack) {
			t.Fatalf("run error = %v, want ErrBusinessTimeMovedBack", err)
		}
		for _, want := range []string{"time.logic_offset = 0s", reached.UTC().Format(time.RFC3339), testBusinessTimeKey, "24h0m0s|game:1", "wipe the deployment's data"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("run error = %v, want it to name %q", err, want)
			}
		}
	case <-time.After(testWaitLimit):
		t.Fatal("run neither served nor returned")
	}
	if initialized {
		t.Fatal("the service was initialized before the business time check refused the start")
	}
	if got := businessTimeMarkOf(t, h); got.UnixMilli() != reached.UnixMilli() {
		t.Fatalf("high-water mark after the refusal = %v, want it untouched at %v", got, reached)
	}
	// 拒绝发生在拿锁之后：锁要释放，下一次（改对偏移后）不用等它过期。
	if value := h.store.value(testSingletonKey); value != nil {
		t.Fatalf("singleton key after the refusal = %q, want it released", value)
	}
}

func TestBusinessTimeMayMoveForwardOrStay(t *testing.T) {
	cases := []struct {
		name   string
		offset string
		// seed 是相对真实时间的高水位；nil 表示这套部署第一次启动。
		seed *time.Duration
		// wantAtLeast 是启动后高水位至少到的位置（相对真实时间）。
		wantAtLeast time.Duration
	}{
		{name: "first start", offset: "24h", seed: nil, wantAtLeast: 24 * time.Hour},
		{name: "same offset restart", offset: "24h", seed: ptr(24*time.Hour - time.Minute), wantAtLeast: 24 * time.Hour},
		{name: "offset moved forward", offset: "48h", seed: ptr(24 * time.Hour), wantAtLeast: 48 * time.Hour},
		{name: "back within the tolerance", offset: "0s", seed: ptr(30 * time.Second), wantAtLeast: 30 * time.Second},
		{name: "offset reduced by less than the real time since", offset: "0s", seed: ptr(-time.Hour), wantAtLeast: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSingletonHarness(t, nil, nil)
			withLogicOffset(t, h, tc.offset)
			if tc.seed != nil {
				seedBusinessTime(h, time.Now().Add(*tc.seed), "24h0m0s|game:1")
			}
			result := h.start()
			h.awaitServed(t, result)
			got := businessTimeMarkOf(t, h)
			if want := time.Now().Add(tc.wantAtLeast); got.Before(want.Add(-time.Second)) {
				t.Fatalf("high-water mark = %v, want at least %v", got, want)
			}
			// 高水位只增：在容差内回退的进程不把它拉低。
			if tc.seed != nil && got.Before(time.Now().Add(*tc.seed).Add(-time.Second)) {
				t.Fatalf("high-water mark moved back to %v", got)
			}
			if err := h.stop(t, result); err != nil {
				t.Fatalf("run: %v", err)
			}
		})
	}
}

func ptr(d time.Duration) *time.Duration { return &d }

func TestTheHighWaterMarkAdvancesWhileRunning(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	withLogicOffset(t, h, "1h")
	h.app.businessTimeInterval = 5 * time.Millisecond
	result := h.start()
	h.awaitServed(t, result)
	first := businessTimeMarkOf(t, h)
	deadline := time.Now().Add(testWaitLimit)
	for !businessTimeMarkOf(t, h).After(first) {
		if time.Now().After(deadline) {
			t.Fatalf("high-water mark stayed at %v while the service ran", first)
		}
		time.Sleep(time.Millisecond)
	}
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 停下之后不再推进（goroutine 已退出）。
	stopped := len(h.markCallsSnapshot())
	time.Sleep(20 * time.Millisecond)
	if after := len(h.markCallsSnapshot()); after != stopped {
		t.Fatalf("high-water mark written %d more times after the run returned", after-stopped)
	}
}

func (h *singletonHarness) markCallsSnapshot() []fakeCASCall {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return append([]fakeCASCall(nil), h.store.markCalls...)
}

func TestAnUnreadableHighWaterMarkRefusesToStart(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	withLogicOffset(t, h, "24h")
	h.store.mu.Lock()
	h.store.markErr = errors.New("redis: connection refused")
	h.store.mu.Unlock()
	result := h.start()
	select {
	case <-h.svc.served:
		t.Fatal("started without being able to read the business time high-water mark")
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), testBusinessTimeKey) {
			t.Fatalf("run error = %v, want the store error naming %s", err, testBusinessTimeKey)
		}
	case <-time.After(testWaitLimit):
		t.Fatal("run neither served nor returned")
	}
}

// 没开单实例锁的进程：偏移非 0 时只为高水位打开一个连接（停机时关掉）；装不了守卫就拒绝启动；
// 偏移为 0 时不碰存储（行为与以前一样）。
func TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime(t *testing.T) {
	t.Run("a non-zero offset checks and keeps the mark", func(t *testing.T) {
		h := newSingletonHarness(t, nil, nil)
		h.app.cfg.Set("singleton.enabled", false)
		withLogicOffset(t, h, "1h")
		seedBusinessTime(h, time.Now().Add(24*time.Hour), "24h0m0s|game:1")
		result := h.start()
		err := awaitRunResult(t, result)
		if !errors.Is(err, ErrBusinessTimeMovedBack) {
			t.Fatalf("run error = %v, want ErrBusinessTimeMovedBack from the store opened for the mark", err)
		}
		if h.opens.Load() != 1 || !h.store.isClosed() {
			t.Fatalf("opens = %d, closed = %v; want the store opened once for the mark and closed after the refusal", h.opens.Load(), h.store.isClosed())
		}
	})
	t.Run("forward offset serves and closes the store at stop", func(t *testing.T) {
		h := newSingletonHarness(t, nil, nil)
		h.app.cfg.Set("singleton.enabled", false)
		withLogicOffset(t, h, "1h")
		result := h.start()
		h.awaitServed(t, result)
		if got := businessTimeMarkOf(t, h); got.Before(time.Now().Add(time.Hour - time.Second)) {
			t.Fatalf("high-water mark = %v, want real time + 1h", got)
		}
		if err := h.stop(t, result); err != nil {
			t.Fatalf("run: %v", err)
		}
		if !h.store.isClosed() {
			t.Fatal("the store opened for the mark was not closed at stop")
		}
	})
	t.Run("no store to keep the mark", func(t *testing.T) {
		h := newSingletonHarness(t, nil, nil)
		h.app.cfg.Set("singleton.enabled", false)
		h.app.singletonOpener = nil
		withLogicOffset(t, h, "24h")
		err := awaitRunResult(t, h.start())
		if !errors.Is(err, ErrBusinessTimeGuardMissing) || !strings.Contains(err.Error(), "App.Singleton") {
			t.Fatalf("run error = %v, want ErrBusinessTimeGuardMissing telling how to install the store", err)
		}
	})
	t.Run("zero offset does not touch the store", func(t *testing.T) {
		h := newSingletonHarness(t, nil, nil)
		h.app.cfg.Set("singleton.enabled", false)
		withLogicOffset(t, h, "0s")
		result := h.start()
		h.awaitServed(t, result)
		if err := h.stop(t, result); err != nil {
			t.Fatalf("run: %v", err)
		}
		if h.opens.Load() != 0 {
			t.Fatalf("opener called %d times with no singleton and no offset", h.opens.Load())
		}
	})
}

// 生产不跑守卫：偏移强制为 0，生产行为一字不变（不读写高水位，主机时钟偏差不会多出一种拒绝启动）。
func TestProductionDoesNotRunTheBusinessTimeGuard(t *testing.T) {
	a := New("roost-test", "0.0.0")
	a.cfg = viper.New()
	a.cfg.Set("env", "production")
	a.registry = NewRegistry(a.cfg)
	opened := 0
	a.Singleton(func(*viper.Viper) (SingletonStore, error) { opened++; return nil, fmt.Errorf("must not be opened") })
	guard, err := a.startBusinessTimeGuard("game", nil)
	if guard != nil || err != nil || opened != 0 {
		t.Fatalf("production: guard = %v, err = %v, opened = %d; want no guard and no store", guard, err, opened)
	}
}

// 维护者第十二轮决定（低优先“业务时间高水位推进失败加计数”）：运行中推进失败只记 Warn、下一拍再试，
// 之前没有任何指标，一直推不上去在面板上看不出来（方案文档“未验证 / 余项”）。现在每次失败计
// app.business_time.advance_failed.total，存储恢复后不再增长。
func TestAFailedHighWaterMarkAdvanceIsCounted(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	withLogicOffset(t, h, "1h")
	h.app.businessTimeInterval = 5 * time.Millisecond
	result := h.start()
	h.awaitServed(t, result)
	registry, ok := Lookup[*metrics.Registry](h.app.registry, ModMetrics)
	if !ok {
		t.Fatal("no metrics registry")
	}
	failures := func() int64 {
		for _, metric := range registry.Snapshot() {
			if metric.Name == businessTimeAdvanceFailedMetric {
				return metric.Value
			}
		}
		return 0
	}
	if got := failures(); got != 0 {
		t.Fatalf("%d advance failures counted while the store worked", got)
	}
	h.store.mu.Lock()
	h.store.markErr = errors.New("redis: connection refused")
	h.store.mu.Unlock()
	deadline := time.Now().Add(testWaitLimit)
	for failures() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("advance failures counted %d while every write of the high-water mark failed, want them counted", failures())
		}
		time.Sleep(time.Millisecond)
	}
	h.store.mu.Lock()
	h.store.markErr = nil
	h.store.mu.Unlock()
	before := businessTimeMarkOf(t, h)
	for !businessTimeMarkOf(t, h).After(before) {
		if time.Now().After(deadline) {
			t.Fatal("high-water mark did not advance after the store recovered")
		}
		time.Sleep(time.Millisecond)
	}
	settled := failures()
	time.Sleep(30 * time.Millisecond)
	if got := failures(); got != settled {
		t.Fatalf("advance failures kept growing (%d -> %d) after the store recovered", settled, got)
	}
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
}
