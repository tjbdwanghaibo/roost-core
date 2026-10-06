package saga

// 发版前审查观察（O-S5-1 之后）：结果到达时协调器还没有这个定义版本，是滚动发布里的暂时状态，不是终态。
//
// O-S5-1 让两个结果消费者共用 isTerminalCompletionError，ErrDefinitionMissing 在里面：滚动发布时，新定义版本的
// saga 由新进程派发，结果却可能先被还没升级的协调器消费，Complete 返回 ErrDefinitionMissing，消息被 Term 掉。
// 记录一直等到步骤超时；领到它的若也是旧进程，就按定义缺失 fence 到 ManualRequired。定义会随新进程上线，重投
// 能被接收，所以它要 nak 退避：定义上线后被接收；定义一直不来时由协调器自己的定义缺失 fence（NC-250，放弃关闭、
// 之后的迟到成功 ack 并告警）收尾，MaxDeliver 兜底。两条结果流仍共用同一个判断（O-S5-1 不拆开）。

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

func TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated(t *testing.T) {
	type deliver func(t *testing.T, completer Completer, completion Completion) error
	streams := map[string]deliver{
		"plain result stream": func(t *testing.T, completer Completer, completion Completion) error {
			t.Helper()
			engine := completer.(*Engine)
			plain := &startJetStream{}
			if _, err := SubscribeCompletions(context.Background(), plain, CompletionConsumerConfig{Stream: "SAGA", Durable: "results", SubjectPrefix: "roost.saga"}, engine); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(completionEnvelope{Version: WireVersion, Completion: completion})
			if err != nil {
				t.Fatal(err)
			}
			return plain.handler(context.Background(), &fnats.JetStreamMsg{Data: raw})
		},
		"native result stream": func(t *testing.T, completer Completer, completion Completion) error {
			t.Helper()
			effect, err := NewCompletionEffect(completion)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
			if err != nil {
				t.Fatal(err)
			}
			return handleNestCompletion(context.Background(), &fnats.JetStreamMsg{Data: raw}, completer)
		},
	}
	for name, deliver := range streams {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, _, rollingIn, waiting := waitingOnANewDefinition(t)
			completion := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true}

			// 结果先到了还没有这个定义版本的协调器。
			err := deliver(t, rollingIn, completion)
			if !errors.Is(err, ErrDefinitionMissing) || fnats.IsPermanent(err) {
				t.Fatalf("completion before the definition is registered = %v (permanent=%v); want ErrDefinitionMissing nak'd with backoff — the definition arrives with the new process, a terminated result is gone", err, fnats.IsPermanent(err))
			}
			if current, _ := store.Get(ctx, waiting.ID); current.Status != StatusWaiting || current.OperationKey != waiting.OperationKey {
				t.Fatalf("the refused completion moved the record: %+v", current)
			}

			// 新定义上线，同一条消息重投：被接收，记录推进到下一步。
			if err := rollingIn.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			if err := deliver(t, rollingIn, completion); err != nil {
				t.Fatalf("redelivery after the definition is registered = %v, want accepted", err)
			}
			if current, _ := store.Get(ctx, waiting.ID); current.Status == StatusWaiting || current.Step != 1 || current.CompletedSteps != 1 {
				t.Fatalf("after the redelivery the record is %+v, want step 0 completed", current)
			}
		})
	}
}

// 定义一直不来：nak 不会无限占着。记录的步骤超时后，没有定义的协调器按 NC-250 fence 到 ManualRequired
// （放弃关闭这次操作），之后重投的成功不再是“定义缺失”，按迟到成功收尾，不再 nak。
func TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence(t *testing.T) {
	ctx := context.Background()
	store, _, rollingIn, waiting := waitingOnANewDefinition(t)
	completion := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(nestwal.EffectEnvelope{TransactionID: "tx-1", EffectID: effect.ID, Topic: effect.Topic, Key: effect.Key, Payload: effect.Payload})
	if err != nil {
		t.Fatal(err)
	}
	message := &fnats.JetStreamMsg{Data: raw}
	if err := handleNestCompletion(ctx, message, rollingIn); !errors.Is(err, ErrDefinitionMissing) || fnats.IsPermanent(err) {
		t.Fatalf("first delivery = %v (permanent=%v), want a retryable ErrDefinitionMissing", err, fnats.IsPermanent(err))
	}
	if err := rollingIn.processClaimed(ctx, waiting, waiting.NextRunAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if fenced, _ := store.Get(ctx, waiting.ID); fenced.Status != StatusManualRequired {
		t.Fatalf("precondition: the coordinator without the definition did not fence the record: %+v", fenced)
	}
	err = handleNestCompletion(ctx, message, rollingIn)
	if err != nil && !fnats.IsPermanent(err) {
		t.Fatalf("redelivery after the fence = %v, want it settled (ack or Term), not nak'd again", err)
	}
	if late := rollingIn.Stats().LateAfterAbandon; late != 1 {
		t.Fatalf("LateAfterAbandon = %d, want the success after the fence reported once", late)
	}
}

// waitingOnANewDefinition 建一条在等第 0 步结果的记录：upgraded 是有这个定义版本的新协调器（派发了它），
// rollingIn 是同一存储上还没有注册这个定义的旧协调器。
func waitingOnANewDefinition(t *testing.T) (*MongoStore, *Engine, *Engine, Record) {
	t.Helper()
	ctx := context.Background()
	store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "rollout"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	publish := PublishFunc(func(context.Context, Command) error { return nil })
	upgraded, err := NewEngine(store, publish, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := upgraded.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	rollingIn, err := NewEngine(store, publish, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	record, err := upgraded.StartSaga(ctx, StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "rollout"})
	if err != nil {
		t.Fatal(err)
	}
	if err := upgraded.processClaimed(ctx, record, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	waiting, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Status != StatusWaiting || waiting.Step != 0 {
		t.Fatalf("precondition: want step 0 dispatched and waiting, got %+v", waiting)
	}
	return store, upgraded, rollingIn, waiting
}
