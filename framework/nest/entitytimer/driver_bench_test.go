package entitytimer

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 假准入只计成本，不运行 handler；覆盖到期堆、投递和统计，不代表 Nest 端到端吞吐。
type benchmarkClient struct{ nest.Client }

func (benchmarkClient) Dispatch(context.Context, nest.HandlerName, int64, nest.Params, ...nest.SendOpt) error {
	return nil
}

func BenchmarkDriverDueBatch(b *testing.B) {
	d := NewDriver(Config{Name: "bench.timer", Handler: nest.NewHandlerName("tick")})
	now := time.Now()
	d.SetClock(func() time.Time { return now })
	d.systemNow = func() time.Time { return now }
	live := &liveSet{}
	if err := d.Bind(benchmarkClient{}, live.lookup); err != nil {
		b.Fatal(err)
	}
	for id := int64(1); id <= 128; id++ {
		owner := newOwner(id)
		live.set(owner)
		d.report(d.newSlot(owner), 1, now)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		now = now.Add(10 * time.Second)
		if got := d.DeliverDue(context.Background()); got != 128 {
			b.Fatalf("delivered %d", got)
		}
	}
}
