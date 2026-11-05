//go:build integration

package dataengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// SubscribeEffects 在真实依赖上（D12 方向 A）：DataEngine 提交带 effect 的记录 → outbox 发布到本次独有的效果流（三副本）
// → SubscribeEffects 的 durable 收到 → handler 在 Mongo 事务里写业务文档，与回执原子提交。核对：
//   - 每个 effect 的业务写恰好一份（handler 经事务 ctx 写 mails，回执与它同一事务）；
//   - 同一 EffectID 不同 MsgID 再发布一次（模拟 outbox 重发越过 JetStream 去重窗口）：读到回执跳过，handler 不再执行；
//   - 毒消息（载荷解不开）Term 后不再重投；
//   - handler 失败一次的 effect 按退避重投后成功。
//
// 资源：realFixture 的库 roost_it_<pid>_<n>、流 ROOST_IT_EFFECTS_<pid>_<n>，用后删除；durable 随流删除。
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh，持有 remote-acceptance.lock，
//
//	ROOST_DATAENGINE_IT=1 GOWORK=off go test -tags integration -count=1 -run '^TestRealSubscribeEffects' ./wiring/dataengine/
func TestRealSubscribeEffectsDeliversEachEffectOnce(t *testing.T) {
	fx := newRealFixture(t)
	durable := fmt.Sprintf("d12-effects-%d", os.Getpid())
	var mu sync.Mutex
	runs := map[string]int{}
	failedOnce := false
	consumer := EffectConsumer{Durable: durable, Topic: "player.level_up", NakBackoffMin: 50 * time.Millisecond, NakBackoffMax: 200 * time.Millisecond}
	sub, err := SubscribeEffects(fx.context(), fx.registry, consumer, func(ctx context.Context, envelope nestwal.EffectEnvelope) error {
		mu.Lock()
		runs[envelope.EffectID]++
		fail := envelope.EffectID == "effect-flaky" && !failedOnce
		if fail {
			failedOnce = true
		}
		mu.Unlock()
		var event struct{ Level int }
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return PoisonEffect(err)
		}
		if fail {
			return errors.New("mail: unavailable")
		}
		_, err := fx.mongo.Database(fx.database).Collection("d12_mails").InsertOne(ctx, bson.M{"_id": envelope.EffectID, "level": event.Level})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Drain()

	record := realRecord(41, nil)
	record.Effects = []coredata.Effect{
		{ID: "effect-ok", Topic: "player.level_up", Key: "101", Payload: []byte(`{"level":2}`)},
		{ID: "effect-poison", Topic: "player.level_up", Key: "102", Payload: []byte(`not json`)},
		{ID: "effect-flaky", Topic: "player.level_up", Key: "103", Payload: []byte(`{"level":3}`)},
	}
	ticket, err := fx.runtime.Projector.CommitSystem(fx.context(), record)
	if err != nil {
		t.Fatal(err)
	}
	if err := coredata.WaitProjection(fx.context(), ticket); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "both good effects mailed", func() bool {
		count, err := fx.mongo.Database(fx.database).Collection("d12_mails").CountDocuments(fx.context(), bson.M{})
		return err == nil && count == 2
	})
	// 同一 EffectID 以新的 MsgID 再发布：越过 JetStream 去重，靠回执跳过。
	raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: record.ID.String(), EffectID: "effect-ok", Topic: "player.level_up", Key: "101", Payload: []byte(`{"level":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.jetStream.Publish(fx.context(), fx.effectSub+".player.level_up", raw, fnats.JetStreamPublishOptions{MsgID: "effect-ok-republished"}); err != nil {
		t.Fatal(err)
	}
	metricsRegistry := app.MustLookup[*metrics.Registry](fx.registry, app.ModMetrics)
	handled := func(result string) int64 {
		for _, metric := range metricsRegistry.Snapshot() {
			if metric.Name == "dataengine.effect_consumer.handled_total" && metric.Labels["consumer"] == durable && metric.Labels["result"] == result {
				return metric.Value
			}
		}
		return 0
	}
	waitFor(t, 20*time.Second, "the republished effect recognised as a duplicate", func() bool { return handled("duplicate") >= 1 })
	time.Sleep(500 * time.Millisecond) // 给毒消息留出被（错误地）重投的时间
	mu.Lock()
	defer mu.Unlock()
	if runs["effect-ok"] != 1 || runs["effect-poison"] != 1 || runs["effect-flaky"] != 2 {
		t.Fatalf("handler runs = %v; want effect-ok once (the republish skipped by its receipt), effect-poison once (terminated), effect-flaky twice (retried)", runs)
	}
	if got := handled("poison"); got != 1 {
		t.Fatalf("handled_total{result=poison} = %d, want 1", got)
	}
}

// best_effort 在真实 NATS 上（2026-11-05 effect 持久性）：DataEngine Mod 的发送器（Nest 在持久点之后调用的
// PublishBestEffort）→ core NATS → SubscribeEffects 的 best_effort 消费者（队列订阅，Order none、4 个 worker）。核对：
//   - 每个 effect 交到 handler 恰好一次，信封（事务号、EffectID、Key、载荷）原样；
//   - handler 出错的那一条只记 failed，不重投；解不开的信封记 poison；
//   - 不经 JetStream：效果流里没有消息（不建 inbox 由单元测试 TestSubscribeEffectsBestEffortHasNoInboxAndNoRedelivery 钉住）。
//
// 发送与订阅共用 fixture 的同一条 NATS 连接，订阅先于发布到达服务端（NATS 单连接有序），所以不会因兴趣还没登记而丢。
func TestRealBestEffortEffectsReachTheConsumerOnceWithoutTheStream(t *testing.T) {
	fx := newRealFixture(t)
	durable := fmt.Sprintf("best-effort-%d", os.Getpid())
	var mu sync.Mutex
	runs := map[string]int{}
	seen := map[string]nestwal.EffectEnvelope{}
	consumer := EffectConsumer{Durable: durable, Topic: "player.notice", Delivery: coredata.EffectBestEffort, Order: EffectOrderNone, Workers: 4}
	sub, err := SubscribeEffects(fx.context(), fx.registry, consumer, func(_ context.Context, envelope nestwal.EffectEnvelope) error {
		mu.Lock()
		runs[envelope.EffectID]++
		seen[envelope.EffectID] = envelope
		mu.Unlock()
		if envelope.EffectID == "notice-fails" {
			return errors.New("push: player offline")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Drain()

	var txID corenest.TransactionID
	txID[15] = 51
	fx.dataMod.PublishBestEffort(txID, []corenest.Effect{
		{ID: "notice-1", Topic: "player.notice", Key: "201", Payload: []byte(`{"n":1}`), Delivery: corenest.EffectBestEffort},
		{ID: "notice-fails", Topic: "player.notice", Key: "202", Payload: []byte(`{"n":2}`), Delivery: corenest.EffectBestEffort},
		{ID: "notice-3", Topic: "player.notice", Key: "201", Payload: []byte(`{"n":3}`), Delivery: corenest.EffectBestEffort},
	})
	client := app.MustLookup[fnats.IClient](fx.registry, mods.ModNats)
	if err := client.Publish(BestEffortSubject(fx.effectSub, "player.notice"), []byte("not json")); err != nil {
		t.Fatal(err)
	}
	metricsRegistry := app.MustLookup[*metrics.Registry](fx.registry, app.ModMetrics)
	counter := func(name, label, value string) int64 {
		var total int64
		for _, metric := range metricsRegistry.Snapshot() {
			if metric.Name == name && metric.Labels[label] == value && (name != "dataengine.effect_consumer.handled_total" || metric.Labels["consumer"] == durable) {
				total += metric.Value
			}
		}
		return total
	}
	waitFor(t, 20*time.Second, "three effects handled", func() bool {
		return counter("dataengine.effect_consumer.handled_total", "result", "ok") == 2 &&
			counter("dataengine.effect_consumer.handled_total", "result", "failed") == 1
	})
	time.Sleep(500 * time.Millisecond) // 给失败的那条留出被（错误地）重投的时间
	mu.Lock()
	defer mu.Unlock()
	if runs["notice-1"] != 1 || runs["notice-fails"] != 1 || runs["notice-3"] != 1 || len(runs) != 3 {
		t.Fatalf("handler runs = %v; want each of the three effects once (the failing one not redelivered)", runs)
	}
	if got := seen["notice-3"]; got.TransactionID != txID.String() || got.Key != "201" || got.Topic != "player.notice" || string(got.Payload) != `{"n":3}` {
		t.Fatalf("envelope = %+v", got)
	}
	if got := counter("dataengine.effects.best_effort_total", "result", "published"); got != 3 {
		t.Fatalf("best_effort_total{result=published} = %d, want 3", got)
	}
	if got := counter("dataengine.effect_consumer.handled_total", "result", "poison"); got != 1 {
		t.Fatalf("handled_total{result=poison} = %d, want 1 (the undecodable message)", got)
	}
	raw, err := gonats.Connect(os.Getenv("ROOST_DATAENGINE_IT_NATS_URL"), gonats.Timeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	js, err := gojs.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(fx.context(), fx.stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(fx.context())
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("effect stream holds %d messages; best_effort effects must not go through JetStream", info.State.Msgs)
	}
}
