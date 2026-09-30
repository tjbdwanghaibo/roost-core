//go:build integration

package remoteentity

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// RR-20260930-19：真 Redis 上验证 NewRedisMarkerWithKeyPrefix 的键布局——两个前缀各自一把 hash，
// 缺省前缀的 hash 键逐字是 remote_entity:marks。键前缀 b19:<pid>:<token>，结束时删除自己的键，不清库。
func TestRealRedisMarkerKeyPrefixIsolatesDeployments(t *testing.T) {
	r := realRemoteRedis(t)
	base := fmt.Sprintf("b19:%d:%s", os.Getpid(), generateToken())
	prefixes := []string{base + ":a", base + ":b"}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := r.Del(ctx, markerKeyForPrefix(prefixes[0]), markerKeyForPrefix(prefixes[1])); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := NewRedisMarkerWithKeyPrefix(r, prefixes[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewRedisMarkerWithKeyPrefix(r, prefixes[1])
	if err != nil {
		t.Fatal(err)
	}
	const id = int64(7313)
	if _, err := a.ClaimOwnership(ctx, id, 1000); err != nil {
		t.Fatal(err)
	}
	if lease, found, err := b.GetOwnership(ctx, id); err != nil || found {
		t.Fatalf("deployment b reads deployment a's lease: found=%v lease=%+v err=%v", found, lease, err)
	}
	if claimed, err := b.ClaimOwnership(ctx, id, 2000); err != nil || claimed.OwnerSid != 2000 {
		t.Fatalf("deployment b claim = %+v, err=%v", claimed, err)
	}
	// 键确实各自落在自己的前缀下：直接按键名读 hash field。
	for i, store := range []*redisMarker{a, b} {
		want := prefixes[i] + ":remote_entity:marks"
		if store.key != want {
			t.Fatalf("marker %d key=%q, want %q", i, store.key, want)
		}
		if raw, err := r.HGet(ctx, want, "7313"); err != nil || len(raw) == 0 {
			t.Fatalf("no lease under %q: raw=%q err=%v", want, raw, err)
		}
	}
	if got := markerKeyForPrefix(""); got != "remote_entity:marks" {
		t.Fatalf("default key %q changed", got)
	}
}
