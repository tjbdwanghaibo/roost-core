package saga

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoCommandInboxExecutesStepOnceForMessageRedelivery(t *testing.T) {
	client := newInboxMongoFake()
	inbox, err := NewMongoCommandInbox(client, "game", "")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := func(context.Context, Command) (Completion, error) {
		calls.Add(1)
		return Completion{Success: true, Data: []byte("reserved")}, nil
	}
	now := time.Now()
	command := Command{ID: "delivery-1", IdempotencyKey: "saga:forward:0", SagaID: "saga", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "reserve", Payload: []byte("input"), CreatedAt: now, DeadlineAt: now.Add(time.Second)}
	first, duplicate, err := inbox.Handle(context.Background(), command, handler)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate || first.CommandID != "delivery-1" {
		t.Fatalf("unexpected first result: %+v duplicate=%v", first, duplicate)
	}
	second, duplicate, err := inbox.Handle(context.Background(), command, handler)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate || second.CommandID != "delivery-1" || string(second.Data) != "reserved" {
		t.Fatalf("unexpected duplicate result: %+v duplicate=%v", second, duplicate)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls=%d", calls.Load())
	}
}

func TestMongoCommandInboxAllowsNewSagaAttempt(t *testing.T) {
	client := newInboxMongoFake()
	inbox, _ := NewMongoCommandInbox(client, "game", "")
	var calls atomic.Int32
	handler := func(context.Context, Command) (Completion, error) {
		attempt := calls.Add(1)
		if attempt == 1 {
			return Completion{Retryable: true, Error: "busy"}, nil
		}
		return Completion{Success: true}, nil
	}
	now := time.Now()
	command := Command{ID: "attempt-1", IdempotencyKey: "stable-op", SagaID: "saga", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-2", Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "reserve", CreatedAt: now, DeadlineAt: now.Add(time.Second)}
	first, _, err := inbox.Handle(context.Background(), command, handler)
	if err != nil || !first.Retryable {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	command.ID = "attempt-2"
	command.Attempt = 2
	second, duplicate, err := inbox.Handle(context.Background(), command, handler)
	if err != nil || duplicate || !second.Success {
		t.Fatalf("second=%+v duplicate=%v err=%v", second, duplicate, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler calls=%d", calls.Load())
	}
}

func TestMongoCommandInboxRejectsCommandIDReuse(t *testing.T) {
	client := newInboxMongoFake()
	inbox, _ := NewMongoCommandInbox(client, "game", "")
	handler := func(context.Context, Command) (Completion, error) {
		return Completion{Success: true}, nil
	}
	now := time.Now()
	command := Command{ID: "one", IdempotencyKey: "op", SagaID: "saga", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-3", Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "reserve", Payload: []byte("a"), CreatedAt: now, DeadlineAt: now.Add(time.Second)}
	if _, _, err := inbox.Handle(context.Background(), command, handler); err != nil {
		t.Fatal(err)
	}
	command.Payload = []byte("different")
	if _, _, err := inbox.Handle(context.Background(), command, handler); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestStorageDigestsUseStableOperationIdentity(t *testing.T) {
	one := Completion{CommandID: "delivery-1", IdempotencyKey: "op", SagaID: "saga", Success: true, Data: []byte("x"), CompletedAt: time.Now()}
	two := one
	two.CompletedAt = two.CompletedAt.Add(time.Hour)
	if string(mustCompletionDigest(t, one)) != string(mustCompletionDigest(t, two)) {
		t.Fatal("completion timestamp changed identity")
	}
	two.CommandID = "delivery-2"
	if string(mustCompletionDigest(t, one)) == string(mustCompletionDigest(t, two)) {
		t.Fatal("different command IDs shared a receipt identity")
	}
	two = one
	two.Data = []byte("y")
	if string(mustCompletionDigest(t, one)) == string(mustCompletionDigest(t, two)) {
		t.Fatal("business result did not change identity")
	}
}

func newInboxMongoFake() *mongotest.Client { return mongotest.NewClient() }

func mustCompletionDigest(t *testing.T, c Completion) []byte {
	t.Helper()
	digest, err := completionDigest(c)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func mustCommandDigest(t *testing.T, c Command) []byte {
	t.Helper()
	digest, err := commandDigest(c)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// 操作状态文档（每个操作一份）的两段有界历史：
//
//   - 拒绝只保留代际最大的两生。同一生的拒绝照常回放；被剪掉那一生的投递不能证明“这一生没被拒绝过”，
//     不执行（errAttemptSuperseded），而不是冒险执行；没被剪掉的老一生照常回放它自己的拒绝。
//   - 被接替的尝试记最近的 maxRememberedSuperseded 条：截止之前它的投递不执行，截止过后它的投递在截止检查处
//     就不执行；一连串接替只留最新的那些（剪掉的早已过了截止）。
func TestOperationStateKeepsTheRefusalsOfTheTwoNewestLives(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	inbox, err := NewMongoCommandInbox(client, "state", "steps")
	if err != nil {
		t.Fatal(err)
	}
	operation := "gift-1:1:0"
	command := func(incarnation, attempt uint32) Command {
		c := mongoStepCommand(operation, attempt, time.Now().Add(time.Hour))
		c.ID = commandID(operation, incarnation, attempt)
		return c
	}
	ran := 0
	refuse := func(context.Context, Command) (Completion, error) { ran++; return Completion{Error: "refused"}, nil }
	retryable := func(context.Context, Command) (Completion, error) {
		ran++
		return Completion{Retryable: true, Error: "dependency down"}, nil
	}
	for life := uint32(0); life < 3; life++ {
		if completion, duplicate, err := inbox.Handle(ctx, command(life, 1), refuse); err != nil || duplicate || completion.Success || completion.Retryable {
			t.Fatalf("life %d attempt 1: %+v duplicate=%v err=%v, want it to run and be refused", life, completion, duplicate, err)
		}
		// 同一生的下一次尝试回放这一生的拒绝。
		if completion, duplicate, err := inbox.Handle(ctx, command(life, 2), retryable); err != nil || !duplicate || completion.CommandID != command(life, 1).ID {
			t.Fatalf("life %d attempt 2: %+v duplicate=%v err=%v, want the refusal of this life replayed", life, completion, duplicate, err)
		}
	}
	// 第 3 生执行（可重试失败）；授予它租约时只留第 1、2 生的拒绝，第 0 生被剪掉。
	if _, duplicate, err := inbox.Handle(ctx, command(3, 1), retryable); err != nil || duplicate {
		t.Fatalf("life 3 attempt 1: duplicate=%v err=%v, want it to run: earlier lives' refusals do not apply", duplicate, err)
	}
	var state stepOperation
	if err := client.Database("state").Collection("steps"+mongoInboxOperationSuffix).FindOne(ctx, bson.M{"_id": operation}, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Refusals) != 2 || state.Refusals[refusalKey(1)].CommandID != command(1, 1).ID || state.Refusals[refusalKey(2)].CommandID != command(2, 1).ID ||
		state.RefusalsDroppedThrough == nil || *state.RefusalsDroppedThrough != 0 {
		t.Fatalf("refusals=%+v dropped_through=%v, want the refusals of lives 1 and 2 and life 0 recorded as dropped", state.Refusals, state.RefusalsDroppedThrough)
	}
	before := ran
	if _, _, err := inbox.Handle(ctx, command(0, 3), retryable); !errors.Is(err, errAttemptSuperseded) {
		t.Fatalf("a late attempt of life 0 (refusal dropped) err=%v, want errAttemptSuperseded: it cannot prove life 0 was never refused", err)
	}
	if completion, duplicate, err := inbox.Handle(ctx, command(1, 3), retryable); err != nil || !duplicate || completion.CommandID != command(1, 1).ID {
		t.Fatalf("a late attempt of life 1: %+v duplicate=%v err=%v, want the refusal of life 1 replayed", completion, duplicate, err)
	}
	if ran != before {
		t.Fatalf("late attempts of earlier lives ran the handler %d time(s), want 0", ran-before)
	}
	// 第 3 生的下一次尝试照常执行（第 3 生只有可重试失败）。
	if _, duplicate, err := inbox.Handle(ctx, command(3, 2), retryable); err != nil || duplicate || ran != before+1 {
		t.Fatalf("life 3 attempt 2: duplicate=%v err=%v ran=%d, want it to run", duplicate, err, ran-before)
	}
}

func TestOperationStateRemembersTheLatestSupersededAttempts(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Millisecond)
	now := start
	inbox.now = func() time.Time { return now }
	operation := "gift-s:1:0"
	attempt := func(n uint32, deadline time.Time) Command {
		c := dataEngineCommand(commandID(operation, 0, n), operation, "x")
		c.Attempt, c.DeadlineAt = n, deadline
		return c
	}
	// k 的截止在 10 分钟后，租约只有 1 分钟（LeaseDuration 短于步骤 Timeout）：租约过期后 k+1 接替时 k 的截止还没到。
	k := attempt(1, start.Add(10*time.Minute))
	if _, err := inbox.Reserve(ctx, k); err != nil {
		t.Fatal(err)
	}
	now = start.Add(2 * time.Minute)
	k1 := attempt(2, start.Add(5*time.Minute))
	if reservation, err := inbox.Reserve(ctx, k1); err != nil || reservation.Duplicate {
		t.Fatalf("k+1 reserve=%+v err=%v, want it to take over k", reservation, err)
	}
	if _, err := inbox.Reserve(ctx, k); !errors.Is(err, errAttemptSuperseded) {
		t.Fatalf("k delivered before its deadline after being superseded: err=%v, want errAttemptSuperseded", err)
	}
	// k 截止过后，它的投递在截止检查处就不执行。
	now = start.Add(11 * time.Minute)
	if _, err := inbox.Reserve(ctx, k); !errors.Is(err, ErrCommandExpired) {
		t.Fatalf("k delivered after its deadline: err=%v, want ErrCommandExpired", err)
	}
	// 记录有上限：一连串截止未到的接替只留最新的 maxRememberedSuperseded 条，最新的一条一定还在。
	for n := uint32(4); n < 4+maxRememberedSuperseded+4; n++ {
		now = now.Add(2 * time.Minute)
		if reservation, err := inbox.Reserve(ctx, attempt(n, now.Add(time.Hour))); err != nil || reservation.Duplicate {
			t.Fatalf("attempt %d reserve=%+v err=%v, want it to take over", n, reservation, err)
		}
	}
	state := inboxOperation(t, client, operation)
	last := attempt(4+maxRememberedSuperseded+2, time.Time{}).ID
	if len(state.Superseded) != maxRememberedSuperseded || !state.supersededAttempt(last) || state.supersededAttempt(k.ID) {
		t.Fatalf("%d superseded attempts remembered (newest %s present=%v, oldest %s present=%v), want the newest %d",
			len(state.Superseded), last, state.supersededAttempt(last), k.ID, state.supersededAttempt(k.ID), maxRememberedSuperseded)
	}
}

// 同一操作实例（saga + 方向 + 步骤）的尝试跨 Resume 累积，不能让新一生的尝试执行不了：判定只读这一份状态文档，
// 尝试次数、Resume 次数都不进入判定也没有上限。断言任意多次尝试与 Resume 之后仍能执行、之后回放，而且整个操作
// 始终只有一份文档。
//
// 夹具：每一生先走真实 Handle：可重试失败、handler 出错（交还租约）后被下一次接管、可重试失败，再补满这一生的
// 可重试失败，五生共 4100 次尝试（超过旧实现按 claim 计数的 4096 上限），都在保留期内。历史：RR-20261006-15。
func TestOperationAttemptsAccumulatedOverResumesDoNotBlockANewLife(t *testing.T) {
	runAccumulatedAttemptsCase(t, mongotest.NewClient(), "attempt_cap", 5, 820)
}

// runAccumulatedAttemptsCase 同时跑在 mongotest 与真实 Mongo 副本集上（mongo_step_operation_real_mongo_integration_test.go）。
func runAccumulatedAttemptsCase(t *testing.T, client fmongo.IMongo, database string, lives, attemptsPerLife uint32) {
	ctx := context.Background()
	inbox, err := NewMongoCommandInbox(client, database, "steps")
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	operations := client.Database(database).Collection("steps" + mongoInboxOperationSuffix)
	business := client.Database(database).Collection("business")
	operation := "gift-1:1:0"
	command := func(incarnation, attempt uint32) Command {
		c := mongoStepCommand(operation, attempt, time.Now().Add(time.Hour))
		c.ID = commandID(operation, incarnation, attempt)
		return c
	}
	retryable := func(context.Context, Command) (Completion, error) {
		return Completion{Success: false, Retryable: true, Error: "dependency down"}, nil
	}
	crashed := func(context.Context, Command) (Completion, error) { return Completion{}, errors.New("handler crashed") }

	for incarnation := uint32(0); incarnation < lives; incarnation++ {
		if completion, duplicate, err := inbox.Handle(ctx, command(incarnation, 1), retryable); err != nil || duplicate || completion.Success {
			t.Fatalf("life %d attempt 1: %+v duplicate=%v err=%v, want its own retryable failure", incarnation, completion, duplicate, err)
		}
		if _, _, err := inbox.Handle(ctx, command(incarnation, 2), crashed); err == nil {
			t.Fatalf("life %d attempt 2: want the handler error", incarnation)
		}
		for attempt := uint32(3); attempt <= attemptsPerLife; attempt++ {
			if completion, duplicate, err := inbox.Handle(ctx, command(incarnation, attempt), retryable); err != nil || duplicate || completion.Success {
				t.Fatalf("life %d attempt %d: %+v duplicate=%v err=%v, want it to run and fail retryably", incarnation, attempt, completion, duplicate, err)
			}
		}
	}

	// 原因修好、再次 Resume：新一生的第一次尝试必须执行。
	next := command(lives, 1)
	completion, duplicate, err := inbox.Handle(ctx, next, func(txCtx context.Context, c Command) (Completion, error) {
		if _, err := business.InsertOne(txCtx, bson.M{"_id": c.ID, "operation": c.IdempotencyKey}); err != nil {
			return Completion{}, err
		}
		return Completion{Success: true, Data: []byte("delivered")}, nil
	})
	if err != nil || duplicate || !completion.Success || completion.CommandID != next.ID {
		t.Fatalf("after %d attempts over %d lives, the first attempt of the next life %s: %+v duplicate=%v err=%v, want it to execute: "+
			"earlier lives' failures must not make the step unexecutable", lives*attemptsPerLife, lives, next.ID, completion, duplicate, err)
	}
	// 生效之后，再来的尝试回放这次成功；只读的 operationSuccess（不执行的投递 ack 前用）也要看到它。
	late := command(lives, 2)
	replayed, duplicate, err := inbox.Handle(ctx, late, retryable)
	if err != nil || !duplicate || replayed.CommandID != next.ID || !replayed.Success {
		t.Fatalf("attempt %s after %s took effect: %+v duplicate=%v err=%v, want the success replayed", late.ID, next.ID, replayed, duplicate, err)
	}
	success, found, err := inbox.operationSuccess(ctx, command(lives, 3))
	if err != nil || !found || success.CommandID != next.ID {
		t.Fatalf("operationSuccess for %s: %+v found=%v err=%v, want the success of %s", operation, success, found, err, next.ID)
	}
	if n, err := business.CountDocuments(ctx, bson.M{"operation": operation}); err != nil || n != 1 {
		t.Fatalf("operation %s took effect %d times (err=%v), want 1", operation, n, err)
	}
	// 判定读写的始终是同一份文档：尝试与 Resume 不留下按尝试的文档。
	if n, err := operations.CountDocuments(ctx, bson.M{}); err != nil || n != 1 {
		t.Fatalf("operation state collection has %d documents (err=%v) after %d attempts, want exactly 1", n, err, lives*attemptsPerLife+3)
	}
}
