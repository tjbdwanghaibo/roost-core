//go:build integration

package saga

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
)

// BenchmarkRealMongoCommandInboxHandle 是 BenchmarkMongoCommandInboxHandle 在真实 Mongo 副本集上的版本：
//
//	source ~/.roost-it/roost-dataengine-it/env.sh
//	GOWORK=off go test -tags integration -run '^$' -bench BenchmarkRealMongoCommandInboxHandle -benchtime 500x -count 5 ./saga/
//
// 资源：库 roost_sagadir_bench_<pid>_<ns>，用后删除。
func BenchmarkRealMongoCommandInboxHandle(b *testing.B) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		b.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set")
	}
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		b.Fatal(err)
	}
	database := fmt.Sprintf("roost_sagadir_bench_%d_%d", os.Getpid(), time.Now().UnixNano())
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(ctx); err != nil {
			b.Errorf("drop %s: %v", database, err)
		}
		_ = client.Close(context.Background())
	})
	benchmarkMongoCommandInboxHandle(b, client, database)
}
