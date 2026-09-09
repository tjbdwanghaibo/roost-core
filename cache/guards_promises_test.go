package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// U-0138 · C2（空洞测试）· nightly gap map core `cache` 8/20。
//
// 分层读：远端失败原样上抛、远端未命中不回填零值也不当命中；读穿透：L1 出错
// 就是出错（不去 load）、loader 未命中 / 出错原样返回且不写 L2 / L1、Set 在 L2
// 失败且未设 IgnoreRemoteError 时报错且不写 L1；Redis 存储：无 key 函数拒绝写、
// hash 键或字段为空拒绝写（都在触达 Redis 之前）；ref-hmap 的 Stale 判定拒绝旧
// 版本且不改写已存值。

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
