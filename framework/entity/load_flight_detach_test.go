package entity

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// RR-20260926-54：共享加载曾直接用领头调用方的 ctx 调 LoadEntity。RR-36 让 EnterGame 以 2s 登录
// 预算领头冷加载后，同一 flight 里预算更长的等待方（例如同一玩家的 Nest 慢阶段准备）在领头方
// 截止时一起得到 DeadlineExceeded。承诺：加载运行在与调用方解耦的 ctx 上（WithoutCancel 派生，
// 只受框架加载超时与停机取消约束）；每个等待方按自己的 ctx 离开；最后一个等待方离开也不取消
// 在途加载，结果照常进入 EntityManager。

// gatedLoader 在 release 关闭前阻塞，期间尊重自己拿到的 ctx；放行后经 RunLocal 发布实体，
// 与 DataEngine 仓库的冷加载顺序一致。
type gatedLoader struct {
	manager *EntityManager
	entered chan struct{}
	release chan struct{}
	loads   atomic.Int32
	// ctxErr 记录 LoadEntity 返回时自己的 ctx 状态。
	ctxErr atomic.Pointer[error]
}

func newGatedLoader(manager *EntityManager) *gatedLoader {
	return &gatedLoader{manager: manager, entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (l *gatedLoader) LoadEntity(ctx context.Context, id int64, _ EntityKind) (IThreadSafeEntity, error) {
	l.loads.Add(1)
	l.entered <- struct{}{}
	select {
	case <-l.release:
	case <-ctx.Done():
	}
	err := ctx.Err()
	l.ctxErr.Store(&err)
	if err != nil {
		return nil, err
	}
	value := newMgrTestEntity(id, testEntityCategoryPlayer)
	var addErr error
	if runErr := RunLocal(ctx, func() { addErr = l.manager.TryAdd(value) }); runErr != nil {
		return nil, runErr
	}
	if addErr != nil {
		return nil, addErr
	}
	return value, nil
}

func (l *gatedLoader) loaderCtxErr() error {
	if p := l.ctxErr.Load(); p != nil {
		return *p
	}
	return nil
}

func waitEntered(t *testing.T, loader *gatedLoader) {
	t.Helper()
	select {
	case <-loader.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the load never reached the loader")
	}
}

type getResult struct {
	value IThreadSafeEntity
	err   error
}

func goGet(access *ManagerAccess, ctx context.Context, id int64) <-chan getResult {
	done := make(chan getResult, 1)
	go func() {
		value, err := access.Get(ctx, id, EntityCategoryNone)
		done <- getResult{value, err}
	}()
	return done
}

// loginBudget 代表 RR-36 的 2s 登录预算；缩短只为让回归快，时序关系不变。
const loginBudget = 200 * time.Millisecond

func TestSharedLoadOutlivesTheLeadersBudget(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := newGatedLoader(manager)
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	const id = int64(5401)

	leaderCtx, cancelLeader := context.WithTimeout(context.Background(), loginBudget)
	defer cancelLeader()
	leader := goGet(access, leaderCtx, id)
	waitEntered(t, loader)

	// 同一 flight 中预算更长的等待方。
	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWaiter()
	waiter := goGet(access, waiterCtx, id)

	select {
	case got := <-leader:
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("leader: value=%v err=%v, want its own login budget to expire", got.value, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the leader did not leave when its own budget expired")
	}
	close(loader.release)

	select {
	case got := <-waiter:
		if got.err != nil || got.value == nil {
			t.Fatalf("waiter with a longer budget: value=%v err=%v (loader ctx at return: %v); the leader's budget ended the shared load",
				got.value, got.err, loader.loaderCtxErr())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got the shared load's result")
	}
	if n := loader.loads.Load(); n != 1 {
		t.Fatalf("loads=%d, want 1: the waiter must share the flight the leader started, not start a new one", n)
	}
	if manager.Get(id) == nil {
		t.Fatal("the loaded entity was not published into the EntityManager")
	}
}

func TestLastWaiterLeavingDoesNotCancelTheLoad(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := newGatedLoader(manager)
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	const id = int64(5402)

	ctx, cancel := context.WithTimeout(context.Background(), loginBudget)
	defer cancel()
	only := goGet(access, ctx, id)
	waitEntered(t, loader)
	if got := <-only; !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("the only waiter: value=%v err=%v, want its own deadline", got.value, got.err)
	}
	// 没有任何等待方了；放行后加载仍完成并进入 EntityManager。
	close(loader.release)
	deadline := time.Now().Add(5 * time.Second)
	for manager.Get(id) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the load was abandoned when its last waiter left (loader ctx at return: %v)", loader.loaderCtxErr())
		}
		time.Sleep(5 * time.Millisecond)
	}
	value, err := access.Get(context.Background(), id, EntityCategoryNone)
	if err != nil || value == nil {
		t.Fatalf("Get after the load finished: value=%v err=%v", value, err)
	}
	if n := loader.loads.Load(); n != 1 {
		t.Fatalf("loads=%d, want the one load whose result was published", n)
	}
}

// 停机：DataEngine Runtime 停止时注销 loader，在途共享加载随之取消，等待方拿到结果而不是
// 各自等到自己的截止时间。
func TestUnregisteringTheLoaderCancelsInFlightLoads(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := newGatedLoader(manager)
	unregister, err := access.ConfigureLoader(loader)
	if err != nil {
		t.Fatal(err)
	}
	const id = int64(5403)
	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWaiter()
	waiter := goGet(access, waiterCtx, id)
	waitEntered(t, loader)

	unregister()
	select {
	case got := <-waiter:
		if got.err == nil || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("waiter after the loader stopped: value=%v err=%v, want the load cancelled", got.value, got.err)
		}
		if waiterCtx.Err() != nil {
			t.Fatal("the waiter was released by its own deadline, not by the loader stopping")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stopping the loader did not cancel the in-flight load; its waiters wait out their own deadlines")
	}
	if !errors.Is(loader.loaderCtxErr(), context.Canceled) {
		t.Fatalf("loader ctx at return: %v, want cancelled", loader.loaderCtxErr())
	}
	if manager.Get(id) != nil {
		t.Fatal("a cancelled load published its entity")
	}
}

// 框架加载上限：没有等待方、loader 也不返回的加载最终结束并释放 flight，Cause 为 ErrEntityLoadTimeout；
// 之后的 Get 重新发起加载。默认上限为 DefaultEntityLoadTimeout。
func TestSharedLoadIsBoundedByTheFrameworkLoadTimeout(t *testing.T) {
	if DefaultEntityLoadTimeout != 30*time.Second {
		t.Fatalf("DefaultEntityLoadTimeout=%v, want 30s", DefaultEntityLoadTimeout)
	}
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	access.ConfigureLoadTimeout(100 * time.Millisecond)
	loader := newGatedLoader(manager)
	if _, err := access.ConfigureLoader(loader); err != nil {
		t.Fatal(err)
	}
	const id = int64(5404)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := <-goGet(access, ctx, id)
	waitEntered(t, loader)
	if !errors.Is(got.err, context.DeadlineExceeded) || !errors.Is(got.err, ErrEntityLoadTimeout) || ctx.Err() != nil {
		t.Fatalf("value=%v err=%v, want the framework load timeout (not the caller's own deadline)", got.value, got.err)
	}
	access.flightMu.Lock()
	_, stuck := access.flights[id]
	access.flightMu.Unlock()
	if stuck {
		t.Fatal("the timed-out load left its flight behind")
	}
	close(loader.release)
	if value, err := access.Get(ctx, id, EntityCategoryNone); err != nil || value == nil {
		t.Fatalf("retry after the timeout: value=%v err=%v", value, err)
	}
	if n := loader.loads.Load(); n != 2 {
		t.Fatalf("loads=%d, want the timed-out load and one fresh retry", n)
	}
}

// 停机取消给出可判别的 Cause；注销可重复调用。
func TestLoaderStopCauseReachesWaiters(t *testing.T) {
	manager := NewEntityManager()
	access := NewManagerAccess(manager)
	loader := newGatedLoader(manager)
	unregister, err := access.ConfigureLoader(loader)
	if err != nil {
		t.Fatal(err)
	}
	waiter := goGet(access, context.Background(), 5405)
	waitEntered(t, loader)
	unregister()
	unregister()
	if got := <-waiter; !errors.Is(got.err, ErrEntityLoaderStopped) || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err=%v, want ErrEntityLoaderStopped wrapped with context.Canceled", got.err)
	}
}
