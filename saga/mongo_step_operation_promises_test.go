package saga

// saga 方向 ②（维护者第六轮决定）：Mongo 步骤（MongoCommandInbox）与原生步骤同一套“操作实例最多生效一次”的收件箱契约。
// 旧实现只保证同一 CommandID 最多一次：协调器判尝试 k 超时、发出 k+1 后，k+1 不知道 k 的存在。N06 S5 的真实两进程强杀里
// 一个 Mongo 步骤被相邻两次尝试都提交了（尝试 k 在 WriteConflict 重试里拖过截止才提交），靠业务按 IdempotencyKey 幂等才兜住。
//
// 两个确定性场景，业务写都在 handler 的 Mongo 事务里、按 CommandID 插一份文档（业务自己不做按操作的幂等），文档数就是生效次数：
//
//  1. 尝试 k 已提交、结果没送达，协调器发出 k+1：k+1 不执行，回放 k 的结果（CommandID 是 k 的）。
//  2. 尝试 k 的事务在途（业务写已做、未提交），k+1 到达：k 的租约有效时 k+1 不执行；k 过了截止，k+1 接替并执行；
//     k 之后再想提交，事务里对操作状态文档的条件写不再匹配，整笔中止。
//
// runMongoStepOperationCases 同时跑在 mongotest 与真实 Mongo 副本集上（mongo_step_operation_real_mongo_integration_test.go）。

import (
	"context"
	"errors"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

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
