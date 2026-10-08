//go:build integration

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// RR-25：使用真实 broker 的 durable、NumDelivered、NAK 延迟和终止通知；
// 只在 transport 已收包、尚未进入 SyncBus 准入的边界暂停，制造真实退订窗口。
type retirementDelivery struct {
	msg     *fnats.JetStreamMsg
	release chan struct{}
	at      time.Time
}
type observedRetirementJS struct {
	fnats.IJetStream
	ctx      context.Context
	received chan retirementDelivery
}

func (j *observedRetirementJS) Subscribe(ctx context.Context, c fnats.JetStreamConsumerConfig, h fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	return j.IJetStream.Subscribe(ctx, c, func(ctx context.Context, m *fnats.JetStreamMsg) error {
		d := retirementDelivery{msg: m, release: make(chan struct{}), at: time.Now()}
		select {
		case j.received <- d:
		case <-j.ctx.Done():
			return j.ctx.Err()
		}
		select {
		case <-d.release:
			return h(ctx, m)
		case <-j.ctx.Done():
			return j.ctx.Err()
		}
	})
}
func TestRealRR25BoundedRetirement(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("requires private ROOST_DATAENGINE_IT_NATS_URL")
	}
	for _, limit := range []int{1, 2, 5} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) { testRealRetirement(t, url, limit, false) })
	}
	t.Run("recover_before_limit", func(t *testing.T) { testRealRetirement(t, url, 5, true) })
}
func testRealRetirement(t *testing.T, url string, limit int, recoverBeforeLimit bool) {
	a, err := natsdriver.Assemble(fnats.DefaultConfig(url), natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	nc, err := gonats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	api, err := gojs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	observed := &observedRetirementJS{IJetStream: a.JetStream, ctx: ctx, received: make(chan retirementDelivery, 16)}
	prefix := fmt.Sprintf("rr25.%d", time.Now().UnixNano())
	b, err := NewJetStreamSyncBus(ctx, observed, JetStreamSyncConfig{LocalSid: 7, Prefix: prefix, MaxDeliver: limit, Storage: fnats.JetStreamStorageMemory})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := b.StopWithContext(stopCtx); err != nil {
			t.Error(err)
		}
		_ = api.DeleteStream(context.Background(), b.cfg.Stream)
	}()
	durable := durableSyncName(prefix, "state", 7)
	advisory, err := nc.SubscribeSync("$JS.EVENT.ADVISORY.CONSUMER.MSG_TERMINATED." + b.cfg.Stream + "." + durable)
	if err != nil {
		t.Fatal(err)
	}
	defer advisory.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := func(context.Context, *fsyncbus.SyncMsg) error { calls.Add(1); return nil }
	local, err := b.Subscribe("state", handler)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := api.Consumer(ctx, b.cfg.Stream, durable)
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.MaxDeliver != limit {
		t.Fatalf("broker MaxDeliver=%d", info.Config.MaxDeliver)
	}
	if err := b.Publish(&fsyncbus.SyncMsg{Topic: "state", Key: 1, Version: 1, FromSid: 99}); err != nil {
		t.Fatal(err)
	}
	var previous time.Time
	attempts := limit
	if recoverBeforeLimit {
		attempts = 2
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		var d retirementDelivery
		select {
		case d = <-observed.received:
		case <-time.After(15 * time.Second):
			t.Fatal("delivery timed out")
		}
		if d.msg.NumDelivered != uint64(attempt) {
			close(d.release)
			t.Fatalf("NumDelivered=%d want %d", d.msg.NumDelivered, attempt)
		}
		if attempt > 1 {
			minimum := time.Second * time.Duration(1<<uint(attempt-2))
			if d.at.Sub(previous) < minimum-100*time.Millisecond {
				close(d.release)
				t.Fatalf("redelivery too early: %s want >=%s", d.at.Sub(previous), minimum)
			}
		}
		previous = d.at
		t.Logf("limit=%d attempt=%d stream_seq=%d", limit, attempt, d.msg.StreamSeq)
		if recoverBeforeLimit && attempt == attempts {
			close(d.release)
			break
		}
		if err := local.Unsubscribe(context.Background()); err != nil {
			close(d.release)
			t.Fatal(err)
		}
		close(d.release)
		waitRetired(t, b, "state")
		if attempt < attempts {
			local, err = b.Subscribe("state", handler)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if recoverBeforeLimit {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			info, err = consumer.Info(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if info.NumAckPending == 0 && calls.Load() == 1 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("recovered message was not handled and acknowledged")
	}
	msg, err := advisory.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("missing terminal advisory: %v", err)
	}
	var event struct {
		StreamSequence uint64 `json:"stream_seq"`
		Deliveries     uint64 `json:"deliveries"`
	}
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		t.Fatal(err)
	}
	if event.Deliveries != uint64(limit) || event.StreamSequence != 1 {
		t.Fatalf("terminal=%+v", event)
	}
	if calls.Load() != 0 {
		t.Fatalf("retired handlers ran %d times", calls.Load())
	}
	local, err = b.Subscribe("state", handler)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-observed.received:
		close(d.release)
		t.Fatal("terminated message redelivered after rebind")
	case <-time.After(1100 * time.Millisecond):
	}
	info, err = consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Delivered.Consumer != uint64(limit) || info.NumAckPending != 0 {
		t.Fatalf("consumer after terminal: deliveries=%d pending=%d", info.Delivered.Consumer, info.NumAckPending)
	}
	if err := local.Unsubscribe(ctx); err != nil {
		t.Fatal(err)
	}
}

// 真实 broker 已投递但连接在 ACK 前断开：同 durable 的下一次准入必须保留服务端计数。
func TestRealRR25UnacknowledgedConnectionRecovery(t *testing.T) {
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Skip("requires private ROOST_DATAENGINE_IT_NATS_URL")
	}
	a, err := natsdriver.Assemble(fnats.DefaultConfig(url), natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	nc, err := gonats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	api, err := gojs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	observed := &observedRetirementJS{IJetStream: a.JetStream, ctx: ctx, received: make(chan retirementDelivery, 4)}
	prefix := fmt.Sprintf("rr25lost.%d", time.Now().UnixNano())
	b, err := NewJetStreamSyncBus(ctx, observed, JetStreamSyncConfig{Prefix: prefix, LocalSid: 7, MaxDeliver: 2, AckWait: 300 * time.Millisecond, Storage: fnats.JetStreamStorageMemory})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		b.Stop()
		if nc.IsClosed() {
			nc, _ = gonats.Connect(url)
			api, _ = gojs.New(nc)
		}
		_ = api.DeleteStream(context.Background(), b.cfg.Stream)
		nc.Close()
	}()
	var calls atomic.Int32
	handler := func(context.Context, *fsyncbus.SyncMsg) error { calls.Add(1); return nil }
	local, err := b.Subscribe("state", handler)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Unsubscribe(ctx); err != nil {
		t.Fatal(err)
	}
	waitRetired(t, b, "state")
	consumer, err := api.Consumer(ctx, b.cfg.Stream, durableSyncName(prefix, "state", 7))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(&fsyncbus.SyncMsg{Topic: "state", Key: 1, FromSid: 99, Version: 1}); err != nil {
		t.Fatal(err)
	}
	batch, err := consumer.Fetch(1, gojs.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	first, ok := <-batch.Messages()
	if !ok {
		t.Fatalf("no first delivery: %v", batch.Error())
	}
	meta, err := first.Metadata()
	if err != nil || meta.NumDelivered != 1 {
		t.Fatalf("first metadata=%+v err=%v", meta, err)
	}
	nc.Close() // 故意不 ACK，保留原 durable 和消息身份。
	if _, err := b.Subscribe("state", handler); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-observed.received:
		if d.msg.NumDelivered != 2 {
			close(d.release)
			t.Fatalf("recovered count=%d", d.msg.NumDelivered)
		}
		close(d.release)
	case <-time.After(5 * time.Second):
		t.Fatal("unacknowledged message not recovered")
	}
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("recovery handler did not run")
	}
}
