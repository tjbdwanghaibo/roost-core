//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/tjbdwanghaibo/roost-core/cache"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
)

// v1.23.0 发版前验证（2026-10-06，记录 docs/bugfix/PRERELEASE-VERIFICATION-2026-10-06.md 第 4 项）：
// Redis Cluster 槽位迁移中（ASK）与迁移后客户端槽位表过期（MOVED）时，Remote 快照 L2 的读、写与带版本删除
// （墓碑 + WAIT）。只在 scripts/mirror-local.sh test-core 起的私有 3 主 3 从 Cluster 上运行，不碰共享隔离环境。
//
// 承诺（redis/driver/replicated.go EvalReplicated 的设计）：
//   - ASK 窗口（源 MIGRATING、目标 IMPORTING，键已搬到目标）：HGET / 写脚本跟随 ASK 落到目标，值正确；
//     墓碑脚本在源上回 ASK（没执行），改经集群客户端普通发送一次，墓碑写在目标上，不 WAIT，计为 skipped；
//     旧版本写不能复活已删除的快照。
//   - 迁移完成、客户端槽位表仍指向源（MOVED）：读写跟随 MOVED；墓碑同样退回普通发送、skipped；
//     槽位表刷新后墓碑 WAIT 打到新主并被它的副本确认（confirmed）。
// 结束时把槽位迁回源节点，删掉用到的键。

func TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone(t *testing.T) {
	env := newTombstoneWaitEnv(t)
	if len(env.cluster) == 0 {
		t.Fatal("the private environment does not export the Redis Cluster")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rcfg := fredis.DefaultConfig("")
	rcfg.ClusterAddrs = env.cluster
	client := redisdriver.NewRedisClient(rcfg)
	t.Cleanup(func() { _ = client.Close() })
	cfg := DefaultConfig()
	cfg.SnapshotL2KeyPrefix = env.prefix
	cfg.SnapshotL2TombstoneWaitTimeout = 500 * time.Millisecond
	store, err := NewSnapshotL2StoreFromConfig(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := staleBackfillKey(t, b2WatermarkKind, 9940)
	redisKey := store.key(key)
	cluster := client.Raw().(*goredis.ClusterClient)
	t.Cleanup(func() { _ = cluster.Del(context.Background(), redisKey).Err() })

	slot, err := cluster.ClusterKeySlot(ctx, redisKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := cluster.MasterForKey(ctx, redisKey)
	if err != nil {
		t.Fatal(err)
	}
	source := rawRedis(t, owner.Options().Addr)
	var target *goredis.Client
	var masters []*goredis.Client
	for _, addr := range env.cluster {
		node := rawRedis(t, addr)
		if role, _ := node.Do(ctx, "role").Slice(); len(role) == 0 || role[0] != "master" {
			continue
		}
		masters = append(masters, node)
		if target == nil && addr != owner.Options().Addr {
			target = node
		}
	}
	if target == nil {
		t.Fatal("no second master to migrate to")
	}
	sourceID, targetID := nodeID(t, source), nodeID(t, target)
	t.Logf("slot %d: %s (%s) -> %s (%s)", slot, source.Options().Addr, sourceID, target.Options().Addr, targetID)
	moveSlot := func(from, to *goredis.Client, fromID, toID string) {
		t.Helper()
		mustDo(t, to, "cluster", "setslot", slot, "importing", fromID)
		mustDo(t, from, "cluster", "setslot", slot, "migrating", toID)
		migrateSlotKeys(t, from, to, slot)
	}
	finishSlot := func(toID string) {
		t.Helper()
		for _, node := range masters {
			mustDo(t, node, "cluster", "setslot", slot, "node", toID)
		}
	}
	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		// 失败时尽力把槽位还给源节点，避免影响同一私有环境里之后的用例：键已经在目标时，先把归属交给目标
		// （只有拥有槽位的节点能把键 MIGRATE 出去），再按正常路径迁回。
		background := context.Background()
		onSource, _ := source.ClusterCountKeysInSlot(background, int(slot)).Result()
		onTarget, _ := target.ClusterCountKeysInSlot(background, int(slot)).Result()
		if onSource == 0 && onTarget > 0 {
			for _, node := range masters {
				_ = node.Do(background, "cluster", "setslot", slot, "node", targetID).Err()
			}
			_ = source.Do(background, "cluster", "setslot", slot, "importing", targetID).Err()
			_ = target.Do(background, "cluster", "setslot", slot, "migrating", sourceID).Err()
			migrateSlotKeys(t, target, source, slot)
		}
		for _, node := range masters {
			_ = node.Do(background, "cluster", "setslot", slot, "node", sourceID).Err()
		}
	})

	if err := store.Set(ctx, staleBackfillEnvelope(key, 1, 1, "v1")); err != nil {
		t.Fatal(err)
	}
	expectSnapshot(t, ctx, store, key, 1)

	// ---- ASK：槽位迁移中，键已经搬到目标。----
	moveSlot(source, target, sourceID, targetID)
	if exists, _ := source.Exists(ctx, redisKey).Result(); exists != 0 {
		t.Fatalf("precondition: %s still on the source after MIGRATE", redisKey)
	}
	if !existsAsking(t, target, redisKey) {
		t.Fatalf("precondition: %s not on the target after MIGRATE", redisKey)
	}
	expectSnapshot(t, ctx, store, key, 1) // HGET 跟随 ASK
	if err := store.Set(ctx, staleBackfillEnvelope(key, 2, 1, "v2")); err != nil {
		t.Fatalf("write during ASK: %v", err)
	}
	expectSnapshot(t, ctx, store, key, 2)
	if onSource, _ := source.Exists(ctx, redisKey).Result(); onSource != 0 {
		t.Fatal("the write during ASK recreated the key on the source instead of following ASK to the target")
	}
	before := store.TombstoneWaitStats()
	if err := store.DeleteAtVersion(ctx, key, 3); err != nil {
		t.Fatalf("tombstone during ASK: %v", err)
	}
	if after := store.TombstoneWaitStats(); after.Skipped != before.Skipped+1 || after.Confirmed != before.Confirmed {
		t.Fatalf("tombstone during ASK: wait stats %+v -> %+v, want one skipped (redirected, no WAIT)", before, after)
	}
	expectNoSnapshot(t, ctx, store, key)
	if err := store.Set(ctx, staleBackfillEnvelope(key, 2, 1, "v2-late")); err != nil && !errors.Is(err, cache.ErrStaleWrite) {
		t.Fatalf("a late older write during ASK: %v", err)
	}
	expectNoSnapshot(t, ctx, store, key) // 旧版本写不能复活

	// ---- MOVED：迁移完成，客户端的槽位表仍指向源。----
	finishSlot(targetID)
	stale := false
	if master, err := cluster.MasterForKey(ctx, redisKey); err == nil && master.Options().Addr == source.Options().Addr {
		stale = true
	}
	before = store.TombstoneWaitStats()
	if err := store.DeleteAtVersion(ctx, key, 4); err != nil {
		t.Fatalf("tombstone after the migration (MOVED): %v", err)
	}
	after := store.TombstoneWaitStats()
	if stale && (after.Skipped != before.Skipped+1 || after.Confirmed != before.Confirmed) {
		t.Fatalf("tombstone through a stale slot table: wait stats %+v -> %+v, want one skipped (MOVED, no WAIT)", before, after)
	}
	t.Logf("MOVED phase: client slot table stale=%v, tombstone wait stats %+v -> %+v", stale, before, after)
	expectNoSnapshot(t, ctx, store, key)
	if err := store.Set(ctx, staleBackfillEnvelope(key, 5, 1, "v5")); err != nil {
		t.Fatalf("write after the migration (MOVED): %v", err)
	}
	expectSnapshot(t, ctx, store, key, 5)
	// 槽位表刷新（MOVED 触发的异步刷新；环境就绪的等待，不是被测行为）。
	deadline := time.Now().Add(10 * time.Second)
	for {
		master, err := cluster.MasterForKey(ctx, redisKey)
		if err == nil && master.Options().Addr == target.Options().Addr {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cluster client did not learn the new owner of slot %d within 10s", slot)
		}
		cluster.ReloadState(ctx)
		time.Sleep(50 * time.Millisecond)
	}
	waitReplicaOf(t, ctx, target)
	before = store.TombstoneWaitStats()
	if err := store.DeleteAtVersion(ctx, key, 6); err != nil {
		t.Fatalf("tombstone on the new owner: %v", err)
	}
	if after := store.TombstoneWaitStats(); after.Confirmed != before.Confirmed+1 {
		t.Fatalf("tombstone on the new owner: wait stats %+v -> %+v, want one confirmed by its replica", before, after)
	}
	expectNoSnapshot(t, ctx, store, key)

	// 槽位迁回源（同样经过 ASK / MOVED），结束后客户端仍能读写。
	moveSlot(target, source, targetID, sourceID)
	finishSlot(sourceID)
	restored = true
	if err := store.Set(ctx, staleBackfillEnvelope(key, 7, 1, "v7")); err != nil {
		t.Fatalf("write after moving the slot back: %v", err)
	}
	expectSnapshot(t, ctx, store, key, 7)
	t.Logf("final tombstone wait stats %+v", store.TombstoneWaitStats())
}

func expectSnapshot(t *testing.T, ctx context.Context, store *remoteSnapshotL2Store, key entity.RemoteSnapshotKey, version uint64) {
	t.Helper()
	got, found, err := store.Get(ctx, key)
	if err != nil || !found || got.StateVersion != version {
		t.Fatalf("Get = version %d found %v err %v, want version %d", got.StateVersion, found, err, version)
	}
}

func expectNoSnapshot(t *testing.T, ctx context.Context, store *remoteSnapshotL2Store, key entity.RemoteSnapshotKey) {
	t.Helper()
	got, found, err := store.Get(ctx, key)
	if err != nil || found {
		t.Fatalf("Get after the tombstone = version %d found %v err %v, want not found", got.StateVersion, found, err)
	}
}

func nodeID(t *testing.T, node *goredis.Client) string {
	t.Helper()
	id, err := node.Do(context.Background(), "cluster", "myid").Text()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustDo(t *testing.T, node *goredis.Client, args ...any) {
	t.Helper()
	if err := node.Do(context.Background(), args...).Err(); err != nil {
		t.Fatalf("%s %v: %v", node.Options().Addr, args, err)
	}
}

// migrateSlotKeys 把 from 上这个槽位的全部键 MIGRATE 到 to（槽位里可能有本用例之外的键，一并搬）。
func migrateSlotKeys(t *testing.T, from, to *goredis.Client, slot int64) {
	t.Helper()
	host, port, _ := strings.Cut(to.Options().Addr, ":")
	for {
		keys, err := from.ClusterGetKeysInSlot(context.Background(), int(slot), 100).Result()
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) == 0 {
			return
		}
		args := []any{"migrate", host, port, "", 0, 5000, "keys"}
		for _, k := range keys {
			args = append(args, k)
		}
		mustDo(t, from, args...)
	}
}

// existsAsking 在 IMPORTING 节点上查键：同一连接先发 ASKING，否则节点回 ASK / MOVED。
func existsAsking(t *testing.T, node *goredis.Client, key string) bool {
	t.Helper()
	conn := node.Conn()
	defer func() { _ = conn.Close() }()
	ctx := context.Background()
	if err := conn.Process(ctx, goredis.NewStatusCmd(ctx, "asking")); err != nil {
		t.Fatal(err)
	}
	n, err := conn.Exists(ctx, key).Result()
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// waitReplicaOf 等 master 至少有一个 online 的副本（环境就绪的等待；ROLE 只列 online 的副本）。
func waitReplicaOf(t *testing.T, ctx context.Context, master *goredis.Client) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if strings.Contains(replicationField(t, master, "slave0"), "state=online") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has no connected replica", master.Options().Addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
