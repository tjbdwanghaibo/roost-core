package activity

import (
	"context"
	"testing"
	"time"
)

// U-0192 · C8 · RR-20260914-03:后台 sweep 必须能跨轮重新发现待派发的活动。旧实现 sweepGroup 只对
// AdvanceExpired **本轮**返回的 advanced 调 DueDispatches/AttemptDispatch;活动完成即从聚合窗口删除,
// 下一轮不再出现——退避到期的重试、由最后一个 notify 直接完成的活动、上轮 Create 失败本轮 heal 的活动,
// 都没有入口。派发状态机原语(显式 DueDispatches→AttemptDispatch 两次)是好的,坏的是循环找不到它们。
// 承诺:完成的活动进入窗口里独立于聚合的 Delivering 索引,保留到全部 dispatch 终态(acked / exhausted),
// 每轮 sweep 都从它出发重试;它不占 MaxPendingActivities 的开活动名额。

func TestSweepRetriesDispatchesAcrossRounds(t *testing.T) {
	for _, mode := range []string{"grace", "notify", "heal"} {
		t.Run(mode, func(t *testing.T) {
			s, c := newActivityService(t)
			ctx := context.Background()
			k := activityKey(mode)
			if mode == "heal" {
				s.cfg.Dispatches = &dispatchesFailingOnce{Store: s.cfg.Dispatches}
			}
			openActivity(t, s, k, 1, 2)
			notify(t, s, k, 1)
			switch mode {
			case "grace":
				c.advance(time.Minute)
			case "notify":
				notify(t, s, k, 2)
			case "heal":
				if _, err := s.NotifyPhase(ctx, k, 2); err == nil {
					t.Fatal("expected the injected dispatch create failure")
				}
			}
			server := &Server{service: s}
			server.sweepGroup(ctx, s, "group-a")
			c.advance(time.Minute)
			server.sweepGroup(ctx, s, "group-a")
			d, found, err := s.LookupDispatch(ctx, k, 1)
			if err != nil || !found {
				t.Fatalf("lookup %v %v", found, err)
			}
			if d.Attempts != 2 {
				t.Fatalf("two due sweeps: attempts=%d want=2 state=%s due=%v", d.Attempts, d.State, d.Due(c.Now().Unix()))
			}
		})
	}
}

// 派发索引到终态才退出:全部 ACK 后不再被扫描;打满尝试预算(exhausted)同样退出,不会永远占位。
func TestDeliveringIndexRetiresAtTerminalStates(t *testing.T) {
	s, c := newActivityService(t)
	ctx := context.Background()
	k := activityKey("ack")
	openActivity(t, s, k, 1, 2)
	notify(t, s, k, 1)
	notify(t, s, k, 2)
	server := &Server{service: s}
	server.sweepGroup(ctx, s, "group-a")
	if keys, _ := s.DeliveringActivities(ctx, "group-a", 10); len(keys) != 1 || keys[0] != k {
		t.Fatalf("completed activity not indexed for delivery: %v", keys)
	}
	for _, sid := range []int32{1, 2} {
		d, _, err := s.LookupDispatch(ctx, k, sid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AckDispatch(ctx, k, sid, d.Token); err != nil {
			t.Fatal(err)
		}
	}
	server.sweepGroup(ctx, s, "group-a")
	if keys, _ := s.DeliveringActivities(ctx, "group-a", 10); len(keys) != 0 {
		t.Fatalf("fully acked activity still indexed: %v", keys)
	}

	// exhausted
	k2 := activityKey("exhaust")
	openActivity(t, s, k2, 1)
	notify(t, s, k2, 1)
	for i := 0; i < s.cfg.DispatchMaxAttempts+1; i++ {
		server.sweepGroup(ctx, s, "group-a")
		c.advance(time.Hour)
	}
	d, _, err := s.LookupDispatch(ctx, k2, 1)
	if err != nil || d.State != DispatchExhausted {
		t.Fatalf("dispatch not exhausted: state=%s attempts=%d err=%v", d.State, d.Attempts, err)
	}
	server.sweepGroup(ctx, s, "group-a")
	if keys, _ := s.DeliveringActivities(ctx, "group-a", 10); len(keys) != 0 {
		t.Fatalf("exhausted activity still indexed: %v", keys)
	}
}
