package entity_test

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// RR-20260927-14（OPEN-ITEMS C14）：卸载后重载（RR-20260926-59）在持续故障下的最坏延迟很长（默认上界约 43 小时，见修复记录），
// 期间订阅者停在旧内容上，但运维只能看到累计计数（UnloadResyncStats），看不到“现在还有多少实体在排队 / 重载”。
// 承诺：低基数（无标签）的 gauge entity.unload_resync.backlog 实时反映等待或正在重载的实体数，排空后回到 0，停止后为 0；
// UnloadResyncStats.Backlog 给出同一个数。默认值不变。

const backlogMetric = "entity.unload_resync.backlog"

func backlogGauge(reg *metrics.Registry) (value int64, labels int, found bool) {
	for _, m := range reg.Snapshot() {
		if m.Name == backlogMetric {
			return m.Value, len(m.Labels), true
		}
	}
	return 0, 0, false
}

func TestUnloadResyncBacklogIsObservable(t *testing.T) {
	reg := metrics.NewRegistry()
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })

	gate := make(chan struct{})
	h := newResyncHarness(t, &scriptedLoader{label: "authority", gate: gate}, entity.UnloadResyncConfig{Workers: 1, QueueCapacity: 8})
	const entities = 3
	stale := make([]*resyncEntity, 0, entities)
	for i := range entities {
		value := h.resident(int64(6100+i), "rejected", false)
		if err := h.sync.Subscribe(1, value.ID(), entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
		stale = append(stale, value)
	}
	if err := h.sync.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	for len(h.frames) > 0 {
		<-h.frames
	}
	for _, value := range stale {
		h.unload(value)
	}
	// 1 个 worker 停在 loader 的 gate 上，其余 2 个在队列里：积压 3。
	waitFor(t, "the reload to reach the loader", func() bool { return h.loader.inFlight.Load() == 1 })
	value, labels, found := backlogGauge(reg)
	if !found || value != entities {
		t.Fatalf("%s = (%d, found=%v) with %d entities waiting or reloading; want %d", backlogMetric, value, found, entities, entities)
	}
	if labels != 0 {
		t.Fatalf("%s carries %d labels; it must stay a single low-cardinality series", backlogMetric, labels)
	}
	if got := h.access.UnloadResyncStats().Backlog; got != entities {
		t.Fatalf("UnloadResyncStats().Backlog = %d, want %d", got, entities)
	}

	close(gate)
	waitFor(t, "the backlog to drain", func() bool {
		v, _, _ := backlogGauge(reg)
		return v == 0 && h.access.UnloadResyncStats().Reloaded == entities
	})
	if got := h.access.UnloadResyncStats().Backlog; got != 0 {
		t.Fatalf("UnloadResyncStats().Backlog after draining = %d, want 0", got)
	}
}

// 停止时丢弃的排队不再计入积压。
func TestUnloadResyncBacklogIsZeroAfterStop(t *testing.T) {
	reg := metrics.NewRegistry()
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })

	h := newResyncHarness(t, &scriptedLoader{label: "authority", gate: make(chan struct{})}, entity.UnloadResyncConfig{Workers: 1})
	h.unload(h.resident(6110, "rejected", true))
	h.unload(h.resident(6111, "rejected", true))
	waitFor(t, "the reload to reach the loader", func() bool { return h.loader.inFlight.Load() == 1 })
	if v, _, _ := backlogGauge(reg); v != 2 {
		t.Fatalf("%s = %d before stop, want 2", backlogMetric, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := backlogGauge(reg); v != 0 {
		t.Fatalf("%s = %d after stop, want 0", backlogMetric, v)
	}
	if got := h.access.UnloadResyncStats().Backlog; got != 0 {
		t.Fatalf("UnloadResyncStats().Backlog after stop = %d, want 0", got)
	}
	h.unhookLoader()
}

// RR-20260928-01（OPEN-ITEMS B42）：一个进程可以有多个 ManagerAccess（多个 EntityManager，或测试 / 工具进程），
// 每个都有自己的卸载后重载接线。gauge 无标签、一个进程一条序列，承诺它是进程内全部接线积压之和：
// 两个接线积压 3 + 2 时为 5；停掉其中一个只撤回它自己的 2，另一个的 3 仍在；全部停掉后为 0。
// 修复前每个接线写自己的绝对值，gauge 是最后写入者的值（2），停掉第二个后被清零（0）而第一个仍有 3 个在等。
func TestUnloadResyncBacklogSumsAcrossManagerAccesses(t *testing.T) {
	reg := metrics.NewRegistry()
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(reg)
	t.Cleanup(func() { metrics.SetDefaultRegistry(previous) })

	// stuck 建一个接线：1 个 worker 停在 loader 的 gate 上，其余在队列里，积压 n。
	stuck := func(base int64, n int) (*resyncHarness, chan struct{}) {
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		h := newResyncHarness(t, &scriptedLoader{label: "authority", gate: gate}, entity.UnloadResyncConfig{Workers: 1, QueueCapacity: 8})
		values := make([]*resyncEntity, 0, n)
		for i := range n {
			value := h.resident(base+int64(i), "rejected", false)
			if err := h.sync.Subscribe(1, value.ID(), entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			values = append(values, value)
		}
		if err := h.sync.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		for len(h.frames) > 0 {
			<-h.frames
		}
		for _, value := range values {
			h.unload(value)
		}
		waitFor(t, "the reload to reach the loader", func() bool { return h.loader.inFlight.Load() == 1 })
		return h, gate
	}
	h1, _ := stuck(6300, 3)
	h2, _ := stuck(6400, 2)

	b1, b2 := h1.access.UnloadResyncStats().Backlog, h2.access.UnloadResyncStats().Backlog
	if b1 != 3 || b2 != 2 {
		t.Fatalf("fixture: backlogs %d + %d, want 3 + 2", b1, b2)
	}
	if v, labels, _ := backlogGauge(reg); v != int64(b1+b2) || labels != 0 {
		t.Fatalf("%s = %d (%d labels) with two ManagerAccess backlogs %d + %d; want their sum on one unlabelled series", backlogMetric, v, labels, b1, b2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h2.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := backlogGauge(reg); v != int64(h1.access.UnloadResyncStats().Backlog) || v != 3 {
		t.Fatalf("%s = %d after stopping the second ManagerAccess; the first still has %d entities waiting or reloading", backlogMetric, v, h1.access.UnloadResyncStats().Backlog)
	}
	if err := h1.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := backlogGauge(reg); v != 0 {
		t.Fatalf("%s = %d after stopping both, want 0", backlogMetric, v)
	}
}
