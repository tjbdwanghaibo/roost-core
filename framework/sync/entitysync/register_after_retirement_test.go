package entitysync

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// registrationResults collects the done callbacks of queued registrations.
type registrationResults struct {
	mu      sync.Mutex
	results []error
}

func (r *registrationResults) done(err error) {
	r.mu.Lock()
	r.results = append(r.results, err)
	r.mu.Unlock()
}

func (r *registrationResults) take() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.results
	r.results = nil
	return out
}

// retiringSubject registers subject id, lets session 1 receive it and then
// retires it, so the subject still owes session 1 its ObjectRemove.
func retiringSubject(t *testing.T, manager *Manager, transport *recordingTransport, id int64, packs *int) *entity.SubjectSyncState {
	t.Helper()
	state := testSubject(t, id, packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1)
	mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	mustFlush(t, manager)
	oneFrame(t, transport, 1)
	if err := manager.Unregister(id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(state); !errors.Is(err, ErrSubjectRetiring) {
		t.Fatalf("register while the remove is owed = %v, want ErrSubjectRetiring", err)
	}
	return state
}

// RR-20260926-55：同一 tick 里 Leave 之后立即 Join（快速重连），subject 仍欠订阅者
// ObjectRemove，Register 返回 ErrSubjectRetiring，这次 Join 就丢了。
// RegisterAfterRetirement 把登记排在退役完成之后：remove 发出（remove-before-create 不变）
// 的同一次 Flush 里重新登记，done 报告结果；新的订阅从全量快照开始。
func TestRegisterAfterRetirementCompletesOnceTheRemoveIsOut(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := retiringSubject(t, manager, transport, 1301, &packs)

	var results registrationResults
	queued, err := manager.RegisterAfterRetirement(state, results.done)
	if err != nil || !queued {
		t.Fatalf("RegisterAfterRetirement on a retiring subject = (%v, %v), want queued", queued, err)
	}
	if err := manager.Subscribe(1, 1301, entity.SyncProfile{}); !errors.Is(err, ErrSubjectRetiring) {
		t.Fatalf("subscribe before the remove went out = %v, want ErrSubjectRetiring", err)
	}
	if got := results.take(); len(got) != 0 {
		t.Fatalf("done called before the retirement completed: %v", got)
	}

	mustFlush(t, manager)
	removed := oneFrame(t, transport, 1)
	if len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("the retirement's remove did not go out first: %+v", removed.wire.Objects)
	}
	oldRef := removed.wire.Objects[0].Ref
	if got := results.take(); len(got) != 1 || got[0] != nil {
		t.Fatalf("done after the retirement = %v, want one nil", got)
	}
	mustSubscribe(t, manager, 1, 1301, entity.SyncProfile{})
	mustFlush(t, manager)
	created := oneFrame(t, transport, 1)
	if len(created.wire.Objects) != 1 || created.wire.Objects[0].Operation != frame.ObjectCreate || created.wire.Objects[0].Ref == oldRef {
		t.Fatalf("the re-registered subject was not sent as a new object (removed %v): %+v", oldRef, created.wire.Objects)
	}
	if err := manager.Register(state); err != nil {
		t.Fatalf("the state is registered again, Register must be a no-op: %v", err)
	}
}

// 退役因订阅者会话关闭而完成（没有 remove 可发）时同样登记。
func TestRegisterAfterRetirementCompletesWhenTheLastSubscriberCloses(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := retiringSubject(t, manager, transport, 1302, &packs)
	var results registrationResults
	if queued, err := manager.RegisterAfterRetirement(state, results.done); err != nil || !queued {
		t.Fatalf("(%v, %v), want queued", queued, err)
	}
	manager.CloseSession(1)
	if got := results.take(); len(got) != 1 || got[0] != nil {
		t.Fatalf("done after the last subscriber closed = %v, want one nil", got)
	}
	if err := manager.Register(state); err != nil {
		t.Fatalf("the state is not registered after its retirement completed: %v", err)
	}
}

// 排队有界且可取消：每个 subject 至多一个排队登记，后到的替换先到的；再次 Unregister、
// 排队的状态已关闭、Manager 关闭都取消它，done 恰好报告一次。
func TestAQueuedRegistrationIsBoundedAndCancellable(t *testing.T) {
	t.Run("a later queue replaces the earlier one", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		packs := 0
		retiringSubject(t, manager, transport, 1311, &packs)
		first, second := testSubject(t, 1311, &packs), testSubject(t, 1311, &packs)
		var firstDone, secondDone registrationResults
		if queued, err := manager.RegisterAfterRetirement(first, firstDone.done); err != nil || !queued {
			t.Fatalf("(%v, %v)", queued, err)
		}
		if queued, err := manager.RegisterAfterRetirement(second, secondDone.done); err != nil || !queued {
			t.Fatalf("(%v, %v)", queued, err)
		}
		if got := firstDone.take(); len(got) != 1 || !errors.Is(got[0], ErrRegistrationCancelled) {
			t.Fatalf("the replaced registration's done = %v, want ErrRegistrationCancelled", got)
		}
		mustFlush(t, manager)
		if got := secondDone.take(); len(got) != 1 || got[0] != nil {
			t.Fatalf("the replacing registration's done = %v, want nil", got)
		}
		if err := manager.Register(first); !errors.Is(err, ErrSubjectRegistered) {
			t.Fatalf("the replaced state took the subject: %v", err)
		}
		if got := firstDone.take(); len(got) != 0 {
			t.Fatalf("the replaced registration reported twice: %v", got)
		}
	})
	t.Run("unregister again cancels it", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		packs := 0
		state := retiringSubject(t, manager, transport, 1312, &packs)
		var results registrationResults
		if queued, err := manager.RegisterAfterRetirement(state, results.done); err != nil || !queued {
			t.Fatalf("(%v, %v)", queued, err)
		}
		if err := manager.Unregister(1312); err != nil {
			t.Fatal(err)
		}
		if got := results.take(); len(got) != 1 || !errors.Is(got[0], ErrRegistrationCancelled) {
			t.Fatalf("done after a second Unregister = %v, want ErrRegistrationCancelled", got)
		}
		mustFlush(t, manager)
		if got := results.take(); len(got) != 0 {
			t.Fatalf("a cancelled registration completed anyway: %v", got)
		}
		if err := manager.Rebind(state); !errors.Is(err, ErrSubjectNotRegistered) {
			t.Fatalf("a cancelled registration left the subject registered: %v", err)
		}
	})
	t.Run("a state closed while queued is not registered", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		packs := 0
		retiringSubject(t, manager, transport, 1313, &packs)
		next := testSubject(t, 1313, &packs)
		var results registrationResults
		if queued, err := manager.RegisterAfterRetirement(next, results.done); err != nil || !queued {
			t.Fatalf("(%v, %v)", queued, err)
		}
		next.Close()
		mustFlush(t, manager)
		if got := results.take(); len(got) != 1 || !errors.Is(got[0], ErrRegistrationCancelled) {
			t.Fatalf("done for a state closed in the queue = %v, want ErrRegistrationCancelled", got)
		}
		if err := manager.Rebind(testSubject(t, 1313, &packs)); !errors.Is(err, ErrSubjectNotRegistered) {
			t.Fatalf("a closed state was registered: %v", err)
		}
	})
	t.Run("closing the manager cancels it", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		packs := 0
		state := retiringSubject(t, manager, transport, 1314, &packs)
		transport.breakSession(1, ErrRetryLater) // the remove cannot go out before the close
		var results registrationResults
		if queued, err := manager.RegisterAfterRetirement(state, results.done); err != nil || !queued {
			t.Fatalf("(%v, %v)", queued, err)
		}
		_ = manager.Close(context.Background()) // the last Flush reports the refused push
		if got := results.take(); len(got) != 1 || !errors.Is(got[0], ErrManagerClosed) {
			t.Fatalf("done after Close = %v, want ErrManagerClosed", got)
		}
	})
}

// 不在退役中时它就是 Register：立即登记（queued=false，done 不调用）；已登记的活状态照旧拒绝。
func TestRegisterAfterRetirementIsRegisterWhenNothingIsRetiring(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1321, &packs)
	var results registrationResults
	if queued, err := manager.RegisterAfterRetirement(state, results.done); err != nil || queued {
		t.Fatalf("fresh subject = (%v, %v), want registered now", queued, err)
	}
	if queued, err := manager.RegisterAfterRetirement(testSubject(t, 1321, &packs), results.done); !errors.Is(err, ErrSubjectRegistered) || queued {
		t.Fatalf("a second live state = (%v, %v), want ErrSubjectRegistered", queued, err)
	}
	if got := results.take(); len(got) != 0 {
		t.Fatalf("done called for a registration that was not queued: %v", got)
	}
}
