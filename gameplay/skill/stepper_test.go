package skill

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// Stepper（D20；维护者 2026-10-21：心跳只是驱动方式之一）：推进规则在推进器层直接测——不经 Nest、不经心跳，驱动
// 由测试扮演：手动驱动按给定的时刻调 StepTo（模拟心跳的拍间隔、跳拍、重复、晚到），定步长循环按帧号调 StepToTick。
// 心跳适配层（combatcomponent.NewRuntimeBeat）的用例在 combatcomponent。

const stepperFireballJSON = `{"schema":"roost.skill/v2","id":"skill.test.stepper.fireball","name":"Fireball","description":"Windup, commit, burn.",
"activation":{"type":"active","policy":{"mode":"tap"},"cast_window":{"windup_ticks":3,"commit_tick":2,"recovery_ticks":2,"movement":"locked","turning":"allowed","interrupt_tags":[],"refund_before_commit":true}},
"input_schema":{"type":"entity"},"cooldown_ticks":20,"global_cooldown_ticks":8,"costs":[{"resource":"mana","amount":5}],"memory":{},"initial_phase":"cast",
"phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":25,"damage_type":"magic","element":"fire"}},{"flow":"finish","reason":"done"}]}}}]}`

const stepperBurnJSON = `{"schema":"roost.skill/v2","id":"skill.test.stepper.burn","name":"Burn","description":"Damage over time.",
"activation":{"type":"active","policy":{"mode":"tap"}},"input_schema":{"type":"entity"},"cooldown_ticks":0,"costs":[],"memory":{},"initial_phase":"cast",
"phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"repeat","times":5,"interval_ticks":3,"index_as":"i","do":{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":2,"damage_type":"physical"}}},{"flow":"finish"}]}}}]}`

func compileStepperProgram(t *testing.T, input string) (*Program, CompileEnvironment) {
	t.Helper()
	return compileRuntimeJSON(t, strings.ReplaceAll(input, "\n", ""))
}

// stepperLogHost 在 Host 每次被推进时记一行：推进到的 tick、Host revision、目标生命与施法者法力。逐 tick 的结果就是
// 这份记录。
type stepperLogHost struct {
	*MemoryHost
	log *[]string
}

func (h stepperLogHost) Advance(tick Tick) (WorldRevision, error) {
	revision, err := h.MemoryHost.Advance(tick)
	*h.log = append(*h.log, fmt.Sprintf("t%d r%d hp%d mana%d", tick, revision, h.HealthForTest(2), h.ResourceForTest(1, "mana")))
	return revision, err
}

// manualDriver 是测试用的手动驱动：记下 Stepper 的通知（电平式，同一状态可能连续多次），awake 是最后一次通知的
// 结果。wakeErr 非 nil 时 Wake 返回它且不醒（扮演挂不上的驱动）。
type manualDriver struct {
	awake   bool
	wakes   int
	sleeps  int
	wakeErr error
}

func (d *manualDriver) Wake() error {
	d.wakes++
	if d.wakeErr != nil {
		return d.wakeErr
	}
	d.awake = true
	return nil
}

func (d *manualDriver) Sleep() { d.sleeps++; d.awake = false }

type stepperWorld struct {
	stepper *Stepper
	driver  *manualDriver
	log     []string
	// inputAt 是输入侧的“现在”（StepperConfig.Now）。
	inputAt time.Time
	// awake 读驱动是否醒着（输入结果里记一笔）；缺省读 manualDriver。
	awake func() bool
}

func newStepperWorld(t *testing.T, clock TickClock, environment CompileEnvironment, config StepperConfig) *stepperWorld {
	t.Helper()
	w := &stepperWorld{driver: &manualDriver{}}
	w.awake = func() bool { return w.driver.awake }
	memory := NewMemoryHost(AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	memory.ConfigureGameplayCatalog(environment.Gameplay)
	memory.UpsertEntity(MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}})
	memory.UpsertEntity(MemoryEntity{ID: 2, Alive: true, Health: 500, MaxHealth: 500})
	memory.UpsertEntity(MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100})
	config.Clock = clock
	config.Now = func() time.Time { return w.inputAt }
	if config.Driver == nil {
		config.Driver = w.driver
	}
	stepper, err := NewStepper(NewRuntime(stepperLogHost{MemoryHost: memory, log: &w.log}, RuntimeOptions{}), config)
	if err != nil {
		t.Fatal(err)
	}
	w.stepper = stepper
	return w
}

// stepperInput 是落在某个 tick 里的一次输入。
type stepperInput struct {
	Tick   Tick
	Caster EntityID
	Burn   bool
}

var stepperPlan = []stepperInput{{Tick: 2, Caster: 1}, {Tick: 6, Caster: 3, Burn: true}, {Tick: 15, Caster: 1}, {Tick: 31, Caster: 1}, {Tick: 33, Caster: 3, Burn: true}, {Tick: 120, Caster: 1}}

var stepperOrigin = time.Unix(1_800_000_000, 0)

var stepperClock = TickClock{Length: 50 * time.Millisecond, Origin: stepperOrigin}

func (w *stepperWorld) start(t *testing.T, fireball, burn *Program, input stepperInput) string {
	t.Helper()
	program := fireball
	if input.Burn {
		program = burn
	}
	id, err := w.stepper.Start(program, CastInput{Caster: input.Caster, Target: 2})
	return fmt.Sprintf("tick %d applied at %d: cast %d err %v awake %v", input.Tick, w.stepper.Runtime().CurrentTick(), id, err, w.awake())
}

// runJitteredDriver 按一种驱动抖动驱动 stepperPlan，返回逐 tick 记录与每次输入的结果。
func runJitteredDriver(t *testing.T, seed int64, gaps func(*rand.Rand) time.Duration) (ticks, inputs []string, stats StepperStats) {
	t.Helper()
	fireball, environment := compileStepperProgram(t, stepperFireballJSON)
	burn, _ := compileStepperProgram(t, stepperBurnJSON)
	w := newStepperWorld(t, stepperClock, environment, StepperConfig{})
	rng := rand.New(rand.NewSource(seed))

	type event struct {
		at    time.Time
		input *stepperInput
	}
	var events []event
	for at := stepperOrigin; at.Before(stepperOrigin.Add(8 * time.Second)); at = at.Add(gaps(rng)) {
		events = append(events, event{at: at})
		if rng.Intn(8) == 0 { // 重复投递同一时刻（同一拍的两份）与晚到的一步（时刻早于上一步）
			events = append(events, event{at: at}, event{at: at.Add(-time.Duration(rng.Intn(120)) * time.Millisecond)})
		}
	}
	for i := range stepperPlan {
		offset := time.Duration(rng.Int63n(int64(stepperClock.Length)))
		events = append(events, event{at: stepperClock.StartOf(stepperPlan[i].Tick).Add(offset), input: &stepperPlan[i]})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })

	for _, e := range events {
		if e.input == nil {
			if err := w.stepper.StepTo(e.at, 1); err != nil {
				t.Fatal(err)
			}
			continue
		}
		w.inputAt = e.at
		inputs = append(inputs, w.start(t, fireball, burn, *e.input))
	}
	return w.log, inputs, w.stepper.Stats()
}

var stepperJitterProfiles = []struct {
	name string
	gaps func(*rand.Rand) time.Duration
}{
	{"every tick", func(*rand.Rand) time.Duration { return 50 * time.Millisecond }},
	{"every other tick", func(*rand.Rand) time.Duration { return 100 * time.Millisecond }},
	{"random 10-400ms", func(r *rand.Rand) time.Duration { return time.Duration(10+r.Intn(390)) * time.Millisecond }},
	{"long gaps", func(r *rand.Rand) time.Duration { return time.Duration(600+r.Intn(390)) * time.Millisecond }},
}

// 同一输入序列（每个输入落在同一个 tick 上，tick 内的位置随机）在不同的驱动抖动下逐 tick 相同：Host 每个 tick 被
// 推进一次、按顺序；空闲点由 Runtime 决定；空闲之后由输入跳到输入的 tick。四种抖动 × 5 个种子：每 tick 一步、
// 每两 tick 一步、10～400ms 随机间隔（含重复与晚到的一步），以及接近追帧上限的长间隔。
func TestStepperResultsDoNotDependOnDriverJitter(t *testing.T) {
	var wantTicks, wantInputs []string
	for i, p := range stepperJitterProfiles {
		for seed := int64(1); seed <= 5; seed++ {
			ticks, inputs, stats := runJitteredDriver(t, seed, p.gaps)
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
	requireStepperLog(t, wantTicks, wantInputs)
}

// requireStepperLog：记录本身要有内容——tick 2 的输入把新 Runtime 一次推到 2，之后逐 tick 推进；燃烧在 tick 18
// 结束、空闲点停住，空闲区间（19～30、46～119）不推进，tick 31 与 120 的输入各一次跳过去；tick 15 的火球在冷却中、
// 在 tick 15 被拒。
func requireStepperLog(t *testing.T, ticks, inputs []string) {
	t.Helper()
	joined := " " + strings.Join(ticks, " ") + " "
	for _, want := range []string{"t2", "t3", "t4", "t18", "t31", "t45", "t120"} {
		if !strings.Contains(joined, " "+want+" ") {
			t.Fatalf("per-tick log %v lacks %q", ticks, want)
		}
	}
	for _, idle := range []string{"t1", "t19", "t30", "t46", "t100"} {
		if strings.Contains(joined, " "+idle+" ") {
			t.Fatalf("the idle tick %s was stepped: %v", idle, ticks)
		}
	}
	if !strings.Contains(strings.Join(inputs, "\n"), "tick 15 applied at 15: cast 0 err skill:") {
		t.Fatalf("the fireball on cooldown at tick 15 was not refused at tick 15: %v", inputs)
	}
}

// lockstepRoom 是一个定步长循环驱动：每帧一个 tick，只推进醒着的 Runtime（Driver 的 Wake / Sleep 记在 awake 里），
// 帧内的输入在推进之后执行。与 Lockstep 房间、scripts/testdata 塔防 / 自走棋夹具的 Advance(now) 同一形状。
type lockstepRoom struct {
	awake map[*Stepper]bool
	steps int // 实际调用 StepToTick 的次数（睡着的 Runtime 不占循环）
}

type lockstepMember struct {
	room    *lockstepRoom
	stepper *Stepper
}

func (m *lockstepMember) Wake() error { m.room.awake[m.stepper] = true; return nil }
func (m *lockstepMember) Sleep()      { delete(m.room.awake, m.stepper) }

// frame 推进一帧：醒着的 Runtime 推进到这一帧的 tick。
func (r *lockstepRoom) frame(tick Tick) error {
	var err error
	for stepper := range r.awake {
		r.steps++
		err = errors.Join(err, stepper.StepToTick(tick, 1))
	}
	return err
}

// 定步长循环驱动与抖动的手动驱动结果逐 tick 相同：同一个 Stepper、同一组输入，换一种驱动方式不改变结果。循环只推进
// 醒着的 Runtime，空闲区间不占循环。
func TestStepperOnAFixedStepLoopMatchesTheJitteredDriver(t *testing.T) {
	wantTicks, wantInputs, _ := runJitteredDriver(t, 1, stepperJitterProfiles[2].gaps)

	fireball, environment := compileStepperProgram(t, stepperFireballJSON)
	burn, _ := compileStepperProgram(t, stepperBurnJSON)
	room := &lockstepRoom{awake: make(map[*Stepper]bool)}
	member := &lockstepMember{room: room}
	w := newStepperWorld(t, stepperClock, environment, StepperConfig{Driver: member})
	member.stepper = w.stepper
	w.awake = func() bool { return room.awake[w.stepper] }
	var inputs []string
	for tick := Tick(0); tick <= 160; tick++ {
		if err := room.frame(tick); err != nil {
			t.Fatal(err)
		}
		for _, input := range stepperPlan {
			if input.Tick != tick {
				continue
			}
			w.inputAt = stepperClock.StartOf(tick)
			inputs = append(inputs, w.start(t, fireball, burn, input))
		}
	}
	if !slices.Equal(w.log, wantTicks) {
		t.Fatalf("fixed-step loop: per-tick results differ from the jittered driver\n got %v\nwant %v", w.log, wantTicks)
	}
	if !slices.Equal(inputs, wantInputs) {
		t.Fatalf("fixed-step loop: inputs\n%s\nwant\n%s", strings.Join(inputs, "\n"), strings.Join(wantInputs, "\n"))
	}
	requireStepperLog(t, w.log, inputs)
	if stepped := int64(room.steps); stepped >= 160 || len(room.awake) != 0 {
		t.Fatalf("the loop stepped %d times over 161 frames with %d still awake, want only the busy frames and everything asleep at the end", stepped, len(room.awake))
	}
}

// 追帧上限：落后超过上限时先逐 tick 推进上限那么多，剩下的一次跳到目标，计 Jumps；Runtime 停在目标 tick。
func TestStepperJumpsPastTheCatchUpLimit(t *testing.T) {
	burn, environment := compileStepperProgram(t, strings.Replace(stepperBurnJSON, `"times":5`, `"times":60`, 1)) // 忙 180 tick
	w := newStepperWorld(t, stepperClock, environment, StepperConfig{})                                           // 上限 DefaultMaxCatchUp 1s = 20 tick
	w.inputAt = stepperOrigin
	if _, err := w.stepper.Start(burn, CastInput{Caster: 3, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if err := w.stepper.StepToTick(10, 1); err != nil {
		t.Fatal(err)
	}
	if got := w.stepper.Stats(); got.Steps != 10 || got.Jumps != 0 {
		t.Fatalf("10 ticks behind: %+v, want 10 steps and no jump", got)
	}
	if err := w.stepper.StepTo(stepperClock.StartOf(110), 1); err != nil {
		t.Fatal(err)
	}
	got := w.stepper.Stats()
	if got.Jumps != 1 || got.JumpedTicks != 80 || w.stepper.Runtime().CurrentTick() != 110 {
		t.Fatalf("100 ticks behind: %+v at tick %d, want 20 more steps, one jump over 80 ticks, at tick 110", got, w.stepper.Runtime().CurrentTick())
	}
	if got.Steps > 30 {
		t.Fatalf("stepped %d ticks, the limit is 20 per step", got.Steps)
	}
}

// 追帧上限乘 max(倍率, 1)：驱动的一步与输入的追帧都按自己的倍率。
func TestStepperCatchUpLimitScalesWithTheRate(t *testing.T) {
	clock := TickClock{Length: 10 * time.Millisecond}
	burn, environment := compileStepperProgram(t, strings.Replace(stepperBurnJSON, `"times":5,"interval_ticks":3`, `"times":60,"interval_ticks":10`, 1)) // 忙 600 tick
	for _, c := range []struct {
		name      string
		rate      float64
		wantJumps int64
	}{{"rate 1", 1, 1}, {"rate 2", 2, 0}, {"rate 0.5 counts as 1", 0.5, 1}} {
		t.Run(c.name, func(t *testing.T) {
			rate := c.rate
			w := newStepperWorld(t, clock, environment, StepperConfig{Rate: func() float64 { return rate }})
			if got := w.stepper.catchUpTicks(rate); got != Tick(100*max(rate, 1)) {
				t.Fatalf("catch-up limit at rate %v = %d ticks, want %d (1s x max(rate, 1) / 10ms)", rate, got, Tick(100*max(rate, 1)))
			}
			if _, err := w.stepper.Start(burn, CastInput{Caster: 3, Target: 2}); err != nil {
				t.Fatal(err)
			}
			if err := w.stepper.StepToTick(150, rate); err != nil { // 驱动的一步落后 150 tick
				t.Fatal(err)
			}
			w.inputAt = clock.StartOf(300) // 输入的追帧再落后 150 tick
			if err := w.stepper.CatchUp(); err != nil {
				t.Fatal(err)
			}
			if got := w.stepper.Stats(); got.Jumps != 2*c.wantJumps || w.stepper.Runtime().CurrentTick() != 300 {
				t.Fatalf("two 150-tick gaps at rate %v: %+v at tick %d, want %d jumps", rate, got, w.stepper.Runtime().CurrentTick(), 2*c.wantJumps)
			}
		})
	}
}

// 通知驱动：有工作时 Wake，推进到空闲后 Sleep；驱动的一步遇到空闲的 Runtime 不推进（tick 停在空闲点）；空闲之后的
// 输入一次跳到输入的 tick。
func TestStepperWakesWhileBusyAndSleepsAtTheIdlePoint(t *testing.T) {
	fireball, environment := compileStepperProgram(t, stepperFireballJSON)
	w := newStepperWorld(t, stepperClock, environment, StepperConfig{})
	w.inputAt = stepperClock.StartOf(3)
	if _, err := w.stepper.Start(fireball, CastInput{Caster: 1, Target: 2}); err != nil {
		t.Fatal(err)
	}
	if !w.driver.awake || w.stepper.Runtime().CurrentTick() != 3 {
		t.Fatalf("after Start: awake %v tick %d, want the driver woken at tick 3", w.driver.awake, w.stepper.Runtime().CurrentTick())
	}
	if err := w.stepper.StepTo(stepperClock.StartOf(40), 1); err != nil {
		t.Fatal(err)
	}
	idle := w.stepper.Runtime().CurrentTick()
	if w.driver.awake || idle != 8 {
		t.Fatalf("after the cast: awake %v tick %d, want asleep at the idle point 8 (windup 3 + recovery 2 from tick 3)", w.driver.awake, idle)
	}
	if err := w.stepper.StepTo(stepperClock.StartOf(60), 1); err != nil || w.stepper.Runtime().CurrentTick() != idle {
		t.Fatalf("a step on an idle runtime moved it to %d (%v)", w.stepper.Runtime().CurrentTick(), err)
	}
	w.inputAt = stepperClock.StartOf(70)
	if err := w.stepper.CatchUp(); err != nil || w.stepper.Runtime().CurrentTick() != 70 || w.stepper.Stats().Idle != 2 {
		t.Fatalf("CatchUp on an idle runtime: tick %d stats %+v (%v), want one more idle jump (the first was Start's 0 -> 3) to 70", w.stepper.Runtime().CurrentTick(), w.stepper.Stats(), err)
	}
	if err := w.stepper.StepTo(stepperClock.StartOf(65), 1); err != nil || w.stepper.Runtime().CurrentTick() != 70 {
		t.Fatalf("a late step (tick 65 after 70) moved the runtime to %d (%v)", w.stepper.Runtime().CurrentTick(), err)
	}
}

// 通知是电平式的：驱动的状态被外部改掉（心跳的启动随回滚作废、Remote 共享模式停掉了心跳）之后，下一次输入或 Sync
// 补上；挂不上的驱动把错误交给输入的调用方，工作留在 Runtime 里，之后 Sync 重试。
func TestStepperNotifiesTheDriverByLevel(t *testing.T) {
	fireball, environment := compileStepperProgram(t, stepperFireballJSON)
	burn, _ := compileStepperProgram(t, stepperBurnJSON)
	w := newStepperWorld(t, stepperClock, environment, StepperConfig{})
	w.inputAt = stepperClock.StartOf(1)
	if _, err := w.stepper.Start(burn, CastInput{Caster: 3, Target: 2}); err != nil || !w.driver.awake {
		t.Fatalf("Start: awake %v (%v)", w.driver.awake, err)
	}
	w.driver.awake = false // 驱动的启动被外部撤销
	if err := w.stepper.Sync(); err != nil || !w.driver.awake {
		t.Fatalf("Sync after the driver lost its state: awake %v (%v), want woken again", w.driver.awake, err)
	}
	w.driver.awake = false
	w.inputAt = stepperClock.StartOf(2)
	if _, err := w.stepper.Start(fireball, CastInput{Caster: 1, Target: 2}); err != nil || !w.driver.awake {
		t.Fatalf("the next input after the driver lost its state: awake %v (%v), want woken again", w.driver.awake, err)
	}

	errShared := errors.New("driver refused")
	w.driver.wakeErr = errShared
	w.driver.awake = false
	w.inputAt = stepperClock.StartOf(3)
	err := w.stepper.Do(func(*Runtime) error { return nil })
	if !errors.Is(err, errShared) || w.driver.awake || !w.stepper.Runtime().Pending() {
		t.Fatalf("a driver that cannot wake: err %v awake %v pending %v, want the error and the work left in the runtime", err, w.driver.awake, w.stepper.Runtime().Pending())
	}
	w.driver.wakeErr = nil
	if err := w.stepper.Sync(); err != nil || !w.driver.awake {
		t.Fatalf("Sync retry: awake %v (%v)", w.driver.awake, err)
	}
}

// 配置检查：Clock、Now 必填，追帧上限不能短于一个 tick；Driver 为 nil 时不通知（手动驱动每一步都推进）。
func TestStepperConfig(t *testing.T) {
	_, environment := compileStepperProgram(t, stepperFireballJSON)
	runtime := NewRuntime(runtimeTestHost(environment), RuntimeOptions{})
	now := func() time.Time { return stepperOrigin }
	for _, c := range []struct {
		name   string
		config StepperConfig
	}{
		{"no clock", StepperConfig{Now: now}},
		{"no now", StepperConfig{Clock: stepperClock}},
		{"catch-up shorter than a tick", StepperConfig{Clock: stepperClock, Now: now, MaxCatchUp: time.Millisecond}},
	} {
		if _, err := NewStepper(runtime, c.config); err == nil {
			t.Errorf("%s: NewStepper accepted it", c.name)
		}
	}
	if _, err := NewStepper(nil, StepperConfig{Clock: stepperClock, Now: now}); err == nil {
		t.Error("NewStepper accepted a nil runtime")
	}
	stepper, err := NewStepper(runtime, StepperConfig{Clock: stepperClock, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := stepper.Sync(); err != nil {
		t.Fatalf("Sync without a driver: %v", err)
	}
	if stepper.Tick() != 0 || stepper.Clock() != stepperClock {
		t.Fatalf("Tick %d Clock %+v", stepper.Tick(), stepper.Clock())
	}
}
