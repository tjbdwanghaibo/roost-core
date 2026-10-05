package clock

import (
	"sync"
	"testing"
	"time"
)

// N11 C1～C3 的控制用例：逻辑时钟的偏移读写、注入实例的隔离与并发安全。
// 偏移以毫秒存储（Milliseconds() 向零截断），所以 Set(t) 之后的 Now() 与 t 的差在 1ms 之内而不是 0——
// 这是现有精度契约（运行记录观察 O10），这里把它固定下来，而不是假定逐纳秒相等。

func TestLogicClockOffsetRoundTripsAtMillisecondResolution(t *testing.T) {
	c := NewLogicClock()
	if c.Offset() != 0 {
		t.Fatalf("a new clock has offset %v", c.Offset())
	}
	c.SetOffset(90*time.Minute + 1500*time.Microsecond)
	if got, want := c.Offset(), 90*time.Minute+time.Millisecond; got != want {
		t.Fatalf("Offset = %v, want %v (stored at millisecond resolution)", got, want)
	}
	c.SetOffset(-2 * time.Hour)
	if got := c.Offset(); got != -2*time.Hour {
		t.Fatalf("negative offset = %v", got)
	}
	before := time.Now()
	now := c.Now()
	after := time.Now()
	if now.Before(before.Add(-2*time.Hour)) || now.After(after.Add(-2*time.Hour)) {
		t.Fatalf("Now = %v, want wall clock minus 2h (between %v and %v)", now, before.Add(-2*time.Hour), after.Add(-2*time.Hour))
	}
	c.Reset()
	if c.Offset() != 0 {
		t.Fatalf("Offset after Reset = %v", c.Offset())
	}
}

func TestLogicClockSetLandsWithinOneMillisecond(t *testing.T) {
	c := NewLogicClock()
	target := time.Now().Add(36 * time.Hour)
	c.Set(target)
	got := c.Now()
	if diff := got.Sub(target); diff < -time.Millisecond || diff > 50*time.Millisecond {
		t.Fatalf("Now after Set = %v, %v from the target; want within the millisecond the offset is stored at", got, diff)
	}
	if got.UnixMilli() < target.UnixMilli()-1 {
		t.Fatalf("UnixMilli %d is more than 1ms before the target %d", got.UnixMilli(), target.UnixMilli())
	}
}

func TestInjectedClocksAreIndependentOfTheGlobalOne(t *testing.T) {
	saved := Offset()
	t.Cleanup(func() { SetOffset(saved) })
	a, b := NewLogicClock(), NewLogicClock()
	a.SetOffset(time.Hour)
	if b.Offset() != 0 || Offset() != saved {
		t.Fatalf("setting one clock moved another: b=%v global=%v", b.Offset(), Offset())
	}
	SetOffset(3 * time.Second)
	if a.Offset() != time.Hour || Offset() != 3*time.Second {
		t.Fatalf("a=%v global=%v", a.Offset(), Offset())
	}
}

func TestLogicClockIsSafeForConcurrentUse(t *testing.T) {
	c := NewLogicClock()
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < 500; round++ {
				if worker%2 == 0 {
					c.SetOffset(time.Duration(round) * time.Millisecond)
				} else {
					_ = c.Now()
					_ = c.UnixMilli()
				}
			}
		}(worker)
	}
	group.Wait()
	if got := c.Offset(); got < 0 || got > 499*time.Millisecond {
		t.Fatalf("final offset %v is not one of the values written", got)
	}
}
