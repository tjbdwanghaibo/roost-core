package nats

import (
	"context"
	"sync"
	"testing"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
)

// RR-20261006-10（W-2026-10-06-02 转 RR）：Stop 的统一口径——重复调用幂等返回 nil，并发调用安全，
// 后到者等第一个做完。旧行为：NatsMod.StopWithContext 并发调用时读写 m.bus / m.asm 没有同步（-race
// 报数据竞争）。App 每次停机对每个 Mod 只串行调一次，现有路径走不到。与 Redis / Mongo Mod 同类，一并统一。
func TestNatsModConcurrentStopIsSafe(t *testing.T) {
	for round := 0; round < 5; round++ {
		rpc := natsdriver.NewRPCClient(nil, fnats.DefaultRetryPolicy(), 1)
		m := &NatsMod{asm: &natsdriver.Assembly{RPC: rpc}}
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
	}
}
