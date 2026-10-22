package skill_test

import (
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
)

// fixedStepRoom 是一个不走心跳的驱动：房间自己的定步长循环（Lockstep 房间、塔防 / 自走棋的 Advance(now) 那种形状），
// 每帧一个 tick，只推进醒着的 Runtime。Stepper 经 Driver 告诉房间哪些 Runtime 有工作。
type fixedStepRoom struct {
	awake map[*skill.Stepper]bool
}

// roomMember 把一个 Stepper 的 Wake / Sleep 记到房间里。
type roomMember struct {
	room    *fixedStepRoom
	stepper *skill.Stepper
}

func (m *roomMember) Wake() error { m.room.awake[m.stepper] = true; return nil }
func (m *roomMember) Sleep()      { delete(m.room.awake, m.stepper) }

// ExampleStepper_fixedStepLoop 用房间的定步长循环驱动技能 Runtime：帧号就是 tick，输入取当前帧的时刻。推进规则与
// 心跳驱动（combatcomponent.NewRuntimeBeat）是同一份 skill.Stepper。
func ExampleStepper_fixedStepLoop() {
	const fireballJSON = `{"schema":"roost.skill/v2","id":"skill.example.stepper.fireball","name":"Fireball","description":"Windup, commit, burn.",` +
		`"activation":{"type":"active","policy":{"mode":"tap"},"cast_window":{"windup_ticks":3,"commit_tick":2,"recovery_ticks":2,"movement":"locked","turning":"allowed","interrupt_tags":[],"refund_before_commit":true}},` +
		`"input_schema":{"type":"entity"},"cooldown_ticks":20,"costs":[],"memory":{},"initial_phase":"cast",` +
		`"phases":[{"id":"cast","timeout_ticks":0,"on":{"enter":{"flow":"sequence","steps":[{"flow":"effect","effect":{"type":"damage","target":"$input.target","amount":25,"damage_type":"magic","element":"fire"}},{"flow":"finish","reason":"done"}]}}}]}`
	definition, err := skill.Parse([]byte(fireballJSON))
	if err != nil {
		panic(err)
	}
	environment := skill.DefaultCompileEnvironment()
	fireball, diagnostics := skill.Compile(definition, environment)
	if fireball == nil {
		panic(fmt.Sprint(diagnostics))
	}
	host := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	host.ConfigureGameplayCatalog(environment.Gameplay)
	host.UpsertEntity(skill.MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100})
	host.UpsertEntity(skill.MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})

	clock := skill.TickClock{Length: 50 * time.Millisecond} // 一帧 50ms
	room := &fixedStepRoom{awake: make(map[*skill.Stepper]bool)}
	member := &roomMember{room: room}
	var frame skill.Tick
	stepper, err := skill.NewStepper(skill.NewRuntime(host, skill.RuntimeOptions{}), skill.StepperConfig{
		Clock:  clock,
		Now:    func() time.Time { return clock.StartOf(frame) }, // 输入落在当前帧
		Driver: member,
	})
	if err != nil {
		panic(err)
	}
	member.stepper = stepper

	for frame = 0; frame <= 10; frame++ {
		for s := range room.awake { // 只推进醒着的 Runtime
			if err := s.StepToTick(frame, 1); err != nil {
				panic(err)
			}
		}
		if frame == 2 { // 这一帧收到施法输入
			if _, err := stepper.Start(fireball, skill.CastInput{Caster: 1, Target: 2}); err != nil {
				panic(err)
			}
		}
		fmt.Printf("frame %d: runtime at tick %d, awake %v, target hp %d\n", frame, stepper.Runtime().CurrentTick(), room.awake[stepper], host.HealthForTest(2))
	}
	// Output:
	// frame 0: runtime at tick 0, awake false, target hp 100
	// frame 1: runtime at tick 0, awake false, target hp 100
	// frame 2: runtime at tick 2, awake true, target hp 100
	// frame 3: runtime at tick 3, awake true, target hp 100
	// frame 4: runtime at tick 4, awake true, target hp 100
	// frame 5: runtime at tick 5, awake true, target hp 75
	// frame 6: runtime at tick 6, awake true, target hp 75
	// frame 7: runtime at tick 7, awake false, target hp 75
	// frame 8: runtime at tick 7, awake false, target hp 75
	// frame 9: runtime at tick 7, awake false, target hp 75
	// frame 10: runtime at tick 7, awake false, target hp 75
}
