package room

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
)

// U-0209 · C8 · RR-20260916-03:同一总线对同一 topic 的多个本地订阅是**广播**,不是分摊。
// 旧实现每次 Subscribe 都用相同 topic/SID 生成同一个持久消费者名,再各自 Consume:真实服务端把
// 消息在两个 Consume 实例之间分摊(实测 6/6),分片流两边都重组不出 Packet;普通 NATS 总线用普通
// Subscribe 是广播语义。承诺:一个 topic 一个底层订阅 + 本地 handler 注册表——第一位创建消费者,
// 后来者只登记 handler;单个退订只删自己,最后一位离开才停底层订阅;handler 之间消息所有权隔离、
// panic 隔离。保留稳定的消费者身份与重启游标。

type countingJetStreamSub struct{ stops atomic.Int32 }

func (s *countingJetStreamSub) Stop()  { s.stops.Add(1) }
func (s *countingJetStreamSub) Drain() { s.Stop() }
func (s *countingJetStreamSub) Closed() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

type countingJetStream struct {
	*fakeJetStream
	subs []*countingJetStreamSub
}

func (f *countingJetStream) Subscribe(ctx context.Context, cfg fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	if _, err := f.fakeJetStream.Subscribe(ctx, cfg, handler); err != nil {
		return nil, err
	}
	sub := &countingJetStreamSub{}
	f.subs = append(f.subs, sub)
	return sub, nil
}

func fanoutMsg(t *testing.T, key int64) []byte {
	t.Helper()
	data, err := json.Marshal(&fsyncbus.SyncMsg{Topic: "state", Key: key, Version: 1, FromSid: 99, Data: []byte("payload")})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestJetStreamSyncBusPromiseSameTopicSubscribersAllReceive(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Stop()
	var first, second []int64
	unsubFirst, err := bus.Subscribe("state", func(m *fsyncbus.SyncMsg) error { first = append(first, m.Key); m.Data[0] = 'X'; return nil })
	if err != nil {
		t.Fatal(err)
	}
	unsubSecond, err := bus.Subscribe("state", func(m *fsyncbus.SyncMsg) error {
		second = append(second, m.Key)
		if string(m.Data) != "payload" {
			t.Fatalf("handler saw another handler's mutation: %q", m.Data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(js.consumers); got != 1 {
		t.Fatalf("two local subscribers created %d durable consumers, want 1 shared", got)
	}
	for key := int64(1); key <= 4; key++ {
		if err := js.deliver("roost.sync.state", fanoutMsg(t, key)); err != nil {
			t.Fatal(err)
		}
	}
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("broadcast became work-sharing: first=%v second=%v", first, second)
	}
	// 退订一个:另一个继续收,底层订阅不停。
	unsubFirst()
	if err := js.deliver("roost.sync.state", fanoutMsg(t, 5)); err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 || len(second) != 5 {
		t.Fatalf("after one unsubscribe: first=%v second=%v", first, second)
	}
	if js.subs[0].stops.Load() != 0 {
		t.Fatal("underlying subscription stopped while a local handler remained")
	}
	// 最后一位离开:底层订阅停止;再订阅重新创建。
	unsubSecond()
	unsubSecond() // 幂等
	if js.subs[0].stops.Load() != 1 {
		t.Fatalf("last unsubscribe must stop the shared subscription exactly once: stops=%d", js.subs[0].stops.Load())
	}
	if _, err := bus.Subscribe("state", func(*fsyncbus.SyncMsg) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := len(js.consumers); got != 2 {
		t.Fatalf("re-subscribe after the last unsubscribe must recreate the consumer: consumers=%d", got)
	}
}

func TestJetStreamSyncBusPromiseHandlerPanicIsIsolated(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Stop()
	if _, err := bus.Subscribe("state", func(*fsyncbus.SyncMsg) error { panic("boom") }); err != nil {
		t.Fatal(err)
	}
	got := 0
	if _, err := bus.Subscribe("state", func(*fsyncbus.SyncMsg) error { got++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := js.deliver("roost.sync.state", fanoutMsg(t, 1)); err != nil {
		t.Fatalf("a panicking sibling handler must not fail the delivery: %v", err)
	}
	if got != 1 {
		t.Fatalf("sibling handler did not run after a panic: got=%d", got)
	}
}

func TestJetStreamSyncBusPromiseConcurrentFirstSubscribersShareOneConsumer(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Stop()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := bus.Subscribe("state", func(*fsyncbus.SyncMsg) error { return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := len(js.consumers); got != 1 {
		t.Fatalf("concurrent first subscribers created %d consumers, want 1", got)
	}
}
