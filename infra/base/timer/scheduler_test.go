package timer

import (
	"fmt"
	"testing"
	"time"
)

// 回调内显式改期覆盖返回间隔（包括零和负值），并保留可恢复的完整 typed 节点。
func TestFiringTimerExplicitRescheduleOverridesReturn(t *testing.T) {
	base := time.Unix(100, 0)
	for _, closure := range []bool{false, true} {
		for _, returned := range []time.Duration{0, -time.Second, time.Second} {
			t.Run(fmt.Sprintf("closure_%v/return_%v", closure, returned), func(t *testing.T) {
				stored := make(map[int64]Node)
				s := NewScheduler(1, 0, nil, func(change ChangeType, node Node) {
					if change == ChangeDelete {
						delete(stored, node.ID)
					} else {
						stored[node.ID] = node
					}
				})
				s.SetClock(func() time.Time { return base })
				fired := 0
				h := func(ctx Context) time.Duration {
					fired++
					if fired == 1 {
						if !s.ChangeTimer(ctx.Node.ID, time.Time{}, time.Minute) {
							t.Fatal("explicit reschedule refused")
						}
						return returned
					}
					return 0
				}
				var id int64
				if closure {
					id = s.NewClosureTimer(time.Second, h)
				} else {
					s.RegisterHandler(1, h)
					id = s.NewTimerWithPriority(time.Second, 1, 7, 2, 3, []byte("march"))
				}
				s.Tick(base.Add(time.Second))
				deadline := base.Add(time.Minute)
				if got := s.NextTime(); !got.Equal(deadline) {
					t.Fatalf("next = %v, want explicit deadline %v", got, deadline)
				}
				if closure {
					if len(stored) != 0 || len(s.Nodes()) != 0 {
						t.Fatal("closure timer became persistent")
					}
				} else {
					node, ok := stored[id]
					if !ok || len(stored) != 1 || !node.End.Equal(deadline) || node.Delay != time.Minute || node.Priority != 7 || node.Param1 != 2 || node.Param2 != 3 || string(node.Payload) != "march" {
						t.Fatalf("stored rescheduled timer = %+v, present=%v", node, ok)
					}
					restored := NewScheduler(1, s.Seed(), []Node{node}, nil)
					restoredFired := 0
					restored.RegisterHandler(1, func(Context) time.Duration { restoredFired++; return 0 })
					restored.Tick(deadline.Add(-time.Nanosecond))
					if restoredFired != 0 {
						t.Fatal("restored timer fired before explicit deadline")
					}
					restored.Tick(deadline)
					if restoredFired != 1 {
						t.Fatalf("restored fired=%d, want 1", restoredFired)
					}
				}
				s.Tick(deadline.Add(-time.Nanosecond))
				if fired != 1 {
					t.Fatalf("fired %d times before explicit deadline", fired)
				}
				s.Tick(deadline)
				if fired != 2 || !s.NextTime().IsZero() || len(stored) != 0 {
					t.Fatalf("after deadline fired=%d next=%v stored=%v", fired, s.NextTime(), stored)
				}
			})
		}
	}
}

// 正在触发的节点在最外层 Tick 收尾前保持移出；取消后不能被返回间隔或后续显式改期复活。
func TestFiringTimerOperationsComposeAcrossReentrantTicks(t *testing.T) {
	base := time.Unix(100, 0)
	for _, operation := range []string{"changed_twice", "cancel_then_change", "change_then_cancel", "nested_change", "later_change", "later_cancel", "nested_future_tick", "panic_after_change"} {
		t.Run(operation, func(t *testing.T) {
			s := newPinnedScheduler(base)
			fired := 0
			var self int64
			change := func(delay time.Duration) {
				if !s.ChangeTimer(self, time.Time{}, delay) {
					t.Fatal("reschedule refused")
				}
			}
			cancel := func() {
				if !s.RemoveTimer(self) {
					t.Fatal("cancellation refused")
				}
			}
			s.RegisterHandler(1, func(ctx Context) time.Duration {
				fired++
				if fired > 1 {
					return 0
				}
				switch operation {
				case "changed_twice":
					change(time.Hour)
					change(time.Minute)
				case "cancel_then_change":
					cancel()
					change(time.Minute)
				case "change_then_cancel":
					change(time.Minute)
					cancel()
				case "nested_change":
					s.Tick(ctx.Now)
					change(time.Minute)
				case "later_change", "later_cancel":
					change(time.Hour)
				case "nested_future_tick":
					change(time.Minute)
					return time.Second
				case "panic_after_change":
					change(time.Minute)
					panic("business handler failed")
				}
				return 0
			})
			s.RegisterHandler(2, func(Context) time.Duration {
				switch operation {
				case "nested_change":
					change(time.Hour)
				case "later_change":
					change(time.Minute)
				case "later_cancel":
					cancel()
				case "nested_future_tick":
					s.Tick(base.Add(time.Hour))
				}
				return 0
			})
			self = s.NewTimer(time.Second, 1, 0, 0, nil)
			if operation != "panic_after_change" {
				s.NewTimer(2*time.Second, 2, 0, 0, nil)
			}
			var panicValue any
			func() { defer func() { panicValue = recover() }(); s.Tick(base.Add(3 * time.Second)) }()
			if (panicValue != nil) != (operation == "panic_after_change") {
				t.Fatalf("unexpected panic: %v", panicValue)
			}
			if fired != 1 {
				t.Fatalf("fired=%d, explicit change must wait for the next outer Tick", fired)
			}
			cancelled := operation == "cancel_then_change" || operation == "change_then_cancel" || operation == "later_cancel"
			if cancelled {
				if !s.NextTime().IsZero() {
					t.Fatalf("cancelled timer reappeared: %+v", s.Nodes())
				}
			} else if !s.NextTime().Equal(base.Add(time.Minute)) {
				t.Fatalf("next=%v, want last explicit change +1m", s.NextTime())
			}
			if s.running || len(s.deferred) != 0 || len(s.firing) != 0 || len(s.firingChanges) != 0 {
				t.Fatalf("Tick retained running=%v deferred=%d firing=%d controls=%d", s.running, len(s.deferred), len(s.firing), len(s.firingChanges))
			}
		})
	}
}

func TestSchedulerFiresAndPersistsChanges(t *testing.T) {
	var changes []ChangeType
	s := NewScheduler(101, 0, nil, func(change ChangeType, _ Node) {
		changes = append(changes, change)
	})
	var fired int
	s.RegisterHandler(1, func(ctx Context) time.Duration {
		fired++
		if ctx.OwnerID != 101 {
			t.Fatalf("owner id = %d, want 101", ctx.OwnerID)
		}
		if ctx.Node.Param1 != 2 || ctx.Node.Param2 != 3 || string(ctx.Node.Payload) != "p" {
			t.Fatalf("unexpected node: %+v", ctx.Node)
		}
		return 0
	})

	id := s.NewTimer(time.Millisecond, 1, 2, 3, []byte("p"))
	if id == 0 {
		t.Fatal("expected timer id")
	}
	nodes := s.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("nodes len = %d, want 1", len(nodes))
	}
	s.Tick(nodes[0].End.Add(time.Nanosecond))
	if fired != 1 {
		t.Fatalf("fired = %d, want 1", fired)
	}
	if len(s.Nodes()) != 0 {
		t.Fatalf("expected timer removed after one-shot fire")
	}
	if len(changes) != 2 || changes[0] != ChangeUpsert || changes[1] != ChangeDelete {
		t.Fatalf("changes = %+v, want upsert/delete", changes)
	}
}

func TestSchedulerReschedulesFromHandler(t *testing.T) {
	now := time.Unix(100, 0)
	s := NewScheduler(1, 7, []Node{{
		ID:    8,
		Type:  2,
		End:   now,
		Delay: time.Second,
	}}, nil)
	var fired int
	s.RegisterHandler(2, func(Context) time.Duration {
		fired++
		if fired == 1 {
			return 2 * time.Second
		}
		return 0
	})

	s.Tick(now)
	if fired != 1 {
		t.Fatalf("first fired = %d, want 1", fired)
	}
	next := s.NextTime()
	if !next.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("next = %v, want %v", next, now.Add(2*time.Second))
	}
	s.Tick(now.Add(2 * time.Second))
	if fired != 2 {
		t.Fatalf("second fired = %d, want 2", fired)
	}
}

func TestSchedulerSetClockStampsNewTimers(t *testing.T) {
	s := NewScheduler(1, 0, nil, nil)
	base := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	s.SetClock(func() time.Time { return base })
	s.RegisterHandler(7, func(Context) time.Duration { return 0 })
	id := s.NewTimer(10*time.Second, 7, 0, 0, nil)
	if id == 0 {
		t.Fatal("NewTimer rejected")
	}
	nodes := s.Nodes()
	if len(nodes) != 1 || !nodes[0].End.Equal(base.Add(10*time.Second)) {
		t.Fatalf("End = %v, want injected clock + delay (wall-clock regression)", nodes[0].End)
	}
	// The tick driven by the same clock fires it exactly on schedule.
	fired := 0
	s.RegisterHandler(7, func(Context) time.Duration { fired++; return 0 })
	s.Tick(base.Add(10*time.Second + time.Nanosecond))
	if fired != 1 {
		t.Fatalf("fired = %d", fired)
	}
}

// Tick 期间（另一个定时器的 handler 里）取消或改期一个“本次 Tick 也到期、但还没触发”的定时器：返回 true 之后，它不再按旧期限触发
// （被取消的不触发，改期的按新期限触发、不丢失）。
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

// handler 里再调用 Tick（重入）不改变外层 Tick 期间操作的语义：推迟操作在最外层 Tick 结束时执行，外层 handler 里“取消正在触发的
// 自己”仍被推迟。
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

// “改期后放回前”的窗口：同一次 Tick 里先改期再取消，结果是取消；改期两次，后一次生效；handler panic 时改期中的节点照样放回，
// 不会从堆里消失。
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

func TestInvalidSavedNodesEmitPersistenceDeletion(t *testing.T) {
	now := time.Unix(100, 0)
	var removed []int64
	s := NewScheduler(1, 0, []Node{{ID: -1, Type: 1, End: now}, {ID: 2, Type: 0, End: now}, {ID: 3, Type: 1}, {ID: 4, Type: 1, End: now}}, func(change ChangeType, node Node) {
		if change != ChangeDelete {
			t.Errorf("invalid restore upserted")
		}
		removed = append(removed, node.ID)
	})
	if len(removed) != 0 {
		t.Fatal("constructor changed storage outside caller transaction")
	}
	s.Tick(now.Add(-time.Second))
	if len(removed) != 3 || len(s.Nodes()) != 1 || s.Nodes()[0].ID != 4 {
		t.Fatalf("invalid nodes remain in persistence: removed=%v live=%v", removed, s.Nodes())
	}
}
