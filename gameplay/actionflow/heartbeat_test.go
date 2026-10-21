package actionflow

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
)

// BeatRunner 的用例直接调用心跳回调（驱动不启动、不在事务里：启停立即生效），按登记状态、tick 顺序与上下文断言。
// 经真实 Nest 与心跳驱动的用例在 gameplay/movement（runner 由心跳 tick、空闲停、移动动作由事件推进）。

// probeAction 记下每次 tick 看到的上下文；interval 为负表示不实现 TickIntervaler。
type probeAction struct {
	name     string
	interval time.Duration
	finishAt int
	log      *[]string
	ticks    int
	owner    entity.IThreadSafeEntity
}

func (a *probeAction) Kind() ActionKind { return 1 }
func (a *probeAction) Start(ctx *ActionContext) error {
	a.owner = ctx.Owner
	return nil
}
func (a *probeAction) Cancel(*ActionContext, string) {}
func (a *probeAction) Tick(ctx *ActionContext) (bool, ActionResult) {
	a.ticks++
	*a.log = append(*a.log, fmt.Sprintf("%s tick %d elapsed %v", a.name, a.ticks, ctx.Elapsed))
	return a.finishAt > 0 && a.ticks >= a.finishAt, ActionResult{}
}

// intervalAction 是实现了 TickIntervaler 的 probeAction。
type intervalAction struct{ probeAction }

func (a *intervalAction) TickInterval() time.Duration { return a.interval }

type beatEntity struct{ *entity.EntityBase }

func (e *beatEntity) Base() *entity.EntityBase { return e.EntityBase }

func newBeatRunnerForTest(t *testing.T, groups map[ActionKind]ActionGroup) (*BeatRunner, *heartbeat.Driver, *beatEntity) {
	t.Helper()
	d := heartbeat.NewDriver(heartbeat.Config{Name: "actionflow-test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	e := &beatEntity{entity.NewEntityBase(1<<20, entity.EntityCategoryOther, true)}
	registry := NewRegistry()
	for kind := range groups {
		if err := registry.RegisterAction(kind, func(param any) (Action, error) { return param.(Action), nil }); err != nil {
			t.Fatal(err)
		}
	}
	b, err := NewBeatRunner(ActionRunnerConfig{
		Registry:     registry,
		GroupForKind: func(kind ActionKind) (ActionGroup, bool) { g, ok := groups[kind]; return g, ok },
		Hooks:        ActionRunnerHooks{OnError: func(err error) { t.Errorf("runner error: %v", err) }},
	}, BeatConfig{Host: d.NewHost(e)})
	if err != nil {
		t.Fatal(err)
	}
	return b, d, e
}

// 每个组的当前动作按自己的周期挂这个组的心跳（参考 ssr：每个组一个登记），周期按档向下取；不需要周期 tick 的动作
// （TickInterval 0）不挂；组空闲时停，驱动上不再有这个登记。
func TestEachGroupBeatsAtItsCurrentActionsInterval(t *testing.T) {
	b, d, _ := newBeatRunnerForTest(t, map[ActionKind]ActionGroup{1: 1, 2: 2, 3: 3})
	var log []string
	state := func(group ActionGroup) string {
		reg := b.Reg(group)
		if reg == nil {
			return "none"
		}
		return fmt.Sprintf("%v %v", reg.Active(), reg.Period())
	}

	event := &intervalAction{probeAction{name: "event", interval: 0, log: &log}}
	if _, err := b.Runner().Start(3, event, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := state(3); got != "false 100ms" || d.Registered() != 0 {
		t.Fatalf("action that needs no ticks: %s registered %d", got, d.Registered())
	}
	slow := &intervalAction{probeAction{name: "slow", interval: 700 * time.Millisecond, log: &log}}
	if _, err := b.Runner().Start(2, slow, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	plain := &probeAction{name: "plain", log: &log}
	if _, err := b.Runner().Start(1, plain, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(state(1), " / ", state(2)); got != "true 100ms / true 500ms" || d.Registered() != 2 || !b.Ticking() {
		t.Fatalf("groups 1 / 2: %s registered %d, want the default tier and the 500ms tier", got, d.Registered())
	}
	if err := b.Runner().End(1, true, NewActionReason("done")); err != nil {
		t.Fatal(err)
	}
	if err := b.Runner().End(2, true, NewActionReason("done")); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(state(1), " / ", state(2)); got != "false 100ms / false 500ms" || d.Registered() != 0 || b.Ticking() {
		t.Fatalf("idle groups: %s registered %d", got, d.Registered())
	}
}

// 一个组的一拍只 tick 这个组，上下文带本拍 Elapsed 与实体；动作结束后这个组的心跳停，下一个排队动作启动时再挂。
func TestAGroupsBeatTicksOnlyThatGroupWithItsElapsed(t *testing.T) {
	b, d, e := newBeatRunnerForTest(t, map[ActionKind]ActionGroup{1: 1, 2: 2})
	var log []string
	first := &probeAction{name: "first", finishAt: 1, log: &log}
	queued := &probeAction{name: "queued", finishAt: 1, log: &log}
	second := &probeAction{name: "second", finishAt: 2, log: &log}
	if _, err := b.Runner().Start(1, first, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Runner().Enqueue(1, queued, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Runner().Start(2, second, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if first.owner != e {
		t.Fatalf("context owner %v", first.owner)
	}
	at := time.Unix(1_800_000_000, 0)
	for _, step := range []struct {
		group   ActionGroup
		elapsed time.Duration
	}{{2, 300 * time.Millisecond}, {1, 100 * time.Millisecond}, {1, 200 * time.Millisecond}, {2, 100 * time.Millisecond}} {
		if err := b.beat(step.group, heartbeat.Beat{Now: at, Elapsed: step.elapsed}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"second tick 1 elapsed 300ms", "first tick 1 elapsed 100ms", "queued tick 1 elapsed 200ms", "second tick 2 elapsed 100ms"}
	if !slices.Equal(log, want) {
		t.Fatalf("ticks %v, want %v", log, want)
	}
	if b.Ticking() || d.Registered() != 0 {
		t.Fatalf("all actions ended but registered %d", d.Registered())
	}
}
