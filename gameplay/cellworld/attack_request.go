package cellworld

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

var hBaseAttackRequest = nest.NewHandlerName("cellworld_base_attack_request")

// AttackRequestWindow counts request numbers, not seconds or hits on a target.
// The high-water mark never goes backwards, including across Leave/Enter and Base recovery.
const AttackRequestWindow int64 = 64

var (
	ErrAttackRequestInvalid  = errors.New("cellworld: invalid attack request number")
	ErrAttackRequestExpired  = errors.New("cellworld: attack request number expired; original outcome unknown")
	ErrAttackRequestConflict = errors.New("cellworld: attack request number used with different parameters")
	ErrAttackLedgerMissing   = errors.New("cellworld: player has no persistent attack request ledger")
)

// AttackReceipt records admission on the Base, not delivery, damage, or a Cell verdict.
type AttackReceipt struct {
	RequestID int64
	Target    int64
	Hit       HitID
}

// AttackRequestPlayer is optional. Read/write under the player's Nest lock, through private
// persistent DAO fields. High and the replacement slot must roll back with the player transaction.
// A missing slot reads as a zero receipt. Only numbered attacks access this ledger.
type AttackRequestPlayer interface {
	CellAttackSlot(slot int64) (high int64, receipt AttackReceipt)
	SetCellAttackSlot(slot, high int64, receipt AttackReceipt)
}

// AttackWithRequest returns the original admission receipt on replay; it never sends a second
// attack on replay, even when no verdict arrived. Zero preserves the old Attack semantics.
func (n *Node) AttackWithRequest(ctx context.Context, playerEntity, target int64, amount int32, requestID int64) (HitID, error) {
	if requestID < 0 {
		return HitID{}, ErrAttackRequestInvalid
	}
	return n.requestAttack(ctx, playerEntity, baseCmd{kind: cmdAttack, target: target, amount: amount, requestID: requestID})
}

// AttackSession 只接受当前在线连接的攻击。在同一笔 Player 事务中先核验会话，再按
// AttackWithRequest 的规则处理：无号为一次新攻击，带号可重放原回执，不等待 Cell 结算。
// 冷恢复后尚无绑定且未进场时，本玩家的活连接只能查旧回执。session 须来自已认证的本机连接。
func (n *Node) AttackSession(ctx context.Context, base int64, session BaseSession, target int64, damage int32, requestID int64) (HitID, error) {
	return n.requestAttack(ctx, base, baseCmd{kind: cmdSessionAttack, session: session, target: target, amount: damage, requestID: requestID})
}

func (n *Node) requestAttack(ctx context.Context, base int64, cmd baseCmd) (HitID, error) {
	handler := hBaseCmd // 无号保持 async，带号回执必须 strict。
	if cmd.requestID != 0 {
		handler = hBaseAttackRequest
	}
	out, err := n.cfg.Nest.Request(ctx, handler, base, nest.Params{cmd})
	if err != nil {
		return HitID{}, err
	}
	return out.(HitID), nil
}

// admitAttack 在一笔 Player 事务内处理会话检查、请求号和出手。
func (n *Node) admitAttack(p BasePlayer, st *BaseState, cmd baseCmd, fx *txEffects) (HitID, error) {
	if cmd.kind == cmdSessionAttack && !n.currentSession(p, st.Session, cmd.session) {
		// 冷 Base 可以没有路由和绑定，却已从 WAL 恢复回执。只开放带号读取；未命中
		// 会在下面的 InWorld 检查返回，不写账本。正常离场保留 Admission，不走此例外。
		if st.InWorld || st.Session != (SessionState{}) || !n.sessionLive(p, cmd.session) {
			return HitID{}, ErrStaleSession
		}
	}
	var ledger AttackRequestPlayer
	var slot, high int64
	if cmd.requestID < 0 {
		return HitID{}, ErrAttackRequestInvalid
	}
	if cmd.requestID > 0 {
		var ok bool
		ledger, ok = p.(AttackRequestPlayer)
		if !ok {
			return HitID{}, ErrAttackLedgerMissing
		}
		slot = cmd.requestID % AttackRequestWindow
		var old AttackReceipt
		high, old = ledger.CellAttackSlot(slot)
		if cmd.requestID <= high-AttackRequestWindow {
			return HitID{}, ErrAttackRequestExpired
		}
		if old.RequestID == cmd.requestID {
			if old.Target != cmd.target {
				return HitID{}, ErrAttackRequestConflict
			}
			return old.Hit, nil
		}
	}
	if !st.InWorld || st.Cell == 0 {
		return HitID{}, ErrNotInWorld
	}
	st.AttackSeq++
	hit := HitID{Attacker: st.Actor, Seq: st.AttackSeq, ID: rand.Text()}
	if ledger != nil {
		ledger.SetCellAttackSlot(slot, max(high, cmd.requestID), AttackReceipt{RequestID: cmd.requestID, Target: cmd.target, Hit: hit})
	}
	p.SetBaseState(*st)
	fx.send(st.Cell, &wireMsg{Kind: kindAttack, Attack: &attackReq{Hit: hit, Entry: st.Entry, Target: cmd.target, Amount: cmd.amount}})
	n.registerCommit(fx)
	return hit, nil
}
