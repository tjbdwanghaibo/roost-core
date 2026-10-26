package level

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

var register sync.Once

type testEntity struct {
	*entity.EntityBase
	entity.DaoManager
	component *LevelComponent
}

func (e *testEntity) Base() *entity.EntityBase { return e.EntityBase }

type getter struct{ e *testEntity }

func (g getter) Get(context.Context, int64, entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return g.e, nil
}
func (g getter) GetMany(_ context.Context, ids []int64, _ []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	out := make([]entity.IThreadSafeEntity, len(ids))
	for i := range ids {
		out[i] = g.e
	}
	return out, nil
}

type harness struct {
	c      *LevelComponent
	dao    *LevelDAO
	engine *nest.NestMgr
	id     int64
	name   nest.HandlerName
}
type committer struct{ reject bool }

func (c *committer) Commit(context.Context, nest.CommitRecord) error {
	if c.reject {
		return errors.New("reject commit")
	}
	return nil
}
func newHarness(t *testing.T, p *Program, policy nest.RollbackPolicy, persistent bool, commit *committer) *harness {
	t.Helper()
	register.Do(func() { entity.MustRegisterEntityKindCategory(238, 1) })
	id, err := entity.BuildEntityID(1, 238)
	if err != nil {
		t.Fatal(err)
	}
	dao := NewTransientLevelDAO(id)
	durability := nest.DurabilityMemory
	if persistent {
		dao = NewLevelDAO(id, "game")
		durability = nest.DurabilityStrict
	}
	c, err := NewLevelComponent(dao, p)
	if err != nil {
		t.Fatal(err)
	}
	e := &testEntity{EntityBase: entity.NewEntityBase(id, 1, !persistent, 238), DaoManager: entity.NewDaoManager(), component: c}
	e.DaoManager.Set(CollectionName, dao)
	opts := []nest.NestOption{nest.NestOptionWithGetter(getter{e}), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 8})}
	if commit != nil {
		opts = append(opts, nest.NestOptionWithTransactionCommitter(commit))
	}
	engine := nest.NewEngine(opts...)
	name := nest.NewHandlerName("level.test.command")
	err = engine.RegisterHandlerWithMeta(name, func(_ []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
		return nil, args[0].(func(*LevelComponent) error)(c)
	}, nest.HandlerMeta{Rollback: policy, Durability: durability})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return &harness{c, dao, engine, id, name}
}
func (h *harness) run(fn func(*LevelComponent) error) error {
	_, err := h.engine.RequestSettled(context.Background(), h.name, h.id, nest.NewParams(fn))
	return err
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func compiled(t *testing.T, d Definition, r Registry) *Program {
	t.Helper()
	p, err := Compile(d, r)
	must(t, err)
	return p
}
func baseDefinition() Definition {
	return Definition{Version: 1, Name: "generic", Revision: 1, TimeDomain: "tick", Initial: []string{"a"}, Variables: []Variable{{Scope: Global, Name: "count", Default: Int(0), Public: true}, {Scope: Global, Name: "secret", Default: String("server")}}, Stages: []StageDefinition{{ID: "a", Triggers: []TriggerDefinition{{ID: "count", Event: "hit", Actions: []Action{{Kind: "add", Scope: Global, Name: "count", Value: Literal(Int(1))}}}}}}}
}
func baseRegistry() Registry {
	return Registry{Events: map[string]map[string]ValueKind{"hit": {}}, Actions: map[string]ActionSpec{}}
}
func initStart(c *LevelComponent) error {
	if err := c.Initialize("instance", math.MaxUint64, 0); err != nil {
		return err
	}
	return c.Start(0)
}
func hit(seq uint64) Event {
	return Event{Instance: "instance", Epoch: math.MaxUint64, Source: "server", Sequence: seq, Kind: "hit"}
}

func TestComponentTransactionRollbackAndAtomicEventBudget(t *testing.T) {
	for _, policy := range []nest.RollbackPolicy{nest.RollbackUndo, nest.RollbackState} {
		t.Run(policy.String(), func(t *testing.T) {
			h := newHarness(t, compiled(t, baseDefinition(), baseRegistry()), policy, false, nil)
			must(t, h.run(initStart))
			before := h.c.Snapshot()
			h.dao.CleanDirty()
			err := h.run(func(c *LevelComponent) error {
				if err := c.Handle(hit(1), 1); err != nil {
					return err
				}
				return errors.New("handler failure")
			})
			if err == nil {
				t.Fatal("missing failure")
			}
			if !reflect.DeepEqual(before, h.c.Snapshot()) || h.dao.DirtyTracker().HasSyncDirty() {
				t.Fatal("rollback leaked state or dirty bits")
			}
			must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(1), 1) }))
			if h.c.Project().Variables["global/count"].Int != 1 {
				t.Fatal("event not applied")
			}
		})
	}
	d := baseDefinition()
	d.Limits = DefaultLimits()
	d.Limits.Steps = 4
	d.Stages[0].Triggers[0].Actions = append(d.Stages[0].Triggers[0].Actions, Action{Kind: "add", Scope: Global, Name: "count", Value: Literal(Int(1))}, Action{Kind: "add", Scope: Global, Name: "count", Value: Literal(Int(1))})
	h := newHarness(t, compiled(t, d, baseRegistry()), nest.RollbackUndo, false, nil)
	must(t, h.run(initStart))
	before := h.c.Snapshot()
	// 调用者即使吞掉预算错误，也不能提交半个事件。
	must(t, h.run(func(c *LevelComponent) error {
		if !errors.Is(c.Handle(hit(1), 1), ErrBudget) {
			return errors.New("expected budget rejection")
		}
		return nil
	}))
	if !reflect.DeepEqual(before, h.c.Snapshot()) {
		t.Fatal("partial event committed")
	}
}
func TestRetryProjectionAndDefinitionOwnership(t *testing.T) {
	d, r := baseDefinition(), baseRegistry()
	p := compiled(t, d, r)
	d.Stages[0].Triggers[0].Actions[0].Value = Literal(Int(999))
	r.Events["hit"]["injected"] = Text
	h := newHarness(t, p, nest.RollbackUndo, false, nil)
	if err := h.c.Initialize("outside", 1, 0); !errors.Is(err, nest.ErrTransactionClosed) {
		t.Fatal("outside transaction mutation", err)
	}
	must(t, h.run(initStart))
	must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(1), 1) }))
	revision := h.c.Snapshot().Revision
	must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(1), 1) }))
	if h.c.Snapshot().Revision != revision {
		t.Fatal("duplicate wrote state")
	}
	event := hit(1)
	event.Stage = "a"
	event.Generation = 1
	if err := h.run(func(c *LevelComponent) error { return c.Handle(event, 2) }); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting sequence", err)
	}
	must(t, h.run(func(c *LevelComponent) error { return c.Handle(hit(2), 2) }))
	if err := h.run(func(c *LevelComponent) error { return c.Handle(hit(1), 3) }); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	view := h.c.Project()
	if view.Variables["global/count"].Int != 2 {
		t.Fatal(view)
	}
	if _, ok := view.Variables["global/secret"]; ok {
		t.Fatal("private variable leaked")
	}
	view.Variables["global/count"] = Int(100)
	if h.c.Project().Variables["global/count"].Int != 2 {
		t.Fatal("view alias")
	}
	before := h.c.Snapshot().Revision
	must(t, h.run(func(c *LevelComponent) error { return c.AdvanceTick(100) }))
	if h.c.Snapshot().Revision != before {
		t.Fatal("idle clock dirtied state")
	}
}
func TestPersistentCommitRejectionAndColdRoundTrip(t *testing.T) {
	for _, policy := range []nest.RollbackPolicy{nest.RollbackUndo, nest.RollbackState} {
		t.Run(policy.String(), func(t *testing.T) {
			commit := &committer{}
			p := compiled(t, baseDefinition(), baseRegistry())
			h := newHarness(t, p, policy, true, commit)
			must(t, h.run(initStart))
			raw, schema, err := h.dao.MarshalPersisted()
			must(t, err)
			commit.reject = true
			err = h.run(func(c *LevelComponent) error { return c.Handle(hit(1), 1) })
			if err == nil {
				t.Fatal("commit rejection lost")
			}
			after, _, err := h.dao.MarshalPersisted()
			must(t, err)
			if !bytes.Equal(raw, after) {
				t.Fatal("rejected commit changed state")
			}
			restored := NewLevelDAO(h.id, "game")
			must(t, restored.RestorePersisted(raw, schema, 7))
			c, err := NewLevelComponent(restored, p)
			must(t, err)
			if !reflect.DeepEqual(c.Snapshot(), h.c.Snapshot()) {
				t.Fatal("cold state mismatch")
			}
			changed := baseDefinition()
			changed.Revision++
			if _, err = NewLevelComponent(restored, compiled(t, changed, baseRegistry())); !errors.Is(err, ErrDefinition) {
				t.Fatal("configuration silently changed", err)
			}
			raw[0] ^= 1
			if err = restored.RestorePersisted(raw, schema, 8); err == nil {
				t.Fatal("bad persistence accepted")
			}
		})
	}
}
