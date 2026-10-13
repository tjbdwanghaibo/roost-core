// 政策重新提交：框架撤销的订阅在重新登记后由各政策恢复，再次撤销、Rebind 与并发离开下仍收敛。

package policy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 卸载后重载不了时框架 RetractUnloadedSubject 退回 remove 并忘掉 subject；被框架撤销的订阅在同 ID 重新登记时交还给订阅它的政策，
// 由政策按自己的判定重新提交；观察者已离开、政策已释放的 pair 不恢复；多个来源各自重新提交、各自撤销。
// 历史：RR-20260926-70。

// opLog decodes every frame and records, per session, the object operations in delivery order.
type opLog struct {
	mu  sync.Mutex
	ops map[entitysync.SessionID][]string
}

func (l *opLog) Push(_ context.Context, session entitysync.SessionID, data []byte) error {
	decoded, err := entitysync.DecodeFrame(data, frame.DefaultLimits())
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ops == nil {
		l.ops = make(map[entitysync.SessionID][]string)
	}
	for _, object := range decoded.Objects {
		name := map[frame.ObjectOperation]string{frame.ObjectCreate: "create", frame.ObjectUpdate: "update", frame.ObjectRemove: "remove"}[object.Operation]
		subject := int64(-1)
		for _, component := range object.Components {
			update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
			if err != nil {
				return err
			}
			subject = update.SubjectID
		}
		l.ops[session] = append(l.ops[session], fmt.Sprintf("%s:%d", name, subject))
	}
	return nil
}

func (l *opLog) take(session entitysync.SessionID) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.ops[session]
	delete(l.ops, session)
	return out
}

func newLoggedManager(t *testing.T) (*entitysync.Manager, *opLog) {
	t.Helper()
	log := &opLog{}
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return manager, log
}

// retractUnloaded is what the unload fallback does: ManagerAccess.Unload closed the state, the reload could
// not be served, the framework retracts the subject; the removes go out and the subject is forgotten.
func retractUnloaded(t *testing.T, manager *entitysync.Manager, state *entity.SubjectSyncState) {
	t.Helper()
	id := state.SubjectID()
	state.Close()
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("setup: retract refused")
	}
	for range 3 {
		_ = manager.Flush(context.Background())
	}
	if got := manager.Subscribers(id); len(got) != 0 {
		t.Fatalf("setup: removes not delivered yet: %v", got)
	}
}

func flushTimes(manager *entitysync.Manager, n int) {
	for range n {
		_ = manager.Flush(context.Background())
	}
}

func holds(manager *entitysync.Manager, observer, subject int64) bool {
	return slices.Contains(manager.Subscribers(subject), entitysync.SessionID(observer))
}

// sideBySide puts player 1 (observer) and entity 2 (registered, with its own session) side by side.
func sideBySide(t *testing.T, manager *entitysync.Manager, in *Interest) *entity.SubjectSyncState {
	t.Helper()
	player(t, manager, 1)
	state2 := subjectState(t, 2)
	if err := manager.Register(state2); err != nil {
		t.Fatal(err)
	}
	if err := manager.OpenSession(entitysync.SessionID(2)); err != nil {
		t.Fatal(err)
	}
	if err := in.Enter(1, spatial.Point{X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	if err := in.Enter(2, spatial.Point{X: 20, Y: 10}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	if !subscribed(manager, 1, 2) {
		t.Fatalf("setup: observer 1 not subscribed to 2: %v", manager.Subscribers(2))
	}
	return state2
}

// 观察者仍在 AOI 内，实体重新登记后自动恢复可见（不需要 pair 变化，也不需要调用方 Apply），订阅者看到的是 remove 之后的一次
// create——不重复、不丢、remove-before-create。
func TestInterestResubmitsASubscriptionTheFrameworkRetracted(t *testing.T) {
	manager, log := newLoggedManager(t)
	in := newInterest(t, manager)
	state2 := sideBySide(t, manager, in)
	flushTimes(manager, 1)
	log.take(1)

	retractUnloaded(t, manager, state2)
	if err := manager.Register(subjectState(t, 2)); err != nil {
		t.Fatalf("re-register after retraction: %v", err)
	}
	flushTimes(manager, 3)
	if !holds(manager, 1, 2) {
		t.Fatalf("entity 2 is registered again and still within observer 1's AOI (Visible=%v), but the policy never re-subscribes: observer 1 lost it until the pair changes", in.Visible(1))
	}
	if !holds(manager, 2, 2) {
		t.Fatalf("entity 2's own (self) subscription was not resubmitted: %v", manager.Subscribers(2))
	}
	if got := log.take(1); !slices.Equal(got, []string{"remove:-1", "create:2"}) {
		t.Fatalf("observer 1 received %v for the retraction and the re-registration, want one remove then one create", got)
	}
	if refusals := in.Apply(); len(refusals) != 0 {
		t.Fatalf("an idle apply after the resubmit refused: %+v", refusals)
	}
	// 恢复的 pair 仍按政策自己的规则释放。
	if err := in.Move(1, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	if subscribed(manager, 1, 2) {
		t.Fatal("a resubmitted pair did not release when the observer moved away")
	}
}

// 观察者在实体缺席期间离开了 AOI：政策已释放该 pair，实体重新登记后不恢复；之后回到范围内照常经政策订阅。
func TestInterestDoesNotResubmitAPairItReleasedWhileTheSubjectWasAway(t *testing.T) {
	manager, log := newLoggedManager(t)
	in := newInterest(t, manager)
	state2 := sideBySide(t, manager, in)
	retractUnloaded(t, manager, state2)
	if err := in.Move(1, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	if refusals := in.Apply(); len(refusals) != 0 {
		t.Fatalf("releasing a pair of an absent subject refused: %+v", refusals)
	}
	log.take(1)
	if err := manager.Register(subjectState(t, 2)); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 3)
	if holds(manager, 1, 2) {
		t.Fatalf("observer 1 left entity 2's range while it was away, but the subscription came back: %v", manager.Subscribers(2))
	}
	if got := log.take(1); len(got) != 0 {
		t.Fatalf("observer 1 received %v for a subject it no longer sees", got)
	}
	if err := in.Move(1, spatial.Point{X: 10, Y: 10}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	if !subscribed(manager, 1, 2) {
		t.Fatal("coming back into range did not subscribe through the policy")
	}
}

// 多来源：Interest、Group、Direct 同时订阅；缺席期间 Direct 解绑。重新登记后仍持有 pair 的来源各自重新提交，
// 已解绑的 Direct 不恢复；之后逐个来源撤销互不影响，最后一个来源撤销才发 remove。
func TestEachPolicyResubmitsItsOwnRetractedSubscription(t *testing.T) {
	manager, log := newLoggedManager(t)
	in := newInterest(t, manager)
	group, err := NewGroup(GroupConfig{Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	direct, err := NewDirect(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	state2 := sideBySide(t, manager, in)
	if err := group.AddSubject(state2); err != nil {
		t.Fatal(err)
	}
	if err := group.Join(1); err != nil {
		t.Fatal(err)
	}
	if err := direct.Bind(1, 2, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 1)

	retractUnloaded(t, manager, state2)
	if err := direct.Unbind(1, 2); err != nil && !errors.Is(err, entitysync.ErrSubjectNotRegistered) {
		t.Fatalf("unbind of an absent subject: %v", err)
	}
	log.take(1)
	if err := manager.Register(subjectState(t, 2)); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 3)
	if !holds(manager, 1, 2) {
		t.Fatalf("entity 2 is registered again, Interest and Group still hold the pair, but nobody re-subscribed: %v", manager.Subscribers(2))
	}
	if got := log.take(1); !slices.Equal(got, []string{"create:2"}) {
		t.Fatalf("observer 1 received %v after the re-registration, want exactly one create", got)
	}
	// Group 撤销：Interest 仍持有，不发 remove。
	if err := group.RemoveSubject(2); err != nil {
		t.Fatal(err)
	}
	flushTimes(manager, 2)
	if !holds(manager, 1, 2) || len(log.take(1)) != 0 {
		t.Fatal("releasing the group's source took the Interest subscription with it")
	}
	// Interest 撤销：Direct 已解绑、Group 已撤销，最后一个来源离开，发 remove。
	if err := in.Move(1, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	flushTimes(manager, 2)
	if holds(manager, 1, 2) {
		t.Fatalf("the subscription outlived every source (did the unbound Direct come back?): %v", manager.Subscribers(2))
	}
	if got := log.take(1); !slices.Equal(got, []string{"remove:-1"}) {
		t.Fatalf("observer 1 received %v when the last source left, want one remove", got)
	}
}

// Group 与 Direct 单独使用时同样自动重新提交；Direct 恢复的是调用方当前绑定的视图。
func TestGroupAndDirectResubmitAfterRetraction(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		manager, _ := newLoggedManager(t)
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
		retractUnloaded(t, manager, state2)
		if err := manager.Register(subjectState(t, 2)); err != nil {
			t.Fatal(err)
		}
		flushTimes(manager, 2)
		if !holds(manager, 1, 2) {
			t.Fatalf("group member 1 does not receive the re-registered subject: %v", manager.Subscribers(2))
		}
	})
	t.Run("direct", func(t *testing.T) {
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
		if err := manager.Register(subjectState(t, 8)); err != nil {
			t.Fatal(err)
		}
		flushTimes(manager, 2)
		if !holds(manager, 7, 8) {
			t.Fatalf("the direct binding was not resubmitted: %v", manager.Subscribers(8))
		}
		if err := direct.Unbind(7, 8); err != nil {
			t.Fatal(err)
		}
		flushTimes(manager, 2)
		if holds(manager, 7, 8) {
			t.Fatal("a resubmitted binding did not release on unbind")
		}
	})
}

// 重新提交在实体重新登记后、下一次 Flush 的政策阶段交付。两者之间新 subject 又被卸载并撤销时，尚未交付的重新提交回到撤销表，
// 第三次登记后照常交还政策，政策仍持有的 pair 恢复可见（Group / Direct 不会因 Subscribe 被拒而永久丢失这对订阅）。
// 历史：RR-20260926-78。

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

// Interest 的重新提交回到撤销表、第三次登记后经 resubmit 恢复，不多发也不少发。
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

// 重新登记、观察者离开 / 留在范围内、Flush 三者并发：最终状态服从政策（走远 → 不订阅；仍在范围内 → 订阅），观察者 1 收到的
// 操作里不出现没有 remove 隔开的连续 create。
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

// 卸载后重载重试用尽时 RetractUnloadedSubject 退回 remove；实体随后被加载出来走的是 Rebind：remove 发完之前到达的 Rebind 排在
// 退役之后登记（返回 nil），这次登记把撤销记录交还 Interest；距离（AOI）、关系（team）与 self 三个来源的观察者都先收到 remove、
// 再收到一次 create，之后新状态上的变化照常送达，恢复的订阅照旧按政策规则释放。remove 发完之后 subject 已被忘掉，Rebind 按契约
// 报告 ErrSubjectNotRegistered，由登记该实体的业务 Register 恢复，这里只固定这条边界。

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
