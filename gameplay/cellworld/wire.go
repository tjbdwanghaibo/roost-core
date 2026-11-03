package cellworld

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 线上格式：每个进程（Cell 或 Base）一个收件主题，消息是 msgpack 编码的 wireMsg，Kind 决定哪个字段有值。
// 原型阶段没有格式版本号与跨版本兼容（未上线，见 §5）；字段用短键控制体积。

// EntityKind 区分玩家（有 BaseApp，接收视野）与 NPC（只在 Cell 里模拟）。
type EntityKind uint8

const (
	KindPlayer EntityKind = 1
	KindNPC    EntityKind = 2
)

// EntityState 是空间代理的完整状态：Ghost 更新、交接与视野都传它。非持久；Cell 崩溃即丢失。
type EntityState struct {
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
}

// Pos 返回位置。
func (s EntityState) Pos() spatial.Point { return spatial.Point{X: s.X, Y: s.Y} }

// HitID 唯一标识一次伤害：攻击者 ID + 攻击者在 Base 上的单调攻击序号。结算方按它去重。
type HitID struct {
	Attacker int64  `msgpack:"a"`
	Seq      uint64 `msgpack:"s"`
}

// appliedHit 是目标 Real 上已结算伤害的去重记录，随交接带走。
type appliedHit struct {
	Hit HitID `msgpack:"h"`
	At  int64 `msgpack:"t"` // 结算时的系统时间（UnixNano），用于去重窗口过期
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
	kindView
	kindAttackResult
	kindLost
	kindEnterRefused
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

// handoffOffer：旧格把 Real 交给新格。State.Ver.Epoch 是新 epoch；重传发同一份内容。
type handoffOffer struct {
	State     EntityState  `msgpack:"s"`
	From      CellID       `msgpack:"f"`
	LastInput uint64       `msgpack:"in,omitempty"`
	GhostedTo []CellID     `msgpack:"g,omitempty"`
	Applied   []appliedHit `msgpack:"a,omitempty"`
}

type handoffReply struct {
	ID       int64  `msgpack:"i"`
	Epoch    uint64 `msgpack:"e"`
	Accepted bool   `msgpack:"ok"`
}

type damageReq struct {
	Hit    HitID `msgpack:"h"`
	Target int64 `msgpack:"t"`
	Amount int32 `msgpack:"n"`
}

type damageReply struct {
	Hit       HitID         `msgpack:"h"`
	Target    int64         `msgpack:"t"`
	Outcome   DamageOutcome `msgpack:"o"`
	Reason    RejectReason  `msgpack:"r,omitempty"`
	Duplicate bool          `msgpack:"d,omitempty"`
	HP        int32         `msgpack:"hp"`
	Hint      int32         `msgpack:"hint,omitempty"` // OutcomeRetry 时所知 owner 的 sid，0 表示不知道
}

type enterReq struct {
	State     EntityState `msgpack:"s"`
	EpochHint uint64      `msgpack:"e"`
	LastInput uint64      `msgpack:"in,omitempty"`
}

type leaveReq struct {
	ID int64 `msgpack:"i"`
}

type inputReq struct {
	ID  int64  `msgpack:"i"`
	Seq uint64 `msgpack:"q"`
	VX  int64  `msgpack:"vx"`
	VY  int64  `msgpack:"vy"`
}

type attackReq struct {
	Hit    HitID `msgpack:"h"`
	Target int64 `msgpack:"t"`
	Amount int32 `msgpack:"n"`
}

type ownerNotice struct {
	ID        int64  `msgpack:"i"`
	Epoch     uint64 `msgpack:"e"`
	LastInput uint64 `msgpack:"in,omitempty"`
}

// ViewEntity 是视野里的一个实体。Ghost 为 true 表示它在本格只是只读镜像（Real 在邻格）。
type ViewEntity struct {
	ID    int64 `msgpack:"i"`
	X     int64 `msgpack:"x"`
	Y     int64 `msgpack:"y"`
	HP    int32 `msgpack:"hp"`
	Ghost bool  `msgpack:"g,omitempty"`
}

type viewMsg struct {
	Observer  int64        `msgpack:"o"`
	Epoch     uint64       `msgpack:"e"`
	Tick      uint64       `msgpack:"t"`
	LastInput uint64       `msgpack:"in,omitempty"`
	Self      EntityState  `msgpack:"s"`
	Entities  []ViewEntity `msgpack:"v,omitempty"`
	Truncated bool         `msgpack:"tr,omitempty"`
}

type attackResultMsg struct {
	Hit       HitID         `msgpack:"h"`
	Target    int64         `msgpack:"t"`
	Outcome   DamageOutcome `msgpack:"o"`
	Reason    RejectReason  `msgpack:"r,omitempty"`
	Duplicate bool          `msgpack:"d,omitempty"`
	HP        int32         `msgpack:"hp"`
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
	Kind   msgKind `msgpack:"k"`
	From   int32   `msgpack:"f"`
	SentAt int64   `msgpack:"t,omitempty"`

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
	View         *viewMsg         `msgpack:"vw,omitempty"`
	AttackResult *attackResultMsg `msgpack:"ar,omitempty"`
	Lost         *lostNotice      `msgpack:"ls,omitempty"`
	EnterRefused *enterRefused    `msgpack:"rf,omitempty"`
}

var errBadMessage = errors.New("cellworld: malformed message")

func encodeMsg(m *wireMsg) ([]byte, error) { return msgpack.Marshal(m) }

// decodeMsg 解码并检查 Kind 与对应字段一致；不一致按坏消息丢弃，不进入状态机。
func decodeMsg(data []byte) (*wireMsg, error) {
	var m wireMsg
	if err := msgpack.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", errBadMessage, err)
	}
	var present bool
	switch m.Kind {
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
	case kindView:
		present = m.View != nil
	case kindAttackResult:
		present = m.AttackResult != nil
	case kindLost:
		present = m.Lost != nil
	case kindEnterRefused:
		present = m.EnterRefused != nil
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
