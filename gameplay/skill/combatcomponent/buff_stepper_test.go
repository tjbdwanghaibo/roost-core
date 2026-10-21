package combatcomponent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
)

// Buff 的推进与驱动无关（维护者 2026-10-21：心跳只是驱动方式之一）：同一份 DriveBuffs 规则由不走心跳的驱动推进——
// 房间自己的定步长循环（每帧一个 tick、只推进醒着的战斗单位）与测试里手动调用。心跳驱动（AttachBeat）的用例见
// beat_test.go。

// buffRoom 是一个定步长循环驱动：每帧在一个事务里把醒着的战斗单位推进到这一帧的 tick。
type buffRoom struct {
	awake map[*CombatComponent]bool
	steps int
	frame int64
}

type buffRoomMember struct {
	room      *buffRoom
	component *CombatComponent
}

func (m *buffRoomMember) Wake() error { m.room.awake[m.component] = true; return nil }
func (m *buffRoomMember) Sleep()      { delete(m.room.awake, m.component) }

var buffRoomClock = skill.TickClock{Length: 100 * time.Millisecond}

func (r *buffRoom) join(t *testing.T, uniqueID int64, config BuffDriveConfig) *CombatComponent {
	t.Helper()
	e, _ := newCombatTestEntity(t, uniqueID)
	member := &buffRoomMember{room: r, component: e.component}
	config.Clock = buffRoomClock
	config.Now = func() time.Time { return buffRoomClock.StartOf(skill.Tick(r.frame)) } // 输入落在当前帧
	config.Driver = member
	if err := e.component.DriveBuffs(config); err != nil {
		t.Fatal(err)
	}
	if err := e.component.OnInitFinish(nil, false); err != nil {
		t.Fatal(err)
	}
	return e.component
}

// inFrame 在一个事务里执行这一帧的输入与推进（buff 的修改都要在事务里）。
func (r *buffRoom) inFrame(t *testing.T, fn func() error) error {
	t.Helper()
	_, err := nest.RunDetachedTransaction(context.Background(), acceptingCombatCommitter{}, "buff_room_frame", func() (any, error) {
		return nil, fn()
	})
	return err
}

// run 推进帧 [from, to]：每帧先执行 inputs[帧]，再推进醒着的战斗单位。
func (r *buffRoom) run(t *testing.T, from, to int64, inputs map[int64]func()) {
	t.Helper()
	for r.frame = from; r.frame <= to; r.frame++ {
		if err := r.inFrame(t, func() error {
			if input := inputs[r.frame]; input != nil {
				input()
			}
			var err error
			for component := range r.awake {
				r.steps++
				err = errors.Join(err, component.StepBuffsToTick(r.frame))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// stepLog 收集 OnStep。
type stepLog struct{ steps []BuffStep }

func (l *stepLog) onStep(step BuffStep) error { l.steps = append(l.steps, step); return nil }

func (l *stepLog) expiredAt() (ticks []int64) {
	for _, step := range l.steps {
		for range step.Expired {
			ticks = append(ticks, step.Tick)
		}
	}
	return ticks
}

// 定步长循环驱动：限时 buff 让战斗单位醒来，在到期的那一帧过期、之后睡；永久 buff 不醒；业务的周期工作（Periodic）
// 没有 buff 也保持醒着，答案变成 false 后下一步自己睡。循环只推进醒着的战斗单位。
func TestBuffsOnAFixedStepLoop(t *testing.T) {
	room := &buffRoom{awake: make(map[*CombatComponent]bool)}
	var stunLog, auraLog, regenLog stepLog
	stunned := room.join(t, 9501, BuffDriveConfig{OnStep: stunLog.onStep})
	aura := room.join(t, 9502, BuffDriveConfig{OnStep: auraLog.onStep})
	regenerating := false
	regen := room.join(t, 9503, BuffDriveConfig{OnStep: regenLog.onStep, Periodic: func() bool { return regenerating }})

	room.run(t, 0, 30, map[int64]func(){
		10: func() {
			stunned.ApplyBuff(combat.BuffSpec{ID: 2, Tags: []combat.Tag{"stun"}, DurationTicks: 5}, stunned.CurrentTick(), 0)
			aura.ApplyBuff(combat.BuffSpec{ID: 1, Tags: []combat.Tag{"aura"}}, aura.CurrentTick(), 0)
		},
		20: func() {
			regenerating = true
			if err := regen.SyncBuffDriver(); err != nil {
				t.Fatal(err)
			}
		},
		25: func() { regenerating = false },
	})

	if got := stunLog.expiredAt(); len(got) != 1 || got[0] != 15 || len(stunned.ActiveBuffs()) != 0 {
		t.Fatalf("the 5-tick stun applied at frame 10 expired at %v (buffs %d), want once at tick 15", got, len(stunned.ActiveBuffs()))
	}
	if len(stunLog.steps) != 6 || stunLog.steps[0].Tick != 10 || stunLog.steps[5].Tick != 15 {
		t.Fatalf("stun steps %+v, want frames 10..15 only", stunLog.steps)
	}
	if len(auraLog.steps) != 0 || len(aura.ActiveBuffs()) != 1 {
		t.Fatalf("a permanent buff: %d steps, %d buffs, want no steps and the aura kept", len(auraLog.steps), len(aura.ActiveBuffs()))
	}
	if len(regenLog.steps) != 6 || regenLog.steps[0].Tick != 20 || regenLog.steps[5].Tick != 25 {
		t.Fatalf("periodic work from frame 20 to 25: steps %+v, want frames 20..25", regenLog.steps)
	}
	if len(room.awake) != 0 || room.steps != 12 {
		t.Fatalf("after the loop: %d awake, %d steps, want everything asleep and only the 12 busy steps", len(room.awake), room.steps)
	}
}

// 驱动不在事务里（这里的循环、一般的非心跳驱动）：施加限时 buff 的帧回滚了，buff 撤销而驱动已被叫醒；下一步没有
// 工作，驱动自己睡——多醒一步，不会一直醒着。心跳驱动的启停随事务，没有这一步（TestARolledBackBuffDoesNotStartTheBeat）。
func TestARolledBackBuffCostsANonTransactionalDriverOneStep(t *testing.T) {
	room := &buffRoom{awake: make(map[*CombatComponent]bool)}
	var log stepLog
	c := room.join(t, 9504, BuffDriveConfig{OnStep: log.onStep})
	room.frame = 3
	errRefused := errors.New("refused")
	if err := room.inFrame(t, func() error {
		c.ApplyBuff(combat.BuffSpec{ID: 2, DurationTicks: 50}, c.CurrentTick(), 0)
		return errRefused
	}); !errors.Is(err, errRefused) {
		t.Fatal(err)
	}
	if len(c.ActiveBuffs()) != 0 || !room.awake[c] {
		t.Fatalf("after the rollback: buffs %d awake %v, want the buff gone and the driver still awake", len(c.ActiveBuffs()), room.awake[c])
	}
	room.run(t, 4, 6, nil)
	if len(log.steps) != 1 || len(room.awake) != 0 {
		t.Fatalf("after the rollback: %d steps, %d awake, want one empty step and then asleep", len(log.steps), len(room.awake))
	}
}

// 手动驱动（Driver 为 nil：不通知，调用方每一步都推进）：StepBuffs 按时刻换算 tick，到期 tick 的前一纳秒不过期、到期
// tick 的起点过期；加载出的过期记录在第一步清掉。
func TestBuffsSteppedByHand(t *testing.T) {
	stored, _ := newCombatTestEntity(t, 9505)
	if _, err := nest.RunDetachedTransaction(context.Background(), acceptingCombatCommitter{}, "buff_manual_store", func() (any, error) {
		stored.component.ApplyBuff(combat.BuffSpec{ID: 7, DurationTicks: 1}, 100, 0) // 很早以前施加、早已到期
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	e, _ := newCombatTestEntity(t, 9505)
	if err := e.component.Dao().RestorePersisted(mustMarshal(t, stored.component.Dao()), combatSchemaVersion, 1); err != nil {
		t.Fatal(err)
	}
	c := e.component
	var log stepLog
	at := buffRoomClock.StartOf(1000)
	if err := c.DriveBuffs(BuffDriveConfig{Clock: buffRoomClock, Now: func() time.Time { return at }, OnStep: log.onStep}); err != nil {
		t.Fatal(err)
	}
	if err := c.OnInitFinish(nil, false); err != nil {
		t.Fatal(err)
	}
	if !c.BuffsPending() {
		t.Fatal("a loaded timed buff is not pending work")
	}
	step := func(now time.Time) {
		t.Helper()
		if _, err := nest.RunDetachedTransaction(context.Background(), acceptingCombatCommitter{}, "buff_manual_step", func() (any, error) {
			return nil, c.StepBuffs(now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	step(at)
	if got := log.expiredAt(); len(got) != 1 || got[0] != 1000 || c.BuffsPending() {
		t.Fatalf("the first step: expired at %v, pending %v, want the stale buff cleared at tick 1000", got, c.BuffsPending())
	}
	const due = 1005
	if _, err := nest.RunDetachedTransaction(context.Background(), acceptingCombatCommitter{}, "buff_manual_apply", func() (any, error) {
		c.ApplyBuff(combat.BuffSpec{ID: 2, DurationTicks: due - 1000}, c.CurrentTick(), 0)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if next, ok := c.NextBuffDue(); !ok || next != due {
		t.Fatalf("NextBuffDue = %d %v, want %d", next, ok, due)
	}
	step(buffRoomClock.StartOf(due).Add(-time.Nanosecond))
	if got := log.expiredAt(); len(got) != 1 {
		t.Fatalf("a step 1ns before tick %d expired the buff: %v", due, got)
	}
	step(buffRoomClock.StartOf(due))
	if got := log.expiredAt(); len(got) != 2 || got[1] != due {
		t.Fatalf("the step at the start of tick %d: expired at %v", due, got)
	}
}

// 配置检查：Clock、Now 必填，只能装一次（DriveBuffs 与 AttachBeat 二选一）；没装时 StepBuffs 返回错误、CurrentTick
// panic。
func TestDriveBuffsConfig(t *testing.T) {
	e, _ := newCombatTestEntity(t, 9506)
	c := e.component
	if err := c.StepBuffs(time.Now()); err == nil {
		t.Error("StepBuffs without a drive returned no error")
	}
	if err := c.SyncBuffDriver(); err != nil {
		t.Errorf("SyncBuffDriver without a drive: %v", err)
	}
	now := func() time.Time { return time.Unix(0, 0) }
	if err := c.DriveBuffs(BuffDriveConfig{Now: now}); err == nil {
		t.Error("DriveBuffs accepted a zero clock")
	}
	if err := c.DriveBuffs(BuffDriveConfig{Clock: buffRoomClock}); err == nil {
		t.Error("DriveBuffs accepted a nil Now")
	}
	if err := c.DriveBuffs(BuffDriveConfig{Clock: buffRoomClock, Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := c.DriveBuffs(BuffDriveConfig{Clock: buffRoomClock, Now: now}); err == nil {
		t.Error("DriveBuffs installed twice")
	}
	if c.BeatReg() != nil || c.CurrentTick() != 0 {
		t.Errorf("a non-heartbeat drive: BeatReg %v CurrentTick %d", c.BeatReg(), c.CurrentTick())
	}
}
