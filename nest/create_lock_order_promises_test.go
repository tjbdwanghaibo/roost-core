package nest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-48：handler 内新建实体的取锁遵循与 Cast 相同的锁序。新实体的锁组不高于已持有的锁组时只 try-lock，
// 被占用时整条消息回滚并以 ErrLockTimeout 重新准入，不在快 worker 上无界等待；memory 模式 handler 内 Create
// 同样在 Guard 作用域内持锁到 handler 结束。

// blockingCommitter 让第一次 Commit 停在 release 上（模拟锁内 WAL 准入，属于快池等待豁免），其余立即返回。
type blockingCommitter struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func newBlockingCommitter() *blockingCommitter {
	return &blockingCommitter{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (c *blockingCommitter) Commit(context.Context, CommitRecord) error {
	if c.calls.Add(1) == 1 {
		c.entered <- struct{}{}
		<-c.release
	}
	return nil
}

func createParam(id int64) *entity.EntityCreateParam {
	return &entity.EntityCreateParam{IsCreate: true, Id: id, Category: entity.EntityCategory(1), Kind: createdInScopeKind}
}

type requestResult struct {
	tag string
	ret any
	err error
}

func sendRequest(mgr *NestMgr, tag string, name HandlerName, id int64, out chan<- requestResult) {
	go func() {
		ret, err := mgr.Request(context.Background(), name, id, nil)
		out <- requestResult{tag: tag, ret: ret, err: err}
	}()
}

func addPilots(t *testing.T, manager *entity.EntityManager, first int64, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id, e := newAsyncPilotEntity(t, first+int64(i), 1)
		if err := manager.TryAdd(e); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// REPRO-2026-09-26-05 §1 探针 A：两个 handler 以相反顺序新建 X、Y。修前两个快 worker 永久停在 RequireEntity；
// 修后冲突方整条回滚并重新准入，两请求都有确定结果（一个建成两者，另一个得到 ErrEntityExists），快池不被占满。
func TestHandlerCreateCrossOrderResolvesWithoutDeadlock(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 9850, 3)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 9860, entity.EntityCategory(1), createdInScopeKind)
	y := mustBuildCastID(t, 9861, entity.EntityCategory(1), createdInScopeKind)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerNumAndMsgCap(2, 1, 16))
	first := make(chan struct{}, 2)
	proceed := make(chan struct{})
	var attempts [2]atomic.Int64
	mk := func(slot int, a, b int64) BaseHandler {
		return func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
			n := attempts[slot].Add(1)
			if _, err := access.Create(createParam(a)); err != nil {
				return nil, err
			}
			if n == 1 {
				first <- struct{}{}
				<-proceed
			}
			if _, err := access.Create(createParam(b)); err != nil {
				return nil, err
			}
			return "ok", nil
		}
	}
	h1, h2, h3 := NewHandlerName("rr48_cross_1"), NewHandlerName("rr48_cross_2"), NewHandlerName("rr48_cross_unrelated")
	mgr.MustRegisterHandlerWithMeta(h1, mk(0, x, y), HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	mgr.MustRegisterHandlerWithMeta(h2, mk(1, y, x), HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	mgr.MustRegisterHandlerWithMeta(h3, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return "ok", nil }, HandlerMeta{})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	out := make(chan requestResult, 2)
	sendRequest(mgr, "h1", h1, ids[0], out)
	sendRequest(mgr, "h2", h2, ids[1], out)
	<-first
	<-first
	close(proceed)
	unrelated := make(chan requestResult, 1)
	sendRequest(mgr, "unrelated", h3, ids[2], unrelated)
	select {
	case r := <-unrelated:
		if r.err != nil {
			t.Fatalf("unrelated memory request failed: %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("unrelated request did not finish within 2s: fast workers are parked on created-entity locks (AB/BA on x=%d y=%d); stats=%+v", x, y, mgr.Stats().Queue.Fast)
	}
	results := map[string]requestResult{}
	for len(results) < 2 {
		select {
		case r := <-out:
			results[r.tag] = r
		case <-time.After(5 * time.Second):
			t.Fatalf("cross-order creates did not both finish within 5s: got %v; stats=%+v", results, mgr.Stats().Queue.Fast)
		}
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	winners, losers := 0, 0
	for tag, r := range results {
		switch {
		case r.err == nil && r.ret == "ok":
			winners++
		case errors.Is(r.err, entity.ErrEntityExists):
			losers++
		default:
			t.Errorf("%s: want ok or ErrEntityExists, got ret=%v err=%v", tag, r.ret, r.err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("want exactly one winner and one ErrEntityExists, got winners=%d losers=%d results=%v", winners, losers, results)
	}
	if attempts[0].Load()+attempts[1].Load() < 3 {
		t.Fatalf("the conflicting request was not re-admitted: attempts=%d/%d", attempts[0].Load(), attempts[1].Load())
	}
	for _, id := range []int64{x, y} {
		if e := manager.Get(id); e == nil || e.IsRemoved() {
			t.Fatalf("created entity %d missing after the winner committed", id)
		}
	}
	// 回复先于 worker 的收尾记账发出，给收尾一个有界窗口；停在锁上的 worker 不会在窗口内归零。
	idleBy := time.Now().Add(time.Second)
	for mgr.Stats().Queue.Fast.Running != 0 && time.Now().Before(idleBy) {
		time.Sleep(time.Millisecond)
	}
	if running := mgr.Stats().Queue.Fast.Running; running != 0 {
		t.Fatalf("fast workers still running 1s after both requests returned: %d", running)
	}
}

// 推断场景：H1 新建 X（与已持有同组）后 Cast 更高锁组的 Z；H2 声明 Z 后新建 X。修前 H1 等 Z、H2 等 X 成环；
// 修后 H2 的 X 只 try-lock，冲突时整条回滚（释放 Z）并重新准入，H1 取得 Z 提交，H2 重试得到 ErrEntityExists。
func TestHandlerCreateThenHigherGroupCastDoesNotFormCycle(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 9930, 1)
	zID := mustBuildCastID(t, 9931, castPlayerCategory, castPlayerKind)
	z := &rollbackTestEntity{EntityBase: entity.NewEntityBase(zID, castPlayerCategory, false, castPlayerKind), dao: &rollbackTestDao{id: zID, Value: 1}}
	if err := manager.TryAdd(z); err != nil {
		t.Fatal(err)
	}
	if entity.GetEntityGroup(zID) <= entity.GetEntityGroup(ids[0]) {
		t.Fatalf("fixture: Z must rank above the pilot group")
	}
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 9932, entity.EntityCategory(1), createdInScopeKind)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerNumAndMsgCap(2, 1, 16))
	h1Created := make(chan struct{})
	h2Holding := make(chan struct{})
	proceed := make(chan struct{})
	var h2Attempts atomic.Int64
	h1, h2 := NewHandlerName("rr48_create_cast_1"), NewHandlerName("rr48_create_cast_2")
	mgr.MustRegisterHandlerWithMeta(h1, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if _, err := access.Create(createParam(x)); err != nil {
			return nil, err
		}
		close(h1Created)
		<-proceed
		if _, err := CastOne[*rollbackTestEntity](zID); err != nil {
			return nil, err
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	mgr.MustRegisterHandlerWithMeta(h2, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if h2Attempts.Add(1) == 1 {
			close(h2Holding)
			<-proceed
		}
		if _, err := access.Create(createParam(x)); err != nil {
			return nil, err
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	out := make(chan requestResult, 2)
	sendRequest(mgr, "h1", h1, ids[0], out)
	<-h1Created
	sendRequest(mgr, "h2", h2, zID, out)
	<-h2Holding
	close(proceed)
	results := map[string]requestResult{}
	for len(results) < 2 {
		select {
		case r := <-out:
			results[r.tag] = r
		case <-time.After(3 * time.Second):
			t.Fatalf("Create+Cast across lock groups did not finish within 3s (cycle H1 waits Z / H2 waits X): got %v; stats=%+v", results, mgr.Stats().Queue.Fast)
		}
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	if r := results["h1"]; r.err != nil || r.ret != "ok" {
		t.Fatalf("h1: want ok, got ret=%v err=%v", r.ret, r.err)
	}
	if r := results["h2"]; !errors.Is(r.err, entity.ErrEntityExists) {
		t.Fatalf("h2: want ErrEntityExists after re-admission, got ret=%v err=%v", r.ret, r.err)
	}
	if n := h2Attempts.Load(); n < 2 {
		t.Fatalf("h2 attempts=%d, want >= 2 (the conflict was rolled back and re-admitted)", n)
	}
}

// REPRO-2026-09-26-05 §1 探针 B：A 新建 X 后停在 strict 提交里（锁内 WAL 准入，豁免）；声明不同目标的 handler
// 也新建 X。修前它们停在 X 的锁上占满快池；修后冲突即回滚并延迟重新准入，无关请求照常完成，A 提交后都得到“已存在”。
// memory 模式 handler 不回滚：冲突前的修改不撤销，所以不重排，调用方直接收到 ErrCreatedEntityLockConflict
// （RR-20260926-64 按维护者 2026-09-27 的决定改了这条子用例的期望；修前它会重排直到“已存在”）。
//
// OPEN-ITEMS B40：高负载下 memory 子用例偶发 `got ret=exists`。用例原先只等到每个 follower 的计数自增（handler 入口），
// 没等它的 Create 真正对 X 做出取锁判定：follower 进入 Create 后在 try-lock 之前被调度延迟，用例已放开 creator 的提交，
// creator 提交并放锁后 follower 才 try-lock，拿到锁、发布时发现 X 已存在——这是用例时序，不是框架缺陷（插桩与变异见
// bf-RR-20260926-64 §后续验证）。现在等每个 follower 的第一次 Create 返回（取锁判定已在 creator 持有 X 期间做出）再放开提交。
func TestSameIDCreateDoesNotOccupyFastPool(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}},
		{"memory", HandlerMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const workers = 3
			manager := entity.NewEntityManager()
			ids := addPilots(t, manager, 9870, workers+2)
			access := entity.NewManagerAccess(manager)
			x := mustBuildCastID(t, 9890, entity.EntityCategory(1), createdInScopeKind)
			committer := newBlockingCommitter()
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(workers, 1, 64))
			creator := NewHandlerName("rr48_same_id_creator_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(creator, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				value, err := access.Create(createParam(x))
				if err != nil {
					return nil, err
				}
				return "created", MarkPersist(value.(*rollbackTestEntity).dao, 1)
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
			var attempts, decided atomic.Int64
			follower := NewHandlerName("rr48_same_id_follower_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(follower, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				attempts.Add(1)
				_, err := access.Create(createParam(x))
				decided.Add(1) // Create 已返回：对 X 的取锁判定已做出（B40）
				if errors.Is(err, entity.ErrEntityExists) {
					return "exists", nil
				}
				return "created", err
			}, tc.meta)
			unrelated := NewHandlerName("rr48_same_id_unrelated_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(unrelated, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) { return "ok", nil }, HandlerMeta{})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			out := make(chan requestResult, workers+1)
			sendRequest(mgr, "A", creator, ids[0], out)
			<-committer.entered // A 持有 X 的锁，停在 strict 提交里
			for i := 1; i <= workers; i++ {
				sendRequest(mgr, fmt.Sprintf("B%d", i), follower, ids[i], out)
			}
			// 等到每个 follower 的第一次 Create 都已返回（creator 仍停在提交里）；-race 下多包并行时 1s 可能不够，
			// memory 子用例的断言依赖 follower 在 creator 提交之前对 X 做出取锁判定（RR-20260926-64）。只等 handler 入口不够：
			// Create 内的 try-lock 可能被调度到放开提交之后（OPEN-ITEMS B40）。
			deadline := time.Now().Add(10 * time.Second)
			for decided.Load() < workers && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if decided.Load() < workers {
				close(committer.release)
				t.Fatalf("followers' Create did not all return within 10s while the creator held X (attempts=%d decided=%d)", attempts.Load(), decided.Load())
			}
			done := make(chan requestResult, 1)
			sendRequest(mgr, "unrelated", unrelated, ids[workers+1], done)
			var blocked bool
			select {
			case <-done:
			case <-time.After(500 * time.Millisecond):
				blocked = true
			}
			close(committer.release)
			results := map[string]requestResult{}
			for len(results) < workers+1 {
				select {
				case r := <-out:
					results[r.tag] = r
				case <-time.After(5 * time.Second):
					t.Fatalf("creates did not finish after the commit was released: %v", results)
				}
			}
			if blocked {
				<-done
			}
			_ = mgr.Shutdown(context.Background())
			if blocked {
				t.Fatalf("same-id Create occupied all %d fast workers while the creator's strict commit was held (follower attempts=%d)", workers, attempts.Load())
			}
			if r := results["A"]; r.err != nil || r.ret != "created" {
				t.Fatalf("A: want created, got ret=%v err=%v", r.ret, r.err)
			}
			for i := 1; i <= workers; i++ {
				r := results[fmt.Sprintf("B%d", i)]
				if tc.meta.Rollback == RollbackNone {
					if !errors.Is(r.err, ErrCreatedEntityLockConflict) || errors.Is(r.err, ErrLockTimeout) {
						t.Fatalf("B%d (memory): want ErrCreatedEntityLockConflict without requeue, got ret=%v err=%v", i, r.ret, r.err)
					}
					continue
				}
				if r.err != nil || r.ret != "exists" {
					t.Fatalf("B%d: want exists after the creator committed, got ret=%v err=%v", i, r.ret, r.err)
				}
			}
			if tc.meta.Rollback == RollbackNone && attempts.Load() != workers {
				t.Fatalf("memory followers ran %d times, want %d (one each, no requeue)", attempts.Load(), workers)
			}
		})
	}
}

// 冲突后业务吞掉错误继续返回成功：RollbackState 事务仍整条回滚并重新准入，已修改的状态不重复生效。
func TestSwallowedCreateLockConflictStillRollsBack(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 9940, 2)
	target := manager.Get(ids[1]).(*rollbackTestEntity)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 9945, entity.EntityCategory(1), createdInScopeKind)
	committer := newBlockingCommitter()
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(2, 1, 16))
	creator := NewHandlerName("rr48_swallow_creator")
	mgr.MustRegisterHandlerWithMeta(creator, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		value, err := access.Create(createParam(x))
		if err != nil {
			return nil, err
		}
		return "created", MarkPersist(value.(*rollbackTestEntity).dao, 1)
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	var attempts atomic.Int64
	var mu sync.Mutex
	var createErrs []error
	firstAttemptDone := make(chan struct{})
	swallow := NewHandlerName("rr48_swallow")
	mgr.MustRegisterHandlerWithMeta(swallow, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		n := attempts.Add(1)
		e := es[0].(*rollbackTestEntity)
		e.dao.Value++
		_, err := access.Create(createParam(x)) // 业务忽略创建失败
		mu.Lock()
		createErrs = append(createErrs, err)
		mu.Unlock()
		if n == 1 {
			close(firstAttemptDone)
		}
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityMemory})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	out := make(chan requestResult, 2)
	sendRequest(mgr, "creator", creator, ids[0], out)
	<-committer.entered
	sendRequest(mgr, "swallow", swallow, ids[1], out)
	select {
	case <-firstAttemptDone:
	case <-time.After(time.Second):
		close(committer.release)
		t.Fatal("handler Create waited on a lock held by another transaction's strict commit instead of failing fast")
	}
	for attempts.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	close(committer.release)
	results := map[string]requestResult{}
	for len(results) < 2 {
		select {
		case r := <-out:
			results[r.tag] = r
		case <-time.After(5 * time.Second):
			t.Fatalf("requests did not finish: %v", results)
		}
	}
	if r := results["swallow"]; r.err != nil || r.ret != "ok" {
		t.Fatalf("swallow: want ok, got ret=%v err=%v", r.ret, r.err)
	}
	if target.dao.Value != 2 {
		t.Fatalf("target value=%d, want 2: a conflicted attempt was not rolled back before re-admission (attempts=%d)", target.dao.Value, attempts.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if !errors.Is(createErrs[0], ErrLockTimeout) {
		t.Fatalf("first attempt Create error=%v, want ErrLockTimeout", createErrs[0])
	}
	if last := createErrs[len(createErrs)-1]; !errors.Is(last, entity.ErrEntityExists) {
		t.Fatalf("final attempt Create error=%v, want ErrEntityExists", last)
	}
}

// REPRO-2026-09-26-05 §1b：memory 模式 handler 内经 ManagerAccess.Create（生成 Lifecycle 入口）新建实体，
// 锁在 handler 结束前由本 handler 持有，并进入本次 SyncMutation；对照 CreateInScope(当前作用域)。
func TestMemoryHandlerCreateHoldsLockUntilHandlerEnds(t *testing.T) {
	for i, via := range []string{"access.Create", "CreateInScope"} {
		t.Run(via, func(t *testing.T) {
			f := newCreateInScopeFixture(t, 9990+int64(i)*4, nil)
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			name := NewHandlerName("rr48_memory_create_" + via)
			var created entity.IThreadSafeEntity
			var lockedByOther, readyMidHandler bool
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				var err error
				if via == "CreateInScope" {
					created, err = f.access.CreateInScope(entity.CurrentGuardScope(), createParam(f.createdID))
				} else {
					created, err = f.access.Create(createParam(f.createdID))
				}
				if err != nil {
					return nil, err
				}
				lockedByOther = tryLockElsewhere(created)
				readyMidHandler = created.Base().Sync().SyncCommitReady()
				return "ok", nil
			}, HandlerMeta{})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()
			if _, err := mgr.Request(context.Background(), name, f.existID, nil); err != nil {
				t.Fatal(err)
			}
			if lockedByOther {
				t.Fatalf("%s in a memory handler: another goroutine locked the new entity while the handler was still running", via)
			}
			if readyMidHandler {
				t.Fatalf("%s in a memory handler: the new entity was outside the handler's sync commit barrier", via)
			}
			if !tryLockElsewhere(created) {
				t.Fatalf("%s: the new entity's lock was not released when the handler ended", via)
			}
			if !created.Base().Sync().SyncCommitReady() {
				t.Fatalf("%s: the new entity's sync barrier was not released after the handler committed", via)
			}
		})
	}
}

func tryLockElsewhere(e entity.IThreadSafeEntity) bool {
	done := make(chan bool)
	go func() {
		ok := e.GetMutex().TryLock()
		if ok {
			e.GetMutex().Unlock()
		}
		done <- ok
	}()
	return <-done
}
