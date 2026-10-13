// handler 内新建实体的锁冲突：按锁序 try-lock、冲突整条回滚并带抖动重新准入，捕获失败与撤销原因。

package nest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// handler 内新建实体的取锁遵循与 Cast 相同的锁序：新实体的锁组不高于已持有的锁组时只 try-lock，被占用时整条消息
// 回滚并以 ErrLockTimeout 重新准入，不在业务 worker 上无界等待；memory 模式 handler 内 Create 同样持锁到 handler 结束。
// 历史：RR-20260926-48。

// blockingCommitter 让第一次 Commit 停在 release 上（模拟锁内 WAL 准入，属于业务池等待豁免），其余立即返回。
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

// 两个 handler 以相反顺序新建 X、Y：冲突方整条回滚并重新准入，两请求都有确定结果（一个建成两者，另一个得到
// ErrEntityExists），业务池不被占满。
func TestHandlerCreateCrossOrderResolvesWithoutDeadlock(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 9850, 3)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 9860, entity.EntityCategory(1), createdInScopeKind)
	y := mustBuildCastID(t, 9861, entity.EntityCategory(1), createdInScopeKind)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
		t.Fatalf("unrelated request did not finish within 2s: business workers are parked on created-entity locks (AB/BA on x=%d y=%d); stats=%+v", x, y, mgr.Stats().Queue.Short)
	}
	results := map[string]requestResult{}
	for len(results) < 2 {
		select {
		case r := <-out:
			results[r.tag] = r
		case <-time.After(5 * time.Second):
			t.Fatalf("cross-order creates did not both finish within 5s: got %v; stats=%+v", results, mgr.Stats().Queue.Short)
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
	for mgr.Stats().Queue.Short.Running != 0 && time.Now().Before(idleBy) {
		time.Sleep(time.Millisecond)
	}
	if running := mgr.Stats().Queue.Short.Running; running != 0 {
		t.Fatalf("business workers still running 1s after both requests returned: %d", running)
	}
}

// H1 新建 X 后 Cast 更高锁组的 Z；H2 声明 Z 后新建 X：H2 的 X 只 try-lock，冲突时整条回滚（释放 Z）并重新准入，
// H1 取得 Z 提交，H2 重试得到 ErrEntityExists，不成环。
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
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
		if _, err := guardFixtureCastOne[*rollbackTestEntity](zID); err != nil {
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
			t.Fatalf("Create+Cast across lock groups did not finish within 3s (cycle H1 waits Z / H2 waits X): got %v; stats=%+v", results, mgr.Stats().Queue.Short)
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

// A 新建 X 后停在 strict 提交里（锁内 WAL 准入，属于豁免）；声明不同目标的 handler 也新建 X：冲突即回滚并延迟重新准入，
// 不占满业务池，无关请求照常完成，A 提交后都得到“已存在”。memory 模式 handler 不回滚，所以不重排，调用方直接收到
// ErrCreatedEntityLockConflict。用例等每个 follower 的第一次 Create 返回（取锁判定已在 creator 持有 X 期间做出）
// 再放开提交，避免把调度延迟误判为框架行为。
func TestSameIDCreateDoesNotOccupyShortPool(t *testing.T) {
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
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: workers, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			creator := NewHandlerName("rr48_same_id_creator_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(creator, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				value, err := access.Create(createParam(x))
				if err != nil {
					return nil, err
				}
				return "created", MarkPersist(value.(*rollbackTestEntity).dao, 1)
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
			var attempts atomic.Int64
			// decidedBy 按 follower（声明的 pilot）记“第一次 Create 已返回”（audit6 疑点 N32）：之前只数 Create 返回总次数，
			// state_strict 的 follower 锁冲突回滚后会重排再 Create，一个 follower 重试几次就凑够 workers，“每个 follower 都已进入
			// Create 并做出判定、占满业务池”的前提因此变弱。现在要求每个 follower 至少各一次。
			var decidedBy sync.Map
			decidedFollowers := func() int {
				n := 0
				decidedBy.Range(func(any, any) bool { n++; return true })
				return n
			}
			follower := NewHandlerName("rr48_same_id_follower_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(follower, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				attempts.Add(1)
				_, err := access.Create(createParam(x))
				decidedBy.Store(es[0].GUId(), struct{}{}) // 这个 follower 的 Create 已返回：对 X 的取锁判定已做出（B40）
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
			for decidedFollowers() < workers && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if n := decidedFollowers(); n < workers {
				close(committer.release)
				t.Fatalf("followers' Create did not all return within 10s while the creator held X (attempts=%d followers decided=%d/%d)", attempts.Load(), n, workers)
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
				t.Fatalf("same-id Create occupied all %d business workers while the creator's strict commit was held (follower attempts=%d)", workers, attempts.Load())
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
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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

// memory 模式 handler 内经 ManagerAccess.Create（生成 Lifecycle 入口）或 CreateInScope 新建实体：锁在 handler 结束前由
// 本 handler 持有，并进入本次 SyncMutation。
func TestMemoryHandlerCreateHoldsLockUntilHandlerEnds(t *testing.T) {
	for i, via := range []string{"access.Create", "CreateInScope"} {
		t.Run(via, func(t *testing.T) {
			f := newCreateInScopeFixture(t, 9990+int64(i)*4, nil)
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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

// 4 对交叉创建同时对称冲突：每对都在重排上限内解开（一方成功、一方 ErrEntityExists），没有请求以锁超时耗尽。
// 大样本统计（-count=500）不放进常规门禁；打破对称的确定性回归是本文件的重排抖动用例。
func TestSymmetricCrossCreatePairsResolveWithinRequeueBudget(t *testing.T) {
	const pairs = 4
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 9900, 2*pairs)
	access := entity.NewManagerAccess(manager)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordsCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 2 * pairs, QueueCap: 64}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	attempts := make([]atomic.Int64, 2*pairs)
	names := make([]HandlerName, 2*pairs)
	held := make(chan struct{}, 2*pairs)
	proceed := make([]chan struct{}, pairs)
	for p := range pairs {
		proceed[p] = make(chan struct{})
		x := mustBuildCastID(t, 9950+int64(2*p), entity.EntityCategory(1), createdInScopeKind)
		y := mustBuildCastID(t, 9951+int64(2*p), entity.EntityCategory(1), createdInScopeKind)
		for side := range 2 {
			slot, first, second := 2*p+side, x, y
			if side == 1 {
				first, second = y, x
			}
			names[slot] = NewHandlerName("b24_cross_" + strconv.Itoa(slot))
			gate := proceed[p]
			mgr.MustRegisterHandlerWithMeta(names[slot], func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				n := attempts[slot].Add(1)
				if _, err := access.Create(createParam(first)); err != nil {
					return nil, err
				}
				if n == 1 {
					held <- struct{}{} // 两侧都持有自己的第一个新实体后才建第二个：首轮必然对称冲突
					<-gate
				}
				if _, err := access.Create(createParam(second)); err != nil {
					return nil, err
				}
				return "ok", nil
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
		}
	}
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	// 失败要快速报出来：停机有上限；放行闸门的 defer 后注册、先执行，断言中途 t.Fatal 时停在 <-gate 的 handler 也会被放行。
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mgr.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()
	var openGates sync.Once
	releaseGates := func() {
		openGates.Do(func() {
			for _, gate := range proceed {
				close(gate)
			}
		})
	}
	defer releaseGates()
	out := make(chan requestResult, 2*pairs)
	for slot := range 2 * pairs {
		sendRequest(mgr, strconv.Itoa(slot), names[slot], ids[slot], out)
	}
	for range 2 * pairs {
		select {
		case <-held:
		case <-time.After(10 * time.Second):
			t.Fatal("not every handler reached its first created entity")
		}
	}
	releaseGates()
	results := make(map[string]requestResult, 2*pairs)
	for len(results) < 2*pairs {
		select {
		case r := <-out:
			results[r.tag] = r
		case <-time.After(30 * time.Second):
			t.Fatalf("symmetric cross creates did not all finish within 30s: %d/%d", len(results), 2*pairs)
		}
	}
	for p := range pairs {
		winners, losers := 0, 0
		for side := range 2 {
			slot := 2*p + side
			r := results[strconv.Itoa(slot)]
			switch {
			case r.err == nil && r.ret == "ok":
				winners++
			case errors.Is(r.err, entity.ErrEntityExists):
				losers++
			case errors.Is(r.err, ErrLockTimeout):
				t.Fatalf("pair %d slot %d exhausted the requeue budget after %d attempts: %v", p, slot, attempts[slot].Load(), r.err)
			default:
				t.Fatalf("pair %d slot %d: want ok or ErrEntityExists, got ret=%v err=%v", p, slot, r.ret, r.err)
			}
			if a := attempts[slot].Load(); a > entityGroupDispatchRequeueMax {
				t.Fatalf("pair %d slot %d ran %d times, above the requeue budget %d", p, slot, a, entityGroupDispatchRequeueMax)
			}
		}
		if winners != 1 || losers != 1 {
			t.Fatalf("pair %d: winners=%d losers=%d, want one each", p, winners, losers)
		}
	}
}

// 对称交叉创建靠“整条回滚 + 重新准入”解开，前提是冲突两侧下一轮不再同时开始：瞬时冲突的重排延迟带抖动。
// 不跑定时器：Dispatcher 不启动延迟循环，直接看 requeueNestDispatch 写进延迟堆的到期时刻。同一原因、同一重排次数的一批消息
// 每条延迟都不短于下限 entityGroupDispatchRequeueDelay，且至少有一对相差超过半个下限。
// 用 [调用前, 调用后] 夹住 time.Now，判定与本机调度快慢无关：固定延迟时 max(下界) − min(上界) ≤ 0，必红。
// 历史：U-0279。
func TestSymmetricTransientRequeuesAreNotReadmittedInLockstep(t *testing.T) {
	const batch = 64
	d := &Dispatcher{delayed: make(map[*delayedMsg]struct{})}
	mgr := &NestMgr{dispatcher: d}
	before := make([]time.Time, batch)
	after := make([]time.Time, batch)
	for i := range batch {
		msg := &Msg{Tid: int64(1000 + i), Type: MsgTypeSingle, PendingRequeues: 7}
		before[i] = time.Now()
		if !requeueNestDispatch(mgr, msg, "lock_timeout") {
			t.Fatalf("message %d was not requeued", i)
		}
		after[i] = time.Now()
	}
	if len(d.delayedHeap) != batch {
		t.Fatalf("delayed messages=%d, want %d", len(d.delayedHeap), batch)
	}
	due := make(map[int64]time.Time, batch)
	for _, dm := range d.delayedHeap {
		if dm.msg.PendingRequeues != 8 {
			t.Fatalf("requeued message %d carries PendingRequeues=%d, want 8", dm.msg.Tid, dm.msg.PendingRequeues)
		}
		due[dm.msg.Tid] = dm.due
	}
	var longestFloor, shortestCeil time.Duration
	for i := range batch {
		at, ok := due[int64(1000+i)]
		if !ok {
			t.Fatalf("message %d is missing from the delay queue", i)
		}
		floor, ceil := at.Sub(after[i]), at.Sub(before[i]) // 本条实际延迟落在 [floor, ceil]
		if ceil < entityGroupDispatchRequeueDelay {
			t.Fatalf("message %d was requeued after at most %v, shorter than the %v floor", i, ceil, entityGroupDispatchRequeueDelay)
		}
		if i == 0 || floor > longestFloor {
			longestFloor = floor
		}
		if i == 0 || ceil < shortestCeil {
			shortestCeil = ceil
		}
	}
	if spread := longestFloor - shortestCeil; spread < entityGroupDispatchRequeueDelay/2 {
		t.Fatalf("%d messages that hit the same conflict in the same round are re-admitted in lockstep: delays spread at most %v (shortest <= %v, longest >= %v), want > %v so symmetric conflicts break up",
			batch, max(spread, 0), shortestCeil, longestFloor, entityGroupDispatchRequeueDelay/2)
	}
}

// handler 内新建实体时事务捕获失败：与新建锁冲突同一种处理——捕获失败记在事务上，即使业务吞掉错误返回 nil，handler 结束时
// 也整条回滚，调用方拿到可 errors.Is 的捕获错误，committer 不被调用，新实体不发布。
// 历史：RR-20260927-11。

const captureFailingKind entity.EntityKind = 236

var errCreatedCaptureFailed = errors.New("test: created entity rollback capture failed")

// captureFailingEntity 的回滚捕获总是失败（RollbackState 下 CaptureEntities 调 RollbackParticipant.CaptureRollback）。
type captureFailingEntity struct {
	*rollbackTestEntity
}

func (*captureFailingEntity) CaptureRollback(*RollbackTx) error { return errCreatedCaptureFailed }

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: entity.EntityCategory(1),
		Kind:     captureFailingKind,
		Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &captureFailingEntity{&rollbackTestEntity{
				EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind),
				dao:        &rollbackTestDao{id: p.Id},
			}}, nil
		},
	})
}

func TestCreateCaptureFailureFailsTheTransactionEvenIfSwallowed(t *testing.T) {
	manager := entity.NewEntityManager()
	pilots := addPilots(t, manager, 38200, 1)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 38205, entity.EntityCategory(1), captureFailingKind)
	committer := &countingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("rr20260927_11_swallowed_capture_failure")
	var createErr error
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		pilot := es[0].(*rollbackTestEntity)
		pilot.dao.Value = 99
		if err := MarkPersist(pilot.dao, 1); err != nil {
			return nil, err
		}
		_, createErr = access.Create(&entity.EntityCreateParam{IsCreate: true, Id: x, Category: entity.EntityCategory(1), Kind: captureFailingKind})
		return "ok", nil // 业务吞掉新建失败
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()

	ret, err := mgr.Request(context.Background(), name, pilots[0], nil)
	if !errors.Is(createErr, errCreatedCaptureFailed) {
		t.Fatalf("fixture: Create in the handler = %v, want the capture failure", createErr)
	}
	if err == nil {
		t.Fatalf("the transaction committed although creating X failed to be captured and the handler swallowed it: ret=%v commits=%d pilot=%d",
			ret, committer.calls.Load(), manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value)
	}
	if !errors.Is(err, errCreatedCaptureFailed) {
		t.Fatalf("reply err=%v, want errors.Is the capture failure", err)
	}
	if errors.Is(err, ErrLockTimeout) {
		t.Fatalf("a capture failure must not be requeued as a lock timeout: %v", err)
	}
	if n := committer.calls.Load(); n != 0 {
		t.Fatalf("committer called %d times for a transaction that must fail", n)
	}
	if v := manager.Get(pilots[0]).(*rollbackTestEntity).dao.Value; v != 1 {
		t.Fatalf("declared entity value=%d after the forced rollback, want 1", v)
	}
	if got := manager.Get(x); got != nil {
		t.Fatalf("X is published after its capture failed: %v", got)
	}
}

// 事务回滚 / 拒绝时撤销 handler 内新建的实体，销毁回调收到 entity.DestroyReasonCreateRevoked：与业务 Destroy 的原因和
// Unload 的 DestroyReasonMemoryUnload 都不同，业务 OnDestroy 能分清“新建被撤销、从未成为权威”。
// 历史：RR-20260927-12。
func TestRevokedCreationIsDestroyedWithCreateRevokedReason(t *testing.T) {
	manager := entity.NewEntityManager()
	pilots := addPilots(t, manager, 38300, 1)
	access := entity.NewManagerAccess(manager)
	x := mustBuildCastID(t, 38305, entity.EntityCategory(1), createdInScopeKind)
	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	name := NewHandlerName("rr20260927_12_revoke_reason")
	boom := errors.New("business failed after creating X")
	var reasons []entity.EntityDestroyReason
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		created, err := access.Create(createParam(x))
		if err != nil {
			return nil, err
		}
		created.Base().SetHooks(nil, func(reason entity.EntityDestroyReason) { reasons = append(reasons, reason) })
		return nil, boom
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mgr.Shutdown(context.Background()) }()
	if _, err := mgr.Request(context.Background(), name, pilots[0], nil); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the business error", err)
	}
	if manager.Get(x) != nil {
		t.Fatal("fixture: the created entity must have been revoked")
	}
	if len(reasons) != 1 || reasons[0] == entity.DestroyReasonCommon || reasons[0] == entity.DestroyReasonMemoryUnload {
		t.Fatalf("revoked creation destroyed with reasons %v; want exactly one call with a reason distinct from DestroyReasonCommon (%d) and DestroyReasonMemoryUnload (%d)",
			reasons, entity.DestroyReasonCommon, entity.DestroyReasonMemoryUnload)
	}
	if reasons[0] != entity.DestroyReasonCreateRevoked {
		t.Fatalf("revoked creation destroyed with reason %d, want DestroyReasonCreateRevoked (%d)", reasons[0], entity.DestroyReasonCreateRevoked)
	}
}
