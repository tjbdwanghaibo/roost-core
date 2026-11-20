package cellworld

import (
	"context"
	"crypto/rand"
	"errors"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
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

type attackCommand struct {
	requestID int64
	target    int64
	amount    int32
}

// AttackWithRequest returns the original admission receipt on replay; it never sends a second
// attack on replay, even when no verdict arrived. Zero preserves the old Attack semantics.
func (n *Node) AttackWithRequest(ctx context.Context, playerEntity, target int64, amount int32, requestID int64) (HitID, error) {
	if requestID == 0 {
		return n.Attack(ctx, playerEntity, target, amount)
	}
	if requestID < 0 {
		return HitID{}, ErrAttackRequestInvalid
	}
	out, err := n.cfg.Nest.Request(ctx, hBaseAttackRequest, playerEntity, nest.Params{attackCommand{requestID, target, amount}})
	if err != nil {
		return HitID{}, err
	}
	return out.(HitID), nil
}

func (n *Node) handleAttackRequest(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	p, err := playerOf(es)
	if err != nil {
		return nil, err
	}
	ledger, ok := p.(AttackRequestPlayer)
	if !ok {
		return nil, ErrAttackLedgerMissing
	}
	cmd := params[0].(attackCommand)
	slot := cmd.requestID % AttackRequestWindow
	high, old := ledger.CellAttackSlot(slot)
	if cmd.requestID <= high-AttackRequestWindow {
		return nil, ErrAttackRequestExpired
	}
	// Read before consulting volatile routing state: reconnect/recovery can leave it empty.
	if old.RequestID == cmd.requestID {
		if old.Target != cmd.target {
			return nil, ErrAttackRequestConflict
		}
		return old.Hit, nil
	}
	st := p.BaseState()
	fx := &txEffects{}
	hit, err := prepareAttack(&st, cmd.target, cmd.amount, fx)
	if err != nil {
		return nil, err
	}
	ledger.SetCellAttackSlot(slot, max(high, cmd.requestID), AttackReceipt{RequestID: cmd.requestID, Target: cmd.target, Hit: hit})
	p.SetBaseState(st)
	n.registerCommit(fx)
	return hit, nil
}

// Only a newly admitted operation may reach this function. Seq is for display; ID identifies
// the internal Hit, not the client operation. No publication before the player commits.
func prepareAttack(st *BaseState, target int64, amount int32, fx *txEffects) (HitID, error) {
	if !st.InWorld || st.Cell == 0 {
		return HitID{}, ErrNotInWorld
	}
	st.AttackSeq++
	hit := HitID{Attacker: st.Actor, Seq: st.AttackSeq, ID: rand.Text()}
	fx.send(st.Cell, &wireMsg{Kind: kindAttack, Attack: &attackReq{Hit: hit, Entry: st.Entry, Target: target, Amount: amount}})
	return hit, nil
}
