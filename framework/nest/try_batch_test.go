// 批量作业（DispatchTryBatch）：同 ID 顺序的两条硬约束（冲突转单目标、执行期间认领）、逐目标事务、时间片、Fence、
// 停机排空、业务池路由与交回原因。

package nest

import (
	"context"
	"errors"
	"maps"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// eventLog 记录各 handler 的执行顺序。
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// waitFor 轮询 cond 直到为真或 2 秒超时。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

// gatedLoadGetter 让对 gated 的第一次 I/O 阶段加载（不带 LoadedEntitiesOnly 的 Get）阻塞到 release，其余照常。
type gatedLoadGetter struct {
	entity.Getter
	gated   int64
	entered chan struct{}
	release chan struct{}
	blocked atomic.Bool
}

func (g *gatedLoadGetter) Get(ctx context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if id == g.gated && !entity.LoadedEntitiesOnly(ctx) && g.blocked.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.release
	}
	return g.Getter.Get(ctx, id, category)
}

func (g *gatedLoadGetter) GetMany(ctx context.Context, ids []int64, categories []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i, id := range ids {
		e, err := g.Get(ctx, id, categories[i])
		if err != nil {
			return nil, err
		}
		out[i] = e
	}
	return out, nil
}

// handBackRecorder 收集交回的目标与原因。
type handBackRecorder struct {
	mu   sync.Mutex
	got  map[int64]error
	seen int
}

func newHandBackRecorder() *handBackRecorder { return &handBackRecorder{got: map[int64]error{}} }

func (r *handBackRecorder) handBack(target TryBatchTarget, err error) {
	r.mu.Lock()
	r.got[target.ID] = err
	r.seen++
	r.mu.Unlock()
}

func (r *handBackRecorder) snapshot() (map[int64]error, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.got), r.seen
}

func startTryBatchEngine(t *testing.T, getter entity.Getter, short WorkerPoolConfig, opts ...NestOption) *NestMgr {
	t.Helper()
	opts = append([]NestOption{NestOptionWithGetter(getter), NestOptionWithWorkerPools(short, WorkerPoolConfig{Workers: 1, QueueCap: 8}, WorkerPoolConfig{Workers: 2, QueueCap: 8})}, opts...)
	engine := NewEngine(opts...)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	return engine
}

// 约束 1：业务消息已准入、尚未执行（排在同 ID 的 I/O 准备之后，不在执行、也不持实体锁）时，批量作业里的同 ID 目标
// 不在本批执行，而是当场转成单目标作业排在这条消息之后：执行顺序是 I/O 那条 → 业务消息 → 心跳，心跳没有被交回
// （不算跳拍），转单目标计数为 1。
// 变异核对：① 冲突判断只看实体锁（不看 tails）；② 冲突时直接执行而不排队（prepareTryBatchGroupLocked 对已有
// tails 的目标照样认领执行）——两者都让心跳插到业务消息之前，本用例变红。
func TestTryBatchConflictRunsAfterAdmittedSameIDJob(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	idA := mustBuildCastID(t, 8501, 1, nestLocalKind)
	idC := mustBuildCastID(t, 8502, 1, nestLocalKind)
	getter.Add(newMockEntity(idA, 1))
	getter.Add(newMockEntity(idC, 1))
	slow := &gatedLoadGetter{Getter: getter, gated: idA, entered: make(chan struct{}), release: make(chan struct{})}
	releaseIO := sync.OnceFunc(func() { close(slow.release) })
	var log eventLog
	record := func(tag string) func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			log.add(tag + ":" + map[bool]string{true: "A", false: "C"}[es[0].ID() == idA])
			return nil, nil
		}
	}
	MustRegisterMemoryHandler(NewHandlerName("tb_io"), record("io"))
	MustRegisterMemoryHandler(NewHandlerName("tb_biz"), record("biz"))
	MustRegisterMemoryHandler(NewHandlerName("tb_beat"), record("beat"))
	engine := startTryBatchEngine(t, slow, WorkerPoolConfig{Workers: 2, QueueCap: 16})
	t.Cleanup(releaseIO) // 先于引擎停机执行（Cleanup 后进先出）

	// 一条走 I/O 准备的消息占住 A 的 tails（I/O 阶段阻塞在加载上，不持实体锁）；业务消息排在它后面等前驱。
	if err := engine.Dispatch(context.Background(), NewHandlerName("tb_io"), idA, nil, SendOptionIO()); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, slow.entered)
	if err := engine.Dispatch(context.Background(), NewHandlerName("tb_biz"), idA, nil); err != nil {
		t.Fatal(err)
	}
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{
		{Handler: NewHandlerName("tb_beat"), ID: idA},
		{Handler: NewHandlerName("tb_beat"), ID: idC},
	}, hb.handBack); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "beat on C", func() bool { return slices.Contains(log.snapshot(), "beat:C") })
	if got := log.snapshot(); slices.Contains(got, "beat:A") {
		t.Fatalf("beat on A ran before the admitted same-ID jobs: %v", got)
	}
	if got := engine.Stats().Queue.Short.TryBatchConverted; got != 1 {
		t.Fatalf("converted %d, want 1", got)
	}
	releaseIO()
	waitFor(t, "beat on A", func() bool { return slices.Contains(log.snapshot(), "beat:A") })
	got := log.snapshot()
	if want := []string{"io:A", "biz:A", "beat:A"}; !slices.Equal(slices.DeleteFunc(slices.Clone(got), func(e string) bool { return e == "beat:C" }), want) {
		t.Fatalf("A order %v, want %v (all %v)", got, want, got)
	}
	if handedBack, n := hb.snapshot(); n != 0 {
		t.Fatalf("conflicting target was handed back (counted as a skipped beat): %v", handedBack)
	}
}

// 约束 2：心跳回调执行期间准入的同 ID 消息排在它后面——留在依赖队列里，没有 worker 取走它去阻塞等实体锁；心跳提交
// （AfterCommit 已执行）之后它才执行。
// 变异核对：不认领（prepareTryBatchGroupLocked 不写 tails）时业务消息直接就绪，第二个 worker 取走它阻塞在实体锁上，
// Running 变成 2、BlockedOnPredecessor 为 0，本用例变红。
func TestTryBatchClaimQueuesLaterSameIDJobBehindTheBeat(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	idA := mustBuildCastID(t, 8511, 1, nestLocalKind)
	getter.Add(newMockEntity(idA, 1))
	entered, gate := make(chan struct{}), make(chan struct{})
	openGate := sync.OnceFunc(func() { close(gate) })
	var log eventLog
	MustRegisterHandlerWithMeta(NewHandlerName("tb_claim_beat"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		AfterCommit(func() { log.add("beat committed") })
		close(entered)
		<-gate
		return nil, nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityMemory})
	MustRegisterMemoryHandler(NewHandlerName("tb_claim_biz"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		log.add("biz")
		return nil, nil
	})
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 2, QueueCap: 16})
	t.Cleanup(openGate) // 先于引擎停机执行（Cleanup 后进先出）
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: NewHandlerName("tb_claim_beat"), ID: idA}}, hb.handBack); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, entered)
	if err := engine.Dispatch(context.Background(), NewHandlerName("tb_claim_biz"), idA, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // 给第二个 worker 机会（变异下它会取走业务消息并阻塞在实体锁上）
	stats := engine.Stats().Queue.Short
	if stats.Running != 1 || stats.BlockedOnPredecessor != 1 {
		t.Fatalf("while the beat runs: running=%d blocked-on-predecessor=%d, want 1 and 1 (the later same-ID message must wait in the dependency queue, not on the entity lock)", stats.Running, stats.BlockedOnPredecessor)
	}
	openGate()
	waitFor(t, "biz", func() bool { return slices.Contains(log.snapshot(), "biz") })
	if got, want := log.snapshot(), []string{"beat committed", "biz"}; !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// 实体锁被 Nest 作业顺序之外的持有者占着（tails 为空，例如别的 handler 的 Cast）时，目标同样转成单目标作业，
// 不在批量作业的 worker 上等锁；持有者放锁后它与普通 Dispatch 一样执行。
func TestTryBatchLockBusyConvertsToSingleJob(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	idA := mustBuildCastID(t, 8521, 1, nestLocalKind)
	ent := newMockEntity(idA, 1)
	getter.Add(ent)
	var ran atomic.Int32
	MustRegisterMemoryHandler(NewHandlerName("tb_lock_beat"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		ran.Add(1)
		return nil, nil
	})
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 2, QueueCap: 16})
	ent.GetMutex().Lock()
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: NewHandlerName("tb_lock_beat"), ID: idA}}, hb.handBack); err != nil {
		ent.GetMutex().Unlock()
		t.Fatal(err)
	}
	waitFor(t, "conversion", func() bool { return engine.Stats().Queue.Short.TryBatchConverted == 1 })
	if ran.Load() != 0 {
		t.Fatal("beat ran while the entity lock was held")
	}
	ent.GetMutex().Unlock()
	waitFor(t, "converted beat", func() bool { return ran.Load() == 1 })
	if handedBack, n := hb.snapshot(); n != 0 {
		t.Fatalf("handed back %v", handedBack)
	}
}

// 冲突目标转单目标作业被准入拒绝（等待容量已满）时才交回、算跳拍：原因是 ErrQueueFull。
// 变异核对：convertTryBatchTargetLocked 不检查 admitLocked 的结果（当作转成功）时没有交回，本用例变红。
func TestTryBatchConversionRejectedIsHandedBack(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	idA := mustBuildCastID(t, 8531, 1, nestLocalKind)
	getter.Add(newMockEntity(idA, 1))
	entered, gate := make(chan struct{}), make(chan struct{})
	openGate := sync.OnceFunc(func() { close(gate) })
	MustRegisterMemoryHandler(NewHandlerName("tb_full_block"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		close(entered)
		<-gate
		return nil, nil
	})
	MustRegisterMemoryHandler(NewHandlerName("tb_full_noop"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return nil, nil })
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 2, QueueCap: 1})
	t.Cleanup(openGate) // 先于引擎停机执行（Cleanup 后进先出）
	if err := engine.Dispatch(context.Background(), NewHandlerName("tb_full_block"), idA, nil); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, entered)
	// 等待位只有 1 个：这条排在 A 后面的消息占满它。
	if err := engine.Dispatch(context.Background(), NewHandlerName("tb_full_noop"), idA, nil); err != nil {
		t.Fatal(err)
	}
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: NewHandlerName("tb_full_noop"), ID: idA}}, hb.handBack); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hand-back", func() bool { _, n := hb.snapshot(); return n == 1 })
	got, _ := hb.snapshot()
	if !errors.Is(got[idA], ErrQueueFull) {
		t.Fatalf("hand-back reason %v, want ErrQueueFull", got[idA])
	}
	if c := engine.Stats().Queue.Short.TryBatchConverted; c != 0 {
		t.Fatalf("converted %d, want 0", c)
	}
}

// 每个目标在自己的 Guard 作用域与事务里执行：一项回滚不影响其他项，AfterCommit 只对提交的项执行，同一实体的两个目标
// 按批内先后执行（第二个在第一个撤销认领后才认领），Sync 捕获随本目标的事务提交。
// 变异核对：各目标共用第一个目标的 Params（tryBatchJob.add 不切子切片）时，失败标志串到其他项，本用例变红。
func TestTryBatchKeepsPerTargetTransactionSemantics(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	id1, e1 := newAsyncPilotEntity(t, 8541, 10)
	id2, e2 := newAsyncPilotEntity(t, 8542, 10)
	id3, e3 := newAsyncPilotEntity(t, 8543, 10)
	m, frames, _ := syncTestManager(t, e1, entitysync.ModeOnChange)
	getter := newMockGetter()
	for _, e := range []*rollbackTestEntity{e1, e2, e3} {
		getter.Add(e)
	}
	var mu sync.Mutex
	committed := map[int64]int{}
	ran := map[int64][]int{}
	name := NewHandlerName("tb_tx")
	MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
		e := es[0].(*rollbackTestEntity)
		if !fctx.InShortWorker() || !fctx.InNestHandler() {
			return nil, errors.New("not on a short worker inside a Nest handler")
		}
		if scope := entity.CurrentGuardScope(); scope == nil || !scope.Guard().GuardedEntity(e) {
			return nil, errors.New("target is not held by the current Guard scope")
		}
		old := e.dao.Value
		if !RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil }) {
			return nil, errors.New("missing undo transaction")
		}
		e.dao.Value++
		e.MarkSyncDirty(1)
		id := e.ID()
		if !AfterCommit(func() { mu.Lock(); committed[id]++; mu.Unlock() }) {
			return nil, errors.New("missing rollback transaction")
		}
		mu.Lock()
		ran[id] = append(ran[id], params[1].(int))
		mu.Unlock()
		if params[0].(bool) {
			return nil, errors.New("boom")
		}
		return nil, nil
	}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityMemory})
	barrier := NewHandlerName("tb_tx_barrier")
	MustRegisterMemoryHandler(barrier, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return nil, nil })
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 4, QueueCap: 64}, NestOptionWithEntitySync(m))
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{
		{Handler: name, ID: id1, Params: Params{false, 0}},
		{Handler: name, ID: id2, Params: Params{true, 1}},
		{Handler: name, ID: id3, Params: Params{false, 2}},
		{Handler: name, ID: id1, Params: Params{false, 3}},
	}, hb.handBack); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "four targets", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ran[id1])+len(ran[id2])+len(ran[id3]) == 4
	})
	for _, id := range []int64{id1, id2, id3} {
		if _, err := engine.Request(context.Background(), barrier, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if e1.dao.Value != 12 || e2.dao.Value != 10 || e3.dao.Value != 11 {
		t.Fatalf("values e1=%d e2=%d e3=%d, want 12 10 11 (e2 rolled back alone)", e1.dao.Value, e2.dao.Value, e3.dao.Value)
	}
	mu.Lock()
	gotCommitted, gotRan := maps.Clone(committed), maps.Clone(ran)
	mu.Unlock()
	if want := map[int64]int{id1: 2, id3: 1}; !maps.Equal(gotCommitted, want) {
		t.Fatalf("after-commit %v, want %v", gotCommitted, want)
	}
	if !slices.Equal(gotRan[id1], []int{0, 3}) {
		t.Fatalf("same-ID order for e1 %v, want [0 3]", gotRan[id1])
	}
	if handedBack, n := hb.snapshot(); n != 0 {
		t.Fatalf("handed back %v", handedBack)
	}
	select {
	case raw := <-frames:
		got := syncValue(t, raw)
		for len(frames) > 0 {
			got = syncValue(t, <-frames)
		}
		if got != 12 {
			t.Fatalf("sync value %d, want 12", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sync change of a batch target was not captured")
	}
}

// 批量作业的组大小与时间片是按压测选的缺省（心跳缺省走批量作业，维护者 2026-10-22 决定 B）：它们决定一个 worker
// 一次被批量作业占住多久，即普通业务消息的 p99（docs/performance/NEST-BATCH-HEARTBEAT-20261021.md §3A、§3B、§3F）。
// 改它们要先补同一套夹具的对照并更新报告，再改这里。
// 变异核对：tryBatchGroup 改回 8 时本用例变红。
func TestTryBatchParametersAreTheMeasuredDefaults(t *testing.T) {
	if tryBatchGroup != 4 || tryBatchSlice != 200*time.Microsecond {
		t.Fatalf("try-batch group %d slice %v, want group 4 slice 200µs (the measured default; re-measure before changing)", tryBatchGroup, tryBatchSlice)
	}
}

// 时间片：回调慢时批量作业用完时间片就让出 worker，排在它后面准入的普通消息不必等整批跑完。
// 变异核对：不检查时间片（tryBatchSlice 改成一小时）时普通消息排在全部 20 个目标之后，本用例变红。
func TestTryBatchYieldsWorkerAfterTimeSlice(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	var targets []TryBatchTarget
	for i := range 20 {
		id := mustBuildCastID(t, int64(8550+i), 1, nestLocalKind)
		getter.Add(newMockEntity(id, 1))
		targets = append(targets, TryBatchTarget{Handler: NewHandlerName("tb_slice_beat"), ID: id})
	}
	other := mustBuildCastID(t, 8580, 1, nestLocalKind)
	getter.Add(newMockEntity(other, 1))
	var log eventLog
	MustRegisterMemoryHandler(NewHandlerName("tb_slice_beat"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		for start := time.Now(); time.Since(start) < 100*time.Microsecond; {
		}
		log.add("beat")
		return nil, nil
	})
	MustRegisterMemoryHandler(NewHandlerName("tb_slice_biz"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		log.add("biz")
		return nil, nil
	})
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 1, QueueCap: 16})
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), targets, hb.handBack); err != nil {
		t.Fatal(err)
	}
	if err := engine.Dispatch(context.Background(), NewHandlerName("tb_slice_biz"), other, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "all events", func() bool { return len(log.snapshot()) == 21 })
	if pos := slices.Index(log.snapshot(), "biz"); pos >= 10 {
		t.Fatalf("ordinary message ran at position %d behind the batch backlog (want < 10 with a %v slice and 100µs callbacks)", pos, tryBatchSlice)
	}
}

// Fence：已排队的批量作业在引擎 fence 后不再执行 handler，每个目标交回 ErrNestFenced；fence 之后的 DispatchTryBatch
// 直接返回错误。
// 变异核对：prepareTryBatchGroupLocked 与 tryBatchExec.run（checkHandlerStart）都不检查 fence 时 handler 照常执行，本用例
// 变红；只去掉其中一处时另一处仍拦下（组内 fence 见下一个用例）。
func TestTryBatchRefusesTargetsAfterFence(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	idA := mustBuildCastID(t, 8591, 1, nestLocalKind)
	idB := mustBuildCastID(t, 8592, 1, nestLocalKind)
	getter.Add(newMockEntity(idA, 1))
	getter.Add(newMockEntity(idB, 1))
	entered, gate := make(chan struct{}), make(chan struct{})
	openGate := sync.OnceFunc(func() { close(gate) })
	var ran atomic.Int32
	MustRegisterMemoryHandler(NewHandlerName("tb_fence_block"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		close(entered)
		<-gate
		return nil, nil
	})
	MustRegisterMemoryHandler(NewHandlerName("tb_fence_beat"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		ran.Add(1)
		return nil, nil
	})
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 1, QueueCap: 16})
	t.Cleanup(openGate) // 先于引擎停机执行（Cleanup 后进先出）
	if err := engine.Dispatch(context.Background(), NewHandlerName("tb_fence_block"), idA, nil); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, entered)
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: NewHandlerName("tb_fence_beat"), ID: idB}}, hb.handBack); err != nil {
		t.Fatal(err)
	}
	engine.Fence(errors.New("test fence"))
	openGate()
	waitFor(t, "hand-back", func() bool { _, n := hb.snapshot(); return n == 1 })
	got, _ := hb.snapshot()
	if !errors.Is(got[idB], ErrNestFenced) || ran.Load() != 0 {
		t.Fatalf("after fence: hand-back %v, handler ran %d times; want ErrNestFenced and 0", got[idB], ran.Load())
	}
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: NewHandlerName("tb_fence_beat"), ID: idB}}, hb.handBack); !errors.Is(err, ErrNestFenced) {
		t.Fatalf("DispatchTryBatch after fence: %v, want ErrNestFenced", err)
	}
}

// Fence 发生在一组执行中途（RV-20261022-01）：已经开始的 handler 收尾，同一批里还没开始的目标——同组其余的与之后
// 各组的——一律不执行业务，各自交回 ErrNestFenced 恰好一次。两种触发：handler 主动调用 NestMgr.Fence；真实 committer
// 返回结果未知（invokeTryBatchTransaction 按 ErrCommitIndeterminate fence 引擎）。6 个目标跨两组（组大小 4）：后 3 个
// 在发生 fence 的那一组里（只能靠逐目标的开始检查拦下），最后 2 个在下一组准备时拦下。
// 变异核对：tryBatchExec.run 不调用 checkHandlerStart（只剩准备组时读一次 fence）时同组后 3 个目标照常执行，本用例变红。
func TestTryBatchStopsUnstartedTargetsAfterFenceInsideGroup(t *testing.T) {
	for i, tc := range []struct {
		name      string
		meta      HandlerMeta
		committer TransactionCommitter
		fence     bool // handler 自己调用 Fence；否则由提交结果未知触发
	}{
		{name: "handler_fence", fence: true},
		{name: "commit_indeterminate", meta: HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, committer: &indeterminateCommitter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ResetHandlersForTest()
			t.Cleanup(ResetHandlersForTest)
			getter := newMockGetter()
			ids := make([]int64, 6)
			var targets []TryBatchTarget
			name := NewHandlerName("tb_fence_mid_group")
			for k := range ids {
				id, e := newAsyncPilotEntity(t, int64(8661+10*i+k), 1)
				ids[k] = id
				getter.Add(e)
				targets = append(targets, TryBatchTarget{Handler: name, ID: id})
			}
			var engine *NestMgr
			var mu sync.Mutex
			ran := map[int64]int{}
			MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				e := es[0].(*rollbackTestEntity)
				mu.Lock()
				ran[e.ID()]++
				mu.Unlock()
				if tc.fence {
					engine.Fence(errors.New("test: persistence result unknown"))
					return nil, nil
				}
				old := e.dao.Value
				RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
				e.dao.Value++
				return nil, MarkPersist(e.dao, 1)
			}, tc.meta)
			var opts []NestOption
			if tc.committer != nil {
				opts = append(opts, NestOptionWithTransactionCommitter(tc.committer))
			}
			engine = startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 1, QueueCap: 16}, opts...)
			hb := newHandBackRecorder()
			if err := engine.DispatchTryBatch(context.Background(), targets, hb.handBack); err != nil {
				t.Fatal(err)
			}
			// Shutdown 等批量作业连同交回全部结束（Fence 不阻止已准入作业的排空）。
			if err := engine.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(engine.FenceError(), ErrNestFenced) {
				t.Fatalf("engine not fenced: %v", engine.FenceError())
			}
			mu.Lock()
			gotRan := maps.Clone(ran)
			mu.Unlock()
			got, seen := hb.snapshot()
			var ranAfter []int64
			for _, id := range ids[1:] {
				if gotRan[id] != 0 {
					ranAfter = append(ranAfter, id)
				}
				if !errors.Is(got[id], ErrNestFenced) {
					t.Errorf("target %d hand-back %v, want ErrNestFenced", id, got[id])
				}
			}
			if gotRan[ids[0]] != 1 || len(ranAfter) != 0 || seen != len(ids)-1 {
				t.Fatalf("first target ran %d (want 1); unstarted targets that still ran after the fence %d %v (want 0); hand-backs %d (want %d exactly once each)",
					gotRan[ids[0]], len(ranAfter), ranAfter, seen, len(ids)-1)
			}
		})
	}
}

// 同组里先执行、取不到实体锁（busy）的目标，本来在收尾时转成单目标作业；若同组后面的目标 fence 了引擎，转成的作业
// 只会被开始检查拒绝、不执行，所以收尾时直接交回 ErrNestFenced（不转、不计转单目标）。
// 变异核对：convertTryBatchTargetLocked 不检查 fence 时目标被转走、没有交回，本用例变红。
func TestTryBatchBusyTargetIsHandedBackAfterFenceInsideGroup(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	busyID := mustBuildCastID(t, 8681, 1, nestLocalKind)
	fenceID := mustBuildCastID(t, 8682, 1, nestLocalKind)
	busy := newMockEntity(busyID, 1)
	getter.Add(busy)
	getter.Add(newMockEntity(fenceID, 1))
	var engine *NestMgr
	var ran atomic.Int32
	name := NewHandlerName("tb_fence_busy")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		ran.Add(1)
		if es[0].ID() == fenceID {
			engine.Fence(errors.New("test: persistence result unknown"))
		}
		return nil, nil
	})
	engine = startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 1, QueueCap: 16})
	busy.GetMutex().Lock() // Nest 作业顺序之外的持有者：批量作业 Try 取不到
	unlock := sync.OnceFunc(func() { busy.GetMutex().Unlock() })
	t.Cleanup(unlock)
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: name, ID: busyID}, {Handler: name, ID: fenceID}}, hb.handBack); err != nil {
		t.Fatal(err)
	}
	// 等这一组收尾（交回或转走）再停机：停机中的转单目标会被准入以 ErrNestStopped 拒绝，不能让停机替代 fence 的判定。
	waitFor(t, "group finished", func() bool {
		_, n := hb.snapshot()
		return n > 0 || engine.Stats().Queue.Short.TryBatchConverted > 0
	})
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	unlock()
	got, seen := hb.snapshot()
	if !errors.Is(got[busyID], ErrNestFenced) || seen != 1 || ran.Load() != 1 {
		t.Fatalf("busy target hand-back %v (hand-backs %d), handler ran %d; want ErrNestFenced once and only the fencing target ran (converted %d)",
			got[busyID], seen, ran.Load(), engine.Stats().Queue.Short.TryBatchConverted)
	}
}

// 停机排空：已准入的批量作业在停机时照常执行完，Shutdown 等它；停机后 DispatchTryBatch 返回 ErrNestStopped。
func TestTryBatchDrainsOnShutdown(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	idA := mustBuildCastID(t, 8601, 1, nestLocalKind)
	idB := mustBuildCastID(t, 8602, 1, nestLocalKind)
	getter.Add(newMockEntity(idA, 1))
	getter.Add(newMockEntity(idB, 1))
	entered, gate := make(chan struct{}), make(chan struct{})
	var ran atomic.Int32
	MustRegisterMemoryHandler(NewHandlerName("tb_drain_block"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		close(entered)
		<-gate
		return nil, nil
	})
	MustRegisterMemoryHandler(NewHandlerName("tb_drain_beat"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		ran.Add(1)
		return nil, nil
	})
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	if err := engine.Dispatch(context.Background(), NewHandlerName("tb_drain_block"), idA, nil); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, entered)
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: NewHandlerName("tb_drain_beat"), ID: idB}}, hb.handBack); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Shutdown(context.Background()) }()
	time.Sleep(10 * time.Millisecond)
	close(gate)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not drain")
	}
	if ran.Load() != 1 {
		t.Fatalf("admitted batch target ran %d times during drain, want 1", ran.Load())
	}
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: NewHandlerName("tb_drain_beat"), ID: idB}}, hb.handBack); !errors.Is(err, ErrNestStopped) {
		t.Fatalf("DispatchTryBatch after shutdown: %v, want ErrNestStopped", err)
	}
}

// 准入前即可判定的交回在调用方同步完成，每个目标恰好一次：非法 ID、空 handler 名 → ErrInvalidMessage；Remote 托管
// → ErrTryBatchUnsupportedTarget。执行期：不在内存 → ErrEntityNotFound（不冷加载）；handler 不存在 → ErrHandlerNotFound。
// 慢池实体在 long 通道执行。批量作业里 Await 被拒绝。
func TestTryBatchHandBackReasonsAndPoolRouting(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	short := mustBuildCastID(t, 8611, 1, nestLocalKind)
	long := mustBuildCastID(t, 8612, 1, awaitLongKind)
	missing := mustBuildCastID(t, 8613, 1, nestLocalKind)
	remote := mustBuildCastID(t, 8614, entity.EntityCategoryRemote, nestRemoteManagedKind)
	getter.Add(newMockEntity(short, 1))
	getter.Add(newMockEntity(long, 1))
	pools := make(chan string, 4)
	MustRegisterMemoryHandler(NewHandlerName("tb_route"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		pool := "short"
		if fctx.InLongWorker() {
			pool = "long"
		}
		awaitErr := Await(func(context.Context) (int, error) { return 0, nil }, func(entity.IThreadSafeEntity, int, error) error { return nil })
		pools <- map[bool]string{true: "long-entity", false: "short-entity"}[es[0].ID() == long] + " on " + pool + " await=" + map[bool]string{true: "rejected", false: "allowed"}[errors.Is(awaitErr, ErrAwaitContext)]
		return nil, nil
	})
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 1, QueueCap: 16})
	hb := newHandBackRecorder()
	err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{
		{Handler: NewHandlerName("tb_route"), ID: short},
		{Handler: NewHandlerName("tb_route"), ID: long},
		{Handler: NewHandlerName("tb_route"), ID: -1},
		{Handler: NewHandlerName(""), ID: short},
		{Handler: NewHandlerName("tb_route"), ID: remote},
		{Handler: NewHandlerName("tb_route"), ID: missing},
		{Handler: NewHandlerName("tb_unknown"), ID: short},
	}, hb.handBack)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{<-pools, <-pools}
	slices.Sort(got)
	if want := []string{"long-entity on long await=rejected", "short-entity on short await=rejected"}; !slices.Equal(got, want) {
		t.Fatalf("pools %v, want %v", got, want)
	}
	waitFor(t, "hand-backs", func() bool { _, n := hb.snapshot(); return n == 5 })
	reasons, _ := hb.snapshot()
	for id, want := range map[int64]error{-1: ErrInvalidMessage, remote: ErrTryBatchUnsupportedTarget, missing: ErrEntityNotFound} {
		if !errors.Is(reasons[id], want) {
			t.Fatalf("hand-back for %d: %v, want %v (all %v)", id, reasons[id], want, reasons)
		}
	}
	// 空 handler 名与未注册 handler 都落在 short 上：两次交回（同 ID 后一次覆盖前一次）。
	if !errors.Is(reasons[short], ErrHandlerNotFound) {
		t.Fatalf("hand-back for unknown handler: %v, want ErrHandlerNotFound", reasons[short])
	}
}

// 公平：一格心跳一次准入的批量作业积压不会让之后准入的普通消息等整格跑完，普通消息积压也不会饿死批量作业——两类就绪
// 作业交替取。
// 变异核对：批量作业与普通作业进同一条 FIFO（enqueueReady 不分 tryBatches）时，普通消息排在 10 个批量作业之后，
// 本用例变红。
func TestTryBatchAlternatesWithOrdinaryJobs(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	ids := make([]int64, 24)
	for i := range ids {
		ids[i] = mustBuildCastID(t, int64(8620+i), 1, nestLocalKind)
		getter.Add(newMockEntity(ids[i], 1))
	}
	entered, gate := make(chan struct{}), make(chan struct{})
	var log eventLog
	MustRegisterMemoryHandler(NewHandlerName("tb_fair_block"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		close(entered)
		<-gate
		return nil, nil
	})
	MustRegisterMemoryHandler(NewHandlerName("tb_fair_beat"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		log.add("beat")
		return nil, nil
	})
	MustRegisterMemoryHandler(NewHandlerName("tb_fair_biz"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		log.add("biz")
		return nil, nil
	})
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 1, QueueCap: 64})
	openGate := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(func() { openGate() }) // 调用当时的 openGate：每轮换新的 gate
	run := func(admit func()) []string {
		t.Helper()
		entered = make(chan struct{})
		gate = make(chan struct{})
		openGate = sync.OnceFunc(func() { close(gate) })
		if err := engine.Dispatch(context.Background(), NewHandlerName("tb_fair_block"), ids[0], nil); err != nil {
			t.Fatal(err)
		}
		stagedSignal(t, entered)
		log.mu.Lock()
		log.events = nil
		log.mu.Unlock()
		admit()
		openGate()
		waitFor(t, "11 events", func() bool { return len(log.snapshot()) == 11 })
		return log.snapshot()
	}
	hb := newHandBackRecorder()
	beats := func(from, to int) {
		for i := from; i < to; i++ {
			if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{{Handler: NewHandlerName("tb_fair_beat"), ID: ids[i]}}, hb.handBack); err != nil {
				t.Fatal(err)
			}
		}
	}
	bizs := func(from, to int) {
		for i := from; i < to; i++ {
			if err := engine.Dispatch(context.Background(), NewHandlerName("tb_fair_biz"), ids[i], nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 10 个批量作业积压后准入的普通消息，至多排在一个批量作业之后。
	if got := run(func() { beats(1, 11); bizs(11, 12) }); slices.Index(got, "biz") > 1 {
		t.Fatalf("ordinary message waited behind the batch backlog: %v", got)
	}
	// 10 条普通消息积压后准入的批量作业，至多排在一条普通消息之后。
	if got := run(func() { bizs(12, 22); beats(22, 23) }); slices.Index(got, "beat") > 1 {
		t.Fatalf("batch job starved behind the ordinary backlog: %v", got)
	}
}

// loadedOnlyRecorder 记录 Get 是否带 LoadedEntitiesOnly（批量作业只读已加载的实体，不冷加载）。
type loadedOnlyRecorder struct {
	entity.Getter
	coldCalls atomic.Int32
}

func (g *loadedOnlyRecorder) Get(ctx context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if !entity.LoadedEntitiesOnly(ctx) {
		g.coldCalls.Add(1)
	}
	return g.Getter.Get(ctx, id, category)
}

// 卸载窗口：批量作业只经 LoadedEntitiesOnly 读目标；目标不在内存（卸载后没有重载）时交回 ErrEntityNotFound，不冷加载、
// 不执行 handler。
// 强制点是 entity.LoadedEntitiesOnly 认业务 worker 身份（批量作业在业务池 worker 上执行）；执行器另包的 loadedGetter
// 只是防御，去掉它本用例仍绿（变异已核对），本用例是契约检查。
func TestTryBatchReadsOnlyLoadedTargets(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	base := newMockGetter()
	loaded := mustBuildCastID(t, 8651, 1, nestLocalKind)
	unloaded := mustBuildCastID(t, 8652, 1, nestLocalKind)
	base.Add(newMockEntity(loaded, 1))
	getter := &loadedOnlyRecorder{Getter: base}
	var ran atomic.Int32
	MustRegisterMemoryHandler(NewHandlerName("tb_loaded_beat"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		ran.Add(1)
		return nil, nil
	})
	engine := startTryBatchEngine(t, getter, WorkerPoolConfig{Workers: 1, QueueCap: 16})
	hb := newHandBackRecorder()
	if err := engine.DispatchTryBatch(context.Background(), []TryBatchTarget{
		{Handler: NewHandlerName("tb_loaded_beat"), ID: loaded},
		{Handler: NewHandlerName("tb_loaded_beat"), ID: unloaded},
	}, hb.handBack); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hand-back", func() bool { _, n := hb.snapshot(); return n == 1 })
	waitFor(t, "loaded target", func() bool { return ran.Load() == 1 })
	got, _ := hb.snapshot()
	if !errors.Is(got[unloaded], ErrEntityNotFound) {
		t.Fatalf("unloaded target hand-back %v, want ErrEntityNotFound", got[unloaded])
	}
	if n := getter.coldCalls.Load(); n != 0 {
		t.Fatalf("%d Get calls without LoadedEntitiesOnly: a try-batch must not cold-load", n)
	}
}

// BenchmarkTryBatch：批量作业的执行开销，每 op 一个目标。64 个目标一批、空内存 handler、short 池 1 个 worker、4 批在途；
// 计时包含最终排空。改批量执行路径（runTryBatchLocked、tryBatchExec.run、逐目标开始检查）的函数体时与改前同机交替对比
// （维护手册：-count 至少 10，benchstat）。
//
// 命令：go test -run '^$' -bench 'BenchmarkTryBatch$' -benchtime 2000000x -count 10 ./framework/nest
func BenchmarkTryBatch(b *testing.B) {
	entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
	const batch, window = 64, 4
	getter := newMockGetter()
	name := NewHandlerName("bench_try_batch")
	targets := make([][]TryBatchTarget, window)
	for slot := range targets {
		for i := range batch {
			id, err := entity.BuildEntityID(int64(200000+slot*batch+i), nestLocalKind)
			if err != nil {
				b.Fatal(err)
			}
			getter.Add(newMockEntity(id, entity.EntityCategory(1)))
			targets[slot] = append(targets[slot], TryBatchTarget{Handler: name, ID: id, Params: Params{slot}})
		}
	}
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 1024}, WorkerPoolConfig{}, WorkerPoolConfig{}), NestOptionWithStageMetrics(false))
	slots := make(chan int, window)
	for slot := range window {
		slots <- slot
	}
	var remaining [window]atomic.Int64
	var completed atomic.Int64
	engine.MustRegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
		completed.Add(1)
		if slot := params[0].(int); remaining[slot].Add(-1) == 0 {
			slots <- slot
		}
		return nil, nil
	}, HandlerMeta{})
	if err := engine.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	ctx := context.Background()
	handBack := func(target TryBatchTarget, err error) { b.Errorf("target %d handed back: %v", target.ID, err) }
	b.ReportAllocs()
	b.ResetTimer()
	for sent := 0; sent < b.N; {
		slot := <-slots
		n := min(batch, b.N-sent)
		remaining[slot].Store(int64(n))
		if err := engine.DispatchTryBatch(ctx, targets[slot][:n], handBack); err != nil {
			b.Fatal(err)
		}
		sent += n
	}
	if err := engine.Shutdown(ctx); err != nil {
		b.Fatal(err)
	}
	b.StopTimer()
	if completed.Load() != int64(b.N) {
		b.Fatalf("completed=%d want=%d", completed.Load(), b.N)
	}
}
