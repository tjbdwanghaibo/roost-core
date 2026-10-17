package timer

import (
	"testing"
	"time"
)

// Tick 只推迟新节点入堆，不能把登记时刻也推迟到其他到期回调执行完之后。
func TestTimerCreatedDuringTickKeepsItsRegistrationDeadline(t *testing.T) {
	for _, closure := range []bool{false, true} {
		name := "typed"
		if closure {
			name = "closure"
		}
		t.Run(name, func(t *testing.T) {
			base := time.Unix(100, 0)
			now := base
			s := NewScheduler(1, 0, nil, nil)
			s.SetClock(func() time.Time { return now })
			var child int64
			var fired int
			handler := func(Context) time.Duration { fired++; return 0 }
			s.RegisterHandler(3, handler)
			s.RegisterHandler(1, func(Context) time.Duration {
				if closure {
					child = s.NewClosureTimer(time.Minute, handler)
				} else {
					child = s.NewTimer(time.Minute, 3, 0, 0, nil)
				}
				return 0
			})
			s.RegisterHandler(2, func(Context) time.Duration {
				// 模拟后续回调的耗时，既不 sleep 也不依赖宿主调度。
				now = now.Add(30 * time.Second)
				return 0
			})
			s.NewTimer(time.Second, 1, 0, 0, nil)
			s.NewTimer(2*time.Second, 2, 0, 0, nil)
			s.Tick(base.Add(3 * time.Second))
			node := s.byID[child]
			if node == nil || !node.End.Equal(base.Add(time.Minute)) {
				t.Fatalf("registration deadline moved after another callback: node=%+v", node)
			}
			s.Tick(base.Add(time.Minute))
			if fired != 1 {
				t.Fatalf("child fired %d times at its registration deadline, want 1", fired)
			}
		})
	}
}

// 与 Tick 外创建保持一致：调用返回后调用方可以复用输入缓冲，持久化钩子必须看到登记时内容。
func TestTimerCreatedDuringTickOwnsItsPayloadImmediately(t *testing.T) {
	base := time.Unix(100, 0)
	var captured []byte
	s := NewScheduler(1, 0, nil, func(change ChangeType, node Node) {
		if change == ChangeUpsert && node.Type == 2 {
			captured = append([]byte(nil), node.Payload...)
		}
	})
	s.SetClock(func() time.Time { return base })
	s.RegisterHandler(1, func(Context) time.Duration {
		payload := []byte("original")
		s.NewTimer(time.Minute, 2, 0, 0, payload)
		copy(payload, "modified")
		return 0
	})
	s.NewTimer(time.Second, 1, 0, 0, nil)
	s.Tick(base.Add(time.Second))
	if string(captured) != "original" {
		t.Fatalf("persistence saw caller's later payload: %q", captured)
	}
	if nodes := s.Nodes(); len(nodes) != 1 || string(nodes[0].Payload) != "original" {
		t.Fatalf("scheduled payload changed after registration: %+v", nodes)
	}
	// 模拟宿主只保留节点后重建调度器，恢复出的任务仍须携带原始参数。
	restored := NewScheduler(1, s.Seed(), s.Nodes(), nil)
	fired := 0
	restored.RegisterHandler(2, func(ctx Context) time.Duration {
		fired++
		if string(ctx.Node.Payload) != "original" {
			t.Fatalf("restored task carries a different payload: %q", ctx.Node.Payload)
		}
		return 0
	})
	restored.Tick(base.Add(time.Minute))
	if fired != 1 {
		t.Fatalf("restored task fired %d times, want 1", fired)
	}
}

// 两条规则叠加：同一个回调里新建子定时器并显式改期自己，二者都按调用那一刻的注入时钟定期限，
// 后续回调推进时钟不改变它们；持久化钩子最后看到的也是这两个期限。
func TestHandlerThatArmsAChildAndReschedulesItselfUsesCallTime(t *testing.T) {
	base := time.Unix(100, 0)
	now := base
	stored := make(map[int64]Node)
	s := NewScheduler(1, 0, nil, func(change ChangeType, node Node) {
		if change == ChangeDelete {
			delete(stored, node.ID)
		} else {
			stored[node.ID] = node
		}
	})
	s.SetClock(func() time.Time { return now })
	var self, child int64
	s.RegisterHandler(3, func(Context) time.Duration { return 0 })
	s.RegisterHandler(1, func(ctx Context) time.Duration {
		if ctx.Node.ID != self {
			return 0
		}
		child = s.NewTimer(time.Minute, 3, 0, 0, []byte("child"))
		if !s.ChangeTimer(self, time.Time{}, 2*time.Minute) {
			t.Fatal("explicit reschedule refused")
		}
		return 0
	})
	s.RegisterHandler(2, func(Context) time.Duration {
		now = now.Add(30 * time.Second) // 后续回调耗时
		return 0
	})
	self = s.NewTimer(time.Second, 1, 0, 0, nil)
	s.NewTimer(2*time.Second, 2, 0, 0, nil)
	s.Tick(base.Add(3 * time.Second))
	want := map[int64]time.Time{child: base.Add(time.Minute), self: base.Add(2 * time.Minute)}
	if len(stored) != len(want) {
		t.Fatalf("stored = %+v, want child and rescheduled self", stored)
	}
	for id, end := range want {
		node, ok := stored[id]
		if !ok || !node.End.Equal(end) {
			t.Fatalf("timer %d stored %+v (present=%v), want end %v", id, node, ok, end)
		}
		if live := s.byID[id]; live == nil || !live.End.Equal(end) {
			t.Fatalf("timer %d scheduled %+v, want end %v", id, live, end)
		}
	}
}
