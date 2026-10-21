package movement

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 测试用实体种类（本测试二进制内唯一）。
const kindMover entity.EntityKind = 241

// 测试用动作种类与组：两个移动动作同组（新的取代旧的），tickAction 在另一组。
const (
	actMoveTo actionflow.ActionKind = 1
	actFollow actionflow.ActionKind = 2
	actTick   actionflow.ActionKind = 3

	groupMove actionflow.ActionGroup = 1
	groupWork actionflow.ActionGroup = 2
)

func init() {
	entity.MustRegisterEntityKindDefs(
		entity.EntityKindDef{Kind: kindMover, Category: entity.EntityCategoryOther, BusinessPool: entity.BusinessPoolShort},
	)
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func mustID(t testing.TB, unique int64) int64 {
	t.Helper()
	id, err := entity.BuildEntityID(unique, kindMover)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// mover 是装了 MapBasic、Move 与心跳驱动的 runner 的实体，与业务实体的接法相同（见 Example）。
type mover struct {
	*entity.EntityBase
	host   *heartbeat.Host
	fields *MemFields
	mb     *MapBasic
	move   *Move
	beat   *actionflow.BeatRunner

	mu    sync.Mutex // ended 由 worker 写、测试 goroutine 读
	ended []endedAction
}

func (e *mover) endedActions() []endedAction {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]endedAction(nil), e.ended...)
}

type endedAction struct {
	id     int64
	kind   actionflow.ActionKind
	status actionflow.ActionStatus
	reason string
}

func (e *mover) Base() *entity.EntityBase { return e.EntityBase }
func (e *mover) MapBasic() *MapBasic      { return e.mb }
func (e *mover) MoveComponent() *Move     { return e.move }
func (e *mover) OnDestroy(entity.EntityDestroyReason) {
	e.mb.OnDestroy()
	e.host.OnDestroy()
}

type moverOptions struct {
	terrain spatial.Terrain
	locate  Locator
	save    func(int64, spatial.Point, Facing) error
	every   time.Duration
	onLeave bool
	onSpace func(string, error)
	sync    *entity.SubjectSyncState
	fields  *MemFields
}

func newMover(t testing.TB, d *heartbeat.Driver, unique int64, registry *actionflow.Registry, o moverOptions) *mover {
	t.Helper()
	if o.fields == nil {
		o.fields = &MemFields{}
	}
	e := &mover{EntityBase: entity.NewEntityBase(mustID(t, unique), entity.EntityCategoryOther, true, kindMover), fields: o.fields}
	if o.sync != nil {
		e.SetSyncState(o.sync)
		e.fields.OnChange = func() { o.sync.MarkDirty(1) }
	}
	e.host = d.NewHost(e)
	var err error
	e.mb, err = NewMapBasic(e, e.host, MapConfig{Fields: e.fields, Save: o.save, SaveEvery: o.every, SaveOnLeave: o.onLeave, OnSpaceError: o.onSpace})
	if err != nil {
		t.Fatal(err)
	}
	e.move, err = NewMove(e.mb, MoveConfig{Terrain: o.terrain, Locate: o.locate})
	if err != nil {
		t.Fatal(err)
	}
	if registry != nil {
		e.beat, err = actionflow.NewBeatRunner(actionflow.ActionRunnerConfig{
			Registry:     registry,
			GroupForKind: groupForKind,
			Hooks: actionflow.ActionRunnerHooks{
				OnEnded: func(s actionflow.ActionSnapshot, reason actionflow.ActionReason) {
					result := reason.ToActionResult(actionflow.ActionStatusSuccess)
					e.mu.Lock()
					defer e.mu.Unlock()
					e.ended = append(e.ended, endedAction{id: s.ID, kind: s.Kind, status: result.Status, reason: result.Reason})
				},
				OnError: func(err error) { t.Errorf("runner error: %v", err) },
			},
		}, actionflow.BeatConfig{Host: e.host})
		if err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func groupForKind(kind actionflow.ActionKind) (actionflow.ActionGroup, bool) {
	switch kind {
	case actMoveTo, actFollow:
		return groupMove, true
	case actTick:
		return groupWork, true
	}
	return 0, false
}

// newRegistry 注册两个移动动作与一个按心跳 tick 的计数动作。
func newRegistry(t testing.TB) *actionflow.Registry {
	t.Helper()
	registry := actionflow.NewRegistry()
	if err := RegisterActions(registry, actMoveTo, actFollow); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterAction(actTick, func(param any) (actionflow.Action, error) {
		return param.(*tickAction), nil
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

// tickAction 每次被 tick 记一次（含 Elapsed、是否在事务里），第 limit 次结束。
type tickAction struct {
	limit int
	mu    sync.Mutex
	ticks []tickSeen
}

func (a *tickAction) seen() []tickSeen {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]tickSeen(nil), a.ticks...)
}

type tickSeen struct {
	elapsed time.Duration
	inTx    bool
}

func (a *tickAction) Kind() actionflow.ActionKind { return actTick }
func (a *tickAction) Start(*actionflow.ActionContext) (bool, actionflow.ActionResult, error) {
	return false, actionflow.ActionResult{}, nil
}
func (a *tickAction) Cancel(*actionflow.ActionContext, string) {}
func (a *tickAction) Tick(ctx *actionflow.ActionContext) (bool, actionflow.ActionResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ticks = append(a.ticks, tickSeen{elapsed: ctx.Elapsed, inTx: nest.CurrentRollbackTx() != nil})
	return len(a.ticks) >= a.limit, actionflow.ActionResult{}
}

// spaceLog 是记录调用顺序的 Space（不经 AOI）。
type spaceLog struct {
	mu        sync.Mutex
	ops       []string
	refuseAll error
}

func (s *spaceLog) Enter(e entity.IThreadSafeEntity, at spatial.Point) error {
	return s.add(fmt.Sprintf("enter %d,%d", at.X, at.Y))
}
func (s *spaceLog) QueueMove(e entity.IThreadSafeEntity, at spatial.Point) error {
	if s.refuseAll != nil {
		return s.refuseAll
	}
	return s.add(fmt.Sprintf("move %d,%d", at.X, at.Y))
}
func (s *spaceLog) Leave(e entity.IThreadSafeEntity) error { return s.add("leave") }

func (s *spaceLog) add(op string) error {
	s.mu.Lock()
	s.ops = append(s.ops, op)
	s.mu.Unlock()
	return nil
}

func (s *spaceLog) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.ops
	s.ops = nil
	return out
}

// fakeClock 是可控的业务时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// beatOf 是测试直接调用回调时交的一拍。
func beatOf(elapsed time.Duration) heartbeat.Beat {
	return heartbeat.Beat{Now: time.Unix(1_800_000_000, 0), Elapsed: elapsed, Period: DefaultMovePeriod}
}

type nullCommitter struct{}

func (nullCommitter) Commit(context.Context, dataengine.CommitRecord) error { return nil }

var errBusiness = errors.New("business refused")

// waitFor 轮询条件（只用于等异步执行完成，不断言耗时）。
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
