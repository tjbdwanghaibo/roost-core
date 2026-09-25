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
