package entitysync

import (
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
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
