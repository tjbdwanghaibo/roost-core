// 缓存基础：本地存储版本与 LRU、分层读穿透、副本同步、Redis 原始存储，以及各存储的入参守卫。

package cache

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
)

type testItem struct {
	ID      int64
	Version int64
	Data    string
}

func testItemConfig() StoreConfig[int64, testItem] {
	return StoreConfig[int64, testItem]{
		KeyOf:       func(item testItem) int64 { return item.ID },
		Stale:       VersionStale(func(item testItem) int64 { return item.Version }),
		ValidateKey: func(id int64) bool { return id != 0 },
	}
}

func TestLocalStoreRejectsStaleVersion(t *testing.T) {
	store := NewLocalStore[int64, testItem](testItemConfig())
	ctx := context.Background()
	if err := store.Set(ctx, testItem{ID: 1, Version: 2, Data: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, testItem{ID: 1, Version: 1, Data: "old"}); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("RR-20261004-NC-15: stale write err=%v, want ErrStaleWrite", err)
	}
	got, ok, err := store.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Data != "new" {
		t.Fatalf("store value = %+v ok=%v", got, ok)
	}
}

func TestLocalStoreEvictsLeastRecentlyUsedEntry(t *testing.T) {
	ctx := context.Background()
	store := NewLocalStore[int64, testItem](
		testItemConfig(),
		WithLocalMaxEntries[int64, testItem](2),
	)
	if err := store.Set(ctx, testItem{ID: 1, Version: 1, Data: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, testItem{ID: 2, Version: 1, Data: "two"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, 1); err != nil || !ok {
		t.Fatalf("Get(1) ok=%v err=%v", ok, err)
	}
	if err := store.Set(ctx, testItem{ID: 3, Version: 1, Data: "three"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get(ctx, 2); err != nil || ok {
		t.Fatalf("Get(2) ok=%v err=%v, want evicted", ok, err)
	}
	if got, ok, err := store.Get(ctx, 1); err != nil || !ok || got.Data != "one" {
		t.Fatalf("Get(1) = %+v ok=%v err=%v", got, ok, err)
	}
	if got, ok, err := store.Get(ctx, 3); err != nil || !ok || got.Data != "three" {
		t.Fatalf("Get(3) = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestLayeredStoreReadsThroughAfterTTL(t *testing.T) {
	ctx := context.Background()
	local := NewLocalStore[int64, testItem](testItemConfig())
	remote := NewLocalStore[int64, testItem](testItemConfig())
	store := NewLayeredStore[int64, testItem](local, remote, time.Nanosecond, testItemConfig())

	if err := remote.Set(ctx, testItem{ID: 1, Version: 1, Data: "first"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(ctx, 1)
	if err != nil || !ok || got.Data != "first" {
		t.Fatalf("first read = %+v ok=%v err=%v", got, ok, err)
	}
	if err := remote.Set(ctx, testItem{ID: 1, Version: 2, Data: "second"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	got, ok, err = store.Get(ctx, 1)
	if err != nil || !ok || got.Data != "second" {
		t.Fatalf("second read = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestReplicaSyncerAppliesUpdate(t *testing.T) {
	bus := newFakeSyncBus()
	store := NewReplicaLocalStore[int64, testItem](testItemConfig(), func(v testItem) int64 { return v.Version }, 10000)
	syncer := NewReplicaSyncer[int64, testItem](bus, ReplicaConfig[int64, testItem]{
		Store:       store,
		Topic:       "test",
		KeyOf:       func(item testItem) int64 { return item.ID },
		VersionOf:   func(item testItem) int64 { return item.Version },
		DeleteKeyOf: func(id int64) int64 { return id },
	})
	if err := syncer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syncer.Stop(context.Background()) }()

	if err := syncer.Publish(context.Background(), testItem{ID: 7, Version: 3, Data: "payload"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(context.Background(), 7)
	if err != nil || !ok || got.Data != "payload" {
		t.Fatalf("replica value = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestRedisRawJSONStoreUsesRedisString(t *testing.T) {
	ctx := context.Background()
	redis := newRefHMapFakeRedis()
	store := NewRedisRawJSONStore[int64, testItem](redis, time.Hour, func(id int64) string {
		return "raw:item:" + strconv.FormatInt(id, 10)
	}, testItemConfig())

	if err := store.Set(ctx, testItem{ID: 7, Version: 2, Data: "payload"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok := redis.hashes["raw:item:7"]; ok {
		t.Fatalf("raw mode should not create hash data: %+v", redis.hashes["raw:item:7"])
	}
	got, ok, err := store.Get(ctx, 7)
	if err != nil || !ok || got.Data != "payload" {
		t.Fatalf("Get = %+v ok=%v err=%v", got, ok, err)
	}
	if redis.expires["raw:item:7"] != time.Hour {
		t.Fatalf("ttl = %s, want %s", redis.expires["raw:item:7"], time.Hour)
	}
}

func TestRedisRawSortedSetStoreUsesZSet(t *testing.T) {
	ctx := context.Background()
	redis := newRefHMapFakeRedis()
	store := NewRedisRawSortedSetStore[int64](redis, "raw:rank", time.Hour)

	if err := store.SetScore(ctx, 1001, 99); err != nil {
		t.Fatalf("SetScore: %v", err)
	}
	score, ok, err := store.Score(ctx, 1001)
	if err != nil || !ok || score != 99 {
		t.Fatalf("Score = %v ok=%v err=%v", score, ok, err)
	}
	if redis.expires["raw:rank"] != time.Hour {
		t.Fatalf("ttl = %s, want %s", redis.expires["raw:rank"], time.Hour)
	}
}

type fakeSyncBus struct {
	handlers map[string][]*fsyncbus.Subscription
}

func newFakeSyncBus() *fakeSyncBus {
	return &fakeSyncBus{handlers: make(map[string][]*fsyncbus.Subscription)}
}

func (b *fakeSyncBus) Publish(msg *fsyncbus.SyncMsg) error {
	if msg == nil {
		return nil
	}
	for _, sub := range b.handlers[msg.Topic] {
		if err := sub.Deliver(context.Background(), msg); err != nil && !errors.Is(err, fsyncbus.ErrUnsubscribed) {
			return err
		}
	}
	return nil
}

func (b *fakeSyncBus) Subscribe(topic string, handler fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	var sub *fsyncbus.Subscription
	sub = fsyncbus.NewSubscription(topic, handler, func() {
		b.handlers[topic] = slices.DeleteFunc(b.handlers[topic], func(s *fsyncbus.Subscription) bool { return s == sub })
	})
	b.handlers[topic] = append(b.handlers[topic], sub)
	return sub, nil
}

var _ fsyncbus.ISyncBus = (*fakeSyncBus)(nil)

// 分层读：远端失败原样上抛、远端未命中不回填零值也不当命中；读穿透：L1 出错就是出错（不去 load）、loader 未命中 / 出错原样返回且
// 不写 L2 / L1、Set 在 L2 失败且未设 IgnoreRemoteError 时报错且不写 L1；Redis 存储：无 key 函数拒绝写、hash 键或字段为空拒绝写
// （都在触达 Redis 之前）；ref-hmap 的 Stale 判定拒绝旧版本且不改写已存值。

func TestLayeredGetPropagatesRemoteErrorsAndDoesNotBackfillMisses(t *testing.T) {
	ctx := context.Background()
	cfg := StoreConfig[int, string]{KeyOf: func(v string) int { return len(v) }}
	boom := errors.New("redis unreachable")
	local := NewLocalStore[int, string](cfg)
	remote := &flakyRemote{getErr: boom, values: map[int]string{}}
	store := NewLayeredStore[int, string](local, remote, time.Minute, cfg)
	if _, ok, err := store.Get(ctx, 3); !errors.Is(err, boom) || ok {
		t.Fatalf("layered Get with a failing remote = (ok=%v, %v), want the remote error", ok, err)
	}
	remote.getErr = nil
	if v, ok, err := store.Get(ctx, 3); err != nil || ok || v != "" {
		t.Fatalf("layered Get on a remote miss = (%q, %v, %v), want a clean miss", v, ok, err)
	}
	if _, ok, _ := local.Get(ctx, 0); ok {
		t.Fatal("a remote miss backfilled a zero value into L1")
	}
	if _, ok, _ := local.Get(ctx, 3); ok {
		t.Fatal("a remote miss backfilled L1")
	}
}

func TestReadThroughStopsAtLocalErrorsLoaderMissesAndStrictRemoteWriteFailures(t *testing.T) {
	ctx := context.Background()
	cfg := StoreConfig[int, string]{KeyOf: func(v string) int { return len(v) }}
	boom := errors.New("l1 broken")
	loads := 0
	loader := func(context.Context, int) (string, bool, error) {
		loads++
		return "", false, nil
	}
	// L1 出错：错误就是答案，不去 load。
	broken := NewReadThroughStore[int, string](&flakyRemote{getErr: boom, values: map[int]string{}}, nil, loader, cfg, ReadThroughOptions{})
	if _, ok, err := broken.Get(ctx, 3); !errors.Is(err, boom) || ok || loads != 0 {
		t.Fatalf("Get with a failing L1 = (ok=%v, %v), loads=%d; want the L1 error and no load", ok, err, loads)
	}
	// loader 未命中：干净的 miss，不写 L2 / L1。
	remote := &flakyRemote{values: map[int]string{}}
	local := NewLocalStore[int, string](cfg)
	missing := NewReadThroughStore[int, string](local, remote, loader, cfg, ReadThroughOptions{})
	if v, ok, err := missing.Get(ctx, 3); err != nil || ok || v != "" || loads != 1 {
		t.Fatalf("Get with a loader miss = (%q, %v, %v), loads=%d", v, ok, err, loads)
	}
	if len(remote.values) != 0 {
		t.Fatalf("a loader miss was written to L2: %v", remote.values)
	}
	if _, ok, _ := local.Get(ctx, 0); ok {
		t.Fatal("a loader miss was written to L1")
	}
	// loader 出错：原样返回。
	loadErr := errors.New("db down")
	failing := NewReadThroughStore[int, string](NewLocalStore[int, string](cfg), remote, func(context.Context, int) (string, bool, error) { return "", false, loadErr }, cfg, ReadThroughOptions{})
	if _, ok, err := failing.Get(ctx, 3); !errors.Is(err, loadErr) || ok {
		t.Fatalf("Get with a failing loader = (ok=%v, %v)", ok, err)
	}
	// Set：L2 失败且严格 → 报错且 L1 不写；宽松 → 成功且 L1 写入。
	setErr := errors.New("redis write failed")
	strictLocal := NewLocalStore[int, string](cfg)
	strict := NewReadThroughStore[int, string](strictLocal, &flakyRemote{setErr: setErr, values: map[int]string{}}, nil, cfg, ReadThroughOptions{})
	if err := strict.Set(ctx, "abc"); !errors.Is(err, setErr) {
		t.Fatalf("strict Set with a failing L2 = %v", err)
	}
	if _, ok, _ := strictLocal.Get(ctx, 3); ok {
		t.Fatal("strict Set wrote L1 although L2 failed")
	}
	lenientLocal := NewLocalStore[int, string](cfg)
	lenient := NewReadThroughStore[int, string](lenientLocal, &flakyRemote{setErr: setErr, values: map[int]string{}}, nil, cfg, ReadThroughOptions{IgnoreRemoteError: true})
	if err := lenient.Set(ctx, "abc"); err != nil {
		t.Fatalf("lenient Set with a failing L2 = %v", err)
	}
	if v, ok, _ := lenientLocal.Get(ctx, 3); !ok || v != "abc" {
		t.Fatal("lenient Set did not reach L1")
	}
}

func TestRedisStoresRefuseWritesWithoutUsableKeys(t *testing.T) {
	ctx := context.Background()
	redis := newRefHMapFakeRedis()
	item := testItem{ID: 7, Version: 1, Data: "payload"}
	if err := NewRedisJSONHashStore[int64, testItem](redis, time.Hour, nil, testItemConfig()).Set(ctx, item); err == nil || !strings.Contains(err.Error(), "hash key func is nil") {
		t.Fatalf("hash Set without a key func = %v", err)
	}
	if err := NewRedisRawJSONStore[int64, testItem](redis, time.Hour, nil, testItemConfig()).Set(ctx, item); err == nil || !strings.Contains(err.Error(), "raw key func is nil") {
		t.Fatalf("raw Set without a key func = %v", err)
	}
	noField := NewRedisJSONHashStore[int64, testItem](redis, time.Hour, func(int64) RedisHashKey { return RedisHashKey{Key: "items"} }, testItemConfig())
	if err := noField.Set(ctx, item); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("hash Set with an empty field = %v", err)
	}
	noKey := NewRedisJSONHashStore[int64, testItem](redis, time.Hour, func(int64) RedisHashKey { return RedisHashKey{Field: "7"} }, testItemConfig())
	if err := noKey.Set(ctx, item); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("hash Set with an empty key = %v", err)
	}
	if len(redis.hashes) != 0 {
		t.Fatalf("refused writes reached redis: %v", redis.hashes)
	}
}

func TestRefHMapStoreRefusesStaleWrites(t *testing.T) {
	ctx := context.Background()
	store := NewRedisRefHMapStore[int64, refHMapSession](newRefHMapFakeRedis(), RefHMapConfig[int64, refHMapSession]{
		Prefix: "roost:test", Name: "session", TTL: time.Hour, MaxDepth: 8, StoreConfig: refHMapSessionConfig(),
	})
	if err := store.Set(ctx, refHMapSession{ID: 1, Version: 5, Snapshot: refHMapSnapshot{State: 5}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, refHMapSession{ID: 1, Version: 4, Snapshot: refHMapSnapshot{State: 4}}); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("Set with an older version = %v, want ErrStaleWrite", err)
	}
	got, ok, err := store.Get(ctx, 1)
	if err != nil || !ok || got.Version != 5 || got.Snapshot.State != 5 {
		t.Fatalf("a refused stale write changed the stored value: %+v ok=%v err=%v", got, ok, err)
	}
	if err := store.Set(ctx, refHMapSession{ID: 1, Version: 6, Snapshot: refHMapSnapshot{State: 6}}); err != nil {
		t.Fatalf("Set with a newer version = %v", err)
	}
}
