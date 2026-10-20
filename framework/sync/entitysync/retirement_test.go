// subject 退役：remove-before-create、退役后排队登记、卸载撤销、注销与 forget 窗口、重新绑定从全量开始。

package entitysync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// 事务内新建的实体被撤销时经 RetractSyncSubject 注销：已持有对象的会话先收到 ObjectRemove，未持有的订阅直接移除；状态随后关闭
// 也不算 Flush 失败；退役完成前同 ID 不能重新登记（remove-before-create）。只撤回同一个状态对象。
// 历史：RR-20260926-35。
func TestRetractSyncSubjectRemovesHeldObjectsBeforeRecreate(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	state := testSubject(t, 1201, &packs)
	if err := manager.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, manager, 1, 2)
	mustSubscribe(t, manager, 1, 1201, entity.SyncProfile{})
	mustFlush(t, manager)
	created := oneFrame(t, transport, 1)
	ref := created.wire.Objects[0].Ref
	mustSubscribe(t, manager, 2, 1201, entity.SyncProfile{}) // 会话 2 尚未收到对象

	other := testSubject(t, 1201, &packs)
	manager.RetractSyncSubject(other) // 不同的状态对象：不是这次登记，不能误撤
	if got := manager.Subscribers(1201); len(got) != 2 {
		t.Fatalf("retracting a foreign state touched the subject: %v", got)
	}

	manager.RetractSyncSubject(state)
	state.Close() // 撤销方随后关闭实体同步状态
	if err := manager.Register(other); !errors.Is(err, ErrSubjectRetiring) {
		t.Fatalf("re-register before the remove went out: %v", err)
	}
	mustFlush(t, manager)
	removed := oneFrame(t, transport, 1)
	if len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove || removed.wire.Objects[0].Ref != ref {
		t.Fatalf("remove must name the delivered object %v, got %+v", ref, removed.wire.Objects)
	}
	if got := transport.take(2); len(got) != 0 {
		t.Fatalf("a session that never held the object got %d frames", len(got))
	}
	if err := manager.Register(other); err != nil {
		t.Fatalf("re-register after the remove: %v", err)
	}
}

// RetractSyncSubject(state) 只撤回以 state 登记的那一次：取到 subject 之后、注销生效之前同 ID 换成另一个状态登记时，撤回不能
// 落到新登记上。窗口由 retractLookedUp 测试缝确定性进入。
// 历史：RR-20260927-28。
func TestRetractSyncSubjectDoesNotRetireALaterRegistration(t *testing.T) {
	for _, shape := range []string{"rebound_to_new_state", "forgotten_then_registered_again"} {
		t.Run(shape, func(t *testing.T) {
			transport := newRecordingTransport()
			manager := newTestManager(t, transport, ManagerConfig{})
			open(t, manager, 1)
			const id = 1211
			first := labelledSubject(id, "first")
			if err := manager.Register(first); err != nil {
				t.Fatal(err)
			}
			second := labelledSubject(id, "second")
			windowSeen := false
			manager.retractLookedUp = func(int64) {
				manager.retractLookedUp = nil
				windowSeen = true
				switch shape {
				case "rebound_to_new_state":
					first.Close() // 旧状态关闭，同 ID 接到新状态上（同一个 subject）
					if err := manager.Rebind(second); err != nil {
						t.Fatalf("premise: rebind to the new state: %v", err)
					}
				case "forgotten_then_registered_again":
					// 旧登记被注销；没有会话持有对象，随即被忘掉，新状态登记为新 subject
					if err := manager.Unregister(id); err != nil {
						t.Fatalf("premise: unregister the old registration: %v", err)
					}
					if manager.subject(id) != nil {
						t.Fatal("premise: the old subject was not forgotten")
					}
					if err := manager.Register(second); err != nil {
						t.Fatalf("premise: register the new state: %v", err)
					}
				}
				mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
			}
			manager.RetractSyncSubject(first)
			if !windowSeen {
				t.Fatal("premise: the window between lookup and unregister was not entered")
			}
			subj := manager.subject(id)
			if subj == nil {
				t.Fatalf("RetractSyncSubject(first) retired the registration of the second state: subject %d is gone", id)
			}
			subj.mu.Lock()
			current, retiring := subj.state, subj.retiring
			subj.mu.Unlock()
			if current != second || retiring {
				t.Fatalf("RetractSyncSubject(first) retired the registration of the second state: state is second=%v, retiring=%v", current == second, retiring)
			}
			mustFlush(t, manager)
			if got := manager.Subscribers(id); len(got) != 1 || got[0] != 1 {
				t.Fatalf("session 1's subscription to the second registration was dropped: %v", got)
			}
		})
	}
}

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

// 快速重连（同一 tick 里 Leave 之后立即 Join）时 subject 仍欠订阅者 ObjectRemove：RegisterAfterRetirement 把登记排在退役完成之后，
// remove 发出的同一次 Flush 里重新登记，done 报告结果；新的订阅从全量快照开始。
// 历史：RR-20260926-55。
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

// 实体被仅内存卸载后 RetractUnloadedSubject 退回 remove（与 Unregister 同一退役）。退役完成前实体又被加载出来时，Rebind / Register
// 把新状态排在退役完成之后登记（RegisterAfterRetirement 的同一机制），不返回 ErrSubjectRetiring；先交付 remove，同一步以新状态
// 重新登记。业务自己 Unregister 的退役不受影响（仍返回 ErrSubjectRetiring）。
// 历史：RR-20260926-59。

func retractedUnloadedSubject(t *testing.T, manager *Manager, transport *recordingTransport, id int64) *entity.SubjectSyncState {
	t.Helper()
	unloaded := labelledSubject(id, "phantom")
	if err := manager.Register(unloaded); err != nil {
		t.Fatal(err)
	}
	mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	mustFlush(t, manager)
	oneFrame(t, transport, 1)
	unloaded.Close() // ManagerAccess.Unload 关闭同步状态
	if !manager.SubjectAwaitsReload(id) {
		t.Fatal("premise: an unloaded subject with a subscriber does not await a reload")
	}
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: retract of an unloaded subject refused")
	}
	if manager.RetractUnloadedSubject(id) || manager.SubjectAwaitsReload(id) {
		t.Fatal("a retiring subject was retracted twice or still awaits a reload")
	}
	return unloaded
}

func TestReloadDuringUnloadRetractionIsRegisteredAfterTheRemove(t *testing.T) {
	for _, via := range []string{"Rebind", "Register"} {
		t.Run(via, func(t *testing.T) {
			transport := newRecordingTransport()
			manager := newTestManager(t, transport, ManagerConfig{})
			open(t, manager, 1)
			retractedUnloadedSubject(t, manager, transport, 3101)

			reloaded := labelledSubject(3101, "mongo")
			var err error
			if via == "Rebind" {
				err = manager.Rebind(reloaded)
			} else {
				err = manager.Register(reloaded)
			}
			if err != nil {
				t.Fatalf("%s of the reloaded state during the retraction=%v, want it queued behind the remove (no ErrSubjectRetiring)", via, err)
			}
			if err = manager.Subscribe(1, 3101, entity.SyncProfile{}); !errors.Is(err, ErrSubjectRetiring) {
				t.Fatalf("subscribe during the retraction=%v, want ErrSubjectRetiring (remove-before-create)", err)
			}
			mustFlush(t, manager)
			removed := oneFrame(t, transport, 1)
			if len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove {
				t.Fatalf("retraction frame=%+v, want ObjectRemove", removed.wire.Objects)
			}
			if err = manager.Rebind(reloaded); err != nil {
				t.Fatalf("the queued state was not registered once the remove went out: %v", err)
			}
			if err = manager.Register(labelledSubject(3101, "other")); !errors.Is(err, ErrSubjectRegistered) {
				t.Fatalf("register of a second state after the queued registration=%v, want ErrSubjectRegistered", err)
			}
			mustSubscribe(t, manager, 1, 3101, entity.SyncProfile{})
			mustFlush(t, manager)
			created := oneFrame(t, transport, 1)
			if created.objects[3101] != frame.ObjectCreate || string(created.updates[3101].Payload.BytesCopy()) != "snapshot:mongo" {
				t.Fatalf("new subscription after the retraction=%v, want a create of the reloaded state", created.objects)
			}
		})
	}
}

func TestBusinessUnregisterStillRefusesRegistrationWhileRetiring(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1)
	retractedUnloadedSubject(t, manager, transport, 3102)
	if err := manager.Unregister(3102); err != nil { // 业务的注销意图优先
		t.Fatal(err)
	}
	if err := manager.Register(labelledSubject(3102, "mongo")); !errors.Is(err, ErrSubjectRetiring) {
		t.Fatalf("register after a business Unregister=%v, want ErrSubjectRetiring (RegisterAfterRetirement is the entry point)", err)
	}
}

// 卸载退役进行中，业务的 RegisterAfterRetirement 返回 queued=true，done 在 remove 交付后报告结果，恰好一次。
// 历史：RR-20260926-72。
func TestRegisterAfterRetirementDuringUnloadRetractionIsQueued(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 1)
	retractedUnloadedSubject(t, manager, transport, 3201)

	joined := labelledSubject(3201, "join")
	var results registrationResults
	queued, err := manager.RegisterAfterRetirement(joined, results.done)
	if err != nil || !queued {
		t.Fatalf("RegisterAfterRetirement during the unload retraction = (queued=%v, err=%v), want queued=true: the registration waits for the remove and done reports it", queued, err)
	}
	if got := results.take(); len(got) != 0 {
		t.Fatalf("done before the remove went out: %v", got)
	}
	mustFlush(t, manager)
	if removed := oneFrame(t, transport, 1); len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("retraction frame=%+v, want ObjectRemove", removed.wire.Objects)
	}
	if got := results.take(); len(got) != 1 || got[0] != nil {
		t.Fatalf("done after the remove went out = %v, want exactly one nil", got)
	}
	if err := manager.Rebind(joined); err != nil {
		t.Fatalf("the queued state is not the registered one: %v", err)
	}
	mustFlush(t, manager)
	if got := results.take(); len(got) != 0 {
		t.Fatalf("done reported more than once: %v", got)
	}
}

// 卸载退役中 rebind 路径（kit 的 OnEntityLoaded）与业务 RegisterAfterRetirement 共用一个排队位，后到的替换先到的；被替换的业务
// 登记由 done 收到 ErrRegistrationCancelled，替换者在 remove 之后登记。
func TestUnloadRetractionQueueLaterReplacesEarlier(t *testing.T) {
	t.Run("a reload replaces a queued business registration", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1)
		retractedUnloadedSubject(t, manager, transport, 3202)
		joined, reloaded := labelledSubject(3202, "join"), labelledSubject(3202, "mongo")
		var results registrationResults
		if queued, err := manager.RegisterAfterRetirement(joined, results.done); err != nil || !queued {
			t.Fatalf("RegisterAfterRetirement during the unload retraction = (queued=%v, err=%v), want queued=true", queued, err)
		}
		if err := manager.Rebind(reloaded); err != nil {
			t.Fatalf("Rebind of the reloaded state during the retraction = %v, want it queued", err)
		}
		if got := results.take(); len(got) != 1 || !errors.Is(got[0], ErrRegistrationCancelled) {
			t.Fatalf("the replaced business registration's done = %v, want ErrRegistrationCancelled", got)
		}
		mustFlush(t, manager)
		oneFrame(t, transport, 1)
		if err := manager.Rebind(reloaded); err != nil {
			t.Fatalf("the replacing reload is not registered: %v", err)
		}
		if err := manager.Register(joined); !errors.Is(err, ErrSubjectRegistered) {
			t.Fatalf("the replaced state took the subject: %v", err)
		}
		if got := results.take(); len(got) != 0 {
			t.Fatalf("the replaced registration reported twice: %v", got)
		}
	})
	t.Run("a business registration replaces a queued reload", func(t *testing.T) {
		transport := newRecordingTransport()
		manager := newTestManager(t, transport, ManagerConfig{})
		open(t, manager, 1)
		retractedUnloadedSubject(t, manager, transport, 3203)
		joined, reloaded := labelledSubject(3203, "join"), labelledSubject(3203, "mongo")
		if err := manager.Rebind(reloaded); err != nil {
			t.Fatalf("Rebind of the reloaded state during the retraction = %v, want it queued", err)
		}
		var results registrationResults
		if queued, err := manager.RegisterAfterRetirement(joined, results.done); err != nil || !queued {
			t.Fatalf("RegisterAfterRetirement after a queued reload = (queued=%v, err=%v), want queued=true", queued, err)
		}
		mustFlush(t, manager)
		oneFrame(t, transport, 1)
		if got := results.take(); len(got) != 1 || got[0] != nil {
			t.Fatalf("the replacing business registration's done = %v, want exactly one nil", got)
		}
		if err := manager.Rebind(joined); err != nil {
			t.Fatalf("the business state is not the registered one: %v", err)
		}
	})
}

// Unregister 无锁取到 subject、取 subj.mu 之前，旧 subject 被 forget，同 ID 重新登记并由政策重新订阅：一次释放只丢掉它所指那次
// 登记上的订阅——新登记上做的订阅（戳更晚）不受影响，待交还的撤销记录也不被清掉。政策按 SubscriptionSourceHooks.Released 的
// 文档规则簿记（与 policy.Direct.released 相同），窗口由 unregisterLookedUp 测试缝确定性进入。
func TestUnregisterOfAForgottenSubjectDoesNotReleaseTheNextRegistration(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 7)
	const id = 8

	var (
		mu    sync.Mutex
		bound = map[RetractedSubscription]SubscriptionStamp{}
	)
	var source *SubscriptionSource
	bind := func(session SessionID, subject int64) {
		stamp, err := source.SubscribeStamped(session, subject, entity.SyncProfile{})
		if err != nil {
			t.Fatalf("bind %d->%d: %v", session, subject, err)
		}
		mu.Lock()
		bound[RetractedSubscription{Session: session, Subject: subject}] = stamp
		mu.Unlock()
	}
	source = manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{
		Resubmit: func(batch []RetractedSubscription) {
			for _, pair := range batch {
				mu.Lock()
				_, still := bound[pair]
				mu.Unlock()
				if still {
					bind(pair.Session, pair.Subject)
				}
			}
		},
		Released: func(pairs []ReleasedSubscription) {
			mu.Lock()
			defer mu.Unlock()
			for _, pair := range pairs {
				key := RetractedSubscription{Session: pair.Session, Subject: pair.Subject}
				if stamp, ok := bound[key]; !ok || stamp > pair.Stamp {
					continue
				}
				delete(bound, key)
				_ = source.Unsubscribe(pair.Session, pair.Subject)
			}
		},
	})

	first := labelledSubject(id, "first")
	if err := manager.Register(first); err != nil {
		t.Fatal(err)
	}
	bind(7, id)
	mustFlush(t, manager)
	transport.take(7)
	// RR-59：卸载后重载不了，退回 remove；remove 还没发出，旧 subject 仍在表里（退役中）。
	first.Close()
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: retract refused")
	}

	var (
		newStamp   SubscriptionStamp
		windowSeen bool
	)
	manager.unregisterLookedUp = func(int64) {
		manager.unregisterLookedUp = nil
		windowSeen = true
		// 窗口内：旧 subject 的 remove 发出、被 forget；实体重新登记，政策在新登记上重新订阅（交还尚未交付）。
		mustFlush(t, manager)
		if got := manager.subject(id); got != nil {
			t.Fatalf("premise: the old subject was not forgotten in the window: %v", got.id)
		}
		if err := manager.Register(labelledSubject(id, "second")); err != nil {
			t.Fatal(err)
		}
		bind(7, id)
		mu.Lock()
		newStamp = bound[RetractedSubscription{Session: 7, Subject: id}]
		mu.Unlock()
	}
	if err := manager.Unregister(id); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if !windowSeen {
		t.Fatal("premise: the window between lookup and lock was not entered")
	}
	mustFlush(t, manager)
	mustFlush(t, manager)

	registered := manager.subject(id) != nil
	mu.Lock()
	stamp, stillBound := bound[RetractedSubscription{Session: 7, Subject: id}]
	mu.Unlock()
	holds := false
	for _, sid := range manager.Subscribers(id) {
		holds = holds || sid == 7
	}
	t.Logf("after Unregister of the forgotten subject: new registration still registered=%v, policy binding kept=%v (stamp %d, bound at %d), session 7 subscribed=%v",
		registered, stillBound, stamp, newStamp, holds)
	if registered && (!stillBound || !holds) {
		t.Fatalf("Unregister looked up the old subject, which was forgotten before it took the lock; the new registration stayed registered, "+
			"but its subscription made after the re-registration (stamp %d) was released by this Unregister: policy binding kept=%v, session 7 subscribed=%v",
			newStamp, stillBound, holds)
	}
}

// 旧 subject 已被 forget 摘表、forgotten 标记还没打上时 Unregister 取到了锁：必须确认取到的仍是表里当前那一个。按 ID 注销的是
// 当前登记，所以重新登记的那一个被退役，政策在新登记上的订阅随之释放，按 ID 待交还的记录也不再留下。
// 历史：RR-20260927-22。
func TestUnregisterBetweenUnlinkAndForgottenRetiresTheCurrentRegistration(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	open(t, manager, 7)
	const id = 8

	var (
		mu    sync.Mutex
		bound = map[RetractedSubscription]SubscriptionStamp{}
	)
	var source *SubscriptionSource
	bind := func(session SessionID, subject int64) {
		stamp, err := source.SubscribeStamped(session, subject, entity.SyncProfile{})
		if err != nil {
			t.Errorf("bind %d->%d: %v", session, subject, err)
			return
		}
		mu.Lock()
		bound[RetractedSubscription{Session: session, Subject: subject}] = stamp
		mu.Unlock()
	}
	source = manager.NewSubscriptionSourceWithHooks(SubscriptionSourceHooks{
		Resubmit: func(batch []RetractedSubscription) {
			for _, pair := range batch {
				mu.Lock()
				_, still := bound[pair]
				mu.Unlock()
				if still {
					bind(pair.Session, pair.Subject)
				}
			}
		},
		Released: func(pairs []ReleasedSubscription) {
			mu.Lock()
			defer mu.Unlock()
			for _, pair := range pairs {
				key := RetractedSubscription{Session: pair.Session, Subject: pair.Subject}
				if stamp, ok := bound[key]; !ok || stamp > pair.Stamp {
					continue
				}
				delete(bound, key)
				_ = source.Unsubscribe(pair.Session, pair.Subject)
			}
		},
	})

	first := labelledSubject(id, "first")
	if err := manager.Register(first); err != nil {
		t.Fatal(err)
	}
	bind(7, id)
	mustFlush(t, manager)
	transport.take(7)
	first.Close()
	if !manager.RetractUnloadedSubject(id) {
		t.Fatal("premise: retract refused")
	}

	// Unregister 取到旧 subject 后停在取锁之前；旧 subject 的 remove 由下面的 Flush 发出并进入 forget。
	lookedUp, proceed, unregistered := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	manager.unregisterLookedUp = func(int64) {
		manager.unregisterLookedUp = nil
		close(lookedUp)
		<-proceed
	}
	go func() { unregistered <- manager.Unregister(id) }()
	<-lookedUp

	var (
		newStamp   SubscriptionStamp
		windowSeen bool
		unregErr   error
	)
	manager.forgetUnlinked = func(int64) {
		manager.forgetUnlinked = nil
		windowSeen = true
		// 旧 subject 已离表、forgotten 尚未标记：同 ID 重新登记，政策在新登记上订阅；然后放 Unregister 取锁并等它返回。
		if err := manager.Register(labelledSubject(id, "second")); err != nil {
			t.Error(err)
		}
		bind(7, id)
		mu.Lock()
		newStamp = bound[RetractedSubscription{Session: 7, Subject: id}]
		mu.Unlock()
		close(proceed)
		unregErr = <-unregistered
	}
	mustFlush(t, manager)
	if !windowSeen {
		t.Fatal("premise: the forget window between unlink and the forgotten mark was not entered")
	}
	if unregErr != nil {
		t.Fatalf("unregister: %v", unregErr)
	}
	mustFlush(t, manager)
	mustFlush(t, manager)

	registered := manager.subject(id) != nil
	mu.Lock()
	stamp, stillBound := bound[RetractedSubscription{Session: 7, Subject: id}]
	mu.Unlock()
	holds := false
	for _, sid := range manager.Subscribers(id) {
		holds = holds || sid == 7
	}
	t.Logf("after Unregister between unlink and forgotten: registered=%v, policy binding kept=%v (stamp %d, bound at %d), session 7 subscribed=%v, pending hand-back records=%d",
		registered, stillBound, stamp, newStamp, holds, manager.resubmitEntries.Load())
	if registered {
		t.Fatalf("Unregister(%d) returned nil but the registration made after the old subject left the table is still registered "+
			"(policy binding kept=%v, session 7 subscribed=%v): it acted on the unlinked old subject instead of the current one", id, stillBound, holds)
	}
	if stillBound || holds {
		t.Fatalf("the current registration was retired but the policy binding made on it (stamp %d) was not released: kept=%v, subscribed=%v", newStamp, stillBound, holds)
	}
	if n := manager.resubmitEntries.Load(); n != 0 {
		t.Fatalf("hand-back records of the retired registration were left behind: %d", n)
	}
}

// 退役收尾（forget）把 subject 从表里摘下之后、清除旧 subject 的脏通知器之前，同一状态对象被 RegisterAfterRetirement 直接登记：
// 新 subject 的通知器不被清掉，此后 MarkDirty 照常进入 pending。forgetUnlinked 测试缝让登记确定性地落在这个窗口里。
// 历史：RR-20260926-69。
func TestRegistrationInTheForgetWindowKeepsItsDirtyNotifier(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	const id = int64(1401)
	state := retiringSubject(t, manager, transport, id, &packs)

	var results registrationResults
	var queued bool
	var regErr error
	windows := 0
	manager.forgetUnlinked = func(subjectID int64) {
		if subjectID != id || windows > 0 {
			return
		}
		windows++
		// 退役刚完成、subject 已离表：业务此刻的登记直接走 Register。
		queued, regErr = manager.RegisterAfterRetirement(state, results.done)
	}
	mustFlush(t, manager) // 交付 remove → forget
	manager.forgetUnlinked = nil
	if windows != 1 {
		t.Fatalf("premise: the forget window was reached %d time(s), want 1", windows)
	}
	if regErr != nil || queued {
		t.Fatalf("premise: registration in the forget window = (queued=%v, err=%v), want a direct registration", queued, regErr)
	}
	if removed := oneFrame(t, transport, 1); len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("retirement frame=%+v, want ObjectRemove", removed.wire.Objects)
	}
	if got := results.take(); len(got) != 0 {
		t.Fatalf("done called for a registration that was not queued: %v", got)
	}
	if subj := manager.subject(id); subj == nil || subj.currentState() != state {
		t.Fatalf("the registration in the forget window is not in place: %v", subj)
	}

	manager.takePending()
	state.MarkDirty(1)
	manager.pendingMu.Lock()
	_, pending := manager.pending[id]
	manager.pendingMu.Unlock()
	if !pending {
		t.Fatalf("registered again in the forget window (queued=false err=nil), but MarkDirty does not schedule it: forget cleared the new subject's dirty notifier")
	}

	// 后续变化照常同步：新订阅先得到 create，之后的 MarkDirty 得到 update。
	mustSubscribe(t, manager, 1, id, entity.SyncProfile{})
	mustFlush(t, manager)
	if created := oneFrame(t, transport, 1); created.objects[id] != frame.ObjectCreate {
		t.Fatalf("subscription after the re-registration=%v, want a create", created.objects)
	}
	state.MarkDirty(1)
	mustFlush(t, manager)
	if updated := oneFrame(t, transport, 1); updated.objects[id] != frame.ObjectUpdate {
		t.Fatalf("change after the re-registration=%v, want an update", updated.objects)
	}
}

// forget 只忘掉调用方看到的那个 subject：同 ID 已重新登记为新 subject 之后，对旧 subject 迟到的 forget
// （两条收尾路径各看到一次“最后一个订阅者已走”）不删新 subject、不清它的通知器。
func TestALateForgetOfTheOldSubjectLeavesTheNewOneAlone(t *testing.T) {
	transport := newRecordingTransport()
	manager := newTestManager(t, transport, ManagerConfig{})
	packs := 0
	const id = int64(1402)
	retiringSubject(t, manager, transport, id, &packs)
	old := manager.subject(id)
	mustFlush(t, manager)
	oneFrame(t, transport, 1)
	next := testSubject(t, id, &packs)
	if err := manager.Register(next); err != nil {
		t.Fatal(err)
	}
	manager.forget(old)
	if subj := manager.subject(id); subj == nil || subj.currentState() != next {
		t.Fatalf("a late forget of the old subject removed the new one: %v", subj)
	}
	manager.takePending()
	next.MarkDirty(1)
	manager.pendingMu.Lock()
	_, pending := manager.pending[id]
	manager.pendingMu.Unlock()
	if !pending {
		t.Fatal("a late forget of the old subject cleared the new subject's dirty notifier")
	}
}

// 实体同步状态关闭（DataEngine 驱逐）后重新绑定：关闭期间不捕获、不重试、不计失败；重新绑定后每个订阅者收到整份全量（已持有
// 对象的是 ObjectUpdate + Full，对象引用不变），之后的增量从这份全量的版本继续，而不是接着旧状态续发。
// 历史：RR-20260926-30。

func labelledSubject(id int64, label string) *entity.SubjectSyncState {
	return entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: id, Namespace: "test",
		Packer: entity.SubjectSyncPackFunc{
			Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
				return entity.TakeFrozenSyncPayload(1, []byte("snapshot:"+label)), nil
			},
			Delta: func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
				return entity.TakeFrozenSyncPayload(1, []byte("delta:"+label)), nil
			},
		},
	})
}

func TestReloadedSubjectIsRebound(t *testing.T) {
	for _, via := range []string{"Rebind", "Register"} {
		t.Run(via, func(t *testing.T) {
			transport := newRecordingTransport()
			manager := newTestManager(t, transport, ManagerConfig{})
			evicted := labelledSubject(3001, "phantom")
			if err := manager.Register(evicted); err != nil {
				t.Fatal(err)
			}
			open(t, manager, 1)
			mustSubscribe(t, manager, 1, 3001, entity.SyncProfile{})
			mustFlush(t, manager)
			created := oneFrame(t, transport, 1)
			evicted.MarkDirty(0x1) // 被跳过的效果已经作为增量发给了客户端
			mustFlush(t, manager)
			oneFrame(t, transport, 1)

			evicted.Close() // 驱逐：EntityBase.ClearBase 关闭同步状态
			evicted.MarkDirty(0x2)
			mustFlush(t, manager)
			if frames := transport.take(1); len(frames) != 0 {
				t.Fatalf("a closed subject still sent %d frame(s)", len(frames))
			}
			if err := manager.LastError(); err != nil {
				t.Fatalf("a closed subject counted as a sync failure: %v", err)
			}

			reloaded := labelledSubject(3001, "mongo")
			var err error
			if via == "Rebind" {
				err = manager.Rebind(reloaded)
			} else {
				err = manager.Register(reloaded)
			}
			if err != nil {
				t.Fatalf("%s the reloaded state: %v", via, err)
			}
			mustFlush(t, manager)
			full := oneFrame(t, transport, 1)
			update := full.updates[3001]
			if full.objects[3001] != frame.ObjectUpdate || !update.Full || string(update.Payload.BytesCopy()) != "snapshot:mongo" {
				t.Fatalf("rebind did not resend a full snapshot of the reloaded state: objects=%v update=%+v payload=%q", full.objects, update, update.Payload.BytesCopy())
			}
			if created.wire.Objects[0].Ref != full.wire.Objects[0].Ref {
				t.Fatalf("the object changed reference across the rebind: %v → %v", created.wire.Objects[0].Ref, full.wire.Objects[0].Ref)
			}
			reloaded.MarkDirty(0x4)
			mustFlush(t, manager)
			delta := oneFrame(t, transport, 1).updates[3001]
			if delta.Full || delta.BaseVersion != update.Version || string(delta.Payload.BytesCopy()) != "delta:mongo" {
				t.Fatalf("delta after the rebind does not continue from the full snapshot: full=%+v delta=%+v", update, delta)
			}
		})
	}
}

func TestRebindRefusesToReplaceALiveState(t *testing.T) {
	manager := newTestManager(t, newRecordingTransport(), ManagerConfig{})
	live := labelledSubject(3002, "live")
	if err := manager.Rebind(live); !errors.Is(err, ErrSubjectNotRegistered) {
		t.Fatalf("rebind of an unregistered subject=%v", err)
	}
	if err := manager.Register(live); err != nil {
		t.Fatal(err)
	}
	if err := manager.Rebind(live); err != nil {
		t.Fatalf("rebind to the registered state itself=%v, want nil", err)
	}
	if err := manager.Rebind(labelledSubject(3002, "other")); !errors.Is(err, ErrSubjectRegistered) {
		t.Fatalf("rebind over a live state=%v, want ErrSubjectRegistered", err)
	}
	if err := manager.Register(labelledSubject(3002, "other")); !errors.Is(err, ErrSubjectRegistered) {
		t.Fatalf("register over a live state=%v, want ErrSubjectRegistered", err)
	}
}

// 一个内容状态只喂一个 Manager：脏通知器在状态上只有一份，第二个 Manager 登记会把它换成自己的，第一个 Manager
// 的订阅者从此收不到变化（自身视图与另一个自建 Manager 同时登记 Player 时就是这样）。第二次登记必须被拒绝、
// 点名实体 / 命名空间与已登记它的 Manager，第一个 Manager 照常交付；同一个 Manager 重复登记仍是空操作；
// 在第一个 Manager 注销、退役完成（remove 已交付）之后，第二个 Manager 可以登记并接管变化通知。
func TestASubjectStateFeedsOneManager(t *testing.T) {
	firstTransport, secondTransport := newRecordingTransport(), newRecordingTransport()
	first := newTestManager(t, firstTransport, ManagerConfig{Name: "self_view"})
	second := newTestManager(t, secondTransport, ManagerConfig{Name: "scene"})
	packs := 0
	const id = int64(1501)
	state := testSubject(t, id, &packs)
	if err := first.Register(state); err != nil {
		t.Fatal(err)
	}
	open(t, first, 1)
	mustSubscribe(t, first, 1, id, entity.SyncProfile{})
	mustFlush(t, first)
	if created := oneFrame(t, firstTransport, 1); created.objects[id] != frame.ObjectCreate {
		t.Fatalf("premise: first frame=%v, want a create", created.objects)
	}
	if err := first.Register(state); err != nil {
		t.Fatalf("registering the same state with the same manager again = %v, want a no-op", err)
	}

	err := second.Register(state)
	if !errors.Is(err, ErrSubjectRegisteredElsewhere) {
		t.Fatalf("a second manager registering the state = %v, want ErrSubjectRegisteredElsewhere", err)
	}
	for _, want := range []string{"1501", `"test"`, `"self_view"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
	if second.subject(id) != nil {
		t.Fatal("the refused registration left a subject in the second manager")
	}
	state.MarkDirty(1)
	mustFlush(t, first)
	if frames := firstTransport.take(1); len(frames) != 1 || decodeFrame(t, frames[0]).objects[id] != frame.ObjectUpdate {
		t.Fatalf("the first manager's subscriber got %d frame(s) for a change after the second registration: the second manager took the dirty notifier", len(frames))
	}

	// 注销之后、remove 交付之前仍属于第一个 Manager（它还欠订阅者 remove）。
	if err := first.Unregister(id); err != nil {
		t.Fatal(err)
	}
	if err := second.Register(state); !errors.Is(err, ErrSubjectRegisteredElsewhere) {
		t.Fatalf("register with the second manager while the first still owes a remove = %v, want ErrSubjectRegisteredElsewhere", err)
	}
	mustFlush(t, first)
	if removed := oneFrame(t, firstTransport, 1); len(removed.wire.Objects) != 1 || removed.wire.Objects[0].Operation != frame.ObjectRemove {
		t.Fatalf("retirement frame=%+v, want ObjectRemove", removed.wire.Objects)
	}
	if err := second.Register(state); err != nil {
		t.Fatalf("register with the second manager after the first retired it = %v, want nil", err)
	}
	open(t, second, 2)
	mustSubscribe(t, second, 2, id, entity.SyncProfile{})
	mustFlush(t, second)
	oneFrame(t, secondTransport, 2)
	state.MarkDirty(1)
	mustFlush(t, second)
	if updated := oneFrame(t, secondTransport, 2); updated.objects[id] != frame.ObjectUpdate {
		t.Fatalf("change after the hand-over=%v, want an update from the second manager", updated.objects)
	}
	if err := first.Register(state); !errors.Is(err, ErrSubjectRegisteredElsewhere) || !strings.Contains(err.Error(), `"scene"`) {
		t.Fatalf("the first manager registering it back = %v, want ErrSubjectRegisteredElsewhere naming \"scene\"", err)
	}
	// Close 放开它持有的状态。
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Register(state); err != nil {
		t.Fatalf("register after the holder closed = %v, want nil", err)
	}
}
