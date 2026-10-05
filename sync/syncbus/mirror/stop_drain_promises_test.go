package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// RR-20261005-NC-174：StopWithContext 等已准入的 ApplyReplica 返回；超时如实返回 ctx 错误，重试再等；
// 退订之后迟到的投递（JetStream fanout 用旧快照调用）不再进入 Store。

type blockingStore struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	applied []int64
}

func (s *blockingStore) ApplyReplica(_ context.Context, env Envelope) error {
	if env.Version == 1 {
		close(s.entered)
		<-s.release
	}
	s.mu.Lock()
	s.applied = append(s.applied, env.Version)
	s.mu.Unlock()
	return nil
}

// lingeringBus 的退订不撤掉已经拿到的 handler：模拟传输层在退订后仍交付一条在途消息。
type lingeringBus struct {
	mu      sync.Mutex
	handler fsyncbus.Handler
}

func (*lingeringBus) Publish(*fsyncbus.SyncMsg) error { return nil }
func (b *lingeringBus) Subscribe(_ string, h fsyncbus.Handler) (func(), error) {
	b.mu.Lock()
	b.handler = h
	b.mu.Unlock()
	return func() {}, nil
}
func (b *lingeringBus) last() fsyncbus.Handler {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handler
}

func replicaMsg(t *testing.T, version int64) *fsyncbus.SyncMsg {
	t.Helper()
	raw, err := json.Marshal(Envelope{Topic: "topic", Key: 1, Version: version, Op: OpUpsert})
	if err != nil {
		t.Fatal(err)
	}
	return &fsyncbus.SyncMsg{Topic: "topic", Key: 1, Version: version, Data: raw}
}

func TestReplicatorStopWithContextWaitsForAdmittedHandlers(t *testing.T) {
	bus := &lingeringBus{}
	store := &blockingStore{entered: make(chan struct{}), release: make(chan struct{})}
	rep := New(bus, "topic", store)
	if err := rep.Start(); err != nil {
		t.Fatal(err)
	}
	first := bus.last()
	done := make(chan error, 1)
	go func() { done <- first(replicaMsg(t, 1)) }()
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not enter the store")
	}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(store.release) })

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rep.StopWithContext(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("StopWithContext with a handler still applying = %v, want context.Canceled", err)
	}
	// 退订后迟到的投递不进入 Store。
	if err := first(replicaMsg(t, 2)); err != nil {
		t.Fatalf("late delivery after stop = %v", err)
	}
	// 停止之后重新启动：新订阅照常接收，旧订阅的在途 handler 仍由之后的 StopWithContext 等待。
	if err := rep.Start(); err != nil {
		t.Fatal(err)
	}
	if err := bus.last()(replicaMsg(t, 3)); err != nil {
		t.Fatal(err)
	}
	if err := rep.StopWithContext(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry StopWithContext before the old handler returned = %v, want context.Canceled", err)
	}
	releaseOnce.Do(func() { close(store.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := rep.StopWithContext(context.Background()); err != nil {
		t.Fatalf("StopWithContext after the handler returned = %v", err)
	}
	if err := rep.StopWithContext(expired); err != nil {
		t.Fatalf("StopWithContext after a completed drain = %v, want nil", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.applied) != 2 || store.applied[0] != 3 || store.applied[1] != 1 {
		t.Fatalf("applied versions = %v, want [3 1] (the late v2 must not reach the store)", store.applied)
	}
}
