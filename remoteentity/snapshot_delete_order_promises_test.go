package remoteentity

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// U-0187 · C8 · RR-20260913-01：Remote 复制侧的删除要按信封版本应用。
// DeleteRemoteSnapshot 把提交版本放在 env.Version 里发出去，但接收侧原来直接
// cache.Delete(key)，不比较版本也不留水位——同一 key 的 delete 与 upsert 乱序投递时，
// 迟到的 delete v2 清掉已到的 full v3，或先到的 delete v2 之后迟到的 full v1 复活已删键。
// 这里用真实 remoteSyncer 生成 delete 信封、真实 SnapshotReplicaStore 应用，只重排投递顺序。

func deleteOrderSetup(t *testing.T, kind entity.EntityKind, seq int64) (*Manager, SnapshotReplicaStore, entity.RemoteSnapshotKey) {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(seq, kind)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	return manager, SnapshotReplicaStore{mgr: manager}, entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

func deleteOrderUpsert(t *testing.T, key entity.RemoteSnapshotKey, version uint64) mirror.Envelope {
	t.Helper()
	record := entity.RemoteSnapshotRecord{
		Key: key, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Full: true, Data: []byte("state"),
	}
	record.Checksum = entity.RemoteSnapshotChecksum(record.Data)
	raw, err := json.Marshal(remoteSnapshotWire{Key: key, Update: record})
	if err != nil {
		t.Fatal(err)
	}
	return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
}

func deleteOrderDelete(t *testing.T, key entity.RemoteSnapshotKey, version uint64) mirror.Envelope {
	t.Helper()
	raw, err := json.Marshal(remoteSnapshotWire{Delete: true, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	// 与 remoteSyncer.DeleteRemoteSnapshot 发出的信封一致：版本在 env.Version。
	return mirror.Envelope{Key: remoteSnapshotReplicaKey(key), Version: int64(version), Op: mirror.OpUpsert, Payload: raw}
}

func TestApplyReplicaPromiseDelayedDeleteKeepsNewerSnapshot(t *testing.T) {
	manager, store, key := deleteOrderSetup(t, 244, 9341)
	ctx := context.Background()
	if err := store.ApplyReplica(ctx, deleteOrderUpsert(t, key, 3)); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyReplica(ctx, deleteOrderDelete(t, key, 2)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := manager.remote.cache.Get(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("old delete removed v3: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
}

func TestApplyReplicaPromiseDeleteFencesDelayedOlderSnapshot(t *testing.T) {
	manager, store, key := deleteOrderSetup(t, 245, 9342)
	ctx := context.Background()
	if err := store.ApplyReplica(ctx, deleteOrderDelete(t, key, 2)); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyReplica(ctx, deleteOrderUpsert(t, key, 1)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := manager.remote.cache.Get(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || ok {
		t.Fatalf("deleted snapshot resurrected: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
	// 顺序对照：按序 upsert v1 → delete v2 → upsert v3 分别得到删除与 v3。
	if err := store.ApplyReplica(ctx, deleteOrderUpsert(t, key, 3)); err != nil {
		t.Fatal(err)
	}
	got, ok, err = manager.remote.cache.Get(ctx, key, entity.RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("ordered newer snapshot lost: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
}
