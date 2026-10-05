package driver

import (
	"context"
	"encoding/json"
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
	defer b.Stop()
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
	for range 2 {
		if err := r.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if len(js.subs) != 2 {
		t.Fatalf("restart not idempotent: subscriptions=%d", len(js.subs))
	}
	// 保留旧 transport 回调与新回调的不同身份；新版消息先到仍须由 Store 保持版本准入。
	// FromSid 显式为远端，避免自回环过滤；正式 Replicator 检查内外身份。
	data := lifecycleMirrorMessage(t, 2)
	if err := js.deliver("roost.sync.state", data); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(s.release) })
	select {
	case err := <-oldDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old callback did not finish")
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
