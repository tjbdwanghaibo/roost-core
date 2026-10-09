package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// U-0281：已过截止时间、又没有回执的步骤命令必须被终结（ack），不能无限 nak。
//
// 截止时间是协调器给这次尝试的等待期限（Command.DeadlineAt = 该尝试的 NextRunAt）：到点后协调器
// 自己按超时进入下一次尝试或补偿，不需要、也不等待步骤侧对这条旧消息再说什么。所以过期命令只剩
// 一件事可做：如果这次尝试已经提交过结果（回执存在），就让结果送达；否则什么也不做并结束这条消息。
//
// 旧行为：原生路径（SubscribeDataEngineStep）对“过期且无回执”返回 context.DeadlineExceeded。
// 它不算永久失败，适配层按 0.25～30s 退避一直 nak 到 MaxDeliver=25000（约 8.7 天），这些消息长期
// 占着 MaxAckPending。演练 drill6 里共享 durable game-gift-debit 被 256 条这样的旧尝试占满（它们在
// 发送方 sid 宕机期间被 Admit 拒绝、随后过期），两个 sid 新发的赠礼全部超时 Failed。Mongo 路径
// （SubscribeMongoStep）在 Replay 未命中时本来就返回 nil，这里的 mongo 子用例是同一承诺的守卫。
//
// 承诺：过期命令无回执时返回 nil（ack），不调用 Admit、不 Reserve、不执行 handler、不发布任何
// completion；有回执时行为不变（原生路径 ack，Mongo 路径重发已提交的 completion）；读回执遇到暂时性
// 错误仍返回错误交给重投，因为那时无法判断它有没有回执。
func TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered(t *testing.T) {
	now := time.Now().UTC()
	expired := Command{
		ID: "gift-1:1:0:2", IdempotencyKey: "gift-1:1:0", SagaID: "gift-1", SagaType: "gift_item", DefinitionVersion: 1,
		BusinessKey: "gift-1", Step: 0, StepName: "debit", Phase: PhaseForward, Attempt: 2,
		Topic: "gift_item.debit", Payload: []byte("x"), CreatedAt: now.Add(-time.Minute), DeadlineAt: now.Add(-time.Second),
	}
	raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: expired})
	if err != nil {
		t.Fatal(err)
	}
	refuseEverywhere := func(context.Context, Command) error {
		t.Fatal("Admit was asked about an expired command; admission must not decide whether a stale message lives on")
		return nil
	}
	noHandler := func(context.Context, Command) (Completion, error) {
		t.Fatal("the handler ran for a command past its deadline")
		return Completion{}, nil
	}

	t.Run("dataengine without receipt", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, err := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift_item.debit", AckWait: 30 * time.Second, Admit: refuseEverywhere}
		if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		before := expiredUnexecuted()
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("expired command without a receipt = %v, want nil (ack): any error is nak'd and redelivered until MaxDeliver, holding a MaxAckPending slot all the while", err)
		}
		if grown := expiredUnexecuted() - before; grown != 1 {
			t.Fatalf("saga.step.expired_unexecuted_total grew by %d, want 1: an acknowledged-without-running command must be countable", grown)
		}
		if count := mongoClient.Collection("game", dataEngineOperationCollection).Len(); count != 0 {
			t.Fatalf("an expired command left %d claim(s) behind", count)
		}
		if client.subject != "" {
			t.Fatalf("an expired command without a receipt published %q", client.subject)
		}
	})

	t.Run("dataengine with receipt", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
		completion := Completion{CommandID: expired.ID, IdempotencyKey: expired.IdempotencyKey, SagaID: expired.SagaID, Success: true, CompletedAt: now}
		effect, _ := NewCompletionEffect(completion)
		if err := inboxReceipts(mongoClient).Seed(dataEngineReceipt{ID: dataEngineStepNamespace + "/" + expired.ID, Digest: mustCommandDigest(t, expired), Payload: effect.Payload}); err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift_item.debit", AckWait: 30 * time.Second, Admit: refuseEverywhere}
		if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("expired command with a receipt = %v, want nil", err)
		}
	})

	t.Run("dataengine receipt unreadable", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
		outage := errors.New("mongo unavailable")
		inboxReceipts(mongoClient).Errors["FindOne"] = outage
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift_item.debit", AckWait: 30 * time.Second, Admit: refuseEverywhere}
		if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); !errors.Is(err, outage) {
			t.Fatalf("expired command whose receipt could not be read = %v, want the read error (redelivered: a receipt may exist)", err)
		}
	})

	t.Run("mongo without receipt", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, err := NewMongoCommandInbox(mongoClient, "saga", "_game_gift_step_inbox")
		if err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "gift_item.debit", Admit: refuseEverywhere}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("expired command without a receipt = %v, want nil (ack)", err)
		}
		if client.subject != "" {
			t.Fatalf("an expired command without a receipt published %q", client.subject)
		}
		if count := mongoClient.Collection("saga", "_game_gift_step_inbox").Len(); count != 0 {
			t.Fatalf("an expired command left %d receipt(s) behind", count)
		}
	})

	t.Run("mongo with receipt", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewMongoCommandInbox(mongoClient, "saga", "_game_gift_step_inbox")
		completion := Completion{CommandID: expired.ID, IdempotencyKey: expired.IdempotencyKey, SagaID: expired.SagaID, Success: true, CompletedAt: now}
		encoded, _ := json.Marshal(completion)
		if err := mongoClient.Collection("saga", "_game_gift_step_inbox").Seed(commandReceiptDoc{ID: expired.ID, Digest: mustCommandDigest(t, expired), Completion: encoded, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "gift_item.debit", Admit: refuseEverywhere}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, noHandler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("expired command with a receipt = %v, want nil", err)
		}
		if !strings.HasPrefix(client.subject, "roost.saga.result.") {
			t.Fatalf("an expired command with a committed completion published %q, want the completion replayed to the coordinator", client.subject)
		}
	})
}

func expiredUnexecuted() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "saga.step.expired_unexecuted_total" {
			return metric.Value
		}
	}
	return 0
}
