package nest

// RR-20261006-18：派发器的 nest.dispatch.*{dispatcher} 序列在派发器销毁后一直留在注册表里，停在最后的值
// （第十二轮 O3）。承诺：派发器排空停止（OnDestroyWithContext 返回 nil）后删掉带它 dispatcher 标签的全部
// 序列；创建再销毁 N 次序列数不增长，/metrics 里不再出现已销毁的派发器；同名派发器还有活着的
// （生产里 NestMgr 都叫 "nest"）时不删，留给最后一个；停机超时（返回 ctx 错误）时不删，重试排空后再删。
// nest.dispatch.slow_reroute.total 只在冷目标改道时产生，不经 observeStats，单独用一次真实改道验证（补测）。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

const seriesLifecycleDispatcherPrefix = "rr18-series-"

// seriesByDispatcher counts the series carrying a dispatcher label this test
// owns, by dispatcher name.
func seriesByDispatcher() map[string]int {
	out := map[string]int{}
	for _, metric := range metrics.Snapshot() {
		if name := metric.Labels["dispatcher"]; strings.HasPrefix(name, seriesLifecycleDispatcherPrefix) {
			out[name]++
		}
	}
	return out
}

func startObservedDispatcher(t *testing.T, name string) *Dispatcher {
	t.Helper()
	d := NewDispatcher(name, 1, 8, func(*Msg) {})
	d.OnInit()
	d.OnRun()
	d.observeStats()
	return d
}

func TestDestroyedDispatchersLeaveNoSeries(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	const rounds = 20
	counts := make([]int, 0, rounds)
	for i := range rounds {
		name := fmt.Sprintf("%s%d", seriesLifecycleDispatcherPrefix, i)
		d := startObservedDispatcher(t, name)
		if got := seriesByDispatcher()[name]; got == 0 {
			t.Fatalf("round %d: the live dispatcher %s reported no series", i, name)
		}
		if err := d.OnDestroyWithContext(context.Background()); err != nil {
			t.Fatalf("round %d: destroy: %v", i, err)
		}
		live := seriesByDispatcher()
		total := 0
		for _, n := range live {
			total += n
		}
		counts = append(counts, total)
	}
	for _, n := range counts {
		if n != 0 {
			t.Fatalf("series of destroyed dispatchers after each create/destroy round: %v; want 0 every round", counts)
		}
	}
	if text := string(metrics.PrometheusText(metrics.Snapshot())); strings.Contains(text, seriesLifecycleDispatcherPrefix) {
		t.Fatalf("/metrics still shows destroyed dispatchers:\n%s", text)
	}
}

func TestADispatcherSharingItsNameKeepsTheSeries(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	name := seriesLifecycleDispatcherPrefix + "shared"
	first := startObservedDispatcher(t, name)
	second := startObservedDispatcher(t, name)
	if err := first.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seriesByDispatcher()[name] == 0 {
		t.Fatalf("destroying one of two live dispatchers named %s deleted the series the other still reports", name)
	}
	if err := second.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := seriesByDispatcher()[name]; got != 0 {
		t.Fatalf("the last dispatcher named %s is destroyed and %d series remain", name, got)
	}
}

func TestADispatcherThatDidNotDrainKeepsItsSeriesUntilItDoes(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	name := seriesLifecycleDispatcherPrefix + "stuck"
	release := make(chan struct{})
	started := make(chan struct{})
	d := NewDispatcher(name, 1, 8, func(*Msg) {
		close(started)
		<-release
	})
	d.OnInit()
	d.OnRun()
	if err := d.TrySendMsg(GenMsg(MsgTypeSingle)); err != nil {
		t.Fatal(err)
	}
	<-started
	d.observeStats()

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.OnDestroyWithContext(expired); err == nil {
		t.Fatal("destroy returned nil while a handler was still running")
	}
	if seriesByDispatcher()[name] == 0 {
		t.Fatal("a dispatcher whose stop timed out deleted its series; its workers are still running")
	}
	close(release)
	if err := d.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatalf("retried destroy: %v", err)
	}
	if got := seriesByDispatcher()[name]; got != 0 {
		t.Fatalf("the drained dispatcher still has %d series", got)
	}
}

// slowRerouteSeries is the nest.dispatch.slow_reroute.total value of the named
// dispatcher, and whether the series exists at all.
func slowRerouteSeries(name string) (int64, bool) {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "nest.dispatch.slow_reroute.total" && metric.Labels["dispatcher"] == name {
			return metric.Value, true
		}
	}
	return 0, false
}

// The reroute counter is written by the dispatch queue when a fast-lane job
// finds its declared target cold before taking the Guard, not by the periodic
// observeStats, so the tests above never create it. This one makes a real
// reroute through a NestMgr (the same eviction window as
// TestDeclaredTargetEvictedAfterAdmissionMovesToSlowKeepingOrder), sees the
// series appear, shuts the engine down and requires the series gone.
func TestTheSlowRerouteSeriesGoesWithItsDispatcher(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	mgr, manager, loader := newColdTargetEngine(t, WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8})
	// NestMgr's dispatcher is always "nest"; a name of its own keeps another
	// test's engine from holding the series this test expects deleted.
	name := seriesLifecycleDispatcherPrefix + "reroute"
	mgr.dispatcher.Name = name
	blockerID := mustBuildCastID(t, 8421, entity.EntityCategory(1), nestLocalKind)
	targetID := mustBuildCastID(t, 8422, entity.EntityCategory(1), nestLocalKind)
	target := newMockEntityWithKind(targetID, entity.EntityCategory(1), nestLocalKind)
	for _, e := range []entity.IThreadSafeEntity{newMockEntityWithKind(blockerID, entity.EntityCategory(1), nestLocalKind), target} {
		if err := manager.TryAdd(e); err != nil {
			t.Fatal(err)
		}
	}
	entered, gate := make(chan struct{}), make(chan struct{})
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("block"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		close(entered)
		<-gate
		return "released", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("touch"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}

	send := func(handler string, id int64) <-chan any {
		t.Helper()
		msg, ch := GenSyncMsg(MsgTypeSingle)
		msg.Name, msg.Tid = handler, id
		if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
			t.Fatal(err)
		}
		return ch
	}
	// The one fast worker is held, so the request is admitted to the fast
	// lane while its target is loaded; evicting it before the worker frees up
	// makes the first fast run find it cold and hand the job to the slow lane.
	blocked := send("block", blockerID)
	stagedSignal(t, entered)
	rerouted := send("touch", targetID)
	if err := manager.Destroy(context.Background(), target, entity.DestroyReasonCommon, false); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if got := stagedWait(t, blocked); got != "released" {
		t.Fatalf("blocker: %v", got)
	}
	if got := stagedWait(t, rerouted); got != "ok" {
		t.Fatalf("request to the evicted target: %v (loads=%d)", got, loader.calls.Load())
	}
	if value, ok := slowRerouteSeries(name); !ok || value != 1 {
		t.Fatalf("nest.dispatch.slow_reroute.total{dispatcher=%q} = %d (present=%v) after one reroute; want 1", name, value, ok)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if value, ok := slowRerouteSeries(name); ok {
		t.Fatalf("the dispatcher %s is shut down and nest.dispatch.slow_reroute.total still reports %d", name, value)
	}
	if text := string(metrics.PrometheusText(metrics.Snapshot())); strings.Contains(text, name) {
		t.Fatalf("/metrics still shows the destroyed dispatcher:\n%s", text)
	}
}
