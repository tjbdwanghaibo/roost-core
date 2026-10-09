package policy

import (
	"errors"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// OPEN-ITEMS B22：RR-20260926-59 修复记录的边界“退回 remove 之后 Interest / AOI 是否重新订阅没验证”。RR-59 的卸载后重载
// 重试用尽（或队列满）时 RetractUnloadedSubject 退回 remove；实体随后被加载出来（kit 的 OnEntityLoaded、重载 worker）走的是
// Rebind，而不是业务的 Register。验证：remove 发完之前到达的 Rebind 按 RR-55 排在退役之后登记（返回 nil），这次登记把
// RR-70 的撤销记录交还 Interest；距离（AOI）、关系（team）与 self 三个来源的观察者都先收到 remove、再收到一次 create，
// 之后新状态上的变化照常送达，恢复的订阅照旧按政策规则释放。remove 发完之后 subject 已被忘掉，Rebind 按契约报告
// ErrSubjectNotRegistered，由登记该实体的业务 Register（RR-70 回归已覆盖其后的恢复），这里只固定这条边界。

func TestARetractedSubjectRebindBeforeTheRemovesComesBackToInterestObservers(t *testing.T) {
	manager, log := newLoggedManager(t)
	in := newInterest(t, manager, "team")
	state2 := sideBySide(t, manager, in) // 观察者 1 经距离看到 2；2 经 self 看到自己
	const farID = int64(3)
	player(t, manager, farID)
	if err := in.Enter(farID, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	in.Relation("team").Set(farID, []int64{2})
	mustApply(t, in)
	if !subscribed(manager, farID, 2) {
		t.Fatalf("setup: team observer 3 not subscribed to 2: %v", manager.Subscribers(2))
	}
	flushTimes(manager, 1)
	for _, session := range []entitysync.SessionID{1, 2, entitysync.SessionID(farID)} {
		log.take(session)
	}

	// RR-59 的退回 remove：卸载关闭了状态，重载不了，框架撤销；remove 还没发出去。
	state2.Close()
	if !manager.RetractUnloadedSubject(2) {
		t.Fatal("setup: retract refused")
	}
	// 实体随后被加载出来（OnEntityLoaded → Rebind），早于 remove 交付：排在退役之后登记。
	reloaded := subjectState(t, 2)
	if err := manager.Rebind(reloaded); err != nil {
		t.Fatalf("Rebind of the reloaded state during the retraction=%v, want it queued behind the removes", err)
	}
	flushTimes(manager, 3)

	for _, observer := range []int64{1, 2, farID} {
		if !holds(manager, observer, 2) {
			t.Fatalf("observer %d does not hold the reloaded subject 2 after the Rebind (subscribers=%v, Visible(1)=%v)", observer, manager.Subscribers(2), in.Visible(1))
		}
	}
	for _, observer := range []int64{1, farID} {
		if got := log.take(entitysync.SessionID(observer)); !slices.Equal(got, []string{"remove:-1", "create:2"}) {
			t.Fatalf("observer %d received %v for the retraction and the reload, want one remove then one create", observer, got)
		}
	}
	if got := log.take(2); !slices.Equal(got, []string{"remove:-1", "create:2"}) {
		t.Fatalf("subject 2's own session received %v, want one remove then one create", got)
	}
	if refusals := in.Apply(); len(refusals) != 0 {
		t.Fatalf("an idle apply after the resubmit refused: %+v", refusals)
	}

	// 登记的是重载出来的状态：它的变化照常送达观察者。
	reloaded.MarkDirty(1)
	flushTimes(manager, 1)
	if got := log.take(1); !slices.Equal(got, []string{"update:2"}) {
		t.Fatalf("observer 1 received %v for a change on the reloaded state, want one update", got)
	}

	// 恢复的订阅仍按政策自己的规则释放：走远只释放距离来源，team 仍持有。
	if err := in.Move(1, spatial.Point{X: 20, Y: 900}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	if subscribed(manager, 1, 2) {
		t.Fatal("the resubmitted distance pair did not release when the observer moved away")
	}
	if !subscribed(manager, farID, 2) {
		t.Fatal("releasing the distance pair took the team subscription with it")
	}
}

func TestARetractedSubjectRebindAfterTheRemovesIsNotRegistered(t *testing.T) {
	manager, log := newLoggedManager(t)
	in := newInterest(t, manager)
	state2 := sideBySide(t, manager, in)
	flushTimes(manager, 1)
	log.take(1)
	retractUnloaded(t, manager, state2) // remove 已发出、subject 已被忘掉
	if err := manager.Rebind(subjectState(t, 2)); !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
		t.Fatalf("Rebind after the retirement completed=%v, want ErrSubjectNotRegistered (the owner registers a new subject)", err)
	}
	flushTimes(manager, 2)
	if holds(manager, 1, 2) {
		t.Fatalf("a refused Rebind subscribed observer 1: %v", manager.Subscribers(2))
	}
	if got := log.take(1); !slices.Equal(got, []string{"remove:-1"}) {
		t.Fatalf("observer 1 received %v, want only the remove", got)
	}
}
