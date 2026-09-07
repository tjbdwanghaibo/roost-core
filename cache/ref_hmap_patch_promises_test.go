package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newSessionRefHMapStore(t *testing.T, maxDepth int) *RedisRefHMapStore[int64, refHMapSession] {
	t.Helper()
	return NewRedisRefHMapStore[int64, refHMapSession](newRefHMapFakeRedis(), RefHMapConfig[int64, refHMapSession]{
		Prefix: "roost:test", Name: "session", TTL: time.Hour, MaxDepth: maxDepth,
		StoreConfig: refHMapSessionConfig(),
	})
}

// U-0102 (C2, B-24 第四项): a patch path is resolved against the reflective
// layout before anything is written. Each way a path can miss the layout is
// refused as ErrRefHMapUnsupported with the path named; a nested type deeper
// than the configured limit is refused when the layout is built.
func TestRefHMapPatchPathRefusesEachShapeItCannotAddress(t *testing.T) {
	store := newSessionRefHMapStore(t, 8)
	plan, err := store.plan(1001)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"snapshot..score":     `empty patch path "snapshot..score"`,
		"":                    `empty patch path ""`,
		"snapshot.nope":       `unknown patch path "snapshot.nope"`,
		"snapshot":            `patch path "snapshot" is not scalar`,
		"snapshot.inner":      `patch path "snapshot.inner" is not scalar`,
		"version.major":       `patch path "version.major" crosses non-struct field`,
		"snapshot.state.bits": `patch path "snapshot.state.bits" crosses non-struct field`,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			_, err := plan.patchTarget(path)
			if !errors.Is(err, ErrRefHMapUnsupported) || !strings.Contains(err.Error(), want) {
				t.Fatalf("patchTarget(%q) = %v; want ErrRefHMapUnsupported containing %q", path, err, want)
			}
		})
	}
	var nilPlan *refHMapPlan
	if _, err := nilPlan.patchTarget("snapshot.inner.score"); !errors.Is(err, ErrRefHMapUnsupported) || !strings.Contains(err.Error(), "empty plan") {
		t.Fatalf("nil plan patchTarget = %v", err)
	}
	target, err := plan.patchTarget("snapshot.inner.score")
	if err != nil || target.node == nil || target.key != "roost:test:{session:1001}:snapshot:inner" {
		t.Fatalf("valid patch target = %+v, %v", target, err)
	}

	// Patch goes through the same resolver: a bad path writes nothing.
	redis := newRefHMapFakeRedis()
	writing := NewRedisRefHMapStore[int64, refHMapSession](redis, RefHMapConfig[int64, refHMapSession]{
		Prefix: "roost:test", Name: "session", TTL: time.Hour, StoreConfig: refHMapSessionConfig(),
	})
	ctx := context.Background()
	if err := writing.Set(ctx, refHMapSession{ID: 7, Version: 1, Snapshot: refHMapSnapshot{Inner: refHMapInner{Score: 3}}}); err != nil {
		t.Fatal(err)
	}
	if err := writing.Patch(ctx, 7, "snapshot.nope", int64(9)); !errors.Is(err, ErrRefHMapUnsupported) {
		t.Fatalf("Patch with an unknown path = %v", err)
	}
	got, ok, err := writing.Get(ctx, 7)
	if err != nil || !ok || got.Snapshot.Inner.Score != 3 {
		t.Fatalf("value after refused patch = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestRefHMapLayoutRefusesTypesDeeperThanTheLimit(t *testing.T) {
	shallow := newSessionRefHMapStore(t, 1)
	if _, err := shallow.plan(1); !errors.Is(err, ErrRefHMapMaxDepth) {
		t.Fatalf("plan with MaxDepth 1 for session.snapshot.inner = %v, want ErrRefHMapMaxDepth", err)
	}
	if _, err := newSessionRefHMapStore(t, 2).plan(1); err != nil {
		t.Fatalf("plan with MaxDepth 2 = %v, want the layout to fit", err)
	}
}

// The JSON store's stale-write rule and key-func requirement: a write the
// store refuses must not read back as success.
func TestRedisJSONStoreRefusesStaleWritesAndMissingKeyFunc(t *testing.T) {
	ctx := context.Background()
	keyOf := func(k int) string { return fmt.Sprintf("stale:%d", k) }
	store := NewRedisJSONStore[int, staleValue](newRefHMapFakeRedis(), time.Hour, keyOf, staleConfig())
	if err := store.Set(ctx, staleValue{Key: 1, Version: 5, Payload: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, staleValue{Key: 1, Version: 4, Payload: "old"}); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("stale Set = %v, want ErrStaleWrite", err)
	}
	got, ok, err := store.Get(ctx, 1)
	if err != nil || !ok || got.Payload != "new" {
		t.Fatalf("value after stale Set = %+v ok=%v err=%v", got, ok, err)
	}
	if err := NewRedisJSONStore[int, staleValue](newRefHMapFakeRedis(), time.Hour, nil, staleConfig()).Set(ctx, staleValue{Key: 1}); err == nil || !strings.Contains(err.Error(), "key func is nil") {
		t.Fatalf("Set without a key func = %v", err)
	}
}
