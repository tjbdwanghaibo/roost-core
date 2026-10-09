package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

type staleValue struct {
	Key     int
	Version int64
	Payload string
}

func staleConfig() StoreConfig[int, staleValue] {
	return StoreConfig[int, staleValue]{
		KeyOf: func(v staleValue) int { return v.Key },
		Stale: func(old, next staleValue) bool { return old.Version > next.Version },
	}
}

// A dropped write must be distinguishable from a completed one. Returning nil
// is how a caller ends up reporting success for a mutation the store refused —
// a cancel that answers OK while the store still holds the old state.
func TestStaleSetIsReportedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	store := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: staleConfig(), Shards: 2})

	if err := store.Set(ctx, staleValue{Key: 1, Version: 5, Payload: "new"}); err != nil {
		t.Fatal(err)
	}
	err := store.Set(ctx, staleValue{Key: 1, Version: 4, Payload: "old"})
	if !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("stale Set returned %v, want ErrStaleWrite", err)
	}
	// And the refusal must be real: the stored value is untouched.
	got, ok, getErr := store.Get(ctx, 1)
	if getErr != nil || !ok {
		t.Fatalf("Get after refused write: ok=%v err=%v", ok, getErr)
	}
	if got.Payload != "new" || got.Version != 5 {
		t.Fatalf("refused write still landed: %+v", got)
	}
	// An equal version is not stale under this predicate, so it must be
	// accepted — the sentinel must not leak into the non-stale path.
	if err := store.Set(ctx, staleValue{Key: 1, Version: 5, Payload: "equal"}); err != nil {
		t.Fatalf("equal-version Set returned %v, want nil", err)
	}
}

// The same contract has to hold for a store with no prior value: there is
// nothing to be stale against, so the write must be accepted.
func TestStaleSetAcceptsFirstWrite(t *testing.T) {
	store := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: staleConfig(), Shards: 2})
	if err := store.Set(context.Background(), staleValue{Key: 7, Version: 1}); err != nil {
		t.Fatalf("first Set returned %v, want nil", err)
	}
}

// A LayeredStore with no TTL configured must not serve from L1 forever. A
// missing config key yields 0 from viper.GetDuration, so treating 0 as
// "cache forever" turns an unset option into a permanently stale first level —
// every replica reading its own private view.
func TestLayeredStoreWithoutTTLAlwaysRevalidates(t *testing.T) {
	ctx := context.Background()
	cfg := StoreConfig[int, staleValue]{KeyOf: func(v staleValue) int { return v.Key }}
	local := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})
	remote := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})

	if err := remote.Set(ctx, staleValue{Key: 1, Payload: "v1"}); err != nil {
		t.Fatal(err)
	}
	layered := NewLayeredStore[int, staleValue](local, remote, 0, cfg)

	if got, ok, err := layered.Get(ctx, 1); err != nil || !ok || got.Payload != "v1" {
		t.Fatalf("first read: %+v ok=%v err=%v", got, ok, err)
	}
	// Remote moves on. With ttl == 0 the L1 must not be trusted, so the next
	// read has to observe the new value.
	if err := remote.Set(ctx, staleValue{Key: 1, Payload: "v2"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := layered.Get(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("second read: ok=%v err=%v", ok, err)
	}
	if got.Payload != "v2" {
		t.Fatalf("served a stale L1 copy %q; ttl<=0 must mean always revalidate", got.Payload)
	}
}

// With a TTL configured the L1 is authoritative for that long — the fix must
// not disable layering altogether.
func TestLayeredStoreServesFromL1WithinTTL(t *testing.T) {
	ctx := context.Background()
	cfg := StoreConfig[int, staleValue]{KeyOf: func(v staleValue) int { return v.Key }}
	local := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})
	remote := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})
	if err := remote.Set(ctx, staleValue{Key: 1, Payload: "v1"}); err != nil {
		t.Fatal(err)
	}
	layered := NewLayeredStore[int, staleValue](local, remote, time.Minute, cfg)

	if _, _, err := layered.Get(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := remote.Set(ctx, staleValue{Key: 1, Payload: "v2"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := layered.Get(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("read within TTL: ok=%v err=%v", ok, err)
	}
	if got.Payload != "v1" {
		t.Fatalf("L1 was not used within its TTL: got %q", got.Payload)
	}
}

// A store with only a local level must keep working: Get short-circuits before
// consulting localValid, so the ttl<=0 change must not make it unreadable.
func TestLayeredStoreWithoutRemoteStillServesLocal(t *testing.T) {
	ctx := context.Background()
	cfg := StoreConfig[int, staleValue]{KeyOf: func(v staleValue) int { return v.Key }}
	local := NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: cfg, Shards: 2})
	layered := NewLayeredStore[int, staleValue](local, nil, 0, cfg)

	if err := layered.Set(ctx, staleValue{Key: 1, Payload: "only"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := layered.Get(ctx, 1)
	if err != nil || !ok || got.Payload != "only" {
		t.Fatalf("local-only layered store: %+v ok=%v err=%v", got, ok, err)
	}
}
