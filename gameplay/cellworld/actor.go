package cellworld

import (
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Role 是一个角色 / NPC 在它所在格里的身份，存在 SceneActor / Monster 的 DAO 里。
type Role uint8

const (
	// RoleNone：还没装好（刚建出实体、未写状态）。
	RoleNone Role = 0
	// RoleReal：唯一写权威。
	RoleReal Role = 1
	// RoleFrozen：已发出交接、等结论；只读，不积分、不接受输入与伤害。
	RoleFrozen Role = 2
	// RoleGone：已交出（或放弃、离场），实体等待销毁；什么都不做。
	RoleGone Role = 3
)

func (r Role) String() string {
	switch r {
	case RoleReal:
		return "real"
	case RoleFrozen:
		return "frozen"
	case RoleGone:
		return "gone"
	}
	return "none"
}

// HandoffState 是冻结中的交接：目标格、epoch、开始与上次发送的系统时间（UnixNano）。
type HandoffState struct {
	Target    CellID `msgpack:"t"`
	Epoch     uint64 `msgpack:"e"`
	StartedAt int64  `msgpack:"s"`
	LastSent  int64  `msgpack:"l"`
}

// ActorState 是一个角色 / NPC 在格内的全部状态：空间与战斗字段（EntityState）加交接、输入、去重、租约与回写的进度。
// 它整份存在 SceneActor / Monster 的 DAO 里（nopersist），事务内改、回滚即还原，交接时整份带走（Offer）。
// 值语义：Handoff 与 Applied 一旦写进 DAO 就不再原地修改，改动一律换新的指针 / 切片（DAO 的回滚前像可能引用旧的）。
type ActorState struct {
	EntityState
	Role       Role          `msgpack:"r"`
	LastInput  uint64        `msgpack:"in,omitempty"`
	Applied    []AppliedHit  `msgpack:"a,omitempty"`
	Handoff    *HandoffState `msgpack:"h,omitempty"`
	RetryAfter int64         `msgpack:"ra,omitempty"`
	// LeaseUntil：玩家的 SceneActor 在这之前没收到 Base 的输入 / 续租就自行离场（Base 进程消失后的清理）。NPC 为 0。
	LeaseUntil int64 `msgpack:"lu,omitempty"`
	// WritebackAt：下一次定时回写 Player 的时间；玩家才有。
	WritebackAt int64 `msgpack:"wb,omitempty"`
	// Entry 是玩家这次入场的序号（Base 编号，见 BaseState.Entry），Enter 时写入、交接时整份带走；Leave 只删 Entry 相同的
	// Real（RV-20261106-03）。NPC 为 0。
	Entry uint64 `msgpack:"en,omitempty"`
}

// Actor 是 game 的 SceneActor / Monster 实体为格内模拟提供的能力。三个方法都只在实体自己的 Nest 锁内调用：
//
//   - CellState / SetCellState：读写 ActorState。SetCellState 必须经 DAO setter 写（登记逆操作与同步脏标），
//     这样心跳或 handler 的事务回滚时状态一起还原（A1：事务内会改的状态放在 DAO）。
//   - StartCell：启动实体的格内心跳（Node.ActorBeat 作回调）。在事务里调用时按心跳的规则在提交后生效。
//   - SetCellPeriod：给格内心跳换档（heartbeat.Reg.SetPeriod）。Node 在角色的档位（beatPeriod）变化的那笔事务里调用：
//     提交后生效、回滚作废；未启动时只记下档位，StartCell 用它。
type Actor interface {
	fentity.IThreadSafeEntity
	CellState() ActorState
	SetCellState(ActorState)
	StartCell() error
	SetCellPeriod(time.Duration) error
}

// actorEffects 是一拍 / 一个 handler 对一个角色产生的、要在提交之后做的事。
type actorEffects struct {
	sends   []outgoing
	report  *ActorReport
	destroy bool
}

// ActorReport 是角色在提交之后告诉所在格区域（Region，Scene 实体上）的事实：最新状态与身份。角色的心跳不能锁 Scene
// （锁序：Scene 在前），所以经 Node 的出站队列批量投给 Scene 的 handler。
type ActorReport struct {
	State   EntityState   `msgpack:"s"`
	Role    Role          `msgpack:"r"`
	Handoff *HandoffState `msgpack:"h,omitempty"`
	// Removed：角色离开本格（交出、放弃、离场）。Tomb 为 true 时区域留墓碑并向邻格发删除（放弃 / 离场）；
	// 交出时为 false，区域按位置把它转成 Ghost（owner = Handoff.Target）。
	Removed bool `msgpack:"rm,omitempty"`
	Tomb    bool `msgpack:"tb,omitempty"`
}

// stepActor 推进一个角色一拍：积分（慢档按 Period 分步，见 advance）→ 判交接（冻结）→ 冻结中的重传 / 放弃 → 租约到期离场 → 定时回写。
// 纯函数：只改 st、返回提交后要做的事，不碰网络与实体管理器（单元测试直接调用）。
func (n *Node) stepActor(st *ActorState, now time.Time, elapsed time.Duration) actorEffects {
	var fx actorEffects
	nowNano := now.UnixNano()
	switch st.Role {
	case RoleReal:
		if st.Kind == KindPlayer && st.LeaseUntil != 0 && nowNano > st.LeaseUntil {
			// Base 消失（进程崩溃或网络隔离）：不再等它，离场。最后一份状态回写给 Base（它若还在会收下）。
			n.leave(st, &fx, nowNano)
			n.stats.leaseExpired.Add(1)
			return fx
		}
		if n.advance(st, nowNano, elapsed, &fx) {
			return fx
		}
		st.Ver.Seq++
		if st.Kind == KindPlayer && st.Base != 0 && nowNano >= st.WritebackAt {
			st.WritebackAt = nowNano + int64(n.cfg.WritebackEvery)
			fx.sends = append(fx.sends, outgoing{sid: st.Base, msg: &wireMsg{Kind: kindWriteback, Writeback: &writebackMsg{State: st.EntityState}}})
		}
	case RoleFrozen:
		p := st.Handoff
		if nowNano-p.StartedAt >= int64(n.cfg.HandoffAbort) {
			// 无法证明目标没装：只删不恢复，告诉 Base 交接放弃，由它按最后位置重新进入。
			n.stats.handoffsAborted.Add(1)
			if st.Kind == KindPlayer && st.Base != 0 {
				fx.sends = append(fx.sends, outgoing{sid: st.Base, msg: &wireMsg{Kind: kindLost, Lost: &lostNotice{State: st.EntityState}}})
			}
			st.Role, st.Handoff = RoleGone, nil
			fx.destroy = true
			fx.report = &ActorReport{State: st.EntityState, Role: RoleGone, Removed: true, Tomb: true, Handoff: p}
			if n.cfg.Hooks.HandoffAborted != nil {
				n.cfg.Hooks.HandoffAborted(n.cell, st.ID, p.Epoch)
			}
			return fx
		}
		if nowNano-p.LastSent >= int64(n.cfg.OfferRetry) {
			// 换一份新的 HandoffState：旧指针可能被事务的前像引用，原地改会让回滚还原出改过的值。
			next := *p
			next.LastSent = nowNano
			st.Handoff = &next
			n.stats.offerRetransmits.Add(1)
			fx.sends = append(fx.sends, n.offerTo(st))
		}
		return fx
	default:
		return fx
	}
	fx.report = &ActorReport{State: st.EntityState, Role: st.Role}
	return fx
}

// freeze 冻结 Real 并准备 Offer：epoch 在这里 +1，交接后的状态、去重表与输入序号整份带走（含档位 Idle：冻结中按快档
// 跳，新格按它装入）。提交之后才发 Offer（事务回滚 = 没冻结 = 没交接）。
func (n *Node) freeze(st *ActorState, target CellID, nowNano int64, fx *actorEffects) {
	st.Ver = Version{Epoch: st.Ver.Epoch + 1, Seq: st.Ver.Seq + 1}
	st.Role = RoleFrozen
	st.Handoff = &HandoffState{Target: target, Epoch: st.Ver.Epoch, StartedAt: nowNano, LastSent: nowNano}
	st.Applied = pruneApplied(st.Applied, nowNano, n.cfg.DedupWindow)
	n.stats.handoffsStarted.Add(1)
	fx.sends = append(fx.sends, n.offerTo(st))
	h := *st.Handoff
	fx.report = &ActorReport{State: st.EntityState, Role: RoleFrozen, Handoff: &h}
}

func (n *Node) offerTo(st *ActorState) outgoing {
	return outgoing{sid: n.cfg.Grid.SID(st.Handoff.Target), msg: &wireMsg{Kind: kindOffer, Offer: &handoffOffer{
		State: st.EntityState, From: n.cell, LastInput: st.LastInput, Applied: append([]AppliedHit(nil), st.Applied...), Entry: st.Entry,
	}}}
}

// leave 让 Real 离场（Base 的 Leave、租约到期）：最后一份状态回写给 Base，邻格的 Ghost 随删除消息消失。
func (n *Node) leave(st *ActorState, fx *actorEffects, nowNano int64) {
	if st.Kind == KindPlayer && st.Base != 0 {
		fx.sends = append(fx.sends, outgoing{sid: st.Base, msg: &wireMsg{Kind: kindWriteback, Writeback: &writebackMsg{State: st.EntityState}}})
	}
	st.Ver.Seq++
	st.Role = RoleGone
	fx.destroy = true
	fx.report = &ActorReport{State: st.EntityState, Role: RoleGone, Removed: true, Tomb: true}
}

// Surface 是带高度的地面（可编辑地形方案 P3 的跨格移动；gameplay/terrain 的 Walker 实现它，与 movement.Surface 同形）。
// 积分时把位置按 Cell 归到地面格，每进入一个新的地面格问一次 Step(上一格, 新格)：不放行就停在上一个点；放行时 speed
// 是新格里的速度千分比（游泳 < 1000），这一格里每走 1 单位消耗 1000 / speed 单位的路程。方法在角色的 Nest 锁内、
// short 池上调用：只能读内存、不取锁、不阻塞。
//
// 跨格：角色在交接之前可能走出本格（至多 Hysteresis + 一步），所以地面要覆盖本格外扩 GhostBand 的范围——分区进程的
// 地形视图本来就装着这个范围，邻格写的块是 Ghost 副本（gameplay/terrain 的 Replicator 推来的已提交版本）。块没装入
// （副本装不进来、换版前后包版本不同、地图外）时 Step 答 false：当作被挡，不猜平地。交接之后角色在目标格由目标格的
// Node 积分，按目标格自己的地面判定。
type Surface interface {
	// Cell 返回世界坐标所在的地面格（不透明的格坐标，只拿来比较与传回 Step）。
	Cell(p spatial.Point) spatial.Point
	// Step 报告能否从地面格 from 走进相邻的地面格 to，以及在 to 里的速度千分比；from == to 表示站在 from，
	// ok 为 false 时 from 本身没有定义（地图外、块没装入），当作被挡。
	Step(from, to spatial.Point) (speed int32, ok bool)
}

// walkResult 是一步沿地面走的结果。
type walkResult uint8

const (
	walkReached walkResult = iota // 走到了这一步的终点
	walkSlowed                    // 路程用完了（在减速的地面格里），没走到终点
	walkBlocked                   // 被挡：下一个地面格不放行，或站立的地面格本身没有定义
)

// integrate 把位置积分 dt（advance 保证 dt 不超过 Period），夹在世界内；NPC 碰到世界边缘反弹，玩家停在边缘。
//
// ground 非 nil 时（地面钩子，NodeConfig.Surface）沿这一步的直线逐个整数点走（walkOn）：被地面挡住时停在上一个
// 合法点，玩家速度清零（与停在世界边缘相同：等 Base 的下一个移动输入），NPC 速度反向（与碰到世界边缘反弹相同，
// 但两个分量都反向：地形的挡没有“哪条轴碰了边”，原路返回一定走得通——它刚从那里来，除非这期间地形又被改了）。
// 在游泳等减速的地面格里路程用完时停在用完的点，速度不变。只有走到了终点，世界边缘的速度规则才生效。
// ground 为 nil 时与没有地面钩子之前逐位相同。
func (n *Node) integrate(st *ActorState, dt time.Duration, ground Surface) {
	if st.VX == 0 && st.VY == 0 || dt <= 0 {
		return
	}
	bounds := n.cfg.Grid.Bounds()
	nx := st.X + st.VX*int64(dt)/int64(time.Second)
	ny := st.Y + st.VY*int64(dt)/int64(time.Second)
	vx, vy := st.VX, st.VY
	clamp := func(v, lo, hi int64, vel *int64) int64 {
		switch {
		case v < lo:
			v = lo
		case v > hi:
			v = hi
		default:
			return v
		}
		if st.Kind == KindNPC {
			*vel = -*vel
		} else {
			*vel = 0
		}
		return v
	}
	tx := clamp(nx, bounds.Min.X, bounds.Max.X-1, &vx)
	ty := clamp(ny, bounds.Min.Y, bounds.Max.Y-1, &vy)
	if ground == nil {
		st.X, st.Y, st.VX, st.VY = tx, ty, vx, vy
		return
	}
	x, y, result := walkOn(ground, st.X, st.Y, tx, ty, max(abs64(nx-st.X), abs64(ny-st.Y)))
	st.X, st.Y = x, y
	switch result {
	case walkReached:
		st.VX, st.VY = vx, vy
	case walkBlocked:
		n.stats.surfaceBlocks.Add(1)
		if st.Kind == KindNPC {
			st.VX, st.VY = -st.VX, -st.VY
		} else {
			st.VX, st.VY = 0, 0
		}
	}
}

// walkOn 沿 (x0, y0) → (tx, ty) 的直线逐个整数点走：第 i 个点是 (x0 + dx·i/n, y0 + dy·i/n)，n = max(|dx|, |dy|)，
// 相邻两点每个分量至多差 1，所以地面格每次至多变到相邻的一格（对角由 Step 自己判断不切角）。按整数点走而不是按地面格
// 的边界解析求交：地面格对这里不透明（Surface 只给 Cell / Step），停下的点精确到 1 单位。每个点问一次 Cell，进入新
// 地面格时问一次 Step。
//
// 路程按千分比计：reach 是这一步没被世界边缘夹过的位移的 max(|dx|, |dy|)（与点数同一度量），预算 reach × 1000，在速度
// 千分比 s 的地面格里每走一个点花 ⌈10⁶ / s⌉（s = 1000 时恰好 1000，reach ≥ n，正常行走一定走到终点）。预算按没夹过的
// 位移算：游泳时夹到世界边缘之前的那段照样按全速的 s / 1000 走，不因终点被夹近而少走。站立的地面格没有定义时一步
// 不走（被挡）。
func walkOn(ground Surface, x0, y0, tx, ty, reach int64) (x, y int64, result walkResult) {
	dx, dy := tx-x0, ty-y0
	steps := max(abs64(dx), abs64(dy))
	if steps == 0 {
		return x0, y0, walkReached
	}
	cell := ground.Cell(spatial.Point{X: x0, Y: y0})
	speed, ok := ground.Step(cell, cell)
	if !ok || speed <= 0 {
		return x0, y0, walkBlocked
	}
	x, y = x0, y0
	left := reach * 1000
	for i := int64(1); i <= steps; i++ {
		cost := (1_000_000 + int64(speed) - 1) / int64(speed)
		if cost > left {
			return x, y, walkSlowed
		}
		px, py := x0+dx*i/steps, y0+dy*i/steps
		if next := ground.Cell(spatial.Point{X: px, Y: py}); next != cell {
			nextSpeed, ok := ground.Step(cell, next)
			if !ok || nextSpeed <= 0 {
				return x, y, walkBlocked
			}
			cell, speed = next, nextSpeed
		}
		x, y, left = px, py, left-cost
	}
	return x, y, walkReached
}

// pruneApplied 删掉超出去重窗口的记录（返回新切片，不改传入的底层数组：它可能还被回滚前像引用）。
func pruneApplied(applied []AppliedHit, nowNano int64, window time.Duration) []AppliedHit {
	cutoff := nowNano - int64(window)
	out := make([]AppliedHit, 0, len(applied))
	for _, a := range applied {
		if a.At >= cutoff {
			out = append(out, a)
		}
	}
	return out
}
