package saga

// saga 方向 ②：SubscribeMongoStep 对收件箱的各个判定怎么回应投递（与原生消费者同一张表）。
//
//   - 回放同一操作实例另一次尝试的成功：发布那次的 completion（CommandID 是那次的），不执行 handler，ack；
//   - 另一次尝试持有有效租约：返回错误（nak），不执行；
//   - 过期投递、没有自己的回执：把同一操作实例已生效的成功重发后 ack（与原生 U-0280 复核 2 相同；旧 Mongo 路径只查自己的回执）；
//   - 被接替的尝试：ack，不执行、不发布。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

func TestMongoStepConsumerFollowsTheOperationInbox(t *testing.T) {
	operation := "gift-1:1:1"
	envelope := func(t *testing.T, command Command) *fnats.JetStreamMsg {
		raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: command})
		if err != nil {
			t.Fatal(err)
		}
		return &fnats.JetStreamMsg{Data: raw}
	}
	subscribe := func(t *testing.T, inbox *MongoCommandInbox, handler StepHandler) *startJetStream {
		t.Helper()
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "gift.deliver"}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, handler); err != nil {
			t.Fatal(err)
		}
		return client
	}
	succeed := func(context.Context, Command) (Completion, error) {
		return Completion{Success: true, Data: []byte("sent")}, nil
	}
	published := func(t *testing.T, client *startJetStream) Completion {
		t.Helper()
		if !strings.HasPrefix(client.subject, "roost.saga.result.") {
			t.Fatalf("published %q, want a saga result", client.subject)
		}
		var result completionEnvelope
		if err := json.Unmarshal(client.data, &result); err != nil {
			t.Fatal(err)
		}
		return result.Completion
	}

	t.Run("another attempt's success is replayed, not re-executed", func(t *testing.T) {
		inbox, _ := NewMongoCommandInbox(mongotest.NewClient(), "saga", "_game_gift_step_inbox")
		k := mongoStepCommand(operation, 1, time.Now().Add(time.Minute))
		if _, _, err := inbox.Handle(context.Background(), k, succeed); err != nil {
			t.Fatal(err)
		}
		client := subscribe(t, inbox, func(context.Context, Command) (Completion, error) {
			t.Fatal("the handler ran although an earlier attempt of the operation had succeeded")
			return Completion{}, nil
		})
		next := mongoStepCommand(operation, 2, time.Now().Add(2*time.Minute))
		if err := client.handler(context.Background(), envelope(t, next)); err != nil {
			t.Fatalf("delivery of %s = %v, want nil (ack after the replay)", next.ID, err)
		}
		if got := published(t, client); got.CommandID != k.ID || !got.Success {
			t.Fatalf("replayed %+v, want the completion of %s as it was recorded", got, k.ID)
		}
	})

	t.Run("a live attempt makes the next one wait", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewMongoCommandInbox(mongoClient, "saga", "_game_gift_step_inbox")
		k := mongoStepCommand(operation, 1, time.Now().Add(time.Minute))
		// k 拿到租约（Reserve 已提交），执行事务还没提交。
		if _, err := inbox.reserve(context.Background(), k, mustCommandDigest(t, k)); err != nil {
			t.Fatal(err)
		}
		client := subscribe(t, inbox, func(context.Context, Command) (Completion, error) {
			t.Fatal("the handler ran while another attempt held a live lease")
			return Completion{}, nil
		})
		next := mongoStepCommand(operation, 2, time.Now().Add(2*time.Minute))
		if err := client.handler(context.Background(), envelope(t, next)); err == nil {
			t.Fatal("delivery while another attempt holds a live lease = nil, want an error (nak and look again later)")
		}
		if client.subject != "" {
			t.Fatalf("published %q while waiting", client.subject)
		}
	})

	t.Run("an expired delivery replays the operation's success before acking", func(t *testing.T) {
		inbox, _ := NewMongoCommandInbox(mongotest.NewClient(), "saga", "_game_gift_step_inbox")
		k := mongoStepCommand(operation, 1, time.Now().Add(time.Minute))
		if _, _, err := inbox.Handle(context.Background(), k, succeed); err != nil {
			t.Fatal(err)
		}
		client := subscribe(t, inbox, succeed)
		expired := mongoStepCommand(operation, 2, time.Now().Add(-time.Second))
		if err := client.handler(context.Background(), envelope(t, expired)); err != nil {
			t.Fatalf("expired delivery = %v, want nil", err)
		}
		if got := published(t, client); got.CommandID != k.ID {
			t.Fatalf("expired delivery replayed %+v, want the success of %s (it may have been dropped during the backoff)", got, k.ID)
		}
	})

	t.Run("a superseded attempt is acknowledged without running", func(t *testing.T) {
		inbox, _ := NewMongoCommandInbox(mongotest.NewClient(), "saga", "_game_gift_step_inbox")
		now := time.Now()
		k := mongoStepCommand(operation, 1, now.Add(time.Minute))
		if _, err := inbox.reserve(context.Background(), k, mustCommandDigest(t, k)); err != nil {
			t.Fatal(err)
		}
		// k 的租约过期后 k+1 接替它。
		inbox.now = func() time.Time { return now.Add(2 * time.Minute) }
		next := mongoStepCommand(operation, 2, now.Add(3*time.Minute))
		if reservation, err := inbox.reserve(context.Background(), next, mustCommandDigest(t, next)); err != nil || reservation.Duplicate {
			t.Fatalf("k+1 reserve = %+v err=%v, want it to take over", reservation, err)
		}
		inbox.now = time.Now
		client := subscribe(t, inbox, func(context.Context, Command) (Completion, error) {
			t.Fatal("a superseded attempt ran")
			return Completion{}, nil
		})
		if err := client.handler(context.Background(), envelope(t, k)); err != nil {
			t.Fatalf("delivery of the superseded attempt = %v, want nil (ack)", err)
		}
		if client.subject != "" {
			t.Fatalf("a superseded attempt published %q", client.subject)
		}
	})
}
