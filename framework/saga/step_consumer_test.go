package saga

import (
	"context"
	"encoding/json"
	"errors"
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

	if err := js.deliver(t, layout, first, []byte(`{"v":2,"part":3}`)); !fnats.IsPermanent(err) {
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
