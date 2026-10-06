package saga

// saga 方向 ②的代价：MongoCommandInbox.Handle 从一个事务（读回执、写回执、handler）变为 Reserve 事务（读回执、读 claim、
// 守卫 upsert、按操作查询、写 claim）加执行事务（handler、claim 条件写、写回执）。同一个基准在修改前后各跑一次：
//
//	GOWORK=off go test -run '^$' -bench BenchmarkMongoCommandInboxHandle -benchtime 2000x -count 5 ./saga/
//
// 真实 Mongo 见 mongo_step_benchmark_real_mongo_integration_test.go。mongotest 按集合快照，集合越大越慢，只用于同口径前后对照。

import (
	"context"
	"fmt"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func BenchmarkMongoCommandInboxHandle(b *testing.B) {
	benchmarkMongoCommandInboxHandle(b, mongotest.NewClient(), "bench_mongo_step")
}

func benchmarkMongoCommandInboxHandle(b *testing.B, client fmongo.IMongo, database string) {
	ctx := context.Background()
	inbox, err := NewMongoCommandInbox(client, database, "steps")
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
		if _, duplicate, err := inbox.Handle(ctx, command, handler); err != nil || duplicate {
			b.Fatalf("handle %s: duplicate=%v err=%v", command.ID, duplicate, err)
		}
	}
}
