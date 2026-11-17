//go:build integration

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

var registerRedisOwnershipKind = sync.OnceFunc(func() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 237, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
})

func TestRealRemoteOwnershipClaimAndTransfer(t *testing.T) {
	firstRedis, secondRedis := realRemoteRedis(t), realRemoteRedis(t)
	key := remoteRedisKey(t, firstRedis)
	registerRedisOwnershipKind()
	const kind entity.EntityKind = 237
	id := testRemoteFullIDWithKind(7211, 1, kind)
	cfg := DefaultConfig()
	cfg.LockKey = key
	cfg.LockTTL = time.Second
	cfg.OpTimeout = time.Second
	lockKey := fmt.Sprintf("lock:%s:%d", key, id)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := firstRedis.Del(ctx, lockKey, lockKey+":fence"); err != nil {
			t.Errorf("cleanup lock: %v", err)
		}
	})
	managers := []*Manager{NewManager(NewVersionedLockFactory(firstRedis), cfg, 1000), NewManager(NewVersionedLockFactory(secondRedis), cfg, 2000)}
	managers[0].SetOwnershipStore(NewRedisMarker(firstRedis, key))
	managers[1].SetOwnershipStore(NewRedisMarker(secondRedis, key))
	for _, m := range managers {
		backend := newRemoteTestLoader()
		backend.add(newTestRemoteEntity(7211, 1, kind))
		m.SetBackend(backend)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type claim struct {
		index int
		err   error
	}
	results := make(chan claim, 2)
	for i, m := range managers {
		go func() { _, err := m.ClaimRemoteOwnership(ctx, id); results <- claim{i, err} }()
	}
	winner := -1
	for range 2 {
		result := <-results
		if result.err == nil {
			if winner >= 0 {
				t.Fatal("two owners claimed")
			}
			winner = result.index
		} else if !errors.Is(result.err, entity.ErrRemoteFenced) {
			t.Fatal(result.err)
		}
	}
	if winner < 0 {
		t.Fatal("no owner claimed")
	}
	old, next := managers[winner], managers[1-winner]
	shared, err := old.EnterRemoteSharedMode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	transferred, err := old.TransferRemoteOwnership(ctx, id, next.localSid)
	if err != nil {
		t.Fatal(err)
	}
	if !transferred.Shared || transferred.OwnerSid != next.localSid || transferred.MarkerEpoch <= shared.MarkerEpoch || transferred.RouteEpoch <= shared.RouteEpoch {
		t.Fatalf("shared=%+v transferred=%+v", shared, transferred)
	}
	if _, err := old.LeaveRemoteSharedMode(ctx, id); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("old owner still writes ownership: %v", err)
	}
	local, err := next.LeaveRemoteSharedMode(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if local.Shared || local.MarkerEpoch <= transferred.MarkerEpoch {
		t.Fatalf("local=%+v", local)
	}
	actual, found, err := old.GetRemoteOwnership(ctx, id)
	if err != nil || !found || actual != local {
		t.Fatalf("old reader sees=%+v found=%v err=%v", actual, found, err)
	}
}

// 共享 kind（ownership=shared）在真实 Mongo 副本集与真实 Redis 版本锁上：两个进程（各自一套 Manager / 正式 Nest）同时
// 首写同一批还没有所有权记录的实体，每笔都成功（没有 fence），记录一条插入就是共享的（代际 1），两边的修改都在权威里；
// 之后第三个进程（同 sid 重启的 A）冷加载照常读写。修前（没有共享认领）后到的一方一律 writer fenced。
func TestRealSharedKindFirstWritesFromTwoProcesses(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" || uri == "" {
		t.Skip("set ROOST_DATAENGINE_IT=1 with isolated MongoDB and Redis")
	}
	redis := realRemoteRedis(t)
	lockKey := remoteRedisKey(t, redis)
	registerSharedKindForTest()
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	database := "shared_kind_it_" + strings.ReplaceAll(generateToken(), "-", "")[:12]
	const entities = 12
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for raw := int64(2400); raw < 2400+entities; raw++ {
			id := testRemoteFullIDWithKind(raw, 1, sharedKindForTest)
			_, _ = redis.Del(ctx, fmt.Sprintf("lock:%s:%d", lockKey, id), fmt.Sprintf("lock:%s:%d:fence", lockKey, id))
		}
		_ = client.Database(database).Drop(ctx)
		_ = client.Close(ctx)
	})
	if err := NewMongoCommitter(client, database, 1000, 0).EnsureRemoteStorage(ctx); err != nil {
		t.Fatal(err)
	}
	for raw := int64(2400); raw < 2400+entities; raw++ {
		id := testRemoteFullIDWithKind(raw, 1, sharedKindForTest)
		setup := writerNodesSetup{db: client, database: database, redis: redis, kind: sharedKindForTest,
			configure: func(cfg *Config) { cfg.LockKey, cfg.LockTTL, cfg.OpTimeout = lockKey, 2*time.Second, 5*time.Second }}
		writers := newWriterNodes(t, setup, raw, nil, 1000, 1001, 1000)
		a, b, restarted := startExclusiveNest(t, writers[0]), startExclusiveNest(t, writers[1]), startExclusiveNest(t, writers[2])
		for _, n := range []exclusiveNestNode{a, b} {
			if _, err := n.loader.LoadEntity(ctx, id, sharedKindForTest); err != nil {
				t.Fatal(err)
			}
		}
		errs := make(chan error, 2)
		start := make(chan struct{})
		go func() { <-start; errs <- a.writeNest(ctx, id, "A", "") }()
		go func() { <-start; errs <- b.writeNest(ctx, id, "", "B") }()
		close(start)
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("entity %d: concurrent first write: %v", raw, err)
			}
		}
		doc, ok := readMeta(t, a.sharedWriterNode, id)
		if !ok || !doc.Shared || doc.Marker != 1 || doc.Version != 2 {
			t.Fatalf("entity %d: ownership record %+v found=%v; want shared at epoch 1 with both commits (version 2)", raw, doc, ok)
		}
		if got := restarted.read(t, ctx, id); got != "a=A;b=B@2" {
			t.Fatalf("entity %d: restarted A read=%q", raw, got)
		}
		if err := restarted.writeNest(ctx, id, "A2", ""); err != nil {
			t.Fatalf("entity %d: restarted A write: %v", raw, err)
		}
		if got := b.read(t, ctx, id); got != "a=A2;b=B@3" {
			t.Fatalf("entity %d: B read=%q", raw, got)
		}
	}
}
