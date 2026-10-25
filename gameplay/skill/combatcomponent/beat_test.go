package combatcomponent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill/combat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/timedomain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// Buff 与技能 Runtime 的心跳，经真实 Nest 引擎与运行中的心跳驱动：有限时 buff / 在途施法时挂登记，
// 每拍推进，清空或空闲后停；事务回滚不启动；加载出的过期记录在第一拍清掉；时间域时停时不前进。
// 断言提交后的状态之前经同一实体的一条空消息等前一拍收尾（settle），不 sleep。

// heartbeatHandlers 在包初始化时（任何测试 ResetHandlersForTest 之前）记下心跳 handler，测试开头重新注册：本包别的
// 用例会清空全局 handler 表。
var heartbeatHandlers = func() map[nest.HandlerName]nest.BaseHandler {
	out := make(map[nest.HandlerName]nest.BaseHandler)
	for _, name := range heartbeat.HandlerNames() {
		out[name] = nest.GetHandler(name)
	}
	return out
}()

var heartbeatDurabilities = []nest.DurabilityPolicy{nest.DurabilityMemory, nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined}

var beatTestRun = nest.NewHandlerName("combatcomponent_test.beat_run")

// testBeatDelivery 是 newBeatFixture 的心跳投递方式；走真实驱动的用例经 forEachBeatDelivery 在单目标 Dispatch 与
// Nest 批量作业（heartbeat.delivery: batch）两种方式下各跑一遍。
var testBeatDelivery = heartbeat.DefaultDelivery

func forEachBeatDelivery(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, delivery := range []heartbeat.Delivery{heartbeat.DeliveryDispatch, heartbeat.DeliveryBatch} {
		t.Run(string(delivery), func(t *testing.T) {
			previous := testBeatDelivery
			testBeatDelivery = delivery
			t.Cleanup(func() { testBeatDelivery = previous })
			fn(t)
		})
	}
}

// beatFixture 是一个真实 Nest 引擎 + 启动的心跳驱动。
type beatFixture struct {
	t      *testing.T
	getter *testGetter
	engine *nest.NestMgr
	driver *heartbeat.Driver
}

func newBeatFixture(t *testing.T) *beatFixture {
	t.Helper()
	nest.ResetHandlersForTest()
	t.Cleanup(nest.ResetHandlersForTest)
	for i, name := range heartbeat.HandlerNames() {
		nest.MustRegisterHandlerWithMeta(name, heartbeatHandlers[name], nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: heartbeatDurabilities[i]})
	}
	nest.MustRegisterHandlerWithMeta(beatTestRun, func(_ []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		return nil, params[0].(func() error)()
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
	f := &beatFixture{t: t, getter: newTestGetter()}
	f.engine = nest.NewEngine(
		nest.NestOptionWithGetter(f.getter),
		nest.NestOptionWithTransactionCommitter(acceptingCombatCommitter{}),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}, nest.WorkerPoolConfig{Workers: 1, QueueCap: 64}),
	)
	if err := f.engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.engine.Shutdown(context.Background()) })
	f.driver = heartbeat.NewDriver(heartbeat.Config{Name: "combat-beat", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Delivery: testBeatDelivery})
	lookup := func(id int64) entity.IThreadSafeEntity {
		e, err := f.getter.Get(context.Background(), id, 0)
		if err != nil {
			return nil
		}
		return e
	}
	if err := f.driver.Bind(f.engine, lookup); err != nil {
		t.Fatal(err)
	}
	if err := f.driver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.driver.Stop(context.Background()) })
	return f
}

func (f *beatFixture) run(id int64, fn func() error) error {
	_, err := f.engine.Request(context.Background(), beatTestRun, id, nest.NewParams(fn))
	return err
}

func (f *beatFixture) mustRun(id int64, fn func() error) {
	f.t.Helper()
	if err := f.run(id, fn); err != nil {
		f.t.Fatal(err)
	}
}

// settle 经同一实体发一条空消息：它在前一拍（含提交后回调：心跳登记的启停）整个结束之后才执行。
func (f *beatFixture) settle(id int64) { f.t.Helper(); f.mustRun(id, func() error { return nil }) }

// active 在实体锁内读登记的业务视角状态（Reg.Active 只在实体锁内读写）。
func (f *beatFixture) active(id int64, reg func() *heartbeat.Reg) (active bool) {
	f.t.Helper()
	f.mustRun(id, func() error { active = reg().Active(); return nil })
	return active
}

// buffs 在实体锁内读生效中的 buff。
func (f *beatFixture) buffs(id int64, c *CombatComponent) (out []combat.BuffInstance) {
	f.t.Helper()
	f.mustRun(id, func() error { out = c.ActiveBuffs(); return nil })
	return out
}

// waitFor 等 cond 成立（观察拍里发生的事件；断言提交后状态前另外 settle）。
func (f *beatFixture) waitFor(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

var beatClock = skill.TickClock{Length: 10 * time.Millisecond} // Unix 纪元起算的绝对 tick

// beatRecorder 收集 OnBeat 的拍（拍在 worker 上执行，测试 goroutine 读）。
type beatRecorder struct {
	mu    sync.Mutex
	beats []BuffBeat
}

func (r *beatRecorder) onBeat(b BuffBeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beats = append(r.beats, b)
	return nil
}

func (r *beatRecorder) expired() (n int, first bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.beats {
		if len(b.Expired) > 0 {
			n += len(b.Expired)
			first = first || b.First
		}
	}
	return n, first
}

func (r *beatRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.beats)
}

func newBeatCombatant(t *testing.T, f *beatFixture, uniqueID int64, config BeatConfig) (*combatTestEntity, int64) {
	t.Helper()
	e, id := newCombatTestEntity(t, uniqueID)
	config.Host = f.driver.NewHost(e)
	config.Clock = beatClock
	if config.OnError == nil {
		config.OnError = func(err error) { t.Errorf("buff beat: %v", err) }
	}
	if err := e.component.AttachBeat(config); err != nil {
		t.Fatal(err)
	}
	if err := e.component.OnInitFinish(nil, false); err != nil {
		t.Fatal(err)
	}
	f.getter.Add(e)
	return e, id
}

// 限时 buff 挂上心跳、到期后在拍里过期、清空后停；永久 buff 不挂。
func TestTimedBuffsBeatUntilTheyExpire(t *testing.T) {
	forEachBeatDelivery(t, func(t *testing.T) {
		f := newBeatFixture(t)
		var rec beatRecorder
		e, id := newBeatCombatant(t, f, 9401, BeatConfig{Granularity: 50 * time.Millisecond, OnBeat: rec.onBeat})
		c := e.component
		if c.BeatReg().Period() != 50*time.Millisecond || c.BeatReg().Active() {
			t.Fatalf("attached beat: period %v active %v, want a stopped 50ms registration", c.BeatReg().Period(), c.BeatReg().Active())
		}
		f.mustRun(id, func() error {
			c.ApplyBuff(combat.BuffSpec{ID: 1, Tags: []combat.Tag{"aura"}}, c.CurrentTick(), 0)
			return nil
		})
		f.settle(id)
		if f.active(id, c.BeatReg) || f.driver.Registered() != 0 {
			t.Fatalf("a permanent buff: active %v registered %d, want no heartbeat", f.active(id, c.BeatReg), f.driver.Registered())
		}
		var applied int64
		f.mustRun(id, func() error {
			applied = c.CurrentTick()
			c.ApplyBuff(combat.BuffSpec{ID: 2, Tags: []combat.Tag{"stun"}, DurationTicks: 5}, applied, 0)
			if !c.BeatReg().Active() {
				return errors.New("a timed buff did not start the beat")
			}
			return nil
		})
		f.settle(id)
		if f.driver.Registered() != 1 {
			t.Fatalf("a timed buff: registered %d, want 1", f.driver.Registered())
		}
		f.waitFor("a beat past the stun's due tick", func() bool { return rec.lastTick() >= applied+5 })
		f.settle(id)
		active, beating := f.buffs(id, c), f.active(id, c.BeatReg)
		if len(active) != 1 || active[0].Spec.ID != 1 || beating || f.driver.Registered() != 0 {
			t.Fatalf("after expiry: buffs %+v active %v registered %d, want only the aura and no heartbeat", active, beating, f.driver.Registered())
		}
		if n, _ := rec.expired(); n != 1 {
			t.Fatalf("expired %d buffs on beats, want the stun once", n)
		}
	})
}

func (r *beatRecorder) lastTick() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.beats) == 0 {
		return 0
	}
	return r.beats[len(r.beats)-1].Tick
}

// 一拍的 tick 是 Beat.Now 向下取整的 tick：到期 tick 的前一个 tick 内的拍不过期，到期 tick 起的拍过期。拍回调在
// handler 的事务里直接调用，时刻由测试给定。
func TestBuffBeatExpiresExactlyAtTheDueTick(t *testing.T) {
	f := newBeatFixture(t)
	var rec beatRecorder
	e, id := newCombatTestEntity(t, 9409)
	if err := e.component.AttachBeat(BeatConfig{Host: heartbeat.NewDriver(heartbeat.Config{Name: "due-tick"}).NewHost(e), Clock: beatClock, OnBeat: rec.onBeat}); err != nil {
		t.Fatal(err)
	}
	f.getter.Add(e)
	c := e.component
	const applied, due = 1000, 1005
	f.mustRun(id, func() error {
		c.ApplyBuff(combat.BuffSpec{ID: 2, DurationTicks: due - applied}, applied, 0)
		return nil
	})
	lastNanoBeforeDue := beatClock.StartOf(due).Add(-time.Nanosecond)
	f.mustRun(id, func() error { return c.runBeat(heartbeat.Beat{Now: lastNanoBeforeDue}, rec.onBeat) })
	if n, _ := rec.expired(); n != 0 {
		t.Fatalf("a beat 1ns before tick %d expired the buff", due)
	}
	f.mustRun(id, func() error { return c.runBeat(heartbeat.Beat{Now: beatClock.StartOf(due)}, rec.onBeat) })
	if n, _ := rec.expired(); n != 1 || rec.lastTick() != due {
		t.Fatalf("the beat at the start of tick %d: expired %d at tick %d", due, n, rec.lastTick())
	}
}

// 施加限时 buff 的 handler 回滚了：buff 撤销，心跳的启动随之作废。
func TestARolledBackBuffDoesNotStartTheBeat(t *testing.T) {
	forEachBeatDelivery(t, func(t *testing.T) {
		f := newBeatFixture(t)
		e, id := newBeatCombatant(t, f, 9402, BeatConfig{})
		c := e.component
		errRefused := errors.New("refused")
		if err := f.run(id, func() error {
			c.ApplyBuff(combat.BuffSpec{ID: 2, DurationTicks: 50}, c.CurrentTick(), 0)
			return errRefused
		}); !errors.Is(err, errRefused) {
			t.Fatal(err)
		}
		f.settle(id)
		if buffs, beating := f.buffs(id, c), f.active(id, c.BeatReg); len(buffs) != 0 || beating || f.driver.Registered() != 0 {
			t.Fatalf("after the rollback: buffs %d active %v registered %d, want nothing", len(buffs), beating, f.driver.Registered())
		}
	})
}

// 加载：存储里有停服期间已经到期的 buff，OnInitFinish 挂上心跳，第一拍清掉它。
func TestLoadedExpiredBuffsAreClearedOnTheFirstBeat(t *testing.T) {
	forEachBeatDelivery(t, func(t *testing.T) {
		f := newBeatFixture(t)
		stored, storedID := newCombatTestEntity(t, 9403)
		f.getter.Add(stored)
		f.mustRun(storedID, func() error {
			stored.component.ApplyBuff(combat.BuffSpec{ID: 7, DurationTicks: 1}, 100, 0) // 很早以前施加、早已到期
			return nil
		})
		payload := mustMarshal(t, stored.component.Dao())

		var rec beatRecorder
		e, id := newCombatTestEntity(t, 9403) // 同一实体重新加载：新实例替换旧实例
		if id != storedID {
			t.Fatal("reloaded entity has a different id")
		}
		if err := e.component.Dao().RestorePersisted(payload, combatSchemaVersion, 1); err != nil {
			t.Fatal(err)
		}
		if err := e.component.AttachBeat(BeatConfig{Host: f.driver.NewHost(e), Clock: beatClock, Granularity: 50 * time.Millisecond, OnBeat: rec.onBeat}); err != nil {
			t.Fatal(err)
		}
		if err := e.component.OnInitFinish(nil, false); err != nil {
			t.Fatal(err)
		}
		if !e.component.BeatReg().Active() { // 加入 getter 之前读：拍还到不了这个实例
			t.Fatal("loaded timed buffs did not start the beat")
		}
		f.getter.Add(e)
		f.waitFor("the first beat to clear the stale buff", func() bool { n, _ := rec.expired(); return n == 1 })
		f.settle(id)
		if _, first := rec.expired(); !first || len(f.buffs(id, e.component)) != 0 || f.driver.Registered() != 0 {
			t.Fatalf("stale buff: cleared on the first beat %v, buffs %d, registered %d", first, len(f.buffs(id, e.component)), f.driver.Registered())
		}
	})
}

// 业务的周期工作（Periodic）没有 buff 也保持心跳；答案变成 false 后下一拍自己停。
func TestPeriodicWorkKeepsTheBuffBeat(t *testing.T) {
	forEachBeatDelivery(t, func(t *testing.T) {
		f := newBeatFixture(t)
		var regenerating atomic.Bool
		var rec beatRecorder
		e, id := newBeatCombatant(t, f, 9405, BeatConfig{Granularity: 50 * time.Millisecond, OnBeat: rec.onBeat, Periodic: regenerating.Load})
		c := e.component
		regenerating.Store(true)
		f.mustRun(id, c.SyncBuffDriver)
		f.waitFor("two regeneration beats", func() bool { return rec.count() >= 2 })
		regenerating.Store(false)
		before := rec.count()
		f.waitFor("one more beat", func() bool { return rec.count() > before })
		f.settle(id)
		if beating := f.active(id, c.BeatReg); beating || f.driver.Registered() != 0 {
			t.Fatalf("periodic work ended: active %v registered %d, want stopped", beating, f.driver.Registered())
		}
	})
}

// domainFields 扮演场景实体 DAO 上的时间域字段。
type domainFields struct{ state timedomain.State }

func (f *domainFields) TimeDomain() timedomain.State { return f.state }
func (f *domainFields) SetTimeDomain(s timedomain.State) {
	previous := f.state
	nest.RecordUndo(f, 1, func() error { f.state = previous; return nil })
	f.state = s
}

func newTestDomain(t *testing.T) *timedomain.Domain {
	t.Helper()
	d, err := timedomain.New(&domainFields{}, timedomain.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// 时间域：时停时 buff 不过期（拍照跳，tick 不动），恢复后按域时间到期。
func TestBuffsStopExpiringWhileTheSceneIsPaused(t *testing.T) {
	forEachBeatDelivery(t, func(t *testing.T) {
		f := newBeatFixture(t)
		domain := newTestDomain(t)
		if err := domain.SetRate(0); err != nil {
			t.Fatal(err)
		}
		var rec beatRecorder
		e, id := newBeatCombatant(t, f, 9406, BeatConfig{Granularity: 50 * time.Millisecond, OnBeat: rec.onBeat, TimeDomain: domain})
		c := e.component
		f.mustRun(id, func() error {
			c.ApplyBuff(combat.BuffSpec{ID: 2, DurationTicks: 3}, c.CurrentTick(), 0)
			return nil
		})
		f.waitFor("five beats in the paused scene (or an expiry)", func() bool { n, _ := rec.expired(); return rec.count() >= 5 || n > 0 })
		f.settle(id)
		if n, _ := rec.expired(); n != 0 || len(f.buffs(id, c)) != 1 {
			t.Fatalf("paused scene: %d expired, %d active, want the buff to stay", n, len(f.buffs(id, c)))
		}
		if err := domain.SetRate(1); err != nil {
			t.Fatal(err)
		}
		f.waitFor("the buff to expire after the scene resumed", func() bool { n, _ := rec.expired(); return n == 1 })
	})
}

// pinBusinessClock 把进程业务时钟换成可控时钟（fctx.Now 冻结的就是它），测试结束恢复。
func pinBusinessClock(t *testing.T, start time.Time) *atomic.Int64 {
	t.Helper()
	var now atomic.Int64
	now.Store(start.UnixNano())
	clock.SetSource(func() time.Time { return time.Unix(0, now.Load()) })
	t.Cleanup(func() { clock.SetSource(nil) })
	return &now
}

func inMessage(fn func()) {
	_, release := fctx.NewContext()
	defer release()
	fn()
}

// 时间域：2 倍速时 tick 走两倍、时停时不动；buff 与 Runtime 的 handler 侧 tick 都取域时间。追帧上限乘倍率见
// TestRuntimeBeatPassesTheDomainRate。
func TestHandlerTicksFollowTheTimeDomain(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	now := pinBusinessClock(t, start)
	domain := newTestDomain(t)
	owner, _ := newCombatTestEntity(t, 9407)
	host := heartbeat.NewDriver(heartbeat.Config{Name: "domain-ticks"}).NewHost(owner)
	if err := owner.component.AttachBeat(BeatConfig{Host: host, Clock: beatClock, TimeDomain: domain}); err != nil {
		t.Fatal(err)
	}
	_, environment := compileBeatProgram(t, beatFireballJSON)
	memory := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	beat, err := NewRuntimeBeat(skill.NewRuntime(memory, skill.RuntimeOptions{}), RuntimeBeatConfig{Host: host, Clock: beatClock, TimeDomain: domain})
	if err != nil {
		t.Fatal(err)
	}
	read := func() (buff int64, runtime skill.Tick) {
		inMessage(func() { buff, runtime = owner.component.CurrentTick(), beat.Tick() })
		return buff, runtime
	}
	base := beatClock.TickAt(start)
	if b, r := read(); b != int64(base) || r != base {
		t.Fatalf("rate 1: ticks %d / %d, want %d", b, r, base)
	}
	if err := domain.SetRate(2); err != nil {
		t.Fatal(err)
	}
	now.Add(int64(100 * time.Millisecond))
	if b, r := read(); b != int64(base)+20 || r != base+20 {
		t.Fatalf("100ms at rate 2: ticks %d / %d, want %d (20 ticks of 10ms)", b, r, base+20)
	}
	if err := domain.SetRate(0); err != nil {
		t.Fatal(err)
	}
	now.Add(int64(time.Second))
	if b, r := read(); b != int64(base)+20 || r != base+20 {
		t.Fatalf("paused for 1s: ticks %d / %d, want still %d", b, r, base+20)
	}
}

// 时间域：心跳适配层把域的倍率交给 Stepper——拍里的一步与 handler 的追帧都按 2 倍速的追帧上限（1s × 2 / 10ms =
// 200 tick），落后 150 tick 不跳。
func TestRuntimeBeatPassesTheDomainRate(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	now := pinBusinessClock(t, start)
	domain := newTestDomain(t)
	if err := domain.SetRate(2); err != nil {
		t.Fatal(err)
	}
	burn, environment := compileBeatProgram(t, strings.Replace(beatBurnJSON, `"times":5,"interval_ticks":3`, `"times":60,"interval_ticks":10`, 1)) // 忙 600 tick
	memory := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	memory.ConfigureGameplayCatalog(environment.Gameplay)
	memory.UpsertEntity(skill.MemoryEntity{ID: 2, Alive: true, Health: 500, MaxHealth: 500})
	memory.UpsertEntity(skill.MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100})
	owner, _ := newCombatTestEntity(t, 9411)
	beat, err := NewRuntimeBeat(skill.NewRuntime(memory, skill.RuntimeOptions{}), RuntimeBeatConfig{
		Host: heartbeat.NewDriver(heartbeat.Config{Name: "domain-rate"}).NewHost(owner), Clock: beatClock, TimeDomain: domain,
		OnError: func(err error) { t.Errorf("runtime beat: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var first skill.Tick
	inMessage(func() {
		first = beat.Tick()
		if _, err := beat.Start(burn, skill.CastInput{Caster: 3, Target: 2}); err != nil {
			t.Fatal(err)
		}
	})
	if err := beat.beat(heartbeat.Beat{Now: beatClock.StartOf(first + 150)}); err != nil { // 拍落后 150 tick
		t.Fatal(err)
	}
	now.Add(int64(1500 * time.Millisecond)) // 域时间走 3s（到第 300 个 tick）：handler 的追帧落后 150 tick
	inMessage(func() {
		if err := beat.CatchUp(); err != nil {
			t.Fatal(err)
		}
	})
	if got := beat.Stats(); got.Jumps != 0 || got.Steps != 300 || beat.Runtime().CurrentTick() != first+300 {
		t.Fatalf("two 150-tick gaps at rate 2: %+v at tick %d, want 300 steps from %d and no jump (limit 200 ticks)", got, beat.Runtime().CurrentTick(), first)
	}
}

// 真实心跳驱动下的 Runtime：Start 挂心跳，拍逐 tick 推进到施法结束，空闲后停；时间域时停时 Runtime 不前进。
func TestRuntimeBeatDrivesTheRuntimeOnTheRealHeartbeat(t *testing.T) {
	forEachBeatDelivery(t, func(t *testing.T) {
		for _, paused := range []bool{false, true} {
			t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
				f := newBeatFixture(t)
				fireball, environment := compileBeatProgram(t, beatFireballJSON)
				memory := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
				memory.ConfigureGameplayCatalog(environment.Gameplay)
				memory.UpsertEntity(skill.MemoryEntity{ID: 1, Alive: true, Health: 100, MaxHealth: 100, Resources: map[string]int64{"mana": 100}})
				memory.UpsertEntity(skill.MemoryEntity{ID: 2, Alive: true, Health: 100, MaxHealth: 100})
				owner, id := newCombatTestEntity(t, 9410)
				f.getter.Add(owner)
				var domain *timedomain.Domain
				if paused {
					domain = newTestDomain(t)
					if err := domain.SetRate(0); err != nil {
						t.Fatal(err)
					}
				}
				beat, err := NewRuntimeBeat(skill.NewRuntime(memory, skill.RuntimeOptions{}), RuntimeBeatConfig{
					Host: f.driver.NewHost(owner), Clock: beatClock, TimeDomain: domain,
					OnError: func(err error) { t.Errorf("runtime beat: %v", err) },
				})
				if err != nil {
					t.Fatal(err)
				}
				var cast skill.CastID
				f.mustRun(id, func() error {
					var err error
					cast, err = beat.Start(fireball, skill.CastInput{Caster: 1, Target: 2})
					return err
				})
				f.settle(id)
				if beating := f.active(id, beat.Reg); !beating || f.driver.Registered() != 1 {
					t.Fatalf("after Start: active %v registered %d", beating, f.driver.Registered())
				}
				finished := func() bool {
					snapshot, _ := beat.Runtime().InspectCast(cast)
					return snapshot.Status == skill.CastFinished
				}
				if paused {
					at := beat.Runtime().CurrentTick()
					executed := f.driver.Stats().Executed
					f.waitFor("five beats in the paused scene", func() bool { return f.driver.Stats().Executed >= executed+5 })
					f.settle(id)
					if beat.Runtime().CurrentTick() != at || finished() {
						t.Fatalf("paused scene: runtime moved from tick %d to %d, finished %v", at, beat.Runtime().CurrentTick(), finished())
					}
					if err := domain.SetRate(1); err != nil {
						t.Fatal(err)
					}
				}
				f.waitFor("the cast to finish on the heartbeat", finished)
				f.settle(id)
				if beating := f.active(id, beat.Reg); beating || f.driver.Registered() != 0 || memory.HealthForTest(2) != 75 {
					t.Fatalf("idle runtime: active %v registered %d hp %d, want stopped after the fireball landed", beating, f.driver.Registered(), memory.HealthForTest(2))
				}
				if stats := beat.Stats(); stats.Steps < 5 || stats.Jumps != 0 {
					t.Fatalf("stats %+v, want at least the 5 ticks of the cast stepped one by one and no jump", stats)
				}
			})
		}
	})
}

// 改到期 tick 不改 buff 集合，但限时与否变了：永久改成限时要挂心跳，限时改成永久（0）要停。
func TestSetBuffDueTickAlignsTheBeat(t *testing.T) {
	forEachBeatDelivery(t, func(t *testing.T) {
		f := newBeatFixture(t)
		e, id := newBeatCombatant(t, f, 9408, BeatConfig{})
		c := e.component
		var buff combat.BuffInstanceID
		f.mustRun(id, func() error {
			buff, _ = c.ApplyBuff(combat.BuffSpec{ID: 3}, c.CurrentTick(), 0)
			return nil
		})
		f.mustRun(id, func() error { c.SetBuffDueTick(buff, c.CurrentTick()+1000); return nil })
		if !f.active(id, c.BeatReg) || f.driver.Registered() != 1 {
			t.Fatalf("a permanent buff made timed: active %v registered %d, want the beat", f.active(id, c.BeatReg), f.driver.Registered())
		}
		f.mustRun(id, func() error { c.SetBuffDueTick(buff, 0); return nil })
		f.settle(id)
		if f.active(id, c.BeatReg) || f.driver.Registered() != 0 {
			t.Fatalf("a timed buff made permanent: active %v registered %d, want stopped", f.active(id, c.BeatReg), f.driver.Registered())
		}
	})
}

// 撤销与重建（与 heartbeat.Reg.Close、actionflow / ai 的 Close 一致）：CloseBuffDriver 撤销 buff 心跳登记、释放名字，
// 同一宿主上马上可以按同名 AttachBeat 重建（成员换场景、换时间域）；OnDestroy 同样撤销。
func TestABuffBeatCanBeRebuiltOnTheSameHost(t *testing.T) {
	d := heartbeat.NewDriver(heartbeat.Config{Name: "buff-rebuild", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	e, _ := newCombatTestEntity(t, 9412)
	host := d.NewHost(e)
	c := e.component
	attach := func() {
		t.Helper()
		if err := c.AttachBeat(BeatConfig{Host: host, Clock: beatClock, OnError: func(err error) { t.Errorf("buff beat: %v", err) }}); err != nil {
			t.Fatal(err)
		}
	}
	attach()
	if err := c.OnInitFinish(nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := nest.RunDetachedTransaction(context.Background(), acceptingCombatCommitter{}, "buff_rebuild_apply", func() (any, error) {
		c.ApplyBuff(combat.BuffSpec{ID: 2, DurationTicks: 1000}, c.CurrentTick(), 0)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := c.BeatReg()
	if !old.Active() || d.Registered() != 1 {
		t.Fatalf("a timed buff: active %v registered %d", old.Active(), d.Registered())
	}
	c.CloseBuffDriver()
	if !old.Closed() || c.BeatReg() != nil || d.Registered() != 0 {
		t.Fatalf("after CloseBuffDriver: old closed %v BeatReg set %v registered %d, want the registration gone", old.Closed(), c.BeatReg() != nil, d.Registered())
	}
	attach() // 同名 "combat" 已释放
	if err := c.SyncBuffDriver(); err != nil || !c.BeatReg().Active() || d.Registered() != 1 {
		t.Fatalf("rebuilt beat after SyncBuffDriver: active %v registered %d (%v), want the timed buff beating again", c.BeatReg().Active(), d.Registered(), err)
	}
	c.OnDestroy(entity.EntityDestroyReason(0))
	if d.Registered() != 0 || c.BeatReg() != nil {
		t.Fatalf("after OnDestroy: registered %d BeatReg set %v, want the registration gone", d.Registered(), c.BeatReg() != nil)
	}
}

// 技能心跳的撤销与重建：Close 之后旧推进器的输入在 Runtime 有工作时报 ErrRegClosed；同一个 Runtime 在同一宿主上按同名
// 重建，Sync 后接着推进。
func TestARuntimeBeatCanBeRebuiltOnTheSameHost(t *testing.T) {
	pinBusinessClock(t, time.Unix(1_800_000_000, 0))
	d := heartbeat.NewDriver(heartbeat.Config{Name: "runtime-rebuild", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	owner, _ := newCombatTestEntity(t, 9413)
	host := d.NewHost(owner)
	burn, environment := compileBeatProgram(t, beatBurnJSON)
	memory := skill.NewMemoryHost(skill.AuthorityIdentity{Revision: environment.Revision, Digest: environment.Digest})
	memory.ConfigureGameplayCatalog(environment.Gameplay)
	memory.UpsertEntity(skill.MemoryEntity{ID: 2, Alive: true, Health: 500, MaxHealth: 500})
	memory.UpsertEntity(skill.MemoryEntity{ID: 3, Alive: true, Health: 100, MaxHealth: 100})
	runtime := skill.NewRuntime(memory, skill.RuntimeOptions{})
	config := RuntimeBeatConfig{Host: host, Clock: beatClock, OnError: func(err error) { t.Errorf("runtime beat: %v", err) }}
	old, err := NewRuntimeBeat(runtime, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Start(burn, skill.CastInput{Caster: 3, Target: 2}); err != nil || d.Registered() != 1 {
		t.Fatalf("Start: registered %d (%v)", d.Registered(), err)
	}
	old.Close()
	if !old.Closed() || d.Registered() != 0 {
		t.Fatalf("after Close: closed %v registered %d", old.Closed(), d.Registered())
	}
	if err := old.Sync(); !errors.Is(err, heartbeat.ErrRegClosed) {
		t.Fatalf("Sync on a closed runtime beat with work = %v, want ErrRegClosed", err)
	}
	rebuilt, err := NewRuntimeBeat(runtime, config) // 同名 "skill" 已释放
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuilt.Sync(); err != nil || !rebuilt.Reg().Active() || d.Registered() != 1 {
		t.Fatalf("rebuilt runtime beat after Sync: active %v registered %d (%v)", rebuilt.Reg().Active(), d.Registered(), err)
	}
}

// 真实心跳驱动：在 handler 里撤销 buff 心跳并按同名重建，旧登记之后不再执行回调（已经投出的拍按过期丢弃），新登记
// 接着推进同一组 buff。
func TestAClosedBuffBeatStopsAndTheRebuiltOneTakesOver(t *testing.T) {
	forEachBeatDelivery(t, func(t *testing.T) {
		f := newBeatFixture(t)
		var before, after beatRecorder
		e, id := newCombatTestEntity(t, 9414)
		host := f.driver.NewHost(e)
		c := e.component
		if err := c.AttachBeat(BeatConfig{Host: host, Clock: beatClock, Granularity: 50 * time.Millisecond, OnBeat: before.onBeat}); err != nil {
			t.Fatal(err)
		}
		if err := c.OnInitFinish(nil, false); err != nil {
			t.Fatal(err)
		}
		f.getter.Add(e)
		f.mustRun(id, func() error {
			c.ApplyBuff(combat.BuffSpec{ID: 2, DurationTicks: 100_000}, c.CurrentTick(), 0)
			return nil
		})
		f.waitFor("two beats on the first registration", func() bool { return before.count() >= 2 })
		var closedAt int
		f.mustRun(id, func() error {
			c.CloseBuffDriver()
			closedAt = before.count()
			if err := c.AttachBeat(BeatConfig{Host: host, Clock: beatClock, Granularity: 50 * time.Millisecond, OnBeat: after.onBeat}); err != nil {
				return err
			}
			return c.SyncBuffDriver()
		})
		f.waitFor("three beats on the rebuilt registration", func() bool { return after.count() >= 3 })
		f.settle(id)
		if got := before.count(); got != closedAt || f.driver.Registered() != 1 || len(f.buffs(id, c)) != 1 {
			t.Fatalf("after the rebuild: old registration ran %d beats after Close, registered %d, buffs %d; want none, one registration, the buff kept",
				got-closedAt, f.driver.Registered(), len(f.buffs(id, c)))
		}
		f.mustRun(id, func() error { c.OnDestroy(entity.EntityDestroyReason(0)); return nil })
		f.settle(id)
		if f.driver.Registered() != 0 {
			t.Fatalf("after OnDestroy: registered %d, want 0", f.driver.Registered())
		}
	})
}
