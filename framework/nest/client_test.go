// Nest 客户端入口与引擎生命周期：上下文传播、准入背压、取消 / fence 拒绝、延迟消息与停机。

package nest

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

func TestClientRequestCarriesContextAndReturnsResult(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	id := mustBuildCastID(t, 8100, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))

	engine := NewEngine(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 8}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithSyncTimeout(time.Second),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	type contextKey string
	const key contextKey = "request"
	name := NewHandlerName("client_context")
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if got := fctx.BaseContext().Value(key); got != "value" {
			return nil, errors.New("request context value not propagated")
		}
		return "ok", nil
	})

	ctx := context.WithValue(context.Background(), key, "value")
	ret, err := engine.Request(ctx, name, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ret != "ok" {
		t.Fatalf("ret=%v want=ok", ret)
	}
}

func TestClientDispatchCarriesOnlyFrameworkEnvelope(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	id := mustBuildCastID(t, 8103, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))

	engine := NewEngine(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 8}, WorkerPoolConfig{}, WorkerPoolConfig{}),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })

	type contextKey string
	const key contextKey = "async-request"
	type observation struct {
		baseValue     any
		businessParam any
		config        any
		frame         uint64
		playerID      int64
		msgID         uint32
		seq           uint32
		traceID       string
		kvFound       bool
		syncWait      time.Duration
		source        string
		handler       string
	}
	observed := make(chan observation, 1)
	name := NewHandlerName("client_async_context_isolated")
	MustRegisterMemoryHandler(name, func(_ []entity.IThreadSafeEntity, params []any, _ ...HandlerOption) (any, error) {
		current := fctx.CurrentContext()
		_, found := current.Get("business")
		var businessParam any
		if len(params) > 0 {
			businessParam = params[0]
		}
		observed <- observation{
			baseValue:     fctx.BaseContext().Value(key),
			businessParam: businessParam,
			config:        current.Config,
			frame:         current.Frame,
			playerID:      current.Meta.PlayerID,
			msgID:         current.Meta.MsgID,
			seq:           current.Meta.Seq,
			traceID:       current.Trace.TraceID,
			kvFound:       found,
			syncWait:      current.SyncWait,
			source:        current.Meta.Source,
			handler:       current.Meta.Handler,
		}
		return nil, nil
	})

	oldConfig := fctx.RuntimeConfig()
	fctx.SetRuntimeConfig("config-generation-8")
	t.Cleanup(func() { fctx.SetRuntimeConfig(oldConfig) })
	base := context.WithValue(context.Background(), key, "must-not-propagate")
	parent, releaseParent := fctx.NewContext(
		fctx.WithBase(base),
		fctx.WithConfig("config-generation-7"),
		fctx.WithFrame(999_999),
		fctx.WithMeta(fctx.RequestMeta{Source: "request", Handler: "parent", PlayerID: 777, MsgID: 11, Seq: 12}),
		fctx.WithTrace(fctx.TraceMeta{TraceID: "parent-trace", Enabled: true}),
		fctx.WithSyncWait(30*time.Second),
	)
	parent.Set("business", "must-not-propagate")
	if err := engine.Dispatch(base, name, id, NewParams("explicit-business-param")); err != nil {
		releaseParent()
		t.Fatal(err)
	}
	releaseParent()

	select {
	case got := <-observed:
		if got.baseValue != nil || got.kvFound || got.syncWait != 0 || got.frame == 999_999 {
			t.Fatalf("execution-local context leaked into async dispatch: %+v", got)
		}
		if got.businessParam != "explicit-business-param" {
			t.Fatalf("explicit business param missing: %+v", got)
		}
		if got.playerID != 777 || got.msgID != 11 || got.seq != 12 || got.traceID != "parent-trace" {
			t.Fatalf("framework message envelope missing: %+v", got)
		}
		if got.config != "config-generation-8" {
			t.Fatalf("config generation missing: %+v", got)
		}
		if got.source != "nest" || got.handler != name.String() {
			t.Fatalf("nest-owned metadata missing: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("async handler did not run")
	}
}

func TestClientAdmissionReportsQueueFull(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	id := mustBuildCastID(t, 8101, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	engine := NewEngine(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 1}, WorkerPoolConfig{}, WorkerPoolConfig{}),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	name := NewHandlerName("client_backpressure")
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil, nil
	})
	if err := engine.Dispatch(context.Background(), name, id, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	var admissionErr error
	for i := 0; i < 64; i++ {
		admissionErr = engine.Dispatch(context.Background(), name, id, nil)
		if admissionErr != nil {
			break
		}
	}
	if !errors.Is(admissionErr, ErrQueueFull) {
		t.Fatalf("dispatch error=%v want ErrQueueFull", admissionErr)
	}
	// 准入错误只归一化一次：ErrQueueFull 的文本在错误链里只出现一次。
	if n := strings.Count(admissionErr.Error(), "dispatch queue full"); n != 1 {
		t.Fatalf("dispatch error text repeats ErrQueueFull %d times: %q", n, admissionErr.Error())
	}
	close(release)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := engine.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Dispatch(context.Background(), name, id, nil); !errors.Is(err, ErrNestStopped) {
		t.Fatalf("dispatch after shutdown=%v want ErrNestStopped", err)
	}
}

func TestClientRejectsCanceledContextBeforeAdmission(t *testing.T) {
	getter := newMockGetter()
	engine := NewEngine(NestOptionWithGetter(getter))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.Dispatch(ctx, NewHandlerName("canceled"), 1, nil); !errors.Is(err, ErrNestCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v want ErrNestCanceled and context.Canceled", err)
	}
}

func TestClientFenceRejectsAdmissionWithCause(t *testing.T) {
	engine := NewEngine(NestOptionWithGetter(newMockGetter()), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 8}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	cause := errors.New("WAL fsync outcome unknown")
	engine.Fence(cause)
	err := engine.Dispatch(context.Background(), NewHandlerName("fenced"), mustBuildCastID(t, 999, entity.EntityCategory(1), nestLocalKind), nil)
	if !errors.Is(err, ErrNestFenced) || !errors.Is(err, cause) {
		t.Fatalf("fenced dispatch err=%v", err)
	}
}

func TestShutdownTimeoutDoesNotBlockIndependentEngine(t *testing.T) {
	ResetHandlersForTest()
	t.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	id := mustBuildCastID(t, 8102, entity.EntityCategory(1), nestLocalKind)
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	engine := NewEngine(NestOptionWithGetter(getter))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	name := NewHandlerName("shutdown_drain")
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		close(entered)
		<-release
		return nil, nil
	})
	if err := engine.Dispatch(context.Background(), name, id, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	err := engine.Shutdown(shutdownCtx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error=%v want deadline", err)
	}
	second := NewEngine(NestOptionWithGetter(getter))
	if err := second.Start(); err != nil {
		t.Fatalf("independent second start: %v", err)
	}
	close(release)
	if err := engine.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkClientRequestSingle(b *testing.B) {
	ResetHandlersForTest()
	b.Cleanup(ResetHandlersForTest)
	getter := newMockGetter()
	entity.MustRegisterEntityKindCategory(nestLocalKind, entity.EntityCategory(1))
	id, err := entity.BuildEntityID(8200, nestLocalKind)
	if err != nil {
		b.Fatal(err)
	}
	getter.Add(newMockEntity(id, entity.EntityCategory(1)))
	engine := NewEngine(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 1024}, WorkerPoolConfig{}, WorkerPoolConfig{}),
	)
	if err := engine.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	name := NewHandlerName("benchmark_client_request")
	MustRegisterMemoryHandler(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		return int64(1), nil
	})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := engine.Request(ctx, name, id, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// 停机时取走的延迟消息：已接受的同步 Request 先收到 ErrNestStopped 终态，不能只回收不回答。
// 历史：RR-20260911-04。
func TestShutdownAnswersDelayedRequestsInsteadOfDroppingThem(t *testing.T) {
	dispatcher := NewDispatcher("nest", 2, 64, func(*Msg) {
		t.Error("a delayed message that never came due must not be dispatched")
	})
	dispatcher.OnInit()

	waiting, ch := GenSyncMsg(MsgTypeSingle)
	dispatcher.delaySendMsg(time.Hour, waiting)
	if got := dispatcher.delayedCount(); got != 1 {
		t.Fatalf("control: delayed count = %d, want 1", got)
	}

	if err := dispatcher.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	select {
	case got := <-ch:
		err, ok := got.(error)
		if !ok {
			t.Fatalf("a shut-down dispatcher answered %#v, want an error", got)
		}
		if !errors.Is(err, ErrNestStopped) {
			t.Fatalf("answer = %v, want ErrNestStopped", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the accepted request got no answer at all; the caller is left to its own timeout")
	}
}

// An asynchronous delayed message has no one waiting, so shutting down must
// not try to answer it — and must not block or panic on a nil channel.
func TestShutdownDropsDelayedFireAndForgetQuietly(t *testing.T) {
	dispatcher := NewDispatcher("nest", 2, 64, func(*Msg) {
		t.Error("a delayed message that never came due must not be dispatched")
	})
	dispatcher.OnInit()
	dispatcher.delaySendMsg(time.Hour, GenMsg(MsgTypeSingle))
	if err := dispatcher.OnDestroyWithContext(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := dispatcher.delayedCount(); got != 0 {
		t.Fatalf("delayed count after shutdown = %d, want 0", got)
	}
}

func TestAsyncContinuationCapturesCurrentRuntimeGeneration(t *testing.T) {
	old := fctx.RuntimeConfig()
	t.Cleanup(func() { fctx.SetRuntimeConfig(old) })
	parent := fctx.ContextSnapshot{Valid: true, Config: "generation-one", Meta: fctx.RequestMeta{PlayerID: 42}}
	fctx.SetRuntimeConfig("generation-two")
	next := asyncMessageContextSnapshot(parent)
	fctx.SetRuntimeConfig("generation-three")
	later := asyncMessageContextSnapshot(next)
	if next.Config != "generation-two" || later.Config != "generation-three" || next.Meta.PlayerID != 42 {
		t.Fatalf("continuation pinned old snapshot: next=%+v later=%+v", next, later)
	}
}

func TestDelayedAdmissionIsBounded(t *testing.T) {
	dispatcher := NewDispatcher("delay_limit", 1, 8, func(*Msg) {})
	dispatcher.ConfigureDelayedAdmission(1, time.Second)
	dispatcher.OnInit()
	defer func() { _ = dispatcher.OnDestroyWithContext(context.Background()) }()
	if err := dispatcher.TryDelaySendMsg(500*time.Millisecond, GenMsg(MsgTypeSingle)); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.TryDelaySendMsg(500*time.Millisecond, GenMsg(MsgTypeSingle)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("queue overflow err=%v", err)
	}
	if err := dispatcher.TryDelaySendMsg(2*time.Second, GenMsg(MsgTypeSingle)); !errors.Is(err, ErrDelayTooLong) {
		t.Fatalf("max delay err=%v", err)
	}
}

// An engine is single-use: Start after Shutdown must refuse, and Start
// without a getter must refuse before any worker pool exists.
func TestEngineStartRefusesAfterShutdownAndWithoutGetter(t *testing.T) {
	if err := NewEngine().Start(); !errors.Is(err, ErrGetterNotSet) {
		t.Fatalf("Start without getter = %v", err)
	}
	mgr := NewEngine(NestOptionWithGetter(newMockGetter()))
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(); !errors.Is(err, ErrNestStopped) {
		t.Fatalf("Start after Shutdown = %v", err)
	}
}

func TestDispatcherDelaySendMsgDoesNotCreatePerMessageGoroutines(t *testing.T) {
	dispatcher := NewDispatcher("nest_delay_test", 1, 64, func(msg *Msg) {
		msg.OnRelease()
	})
	dispatcher.OnInit()
	defer dispatcher.OnDestroy()

	before := runtime.NumGoroutine()
	for i := 0; i < 32; i++ {
		dispatcher.delaySendMsg(time.Second, GenMsg(MsgTypeSingle))
	}
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if delta := after - before; delta > 8 {
		t.Fatalf("goroutines grew by %d, want a bounded dispatcher scheduler instead of per-message goroutines", delta)
	}
	if stats := dispatcher.Stats(); stats.Delayed != 32 {
		t.Fatalf("delayed stats = %d, want 32", stats.Delayed)
	}
}

func TestSyncUsesRequestSyncWait(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 101, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.Add(e)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	MustRegisterMemoryHandler(NewHandlerName("test_request_sync_wait"), func(es []entity.IThreadSafeEntity, param []any, opts ...HandlerOption) (any, error) {
		time.Sleep(50 * time.Millisecond)
		return "late", nil
	})

	_, release := fctx.NewContext(fctx.WithSyncWait(5 * time.Millisecond))
	defer release()

	_, err := Nest.Request(context.Background(), NewHandlerName("test_request_sync_wait"), id, nil)
	if !errors.Is(err, ErrNestTimeout) {
		t.Fatalf("Sync err = %v, want %v", err, ErrNestTimeout)
	}
}

func TestSyncCarriesCurrentContextIntoHandler(t *testing.T) {
	t.Cleanup(ResetHandlersForTest) // 清掉本用例注册的包级 handler，-count>1 重跑不重复注册
	getter := newMockGetter()
	id := mustBuildCastID(t, 102, entity.EntityCategory(1), nestLocalKind)
	e := newMockEntity(id, entity.EntityCategory(1))
	getter.Add(e)

	InitNest(
		NestOptionWithGetter(getter),
		NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}),
		NestOptionWithTickDuration(100*time.Millisecond),
	)
	defer StopNest()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	MustRegisterMemoryHandler(NewHandlerName("test_request_context_in_handler"), func(es []entity.IThreadSafeEntity, param []any, opts ...HandlerOption) (any, error) {
		c := fctx.CurrentContext()
		if c == nil {
			return nil, errors.New("handler has no request context")
		}
		if c.Base != base {
			return nil, errors.New("handler did not receive caller base context")
		}
		if c.SyncWait != 17*time.Millisecond {
			return nil, errors.New("handler did not receive caller sync wait")
		}
		if c.Meta.Source != "nest" || c.Meta.PlayerID != 777 || c.Meta.Handler != "test_request_context_in_handler" {
			return nil, errors.New("handler meta was not merged correctly")
		}
		return "ok", nil
	})

	_, release := fctx.NewContext(
		fctx.WithBase(base),
		fctx.WithSyncWait(17*time.Millisecond),
		fctx.WithPlayerProtocol(777, 10, 20),
	)
	defer release()

	ret, err := Nest.Request(base, NewHandlerName("test_request_context_in_handler"), id, nil)
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	if ret != "ok" {
		t.Fatalf("ret = %v, want ok", ret)
	}
}

func TestDispatcherStoppedReturnsErrorForSyncMessage(t *testing.T) {
	d := NewDispatcher("test_stopped", 1, 8, func(msg *Msg) {})
	d.OnInit()
	d.OnRun()
	d.OnDestroy()

	msg, ch := GenSyncMsg(MsgTypeSingle)
	d.sendMsg(msg)

	select {
	case ret := <-ch:
		if ret != ErrNestStopped {
			t.Fatalf("ret = %v, want %v", ret, ErrNestStopped)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stopped error")
	}
}
