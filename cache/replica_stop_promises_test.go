package cache

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// RR-20261006-36：ReplicaSyncer.Stop 曾不等在途的 Store 写入（修前红文本见 docs/bug/RR-20261006-36.md）。
// 承诺（A3 ② 之后）：Stop(ctx) 在 Store 写入返回前不返回 nil，超时返回 ctx 错误、重试再等。
type blockingItemStore struct {
	Store[int64, testItem]
	entered, release, returned chan struct{}
}

func (s blockingItemStore) Set(context.Context, testItem) error {
	close(s.entered)
	<-s.release
	close(s.returned)
	return nil
}

func TestReplicaSyncerStopWaitsForInFlightStoreWrite(t *testing.T) {
	bus := newFakeSyncBus()
	store := blockingItemStore{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
	s := NewReplicaSyncer(bus, ReplicaConfig[int64, testItem]{Store: store, Topic: "items", KeyOf: func(v testItem) int64 { return v.ID }})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(testItem{ID: 7})
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
