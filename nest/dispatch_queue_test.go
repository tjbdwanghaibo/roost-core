package nest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/worker"
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
func TestSlowPoolUsesSharedWorkersAndBoundedWaiting(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	q := newDispatchQueue("test", WorkerPoolConfig{1, 2}, WorkerPoolConfig{2, 1}, func(*Msg) {}, func(*Msg) { entered <- struct{}{}; <-release })
	q.start()
	defer q.stop(context.Background())
	defer close(release)
	admitQueue(t, q, true, 1)
	stagedSignal(t, entered)
	// 旧哈希池这两个 key 可以碰槽；共享慢池由任意空闲 worker 取得。
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
	mgr := NewEngine(NestOptionWithGetter(slow), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 4}))
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
	// 加载阻塞时快池能执行内部工作；它不等待 Entity 本地锁。
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
	mgr.dispatcher.queue.continueFast(env)
	stagedSignal(t, progress)
	close(slow.release)
	deadline := time.After(2 * time.Second)
	for mgr.Stats().FastContinuations == 0 {
		select {
		case <-deadline:
			t.Fatal("slow handler did not enter fast queue")
		default:
			runtime.Gosched()
		}
	}
	if handlerRan.Load() {
		t.Fatal("slow worker executed handler while fast worker occupied")
	}
	close(fastRelease)
	if got := stagedWait(t, ch); got != "ok" {
		t.Fatal(got)
	}
}

// BenchmarkMixedFastSlow 仅测调度：1%/5% 前置 I/O 用 500µs 等待模拟，
// 慢阶段交回同一快池执行。它不代表 Mongo/NATS 的端到端 TPS。
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
				q.continueFast(envelope)
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
	admitQueue(t, q, false, 2) // 等慢前驱；不能占 fast worker。
	admitQueue(t, q, false, 3) // 无 ID 冲突，仅等 worker。
	rejected := queueMessage(4)
	if err := q.admit(rejected, false); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("rejection: %v", err)
	}
	rejected.OnRelease()
	// 直接建立可测量的排队时长，不依赖 Windows 的真实时钟分辨率。
	q.mu.Lock()
	for job := q.waiting[0].head; job != nil; job = job.waitingNext {
		job.admittedAt = time.Now().Add(-time.Second)
		if !job.readyAt.IsZero() {
			job.readyAt = job.admittedAt
		}
	}
	q.mu.Unlock()
	_, _, _, details := q.snapshotStats()
	got := details.Fast
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
	got = details.Fast
	if got.Running+got.Ready+got.BlockedOnPredecessor+got.WaitingForWorker != 0 || got.OldestWaiting != 0 || got.Started != 3 || got.DependencyWait <= 0 || got.WorkerWait <= 0 {
		t.Fatalf("settled stats: %+v", details)
	}
}

func TestFastLogicMarksGetterContextAndSlowPreparationCanLoad(t *testing.T) {
	base := newMockGetter()
	id := mustBuildCastID(t, 9950, entity.EntityCategory(1), nestLocalKind)
	base.Add(newMockEntity(id, entity.EntityCategory(1)))
	getter := &policyTestGetter{Getter: base}
	mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{1, 8}))
	mgr.MustRegisterHandlerWithMeta(NewHandlerName("policy"), func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		// preparedGetter 缓存以外的动态目标也必须继承快阶段约束。
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

// 一个续行占满快池时，外部准入与 QueueLen 必须看见同一个真实等待位。
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
	q.continueFast(queueMessage(1))
	stagedSignal(t, entered)
	admitQueue(t, q, false, 2)
	rejected := queueMessage(3)
	if err := q.admit(rejected, false); !errors.Is(err, worker.ErrWorkerQueueFull) {
		t.Fatalf("admission=%v", err)
	}
	rejected.OnRelease()
	fast, _, _, stats := q.snapshotStats()
	if fast.QueueLen != 1 || stats.Fast.WaitingForWorker != 1 || stats.Fast.PeakWaiting != 1 || stats.ContinuationRunning != 1 {
		t.Fatalf("pool=%+v stats=%+v", fast, stats)
	}
	unblock()
	if err := q.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fast, _, pending, stats := q.snapshotStats()
	if fast.QueueLen != 0 || pending != 0 || stats.ContinuationRunning != 0 || stats.Fast.WaitingForWorker != 0 {
		t.Fatalf("not drained: %+v %+v", fast, stats)
	}
}
