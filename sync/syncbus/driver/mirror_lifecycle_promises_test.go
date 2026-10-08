package driver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"
)

// N05：正式 JetStreamSyncBus 适配器上的 Stop 只退订，不排空已进入的 Store。
// 允许旧在途完成；重订阅、同 topic 其他订阅和最后一次退订的资源责任须保持正确。
type lifecycleMirrorStore struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	version int64
}

func lifecycleMirrorMessage(t *testing.T, version int64) []byte {
	t.Helper()
	env, err := json.Marshal(mirror.Envelope{Topic: "state", Key: 1, Version: version, Op: mirror.OpUpsert})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(fsyncbus.SyncMsg{Topic: "state", Key: 1, Version: version, FromSid: 99, Data: env})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func (s *lifecycleMirrorStore) ApplyReplica(_ context.Context, e mirror.Envelope) error {
	if e.Version == 1 {
		close(s.entered)
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Version > s.version {
		s.version = e.Version
	}
	return nil
}
func TestMirrorStopAndRestartWithOldDeliveryInFlight(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	b, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Stop)
	s := &lifecycleMirrorStore{entered: make(chan struct{}), release: make(chan struct{})}
	r := mirror.New(b, "state", s)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	oldMessage := lifecycleMirrorMessage(t, 1)
	oldDone := make(chan error, 1)
	go func() { oldDone <- js.deliver("roost.sync.state", oldMessage) }()
	select {
	case <-s.entered:
	case <-time.After(time.Second):
		t.Fatal("old callback did not enter")
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(s.release) }) })
	stopped := make(chan struct{})
	go func() { r.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop unexpectedly waited for an in-flight handler")
	}
	select {
	case <-oldDone:
		t.Fatal("old callback finished before release")
	default:
	}
	if js.subs[0].stops.Load() != 1 {
		t.Fatal("last subscriber did not release underlying consumer")
	}
	// RR-25：退役代还在执行时重订必须明确失败，不能把新 consumer 开在旧回调旁边。
	if err := r.Start(); !errors.Is(err, fsyncbus.ErrSubscriptionBusy) {
		t.Fatalf("restart during retirement = %v", err)
	}
	if len(js.subs) != 1 {
		t.Fatal("retirement started another consumer")
	}
	releaseOnce.Do(func() { close(s.release) })
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	waitRetired(t, b, "state")
	for range 2 {
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if len(js.subs) != 2 {
		t.Fatalf("restart not idempotent: subscriptions=%d", len(js.subs))
	}
	if err := js.deliver("roost.sync.state", lifecycleMirrorMessage(t, 2)); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	version := s.version
	s.mu.Unlock()
	if version != 2 {
		t.Fatalf("new delivery lost to old callback: version=%d", version)
	}
	r.Stop()
	r.Stop()
	if js.subs[1].stops.Load() != 1 {
		t.Fatal("repeated Stop did not release exactly once")
	}
}
