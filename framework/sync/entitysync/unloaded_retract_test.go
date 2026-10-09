package entitysync

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// RR-20260926-59 与 RR-55 / RR-35 / RR-30 的统一：实体被仅内存卸载后重载不了，RetractUnloadedSubject 退回
// remove（与 Unregister 同一退役）。退役完成前实体又被加载出来（业务访问、kit 的 OnEntityLoaded）时，
// Rebind / Register 把新状态排在退役完成之后登记（RegisterAfterRetirement 的同一机制），不返回
// ErrSubjectRetiring；remove-before-create 不变：先交付 remove，同一步以新状态重新登记。
// 业务自己 Unregister 的退役不受影响（仍返回 ErrSubjectRetiring，业务按 RR-55 走 RegisterAfterRetirement）。

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

// RR-20260926-72：RR-59 卸载退役进行中，业务的 RegisterAfterRetirement 被 rebind 的排队分支静默排在退役之后，
// 旧实现却返回 queued=false、err=nil（“已登记”），done 永不调用。契约（RR-55）：还不能登记的登记返回
// queued=true，done 在 remove 交付后报告结果，恰好一次。
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

// RR-20260926-72：卸载退役中 rebind 路径（kit 的 OnEntityLoaded）与业务 RegisterAfterRetirement 共用一个排队位，
// 后到的替换先到的；被替换的业务登记由 done 收到 ErrRegistrationCancelled，替换者在 remove 之后登记。
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
