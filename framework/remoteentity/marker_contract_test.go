package remoteentity

import (
	"context"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"strings"
	"sync"
	"testing"
)

func TestMarkerLeaseStaysExactDecimalAtLargeEpochs(t *testing.T) {
	ctx := context.Background()
	for _, epoch := range []uint64{1 << 46, 100_000_000_000_000, 1<<53 + 1} {
		stub := newMarkerEvalStub()
		store := newRedisMarkerForEval(stub, "marks")
		// A lease already at a large epoch, in the exact form Go writes.
		seed := entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: epoch, RouteEpoch: 1}
		stub.values["42"] = formatMarkerLease(seed)

		shared, err := store.EnterSharedExpected(ctx, 42, seed)
		if err != nil {
			t.Fatalf("epoch %d: enter shared = %v; the record was rewritten before this failed", epoch, err)
		}
		if shared.MarkerEpoch != epoch+1 {
			t.Fatalf("epoch %d: new epoch = %d, want %d", epoch, shared.MarkerEpoch, epoch+1)
		}
		// And the stored record must still be readable.
		got, ok, err := store.GetOwnership(ctx, 42)
		if err != nil || !ok {
			t.Fatalf("epoch %d: reading back the lease = ok:%v err:%v", epoch, ok, err)
		}
		if got.MarkerEpoch != epoch+1 || !got.Shared {
			t.Fatalf("epoch %d: read back %#v", epoch, got)
		}
	}
}

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
