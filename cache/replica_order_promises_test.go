package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
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
