package versionstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0149 · C2 · gap map core `versionstore` 4/11：内存 / Redis 存储的 Update 拒绝 nil 变更函数；Redis
// 存储缺客户端不能构造；带期望版本的 Delete 在版本不符时报 ErrVersionMismatch 且不删。
func TestStoresRefuseNilMutatorsMissingClientsAndStaleDeletes(t *testing.T) {
	ctx := context.Background()
	if _, _, err := NewMemoryStore[string, counter]().Update(ctx, "k", nil); err == nil || !strings.Contains(err.Error(), "mutate is nil") {
		t.Fatalf("memory Update(nil) = %v", err)
	}
	cfg := RedisConfig[string, counter]{Prefix: "test:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{}}
	if _, err := NewRedisStore[string, counter](nil, cfg); err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("NewRedisStore(nil) = %v", err)
	}
	store, err := NewRedisStore[string, counter](newFakeRedis(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, "k", nil); err == nil || !strings.Contains(err.Error(), "mutate is nil") {
		t.Fatalf("redis Update(nil) = %v", err)
	}
	created, ok, err := store.Create(ctx, "k", counter{})
	if err != nil || !ok {
		t.Fatalf("Create = (%+v, %v, %v)", created, ok, err)
	}
	if err := store.Delete(ctx, "k", Versioned[counter]{Value: created.Value, Version: created.Version + 5}); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("Delete with a stale version = %v, want ErrVersionMismatch", err)
	}
	if _, found, err := store.Get(ctx, "k"); err != nil || !found {
		t.Fatalf("a refused delete removed the value: found=%v err=%v", found, err)
	}
	if err := store.Delete(ctx, "k", created); err != nil {
		t.Fatalf("Delete with the current version = %v", err)
	}
}
