package policy

import (
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"testing"
)

func TestGroupRejoinRestoresReopenedSession(t *testing.T) {
	m := newManager(t)
	player(t, m, 1)
	player(t, m, 2)
	g, err := NewGroup(GroupConfig{Manager: m, MaxMembers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.AddSubject(subjectState(t, 2)); err != nil {
		t.Fatal(err)
	}
	if err = g.Join(1); err != nil {
		t.Fatal(err)
	}
	m.CloseSession(entitysync.SessionID(1))
	if err = m.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	_ = g.Join(1)
	if !subscribed(m, 1, 2) {
		t.Fatal("rejoining a reopened member did not restore its view")
	}
	if g.Members() != 1 {
		t.Fatal("rejoin changed membership count")
	}
	if err = g.Leave(1); err != nil {
		t.Fatal(err)
	}
	if subscribed(m, 1, 2) {
		t.Fatal("leave retained restored view")
	}
}
