package remoteentity

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0187 复核补修 · C8 · RR-20260913-01 残余:L2 的删除也要带版本,比较与 DEL 在一个脚本里。
// 第一版 DeleteAtVersion 只比较本机 L1,L1 为空时对共享的 L2 无条件 Del —— 另一节点刚发布的
// v3 被一条旧的 delete v2 清掉。版本按精确十进制字符串比较,与 CAS 脚本同一套规则,2^53 以上
// 的相邻版本不能塌成同一个值。新 API,修前只是不编译;行为上的红是 entity 层的
// TestColdL1DeletePreservesNewerL2(修前 `old delete removed newer L2: found=false version=0`)。
func TestRemoteSnapshotL2DeleteAtVersionKeepsNewerSnapshot(t *testing.T) {
	const kind entity.EntityKind = 249
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(1902, kind)
	if err != nil {
		t.Fatal(err)
	}
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}
	store := NewSnapshotL2Store(newSnapshotRedisFake(), time.Minute)
	ctx := context.Background()
	put := func(version uint64) {
		t.Helper()
		if err := store.Set(ctx, entity.RemoteSnapshotEnvelope{Key: key, StateVersion: version, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Full: true, Payload: entity.CopyFrozenRemoteSnapshotPayload([]byte("s"))}); err != nil {
			t.Fatal(err)
		}
	}
	put(3)
	if err := store.DeleteAtVersion(ctx, key, 2); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := store.Get(ctx, key); !ok || got.StateVersion != 3 {
		t.Fatalf("old delete removed newer L2: found=%v version=%d", ok, got.StateVersion)
	}
	if err := store.DeleteAtVersion(ctx, key, 3); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.Get(ctx, key); ok {
		t.Fatal("delete at the stored version must remove the key")
	}
	// 精度:2^53+1 存着,delete 2^53 不能删(float64 下两者相等)。
	put(1<<53 + 1)
	if err := store.DeleteAtVersion(ctx, key, 1<<53); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.Get(ctx, key); !ok {
		t.Fatal("delete at 2^53 removed the snapshot at 2^53+1: version compared as float")
	}
	// 不存在的键:删除是空操作,不报错。
	if err := store.DeleteAtVersion(ctx, entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 2}, 9); err != nil {
		t.Fatal(err)
	}
}
