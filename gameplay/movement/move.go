package movement

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// DefaultMovePeriod 是移动心跳的缺省档位（10Hz）。
const DefaultMovePeriod = 100 * time.Millisecond

// StopReason 是一次移动自己结束的原因（调用方 Stop 的不算，不回调）。
type StopReason uint8

const (
	// Arrived：走完路径；Follow 设了 StopWhenReached 时到达保持距离。
	Arrived StopReason = iota + 1
	// Replaced：被同一 Move 上新的 MoveTo / Follow 取代。
	Replaced
	// Blocked：下一格被地形挡住，停在最后一个可走的位置。
	Blocked
	// LeftScene：自己离开了开始移动时所在的场景。
	LeftScene
	// TargetLost：跟随目标不在内存（卸载、删档）或找不到。
	TargetLost
	// TargetLeft：跟随目标不在自己的场景里。
	TargetLeft
	// Refused：写位置被拒（AOI 排队满、位置越界等），Result.Err 是原因；位置停在上一拍。
	Refused
)

func (r StopReason) String() string {
	switch r {
	case Arrived:
		return "arrived"
	case Replaced:
		return "replaced"
	case Blocked:
		return "blocked"
	case LeftScene:
		return "left scene"
	case TargetLost:
		return "target lost"
	case TargetLeft:
		return "target left the scene"
	case Refused:
		return "position refused"
	default:
		return fmt.Sprintf("stop reason %d", uint8(r))
	}
}

// Result 是一次移动结束的结论。
type Result struct {
	Reason StopReason
	// At 是结束时的位置。
	At  spatial.Point
	Err error
}

// Done 收到一次移动自己结束的结论（到达、被取代、被阻挡、离开场景、目标消失、被拒）。在实体锁与事务内调用（多半在
// 移动心跳的回调里），只做内存计算与入队；token 是 MoveTo / Follow 返回的那个。
type Done func(token uint64, result Result)

// Locator 返回实体 id 最近一次提交的位置（MapBasic.Published）；ok 为 false 表示目标不在内存。在跟随者的实体锁内
// 调用，不能取目标的锁、不能阻塞。LocateIn 用实体查找构造它。
type Locator func(id int64) (Snapshot, bool)

// MoveConfig 是 NewMove 的输入。
type MoveConfig struct {
	// Period 是移动心跳的档位，必须是 heartbeat.Periods 之一；0 取 DefaultMovePeriod。
	Period time.Duration
	// Terrain 非 nil 时每走进一个新格都问它：越界或被挡就以 Blocked 停下。
	Terrain spatial.Terrain
	// Locate 是 Follow 读目标位置的入口；nil 时 Follow 返回错误。
	Locate Locator
	// Options 传给 heartbeat.Host.Register（例如 WithDurability；缺省 async：只改 nopersist 字段时不写 WAL）。
	Options []heartbeat.RegOption
}

// MoveOwner 由带 Move 的实体实现，MoveTo / Follow 动作据此找到组件。
type MoveOwner interface {
	MoveComponent() *Move
}

type moveMode uint8

const (
	modeIdle moveMode = iota
	modePath
	modeFollow
)

// moveState 是 Move 的推进状态，整体随事务回滚恢复。path 一经设定不再修改（回滚快照共享它）。
type moveState struct {
	mode    moveMode
	token   uint64
	done    Done
	speed   float64 // 每秒格数
	sceneID int64
	// x, y 是亚格精度的位置：每拍的位移常常不足一格，只按格写进 MapBasic 会把零头丢光。
	x, y float64
	// cell 是 x, y 上一次写进 MapBasic 的格；MapBasic 的位置被别处改了（瞬移）时从新位置重新起算。
	cell spatial.Point
	path []spatial.Point
	next int
	// Follow
	target          int64
	keep            int64
	stopWhenReached bool
}

// Move 是实体的移动组件。它的方法只能在实体锁内调用。
type Move struct {
	mb      *MapBasic
	reg     *heartbeat.Reg
	terrain spatial.Terrain
	locate  Locator

	st moveState
	// beatStart 是本拍开始时的推进状态：AOI 排队满时整拍作废、回到这里（只在 beat 内有效）。
	beatStart moveState
	nextToken uint64 // 只增，不随回滚恢复：被回滚的移动用过的号不再发
	rbTx      nest.TransactionID
}

// NewMove 构造组件并在 host 上登记移动心跳 "move"（停止状态）；在实体工厂或 OnInitFinish 里调用。
func NewMove(host *heartbeat.Host, mb *MapBasic, config MoveConfig) (*Move, error) {
	if host == nil || mb == nil {
		return nil, errors.New("movement: move needs a heartbeat host and a map basic")
	}
	if config.Period == 0 {
		config.Period = DefaultMovePeriod
	}
	m := &Move{mb: mb, terrain: config.Terrain, locate: config.Locate}
	reg, err := host.Register("move", config.Period, m.beat, config.Options...)
	if err != nil {
		return nil, err
	}
	m.reg = reg
	return m, nil
}

// MapBasic 返回移动写入的位置组件。
func (m *Move) MapBasic() *MapBasic { return m.mb }

// Reg 返回移动心跳登记（诊断、测试用；启停由 Move 管理）。
func (m *Move) Reg() *heartbeat.Reg { return m.reg }

// Moving 报告是否有进行中的移动（含跟随）。
func (m *Move) Moving() bool { return m.st.mode != modeIdle }

// Token 返回进行中移动的编号，没有时为 0。
func (m *Move) Token() uint64 { return m.st.token }

// MoveTo 沿路径移动：path 是途经点（不含当前位置），最后一个是终点；speed 是每秒格数。取代进行中的移动（旧的以
// Replaced 回调）。返回本次移动的编号；之后每拍按 Elapsed 推进，到达等结论经 done 回调（done 可为 nil）。
// 寻路这类重计算在调用之前做，不在每拍里做。
func (m *Move) MoveTo(path []spatial.Point, speed float64, done Done) (uint64, error) {
	if len(path) == 0 {
		return 0, errors.New("movement: move to needs at least one point")
	}
	if err := m.checkStart(speed); err != nil {
		return 0, err
	}
	st := m.startState(speed, done)
	st.mode, st.path = modePath, append([]spatial.Point(nil), path...)
	return m.begin(st), nil
}

// Follow 跟随实体 target：每拍经 MoveConfig.Locate 读它最近一次提交的位置，走到与它相距 keep 格以内；
// stopWhenReached 为 true 时到达即以 Arrived 结束，否则一直跟随直到目标消失（TargetLost）、离开本场景
// （TargetLeft）或被 Stop / 取代。
func (m *Move) Follow(target int64, speed float64, keep int64, stopWhenReached bool, done Done) (uint64, error) {
	if m.locate == nil {
		return 0, errors.New("movement: follow needs MoveConfig.Locate")
	}
	if target == 0 || target == m.mb.owner.ID() {
		return 0, fmt.Errorf("movement: cannot follow entity %d", target)
	}
	if keep < 0 {
		return 0, fmt.Errorf("movement: follow distance %d is negative", keep)
	}
	if err := m.checkStart(speed); err != nil {
		return 0, err
	}
	st := m.startState(speed, done)
	st.mode, st.target, st.keep, st.stopWhenReached = modeFollow, target, keep, stopWhenReached
	return m.begin(st), nil
}

// Stop 停止编号为 token 的移动（0 表示不论哪个）；不是进行中的那个时什么也不做、返回 false。调用方主动停的不回调
// Done。停下就停心跳。
func (m *Move) Stop(token uint64) bool {
	if m.st.mode == modeIdle || (token != 0 && token != m.st.token) {
		return false
	}
	m.guard()
	m.st = moveState{}
	m.reg.Stop()
	return true
}

func (m *Move) checkStart(speed float64) error {
	if !(speed > 0) || math.IsInf(speed, 1) {
		return fmt.Errorf("movement: speed %v must be positive and finite", speed)
	}
	if !m.mb.InScene() {
		return ErrNotInScene
	}
	return nil
}

func (m *Move) startState(speed float64, done Done) moveState {
	at := m.mb.Pos()
	m.nextToken++
	return moveState{token: m.nextToken, done: done, speed: speed, sceneID: m.mb.SceneID(),
		x: float64(at.X), y: float64(at.Y), cell: at}
}

// begin 换上新的移动并挂心跳；进行中的旧移动以 Replaced 回调（在新状态生效之后，回调里读到的是新移动）。
func (m *Move) begin(st moveState) uint64 {
	m.guard()
	old := m.st
	m.st = st
	m.reg.Start()
	if old.mode != modeIdle && old.done != nil {
		at := m.mb.Pos()
		old.done(old.token, Result{Reason: Replaced, At: at})
	}
	return st.token
}

// beat 是移动心跳的回调：按 Elapsed 推进一次（不追帧：掉拍后 Elapsed 覆盖整段，一次走完），写 MapBasic（经
// QueueMove 进 AOI），到达 / 被阻挡 / 离开场景 / 目标消失时停心跳并回调 Done。
func (m *Move) beat(beat heartbeat.Beat) error {
	if m.st.mode == modeIdle {
		m.reg.Stop()
		return nil
	}
	m.guard()
	if !m.mb.InScene() || m.mb.SceneID() != m.st.sceneID {
		m.finish(Result{Reason: LeftScene, At: m.mb.Pos()})
		return nil
	}
	if at := m.mb.Pos(); at != m.st.cell {
		// 位置被别处改了（瞬移、拉回）：从新位置接着走。
		m.st.x, m.st.y, m.st.cell = float64(at.X), float64(at.Y), at
	}
	m.beatStart = m.st
	defer func() { m.beatStart = moveState{} }()
	budget := m.st.speed * beat.Elapsed.Seconds()
	switch m.st.mode {
	case modePath:
		m.walkPath(budget)
	case modeFollow:
		m.follow(budget)
	}
	return nil
}

// walkPath 沿路径走 budget 格。
func (m *Move) walkPath(budget float64) {
	from := m.st.cell
	for budget > 0 && m.st.next < len(m.st.path) {
		goal := m.st.path[m.st.next]
		used, reached, blocked := m.advance(float64(goal.X), float64(goal.Y), budget, 0)
		budget -= used
		if blocked {
			m.commitStep(from, Result{Reason: Blocked})
			return
		}
		if !reached {
			break
		}
		m.st.x, m.st.y = float64(goal.X), float64(goal.Y)
		m.st.next++
	}
	if m.st.next >= len(m.st.path) {
		m.commitStep(from, Result{Reason: Arrived})
		return
	}
	m.commitStep(from, Result{})
}

// follow 朝目标最近一次提交的位置走 budget 格，停在距它 keep 格处。
func (m *Move) follow(budget float64) {
	snap, ok := m.locate(m.st.target)
	switch {
	case !ok || !snap.Present:
		m.finish(Result{Reason: TargetLost, At: m.mb.Pos()})
		return
	case snap.SceneID != m.st.sceneID:
		m.finish(Result{Reason: TargetLeft, At: m.mb.Pos()})
		return
	}
	from := m.st.cell
	gx, gy := float64(snap.Pos.X), float64(snap.Pos.Y)
	keep := float64(m.st.keep)
	if math.Hypot(gx-m.st.x, gy-m.st.y) <= keep {
		if m.st.stopWhenReached {
			m.finish(Result{Reason: Arrived, At: m.mb.Pos()})
		}
		return
	}
	_, reached, blocked := m.advance(gx, gy, budget, keep)
	switch {
	case blocked:
		m.commitStep(from, Result{Reason: Blocked})
	case reached && m.st.stopWhenReached:
		m.commitStep(from, Result{Reason: Arrived})
	default:
		m.commitStep(from, Result{})
	}
}

// advance 从 (x, y) 朝 (gx, gy) 走至多 budget 格，停在距目标 stopShort 处；返回用掉的距离、是否到了（停止点）、是否
// 被挡。Terrain 按 0.5 格的步长采样：每进入一个新格问一次，被挡就停在上一个采样点。
func (m *Move) advance(gx, gy, budget, stopShort float64) (used float64, reached, blocked bool) {
	dx, dy := gx-m.st.x, gy-m.st.y
	dist := math.Hypot(dx, dy) - stopShort
	if dist <= 0 {
		return 0, true, false
	}
	step := min(budget, dist)
	reached = step >= dist
	ux, uy := dx/(dist+stopShort), dy/(dist+stopShort)
	if m.terrain == nil {
		m.st.x += ux * step
		m.st.y += uy * step
		return step, reached, false
	}
	samples := int(math.Ceil(step / 0.5))
	last := roundPoint(m.st.x, m.st.y)
	sx, sy := m.st.x, m.st.y
	for i := 1; i <= samples; i++ {
		d := step * float64(i) / float64(samples)
		px, py := sx+ux*d, sy+uy*d
		cell := roundPoint(px, py)
		if cell != last {
			if !m.terrain.InBounds(cell) || m.terrain.Blocked(cell) {
				return d, false, true
			}
			last = cell
		}
		m.st.x, m.st.y = px, py
	}
	return step, reached, false
}

// commitStep 把这一拍走到的格写进 MapBasic（朝向取本拍的位移方向）；result 有结论时随后结束。写被拒时：AOI 排队满
// （policy.ErrInterestQueueFull，背压）整拍作废、停在原处，下一拍接着走（这一拍的时间不补）；其他错误退回本拍开始
// 的位置、以 Refused 结束。
func (m *Move) commitStep(from spatial.Point, result Result) {
	cell := roundPoint(m.st.x, m.st.y)
	if cell != m.mb.Pos() {
		if err := m.mb.SetPos(cell); err != nil {
			if errors.Is(err, policy.ErrInterestQueueFull) {
				m.st = m.beatStart
				return
			}
			m.st.x, m.st.y = float64(from.X), float64(from.Y)
			m.finish(Result{Reason: Refused, At: from, Err: err})
			return
		}
		m.mb.SetFacing(FacingOf(float64(cell.X-from.X), float64(cell.Y-from.Y)))
	}
	m.st.cell = cell
	if result.Reason != 0 {
		result.At = cell
		m.finish(result)
	}
}

// finish 结束进行中的移动：停心跳，再回调 Done（回调在状态清空之后，回调里可以立刻开始新的移动）。
func (m *Move) finish(result Result) {
	done, token := m.st.done, m.st.token
	m.st = moveState{}
	m.reg.Stop()
	if done != nil {
		done(token, result)
	}
}

// guard 在当前事务里第一次改 moveState 前登记回滚恢复（每个事务一次）：回滚的一拍不算走过，下一拍的 Elapsed
// 覆盖它；回滚的 MoveTo / Stop 不生效。心跳登记的启停由 heartbeat 自己随事务处理。
func (m *Move) guard() {
	tx := nest.CurrentRollbackTx()
	if tx == nil || tx.ID() == m.rbTx {
		return
	}
	m.rbTx = tx.ID()
	prev := m.st
	tx.DeferRollback(func() error {
		m.st = prev
		return nil
	})
}

func roundPoint(x, y float64) spatial.Point {
	return spatial.Point{X: int64(math.Round(x)), Y: int64(math.Round(y))}
}
