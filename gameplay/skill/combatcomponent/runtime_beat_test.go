package combatcomponent

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

// 技能 Runtime 的心跳适配：推进规则本身在 skill.Stepper，推进器层的用例在 gameplay/skill/stepper_test.go；
// 这里经心跳适配层（拍回调、Reg 的启停、输入侧取 fctx.Now）跑同样的抖动——不经 Nest，直接按给定的时刻调用拍回调，
// 输入的时刻经可控的业务时钟给出。真实心跳驱动下的启停见 beat_test.go。

const beatFireballJSON = `{"schema":"roost.skill/v2","id":"skill.test.beat.fireball","name":"Fireball","description":"Windup, commit, burn.",
"activation":{"type":"active","policy":{"mode":"tap"},"cast_window":{"windup_ticks":3,"commit_tick":2,"recovery_ticks":2,"movement":"locked","turning":"allowed","interrupt_tags":[],"refund_before_commit":true}},
"input_schema":{"type":"entity"},"cooldown_ticks":20,"global_cooldown_ticks":8,"costs":[{"resource":"mana","amount":5}],"memory":{},"initial_phase":"cast",
"phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":25,"damage_type":"magic","element":"fire"}},{"flow":"finish","reason":"done"}]}}}]}`

const beatBurnJSON = `{"schema":"roost.skill/v2","id":"skill.test.beat.burn","name":"Burn","description":"Damage over time.",
"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast",
"phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"repeat","times":5,"interval_ticks":3,"index_as":"i","do":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":2,"damage_type":"physical"}}},{"flow":"finish"}]}}}]}`

func compileBeatProgram(t *testing.T, input string) (*skill.Program, skill.CompileEnvironment) {
	t.Helper()
	definition, err := skill.Parse([]byte(strings.ReplaceAll(input, "\n", "")))
	if err != nil {
		t.Fatal(err)
	}
	environment := skill.DefaultCompileEnvironment()
	program, diagnostics := skill.Compile(definition, environment)
	if program == nil {
		t.Fatalf("compile: %v", diagnostics)
	}
	return program, environment
}

// tickLogHost 在 Host 每次被推进时记一行：推进到的 tick、Host revision 与目标生命。逐 tick 的结果就是这份记录。
type tickLogHost struct {
	*skill.MemoryHost
	log *[]string
}

func (h tickLogHost) Advance(tick skill.Tick) (skill.WorldRevision, error) {
	revision, err := h.MemoryHost.Advance(tick)
	*h.log = append(*h.log, fmt.Sprintf("t%d r%d hp%d mana%d", tick, revision, h.HealthForTest(2), h.ResourceForTest(1, "mana")))
	return revision, err
}

type beatWorld struct {
	beat *RuntimeBeat
	host tickLogHost
	log  []string
	reg  *heartbeat.Reg
	// now 是业务时钟（pinBusinessClock）：handler 侧的输入经 fctx.Now 读到它。
	now *atomic.Int64
}

func (w *beatWorld) setNow(at time.Time) { w.now.Store(at.UnixNano()) }

// newBeatWorld 构造一个由 RuntimeBeat 驱动的 Runtime：心跳宿主来自一个不启动的驱动（登记与启停照常记账，没有投递），
// 拍由测试直接调用。
func newBeatWorld(t *testing.T, clock skill.TickClock, environment skill.CompileEnvironment, config RuntimeBeatConfig) *beatWorld {
	t.Helper()
	w := &beatWorld{now: pinBusinessClock(t, time.Unix(0, 0))}
	memory := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	memory.ConfigureGameplayCatalog(environment.Gameplay)
	memory.UpsertEntity(skill.MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}})
	memory.UpsertEntity(skill.MemoryEntity{ID: 2, Alive: true, Health: 500, MaxHealth: 500})
	memory.UpsertEntity(skill.MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100})
	w.host = tickLogHost{MemoryHost: memory, log: &w.log}
	owner, _ := newCombatTestEntity(t, 9300)
	config.Host = heartbeat.NewDriver(heartbeat.Config{Name: "runtime-beat-unit"}).NewHost(owner)
	config.Clock = clock
	if config.OnError == nil {
		config.OnError = func(err error) { t.Errorf("runtime beat: %v", err) }
	}
	beat, err := NewRuntimeBeat(skill.NewRuntime(w.host, skill.RuntimeOptions{}), config)
	if err != nil {
		t.Fatal(err)
	}
	w.beat, w.reg = beat, beat.Reg()
	return w
}

// beatInput 是落在某个 tick 里的一次输入（Offset 是它在 tick 内的位置，各个抖动配置下不同）。
type beatInput struct {
	Tick   skill.Tick
	Caster skill.EntityID
	Burn   bool
}

// runJitterProfile 按一种心跳抖动驱动同一组输入，返回逐 tick 记录与每次输入的结果。
func runJitterProfile(t *testing.T, seed int64, gaps func(*rand.Rand) time.Duration) (ticks, inputs []string, stats skill.StepperStats) {
	t.Helper()
	fireball, environment := compileBeatProgram(t, beatFireballJSON)
	burn, _ := compileBeatProgram(t, beatBurnJSON)
	origin := time.Unix(1_800_000_000, 0)
	clock := skill.TickClock{Length: 50 * time.Millisecond, Origin: origin}
	w := newBeatWorld(t, clock, environment, RuntimeBeatConfig{})
	rng := rand.New(rand.NewSource(seed))

	type event struct {
		at    time.Time
		input *beatInput
	}
	var events []event
	for at := origin; at.Before(origin.Add(8 * time.Second)); at = at.Add(gaps(rng)) {
		events = append(events, event{at: at})
		if rng.Intn(8) == 0 { // 重复投递同一时刻（同一拍的两份）与晚到的拍（时刻早于上一拍）
			events = append(events, event{at: at}, event{at: at.Add(-time.Duration(rng.Intn(120)) * time.Millisecond)})
		}
	}
	plan := []beatInput{{Tick: 2, Caster: 1}, {Tick: 6, Caster: 3, Burn: true}, {Tick: 15, Caster: 1}, {Tick: 31, Caster: 1}, {Tick: 33, Caster: 3, Burn: true}, {Tick: 120, Caster: 1}}
	for i := range plan {
		offset := time.Duration(rng.Int63n(int64(clock.Length)))
		events = append(events, event{at: clock.StartOf(plan[i].Tick).Add(offset), input: &plan[i]})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })

	for _, e := range events {
		if e.input == nil {
			if err := w.beat.beat(heartbeat.Beat{Now: e.at}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		w.setNow(e.at)
		program := fireball
		if e.input.Burn {
			program = burn
		}
		id, err := w.beat.Start(program, skill.CastInput{Caster: e.input.Caster, Target: 2})
		inputs = append(inputs, fmt.Sprintf("tick %d applied at %d: cast %d err %v active %v", e.input.Tick, w.beat.Runtime().CurrentTick(), id, err, w.reg.Active()))
	}
	return w.log, inputs, w.beat.Stats()
}

// 同一输入序列（每个输入落在同一个 tick 上，tick 内的位置随机）在不同的心跳抖动下逐 tick 相同：Host 每个 tick 被推进
// 一次、按顺序；空闲点由 Runtime 决定；空闲之后由输入跳到输入的 tick。四种抖动：每 tick 一拍、每两 tick 一拍、
// 10～400ms 随机间隔（含重复与晚到的拍），以及接近追帧上限的长间隔。
func TestRuntimeBeatResultsDoNotDependOnHeartbeatJitter(t *testing.T) {
	profiles := []struct {
		name string
		gaps func(*rand.Rand) time.Duration
	}{
		{"every tick", func(*rand.Rand) time.Duration { return 50 * time.Millisecond }},
		{"every other tick", func(*rand.Rand) time.Duration { return 100 * time.Millisecond }},
		{"random 10-400ms", func(r *rand.Rand) time.Duration { return time.Duration(10+r.Intn(390)) * time.Millisecond }},
		{"long gaps", func(r *rand.Rand) time.Duration { return time.Duration(600+r.Intn(390)) * time.Millisecond }},
	}
	var wantTicks, wantInputs []string
	for i, p := range profiles {
		for seed := int64(1); seed <= 5; seed++ {
			ticks, inputs, stats := runJitterProfile(t, seed, p.gaps)
			if stats.Jumps != 0 {
				t.Fatalf("%s seed %d: %d jumps past the catch-up limit, want none: %+v", p.name, seed, stats.Jumps, stats)
			}
			if i == 0 && seed == 1 {
				wantTicks, wantInputs = ticks, inputs
				continue
			}
			if !slices.Equal(inputs, wantInputs) {
				t.Fatalf("%s seed %d: inputs\n%s\nwant\n%s", p.name, seed, strings.Join(inputs, "\n"), strings.Join(wantInputs, "\n"))
			}
			if !slices.Equal(ticks, wantTicks) {
				t.Fatalf("%s seed %d: per-tick results differ\n got %v\nwant %v", p.name, seed, ticks, wantTicks)
			}
		}
	}
	// 记录本身要有内容：tick 2 的输入把新 Runtime 一次推到 2，之后逐 tick 推进；燃烧在 tick 18 结束、空闲点停住，
	// 空闲区间（19～30、46～119）不推进，tick 31 与 120 的输入各一次跳过去。
	joined := " " + strings.Join(wantTicks, " ") + " "
	for _, want := range []string{"t2", "t3", "t4", "t18", "t31", "t45", "t120"} {
		if !strings.Contains(joined, " "+want+" ") {
			t.Fatalf("per-tick log %v lacks %q", wantTicks, want)
		}
	}
	for _, idle := range []string{"t1", "t19", "t30", "t46", "t100"} {
		if strings.Contains(joined, " "+idle+" ") {
			t.Fatalf("the idle tick %s was stepped: %v", idle, wantTicks)
		}
	}
	if !strings.Contains(strings.Join(wantInputs, "\n"), "tick 15 applied at 15: cast 0 err skill:") {
		t.Fatalf("the fireball on cooldown at tick 15 was not refused at tick 15: %v", wantInputs)
	}
}

// 追帧上限：落后超过上限时先逐 tick 推进上限那么多，剩下的一次跳到目标，计 Jumps；Runtime 停在目标 tick。
func TestRuntimeBeatJumpsPastTheCatchUpLimit(t *testing.T) {
	burn, environment := compileBeatProgram(t, strings.Replace(beatBurnJSON, `"times":5`, `"times":60`, 1)) // 忙 180 tick
	origin := time.Unix(1_800_000_000, 0)
	clock := skill.TickClock{Length: 50 * time.Millisecond, Origin: origin}
	w := newBeatWorld(t, clock, environment, RuntimeBeatConfig{Period: 100 * time.Millisecond}) // 上限 max(1s, 100ms) = 20 tick
	w.setNow(origin)
	if _, err := w.beat.Start(burn, skill.CastInput{Caster: 3, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if err := w.beat.beat(heartbeat.Beat{Now: clock.StartOf(10)}); err != nil {
		t.Fatal(err)
	}
	if got := w.beat.Stats(); got.Steps != 10 || got.Jumps != 0 {
		t.Fatalf("10 ticks behind: %+v, want 10 steps and no jump", got)
	}
	if err := w.beat.beat(heartbeat.Beat{Now: clock.StartOf(110)}); err != nil {
		t.Fatal(err)
	}
	got := w.beat.Stats()
	if got.Jumps != 1 || got.JumpedTicks != 80 || w.beat.Runtime().CurrentTick() != 110 {
		t.Fatalf("100 ticks behind: %+v at tick %d, want 20 more steps, one jump over 80 ticks, at tick 110", got, w.beat.Runtime().CurrentTick())
	}
	if got.Steps > 30 {
		t.Fatalf("stepped %d ticks, the limit is 20 per beat", got.Steps)
	}
}

// 心跳与输入的启停：有工作时挂上，推进到空闲后停；空闲 Runtime 的拍不推进（tick 停在空闲点）。
func TestRuntimeBeatRegistersWhileBusyAndStopsAtTheIdlePoint(t *testing.T) {
	fireball, environment := compileBeatProgram(t, beatFireballJSON)
	origin := time.Unix(1_800_000_000, 0)
	clock := skill.TickClock{Length: 50 * time.Millisecond, Origin: origin}
	w := newBeatWorld(t, clock, environment, RuntimeBeatConfig{})
	if w.reg.Active() || w.reg.Period() != 50*time.Millisecond {
		t.Fatalf("a new runtime beat: active %v period %v, want stopped at the tier of one tick", w.reg.Active(), w.reg.Period())
	}
	w.setNow(clock.StartOf(3))
	if _, err := w.beat.Start(fireball, skill.CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if !w.reg.Active() || w.beat.Runtime().CurrentTick() != 3 {
		t.Fatalf("after Start: active %v tick %d, want the beat running from tick 3", w.reg.Active(), w.beat.Runtime().CurrentTick())
	}
	if err := w.beat.beat(heartbeat.Beat{Now: clock.StartOf(40)}); err != nil {
		t.Fatal(err)
	}
	idle := w.beat.Runtime().CurrentTick()
	if w.reg.Active() || idle != 8 {
		t.Fatalf("after the cast: active %v tick %d, want stopped at the idle point 8 (windup 3 + recovery 2 from tick 3)", w.reg.Active(), idle)
	}
	if err := w.beat.beat(heartbeat.Beat{Now: clock.StartOf(60)}); err != nil || w.beat.Runtime().CurrentTick() != idle {
		t.Fatalf("a beat on an idle runtime moved it to %d (%v)", w.beat.Runtime().CurrentTick(), err)
	}
	w.setNow(clock.StartOf(70))
	if err := w.beat.CatchUp(); err != nil || w.beat.Runtime().CurrentTick() != 70 || w.beat.Stats().Idle != 2 {
		t.Fatalf("CatchUp on an idle runtime: tick %d stats %+v (%v), want one more idle jump (the first was Start's 0 -> 3) to 70", w.beat.Runtime().CurrentTick(), w.beat.Stats(), err)
	}
}
