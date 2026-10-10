package driver

import (
	"context"
	"encoding/json"
	"errors"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDurableSyncNamePromiseDistinguishesPrefixes(t *testing.T) {
	one := newFakeJetStream()
	two := newFakeJetStream()
	busOne, err := NewJetStreamSyncBus(context.Background(), one, JetStreamSyncConfig{LocalSid: 7, Prefix: "one", Stream: "SHARED"})
	if err != nil {
		t.Fatal(err)
	}
	defer busOne.Stop()
	busTwo, err := NewJetStreamSyncBus(context.Background(), two, JetStreamSyncConfig{LocalSid: 7, Prefix: "two", Stream: "SHARED"})
	if err != nil {
		t.Fatal(err)
	}
	defer busTwo.Stop()
	noop := func(context.Context, *fsyncbus.SyncMsg) error { return nil }
	if _, err := busOne.Subscribe("state", noop); err != nil {
		t.Fatal(err)
	}
	if _, err := busTwo.Subscribe("state", noop); err != nil {
		t.Fatal(err)
	}
	a, b := one.consumers[0], two.consumers[0]
	if a.Stream == b.Stream && a.Durable == b.Durable && a.FilterSubject != b.FilterSubject {
		t.Fatalf("same (stream, durable) %q/%q for different subjects %q vs %q", a.Stream, a.Durable, a.FilterSubject, b.FilterSubject)
	}
}

func TestDurableSyncNamePromiseKeepsTheDefaultPrefixNameStable(t *testing.T) {
	// 审查记录的、升级前的名字:默认 Prefix 下必须逐字不变,否则已部署消费者的 ACK 游标被抛弃。
	const legacy = "sync_state_7_aeabf91c58d07db1"
	cfg := normalizeJetStreamSyncConfig(JetStreamSyncConfig{LocalSid: 7})
	if got := durableSyncName(cfg.Prefix, "state", 7); got != legacy {
		t.Fatalf("default-prefix durable name changed: %q want %q", got, legacy)
	}
	if got := durableSyncName("one", "state", 7); got == legacy {
		t.Fatal("a non-default prefix must not collide with the default identity")
	}
	if durableSyncName("one", "state", 7) != durableSyncName("one", "state", 7) {
		t.Fatal("durable name must be stable across calls")
	}
}

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
	unsubFirst, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
		first = append(first, m.Key)
		m.Data[0] = 'X'
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	unsubSecond, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
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
	if err := unsubFirst.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	if err := unsubSecond.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := unsubSecond.Unsubscribe(context.Background()); err != nil { // 幂等
		t.Fatal(err)
	}
	if js.subs[0].stops.Load() != 1 {
		t.Fatalf("last unsubscribe must stop the shared subscription exactly once: stops=%d", js.subs[0].stops.Load())
	}
	if _, err := bus.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil }); err != nil {
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
	if _, err := bus.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { panic("boom") }); err != nil {
		t.Fatal(err)
	}
	got := 0
	if _, err := bus.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { got++; return nil }); err != nil {
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
			// RR-25：创建中的同 topic 返回 Busy；重试仍必须复用唯一 consumer。
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := bus.Subscribe("state", func(context.Context, *fsyncbus.SyncMsg) error { return nil })
				if err == nil {
					return
				}
				if !errors.Is(err, fsyncbus.ErrSubscriptionBusy) || time.Now().After(deadline) {
					t.Error(err)
					return
				}
				runtime.Gosched()
			}
		}()
	}
	wg.Wait()
	if got := len(js.consumers); got != 1 {
		t.Fatalf("concurrent first subscribers created %d consumers, want 1", got)
	}
}

func TestJetStreamSubscribeLiveUsesASeparateDeliverNewDurable(t *testing.T) {
	js := newFakeJetStream()
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7, Stream: "ROOST_SYNC_TEST", Storage: fnats.JetStreamStorageMemory})
	if err != nil {
		t.Fatal(err)
	}
	var live fsyncbus.ILiveSubscriber = bus
	nop := func(context.Context, *fsyncbus.SyncMsg) error { return nil }
	unsubAll, err := bus.Subscribe("remote_entity_snapshot", nop)
	if err != nil {
		t.Fatal(err)
	}
	unsubLive, err := live.SubscribeLive("remote_entity_snapshot", nop)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.SubscribeLive("remote_entity_snapshot", nop); err != nil {
		t.Fatal(err)
	}
	if len(js.consumers) != 2 {
		t.Fatalf("consumers=%d; want one DeliverAll and one DeliverNew durable for the topic (the second live subscriber fans out locally)", len(js.consumers))
	}
	all, fresh := js.consumers[0], js.consumers[1]
	if all.DeliverPolicy != fnats.JetStreamDeliverAll || fresh.DeliverPolicy != fnats.JetStreamDeliverNew {
		t.Fatalf("deliver policies all=%q live=%q", all.DeliverPolicy, fresh.DeliverPolicy)
	}
	if all.Durable == fresh.Durable {
		t.Fatalf("the live consumer reuses the DeliverAll durable %q; the server refuses to change a durable's deliver policy", all.Durable)
	}
	if all.FilterSubject != fresh.FilterSubject {
		t.Fatalf("filter subjects differ: %q vs %q", all.FilterSubject, fresh.FilterSubject)
	}
	if bus.topics["remote_entity_snapshot"] == bus.topics["remote_entity_snapshot\x00live"] {
		t.Fatal("the two subscription kinds share one local fanout")
	}
	if err := unsubAll.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := unsubLive.Unsubscribe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := bus.topics["remote_entity_snapshot\x00live"]; !ok {
		t.Fatal("removing one of two live subscribers released the shared live consumer")
	}

	var plain any = NewNatsSyncBus(nil, 7, "roost.sync")
	if _, ok := plain.(fsyncbus.ILiveSubscriber); ok {
		t.Fatal("the plain NATS bus is at-most-once and must not claim confirmed subscriptions")
	}
}

func stopWaitingIn(t *testing.T, stopped <-chan error, cancel context.CancelFunc, waitingFrame string) (err error, waited bool) {
	t.Helper()
	buf := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-stopped:
			return err, false
		default:
		}
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), waitingFrame) {
			if cancel == nil {
				return nil, true // 不限时的停止：确认它在等即可，由调用方放行 handler
			}
			cancel()
			select {
			case err := <-stopped:
				return err, true
			case <-time.After(5 * time.Second):
				t.Fatal("stop ignored its context while waiting for the handler")
			}
		}
		runtime.Gosched()
	}
	t.Fatal("stop neither returned nor waited")
	return nil, false
}

func TestJetStreamSyncBusStopWaitsForAnInFlightHandler(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var handled []int64
	var handledMu sync.Mutex
	if _, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
		if m.Key == 1 {
			close(entered)
			<-release
		}
		handledMu.Lock()
		handled = append(handled, m.Key)
		handledMu.Unlock()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan error, 1)
	go func() { delivered <- js.deliver("roost.sync.state", fanoutMsg(t, 1)) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not enter")
	}

	// 修前唯一的停止入口 Stop()：在途 handler 返回之前不能返回。
	stopped := make(chan error, 1)
	go func() { bus.Stop(); stopped <- nil }()
	if _, waited := stopWaitingIn(t, stopped, nil, "jetStreamSyncBus).StopWithContext"); !waited {
		t.Fatal("Stop returned while a handler was still running on the subscription")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after the handler finished")
	}
	if js.subs[0].stops.Load() != 1 {
		t.Fatalf("subscription stops=%d, want 1", js.subs[0].stops.Load())
	}
}

func TestJetStreamSyncBusStopWithContextIsBoundedAndRetryable(t *testing.T) {
	js := &countingJetStream{fakeJetStream: newFakeJetStream()}
	bus, err := NewJetStreamSyncBus(context.Background(), js, JetStreamSyncConfig{LocalSid: 7})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	calls := 0
	if _, err := bus.Subscribe("state", func(_ context.Context, m *fsyncbus.SyncMsg) error {
		calls++
		if m.Key == 1 {
			close(entered)
			<-release
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan error, 1)
	go func() { delivered <- js.deliver("roost.sync.state", fanoutMsg(t, 1)) }()
	<-entered

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bus.StopWithContext(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("StopWithContext with a handler running = %v, want context.Canceled", err)
	}
	// 停止开始后到达的投递交还 broker（返回错误 → driver NAK），不进业务。
	if err := js.deliver("roost.sync.state", fanoutMsg(t, 2)); !errors.Is(err, errJetStreamSyncStopping) {
		t.Fatalf("delivery after stop = %v, want errJetStreamSyncStopping", err)
	}
	if _, err := bus.Subscribe("other", func(context.Context, *fsyncbus.SyncMsg) error { return nil }); err == nil {
		t.Fatal("Subscribe after stop created a consumer whose deliveries could never be admitted")
	}
	if err := bus.StopWithContext(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry StopWithContext before the handler returned = %v, want context.Canceled", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	if err := bus.StopWithContext(context.Background()); err != nil {
		t.Fatalf("StopWithContext after the handler returned = %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	if js.subs[0].stops.Load() != 1 {
		t.Fatalf("subscription stopped %d times, want 1", js.subs[0].stops.Load())
	}
}
