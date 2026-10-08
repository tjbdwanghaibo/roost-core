package cache

import (
	"context"
	"testing"
	"time"
)

// 冷键不再读取时，L1/L2 淘汰不能留下随历史访问量增长的准入元数据。
func TestLayeredColdKeysHaveBoundedMetadata(t *testing.T) {
	cfg := StoreConfig[int, int]{KeyOf: func(v int) int { return v }}
	local := NewAtomicLocalStore(AtomicLocalConfig[int, int]{StoreConfig: cfg, Shards: 1, MaxEntries: 1})
	remote := NewAtomicLocalStore(AtomicLocalConfig[int, int]{StoreConfig: cfg, Shards: 1, MaxEntries: 1})
	store := NewLayeredStore[int, int](local, remote, time.Hour, cfg)
	for key := range 70000 {
		if err := store.Set(context.Background(), key); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(store.expiry); n > 65536 {
		t.Fatalf("expiry entries=%d, want <=65536 while each backing cache holds 1", n)
	}
}

func TestLayeredExpiryEvictionRevalidatesAuthority(t *testing.T) {
	cfg := staleConfig()
	local, remote := NewLocalStore(cfg), NewLocalStore(cfg)
	store := NewLayeredStore[int, staleValue](local, remote, time.Hour, cfg, LayeredOptions{MaxExpiryEntries: 2})
	ctx := context.Background()
	for key := 1; key <= 3; key++ {
		if err := store.Set(ctx, staleValue{Key: key, Version: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := remote.Set(ctx, staleValue{Key: 1, Version: 2}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get(ctx, 1)
	if err != nil || !ok || got.Version != 2 {
		t.Fatalf("evicted admission served stale L1: got=%+v ok=%v err=%v", got, ok, err)
	}
	if len(store.expiry) != 2 || store.order.Len() != 2 {
		t.Fatalf("metadata not bounded: map=%d order=%d", len(store.expiry), store.order.Len())
	}
}

func TestLayeredExpiryChurnAndColdExpiration(t *testing.T) {
	cfg := StoreConfig[int, int]{KeyOf: func(v int) int { return v }}
	store := NewLayeredStore[int, int](NewLocalStore(cfg), NewLocalStore(cfg), time.Second, cfg, LayeredOptions{MaxExpiryEntries: 8})
	now := time.Now()
	for i := range 10000 {
		store.setLocalExpiry(i%8, now)
	}
	if len(store.expiry) != 8 || store.order.Len() != 8 {
		t.Fatal("overwrites retained old metadata")
	}
	store.setLocalExpiry(9, now.Add(time.Second))
	if len(store.expiry) != 1 || store.order.Len() != 1 {
		t.Fatal("cold expired entries were not reclaimed")
	}
	store.clearLocalExpiry(9)
	if len(store.expiry) != 0 || store.order.Len() != 0 {
		t.Fatal("delete retained metadata")
	}
}
