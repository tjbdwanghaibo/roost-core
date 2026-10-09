package saga

// RR-20261006-15 的承诺：同一操作实例（saga + 方向 + 步骤）的尝试跨 Resume 累积，不能让新一生的尝试执行不了。
// 修前（按操作扫描全部 claim）一个操作累积超过 4096 份 claim 后每次 Reserve 都报 ErrConflict；RR-15 / -16 之后改为只取
// 有影响的 claim 并分两类计上限。收件箱改为每个操作一份状态文档之后（docs/feature/SAGA-OPERATION-STATE-DOC-2026-10-06.md），
// 判定只读这一份文档，尝试次数、Resume 次数都不进入判定，也没有上限：这里断言任意多次尝试与 Resume 之后仍能执行、
// 之后回放，而且整个操作始终只有一份文档。
//
// 夹具：每一生先走真实 Handle：可重试失败、handler 出错（交还租约）后被下一次接管、可重试失败，再用真实 Handle 补满这一生的
// 可重试失败，五生共 4100 次尝试（超过修前的 4096），都在保留期内。

import (
	"context"
	"errors"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

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
