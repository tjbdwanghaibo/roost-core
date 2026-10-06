package driver

import (
	"context"
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// Mirror 第 4 步（docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md）：可确认订阅。
//
// 承诺：SubscribeLive 用 DeliverNew 的 durable 消费者（返回即确认：之后入流的消息都会投递，之前的历史
// 不重放）；它的 durable 名与同主题的普通订阅（DeliverAll）分开——服务端不允许改已有 durable 的投递
// 策略，共用名字会让已部署环境建消费者失败；两种订阅的本地 handler 互不共享。普通 NATS 总线不提供
// 这项能力。真实 JetStream 上的行为见 remoteentity 的 TestRealJetStreamLiveSubscription*。
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
