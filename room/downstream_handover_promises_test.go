package room

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	coreentitysync "github.com/tjbdwanghaibo/roost-core/entitysync"
	core "github.com/tjbdwanghaibo/roost-core/statesync"
)

// U-0208 · C8 · RR-20260915-05:更换 downstream 是一次生命周期交接,不只是换一个指针。
// NewRoomBroadcaster 会向初始 sink 注册 handleSlowConsumer(默认剔除 → room 退订、释放名额),
// SetDownstream 却只替换 envelopeSink.downstream:新 sink 没有本 room 的回调,旧 sink 的回调一直
// 保留到 room.Close。替换后新 sink 正常剔除并跑完自己的 OnSlowConsumer,room 里的订阅却原封不动。
// 承诺:替换时先在新 sink 上注册(失败则保留旧 sink 与旧回调),成功后切换 downstream、接管 unregister
// 所有权、解除旧注册;同一实例重复设置不重复注册。

func handoverSink(t *testing.T, tr *recordingAtomicTransport, done chan struct{}) *RoomTransportSink {
	t.Helper()
	s, err := NewRoomTransportSink(RoomTransportSinkConfig{
		Transport: tr, CallbackWorkers: 1,
		Sessions: RoomSessionResolverFunc(func(_ context.Context, r coreentitysync.SubscriberRef) (core.SessionID, error) {
			return core.SessionID(r.ID), nil
		}),
		OnSlowConsumer: func(context.Context, RoomSlowConsumer) {
			if done != nil {
				done <- struct{}{}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func handlerCount(s *RoomTransportSink) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.handlers)
}

func TestSetDownstreamPromiseHandsOverTheSlowConsumerHandler(t *testing.T) {
	for _, mode := range []string{"initial_sink", "replace_before_subscribe", "old_handler_detached"} {
		t.Run(mode, func(t *testing.T) {
			done := make(chan struct{}, 2)
			oldTransport, newTransport := &recordingAtomicTransport{}, &recordingAtomicTransport{}
			oldSink := handoverSink(t, oldTransport, done)
			newSink := handoverSink(t, newTransport, done)
			r, err := NewRoomBroadcaster(7, oldSink)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "initial_sink" {
				if err := r.SetDownstream(newSink); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "old_handler_detached" {
				if count := handlerCount(oldSink); count != 0 {
					t.Fatalf("old sink still holds %d room callback after replacement", count)
				}
				if count := handlerCount(newSink); count != 1 {
					t.Fatalf("new sink holds %d room callbacks, want 1", count)
				}
				return
			}
			s := testRoomState(101, nil)
			if err := r.RegisterSubject(s); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Subscribe(context.Background(), coreentitysync.SubscriberRef{ID: 1}, 101, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			active := oldTransport
			if mode != "initial_sink" {
				active = newTransport
			}
			active.slowSession = 1
			s.MarkFullDirty(entity.SyncFullReasonResync)
			if err := r.FlushDirty(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("callback not delivered")
			}
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) && len(r.Subscribers(101)) != 0 {
				time.Sleep(time.Millisecond)
			}
			if subs := r.Subscribers(101); len(subs) != 0 {
				t.Fatalf("new sink callback finished but stale membership=%+v", subs)
			}
		})
	}
}

// 交接的两条边:同一实例重复设置不重复注册;新 sink 注册失败(已关闭)则保留旧 sink 与旧回调。
func TestSetDownstreamPromiseIsIdempotentAndKeepsTheOldSinkOnFailure(t *testing.T) {
	oldSink := handoverSink(t, &recordingAtomicTransport{}, nil)
	r, err := NewRoomBroadcaster(7, oldSink)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetDownstream(oldSink); err != nil {
		t.Fatalf("same instance: %v", err)
	}
	if count := handlerCount(oldSink); count != 1 {
		t.Fatalf("same-instance SetDownstream changed the registration: handlers=%d", count)
	}
	closedSink := handoverSink(t, &recordingAtomicTransport{}, nil)
	if err := closedSink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDownstream(closedSink); err == nil {
		t.Fatal("a sink that refuses the room handler must not be installed")
	}
	if count := handlerCount(oldSink); count != 1 {
		t.Fatalf("failed handover dropped the old registration: handlers=%d", count)
	}
	// 旧 sink 仍是 downstream:剔除仍能清理订阅。
	s := testRoomState(101, nil)
	if err := r.RegisterSubject(s); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Subscribe(context.Background(), coreentitysync.SubscriberRef{ID: 1}, 101, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
}
