package timer

import (
	"testing"
	"time"
)

// RR-20261005-NC-141：Tick 期间（另一个定时器的 handler 里）取消或改期一个“本次 Tick 也到期、但还没触发”的定时器，
// 旧实现把操作推迟到 Tick 结束才执行，于是被取消的定时器照样按旧期限触发，改期的定时器先按旧期限触发、
// 之后改期找不到节点而丢失——而两个调用都返回了 true。承诺：返回 true 之后，它不再按旧期限触发。
//
// 用注入时钟和显式 Tick 时刻控制顺序：A 的期限早于 B，同一次 Tick 里 A 先触发。

func newPinnedScheduler(now time.Time) *Scheduler {
	s := NewScheduler(1, 0, nil, nil)
	s.SetClock(func() time.Time { return now })
	return s
}

func TestRemoveFromAHandlerStopsATimerDueInTheSameTick(t *testing.T) {
	now := time.Unix(100, 0)
	s := newPinnedScheduler(now)
	var victim int64
	var removed bool
	firedVictim := 0
	s.RegisterHandler(1, func(Context) time.Duration { removed = s.RemoveTimer(victim); return 0 })
	s.RegisterHandler(2, func(Context) time.Duration { firedVictim++; return 0 })
	s.NewTimer(time.Second, 1, 0, 0, nil)
	victim = s.NewTimer(2*time.Second, 2, 0, 0, nil)

	s.Tick(now.Add(3 * time.Second))
	if !removed {
		t.Fatal("RemoveTimer of a pending timer returned false")
	}
	if firedVictim != 0 {
		t.Fatalf("a timer cancelled before it fired still fired %d time(s) in the same tick", firedVictim)
	}
	if len(s.Nodes()) != 0 {
		t.Fatalf("nodes = %+v, want none", s.Nodes())
	}
}

func TestChangeFromAHandlerPostponesATimerDueInTheSameTick(t *testing.T) {
	now := time.Unix(100, 0)
	s := newPinnedScheduler(now)
	var victim int64
	var changed bool
	firedVictim := 0
	s.RegisterHandler(1, func(Context) time.Duration { changed = s.ChangeTimer(victim, time.Time{}, time.Hour); return 0 })
	s.RegisterHandler(2, func(Context) time.Duration { firedVictim++; return 0 })
	s.NewTimer(time.Second, 1, 0, 0, nil)
	victim = s.NewTimer(2*time.Second, 2, 0, 0, nil)

	s.Tick(now.Add(3 * time.Second))
	if !changed {
		t.Fatal("ChangeTimer of a pending timer returned false")
	}
	if firedVictim != 0 {
		t.Fatalf("a timer postponed by an hour still fired %d time(s) on its old deadline", firedVictim)
	}
	nodes := s.Nodes()
	if len(nodes) != 1 || nodes[0].ID != victim || !nodes[0].End.Equal(now.Add(time.Hour)) {
		t.Fatalf("nodes = %+v, want the postponed timer due at %v", nodes, now.Add(time.Hour))
	}
	s.Tick(now.Add(time.Hour))
	if firedVictim != 1 {
		t.Fatalf("the postponed timer fired %d time(s) at its new deadline, want 1", firedVictim)
	}
}

// 邻近分支：推迟语义原本覆盖的几种情形保持不变——handler 取消自己（即使返回了重排间隔）、
// 同一个 handler 里新建再取消、Tick 期间新建的定时器不在本次 Tick 触发、改自己的期限覆盖返回值。
func TestDeferredOperationsDuringTickKeepTheirMeaning(t *testing.T) {
	now := time.Unix(100, 0)

	t.Run("a handler cancels itself and asks to come back", func(t *testing.T) {
		s := newPinnedScheduler(now)
		var self int64
		fired := 0
		s.RegisterHandler(1, func(Context) time.Duration { fired++; s.RemoveTimer(self); return time.Second })
		self = s.NewTimer(time.Second, 1, 0, 0, nil)
		s.Tick(now.Add(time.Second))
		s.Tick(now.Add(time.Hour))
		if fired != 1 || len(s.Nodes()) != 0 {
			t.Fatalf("fired = %d, nodes = %+v; a self-cancelled timer must not come back", fired, s.Nodes())
		}
	})
	t.Run("created and cancelled inside one handler", func(t *testing.T) {
		s := newPinnedScheduler(now)
		s.RegisterHandler(1, func(Context) time.Duration {
			id := s.NewTimer(time.Second, 2, 0, 0, nil)
			if !s.RemoveTimer(id) {
				t.Error("removing a timer created in the same handler returned false")
			}
			return 0
		})
		s.RegisterHandler(2, func(Context) time.Duration { t.Error("the cancelled new timer fired"); return 0 })
		s.NewTimer(time.Second, 1, 0, 0, nil)
		s.Tick(now.Add(time.Second))
		s.Tick(now.Add(time.Hour))
		if len(s.Nodes()) != 0 {
			t.Fatalf("nodes = %+v", s.Nodes())
		}
	})
	t.Run("a timer created during a tick waits for the next one", func(t *testing.T) {
		s := NewScheduler(1, 0, nil, nil)
		clock := now
		s.SetClock(func() time.Time { return clock })
		fired := 0
		s.RegisterHandler(1, func(Context) time.Duration { s.NewTimer(time.Millisecond, 2, 0, 0, nil); return 0 })
		s.RegisterHandler(2, func(Context) time.Duration { fired++; return 0 })
		s.NewTimer(time.Second, 1, 0, 0, nil)
		s.Tick(now.Add(time.Hour))
		if fired != 0 {
			t.Fatalf("a timer created during the tick fired in that tick (%d)", fired)
		}
		s.Tick(now.Add(time.Hour))
		if fired != 1 {
			t.Fatalf("the new timer fired %d time(s) on the next tick, want 1", fired)
		}
	})
	t.Run("a handler reschedules itself", func(t *testing.T) {
		s := newPinnedScheduler(now)
		var self int64
		s.RegisterHandler(1, func(Context) time.Duration { s.ChangeTimer(self, time.Time{}, time.Minute); return time.Second })
		self = s.NewTimer(time.Second, 1, 0, 0, nil)
		s.Tick(now.Add(time.Second))
		nodes := s.Nodes()
		if len(nodes) != 1 || !nodes[0].End.Equal(now.Add(time.Minute)) {
			t.Fatalf("nodes = %+v, want the explicit change to win over the returned interval", nodes)
		}
	})
}

// 持久化钩子看到的变化序列与最终状态一致：被取消的到期定时器只发一次删除，被改期的只发一次 upsert。
func TestChangesSeenByTheHookDuringATickMatchTheFinalState(t *testing.T) {
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
	var cancelled, postponed int64
	s.RegisterHandler(1, func(Context) time.Duration {
		s.RemoveTimer(cancelled)
		s.ChangeTimer(postponed, time.Time{}, time.Hour)
		return 0
	})
	s.RegisterHandler(2, func(Context) time.Duration { t.Error("a cancelled or postponed timer fired"); return 0 })
	s.NewTimer(time.Second, 1, 0, 0, nil)
	cancelled = s.NewTimer(2*time.Second, 2, 0, 0, nil)
	postponed = s.NewTimer(2*time.Second, 2, 0, 0, nil)
	s.Tick(now.Add(3 * time.Second))
	if _, ok := stored[cancelled]; ok {
		t.Error("the cancelled timer is still stored")
	}
	if node, ok := stored[postponed]; !ok || !node.End.Equal(now.Add(time.Hour)) {
		t.Errorf("stored postponed timer = %+v (present %v), want End %v", node, ok, now.Add(time.Hour))
	}
	if len(stored) != len(s.Nodes()) {
		t.Errorf("hook view has %d nodes, scheduler has %d", len(stored), len(s.Nodes()))
	}
}

// RR-20261005-NC-147：handler 里再调用 Tick（重入）时，旧实现在内层 Tick 结束时把 running 清成 false 并提前执行
// 外层登记的推迟操作；回到外层 handler 后，“取消正在触发的自己”不再被推迟，直接查不到节点返回 false，
// 外层随后按返回值把自己重新排上。承诺：重入的 Tick 不改变外层 Tick 期间操作的语义，推迟操作在最外层 Tick 结束时执行。
func TestReentrantTickKeepsTheOuterTickSemantics(t *testing.T) {
	now := time.Unix(100, 0)
	s := newPinnedScheduler(now)
	var outer int64
	var removed bool
	innerFired := 0
	s.RegisterHandler(1, func(c Context) time.Duration {
		s.Tick(c.Now) // re-entrant: fires the other due timer
		removed = s.RemoveTimer(outer)
		return time.Second
	})
	s.RegisterHandler(2, func(Context) time.Duration { innerFired++; return 0 })
	outer = s.NewTimer(time.Second, 1, 0, 0, nil)
	s.NewTimer(2*time.Second, 2, 0, 0, nil)

	s.Tick(now.Add(3 * time.Second))
	if innerFired != 1 {
		t.Fatalf("the re-entrant tick fired the other timer %d time(s), want 1", innerFired)
	}
	if !removed {
		t.Fatal("cancelling the firing timer after a re-entrant tick returned false")
	}
	if nodes := s.Nodes(); len(nodes) != 0 {
		t.Fatalf("nodes = %+v; the timer that cancelled itself was re-armed", nodes)
	}
}

// NC-141 修复引入的“改期后放回前”窗口：同一次 Tick 里先改期再取消，结果是取消；改期两次，后一次生效；
// handler panic 时改期中的节点照样放回，不会从堆里消失。
func TestPostponedTimerDuringTickComposesWithLaterOperations(t *testing.T) {
	now := time.Unix(100, 0)
	setup := func(op func(s *Scheduler, victim int64)) (*Scheduler, int64) {
		s := newPinnedScheduler(now)
		var victim int64
		s.RegisterHandler(1, func(Context) time.Duration { op(s, victim); return 0 })
		s.RegisterHandler(2, func(Context) time.Duration { t.Error("the victim fired on its old deadline"); return 0 })
		s.NewTimer(time.Second, 1, 0, 0, nil)
		victim = s.NewTimer(2*time.Second, 2, 0, 0, nil)
		return s, victim
	}
	t.Run("postponed then cancelled", func(t *testing.T) {
		s, _ := setup(func(s *Scheduler, victim int64) {
			s.ChangeTimer(victim, time.Time{}, time.Hour)
			if !s.RemoveTimer(victim) {
				t.Error("cancelling a postponed timer returned false")
			}
		})
		s.Tick(now.Add(3 * time.Second))
		if nodes := s.Nodes(); len(nodes) != 0 {
			t.Fatalf("nodes = %+v, want the postponed timer cancelled", nodes)
		}
	})
	t.Run("postponed twice", func(t *testing.T) {
		s, victim := setup(func(s *Scheduler, victim int64) {
			s.ChangeTimer(victim, time.Time{}, time.Hour)
			s.ChangeTimer(victim, time.Time{}, time.Minute)
		})
		s.Tick(now.Add(3 * time.Second))
		nodes := s.Nodes()
		if len(nodes) != 1 || nodes[0].ID != victim || !nodes[0].End.Equal(now.Add(time.Minute)) {
			t.Fatalf("nodes = %+v, want the last change (+1m) to win", nodes)
		}
	})
	t.Run("the handler panics after postponing", func(t *testing.T) {
		s, victim := setup(func(s *Scheduler, victim int64) {
			s.ChangeTimer(victim, time.Time{}, time.Hour)
			panic("handler failed")
		})
		func() {
			defer func() { _ = recover() }()
			s.Tick(now.Add(3 * time.Second))
		}()
		nodes := s.Nodes()
		if len(nodes) != 1 || nodes[0].ID != victim || !nodes[0].End.Equal(now.Add(time.Hour)) {
			t.Fatalf("nodes = %+v, want the postponed timer back in the heap", nodes)
		}
		if s.running || len(s.deferred) != 0 {
			t.Fatalf("running = %v, deferred = %d after a panicking tick", s.running, len(s.deferred))
		}
	})
}
