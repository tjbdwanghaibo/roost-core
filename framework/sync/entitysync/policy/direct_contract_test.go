package policy

import (
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"testing"
)

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

func rebindThenRetract(t *testing.T, deliverFirstBinding bool, drop func(*testing.T, *entitysync.Manager, *Direct, *entity.SubjectSyncState) *entity.SubjectSyncState) (int, bool) {
	t.Helper()
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
	if deliverFirstBinding {
		flushTimes(manager, 1)
	}
	if drop != nil {
		state8 = drop(t, manager, direct, state8)
	}
	retractUnloaded(t, manager, state8)
	bound := directBindings(direct)
	if err := manager.Register(subjectState(t, 8)); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	return bound, holds(manager, 7, 8)
}

func TestAStaleReleaseKeepsARebindingThatIsThenRetracted(t *testing.T) {
	// 对照：没有释放通知时，RR-59 撤销后 Direct 按绑定恢复订阅（RR-70）。
	if bound, restored := rebindThenRetract(t, true, nil); bound != 1 || !restored {
		t.Fatalf("premise: the retraction alone is not restored: bound=%d holds(7,8)=%v", bound, restored)
	}

	t.Run("session closed and reopened", func(t *testing.T) {
		bound, restored := rebindThenRetract(t, true, func(t *testing.T, manager *entitysync.Manager, direct *Direct, state8 *entity.SubjectSyncState) *entity.SubjectSyncState {
			manager.CloseSession(7) // 释放通知指向第一次打开
			if err := manager.OpenSession(7); err != nil {
				t.Fatal(err)
			}
			if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			return state8
		})
		if bound != 1 || !restored {
			t.Fatalf("the release of the closed lifetime dropped the binding made on the reopened session: bound after retraction=%d, after reload holds(7,8)=%v, want 1 and true", bound, restored)
		}
	})

	t.Run("subject unregistered and registered again", func(t *testing.T) {
		// 第一次绑定未交付：Unregister 时 subject 立刻被忘掉，通知仍排着，同 ID 可以马上重新登记。
		bound, restored := rebindThenRetract(t, false, func(t *testing.T, manager *entitysync.Manager, direct *Direct, _ *entity.SubjectSyncState) *entity.SubjectSyncState {
			if err := manager.Unregister(8); err != nil { // 释放通知指向第一次登记
				t.Fatal(err)
			}
			next := subjectState(t, 8)
			if err := manager.Register(next); err != nil {
				t.Fatalf("premise: subject 8 was not forgotten at once: %v", err)
			}
			if err := direct.Bind(7, 8, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			return next
		})
		if bound != 1 || !restored {
			t.Fatalf("the release of the unregistered subject dropped the binding made on its new registration: bound after retraction=%d, after reload holds(7,8)=%v, want 1 and true", bound, restored)
		}
	})
}
