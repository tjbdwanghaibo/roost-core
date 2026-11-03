package cellworld

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Base 侧：Player 留在自己的 sid（BaseApp），只跟随玩家当前所在的格——转发输入与攻击、接收 witness 喂给客户端、
// 收下 SceneActor 的回写。玩家的路由状态（BaseState）存在 Player 实体上（nopersist 字段；HP、位置与快照版本由 game
// 映射到持久字段），看门狗与续租是 Player 的心跳（BaseBeat），不再是 v1 里带锁的玩家表。

// ErrNotInWorld 表示玩家没有进入分区 Scene（未 Enter 或已 Leave）。
var ErrNotInWorld = errors.New("cellworld: player is not in the partitioned scene")

// BaseState 是 Player 上的分区 Scene 路由状态。时间都是系统时间 UnixNano。
type BaseState struct {
	// Actor 是玩家的 SceneActor ID（跨格不变）；InWorld 表示已 Enter、未 Leave。
	Actor   int64 `msgpack:"a"`
	InWorld bool  `msgpack:"w"`
	// Cell / Epoch：当前所知的 owner 格 sid 与 epoch（只前进）。
	Cell  int32  `msgpack:"c"`
	Epoch uint64 `msgpack:"e"`
	// X / Y / HP / SnapVer：Player 的最近快照（回写与 witness 里的自己），重建 SceneActor 用。SnapVer 是被接受的
	// 最新回写版本：只接受更新的（幂等）。game 把这几项映射到持久字段。
	X, Y    int64   `msgpack:"x"`
	HP      int32   `msgpack:"hp"`
	SnapVer Version `msgpack:"sv"`
	// 移动意图（单调序号）与攻击序号。
	IntentSeq uint64 `msgpack:"is"`
	VX, VY    int64  `msgpack:"v"`
	AttackSeq uint64 `msgpack:"as"`
	// 看门狗：最近一次 witness、Enter、续租的时间；Enter 被拒时的 owner 提示；交接放弃（Lost）的 epoch 与时间。
	LastViewAt  int64  `msgpack:"lv"`
	LastEnterAt int64  `msgpack:"le"`
	LastLeaseAt int64  `msgpack:"ll"`
	EnterHint   int32  `msgpack:"eh"`
	LostEpoch   uint64 `msgpack:"lo"`
	LostAt      int64  `msgpack:"la"`
	// Shown：玩家已经以自己的 SceneActor mirror 进入 Base 的 Interest（第一份 witness 之后）。自己的 mirror 过期删除
	// （Hide + 注销）后再出现时，Interest 自己恢复“玩家看自己”（self 跟着目标的 Hide / Show 走，E2），这里不用再管。
	Shown bool `msgpack:"sh"`
}

// Pos 返回快照位置。
func (s BaseState) Pos() spatial.Point { return spatial.Point{X: s.X, Y: s.Y} }

// BasePlayer 是 game 的 Player 实体为 Base 侧提供的能力，方法只在实体自己的 Nest 锁内调用。SetBaseState 必须经 DAO
// setter 写（事务回滚还原）。StartBase 启动 Player 的 Base 心跳（Node.BaseBeat 作回调，周期 WatchPeriod）。
type BasePlayer interface {
	fentity.IThreadSafeEntity
	BaseState() BaseState
	SetBaseState(BaseState)
	StartBase() error
}

// View 是 Base 接受的一份 witness（观察点用）。
type View struct {
	Player    int64 // 角色（SceneActor）ID
	Cell      int32 // 发出它的格 sid
	Epoch     uint64
	Tick      uint64
	Self      EntityState
	Entities  []EntityState
	Truncated bool
	At        time.Time
	SentAt    time.Time
}

// AttackResult 是一次攻击的结论（见 DamageOutcome）。
type AttackResult struct {
	Hit       HitID
	Target    int64
	Outcome   DamageOutcome
	Reason    RejectReason
	Duplicate bool
	HP        int32
}

// ---- 业务入口：每个都是 Player 上的一次 Nest 请求 ----

type baseCmdKind uint8

const (
	cmdEnter baseCmdKind = iota + 1
	cmdMove
	cmdAttack
	cmdLeave
	cmdState
)

type baseCmd struct {
	kind   baseCmdKind
	actor  int64
	pos    spatial.Point
	hp     int32
	vx, vy int64
	target int64
	amount int32
}

// Enter 让 Player（playerEntity）以 SceneActor actor 在 pos 进入分区 Scene。Player 已有快照（之前回写过）时 hp 被快照覆盖。
func (n *Node) Enter(ctx context.Context, playerEntity, actor int64, pos spatial.Point, hp int32) error {
	_, err := n.cfg.Nest.Request(ctx, hBaseCmd, playerEntity, nest.Params{baseCmd{kind: cmdEnter, actor: actor, pos: pos, hp: hp}})
	return err
}

// Move 设定移动意图（速度，单位 / 秒），发给当前 owner；owner 未知或交接中丢失时由 witness 里的输入序号落后触发重发。
func (n *Node) Move(ctx context.Context, playerEntity int64, vx, vy int64) error {
	_, err := n.cfg.Nest.Request(ctx, hBaseCmd, playerEntity, nest.Params{baseCmd{kind: cmdMove, vx: vx, vy: vy}})
	return err
}

// Attack 让玩家攻击 target，结论经 Hooks.AttackResult 回来。owner 未知时返回 ErrNotInWorld。
func (n *Node) Attack(ctx context.Context, playerEntity, target int64, amount int32) (HitID, error) {
	out, err := n.cfg.Nest.Request(ctx, hBaseCmd, playerEntity, nest.Params{baseCmd{kind: cmdAttack, target: target, amount: amount}})
	if err != nil {
		return HitID{}, err
	}
	return out.(HitID), nil
}

// Leave 让玩家离开分区 Scene（尽力而为：消息丢了由 SceneActor 的租约兜底离场）。
func (n *Node) Leave(ctx context.Context, playerEntity int64) error {
	_, err := n.cfg.Nest.Request(ctx, hBaseCmd, playerEntity, nest.Params{baseCmd{kind: cmdLeave}})
	return err
}

// BaseStateOf 读 Player 的 BaseState（测试与诊断）。
func (n *Node) BaseStateOf(ctx context.Context, playerEntity int64) (BaseState, error) {
	out, err := n.cfg.Nest.Request(ctx, hBaseCmd, playerEntity, nest.Params{baseCmd{kind: cmdState}})
	if err != nil {
		return BaseState{}, err
	}
	return out.(BaseState), nil
}

func playerOf(es []fentity.IThreadSafeEntity) (BasePlayer, error) {
	if len(es) == 0 || es[0] == nil {
		return nil, errors.New("cellworld: player is not loaded")
	}
	p, ok := es[0].(BasePlayer)
	if !ok {
		return nil, fmt.Errorf("cellworld: entity %T is not a base player", es[0])
	}
	return p, nil
}

func (n *Node) handleBaseCmd(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	p, err := playerOf(es)
	if err != nil {
		return nil, err
	}
	cmd := params[0].(baseCmd)
	st := p.BaseState()
	now := time.Now()
	fx := &txEffects{}
	var ret any
	switch cmd.kind {
	case cmdState:
		return st, nil
	case cmdEnter:
		if st.InWorld {
			return nil, fmt.Errorf("cellworld: player %d already entered", p.ID())
		}
		cell, ok := n.cfg.Grid.CellAt(cmd.pos)
		if cmd.actor == 0 || !ok {
			return nil, fmt.Errorf("cellworld: enter %d at %v: outside world", cmd.actor, cmd.pos)
		}
		// 重新进入（离开后再进、Base 重启后进）：保留快照与序号。回写过的玩家以 Player 的快照 HP 为准。
		prev := st
		st = BaseState{Actor: cmd.actor, InWorld: true, X: cmd.pos.X, Y: cmd.pos.Y, HP: cmd.hp,
			SnapVer: prev.SnapVer, IntentSeq: prev.IntentSeq, AttackSeq: prev.AttackSeq, LostEpoch: prev.LostEpoch}
		if prev.SnapVer != (Version{}) {
			st.HP = prev.HP
		}
		st.Epoch = max(prev.Epoch, prev.SnapVer.Epoch)
		n.enterMsg(&st, n.cfg.Grid.SID(cell), max(st.Epoch, st.LostEpoch)+1, now, fx)
		if err := p.StartBase(); err != nil {
			return nil, err
		}
	case cmdMove:
		if !st.InWorld {
			return nil, ErrNotInWorld
		}
		st.IntentSeq++
		st.VX, st.VY = cmd.vx, cmd.vy
		if st.Cell != 0 {
			fx.send(st.Cell, n.inputMsg(&st, now))
		}
	case cmdAttack:
		if !st.InWorld || st.Cell == 0 {
			return nil, ErrNotInWorld
		}
		st.AttackSeq++
		hit := HitID{Attacker: st.Actor, Seq: st.AttackSeq}
		fx.send(st.Cell, &wireMsg{Kind: kindAttack, Attack: &attackReq{Hit: hit, Target: cmd.target, Amount: cmd.amount}})
		ret = hit
	case cmdLeave:
		if !st.InWorld {
			return nil, ErrNotInWorld
		}
		if st.Cell != 0 {
			fx.send(st.Cell, &wireMsg{Kind: kindLeave, Leave: &leaveReq{ID: st.Actor}})
		}
		n.unshow(&st, now)
		st.InWorld, st.Cell = false, 0
	}
	p.SetBaseState(st)
	n.registerCommit(fx)
	return ret, nil
}

func (n *Node) enterMsg(st *BaseState, sid int32, hint uint64, now time.Time, fx *txEffects) {
	st.LastEnterAt = now.UnixNano()
	fx.send(sid, &wireMsg{Kind: kindEnter, Enter: &enterReq{
		State: EntityState{ID: st.Actor, Kind: KindPlayer, X: st.X, Y: st.Y, HP: st.HP}, EpochHint: hint, LastInput: 0,
	}})
}

func (n *Node) inputMsg(st *BaseState, now time.Time) *wireMsg {
	st.LastLeaseAt = now.UnixNano()
	return &wireMsg{Kind: kindInput, Input: &inputReq{ID: st.Actor, Seq: st.IntentSeq, VX: st.VX, VY: st.VY}}
}

// ---- 入站：发给 Player 的消息 ----

func (n *Node) handleBase(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	p, err := playerOf(es)
	if err != nil {
		return nil, err
	}
	m := params[0].(*wireMsg)
	st := p.BaseState()
	now := time.Now()
	fx := &txEffects{}
	switch m.Kind {
	case kindOwner:
		if st.InWorld && m.Owner.ID == st.Actor {
			n.switchOwner(&st, m.From, m.Owner.Epoch, m.Owner.LastInput, now, fx)
		}
	case kindWitness:
		n.onWitness(&st, m.From, m.SentAt, m.Witness, now, fx)
	case kindAttackResult:
		r := m.AttackResult
		if hook := n.cfg.Hooks.AttackResult; hook != nil {
			result := AttackResult{Hit: r.Hit, Target: r.Target, Outcome: r.Outcome, Reason: r.Reason, Duplicate: r.Duplicate, HP: r.HP}
			fx.hooks = append(fx.hooks, func() { hook(result) })
		}
	case kindLost:
		s := m.Lost.State
		if st.InWorld && s.ID == st.Actor && s.Ver.Epoch > st.Epoch && s.Ver.Epoch > st.LostEpoch {
			n.stats.lost.Add(1)
			st.LostEpoch, st.LostAt = s.Ver.Epoch, now.UnixNano()
			st.X, st.Y, st.HP = s.X, s.Y, s.HP
		}
	case kindEnterRefused:
		if st.InWorld && m.EnterRefused.ID == st.Actor {
			n.stats.enterRefusals.Add(1)
			st.EnterHint = m.EnterRefused.Hint
		}
	case kindWriteback:
		s := m.Writeback.State
		if s.ID == st.Actor && s.Ver.Newer(st.SnapVer) {
			// 只接受更新的快照：同一版本重复到达、旧格迟到的回写都不再写。
			st.SnapVer = s.Ver
			st.HP = s.HP
			if st.InWorld {
				st.X, st.Y = s.X, s.Y
			}
			n.stats.writebacks.Add(1)
			if hook := n.cfg.Hooks.Writeback; hook != nil {
				fx.hooks = append(fx.hooks, func() { hook(s.ID, s) })
			}
		}
	}
	p.SetBaseState(st)
	n.registerCommit(fx)
	return nil, nil
}

// switchOwner 在看到更大的 epoch 时更新 owner；owner 已应用的输入落后于最新意图就重发。
func (n *Node) switchOwner(st *BaseState, sid int32, epoch, lastInput uint64, now time.Time, fx *txEffects) bool {
	if epoch <= st.Epoch {
		return false
	}
	st.Cell, st.Epoch = sid, epoch
	st.EnterHint = 0
	st.LastViewAt = now.UnixNano() // 新 owner 的第一份 witness 最多一拍后到；静默看门狗从这里重新计时
	n.stats.ownerChanges.Add(1)
	if st.IntentSeq > lastInput {
		n.stats.inputsResent.Add(1)
		fx.send(sid, n.inputMsg(st, now))
	}
	if hook := n.cfg.Hooks.OwnerChanged; hook != nil {
		actor := st.Actor
		fx.hooks = append(fx.hooks, func() { hook(actor, sid, epoch) })
	}
	return true
}

// onWitness 只接受 epoch 不回退、且同 epoch 来自当前 owner 的 witness；接受后把其中每个对象喂给 Sync mirror（按对象
// 版本取新的：交接时旧格、新格的 witness 交错到达也不会让对象 remove 再 create）。
func (n *Node) onWitness(st *BaseState, from int32, sentAt int64, w *witnessMsg, now time.Time, fx *txEffects) {
	if !st.InWorld || w.Observer != st.Actor || w.Epoch < st.Epoch || (w.Epoch == st.Epoch && from != st.Cell) {
		n.stats.viewsStale.Add(1)
		return
	}
	resent := n.switchOwner(st, from, w.Epoch, w.LastInput, now, fx)
	if !resent && st.IntentSeq > w.LastInput {
		n.stats.inputsResent.Add(1)
		fx.send(st.Cell, n.inputMsg(st, now))
	}
	st.X, st.Y = w.Self.X, w.Self.Y
	st.LastViewAt = now.UnixNano()
	n.stats.viewsAccepted.Add(1)
	n.feedMirrors(st, w, now)
	if hook := n.cfg.Hooks.Witness; hook != nil {
		view := View{Player: w.Observer, Cell: from, Epoch: w.Epoch, Tick: w.Tick, Self: w.Self, Entities: w.Entities,
			Truncated: w.Truncated, At: now, SentAt: time.Unix(0, sentAt)}
		fx.hooks = append(fx.hooks, func() { hook(view) })
	}
}

// feedMirrors 把 witness 里的对象交给 Sync（Base 进程的 Manager），第一次时让玩家以自己的 SceneActor mirror 进入
// Interest。mirror 不是事务状态（按版本幂等，没有回滚），所以在 handler 里直接调用；这里之后没有会失败的步骤。
func (n *Node) feedMirrors(st *BaseState, w *witnessMsg, now time.Time) {
	if n.cfg.Mirrors == nil {
		return
	}
	apply := func(s EntityState) {
		namespace, kind := n.cfg.MirrorKind(s)
		if _, err := n.cfg.Mirrors.Apply(policy.MirrorItem[EntityState]{
			ID: s.ID, Namespace: namespace, Kind: kind, Version: policy.MirrorVersion{Epoch: s.Ver.Epoch, Seq: s.Ver.Seq}, Content: s,
		}, now); err != nil {
			n.stats.mirrorErrors.Add(1)
		}
	}
	apply(w.Self)
	for _, s := range w.Entities {
		if s.ID != w.Self.ID {
			apply(s)
		}
	}
	if !st.Shown {
		if err := n.cfg.Interest.Enter(n.cfg.Session(st.Actor), st.Actor, w.Self.Pos(), n.cfg.View); err != nil {
			slog.Debug("cellworld: player not entered into interest", "actor", st.Actor, "err", err)
			return
		}
		st.Shown = true
	}
	// 过期清理按 Base 心跳节奏（同一进程里多个玩家共用一份 mirror，按时间戳限频，只有一个玩家真正去扫）。
	if last := n.lastExpire.Load(); now.UnixNano()-last >= int64(n.cfg.WatchPeriod) && n.lastExpire.CompareAndSwap(last, now.UnixNano()) {
		n.cfg.Mirrors.Expire(now)
	}
}

// unshow 让离开的玩家退出 Base 的 Interest，删掉自己的 mirror。
func (n *Node) unshow(st *BaseState, now time.Time) {
	if n.cfg.Mirrors == nil || !st.Shown {
		return
	}
	if err := n.cfg.Interest.Leave(n.cfg.Session(st.Actor), st.Actor); err != nil {
		slog.Debug("cellworld: player not left interest", "actor", st.Actor, "err", err)
	}
	if _, ver, ok := n.cfg.Mirrors.Get(st.Actor); ok {
		n.cfg.Mirrors.Remove(st.Actor, ver, now)
	}
	st.Shown = false
}

// BaseBeat 是 Player 的 Base 心跳（看门狗 + 续租）：初次 Enter 丢失则重发；交接放弃（Lost）后宽限期内没有更新的
// owner 就按最后位置重进；witness 静默超过 SilenceTimeout（owner 格崩溃或不可达）也重进——新格按 Player 快照重建
// SceneActor。重进的 epoch 提示比已知的大，目标格已有 Real 时幂等返回，有存活 Ghost 时拒绝并提示 owner。
// 有 owner 时每 LeasePeriod 重发一次最新意图，兼作 SceneActor 的租约续期。
func (n *Node) BaseBeat(p BasePlayer, now time.Time) error {
	st := p.BaseState()
	if !st.InWorld {
		return nil
	}
	nowNano := now.UnixNano()
	fx := &txEffects{}
	target := st.EnterHint
	if target == 0 {
		if cell, ok := n.cfg.Grid.CellAt(st.Pos()); ok {
			target = n.cfg.Grid.SID(cell)
		}
	}
	sinceEnter := nowNano - st.LastEnterAt
	switch {
	case target == 0:
	case st.EnterHint != 0 && sinceEnter >= int64(n.cfg.WatchPeriod):
		n.enterMsg(&st, target, max(st.Epoch, st.LostEpoch)+1, now, fx)
		st.EnterHint = 0
	case st.LostEpoch > st.Epoch && nowNano-st.LostAt >= int64(n.cfg.LostGrace) && sinceEnter >= int64(n.cfg.LostGrace):
		n.stats.reenters.Add(1)
		n.enterMsg(&st, target, st.LostEpoch+1, now, fx)
	case st.Epoch == 0 && sinceEnter >= int64(n.cfg.SilenceTimeout):
		n.enterMsg(&st, target, 1, now, fx)
	case st.Epoch > 0 && nowNano-st.LastViewAt >= int64(n.cfg.SilenceTimeout) && sinceEnter >= int64(n.cfg.SilenceTimeout):
		n.stats.reenters.Add(1)
		n.enterMsg(&st, target, max(st.Epoch, st.LostEpoch)+1, now, fx)
	case st.Cell != 0 && nowNano-st.LastLeaseAt >= int64(n.cfg.LeasePeriod):
		fx.send(st.Cell, n.inputMsg(&st, now))
	}
	p.SetBaseState(st)
	n.registerCommit(fx)
	return nil
}
