package policy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// RR-20260926-70（维护者 2026-09-27 批准：通知政策重新提交订阅）：RR-59 卸载后重载不了，框架 RetractUnloadedSubject
// 退回 remove 并忘掉 subject；政策层仍记着这些 pair“已订阅”，实体之后重新登记时谁也不再说一遍，观察者仍在范围内
// 却永久看不到它。修复后被框架撤销的订阅在同 ID 重新登记时交还给订阅它的政策，由政策按自己的判定重新提交；
// 观察者已离开、政策已释放的 pair 不恢复；多个来源各自重新提交、各自撤销。

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

// retractUnloaded is what the RR-59 fallback does: ManagerAccess.Unload closed the state, the reload could
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

// REPRO-2026-09-26-06 §6 的正式版：观察者仍在 AOI 内，实体重新登记后自动恢复可见（不需要 pair 变化，也不需要
// 调用方 Apply），且订阅者看到的是 remove 之后的一次 create——不重复、不丢、remove-before-create。
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
