package timer

import (
	"fmt"
	"slices"
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
					// 取消即移出 byID：之后的改期找不到节点，返回 false 且不复活。
					if s.ChangeTimer(self, time.Time{}, time.Minute) {
						t.Fatal("reschedule after cancellation was accepted")
					}
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
			// 收尾后不残留回调控制状态（原断言 deferred / firing / firingChanges 为空，这些字段已随重构删除）。
			checkSettled(t, s)
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

// checkSettled 核对 Tick 外的调度器不变量：没有在途的 Tick 与收尾队列，byID 与堆一一对应且都在堆中、
// 堆的下标正确、堆序成立。
func checkSettled(t *testing.T, s *Scheduler) {
	t.Helper()
	if s.running || len(s.queued) != 0 {
		t.Fatalf("Tick retained running=%v queued=%d", s.running, len(s.queued))
	}
	if len(s.byID) != len(s.nodes) {
		t.Fatalf("byID has %d nodes, heap has %d", len(s.byID), len(s.nodes))
	}
	for i, node := range s.nodes {
		if int(node.index) != i || node.state != stateHeap || s.byID[node.ID] != node {
			t.Fatalf("heap[%d] = id %d index %d state %d, byID match %v", i, node.ID, node.index, node.state, s.byID[node.ID] == node)
		}
		if i > 0 && s.nodes.Less(i, (i-1)/2) {
			t.Fatalf("heap order broken at %d", i)
		}
	}
}

// checkEveryIDAnnounced：调度器分配过的每个 typed ID 都至少经过一次 upsert。宿主（World）按 upsert 推进
// 持久 seed，漏掉的 ID 在重建后会再次分配，与已经交给调用方的 ID 重复。只用于全部是 typed 节点的场景。
func checkEveryIDAnnounced(t *testing.T, s *Scheduler, upserted map[int64]bool) {
	t.Helper()
	for id := int64(1); id <= s.Seed(); id++ {
		if !upserted[id] {
			t.Errorf("allocated ID %d never reached the hook as an upsert (seed %d); a rebuilt scheduler would reuse it", id, s.Seed())
		}
	}
}

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
		// 原断言 running 与 deferred 为空；deferred 已随重构删除，改查收尾后的完整不变量。
		checkSettled(t, s)
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

// 最外层 Tick 收尾时 onChange 钩子 panic：调度器状态已经落定，之后的 Tick 不重放那次收尾的变更，
// 也不残留回调控制状态（长期复用同一个 Scheduler 的宿主依赖这一点；World 每次重建不受影响）。
// 历史：见 1d55586f（收尾闭包与控制表在钩子 panic 时不清空，再 Tick 会重放）。
func TestOnChangePanicDuringSettlementDoesNotReplay(t *testing.T) {
	base := time.Unix(100, 0)
	for _, scenario := range []string{"child_then_idle_tick", "self_reschedule_then_idle_tick", "self_reschedule_then_due_tick"} {
		t.Run(scenario, func(t *testing.T) {
			var events []string
			failNextUpsert := false
			s := NewScheduler(1, 0, nil, func(change ChangeType, node Node) {
				if failNextUpsert && change == ChangeUpsert {
					failNextUpsert = false
					panic("storage hook failed")
				}
				events = append(events, fmt.Sprintf("%d:%d@%v", change, node.ID, node.End.Sub(base)))
			})
			s.SetClock(func() time.Time { return base })
			var self int64
			fired := 0
			s.RegisterHandler(2, func(Context) time.Duration { return 0 })
			s.RegisterHandler(1, func(ctx Context) time.Duration {
				fired++
				if fired > 1 {
					return time.Second // 之后的触发按返回值重排自己
				}
				if scenario == "child_then_idle_tick" {
					s.NewTimer(time.Hour, 2, 0, 0, nil)
				} else {
					s.ChangeTimer(self, time.Time{}, time.Minute)
				}
				failNextUpsert = true // 只让收尾时的第一次 upsert 失败
				return 0
			})
			self = s.NewTimer(time.Second, 1, 0, 0, nil)
			var panicValue any
			func() { defer func() { panicValue = recover() }(); s.Tick(base.Add(time.Second)) }()
			if panicValue == nil {
				t.Fatal("the hook did not panic during settlement")
			}
			heapNodes := len(s.Nodes())
			events = nil
			if scenario != "self_reschedule_then_due_tick" {
				// 没有到期节点的 Tick 不应产生任何变更，也不应改变堆。
				s.Tick(base.Add(2 * time.Second))
				if len(events) != 0 || len(s.Nodes()) != heapNodes {
					t.Fatalf("an idle Tick replayed the failed settlement: events=%v nodes %d -> %d (%+v)", events, heapNodes, len(s.Nodes()), s.Nodes())
				}
				return
			}
			// 自己按显式期限 +1m 触发后返回 1s，必须按返回值重排在 +1m+1s，持久化钩子只看到这次触发的删除与重排。
			s.Tick(base.Add(time.Minute))
			if want := base.Add(time.Minute + time.Second); fired != 2 || !s.NextTime().Equal(want) || len(s.Nodes()) != 1 {
				t.Fatalf("after firing at the explicit deadline fired=%d next=%v nodes=%d, want fired 2 and one node due %v", fired, s.NextTime().Sub(base), len(s.Nodes()), want.Sub(base))
			}
			if want := []string{"2:1@1m0s", "1:1@1m1s"}; !slices.Equal(events, want) {
				t.Fatalf("events=%v, want %v", events, want)
			}
		})
	}
}

// 操作 × 节点状态组合：Tick 期间对处在每种状态的目标做改期 / 取消 / 不操作，触发中的回调返回 0 / 负 / 正，
// 操作分别在外层回调里直接做和在重入 Tick 触发的回调里做。断言本次 Tick 后目标的期限（或不存在）、
// 本次 Tick 里目标触发的次数、操作返回值、持久化钩子视图与调度器一致、收尾不变量；再在期望期限前后各
// Tick 一次，确认目标恰好按该期限触发一次。行为表见 docs/maintenance/REFACTOR-2026-10-17-timer-scheduler.md §2.2。
func TestOperationsOnEveryNodeStateDuringTick(t *testing.T) {
	const absent = time.Duration(-1)
	base := time.Unix(100, 0)
	tickAt := base.Add(3 * time.Second)
	type row struct {
		state string        // 目标在操作那一刻的状态
		op    string        // change(+1m) / remove / none
		ret   time.Duration // 外层回调 F 第一次触发的返回值
		want  time.Duration // 本次 Tick 后目标期限相对 base 的偏移；absent 表示不存在
		fired int           // 本次 Tick 里目标（F 自己除外）触发的次数
	}
	rows := []row{
		{"heap", "change", 0, time.Minute, 0}, {"heap", "remove", 0, absent, 0}, {"heap", "none", 0, time.Hour, 0},
		{"heap_due", "change", 0, time.Minute, 0}, {"heap_due", "remove", 0, absent, 0}, {"heap_due", "none", 0, absent, 1},
		{"queued_fresh", "change", 0, time.Minute, 0}, {"queued_fresh", "remove", 0, absent, 0}, {"queued_fresh", "none", 0, 10 * time.Minute, 0},
		{"queued_moved", "change", 0, time.Minute, 0}, {"queued_moved", "remove", 0, absent, 0}, {"queued_moved", "none", 0, 10 * time.Minute, 0},
		{"cancelled", "change", 0, absent, 0}, {"cancelled", "remove", 0, absent, 0}, {"cancelled", "none", 0, absent, 0},
		{"done", "change", 0, absent, 1}, {"done", "remove", 0, absent, 1}, {"done", "none", 0, absent, 1},
		{"rearmed", "change", 0, time.Minute, 1}, {"rearmed", "remove", 0, absent, 1}, {"rearmed", "none", 0, 3*time.Second + time.Hour, 1},
		{"unknown", "change", 0, absent, 0}, {"unknown", "remove", 0, absent, 0},
	}
	for _, ret := range []time.Duration{0, -time.Second, time.Second} {
		byReturn := absent
		if ret > 0 {
			byReturn = 3*time.Second + ret // 按本次 Tick 的时刻重排
		}
		rows = append(rows,
			row{"firing", "change", ret, time.Minute, 0}, row{"firing", "remove", ret, absent, 0}, row{"firing", "none", ret, byReturn, 0},
			row{"queued_fired", "change", ret, time.Minute, 0}, row{"queued_fired", "remove", ret, absent, 0}, row{"queued_fired", "none", ret, time.Hour, 0},
		)
	}
	for _, via := range []string{"direct", "nested_tick"} {
		for _, r := range rows {
			t.Run(fmt.Sprintf("%s/%s/%s/return_%v", via, r.state, r.op, r.ret), func(t *testing.T) {
				mirror := make(map[int64]time.Time)
				upserted := make(map[int64]bool)
				s := NewScheduler(1, 0, nil, func(change ChangeType, node Node) {
					if change == ChangeDelete {
						if _, ok := mirror[node.ID]; !ok {
							t.Errorf("hook got a delete for %d it does not hold", node.ID)
						}
						delete(mirror, node.ID)
						return
					}
					mirror[node.ID] = node.End
					upserted[node.ID] = true
				})
				s.SetClock(func() time.Time { return base })
				fires := make(map[int64]int)
				var outer, target int64
				opOK := true
				// 读视图（Lookup / LiveNodes）与操作看到的是同一个活节点集合：操作前可读当且仅当改期 / 取消会成功，
				// 操作后立即读到新期限或读不到。
				checkLive := func(want bool, end time.Time) {
					t.Helper()
					node, ok := s.Lookup(target)
					if ok != want || (ok && (node.ID != target || !node.End.Equal(end))) {
						t.Errorf("Lookup(%d) = %+v present=%v, want present=%v end=%v", target, node, ok, want, end)
					}
					live := s.LiveNodes()
					listed := slices.ContainsFunc(live, func(node Node) bool { return node.ID == target })
					if listed != want || !slices.IsSortedFunc(live, func(a, b Node) int {
						if firesBefore(&a, &b) {
							return -1
						}
						return 1
					}) {
						t.Errorf("LiveNodes() = %+v, want target listed=%v in firing order", live, want)
					}
				}
				operate := func() {
					current, present := s.Lookup(target)
					switch r.op {
					case "change":
						opOK = s.ChangeTimer(target, time.Time{}, time.Minute)
						if opOK != present {
							t.Errorf("Lookup said present=%v but change returned %v", present, opOK)
						}
						checkLive(opOK, base.Add(time.Minute))
					case "remove":
						opOK = s.RemoveTimer(target)
						if opOK != present {
							t.Errorf("Lookup said present=%v but remove returned %v", present, opOK)
						}
						checkLive(false, time.Time{})
					default:
						checkLive(present, current.End)
					}
				}
				s.RegisterHandler(1, func(ctx Context) time.Duration { // F
					fires[ctx.Node.ID]++
					if fires[ctx.Node.ID] > 1 {
						return 0
					}
					switch r.state {
					case "queued_fresh":
						target = s.NewTimer(10*time.Minute, 3, 0, 0, nil)
					case "queued_moved":
						s.ChangeTimer(target, time.Time{}, 10*time.Minute)
					case "queued_fired":
						s.ChangeTimer(outer, time.Time{}, time.Hour)
					case "cancelled":
						s.RemoveTimer(target)
					}
					if via == "direct" {
						operate()
					} else {
						s.Tick(ctx.Now) // 重入：触发 G，由 G 的回调操作目标
					}
					return r.ret
				})
				s.RegisterHandler(2, func(ctx Context) time.Duration { // G
					if via == "nested_tick" {
						operate()
					}
					return 0
				})
				s.RegisterHandler(3, func(ctx Context) time.Duration { fires[ctx.Node.ID]++; return 0 })
				s.RegisterHandler(4, func(ctx Context) time.Duration {
					fires[ctx.Node.ID]++
					if fires[ctx.Node.ID] == 1 {
						return time.Hour
					}
					return 0
				})
				switch r.state {
				case "heap", "cancelled":
					target = s.NewTimer(time.Hour, 3, 0, 0, nil)
				case "heap_due", "queued_moved":
					target = s.NewTimer(2*time.Second, 3, 0, 0, nil)
				case "done":
					target = s.NewTimer(time.Second/2, 3, 0, 0, nil)
				case "rearmed":
					target = s.NewTimer(time.Second/2, 4, 0, 0, nil)
				case "unknown":
					target = 999
				}
				outer = s.NewTimer(time.Second, 1, 0, 0, nil)
				s.NewTimer(time.Second+time.Second/2, 2, 0, 0, nil)
				if r.state == "firing" || r.state == "queued_fired" {
					target = outer
				}

				s.Tick(tickAt)
				checkSettled(t, s)
				checkEveryIDAnnounced(t, s, upserted)
				// 目标已不在 byID（已取消、本轮已触发结束、从未登记）时返回 false，与 Tick 外一致。
				wantOK := r.op == "none" || (r.state != "cancelled" && r.state != "done" && r.state != "unknown")
				if opOK != wantOK {
					t.Fatalf("%s during the tick returned %v, want %v", r.op, opOK, wantOK)
				}
				if got := fires[target]; target != outer && got != r.fired {
					t.Fatalf("target fired %d times in the tick, want %d", got, r.fired)
				}
				if fires[outer] != 1 {
					t.Fatalf("the outer timer fired %d times in the tick, want 1", fires[outer])
				}
				nodes := s.Nodes()
				if len(nodes) != len(mirror) {
					t.Fatalf("hook holds %v, scheduler holds %+v", mirror, nodes)
				}
				for _, node := range nodes {
					if end, ok := mirror[node.ID]; !ok || !end.Equal(node.End) {
						t.Fatalf("hook holds %v for %d, scheduler has end %v", end, node.ID, node.End)
					}
				}
				if r.want == absent {
					if len(nodes) != 0 {
						t.Fatalf("nodes after the tick = %+v, want none", nodes)
					}
					return
				}
				due := base.Add(r.want)
				if len(nodes) != 1 || nodes[0].ID != target || !nodes[0].End.Equal(due) {
					t.Fatalf("nodes after the tick = %+v, want only %d due at +%v", nodes, target, r.want)
				}
				before := fires[target]
				s.Tick(due.Add(-time.Nanosecond))
				if fires[target] != before {
					t.Fatal("target fired before its deadline")
				}
				s.Tick(due)
				if fires[target] != before+1 || len(s.Nodes()) != 0 || len(mirror) != 0 {
					t.Fatalf("at its deadline target fired %d more times, nodes=%+v hook=%v; want once and nothing left", fires[target]-before, s.Nodes(), mirror)
				}
				checkSettled(t, s)
			})
		}
	}
}

// 钩子可见状态 × 操作：持久化钩子里可以调用调度器（包括 Tick），钩子能看到回调里看不到的节点状态。
// TestOperationsOnEveryNodeStateDuringTick 只覆盖回调里（直接 / 重入 Tick）的调用位置；这里覆盖钩子：
//   - popping：到期出堆、回调执行之前（在出堆的删除通知里）；
//   - pending_fresh / pending_moved / pending_fired：最外层收尾已把节点入堆、还没轮到它的 upsert（先落定再回调），
//     分别来自 Tick 内新建、Tick 内对堆中节点改期、触发中节点显式改期自己；由排在它前面的节点 K 的收尾 upsert 触发；
//   - popping_fresh：pending_fresh 被 K 的钩子里重入的 Tick 触发，在补发的首次 upsert 里操作。
//
// 操作：改期 (+1m) / 取消 / Tick 到 +10m / 不操作。断言与回调位置的矩阵相同：本次 Tick 后目标的期限（或不存在）
// 与触发次数、操作返回值、钩子视图与调度器一致且不收到不持有的删除、每个分配过的 ID 都经过 upsert、收尾不变量，
// 再在期望期限前后各 Tick 一次。行为表见 docs/maintenance/REFACTOR-2026-10-17-timer-scheduler.md §2.2、§9。
func TestHookOperationsOnEveryNodeStateItCanSee(t *testing.T) {
	const absent = time.Duration(-1)
	base := time.Unix(100, 0)
	type row struct {
		state string
		op    string        // change / remove / tick / none
		ret   time.Duration // 外层回调 F 第一次触发的返回值（pending_fired 用，显式改期优先）
		want  time.Duration // 操作所在的 Tick 结束后目标期限相对 base 的偏移；absent 表示不存在
		fired int           // 到那时目标触发的次数（pending_fired 的目标是 F 自己，含第一次）
	}
	// 每行依次是改期 / 取消 / Tick / 不操作的期望：{操作所在 Tick 结束后的期限, 目标触发次数}。
	type outcome struct {
		want  time.Duration
		fired int
	}
	var rows []row
	add := func(state string, ret time.Duration, outcomes [4]outcome) {
		for i, op := range []string{"change", "remove", "tick", "none"} {
			rows = append(rows, row{state, op, ret, outcomes[i].want, outcomes[i].fired})
		}
	}
	m := time.Minute
	add("popping", 0, [4]outcome{{m, 0}, {absent, 0}, {absent, 1}, {absent, 1}})
	add("pending_fresh", 0, [4]outcome{{m, 0}, {absent, 0}, {absent, 1}, {10 * m, 0}})
	add("pending_moved", 0, [4]outcome{{m, 0}, {absent, 0}, {absent, 1}, {10 * m, 0}})
	for _, ret := range []time.Duration{0, -time.Second, time.Second} {
		add("pending_fired", ret, [4]outcome{{m, 1}, {absent, 1}, {absent, 2}, {10 * m, 1}})
	}
	add("popping_fresh", 0, [4]outcome{{m, 0}, {absent, 0}, {absent, 1}, {absent, 1}})

	for _, r := range rows {
		t.Run(fmt.Sprintf("%s/%s/return_%v", r.state, r.op, r.ret), func(t *testing.T) {
			var s *Scheduler
			var outer, target, k int64
			mirror := make(map[int64]time.Time)
			upserted := make(map[int64]bool)
			fires := make(map[int64]int)
			opOK, opDone, kSeen := true, false, false
			operate := func() {
				opDone = true
				switch r.op {
				case "change":
					opOK = s.ChangeTimer(target, time.Time{}, time.Minute)
				case "remove":
					opOK = s.RemoveTimer(target)
				case "tick":
					s.Tick(base.Add(10 * time.Minute))
				}
			}
			s = NewScheduler(1, 0, nil, func(change ChangeType, node Node) {
				if change == ChangeDelete {
					if _, ok := mirror[node.ID]; !ok {
						t.Errorf("hook got a delete for %d it does not hold", node.ID)
					}
					delete(mirror, node.ID)
				} else {
					mirror[node.ID] = node.End
					upserted[node.ID] = true
				}
				switch {
				case change == ChangeUpsert && node.ID == k && !kSeen:
					kSeen = true
					if r.state == "popping_fresh" {
						s.Tick(base.Add(10 * time.Minute)) // 触发还没轮到 upsert 的目标；操作在它的首次 upsert 里
					} else if r.state != "popping" {
						operate()
					}
					s.RemoveTimer(k) // K 只用来触发，不留在结果里
				case opDone:
				case r.state == "popping" && change == ChangeDelete && node.ID == target,
					r.state == "popping_fresh" && change == ChangeUpsert && node.ID == target:
					operate()
				}
			})
			s.SetClock(func() time.Time { return base })
			s.RegisterHandler(1, func(ctx Context) time.Duration { // F
				fires[ctx.Node.ID]++
				if fires[ctx.Node.ID] > 1 {
					return 0
				}
				k = s.NewTimer(time.Hour, 5, 0, 0, nil) // 先进收尾队列，它的 upsert 排在目标前面
				switch r.state {
				case "pending_fresh", "popping_fresh":
					target = s.NewTimer(10*time.Minute, 3, 0, 0, nil)
				case "pending_moved":
					s.ChangeTimer(target, time.Time{}, 10*time.Minute)
				case "pending_fired":
					s.ChangeTimer(outer, time.Time{}, 10*time.Minute)
				}
				return r.ret
			})
			s.RegisterHandler(3, func(ctx Context) time.Duration { fires[ctx.Node.ID]++; return 0 })
			s.RegisterHandler(5, func(Context) time.Duration { t.Error("trigger K fired"); return 0 })
			switch r.state {
			case "popping":
				target = s.NewTimer(2*time.Second, 3, 0, 0, nil)
			case "pending_moved":
				target = s.NewTimer(time.Hour, 3, 0, 0, nil)
			}
			outer = s.NewTimer(time.Second, 1, 0, 0, nil)
			if r.state == "pending_fired" {
				target = outer
			}

			s.Tick(base.Add(3 * time.Second))
			checkSettled(t, s)
			checkEveryIDAnnounced(t, s, upserted)
			if !opDone && r.op != "none" {
				t.Fatalf("the hook never saw the target in state %s", r.state)
			}
			if !opOK {
				t.Fatalf("%s from the hook returned false, want true", r.op)
			}
			if got := fires[target]; got != r.fired {
				t.Fatalf("target fired %d times, want %d", got, r.fired)
			}
			nodes := s.Nodes()
			if len(nodes) != len(mirror) {
				t.Fatalf("hook holds %v, scheduler holds %+v", mirror, nodes)
			}
			for _, node := range nodes {
				if end, ok := mirror[node.ID]; !ok || !end.Equal(node.End) {
					t.Fatalf("hook holds %v for %d, scheduler has end %v", end, node.ID, node.End)
				}
			}
			if r.want == absent {
				if len(nodes) != 0 {
					t.Fatalf("nodes after the tick = %+v, want none", nodes)
				}
				return
			}
			due := base.Add(r.want)
			if len(nodes) != 1 || nodes[0].ID != target || !nodes[0].End.Equal(due) {
				t.Fatalf("nodes after the tick = %+v, want only %d due at +%v", nodes, target, r.want)
			}
			before := fires[target]
			s.Tick(due.Add(-time.Nanosecond))
			if fires[target] != before {
				t.Fatal("target fired before its deadline")
			}
			s.Tick(due)
			if fires[target] != before+1 || len(s.Nodes()) != 0 || len(mirror) != 0 {
				t.Fatalf("at its deadline target fired %d more times, nodes=%+v hook=%v; want once and nothing left", fires[target]-before, s.Nodes(), mirror)
			}
			checkSettled(t, s)
		})
	}
}
