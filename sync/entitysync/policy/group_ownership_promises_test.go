package policy

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func TestGroupAddFailureCanBeRetriedWithoutPartialMembership(t *testing.T) {
	manager := newManager(t)
	group, err := NewGroup(GroupConfig{Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := group.Join(id); err != nil {
			t.Fatal(err)
		}
	}
	state := subjectState(t, 5101)
	if err := group.AddSubject(state); err == nil {
		t.Fatal("missing session should refuse AddSubject")
	}
	if group.Subjects() != 0 || len(manager.Subscribers(5101)) != 0 {
		t.Fatal("failed addition left partial group membership")
	}
	if err := manager.OpenSession(2); err != nil {
		t.Fatal(err)
	}
	if err := group.AddSubject(state); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(manager.Subscribers(5101)) != 2 {
		t.Fatal("retry did not subscribe both members")
	}
}

func TestPoliciesReleaseOnlyTheirOwnSubscriptions(t *testing.T) {
	for _, operation := range []string{"leave", "remove", "close", "direct"} {
		t.Run(operation, func(t *testing.T) {
			manager := newManager(t)
			player(t, manager, 1)
			player(t, manager, 2)
			group, err := NewGroup(GroupConfig{Manager: manager})
			if err != nil {
				t.Fatal(err)
			}
			if err := group.AddSubject(subjectState(t, 2)); err != nil {
				t.Fatal(err)
			}
			if err := group.Join(1); err != nil {
				t.Fatal(err)
			}
			direct, err := NewDirect(manager, nil)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := direct.Bind(1, 2, entity.SyncProfile{}); err != nil {
					t.Fatal(err)
				}
			}
			switch operation {
			case "leave":
				err = group.Leave(1)
			case "remove":
				err = group.RemoveSubject(2)
			case "close":
				err = group.Close()
			case "direct":
				err = direct.Unbind(1, 2)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !subscribed(manager, 1, 2) {
				t.Fatal("one policy revoked another policy's subscription")
			}
			if operation == "direct" {
				err = group.Leave(1)
			} else {
				err = direct.Unbind(1, 2)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if subscribed(manager, 1, 2) {
				t.Fatal("repeated Bind leaked a subscription")
			}
		})
	}
}
