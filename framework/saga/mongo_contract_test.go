package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMongoResumePersistsGenerationAndAcceptsFreshCompletion(t *testing.T) {
	for _, phase := range []Phase{PhaseForward, PhaseCompensate} {
		t.Run(phase.String(), func(t *testing.T) {
			ctx := context.Background()
			client := mongotest.NewClient()
			store, err := NewMongoStore(client, MongoStoreOptions{Database: "resume"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			newEngine := func() *Engine {
				e, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
				if err != nil {
					t.Fatal(err)
				}
				if err := e.Register(testDefinition()); err != nil {
					t.Fatal(err)
				}
				return e
			}
			e := newEngine()
			now := time.Now().UTC().Truncate(time.Millisecond)
			r := Record{ID: "resume", Type: "rally", DefinitionVersion: 1, BusinessKey: "resume", Status: StatusPending, Phase: phase, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if phase == PhaseCompensate {
				r.Status = StatusCompensating
				r.CompletedSteps = 1
			}
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			var oldIDs []string
			for generation := uint32(0); generation < 3; generation++ {
				// 新协调器/新 Store 从持久来源恢复；不使用 Resume 返回的内存对象派发。
				store, err = NewMongoStore(client, MongoStoreOptions{Database: "resume"})
				if err != nil {
					t.Fatal(err)
				}
				e = newEngine()
				before, err := store.Get(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if before.Incarnation != generation {
					t.Errorf("persisted generation=%d, want %d", before.Incarnation, generation)
				}
				if err := e.processClaimed(ctx, before, now); err != nil {
					t.Fatal(err)
				}
				waiting, err := store.Get(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, old := range oldIDs {
					if waiting.CommandID == old {
						t.Errorf("redispatch reused old command ID %q at generation %d", old, generation)
					}
				}
				oldIDs = append(oldIDs, waiting.CommandID)
				items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "review", Now: now, LeaseDuration: time.Minute, Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				if len(items) != 1 || items[0].Command.ID != waiting.CommandID {
					t.Fatalf("outbox did not carry current command: %+v", items)
				}
				result := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: r.ID}
				if generation == 2 {
					result.Success = true
				} else {
					result.Error = "repair needed"
				}
				after, err := e.Complete(ctx, result)
				if err != nil {
					t.Fatalf("generation %d fresh completion failed: %v", generation, err)
				}
				if generation == 2 {
					want := StatusPending
					if phase == PhaseCompensate {
						want = StatusCompensated
					}
					if after.Status != want {
						t.Fatalf("completion did not advance: %+v", after)
					}
					if _, err := e.Complete(ctx, result); err != nil {
						t.Fatalf("same receipt redelivery: %v", err)
					}
					break
				}
				want := StatusFailed
				if phase == PhaseCompensate {
					want = StatusManualRequired
				}
				if after.Status != want {
					t.Fatalf("failure did not enter repair state: %+v", after)
				}
				resumed, err := e.Resume(ctx, ResumeRequest{ID: r.ID, Now: now})
				if err != nil {
					t.Fatal(err)
				}
				if resumed.Incarnation != generation+1 {
					t.Errorf("Resume generation=%d, want %d", resumed.Incarnation, generation+1)
				}
			}
		})
	}
}

func TestMongoIncarnationSurvivesEveryRecordReadAndReplace(t *testing.T) {
	for _, generation := range []uint32{0, 1, 17, ^uint32(0)} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			ctx := context.Background()
			store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "roundtrip"})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			r := Record{ID: "compat", Type: "rally", DefinitionVersion: 1, BusinessKey: "compat", Status: StatusPending, Phase: PhaseForward, Incarnation: generation, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			var persisted bson.M
			if err := store.sagas().FindOne(ctx, bson.M{"_id": r.ID}, &persisted); err != nil {
				t.Fatal(err)
			}
			if generation == 0 {
				if _, ok := persisted["incarnation"]; ok {
					t.Fatal("legacy generation zero should keep historical BSON shape")
				}
			}
			for _, read := range []func() (Record, error){func() (Record, error) { return store.Get(ctx, r.ID) }, func() (Record, error) { return store.GetByBusinessKey(ctx, r.Type, r.BusinessKey) }} {
				got, err := read()
				if err != nil {
					t.Fatal(err)
				}
				if got.Incarnation != generation {
					t.Errorf("record read lost generation: got %d want %d", got.Incarnation, generation)
				}
			}
			rows, err := store.List(ctx, Query{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Incarnation != generation {
				t.Fatalf("List lost generation: %+v", rows)
			}
			r.Version++
			if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: r}); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Incarnation != generation || got.Version != 2 {
				t.Fatalf("replacement lost generation: %+v", got)
			}
		})
	}
}

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

func TestMongoStoreSkipsACorruptRecordWithoutFailingTheBatch(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	store, err := NewMongoStore(client, MongoStoreOptions{Database: "corrupt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := func(id string, offset time.Duration) Record {
		return Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "key-" + id, Status: StatusPending, Phase: PhaseForward, Version: 1,
			NextRunAt: now.Add(offset), CreatedAt: now, UpdatedAt: now.Add(offset)}
	}
	for _, r := range []Record{record("good-a", -2*time.Second), record("good-c", -time.Second)} {
		if err := store.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// 坏记录排在最前（next_run_at 最早）：业务键被不兼容的写者丢掉。
	corrupt := toRecordDoc(record("corrupt-b", -3*time.Second))
	corrupt.BusinessKey = ""
	if err := client.Collection("corrupt", defaultSagaCollection).Seed(corrupt); err != nil {
		t.Fatal(err)
	}
	ids := func(records []Record) []string {
		out := make([]string, 0, len(records))
		for _, r := range records {
			out = append(out, r.ID)
		}
		sort.Strings(out)
		return out
	}

	before := counterValue("saga.store.corrupt_record_total")
	claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: "coordinator", Now: now, LeaseDuration: 15 * time.Second, Limit: 10})
	if err != nil {
		t.Errorf("ClaimDue with one corrupt record among three = (%v, %v); want the two good records and no error — the coordinator discards a batch that comes back with an error, so the good records wait out their lease", ids(claimed), err)
	}
	if got := ids(claimed); len(got) != 2 || got[0] != "good-a" || got[1] != "good-c" {
		t.Errorf("ClaimDue returned %v, want [good-a good-c]", got)
	}
	if grown := counterValue("saga.store.corrupt_record_total") - before; grown != 1 {
		t.Errorf("ClaimDue: saga.store.corrupt_record_total grew by %d, want 1", grown)
	}

	before = counterValue("saga.store.corrupt_record_total")
	listed, err := store.List(ctx, Query{Limit: 100})
	if err != nil {
		t.Errorf("List with one corrupt record = (%v, %v); want the good records and no error", ids(listed), err)
	}
	if got := ids(listed); len(got) != 2 || got[0] != "good-a" || got[1] != "good-c" {
		t.Errorf("List returned %v, want [good-a good-c]", got)
	}
	if grown := counterValue("saga.store.corrupt_record_total") - before; grown != 1 {
		t.Errorf("List: saga.store.corrupt_record_total grew by %d, want 1", grown)
	}
	// 坏记录原样保留，不被改写。
	var raw recordDoc
	if err := client.Collection("corrupt", defaultSagaCollection).FindOne(ctx, map[string]any{"_id": "corrupt-b"}, &raw); err != nil || raw.BusinessKey != "" || raw.Status != StatusPending {
		t.Fatalf("the corrupt record was changed: %+v err=%v", raw, err)
	}
}
