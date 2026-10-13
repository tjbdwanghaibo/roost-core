package saga

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func dataEngineCommand(id, operation string, payload string) Command {
	now := time.Now().UTC()
	return Command{
		ID: id, IdempotencyKey: operation, SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "r-1", StepName: "reserve", Phase: PhaseForward, Attempt: 1,
		Topic: "rally.reserve", Payload: []byte(payload), CreatedAt: now, DeadlineAt: now.Add(time.Minute),
	}
}

type stepFenceCommitter struct{ record coredata.CommitRecord }

func (committer *stepFenceCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	committer.record = coredata.CloneCommitRecord(record)
	return nil
}

func TestDataEngineStepBindCarriesExplicitReservationFence(t *testing.T) {
	inbox, err := NewDataEngineStepInbox(newDataEngineInboxMongo(), "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("command-fenced", "operation-fenced", "payload")
	reservation := inbox.activeReservation(command.IdempotencyKey, command.ID, mustCommandDigest(t, command), 9)
	ctx := withReservation(context.Background(), reservation)
	extracted, ok := ReservationFromContext(ctx)
	if !ok || extracted.Token != reservation.Token {
		t.Fatalf("reservation=%+v ok=%t", extracted, ok)
	}
	committer := &stepFenceCommitter{}
	_, err = corenest.RunIsolatedTransaction(ctx, committer, "saga-fence-test", func() (any, error) {
		return nil, inbox.Bind(command, extracted)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(committer.record.Receipts) != 2 {
		t.Fatalf("receipts=%+v", committer.record.Receipts)
	}
	fence, control, err := coredata.DecodeLeaseFenceReceipt(committer.record.Receipts[0])
	if err != nil || !control || fence.Owner != "worker-1" || fence.Token != 9 || fence.Resource != dataEngineOperationCollection || fence.DocumentID != command.IdempotencyKey || !bytes.Equal(fence.Digest, mustCommandDigest(t, command)) {
		t.Fatalf("fence=%+v control=%t err=%v", fence, control, err)
	}
}

func TestDataEngineStepBindRejectsReservationFromAnotherCommand(t *testing.T) {
	inbox, err := NewDataEngineStepInbox(newDataEngineInboxMongo(), "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	reserved := dataEngineCommand("command-a", "operation-a", "payload-a")
	other := dataEngineCommand("command-b", "operation-b", "payload-b")
	reservation := inbox.activeReservation(reserved.IdempotencyKey, reserved.ID, mustCommandDigest(t, reserved), 1)
	committer := &stepFenceCommitter{}
	_, err = corenest.RunIsolatedTransaction(context.Background(), committer, "saga-fence-test", func() (any, error) {
		return nil, inbox.Bind(other, reservation)
	})
	if err == nil {
		t.Fatal("reservation from another command was accepted")
	}
	if !committer.record.Empty() {
		t.Fatalf("mismatched reservation committed record=%+v", committer.record)
	}
}

func TestDataEngineStepInboxReservesCommandIdentityAndAllowsNewAttempt(t *testing.T) {
	client := newDataEngineInboxMongo()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("command-1", "operation-1", "a")
	first, err := inbox.Reserve(context.Background(), command)
	if err != nil || first.Duplicate || first.Token != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	duplicate, err := inbox.Reserve(context.Background(), command)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	conflict := command
	conflict.Payload = []byte("different")
	if _, err := inbox.Reserve(context.Background(), conflict); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	// U-0280：同一操作实例（IdempotencyKey）的另一次尝试，在第一次尝试的租约有效时不能开始，
	// 只能等它有结论；租约过期（封顶在第一次尝试的截止时间）后接替它，第一次尝试从此不会再执行。
	newAttempt := command
	newAttempt.ID = "command-2"
	newAttempt.Attempt = 2
	newAttempt.DeadlineAt = command.DeadlineAt.Add(time.Minute)
	if reservation, err := inbox.Reserve(context.Background(), newAttempt); !errors.Is(err, errOperationAttemptInFlight) {
		t.Fatalf("new attempt during the first attempt's lease=%+v err=%v, want errOperationAttemptInFlight", reservation, err)
	}
	inbox.now = func() time.Time { return command.DeadlineAt }
	if reservation, err := inbox.Reserve(context.Background(), newAttempt); err != nil || reservation.Duplicate {
		t.Fatalf("new attempt after the first attempt's deadline=%+v err=%v", reservation, err)
	}
	// 状态文档：第二次尝试是当前尝试（token 2，第一次尝试的 fence 从此不再匹配），第一次尝试记为被它接替。
	state := inboxOperation(t, client, command.IdempotencyKey)
	if state.CommandID != newAttempt.ID || state.LeaseToken != 2 || state.Status != operationStatusPending ||
		len(state.Superseded) != 1 || state.Superseded[0].CommandID != command.ID || state.Superseded[0].By != newAttempt.ID {
		t.Fatalf("operation state after the first attempt was superseded=%+v", state)
	}
	if _, err := inbox.Reserve(context.Background(), command); !errors.Is(err, errAttemptSuperseded) && !errors.Is(err, ErrCommandExpired) {
		t.Fatalf("redelivery of the superseded first attempt err=%v, want errAttemptSuperseded or ErrCommandExpired", err)
	}
}

func TestDataEngineStepInboxReplaysAuthoritativeReceiptAndCompletesClaim(t *testing.T) {
	client := newDataEngineInboxMongo()
	inbox, _ := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	command := dataEngineCommand("command-1", "operation-1", "a")
	if _, err := inbox.Reserve(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	completion := Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true, Data: []byte("reserved"), CompletedAt: time.Now().UTC()}
	effect, _ := NewCompletionEffect(completion)
	if err := inboxReceipts(client).Seed(dataEngineReceipt{
		ID: dataEngineStepNamespace + "/" + command.ID, Digest: mustCommandDigest(t, command), Payload: effect.Payload,
	}); err != nil {
		t.Fatal(err)
	}
	replayed, found, err := inbox.Replay(context.Background(), command)
	if err != nil || !found || replayed.CommandID != command.ID || string(replayed.Data) != "reserved" {
		t.Fatalf("completion=%+v found=%v err=%v", replayed, found, err)
	}
	reservation, err := inbox.Reserve(context.Background(), command)
	if err != nil || !reservation.Duplicate || reservation.Completion.CommandID != command.ID {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
}

func TestDataEngineStepInboxUsesAbsoluteOperationExpiry(t *testing.T) {
	client := newDataEngineInboxMongo()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	operations := inboxOperations(client)
	// 每个操作一份状态文档，所有读写按 _id：只有过期清理一个索引（状态文档方案，claims 集合的四个索引已删除）。
	if len(operations.Indexes) != 1 || !operations.HasIndex("expires_at") {
		t.Fatalf("operation indexes=%+v, want only the expiry index", operations.Indexes)
	}
	expiry := operations.Indexes[0]
	if !expiry.ExpireAt || expiry.TTL != 0 || !expiry.RecreateOnConflict {
		t.Fatalf("operation expiry index=%+v", expiry)
	}
}

func newDataEngineInboxMongo() *mongotest.Client { return mongotest.NewClient() }

func inboxOperations(client *mongotest.Client) *mongotest.Collection {
	return client.Collection("game", dataEngineOperationCollection)
}

// inboxOperation 读原生收件箱里一个操作实例的状态文档。
func inboxOperation(t *testing.T, client *mongotest.Client, operationKey string) stepOperation {
	t.Helper()
	var state stepOperation
	if err := inboxOperations(client).FindOne(context.Background(), bson.M{"_id": operationKey}, &state); err != nil {
		t.Fatalf("operation %s: %v", operationKey, err)
	}
	return state
}

func inboxReceipts(client *mongotest.Client) *mongotest.Collection {
	return client.Collection("game", dataEngineReceiptCollection)
}

// 投影器在另一个包里按本包写的操作状态文档做 fence 判定，编译器不把两边连起来，而且不匹配是静默的：
// 无法满足的谓词看起来与正常过期的租约一样，每个被 fence 的事务都会被当成跳过的空操作确认。
// 这里把真实状态文档经 BSON 往返后用真实谓词求值，并验证每个单字段偏差都被拒绝；任一侧改名或改类型都会在此变红。
func TestDataEngineOperationStateSatisfiesProjectorFencePredicate(t *testing.T) {
	client := mongotest.NewClient()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{
		Owner: "worker-1", LeaseDuration: time.Minute, ReceiptTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("command-fence", "operation-fence", "payload")
	reservation, err := inbox.Reserve(context.Background(), command)
	if err != nil || reservation.Token == 0 || reservation.Duplicate {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}

	// Bind produces the fence the projector will later evaluate.
	committer := &stepFenceCommitter{}
	ctx := withReservation(context.Background(), reservation)
	extracted, _ := ReservationFromContext(ctx)
	if _, err := corenest.RunIsolatedTransaction(ctx, committer, "saga-fence-contract", func() (any, error) {
		return nil, inbox.Bind(command, extracted)
	}); err != nil {
		t.Fatal(err)
	}
	fence, control, err := coredata.DecodeLeaseFenceReceipt(committer.record.Receipts[0])
	if err != nil || !control {
		t.Fatalf("fence=%+v control=%v err=%v", fence, control, err)
	}

	operations := inboxOperations(client)
	now := time.Now().UTC()
	var found bson.M
	if err := operations.FindOne(context.Background(), fence.Predicate(now), &found); err != nil {
		stored, _ := operations.Lookup(fence.DocumentID)
		t.Fatalf("the projector's predicate does not match the operation state this package writes: %v\npredicate=%v\nstored=%v",
			err, fence.Predicate(now), stored)
	}

	// Every field of the predicate must be load-bearing: if any deviation
	// still matched, the fence would not actually be fencing anything.
	for name, mutate := range map[string]func(*coredata.LeaseFence){
		"other owner":  func(f *coredata.LeaseFence) { f.Owner = "worker-2" },
		"other token":  func(f *coredata.LeaseFence) { f.Token++ },
		"other digest": func(f *coredata.LeaseFence) { f.Digest = bytes.Repeat([]byte{9}, len(f.Digest)) },
		"other document": func(f *coredata.LeaseFence) {
			f.DocumentID = "other-operation"
		},
	} {
		deviated := fence
		mutate(&deviated)
		if err := operations.FindOne(context.Background(), deviated.Predicate(now), &found); !errors.Is(err, fmongo.ErrNotFound) {
			t.Fatalf("%s: predicate still matched (err=%v)", name, err)
		}
	}

	// An expired lease must stop matching without touching the document.
	if err := operations.FindOne(context.Background(), fence.Predicate(now.Add(2*time.Minute)), &found); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("expired lease still matched: %v", err)
	}

	// A settled attempt must stop matching: the status value is shared with
	// the projector precisely so this transition is respected.
	completion := Completion{
		CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID,
		Success: true, CompletedAt: now,
	}
	if err := inbox.markCompleted(context.Background(), command.IdempotencyKey, command.ID, completion); err != nil {
		t.Fatal(err)
	}
	if err := operations.FindOne(context.Background(), fence.Predicate(now), &found); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("settled attempt still matched the pending predicate: %v", err)
	}
}

// Reserve 在事务内读到权威回执时顺手结算状态文档（markCompleted）。返回值以回执为准、不因这次写失败而改变，
// 但失败不能静默：每次失败记一条带 command_id 与原因的 Warn，并累加 saga.step_inbox.mark_completed_error_total。
// 历史：RR-20260927-16。
func TestReserveReportsSwallowedMarkCompletedFailure(t *testing.T) {
	client := newDataEngineInboxMongo()
	injected := errors.New("injected operation state update failure")
	flaky := &operationUpdateFailOnce{Client: client, err: injected}
	inbox, err := NewDataEngineStepInbox(flaky, "game", DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("command-mark", "operation-mark", "a")
	if _, err := inbox.Reserve(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	completion := Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true, Data: []byte("done"), CompletedAt: time.Now().UTC()}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		t.Fatal(err)
	}
	if err := inboxReceipts(client).Seed(dataEngineReceipt{
		ID: dataEngineStepNamespace + "/" + command.ID, Digest: mustCommandDigest(t, command), Payload: effect.Payload,
	}); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	before := markCompletedErrors()
	flaky.armed.Store(true)

	reservation, err := inbox.Reserve(context.Background(), command)
	// 返回值不变：第一次尝试的写错误让事务中止，驱动重跑后结算成功，Reserve 照常返回 Duplicate + 回执里的完成结果。
	if err != nil || !reservation.Duplicate || reservation.Completion.CommandID != command.ID || string(reservation.Completion.Data) != "done" {
		t.Fatalf("reservation=%+v err=%v, want the duplicate completion from the receipt", reservation, err)
	}
	if flaky.failed.Load() != 1 {
		t.Fatalf("premise: injected operation state update failures=%d, want 1", flaky.failed.Load())
	}
	if got := markCompletedErrors() - before; got != 1 {
		t.Fatalf("saga.step_inbox.mark_completed_error_total grew by %d, want 1: the swallowed failure is silent", got)
	}
	text := logs.String()
	if !strings.Contains(text, "level=WARN") || !strings.Contains(text, "command_id="+command.ID) || !strings.Contains(text, injected.Error()) {
		t.Fatalf("no warning with the command id and cause for the swallowed markCompleted failure; logs=%q", text)
	}
	if state := inboxOperation(t, client, command.IdempotencyKey); state.CommandID != command.ID || state.Status != operationStatusSettled || state.Result != operationResultSuccess {
		t.Fatalf("operation state=%+v, want the attempt settled after the retried transaction", state)
	}
}

func markCompletedErrors() int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "saga.step_inbox.mark_completed_error_total" {
			return metric.Value
		}
	}
	return 0
}

// operationUpdateFailOnce 在 armed 之后让状态文档集合的第一次 UpdateOne 返回 err（经伪 Mongo 的事务语义：写错误中止事务）。
type operationUpdateFailOnce struct {
	*mongotest.Client
	err    error
	armed  atomic.Bool
	failed atomic.Int32
}

func (m *operationUpdateFailOnce) Database(name string) fmongo.IDatabase {
	return operationFailDatabase{IDatabase: m.Client.Database(name), mongo: m}
}

type operationFailDatabase struct {
	fmongo.IDatabase
	mongo *operationUpdateFailOnce
}

func (d operationFailDatabase) Collection(name string) fmongo.ICollection {
	coll := d.IDatabase.Collection(name)
	if name != dataEngineOperationCollection {
		return coll
	}
	return operationFailCollection{ICollection: coll, mongo: d.mongo}
}

type operationFailCollection struct {
	fmongo.ICollection
	mongo *operationUpdateFailOnce
}

func (c operationFailCollection) UpdateOne(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	if c.mongo.armed.CompareAndSwap(true, false) {
		c.mongo.failed.Add(1)
		// 与服务端一致：事务里的写错误中止事务。借真实集合一次注定失败的写入登记中止原因。
		raw := c.mongo.Client.Collection("game", dataEngineOperationCollection)
		raw.Errors["UpdateOne"] = c.mongo.err
		defer delete(raw.Errors, "UpdateOne")
	}
	return c.ICollection.UpdateOne(ctx, filter, update)
}

// 原生步骤的 Nest 事务在 WAL 准入处被实体屏障拒绝（ErrFencedEntityPending）时整体回滚、没有带本 token 的记录进 WAL：
// 消费者交还刚拿到的租约，屏障解除后的重投立刻重新 Reserve 并执行 handler，而不是读到“租约有效”的 Duplicate
// 一直等到租约过期；其他 handler 错误照旧保留租约。历史：RR-20260926-30。
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

// 取消等待不等于撤销已经可能进入 WAL 的业务：普通取消保留租约，晚到的权威回执出现后重投只回放不重跑；
// 只有明确在 WAL 之前的 ErrFencedEntityPending 才交还租约。这里注入持久回执，不代替真实 WAL / 投影验收。
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
