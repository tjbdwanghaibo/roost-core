package dataengine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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
	sub     *effectsSubscription
}

// effectsSubscription 是订阅替身：Drain 只发起，在途投递处理完（release）之后才关闭 Closed，与真实消费者一样。
type effectsSubscription struct {
	mu      sync.Mutex
	drains  int
	release chan struct{}
	once    sync.Once
	closed  chan struct{}
}

func (s *effectsSubscription) Stop() { s.once.Do(func() { close(s.closed) }) }
func (s *effectsSubscription) Drain() {
	s.mu.Lock()
	s.drains++
	s.mu.Unlock()
	go func() {
		<-s.release
		s.Stop()
	}()
}
func (s *effectsSubscription) Closed() <-chan struct{} { return s.closed }

func (*effectsJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (*effectsJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (j *effectsJetStream) Subscribe(_ context.Context, cfg fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.config, j.handler = cfg, handler
	j.sub = &effectsSubscription{release: make(chan struct{}), closed: make(chan struct{})}
	return j.sub, nil
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
		"no durable":       {Topic: "player.level_up"},
		"no topic":         {Durable: "game-mail"},
		"dotted durable":   {Durable: "game.mail", Topic: "player.level_up"},
		"negative workers": {Durable: "game-mail", Topic: "player.level_up", Workers: -1},
	} {
		if _, err := SubscribeEffects(context.Background(), f.registry, consumer, handler); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := SubscribeEffects(context.Background(), f.registry, EffectConsumer{Durable: "game-mail", Topic: "player.level_up"}, nil); err == nil {
		t.Error("nil handler accepted")
	}
}

// effect 消费者是外部输入：SubscribeEffects 把它登记给 App（app.AddIngress），App 停机第一步 Drain 并等在途 effect
// 处理完（Closed），之后才停 Nest / DataEngine（App 生命周期方案缺口 6）。修前只靠业务 Shutdown 里的 Drain()，它只发起、
// 不等，Shutdown 返回时在途 effect 还在跑，App 接着停它用的依赖；忘了 Drain 的业务服务则一直收到停机最后。
func TestSubscribeEffectsIsClosedWithTheAppsInputs(t *testing.T) {
	f := newEffectsFixture(t, map[string]any{"dataengine.database": "planet"})
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	subscription, err := SubscribeEffects(context.Background(), f.registry, consumer, func(context.Context, nestwal.EffectEnvelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		closed <- app.CloseRegisteredIngress(ctx, f.registry)
	}()
	select {
	case err := <-closed:
		t.Fatalf("the App's inputs closed (%v) with an effect still in flight; drains=%d: the consumer is not one of the App's inputs", err, f.js.sub.drains)
	case <-time.After(50 * time.Millisecond):
	}
	close(f.js.sub.release)
	if err := <-closed; err != nil {
		t.Fatalf("closing the inputs: %v", err)
	}
	subscription.Drain() // 业务 Shutdown 里的 Drain() 照旧可以留着：之后是空操作
	if f.js.sub.drains < 1 {
		t.Fatalf("consumer drained %d times", f.js.sub.drains)
	}
}

// —— 进程内并发：同一实体（信封 Key）按到达顺序逐条处理，不同实体并发，在途条数有上界 ——
//
// serialJetStream 是按 nats.go 的方式投递的替身：一个 goroutine 逐条调用回调（回调不返回，下一条就不投），支持
// DeferSettle（推迟的结算由 settle 或回调的错误返回二者先到的那个完成，恰好一次）。每条投递记下回调进入 / 返回与结算。
type serialJetStream struct {
	effectsJetStream
	queue chan *serialDelivery
	stop  context.CancelFunc
}

type serialDelivery struct {
	id       string
	msg      *fnats.JetStreamMsg
	entered  chan struct{} // 回调开始处理这一条
	returned chan struct{} // 回调返回
	settled  chan error    // 结算结论（nil = ack）

	mu       sync.Mutex
	deferred bool
	done     bool
	defers   int
}

func (d *serialDelivery) settle(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done {
		return
	}
	d.done = true
	d.settled <- err
}

func newSerialEffectsFixture(t *testing.T, yaml map[string]any) (effectsFixture, *serialJetStream) {
	t.Helper()
	f := newEffectsFixture(t, yaml)
	js := &serialJetStream{queue: make(chan *serialDelivery, 64)}
	// 换掉登记的 JetStream：同一个 Registry 不能重复登记，另建一个。
	cfg := f.registry.Config()
	f.registry = app.NewRegistry(cfg)
	f.metrics = app.MustLookup[*metrics.Registry](f.registry, app.ModMetrics)
	if err := f.registry.Register(mods.ModMongo, f.mongo); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.Register(mods.ModNatsJetStream, js); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	js.stop = cancel
	t.Cleanup(cancel)
	go js.loop(ctx)
	return f, js
}

func (j *serialJetStream) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-j.queue:
			j.mu.Lock()
			handler := j.handler
			j.mu.Unlock()
			d.msg.DeferSettle = func() func(error) {
				d.mu.Lock()
				defer d.mu.Unlock()
				d.defers++
				if d.deferred {
					return nil
				}
				d.deferred = true
				return d.settle
			}
			close(d.entered)
			err := handler(ctx, d.msg)
			d.mu.Lock()
			deferred := d.deferred
			d.mu.Unlock()
			close(d.returned)
			if !deferred || err != nil {
				d.settle(err)
			}
		}
	}
}

func (j *serialJetStream) push(t *testing.T, id, key string) *serialDelivery {
	t.Helper()
	return j.pushWith(t, id, key, nil)
}

func (j *serialJetStream) pushWith(t *testing.T, id, key string, inProgress func() error) *serialDelivery {
	t.Helper()
	raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-" + id, EffectID: id, Topic: "player.level_up", Key: key, Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	d := &serialDelivery{id: id, msg: &fnats.JetStreamMsg{Subject: j.config.FilterSubject, Data: raw, NumDelivered: 1, InProgress: inProgress},
		entered: make(chan struct{}), returned: make(chan struct{}), settled: make(chan error, 1)}
	j.queue <- d
	return d
}

// effectGate 让测试钉住某些 effect 的 handler：started 记第一次进入 handler 的顺序，block 里的 effect 等 release。
// inbox 事务冲突时 handler 会被重跑，只记第一次。
type effectGate struct {
	mu      sync.Mutex
	started []string
	seen    map[string]chan struct{}
	block   map[string]chan struct{}
}

func newEffectGate(blocked ...string) *effectGate {
	g := &effectGate{seen: map[string]chan struct{}{}, block: map[string]chan struct{}{}}
	for _, id := range blocked {
		g.block[id] = make(chan struct{})
	}
	return g
}

func (g *effectGate) startedCh(id string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := g.seen[id]
	if ch == nil {
		ch = make(chan struct{})
		g.seen[id] = ch
	}
	return ch
}

func (g *effectGate) handler(ctx context.Context, envelope nestwal.EffectEnvelope) error {
	ch := g.startedCh(envelope.EffectID)
	g.mu.Lock()
	select {
	case <-ch:
	default:
		g.started = append(g.started, envelope.EffectID)
		close(ch)
	}
	release := g.block[envelope.EffectID]
	g.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (g *effectGate) release(id string) { close(g.block[id]) }

func (g *effectGate) order() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.started...)
}

func waitFor(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: not within 5s", what)
	}
}

func waitSettled(t *testing.T, d *serialDelivery) error {
	t.Helper()
	select {
	case err := <-d.settled:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("effect %s: not settled within 5s (the callback returned: %v)", d.id, isClosed(d.returned))
		return nil
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func notSettled(t *testing.T, d *serialDelivery) {
	t.Helper()
	select {
	case err := <-d.settled:
		t.Fatalf("effect %s settled (%v) while it must still be pending", d.id, err)
	default:
	}
}

// 一个实体的 effect 被钉住时，别的实体的 effect 照样处理完并确认。修前 handleEffect 在 nats.go 的回调里同步跑完，
// 一个慢 handler 挡住这个 durable 上的全部 effect（联盟 Join 基准里单进程约 50 条/秒，benchmarks/alliance-join-1000-20261105）。
func TestSubscribeEffectsRunsDifferentEntitiesConcurrently(t *testing.T) {
	f, js := newSerialEffectsFixture(t, nil)
	gate := newEffectGate("a-1")
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	if _, err := SubscribeEffects(context.Background(), f.registry, consumer, gate.handler); err != nil {
		t.Fatal(err)
	}
	a1 := js.push(t, "a-1", "1001")
	waitFor(t, "a-1 handler started", gate.startedCh("a-1"))
	b1 := js.push(t, "b-1", "2002")
	if err := waitSettled(t, b1); err != nil {
		t.Fatalf("b-1 settled with %v, want ack", err)
	}
	notSettled(t, a1)
	gate.release("a-1")
	if err := waitSettled(t, a1); err != nil {
		t.Fatalf("a-1 settled with %v, want ack", err)
	}
	if got := f.handled("ok"); got != 2 {
		t.Fatalf("handled_total{result=ok} = %d, want 2", got)
	}
}

// 同一实体的第二条在第一条完成之前不开始（回调已经接纳它并返回，nats.go 接着投下一条），第一条完成后按到达顺序处理。
func TestSubscribeEffectsKeepsOneEntityInArrivalOrder(t *testing.T) {
	f, js := newSerialEffectsFixture(t, nil)
	gate := newEffectGate("a-1")
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	if _, err := SubscribeEffects(context.Background(), f.registry, consumer, gate.handler); err != nil {
		t.Fatal(err)
	}
	a1 := js.push(t, "a-1", "1001")
	waitFor(t, "a-1 handler started", gate.startedCh("a-1"))
	a2 := js.push(t, "a-2", "1001")
	b1 := js.push(t, "b-1", "2002")
	// b-1 结算说明回调已经越过 a-2（逐条投递）：a-2 已被接纳，但 a-1 没完成，a-2 不能开始。
	if err := waitSettled(t, b1); err != nil {
		t.Fatalf("b-1 settled with %v, want ack", err)
	}
	if isClosed(gate.startedCh("a-2")) {
		t.Fatalf("a-2 started while a-1 of the same entity was still running; order %v", gate.order())
	}
	notSettled(t, a2)
	gate.release("a-1")
	for _, d := range []*serialDelivery{a1, a2} {
		if err := waitSettled(t, d); err != nil {
			t.Fatalf("%s settled with %v, want ack", d.id, err)
		}
	}
	order := gate.order()
	if want := []string{"a-1", "b-1", "a-2"}; strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("handler start order = %v, want %v", order, want)
	}
}

// 在途（已接纳未结算）达到上界 MaxAckPending 时，下一条的回调等在那里：不开始、不确认，nats.go 也不再投后面的；
// 有一条结算后它才被接纳。
func TestSubscribeEffectsBoundsEffectsInFlight(t *testing.T) {
	f, js := newSerialEffectsFixture(t, nil)
	gate := newEffectGate("a-1", "b-1")
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up", MaxAckPending: 2}
	if _, err := SubscribeEffects(context.Background(), f.registry, consumer, gate.handler); err != nil {
		t.Fatal(err)
	}
	a1 := js.push(t, "a-1", "1001")
	b1 := js.push(t, "b-1", "2002")
	waitFor(t, "a-1 handler started", gate.startedCh("a-1"))
	waitFor(t, "b-1 handler started", gate.startedCh("b-1"))
	c1 := js.push(t, "c-1", "3003")
	d1 := js.push(t, "d-1", "4004")
	waitFor(t, "the callback took c-1", c1.entered)
	time.Sleep(50 * time.Millisecond) // 给错误实现留出越过上界的机会；下面的断言不依赖这段时间成立
	if isClosed(c1.returned) || isClosed(gate.startedCh("c-1")) || isClosed(d1.entered) {
		t.Fatalf("over the in-flight bound: c-1 callback returned=%v started=%v, d-1 delivered=%v",
			isClosed(c1.returned), isClosed(gate.startedCh("c-1")), isClosed(d1.entered))
	}
	notSettled(t, c1)
	gate.release("a-1")
	for _, d := range []*serialDelivery{a1, c1, d1} {
		if err := waitSettled(t, d); err != nil {
			t.Fatalf("%s settled with %v, want ack", d.id, err)
		}
	}
	notSettled(t, b1)
	gate.release("b-1")
	if err := waitSettled(t, b1); err != nil {
		t.Fatalf("b-1 settled with %v, want ack", err)
	}
}

// 同时在跑的 handler 不超过 Workers（dataengine.effect_consumer.workers）：满了之后的 effect 已接纳（回调返回、
// 推迟结算），等有 handler 返回才开始。
func TestSubscribeEffectsCapsRunningHandlersAtWorkers(t *testing.T) {
	f, js := newSerialEffectsFixture(t, map[string]any{"dataengine.effect_consumer.workers": 2})
	gate := newEffectGate("a-1", "b-1")
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	if _, err := SubscribeEffects(context.Background(), f.registry, consumer, gate.handler); err != nil {
		t.Fatal(err)
	}
	js.push(t, "a-1", "1001")
	js.push(t, "b-1", "2002")
	waitFor(t, "a-1 handler started", gate.startedCh("a-1"))
	waitFor(t, "b-1 handler started", gate.startedCh("b-1"))
	c1 := js.push(t, "c-1", "3003")
	waitFor(t, "c-1 admitted (callback returned)", c1.returned)
	time.Sleep(50 * time.Millisecond) // 同上：只给错误实现机会
	if isClosed(gate.startedCh("c-1")) {
		t.Fatalf("c-1 started with 2 handlers already running and workers = 2; order %v", gate.order())
	}
	gate.release("a-1")
	if err := waitSettled(t, c1); err != nil {
		t.Fatalf("c-1 settled with %v, want ack", err)
	}
	gate.release("b-1")
}

// Workers = 1 与引入并发之前完全相同：在回调里同步处理，不推迟结算，一个慢 effect 挡住后面的。配置
// （dataengine.effect_consumer.workers）与 EffectConsumer.Workers（覆盖配置）两种写法。
func TestSubscribeEffectsWithOneWorkerHandlesInTheCallback(t *testing.T) {
	for name, setup := range map[string]struct {
		yaml    map[string]any
		workers int
	}{
		"config":   {yaml: map[string]any{"dataengine.effect_consumer.workers": 1}},
		"consumer": {workers: 1},
	} {
		t.Run(name, func(t *testing.T) {
			f, js := newSerialEffectsFixture(t, setup.yaml)
			gate := newEffectGate("a-1")
			consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up", Workers: setup.workers}
			if _, err := SubscribeEffects(context.Background(), f.registry, consumer, gate.handler); err != nil {
				t.Fatal(err)
			}
			a1 := js.push(t, "a-1", "1001")
			waitFor(t, "a-1 handler started", gate.startedCh("a-1"))
			b1 := js.push(t, "b-1", "2002")
			time.Sleep(50 * time.Millisecond)
			if isClosed(a1.returned) || isClosed(b1.entered) {
				t.Fatalf("workers = 1: a-1 callback returned=%v, b-1 delivered=%v; want a-1 handled inside the callback", isClosed(a1.returned), isClosed(b1.entered))
			}
			gate.release("a-1")
			for _, d := range []*serialDelivery{a1, b1} {
				if err := waitSettled(t, d); err != nil {
					t.Fatalf("%s settled with %v, want ack", d.id, err)
				}
				if d.defers != 0 {
					t.Fatalf("workers = 1: %s called DeferSettle %d times, want 0", d.id, d.defers)
				}
			}
		})
	}
}

// 订阅 Stop（handler 的 ctx 取消）时，还在排队等 worker 的 effect 不再进 handler，按 ctx 错误 nak 交还 broker。
func TestSubscribeEffectsReturnsQueuedEffectsOnStop(t *testing.T) {
	f, js := newSerialEffectsFixture(t, map[string]any{"dataengine.effect_consumer.workers": 2})
	gate := newEffectGate("a-1", "b-1")
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	if _, err := SubscribeEffects(context.Background(), f.registry, consumer, gate.handler); err != nil {
		t.Fatal(err)
	}
	a1, b1 := js.push(t, "a-1", "1001"), js.push(t, "b-1", "2002")
	waitFor(t, "a-1 handler started", gate.startedCh("a-1"))
	waitFor(t, "b-1 handler started", gate.startedCh("b-1"))
	c1 := js.push(t, "c-1", "3003")
	waitFor(t, "c-1 admitted", c1.returned)
	js.stop()
	for _, d := range []*serialDelivery{a1, b1, c1} {
		if err := waitSettled(t, d); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s settled with %v after Stop, want the ctx error (nak)", d.id, err)
		}
	}
	if isClosed(gate.startedCh("c-1")) {
		t.Fatal("c-1 entered the handler after Stop")
	}
}

// handler panic 只结束这一条：它按错误结算（nak 重投），同一实体后面的 effect 照常处理。
func TestSubscribeEffectsSurvivesAHandlerPanic(t *testing.T) {
	f, js := newSerialEffectsFixture(t, nil)
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up"}
	if _, err := SubscribeEffects(context.Background(), f.registry, consumer, func(_ context.Context, envelope nestwal.EffectEnvelope) error {
		if envelope.EffectID == "a-1" {
			panic("boom")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a1, a2 := js.push(t, "a-1", "1001"), js.push(t, "a-2", "1001")
	if err := waitSettled(t, a1); err == nil || fnats.IsPermanent(err) {
		t.Fatalf("a panicking handler settled with %v, want a retryable error", err)
	}
	if err := waitSettled(t, a2); err != nil {
		t.Fatalf("a-2 after a panic settled with %v, want ack", err)
	}
}

// 排队期间发 in-progress（broker 不会把还在本进程排队的 effect 重投给别的实例），排队久的开始处理前再发一次；
// 不排队直接处理的不发。a-1 钉到自己的 AckWait（200ms）截止，同实体的 a-2 排队约 200ms：期间心跳（每 100ms）至少一次，
// 开始前再一次。
func TestSubscribeEffectsKeepsQueuedEffectsInProgress(t *testing.T) {
	f, js := newSerialEffectsFixture(t, nil)
	gate := newEffectGate("a-1")
	consumer := EffectConsumer{Durable: "game-level-up-mail", Topic: "player.level_up", AckWait: 200 * time.Millisecond}
	if _, err := SubscribeEffects(context.Background(), f.registry, consumer, gate.handler); err != nil {
		t.Fatal(err)
	}
	var first, second atomic.Int32
	a1 := js.pushWith(t, "a-1", "1001", func() error { first.Add(1); return nil })
	waitFor(t, "a-1 handler started", gate.startedCh("a-1"))
	a2 := js.pushWith(t, "a-2", "1001", func() error { second.Add(1); return nil })
	if err := waitSettled(t, a1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a-1 settled with %v, want its ack-wait deadline", err)
	}
	if err := waitSettled(t, a2); err != nil {
		t.Fatalf("a-2 settled with %v, want ack", err)
	}
	if got := first.Load(); got != 0 {
		t.Errorf("a-1 started at once but sent %d in-progress acks", got)
	}
	if got := second.Load(); got < 2 {
		t.Errorf("a-2 queued ~200ms at ack wait 200ms but sent %d in-progress acks, want ≥ 2 (heartbeat + before start)", got)
	}
}
