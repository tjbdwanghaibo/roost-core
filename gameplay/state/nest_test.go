package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// 真实 Nest 引擎 + 启动的心跳驱动：状态随事务回滚（两种回滚策略），持久状态进提交记录、不持久的不进；Gate 经 Behavior
// 计数冻结动作组，打断模式取消在途动作并停掉组的心跳。

const kindHolder entity.EntityKind = 243

const (
	actWalk actionflow.ActionKind = 1
	actCast actionflow.ActionKind = 2

	groupMove actionflow.ActionGroup = 1
	groupCast actionflow.ActionGroup = 2
)

func init() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kindHolder, Category: entity.EntityCategoryOther, BusinessPool: entity.BusinessPoolShort})
}

var (
	runUndo  = nest.NewHandlerName("state_test.undo")
	runState = nest.NewHandlerName("state_test.state")
	runMem   = nest.NewHandlerName("state_test.memory")
)

func init() {
	run := func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		return nil, params[0].(func(*holder) error)(es[0].(*holder))
	}
	nest.MustRegisterHandlerWithMeta(runUndo, run, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
	nest.MustRegisterHandlerWithMeta(runState, run, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityAsync})
	nest.MustRegisterHandlerWithMeta(runMem, run, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityMemory})
}

// holder 是装了状态组件与 Behavior 的实体；StateDao 登记为它的 DAO（rollback=state 的快照与持久都经它）。
type holder struct {
	*entity.EntityBase
	host     *heartbeat.Host
	set      *Set
	behavior *actionflow.Behavior
	gate     *Gated
	// unlisted 为 true 时 RangeDao 不交出 StateDao（接线时漏登记进 DaoManager）。
	unlisted bool
	mu       sync.Mutex
	ended    []string
	errors   []error
}

func (h *holder) Base() *entity.EntityBase { return h.EntityBase }
func (h *holder) RangeDao(f func(entity.DaoInterface)) {
	if f != nil && !h.unlisted {
		f(h.set.Dao())
	}
}
func (h *holder) OnDestroy(entity.EntityDestroyReason) { h.host.OnDestroy() }

func (h *holder) endedActions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ended...)
}

// walkAction 一直走（每 100ms tick 一次），直到被结束。
type walkAction struct{ kind actionflow.ActionKind }

func (a *walkAction) Kind() actionflow.ActionKind { return a.kind }
func (a *walkAction) Start(*actionflow.ActionContext) (bool, actionflow.ActionResult, error) {
	return false, actionflow.ActionResult{}, nil
}
func (a *walkAction) Tick(*actionflow.ActionContext) (bool, actionflow.ActionResult) {
	return false, actionflow.ActionResult{}
}
func (a *walkAction) Cancel(*actionflow.ActionContext, string) {}

type recordingCommitter struct {
	mu      sync.Mutex
	records []nest.CommitRecord
}

func (c *recordingCommitter) Commit(_ context.Context, record nest.CommitRecord) error {
	c.mu.Lock()
	c.records = append(c.records, record)
	c.mu.Unlock()
	return nil
}

func (c *recordingCommitter) take() []nest.CommitRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.records
	c.records = nil
	return out
}

type fixture struct {
	t         *testing.T
	engine    *nest.NestMgr
	manager   *entity.EntityManager
	driver    *heartbeat.Driver
	committer *recordingCommitter
	catalog   *Catalog
	registry  *actionflow.Registry
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	manager := entity.NewEntityManager()
	committer := &recordingCommitter{}
	engine := nest.NewEngine(
		nest.NestOptionWithGetter(entity.NewManagerAccess(manager)),
		nest.NestOptionWithTransactionCommitter(committer),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}),
	)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	driver := heartbeat.NewDriver(heartbeat.Config{Name: "state-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := driver.Bind(engine, manager.Get); err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Stop(context.Background()) })
	registry := actionflow.NewRegistry()
	for _, kind := range []actionflow.ActionKind{actWalk, actCast} {
		if err := registry.RegisterAction(kind, func(any) (actionflow.Action, error) { return &walkAction{kind: kind}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{t: t, engine: engine, manager: manager, driver: driver, committer: committer, catalog: testCatalog(t), registry: registry}
}

// add 建实体：状态组件、Behavior（心跳驱动）与门控 rooted → 移动组打断、stunned / silenced → 施法组暂停。
func (f *fixture) add(unique int64) *holder {
	f.t.Helper()
	id, err := entity.BuildEntityID(unique, kindHolder)
	if err != nil {
		f.t.Fatal(err)
	}
	h := &holder{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryOther, false, kindHolder)}
	h.host = f.driver.NewHost(h)
	h.set = NewSet(NewStateDao(id, "game", f.catalog), SetConfig{OnError: func(err error) { f.t.Errorf("state error: %v", err) }})
	h.behavior, err = actionflow.NewBehavior(h, actionflow.BehaviorConfig{
		Registry: f.registry,
		GroupForKind: func(kind actionflow.ActionKind) (actionflow.ActionGroup, bool) {
			return map[actionflow.ActionKind]actionflow.ActionGroup{actWalk: groupMove, actCast: groupCast}[kind], true
		},
		Beat:     &actionflow.BeatConfig{Host: h.host},
		Observer: endedRecorder{h},
		OnError:  func(err error) { h.mu.Lock(); h.errors = append(h.errors, err); h.mu.Unlock() },
	})
	if err != nil {
		f.t.Fatal(err)
	}
	h.gate, err = Gate(h.set, h.behavior, func(err error) { f.t.Errorf("gate error: %v", err) },
		Rule{State: Rooted, Group: groupMove, Mode: actionflow.FreezeCancel},
		Rule{State: Stunned, Group: groupCast, Mode: actionflow.FreezePause},
		Rule{State: Silenced, Group: groupCast, Mode: actionflow.FreezePause},
	)
	if err != nil {
		f.t.Fatal(err)
	}
	f.manager.Add(h)
	return h
}

type endedRecorder struct{ h *holder }

func (r endedRecorder) OnActionEnd(id int64, _ actionflow.ActionKind, reason actionflow.ActionReason) {
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	result := reason.ToActionResult(actionflow.ActionStatusSuccess)
	r.h.ended = append(r.h.ended, fmt.Sprintf("%d %d %s", id, result.Status, result.Reason))
}
func (endedRecorder) OnMissionEnd(actionflow.Mission, actionflow.ActionReason) {}

func (f *fixture) run(handler nest.HandlerName, h *holder, fn func(*holder) error) error {
	_, err := f.engine.Request(context.Background(), handler, h.ID(), nest.NewParams(fn))
	return err
}

func (f *fixture) mustRun(handler nest.HandlerName, h *holder, fn func(*holder) error) {
	f.t.Helper()
	if err := f.run(handler, h, fn); err != nil {
		f.t.Fatal(err)
	}
}

// settle 让 h 上已经开始的消息（含提交后回调：心跳启停生效）收尾，见 movement 的同名方法。
func (f *fixture) settle(h *holder) { f.mustRun(runUndo, h, func(*holder) error { return nil }) }

var errBusiness = errors.New("business refused")

// 状态随事务回滚，两种回滚策略都一样；提交的改动里只有声明了 Persist 的状态进提交记录（不持久的
// 在 memory 持久级别下也能提交），声明了 Sync 的标同步脏。
func TestStatesFollowTheTransaction(t *testing.T) {
	f := newFixture(t)
	h := f.add(1)
	for _, handler := range []nest.HandlerName{runUndo, runState} {
		if err := f.run(handler, h, func(h *holder) error {
			for _, step := range []error{h.set.Add(Rooted, buff1), h.set.Add(Protected, action), h.set.Add(Manual, action)} {
				if step != nil {
					return step
				}
			}
			return errBusiness
		}); !errors.Is(err, errBusiness) {
			t.Fatalf("%s: %v", handler.String(), err)
		}
		if got := read(f, h, func(h *holder) []ID { return h.set.Active() }); len(got) != 0 {
			t.Fatalf("%s: states survived the rollback: %v", handler.String(), got)
		}
	}
	f.committer.take()

	f.mustRun(runMem, h, func(h *holder) error { return h.set.Add(Rooted, buff1) })
	if records := f.committer.take(); mutationsOf(records) != 0 {
		t.Fatalf("a memory-only state produced %d mutations", mutationsOf(records))
	}
	if !h.set.Dao().DirtyTracker().HasSyncDirty() {
		t.Fatal("a synced state change left no sync dirty bit")
	}
	if err := f.run(runMem, h, func(h *holder) error { return h.set.Add(Protected, action) }); err == nil {
		t.Fatal("a persistent state committed under durability=memory")
	}
	f.mustRun(runUndo, h, func(h *holder) error { return h.set.Add(Protected, action) })
	records := f.committer.take()
	if mutationsOf(records) != 1 {
		t.Fatalf("persistent state: %d mutations, want 1", mutationsOf(records))
	}
	m := records[len(records)-1].Mutations[0]
	if m.Mask != FieldStates || m.Kind != dataengine.MutationPut || m.Key.Resource != CollectionName {
		t.Fatalf("mutation %+v", m)
	}
}

// rollback=state 靠 Nest 对实体 DaoManager 里的 DAO 拍快照恢复；StateDao 没登记进去时快照里没有它，回滚恢复不了
// 状态。这时改状态必须当场失败（不改、返回错误），不能在 handler 失败后静默留下（定身之类的控制状态残留）。
// rollback=undo 不依赖登记（逆操作挂在 DAO 上），照常回滚。
func TestAnUnlistedStateDaoRefusesChangesUnderRollbackState(t *testing.T) {
	f := newFixture(t)
	h := f.add(2)
	h.unlisted = true
	var addErr error
	err := f.run(runState, h, func(h *holder) error {
		if addErr = h.set.Add(Rooted, buff1); addErr != nil {
			return addErr
		}
		return errBusiness
	})
	if got := read(f, h, func(h *holder) []ID { return h.set.Active() }); !errors.Is(addErr, ErrRollbackNotCaptured) || len(got) != 0 {
		t.Fatalf("rollback=state with an unlisted StateDao: Add = %v, handler = %v, states after the rollback %v "+
			"(want ErrRollbackNotCaptured and nothing left)", addErr, err, got)
	}
	// rollback=undo 不依赖登记：照常改、照常回滚。
	if err := f.run(runUndo, h, func(h *holder) error {
		if err := h.set.Add(Rooted, buff1); err != nil {
			return err
		}
		return errBusiness
	}); !errors.Is(err, errBusiness) {
		t.Fatalf("rollback=undo with an unlisted StateDao: %v", err)
	}
	if got := read(f, h, func(h *holder) []ID { return h.set.Active() }); len(got) != 0 {
		t.Fatalf("rollback=undo left %v", got)
	}
}

func mutationsOf(records []nest.CommitRecord) int {
	n := 0
	for _, r := range records {
		n += len(r.Mutations)
	}
	return n
}

func read[T any](f *fixture, h *holder, fn func(*holder) T) T {
	f.t.Helper()
	var out T
	f.mustRun(runUndo, h, func(h *holder) error { out = fn(h); return nil })
	return out
}

// 门控（经真实 Nest 与心跳）：rooted 生效时打断移动组的在途动作、停掉组的心跳，拒绝新的移动；两个来源都撤掉
// 才解冻。stunned 与 silenced 各自冻结施法组（暂停），计数：撤掉一个仍冻结。
func TestGateFreezesActionGroupsWhileStatesHold(t *testing.T) {
	f := newFixture(t)
	h := f.add(2)
	f.mustRun(runUndo, h, func(h *holder) error {
		if _, err := h.behavior.CreateAction(actWalk, nil); err != nil {
			return err
		}
		_, err := h.behavior.CreateAction(actCast, nil)
		return err
	})
	f.settle(h)
	if f.driver.Registered() != 2 {
		t.Fatalf("two running actions: registered %d", f.driver.Registered())
	}

	f.mustRun(runUndo, h, func(h *holder) error {
		if err := h.set.Add(Rooted, buff1); err != nil {
			return err
		}
		return h.set.Add(Rooted, buff2)
	})
	f.settle(h)
	if got := h.endedActions(); len(got) != 1 || got[0] != "1 4 action group frozen" || f.driver.Registered() != 1 {
		t.Fatalf("rooted: ended %v registered %d", got, f.driver.Registered())
	}
	if err := f.run(runUndo, h, func(h *holder) error { _, err := h.behavior.CreateAction(actWalk, nil); return err }); !errors.Is(err, actionflow.ErrActionGroupFrozen) {
		t.Fatalf("walk while rooted: %v", err)
	}
	f.mustRun(runUndo, h, func(h *holder) error { return h.set.Remove(Rooted, buff1) })
	if frozen := read(f, h, func(h *holder) bool { return h.behavior.Frozen(groupMove) }); !frozen {
		t.Fatal("one of two rooting sources removed: the move group thawed")
	}
	f.mustRun(runUndo, h, func(h *holder) error { return h.set.Remove(Rooted, buff2) })
	if frozen := read(f, h, func(h *holder) bool { return h.behavior.Frozen(groupMove) }); frozen {
		t.Fatal("all rooting sources removed: the move group is still frozen")
	}

	f.mustRun(runUndo, h, func(h *holder) error {
		if err := h.set.Add(Stunned, buff1); err != nil {
			return err
		}
		return h.set.Add(Silenced, buff2)
	})
	f.settle(h)
	count := read(f, h, func(h *holder) int { return h.behavior.Runner().FreezeCount(groupCast) })
	if count != 2 || f.driver.Registered() != 0 || len(h.endedActions()) != 1 {
		t.Fatalf("stunned + silenced: freeze count %d registered %d ended %v", count, f.driver.Registered(), h.endedActions())
	}
	f.mustRun(runUndo, h, func(h *holder) error { return h.set.Remove(Stunned, buff1) })
	f.mustRun(runUndo, h, func(h *holder) error { return h.set.Remove(Silenced, buff2) })
	f.settle(h)
	if count := read(f, h, func(h *holder) int { return h.behavior.Runner().FreezeCount(groupCast) }); count != 0 || f.driver.Registered() != 1 {
		t.Fatalf("thawed cast group: count %d registered %d (the paused cast resumes beating)", count, f.driver.Registered())
	}
}

// 门控与回滚：状态随回滚撤销而冻结计数不回滚（runner 不随事务回滚）；下一次任何状态改动、或 Refresh，按差纠正——
// 每条规则至多持有一次冻结，不会累积。
func TestGateRealignsAfterARolledBackStateChange(t *testing.T) {
	f := newFixture(t)
	h := f.add(3)
	if err := f.run(runUndo, h, func(h *holder) error {
		if err := h.set.Add(Rooted, buff1); err != nil {
			return err
		}
		return errBusiness
	}); !errors.Is(err, errBusiness) {
		t.Fatal(err)
	}
	if rooted, frozen := read2(f, h); rooted || !frozen {
		t.Fatalf("after the rollback: rooted %v frozen %v (documented: the freeze stays until the next notification)", rooted, frozen)
	}
	f.mustRun(runUndo, h, func(h *holder) error { return h.set.Add(Manual, action) })
	if _, frozen := read2(f, h); frozen {
		t.Fatal("an unrelated state change did not realign the gate")
	}

	// 同样的回滚，用 Refresh 立即纠正；再加一次 rooted 后计数只有 1。
	_ = f.run(runUndo, h, func(h *holder) error { _ = h.set.Add(Rooted, buff1); return errBusiness })
	f.mustRun(runUndo, h, func(h *holder) error { h.set.Refresh(); return nil })
	if _, frozen := read2(f, h); frozen {
		t.Fatal("Refresh did not realign the gate")
	}
	_ = f.run(runUndo, h, func(h *holder) error { _ = h.set.Add(Rooted, buff1); return errBusiness })
	f.mustRun(runUndo, h, func(h *holder) error { return h.set.Add(Rooted, buff2) })
	if count := read(f, h, func(h *holder) int { return h.behavior.Runner().FreezeCount(groupMove) }); count != 1 {
		t.Fatalf("freeze count %d after a rolled-back and a committed rooting, want 1", count)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.errors) != 0 {
		t.Fatalf("behavior errors %v", h.errors)
	}
}

func read2(f *fixture, h *holder) (rooted, frozen bool) {
	f.t.Helper()
	f.mustRun(runUndo, h, func(h *holder) error {
		rooted, frozen = h.set.Has(Rooted), h.behavior.Frozen(groupMove)
		return nil
	})
	return rooted, frozen
}

// Gate 装上时按当前状态对齐，Close 撤销它持有的全部冻结。
func TestGateAlignsWhenInstalledAndReleasesOnClose(t *testing.T) {
	f := newFixture(t)
	h := f.add(4)
	f.mustRun(runUndo, h, func(h *holder) error {
		h.gate.Close()
		if err := h.set.Add(Rooted, buff1); err != nil {
			return err
		}
		if h.behavior.Frozen(groupMove) {
			return errors.New("a closed gate still froze the move group")
		}
		var err error
		h.gate, err = Gate(h.set, h.behavior, nil, Rule{State: Rooted, Group: groupMove})
		if err != nil {
			return err
		}
		if !h.behavior.Frozen(groupMove) {
			return errors.New("a gate installed while rooted did not freeze")
		}
		h.gate.Close()
		if h.behavior.Frozen(groupMove) {
			return errors.New("Close kept the freeze")
		}
		return nil
	})
	if _, err := Gate(h.set, h.behavior, nil, Rule{State: 99}); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("gate on an unknown state: %v", err)
	}
}
