package cellworld

import (
	"context"
	"crypto/rand"
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
// 收下 SceneActor 的回写。路由字段在 Player 的 nopersist DAO 中；入场计数、面板、HP 与快照位置 / 版本必须持久化。
// 看门狗与续租是 Player 的心跳（BaseBeat），不另建带锁的玩家表。

// ErrNotInWorld 表示玩家没有进入分区 Scene（未 Enter 或已 Leave）。
var ErrNotInWorld = errors.New("cellworld: player is not in the partitioned scene")

// BaseState 是 Player 上的分区 Scene 路由状态。时间都是系统时间 UnixNano。
type BaseState struct {
	Recovery RecoveryState `msgpack:"rc"`
	// Actor 是玩家的 SceneActor ID（跨格不变）；InWorld 表示已 Enter、未 Leave。
	Actor   int64 `msgpack:"a"`
	InWorld bool  `msgpack:"w"`
	// Cell / Epoch：当前所知的 owner 格 sid 与 epoch（只前进）。
	Cell  int32  `msgpack:"c"`
	Epoch uint64 `msgpack:"e"`
	// X / Y / HP / SnapVer：Player 的最近快照（回写与 witness 里的自己），重建 SceneActor 用。SnapVer 是被接受的
	// 最新回写版本：只接受更新的（幂等）。game 把这几项映射到持久字段。
	// X、Y、VX、VY 各用自己的标签（RV-20261106-04：曾经 `X, Y` 共用 x、`VX, VY` 共用 v，MsgPack 编码丢了 X 与 VX）。
	X       int64   `msgpack:"x"`
	Y       int64   `msgpack:"y"`
	HP      int32   `msgpack:"hp"`
	SnapVer Version `msgpack:"sv"`
	// 移动意图（单调序号）与攻击序号。
	IntentSeq uint64 `msgpack:"is"`
	VX        int64  `msgpack:"vx"`
	VY        int64  `msgpack:"vy"`
	AttackSeq uint64 `msgpack:"as"`
	// Entry 是持久化的入场计数，每次 Enter 加一。接入方必须把它映射到持久 DAO 字段。
	// Enter 使用 strict 提交，编号落入持久 WAL 后才发消息；看门狗重试不换号，不依赖机器时钟。
	Entry uint64 `msgpack:"en"`
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
	// Panel 是 Base 从养成输入推出的最新面板（Ver 单调，只在数值变化时 +1）：Enter 带它建 SceneActor，变化时发给 owner。
	// game 把它映射到持久字段：序号跨 Base 重启单调，Real 才不会把重启后的新面板当成旧的。
	Panel Panel `msgpack:"pn"`
	// PanelAcked 是 Real 已应用的最高面板序号（面板结论、owner 通知、witness 里的自己、回写都会带来）；小于 Panel.Ver
	// 表示还有待发的面板。PanelSentAt 是上一次发送的时间（按 LeasePeriod 重发），PanelSince 是这一版开始等待确认的
	// 时间，超过 PanelDeadline 还没确认就报一次“结果未知”（PanelUnknown 记下已报过；可能已经应用，Base 照旧重发）。
	PanelAcked   uint64 `msgpack:"pa"`
	PanelSentAt  int64  `msgpack:"ps"`
	PanelSince   int64  `msgpack:"pi"`
	PanelUnknown bool   `msgpack:"pu"`
	// 队伍面板（group.go，不持久）：GroupSentAt 是上一次把自己的快照发给别的队员的 Base 的时间，GroupSyncAt 是上一次
	// 请求重读成员表的时间；GroupViewID / GroupViewVer 是组关系视野现在按哪一份成员表设的（与缓存的成员表不同就重设）。
	GroupSentAt  int64  `msgpack:"gs"`
	GroupSyncAt  int64  `msgpack:"gy"`
	GroupViewID  int64  `msgpack:"gi"`
	GroupViewVer uint64 `msgpack:"gw"`
}

// Pos 返回快照位置。
func (s BaseState) Pos() spatial.Point { return spatial.Point{X: s.X, Y: s.Y} }

// BasePlayer 是 game 的 Player 实体为 Base 侧提供的能力，方法只在实体自己的 Nest 锁内调用。SetBaseState 必须经 DAO
// setter 写（事务回滚还原）。StartBase 启动 Player 的 Base 心跳（Node.BaseBeat 作回调，周期 WatchPeriod）。
// CellPanel 是玩家的养成输入（等级、装备……）现在推出的面板数值（Ver 不用填，Node 编号）；没有面板来源时返回零值。
type BasePlayer interface {
	fentity.IThreadSafeEntity
	BaseState() BaseState
	SetBaseState(BaseState)
	StartBase() error
	CellPanel() Panel
	// 只在死亡结算 handler 中访问账本，常规心跳不读写整张回执表。
	CellDeathReceipt(target int64) uint64
	RecordCellDeathReceipt(target int64, life uint64, limit int) error
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

// AttackResult 是一次攻击的结论（见 DamageOutcome）。Damage 是真正结算的伤害值（攻击者面板的攻击，没有面板时取
// Attack 的缺省值）；只有 Applied 且不是重复时非零。
type AttackResult struct {
	Hit       HitID
	Target    int64
	Outcome   DamageOutcome
	Reason    RejectReason
	Duplicate bool
	HP        int32
	Damage    int32
}

// PanelResult 是 Base 得到的面板结论：Applied（Real 上的序号已到 Ver；Duplicate 表示这一版此前已应用过）或 Unknown
// （PanelDeadline 内没有确认：可能已经应用，Base 照旧以同一序号重发，直到确认）。HP / MaxHP 是 Real 应用后的值
// （Unknown 时为 0：不声称）。
type PanelResult struct {
	Actor     int64
	Ver       uint64
	Outcome   DamageOutcome
	Duplicate bool
	HP, MaxHP int32
}

// ---- 业务入口：每个都是 Player 上的一次 Nest 请求 ----

type baseCmdKind uint8

const (
	cmdEnter baseCmdKind = iota + 1
	cmdMove
	cmdAttack
	cmdLeave
	cmdState
	cmdPanel
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
	_, err := n.cfg.Nest.Request(ctx, hBaseEnter, playerEntity, nest.Params{baseCmd{kind: cmdEnter, actor: actor, pos: pos, hp: hp}})
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

// RefreshPanel 让 Base 重新读一次玩家的面板（BasePlayer.CellPanel）：数值变了就编下一个序号、发给 owner。用于不在
// Player 事务里的面板来源变化（例如配置重载）；在 Player 的 handler 里（升级、换装）用 UpdatePanel，同一笔事务提交。
func (n *Node) RefreshPanel(ctx context.Context, playerEntity int64) error {
	_, err := n.cfg.Nest.Request(ctx, hBaseCmd, playerEntity, nest.Params{baseCmd{kind: cmdPanel}})
	return err
}

// UpdatePanel 在调用方当前的 Player 事务里（Player 的 Nest 锁内，例如升级、换装的 handler）读玩家的面板：数值变了就把
// 新面板与序号写进 BaseState（经 SetBaseState：事务回滚即撤销），提交之后发给当前 owner 格；不在世界里时只记下，
// Enter 带它。不发同步请求、不等结论：结论由 Base 心跳、owner 通知、witness 收拢，丢了按 LeasePeriod 重发。
func (n *Node) UpdatePanel(p BasePlayer) error {
	if err := n.guardTransaction(); err != nil {
		return err
	}
	st := p.BaseState()
	fx := &txEffects{}
	n.panelChanged(&st, p.CellPanel(), time.Now(), fx)
	p.SetBaseState(st)
	n.registerCommit(fx)
	return nil
}

// panelChanged：面板数值变了就编新序号，玩家在世界里且知道 owner 时发出（提交之后）。
func (n *Node) panelChanged(st *BaseState, values Panel, now time.Time, fx *txEffects) {
	if n.takePanel(st, values, now) && st.InWorld && st.Cell != 0 {
		n.sendPanel(st, st.Cell, now, fx)
	}
}

// sendPanel 把待确认的面板发给 sid（当前 owner）。
func (n *Node) sendPanel(st *BaseState, sid int32, now time.Time, fx *txEffects) {
	st.PanelSentAt = now.UnixNano()
	n.stats.panelsSent.Add(1)
	fx.send(sid, &wireMsg{Kind: kindPanel, Panel: &panelReq{ID: st.Actor, Panel: st.Panel}})
}

// ackPanel 记下 Real 上已应用的面板序号（来自面板结论、owner 通知、witness 里的自己、回写；只前进）。
func ackPanel(st *BaseState, ver uint64) {
	if ver > st.PanelAcked {
		st.PanelAcked = ver
	}
}

// panelPending：有一版面板还没被 Real 确认。
func (st BaseState) panelPending() bool { return st.Panel.Ver > st.PanelAcked }

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
		if prev.Entry == ^uint64(0) {
			return nil, errors.New("cellworld: entry sequence exhausted")
		}
		st = BaseState{Actor: cmd.actor, InWorld: true, X: cmd.pos.X, Y: cmd.pos.Y, HP: cmd.hp,
			SnapVer: prev.SnapVer, IntentSeq: prev.IntentSeq, AttackSeq: prev.AttackSeq, LostEpoch: prev.LostEpoch, Panel: prev.Panel, PanelAcked: prev.PanelAcked,
			Entry: prev.Entry + 1, Recovery: prev.Recovery.At(now)}
		if prev.SnapVer != (Version{}) {
			st.HP = prev.HP
		}
		st.Epoch = max(prev.Epoch, prev.SnapVer.Epoch)
		// 面板取玩家现在的养成输入（数值变了才编新序号）；Enter 带它建 SceneActor，Real 上的序号经 owner 通知确认。
		n.takePanel(&st, p.CellPanel(), now)
		st.PanelSince = now.UnixNano()
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
		// Seq 用于展示，不承担跨重启唯一性；随机 ID 也覆盖回滚后重试和 Player 卸载重载。
		hit := HitID{Attacker: st.Actor, Seq: st.AttackSeq, ID: rand.Text()}
		fx.send(st.Cell, &wireMsg{Kind: kindAttack, Attack: &attackReq{Hit: hit, Entry: st.Entry, Target: cmd.target, Amount: cmd.amount}})
		ret = hit
	case cmdPanel:
		n.panelChanged(&st, p.CellPanel(), now, fx)
	case cmdLeave:
		if !st.InWorld {
			return nil, ErrNotInWorld
		}
		if st.Cell != 0 {
			fx.send(st.Cell, &wireMsg{Kind: kindLeave, Leave: &leaveReq{ID: st.Actor, Entry: st.Entry}})
		}
		leaving := st
		fx.hooks = append(fx.hooks, func() { n.unshow(&leaving, now) })
		st.Shown = false
		st.GroupViewID, st.GroupViewVer = 0, 0
		st.InWorld, st.Cell = false, 0
	}
	p.SetBaseState(st)
	n.registerCommit(fx)
	return ret, nil
}

func (n *Node) enterMsg(st *BaseState, sid int32, hint uint64, now time.Time, fx *txEffects) {
	st.LastEnterAt = now.UnixNano()
	fx.send(sid, &wireMsg{Kind: kindEnter, Enter: &enterReq{
		Recovery: st.Recovery,
		State:    EntityState{ID: st.Actor, Kind: KindPlayer, X: st.X, Y: st.Y, HP: st.HP, Panel: st.Panel}, EpochHint: hint, LastInput: 0,
		Entry: st.Entry,
	}})
}

// takePanel 把养成输入推出的面板数值记进 st：与已记的数值相同时什么都不做（序号不动），否则编下一个序号、重新开始
// 等待确认。返回是否变了。
func (n *Node) takePanel(st *BaseState, values Panel, now time.Time) bool {
	if values.sameValues(st.Panel) {
		return false
	}
	values.Ver = st.Panel.Ver + 1
	st.Panel = values
	st.PanelSince, st.PanelUnknown = now.UnixNano(), false
	return true
}

func (n *Node) inputMsg(st *BaseState, now time.Time) *wireMsg {
	st.LastLeaseAt = now.UnixNano()
	return &wireMsg{Kind: kindInput, Input: &inputReq{ID: st.Actor, Entry: st.Entry, Seq: st.IntentSeq, VX: st.VX, VY: st.VY}}
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
			if m.Owner.Epoch >= st.Epoch {
				ackPanel(&st, m.Owner.PanelVer)
			}
			n.switchOwner(&st, m.From, m.Owner.Epoch, m.Owner.LastInput, now, fx)
		}
	case kindWitness:
		n.onWitness(&st, m.From, m.SentAt, m.Witness, now, fx)
	case kindAttackResult:
		r := m.AttackResult
		if hook := n.cfg.Hooks.AttackResult; hook != nil {
			result := AttackResult{Hit: r.Hit, Target: r.Target, Outcome: r.Outcome, Reason: r.Reason, Duplicate: r.Duplicate, HP: r.HP, Damage: r.Damage}
			fx.hooks = append(fx.hooks, func() { hook(result) })
		}
	case kindPanelResult:
		r := m.PanelResult
		if st.InWorld && r.ID == st.Actor && r.Outcome == OutcomeApplied {
			ackPanel(&st, r.Ver)
			if hook := n.cfg.Hooks.PanelResult; hook != nil {
				result := PanelResult{Actor: r.ID, Ver: r.Ver, Outcome: r.Outcome, Duplicate: r.Duplicate, HP: r.HP, MaxHP: r.MaxHP}
				fx.hooks = append(fx.hooks, func() { hook(result) })
			}
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
	case kindGroupNotice:
		if gp, ok := p.(GroupPlayer); ok {
			n.onGroupNotice(gp, &st, m.GroupNotice.Group)
		}
	case kindWriteback:
		s := m.Writeback.State
		if s.ID == st.Actor && s.Ver.Newer(st.SnapVer) {
			// 只接受更新的快照：同一版本重复到达、旧格迟到的回写都不再写。
			if err := m.Writeback.Recovery.Validate(); err != nil {
				return nil, err
			}
			st.Recovery = m.Writeback.Recovery.Clone().At(now)
			st.SnapVer = s.Ver
			st.HP = s.HP
			if st.InWorld {
				st.X, st.Y = s.X, s.Y
			}
			// 面板的结论随快照回来：Real 上已应用的序号（game 把它与快照 HP 一起映射到持久字段）。
			ackPanel(&st, s.Panel.Ver)
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
	if st.panelPending() {
		// 待确认的面板可能发给了旧 owner（交接中答“重试”）或丢了：新 owner 立即补发（Real 按序号幂等）。
		n.sendPanel(st, sid, now, fx)
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
	ackPanel(st, w.Self.Panel.Ver) // witness 里的自己带着 Real 上的面板：结论回复丢了也能确认
	resent := n.switchOwner(st, from, w.Epoch, w.LastInput, now, fx)
	if !resent && st.IntentSeq > w.LastInput {
		n.stats.inputsResent.Add(1)
		fx.send(st.Cell, n.inputMsg(st, now))
	}
	st.X, st.Y = w.Self.X, w.Self.Y
	st.LastViewAt = now.UnixNano()
	n.stats.viewsAccepted.Add(1)
	// Mirror / Interest 不参与 DAO 回滚，只应用已经提交的视野副本。
	if n.cfg.Mirrors != nil {
		st.Shown = true
		snapshot := *st
		fx.hooks = append(fx.hooks, func() { n.feedMirrors(&snapshot, w, now) })
	}
	if hook := n.cfg.Hooks.Witness; hook != nil {
		view := View{Player: w.Observer, Cell: from, Epoch: w.Epoch, Tick: w.Tick, Self: w.Self, Entities: w.Entities,
			Truncated: w.Truncated, At: now, SentAt: time.Unix(0, sentAt)}
		fx.hooks = append(fx.hooks, func() { hook(view) })
	}
}

// feedMirrors 把 witness 里的对象交给 Sync（Base 进程的 Manager），第一次时让玩家以自己的 SceneActor mirror 进入
// Interest。mirror 不是事务状态（按版本幂等，没有回滚），只在提交后调用。登记失败由下一份视野重试。
func (n *Node) feedMirrors(st *BaseState, w *witnessMsg, now time.Time) {
	if n.cfg.Mirrors == nil {
		return
	}
	apply := func(s EntityState) {
		version := policy.MirrorVersion{Epoch: s.Ver.Epoch, Seq: s.Ver.Seq}
		// 视野 Tick 已在接收侧去重；同一内容的当前可见事实只续活，不标脏。
		if n.cfg.Mirrors.Touch(s.ID, version, now) {
			return
		}
		namespace, kind := n.cfg.MirrorKind(s)
		if _, err := n.cfg.Mirrors.Apply(policy.MirrorItem[EntityState]{
			ID: s.ID, Namespace: namespace, Kind: kind, Version: version, Content: s,
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
	if _, exists := n.cfg.Interest.ObserverView(n.cfg.Session(st.Actor)); !exists {
		if err := n.cfg.Interest.Enter(n.cfg.Session(st.Actor), st.Actor, w.Self.Pos(), n.cfg.View); err != nil {
			slog.Debug("cellworld: player not entered into interest", "actor", st.Actor, "err", err)
			return
		}
		st.Shown = true
	}
	n.expireMirrors(now)
}

// expireMirrors 按 Base 心跳节奏清理过期的 mirror（同一进程里多个玩家共用一份 mirror，按时间戳限频，只有一个玩家真正
// 去扫）。witness 与队伍面板的心跳都调它：本进程的玩家都收不到 witness 时（例如 owner 格崩溃），收到的队友快照照样过期。
func (n *Node) expireMirrors(now time.Time) {
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
	if st.GroupViewID != 0 {
		n.removeGroupView(n.cfg.PlayerEntity(st.Actor))
		st.GroupViewID, st.GroupViewVer = 0, 0
	}
}

// BaseBeat 是 Player 的 Base 心跳（看门狗 + 续租）：初次 Enter 丢失则重发；交接放弃（Lost）后宽限期内没有更新的
// owner 就按最后位置重进；witness 静默超过 SilenceTimeout（owner 格崩溃或不可达）也重进——新格按 Player 快照重建
// SceneActor。重进的 epoch 提示比已知的大，目标格已有 Real 时幂等返回，有存活 Ghost 时拒绝并提示 owner。
// 有 owner 时每 LeasePeriod 重发一次最新意图，兼作 SceneActor 的租约续期。
func (n *Node) BaseBeat(p BasePlayer, now time.Time) error {
	if err := n.guardTransaction(); err != nil {
		return err
	}
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
	// 待确认的面板：按 LeasePeriod 重发给当前 owner；超过 PanelDeadline 还没确认就报一次“结果未知”（可能已应用，
	// 照旧重发，直到 owner 通知 / witness / 回写 / 结论带回它的序号）。
	if st.Cell != 0 && st.panelPending() {
		if nowNano-st.PanelSentAt >= int64(n.cfg.LeasePeriod) {
			n.sendPanel(&st, st.Cell, now, fx)
		}
		if !st.PanelUnknown && nowNano-st.PanelSince >= int64(n.cfg.PanelDeadline) {
			st.PanelUnknown = true
			n.stats.panelUnknown.Add(1)
			if hook := n.cfg.Hooks.PanelResult; hook != nil {
				result := PanelResult{Actor: st.Actor, Ver: st.Panel.Ver, Outcome: OutcomeUnknown}
				fx.hooks = append(fx.hooks, func() { hook(result) })
			}
		}
	}
	if gp, ok := p.(GroupPlayer); ok && n.cfg.GroupView != nil {
		n.groupBeat(gp, &st, now, fx)
	}
	p.SetBaseState(st)
	n.registerCommit(fx)
	return nil
}
