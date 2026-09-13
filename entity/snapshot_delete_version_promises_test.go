package entity

import (
	"context"
	"testing"
	"time"
)

// U-0187 · C8 · RR-20260913-01：快照删除必须带版本，并且与 Publish 走同一条排序边界。
// 旧的 Delete(key) 是无版本失效原语：迟到的 delete v2 会清掉已到的 full v3；先到的
// delete v2 之后迟到的 full v1 会把已删的键复活。DeleteAtVersion 在 publish 分片锁下
// 比较 L1 版本（比 delete 新的快照保留），并留下带 TTL 的墓碑挡住 <= 该版本的写入；
// 更新的快照进来时清掉墓碑。

func deleteVersionKey(t *testing.T, kind EntityKind, seq int64) RemoteSnapshotKey {
	t.Helper()
	MustRegisterEntityKindDefs(EntityKindDef{Kind: kind, Category: 1, RemotePolicy: RemotePolicyManaged})
	id, err := BuildEntityID(seq, kind)
	if err != nil {
		t.Fatal(err)
	}
	return RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
}

func deleteVersionEnvelope(key RemoteSnapshotKey, version uint64, body string) RemoteSnapshotEnvelope {
	return RemoteSnapshotEnvelope{
		Key: key, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true,
		Payload: CopyFrozenRemoteSnapshotPayload([]byte(body)),
	}
}

func TestRemoteSnapshotDeleteAtVersionPromiseKeepsNewerSnapshot(t *testing.T) {
	key := deleteVersionKey(t, 236, 9101)
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 8, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, nil, nil)
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 3, "v3")); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAtVersion(context.Background(), key, 2); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(context.Background(), key, RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("old delete removed v3: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
}

func TestRemoteSnapshotDeleteAtVersionPromiseFencesOlderSnapshot(t *testing.T) {
	key := deleteVersionKey(t, 237, 9102)
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 8, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4}, nil, nil)
	if err := c.DeleteAtVersion(context.Background(), key, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Get(context.Background(), key, RemoteReadCached, 0)
	if err != nil || ok {
		t.Fatalf("deleted snapshot resurrected: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
	// 同版本的迟到写也在墓碑之内。
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 2, "v2")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(context.Background(), key, RemoteReadCached, 0); ok {
		t.Fatal("snapshot at the delete's own version resurrected the key")
	}
	// 比墓碑新的快照让键重新活过来，并清掉墓碑。
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 3, "v3")); err != nil {
		t.Fatal(err)
	}
	got, ok, err = c.Get(context.Background(), key, RemoteReadCached, 0)
	if err != nil || !ok || got.StateVersion != 3 {
		t.Fatalf("newer snapshot did not revive the key: found=%v version=%d err=%v", ok, got.StateVersion, err)
	}
	c.tombMu.Lock()
	_, still := c.tombstones[key]
	c.tombMu.Unlock()
	if still {
		t.Fatal("tombstone survived a newer snapshot")
	}
}

// 边界：墓碑只活 TombstoneTTL。这是有意的——比可重放窗口还晚的旧快照不在协议保护范围里。
func TestRemoteSnapshotDeleteAtVersionPromiseTombstoneExpires(t *testing.T) {
	key := deleteVersionKey(t, 238, 9103)
	c := NewRemoteSnapshotCache(RemoteSnapshotCacheConfig{Shards: 1, MaxEntries: 8, MaxBytes: 4096, TTL: time.Minute, MaxWaiters: 4, TombstoneTTL: time.Millisecond}, nil, nil)
	if err := c.DeleteAtVersion(context.Background(), key, 2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := c.Publish(context.Background(), deleteVersionEnvelope(key, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(context.Background(), key, RemoteReadCached, 0); !ok {
		t.Fatal("an expired tombstone must stop fencing")
	}
}
