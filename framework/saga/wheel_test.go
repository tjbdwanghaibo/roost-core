package saga

import (
	"slices"
	"testing"
	"time"
)

// 分区时间轮：只取已到期的条目；同一 tick 里还没到期的不推迟一整圈（原型的轮缺陷）；多转几圈的留在槽里；落后超过一圈每槽扫一遍；
// 墙钟回拨时被夹到 cursor 槽的条目按到期时刻判定。
func TestWheelReturnsOnlyDueEntries(t *testing.T) {
	base := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	w := newWheel(5*time.Millisecond, 8, base)
	w.schedule("a", base.Add(7*time.Millisecond))
	w.schedule("b", base.Add(9*time.Millisecond))     // 与 a 同一 tick（5..10ms）
	w.schedule("far", base.Add(200*time.Millisecond)) // 多转几圈
	if got := w.advance(base.Add(8*time.Millisecond), nil); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("at 8ms: %v", got)
	}
	// b 在同一 tick 里、已经过了一个 tick 的一部分：下一次推进就要拿到，不能等一整圈（8 槽 × 5ms = 40ms）。
	if got := w.advance(base.Add(9*time.Millisecond), nil); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("at 9ms: %v", got)
	}
	if got := w.advance(base.Add(100*time.Millisecond), nil); len(got) != 0 || w.pending() != 1 {
		t.Fatalf("at 100ms: %v pending %d", got, w.pending())
	}
	if got := w.advance(base.Add(201*time.Millisecond), nil); !slices.Equal(got, []string{"far"}) {
		t.Fatalf("at 201ms: %v", got)
	}
	// 已经过去的时刻：落在下一次推进会扫的槽里。
	w.schedule("past", base.Add(time.Millisecond))
	if got := w.advance(base.Add(202*time.Millisecond), nil); !slices.Equal(got, []string{"past"}) {
		t.Fatalf("past entry: %v", got)
	}
	// 落后好几圈：每个槽扫一遍，不丢条目。
	w.schedule("x", base.Add(300*time.Millisecond))
	w.schedule("y", base.Add(320*time.Millisecond))
	got := w.advance(base.Add(10*time.Second), nil)
	slices.Sort(got)
	if !slices.Equal(got, []string{"x", "y"}) || w.pending() != 0 {
		t.Fatalf("far behind: %v pending %d", got, w.pending())
	}
}

func TestWheelHandlesAClockStepBack(t *testing.T) {
	base := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	w := newWheel(5*time.Millisecond, 1024, base.Add(time.Hour))
	_ = w.advance(base.Add(time.Hour), nil)
	// 墙钟被拨回一小时：新登记的条目到期时刻早于 cursor，被夹在 cursor 槽里，到点就要拿到，不能等时钟追回一小时。
	w.schedule("soon", base.Add(time.Second))
	if got := w.advance(base.Add(500*time.Millisecond), nil); len(got) != 0 {
		t.Fatalf("before due: %v", got)
	}
	if got := w.advance(base.Add(time.Second), nil); !slices.Equal(got, []string{"soon"}) {
		t.Fatalf("after the step back: %v", got)
	}
}
