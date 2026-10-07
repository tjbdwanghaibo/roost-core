package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
)

// RR-20260926-47：RR-25 的准入判定在发送方 goroutine（包括单一的延迟派发 goroutine）上调用 Getter.Get。
// 自定义 Getter 不遵守 LoadedEntitiesOnly 时，发送方同步执行 I/O，延迟消息被串行化。
// 承诺：准入判定只用不做加载的查询接口（entity.LoadedChecker，ManagerAccess 实现）；自定义 Getter 未实现时
// 准入不判冷、不自动转慢，Getter 只在 worker 上被调用。

// probeIOGetter 模拟不遵守 LoadedEntitiesOnly 的自定义 Getter：在任何 worker 之外被调用时同步“加载”200ms。
type probeIOGetter struct {
	entity.Getter
	outsideWorker atomic.Int32
}

func (g *probeIOGetter) Get(ctx context.Context, id int64, c entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if fctx.CurrentContext() == nil {
		g.outsideWorker.Add(1)
		time.Sleep(200 * time.Millisecond) // 模拟 I/O：发生在发送方 / 延迟派发 goroutine 上即为缺陷
	}
	return g.Getter.Get(ctx, id, c)
}

func TestAdmissionNeverRunsCustomGetterOnSender(t *testing.T) {
	id, e := newAsyncPilotEntity(t, 9800, 1)
	base := newMockGetter()
	base.Add(e)
	g := &probeIOGetter{Getter: base}
	mgr := NewEngine(NestOptionWithGetter(g), NestOptionWithWorkerNumAndMsgCap(1, 16))
	name := NewHandlerName("admission_custom_getter")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return "ok", nil }, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())

	msg, ch := GenSyncMsg(MsgTypeSingle)
	msg.Name, msg.Tid = name.String(), id
	started := time.Now()
	if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
		t.Fatal(err)
	}
	sendTook := time.Since(started)
	if got := stagedWait(t, ch); got != "ok" {
		t.Fatalf("reply=%v", got)
	}
	if n := g.outsideWorker.Load(); n != 0 {
		t.Fatalf("admission called the custom Getter %d time(s) on the sender goroutine; TrySendMsg took %v", n, sendTook)
	}
}

func TestDelayedAdmissionIsNotSerializedByCustomGetter(t *testing.T) {
	id, e := newAsyncPilotEntity(t, 9810, 1)
	base := newMockGetter()
	base.Add(e)
	g := &probeIOGetter{Getter: base}
	mgr := NewEngine(NestOptionWithGetter(g), NestOptionWithWorkerNumAndMsgCap(4, 16))
	name := NewHandlerName("admission_custom_getter_delayed")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return "ok", nil }, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())

	const n = 3
	done := make(chan time.Duration, n)
	begin := time.Now()
	for range n {
		msg, ch := GenSyncMsg(MsgTypeSingle)
		msg.Name, msg.Tid = name.String(), id
		if err := mgr.dispatcher.TryDelaySendMsg(10*time.Millisecond, msg); err != nil {
			t.Fatal(err)
		}
		go func() { <-ch; done <- time.Since(begin) }()
	}
	var last time.Duration
	for range n {
		select {
		case d := <-done:
			last = max(last, d)
		case <-time.After(3 * time.Second):
			t.Fatal("delayed messages did not complete")
		}
	}
	if calls := g.outsideWorker.Load(); calls != 0 {
		t.Fatalf("delay loop called the custom Getter %d time(s); last of %d 10ms-delayed messages finished at %v", calls, n, last)
	}
	// 基线上三条依次完成于约 212 / 413 / 614ms；不再在 delay loop 上做 I/O 后应远低于一次 200ms “加载”。
	if last >= 190*time.Millisecond {
		t.Fatalf("delayed admissions look serialized: last finished at %v", last)
	}
}

// 未实现 LoadedChecker 的自定义 Getter：准入不判冷、不自动转慢（旧行为），冷目标照 RR-02 返回 ErrColdLoadInLogic，
// loader 不被调用。Getter 本身遵守 LoadedEntitiesOnly，所以红来自“准入仍然用它判冷并转慢”。
type contractGetter struct{ access *entity.ManagerAccess }

func (g contractGetter) Get(ctx context.Context, id int64, c entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.access.Get(ctx, id, c)
}
func (g contractGetter) GetMany(ctx context.Context, ids []int64, cs []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	return g.access.GetMany(ctx, ids, cs)
}

// loadedCheckerGetter 在 contractGetter 之上实现 IsLoaded，并记录 worker 之外的 Get 调用。
type loadedCheckerGetter struct {
	contractGetter
	isLoaded, outsideWorker atomic.Int32
}

func (g *loadedCheckerGetter) Get(ctx context.Context, id int64, c entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	if fctx.CurrentContext() == nil {
		g.outsideWorker.Add(1)
	}
	return g.contractGetter.Get(ctx, id, c)
}
func (g *loadedCheckerGetter) IsLoaded(id int64) bool {
	g.isLoaded.Add(1)
	return g.access.Manager().Get(id) != nil // 本测试总配置了 loader：不在内存即需要加载
}

func newLoaderAccess(t *testing.T) (*entity.ManagerAccess, *entity.EntityManager, *stageLoader) {
	t.Helper()
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	loader := &stageLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	return access, manager, loader
}

func TestAdmissionColdProbeRequiresLoadedChecker(t *testing.T) {
	t.Run("custom_getter_without_checker_keeps_old_behavior", func(t *testing.T) {
		access, _, loader := newLoaderAccess(t)
		mgr := NewEngine(NestOptionWithGetter(contractGetter{access: access}), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8}))
		mgr.MustRegisterHandlerWithMeta(NewHandlerName("old_cold"), func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return "ok", nil }, HandlerMeta{})
		startColdTargetEngine(t, mgr)
		cold := mustBuildCastID(t, 9821, entity.EntityCategory(1), nestLocalKind)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err := mgr.Request(ctx, NewHandlerName("old_cold"), cold, nil)
		if !errors.Is(err, entity.ErrColdLoadInLogic) || loader.calls.Load() != 0 || mgr.Stats().Queue.Slow.Started != 0 {
			t.Fatalf("custom Getter without IsLoaded was probed and moved to the slow stage: err=%v loads=%d slow started=%d", err, loader.calls.Load(), mgr.Stats().Queue.Slow.Started)
		}
	})
	t.Run("custom_getter_with_checker_auto_slows", func(t *testing.T) {
		access, _, loader := newLoaderAccess(t)
		g := &loadedCheckerGetter{contractGetter: contractGetter{access: access}}
		mgr := NewEngine(NestOptionWithGetter(g), NestOptionWithWorkerPools(WorkerPoolConfig{1, 8}, WorkerPoolConfig{2, 8}))
		mgr.MustRegisterHandlerWithMeta(NewHandlerName("checked_cold"), func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
			if es[0] == nil {
				return nil, errors.New("declared target missing in handler")
			}
			return "ok", nil
		}, HandlerMeta{})
		startColdTargetEngine(t, mgr)
		cold := mustBuildCastID(t, 9831, entity.EntityCategory(1), nestLocalKind)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if ret, err := mgr.Request(ctx, NewHandlerName("checked_cold"), cold, nil); err != nil || ret != "ok" {
			t.Fatalf("cold target with IsLoaded getter: ret=%v err=%v loads=%d", ret, err, loader.calls.Load())
		}
		if loader.calls.Load() != 1 || g.isLoaded.Load() == 0 {
			t.Fatalf("loads=%d isLoaded calls=%d, want one slow-stage load decided by IsLoaded", loader.calls.Load(), g.isLoaded.Load())
		}
		if n := g.outsideWorker.Load(); n != 0 {
			t.Fatalf("admission called Getter.Get %d time(s) outside any worker", n)
		}
	})
}
