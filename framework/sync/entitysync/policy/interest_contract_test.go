package policy

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"testing"
	"time"
)

func TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen(t *testing.T) {
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	in := newInterest(t, m, "team")
	player(t, m, 1)
	player(t, m, 2)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
	mustApply(t, in)
	m.CloseSession(2)

	e := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
	e.SetSyncState(subjectState(t, 2))
	batch := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
	if err := in.QueueRelation(e, "team", []int64{1}); err != nil {
		t.Fatal(err)
	}
	batch.Admit()
	batch.Confirm()
	batch.Release()

	stalledBefore := counterValue("entitysync_interest_retry_stalled_total")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Drain(ctx); err != nil {
		t.Fatalf("Drain with a pair whose session is closed for good: %v (retry=%d)", err, retryLen(in))
	}
	if got := counterValue("entitysync_interest_retry_stalled_total") - stalledBefore; got != 1 {
		t.Fatalf("stalled retry counted %d times, want 1", got)
	}
	if retryLen(in) != 1 {
		t.Fatalf("stalled pair left the retry list: retry=%d", retryLen(in))
	}
	// 再排一轮 Drain 也不再计数（每个 pair 只告警一次）。
	if err := m.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := counterValue("entitysync_interest_retry_stalled_total") - stalledBefore; got != 1 {
		t.Fatalf("stalled retry re-counted: %d", got)
	}
	// 会话重开：停滞的 pair 仍被重说，订阅成功。
	if err := m.OpenSession(2); err != nil {
		t.Fatal(err)
	}
	if !subscribed(m, 2, 1) {
		t.Fatal("stalled pair was dropped: reopened session never subscribed")
	}
	if retryLen(in) != 0 {
		t.Fatalf("accepted pair still in retry: %d", retryLen(in))
	}
}

func retryLen(in *Interest) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.retry)
}

func counterValue(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

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
