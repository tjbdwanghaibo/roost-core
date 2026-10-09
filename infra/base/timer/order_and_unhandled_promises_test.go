package timer

import (
	"bytes"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 维护者决定 D-L1（2026-10-06）：同一期限的定时器按 (期限, priority, 登记顺序) 触发，priority 数值小的先触发，
// 缺省 priority 为 0，登记顺序就是节点 ID（同一个 Scheduler 的 ID 单调递增，改期与按返回值重排都保留 ID）。
// 旧实现的堆只比较期限，同期限的顺序由堆的形状决定：依次登记 1..5 会按 1、5、4、3、2 触发。
//
// 维护者决定 D-L2：到期节点的类型没有注册 handler 时，删除节点照旧，但打一条 Warn 并计
// timer.unhandled_dropped_total{kind=<类型>}；宿主加载时用 ReportUnhandledTypes 对没有 handler 的存量类型
// 每种告警一次。旧实现静默删除，没有日志也没有计数。

func fireOrder(t *testing.T, s *Scheduler, types ...int32) *[]int64 {
	t.Helper()
	fired := &[]int64{}
	for _, timerType := range types {
		s.RegisterHandler(timerType, func(c Context) time.Duration {
			*fired = append(*fired, c.Node.ID)
			return 0
		})
	}
	return fired
}

func TestTimersWithTheSameDeadlineFireInRegistrationOrder(t *testing.T) {
	now := time.Unix(100, 0)

	t.Run("armed in one scheduler", func(t *testing.T) {
		s := newPinnedScheduler(now)
		fired := fireOrder(t, s, 1, 2)
		var want []int64
		for i := range 6 {
			want = append(want, s.NewTimer(time.Second, int32(1+i%2), 0, 0, nil))
		}
		s.Tick(now.Add(time.Second))
		if !slices.Equal(*fired, want) {
			t.Fatalf("timers due at the same instant fired in order %v, want registration order %v", *fired, want)
		}
	})

	// 宿主从存储重建（World 每次调用都从 DAO 的 map 重建，map 的遍历顺序是随机的）：顺序仍由 ID 决定。
	t.Run("rebuilt from stored nodes in any order", func(t *testing.T) {
		end := now.Add(time.Second)
		saved := []Node{
			{ID: 4, Type: 1, End: end, Delay: time.Second},
			{ID: 2, Type: 1, End: end, Delay: time.Second},
			{ID: 6, Type: 1, End: end, Delay: time.Second},
			{ID: 1, Type: 1, End: end, Delay: time.Second},
			{ID: 5, Type: 1, End: end, Delay: time.Second},
			{ID: 3, Type: 1, End: end, Delay: time.Second},
		}
		s := NewScheduler(1, 0, saved, nil)
		fired := fireOrder(t, s, 1)
		s.Tick(end)
		if want := []int64{1, 2, 3, 4, 5, 6}; !slices.Equal(*fired, want) {
			t.Fatalf("stored timers due at the same instant fired in order %v, want %v", *fired, want)
		}
	})

	// 改期和按返回值重排都保留 ID，所以“登记顺序”指最初登记的顺序。
	t.Run("rescheduled timers keep their place", func(t *testing.T) {
		s := newPinnedScheduler(now)
		fired := fireOrder(t, s, 1)
		first := s.NewTimer(time.Minute, 1, 0, 0, nil)
		second := s.NewTimer(time.Second, 1, 0, 0, nil)
		if !s.ChangeTimer(first, now, time.Second) {
			t.Fatal("ChangeTimer returned false")
		}
		s.Tick(now.Add(time.Second))
		if want := []int64{first, second}; !slices.Equal(*fired, want) {
			t.Fatalf("fired %v, want %v: the postponed-then-advanced timer was registered first", *fired, want)
		}
	})
}

func TestPriorityOrdersTimersWithTheSameDeadline(t *testing.T) {
	now := time.Unix(100, 0)
	s := newPinnedScheduler(now)
	fired := fireOrder(t, s, 1)
	high := s.NewTimerWithPriority(time.Second, 1, 5, 0, 0, nil)
	urgent := s.NewTimerWithPriority(time.Second, 1, -1, 0, 0, nil)
	plain := s.NewTimer(time.Second, 1, 0, 0, nil)
	high2 := s.NewTimerWithPriority(time.Second, 1, 5, 0, 0, nil)
	plain2 := s.NewTimerWithPriority(time.Second, 1, 0, 0, 0, nil)
	// 期限先比：更早的期限即使 priority 更大也先触发。
	earlier := s.NewTimerWithPriority(time.Second-time.Millisecond, 1, 100, 0, 0, nil)

	s.Tick(now.Add(time.Second))
	want := []int64{earlier, urgent, plain, plain2, high, high2}
	if !slices.Equal(*fired, want) {
		t.Fatalf("fired %v, want %v (deadline, then priority ascending, then registration order)", *fired, want)
	}
}

func TestPriorityIsKeptThroughStorageAndRescheduling(t *testing.T) {
	now := time.Unix(100, 0)
	stored := map[int64]Node{}
	s := NewScheduler(1, 0, nil, func(change ChangeType, node Node) {
		if change == ChangeDelete {
			delete(stored, node.ID)
			return
		}
		stored[node.ID] = node
	})
	s.SetClock(func() time.Time { return now })
	s.RegisterHandler(1, func(Context) time.Duration { return time.Second }) // 重排自己
	id := s.NewTimerWithPriority(time.Second, 1, 7, 0, 0, nil)
	if got := stored[id].Priority; got != 7 {
		t.Fatalf("the change hook saw priority %d, want 7", got)
	}
	s.Tick(now.Add(time.Second))
	if got := stored[id].Priority; got != 7 {
		t.Fatalf("after re-arming by return value the stored priority is %d, want 7", got)
	}
	s.ChangeTimer(id, now, time.Minute)
	if got := stored[id].Priority; got != 7 {
		t.Fatalf("after ChangeTimer the stored priority is %d, want 7", got)
	}
	nodes := s.Nodes()
	if len(nodes) != 1 || nodes[0].Priority != 7 {
		t.Fatalf("Nodes() = %+v, want the priority carried", nodes)
	}
	rebuilt := NewScheduler(1, 0, nodes, nil)
	if got := rebuilt.Nodes(); len(got) != 1 || got[0].Priority != 7 {
		t.Fatalf("rebuilt Nodes() = %+v, want the priority carried", got)
	}
}

// captureWarnings 把默认 slog 换成写进缓冲区的 handler，把默认指标表换成新表；用例结束时都换回去。
func captureWarnings(t *testing.T) (*bytes.Buffer, *metrics.Registry) {
	t.Helper()
	var buf bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	registry := metrics.NewRegistry()
	previousRegistry := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(registry)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previousRegistry) })
	return &buf, registry
}

func counterValue(registry *metrics.Registry, name string, labels metrics.Labels) int64 {
	for _, metric := range registry.Snapshot() {
		if metric.Name == name && maps.Equal(metric.Labels, labels) {
			return metric.Value
		}
	}
	return 0
}

func TestADueTimerWithoutAHandlerIsDroppedWithAWarningAndACount(t *testing.T) {
	logs, registry := captureWarnings(t)
	now := time.Unix(100, 0)
	deleted := map[int64]bool{}
	s := NewScheduler(42, 0, nil, func(change ChangeType, node Node) {
		if change == ChangeDelete {
			deleted[node.ID] = true
		}
	})
	s.SetClock(func() time.Time { return now })
	handled := 0
	s.RegisterHandler(1, func(Context) time.Duration { handled++; return 0 })
	s.NewTimer(time.Second, 1, 0, 0, nil)
	orphanA := s.NewTimer(time.Second, 7, 0, 0, nil)
	orphanB := s.NewTimer(time.Second, 7, 0, 0, nil)

	s.Tick(now.Add(time.Second))
	if handled != 1 {
		t.Fatalf("the handled timer fired %d time(s), want 1", handled)
	}
	if !deleted[orphanA] || !deleted[orphanB] || len(s.Nodes()) != 0 {
		t.Fatalf("deleted = %v, nodes = %+v; a due timer with no handler is still removed", deleted, s.Nodes())
	}
	if got := counterValue(registry, UnhandledDroppedMetric, metrics.Labels{"kind": "7"}); got != 2 {
		t.Errorf("%s{kind=\"7\"} = %d, want 2", UnhandledDroppedMetric, got)
	}
	if got := counterValue(registry, UnhandledDroppedMetric, metrics.Labels{"kind": "1"}); got != 0 {
		t.Errorf("%s{kind=\"1\"} = %d for a handled type, want 0", UnhandledDroppedMetric, got)
	}
	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 2 {
		t.Fatalf("got %d warnings, want one per dropped node:\n%s", n, out)
	}
	for _, field := range []string{"owner_id=42", "type=7", "timer_id="} {
		if !strings.Contains(out, field) {
			t.Errorf("the warning does not carry %s:\n%s", field, out)
		}
	}
}

func TestStoredTypesWithoutAHandlerAreReportedOncePerType(t *testing.T) {
	logs, _ := captureWarnings(t)
	end := time.Unix(200, 0)
	s := NewScheduler(42, 0, []Node{
		{ID: 1, Type: 3, End: end},
		{ID: 2, Type: 3, End: end},
		{ID: 3, Type: 9, End: end},
		{ID: 4, Type: 1, End: end},
	}, nil)
	s.RegisterHandler(1, func(Context) time.Duration { return 0 })
	s.NewClosureTimer(time.Second, func(Context) time.Duration { return 0 }) // 闭包定时器自带 handler，不算

	got := s.ReportUnhandledTypes()
	if want := []int32{3, 9}; !slices.Equal(got, want) {
		t.Fatalf("ReportUnhandledTypes() = %v, want %v", got, want)
	}
	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 2 {
		t.Fatalf("got %d warnings, want one per type:\n%s", n, out)
	}
	if !strings.Contains(out, "type=3") || !strings.Contains(out, "nodes=2") || !strings.Contains(out, "type=9") {
		t.Errorf("the warnings do not name the types and how many nodes each has:\n%s", out)
	}
	// 加载时只报告，不删除、不计数：节点到期才删。
	if len(s.Nodes()) != 4 {
		t.Fatalf("reporting changed the schedule: %+v", s.Nodes())
	}
}
