package policy

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/spatial"
)

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
