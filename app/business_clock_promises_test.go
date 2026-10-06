package app

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/clock"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

// D-L3（维护者第六轮决定）：time.logic_offset 只移动业务时钟。偏移 +1 天时，Registry 的业务时钟与
// 请求上下文的 fctx.Now 都前移一天（日期跨过去，日重置读它就会触发）；系统时钟不动——单实例锁按
// 真实时间续期、ctx 截止按真实时间算。

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
