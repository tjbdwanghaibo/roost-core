package saga

// RR-20261006-16：RR-20261006-15 让 Reserve 只读“有影响的 claim”，但升级前写的 completed claim 没有 outcome，只能全部当作
// “可能有影响”读出来，而且仍和新写的 claim 共用 4096 的上限。升级前的进程按“这个操作的全部 claim”计数，第 4097 份
// 是在看到 4096 份时写下的，所以升级前就已经卡住的操作恰好有 4097 份不带 outcome 的 claim：升级之后新进程照样
// 报 ErrConflict，修好原因再 Resume 也执行不了，要等旧 claim 过了 TTL（默认 30 天）——RR-15 要解决的正是这个现象，
// 卡住的操作却要等到 TTL 才恢复。混跑期间旧进程写的 claim 同样不带 outcome。
//
// 承诺：升级前（或混跑中旧进程）写的 claim 不能让新进程的尝试执行不了；它们的数量受旧进程自己的上限约束
// （见 maxLegacyOperationClaims），不和有 outcome 的 claim 共用一个上限。
//
// 夹具：真实 Handle 写一份可重试失败的 claim，再原样复制、去掉 outcome，凑成五生共 4097 份——等于修前进程按全部
// claim 计数时一个操作能留下的最多份数（看到 4096 份时还能再写一份）。

import (
	"context"
	"errors"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOperationStuckBeforeUpgradeExecutesAfterUpgrade(t *testing.T) {
	runLegacyStuckOperationCase(t, mongotest.NewClient(), "legacy_cap")
}

// runLegacyStuckOperationCase 同时跑在 mongotest 与真实 Mongo 副本集上（mongo_step_operation_real_mongo_integration_test.go）。
func runLegacyStuckOperationCase(t *testing.T, client fmongo.IMongo, database string) {
	ctx := context.Background()
	inbox, err := NewMongoCommandInbox(client, database, "steps")
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	claims := client.Database(database).Collection("steps" + mongoInboxClaimSuffix)
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
	first := command(0, 1)
	if completion, duplicate, err := inbox.Handle(ctx, first, retryable); err != nil || duplicate || completion.Success {
		t.Fatalf("fixture attempt %s: %+v duplicate=%v err=%v, want its own retryable failure", first.ID, completion, duplicate, err)
	}
	var written bson.M
	if err := claims.FindOne(ctx, bson.M{"_id": stepClaimID(first.ID)}, &written); err != nil {
		t.Fatal(err)
	}
	if _, err := claims.UpdateOne(ctx, bson.M{"_id": stepClaimID(first.ID)}, bson.M{"$unset": bson.M{"outcome": ""}}); err != nil {
		t.Fatal(err)
	}
	delete(written, "outcome")

	const lives, attemptsPerLife, legacyTotal = 5, 820, 4097
	copies := make([]any, 0, legacyTotal-1)
	for n := 1; n < legacyTotal; n++ {
		incarnation, attempt := uint32(n/attemptsPerLife), uint32(n%attemptsPerLife)+1
		clone := bson.M{}
		for key, value := range written {
			clone[key] = value
		}
		id := command(incarnation, attempt).ID
		clone["_id"], clone["command_id"], clone["incarnation"] = stepClaimID(id), id, incarnation
		copies = append(copies, clone)
	}
	if _, err := claims.InsertMany(ctx, copies); err != nil {
		t.Fatal(err)
	}
	legacy := bson.M{"namespace": stepClaimNamespace, "operation_key": operation, "outcome": bson.M{"$exists": false}}
	if total, err := claims.CountDocuments(ctx, legacy); err != nil || total != legacyTotal {
		t.Fatalf("fixture has %d claims without outcome (err=%v), want %d", total, err, legacyTotal)
	}

	// 升级后、原因修好再 Resume：新一生的第一次尝试必须执行。
	next := command(lives, 1)
	completion, duplicate, err := inbox.Handle(ctx, next, func(txCtx context.Context, c Command) (Completion, error) {
		if _, err := business.InsertOne(txCtx, bson.M{"_id": c.ID, "operation": c.IdempotencyKey}); err != nil {
			return Completion{}, err
		}
		return Completion{Success: true, Data: []byte("delivered")}, nil
	})
	if err != nil || duplicate || !completion.Success || completion.CommandID != next.ID {
		t.Fatalf("an operation left with %d claims written before the upgrade: the first attempt of the next life %s: %+v duplicate=%v err=%v, "+
			"want it to execute: claims without outcome must not keep the step unexecutable after the upgrade", legacyTotal, next.ID, completion, duplicate, err)
	}
	late := command(lives, 2)
	replayed, duplicate, err := inbox.Handle(ctx, late, retryable)
	if err != nil || !duplicate || replayed.CommandID != next.ID || !replayed.Success {
		t.Fatalf("attempt %s after %s took effect: %+v duplicate=%v err=%v, want the success replayed", late.ID, next.ID, replayed, duplicate, err)
	}
	success, found, err := inbox.operationSuccess(ctx, command(lives, 3))
	if err != nil || !found || success.CommandID != next.ID {
		t.Fatalf("operationSuccess for %s: %+v found=%v err=%v, want the success of %s", operation, success, found, err, next.ID)
	}
	if count, err := business.CountDocuments(ctx, bson.M{"operation": operation}); err != nil || count != 1 {
		t.Fatalf("operation %s took effect %d time(s) (err=%v), want 1", operation, count, err)
	}
}

// 两类 claim 各自的上限仍在：不带 outcome 的超过 maxLegacyOperationClaims、带 outcome 的有影响的超过
// maxDecisiveOperationClaims，都按数据异常报 ErrConflict，不做无界扫描。
func TestOperationClaimsLimitsCountLegacyAndDecisiveClaimsSeparately(t *testing.T) {
	ctx := context.Background()
	client := mongotest.NewClient()
	inbox, err := NewMongoCommandInbox(client, "claim_limits", "steps")
	if err != nil {
		t.Fatal(err)
	}
	claims := client.Database("claim_limits").Collection("steps" + mongoInboxClaimSuffix)
	fill := func(operation string, n int, doc bson.M) {
		docs := make([]any, 0, n)
		for i := 0; i < n; i++ {
			d := bson.M{"_id": stepClaimID(commandID(operation, 0, uint32(i+1))), "namespace": stepClaimNamespace, "operation_key": operation}
			for key, value := range doc {
				d[key] = value
			}
			docs = append(docs, d)
		}
		if _, err := claims.InsertMany(ctx, docs); err != nil {
			t.Fatal(err)
		}
	}
	fill("legacy-at-limit", maxLegacyOperationClaims, bson.M{"status": claimStatusCompleted})
	fill("legacy-over", maxLegacyOperationClaims+1, bson.M{"status": claimStatusCompleted})
	fill("decisive-over", maxDecisiveOperationClaims+1, bson.M{"status": claimStatusPending})
	if found, err := inbox.operationClaims(ctx, "legacy-at-limit", true, 0); err != nil || len(found) != maxLegacyOperationClaims {
		t.Fatalf("%d claims without outcome: got %d, err=%v, want all of them", maxLegacyOperationClaims, len(found), err)
	}
	for _, operation := range []string{"legacy-over", "decisive-over"} {
		if _, err := inbox.operationClaims(ctx, operation, true, 0); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s: err=%v, want ErrConflict", operation, err)
		}
	}
}
