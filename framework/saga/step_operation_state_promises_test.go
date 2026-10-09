package saga

// 操作状态文档（docs/feature/SAGA-OPERATION-STATE-DOC-2026-10-06.md）的两段有界历史：
//
//   - 拒绝只保留代际最大的两生（方案 4.5）。同一生的拒绝照常回放；被剪掉的那一生的投递不能证明“这一生没被拒绝过”，
//     不执行（errAttemptSuperseded），而不是冒险执行；没被剪掉的老一生照常回放它自己的拒绝。
//   - 被接替的尝试记最近的 maxRememberedSuperseded 条（方案 4.4）：截止之前它的投递不执行，截止过后它的投递在截止检查处
//     就不执行；一连串接替只留最新的那些（剪掉的早已过了截止）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

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
