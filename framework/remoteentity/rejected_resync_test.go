package remoteentity

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// RR-20260926-59：Durability 1 在 Commit 返回时推测性确认 Sync，投影器拒绝前订阅者可能已收到被拒绝的内容。
// RR-39 卸载只关闭同步状态、不发修正帧，订阅者停在从未成为权威的 a=rejected 上（REPRO-2026-09-26-05 §8）。
// 修复后：卸载时 subject 仍有订阅者，框架在快池之外从权威重载实例并 Rebind，订阅者下一帧收到同一对象的
// 权威全量（ObjectUpdate + Full）；无订阅者时不主动重载。

type resyncSubscriber struct {
	sync   *entitysync.Manager
	frames chan []byte
	ref    frame.ObjectRef
	// decided 收到卸载时“是否为订阅者重载”的判定（Unload 登记重载前查询的结果），让无订阅者用例不必猜时序。
	decided chan bool
}

// observedSync 转发给 entitysync.Manager，并把 SubjectAwaitsReload 的结果交给测试。
type observedSync struct {
	*entitysync.Manager
	decided chan bool
}

func (o observedSync) SubjectAwaitsReload(id int64) bool {
	awaits := o.Manager.SubjectAwaitsReload(id)
	select {
	case o.decided <- awaits:
	default:
	}
	return awaits
}

func enableFullDocSync(e *reloadableFullDocEntity) {
	packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		return entity.CopyFrozenSyncPayload(1, []byte("a="+e.a)), nil
	}
	e.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: e.ID(), Namespace: "test", Packer: entity.SubjectSyncPackFunc{
		Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) },
	}})
}

// newResyncFixture：正式 ManagerAccess 作 Remote loader，loader 按权威构造实例并在发布前启用同步（与生成工厂相同）；
// entitysync.Manager 经 ManagerAccess.ConfigureUnloadResync 接上卸载后重载（kit 在 Nest Mod 启动时做同样的接线）。
func newResyncFixture(t *testing.T, rawID int64, subscribe bool) (reloadFixture, *reloadableFullDocEntity, *resyncSubscriber) {
	t.Helper()
	f, live := newReloadFixture(t, rawID)
	f.loader.prepare = enableFullDocSync
	enableFullDocSync(live)
	sub := &resyncSubscriber{frames: make(chan []byte, 16), decided: make(chan bool, 16)}
	var err error
	sub.sync, err = entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		sub.frames <- append([]byte(nil), p...)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.sync.Close(context.Background()) })
	stop, err := f.access.ConfigureUnloadResync(observedSync{Manager: sub.sync, decided: sub.decided}, entity.UnloadResyncConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	if err = sub.sync.Register(live.Sync()); err != nil {
		t.Fatal(err)
	}
	if !subscribe {
		return f, live, sub
	}
	if err = sub.sync.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err = sub.sync.Subscribe(1, live.ID(), entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	created := sub.next(t, time.Second, "the initial create")
	if len(created.Objects) != 1 || created.Objects[0].Operation != frame.ObjectCreate {
		t.Fatalf("initial frame=%+v", created.Objects)
	}
	sub.ref = created.Objects[0].Ref
	return f, live, sub
}

// next 驱动 Flush 直到收到一帧（重载在后台 worker 上完成，这里只等事件发生，不靠固定睡眠判定结果）。
func (sub *resyncSubscriber) next(t *testing.T, timeout time.Duration, what string) frame.Frame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		select {
		case raw := <-sub.frames:
			decoded, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			return decoded
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no frame within %v: %s", timeout, what)
		}
		if err := sub.sync.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func fullPayload(t *testing.T, object frame.ObjectDelta) (string, bool) {
	t.Helper()
	if len(object.Components) != 1 {
		t.Fatalf("object carries %d components", len(object.Components))
	}
	update, err := entitysync.DecodeSubjectUpdate(object.Components[0].Data, 0)
	if err != nil {
		t.Fatal(err)
	}
	return string(update.Payload.BytesCopy()), update.Full
}

func TestRejectedAsyncWriteResyncsSubscribersFromAuthority(t *testing.T) {
	f, live, sub := newResyncFixture(t, 1961, true)
	tx := remoteTestTxID(0xD1)
	batch := f.prepareRejected(t, live, tx, 1) // 内存 a=rejected
	live.MarkSyncDirty(1)
	if _, err := batch.Commit(context.Background()); err != nil { // Durability 1：推测性成功
		t.Fatal(err)
	}
	speculative := sub.next(t, time.Second, "the speculative Durability 1 frame")
	if payload, _ := fullPayload(t, speculative.Objects[0]); payload != "a=rejected" {
		t.Fatalf("premise: speculative frame=%q, want a=rejected", payload)
	}
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live.destroyed:
	case <-time.After(3 * time.Second):
		t.Fatal("rejected instance was not unloaded")
	}
	if doc := f.storedDocument(t); doc != "" {
		t.Fatalf("authority doc=%q, want none", doc)
	}
	correction := sub.next(t, 3*time.Second, "authority rejected the transaction and the instance was unloaded, but the subscriber still holds a=rejected (no authoritative full, no remove)")
	if len(correction.Objects) != 1 || correction.Objects[0].Operation != frame.ObjectUpdate || correction.Objects[0].Ref != sub.ref {
		t.Fatalf("correction frame=%+v, want one ObjectUpdate of the held object %+v", correction.Objects, sub.ref)
	}
	if payload, full := fullPayload(t, correction.Objects[0]); payload != "a=" || !full {
		t.Fatalf("subscriber got %q (full=%v) after the rejection, want the authority full state %q", payload, full, "a=")
	}
	fresh := f.access.LookupLocalRemoteEntity(f.id, entity.EntityKindNone)
	if fresh == nil || fresh == entity.IThreadSafeRemoteEntity(live) {
		t.Fatal("authority instance is not resident after the resync")
	}
	if stats := f.access.UnloadResyncStats(); stats.Reloaded != 1 || stats.Retracted != 0 {
		t.Fatalf("resync stats=%+v", stats)
	}
}

func TestRejectedWriteWithoutSubscribersIsNotReloaded(t *testing.T) {
	f, live, sub := newResyncFixture(t, 1962, false)
	tx := remoteTestTxID(0xD2)
	batch := f.prepareRejected(t, live, tx, 1)
	if _, err := batch.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mgr.RejectRemoteTransaction(tx, "lease expired")
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live.destroyed:
	case <-time.After(3 * time.Second):
		t.Fatal("rejected instance was not unloaded")
	}
	select {
	case awaits := <-sub.decided:
		if awaits {
			t.Fatal("unload of an entity without subscribers decided to reload it")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unload never consulted the sync side")
	}
	loads := f.loader.loads.Load()
	if stats := f.access.UnloadResyncStats(); stats.Scheduled != 0 {
		t.Fatalf("unload without subscribers scheduled a reload: %+v", stats)
	}
	if f.access.LookupLocalRemoteEntity(f.id, entity.EntityKindNone) != nil || f.loader.loads.Load() != loads {
		t.Fatal("entity without subscribers was reloaded proactively")
	}
}
