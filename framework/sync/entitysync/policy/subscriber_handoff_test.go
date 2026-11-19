package policy

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// Another source claims a newly freed slot before the waiting policy's next phase.
// The waiting member must add its source even though the subject is full again.
func TestGroupViewSharesSlotTakenBetweenPhases(t *testing.T) {
	self := newSelfFixtureWith(t, &selfLane{}, entitysync.ManagerConfig{MaxSubscribersPerSubject: 1})
	groups, err := NewGroupView(GroupViewConfig{Manager: self.manager, Profile: roomProfile, Lookup: self.memory.lookup})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(groups.Close)
	f := &groupFixture{selfFixture: self, groups: groups}
	for _, id := range []int64{1, 2} {
		f.loaded(t, id)
		f.view.Activate(id, "c1")
	}
	f.memory.put(subjectState(t, groupRoom))
	joins(t, groups, 1, 2)
	f.flush(t)
	groups.Leave(groupRoom, 1)
	f.flush(t)
	other := f.manager.NewSubscriptionSource()
	if err := other.Subscribe(2, groupRoom, roomProfile); err != nil {
		t.Fatal(err)
	}
	f.flush(t)
	if !groups.source.Holds(2, groupRoom) {
		t.Fatal("group did not share member 2's occupied slot")
	}
	if err := other.Unsubscribe(2, groupRoom); err != nil {
		t.Fatal(err)
	}
	if !groups.source.Holds(2, groupRoom) {
		t.Fatal("removing the other source lost the group subscription")
	}
}

func TestInterestSharesOccupiedSlotWithoutSkippingFIFO(t *testing.T) {
	m, in := newCrowdInterest(t, entitysync.ManagerConfig{MaxSubscribersPerSubject: 1}, nil)
	holdCrowd(t, m, in, 1, 2, 3)
	mustFlush(t, m)
	setRelation(t, in, 1)
	mustFlush(t, m)
	other := m.NewSubscriptionSource()
	if err := other.Subscribe(3, crowdSubject, roomProfile); err != nil {
		t.Fatal(err)
	}
	mustFlush(t, m)
	if !in.subscriptions.Holds(3, crowdSubject) || in.subscriptions.Holds(2, crowdSubject) {
		t.Fatal("expected 3 to share its existing slot, 2 to keep waiting for a new one")
	}
	if err := other.Unsubscribe(3, crowdSubject); err != nil {
		t.Fatal(err)
	}
	if !in.subscriptions.Holds(3, crowdSubject) {
		t.Fatal("interest lost its subscription during handoff")
	}
	setRelation(t, in, 3)
	mustFlush(t, m)
	mustFlush(t, m)
	if !in.subscriptions.Holds(2, crowdSubject) {
		t.Fatal("first waiter did not receive the next free slot")
	}
}
