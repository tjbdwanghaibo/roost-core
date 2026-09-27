package policy

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/spatial"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
)

// RR-20260926-78：RR-70 的重新提交在实体重新登记后、下一次 Flush 的政策阶段交付。两者之间新 subject 又被卸载并撤销
// （有非政策来源的订阅者使 SubjectAwaitsReload 为真，重载器照样调度它）时，承诺是：尚未交付的重新提交回到撤销表，
// 第三次登记后照常交还政策，政策仍持有的 pair 恢复可见。旧行为：第二次撤销只记当时存在的订阅（政策的还没重说，
// 一条也没有），排队的重新提交留在队列里、在 subject 已退役或已忘掉时交付，Group / Direct 的 Subscribe 被拒绝后
// 记 Warn 放弃，这对订阅永久丢失；Interest 靠重试表恢复（本修复不改变它）。
// 探针来源：REPRO-2026-09-26-07 §2（审计员 Aud4 探针，此处为正式版）。

// secondRetractionBeforeDelivery：实体已重新登记（重新提交排队、政策阶段尚未运行），这个新 subject 又被卸载、重载不了、
// 被撤销，remove 交付完、subject 被忘掉之后第三次登记，再给政策阶段机会运行。
func secondRetractionBeforeDelivery(t *testing.T, manager *entitysync.Manager, id int64) {
	t.Helper()
	again := subjectState(t, id)
	if err := manager.Register(again); err != nil {
		t.Fatal(err)
	}
	// 非政策来源的订阅者（业务直接 Subscribe）让 SubjectAwaitsReload 为真，重载器才会调度，也才会再次撤销。
	if err := manager.OpenSession(99); err != nil {
		t.Fatal(err)
	}
	if err := manager.Subscribe(99, id, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	again.Close()
	if !manager.SubjectAwaitsReload(id) {
		t.Fatal("premise: the reloader would not schedule the re-registered subject")
	}
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: the second retraction was refused")
	}
	flushTimes(manager, 3)
	if got := manager.Subscribers(id); len(got) != 0 {
		t.Fatalf("premise: the second retraction has not finished: %v", got)
	}
	if err := manager.Register(subjectState(t, id)); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 3)
}

func TestGroupResubmitsAfterASecondRetractionBeforeDelivery(t *testing.T) {
	manager, log := newLoggedManager(t)
	group, err := NewGroup(GroupConfig{Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	state2 := subjectState(t, 2)
	if err := group.AddSubject(state2); err != nil {
		t.Fatal(err)
	}
	if err := group.Join(1); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 1)
	log.take(1)
	retractUnloaded(t, manager, state2)
	secondRetractionBeforeDelivery(t, manager, 2)
	if !holds(manager, 1, 2) {
		t.Fatalf("group member 1 permanently lost subject 2 after a second retraction before the resubmit was delivered (group subjects=%d members=%d): %v", group.Subjects(), group.Members(), manager.Subscribers(2))
	}
	if got := log.take(1); !slices.Equal(got, []string{"remove:-1", "create:2"}) {
		t.Fatalf("member 1 received %v across both retractions, want one remove then one create", got)
	}
	// 恢复的订阅照旧归组管理。
	if err := group.RemoveSubject(2); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	if holds(manager, 1, 2) {
		t.Fatal("a resubmitted group subscription did not release on RemoveSubject")
	}
}

func TestDirectResubmitsAfterASecondRetractionBeforeDelivery(t *testing.T) {
	manager, log := newLoggedManager(t)
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
	log.take(7)
	retractUnloaded(t, manager, state8)
	secondRetractionBeforeDelivery(t, manager, 8)
	if !holds(manager, 7, 8) {
		t.Fatalf("direct binding 7->8 permanently lost after a second retraction before the resubmit was delivered: %v", manager.Subscribers(8))
	}
	if got := log.take(7); !slices.Equal(got, []string{"remove:-1", "create:8"}) {
		t.Fatalf("observer 7 received %v across both retractions, want one remove then one create", got)
	}
	if err := direct.Unbind(7, 8); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	if holds(manager, 7, 8) {
		t.Fatal("a resubmitted binding did not release on Unbind")
	}
}

// Interest 修前靠重试表恢复；修后重新提交回到撤销表、第三次登记后经 resubmit 恢复。两条路都不能多发或少发。
func TestInterestStillRecoversAfterASecondRetractionBeforeDelivery(t *testing.T) {
	manager, log := newLoggedManager(t)
	in := newInterest(t, manager)
	state2 := sideBySide(t, manager, in)
	flushTimes(manager, 1)
	log.take(1)
	retractUnloaded(t, manager, state2)
	secondRetractionBeforeDelivery(t, manager, 2)
	_ = in.Apply()
	flushTimes(manager, 2)
	if !holds(manager, 1, 2) {
		t.Fatalf("interest observer 1 lost subject 2: %v", manager.Subscribers(2))
	}
	if got := log.take(1); !slices.Equal(got, []string{"remove:-1", "create:2"}) {
		t.Fatalf("observer 1 received %v across both retractions, want one remove then one create", got)
	}
	if refusals := in.Apply(); len(refusals) != 0 {
		t.Fatalf("an idle apply after the recovery refused: %+v", refusals)
	}
	if err := in.Move(1, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	if subscribed(manager, 1, 2) {
		t.Fatal("the recovered pair did not release when the observer moved away")
	}
}

// 重新登记、观察者离开 / 留在范围内、Flush 三者并发（REPRO-2026-09-26-07 §2 的 race 探针正式版，RR-70 / 78 共同承诺）：
// 最终状态服从政策（走远 → 不订阅；仍在范围内 → 订阅），观察者 1 收到的操作里不出现没有 remove 隔开的连续 create。
func TestResubmitRacesLeaveAndFlush(t *testing.T) {
	for _, walkAway := range []bool{true, false} {
		bad := 0
		for round := range 200 {
			manager, log := newLoggedManager(t)
			in := newInterest(t, manager)
			state2 := sideBySide(t, manager, in)
			flushTimes(manager, 1)
			retractUnloaded(t, manager, state2)
			log.take(1)
			var wg sync.WaitGroup
			start := make(chan struct{})
			wg.Add(3)
			go func() {
				defer wg.Done()
				<-start
				for range 3 {
					_ = manager.Flush(context.Background())
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				if err := manager.Register(subjectState(t, 2)); err != nil {
					t.Error(err)
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				target := spatial.Point{X: 12, Y: 10}
				if walkAway {
					target = spatial.Point{X: 900, Y: 900}
				}
				_ = in.Move(1, target)
				_ = in.Apply()
			}()
			close(start)
			wg.Wait()
			for range 3 {
				_ = in.Apply()
				flushTimes(manager, 2)
			}
			got := holds(manager, 1, 2)
			ops := log.take(1)
			if got == walkAway {
				bad++
				if bad <= 3 {
					t.Logf("walkAway=%v round %d: observer 1 subscribed=%v ops=%v", walkAway, round, got, ops)
				}
			}
			last := ""
			for _, op := range ops {
				if op == last && op != "update:2" {
					t.Errorf("walkAway=%v round %d: repeated %s in %v", walkAway, round, op, ops)
				}
				last = op
			}
			_ = manager.Close(context.Background())
		}
		if bad > 0 {
			t.Errorf("walkAway=%v: %d rounds ended against the policy", walkAway, bad)
		}
	}
}
