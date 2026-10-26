package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
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

func TestMongoStepAttemptsOfOneOperationTakeEffectOnce(t *testing.T) {
	runMongoStepOperationCases(t, func(*testing.T) (fmongo.IMongo, string) { return mongotest.NewClient(), "mongo_step_op" })
}

func mongoStepCommand(operation string, attempt uint32, deadline time.Time) Command {
	now := time.Now().UTC()
	return Command{
		ID: commandID(operation, 0, attempt), IdempotencyKey: operation, SagaID: "gift-1", SagaType: "gift", DefinitionVersion: 1,
		BusinessKey: "g-1", StepName: "deliver", Phase: PhaseForward, Attempt: attempt,
		Topic: "gift.deliver", Payload: []byte("state"), CreatedAt: now, DeadlineAt: deadline,
	}
}

func runMongoStepOperationCases(t *testing.T, open func(*testing.T) (fmongo.IMongo, string)) {
	t.Run("committed attempt is replayed by the next attempt", func(t *testing.T) {
		client, database := open(t)
		ctx := context.Background()
		inbox, err := NewMongoCommandInbox(client, database, "steps")
		if err != nil {
			t.Fatal(err)
		}
		if err := inbox.EnsureInfrastructure(ctx); err != nil {
			t.Fatal(err)
		}
		business := client.Database(database).Collection("business")
		executed := []string{}
		handler := func(txCtx context.Context, command Command) (Completion, error) {
			executed = append(executed, command.ID)
			if _, err := business.InsertOne(txCtx, bson.M{"_id": command.ID, "operation": command.IdempotencyKey}); err != nil {
				return Completion{}, err
			}
			return Completion{Success: true, Data: []byte("mail sent by " + command.ID)}, nil
		}
		operation := "gift-1:1:1"
		k := mongoStepCommand(operation, 1, time.Now().Add(time.Minute))
		if _, _, err := inbox.Handle(ctx, k, handler); err != nil {
			t.Fatal(err)
		}
		// 结果没送达，协调器在 k 的截止后发出 k+1（同一操作实例，新的 CommandID）。
		next := mongoStepCommand(operation, 2, time.Now().Add(2*time.Minute))
		replayed, duplicate, err := inbox.Handle(ctx, next, handler)
		if err != nil {
			t.Fatal(err)
		}
		count, err := business.CountDocuments(ctx, bson.M{"operation": operation})
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("operation %s took effect %d time(s), want 1: the business write ran for %v although attempt %s had already committed", operation, count, executed, k.ID)
		}
		if !duplicate || replayed.CommandID != k.ID || string(replayed.Data) != "mail sent by "+k.ID || !replayed.Success {
			t.Fatalf("attempt %s returned %+v duplicate=%v, want the committed result of %s replayed", next.ID, replayed, duplicate, k.ID)
		}
	})

	t.Run("in-flight attempt past its deadline is taken over and cannot commit", func(t *testing.T) {
		client, database := open(t)
		ctx := context.Background()
		inbox, err := NewMongoCommandInbox(client, database, "steps")
		if err != nil {
			t.Fatal(err)
		}
		if err := inbox.EnsureInfrastructure(ctx); err != nil {
			t.Fatal(err)
		}
		business := client.Database(database).Collection("business")
		operation := "gift-1:1:1"
		k := mongoStepCommand(operation, 1, time.Now().Add(1500*time.Millisecond))
		next := mongoStepCommand(operation, 2, time.Now().Add(time.Minute))
		written, release := make(chan struct{}), make(chan struct{})
		executions := map[string]int{}
		handler := func(txCtx context.Context, command Command) (Completion, error) {
			executions[command.ID]++
			if _, err := business.InsertOne(txCtx, bson.M{"_id": command.ID, "operation": command.IdempotencyKey}); err != nil {
				return Completion{}, err
			}
			if command.ID == k.ID && executions[k.ID] == 1 {
				// k 的业务写已做、事务未提交：卡住，直到 k+1 处理完。
				close(written)
				<-release
			}
			return Completion{Success: true, Data: []byte("by " + command.ID)}, nil
		}
		kDone := make(chan error, 1)
		go func() {
			_, _, err := inbox.Handle(ctx, k, handler)
			kDone <- err
		}()
		<-written
		// k 的租约有效：k+1 不能执行。
		ranEarly := false
		if _, _, err := inbox.Handle(ctx, next, handler); err == nil {
			ranEarly = true
			t.Errorf("attempt %s ran while attempt %s was still in flight with a live lease", next.ID, k.ID)
		}
		// 等 k 过了自己的截止（条件等待，不是任意 sleep）。
		for !time.Now().After(k.DeadlineAt) {
			time.Sleep(10 * time.Millisecond)
		}
		if !ranEarly {
			completion, duplicate, err := inbox.Handle(ctx, next, handler)
			if err != nil || duplicate || completion.CommandID != next.ID {
				close(release)
				<-kDone
				t.Fatalf("attempt %s after %s's deadline: %+v duplicate=%v err=%v, want it to take over and execute", next.ID, k.ID, completion, duplicate, err)
			}
		}
		close(release)
		kErr := <-kDone
		count, err := business.CountDocuments(ctx, bson.M{"operation": operation})
		if err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("operation %s took effect %d time(s), want 1: attempt %s committed after %s took it over (k returned %v, executions=%v)", operation, count, k.ID, next.ID, kErr, executions)
		}
		if !errors.Is(kErr, errAttemptFenced) {
			t.Fatalf("attempt %s returned %v, want errAttemptFenced: it lost its lease before it could commit", k.ID, kErr)
		}
		if _, found, err := inbox.Replay(ctx, k); err != nil || found {
			t.Fatalf("fenced attempt %s left a receipt: found=%v err=%v", k.ID, found, err)
		}
	})
}

func TestMongoCommandInboxCancelledBusinessWriteCanRetryAtomically(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	inbox, err := NewMongoCommandInbox(client, "inbox_cancel", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("cancel-command", "cancel-operation", "initial")
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
		}
		return Completion{Success: true}, nil
	}
	if _, _, err := inbox.Handle(cancelCtx, command, handler); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled handler=%v", err)
	}
	if handlerCalls != 1 {
		t.Fatal("business cancellation not reached")
	}
	if count, err := business.CountDocuments(ctx, bson.M{}); err != nil || count != 0 {
		t.Fatalf("business escaped transaction: count=%d err=%v", count, err)
	}
	if _, found, err := inbox.Replay(ctx, command); err != nil || found {
		t.Fatalf("aborted receipt visible: found=%t err=%v", found, err)
	}
	for i := 0; i < 2; i++ {
		completion, duplicate, err := inbox.Handle(ctx, command, handler)
		if err != nil || !completion.Success || duplicate != (i == 1) {
			t.Fatalf("retry/duplicate=%+v duplicate=%t err=%v", completion, duplicate, err)
		}
	}
	if handlerCalls != 2 {
		t.Fatalf("business replayed after commit: calls=%d", handlerCalls)
	}
	if count, err := business.CountDocuments(ctx, bson.M{}); err != nil || count != 1 {
		t.Fatalf("business retry count=%d err=%v", count, err)
	}
}
