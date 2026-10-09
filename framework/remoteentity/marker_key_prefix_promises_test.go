package remoteentity

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// RR-20260930-19（REMAINING §3 N25）：非 authority 兼容装配的 Redis 所有权标记只有一把 hash 键，缺省 remote_entity:marks 不带部署前缀，
// 共用一个 Redis db 的两个部署会互相读到对方的租约（一个部署 Claim 之后另一个部署把它当成已有 owner）。
// 承诺：NewRedisMarkerWithKeyPrefix(redis, prefix) 非空时全部标记读写都落在 "<prefix>:remote_entity:marks"，
// 两个前缀不同的部署互不可见；为空（默认）时键与旧版本逐字相同，且与 NewRedisMarker(redis, "") 互读。
// 与 RR-20260927-17 的 L2 前缀同形，同一个部署前缀值可以同时用于两者。

// keyedMarkerEval 按 KEYS[1] 分开保存，记录标记实际写到了哪把键（包内原有的 markerEvalStub 忽略键）。
type keyedMarkerEval struct {
	mu   sync.Mutex
	keys map[string]*markerEvalStub
}

func newKeyedMarkerEval() *keyedMarkerEval {
	return &keyedMarkerEval{keys: map[string]*markerEvalStub{}}
}

func (k *keyedMarkerEval) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	k.mu.Lock()
	stub := k.keys[keys[0]]
	if stub == nil {
		stub = newMarkerEvalStub()
		k.keys[keys[0]] = stub
	}
	k.mu.Unlock()
	return stub.Eval(ctx, script, keys, args...)
}

func (k *keyedMarkerEval) touched() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	keys := make([]string, 0, len(k.keys))
	for key := range k.keys {
		keys = append(keys, key)
	}
	return keys
}

func TestRedisMarkerDefaultKeyIsUnchanged(t *testing.T) {
	redis := newKeyedMarkerEval()
	store, err := newRedisMarkerForEvalWithKeyPrefix(redis, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOwnership(context.Background(), 7311, 1000); err != nil {
		t.Fatal(err)
	}
	// 旧版本的键逐字写出来，不经 markerKeyForPrefix，防止两边一起改。
	if got := redis.touched(); len(got) != 1 || got[0] != "remote_entity:marks" {
		t.Fatalf("default marker keys=%v, want exactly %q (unchanged, no migration)", got, "remote_entity:marks")
	}
	legacy := newRedisMarkerForEval(redis, "")
	if lease, found, err := legacy.GetOwnership(context.Background(), 7311); err != nil || !found || lease.OwnerSid != 1000 {
		t.Fatalf("marker without a prefix does not read the default marker's lease: found=%v lease=%+v err=%v", found, lease, err)
	}
}

func TestRedisMarkerKeyPrefixIsolatesDeploymentsSharingOneRedis(t *testing.T) {
	redis := newKeyedMarkerEval()
	ctx := context.Background()
	a, err := newRedisMarkerForEvalWithKeyPrefix(redis, "roost:game-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newRedisMarkerForEvalWithKeyPrefix(redis, "roost:game-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimOwnership(ctx, 7312, 1000); err != nil {
		t.Fatal(err)
	}
	// 另一个部署看不到这份租约：GetOwnership 找不到，自己的 Claim 也不会被当成别人的 owner 拒绝。
	if lease, found, err := b.GetOwnership(ctx, 7312); err != nil || found {
		t.Fatalf("deployment b reads deployment a's lease: found=%v lease=%+v err=%v", found, lease, err)
	}
	claimed, err := b.ClaimOwnership(ctx, 7312, 2000)
	if err != nil || claimed.OwnerSid != 2000 {
		t.Fatalf("deployment b claim = %+v, err=%v; want its own owner 2000", claimed, err)
	}
	if lease, found, err := a.GetOwnership(ctx, 7312); err != nil || !found || lease.OwnerSid != 1000 {
		t.Fatalf("deployment a's lease changed by deployment b: found=%v lease=%+v err=%v", found, lease, err)
	}
	for _, key := range redis.touched() {
		if key != "roost:game-a:remote_entity:marks" && key != "roost:game-b:remote_entity:marks" {
			t.Fatalf("marker touched key %q outside both prefixes", key)
		}
	}
	if got := len(redis.touched()); got != 2 {
		t.Fatalf("touched %d keys, want one hash per deployment: %v", got, redis.touched())
	}
}

func TestMarkerKeyPrefixValidation(t *testing.T) {
	for _, prefix := range []string{"", "roost:game-a", "{roost:game-a}", "roost:{game-a}:remote"} {
		if err := ValidateMarkerKeyPrefix(prefix); err != nil {
			t.Fatalf("prefix %q rejected: %v", prefix, err)
		}
	}
	// 标记只有一把 hash 键，hash tag 无害、允许；但写了花括号就必须是首个非空闭合 tag——
	// Redis 对 "e{" / "e{}" 不做 tag 哈希，运维会以为钉住了槽而没有（与 lock_key 的 Cluster 规则同一判断）。
	for _, prefix := range []string{" roost", "roost ", "roost game", "roost\tgame", "roost{", "roost{}", "roost{}{game}"} {
		if err := ValidateMarkerKeyPrefix(prefix); err == nil {
			t.Fatalf("prefix %q accepted", prefix)
		}
	}
	if _, err := newRedisMarkerForEvalWithKeyPrefix(newKeyedMarkerEval(), "roost{}"); err == nil || !strings.Contains(err.Error(), "hash tag") {
		t.Fatalf("constructor accepted an empty hash tag: %v", err)
	}
}
