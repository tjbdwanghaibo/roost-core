// 调度队列：短 / 长 / I/O 三池按全部目标排序且不占业务池 worker，前驱合流分流，准入预算与统计，目标存储所有权。

package nest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
)

func queueMessage(ids ...int64) *Msg {
	m := GenMsg(MsgTypeMulti)
	m.Tids = ids
	m.OnSend()
	return m
}
func admitQueue(t *testing.T, q *dispatchQueue, slow bool, ids ...int64) {
	t.Helper()
	m := queueMessage(ids...)
	if err := q.admit(m, slow); err != nil {
		m.OnRelease()
		t.Fatal(err)
	}
}
func TestTwoPoolsOrderAllTargetsWithoutOccupyingFastWorker(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "multi"}[multi], func(t *testing.T) {
			entered, release, independent := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var overtook atomic.Bool
			var after atomic.Bool
			q := newDispatchQueue("test", WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 4}, func(m *Msg) {
				if m.Key() == 99 {
					close(independent)
				} else {
					if !after.Load() {
						overtook.Store(true)
					}
				}
			}, func(*Msg) { close(entered); <-release; after.Store(true) })
			q.start()
			defer q.stop(context.Background())
			defer close(release)
			ids := []int64{1}
			target := int64(1)
			if multi {
				ids = []int64{1, 2}
				target = 2
			}
			admitQueue(t, q, true, ids...)
			stagedSignal(t, entered)
			admitQueue(t, q, false, target)
			admitQueue(t, q, false, 99)
			stagedSignal(t, independent)
			if overtook.Load() {
				t.Fatal("later target ran before slow predecessor")
			}
		})
	}
}

// 两个前驱各占交错的 ID；合流消息必须等两个都完成，随后才能放行两个分支。
func TestQueueSharedPredecessorsJoinAndSplit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b, join := make(chan struct{}), make(chan struct{}), make(chan struct{})
		closeA := sync.OnceFunc(func() { close(a) })
		closeB := sync.OnceFunc(func() { close(b) })
		closeJoin := sync.OnceFunc(func() { close(join) })
		entered := make(chan struct{}, 2)
		joined := make(chan struct{}, 1)
		finished := make(chan struct{}, 2)
		q := newDispatchQueue("join_split", WorkerPoolConfig{2, 8}, WorkerPoolConfig{2, 8}, func(msg *Msg) {
			if len(msg.Tids) > 1 {
				joined <- struct{}{}
				<-join
			} else {
				finished <- struct{}{}
			}
		}, func(msg *Msg) {
			entered <- struct{}{}
			if msg.Key() == 1 {
				<-a
			} else {
				<-b
			}
		})
		q.start()
		defer q.stop(context.Background())
		defer closeA()
		defer closeB()
		defer closeJoin()
		admitQueue(t, q, true, 1, 3)
		admitQueue(t, q, true, 2, 4)
		<-entered
		<-entered
		admitQueue(t, q, false, 4, 3, 2, 1, 3)
		admitQueue(t, q, false, 1)
		admitQueue(t, q, false, 4)
		closeA()
		synctest.Wait()
		if len(joined) != 0 || len(finished) != 0 {
			t.Fatal("ran before all predecessors completed")
		}
		closeB()
		synctest.Wait()
		if len(joined) != 1 || len(finished) != 0 {
			t.Fatal("join/split ordering violated")
		}
		closeJoin()
		synctest.Wait()
		if len(finished) != 2 {
			t.Fatal("successors did not drain")
		}
	})
}
func TestIOPoolUsesSharedWorkersAndBoundedWaiting(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	q := newDispatchQueue("test", WorkerPoolConfig{1, 2}, WorkerPoolConfig{2, 1}, func(*Msg) {}, func(*Msg) { entered <- struct{}{}; <-release })
	q.start()
	defer q.stop(context.Background())
	defer close(release)
	admitQueue(t, q, true, 1)
	stagedSignal(t, entered)
	// 旧哈希池这两个 key 可以碰槽；共享I/O 池由任意空闲 worker 取得。
	id := int64(2)
	for hashKey(id)%2 != hashKey(1)%2 {
		id++
	}
	admitQueue(t, q, true, id)
	stagedSignal(t, entered)
	admitQueue(t, q, true, 1)
	extra := queueMessage(9)
	if err := q.admit(extra, true); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("queue limit=%v", err)
	}
	extra.OnRelease()
	fast, slow, _ := q.stats()
	if fast.WorkerNum != 1 || slow.WorkerNum != 2 || slow.QueueLen != 1 || slow.QueueCap != 1 {
		t.Fatalf("stats=%+v %+v", fast, slow)
	}
}

type slowTestGetter struct {
	entity.Getter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *slowTestGetter) Get(ctx context.Context, id int64, category entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	g.once.Do(func() { close(g.entered); <-g.release })
	return g.Getter.Get(ctx, id, category)
}
func TestSlowLoadAndHandlerUseDifferentWorkers(t *testing.T) {
	getter := newMockGetter()
	id := mustBuildCastID(t, 9910, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	slow := &slowTestGetter{Getter: getter, entered: make(chan struct{}), release: make(chan struct{})}
	mgr := NewEngine(NestOptionWithGetter(slow), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{2, 4}))
	var handlerRan atomic.Bool
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("load"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		handlerRan.Store(true)
		if entity.CurrentGuardScope() == nil {
			t.Error("handler missing fast guard")
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	defer func() {
		select {
		case <-slow.release:
		default:
			close(slow.release)
		}
	}()
	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name = "load"
	msg.Tid = id
	msg.Cost = true
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	stagedSignal(t, slow.entered)
	// 加载阻塞时业务池能执行内部工作；它不等待 Entity 本地锁。
	progress := make(chan struct{})
	fastRelease := make(chan struct{})
	defer func() {
		select {
		case <-fastRelease:
		default:
			close(fastRelease)
		}
	}()
	env := GenMsg(MsgTypeSingle)
	env.remoteLogic = &remoteLogicCall{msg: msg, done: make(chan struct{}), fn: func() { close(progress); <-fastRelease }}
	env.OnSend()
	mgr.dispatcher.queue.continueBusiness(env)
	stagedSignal(t, progress)
	close(slow.release)
	deadline := time.After(2 * time.Second)
	for mgr.Stats().ShortContinuations == 0 {
		select {
		case <-deadline:
			t.Fatal("I/O-prepared handler did not enter the business queue")
		default:
			runtime.Gosched()
		}
	}
	if handlerRan.Load() {
		t.Fatal("io worker executed handler while business worker occupied")
	}
	close(fastRelease)
	if got := stagedWait(t, ch); got != "ok" {
		t.Fatal(got)
	}
}

// BenchmarkMixedFastSlow 仅测调度：1%/5% 前置 I/O 用 500µs 等待模拟，
// I/O 阶段交回同一业务池执行。它不代表 Mongo/NATS 的端到端 TPS。
func BenchmarkMixedFastSlow(b *testing.B) {
	for _, percent := range []uint64{1, 5} {
		b.Run(fmt.Sprintf("slow_%d_percent", percent), func(b *testing.B) {
			var q *dispatchQueue
			q = newDispatchQueue("mixed", WorkerPoolConfig{4, 4096}, WorkerPoolConfig{32, 4096}, func(m *Msg) {
				if m.remoteLogic != nil {
					return
				}
				m.RetChan <- struct{}{}
			}, func(m *Msg) {
				time.Sleep(500 * time.Microsecond)
				call := &remoteLogicCall{done: make(chan struct{})}
				envelope := GenMsg(MsgTypeSingle)
				envelope.remoteLogic = call
				envelope.OnSend()
				q.continueBusiness(envelope)
				<-call.done
				m.RetChan <- struct{}{}
			})
			q.start()
			defer q.stop(context.Background())
			var sequence atomic.Uint64
			b.SetParallelism(32)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					n := sequence.Add(1)
					msg, ch := GenSyncMsg(MsgTypeSingle)
					msg.Tid = int64(n%1000 + 1)
					msg.OnSend()
					if err := q.admit(msg, n%100 < percent); err != nil {
						msg.OnRelease()
						b.Error(err)
						return
					}
					<-ch
				}
			})
		})
	}
}

func TestQueueStatsSeparateDependencyAndWorkerWait(t *testing.T) {
	fastEntered, slowEntered, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	q := newDispatchQueue("observed", WorkerPoolConfig{1, 2}, WorkerPoolConfig{1, 2}, func(m *Msg) {
		if m.Key() == 1 {
			close(fastEntered)
			<-release
		}
	}, func(*Msg) { close(slowEntered); <-release })
	q.start()
	defer q.stop(context.Background())
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	admitQueue(t, q, false, 1)
	stagedSignal(t, fastEntered)
	admitQueue(t, q, true, 2)
	stagedSignal(t, slowEntered)
	admitQueue(t, q, false, 2) // 等 I/O 前驱；不能占 business worker。
	admitQueue(t, q, false, 3) // 无 ID 冲突，仅等 worker。
	rejected := queueMessage(4)
	if err := q.admit(rejected, false); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("rejection: %v", err)
	}
	rejected.OnRelease()
	// 直接建立可测量的排队时长，不依赖 Windows 的真实时钟分辨率。
	q.mu.Lock()
	for job := q.lanes[dispatchShortLane].waiting.head; job != nil; job = job.waitingNext {
		job.admittedAt = time.Since(q.clockOrigin) - time.Second
		if job.predecessors == 0 {
			job.readyAt = job.admittedAt
		}
	}
	q.mu.Unlock()
	_, _, _, details := q.snapshotStats()
	got := details.Short
	if got.Running != 1 || got.Ready != 1 || got.BlockedOnPredecessor != 1 || got.WaitingForWorker != 1 || got.PeakWaiting != 2 || got.Rejected != 1 || got.OldestWaiting <= 0 {
		t.Fatalf("waiting stats: %+v", details)
	}
	if got.DependencyWait != 0 {
		t.Fatalf("independent admission counted as dependency: %v", got.DependencyWait)
	}
	unblock()
	if err := q.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, _, details = q.snapshotStats()
	got = details.Short
	if got.Running+got.Ready+got.BlockedOnPredecessor+got.WaitingForWorker != 0 || got.OldestWaiting != 0 || got.Started != 3 || got.DependencyWait <= 0 || got.WorkerWait <= 0 {
		t.Fatalf("settled stats: %+v", details)
	}
}

func TestFastLogicMarksGetterContextAndSlowPreparationCanLoad(t *testing.T) {
	base := newMockGetter()
	id := mustBuildCastID(t, 9950, entity.EntityCategory(1), nestLocalKind)
	base.Add(newMockEntity(id, entity.EntityCategory(1)))
	getter := &policyTestGetter{Getter: base}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{}, WorkerPoolConfig{1, 8}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("policy"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		// preparedGetter 缓存以外的动态目标也必须继承业务阶段约束。
		_, err := mgr.dispatchGetter().Get(nestBaseContext(), id+1, entity.EntityCategory(1))
		if !errors.Is(err, entity.ErrColdLoadInLogic) {
			return nil, fmt.Errorf("dynamic miss: %v", err)
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackNone, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	for _, slow := range []bool{false, true} {
		msg, ch := GenSyncMsg(MsgTypeSingle)
		msg.Name, msg.Tid, msg.Cost = "policy", id, slow
		if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
			t.Fatal(err)
		}
		if got := stagedWait(t, ch); got != "ok" {
			t.Fatalf("slow=%v: %v", slow, got)
		}
	}
	if getter.fast.Load() == 0 || getter.slow.Load() == 0 {
		t.Fatalf("fast=%d slow=%d", getter.fast.Load(), getter.slow.Load())
	}
}

type policyTestGetter struct {
	entity.Getter
	fast, slow atomic.Int32
}

func (g *policyTestGetter) Get(ctx context.Context, id int64, cat entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if entity.LoadedEntitiesOnly(ctx) {
		g.fast.Add(1)
	} else {
		g.slow.Add(1)
	}
	value, err := g.Getter.Get(ctx, id, cat)
	if value == nil && entity.LoadedEntitiesOnly(ctx) {
		return nil, entity.ErrColdLoadInLogic
	}
	return value, err
}

// 一个续行占满业务池时，外部准入与 QueueLen 必须看见同一个真实等待位。
func TestQueueStatsAndAdmissionCountRunningContinuation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	q := newDispatchQueue("continuation", WorkerPoolConfig{1, 1}, WorkerPoolConfig{1, 1}, func(m *Msg) {
		if m.Key() == 1 {
			close(entered)
			<-release
		}
	}, nil)
	q.start()
	defer q.stop(context.Background())
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	q.continueBusiness(queueMessage(1))
	stagedSignal(t, entered)
	admitQueue(t, q, false, 2)
	rejected := queueMessage(3)
	if err := q.admit(rejected, false); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("admission=%v", err)
	}
	rejected.OnRelease()
	fast, _, _, stats := q.snapshotStats()
	if fast.QueueLen != 1 || stats.Short.WaitingForWorker != 1 || stats.Short.PeakWaiting != 1 || stats.ContinuationRunning != 1 {
		t.Fatalf("pool=%+v stats=%+v", fast, stats)
	}
	unblock()
	if err := q.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fast, _, pending, stats := q.snapshotStats()
	if fast.QueueLen != 0 || pending != 0 || stats.ContinuationRunning != 0 || stats.Short.WaitingForWorker != 0 {
		t.Fatalf("not drained: %+v %+v", fast, stats)
	}
}

// 目标副本必须在消息切片后续被改写时仍保持稳定，否则完成时会释放错误的ID链。
func TestDispatchJobOwnsTargetIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  Msg
		want []int64
	}{
		{"empty", Msg{}, []int64{0}},
		{"single", Msg{Tid: 7}, []int64{7}},
		{"single slice", Msg{Tids: []int64{7}}, []int64{7}},
		{"multiple", Msg{Tid: 3, Tids: []int64{9, 3}, GroupTIds: [][]int64{{7, 9}, {1}}}, []int64{1, 3, 7, 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := slices.Clone(tc.msg.Tids)
			job := newDispatchJob(&tc.msg, false)
			if !slices.Equal(job.ids, tc.want) || !slices.Equal(tc.msg.Tids, original) {
				t.Fatalf("ids=%v, message=%v", job.ids, tc.msg.Tids)
			}
			if len(tc.msg.GroupTIds) > 0 && !slices.Equal(tc.msg.GroupTIds[0], []int64{7, 9}) {
				t.Fatal("input group changed")
			}
			for i := range tc.msg.Tids {
				tc.msg.Tids[i] = 99
			}
			for _, group := range tc.msg.GroupTIds {
				for i := range group {
					group[i] = 99
				}
			}
			tc.msg.Tid = 99
			if !slices.Equal(job.ids, tc.want) {
				t.Fatalf("job aliases message targets: %v", job.ids)
			}
		})
	}
}

// workers 尚未被 Go 调度时，准入仍应预留其执行额度。这里仅启用准入，
// 不启动消费 goroutine，确定性复现突发抵达先于 worker 运行的时序。
func TestWorkerBudgetAdmitsBurstBeforeWorkersRun(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(map[bool]string{false: "fast", true: "slow"}[slow], func(t *testing.T) {
			q := newDispatchQueue("burst", WorkerPoolConfig{1024, 16}, WorkerPoolConfig{1024, 16}, nil, nil)
			q.started = true
			var accepted []*Msg
			defer func() {
				for _, msg := range accepted {
					msg.OnRelease()
				}
			}()
			for i := range 1040 {
				msg := queueMessage(mustBuildCastID(t, int64(i+1), 1, nestLocalKind))
				if err := q.admit(msg, slow); err != nil {
					msg.OnRelease()
					t.Fatalf("burst %d rejected with unused worker capacity: %v", i+1, err)
				}
				accepted = append(accepted, msg)
			}
			extra := queueMessage(2000)
			defer extra.OnRelease()
			if err := q.admit(extra, slow); !errors.Is(err, worker.ErrWorkerQueueFull) {
				t.Fatalf("overload=%v", err)
			}
			fastStats, slowStats, _ := q.stats()
			stats := fastStats
			if slow {
				stats = slowStats
			}
			if stats.QueueLen != 16 {
				t.Fatalf("waiting=%d want 16", stats.QueueLen)
			}
		})
	}
}

func TestWorkerBudgetDoesNotReserveWorkersForBlockedIDs(t *testing.T) {
	q := newDispatchQueue("ordered", WorkerPoolConfig{1, 16}, WorkerPoolConfig{1024, 16}, nil, nil)
	q.started = true
	var accepted []*Msg
	defer func() {
		for _, msg := range accepted {
			msg.OnRelease()
		}
	}()
	for range 17 {
		msg := queueMessage(1)
		if err := q.admit(msg, true); err != nil {
			msg.OnRelease()
			t.Fatal(err)
		}
		accepted = append(accepted, msg)
	}
	blocked := queueMessage(1)
	defer blocked.OnRelease()
	if err := q.admit(blocked, true); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("blocked ID bypassed waiting budget: %v", err)
	}
	// 热 ID 等待已满，独立 ID 仍可使用其余 worker 额度。
	independent := queueMessage(2)
	if err := q.admit(independent, true); err != nil {
		independent.OnRelease()
		t.Fatalf("unused worker rejected independent ID: %v", err)
	}
	accepted = append(accepted, independent)
}

func TestWorkerBudget1024WorkersDrainWithSmallQueue(t *testing.T) {
	entered := make(chan struct{}, 1024)
	release := make(chan struct{})
	q := newDispatchQueue("drain", WorkerPoolConfig{2, 16}, WorkerPoolConfig{1024, 16}, nil, func(*Msg) { entered <- struct{}{}; <-release })
	q.start()
	defer q.stop(context.Background())
	defer close(release)
	for i := range 1024 {
		admitQueue(t, q, true, int64(i+1))
	}
	for range 1024 {
		stagedSignal(t, entered)
	}
	for range 16 {
		admitQueue(t, q, true, 1)
	}
	_, stats, _ := q.stats()
	if stats.QueueLen != 16 {
		t.Fatalf("waiting=%d", stats.QueueLen)
	}
	extra := queueMessage(1)
	if err := q.admit(extra, true); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("limit=%v", err)
	}
	extra.OnRelease()
}

func TestClientTargetStorageOwnership(t *testing.T) {
	a := mustBuildCastID(t, 8301, entity.EntityCategory(1), nestLocalKind)
	b := mustBuildCastID(t, 8302, entity.EntityCategory(1), nestLocalKind)
	input := [][]int64{{b, a, b}, {a}, {}}
	want := [][]int64{{b, a, b}, {a}, {}}
	groups, err := normalizeClientGroups(input)
	if err != nil || !reflect.DeepEqual(groups, want) {
		t.Fatalf("ordered groups: %v, %v", groups, err)
	}
	input[0][0] = a
	input[1] = []int64{b}
	if !reflect.DeepEqual(groups, want) {
		t.Fatal("targets alias caller storage")
	}
	// 对一组 append 不得覆盖下一组，即使这些组共用底层数组。
	extended := append(groups[0], b)
	if len(extended) != 4 || !reflect.DeepEqual(groups, want) {
		t.Fatal("append crossed group boundary")
	}
	ids, err := normalizeClientIDs(want[0])
	if err != nil {
		t.Fatal(err)
	}
	want[0][0] = a
	if ids[0] != b || ids[2] != b {
		t.Fatal("flat targets lost order or caller isolation")
	}
	_, err = normalizeClientGroups([][]int64{{a}, {b, 0}})
	if !errors.Is(err, ErrInvalidMessage) || !strings.Contains(err.Error(), "entity group 1: entity index 1:") {
		t.Fatalf("invalid target location: %v", err)
	}
}

// 固定目标总数分布，测量标准化和存储分配，不代表 Nest 调度吞吐。
func BenchmarkClientGroupTargets(b *testing.B) {
	// 与同包 Client 基准复用同一 Kind/category，不新增注册号。
	entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
	id, err := entity.BuildEntityID(8300, nestLocalKind)
	if err != nil {
		b.Fatal(err)
	}
	for _, n := range []int{1, 8, 32} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			groups := make([][]int64, n)
			for i := range groups {
				groups[i] = []int64{id, id, id, id}
			}
			b.ReportAllocs()
			for b.Loop() {
				result, err := normalizeClientGroups(groups)
				if err != nil || len(result) != n || result[n-1][3] != id {
					b.Fatal(result, err)
				}
			}
			b.ReportMetric(float64(unsafe.Sizeof(Msg{})), "Msg-B")
		})
	}
}

// 业务目标有顺序和重复项，不能使用排序去重的 tail 集合代替。
func TestDispatchGroupedTargetsKeepOrderAndBoundaries(t *testing.T) {
	getter := newMockGetter()
	a := mustBuildCastID(t, 8321, 1, nestLocalKind)
	b := mustBuildCastID(t, 8322, 1, nestLocalKind)
	for _, id := range []int64{a, b} {
		getter.Add(newMockEntity(id, 1))
	}
	mgr := NewEngine(NestOptionWithGetter(getter))
	name := NewHandlerName("grouped_order")
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, opts ...HandlerOption) (any, error) {
		ids := make([]int64, len(es))
		for i, e := range es {
			ids[i] = e.GUId()
		}
		var option HandlerOptionParam
		for _, apply := range opts {
			apply(&option)
		}
		if !reflect.DeepEqual(ids, []int64{b, a, b, a}) || !reflect.DeepEqual(option.GroupLen, []int{3, 1}) {
			return nil, fmt.Errorf("ids=%v groups=%v", ids, option.GroupLen)
		}
		// Handler 可以替换参数切片，不能影响框架释放原实体引用。
		es[0] = nil
		return nil, nil
	}, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	if _, err := mgr.RequestMultiGroup(context.Background(), name, [][]int64{{b, a, b}, {a}}, nil); err != nil {
		t.Fatal(err)
	}
}
