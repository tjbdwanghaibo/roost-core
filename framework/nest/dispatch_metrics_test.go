// 派发观测：队列 / 延迟 gauge、处理计数、慢日志门限与限流、锁持有时长、trace 事件、指标序列生命周期与阶段耗时。

package nest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	flog "github.com/tjbdwanghaibo/roost-core/infra/observe/log"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// 派发器的 nest.dispatch.*{dispatcher} 序列随派发器排空停止（OnDestroyWithContext 返回 nil）一起删除：创建再销毁 N 次
// 序列数不增长；同名派发器还有活着的时不删，留给最后一个；停机超时时不删，重试排空后再删。
// nest.dispatch.io_reroute.total 只在冷目标改道时产生，不经 observeStats，单独用一次真实改道验证。
// 历史：RR-20261006-18。

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

// ioRerouteSeries is the nest.dispatch.io_reroute.total value of the named
// dispatcher, and whether the series exists at all.
func ioRerouteSeries(name string) (int64, bool) {
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "nest.dispatch.io_reroute.total" && metric.Labels["dispatcher"] == name {
			return metric.Value, true
		}
	}
	return 0, false
}

// The reroute counter is written by the dispatch queue when a fast-lane job
// finds its declared target cold before taking the Guard, not by the periodic
// observeStats, so the tests above never create it. This one makes a real
// reroute through a NestMgr (the same eviction window as
// TestDeclaredTargetEvictedAfterAdmissionMovesToIOKeepingOrder), sees the
// series appear, shuts the engine down and requires the series gone.
func TestTheIORerouteSeriesGoesWithItsDispatcher(t *testing.T) {
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
	// The one business worker is held, so the request is admitted to the business
	// lane while its target is loaded; evicting it before the worker frees up
	// makes the first business-pool run find it cold and hand the job to the io lane.
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
	if value, ok := ioRerouteSeries(name); !ok || value != 1 {
		t.Fatalf("nest.dispatch.io_reroute.total{dispatcher=%q} = %d (present=%v) after one reroute; want 1", name, value, ok)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if value, ok := ioRerouteSeries(name); ok {
		t.Fatalf("the dispatcher %s is shut down and nest.dispatch.io_reroute.total still reports %d", name, value)
	}
	if text := string(metrics.PrometheusText(metrics.Snapshot())); strings.Contains(text, name) {
		t.Fatalf("/metrics still shows the destroyed dispatcher:\n%s", text)
	}
}

func TestNestStageMetricsFollowExecutionPaths(t *testing.T) {
	for _, mode := range []string{"disabled", "memory", "strict", "pipeline", "async", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			metrics.DefaultRegistry().Reset()
			t.Cleanup(func() { metrics.DefaultRegistry().Reset() })
			id, e := newAsyncPilotEntity(t, 9990, 10)
			getter := newMockGetter()
			getter.Add(e)
			opts := []NestOption{NestOptionWithGetter(getter), NestOptionWithTransactionCommitter(newPipelinedTestCommitter(true))}
			if mode != "disabled" {
				opts = append(opts, NestOptionWithStageMetrics(true))
			}
			if mode == "async" {
				opts = append(opts, NestOptionWithPipelinedAsyncCompletion(1, 4))
			}
			engine := NewEngine(opts...)
			name := NewHandlerName("stages_" + mode)
			meta := HandlerMeta{Durability: DurabilityMemory}
			if mode == "strict" {
				meta = HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}
			}
			if mode == "pipeline" || mode == "async" {
				meta = HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}
			}
			if mode == "rollback" {
				meta.Rollback = RollbackState
			}
			failure := errors.New("business rejected")
			engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				if mode == "rollback" {
					return nil, failure
				}
				if meta.Durability != DurabilityMemory {
					return nil, MarkPersist(e.dao, 1)
				}
				return nil, nil
			}, meta)
			if err := engine.Start(); err != nil {
				t.Fatal(err)
			}
			defer engine.Shutdown(context.Background())
			_, err := engine.Request(context.Background(), name, id, nil)
			if mode == "rollback" {
				if !errors.Is(err, failure) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := engine.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, metric := range metrics.Snapshot() {
				if metric.Name == "nest.stage.duration" && metric.Labels["handler"] == name.String() {
					seen[metric.Labels["stage"]] = true
				}
			}
			if mode == "disabled" {
				if len(seen) != 0 {
					t.Fatalf("default emitted stages: %v", seen)
				}
				return
			}
			want := []string{"queue", "load", "lock", "handler", "release", "cleanup"}
			if mode == "rollback" {
				want = append(want, "capture", "rollback")
				if seen["admission"] {
					t.Error("rejected transaction recorded successful admission")
				}
			} else {
				want = append(want, "admission")
			}
			if mode == "strict" {
				want = append(want, "capture", "prepare", "durable_commit")
			}
			if mode == "pipeline" || mode == "async" {
				want = append(want, "capture", "prepare", "enqueue", "durable_wait")
			}
			if mode == "async" {
				want = append(want, "commit_queue", "completion_queue", "completion_unlock", "completion_order", "completion")
			}
			for _, stage := range want {
				if !seen[stage] {
					t.Errorf("missing stage %q: %v", stage, seen)
				}
			}
		})
	}
}

func TestTickRegistrationInsideCallbackUsesNextSnapshot(t *testing.T) {
	resetTickCallbacksForTest()
	t.Cleanup(resetTickCallbacksForTest)
	var calls []string
	var once sync.Once
	MustRegisterTickCallback(NewTickCallbackName("register"), func(TickMsg) {
		calls = append(calls, "first")
		once.Do(func() {
			MustRegisterTickCallback(NewTickCallbackName("later"), func(TickMsg) { calls = append(calls, "later") })
		})
	})
	MustRegisterTickCallback(NewTickCallbackName("peer"), func(TickMsg) { calls = append(calls, "peer") })
	ticker := NewTicker(time.Hour)
	ticker.doTick()
	if !slices.Equal(calls, []string{"first", "peer"}) {
		t.Fatal(calls)
	}
	ticker.doTick()
	if !slices.Equal(calls, []string{"first", "peer", "first", "peer", "later"}) {
		t.Fatal(calls)
	}
}

func TestSlowDispatchWatchReuseAfterTimerExpiry(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				watch := startSlowDispatchTraceWatch(&Msg{Name: "watch-reuse"}, time.Now())
				// 只验证过期/Stop 竞争的所有权交接，避免为压力回归打印数千份堆栈。
				watch.traced.Store(true)
				watch.timer.Reset(0)
				watch.stop(0, "ok")
			}
		})
	}
	wg.Wait()
}

func TestSlowTraceGateBoundsConcurrentDiagnostics(t *testing.T) {
	var gate slowTraceGate
	now := time.Now()
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 128 {
		wg.Go(func() {
			if ok, _ := gate.take(now); ok {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if got := accepted.Load(); got != 1 {
		t.Fatalf("detailed logs for one burst=%d", got)
	}
	if ok, _ := gate.take(now.Add(nestSlowDispatchLogInterval - time.Nanosecond)); ok {
		t.Fatal("window reopened early")
	}
	if ok, skipped := gate.take(now.Add(nestSlowDispatchLogInterval)); !ok || skipped != 128 {
		t.Fatalf("next window allowed=%v suppressed=%d", ok, skipped)
	}
	if ok, skipped := gate.take(now.Add(2 * nestSlowDispatchLogInterval)); !ok || skipped != 0 {
		t.Fatalf("idle next window allowed=%v suppressed=%d", ok, skipped)
	}
}

func TestDispatcherObserveStatsRecordsQueueGauge(t *testing.T) {
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	dispatcher := NewDispatcher("nest", 2, 64, func(*Msg) {})
	dispatcher.OnInit()
	defer dispatcher.OnDestroy()

	dispatcher.delaySendMsg(time.Hour, GenMsg(MsgTypeSingle))
	dispatcher.observeStats()

	wantPools := map[string]bool{"short": false, "io": false}
	delayedSeen := false
	for _, metric := range metrics.Snapshot() {
		if metric.Name == "nest.dispatch.delayed_messages" &&
			metric.Labels["dispatcher"] == "nest" &&
			metric.Value == 1 {
			delayedSeen = true
		}
		if metric.Name != "nest.dispatch.queue_len" {
			continue
		}
		if metric.Labels["dispatcher"] != "nest" {
			continue
		}
		pool := metric.Labels["pool"]
		if _, ok := wantPools[pool]; ok && metric.Value == 0 {
			wantPools[pool] = true
		}
	}
	for pool, seen := range wantPools {
		if !seen {
			t.Fatalf("missing queue gauge for pool %s", pool)
		}
	}
	if !delayedSeen {
		t.Fatalf("missing delayed gauge in metrics: %+v", metrics.Snapshot())
	}
}

func TestDispatcherStatsCountsProcessedAndSlowMessages(t *testing.T) {
	dispatcher := NewDispatcher("nest", 2, 64, func(*Msg) {})
	mgr := &NestMgr{dispatcher: dispatcher}

	mgr.recordDispatch(199 * time.Millisecond)
	mgr.recordDispatch(200 * time.Millisecond)
	mgr.recordDispatch(350 * time.Millisecond)

	stats := mgr.Stats()
	if stats.Work.ProcessedMessages != 3 {
		t.Fatalf("processed messages = %d, want 3", stats.Work.ProcessedMessages)
	}
	if stats.Work.Slow200msMessages != 2 {
		t.Fatalf("slow messages = %d, want 2", stats.Work.Slow200msMessages)
	}
}

func TestShouldLogSlowDispatchUsesThreshold(t *testing.T) {
	if shouldLogSlowDispatch(nestSlowDispatchThreshold - time.Nanosecond) {
		t.Fatal("duration below threshold should not be slow")
	}
	if !shouldLogSlowDispatch(nestSlowDispatchThreshold) {
		t.Fatal("duration at threshold should be slow")
	}
}

func TestShouldTraceSlowDispatchUsesSlowThreshold(t *testing.T) {
	if shouldTraceSlowDispatch(nestSlowDispatchTraceThreshold) {
		t.Fatal("duration at threshold should not dump slow dispatch trace")
	}
	if !shouldTraceSlowDispatch(nestSlowDispatchTraceThreshold + time.Nanosecond) {
		t.Fatal("duration over threshold should dump slow dispatch trace")
	}
}

func TestNestDispatchLogsSlowTraceWithMsgInfoWithoutGlobalStack(t *testing.T) {
	slowDispatchLogGate.mu.Lock()
	slowDispatchLogGate.next = time.Time{}
	slowDispatchLogGate.suppressed = 0
	slowDispatchLogGate.mu.Unlock()
	var buf bytes.Buffer
	if err := flog.Init(flog.Options{
		Level:          slog.LevelWarn,
		Output:         &buf,
		DisableGoID:    true,
		DisableFrame:   true,
		DisableContext: true,
	}); err != nil {
		t.Fatalf("init log: %v", err)
	}
	t.Cleanup(func() {
		_ = flog.Init(flog.Options{Level: slog.LevelInfo, Output: io.Discard})
	})

	getter := newMockGetter()
	id := mustBuildCastID(t, 1001, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	mgr := &NestMgr{getter: getter}
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	MustRegisterMemoryHandler(NewHandlerName("auto_heartbeat"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		time.Sleep(nestSlowDispatchTraceThreshold + 20*time.Millisecond)
		return nil, nil
	})
	msg := &Msg{
		Name:      "auto_heartbeat",
		Type:      MsgTypeSingle,
		Tid:       id,
		Tids:      []int64{id, 456},
		GroupTIds: [][]int64{{1, 2}, {3}},
		Params:    []any{"tick", 7},
		RetChan:   make(chan any, 1),
		RefCount:  2,
		Cost:      true,
		HasRemote: false,
	}
	NestDispatch(mgr, msg)

	out := buf.String()
	if strings.Contains(out, "stack=") || strings.Contains(out, "stack_truncated=") {
		t.Fatal("automatic slow diagnostics must not dump global goroutine stacks")
	}
	for _, want := range []string{
		"slow dispatch trace",
		"handler=auto_heartbeat",
		"type=Single",
		"msg_info=",
		"param_types",
		"string",
		"int",
		"has_ret_chan",
		"groups",
		"tids",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("slow dispatch trace log missing %q:\n%s", want, out)
		}
	}
}

func TestNestTracePropagatesContextAndRecordsEvents(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	metrics.DefaultRegistry().Reset()
	t.Cleanup(func() { metrics.DefaultRegistry().Reset() })

	getter := newMockGetter()
	id := mustBuildCastID(t, 103, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	name := NewHandlerName("test_nest_trace_context")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, param []any, opts ...HandlerOption) (any, error) {
		c := fctx.CurrentContext()
		if c == nil {
			return nil, errors.New("handler has no request context")
		}
		if !c.Trace.Active() {
			return nil, errors.New("handler trace is not active")
		}
		if c.Trace.TraceID != "trace-nest-test" {
			return nil, errors.New("handler got wrong trace id")
		}
		if c.Trace.Tags["player_id"] != "777" {
			return nil, errors.New("handler got wrong trace tags")
		}
		return "ok", nil
	})

	_, release := fctx.NewContext(
		fctx.WithPlayerProtocol(777, 13003, 9),
		fctx.WithTrace(fctx.TraceMeta{
			TraceID: "trace-nest-test",
			Enabled: true,
			Reason:  "test",
			Tags: map[string]string{
				"player_id": "777",
			},
		}),
	)
	defer release()

	ret, err := Nest.Request(context.Background(), name, id, nil)
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if ret != "ok" {
		t.Fatalf("ret = %v, want ok", ret)
	}
	// The worker answers RetChan before it records dispatch_done, so the
	// reply can race the last counter: wait for it instead of reading once.
	for _, event := range []string{"enqueue", "dispatch_start", "dispatch_done"} {
		deadline := time.Now().Add(2 * time.Second)
		for !hasNestTraceCounter(event, name.String(), "ok") {
			if time.Now().After(deadline) {
				t.Fatalf("missing nest trace event %q in metrics: %+v", event, metrics.Snapshot())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func hasNestTraceCounter(event string, handler string, result string) bool {
	for _, metric := range metrics.Snapshot() {
		if metric.Name != "nest.trace.events.total" {
			continue
		}
		if metric.Labels["event"] == event &&
			metric.Labels["handler"] == handler &&
			metric.Labels["result"] == result &&
			metric.Value > 0 {
			return true
		}
	}
	return false
}

func TestDispatchRecordsLockHoldAndFlagsSlowHandlers(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 320, entity.EntityCategory(1), nestLocalKind)
	getter.Add(&rollbackTestEntity{
		EntityBase: entity.NewEntityBase(id, entity.EntityCategory(1), false, nestLocalKind),
		dao:        &rollbackTestDao{id: id},
	})
	registry := metrics.NewRegistry()
	previous := metrics.DefaultRegistry()
	metrics.SetDefaultRegistry(registry)
	defer metrics.SetDefaultRegistry(previous)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
		NestOptionWithSlowLockThreshold(time.Millisecond),
	)
	defer StopNest()
	MustRegisterHandlerWithMeta(NewHandlerName("test_lock_hold_slow"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		time.Sleep(3 * time.Millisecond)
		return nil, nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityMemory})
	if _, err := Nest.Request(context.Background(), NewHandlerName("test_lock_hold_slow"), id, nil); err != nil {
		t.Fatal(err)
	}
	holdSeen, slowSeen := false, false
	for _, metric := range registry.Snapshot() {
		switch metric.Name {
		case "nest.handler.lock_hold":
			holdSeen = holdSeen || metric.Labels["handler"] == "test_lock_hold_slow"
		case "nest.handler.lock_hold.slow.total":
			slowSeen = slowSeen || (metric.Labels["handler"] == "test_lock_hold_slow" && metric.Value >= 1)
		}
	}
	if !holdSeen || !slowSeen {
		t.Fatalf("lock hold metrics missing: hold=%v slow=%v (%+v)", holdSeen, slowSeen, registry.Snapshot())
	}
}
