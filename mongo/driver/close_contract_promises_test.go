package driver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// RR-20261006-10（W-2026-10-06-02 转 RR）：驱动 Close 的统一口径——重复 Close 幂等返回 nil，第一次的
// 错误只报一次；并发 Close 的后到者等第一个做完再返回；Close 之后的其他调用返回已关闭错误（这里是
// mongo.ErrClientDisconnected）。
//
// 旧行为：重复 Close 已经返回 nil（NC-260），但并发 Close 的后到者从驱动拿到 ErrClientDisconnected
// 立刻按“已关闭”返回 nil，第一个调用者还在等在途连接归还、关连接池——后到者据此释放依赖就早了。
// 用 disconnect 替身把第一次断开卡住，观察后到者是否在它完成之前返回。
func TestClientConcurrentCloseWaitsForTheFirstDisconnect(t *testing.T) {
	cfg := fmongo.DefaultConfig("mongodb://127.0.0.1:1")
	cfg.ConnectTimeout = 100 * time.Millisecond
	c, err := NewClient(cfg, IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	c.disconnect = func(ctx context.Context) error {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if !first {
			// 与 mongo-driver 相同：拓扑已经在关闭，第二次断开立刻返回 ErrClientDisconnected。
			return mongo.ErrClientDisconnected
		}
		close(entered)
		<-release
		return c.cli.Disconnect(ctx)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- c.Close(context.Background()) }()
	<-entered

	secondDone := make(chan error, 1)
	go func() { secondDone <- c.Close(context.Background()) }()
	select {
	case err := <-secondDone:
		close(release)
		<-firstDone
		t.Fatalf("concurrent Close returned %v while the first Close was still disconnecting; want it to wait", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Close = %v", err)
	}
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("concurrent Close = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Close still blocked after the first finished")
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close after close = %v, want nil", err)
	}
}

// 控制（修前修后都通过）：第一次断开失败（FLE 客户端断开失败这一类）时拓扑没断开，再调 Close 要重做断开，
// 而不是按“已关闭”吞掉；关完之后的命令返回 ErrClientDisconnected。
func TestClientCloseRetriesAfterAFailedDisconnect(t *testing.T) {
	cfg := fmongo.DefaultConfig("mongodb://127.0.0.1:1")
	cfg.ConnectTimeout = 100 * time.Millisecond
	c, err := NewClient(cfg, IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("disconnect failed")
	calls := 0
	c.disconnect = func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return boom
		}
		return c.cli.Disconnect(ctx)
	}
	if err := c.Close(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("first Close = %v, want the disconnect error", err)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("retry Close = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("disconnect calls = %d, want 2: the retry must disconnect again", calls)
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close after a successful retry = %v, want nil", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx); !errors.Is(err, mongo.ErrClientDisconnected) {
		t.Fatalf("Ping after Close = %v, want mongo.ErrClientDisconnected", err)
	}
}
