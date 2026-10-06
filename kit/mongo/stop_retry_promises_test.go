package mongo

import (
	"context"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
)

// RR-20261005-NC-260（小防护 B，与 NC-173 残余 / NC-233 同类）：第三方客户端的 Close 不幂等。
// mongo-driver 的 Disconnect 第二次调用返回 “client is disconnected”（拓扑已关闭）。Mongo Mod 只在 Close
// 成功时交出 client，客户端一旦已经断开（别处关过一次），之后每次 Stop 都调 Disconnect、都得到同一个错误，
// 停机契约第 4 步“已停完的对象再调用返回 nil”永远不成立，调用方据此以为连接还没释放。
// 用不拨号的真实客户端（mongo-driver v2 Connect 不立即连接），先关闭一次来制造“已断开”。
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
