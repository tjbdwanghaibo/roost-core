package policy

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// RR-20260926-79：Direct 为 RR-70 的重新提交记着调用方的绑定。承诺：会话关闭（CloseSession / 传输失败）或业务 Unregister
// 该实体后，Manager 已丢掉的订阅不再留在绑定表里，表的大小落在活跃绑定数上；通知途中调用方重新 Bind 的保留；RR-59 撤销
// 不删除绑定（RR-70 的恢复语义不变）。旧行为：绑定只在 Unbind 时删除，表随历史绑定数单调增长；会话关闭时实体恰好缺席的
// 绑定在实体重新登记后被重新提交，落到同 ID 重开的新连接上。探针来源：REPRO-2026-09-26-07 §2（审计员 Aud4 探针，此处为正式版）。

func directBindings(d *Direct) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.bound)
}

func TestDirectForgetsBindingsOfClosedSessionsAndUnregisteredSubjects(t *testing.T) {
	manager, _ := newLoggedManager(t)
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	const n = 1000
	for i := int64(0); i < n; i++ {
		observer, subject := 10000+i, 20000+i
		player(t, manager, observer)
		if err := manager.Register(subjectState(t, subject)); err != nil {
			t.Fatal(err)
		}
		if err := direct.Bind(observer, subject, entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	flushTimes(manager, 1)
	for i := int64(0); i < n; i++ {
		observer, subject := 10000+i, 20000+i
		if i%2 == 0 {
			manager.CloseSession(entitysync.SessionID(observer))
		} else if err := manager.Unregister(subject); err != nil {
			t.Fatal(err)
		}
	}
	flushTimes(manager, 3)
	live := 0
	for i := int64(0); i < n; i++ {
		if holds(manager, 10000+i, 20000+i) {
			live++
		}
	}
	if bound := directBindings(direct); bound != live {
		t.Fatalf("Direct keeps %d bindings with no manager subscription behind them (bound=%d live=%d)", bound-live, bound, live)
	}
}

// 通知在下一次政策阶段才到：之间在重开的会话、重新登记的实体上再次 Bind 的绑定是新的，不能被旧通知删掉。
func TestDirectKeepsABindingMadeAgainBeforeTheReleaseArrives(t *testing.T) {
	manager, _ := newLoggedManager(t)
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	player(t, manager, 7)
	if err := manager.Register(subjectState(t, 8)); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 1)
	manager.CloseSession(7)
	if err := manager.OpenSession(7); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	// 实体 9 从未交付给 7（注销后立刻被忘掉），随即以同 ID 重新登记并再次 Bind。
	if err := manager.Register(subjectState(t, 9)); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 9, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unregister(9); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(subjectState(t, 9)); err != nil {
		t.Fatalf("premise: subject 9 was not forgotten at once: %v", err)
	}
	if err := direct.Bind(7, 9, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	if !holds(manager, 7, 8) || !holds(manager, 7, 9) {
		t.Fatalf("premise: rebinding did not subscribe: 8=%v 9=%v", manager.Subscribers(8), manager.Subscribers(9))
	}
	if bound := directBindings(direct); bound != 2 {
		t.Fatalf("bindings made again before the release arrived were dropped: bound=%d, want 2", bound)
	}
}

// 会话在实体缺席（RR-59 撤销后）期间关闭：绑定随会话删除，实体重新登记后不重新提交到同 ID 重开的新连接上；
// 会话一直开着时撤销不删除绑定（RR-70 不变，另见 TestGroupAndDirectResubmitAfterRetraction）。
func TestDirectDropsTheBindingOfARetractedSubjectWhenItsSessionCloses(t *testing.T) {
	manager, _ := newLoggedManager(t)
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	player(t, manager, 7)
	state8 := subjectState(t, 8)
	if err := manager.Register(state8); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 1)
	retractUnloaded(t, manager, state8)
	if bound := directBindings(direct); bound != 1 {
		t.Fatalf("the RR-59 retraction dropped the binding (bound=%d); RR-70 needs it to resubmit", bound)
	}
	manager.CloseSession(7)
	flushTimes(manager, 1)
	if bound := directBindings(direct); bound != 0 {
		t.Fatalf("the binding of a closed session outlived it while its subject was away: bound=%d", bound)
	}
	if err := manager.OpenSession(7); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(subjectState(t, 8)); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	if holds(manager, 7, 8) {
		t.Fatalf("a binding of the closed connection was resubmitted to the reopened one: %v", manager.Subscribers(8))
	}
}
