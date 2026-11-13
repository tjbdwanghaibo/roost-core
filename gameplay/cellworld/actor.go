package cellworld

import (
	"crypto/rand"
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
	// RolePending 已预留容量，正在锁外确认授权；不移动、不受伤、不出现在视野里。
	RolePending Role = 4
	// Dying 已停止模拟，等持久死亡事实；未确认前不对外报告结算成功。
	RoleDying Role = 5
)

func (r Role) String() string {
	switch r {
	case RoleReal:
		return "real"
	case RoleFrozen:
		return "frozen"
	case RoleGone:
		return "gone"
	case RolePending:
		return "pending"
	case RoleDying:
		return "dying"
	}
	return "none"
}

// HandoffState 是冻结中的交接：目标格、epoch、开始与上次发送的系统时间（UnixNano）。
type HandoffState struct {
	ID        string `msgpack:"id"`
	Target    CellID `msgpack:"t"`
	Epoch     uint64 `msgpack:"e"`
	StartedAt int64  `msgpack:"s"`
	LastSent  int64  `msgpack:"l"`
}

// ActorState 是一个角色 / NPC 在格内的全部状态：空间与战斗字段（EntityState）加交接、输入、去重、租约与回写的进度。
// 它整份存在 SceneActor / Monster 的 DAO 里（nopersist），事务内改、回滚即还原，交接时整份带走（Offer）。
// 值语义：Handoff 与 Applied 一旦写进 DAO 就不再原地修改，改动一律换新的指针 / 切片（DAO 的回滚前像可能引用旧的）。
type ActorState struct {
	SpawnGeneration uint64 `msgpack:"sg,omitempty"`
	EntityState
	Recovery    RecoveryState `msgpack:"rc"`
	Death       *DeathFact    `msgpack:"df,omitempty"`
	DeathAcked  bool          `msgpack:"da,omitempty"`
	DeathNextAt int64         `msgpack:"dt,omitempty"`
	// Grant 是已取得的共享写授权；Pending 只在等待初次入场 / 接受交接时非空。
	Grant      OwnerGrant    `msgpack:"og"`
	Pending    *PendingOwner `msgpack:"op,omitempty"`
	CloseEntry bool          `msgpack:"ce,omitempty"`
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
	// FracX / FracY 是位置不足 1 单位的零头，千分位整数，范围 (-1000, 1000)：真实位置 = X + FracX / 1000。积分（integrate）
	// 每子步把零头与新位移相加，整数部分加到位置、余数留在这里，所以拍长不是 Period 的整数倍、每拍不足 1 单位的低速都不丢
	// 位移。与位置一起在 DAO 里（事务回滚一起还原），随交接带走；进场、生成为 0。各情形的规则见 integrate。
	FracX int32 `msgpack:"fx,omitempty"`
	FracY int32 `msgpack:"fy,omitempty"`
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
	hooks  []func()
	sends  []outgoing
	report *ActorReport
	owners []ownerWork
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
	case RolePending, RoleGone, RoleDying:
		fx.owners = append(fx.owners, n.ownerWork(*st))
		return fx
	case RoleReal:
		beforeState, beforeRecovery := st.EntityState, st.Recovery.Revision
		beforeEffects, beforeCooldowns := len(st.Recovery.Effects), len(st.Recovery.Cooldowns)
		st.Recovery = st.Recovery.At(now)
		if st.Death != nil {
			if st.Kind == KindPlayer && nowNano > st.LeaseUntil {
				st.CloseEntry = true
			}
			if nowNano >= st.DeathNextAt {
				st.DeathNextAt = now.Add(n.cfg.DeathRetry).UnixNano()
				fx.owners = append(fx.owners, ownerWork{kind: ownerDeathWork, state: *st})
			}
			fx.report = &ActorReport{State: st.EntityState, Role: st.Role}
			return fx
		}
		if st.Kind == KindPlayer && st.LeaseUntil != 0 && nowNano > st.LeaseUntil {
			// Base 消失（进程崩溃或网络隔离）：不再等它，离场。最后一份状态回写给 Base（它若还在会收下）。
			n.leave(st, &fx, nowNano)
			n.stats.leaseExpired.Add(1)
			return fx
		}
		if n.advance(st, nowNano, elapsed, &fx) {
			return fx
		}
		// 心跳与内容版本不是一个时钟；静止对象只续活，不触发客户端全量重打包。
		if st.EntityState != beforeState || st.Recovery.Revision != beforeRecovery || len(st.Recovery.Effects) != beforeEffects || len(st.Recovery.Cooldowns) != beforeCooldowns {
			st.Ver.Seq++
		}
		if st.Kind == KindPlayer && st.Base != 0 && nowNano >= st.WritebackAt {
			st.WritebackAt = nowNano + int64(n.cfg.WritebackEvery)
			fx.sends = append(fx.sends, outgoing{sid: st.Base, msg: &wireMsg{Kind: kindWriteback, Writeback: &writebackMsg{State: st.EntityState, Recovery: st.Recovery}}})
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
			fx.owners = append(fx.owners, n.ownerWork(*st))
			fx.report = &ActorReport{State: st.EntityState, Role: RoleGone, Removed: true, Tomb: true, Handoff: p}
			if n.cfg.Hooks.HandoffAborted != nil {
				id, epoch := st.ID, p.Epoch
				fx.hooks = append(fx.hooks, func() { n.cfg.Hooks.HandoffAborted(n.cell, id, epoch) })
			}
			return fx
		}
		if nowNano-p.LastSent >= int64(n.cfg.OfferRetry) {
			// 换一份新的 HandoffState：旧指针可能被事务的前像引用，原地改会让回滚还原出改过的值。
			next := *p
			next.LastSent = nowNano
			st.Handoff = &next
			n.stats.offerRetransmits.Add(1)
			fx.owners = append(fx.owners, n.ownerWork(*st))
		}
		return fx
	default:
		return fx
	}
	fx.report = &ActorReport{State: st.EntityState, Role: st.Role}
	return fx
}

// freeze 冻结 Real 并准备 Offer：epoch 在这里 +1，交接后的状态、去重表、输入序号与位置零头整份带走（含档位 Idle：冻结中按快档
// 跳，新格按它装入）。提交之后才发 Offer（事务回滚 = 没冻结 = 没交接）。
func (n *Node) freeze(st *ActorState, target CellID, nowNano int64, fx *actorEffects) {
	st.Ver = Version{Epoch: st.Ver.Epoch + 1, Seq: st.Ver.Seq + 1}
	st.Role = RoleFrozen
	st.Handoff = &HandoffState{ID: rand.Text(), Target: target, Epoch: st.Ver.Epoch, StartedAt: nowNano, LastSent: nowNano}
	st.Applied = pruneApplied(st.Applied, nowNano, n.cfg.DedupWindow)
	n.stats.handoffsStarted.Add(1)
	fx.owners = append(fx.owners, n.ownerWork(*st))
	h := *st.Handoff
	fx.report = &ActorReport{State: st.EntityState, Role: RoleFrozen, Handoff: &h}
}

func (n *Node) offerTo(st *ActorState) outgoing {
	return outgoing{sid: n.cfg.Grid.SID(st.Handoff.Target), msg: &wireMsg{Kind: kindOffer, Offer: &handoffOffer{
		State: st.EntityState, From: n.cell, Grant: st.Grant, Transfer: n.ownerTransfer(*st), LastInput: st.LastInput, Applied: transferApplied(st.Applied, st.Handoff.LastSent, n.cfg.DedupWindow), Entry: st.Entry,
		FracX: st.FracX, FracY: st.FracY, Recovery: st.Recovery, SpawnGeneration: st.SpawnGeneration,
	}}}
}

func transferApplied(applied []AppliedHit, now int64, window time.Duration) []transferredHit {
	out := make([]transferredHit, 0, len(applied))
	for _, a := range applied {
		remaining := int64(window) - max(0, now-a.At)
		if remaining > 0 {
			out = append(out, transferredHit{Hit: a.Hit, Remaining: remaining})
		}
	}
	return out
}

func restoreApplied(applied []transferredHit, now int64, window time.Duration) []AppliedHit {
	out := make([]AppliedHit, 0, len(applied))
	for _, a := range applied {
		if a.Remaining > 0 {
			out = append(out, AppliedHit{Hit: a.Hit, At: now - int64(window) + min(a.Remaining, int64(window))})
		}
	}
	return out
}

// leave 让 Real 离场（Base 的 Leave、租约到期）：最后一份状态回写给 Base，邻格的 Ghost 随删除消息消失。
func (n *Node) leave(st *ActorState, fx *actorEffects, nowNano int64) {
	if st.Kind == KindPlayer && st.Base != 0 {
		fx.sends = append(fx.sends, outgoing{sid: st.Base, msg: &wireMsg{Kind: kindWriteback, Writeback: &writebackMsg{State: st.EntityState, Recovery: st.Recovery}}})
	}
	st.Ver.Seq++
	st.Role = RoleGone
	fx.owners = append(fx.owners, n.ownerWork(*st))
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

// milli 是积分的计算单位：1 单位 = 1000 千分位。位移全程按千分位整数算（维护者 2026-11-10：“截断按照现在的浮点数*1000
// 作为整型数处理”）；位置字段与线上的位置仍是整数单位，不足 1 单位的零头存在 ActorState.FracX / FracY。
const milli = 1000

// integrate 把位置积分 dt（advance 保证 dt 不超过 Period），夹在世界内；NPC 碰到世界边缘反弹，玩家停在边缘。
//
// 位移按千分位整数计：这一子步的位移 = 速度（单位 / 秒）× dt / 1ms 千分位（向零截断，每子步丢掉不到 1 千分位），加上
// 上次留下的零头，整数部分加到位置、余数留作零头（settle）。真实位置 = 位置 + 零头 / 1000。零头的规则：
//
//   - 符号与范围：零头与“零头 + 位移”同号（Go 的整除、取余都向零截断），范围 (-1000, 1000)。整数位置是真实位置朝来路
//     截到整数：往负方向走时零头为负、整数位置在真实位置的正侧，走满 1 单位整数位置才变。
//   - 世界边缘：真实位置出了 [Min, Max-1]（整数位置越界，或恰在边上而零头指向界外）就停在边上、这条轴的零头清零，
//     并按原规则改这条轴的速度（玩家清零、NPC 反向）。
//   - 地面挡住：停在上一个合法整数点，两条轴的零头都清零（零头指向被挡的方向）。
//   - 游泳等减速：位移按地面格的速度千分比缩短，零头照常累计（见 walkOn），不按整点丢余数。
//   - 速度改变（新输入）、换档、交接被拒恢复 Real：零头不动——它是位置的一部分，与速度无关。
//   - 交接：零头随 Offer 带到目标格（handoffOffer.FracX / FracY），目标格接着累加；冻结中不积分，零头不变。
//   - 进场（Enter，含交接放弃后 Base 按最后位置重新进入）与生成（Spawn）：从整数位置开始，零头为 0。
//
// ground 非 nil 时（地面钩子，NodeConfig.Surface）沿这一步的直线逐个整数点走（walkOn）：被地面挡住时停在上一个
// 合法点，玩家速度清零（与停在世界边缘相同：等 Base 的下一个移动输入），NPC 速度反向（与碰到世界边缘反弹相同，
// 但两个分量都反向：地形的挡没有“哪条轴碰了边”，原路返回一定走得通——它刚从那里来，除非这期间地形又被改了）。
// 在游泳等减速的地面格里位移按速度千分比缩短，速度不变；世界边缘的速度规则按缩短后的终点判断。
// ground 为 nil 时与处处放行、速度 1000 的地面逐位相同。
func (n *Node) integrate(st *ActorState, dt time.Duration, ground Surface) {
	if st.VX == 0 && st.VY == 0 || dt <= 0 {
		return
	}
	// 单位 / 秒 × 纳秒 / 10⁶ = 千分位（× 1000 / 10⁹），溢出范围与原来的 V·dt / 1s 相同。
	dx := st.VX * int64(dt) / int64(time.Millisecond)
	dy := st.VY * int64(dt) / int64(time.Millisecond)
	bounds := n.cfg.Grid.Bounds()
	area := walkArea{lo: bounds.Min, hi: spatial.Point{X: bounds.Max.X - 1, Y: bounds.Max.Y - 1}}
	var w walkStop
	if ground == nil {
		w = area.settle(st.X, st.Y, int64(st.FracX)+dx, int64(st.FracY)+dy)
	} else {
		w = walkOn(ground, area, st.X, st.Y, int64(st.FracX), int64(st.FracY), dx, dy)
	}
	st.X, st.Y, st.FracX, st.FracY = w.x, w.y, int32(w.fx), int32(w.fy)
	if w.blocked {
		n.stats.surfaceBlocks.Add(1)
		if st.Kind == KindNPC {
			st.VX, st.VY = -st.VX, -st.VY
		} else {
			st.VX, st.VY = 0, 0
		}
		return
	}
	turn := func(v int64) int64 {
		if st.Kind == KindNPC {
			return -v
		}
		return 0
	}
	if w.edgeX {
		st.VX = turn(st.VX)
	}
	if w.edgeY {
		st.VY = turn(st.VY)
	}
}

// walkArea 是位置的合法范围：世界内的整数点 [lo, hi]（闭区间）。
type walkArea struct{ lo, hi spatial.Point }

// walkStop 是一子步积分的落点：整数位置、零头（千分位），被世界边缘夹住的轴，是否被地面挡住。
type walkStop struct {
	x, y, fx, fy int64
	edgeX, edgeY bool
	blocked      bool
}

// settle 从整数点 (x, y) 走 (mx, my) 千分位（已含零头），按 integrate 的零头规则落到世界内。
func (a walkArea) settle(x, y, mx, my int64) walkStop {
	var w walkStop
	w.x, w.fx, w.edgeX = settleAxis(x, mx, a.lo.X, a.hi.X)
	w.y, w.fy, w.edgeY = settleAxis(y, my, a.lo.Y, a.hi.Y)
	return w
}

// settleAxis：整数部分加到位置、余数留作零头（都向零截断，零头与 m 同号）；真实位置出了 [lo, hi] 就停在边上、零头清零。
func settleAxis(p, m, lo, hi int64) (pos, frac int64, edge bool) {
	pos, frac = p+m/milli, m%milli
	switch {
	case pos < lo || pos == lo && frac < 0:
		return lo, 0, true
	case pos > hi || pos == hi && frac > 0:
		return hi, 0, true
	}
	return pos, frac, false
}

// validFrac 是从线上收下的零头：不在 (-1000, 1000) 里的（坏消息）当作 0，不让一个子步把位置挪出好几个单位。
func validFrac(f int32) int32 {
	if f <= -milli || f >= milli {
		return 0
	}
	return f
}

// walkOn 沿地面从整数点 (x0, y0)（零头 fx0, fy0）走全速位移 (dx, dy) 千分位。
//
// 先按站立的地面格的速度千分比 s 把位移缩成 dx·s/1000，按 settle 算出终点（含世界边缘），再沿 (x0, y0) → 终点的直线
// 逐个整数点走：第 i 个点是 (x0 + DX·i/N, y0 + DY·i/N)，N = max(|DX|, |DY|)，相邻两点每个分量至多差 1，所以地面格每次
// 至多变到相邻的一格（对角由 Step 自己判断不切角）。每个点问一次 Cell，进入新地面格时问一次 Step：不放行就停在上一个点
// （blocked，零头清零）；放行且速度不同时，从这个点起把剩下的位移（没被世界边缘夹过的真实终点 − 这个点）按新旧速度之比
// 缩放、重新算终点接着走。所以进入新格的那一步按旧格的速度算（与原来按路程预算、每点花 ⌈10⁶ / s⌉ 的规则一致），但没有
// 按整点丢掉的余数，减速格里零头照常累计。位移按没夹过的终点缩放：游泳时夹到世界边缘之前的那段照样按全速的 s / 1000 走，
// 不因终点被夹近而少走。
//
// 按整数点走而不是按地面格的边界解析求交：地面格对这里不透明（Surface 只给 Cell / Step），停下的点精确到 1 单位。站立的
// 地面格没有定义时一步不走（被挡）。速度 1000 处处放行的地面与没有地面（settle）逐位相同。
func walkOn(ground Surface, area walkArea, x0, y0, fx0, fy0, dx, dy int64) walkStop {
	cell := ground.Cell(spatial.Point{X: x0, Y: y0})
	speed, ok := ground.Step(cell, cell)
	if !ok || speed <= 0 {
		return walkStop{x: x0, y: y0, blocked: true}
	}
	// (mx, my)：没夹过的真实终点相对整数点 (x0, y0) 的千分位位移。
	mx, my := fx0+dx*int64(speed)/1000, fy0+dy*int64(speed)/1000
	for {
		end := area.settle(x0, y0, mx, my)
		ex, ey := end.x-x0, end.y-y0
		steps := max(abs64(ex), abs64(ey))
		x, y := x0, y0
		rescaled := false
		for i := int64(1); i <= steps; i++ {
			px, py := x0+ex*i/steps, y0+ey*i/steps
			if next := ground.Cell(spatial.Point{X: px, Y: py}); next != cell {
				nextSpeed, ok := ground.Step(cell, next)
				if !ok || nextSpeed <= 0 {
					return walkStop{x: x, y: y, blocked: true}
				}
				cell = next
				if nextSpeed != speed {
					mx = (mx - (px-x0)*milli) * int64(nextSpeed) / int64(speed)
					my = (my - (py-y0)*milli) * int64(nextSpeed) / int64(speed)
					x0, y0, speed, rescaled = px, py, nextSpeed, true
					break
				}
			}
			x, y = px, py
		}
		if !rescaled {
			return end
		}
	}
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
