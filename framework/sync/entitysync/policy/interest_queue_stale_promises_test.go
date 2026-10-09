package policy

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// RR-20261006-48（F04-1）：排队模式下，某 id 的位置事实已就绪、尚未在政策阶段应用时，业务对同一 id 调
// Leave / Hide。旧实现 Leave / Hide 不碰 in.facts，applyQueued 里 MoveSubject 返回 ErrInterestUnknown，
// 事实被留下并继续阻塞该 id，错误一路返回到 Flush——Flush 在取 pending 之前失败，此后每一次都一样：
// Manager 上所有 subject 停发，Stop / Drain 一直报错。承诺：Leave / Hide 是比排队事实更晚的业务意图，
// 被它们作废的事实随之丢弃，Flush 不因此失败，其他 subject 照常出帧；Leave / Hide 没有作废的事实
// （Hide 不改该 id 作为观察者的关系）照常应用，不丢合法的兴趣变更；作废的位置事实也不会套到之后
// 重新 Enter 的同一 id 上。
func TestQueuedFactsInvalidatedByLeaveOrHideDoNotWedgeFlush(t *testing.T) {
	ctx := context.Background()
	t.Run("leave", func(t *testing.T) {
		frames := &sink{}
		m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: frames})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		in := newInterest(t, m, "team")
		player(t, m, 1)
		player(t, m, 2)
		player(t, m, 4)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
		mustApply(t, in)
		e := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
		e.SetSyncState(subjectState(t, 2))
		batch := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
		if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}, true); err != nil {
			t.Fatal(err)
		}
		if err := in.QueueRelation(e, "team", []int64{1}); err != nil {
			t.Fatal(err)
		}
		batch.Admit()
		batch.Confirm()
		batch.Release()
		// 事实已就绪、尚未应用：此刻业务让 2 离开。
		if err := in.Leave(2); err != nil {
			t.Fatal(err)
		}
		for round := 0; round < 3; round++ {
			if err := m.Flush(ctx); err != nil {
				t.Fatalf("flush %d after Leave with a ready queued fact: %v", round, err)
			}
		}
		if subscribed(m, 2, 1) {
			t.Fatal("relation fact queued before Leave resurrected 2's interest in 1")
		}
		// 其他 subject 照常出帧：4 走进 1 的视野，会话 1 要收到它的快照。
		before := frames.count(1)
		mustEnter(t, in, 4, spatial.Point{X: 15, Y: 10})
		if err := m.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if frames.count(1) == before {
			t.Fatal("session 1 received nothing after the invalidated fact: the manager stopped emitting")
		}
		// 同一 id 重新进入：被 Leave 作废的旧位置不能套到它身上。
		mustEnter(t, in, 2, spatial.Point{X: 30, Y: 10})
		if err := m.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if !subscribed(m, 1, 2) {
			t.Fatal("stale queued move applied to the re-entered id")
		}
	})

	t.Run("hide", func(t *testing.T) {
		m := newManager(t)
		in := newInterest(t, m, "team")
		player(t, m, 1)
		player(t, m, 3)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		if err := in.Show(3, spatial.Point{X: 20, Y: 10}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, in)
		e := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
		e.SetSyncState(subjectState(t, 3))
		batch := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
		if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}, false); err != nil {
			t.Fatal(err)
		}
		// 3 作为观察者的关系事实：Hide 只把 3 从被观察方移走，这条不因 Hide 失效。
		if err := in.QueueRelation(e, "team", []int64{1}); err != nil {
			t.Fatal(err)
		}
		batch.Admit()
		batch.Confirm()
		batch.Release()
		if err := in.Hide(3); err != nil {
			t.Fatal(err)
		}
		for round := 0; round < 3; round++ {
			if err := m.Flush(ctx); err != nil {
				t.Fatalf("flush %d after Hide with a ready queued fact: %v", round, err)
			}
		}
		if !subscribed(m, 3, 1) {
			t.Fatal("relation fact Hide did not invalidate was lost")
		}
		if err := m.Stop(ctx); err != nil {
			t.Fatalf("Stop after Hide with a ready queued fact: %v", err)
		}
	})
}

// 结构性守卫（RR-20261006-48）：将来若又出现绕过 Leave / Hide 改写的移除路径，应用失败的事实报告一次后丢弃，
// 不再让之后每一次 Flush 都失败。
func TestQueuedFactThatCannotApplyFailsOneFlushOnly(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m)
	player(t, m, 1)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustApply(t, in)
	in.mu.Lock()
	in.queueActive = true
	in.facts = append(in.facts, queuedInterestFact{id: 99, at: spatial.Point{X: 5, Y: 5}, condition: func() (bool, bool) { return true, false }})
	in.mu.Unlock()
	if err := m.Flush(context.Background()); err == nil {
		t.Fatal("unappliable fact not reported")
	}
	for round := 0; round < 2; round++ {
		if err := m.Flush(context.Background()); err != nil {
			t.Fatalf("flush %d still failing on a dropped fact: %v", round, err)
		}
	}
}

func mustEnter(t *testing.T, in *Interest, id int64, at spatial.Point) {
	t.Helper()
	if err := in.Enter(id, at); err != nil {
		t.Fatal(err)
	}
}

func (s *sink) count(session entitysync.SessionID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames[session]
}
