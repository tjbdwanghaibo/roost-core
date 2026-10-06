package saga

// RR-20261006-15：同一操作实例（saga + 方向 + 步骤）的尝试跨 Resume 累积，Reserve 却按“这个操作的全部 claim”扫描、
// 超过 4096 份就以 ErrConflict 拒绝。协调器每一生最多派发 1000 次尝试（Definition.Validate），Resume 次数不设上限，
// claim 在 receiptTTL（默认 30 天）内都在：一个步骤反复失败、被 Resume 四五次之后，这一步的每次尝试 Reserve 都报
// ErrConflict，直到旧 claim 过期——修好了原因再 Resume 也执行不了。只读的 operationSuccess 用同一个 Limit 截断而不报错，
// 超过上限时可能看不到已经生效的成功。
//
// 承诺：旧一生的可重试失败、被接替的尝试都不影响新一生（resolveOtherAttempts 本来就跳过它们），它们的数量也不能让
// 新一生的尝试执行不了。
//
// 夹具：每一生先走真实 Handle 产生三份 claim（可重试失败、handler 出错后被下一次接替、可重试失败），再把这一生
// 最后一份可重试失败的 claim 原样复制、只换 CommandID，凑到每生 820 次尝试。复制而不是 4100 次真实 Handle，
// 是因为替身每次扫描全集合，全量真实路径要近一分钟；全量真实路径修前同样在第 4097 次尝试报 ErrConflict（记录里有原文）。

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestOperationAttemptsAccumulatedOverResumesDoNotBlockANewLife(t *testing.T) {
	runAccumulatedAttemptsCase(t, mongotest.NewClient(), "attempt_cap")
}

// runAccumulatedAttemptsCase 同时跑在 mongotest 与真实 Mongo 副本集上（mongo_step_operation_real_mongo_integration_test.go）。
func runAccumulatedAttemptsCase(t *testing.T, client fmongo.IMongo, database string) {
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
	crashed := func(context.Context, Command) (Completion, error) { return Completion{}, errors.New("handler crashed") }

	// 五生、每生 820 次尝试（每生不超过 Definition 允许的 1000 次），共 4100 次，都在 claim TTL 内。
	const lives, attemptsPerLife = 5, 820
	for incarnation := uint32(0); incarnation < lives; incarnation++ {
		if completion, duplicate, err := inbox.Handle(ctx, command(incarnation, 1), retryable); err != nil || duplicate || completion.Success {
			t.Fatalf("life %d attempt 1: %+v duplicate=%v err=%v, want its own retryable failure", incarnation, completion, duplicate, err)
		}
		if _, _, err := inbox.Handle(ctx, command(incarnation, 2), crashed); err == nil {
			t.Fatalf("life %d attempt 2: want the handler error", incarnation)
		}
		if completion, duplicate, err := inbox.Handle(ctx, command(incarnation, 3), retryable); err != nil || duplicate || completion.Success {
			t.Fatalf("life %d attempt 3: %+v duplicate=%v err=%v, want it to take over attempt 2 and fail retryably", incarnation, completion, duplicate, err)
		}
		var written bson.M
		if err := claims.FindOne(ctx, bson.M{"_id": stepClaimID(command(incarnation, 3).ID)}, &written); err != nil {
			t.Fatal(err)
		}
		copies := make([]any, 0, attemptsPerLife-3)
		for attempt := uint32(4); attempt <= attemptsPerLife; attempt++ {
			clone := bson.M{}
			for key, value := range written {
				clone[key] = value
			}
			id := command(incarnation, attempt).ID
			clone["_id"], clone["command_id"] = stepClaimID(id), id
			copies = append(copies, clone)
		}
		if _, err := claims.InsertMany(ctx, copies); err != nil {
			t.Fatal(err)
		}
	}
	if total, err := claims.CountDocuments(ctx, bson.M{"namespace": stepClaimNamespace, "operation_key": operation}); err != nil || total != lives*attemptsPerLife {
		t.Fatalf("fixture has %d claims (err=%v), want %d", total, err, lives*attemptsPerLife)
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
}

// 拒绝只在同一生里回放（RR-20261006-15 收窄扫描后的对照）：第 0 代的 claim 有两种写法（新建时 omitempty 不写
// incarnation，接管自己过期的 claim 时写成 0），都要被选中；别的生的拒绝、可重试失败与被接替的尝试不被选中。
func TestOperationClaimsFilterSelectsOnlyDecisiveClaims(t *testing.T) {
	runOperationClaimsFilterCase(t, mongotest.NewClient().Database("filter").Collection("claims"))
}

func runOperationClaimsFilterCase(t *testing.T, claims fmongo.ICollection) {
	ctx := context.Background()
	operation := "gift-1:1:0"
	docs := []any{
		bson.M{"_id": "pending", "namespace": stepClaimNamespace, "operation_key": operation, "status": claimStatusPending},
		bson.M{"_id": "success-r2", "namespace": stepClaimNamespace, "operation_key": operation, "status": claimStatusCompleted, "outcome": claimOutcomeSuccess, "incarnation": uint32(2)},
		bson.M{"_id": "legacy", "namespace": stepClaimNamespace, "operation_key": operation, "status": claimStatusCompleted},
		bson.M{"_id": "refused-r0-new", "namespace": stepClaimNamespace, "operation_key": operation, "status": claimStatusCompleted, "outcome": claimOutcomeRefused},
		bson.M{"_id": "refused-r0-takeover", "namespace": stepClaimNamespace, "operation_key": operation, "status": claimStatusCompleted, "outcome": claimOutcomeRefused, "incarnation": uint32(0)},
		bson.M{"_id": "refused-r1", "namespace": stepClaimNamespace, "operation_key": operation, "status": claimStatusCompleted, "outcome": claimOutcomeRefused, "incarnation": uint32(1)},
		bson.M{"_id": "retryable-r0", "namespace": stepClaimNamespace, "operation_key": operation, "status": claimStatusCompleted, "outcome": claimOutcomeRetryable},
		bson.M{"_id": "superseded", "namespace": stepClaimNamespace, "operation_key": operation, "status": claimStatusSuperseded},
		bson.M{"_id": "other-operation", "namespace": stepClaimNamespace, "operation_key": "gift-1:1:1", "status": claimStatusPending},
	}
	if _, err := claims.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		refusals    bool
		incarnation uint32
		want        []string
	}{
		{"life 0", true, 0, []string{"legacy", "pending", "refused-r0-new", "refused-r0-takeover", "success-r2"}},
		{"life 1", true, 1, []string{"legacy", "pending", "refused-r1", "success-r2"}},
		{"success only", false, 0, []string{"legacy", "pending", "success-r2"}},
	} {
		var found []bson.M
		if err := claims.Find(ctx, operationClaimsFilter(operation, tc.refusals, tc.incarnation), &found, fmongo.FindOption{}); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, doc := range found {
			ids = append(ids, doc["_id"].(string))
		}
		slices.Sort(ids)
		if !slices.Equal(ids, tc.want) {
			t.Errorf("%s: selected %v, want %v", tc.name, ids, tc.want)
		}
	}
}
