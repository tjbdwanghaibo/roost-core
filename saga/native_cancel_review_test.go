package saga

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// N06：取消等待不等于撤销已经可能进入 WAL 的业务。普通取消保留租约，晚到权威回执后重投不重跑。
// 明确 WAL 前 ErrFencedEntityPending 才交还租约。这里注入持久回执，不冒认真实 WAL/投影验收。
func TestNativeStepCancellationAndFenceRecovery(t *testing.T) {
	for _, scenario := range []string{"cancelled_delivery", "fenced_before_wal"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			backend := mongotest.NewClient()
			inbox, err := NewDataEngineStepInbox(backend, "game", DataEngineStepInboxOptions{Owner: "native-review", LeaseDuration: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			inbox.now = func() time.Time { return now }
			command := dataEngineCommand("native-cancel", "native-operation", "initial")
			client := &startJetStream{}
			transport, err := NewJetStreamPublisher(client, "roost.saga")
			if err != nil {
				t.Fatal(err)
			}
			cancelCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			calls := 0
			var tokens []uint64
			_, err = SubscribeDataEngineStep(ctx, client, transport, inbox, StepConsumerConfig{Stream: "SAGA", Durable: "native-review", Topic: command.Topic}, func(deliveryCtx context.Context, c Command) (Completion, error) {
				calls++
				reservation, ok := ReservationFromContext(deliveryCtx)
				if !ok {
					t.Fatal("missing active reservation")
				}
				tokens = append(tokens, reservation.Token)
				if calls == 1 {
					if scenario == "cancelled_delivery" {
						cancel()
						return Completion{}, context.Canceled
					}
					return Completion{}, coredata.ErrFencedEntityPending
				}
				// 第二次 handler 代表业务最终持久提交；只使用权威回执推进 ACK，不信返回值。
				completion := Completion{CommandID: c.ID, IdempotencyKey: c.IdempotencyKey, SagaID: c.SagaID, Success: true}
				effect, err := NewCompletionEffect(completion)
				if err != nil {
					return Completion{}, err
				}
				_, err = inbox.receipts().InsertOne(deliveryCtx, dataEngineReceipt{ID: dataEngineStepNamespace + "/" + c.ID, Digest: mustCommandDigest(t, c), Payload: effect.Payload})
				return completion, err
			})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: command})
			if err != nil {
				t.Fatal(err)
			}
			message := &fnats.JetStreamMsg{Data: raw}
			firstCtx := ctx
			want := coredata.ErrFencedEntityPending
			if scenario == "cancelled_delivery" {
				firstCtx = cancelCtx
				want = context.Canceled
			}
			if err := client.handler(firstCtx, message); !errors.Is(err, want) {
				t.Fatalf("first delivery=%v", err)
			}
			var claim stepOperation
			if err := inbox.operations().FindOne(ctx, bson.M{"_id": command.IdempotencyKey}, &claim); err != nil {
				t.Fatal(err)
			}
			if claim.CommandID != command.ID || claim.LeaseToken != 1 || claim.Status != operationStatusPending {
				t.Fatalf("first claim=%+v", claim)
			}
			if scenario == "cancelled_delivery" {
				if !claim.LeaseUntil.After(now) {
					t.Fatal("unknown in-flight result lost lease")
				}
				if reservation, err := inbox.Reserve(ctx, command); err != nil || !reservation.Duplicate {
					t.Fatalf("cancel retry lost fencing: %+v %v", reservation, err)
				}
				completion := Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true}
				effect, err := NewCompletionEffect(completion)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := inbox.receipts().InsertOne(ctx, dataEngineReceipt{ID: dataEngineStepNamespace + "/" + command.ID, Digest: mustCommandDigest(t, command), Payload: effect.Payload}); err != nil {
					t.Fatal(err)
				}
			} else if claim.LeaseUntil.After(now) {
				t.Fatal("unused fenced lease was not released")
			}
			for i := 0; i < 2; i++ {
				if err := client.handler(ctx, message); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cancelled_delivery" && calls != 1 {
				t.Fatalf("late receipt reran business: calls=%d", calls)
			}
			if scenario == "fenced_before_wal" && (calls != 2 || len(tokens) != 2 || tokens[1] != tokens[0]+1) {
				t.Fatalf("fence recovery did not acquire new generation: calls=%d tokens=%v", calls, tokens)
			}
			if err := inbox.operations().FindOne(ctx, bson.M{"_id": command.IdempotencyKey}, &claim); err != nil || claim.CommandID != command.ID || claim.Status != operationStatusSettled {
				t.Fatalf("claim not settled=%+v err=%v", claim, err)
			}
		})
	}
}
