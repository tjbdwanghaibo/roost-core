package cache

import (
	"context"
	"errors"
	"testing"
)

type flakyRemote struct {
	getErr, setErr error
	values         map[int]string
}

func (r *flakyRemote) Get(_ context.Context, key int) (string, bool, error) {
	if r.getErr != nil {
		return "", false, r.getErr
	}
	v, ok := r.values[key]
	return v, ok, nil
}
func (r *flakyRemote) Set(_ context.Context, value string) error {
	if r.setErr != nil {
		return r.setErr
	}
	r.values[len(value)] = value
	return nil
}
func (r *flakyRemote) Delete(context.Context, int) error { return nil }

// IgnoreRemoteError is the only knob deciding whether an L2 outage is an
// outage for readers too. Both settings are pinned on both L2 calls: with
// the default a remote failure is the caller's error (and the loader is not
// consulted), with the flag the store degrades to L1 + loader and the failure
// is only counted.
func TestReadThroughRemoteFailurePolicyIsHonouredOnGetAndSet(t *testing.T) {
	cfg := StoreConfig[int, string]{KeyOf: func(v string) int { return len(v) }}
	build := func(remote *flakyRemote, ignore bool) (*ReadThroughStore[int, string], *int) {
		loads := 0
		store := NewReadThroughStore[int, string](NewLocalStore[int, string](cfg), remote, func(context.Context, int) (string, bool, error) {
			loads++
			return "abc", true, nil
		}, cfg, ReadThroughOptions{IgnoreRemoteError: ignore})
		return store, &loads
	}
	boom := errors.New("redis unreachable")

	strict, loads := build(&flakyRemote{getErr: boom, values: map[int]string{}}, false)
	if _, ok, err := strict.Get(context.Background(), 3); !errors.Is(err, boom) || ok {
		t.Fatalf("strict store with a failing L2: ok=%v err=%v, want the remote error", ok, err)
	}
	if *loads != 0 {
		t.Fatal("strict store consulted the loader although the remote failed")
	}
	if strict.Stats().RemoteError != 1 {
		t.Fatalf("remote errors counted = %d, want 1", strict.Stats().RemoteError)
	}

	lenient, loads := build(&flakyRemote{getErr: boom, values: map[int]string{}}, true)
	if v, ok, err := lenient.Get(context.Background(), 3); err != nil || !ok || v != "abc" || *loads != 1 {
		t.Fatalf("lenient store must degrade to the loader: v=%q ok=%v err=%v loads=%d", v, ok, err, *loads)
	}
	if lenient.Stats().RemoteError != 1 {
		t.Fatalf("lenient store must still count the remote failure: %d", lenient.Stats().RemoteError)
	}

	strictSet, _ := build(&flakyRemote{setErr: boom, values: map[int]string{}}, false)
	if _, ok, err := strictSet.Get(context.Background(), 3); !errors.Is(err, boom) || ok {
		t.Fatalf("strict store whose L2 write-back fails: ok=%v err=%v, want the remote error", ok, err)
	}
	lenientSet, _ := build(&flakyRemote{setErr: boom, values: map[int]string{}}, true)
	if v, ok, err := lenientSet.Get(context.Background(), 3); err != nil || !ok || v != "abc" {
		t.Fatalf("lenient store whose L2 write-back fails must still serve the loaded value: v=%q ok=%v err=%v", v, ok, err)
	}
}
