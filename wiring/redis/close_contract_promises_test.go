package redis

import (
	"context"
	"sync"
	"testing"

	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
)

// RR-20261006-10（W-2026-10-06-02 转 RR）：Close / Stop 的统一口径——重复调用幂等返回 nil，第一次的
// 错误只报一次；并发调用安全，后到者等第一个做完。
//
// 旧行为：单实例锁 store 的 Close 粘滞返回第一次的结果（第一次出错，之后每次都是同一个错误）；
// RedisMod.StopWithContext 并发调用时读写 m.asm 没有同步（-race 报数据竞争），可能把连接池关两次。
// App 每次停机对每个 Mod 只串行调一次，现有路径走不到，但停止入口的契约是“幂等、可重入”。
// 不可达地址，不需要真实 Redis。
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
