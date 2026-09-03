package versionstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type counter struct {
	Name  string `json:"name"`
	Total int64  `json:"total"`
}

// Both implementations are driven through the same contract suite. A contract
// with one implementation drifts into that implementation's accidents, and the
// defect this package exists to prevent is exactly an implementation that
// satisfies the interface without honouring it.
func forEachStore(t *testing.T, run func(t *testing.T, store Store[string, counter])) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		run(t, NewMemoryStore[string, counter]())
	})
	t.Run("redis", func(t *testing.T) {
		store, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
			Prefix: "test:",
			KeyOf:  func(key string) string { return key },
			Codec:  JSONCodec[counter]{},
		})
		if err != nil {
			t.Fatal(err)
		}
		run(t, store)
	})
}

// A stored value always has Version >= 1, so no caller can opt out of the
// comparison by leaving a version field at its zero value — the way a service
// that submitted Version: 0 skipped its guard entirely and double-counted
// every redelivered event.
func TestStoredVersionIsNeverZeroAndIncrementsPerWrite(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		if got, found, err := store.Get(ctx, "a"); err != nil || found || got.Version != 0 {
			t.Fatalf("absent key: %+v found=%v err=%v", got, found, err)
		}
		for want := uint64(1); want <= 3; want++ {
			result, applied, err := store.Update(ctx, "a", func(current counter, _ bool) (counter, bool, error) {
				current.Total++
				return current, true, nil
			})
			if err != nil || !applied {
				t.Fatalf("update %d: applied=%v err=%v", want, applied, err)
			}
			if result.Version != want {
				t.Fatalf("version after %d writes = %d, want %d", want, result.Version, want)
			}
		}
		stored, found, err := store.Get(ctx, "a")
		if err != nil || !found {
			t.Fatalf("get: found=%v err=%v", found, err)
		}
		if stored.Value.Total != 3 || stored.Version != 3 {
			t.Fatalf("stored = %+v, want total 3 version 3", stored)
		}
	})
}

// mutate declining to save must leave the store untouched and report that
// nothing was applied — not report success for a write that did not happen.
func TestUpdateDecliningToSaveChangesNothing(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			c.Total = 5
			return c, true, nil
		}); err != nil {
			t.Fatal(err)
		}
		result, applied, err := store.Update(ctx, "a", func(c counter, found bool) (counter, bool, error) {
			if !found {
				t.Fatal("mutate saw the key as absent")
			}
			c.Total = 99
			return c, false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			t.Fatal("Update reported applied for a mutate that declined to save")
		}
		if result.Value.Total != 5 || result.Version != 1 {
			t.Fatalf("declined update returned %+v, want the stored value", result)
		}
		stored, _, _ := store.Get(ctx, "a")
		if stored.Value.Total != 5 || stored.Version != 1 {
			t.Fatalf("store changed after a declined update: %+v", stored)
		}
	})
}

// An error from mutate must abort without writing.
func TestUpdatePropagatesMutateErrorWithoutWriting(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		sentinel := errors.New("business rule")
		if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			return c, true, sentinel
		}); !errors.Is(err, sentinel) {
			t.Fatalf("Update returned %v, want the mutate error", err)
		}
		if _, found, _ := store.Get(ctx, "a"); found {
			t.Fatal("a failed mutate wrote a value")
		}
	})
}

// Create is the insert-only path: it must refuse an existing key rather than
// overwrite it. Blind overwrite on create is how an ID collision silently
// destroys an existing record.
func TestCreateRefusesAnExistingKey(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		first, created, err := store.Create(ctx, "a", counter{Name: "first"})
		if err != nil || !created {
			t.Fatalf("first Create: created=%v err=%v", created, err)
		}
		if first.Version != 1 {
			t.Fatalf("Create assigned version %d, want 1", first.Version)
		}
		second, created, err := store.Create(ctx, "a", counter{Name: "second"})
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Fatal("Create overwrote an existing key")
		}
		if second.Version != 0 {
			t.Fatalf("refused Create returned version %d, want 0", second.Version)
		}
		// The refused Create hands back the ZERO value, not the value it
		// collided with. Every implementation must agree on this, because the
		// interface now promises it — and because assuming otherwise is a
		// quiet mistake: the zero value's fields read as empty, so a caller
		// inspecting them takes the branch meant for "absent" while the key
		// is occupied. That misreading turned an exclusive per-owner claim
		// into no exclusion at all in a consumer of this package.
		if second.Value != (counter{}) {
			t.Fatalf("refused Create returned the colliding value %+v; the contract says "+
				"the zero value, and a caller that needs the existing one must Get it",
				second.Value)
		}
		stored, _, _ := store.Get(ctx, "a")
		if stored.Value.Name != "first" {
			t.Fatalf("existing value was replaced: %+v", stored)
		}
	})
}

// Delete is version-checked: a caller holding a stale version must be refused
// rather than deleting whatever is there now.
func TestDeleteRequiresTheCallersVersion(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		stored, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			c.Total = 1
			return c, true, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, "a", Versioned[counter]{Version: stored.Version + 1}); !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("Delete with a wrong version returned %v, want ErrVersionMismatch", err)
		}
		if _, found, _ := store.Get(ctx, "a"); !found {
			t.Fatal("a refused Delete removed the value")
		}
		if err := store.Delete(ctx, "a", stored); err != nil {
			t.Fatalf("Delete with the held version: %v", err)
		}
		if _, found, _ := store.Get(ctx, "a"); found {
			t.Fatal("Delete left the value in place")
		}
		// Deleting an absent key is a no-op only for a caller that holds no
		// version; a caller holding one is out of date and must be told.
		if err := store.Delete(ctx, "a", Versioned[counter]{}); err != nil {
			t.Fatalf("Delete of an absent key with no held version: %v", err)
		}
		if err := store.Delete(ctx, "a", Versioned[counter]{Version: 9}); !errors.Is(err, ErrVersionMismatch) {
			t.Fatalf("Delete of an absent key holding a version returned %v", err)
		}
	})
}

// The property the whole package exists for: concurrent read-modify-write must
// not lose an update. Every increment has to survive.
func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		const writers, perWriter = 8, 25

		var wait sync.WaitGroup
		errs := make([]error, writers)
		for writer := 0; writer < writers; writer++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				for i := 0; i < perWriter; i++ {
					if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
						c.Total++
						return c, true, nil
					}); err != nil {
						errs[index] = err
						return
					}
				}
			}(writer)
		}
		wait.Wait()
		for writer, err := range errs {
			if err != nil {
				t.Fatalf("writer %d: %v", writer, err)
			}
		}
		stored, found, err := store.Get(ctx, "a")
		if err != nil || !found {
			t.Fatalf("get: found=%v err=%v", found, err)
		}
		if stored.Value.Total != writers*perWriter {
			t.Fatalf("total = %d, want %d: updates were lost", stored.Value.Total, writers*perWriter)
		}
		if stored.Version != uint64(writers*perWriter) {
			t.Fatalf("version = %d, want %d: a write did not bump the version", stored.Version, writers*perWriter)
		}
	})
}

// Only one concurrent Create may win.
func TestConcurrentCreateHasExactlyOneWinner(t *testing.T) {
	forEachStore(t, func(t *testing.T, store Store[string, counter]) {
		ctx := context.Background()
		const racers = 8
		var wait sync.WaitGroup
		var mu sync.Mutex
		wins := 0
		for racer := 0; racer < racers; racer++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				_, created, err := store.Create(ctx, "a", counter{Name: fmt.Sprintf("racer-%d", index)})
				if err != nil {
					return
				}
				if created {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}(racer)
		}
		wait.Wait()
		if wins != 1 {
			t.Fatalf("%d creators reported success, want exactly 1", wins)
		}
	})
}

// Exhausting the retry budget must be reported as a conflict, distinguishable
// from a store failure: contention and an unreachable backend need different
// operational responses.
func TestUpdateReportsConflictWhenTheBudgetIsExhausted(t *testing.T) {
	fake := newFakeRedis()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{}, MaxAttempts: 3, RetryBackoff: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
		return c, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	// Every compare-and-set now loses, as if another writer always won.
	fake.failEveryCAS = true
	_, applied, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
		c.Total++
		return c, true, nil
	})
	if applied {
		t.Fatal("Update reported applied despite never winning the compare-and-set")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Update returned %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("the conflict error does not say how many attempts were made: %v", err)
	}
}

// The compare must be on the version, not on a re-encoded value: a codec whose
// output changes for the same value must not wedge writes. This is the failure
// that took down every role write in a service that compared marshalled JSON.
func TestUpdateSurvivesACodecWhoseOutputChanges(t *testing.T) {
	drifting := &driftingCodec{}
	store, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
		KeyOf: func(key string) string { return key }, Codec: drifting,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, applied, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) {
			c.Total++
			return c, true, nil
		}); err != nil || !applied {
			t.Fatalf("write %d with a drifting codec: applied=%v err=%v", i, applied, err)
		}
	}
	stored, _, err := store.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Value.Total != 5 {
		t.Fatalf("total = %d, want 5", stored.Value.Total)
	}
}

// driftingCodec appends a per-call suffix so that encoding the same value
// twice never produces the same bytes.
type driftingCodec struct{ calls int }

func (c *driftingCodec) Encode(value counter) ([]byte, error) {
	c.calls++
	return []byte(fmt.Sprintf(`{"name":%q,"total":%d,"nonce":%d}`, value.Name, value.Total, c.calls)), nil
}

func (c *driftingCodec) Decode(raw []byte) (counter, error) {
	return JSONCodec[counter]{}.Decode(raw)
}

func TestNewRedisStoreRejectsIncompleteConfig(t *testing.T) {
	if _, err := NewRedisStore[string, counter](nil, RedisConfig[string, counter]{}); err == nil {
		t.Fatal("nil client accepted")
	}
	if _, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{Codec: JSONCodec[counter]{}}); err == nil {
		t.Fatal("nil key func accepted")
	}
	if _, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
		KeyOf: func(string) string { return "k" },
	}); !errors.Is(err, ErrCodecNil) {
		t.Fatal("nil codec accepted")
	}
}

func TestEmptyRenderedKeyIsRejected(t *testing.T) {
	store, err := NewRedisStore[string, counter](newFakeRedis(), RedisConfig[string, counter]{
		KeyOf: func(string) string { return "" }, Codec: JSONCodec[counter]{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(context.Background(), "a"); !errors.Is(err, ErrKeyEmpty) {
		t.Fatalf("empty rendered key returned %v, want ErrKeyEmpty", err)
	}
}

// A malformed envelope must be an error, not a zero value silently treated as
// the current state — decoding garbage to a zero and carrying on is how a
// corrupt record becomes a wrong write.
func TestMalformedEnvelopeIsAnError(t *testing.T) {
	fake := newFakeRedis()
	fake.values["a"] = []byte("no-newline-here")
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Get(context.Background(), "a"); err == nil {
		t.Fatal("a malformed envelope decoded without error")
	}
	fake.values["b"] = []byte("0\n{}")
	if _, _, err := store.Get(context.Background(), "b"); err == nil {
		t.Fatal("a zero envelope version decoded without error")
	}
}
