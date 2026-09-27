package policy

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// RR-20260926-85：RR-79 的释放通知在下一次政策阶段才送达。承诺：通知只删除它所指那一次会话生命期（会话关闭）或那一次
// 登记（业务 Unregister）里的绑定；通知途中在重开的会话 / 重新登记的实体上再次 Bind 的绑定是新的，即使它随后被 RR-59
// 撤销、正等待重新提交，也要保留，它的 RR-70 撤销记录不能被删，实体重新登记后照常恢复订阅。旧行为：Direct 用 Holds 的
// 当前值判定，subject 退役（被撤销）或已被忘掉时 Holds 为假，于是删掉新绑定并 Unsubscribe，Manager 随之删掉这对 pair 的
// 撤销记录，实体重新登记后 7 再也看不到 8。探针来源：REPRO-2026-09-26-08 §2（审计员 aud5 探针 F，此处为正式版；另补
// 同形的“注销后重新登记”一支）。事件顺序由调用决定：旧通知在 retractUnloaded 内第一次 Flush 的政策阶段送达，此时撤销已发生。

// rebindThenRetract 先让 7 绑定 8，再由 drop 让框架丢掉这次绑定（排下释放通知）并在新的会话生命期 / 新登记上重新 Bind，
// 不经过政策阶段；随后 RR-59 撤销 8、8 重新登记。返回撤销后 Direct 的绑定数与重新登记后 7 是否恢复订阅。
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
