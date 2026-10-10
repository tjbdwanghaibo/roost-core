package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
	"sync"
	"testing"
	"time"
)

func TestReplicaDeleteAndUpsertShareVersionOrder(t *testing.T) {
	for _, order := range [][]int{{3, -2}, {3, -4, 3}, {-4, 3}} {
		t.Run(fmtOrder(order), func(t *testing.T) {
			bus := newFakeSyncBus()
			store := NewReplicaLocalStore[int64, testItem](testItemConfig(), func(v testItem) int64 { return v.Version }, 10000)
			s := NewReplicaSyncer(bus, ReplicaConfig[int64, testItem]{Store: store, Topic: "test", KeyOf: func(v testItem) int64 { return v.ID }, VersionOf: func(v testItem) int64 { return v.Version }, DeleteKeyOf: func(k int64) int64 { return k }})
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			defer s.Stop(context.Background())
			for _, v := range order {
				if v < 0 {
					_ = s.PublishDelete(context.Background(), 7, int64(-v))
				} else {
					_ = s.Publish(context.Background(), testItem{ID: 7, Version: int64(v)})
				}
			}
			got, ok, err := store.Get(context.Background(), 7)
			want := order[1] == -2
			if err != nil || ok != want || (want && got.Version != 3) {
				t.Fatalf("version order %v left value=%+v present=%v; want present=%v", order, got, ok, want)
			}
		})
	}
}
func fmtOrder(v []int) string { return fmt.Sprint(v) }
func TestReplicaRejectsMissingIdentityExtractors(t *testing.T) {
	for _, missing := range []string{"key", "version", "delete"} {
		t.Run(missing, func(t *testing.T) {
			c := ReplicaConfig[int64, testItem]{Store: NewReplicaLocalStore[int64, testItem](testItemConfig(), func(v testItem) int64 { return v.Version }, 10000), Topic: "test", KeyOf: func(v testItem) int64 { return v.ID }, VersionOf: func(v testItem) int64 { return v.Version }, DeleteKeyOf: func(k int64) int64 { return k }}
			switch missing {
			case "key":
				c.KeyOf = nil
			case "version":
				c.VersionOf = nil
			case "delete":
				c.DeleteKeyOf = nil
			}
			s := NewReplicaSyncer(newFakeSyncBus(), c)
			if err := s.Start(); err == nil {
				t.Fatal("invalid replica configuration started successfully")
			}
		})
	}
}

func TestReplicaLocalStoreRetainsDeleteWatermarkAtCapacity(t *testing.T) {
	ctx := context.Background()
	store := NewReplicaLocalStore[int64, testItem](testItemConfig(), func(v testItem) int64 { return v.Version }, 1)
	if err := store.ApplyReplicaValue(ctx, 7, testItem{}, 4, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, testItem{ID: 8, Version: 1}); !errors.Is(err, ErrReplicaCapacity) {
		t.Fatalf("capacity error=%v", err)
	}
	if err := store.Set(ctx, testItem{ID: 7, Version: 3}); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("tombstone was evicted: %v", err)
	}
	if err := store.Set(ctx, testItem{ID: 7, Version: 4}); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("same-version delete lost: %v", err)
	}
	if err := store.Set(ctx, testItem{ID: 7, Version: 5}); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(ctx, testItem{ID: 7, Version: 5, Data: "conflict"}); !errors.Is(err, ErrConflictingWrite) {
		t.Fatalf("conflict=%v", err)
	}
}
func TestReplicaLocalStoreConcurrentDeletesAndWrites(t *testing.T) {
	ctx := context.Background()
	store := NewReplicaLocalStore[int64, testItem](testItemConfig(), func(v testItem) int64 { return v.Version }, 1)
	var wg sync.WaitGroup
	for v := int64(1); v <= 100; v++ {
		wg.Go(func() {
			_ = store.Set(ctx, testItem{ID: 7, Version: v})
			_ = store.ApplyReplicaValue(ctx, 7, testItem{}, v, true)
		})
	}
	wg.Wait()
	if _, ok, err := store.Get(ctx, 7); err != nil || ok {
		t.Fatalf("latest delete did not win: %v %v", ok, err)
	}
	if err := store.Set(ctx, testItem{ID: 7, Version: 100}); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("latest watermark missing: %v", err)
	}
}

func TestReplicaPayloadIdentityBeforeStoreMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value testItem
	}{
		{"key", testItem{ID: 8, Version: 3, Data: "bad"}},
		{"version", testItem{ID: 7, Version: 4, Data: "bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			bus := newFakeSyncBus()
			store := NewReplicaLocalStore[int64, testItem](testItemConfig(), func(v testItem) int64 { return v.Version }, 10000)
			for _, id := range []int64{7, 8} {
				if err := store.Set(ctx, testItem{ID: id, Version: 2, Data: "before"}); err != nil {
					t.Fatal(err)
				}
			}
			s := NewReplicaSyncer(bus, ReplicaConfig[int64, testItem]{Store: store, Topic: "identity", KeyOf: func(v testItem) int64 { return v.ID }, VersionOf: func(v testItem) int64 { return v.Version }, DeleteKeyOf: func(k int64) int64 { return k }})
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Stop(context.Background()) }()
			raw, err := mirror.MarshalPayload(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			// 通过真实 Replicator：SyncMsg 与 Envelope 两层身份完全一致。
			err = mirror.New(bus, "identity", nil).Publish(ctx, mirror.Envelope{Key: 7, Version: 3, Payload: raw})
			for _, id := range []int64{7, 8} {
				v, ok, readErr := store.Get(ctx, id)
				if readErr != nil || !ok || v.Data != "before" || v.Version != 2 {
					t.Errorf("refused message changed key %d: %+v ok=%v err=%v", id, v, ok, readErr)
				}
			}
			if err == nil {
				t.Error("payload identity mismatch returned success")
			}
			if err := s.Publish(ctx, testItem{ID: 7, Version: 3, Data: "after"}); err != nil {
				t.Fatal(err)
			}
			v, ok, err := store.Get(ctx, 7)
			if err != nil || !ok || v.Data != "after" {
				t.Fatalf("honest recovery: %+v %v %v", v, ok, err)
			}
		})
	}
}

// 写前身份校验不能绕过原 Store 对 nil 的拒绝而引入 callback panic。
func TestReplicaNullPointerRefusedBeforeIdentityCallback(t *testing.T) {
	store := NewReplicaLocalStore[int64, *testItem](StoreConfig[int64, *testItem]{
		KeyOf: func(v *testItem) int64 { return v.ID },
		ValidateValue: func(v *testItem) error {
			if v == nil {
				return ErrInvalidKey
			}
			return nil
		},
	}, func(v *testItem) int64 { return v.Version }, 10000)
	bus := newFakeSyncBus()
	s := NewReplicaSyncer(bus, ReplicaConfig[int64, *testItem]{Store: store, Topic: "pointer", KeyOf: func(v *testItem) int64 { return v.ID }, VersionOf: func(v *testItem) int64 { return v.Version }, DeleteKeyOf: func(k int64) int64 { return k }})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop(context.Background()) }()
	if err := mirror.New(bus, "pointer", nil).Publish(context.Background(), mirror.Envelope{Key: 7, Version: 1, Payload: []byte(" null ")}); err == nil {
		t.Fatal("null pointer must be refused")
	}
	if err := s.Publish(context.Background(), &testItem{ID: 7, Version: 1, Data: "after"}); err != nil {
		t.Fatal(err)
	}
	v, ok, err := store.Get(context.Background(), 7)
	if err != nil || !ok || v.Data != "after" {
		t.Fatalf("pointer recovery: %+v %v %v", v, ok, err)
	}
}

type blockingItemStore struct {
	Store[int64, testItem]
	entered, release, returned chan struct{}
}

func (s blockingItemStore) ApplyReplicaValue(context.Context, int64, testItem, int64, bool) error {
	close(s.entered)
	<-s.release
	close(s.returned)
	return nil
}

func TestReplicaSyncerStopWaitsForInFlightStoreWrite(t *testing.T) {
	bus := newFakeSyncBus()
	store := blockingItemStore{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	s := NewReplicaSyncer(bus, ReplicaConfig[int64, testItem]{Store: store, Topic: "items", KeyOf: func(v testItem) int64 { return v.ID }, VersionOf: func(v testItem) int64 { return v.Version }, DeleteKeyOf: func(k int64) int64 { return k }})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(testItem{ID: 7, Version: 1})
	raw, _ := json.Marshal(mirror.Envelope{Key: 7, Version: 1, Op: mirror.OpUpsert, Payload: payload})
	sub := bus.handlers["items"][0]
	go func() {
		_ = sub.Deliver(context.Background(), &fsyncbus.SyncMsg{Topic: "items", Key: 7, Version: 1, Data: raw})
	}()
	<-store.entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with a Store write in flight = %v, want DeadlineExceeded", err)
	}
	close(store.release)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop = %v, want nil", err)
	}
	select {
	case <-store.returned:
	default:
		t.Fatal("Stop returned nil while the Store write was still running")
	}
}
