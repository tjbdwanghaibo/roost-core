package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 参与方新路径的步骤消费者与 Mongo 步骤收件箱（第 3 批）。消费者用记录型 JetStream 替身：按（流，过滤）登记 handler，
// 发布原样记下；真实三节点上的端到端见 step_consumer_real_integration_test.go。

type stepJetStream struct {
	mu         sync.Mutex
	handlers   map[string]fnats.JetStreamHandler // stream + " " + filter
	configs    []fnats.JetStreamConsumerConfig
	published  []stepPublish
	publishErr error
}

type stepPublish struct {
	subject, msgID string
	input          resultInput
}

func newStepJetStream() *stepJetStream {
	return &stepJetStream{handlers: map[string]fnats.JetStreamHandler{}}
}

func (j *stepJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }

func (j *stepJetStream) Publish(_ context.Context, subject string, data []byte, options fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.publishErr != nil {
		return fnats.JetStreamPublishAck{}, j.publishErr
	}
	var input resultInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fnats.JetStreamPublishAck{}, err
	}
	j.published = append(j.published, stepPublish{subject: subject, msgID: options.MsgID, input: input})
	return fnats.JetStreamPublishAck{Sequence: uint64(len(j.published))}, nil
}

func (j *stepJetStream) Subscribe(_ context.Context, config fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.handlers[config.Stream+" "+config.FilterSubject] = handler
	j.configs = append(j.configs, config)
	return &recordedSubscription{closed: make(chan struct{})}, nil
}

// deliver 把一条命令事件交给 stream 上这个主题的 durable（替身消息不支持推迟结算，在途槽同步执行）。
func (j *stepJetStream) deliver(t *testing.T, layout Layout, command Command, raw []byte) error {
	t.Helper()
	if raw == nil {
		ev := logEvent{V: WireVersion, Part: int(command.Partition), Writer: "w", Kind: "dispatch", Command: &command}
		ev.Saga.Record.ID = command.SagaID
		var err error
		if raw, err = json.Marshal(ev); err != nil {
			t.Fatal(err)
		}
	}
	k := int(command.Partition)
	j.mu.Lock()
	handler, ok := j.handlers[layout.StreamOf(k)+" "+layout.CommandFilter(command.Topic)]
	j.mu.Unlock()
	if !ok {
		t.Fatalf("no durable on %s for topic %s", layout.StreamOf(k), command.Topic)
	}
	return handler(context.Background(), &fnats.JetStreamMsg{Subject: layout.CommandEventSubject(k, command.Topic), Data: raw})
}

func (j *stepJetStream) takePublished() []stepPublish {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := j.published
	j.published = nil
	return out
}

func stepLayout() Layout {
	return Layout{Prefix: "roost.saga", Stream: "ROOST_SAGA", Partitions: 4, Groups: 2}
}

// 原生步骤消费者：每条流一个同名 durable（过滤全部分区的主题）；执行与回放都把结果发到命令所属分区的 in.res（MsgID
// res.<生效的 CommandID>），发布成功才 ack；过期未执行直接 ack、不发；坏信封 Term；步骤体出错与发布失败 nak。
func TestNativeStepConsumerPublishesToThePartitionThenAcks(t *testing.T) {
	js, layout, h := newStepJetStream(), stepLayout(), newNativeHarness()
	subs, err := SubscribeNativeStep(context.Background(), js, layout, StepConsumerConfig{Durable: "game-rally-reserve", Topic: "rally.reserve"}, h.body)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != layout.Groups || len(js.configs) != layout.Groups {
		t.Fatalf("subscriptions = %d, want one per stream (%d)", len(subs), layout.Groups)
	}
	for g, config := range js.configs {
		if config.Stream != layout.Streams()[g] || config.Durable != "game-rally-reserve" || config.FilterSubject != "roost.saga.*.ev.cmd.rally.reserve" {
			t.Fatalf("consumer %d = %+v", g, config)
		}
	}
	first := stepCommand(0, 1, time.Minute)
	if err := js.deliver(t, layout, first, nil); err != nil {
		t.Fatal(err)
	}
	second := stepCommand(0, 2, time.Minute)
	if err := js.deliver(t, layout, second, nil); err != nil {
		t.Fatal(err)
	}
	published := js.takePublished()
	if len(published) != 2 || h.business != 1 {
		t.Fatalf("published %d results, business ran %d times; want 2 and 1", len(published), h.business)
	}
	for _, p := range published {
		if p.subject != "roost.saga.3.in.res" || p.msgID != "res."+first.ID || p.input.V != WireVersion || p.input.Completion.CommandID != first.ID || !p.input.Completion.Success || !p.input.Completion.CompletedAt.IsZero() {
			t.Fatalf("result = %+v, want %s on roost.saga.3.in.res", p, first.ID)
		}
	}

	expired := stepCommand(0, 1, -time.Second)
	expired.IdempotencyKey, expired.ID = "saga-1:1:5", "saga-1:1:5:1"
	if err := js.deliver(t, layout, expired, nil); err != nil {
		t.Fatalf("an expired command without a receipt was not acked: %v", err)
	}
	if got := js.takePublished(); len(got) != 0 {
		t.Fatalf("an expired command published %+v", got)
	}

	if err := js.deliver(t, layout, first, []byte(`{"v":3,"part":3}`)); !fnats.IsPermanent(err) {
		t.Fatalf("an event without a command = %v, want a permanent (Term) error", err)
	}
	other := first
	other.Partition = 1
	raw, _ := json.Marshal(logEvent{V: WireVersion, Part: 3, Writer: "w", Kind: "dispatch", Command: &other})
	if err := js.deliver(t, layout, other, raw); !fnats.IsPermanent(err) {
		t.Fatalf("a command for another partition than its event = %v, want Term", err)
	}

	js.publishErr = errors.New("no PubAck")
	if err := js.deliver(t, layout, first, nil); err == nil || fnats.IsPermanent(err) {
		t.Fatalf("a failed result publish = %v, want a retryable error (nak)", err)
	}
	js.publishErr = nil
	failing := func(context.Context, NativeStep) error { return errors.New("nest unavailable") }
	if _, err := SubscribeNativeStep(context.Background(), js, layout, StepConsumerConfig{Durable: "game-rally-fail", Topic: "rally.fail"}, failing); err != nil {
		t.Fatal(err)
	}
	broken := stepCommand(0, 1, time.Minute)
	broken.Topic = "rally.fail"
	if err := js.deliver(t, layout, broken, nil); err == nil || fnats.IsPermanent(err) {
		t.Fatalf("a failing step body = %v, want a retryable error (nak)", err)
	}
}

func TestStepConsumerRejectsUnsafeConfiguration(t *testing.T) {
	js, layout := newStepJetStream(), stepLayout()
	body := func(context.Context, NativeStep) error { return nil }
	for _, config := range []StepConsumerConfig{
		{Topic: "rally.reserve"},
		{Durable: "d", Topic: "bad topic"},
		{Durable: "d", Topic: "rally.reserve", MaxAckPending: 4, NativeWorkers: 8},
	} {
		if _, err := SubscribeNativeStep(context.Background(), js, layout, config, body); err == nil {
			t.Fatalf("config %+v accepted", config)
		}
	}
	if _, err := SubscribeNativeStep(context.Background(), js, Layout{Prefix: "p", Stream: "S", Partitions: 1, Groups: 2}, StepConsumerConfig{Durable: "d", Topic: "t"}, body); err == nil {
		t.Fatal("an invalid layout was accepted")
	}
}

// Mongo 步骤收件箱（M-A）：一个事务里查回执、执行、写回执。回放规则与原生步骤相同（成功对任何尝试，拒绝只对同一代际），
// 可重试失败只对写它的那次尝试回放；过期不执行；写回执前截止已过则整个事务回滚（业务写随之撤销）后按过期处理。
func TestMongoStepInboxReceiptRules(t *testing.T) {
	client := mongotest.NewClient()
	inbox, err := NewMongoStepInbox(client, "game", "_rally_step_receipts", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	ledger := client.Collection("game", "ledger")
	runs := 0
	var result Completion
	handler := func(ctx context.Context, command Command) (Completion, error) {
		runs++
		if _, err := client.Database("game").Collection("ledger").InsertOne(ctx, bson.M{"_id": command.ID}); err != nil {
			return Completion{}, err
		}
		return result, nil
	}
	handle := func(command Command) StepOutcome {
		t.Helper()
		outcome, err := inbox.Handle(context.Background(), command, handler)
		if err != nil {
			t.Fatalf("handle %s: %v", command.ID, err)
		}
		return outcome
	}

	result = Completion{Retryable: true, Error: "busy"}
	first := stepCommand(0, 1, time.Minute)
	if o := handle(first); o.Kind != StepExecuted || !o.Completion.Retryable {
		t.Fatalf("retryable = %+v", o)
	}
	if o := handle(first); o.Kind != StepReplayed || o.Completion.CommandID != first.ID || !o.Completion.Retryable {
		t.Fatalf("redelivery of the retryable attempt = %+v, want its own result replayed", o)
	}
	result = Completion{Error: "refused"}
	second := stepCommand(0, 2, time.Minute)
	if o := handle(second); o.Kind != StepExecuted || o.Completion.Success || o.Completion.Retryable {
		t.Fatalf("next attempt after a retryable failure = %+v, want an execution", o)
	}
	if o := handle(stepCommand(0, 3, time.Minute)); o.Kind != StepReplayed || o.Completion.CommandID != second.ID {
		t.Fatalf("same incarnation after a refusal = %+v, want the refusal replayed", o)
	}
	result = Completion{Success: true, Data: []byte("ok")}
	reopened := stepCommand(1, 1, time.Minute)
	if o := handle(reopened); o.Kind != StepExecuted || !o.Completion.Success {
		t.Fatalf("new incarnation = %+v, want an execution", o)
	}
	if o := handle(stepCommand(2, 1, -time.Second)); o.Kind != StepReplayed || o.Completion.CommandID != reopened.ID || string(o.Completion.Data) != "ok" {
		t.Fatalf("expired attempt with a success receipt = %+v, want the success replayed", o)
	}
	if runs != 3 || ledger.Len() != 3 {
		t.Fatalf("handler ran %d times, ledger has %d rows; want 3 and 3", runs, ledger.Len())
	}

	fresh := stepCommand(0, 1, -time.Second)
	fresh.IdempotencyKey, fresh.ID = "saga-1:1:7", "saga-1:1:7:1"
	if o := handle(fresh); o.Kind != StepExpired || runs != 3 {
		t.Fatalf("expired attempt without a receipt = %+v (runs %d)", o, runs)
	}
	// 截止落在 handler 执行期间：写回执前发现，整个事务回滚，按过期处理。
	slow := stepCommand(0, 1, 50*time.Millisecond)
	slow.IdempotencyKey, slow.ID = "saga-1:1:8", "saga-1:1:8:1"
	slowHandler := func(ctx context.Context, command Command) (Completion, error) {
		if _, err := client.Database("game").Collection("ledger").InsertOne(ctx, bson.M{"_id": command.ID}); err != nil {
			return Completion{}, err
		}
		time.Sleep(80 * time.Millisecond)
		return Completion{Success: true}, nil
	}
	if o, err := inbox.Handle(context.Background(), slow, slowHandler); err != nil || o.Kind != StepExpired {
		t.Fatalf("deadline passed inside the handler = %+v %v, want expired", o, err)
	}
	if ledger.Len() != 3 {
		t.Fatalf("ledger has %d rows; the attempt that missed its deadline must not commit its write", ledger.Len())
	}
	if _, err := NewMongoStepInbox(client, "game", " ", time.Hour); err == nil || !strings.Contains(err.Error(), "collection") {
		t.Fatalf("missing collection = %v", err)
	}
}

// 业务被取消（ctx 结束）的 Mongo 步骤：事务整体回滚，业务写与回执都不可见；重试执行一次并提交，之后的重投回放、不再执行。
// 历史：TestMongoCommandInboxCancelledBusinessWriteCanRetryAtomically（MongoCommandInbox → MongoStepInbox）。
func TestMongoStepInboxCancelledBusinessWriteCanRetryAtomically(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	inbox, err := NewMongoStepInbox(client, "inbox_cancel", "_cancel_step_inbox", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	command := stepCommand(0, 1, time.Minute)
	business := client.Database("inbox_cancel").Collection("business")
	cancelCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	handlerCalls := 0
	handler := func(txCtx context.Context, _ Command) (Completion, error) {
		handlerCalls++
		if _, err := business.InsertOne(txCtx, bson.M{"_id": "award", "value": 42}); err != nil {
			return Completion{}, err
		}
		if handlerCalls == 1 {
			cancel()
			return Completion{}, txCtx.Err()
		}
		return Completion{Success: true}, nil
	}
	if _, err := inbox.Handle(cancelCtx, command, handler); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled handler = %v", err)
	}
	if count, err := business.CountDocuments(ctx, bson.M{}); err != nil || count != 0 {
		t.Fatalf("business escaped the transaction: count=%d err=%v", count, err)
	}
	for i := 0; i < 2; i++ {
		outcome, err := inbox.Handle(ctx, command, handler)
		want := StepExecuted
		if i == 1 {
			want = StepReplayed
		}
		if err != nil || outcome.Kind != want || !outcome.Completion.Success {
			t.Fatalf("retry %d = %+v %v, want %v success", i, outcome, err, want)
		}
	}
	if handlerCalls != 2 {
		t.Fatalf("handler ran %d times, want 2 (the cancelled one and the retry)", handlerCalls)
	}
	if count, err := business.CountDocuments(ctx, bson.M{}); err != nil || count != 1 {
		t.Fatalf("business rows after the retry = %d err=%v, want 1", count, err)
	}
}

// ---- 按 sid 确切路由（路由步骤） ----
//
// routeBroker 是按 subject 投递的 JetStream 替身，只模拟这里要证明的那部分语义：一条消息交给过滤匹配的每个 durable；同一
// durable 的多个成员（多个进程绑定同一个 durable）按轮转拿到投递，handler 返回错误就是 nak，broker 按消费者的 nak 退避
// （250ms 起翻倍，30s 封顶，与 stepConsumerDefaults 的缺省相同）把它重投给下一个成员；durable 没有成员（进程停机）时
// 消息留在 durable 上，成员订阅时再投。退避不真的等待，只累计成“这条消息被退避耽搁了多久”，所以用例的事件顺序是确定的。
type routeBroker struct {
	mu        sync.Mutex
	durables  map[string]*brokerDurable
	order     []string
	published []stepPublish
}

type brokerDurable struct {
	stream, name, filter string
	members              []brokerMember
	next                 int
	held                 []brokerMessage
}

type brokerMember struct {
	sid     int32
	handler fnats.JetStreamHandler
}

type brokerMessage struct {
	subject string
	data    []byte
}

// brokerDelivery 是一次投递：交给了哪个 sid 的进程、哪个 durable，handler 是否拒绝（nak）。
type brokerDelivery struct {
	sid     int32
	durable string
	nak     bool
}

func newRouteBroker() *routeBroker { return &routeBroker{durables: map[string]*brokerDurable{}} }

// as 是 sid 这个进程看到的 JetStream。
func (b *routeBroker) as(sid int32) fnats.IJetStream { return brokerView{broker: b, sid: sid} }

type brokerView struct {
	broker *routeBroker
	sid    int32
}

func (brokerView) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }

func (v brokerView) Publish(_ context.Context, subject string, data []byte, options fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	var input resultInput
	if err := json.Unmarshal(data, &input); err != nil {
		return fnats.JetStreamPublishAck{}, err
	}
	v.broker.mu.Lock()
	defer v.broker.mu.Unlock()
	v.broker.published = append(v.broker.published, stepPublish{subject: subject, msgID: options.MsgID, input: input})
	return fnats.JetStreamPublishAck{Sequence: uint64(len(v.broker.published))}, nil
}

// Subscribe 绑定（没有时建出）durable。同名 durable 的过滤必须相同——JetStream 拒绝用另一个过滤绑定已有 durable。
func (v brokerView) Subscribe(_ context.Context, config fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	b := v.broker
	b.mu.Lock()
	key := config.Stream + " " + config.Durable
	durable := b.durables[key]
	if durable == nil {
		durable = &brokerDurable{stream: config.Stream, name: config.Durable, filter: config.FilterSubject}
		b.durables[key] = durable
		b.order = append(b.order, key)
	}
	if durable.filter != config.FilterSubject {
		b.mu.Unlock()
		return nil, fmt.Errorf("durable %s on %s has filter %s, not %s", config.Durable, config.Stream, durable.filter, config.FilterSubject)
	}
	durable.members = append(durable.members, brokerMember{sid: v.sid, handler: handler})
	held := durable.held
	durable.held = nil
	b.mu.Unlock()
	for _, message := range held {
		b.offer(durable, message)
	}
	return &recordedSubscription{closed: make(chan struct{})}, nil
}

// emit 发布一条协调器事件：交给流上过滤匹配的每个 durable，返回这条消息的全部投递与 nak 退避累计。
func (b *routeBroker) emit(stream, subject string, data []byte) ([]brokerDelivery, time.Duration) {
	b.mu.Lock()
	var matched []*brokerDurable
	for _, key := range b.order {
		if durable := b.durables[key]; durable.stream == stream && subjectMatches(durable.filter, subject) {
			matched = append(matched, durable)
		}
	}
	b.mu.Unlock()
	var deliveries []brokerDelivery
	var waited time.Duration
	for _, durable := range matched {
		got, backoff := b.offer(durable, brokerMessage{subject: subject, data: data})
		deliveries = append(deliveries, got...)
		waited += backoff
	}
	return deliveries, waited
}

// offer 把一条消息交给 durable 的成员，nak 就按退避重投给下一个成员，直到有成员结算它；没有成员时留在 durable 上。
func (b *routeBroker) offer(durable *brokerDurable, message brokerMessage) ([]brokerDelivery, time.Duration) {
	var deliveries []brokerDelivery
	var waited time.Duration
	for attempt := 1; attempt <= 16; attempt++ {
		b.mu.Lock()
		if len(durable.members) == 0 {
			durable.held = append(durable.held, message)
			b.mu.Unlock()
			return deliveries, waited
		}
		member := durable.members[durable.next%len(durable.members)]
		durable.next++
		b.mu.Unlock()
		err := member.handler(context.Background(), &fnats.JetStreamMsg{Subject: message.subject, Data: message.data, Stream: durable.stream, Consumer: durable.name, NumDelivered: uint64(attempt)})
		deliveries = append(deliveries, brokerDelivery{sid: member.sid, durable: durable.name, nak: err != nil})
		if err == nil || fnats.IsPermanent(err) {
			return deliveries, waited
		}
		waited += min(250*time.Millisecond<<(attempt-1), 30*time.Second)
	}
	return deliveries, waited
}

func (b *routeBroker) takePublished() []stepPublish {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.published
	b.published = nil
	return out
}

// subjectMatches 是 NATS 的过滤匹配：'*' 匹配一个 token，'>' 匹配其余全部（至少一个）。
func subjectMatches(filter, subject string) bool {
	f, s := strings.Split(filter, "."), strings.Split(subject, ".")
	for i, token := range f {
		if token == ">" {
			return len(s) > i
		}
		if i >= len(s) || (token != "*" && token != s[i]) {
			return false
		}
	}
	return len(f) == len(s)
}

// sidPayload 是路由用例的步骤载荷：步骤改的是哪个玩家，他绑定在哪个 sid（gift.State.FromSID 的同类）。
type sidPayload struct {
	Player int64 `json:"player"`
	SID    int32 `json:"sid"`
}

func routedDefinition() Definition {
	return Definition{Type: "gift", Version: 1, Steps: []Step{{Name: "debit", ForwardTopic: "gift.debit", CompensateTopic: "gift.debit.compensate", Routed: true,
		Timeout: time.Second, MaxAttempts: 3, BackoffMin: time.Millisecond, BackoffMax: 10 * time.Millisecond}}}
}

// routedPartition 是只有一个分区的协调器（unitPartition），登记了路由定义。
func routedPartition(t *testing.T) *unitPartition {
	t.Helper()
	u := newUnitPartition(t, nil)
	if err := u.e.Register(routedDefinition()); err != nil {
		t.Fatal(err)
	}
	// 参与方按系统时钟判命令截止（nativeRun.now），协调器的测试时钟要对齐它，否则命令一到就是过期的。
	u.clock.Set(time.Now())
	return u
}

// routedStart 是一条 gift 启动意图，地址是 route。
func routedStart(t *testing.T, u *unitPartition, businessKey string, payload sidPayload, route string) fnats.LogMsg {
	t.Helper()
	// 路由 saga 必须带截止（TestARoutedSagaMustCarryADeadline）；不关心截止的用例取一小时后。
	return routedStartUntil(t, u, businessKey, payload, route, u.clock.Now().Add(time.Hour))
}

// routedStartUntil 同 routedStart，带 saga 截止（零值表示没有）。
func routedStartUntil(t *testing.T, u *unitPartition, businessKey string, payload sidPayload, route string, deadline time.Time) fnats.LogMsg {
	t.Helper()
	data, _ := json.Marshal(payload)
	request, err := prepareStart(StartRequest{Type: "gift", BusinessKey: businessKey, DefinitionVersion: 1, Data: data, Route: route, DeadlineAt: deadline}, u.e.opts.MaxPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := startIntentDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(startInput{V: WireVersion, Request: request, Attempt: "a-" + businessKey, Digest: digest})
	return fnats.LogMsg{Subject: u.e.layout.StartSubject(0), Data: raw, MsgID: "start.a-" + businessKey}
}

// dispatched 取出这一轮发出的命令事件：subject 与线上字节（与发布同一编码）。
func dispatched(t *testing.T, r *round) (string, []byte, Command) {
	t.Helper()
	for i, ev := range r.events {
		if ev.Command == nil {
			continue
		}
		ev.In = r.maxIn
		raw, err := encodeEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		return r.eventSubjects[i], raw, *ev.Command
	}
	t.Fatal("the round dispatched no command")
	return "", nil, Command{}
}

// sidProcess 是一个游戏进程的路由步骤消费者：它的 sid、回执所在的实体（替身）与准入。
type sidProcess struct {
	sid     int32
	harness *nativeHarness
}

// ownerOnly 是旧的“谁拿到都先问是不是我的”准入：不是本 sid 的玩家就拒绝（nak，留给别的进程）。确切路由之后它是最后一道
// 校验，正常情况下永远不触发。
func (p *sidProcess) ownerOnly(_ context.Context, command Command) error {
	var payload sidPayload
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return err
	}
	if payload.SID != p.sid {
		return fmt.Errorf("player %d is bound to sid %d, not %d; leaving the step", payload.Player, payload.SID, p.sid)
	}
	return nil
}

// subscribeSids 让每个 sid 的进程按生产的形状订阅路由步骤：同一个 Durable 基名、Route = SidRoute(本进程 sid)。
func subscribeSids(t *testing.T, broker *routeBroker, layout Layout, sids []int32, admit func(*sidProcess) func(context.Context, Command) error) map[int32]*sidProcess {
	t.Helper()
	processes := map[int32]*sidProcess{}
	for _, sid := range sids {
		process := &sidProcess{sid: sid, harness: newNativeHarness()}
		processes[sid] = process
		config := StepConsumerConfig{Durable: "game-gift-debit", Topic: "gift.debit", Route: SidRoute(sid), Admit: admit(process)}
		if _, err := SubscribeNativeStep(context.Background(), broker.as(sid), layout, config, process.harness.body); err != nil {
			t.Fatal(err)
		}
	}
	return processes
}

// 确切路由：三个 sid 的进程都订阅了同一步骤，发送方（协调器）按这笔 saga 的路由键（属主 sid 1002）投递，只有 1002 的
// 消费者收到，一次投递就执行并发结果——非属主进程一次也没拿到，没有任何 nak 退避。同一条消息被 JetStream 重投（ack 丢失）
// 时仍只到 1002，回执回放，业务不再执行，两次结果逐字节相同（同一 MsgID，协调器按回执去重）。
func TestARoutedStepIsDeliveredOnlyToTheOwnerSidsConsumer(t *testing.T) {
	u := routedPartition(t)
	broker := newRouteBroker()
	processes := subscribeSids(t, broker, u.e.layout, []int32{1000, 1001, 1002}, func(p *sidProcess) func(context.Context, Command) error { return p.ownerOnly })

	r := u.round(u.clock.Now(), routedStart(t, u, "gift-1", sidPayload{Player: 7, SID: 1002}, SidRoute(1002)))
	subject, raw, command := dispatched(t, r)
	deliveries, waited := broker.emit(u.e.layout.StreamOf(0), subject, raw)
	var nonOwners []int32
	for _, delivery := range deliveries {
		if delivery.sid != 1002 {
			nonOwners = append(nonOwners, delivery.sid)
		}
	}
	if len(nonOwners) != 0 || len(deliveries) != 1 || waited != 0 {
		t.Fatalf("non-owner sids %v received the step; the result came after %v of nak backoff over %d deliveries (%+v); want one delivery to sid 1002 and no backoff",
			nonOwners, waited, len(deliveries), deliveries)
	}
	if processes[1002].harness.business != 1 || processes[1000].harness.business+processes[1001].harness.business != 0 {
		t.Fatalf("business ran %d / %d / %d times on sids 1000 / 1001 / 1002, want only once on 1002",
			processes[1000].harness.business, processes[1001].harness.business, processes[1002].harness.business)
	}
	if command.Route != SidRoute(1002) {
		t.Fatalf("the command carries route %q, want %q", command.Route, SidRoute(1002))
	}

	// JetStream 的重投（ack 丢失）：同一条消息再来一次。
	again, _ := broker.emit(u.e.layout.StreamOf(0), subject, raw)
	if len(again) != 1 || again[0].sid != 1002 || processes[1002].harness.business != 1 {
		t.Fatalf("the redelivery went to %+v and the business ran %d times, want sid 1002 and once", again, processes[1002].harness.business)
	}
	published := broker.takePublished()
	if len(published) != 2 || published[0].msgID != "res."+command.ID || published[1].msgID != published[0].msgID ||
		receiptDigest(published[0].input.Completion) != receiptDigest(published[1].input.Completion) || !published[0].input.Completion.Success {
		t.Fatalf("results = %+v, want the same success twice under res.%s", published, command.ID)
	}
	u.round(u.clock.Now(), u.resultInput(published[0].input.Completion), u.resultInput(published[1].input.Completion))
	if record := u.record(command.SagaID); record.Status != StatusCompleted {
		t.Fatalf("saga = %s after the result and its duplicate, want completed", record.Status)
	}
}

// bindings 是用例里“玩家绑定在哪个 sid、是否正在换服”的事实（业务的绑定表的替身）。
type bindings struct {
	mu     sync.Mutex
	sid    map[int64]int32
	moving map[int64]bool
}

func (b *bindings) set(player int64, sid int32, moving bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sid[player], b.moving[player] = sid, moving
}

// admitBound 是路由步骤的属主核对（防第二个写者的最后一道校验）：载荷没有 sid → 拒收；玩家正在换服 → 退回、不给地址（换服
// 完成前照原地址重试）；玩家已绑定别的 sid → 退回、给出最新绑定；绑定在本 sid → 执行。
func (b *bindings) admitBound(p *sidProcess) func(context.Context, Command) error {
	return func(_ context.Context, command Command) error {
		var payload sidPayload
		if err := json.Unmarshal(command.Payload, &payload); err != nil || payload.SID <= 0 {
			return RejectStep("the payload names no sid to route by")
		}
		b.mu.Lock()
		bound, moving := b.sid[payload.Player], b.moving[payload.Player]
		b.mu.Unlock()
		switch {
		case moving:
			return BounceStep("", "player is moving between sids")
		case bound != p.sid:
			return BounceStep(SidRoute(bound), "player is bound to another sid")
		}
		return nil
	}
}

// 换服：玩家 7 绑定在 1000、正在换到 1001。投到 1000 的步骤被退回发送端（不执行、零写入、发回可重试失败、ack），
// 协调器按退避照原地址再派；换服完成后 1000 再次退回并给出新绑定 1001，协调器改投 1001，在那里执行恰好一次。全程没有
// 任何 nak（退回不是让别的进程争抢），1002 一次也没收到。
func TestAStepBouncedDuringAMoveRunsOnceOnTheNewSid(t *testing.T) {
	u := routedPartition(t)
	broker := newRouteBroker()
	facts := &bindings{sid: map[int64]int32{}, moving: map[int64]bool{}}
	facts.set(7, 1000, true)
	processes := subscribeSids(t, broker, u.e.layout, []int32{1000, 1001, 1002}, facts.admitBound)
	stream := u.e.layout.StreamOf(0)

	// 第一次尝试：投到 1000，玩家正在换服。
	r := u.round(u.clock.Now(), routedStart(t, u, "gift-move", sidPayload{Player: 7, SID: 1000}, SidRoute(1000)))
	subject, raw, first := dispatched(t, r)
	deliveries, waited := broker.emit(stream, subject, raw)
	if len(deliveries) != 1 || deliveries[0].sid != 1000 || deliveries[0].nak || waited != 0 {
		t.Fatalf("first attempt deliveries %+v (backoff %v), want one acknowledged delivery to sid 1000", deliveries, waited)
	}
	for sid, process := range processes {
		if process.harness.business != 0 || len(process.harness.entity.receipts) != 0 {
			t.Fatalf("sid %d ran the business %d times / wrote %d receipts while the player was moving", sid, process.harness.business, len(process.harness.entity.receipts))
		}
	}
	published := broker.takePublished()
	if len(published) != 1 || published[0].msgID != "res."+first.ID || published[0].input.Completion.Success || !published[0].input.Completion.Retryable || published[0].input.Completion.Route != "" {
		t.Fatalf("bounce result = %+v, want one retryable failure for %s without a new route", published, first.ID)
	}
	u.round(u.clock.Now(), u.resultInput(published[0].input.Completion))
	if record := u.record(first.SagaID); record.Route != SidRoute(1000) || record.Status != StatusPending || record.Attempt != 1 {
		t.Fatalf("after the bounce: route %q status %s attempt %d, want route 1000, pending the retry after attempt 1", record.Route, record.Status, record.Attempt)
	}

	// 换服完成：玩家绑定到 1001。下一次尝试仍投到原地址 1000，1000 退回并给出新绑定。
	facts.set(7, 1001, false)
	r = u.round(u.clock.Advance(backoffPassed))
	subject, raw, second := dispatched(t, r)
	if second.Route != SidRoute(1000) {
		t.Fatalf("second attempt route %q, want the old route 1000 (nobody told the sender yet)", second.Route)
	}
	if deliveries, _ := broker.emit(stream, subject, raw); len(deliveries) != 1 || deliveries[0].sid != 1000 || deliveries[0].nak {
		t.Fatalf("second attempt deliveries %+v, want one acknowledged delivery to sid 1000", deliveries)
	}
	published = broker.takePublished()
	if len(published) != 1 || published[0].input.Completion.Route != SidRoute(1001) {
		t.Fatalf("second bounce = %+v, want a retryable failure naming route 1001", published)
	}
	u.round(u.clock.Now(), u.resultInput(published[0].input.Completion))
	if record := u.record(first.SagaID); record.Route != SidRoute(1001) {
		t.Fatalf("after the bounce with a new route the saga's route is %q, want 1001", record.Route)
	}

	// 第三次尝试投到 1001，执行一次。
	r = u.round(u.clock.Advance(backoffPassed))
	subject, raw, third := dispatched(t, r)
	deliveries, _ = broker.emit(stream, subject, raw)
	if len(deliveries) != 1 || deliveries[0].sid != 1001 || deliveries[0].nak || third.Route != SidRoute(1001) {
		t.Fatalf("third attempt (route %q) deliveries %+v, want one acknowledged delivery to sid 1001", third.Route, deliveries)
	}
	if processes[1001].harness.business != 1 || processes[1000].harness.business+processes[1002].harness.business != 0 {
		t.Fatalf("business ran %d / %d / %d times on 1000 / 1001 / 1002, want once on 1001",
			processes[1000].harness.business, processes[1001].harness.business, processes[1002].harness.business)
	}
	published = broker.takePublished()
	if len(published) != 1 || !published[0].input.Completion.Success {
		t.Fatalf("result on 1001 = %+v, want one success", published)
	}
	u.round(u.clock.Now(), u.resultInput(published[0].input.Completion))
	if record := u.record(first.SagaID); record.Status != StatusCompleted {
		t.Fatalf("saga = %s, want completed", record.Status)
	}
}

// 换服一直不完成：每次派发都被退回。退回不算重试（维护者 2026-11-11）——退回次数远超 MaxAttempts，saga 仍在正向等着，
// 尝试一次也没耗；时间上受 saga 截止约束，截止一到走补偿（第 0 步没有可补偿的，saga failed）。业务一次也没执行。
func TestABouncedStepKeepsItsAttemptsAndCompensatesAtTheDeadline(t *testing.T) {
	u := routedPartition(t)
	broker := newRouteBroker()
	facts := &bindings{sid: map[int64]int32{}, moving: map[int64]bool{}}
	facts.set(7, 1000, true)
	processes := subscribeSids(t, broker, u.e.layout, []int32{1000, 1001}, facts.admitBound)
	stream := u.e.layout.StreamOf(0)
	deadline := u.clock.Now().Add(time.Minute)
	r := u.round(u.clock.Now(), routedStartUntil(t, u, "gift-stuck", sidPayload{Player: 7, SID: 1000}, SidRoute(1000), deadline))
	bounces := 3 * int(routedDefinition().Steps[0].MaxAttempts)
	var id string
	for bounce := 1; bounce <= bounces; bounce++ {
		subject, raw, command := dispatched(t, r)
		id = command.SagaID
		if command.Route != SidRoute(1000) {
			t.Fatalf("dispatch %d went to route %q, want 1000", bounce, command.Route)
		}
		broker.emit(stream, subject, raw)
		published := broker.takePublished()
		if len(published) != 1 || !published[0].input.Completion.Bounced {
			t.Fatalf("dispatch %d result = %+v, want one bounce", bounce, published)
		}
		r = u.round(u.clock.Now(), u.resultInput(published[0].input.Completion))
		record := u.record(id)
		if record.Status != StatusPending || record.Phase != PhaseForward {
			t.Fatalf("after bounce %d of %d (MaxAttempts %d) the saga is %s/%s (%q), want still pending forward: a bounce must not use up the attempts",
				bounce, bounces, routedDefinition().Steps[0].MaxAttempts, record.Status, record.Phase, record.LastError)
		}
		if bounce < bounces {
			r = u.round(u.clock.Advance(backoffPassed))
		}
	}
	if record := u.record(id); int(record.Bounces) != bounces || record.Attempt != record.Bounces {
		t.Fatalf("after %d bounces the record has attempt %d, bounces %d; want every attempt counted as a bounce", bounces, record.Attempt, record.Bounces)
	}
	u.clock.Set(deadline.Add(time.Millisecond))
	u.round(u.clock.Now())
	if record := u.record(id); record.Status != StatusFailed || record.LastError != "saga deadline exceeded" {
		t.Fatalf("at the deadline the saga is %s (%q), want failed by the deadline (nothing to compensate)", record.Status, record.LastError)
	}
	if processes[1000].harness.business+processes[1001].harness.business != 0 {
		t.Fatal("a bounced step ran the business")
	}
}

// 属主停机：投给 1002 的命令留在 1002 的 durable 上，别的 sid 拿不到；协调器照常按超时派下一次尝试（同一地址），
// 1002 恢复订阅后积压的两次尝试都投给它，业务只执行一次（第二次读到回执回放，结果的 CommandID 是生效的那次）。
func TestARoutedStepWaitsForItsOwnerToComeBack(t *testing.T) {
	u := routedPartition(t)
	broker := newRouteBroker()
	facts := &bindings{sid: map[int64]int32{}, moving: map[int64]bool{}}
	facts.set(7, 1002, false)
	subscribeSids(t, broker, u.e.layout, []int32{1000, 1001}, facts.admitBound)
	stream := u.e.layout.StreamOf(0)

	r := u.round(u.clock.Now(), routedStart(t, u, "gift-down", sidPayload{Player: 7, SID: 1002}, SidRoute(1002)))
	subject, raw, first := dispatched(t, r)
	if deliveries, _ := broker.emit(stream, subject, raw); len(deliveries) != 0 {
		t.Fatalf("with sid 1002 down the step went to %+v, want nowhere", deliveries)
	}
	// 1002 的 durable 在它第一次启动时就建好了：这里先让它登记、再停下（没有成员），消息留在 durable 上。
	owner := &sidProcess{sid: 1002, harness: newNativeHarness()}
	key := stream + " game-gift-debit-1002"
	broker.mu.Lock()
	if broker.durables[key] != nil {
		broker.mu.Unlock()
		t.Fatal("no process of sid 1002 subscribed, yet a durable for it exists")
	}
	broker.durables[key] = &brokerDurable{stream: stream, name: "game-gift-debit-1002", filter: u.e.layout.RoutedCommandFilter(SidRoute(1002), "gift.debit")}
	broker.order = append(broker.order, key)
	broker.mu.Unlock()
	broker.emit(stream, subject, raw)

	// 超时：协调器派第二次尝试，地址不变。
	r = u.round(u.clock.Advance(routedDefinition().Steps[0].Timeout + time.Millisecond))
	r = u.round(u.clock.Advance(backoffPassed))
	subject2, raw2, second := dispatched(t, r)
	if second.Route != SidRoute(1002) || second.Attempt != 2 {
		t.Fatalf("after the timeout attempt %d went to route %q, want attempt 2 to 1002", second.Attempt, second.Route)
	}
	if deliveries, _ := broker.emit(stream, subject2, raw2); len(deliveries) != 0 {
		t.Fatalf("the second attempt reached %+v while 1002 is down", deliveries)
	}

	// 1002 回来：绑定它的 durable，积压的两次尝试依次投给它。
	config := StepConsumerConfig{Durable: "game-gift-debit", Topic: "gift.debit", Route: SidRoute(1002), Admit: facts.admitBound(owner)}
	if _, err := SubscribeNativeStep(context.Background(), broker.as(1002), u.e.layout, config, owner.harness.body); err != nil {
		t.Fatal(err)
	}
	if owner.harness.business != 1 {
		t.Fatalf("after 1002 came back the business ran %d times, want once", owner.harness.business)
	}
	published := broker.takePublished()
	if len(published) != 2 || published[0].input.Completion.CommandID != first.ID || published[1].input.Completion.CommandID != first.ID {
		t.Fatalf("results = %+v, want two results both naming the effective attempt %s", published, first.ID)
	}
	u.round(u.clock.Now(), u.resultInput(published[0].input.Completion))
	if record := u.record(first.SagaID); record.Status != StatusCompleted {
		t.Fatalf("saga = %s, want completed by the late result of the first attempt", record.Status)
	}
}

// 不带路由 sid：定义有路由步骤而启动没给 Route，启动被拒绝（不兼容、不兜底）；Route 不是合法路由键同样拒绝。
// 载荷里没有 sid 的命令（理论上到不了这里）在属主那里被拒收：不执行，发回不可重试失败，协调器随即判失败。
func TestARoutedSagaWithoutARouteIsRefused(t *testing.T) {
	u := routedPartition(t)
	for _, route := range []string{"", "10.02", "a b"} {
		r := u.round(u.clock.Now(), routedStart(t, u, "gift-noroute-"+strconv.Itoa(len(route)), sidPayload{Player: 7, SID: 1000}, route))
		if len(r.events) != 0 {
			t.Fatalf("route %q: a routed saga started (%d events), want the start refused", route, len(r.events))
		}
	}
	if _, err := u.e.StartSaga(context.Background(), StartRequest{Type: "gift", DefinitionVersion: 1, BusinessKey: "k", Data: []byte(`{}`)}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("StartSaga without a route = %v, want ErrInvalidRecord", err)
	}

	broker := newRouteBroker()
	facts := &bindings{sid: map[int64]int32{}, moving: map[int64]bool{}}
	processes := subscribeSids(t, broker, u.e.layout, []int32{1000}, facts.admitBound)
	r := u.round(u.clock.Now(), routedStart(t, u, "gift-nosid", sidPayload{Player: 7}, SidRoute(1000)))
	subject, raw, command := dispatched(t, r)
	broker.emit(u.e.layout.StreamOf(0), subject, raw)
	published := broker.takePublished()
	if len(published) != 1 || published[0].input.Completion.Success || published[0].input.Completion.Retryable || processes[1000].harness.business != 0 {
		t.Fatalf("result = %+v (business %d), want one non-retryable failure and nothing run", published, processes[1000].harness.business)
	}
	u.round(u.clock.Now(), u.resultInput(published[0].input.Completion))
	if record := u.record(command.SagaID); record.Status != StatusFailed {
		t.Fatalf("saga = %s, want failed", record.Status)
	}
}

// 维护者 2026-11-11（方案 A）：退回不消耗尝试次数，路由步骤的正向唯一的时间上限就是 saga 截止，所以有路由步骤的
// saga 启动必须带截止，Resume 也不许把它清掉；没有截止的路由 saga 会按 BackoffMax 一直退回重派。不路由的定义不受影响。
func TestARoutedSagaMustCarryADeadline(t *testing.T) {
	u := routedPartition(t)
	if _, err := u.e.StartSaga(context.Background(), StartRequest{Type: "gift", DefinitionVersion: 1, BusinessKey: "k-nodeadline", Data: []byte(`{}`), Route: SidRoute(1000)}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("StartSaga with a route and no deadline = %v, want ErrInvalidRecord: a bounce does not use up attempts, so the deadline is the only bound", err)
	}
	r := u.round(u.clock.Now(), routedStartUntil(t, u, "gift-nodeadline", sidPayload{Player: 7, SID: 1000}, SidRoute(1000), time.Time{}))
	if len(r.events) != 0 {
		t.Fatalf("a routed saga without a deadline started (%d events), want the start refused", len(r.events))
	}

	definition := routedDefinition()
	failed := Record{Status: StatusFailed, Phase: PhaseForward, Route: SidRoute(1000), DeadlineAt: u.clock.Now().Add(time.Minute)}
	if _, err := planResume(failed, definition, true, ResumeRequest{ID: "s", Now: u.clock.Now(), ClearDeadline: true}, time.Time{}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Resume clearing the deadline of a routed saga = %v, want ErrInvalidRecord", err)
	}
	if _, err := planResume(failed, definition, true, ResumeRequest{ID: "s", Now: u.clock.Now()}, u.clock.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Resume with a new deadline = %v, want accepted", err)
	}
}

// Engine.RoutedTopic（wiring/saga 的 Steps 用它核对消费者与定义）：路由步骤的正向与补偿主题（含不写补偿主题时的
// <forward>.compensate）都是路由的；不路由的步骤不是；没有定义用到的主题 known=false。
func TestEngineReportsWhichTopicsAreRouted(t *testing.T) {
	u := routedPartition(t)
	implicit := Definition{Type: "trade", Version: 1, Steps: []Step{{Name: "hold", ForwardTopic: "trade.hold", Routed: true,
		Timeout: time.Second, MaxAttempts: 1, BackoffMin: time.Millisecond, BackoffMax: time.Millisecond}}}
	if err := u.e.Register(implicit); err != nil {
		t.Fatal(err)
	}
	for topic, want := range map[string][2]bool{
		"gift.debit": {true, true}, "gift.debit.compensate": {true, true}, "trade.hold.compensate": {true, true},
		"reserve": {false, true}, "reserve.cancel": {false, true}, "nobody.uses.this": {false, false},
	} {
		if routed, known := u.e.RoutedTopic(topic); routed != want[0] || known != want[1] {
			t.Errorf("RoutedTopic(%q) = %t, %t; want %t, %t", topic, routed, known, want[0], want[1])
		}
	}
}
