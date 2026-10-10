package cache

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestReadThroughLoaderFillRefusalIsNotReadFailure(t *testing.T) {
	conflictCfg := staleConfig()
	conflictCfg.Conflict = func(old, next staleValue) bool { return old.Version == next.Version && old.Payload != next.Payload }
	cases := []struct {
		name string
		// local 构造 L1；publish 是 loader 执行期间落到 L1 / L2 的并发写。
		local   func() Store[int, staleValue]
		remote  func() Store[int, staleValue]
		publish func(ctx context.Context, local, remote Store[int, staleValue]) error
		want    staleValue
		wantOK  bool
		wantErr error
	}{
		{
			name:  "local_stale",
			local: func() Store[int, staleValue] { return NewLocalStore(staleConfig()) },
			publish: func(ctx context.Context, l, _ Store[int, staleValue]) error {
				return l.Set(ctx, staleValue{Key: 1, Version: 2, Payload: "published"})
			},
			want: staleValue{Key: 1, Version: 2, Payload: "published"}, wantOK: true,
		},
		{
			name: "atomic_stale",
			local: func() Store[int, staleValue] {
				return NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: staleConfig(), Shards: 1})
			},
			publish: func(ctx context.Context, l, _ Store[int, staleValue]) error {
				return l.Set(ctx, staleValue{Key: 1, Version: 2, Payload: "published"})
			},
			want: staleValue{Key: 1, Version: 2, Payload: "published"}, wantOK: true,
		},
		{
			name: "atomic_conflict",
			local: func() Store[int, staleValue] {
				return NewAtomicLocalStore(AtomicLocalConfig[int, staleValue]{StoreConfig: conflictCfg, Shards: 1})
			},
			publish: func(ctx context.Context, l, _ Store[int, staleValue]) error {
				return l.Set(ctx, staleValue{Key: 1, Version: 1, Payload: "published"})
			},
			want: staleValue{Key: 1, Version: 1, Payload: "published"}, wantOK: true,
		},
		{
			// L1 拒绝且读回没有值：stale 表示被更新的删除取代，读到 miss。
			name:  "stale_without_value",
			local: func() Store[int, staleValue] { return &admissionFailingLocal{setErr: ErrStaleWrite} },
		},
		{
			// conflict 且读回没有值：拒绝原样返回，不交付被拒的 loader 值。
			name:    "conflict_without_value",
			local:   func() Store[int, staleValue] { return &admissionFailingLocal{setErr: ErrConflictingWrite} },
			wantErr: ErrConflictingWrite,
		},
		{
			// strict 模式：loader 期间另一个副本把 v2 发布进 L2，回写 v1 被 L2
			// 以 stale 拒绝；L2 已有更新的值，这次读取照常返回 loader 的结果。
			name:   "remote_stale",
			local:  func() Store[int, staleValue] { return NewLocalStore(staleConfig()) },
			remote: func() Store[int, staleValue] { return NewLocalStore(staleConfig()) },
			publish: func(ctx context.Context, _, r Store[int, staleValue]) error {
				return r.Set(ctx, staleValue{Key: 1, Version: 2, Payload: "elsewhere"})
			},
			want: staleValue{Key: 1, Version: 1, Payload: "loaded"}, wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			local := tc.local()
			var remote Store[int, staleValue]
			if tc.remote != nil {
				remote = tc.remote()
			}
			loader := func(ctx context.Context, key int) (staleValue, bool, error) {
				if tc.publish != nil {
					if err := tc.publish(ctx, local, remote); err != nil {
						t.Errorf("concurrent publish: %v", err)
					}
				}
				return staleValue{Key: key, Version: 1, Payload: "loaded"}, true, nil
			}
			store := NewReadThroughStore[int, staleValue](local, remote, loader, staleConfig(), ReadThroughOptions{})
			got, ok, err := store.Get(ctx, 1)
			t.Logf("Get = %+v ok=%v err=%v", got, ok, err)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || ok || got != (staleValue{}) {
					t.Fatalf("want refusal %v without a value, got %+v ok=%v err=%v", tc.wantErr, got, ok, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read failed with a write admission error: %v", err)
			}
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("Get = %+v ok=%v, want %+v ok=%v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

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

func TestReadThroughReturnsACanceledWaitersSlot(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	store := NewReadThroughStore[int, int](nil, nil, func(context.Context, int) (int, bool, error) {
		close(started)
		<-release
		return 7, true, nil
	}, StoreConfig[int, int]{KeyOf: func(v int) int { return v }}, ReadThroughOptions{MaxWaitersPerKey: 1})
	go func() { defer close(finished); _, _, _ = store.Get(context.Background(), 7) }()
	<-started
	defer func() { close(release); <-finished }()

	wait := func() (context.CancelFunc, chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, _, err := store.Get(ctx, 7); done <- err }()
		return cancel, done
	}
	awaitCoalesced := func(n uint64, done chan error) {
		for store.Stats().Coalesced < n {
			select {
			case err := <-done:
				t.Fatalf("waiter %d refused: %v", n, err)
			default:
				runtime.Gosched()
			}
		}
	}
	cancelFirst, first := wait()
	awaitCoalesced(1, first)
	// The slot is taken: a second live follower is refused.
	if _, _, err := store.Get(context.Background(), 7); !errors.Is(err, ErrLoadWaitersExceeded) {
		t.Fatalf("second follower with the slot taken = %v, want ErrLoadWaitersExceeded", err)
	}
	cancelFirst()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled follower = %v", err)
	}
	// The follower left, so the slot is free again.
	cancelNext, next := wait()
	awaitCoalesced(2, next)
	cancelNext()
	if err := <-next; !errors.Is(err, context.Canceled) {
		t.Fatalf("replacement follower = %v", err)
	}
}
