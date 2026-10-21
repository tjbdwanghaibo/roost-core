package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func within(got, want time.Time, slack time.Duration) bool {
	d := got.Sub(want)
	return d > -slack && d < slack
}

func TestBusinessClockFollowsTheConfiguredOffset(t *testing.T) {
	cfg := viper.New()
	cfg.Set("time.logic_offset", "24h")
	business := BusinessClock(NewRegistry(cfg)).Now()
	wall := time.Now()
	if !within(business, wall.Add(24*time.Hour), time.Second) {
		t.Fatalf("business clock %v, want real time + 24h (%v)", business, wall.Add(24*time.Hour))
	}
	if business.YearDay() == wall.YearDay() {
		t.Fatalf("business clock %v is on the same day as %v; a day reset reading it would not fire", business, wall)
	}
	// 没写偏移：业务时钟就是真实时间；Registry 为 nil 时退回进程级业务时钟。
	if got := BusinessClock(NewRegistry(viper.New())).Now(); !within(got, time.Now(), time.Second) {
		t.Fatalf("business clock with no offset = %v, want real time", got)
	}
	if got := BusinessClock(nil).Now(); !within(got, clock.Now(), time.Second) {
		t.Fatalf("BusinessClock(nil) = %v, want the process business clock %v", got, clock.Now())
	}
}

func TestOffsetMovesBusinessTimeButNotTheSingletonLease(t *testing.T) {
	saved := clock.Offset()
	t.Cleanup(func() { clock.SetOffset(saved) })

	h := newSingletonHarness(t, nil, nil)
	h.app.cfg.Set("time.logic_offset", "24h")
	var (
		business, requestNow, wall time.Time
		ctxLeft                    time.Duration
	)
	h.svc.onInit = func(r *Registry) error {
		wall = time.Now()
		business = BusinessClock(r).Now()
		requestNow = fctx.Now()
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		deadline, _ := ctx.Deadline()
		ctxLeft = time.Until(deadline)
		return nil
	}
	result := h.start()
	h.awaitServed(t, result)

	if !within(business, wall.Add(24*time.Hour), time.Second) || !within(requestNow, wall.Add(24*time.Hour), time.Second) {
		t.Fatalf("business clock %v, fctx.Now %v; want both real time + 24h (%v)", business, requestNow, wall.Add(24*time.Hour))
	}
	if ctxLeft > time.Hour || ctxLeft < time.Hour-time.Second {
		t.Fatalf("a 1h context had %v left; the ctx deadline must be on the system clock", ctxLeft)
	}
	// 单实例锁的租约窗口按它自己的系统时钟（这里是替身）算，续期照常。
	if status := h.status(t); status.state != singletonHeld || !status.validUntil.Equal(h.clock.Now().Add(testSingletonTTL)) {
		t.Fatalf("lock status = %+v, want held until system now + ttl (%v)", status, h.clock.Now().Add(testSingletonTTL))
	}
	h.awaitWaiting(t, result, true)
	h.clock.AdvanceToNext(t)
	h.awaitWaiting(t, result, true)
	if status := h.status(t); status.state != singletonHeld || !status.validUntil.Equal(h.clock.Now().Add(testSingletonTTL)) {
		t.Fatalf("lock status after a renewal = %+v, want held until system now + ttl (%v)", status, h.clock.Now().Add(testSingletonTTL))
	}
	if err := h.runtimeFailure(t).Err(); err != nil {
		t.Fatalf("runtime failure under a business offset: %v", err)
	}
	if err := h.stop(t, result); err != nil {
		t.Fatalf("run: %v", err)
	}
}

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

// 业务时区与每日重置时刻（DESIGN-TODO D18）：time.business_timezone / time.business_day_start 在 App 的声明里，
// 缺省 UTC 00:00，生成器把它们写进每个服务的配置（Starter），同一工程各服务必须相同（shared）；非法 IANA 名、
// 依赖机器环境的 Local、不是 HH:MM 的重置时刻在加载时拒绝（ValidateServiceConfig 与 --check-config 同一处）；
// Registry 的业务时钟带着这份日历。
func TestBusinessTimezoneIsDeclaredCheckedAndCarriedByTheBusinessClock(t *testing.T) {
	keys := map[string]configschema.Key{}
	for _, key := range AppConfigSchema().Keys {
		keys[key.Name] = key
	}
	for name, want := range map[string]string{"time.business_timezone": "UTC", "time.business_day_start": "00:00"} {
		key, ok := keys[name]
		if !ok {
			t.Fatalf("App does not declare %s", name)
		}
		if key.Default != want || !key.Starter || key.Example != want || key.Shared != name {
			t.Fatalf("%s = %+v; want default and example %q, written into generated configs, shared across services", name, key, want)
		}
	}

	cfg := viper.New()
	cal := BusinessCalendar(NewRegistry(cfg))
	if cal.Location() != time.UTC || cal.DayStartOffset() != 0 {
		t.Fatalf("default business calendar = %s, want UTC 00:00", cal)
	}
	cfg.Set("time.business_timezone", "Asia/Shanghai")
	cfg.Set("time.business_day_start", "05:00")
	cfg.Set("time.logic_offset", "1h")
	r := NewRegistry(cfg)
	cal = BusinessCalendar(r)
	if cal.Location().String() != "Asia/Shanghai" || cal.DayStartOffset() != 5*time.Hour {
		t.Fatalf("configured business calendar = %s, want Asia/Shanghai 05:00", cal)
	}
	if got := clock.CalendarOf(BusinessClock(r)); got.String() != "Asia/Shanghai 05:00" {
		t.Fatalf("the registry's business clock carries %s", got)
	}
	if !within(BusinessClock(r).Now(), time.Now().Add(time.Hour), time.Second) {
		t.Fatal("the business clock lost its offset when it gained a calendar")
	}

	for _, bad := range []struct{ key, value, want string }{
		{"time.business_timezone", "Mars/Olympus_Mons", "time.business_timezone"},
		{"time.business_timezone", "Local", "time.business_timezone"},
		{"time.business_day_start", "5am", "time.business_day_start"},
		{"time.business_day_start", "24:00", "time.business_day_start"},
	} {
		cfg := productionServiceConfig("game")
		cfg.Set("env", "dev")
		cfg.Set(bad.key, bad.value)
		if err := ValidateServiceConfig(cfg); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s = %q: ValidateServiceConfig error = %v, want a refusal naming %s", bad.key, bad.value, err, bad.want)
		}
	}
	good := productionServiceConfig("game")
	good.Set("time.business_timezone", "America/New_York")
	good.Set("time.business_day_start", "04:00")
	if err := ValidateServiceConfig(good); err != nil {
		t.Fatalf("a valid business time zone was refused: %v", err)
	}
}

// App 启动时把业务日历设成进程级日历（clock.BusinessDay 等全局函数读它），并把实际时区与重置时刻写进启动日志。
func TestRunSetsTheProcessCalendarAndLogsIt(t *testing.T) {
	saved := clock.ProcessCalendar()
	t.Cleanup(func() { clock.SetCalendar(saved) })
	dir := t.TempDir()
	a := New("roost-test", "0.0.0")
	a.Mods(exitReasonMod{})
	a.RegisterServer("game", &errService{})
	a.cfg.Set("log.file", true)
	a.cfg.Set("log.stdout", false)
	a.cfg.Set("log.dir", dir)
	a.cfg.Set("log.rotate_interval", 0)
	a.cfg.Set("time.business_timezone", "Asia/Shanghai")
	a.cfg.Set("time.business_day_start", "05:00")
	a.RootCmd().SetArgs([]string{"game"})

	if err := a.Execute(); !errors.Is(err, errExitReasonForLog) {
		t.Fatalf("Execute err = %v, want the mod init error", err)
	}
	if got := clock.ProcessCalendar().String(); got != "Asia/Shanghai 05:00" {
		t.Fatalf("process calendar after start = %s", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "game-1000.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "business_timezone=Asia/Shanghai") || !strings.Contains(string(raw), "business_day_start=05:00") {
		t.Fatalf("the start log does not name the business time zone and day start:\n%s", raw)
	}
}
