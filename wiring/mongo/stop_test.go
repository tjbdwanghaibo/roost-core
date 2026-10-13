package mongo

import (
	"context"
	"sync"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

// Stop 的统一口径：重复调用幂等返回 nil，并发调用安全，后到者等第一个做完（-race 下不能有数据竞争）。用不拨号的真实客户端。
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

// 第三方客户端的 Close 不幂等（mongo-driver 第二次 Disconnect 返回 “client is disconnected”）：客户端已经断开时，Stop 也要返回 nil
// 并交出 client（停机契约第 4 步“已停完的对象再调用返回 nil”）。用不拨号的真实客户端，先关闭一次来制造“已断开”。
func TestMongoModStopConvergesWhenTheClientIsAlreadyDisconnected(t *testing.T) {
	cfg := fmongo.DefaultConfig("mongodb://127.0.0.1:1")
	cfg.ConnectTimeout = 100 * time.Millisecond
	cli, err := mongodriver.NewClient(cfg, mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	m := &MongoMod{client: cli, cfg: cfg}
	if err := cli.Close(context.Background()); err != nil {
		t.Fatalf("first Disconnect = %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if err := m.StopWithContext(context.Background()); err != nil {
			t.Fatalf("Stop #%d on an already disconnected client = %v, want nil: the connection is released and a retry can never succeed", attempt, err)
		}
	}
}

// 正常路径的控制：一次停止关闭连接，再调用返回 nil。
func TestMongoModStopTwiceReturnsNil(t *testing.T) {
	cfg := fmongo.DefaultConfig("mongodb://127.0.0.1:1")
	cfg.ConnectTimeout = 100 * time.Millisecond
	cli, err := mongodriver.NewClient(cfg, mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	m := &MongoMod{client: cli, cfg: cfg}
	if err := m.StopWithContext(context.Background()); err != nil {
		t.Fatalf("first Stop = %v", err)
	}
	if err := m.StopWithContext(context.Background()); err != nil {
		t.Fatalf("second Stop = %v, want nil", err)
	}
	// 底层客户端再关也返回 nil（driver 层 Close 幂等）。
	if err := cli.Close(context.Background()); err != nil {
		t.Fatalf("Close on the released client = %v, want nil", err)
	}
}
