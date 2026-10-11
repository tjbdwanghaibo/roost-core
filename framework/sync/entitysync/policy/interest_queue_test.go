package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func TestQueuedRelationDeltaCommitRollbackAndOrder(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m, "team")
	in.batchSize = 1 // 同次提交的 Set 和 Delta 即使超过批次额度也必须整体应用。
	for _, id := range []int64{1, 2, 3} {
		player(t, m, id)
	}
	e := factEntity(t, 2)
	es := []entity.IThreadSafeEntity{e}
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	first := entity.BeginSyncMutation(es, m)
	check(in.QueueRelation(e, "team", []int64{1}))
	added, removed := []int64{3}, []int64{1}
	check(in.QueueRelationDelta(e, "team", added, removed))
	added[0], removed[0] = 1, 3
	first.Admit()
	first.Release()
	flushInterest(t, m)
	if subscribed(m, 2, 1) || subscribed(m, 2, 3) {
		t.Fatal("unconfirmed delta escaped")
	}
	first.Confirm()
	flushInterest(t, m)
	if subscribed(m, 2, 1) || !subscribed(m, 2, 3) {
		t.Fatal("confirmed delta or input copy failed")
	}
	in.batchSize = 256
	rollback := entity.BeginSyncMutation(es, m)
	check(in.QueueRelationDelta(e, "team", []int64{1}, []int64{3}))
	rollback.Finish(false)
	rollback.Release()
	flushInterest(t, m)
	if subscribed(m, 2, 1) || !subscribed(m, 2, 3) {
		t.Fatal("rollback changed relation")
	}
	head := entity.BeginSyncMutation(es, m)
	check(in.QueueRelationDelta(e, "team", []int64{1}, nil))
	head.Admit()
	head.Release()
	tail := entity.BeginSyncMutation(es, m)
	check(in.QueueRelationDelta(e, "team", nil, []int64{1}))
	tail.Admit()
	tail.Confirm()
	tail.Release()
	flushInterest(t, m)
	if len(in.facts) != 2 {
		t.Fatal("later delta overtook unconfirmed head")
	}
	head.Confirm()
	flushInterest(t, m)
	if len(in.facts) != 0 || subscribed(m, 2, 1) || !subscribed(m, 2, 3) {
		t.Fatal("ordered deltas failed")
	}
}

func TestQueuedRelationDeltaPreservesOtherSources(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m, "team", "friends")
	for _, id := range []int64{1, 2} {
		player(t, m, id)
	}
	in.Relation("team").Set(2, []int64{1})
	in.Relation("friends").Set(2, []int64{1})
	mustApply(t, in)
	e := factEntity(t, 2)
	if err := in.QueueRelationDelta(e, "team", nil, []int64{1}); err != nil {
		t.Fatal(err)
	}
	flushInterest(t, m)
	if !subscribed(m, 2, 1) {
		t.Fatal("removing team lost friends subscription")
	}
	if err := in.QueueRelationDelta(e, "friends", nil, []int64{1}); err != nil {
		t.Fatal(err)
	}
	flushInterest(t, m)
	if subscribed(m, 2, 1) {
		t.Fatal("last source removal did not unsubscribe")
	}
}

func TestQueuedRelationDeltaAdmissionAndClose(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m, "team")
	e := factEntity(t, 2)
	if err := in.QueueRelationDelta(nil, "team", nil, nil); err == nil {
		t.Fatal("nil entity accepted")
	}
	for _, source := range []string{"", "unknown"} {
		if err := in.QueueRelationDelta(e, source, nil, nil); err == nil {
			t.Fatal("invalid source accepted")
		}
	}
	in.maxQueuedFacts = 1
	if err := in.QueueRelationDelta(e, "team", []int64{1}, nil); err != nil {
		t.Fatal(err)
	}
	prepared, err := e.Sync().PrepareViews(nil, []entity.SyncProfile{{}})
	if prepared != nil {
		prepared.Abort()
	}
	if !errors.Is(err, entity.ErrSyncCommitPending) {
		t.Fatalf("delta did not hold publication: %v", err)
	}
	if err := in.QueueRelationDelta(e, "team", nil, []int64{1}); !errors.Is(err, ErrInterestQueueFull) {
		t.Fatalf("budget error %v", err)
	}
	in.Close()
	prepared, err = e.Sync().PrepareViews(nil, []entity.SyncProfile{{}})
	if err != nil {
		t.Fatalf("close leaked publication hold: %v", err)
	}
	prepared.Abort()
	if len(in.facts) != 0 {
		t.Fatal("close retained delta")
	}
	if err := in.QueueRelationDelta(e, "team", nil, nil); err == nil {
		t.Fatal("closed interest accepted delta")
	}
}

func TestQueuedRelationDeltaExitAndReentry(t *testing.T) {
	for _, action := range []string{"leave-observer", "hide-observer", "hide-added", "hide-removed"} {
		t.Run(action, func(t *testing.T) {
			m := newManager(t)
			in := newInterest(t, m, "team")
			for _, id := range []int64{1, 2, 3} {
				player(t, m, id)
			}
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
			check(in.Show(1, spatial.Point{X: 10, Y: 10}))
			check(in.Show(3, spatial.Point{X: 400, Y: 400}))
			in.Relation("team").Set(2, []int64{3})
			mustApply(t, in)
			e := factEntity(t, 2)
			mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
			check(in.QueueRelationDelta(e, "team", []int64{1}, []int64{3}))
			mutation.Admit()
			mutation.Confirm()
			mutation.Release()
			switch action {
			case "leave-observer":
				check(in.Leave(2))
				mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
			case "hide-observer":
				check(in.Hide(2))
			case "hide-added":
				check(in.Hide(1))
				check(in.Show(1, spatial.Point{X: 10, Y: 10}))
			case "hide-removed":
				check(in.Hide(3))
				check(in.Show(3, spatial.Point{X: 400, Y: 400}))
				in.Relation("team").Set(2, []int64{3})
			}
			flushInterest(t, m)
			want1 := action == "hide-observer" || action == "hide-removed"
			want3 := action == "hide-removed"
			if subscribed(m, 2, 1) != want1 || subscribed(m, 2, 3) != want3 {
				t.Fatalf("stale delta after %s: one=%v three=%v", action, subscribed(m, 2, 1), subscribed(m, 2, 3))
			}
			prepared, err := e.Sync().PrepareViews(nil, []entity.SyncProfile{{}})
			if err != nil {
				t.Fatalf("exit/application leaked publication: %v", err)
			}
			prepared.Abort()
		})
	}
}

type queuedEntity struct{ *entity.EntityBase }

func (e *queuedEntity) Base() *entity.EntityBase { return e.EntityBase }

func TestQueuedInterestFactsFollowCommitAndRollback(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m, "team")
	player(t, m, 1)
	player(t, m, 2)
	if err := in.Enter(1, spatial.Point{X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	if err := in.Enter(2, spatial.Point{X: 20, Y: 10}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	e := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
	e.SetSyncState(subjectState(t, 2))
	es := []entity.IThreadSafeEntity{e}
	first := entity.BeginSyncMutation(es, m)
	if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}, true); err != nil {
		t.Fatal(err)
	}
	first.Admit()
	first.Release()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(in.Visible(1)) != 1 {
		t.Fatal("unconfirmed movement applied")
	}
	first.Confirm()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(in.Visible(1)) != 0 {
		t.Fatal("confirmed movement not applied")
	}
	second := entity.BeginSyncMutation(es, m)
	if err := in.QueueMove(e, spatial.Point{X: 20, Y: 10}, true); err != nil {
		t.Fatal(err)
	}
	if err := in.QueueRelation(e, "team", []int64{1}); err != nil {
		t.Fatal(err)
	}
	second.Finish(false)
	second.Release()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(in.Visible(1)) != 0 || len(in.facts) != 0 {
		t.Fatal("rolled back facts escaped or leaked")
	}
	if subscribed(m, 2, 1) {
		t.Fatal("rolled back relation escaped")
	}
}
