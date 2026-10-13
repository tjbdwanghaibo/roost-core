package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
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

// 协调器 A 领取记录后租约过期、B 接管，A 之后才执行 Apply：A 的晚到 Apply 一律 ErrConflict，不覆盖 B 的结论、
// 不重复推进（attempt 不多加、outbox 不多出一条命令）。fence 由 MongoStore.Apply 的过滤条件给出：version 相等，
// 且 ExpectedLease 的 owner + token 相等（ClaimDue 每次领取 $inc lease_token）。覆盖三种到达顺序：B 已领取未 Apply、
// B 已 Apply、A 在超时分支上与 completion 交错。领取走完整的 MongoStore.ClaimDue；真实 Mongo 上的同组用例见
// -tags integration 的 TestRealMongoCoordinatorLeaseTakeover。
func TestCoordinatorLeaseTakeoverFencesTheLateApply(t *testing.T) {
	runCoordinatorTakeoverCases(t, func(t *testing.T) fmongo.IMongo { return mongotest.NewClient() }, "takeover")
}

func runCoordinatorTakeoverCases(t *testing.T, newClient func(*testing.T) fmongo.IMongo, database string) {
	ctx := context.Background()
	setup := func(t *testing.T, status Status) (*MongoStore, *Engine, *Engine, Record, time.Time) {
		t.Helper()
		// 每个子用例用自己的集合：ClaimDue 领取全部到期记录，共用集合时前一个子用例留下的记录会被一起领到。
		suffix := "_" + NewID()
		store, err := NewMongoStore(newClient(t), MongoStoreOptions{Database: database, SagaCollection: "_sagas" + suffix, OutboxCollection: "_saga_outbox" + suffix, CompletionCollection: "_saga_completions" + suffix, OperationCollection: "_saga_operations" + suffix})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.EnsureInfrastructure(ctx); err != nil {
			t.Fatal(err)
		}
		newEngine := func(owner string) *Engine {
			options := DefaultOptions()
			options.Owner = owner
			engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), options)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			return engine
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		id := "takeover-" + NewID()
		record := Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		a, b := newEngine("coordinator-a"), newEngine("coordinator-b")
		if status == StatusWaiting {
			if err := a.processClaimed(ctx, record, now); err != nil {
				t.Fatal(err)
			}
			current, err := store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			record = current
			now = current.NextRunAt // 第一次尝试的截止：协调器下一次领取它是为了判超时
		}
		return store, a, b, record, now
	}
	claim := func(t *testing.T, store *MongoStore, owner string, now time.Time) Record {
		t.Helper()
		records, err := store.ClaimDue(ctx, ClaimRequest{Owner: owner, Now: now, LeaseDuration: 2 * time.Second, Limit: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 {
			t.Fatalf("%s claimed %d records, want 1", owner, len(records))
		}
		return records[0]
	}
	queued := func(t *testing.T, store *MongoStore, now time.Time) []OutboxRecord {
		t.Helper()
		items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: now.Add(time.Hour), LeaseDuration: time.Minute, Limit: 16})
		if err != nil {
			t.Fatal(err)
		}
		return items
	}

	t.Run("B claimed, A applies before B", func(t *testing.T) {
		store, a, b, _, now := setup(t, StatusPending)
		byA := claim(t, store, "coordinator-a", now)
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second)) // A 的租约已过期
		if err := a.processClaimed(ctx, byA, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late dispatch after B took the lease = %v, want ErrConflict", err)
		}
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); err != nil {
			t.Fatalf("B's dispatch after A's rejected one = %v", err)
		}
		final, err := store.Get(ctx, byB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != StatusWaiting || final.Attempt != 1 || final.Version != byB.Version+1 {
			t.Fatalf("after takeover: %+v, want one dispatch (attempt 1)", final)
		}
		if items := queued(t, store, now); len(items) != 1 || items[0].Command.ID != final.CommandID {
			t.Fatalf("outbox after takeover: %+v, want exactly B's command %s", items, final.CommandID)
		}
	})

	t.Run("B already applied, A applies late", func(t *testing.T) {
		store, a, b, _, now := setup(t, StatusPending)
		byA := claim(t, store, "coordinator-a", now)
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second))
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := a.processClaimed(ctx, byA, now.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late dispatch after B applied = %v, want ErrConflict", err)
		}
		final, err := store.Get(ctx, byB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Attempt != 1 || final.Version != byB.Version+1 {
			t.Fatalf("A's late apply moved the record: %+v", final)
		}
		if items := queued(t, store, now); len(items) != 1 {
			t.Fatalf("outbox has %d commands, want 1", len(items))
		}
	})

	t.Run("A's timeout decision loses to B's and to a completion", func(t *testing.T) {
		store, a, b, waiting, now := setup(t, StatusWaiting)
		byA := claim(t, store, "coordinator-a", now) // A 领取它是为了判超时
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second))
		// 结果在 B 判超时之前到达：Complete 只按版本 fence，接收它。
		if _, err := b.Complete(ctx, Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true}); err != nil {
			t.Fatal(err)
		}
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("B's timeout after the completion = %v, want ErrConflict", err)
		}
		if err := a.processClaimed(ctx, byA, now.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late timeout = %v, want ErrConflict", err)
		}
		final, err := store.Get(ctx, waiting.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != StatusPending || final.Step != 1 || final.CompletedSteps != 1 || final.Attempt != 0 {
			t.Fatalf("the accepted completion was overwritten by a stale timeout: %+v", final)
		}
	})
}

// 发布器与协调器在同一操作的 outbox 上交错：
//   - 发布器领取了第 1 次尝试的命令、正在发布，协调器判超时后派发第 2 次尝试（Apply 带 Outbox 时按 IdempotencyKey
//     删掉旧命令再插入新命令）：发布器之后的 Ack / Nack 都是 ErrConflict，不能把被替换的命令写回去；outbox 只剩第 2 次。
//   - 发布成功但 Ack 结果未知（Ack 没有执行）：租约过期后另一个发布器重新领取到同一条命令，CommandID 不变——它就是
//     JetStream 的 MsgID，去重窗口内的重发被 broker 去重，窗口外的重发由步骤收件箱按 CommandID 去重。
func TestOutboxSupersedeAndUnknownAckOnMongoStore(t *testing.T) {
	ctx := context.Background()
	engine, store, record := waitingOnMongo(t)
	now := record.CreatedAt
	// waitingOnMongo 直接写入 Waiting 记录，没有 outbox；先按正常路径派发出第 1 次尝试的命令。
	pending := record.Clone()
	pending.Version++
	pending.Status, pending.Attempt, pending.OperationKey, pending.CommandID = StatusPending, 0, "", ""
	if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: pending}); err != nil {
		t.Fatal(err)
	}
	if err := engine.processClaimed(ctx, pending, now); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "publisher-a", Now: now, LeaseDuration: 2 * time.Second, Limit: 4})
	if err != nil || len(claimed) != 1 || claimed[0].Command.ID != first.CommandID {
		t.Fatalf("publisher-a claim = %+v, %v; want the first attempt %s", claimed, err, first.CommandID)
	}

	t.Run("unknown ack: the same CommandID is published again after the lease", func(t *testing.T) {
		again, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "publisher-b", Now: now.Add(3 * time.Second), LeaseDuration: 2 * time.Second, Limit: 4})
		if err != nil || len(again) != 1 || again[0].Command.ID != first.CommandID {
			t.Fatalf("re-claim after publisher-a's lease = %+v, %v; want the same command %s", again, err, first.CommandID)
		}
		if err := store.AckOutbox(ctx, first.CommandID, claimed[0].Lease); !errors.Is(err, ErrConflict) {
			t.Fatalf("publisher-a's late ack after publisher-b took the lease = %v, want ErrConflict", err)
		}
		claimed = again
	})

	t.Run("supersede while a publisher holds the old command", func(t *testing.T) {
		// 第 1 次尝试超时：退避后派发第 2 次。
		if err := engine.processClaimed(ctx, first, first.NextRunAt); err != nil {
			t.Fatal(err)
		}
		backoff, err := store.Get(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.processClaimed(ctx, backoff, backoff.NextRunAt); err != nil {
			t.Fatal(err)
		}
		second, err := store.Get(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if second.Attempt != 2 {
			t.Fatalf("want the second attempt dispatched: %+v", second)
		}
		holder := claimed[0]
		if err := store.NackOutbox(ctx, holder.Command.ID, holder.Lease, now.Add(time.Minute), "publish failed"); !errors.Is(err, ErrConflict) {
			t.Fatalf("nack of a superseded command = %v, want ErrConflict", err)
		}
		if err := store.AckOutbox(ctx, holder.Command.ID, holder.Lease); !errors.Is(err, ErrConflict) {
			t.Fatalf("ack of a superseded command = %v, want ErrConflict", err)
		}
		items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: now.Add(time.Hour), LeaseDuration: time.Minute, Limit: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Command.ID != second.CommandID {
			t.Fatalf("outbox after supersede = %+v, want only %s", items, second.CommandID)
		}
	})
}

// 候选扫描不授予发布权：另一个发布者 Nack 延后消息之后，原扫描者必须在原子领取处复查 NextAttemptAt，不能绕过退避。
// 正式 MongoStore、后端 mongotest；钩子在 Find 返回后同步执行另一个 Store，确定交错而不靠 sleep。
type outboxScanClient struct {
	fmongo.IMongo
	afterScan func()
}

func (c *outboxScanClient) Database(name string) fmongo.IDatabase {
	return outboxScanDatabase{IDatabase: c.IMongo.Database(name), client: c}
}

type outboxScanDatabase struct {
	fmongo.IDatabase
	client *outboxScanClient
}

func (d outboxScanDatabase) Collection(name string) fmongo.ICollection {
	return outboxScanCollection{ICollection: d.IDatabase.Collection(name), client: d.client}
}

type outboxScanCollection struct {
	fmongo.ICollection
	client *outboxScanClient
}

func (c outboxScanCollection) Find(ctx context.Context, filter, result any, options ...fmongo.FindOption) error {
	err := c.ICollection.Find(ctx, filter, result, options...)
	if _, ok := result.(*[]outboxDoc); ok && err == nil && c.client.afterScan != nil {
		hook := c.client.afterScan
		c.client.afterScan = nil
		hook()
	}
	return err
}

func TestOutboxClaimRechecksDueAfterAnotherPublisher(t *testing.T) {
	for _, mode := range []string{"future_nack", "still_leased", "due_nack"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			backend := mongotest.NewClient()
			client := &outboxScanClient{IMongo: backend}
			opts := MongoStoreOptions{Database: "outbox_due_review"}
			stale, err := NewMongoStore(client, opts)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := NewMongoStore(backend, opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := fresh.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(fresh, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			r, err := e.StartSaga(ctx, StartRequest{ID: "outbox-due", Type: "rally", DefinitionVersion: 1, BusinessKey: "outbox-due", Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.processClaimed(ctx, r, now); err != nil {
				t.Fatal(err)
			}
			query := ClaimRequest{Owner: "stale", Now: now, LeaseDuration: time.Minute, Limit: 1}
			var claimed OutboxRecord
			fired := false
			client.afterScan = func() {
				fired = true
				other := query
				other.Owner = "fresh"
				items, err := fresh.ClaimOutbox(ctx, other)
				if err != nil || len(items) != 1 {
					t.Fatalf("fresh claim: %v %v", items, err)
				}
				claimed = items[0]
				if mode != "still_leased" {
					next := now
					if mode == "future_nack" {
						next = now.Add(time.Hour)
					}
					if err := fresh.NackOutbox(ctx, claimed.Command.ID, claimed.Lease, next, "publish failed"); err != nil {
						t.Fatal(err)
					}
				}
			}
			items, err := stale.ClaimOutbox(ctx, query)
			if !fired {
				t.Fatal("scan hook did not execute")
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "due_nack" {
				if len(items) != 1 {
					t.Fatalf("due retry was refused: %v", items)
				}
				return
			}
			if len(items) != 0 {
				t.Fatalf("stale scan claimed unavailable retry (%s): next=%v now=%v", mode, items[0].NextAttemptAt, now)
			}
			if mode == "still_leased" {
				query.Now = now.Add(time.Minute)
			} else {
				query.Now = now.Add(time.Hour)
			}
			query.Owner = "recovery"
			items, err = fresh.ClaimOutbox(ctx, query)
			if err != nil || len(items) != 1 || items[0].Lease.Token <= claimed.Lease.Token {
				t.Fatalf("due recovery: %v %v", items, err)
			}
			if err := fresh.AckOutbox(ctx, claimed.Command.ID, claimed.Lease); !errors.Is(err, ErrConflict) {
				t.Fatalf("old ack passed fence: %v", err)
			}
			if err := fresh.NackOutbox(ctx, claimed.Command.ID, claimed.Lease, query.Now, "old"); !errors.Is(err, ErrConflict) {
				t.Fatalf("old nack passed fence: %v", err)
			}
			if err := fresh.AckOutbox(ctx, items[0].Command.ID, items[0].Lease); err != nil {
				t.Fatal(err)
			}
			if got, err := fresh.ClaimOutbox(ctx, query); err != nil || len(got) != 0 {
				t.Fatalf("outbox did not drain: %v %v", got, err)
			}
		})
	}
}

// 在事务写到指定阶段后确定性取消：检查没有半写，并用新 ctx 重试到最终收敛。
// 替身事务模型只能证明框架使用同一个 txCtx，不能证明真实 Mongo 的未知提交 / HA。
type sagaCancelWriteClient struct {
	fmongo.IMongo
	collection string
	method     string
	cancel     context.CancelFunc
	fired      bool
}

func (c *sagaCancelWriteClient) Database(name string) fmongo.IDatabase {
	return sagaCancelWriteDatabase{IDatabase: c.IMongo.Database(name), client: c}
}

type sagaCancelWriteDatabase struct {
	fmongo.IDatabase
	client *sagaCancelWriteClient
}

func (d sagaCancelWriteDatabase) Collection(name string) fmongo.ICollection {
	return sagaCancelWriteCollection{ICollection: d.IDatabase.Collection(name), name: name, client: d.client}
}

type sagaCancelWriteCollection struct {
	fmongo.ICollection
	name   string
	client *sagaCancelWriteClient
}

func (c sagaCancelWriteCollection) afterWrite(method string, err error) {
	if err == nil && c.client.cancel != nil && c.name == c.client.collection && method == c.client.method {
		c.client.fired = true
		c.client.cancel()
		c.client.cancel = nil
	}
}
func (c sagaCancelWriteCollection) InsertOne(ctx context.Context, doc any) (string, error) {
	id, err := c.ICollection.InsertOne(ctx, doc)
	c.afterWrite("InsertOne", err)
	return id, err
}
func (c sagaCancelWriteCollection) ReplaceOne(ctx context.Context, filter, replacement any) (*fmongo.UpdateResult, error) {
	result, err := c.ICollection.ReplaceOne(ctx, filter, replacement)
	c.afterWrite("ReplaceOne", err)
	return result, err
}

func TestSagaCompletionTransactionCancellationAndRetry(t *testing.T) {
	for _, stage := range []string{"state", "receipt", "operation", "callback_retry"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			backend := mongotest.NewClient()
			client := &sagaCancelWriteClient{IMongo: backend}
			store, err := NewMongoStore(client, MongoStoreOptions{Database: "cancel_review"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			r, err := e.StartSaga(ctx, StartRequest{ID: "cancel-review", Type: "rally", BusinessKey: "cancel-review", DefinitionVersion: 1, Data: []byte("initial")})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.processClaimed(ctx, r, r.CreatedAt); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			completion := Completion{SagaID: r.ID, CommandID: before.CommandID, IdempotencyKey: before.OperationKey, Success: true, Data: []byte("step-result")}
			if stage == "callback_retry" {
				backend.TransientRetries = 1
			} else {
				cancelCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				client.cancel = cancel
				switch stage {
				case "state":
					client.collection, client.method = defaultSagaCollection, "ReplaceOne"
				case "receipt":
					client.collection, client.method = defaultCompletionCollection, "InsertOne"
				case "operation":
					client.collection, client.method = defaultOperationCollection, "InsertOne"
				}
				if _, err := e.Complete(cancelCtx, completion); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled completion=%v", err)
				}
				if !client.fired {
					t.Fatal("cancellation stage never executed")
				}
				after, err := store.Get(ctx, r.ID)
				if err != nil || after.Version != before.Version || after.Status != StatusWaiting || string(after.Data) != "initial" || after.StartDigest != r.StartDigest {
					t.Fatalf("partial saga write=%+v err=%v", after, err)
				}
				if recorded, err := store.CompletionRecorded(ctx, completion); err != nil || recorded {
					t.Fatalf("partial receipt: recorded=%t err=%v", recorded, err)
				}
				if count, err := store.outbox().CountDocuments(ctx, bson.M{}); err != nil || count != 1 {
					t.Fatalf("outbox prematurely removed: count=%d err=%v", count, err)
				}
			}
			attempts := backend.Attempts()
			for i := 0; i < 2; i++ {
				after, err := e.Complete(ctx, completion)
				if err != nil || after.Version != before.Version+1 || after.Status != StatusPending || string(after.Data) != "step-result" || after.StartDigest != r.StartDigest {
					t.Fatalf("retry/duplicate=%+v err=%v", after, err)
				}
			}
			if stage == "callback_retry" && backend.Attempts()-attempts < 2 {
				t.Fatal("callback retry did not run")
			}
			if count, err := store.outbox().CountDocuments(ctx, bson.M{}); err != nil || count != 0 {
				t.Fatalf("outbox not settled: count=%d err=%v", count, err)
			}
		})
	}
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
