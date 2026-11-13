package cellworld

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 线上格式：每个进程（Cell 或 Base）一个收件主题，消息是 msgpack 编码的 wireMsg，Kind 决定哪个字段有值。
// wireMsg.V 是格式版本（WireVersion）：不是当前版本的消息按坏消息丢弃（未上线，不做跨版本兼容）。字段用短键控制体积。

// WireVersion 是当前线上格式版本。v2：加版本号、witness 带每个对象的版本、写回消息。v3：EntityState 带心跳档位（Idle，D1）。
// v4：EntityState 带战斗面板（Panel）；面板命令与结论（kindPanel / kindPanelResult）；伤害结论带实际伤害值；owner 通知带
// Real 上的面板序号。v5：队伍面板的队员快照与成员表通知（kindGroupSnap / kindGroupNotice，group.go）。未上线，旧版本的
// 消息按坏消息丢弃（升级须先停旧进程）。v6：入场序号（Entry，RV-20261106-03）随 Enter、Leave 与交接 Offer 传递，Leave
// 只结束同一次入场的角色。v7：扩展消息（kindExt：按命名空间交给 NodeConfig.Ext 的处理器，内容由处理器自己编码；
// 第一个用户是可编辑地形的边界块副本，gameplay/terrain 的 Replicator）。v8：交接 Offer 带位置不足 1 单位的零头
// （FracX / FracY，千分位，积分按千分位整数累计）。
// v9：持久入场序号、输入携带入场身份、攻击随机 ID、交接去重记录使用剩余时长。
// v10：全局授权、进程 incarnation 与不可重用的交接身份。
// v11：内部状态差值、Witness 成员增删、按 Base 合批及基线重置。
// v12：基线重置只列出解不出的对象与观察者（replicaReset），不再整个 peer 退回全量。
const WireVersion uint8 = 12

// EntityKind 区分玩家（有 BaseApp，接收视野）与 NPC（只在 Cell 里模拟）。
type EntityKind uint8

const (
	KindPlayer EntityKind = 1
	KindNPC    EntityKind = 2
)

// EntityState 是空间代理的完整状态：Ghost 更新、交接与视野都传它。非持久；Cell 崩溃即丢失。
type EntityState struct {
	// Life 在首次取得写权时分配，普通交接和崩溃恢复不变；用于跨重生结算去重。
	Life uint64     `msgpack:"lf"`
	ID   int64      `msgpack:"i"`
	Kind EntityKind `msgpack:"k"`
	// Base 是玩家所属 BaseApp 的 sid（NPC 为 0），Cell 把视野与 owner 变化发给它。
	Base int32 `msgpack:"b,omitempty"`
	X    int64 `msgpack:"x"`
	Y    int64 `msgpack:"y"`
	// VX、VY 是速度，单位 / 秒。
	VX  int64   `msgpack:"vx,omitempty"`
	VY  int64   `msgpack:"vy,omitempty"`
	HP  int32   `msgpack:"hp"`
	Ver Version `msgpack:"v"`
	// Idle：NPC 在慢档（附近没有玩家，心跳降到 NodeConfig.IdlePeriod，D1）。随 Ghost 与交接传出：邻格按它放宽 Ghost 的
	// 过期时间（慢档的 Ghost 每个慢拍才更新一次），新格按它以慢档装入。玩家恒为 false。
	Idle bool `msgpack:"il,omitempty"`
	// Panel 是战斗面板（最大 HP、攻击、战力）。权威在 Real：随 Ghost、交接、witness、回写传出，交接时整份带走。
	Panel Panel `msgpack:"pn,omitempty"`
}

// Panel 是角色的战斗面板（维护者 2026-11-04 第 3 项 = A）：最大 HP、攻击、战力，将来的 buff 层也归它。权威在角色所在格的
// Real；养成输入（等级、经验、装备）留在 Base 的 Player 上，Base 从养成输入推出面板、带单调序号 Ver 发给 Real
// （kindPanel，与攻击同一套转发、去重、结果未知语义），Real 只接受序号更大的（幂等：重复、迟到、乱序都只应用一次），
// 结果随 witness 与回写回到 Base。
//
// 零值字段表示“面板没有给出”：Attack 为 0 时伤害取请求里的缺省值（NodeConfig 之外由调用方给的固定伤害），MaxHP 为 0
// 时当前 HP 不设上限（NPC、没有面板来源的玩家）。面板变化只改上限、不回血：上限变小时当前 HP 夹到新上限，变大时当前 HP
// 不变（FEATURE §12.2 的暂定，可改）。
type Panel struct {
	MaxHP  int32  `msgpack:"mh,omitempty"`
	Attack int32  `msgpack:"at,omitempty"`
	Power  int32  `msgpack:"pw,omitempty"`
	Ver    uint64 `msgpack:"v,omitempty"`
}

// sameValues 比较面板的数值（不比较序号）。
func (p Panel) sameValues(q Panel) bool {
	return p.MaxHP == q.MaxHP && p.Attack == q.Attack && p.Power == q.Power
}

// capHP 把当前 HP 夹到面板上限（MaxHP 为 0 表示没有上限）。只降不升：面板变化不回血。
func (s *EntityState) capHP() {
	if s.Panel.MaxHP > 0 && s.HP > s.Panel.MaxHP {
		s.HP = s.Panel.MaxHP
	}
}

// Pos 返回位置。
func (s EntityState) Pos() spatial.Point { return spatial.Point{X: s.X, Y: s.Y} }

// HitID 唯一标识一次伤害。ID 是随机令牌，Seq 只用于展示；不能用可能在重启后归零的 Seq 去重。
type HitID struct {
	Attacker int64  `msgpack:"a"`
	Seq      uint64 `msgpack:"s"`
	ID       string `msgpack:"i"`
}

// AppliedHit 是目标 Real 上已结算伤害的去重记录，随交接带走（存在 SceneActor / Monster 的 DAO 里）。
type AppliedHit struct {
	Hit HitID `msgpack:"h"`
	At  int64 `msgpack:"t"` // 结算时的系统时间（UnixNano），用于去重窗口过期
}

// transferredHit 只传剩余有效期；接收方在自己的时钟上重建 At，不比较跨机 UnixNano。
// 网络耗时不扣除，因此保守延长去重窗口；MaxAppliedHits 仍限制内存占用。
type transferredHit struct {
	Hit       HitID `msgpack:"h"`
	Remaining int64 `msgpack:"r"`
}

// DamageOutcome 是一次伤害请求的结论。
type DamageOutcome uint8

const (
	// OutcomeApplied：已结算（含“此前已结算过”的重复请求，此时 Duplicate 为 true）。
	OutcomeApplied DamageOutcome = 1
	// OutcomeRejected：明确没有结算，Reason 说明原因。
	OutcomeRejected DamageOutcome = 2
	// OutcomeRetry：只在 Cell 之间出现：收到的 Cell 不是目标的 Real，Hint 给出它所知的 owner。
	OutcomeRetry DamageOutcome = 3
	// OutcomeUnknown：攻击者 Cell 在期限内没拿到结论；可能已经结算，不能当成没结算。
	OutcomeUnknown DamageOutcome = 4
)

func (o DamageOutcome) String() string {
	switch o {
	case OutcomeApplied:
		return "applied"
	case OutcomeRejected:
		return "rejected"
	case OutcomeRetry:
		return "retry"
	case OutcomeUnknown:
		return "unknown"
	}
	return "outcome(" + strconv.Itoa(int(o)) + ")"
}

// RejectReason 说明 OutcomeRejected 的原因。
type RejectReason uint8

const (
	ReasonNone       RejectReason = 0
	ReasonNotOwner   RejectReason = 1 // 攻击者不是收到请求的 Cell 的 Real
	ReasonNotVisible RejectReason = 2 // 目标不在攻击者 Cell 的视野里
	ReasonOutOfRange RejectReason = 3 // 超出攻击距离
	ReasonDead       RejectReason = 4 // 目标 HP 已为 0
	ReasonBusy       RejectReason = 5 // 目标的去重表在窗口内已满，拒绝新伤害以免重复结算
)

type msgKind uint8

const (
	kindGhost msgKind = iota + 1
	kindOffer
	kindOfferReply
	kindDamage
	kindDamageReply
	kindEnter
	kindLeave
	kindInput
	kindAttack
	kindOwner
	kindWitness
	kindAttackResult
	kindLost
	kindEnterRefused
	kindWriteback
	kindPanel
	kindPanelResult
	kindGroupSnap
	kindGroupNotice
	kindExt
	kindReplicaReset
	kindDeath
	kindDeathAck
)

// ghostBatch：Real 的 owner 每拍发给一个邻格的 Ghost 更新（完整状态）与删除。
type ghostBatch struct {
	Updates []EntityState `msgpack:"u,omitempty"`
	Removes []ghostRemove `msgpack:"r,omitempty"`
}

type ghostRemove struct {
	ID  int64   `msgpack:"i"`
	Ver Version `msgpack:"v"`
}

// handoffOffer：旧格把 Real 交给新格。State.Ver.Epoch 是新 epoch；重传发同一份内容。Combat 是角色的其余战斗字段
// （去重表、已应用输入序号），整份随交接带走。
type handoffOffer struct {
	SpawnGeneration uint64           `msgpack:"sg,omitempty"`
	Recovery        RecoveryState    `msgpack:"rc"`
	Grant           OwnerGrant       `msgpack:"og"`
	Transfer        OwnerTransfer    `msgpack:"ot"`
	State           EntityState      `msgpack:"s"`
	From            CellID           `msgpack:"f"`
	LastInput       uint64           `msgpack:"in,omitempty"`
	Applied         []transferredHit `msgpack:"a,omitempty"`
	// Entry 是玩家这次入场的序号（ActorState.Entry），随交接带走：新格的 Real 才认得同一次入场的 Leave。NPC 为 0。
	Entry uint64 `msgpack:"en,omitempty"`
	// FracX / FracY 是位置不足 1 单位的零头（ActorState.FracX / FracY，千分位），随交接带走：目标格接着累加，跨格不丢位移。
	FracX int32 `msgpack:"fx,omitempty"`
	FracY int32 `msgpack:"fy,omitempty"`
}

type handoffReply struct {
	ID       int64  `msgpack:"i"`
	Epoch    uint64 `msgpack:"e"`
	Accepted bool   `msgpack:"ok"`
}

type damageReq struct {
	RewardBase int32  `msgpack:"b"`
	Life       uint64 `msgpack:"lf"`
	Hit        HitID  `msgpack:"h"`
	Target     int64  `msgpack:"t"`
	Amount     int32  `msgpack:"n"`
}

type damageReply struct {
	Hit       HitID         `msgpack:"h"`
	Target    int64         `msgpack:"t"`
	Outcome   DamageOutcome `msgpack:"o"`
	Reason    RejectReason  `msgpack:"r,omitempty"`
	Duplicate bool          `msgpack:"d,omitempty"`
	HP        int32         `msgpack:"hp"`
	Hint      int32         `msgpack:"hint,omitempty"` // OutcomeRetry 时所知 owner 的 sid，0 表示不知道
	// Damage 是这次真正结算的伤害值（攻击者面板的攻击，没有面板时取请求的缺省值）；重复、拒绝、重试时为 0。
	Damage int32 `msgpack:"dmg,omitempty"`
}

// enterReq：Base 让玩家进入（首次、Cell 崩溃后重进、交接放弃后补回）。State 是 Player 的最近快照（位置、HP），
// 新格按它建 SceneActor。
type enterReq struct {
	Recovery  RecoveryState `msgpack:"rc"`
	State     EntityState   `msgpack:"s"`
	EpochHint uint64        `msgpack:"e"`
	LastInput uint64        `msgpack:"in,omitempty"`
	// Entry 是 Base 给这次入场编的序号（BaseState.Entry）：看门狗重进、交接放弃后补回都带同一个序号，再次 Enter 才换新的。
	Entry uint64 `msgpack:"en"`
}

// leaveReq：Base 让玩家离场。Entry 是要结束的那一次入场（RV-20261106-03）：传输允许重复、乱序，上一次入场的 Leave
// 可能在玩家以同 ID 重新入场之后才到；只有 ID 时它会删掉新入场的角色。Real 只接受 Entry 与自己相同的 Leave。
type leaveReq struct {
	ID    int64  `msgpack:"i"`
	Entry uint64 `msgpack:"en"`
}

// inputReq 是移动意图，同时是 Base 对 SceneActor 的租约续期：Base 每 LeasePeriod 重发一次最新意图（序号不变），
// SceneActor 收到任何一条都续租，只有序号前进的才改速度。
type inputReq struct {
	ID    int64  `msgpack:"i"`
	Entry uint64 `msgpack:"en"`
	Seq   uint64 `msgpack:"q"`
	VX    int64  `msgpack:"vx"`
	VY    int64  `msgpack:"vy"`
}

type attackReq struct {
	Hit    HitID  `msgpack:"h"`
	Entry  uint64 `msgpack:"en"`
	Target int64  `msgpack:"t"`
	Amount int32  `msgpack:"n"`
}

type ownerNotice struct {
	ID        int64  `msgpack:"i"`
	Epoch     uint64 `msgpack:"e"`
	LastInput uint64 `msgpack:"in,omitempty"`
	// PanelVer 是 Real 上面板的序号：Base 据此判断待发的面板是否已经到了（与 LastInput 同一种用法）。
	PanelVer uint64 `msgpack:"pv,omitempty"`
}

// witnessMsg 是 Cell 给玩家 Base 的视野：半径 R 内的本格 Real / Frozen 与邻格 Ghost（含自己）。每个对象带它在 Cell
// 上的版本 (epoch, seq)，Base 据此喂 Sync mirror（policy.Mirrors）：交接时旧格、新格的 witness 交错到达，按对象版本取新的。
type witnessMsg struct {
	FullMembership bool          `msgpack:"fm,omitempty"`
	BaseTick       uint64        `msgpack:"bt,omitempty"`
	Added          []int64       `msgpack:"add,omitempty"`
	Removed        []int64       `msgpack:"rm,omitempty"`
	Observer       int64         `msgpack:"o"`
	Epoch          uint64        `msgpack:"e"`
	Tick           uint64        `msgpack:"t"`
	LastInput      uint64        `msgpack:"in,omitempty"`
	Self           EntityState   `msgpack:"s"`
	Entities       []EntityState `msgpack:"v,omitempty"`
	Truncated      bool          `msgpack:"tr,omitempty"`
}

// writebackMsg 是 SceneActor 回写 Player 的快照（HP、位置与面板）。Ver 是 SceneActor 当时的版本，
// 跨交接、跨重建单调；Base 只接受比 Player 上已存版本新的快照（幂等）。
type writebackMsg struct {
	Recovery RecoveryState `msgpack:"rc"`
	State    EntityState   `msgpack:"s"`
}

type attackResultMsg struct {
	Hit       HitID         `msgpack:"h"`
	Target    int64         `msgpack:"t"`
	Outcome   DamageOutcome `msgpack:"o"`
	Reason    RejectReason  `msgpack:"r,omitempty"`
	Duplicate bool          `msgpack:"d,omitempty"`
	HP        int32         `msgpack:"hp"`
	Damage    int32         `msgpack:"dmg,omitempty"`
}

// panelReq 是 Base 发给玩家当前 owner 格的面板变化（只带最新的一份：面板是状态，不是事件，Real 按 Ver 取新的）。
type panelReq struct {
	ID    int64 `msgpack:"i"`
	Panel Panel `msgpack:"p"`
}

// panelResult 是 Real 对面板命令的结论：Applied（Duplicate 表示这个序号或更新的已经应用过）带 Real 上现在的序号、HP 与
// 上限；收到的格不是 Real 时是 Retry（Hint 是所知 owner 的 sid），Base 等 owner 变化或重发间隔到了再发。
type panelResult struct {
	ID        int64         `msgpack:"i"`
	Ver       uint64        `msgpack:"v"`
	Outcome   DamageOutcome `msgpack:"o"`
	Duplicate bool          `msgpack:"d,omitempty"`
	HP        int32         `msgpack:"hp"`
	MaxHP     int32         `msgpack:"mh,omitempty"`
	Hint      int32         `msgpack:"hint,omitempty"`
}

// groupSnap 是 Base 发给别的队员的 Base 的一份快照（队伍面板，group.go）：本进程这名队员的 SceneActor 状态，取自本进程
// 自己的 mirror（最新一份 witness 里的自己，带 Real 上的版本），收件方按版本喂 mirror。Group / Ver 是发送方缓存的成员表。
type groupSnap struct {
	Group int64       `msgpack:"g"`
	Ver   uint64      `msgpack:"gv"`
	State EntityState `msgpack:"s"`
}

// groupNotice 是成员表变化的通知：发给变化前后全部成员所在的 Base，To 是收件 Base 上要更新的玩家（Player 实体 ID）。
type groupNotice struct {
	Group Group   `msgpack:"g"`
	To    []int64 `msgpack:"to"`
}

// extMsg 是扩展消息（v7）：Space 选处理器（NodeConfig.Ext 的键），Body 是处理器自己的编码，cellworld 不解释。
type extMsg struct {
	Space string `msgpack:"sp"`
	Body  []byte `msgpack:"b"`
}

type lostNotice struct {
	State EntityState `msgpack:"s"`
}

type enterRefused struct {
	ID   int64 `msgpack:"i"`
	Hint int32 `msgpack:"h,omitempty"`
}

// wireMsg 是唯一的线上外壳。From 是发送进程的 sid；SentAt 是发送方系统时间（UnixNano），只用于测量。
type wireMsg struct {
	Death     *DeathFact    `msgpack:"df,omitempty"`
	DeathAck  *deathAck     `msgpack:"da,omitempty"`
	Replica   bool          `msgpack:"rp,omitempty"`
	Deltas    []stateDelta  `msgpack:"ds,omitempty"`
	Witnesses []*witnessMsg `msgpack:"vs,omitempty"`
	V         uint8         `msgpack:"ver"`
	Kind      msgKind       `msgpack:"k"`
	From      int32         `msgpack:"f"`
	SentAt    int64         `msgpack:"t,omitempty"`

	Ghost        *ghostBatch      `msgpack:"g,omitempty"`
	Offer        *handoffOffer    `msgpack:"of,omitempty"`
	OfferReply   *handoffReply    `msgpack:"or,omitempty"`
	Damage       *damageReq       `msgpack:"dm,omitempty"`
	DamageReply  *damageReply     `msgpack:"dr,omitempty"`
	Enter        *enterReq        `msgpack:"en,omitempty"`
	Leave        *leaveReq        `msgpack:"lv,omitempty"`
	Input        *inputReq        `msgpack:"in,omitempty"`
	Attack       *attackReq       `msgpack:"at,omitempty"`
	Owner        *ownerNotice     `msgpack:"ow,omitempty"`
	Witness      *witnessMsg      `msgpack:"vw,omitempty"`
	AttackResult *attackResultMsg `msgpack:"ar,omitempty"`
	Lost         *lostNotice      `msgpack:"ls,omitempty"`
	EnterRefused *enterRefused    `msgpack:"rf,omitempty"`
	Writeback    *writebackMsg    `msgpack:"wb,omitempty"`
	Panel        *panelReq        `msgpack:"pn,omitempty"`
	PanelResult  *panelResult     `msgpack:"pr,omitempty"`
	GroupSnap    *groupSnap       `msgpack:"gs,omitempty"`
	GroupNotice  *groupNotice     `msgpack:"gn,omitempty"`
	Ext          *extMsg          `msgpack:"ex,omitempty"`
	ReplicaReset *replicaReset    `msgpack:"rr,omitempty"`
}

// replicaReset 是接收方请求发送方忘掉的那部分基线：States 是 Kind 消息里解不出（增量对不上或缺基线）的对象，
// Views 是成员表基线对不上的观察者。发送方删掉这些基线，下一次引用时发全量（观察者发全量成员表，连带成员状态全量）。
// 为 nil 时忘掉这个 peer 的全部基线。
type replicaReset struct {
	Kind   msgKind `msgpack:"k"`
	States []int64 `msgpack:"s,omitempty"`
	Views  []int64 `msgpack:"v,omitempty"`
}

var errBadMessage = errors.New("cellworld: malformed message")

func encodeMsg(m *wireMsg) ([]byte, error) {
	m.V = WireVersion
	return msgpack.Marshal(m)
}

// decodeMsg 解码并检查 Kind 与对应字段一致；不一致按坏消息丢弃，不进入状态机。
func decodeMsg(data []byte) (*wireMsg, error) {
	var m wireMsg
	if err := msgpack.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadMessage, err)
	}
	if m.V != WireVersion {
		return nil, fmt.Errorf("%w: wire version %d, want %d", errBadMessage, m.V, WireVersion)
	}
	var present bool
	switch m.Kind {
	case kindReplicaReset:
		present = true
	case kindDeath:
		present = m.Death != nil
	case kindDeathAck:
		present = m.DeathAck != nil
	case kindGhost:
		present = m.Ghost != nil
	case kindOffer:
		present = m.Offer != nil
	case kindOfferReply:
		present = m.OfferReply != nil
	case kindDamage:
		present = m.Damage != nil
	case kindDamageReply:
		present = m.DamageReply != nil
	case kindEnter:
		present = m.Enter != nil
	case kindLeave:
		present = m.Leave != nil
	case kindInput:
		present = m.Input != nil
	case kindAttack:
		present = m.Attack != nil
	case kindOwner:
		present = m.Owner != nil
	case kindWitness:
		present = m.Witness != nil
	case kindAttackResult:
		present = m.AttackResult != nil
	case kindLost:
		present = m.Lost != nil
	case kindEnterRefused:
		present = m.EnterRefused != nil
	case kindWriteback:
		present = m.Writeback != nil
	case kindPanel:
		present = m.Panel != nil
	case kindPanelResult:
		present = m.PanelResult != nil
	case kindGroupSnap:
		present = m.GroupSnap != nil
	case kindGroupNotice:
		present = m.GroupNotice != nil
	case kindExt:
		present = m.Ext != nil
	}
	if !present {
		return nil, fmt.Errorf("%w: kind %d without body", errBadMessage, m.Kind)
	}
	return &m, nil
}

// Subject 返回 sid 的收件主题。world 只允许字母、数字、'-' 与 '_'（由 validWorld 检查）。
func Subject(world string, sid int32) string {
	return "roost.cellworld." + world + "." + strconv.FormatInt(int64(sid), 10)
}

func validWorld(world string) bool {
	if world == "" || len(world) > 64 {
		return false
	}
	for _, r := range world {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
