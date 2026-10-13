package redis

import (
	"context"
	"sync"
	"testing"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

// Close / Stop 的统一口径：重复调用幂等返回 nil，第一次的错误只报一次；并发调用安全，后到者等第一个做完（-race 下不能有数据竞争，
// 不能把连接池关两次）。不可达地址，不需要真实 Redis。
func TestSingletonStoreCloseErrorIsReportedOnce(t *testing.T) {
	s, err := newSingletonStore(fredis.DefaultConfig("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	// 锁客户端的底层连接池先被关过：第一次 Close 报这个错误。
	_ = s.lock.(*redisdriver.Client).Raw().Close()
	if err := s.Close(); err == nil {
		t.Fatal("first Close with the lock pool closed underneath = nil, want the error reported once")
	}
	for attempt := 2; attempt <= 3; attempt++ {
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d = %v, want nil: the first error is reported once", attempt, err)
		}
	}
}

func TestRedisModConcurrentStopIsSafe(t *testing.T) {
	for round := 0; round < 5; round++ {
		asm, err := redisdriver.Assemble(fredis.DefaultConfig("127.0.0.1:1"))
		if err != nil {
			t.Fatal(err)
		}
		m := &RedisMod{asm: asm, cfg: fredis.DefaultConfig("127.0.0.1:1")}
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
		if err := m.StopWithContext(context.Background()); err != nil {
			t.Fatalf("Stop after stop = %v, want nil", err)
		}
	}
}

// Redis Mod 的停止入口幂等：go-redis 的 Close 先把连接池标记为关闭再返回逐个关连接时的第一个错误，返回错误时资源同样已经释放，
// 所以 Close 出错之后再调用 Stop 返回 nil（停机契约第 4 步）。用例先关闭一次底层客户端来制造“Close 返回错误”。
func TestRedisModStopConvergesAfterACloseError(t *testing.T) {
	asm, err := redisdriver.Assemble(fredis.DefaultConfig("127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	m := &RedisMod{asm: asm, cfg: fredis.DefaultConfig("127.0.0.1:1")}
	_ = asm.Client.Raw().Close() // 连接池已在底层关闭；Mod 的 Close 将返回错误（驱动 Close 幂等后要绕过它，RR-20261006-10）

	if err := m.StopWithContext(context.Background()); err == nil {
		t.Fatal("first Stop = nil, want the Close error reported once")
	}
	if err := m.StopWithContext(context.Background()); err != nil {
		t.Fatalf("Stop after the pool was closed = %v, want nil: retrying can never succeed", err)
	}
}
