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
	waker    Waker
	owner    entity.IThreadSafeEntity
}

func (a *probeAction) Kind() ActionKind { return 1 }
func (a *probeAction) Start(ctx *ActionContext) error {
	a.waker, a.owner = ctx.Waker, ctx.Owner
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

// 需要周期 tick 的动作在途时挂心跳，周期取最短的那个（按档向下取）；由事件推进的动作（TickInterval 0）不挂；
// 一个都没有时停，驱动上不再有这个登记。
func TestBeatRunnerHeartbeatFollowsTheActionsThatNeedTicks(t *testing.T) {
	b, d, _ := newBeatRunnerForTest(t, map[ActionKind]ActionGroup{1: 1, 2: 2, 3: 3})
	var log []string
	state := func() string { return fmt.Sprintf("%v %v %d", b.Reg().Active(), b.Reg().Period(), d.Registered()) }

	event := &intervalAction{probeAction{name: "event", interval: 0, log: &log}}
	if _, err := b.Runner().Start(3, event, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "false 100ms 0" {
		t.Fatalf("event-driven action only: %s", got)
	}
	slow := &intervalAction{probeAction{name: "slow", interval: 700 * time.Millisecond, log: &log}}
	if _, err := b.Runner().Start(2, slow, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "true 500ms 1" {
		t.Fatalf("700ms action: %s, want the 500ms tier", got)
	}
	plain := &probeAction{name: "plain", log: &log}
	if _, err := b.Runner().Start(1, plain, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "true 100ms 1" {
		t.Fatalf("plus a default action: %s", got)
	}
	if err := b.Runner().End(1, true, NewActionReason("done")); err != nil {
		t.Fatal(err)
	}
	if err := b.Runner().End(2, true, NewActionReason("done")); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "false 500ms 0" || b.Ticking() {
		t.Fatalf("only the event-driven action left: %s", got)
	}
}

// 每拍按组号顺序 tick 需要周期 tick 的组，上下文带本拍 Elapsed、Waker 与实体；由事件推进的动作不被拍 tick，Wake
// 马上 tick 它的组（Elapsed 为 0）；动作结束后心跳停。
func TestBeatTicksTheGroupsInOrderAndWakeTicksAnEventDrivenAction(t *testing.T) {
	b, d, e := newBeatRunnerForTest(t, map[ActionKind]ActionGroup{1: 1, 2: 2, 3: 3})
	var log []string
	second := &probeAction{name: "second", finishAt: 2, log: &log}
	first := &probeAction{name: "first", finishAt: 1, log: &log}
	event := &intervalAction{probeAction{name: "event", finishAt: 1, log: &log}}
	for kind, a := range map[ActionKind]Action{2: second, 1: first, 3: event} {
		if _, err := b.Runner().Start(kind, a, 0, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if event.waker != b || first.owner != e {
		t.Fatalf("context: waker %v owner %v", event.waker, first.owner)
	}
	for _, elapsed := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond} {
		if err := b.beat(heartbeat.Beat{Now: time.Unix(1_800_000_000, 0), Elapsed: elapsed}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"first tick 1 elapsed 100ms", "second tick 1 elapsed 100ms", "second tick 2 elapsed 300ms"}
	if !slices.Equal(log, want) {
		t.Fatalf("ticks %v, want %v", log, want)
	}
	if b.Reg().Active() || d.Registered() != 0 {
		t.Fatalf("all periodic actions ended but the heartbeat is active %v registered %d", b.Reg().Active(), d.Registered())
	}
	if err := event.waker.Wake(event); err != nil {
		t.Fatal(err)
	}
	if last := log[len(log)-1]; last != "event tick 1 elapsed 0s" || b.Runner().Current(3) != nil {
		t.Fatalf("wake: log %v current %v", log, b.Runner().Current(3))
	}
	if err := event.waker.Wake(event); err == nil {
		t.Fatal("waking an action that is no longer current succeeded")
	}
}
