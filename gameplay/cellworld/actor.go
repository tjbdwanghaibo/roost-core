package cellworld

import (
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
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

// integrate 把位置积分 dt（advance 保证 dt 不超过 Period），夹在世界内；NPC 碰到世界边缘反弹，玩家停在边缘。
func (n *Node) integrate(st *ActorState, dt time.Duration) {
	if st.VX == 0 && st.VY == 0 || dt <= 0 {
		return
	}
	bounds := n.cfg.Grid.Bounds()
	nx := st.X + st.VX*int64(dt)/int64(time.Second)
	ny := st.Y + st.VY*int64(dt)/int64(time.Second)
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
	st.X = clamp(nx, bounds.Min.X, bounds.Max.X-1, &st.VX)
	st.Y = clamp(ny, bounds.Min.Y, bounds.Max.Y-1, &st.VY)
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
