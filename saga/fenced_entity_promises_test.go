package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
)

// RR-20260926-30：原生步骤的 Nest 事务在 WAL 准入处被实体屏障拒绝（ErrFencedEntityPending，常见是本命令
// 上一次投递的记录还没有投影结果）。这次事务整体回滚，没有带本 token 的记录进入 WAL。
// 承诺：消费者交还这次刚拿到的租约，屏障解除后的重投能立刻重新 Reserve 并再次执行 handler，
// 而不是读到“租约有效”的 Duplicate、一直等到本次租约自然过期；其他 handler 错误照旧保留租约。
func TestDataEngineStepHandsBackTheLeaseWhenTheEntityIsFenced(t *testing.T) {
	now := time.Now()
	command := Command{
		ID: "cmd-fenced", IdempotencyKey: "op-fenced", SagaID: "saga-1", SagaType: "gift", DefinitionVersion: 1,
		BusinessKey: "gift-1", StepName: "debit", Phase: PhaseForward, Attempt: 1,
		Topic: "debit", CreatedAt: now, DeadlineAt: now.Add(time.Minute),
	}
	raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: command})
	if err != nil {
		t.Fatal(err)
	}
	fenced := errors.Join(corenest.ErrCommitRejected, fmt.Errorf("gift debit: %w", coredata.ErrFencedEntityPending))
	for name, tc := range map[string]struct {
		handlerErr error
		released   bool
	}{
		"fenced entity":        {fenced, true},
		"other handler errors": {errors.New("business adapter failed"), false},
	} {
		t.Run(name, func(t *testing.T) {
			mongoClient := mongotest.NewClient()
			inbox, err := NewDataEngineStepInbox(mongoClient, "game", DataEngineStepInboxOptions{Owner: "game-1", LeaseDuration: 2 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			client := &startJetStream{}
			transport, _ := NewJetStreamPublisher(client, "roost.saga")
			var tokens []uint64
			handler := func(ctx context.Context, _ Command) (Completion, error) {
				reservation, _ := ReservationFromContext(ctx)
				tokens = append(tokens, reservation.Token)
				return Completion{}, tc.handlerErr
			}
			config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "debit", AckWait: 30 * time.Second}
			if _, err := SubscribeDataEngineStep(context.Background(), client, transport, inbox, config, handler); err != nil {
				t.Fatal(err)
			}
			if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); !errors.Is(err, tc.handlerErr) {
				t.Fatalf("delivery=%v, want the handler error", err)
			}
			claim := inboxOperation(t, mongoClient, command.IdempotencyKey)
			if released := !claim.LeaseUntil.After(time.Now()); released != tc.released || claim.LeaseToken != 1 {
				t.Fatalf("after the failed delivery: lease released=%v want %v (token=%d lease_until=%v)", released, tc.released, claim.LeaseToken, claim.LeaseUntil)
			}
			if !tc.released {
				return
			}
			if err := client.handler(context.Background(), &fnats.JetStreamMsg{Data: raw}); !errors.Is(err, tc.handlerErr) {
				t.Fatalf("redelivery=%v", err)
			}
			if len(tokens) != 2 || tokens[1] != 2 {
				t.Fatalf("redelivery after the barrier did not reserve again: handler tokens=%v", tokens)
			}
		})
	}
}
