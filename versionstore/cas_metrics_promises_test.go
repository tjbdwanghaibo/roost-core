package versionstore

// 维护者第十二轮决定（CAS 冲突率口径，N06 观察 2）：compare-and-set 冲突率在 versionstore 层统一
// 计数，不再由各服务各报一份。之前只有 chat 在拿到 ErrConflict 时报 Conflict，其他服务、account /
// activity 一处都没有，kit/service/README 第 6 条要求的“冲突率”没有统一口径。
//
// 承诺：RedisStore.Update 的每次 compare-and-set 计一次 versionstore.cas.total{store, result}，
// 赢了 result=applied、输了 result=lost；预算用尽返回 ErrConflict 时另计一次
// versionstore.conflict.total{store}。store 是该存储的键前缀（每个存储一个，低基数）。冲突率 =
// lost / (applied + lost)。

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

func TestUpdateCountsEveryCompareAndSetAndTheExhaustedConflict(t *testing.T) {
	old := metrics.DefaultRegistry()
	reg := metrics.NewRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(old) })

	fake := newFakeRedis()
	store, err := NewRedisStore[string, counter](fake, RedisConfig[string, counter]{
		Prefix: "roost:test:counter:", KeyOf: func(key string) string { return key }, Codec: JSONCodec[counter]{},
		MaxAttempts: 3, RetryBackoff: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bump := func(c counter, _ bool) (counter, bool, error) { c.Total++; return c, true, nil }
	for range 2 {
		if _, _, err := store.Update(ctx, "a", bump); err != nil {
			t.Fatal(err)
		}
	}
	fake.failEveryCAS = true
	if _, _, err := store.Update(ctx, "a", bump); !errors.Is(err, ErrConflict) {
		t.Fatalf("Update under permanent contention = %v, want ErrConflict", err)
	}
	// 不保存（save=false）不发 compare-and-set，也不计数。
	if _, _, err := store.Update(ctx, "a", func(c counter, _ bool) (counter, bool, error) { return c, false, nil }); err != nil {
		t.Fatal(err)
	}

	value := func(name string, labels metrics.Labels) int64 {
		for _, metric := range reg.Snapshot() {
			if metric.Name != name || len(metric.Labels) != len(labels) {
				continue
			}
			match := true
			for k, v := range labels {
				match = match && metric.Labels[k] == v
			}
			if match {
				return metric.Value
			}
		}
		return 0
	}
	const label = "roost:test:counter:"
	if got := value(MetricCompareAndSet, metrics.Labels{"store": label, "result": "applied"}); got != 2 {
		t.Errorf("%s{result=applied} = %d, want 2", MetricCompareAndSet, got)
	}
	if got := value(MetricCompareAndSet, metrics.Labels{"store": label, "result": "lost"}); got != 3 {
		t.Errorf("%s{result=lost} = %d, want 3 (every attempt of the exhausted Update)", MetricCompareAndSet, got)
	}
	if got := value(MetricConflict, metrics.Labels{"store": label}); got != 1 {
		t.Errorf("%s = %d, want 1", MetricConflict, got)
	}
}
