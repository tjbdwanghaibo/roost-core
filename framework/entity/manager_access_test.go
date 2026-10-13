// ManagerAccess：保持顺序与缺失项、冷加载并发与单飞、等待方按自己的 ctx 离开、只查已加载的入口不触发加载。

package entity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

func TestManagerAccessPreservesOrderAndMissingEntries(t *testing.T) {
	manager := NewEntityManager()
	first := newMgrTestEntity(1001, testEntityCategoryPlayer)
	second := newMgrTestEntity(1002, testEntityCategoryPlayer)
	manager.Add(first)
	manager.Add(second)
	access := NewManagerAccess(manager)
	values, err := access.GetMany(context.Background(), []int64{1002, 9999, 1001}, []EntityCategory{
		testEntityCategoryPlayer, testEntityCategoryPlayer, testEntityCategoryPlayer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || values[0] != second || values[1] != nil || values[2] != first {
		t.Fatalf("unexpected ordered lookup: %+v", values)
	}
}

type concurrentAggregateLoader struct {
	manager *EntityManager
	active  atomic.Int32
	max     atomic.Int32
}

func (l *concurrentAggregateLoader) LoadEntity(_ context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	active := l.active.Add(1)
	for current := l.max.Load(); active > current && !l.max.CompareAndSwap(current, active); current = l.max.Load() {
	}
	time.Sleep(10 * time.Millisecond)
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	if err := l.manager.TryAdd(value); err != nil {
		l.active.Add(-1)
		return nil, err
	}
	l.active.Add(-1)
	return value, nil
}

func TestManagerAccessGetManyLoadsColdEntitiesConcurrently(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	access.ConfigureLoadConcurrency(4)
	loader := &concurrentAggregateLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	ids := []int64{2001, 2002, 2003, 2004}
	values, err := access.GetMany(context.Background(), ids, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != len(ids) || loader.max.Load() < 2 {
		t.Fatalf("cold load did not run concurrently: values=%d max=%d", len(values), loader.max.Load())
	}
}

type countingAggregateLoader struct {
	manager *EntityManager
	loads   atomic.Int32
	fail    atomic.Bool
	block   chan struct{}
}

func (l *countingAggregateLoader) LoadEntity(_ context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	l.loads.Add(1)
	if l.block != nil {
		<-l.block
	}
	if l.fail.Load() {
		return nil, ErrEntityNotManaged
	}
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	if err := l.manager.TryAdd(value); err != nil {
		return nil, err
	}
	return value, nil
}

// Regression: a cold-cache Get used to issue one LoadEntity per concurrent
// caller — a hot entity's cache miss stampeded the database. Concurrent
// loads of one id must collapse into a single flight.
func TestManagerAccessColdLoadIsSingleFlight(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := &countingAggregateLoader{manager: manager, block: make(chan struct{})}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	const callers = 16
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			value, err := access.Get(context.Background(), 4242, testEntityCategoryPlayer)
			if err == nil && value == nil {
				err = ErrEntityNotManaged
			}
			results <- err
		}()
	}
	for loader.loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let every waiter join the flight
	close(loader.block)
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := loader.loads.Load(); got != 1 {
		t.Fatalf("concurrent cold loads issued %d LoadEntity calls, want 1", got)
	}
	// A failed flight shares the error but is removed, so a retry loads fresh.
	failing := &countingAggregateLoader{manager: manager}
	failing.fail.Store(true)
	if _, err := access.ConfigureLoader(failing); err != nil {
		t.Fatal(err)
	}
	if _, err := access.Get(context.Background(), 5555, testEntityCategoryPlayer); err == nil {
		t.Fatal("expected load failure")
	}
	failing.fail.Store(false)
	if value, err := access.Get(context.Background(), 5555, testEntityCategoryPlayer); err != nil || value == nil {
		t.Fatalf("retry after failed flight: value=%v err=%v", value, err)
	}
	if got := failing.loads.Load(); got != 2 {
		t.Fatalf("retry did not start a fresh load: loads=%d", got)
	}
}

// A waiter whose own context is cancelled stops waiting without killing the
// in-flight load for everyone else.
func TestManagerAccessFlightWaiterHonorsOwnContext(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := &countingAggregateLoader{manager: manager, block: make(chan struct{})}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		_, err := access.Get(context.Background(), 7777, testEntityCategoryPlayer)
		started <- err
	}()
	for loader.loads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := access.Get(cancelled, 7777, testEntityCategoryPlayer)
		waiterDone <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := awaitChan(t, waiterDone, "the waiter to observe its own cancellation"); err != context.Canceled {
		t.Fatalf("cancelled waiter returned %v", err)
	}
	close(loader.block)
	if err := awaitChan(t, started, "the in-flight load to start"); err != nil {
		t.Fatalf("original load was affected by waiter cancellation: %v", err)
	}
	if got := loader.loads.Load(); got != 1 {
		t.Fatalf("loads = %d, want 1", got)
	}
}

// awaitChan receives from ch with an upper bound. A bare receive made a broken
// property fail as a go test timeout — a stack dump after the default ten
// minutes, naming no expectation — so every wait that IS the assertion is
// bounded and says what it was waiting for.
func awaitChan[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func TestLoadedOnlyLookupNeverStartsOrJoinsColdLoad(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := &countingAggregateLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	ctx := WithLoadedEntitiesOnly(context.Background())
	if _, err := access.Get(ctx, 4242, EntityCategoryNone); !errors.Is(err, ErrColdLoadInLogic) {
		t.Fatalf("cold Get: %v", err)
	}
	if _, err := access.GetMany(ctx, []int64{4242, 4243}, nil); !errors.Is(err, ErrColdLoadInLogic) {
		t.Fatalf("cold GetMany: %v", err)
	}
	// 即使已有加载正在进行，快阶段也不得加入它并占用 worker。
	access.flights[4242] = &entityLoadFlight{done: make(chan struct{})}
	if _, err := access.Get(ctx, 4242, EntityCategoryNone); !errors.Is(err, ErrColdLoadInLogic) {
		t.Fatalf("in flight: %v", err)
	}
	delete(access.flights, 4242)
	if loader.loads.Load() != 0 {
		t.Fatal("fast lookup called loader")
	}
	value, err := access.Get(context.Background(), 4242, EntityCategoryNone)
	if err != nil || value == nil {
		t.Fatalf("slow preparation: %v", err)
	}
	values, err := access.GetMany(ctx, []int64{4242, 4242}, nil)
	if err != nil || len(values) != 2 || values[0] != value || values[1] != value {
		t.Fatalf("prepared lookup: %v %v", values, err)
	}
}

// ManagerAccess.IsLoaded 是 Nest 准入的冷目标判定，只读内存：已加载 → true；未加载且有 loader → false；没有 loader → true
// （I/O 准备阶段也读不到）。不调用 loader、不加入在途加载、不 Touch。
func TestManagerAccessIsLoadedNeverLoads(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	hot := newMgrTestEntity(5101, testEntityCategoryPlayer)
	manager.Add(hot)
	if !access.IsLoaded(5102) {
		t.Fatal("without a loader a missing entity has nothing to load")
	}
	loader := &countingAggregateLoader{manager: manager, block: make(chan struct{})}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	if !access.IsLoaded(5101) {
		t.Fatal("loaded entity reported cold")
	}
	if access.IsLoaded(5102) {
		t.Fatal("missing entity with a loader reported loaded")
	}
	// 在途加载存在时也只回答内存状态，不等待。
	access.flights[5102] = &entityLoadFlight{done: make(chan struct{})}
	if access.IsLoaded(5102) {
		t.Fatal("in-flight load reported loaded")
	}
	delete(access.flights, 5102)
	if loader.loads.Load() != 0 {
		t.Fatalf("IsLoaded called the loader %d time(s)", loader.loads.Load())
	}
	close(loader.block)
	if _, err := access.Get(context.Background(), 5102, EntityCategoryNone); err != nil {
		t.Fatal(err)
	}
	if !access.IsLoaded(5102) {
		t.Fatal("entity published by the loader still reported cold")
	}
	var checker LoadedChecker = access
	_ = checker
}

// 快阶段的冷缺失本身不阻塞：访问未加载且配置了 loader 的目标时不做 I/O、不等待 singleflight，返回可 errors.Is 的
// ErrColdLoadInLogic，业务可以据此降级。只有真正会等待的入口（I/O、等待 flight / 投影）才 fail-fast。
// 历史：RR-20260926-26。
func TestFastWorkerColdMissReturnsErrorInsteadOfPanicking(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := &countingAggregateLoader{manager: manager}
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	hot := newMgrTestEntity(5101, testEntityCategoryPlayer)
	manager.Add(hot)
	_, release := fctx.NewContext(fctx.WithShortWorker(), fctx.WithHandler("optional_friend"))
	defer release()

	for _, detached := range []bool{false, true} {
		ctx := WithLoadedEntitiesOnly(context.Background())
		if detached {
			// 保存的 Background ctx 不带加载约束，业务 worker 标记本身也必须生效。
			ctx = context.Background()
		}
		checkColdMiss(t, "Get", func() error {
			_, err := access.Get(ctx, 5102, EntityCategoryNone)
			return err
		})
		checkColdMiss(t, "GetMany", func() error {
			_, err := access.GetMany(ctx, []int64{5101, 5102}, nil)
			return err
		})
	}
	if loads := loader.loads.Load(); loads != 0 {
		t.Fatalf("fast-stage cold miss reached the loader %d times", loads)
	}
	// 已加载实体仍按内存读取成功。
	if value, err := access.Get(context.Background(), 5101, EntityCategoryNone); err != nil || value != hot {
		t.Fatalf("hot Get: %v %v", value, err)
	}
}

func checkColdMiss(t *testing.T, name string, call func() error) {
	t.Helper()
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("%s: fast-stage cold miss panicked instead of returning ErrColdLoadInLogic: %v", name, r)
			}
		}()
		err = call()
	}()
	if !errors.Is(err, ErrColdLoadInLogic) || !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
		t.Fatalf("%s: err=%v, want ErrColdLoadInLogic with the fast-worker marker", name, err)
	}
}
