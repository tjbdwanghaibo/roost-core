package saga

// Mongo 步骤收件箱（M-A）一次 Handle 的代价：一个事务里读回执、handler、条件写回执。同一个基准在修改前后各跑一次：
//
//	GOWORK=off go test -run '^$' -bench BenchmarkMongoStepInboxHandle -benchtime 2000x -count 5 ./framework/saga/
//
// 真实 Mongo 见 mongo_step_benchmark_real_mongo_integration_test.go。mongotest 按集合快照，集合越大越慢，只用于同口径前后对照。
// 历史：BenchmarkMongoCommandInboxHandle（MongoCommandInbox 删除，形状保留）。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func BenchmarkMongoStepInboxHandle(b *testing.B) {
	benchmarkMongoStepInboxHandle(b, mongotest.NewClient(), "bench_mongo_step")
}

func benchmarkMongoStepInboxHandle(b *testing.B, client fmongo.IMongo, database string) {
	ctx := context.Background()
	inbox, err := NewMongoStepInbox(client, database, "steps", time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		b.Fatal(err)
	}
	business := client.Database(database).Collection("business")
	handler := func(txCtx context.Context, command Command) (Completion, error) {
		if _, err := business.InsertOne(txCtx, bson.M{"_id": command.ID}); err != nil {
			return Completion{}, err
		}
		return Completion{Success: true}, nil
	}
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		command := mongoStepCommand(fmt.Sprintf("bench-%s-%d:1:0", run, index), 1, time.Now().Add(time.Minute))
		if outcome, err := inbox.Handle(ctx, command, handler); err != nil || outcome.Kind != StepExecuted {
			b.Fatalf("handle %s: %+v err=%v", command.ID, outcome, err)
		}
	}
}

// mongoStepCommand 是一个操作的一次尝试（操作键 <saga>:<phase>:<step>，CommandID = 操作键:尝试号）。
func mongoStepCommand(operation string, attempt uint32, deadline time.Time) Command {
	saga, _, _ := strings.Cut(operation, ":")
	now := time.Now().UTC()
	return Command{ID: fmt.Sprintf("%s:%d", operation, attempt), IdempotencyKey: operation, SagaID: saga, SagaType: "bench", DefinitionVersion: 1,
		BusinessKey: saga, StepName: "step", Phase: PhaseForward, Attempt: attempt, Topic: "bench.step", CreatedAt: now, DeadlineAt: deadline}
}
