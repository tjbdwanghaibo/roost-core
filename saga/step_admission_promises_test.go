package saga

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
)

// 多进程：一条命令投递到不该执行它的进程时，该进程必须在“动任何状态之前”拒绝。
// 来源是 game-demo 第十九批的实跑（GAME_DEMO_TEMPLATE §9.11.3）。
//
// 旧行为：两个步骤消费者都先 Reserve（DataEngine 走 Mongo 租约、Mongo inbox 走去重记录）
// 再调用 handler，所以“这台机器不该执行”只能写在 handler 里 —— 那时租约已经被错误的进程
// 拿走了，真正的所有者在租约到期（默认 2 分钟）前无法执行，消息在两个消费者之间反复重投。
// 实跑里一次 16 机器人的礼物 saga 产生了 248 次投递、24 条卡住。
//
// 承诺：StepConsumerConfig.Admit 在 Reserve 之前被调用；它返回错误时不留下任何认领，
// handler 不执行，错误原样返回（消息被 nak，交给下一个消费者）。
func TestStepConsumersAdmitBeforeTakingTheClaim(t *testing.T) {
	refused := errors.New("not this process's player")
	now := time.Now()
	command := Command{
		ID: "cmd-1", IdempotencyKey: "op-1", SagaID: "saga-1", SagaType: "gift", DefinitionVersion: 1,
		BusinessKey: "gift-1", Step: 0, StepName: "debit", Phase: PhaseForward, Attempt: 1,
		Topic: "debit", Payload: []byte("x"), CreatedAt: now, DeadlineAt: now.Add(time.Minute),
	}
	raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: command})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("dataengine", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, err := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-2", LeaseDuration: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		var ran bool
		handler := func(context.Context, Command) (Completion, error) {
			ran = true
			return Completion{}, nil
		}
		config := StepConsumerConfig{
			Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "debit", AckWait: 30 * time.Second,
			Admit: func(_ context.Context, c Command) error {
				if c.ID != command.ID {
					t.Fatalf("admit saw command %q", c.ID)
				}
				return refused
			},
		}
		if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, handler); err != nil {
			t.Fatal(err)
		}
		err = client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw})
		if !errors.Is(err, refused) {
			t.Fatalf("delivery to a process that may not run the command = %v, want the admission refusal", err)
		}
		if ran {
			t.Fatal("the handler ran on a process that was refused admission")
		}
		if count := mongoClient.Collection("game", dataEngineClaimCollection).Len(); count != 0 {
			t.Fatalf("a refused delivery left %d claim(s) behind; the owner cannot run the command until they expire", count)
		}
	})

	t.Run("mongo", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, err := NewMongoCommandInbox(mongoClient, "game", "")
		if err != nil {
			t.Fatal(err)
		}
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		var ran bool
		handler := func(context.Context, Command) (Completion, error) {
			ran = true
			return Completion{Success: true}, nil
		}
		config := StepConsumerConfig{
			Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "debit",
			Admit:  func(context.Context, Command) error { return refused },
		}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, handler); err != nil {
			t.Fatal(err)
		}
		err = client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw})
		if !errors.Is(err, refused) {
			t.Fatalf("delivery to a process that may not run the command = %v, want the admission refusal", err)
		}
		if ran {
			t.Fatal("the handler ran on a process that was refused admission")
		}
		if _, found, err := inbox.Replay(context.Background(), command); err != nil || found {
			t.Fatalf("a refused delivery left a receipt: found=%v err=%v", found, err)
		}
	})

	// An admitting Admit changes nothing: the command runs as before.
	t.Run("admitted", func(t *testing.T) {
		mongoClient := mongotest.NewClient()
		inbox, _ := NewMongoCommandInbox(mongoClient, "game", "")
		client := &startJetStream{}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		var ran bool
		handler := func(context.Context, Command) (Completion, error) {
			ran = true
			return Completion{Success: true}, nil
		}
		config := StepConsumerConfig{
			Stream: "ROOST_SAGA", Durable: "game-gift-deliver", Topic: "debit",
			Admit:  func(context.Context, Command) error { return nil },
		}
		if _, err := SubscribeMongoStep(context.Background(), client, transport, inbox, config, handler); err != nil {
			t.Fatal(err)
		}
		if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatal(err)
		}
		if !ran {
			t.Fatal("an admitted command did not run")
		}
	})
}
