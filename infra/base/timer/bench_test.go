package timer

import (
	"testing"
	"time"
)

// 调度器基准：只用公开 API，重构前后可以用同一份文件对照。
// 每个基准都是稳态：堆大小在迭代之间保持不变，每次迭代一次 Tick 触发固定数量的节点。

const (
	benchHeapSize = 10000 // 堆里常驻的定时器数
	benchPerTick  = 100   // 每次 Tick 到期并触发的节点数
)

// benchScheduler 建一个注入时钟的调度器：benchHeapSize 个 typed 定时器按 1ms 间隔排开，
// 每次 Tick 推进 benchPerTick 毫秒，正好触发 benchPerTick 个。onChange 计数，模拟宿主写存储的最小开销。
func benchScheduler(b *testing.B) (*Scheduler, *time.Time) {
	b.Helper()
	now := time.Unix(1000, 0)
	changes := 0
	s := NewScheduler(1, 0, nil, func(ChangeType, Node) { changes++ })
	s.SetClock(func() time.Time { return now })
	return s, &now
}

func benchArm(s *Scheduler, timerType int32) []int64 {
	ids := make([]int64, 0, benchHeapSize)
	for i := 1; i <= benchHeapSize; i++ {
		ids = append(ids, s.NewTimer(time.Duration(i)*time.Millisecond, timerType, int64(i), 0, []byte("payload")))
	}
	return ids
}

// 回调按返回值重排自己：Tick 的出堆、回调、入堆主路径。
func BenchmarkTickRearmByReturn(b *testing.B) {
	s, now := benchScheduler(b)
	s.RegisterHandler(1, func(Context) time.Duration { return benchHeapSize * time.Millisecond })
	benchArm(s, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		*now = now.Add(benchPerTick * time.Millisecond)
		s.Tick(*now)
	}
}

// 回调显式改期自己（覆盖返回值）：节点等最外层 Tick 收尾再入堆。
func BenchmarkTickExplicitRescheduleSelf(b *testing.B) {
	s, now := benchScheduler(b)
	s.RegisterHandler(1, func(ctx Context) time.Duration {
		s.ChangeTimer(ctx.Node.ID, ctx.Now, benchHeapSize*time.Millisecond)
		return 0
	})
	benchArm(s, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		*now = now.Add(benchPerTick * time.Millisecond)
		s.Tick(*now)
	}
}

// 回调改期另一个仍在堆里的定时器：目标移出堆、收尾放回。触发的节点按返回值重排自己。
func BenchmarkTickRescheduleOther(b *testing.B) {
	s, now := benchScheduler(b)
	var others []int64
	next := 0
	s.RegisterHandler(1, func(ctx Context) time.Duration {
		s.ChangeTimer(others[next%len(others)], ctx.Now, time.Hour)
		next++
		return benchHeapSize * time.Millisecond
	})
	s.RegisterHandler(2, func(Context) time.Duration { return 0 })
	benchArm(s, 1)
	for i := range benchHeapSize {
		others = append(others, s.NewTimer(time.Hour+time.Duration(i)*time.Millisecond, 2, 0, 0, nil))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		*now = now.Add(benchPerTick * time.Millisecond)
		s.Tick(*now)
	}
}

// 回调里新建一个定时器并结束自己：新节点等收尾入堆，堆大小不变。
func BenchmarkTickCreateInHandler(b *testing.B) {
	s, now := benchScheduler(b)
	s.RegisterHandler(1, func(ctx Context) time.Duration {
		s.NewTimer(benchHeapSize*time.Millisecond, 1, ctx.Node.Param1, 0, ctx.Node.Payload)
		return 0
	})
	benchArm(s, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		*now = now.Add(benchPerTick * time.Millisecond)
		s.Tick(*now)
	}
}

// Tick 外的登记与取消（宿主武装 / 撤销定时器的路径）。
func BenchmarkNewAndRemoveOutsideTick(b *testing.B) {
	s, _ := benchScheduler(b)
	s.RegisterHandler(1, func(Context) time.Duration { return 0 })
	benchArm(s, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		id := s.NewTimer(time.Duration(i%benchHeapSize+1)*time.Millisecond, 1, 0, 0, nil)
		s.RemoveTimer(id)
	}
}

// World 的用法：每次调用从存储节点重建调度器，Tick 一次后丢弃。
func BenchmarkRebuildAndTick(b *testing.B) {
	base := time.Unix(1000, 0)
	saved := make([]Node, 0, 1000)
	for i := 1; i <= 1000; i++ {
		saved = append(saved, Node{ID: int64(i), Type: 1, End: base.Add(time.Duration(i) * time.Millisecond), Delay: time.Second})
	}
	handler := func(Context) time.Duration { return time.Hour }
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s := NewScheduler(1, 1000, saved, func(ChangeType, Node) {})
		s.RegisterHandler(1, handler)
		s.SetClock(func() time.Time { return base })
		s.Tick(base.Add(benchPerTick * time.Millisecond))
	}
}
