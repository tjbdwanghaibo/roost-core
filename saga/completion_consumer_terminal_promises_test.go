package saga

// 观察 O-S5-1（N06 S5 review）：两个结果消费者对“重投也不会变”的错误处理不一致。原生结果消费者（effect 流）对
// ErrNotWaiting / ErrNotFound / ErrInvalidRecord / ErrDefinitionMissing Term；普通结果流（SubscribeCompletions，
// Mongo 步骤与收件箱回放发布在这里）把它们原样返回，按退避 nak 到 MaxDeliver（默认 25 000 次），期间占着 MaxAckPending。
//
// saga 方向 ②之后两条流上的 completion 都来自带操作实例回放的收件箱：退避中到达、被丢弃的成功会由下一次尝试回放、
// 由过期投递重发，普通流不再需要靠 nak 等协调器回到等待。两个消费者共用 isTerminalCompletionError，
// 并把 ErrIdentityConflict（同一 CommandID 不同内容，回执是持久的，重投不会变）加入。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

func TestCompletionConsumersTermTheSameTerminalErrors(t *testing.T) {
	ctx := context.Background()
	mongoClient := mongotest.NewClient()
	store, err := NewMongoStore(mongoClient, MongoStoreOptions{Database: "o_s5_1"})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	record, err := engine.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "o-s5-1"})
	if err != nil {
		t.Fatal(err)
	}
	operation := operationKey(record.ID, PhaseForward, 0)
	conflicting := Completion{CommandID: commandID(operation, 0, 1), IdempotencyKey: operation, SagaID: record.ID, Success: true, CompletedAt: time.Now()}
	other := conflicting
	other.Data = []byte("another result under the same command id")
	digest, err := completionDigest(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := mongoClient.Collection("o_s5_1", defaultCompletionCollection).Seed(completionDoc{ID: conflicting.CommandID, Digest: digest, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		completion Completion
		want       error
	}{
		{"saga does not exist", Completion{CommandID: "missing:0:0:1", IdempotencyKey: "missing:0:0", SagaID: "missing", Success: true}, ErrNotFound},
		{"step is not waiting and has no history", Completion{CommandID: commandID(operation, 0, 7), IdempotencyKey: operation, SagaID: record.ID, Success: true}, ErrNotWaiting},
		{"same command id, different result", conflicting, ErrIdentityConflict},
	}
	plain := &startJetStream{}
	if _, err := SubscribeCompletions(ctx, plain, CompletionConsumerConfig{Stream: "SAGA", Durable: "results", SubjectPrefix: "roost.saga"}, engine); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(completionEnvelope{Version: WireVersion, Completion: tc.completion})
			if err != nil {
				t.Fatal(err)
			}
			err = plain.handler(ctx, &fnats.JetStreamMsg{Data: raw})
			if !errors.Is(err, tc.want) || !fnats.IsPermanent(err) {
				t.Fatalf("plain result stream = %v (permanent=%v), want %v terminated: a redelivery cannot change it, and naking it holds a MaxAckPending slot until MaxDeliver", err, fnats.IsPermanent(err), tc.want)
			}
			effect, err := NewCompletionEffect(tc.completion)
			if err != nil {
				t.Fatal(err)
			}
			raw, err = json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
			if err != nil {
				t.Fatal(err)
			}
			if err := handleNestCompletion(ctx, &fnats.JetStreamMsg{Data: raw}, engine); !errors.Is(err, tc.want) || !fnats.IsPermanent(err) {
				t.Fatalf("native result stream = %v (permanent=%v), want %v terminated like the plain stream", err, fnats.IsPermanent(err), tc.want)
			}
		})
	}
}
