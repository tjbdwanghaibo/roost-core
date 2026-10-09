package mongo

import (
	"context"
	"sync"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

// RR-20261006-10（W-2026-10-06-02 转 RR）：Stop 的统一口径——重复调用幂等返回 nil，并发调用安全，
// 后到者等第一个做完。旧行为：MongoMod.StopWithContext 并发调用时读写 m.client 没有同步（-race 报
// 数据竞争）。App 每次停机对每个 Mod 只串行调一次，现有路径走不到。用不拨号的真实客户端。
func TestMongoModConcurrentStopIsSafe(t *testing.T) {
	for round := 0; round < 5; round++ {
		cfg := fmongo.DefaultConfig("mongodb://127.0.0.1:1")
		cfg.ConnectTimeout = 100 * time.Millisecond
		cli, err := mongodriver.NewClient(cfg, mongodriver.IndexMigrationPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		m := &MongoMod{client: cli, cfg: cfg}
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func(i int) { defer wg.Done(); errs[i] = m.StopWithContext(context.Background()) }(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent Stop #%d = %v, want nil (all = %v)", round, i, err, errs)
			}
		}
		if m.Client() != nil {
			t.Fatalf("round %d: client still held after a successful Stop", round)
		}
	}
}
