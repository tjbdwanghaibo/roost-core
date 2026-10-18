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

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
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
