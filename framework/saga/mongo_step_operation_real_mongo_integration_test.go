//go:build integration

package saga

// saga 方向 ②在真实 Mongo 副本集上的红绿：mongotest 按集合检测写冲突，比服务端的文档级冲突保守；
// “相邻两次尝试都提交”要在真实服务端上复现（两次尝试写的是不同文档，互不冲突）。场景说明见 mongo_step_operation_promises_test.go。
//
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh 后
//   GOWORK=off go test -tags integration -count=1 -run '^TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce$' ./framework/saga/
// 资源：库 roost_sagadir_<pid>_<ns>，用后删除。

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

func realSagadirMongo(t *testing.T) (*driver.Client, string) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; source ~/.roost-it/roost-dataengine-it/env.sh")
	}
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_sagadir_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
		_ = client.Close(context.Background())
	})
	return client, database
}

func TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce(t *testing.T) {
	runMongoStepOperationCases(t, func(t *testing.T) (fmongo.IMongo, string) { return realSagadirMongo(t) })
}

// RR-20261006-15 的承诺在状态文档形状下：任意多次尝试与 Resume 之后新一生照常执行（真实副本集，次数比 mongotest 少，
// 每次 Handle 是两次落盘提交）。
func TestRealMongoOperationAttemptsAccumulatedOverResumesDoNotBlockANewLife(t *testing.T) {
	client, database := realSagadirMongo(t)
	runAccumulatedAttemptsCase(t, client, database, 5, 220)
}
