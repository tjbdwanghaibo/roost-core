package dataengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// SubscribeEffects 是事务性 effect 消费者的框架入口（D12 方向 A）：流、主题前缀与回执库取 DataEngine Mod 的声明，
// 业务只给处理函数。JetStream 是记录订阅、能手动投递的替身；回执是 mongotest 上真实的 nestwal.MongoEffectInbox。

type effectsJetStream struct {
	mu      sync.Mutex
	config  fnats.JetStreamConsumerConfig
	handler fnats.JetStreamHandler
}

func (*effectsJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (*effectsJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (j *effectsJetStream) Subscribe(_ context.Context, cfg fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.config, j.handler = cfg, handler
	return nil, nil
}

func (j *effectsJetStream) deliver(t *testing.T, data []byte) error {
	t.Helper()
	j.mu.Lock()
	handler := j.handler
	j.mu.Unlock()
	return handler(context.Background(), &fnats.JetStreamMsg{Subject: j.config.FilterSubject, Data: data, StreamSeq: 7, NumDelivered: 1})
}

func effectEnvelope(t *testing.T, id string, payload string) []byte {
	t.Helper()
	raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: id, Topic: "player.level_up", Payload: []byte(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type effectsFixture struct {
	registry *app.Registry
	mongo    *mongotest.Client
	js       *effectsJetStream
	metrics  *metrics.Registry
}

func newEffectsFixture(t *testing.T, yaml map[string]any) effectsFixture {
	t.Helper()
	cfg := viper.New()
	for key, value := range yaml {
		cfg.Set(key, value)
	}
	f := effectsFixture{registry: app.NewRegistry(cfg), mongo: mongotest.NewClient(), js: &effectsJetStream{}}
	f.metrics = app.MustLookup[*metrics.Registry](f.registry, app.ModMetrics)
	if err := f.registry.Register(mods.ModMongo, f.mongo); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.Register(mods.ModNatsJetStream, f.js); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f effectsFixture) handled(result string) int64 {
	for _, metric := range f.metrics.Snapshot() {
		if metric.Name == "dataengine.effect_consumer.handled_total" && metric.Labels["consumer"] == "game-level-up-mail" && metric.Labels["result"] == result {
			return metric.Value
		}
	}
	return 0
}

// 订阅的流、主题、回执库都取 DataEngine Mod 的声明；没写的消费者参数取框架缺省（有退避、MaxDeliver 足够长）。
func TestSubscribeEffectsReadsTheDataEngineDeclaration(t *testing.T) {
	f := newEffectsFixture(t, map[string]any{"dataengine.database": "planet", "dataengine.effects.stream": "PLANET_EFFECTS", "dataengine.effects.subject_prefix": ".planet.effect."})
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	if _, err := SubscribeEffects(context.Background(), f.registry, consumer, func(context.Context, nestwal.EffectEnvelope) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got := f.js.config
	if got.Stream != "PLANET_EFFECTS" || got.FilterSubject != "planet.effect.player.level_up" || got.Durable != "game-level-up-mail" ||
		got.AckWait != DefaultEffectAckWait || got.MaxDeliver != DefaultEffectMaxDeliver || got.MaxAckPending != DefaultEffectMaxAckPending ||
		got.NakBackoffMin <= 0 || got.NakBackoffMax < got.NakBackoffMin || got.DeliverPolicy != fnats.JetStreamDeliverAll {
		t.Fatalf("consumer config = %+v", got)
	}
	if err := f.js.deliver(t, effectEnvelope(t, "effect-1", `{}`)); err != nil {
		t.Fatal(err)
	}
	var receipt bson.M
	if err := f.mongo.Collection("planet", "_effect_inbox_game-level-up-mail").FindOne(context.Background(), bson.M{"_id": "effect-1"}, &receipt); err != nil {
		t.Fatalf("receipt not in dataengine.database / per-durable collection: %v", err)
	}
}

// 至少一次投递、按 EffectID 至多一次生效：成功后再投读到回执跳过；handler 失败不写回执、nak 重投，下次再执行。
func TestSubscribeEffectsRunsEachEffectOnceAndRetriesFailures(t *testing.T) {
	f := newEffectsFixture(t, nil)
	runs := map[string]int{}
	fail := true
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	_, err := SubscribeEffects(context.Background(), f.registry, consumer, func(_ context.Context, envelope nestwal.EffectEnvelope) error {
		runs[envelope.EffectID]++
		if envelope.EffectID == "effect-flaky" && fail {
			return errors.New("mail: unavailable")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.js.deliver(t, effectEnvelope(t, "effect-1", `{"level":2}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.js.deliver(t, effectEnvelope(t, "effect-flaky", `{}`)); err == nil || fnats.IsPermanent(err) {
		t.Fatalf("a failing handler = %v, want a retryable error (nak)", err)
	}
	fail = false
	if err := f.js.deliver(t, effectEnvelope(t, "effect-flaky", `{}`)); err != nil {
		t.Fatal(err)
	}
	if runs["effect-1"] != 1 || runs["effect-flaky"] != 2 {
		t.Fatalf("runs = %v, want effect-1 once (duplicate skipped) and effect-flaky twice (retried)", runs)
	}
	for result, want := range map[string]int64{"ok": 2, "duplicate": 1, "retry": 1} {
		if got := f.handled(result); got != want {
			t.Errorf("handled_total{result=%s} = %d, want %d", result, got, want)
		}
	}
}

// 毒消息直接 Term 不重投：handler 标记 PoisonEffect、信封解不开、同一 EffectID 内容不同。修前 demo 的升级邮件把解不开的
// 载荷当普通错误，nak 到 MaxDeliver（20 次）才丢。
func TestSubscribeEffectsTerminatesPoisonEffects(t *testing.T) {
	f := newEffectsFixture(t, nil)
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	_, err := SubscribeEffects(context.Background(), f.registry, consumer, func(_ context.Context, envelope nestwal.EffectEnvelope) error {
		var event struct{ Level int }
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return PoisonEffect(err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.js.deliver(t, effectEnvelope(t, "effect-ok", `{"level":3}`)); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"handler poison":    effectEnvelope(t, "effect-bad", `not json`),
		"broken envelope":   []byte(`{"effect_id":`),
		"no effect id":      []byte(`{"topic":"player.level_up"}`),
		"identity conflict": effectEnvelope(t, "effect-ok", `{"level":4}`),
	} {
		if err := f.js.deliver(t, data); !fnats.IsPermanent(err) {
			t.Errorf("%s = %v, want a permanent error (Term, no redelivery)", name, err)
		}
	}
	if got := f.handled("poison"); got != 4 {
		t.Errorf("handled_total{result=poison} = %d, want 4", got)
	}
}

// handler 的 ctx 以 AckWait 为截止：超过它 broker 会把消息重投给别的副本，这边不应还在跑。
func TestSubscribeEffectsBoundsTheHandlerByAckWait(t *testing.T) {
	f := newEffectsFixture(t, nil)
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up", AckWait: 5 * time.Second}
	var deadline time.Time
	_, err := SubscribeEffects(context.Background(), f.registry, consumer, func(ctx context.Context, _ nestwal.EffectEnvelope) error {
		deadline, _ = ctx.Deadline()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.js.deliver(t, effectEnvelope(t, "effect-1", `{}`)); err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 5*time.Second {
		t.Fatalf("handler deadline in %s, want within the 5s ack wait", remaining)
	}
}

// 回执 TTL 必须长于 dataengine.effects.max_age，缺参数与非法 durable 拒绝。
func TestSubscribeEffectsRefusesUnsafeOrIncompleteConsumers(t *testing.T) {
	handler := func(context.Context, nestwal.EffectEnvelope) error { return nil }
	f := newEffectsFixture(t, map[string]any{"dataengine.effects.max_age": "800h"})
	if _, err := SubscribeEffects(context.Background(), f.registry, EffectConsumer{Durable: "game-mail", Topic: "player.level_up"}, handler); err == nil ||
		!strings.Contains(err.Error(), "dataengine.effects.max_age") {
		t.Fatalf("30-day receipts against an 800h stream = %v, want a refusal naming dataengine.effects.max_age", err)
	}
	f = newEffectsFixture(t, nil)
	for name, consumer := range map[string]EffectConsumer{
		"no durable":     {Topic: "player.level_up"},
		"no topic":       {Durable: "game-mail"},
		"dotted durable": {Durable: "game.mail", Topic: "player.level_up"},
	} {
		if _, err := SubscribeEffects(context.Background(), f.registry, consumer, handler); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := SubscribeEffects(context.Background(), f.registry, EffectConsumer{Durable: "game-mail", Topic: "player.level_up"}, nil); err == nil {
		t.Error("nil handler accepted")
	}
}
