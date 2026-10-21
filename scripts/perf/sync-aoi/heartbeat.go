package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/movement"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 心跳驱动的移动负载（-move-driver=heartbeat，DESIGN-TODO D20 第 4 期）：每个实体装 gameplay/heartbeat 的宿主、
// movement 的 MapBasic / Move 与 actionflow 的 BeatRunner，和业务实体的接法相同（gameplay/movement Example）。
// 被选中的移动者各自巡逻：MoveTo 一个随机目标，到达后在 OnEnded 里发起下一段（经 runner 的延后队列），由 runner 的
// 移动组心跳每拍 Move.Advance(Beat.Elapsed)、写 MapBasic、QueueMove 进 AOI。压测端循环不再发改值请求，只等 tick、
// 设帧信封与 Apply。
//
// 几何与全局循环同一条带：基点 patrolBase（与 position 的偏移 0 相同），X 在 [基点, 基点 + MoveAmplitude] 之间，
// Y 不变，客户端按这条带核对（verifyPatrol）。移动者数按与全局循环相同的改值速率折算（movers），速度缺省每拍走
// 几格、每拍都换格，所以每拍一次改值。

const (
	moveDriverLoop      = "loop"
	moveDriverHeartbeat = "heartbeat"

	loadScene   = int64(1)
	loadMoveTo  = actionflow.ActionKind(1)
	loadMoveGrp = actionflow.ActionGroup(1)
)

// loadTick 是压测端当前的 tick（测量前的预热为负数或 0）：心跳里改值时记进 Step，客户端据此区分测量窗口、
// 判断全量是不是本 tick 的改值（与全局循环里 Step = tick 同口径）。
var loadTick atomic.Int64

// loadMover 是一个实体的移动组件。只在实体锁内读写（加载阶段除外）。
type loadMover struct {
	host  *heartbeat.Host
	mb    *movement.MapBasic
	move  *movement.Move
	beat  *actionflow.BeatRunner
	base  spatial.Point
	span  int64
	speed float64
	// period 是移动组心跳的档位。
	period time.Duration
	// leg 是第几段巡逻，决定下一个目标（确定的伪随机）。
	leg uint64
	// sceneID、facing 是 MapBasic 的另两个字段（不同步，客户端不核对）。
	sceneID int64
	facing  movement.Facing
	// lastSet 是上次写位置时这一拍的业务时间（UnixNano），用来量心跳间隔。
	lastSet int64
	stats   *moveStats
}

// MoveComponent 让 movement 的动作找到实体的 Move（movement.MoveOwner）。
func (s *subject) MoveComponent() *movement.Move {
	if s.mover == nil {
		return nil
	}
	return s.mover.move
}

// subjectFields 把 MapBasic 的状态映射到压测组件：位置就是同步出去的 X / Y，写位置时按全局循环的规则一起写 Step /
// HP / Planned / Changed 并标同步脏。没有生成 DAO 的逆操作：心跳 handler 用 memory 级别，回调不返回错误（BeatRunner
// 把 runner 错误交给 OnError），这一拍只会因提交失败回滚，压测里按失败处理（flush 失败计数非零即判失败）。
type subjectFields struct{ s *subject }

func (f subjectFields) SceneID() int64              { return f.s.mover.sceneID }
func (f subjectFields) SetSceneID(v int64)          { f.s.mover.sceneID = v }
func (f subjectFields) Pos() spatial.Point          { return spatial.Point{X: f.s.data.X, Y: f.s.data.Y} }
func (f subjectFields) Facing() movement.Facing     { return f.s.mover.facing }
func (f subjectFields) SetFacing(v movement.Facing) { f.s.mover.facing = v }
func (f subjectFields) SetPos(at spatial.Point) {
	s := f.s
	if s.data.X == at.X && s.data.Y == at.Y {
		return
	}
	step := loadTick.Load()
	beatNow := fctx.Now().UnixNano()
	s.data.Step = step
	s.data.X, s.data.Y = at.X, at.Y
	s.data.HP = 100 - (step+10000)%97
	// 心跳模式没有“计划时刻”：Planned 取这一拍消息冻结的业务时间（≈ handler 开始），驱动与快池的迟到看 beat_interval。
	s.data.Planned = beatNow
	s.data.Changed = time.Now().UnixNano()
	s.tracker.MarkSync(1)
	if m := s.mover; m.stats != nil && step > 0 {
		if m.lastSet != 0 {
			m.stats.interval(time.Duration(beatNow-m.lastSet), m.period)
		}
		m.lastSet = beatNow
	}
}

// loadSpace 把 MapBasic 接到压测的 Interest：实体在构造时已经 Show / Enter（与全局循环同一套初始化），所以进出场景
// 什么都不做；位置变化在实体锁与事务内 QueueMove（走提交门，提交后由 Manager 的政策阶段应用）。
type loadSpace struct{ interest *policy.Interest }

func (loadSpace) Enter(entity.IThreadSafeEntity, spatial.Point) error { return nil }
func (s loadSpace) QueueMove(e entity.IThreadSafeEntity, at spatial.Point) error {
	return s.interest.QueueMove(e, at)
}
func (loadSpace) Leave(entity.IThreadSafeEntity) error { return nil }

// moveStats 汇总移动负载的诊断：心跳间隔（同一实体相邻两次写位置的业务时间差，只计测量窗口）、巡逻段数、动作失败。
type moveStats struct {
	mu        sync.Mutex
	intervals []float64
	late      int64 // 间隔超过档位 + 50ms 的次数
	legs      atomic.Int64
	failed    atomic.Int64
	firstErr  atomic.Pointer[string]
}

func (m *moveStats) interval(d, period time.Duration) {
	m.mu.Lock()
	m.intervals = append(m.intervals, milliseconds(d))
	if d > period+50*time.Millisecond {
		m.late++
	}
	m.mu.Unlock()
}

func (m *moveStats) fail(err error) {
	m.failed.Add(1)
	text := err.Error()
	m.firstErr.CompareAndSwap(nil, &text)
}

// moversFor 按全局循环的改值速率折算移动者数：Nest 模式压测端每 50ms 一个 tick、每 tick 改 count 个实体，即每秒
// 20 × count 次改值；心跳模式每个移动者每拍一次改值（速度保证每拍换格），每秒 1 / 档位 次。override > 0 时按它。
func moversFor(c config) int {
	if c.Movers > 0 {
		return min(c.Movers, c.Entities)
	}
	count := max(1, (c.Entities*c.Dirty+99)/100)
	perSecond := float64(count) * float64(time.Second/(50*time.Millisecond))
	return min(c.Entities, int(math.Ceil(perSecond*c.MovePeriod.Seconds())))
}

// moverIndexes 用与全局循环相同的互素步长挑出移动者，散在整张地图上。
func moverIndexes(c config, n int) []int {
	stride := 7919
	for gcd(stride, c.Entities) != 1 {
		stride += 2
	}
	out := make([]int, n)
	for j := range out {
		out[j] = j * stride % c.Entities
	}
	return out
}

// nextTarget 是第 leg 段的目标 X：在 [基点, 基点 + span] 里确定地伪随机挑一个，并且不等于当前 X（否则 MoveTo
// 启动即到达，OnEnded 里又发起下一段，会在一条消息里连续触发）。
func nextTarget(id int64, leg uint64, base spatial.Point, span int64, current int64) spatial.Point {
	x := base.X + int64(splitmix64(uint64(id)^leg*0x9e3779b97f4a7c15)%uint64(span+1))
	if x == current {
		x = base.X + (x-base.X+1+span/2)%(span+1)
	}
	return spatial.Point{X: x, Y: base.Y}
}

// heartbeatLoad 是心跳模式的进程侧状态：驱动、动作注册表、移动者与统计。
type heartbeatLoad struct {
	driver   *heartbeat.Driver
	registry *actionflow.Registry
	movers   []*subject
	stats    *moveStats
	c        config
}

// newHeartbeatLoad 给每个实体装移动组件并进场景（加载阶段，不在事务里，立即生效）。Interest 的 Show / Enter 已由
// 公共初始化做完，loadSpace 的 Enter 不重复登记。
func newHeartbeatLoad(states []*subject, c config, interest *policy.Interest) (*heartbeatLoad, error) {
	load := &heartbeatLoad{
		// 跳拍 Warn 照常限频打到标准错误（进 sample-N.log）；压测结论以计数为准（server.json 的 heartbeat.driver）。
		driver:   heartbeat.NewDriver(heartbeat.Config{Name: "sync-aoi"}),
		registry: actionflow.NewRegistry(),
		stats:    &moveStats{},
		c:        c,
	}
	if err := movement.RegisterActions(load.registry, loadMoveTo, actionflow.ActionKind(2)); err != nil {
		return nil, err
	}
	space := loadSpace{interest: interest}
	for _, s := range states {
		m := &loadMover{base: patrolBase(s.data.ID, c), span: c.MoveAmplitude, speed: c.MoveSpeed, period: c.MovePeriod, stats: load.stats}
		s.mover = m
		m.host = load.driver.NewHost(s)
		var err error
		if m.mb, err = movement.NewMapBasic(s, m.host, movement.MapConfig{Fields: subjectFields{s}}); err != nil {
			return nil, err
		}
		if m.move, err = movement.NewMove(m.mb, movement.MoveConfig{}); err != nil {
			return nil, err
		}
		owner := s
		m.beat, err = actionflow.NewBeatRunner(actionflow.ActionRunnerConfig{
			Registry: load.registry,
			GroupForKind: func(actionflow.ActionKind) (actionflow.ActionGroup, bool) {
				return loadMoveGrp, true
			},
			Hooks: actionflow.ActionRunnerHooks{
				OnEnded: func(snap actionflow.ActionSnapshot, reason actionflow.ActionReason) {
					owner.mover.ended(owner, snap, reason)
				},
				OnError: load.stats.fail,
			},
		}, actionflow.BeatConfig{Host: m.host, Period: c.MovePeriod, Options: []heartbeat.RegOption{heartbeat.WithDurability(nest.DurabilityMemory)}})
		if err != nil {
			return nil, err
		}
		if err := m.mb.Enter(loadScene, space, spatial.Point{X: s.data.X, Y: s.data.Y}); err != nil {
			return nil, err
		}
	}
	for _, index := range moverIndexes(c, moversFor(c)) {
		load.movers = append(load.movers, states[index])
	}
	return load, nil
}

// ended 在巡逻的一段结束后发起下一段（runner 回调里调用，延后执行）。被同组新动作取代（取消）时不接；失败计数。
func (m *loadMover) ended(s *subject, snap actionflow.ActionSnapshot, reason actionflow.ActionReason) {
	result := reason.ToActionResult(actionflow.ActionStatusSuccess)
	switch result.Status {
	case actionflow.ActionStatusSuccess:
		m.stats.legs.Add(1)
		if err := m.startLeg(s); err != nil {
			m.stats.fail(err)
		}
	case actionflow.ActionStatusCanceled:
	default:
		m.stats.fail(fmt.Errorf("move action %d on entity %d ended %v: %s", snap.ID, s.data.ID, result.Status, result.Reason))
	}
}

func (m *loadMover) startLeg(s *subject) error {
	m.leg++
	target := nextTarget(s.data.ID, m.leg, m.base, m.span, s.data.X)
	_, err := m.beat.Runner().Start(loadMoveTo, movement.MoveToParam{Path: []spatial.Point{target}, Speed: m.speed, Period: m.period}, 0, time.Time{})
	return err
}

var loadStartMoving = nest.NewHandlerName("sync_aoi_start_moving")

func init() {
	nest.MustRegisterMemoryHandler(loadStartMoving, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		s := es[0].(*subject)
		// 最后一步才调 runner（D20 决定 3：runner 不随事务回滚）。
		return nil, s.mover.startLeg(s)
	})
}

// start 接上 Nest、启动驱动，再经 Nest 让每个移动者发起第一段巡逻。
func (l *heartbeatLoad) start(ctx context.Context, engine *nest.NestMgr, getter loadGetter) error {
	if err := l.driver.Bind(engine, func(id int64) entity.IThreadSafeEntity {
		if s := getter[id]; s != nil {
			return s
		}
		return nil
	}); err != nil {
		return err
	}
	if err := l.driver.Start(); err != nil {
		return err
	}
	for _, s := range l.movers {
		if _, err := engine.Request(ctx, loadStartMoving, s.ID(), nil); err != nil {
			return fmt.Errorf("start moving %d: %w", s.ID(), err)
		}
	}
	return nil
}

// stop 停驱动（Stop 返回后不再投递；已在 Nest 队列里的拍由随后的 engine.Shutdown 执行完）。
func (l *heartbeatLoad) stop(ctx context.Context) error {
	if l == nil {
		return nil
	}
	return l.driver.Stop(ctx)
}

// report 是 server.json 的 heartbeat 一节。
func (l *heartbeatLoad) report() map[string]any {
	l.stats.mu.Lock()
	intervals := slices.Clone(l.stats.intervals)
	late := l.stats.late
	l.stats.mu.Unlock()
	out := map[string]any{
		"movers": len(l.movers), "period_ms": milliseconds(l.c.MovePeriod), "speed_cells_per_second": l.c.MoveSpeed,
		"driver": l.driver.Stats(), "beat_interval_ms": quantiles(intervals), "beat_interval_samples": len(intervals),
		"beat_interval_over_period_plus_50ms": late, "legs_completed": l.stats.legs.Load(), "action_failures": l.stats.failed.Load(),
	}
	if first := l.stats.firstErr.Load(); first != nil {
		out["first_action_error"] = *first
	}
	return out
}

// verdict 是心跳模式的额外门槛：没有跳拍、卡住、准入失败，没有动作失败，测量窗口里确实有心跳改值。
func (l *heartbeatLoad) verdict() error {
	s := l.driver.Stats()
	if s.SkippedInflight+s.SkippedQueueFull+s.SkippedRejected+s.Stalled > 0 {
		return fmt.Errorf("heartbeat skipped beats (inflight %d, queue_full %d, rejected %d, stalled %d); reports retained", s.SkippedInflight, s.SkippedQueueFull, s.SkippedRejected, s.Stalled)
	}
	if n := l.stats.failed.Load(); n > 0 {
		first := ""
		if p := l.stats.firstErr.Load(); p != nil {
			first = *p
		}
		return fmt.Errorf("%d move action failures, first: %s; reports retained", n, first)
	}
	l.stats.mu.Lock()
	samples := len(l.stats.intervals)
	l.stats.mu.Unlock()
	if s.Executed == 0 || samples == 0 {
		return errors.New("heartbeat mode measured no beats")
	}
	return nil
}
