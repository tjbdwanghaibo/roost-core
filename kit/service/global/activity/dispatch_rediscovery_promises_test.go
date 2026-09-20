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
// 每轮 sweep 都能重新找到它;它不占 MaxPendingActivities 的开活动名额。
//
// **2026-09-19 行为变化(RR-20260919-10,U-0257)**:sweep 不再调 AttemptDispatch。
// 那一步会消耗一次交付预算并把 payload 丢掉——这一侧根本没有传输,于是一个只是暂时
// 掉线的游戏服会发现自己的结果被"重试"到 exhausted,一次都没收到。尝试次数改成在
// **游戏真的取走 payload 时**消耗(游戏侧的 AttemptDispatch,由 OwedDispatches 指路)。
// 所以这里的断言从"attempts 递增"改成"每一轮之后仍然找得到、且仍然可取"——
// 这才是 U-0192 当初要保的东西:循环找不到它们。

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

			// Still rediscoverable after a round it was not created in: the
			// Delivering index, not this round's completions, is what the
			// sweep works from.
			keys, err := s.DeliveringActivities(ctx, "group-a", 10)
			if err != nil {
				t.Fatalf("delivering: %v", err)
			}
			if len(keys) != 1 || keys[0] != k {
				t.Fatalf("the completed activity is not in the delivering index after two sweeps: %v", keys)
			}
			due, err := s.DueDispatches(ctx, k, 10)
			if err != nil {
				t.Fatalf("due: %v", err)
			}
			if len(due) == 0 {
				t.Fatal("the activity is indexed but its dispatch is not due to anybody")
			}
			// And the budget is intact: the sweeps delivered nothing, so they
			// spent nothing.
			d, found, err := s.LookupDispatch(ctx, k, 1)
			if err != nil || !found {
				t.Fatalf("lookup %v %v", found, err)
			}
			if d.Attempts != 0 {
				t.Fatalf("the sweep spent %d attempts without delivering anything", d.Attempts)
			}
			// The game takes it, and that is what costs an attempt.
			taken, err := s.AttemptDispatch(ctx, k, 1)
			if err != nil {
				t.Fatalf("the game could not take its result: %v", err)
			}
			if taken.Attempts != 1 {
				t.Fatalf("the game's take is attempt %d, want 1", taken.Attempts)
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

	// exhausted — now driven by the taker, because that is who spends the
	// budget. A game that keeps taking the payload and never acks is the case
	// exhaustion is for; a game that is merely absent is not.
	k2 := activityKey("exhaust")
	openActivity(t, s, k2, 1)
	notify(t, s, k2, 1)
	for i := 0; i < s.cfg.DispatchMaxAttempts+1; i++ {
		_, _ = s.AttemptDispatch(ctx, k2, 1)
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
