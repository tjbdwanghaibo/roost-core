package movement

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// DefaultMovePeriod 是移动动作缺省的 tick 周期（5Hz）：MoveTo / Follow 由实体的 ActionRunner 按它挂心跳。取 MMO / SLG
// 常见的 2～5Hz 的上端；要更细的（动作游戏）在 MoveToParam / FollowParam.Period 里给。
const DefaultMovePeriod = 200 * time.Millisecond

// StopReason 是一次移动结束的原因；0 表示还在移动。
type StopReason uint8

const (
	// Arrived：走完路径；Follow 设了 stopWhenReached 时到达保持距离。
	Arrived StopReason = iota + 1
	// Replaced：调用方手里的编号已不是进行中的移动（被另一次 MoveTo / Follow 取代，或已 Stop）。
	Replaced
	// Blocked：下一格被地形挡住，停在最后一个可走的位置。
	Blocked
	// LeftScene：自己离开了开始移动时所在的场景。
	LeftScene
	// TargetLost：跟随目标不在内存（卸载、删档）或找不到。
	TargetLost
	// TargetLeft：跟随目标不在自己的场景里。
	TargetLeft
	// Refused：写位置被拒（位置越界、AOI 不认识这个实体等），Result.Err 是原因；位置停在上一拍。
	Refused
)

func (r StopReason) String() string {
	switch r {
	case 0:
		return "moving"
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

// Result 是一次 Advance 的结论：Reason 为 0 表示还在移动，否则移动已结束（Move 回到空闲）。
type Result struct {
	Reason StopReason
	// At 是这一拍之后的位置。
	At  spatial.Point
	Err error
}

// Done 报告移动是否已结束。
func (r Result) Done() bool { return r.Reason != 0 }

// Locator 返回实体 id 最近一次提交的位置（MapBasic.Published）；ok 为 false 表示目标不在内存。在跟随者的实体锁内
// 调用，不能取目标的锁、不能阻塞。LocateIn 用实体查找构造它。
type Locator func(id int64) (Snapshot, bool)

// MoveConfig 是 NewMove 的输入。
type MoveConfig struct {
	// Terrain 非 nil 时每走进一个新格都问它：越界或被挡就以 Blocked 停下。
	Terrain spatial.Terrain
	// Surface 非 nil 时每拍开始调用一次，取这一拍的带高度地面（可编辑地形，见 Surface）；返回 nil 表示这一拍没有
	// 高度规则，推进与不设 Surface 时逐位相同。用函数而不是值：地面可能在实体建好之后才装上（游戏服务启动顺序），
	// 也可能随场景卸下。Terrain 与 Surface 可以同时设，两者都要放行才走。
	Surface func() Surface
	// Locate 是 Follow 读目标位置的入口；nil 时 Follow 返回错误。
	Locate Locator
}

// Surface 是带高度的地面（可编辑地形方案 P3，gameplay/terrain 的 Walker 实现它）。Move 把位置按 Cell 归到地面格，
// 每进入一个新的地面格问一次 Step(上一格, 新格)：不放行就以 Blocked 停在上一个采样点；放行时 speed 是新格里的速度
// 千分比（游泳 < 1000），这一格里每走 1 单位消耗 1000 / speed 单位的路程预算。方法在实体锁内调用，只能读内存、
// 不能阻塞（Nest short 池）。
type Surface interface {
	// Cell 返回世界坐标所在的地面格（不透明的格坐标，只拿来比较与传回 Step）。
	Cell(p spatial.Point) spatial.Point
	// Step 报告能否从地面格 from 走进相邻的地面格 to，以及在 to 里的速度千分比；from == to 表示站在 from，
	// ok 为 false 时 from 本身没有定义（地图外、块没装入），当作被挡。
	Step(from, to spatial.Point) (speed int32, ok bool)
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

// Move 是实体的移动组件，被动：只提供设定与推进的接口，自己不挂心跳。推进由动作驱动（参考 ssr：action 挂心跳、
// 在心跳里调 move）——MoveTo / Follow 动作在实体 ActionRunner 的心跳里每拍调一次 Advance（所有
// 控制实体行为的逻辑都经 action，不绕过动作直接驱动组件）。方法只能在实体锁内调用。
type Move struct {
	mb      *MapBasic
	terrain spatial.Terrain
	surface func() Surface
	locate  Locator
	// ground 是本次 Advance 的地面（只在 Advance 内有效；nil 表示这一拍没有高度规则）。
	ground Surface

	st        moveState
	nextToken uint64 // 只增，不随回滚恢复：被回滚的移动用过的号不再发
	rbTx      nest.TransactionID
	// result 是本次 Advance 的结论（只在 Advance 内有效）。
	result Result
	// beatStart 是本次 Advance 开始时的推进状态：AOI 排队满时整拍作废、回到这里（只在 Advance 内有效）。
	beatStart moveState
}

// NewMove 构造组件；在实体工厂或 OnInitFinish 里调用。
func NewMove(mb *MapBasic, config MoveConfig) (*Move, error) {
	if mb == nil {
		return nil, errors.New("movement: move needs a map basic")
	}
	return &Move{mb: mb, terrain: config.Terrain, surface: config.Surface, locate: config.Locate}, nil
}

// MapBasic 返回移动写入的位置组件。
func (m *Move) MapBasic() *MapBasic { return m.mb }

// Moving 报告是否有进行中的移动（含跟随）。
func (m *Move) Moving() bool { return m.st.mode != modeIdle }

// Token 返回进行中移动的编号，没有时为 0。
func (m *Move) Token() uint64 { return m.st.token }

// MoveTo 设定沿路径移动：path 是途经点（不含当前位置），最后一个是终点；speed 是每秒格数。取代进行中的移动
// （旧编号之后 Advance 得到 Replaced）。返回本次移动的编号，之后由调用方（MoveTo 动作）每拍 Advance。寻路这类
// 重计算在调用之前做，不在每拍里做。
func (m *Move) MoveTo(path []spatial.Point, speed float64) (uint64, error) {
	if len(path) == 0 {
		return 0, errors.New("movement: move to needs at least one point")
	}
	if err := m.checkStart(speed); err != nil {
		return 0, err
	}
	st := m.startState(speed)
	st.mode, st.path = modePath, append([]spatial.Point(nil), path...)
	return m.begin(st), nil
}

// Follow 设定跟随实体 target：每拍经 MoveConfig.Locate 读它最近一次提交的位置，走到与它相距 keep 格以内；
// stopWhenReached 为 true 时到达即以 Arrived 结束，否则一直跟随直到目标消失（TargetLost）、离开本场景
// （TargetLeft）或被 Stop / 取代。
func (m *Move) Follow(target int64, speed float64, keep int64, stopWhenReached bool) (uint64, error) {
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
	st := m.startState(speed)
	st.mode, st.target, st.keep, st.stopWhenReached = modeFollow, target, keep, stopWhenReached
	return m.begin(st), nil
}

// Stop 停止编号为 token 的移动（0 表示不论哪个）；不是进行中的那个时什么也不做、返回 false。
func (m *Move) Stop(token uint64) bool {
	if m.st.mode == modeIdle || (token != 0 && token != m.st.token) {
		return false
	}
	m.guard()
	m.st = moveState{}
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

func (m *Move) startState(speed float64) moveState {
	at := m.mb.Pos()
	m.nextToken++
	return moveState{token: m.nextToken, speed: speed, sceneID: m.mb.SceneID(), x: float64(at.X), y: float64(at.Y), cell: at}
}

func (m *Move) begin(st moveState) uint64 {
	m.guard()
	m.st = st
	return st.token
}

// Advance 把编号为 token 的移动推进 elapsed（调用方传心跳这一拍的 Beat.Elapsed：不追帧，掉拍后一次走完整段），
// 走到的格写进 MapBasic（经 QueueMove 进 AOI），返回结论；结论非 0 时移动已结束。token 不是进行中的移动时返回
// Replaced、什么都不改。AOI 排队满（policy.ErrInterestQueueFull，背压）时整拍作废、停在原处，返回“还在移动”，
// 下一拍接着走（这一拍的时间不补）。
func (m *Move) Advance(token uint64, elapsed time.Duration) Result {
	if m.st.mode == modeIdle || token != m.st.token {
		return Result{Reason: Replaced, At: m.mb.Pos()}
	}
	m.guard()
	m.result = Result{}
	if m.surface != nil {
		m.ground = m.surface()
	}
	defer func() { m.result, m.beatStart, m.ground = Result{}, moveState{}, nil }()
	if !m.mb.InScene() || m.mb.SceneID() != m.st.sceneID {
		m.finish(Result{Reason: LeftScene, At: m.mb.Pos()})
		return m.result
	}
	if at := m.mb.Pos(); at != m.st.cell {
		// 位置被别处改了（瞬移、拉回）：从新位置接着走。
		m.st.x, m.st.y, m.st.cell = float64(at.X), float64(at.Y), at
	}
	m.beatStart = m.st
	budget := m.st.speed * max(elapsed, 0).Seconds()
	switch m.st.mode {
	case modePath:
		m.walkPath(budget)
	case modeFollow:
		m.follow(budget)
	}
	if !m.result.Done() {
		m.result.At = m.mb.Pos()
	}
	return m.result
}

// walkPath 沿路径走 budget 格。
func (m *Move) walkPath(budget float64) {
	from := m.st.cell
	// 与当前位置重合的途经点不耗距离，0 时长（动作 Start 时的核对）也照样越过，已经在终点即到达。
	for m.st.next < len(m.st.path) {
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
	if m.ground != nil {
		return m.advanceOn(m.ground, gx, gy, budget, stopShort)
	}
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

// advanceOn 是有带高度地面时的 advance：同样按 0.5 格的步长采样（Terrain 照旧每进入一个新格问一次），另外每进入一个
// 新的地面格问 ground.Step；地面格的速度千分比折算路程预算（游泳时同样的预算走得更近）。被挡时停在上一个采样点。
// 站立的地面格本身没有定义（地图外、块没装入）时一步也不走，当作被挡（不猜）。
func (m *Move) advanceOn(ground Surface, gx, gy, budget, stopShort float64) (used float64, reached, blocked bool) {
	dx, dy := gx-m.st.x, gy-m.st.y
	dist := math.Hypot(dx, dy) - stopShort
	if dist <= 0 {
		return 0, true, false
	}
	ux, uy := dx/(dist+stopShort), dy/(dist+stopShort)
	last := roundPoint(m.st.x, m.st.y)
	cell := ground.Cell(last)
	speed, ok := ground.Step(cell, cell)
	if !ok || speed <= 0 {
		return 0, false, true
	}
	sx, sy := m.st.x, m.st.y
	left := budget
	var walked float64
	// 1e-9 的余量：walked 是逐段累加的浮点数，最后一段 walked + (dist − walked) 可能差一个 ulp 到不了 dist。
	for dist-walked > 1e-9 && left > 0 {
		seg := min(0.5, dist-walked)
		cost := seg * 1000 / float64(speed)
		if cost > left {
			seg, cost = left*float64(speed)/1000, left
		}
		d := walked + seg
		px, py := sx+ux*d, sy+uy*d
		if at := roundPoint(px, py); at != last {
			if m.terrain != nil && (!m.terrain.InBounds(at) || m.terrain.Blocked(at)) {
				return budget - left, false, true
			}
			if next := ground.Cell(at); next != cell {
				nextSpeed, ok := ground.Step(cell, next)
				if !ok || nextSpeed <= 0 {
					return budget - left, false, true
				}
				cell, speed = next, nextSpeed
			}
			last = at
		}
		m.st.x, m.st.y = px, py
		walked, left = d, left-cost
	}
	return budget - left, dist-walked <= 1e-9, false
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

// finish 结束进行中的移动：清状态，记下本次 Advance 的结论。
func (m *Move) finish(result Result) {
	m.st = moveState{}
	m.result = result
}

// guard 在当前事务里第一次改 moveState 前登记回滚恢复（每个事务一次）：回滚的一拍不算走过，下一拍的 Elapsed
// 覆盖它；回滚的 MoveTo / Stop 不生效。
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
