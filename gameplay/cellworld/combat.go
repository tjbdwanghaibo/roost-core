package cellworld

import (
	"slices"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 跨格交互（v1 起只有伤害），见 FEATURE-2026-11-03-seamless-world §5.4 / §7.8 (c)：不加跨进程锁，统一“转发给目标
// Real 结算”。结算方是目标的 Real：HP 与去重表在目标 SceneActor / Monster 的 DAO 上，随交接带走，所以重复、迟到、
// 改投新 owner 的请求都只结算一次。攻击者所在格只做前置检查（攻击者是本格 Real、目标在本格视野里且在距离内）；
// 目标不在本格是 Real 时把请求转发给它所知的 owner，待决请求记在攻击者所在格的区域表里，到期报“结果未知”。

// handleAttack：Base 发来的攻击，锁 Scene、攻击者与目标（目标不在本格时为 nil）。
func (n *Node) handleAttack(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	r, err := regionOf(es)
	if err != nil {
		return nil, err
	}
	m := params[0].(*wireMsg)
	attacker := actorOf(es, m.Attack.Hit.Attacker)
	target := actorOf(es, m.Attack.Target)
	fx := &txEffects{}
	n.onAttack(r, attacker, target, m.From, m.Attack, time.Now(), fx)
	n.registerCommit(fx)
	return nil, nil
}

func (n *Node) onAttack(r *Region, attacker, target Actor, from int32, req *attackReq, now time.Time, fx *txEffects) {
	result := func(outcome DamageOutcome, reason RejectReason, duplicate bool, hp int32) {
		fx.send(from, &wireMsg{Kind: kindAttackResult, AttackResult: &attackResultMsg{
			Hit: req.Hit, Target: req.Target, Outcome: outcome, Reason: reason, Duplicate: duplicate, HP: hp,
		}})
	}
	if _, pending := r.attacks.get(req.Hit); pending {
		return // 同一攻击已在途，结论到了会报
	}
	if attacker == nil || attacker.CellState().Role != RoleReal {
		result(OutcomeRejected, ReasonNotOwner, false, 0)
		return
	}
	from0 := attacker.CellState().Pos()
	var targetState EntityState
	var targetRole Role
	var owner CellID
	known := false
	if target != nil {
		if ts := target.CellState(); ts.Role == RoleReal || ts.Role == RoleFrozen {
			targetState, targetRole, known = ts.EntityState, ts.Role, true
			if ts.Handoff != nil {
				owner = ts.Handoff.Target
			}
		}
	}
	if !known {
		targetState, targetRole, owner, known = r.lookup(req.Target, n.cell)
	}
	if !known {
		result(OutcomeRejected, ReasonNotVisible, false, 0)
		return
	}
	if !spatial.WithinDistance(from0, targetState.Pos(), n.cfg.AttackRange) {
		result(OutcomeRejected, ReasonOutOfRange, false, targetState.HP)
		return
	}
	if target != nil && targetRole == RoleReal {
		outcome, reason, duplicate, hp := n.applyDamage(target, req.Hit, req.Amount, now, fx)
		result(outcome, reason, duplicate, hp)
		return
	}
	p := pendingAttack{
		Base: from, Hit: req.Hit, Target: req.Target, Amount: req.Amount,
		TargetCell: owner, Deadline: now.Add(n.cfg.AttackDeadline).UnixNano(), NextSend: now.Add(n.cfg.AttackRetry).UnixNano(),
	}
	r.attacks.set(req.Hit, p)
	n.stats.damageForwarded.Add(1)
	// owner 是本格自己时（目标刚交进来、实体还没建好或刚交出）也走一遍消息：发给自己的收件主题，经目标的锁结算。
	fx.sends = append(fx.sends, n.damageTo(p))
}

// damageTo 把待决伤害发给 TargetCell。
func (n *Node) damageTo(p pendingAttack) outgoing {
	return outgoing{sid: n.cfg.Grid.SID(p.TargetCell), msg: &wireMsg{Kind: kindDamage, Damage: &damageReq{Hit: p.Hit, Target: p.Target, Amount: p.Amount}}}
}

// onDamage：本格是目标 Real 就结算（去重），否则回“重试”与所知 owner。
func (n *Node) onDamage(r *Region, target Actor, from int32, req *damageReq, now time.Time, fx *txEffects) {
	reply := &damageReply{Hit: req.Hit, Target: req.Target}
	switch {
	case target != nil && target.CellState().Role == RoleReal:
		reply.Outcome, reply.Reason, reply.Duplicate, reply.HP = n.applyDamage(target, req.Hit, req.Amount, now, fx)
	default:
		reply.Outcome = OutcomeRetry
		if target != nil {
			if ts := target.CellState(); ts.Role == RoleFrozen && ts.Handoff != nil {
				reply.Hint = n.cfg.Grid.SID(ts.Handoff.Target)
			}
		}
		if reply.Hint == 0 {
			if _, _, owner, ok := r.lookup(req.Target, n.cell); ok && owner != n.cell {
				reply.Hint = n.cfg.Grid.SID(owner)
			}
		}
	}
	fx.send(from, &wireMsg{Kind: kindDamageReply, DamageReply: reply})
}

// onDamageReply：结论报给 Base；“重试”按提示改投，下一拍到点再发。
func (n *Node) onDamageReply(r *Region, rep *damageReply, now time.Time, fx *txEffects) {
	p, ok := r.attacks.get(rep.Hit)
	if !ok {
		n.stats.staleDrops.Add(1) // 重复回复或已到期
		return
	}
	switch rep.Outcome {
	case OutcomeApplied, OutcomeRejected:
		r.attacks.del(rep.Hit)
		fx.send(p.Base, &wireMsg{Kind: kindAttackResult, AttackResult: &attackResultMsg{
			Hit: p.Hit, Target: p.Target, Outcome: rep.Outcome, Reason: rep.Reason, Duplicate: rep.Duplicate, HP: rep.HP,
		}})
	case OutcomeRetry:
		n.stats.damageRetries.Add(1)
		if hint, ok := n.cfg.Grid.CellOf(rep.Hint); ok && hint != p.TargetCell {
			p.TargetCell = hint
			r.attacks.set(rep.Hit, p)
		}
	}
}

// applyDamage 在目标 Real 上结算一次伤害（目标的锁内，改它的 DAO）。命中去重表返回重复；去重窗口内已满时拒绝
// （不淘汰旧记录，否则迟到的重试会被再结算一次）。
func (n *Node) applyDamage(target Actor, hit HitID, amount int32, now time.Time, fx *txEffects) (DamageOutcome, RejectReason, bool, int32) {
	st := target.CellState()
	nowNano := now.UnixNano()
	applied := pruneApplied(st.Applied, nowNano, n.cfg.DedupWindow)
	if slices.ContainsFunc(applied, func(a AppliedHit) bool { return a.Hit == hit }) {
		return OutcomeApplied, ReasonNone, true, st.HP
	}
	if st.HP <= 0 {
		return OutcomeRejected, ReasonDead, false, st.HP
	}
	if len(applied) >= n.cfg.MaxAppliedHits {
		return OutcomeRejected, ReasonBusy, false, st.HP
	}
	st.HP = max(0, st.HP-max(0, amount))
	st.Applied = append(applied, AppliedHit{Hit: hit, At: nowNano})
	target.SetCellState(st)
	if hook := n.cfg.Hooks.DamageApplied; hook != nil {
		cell, id, hp := n.cell, st.ID, st.HP
		fx.hooks = append(fx.hooks, func() { hook(cell, id, hit, hp) })
	}
	return OutcomeApplied, ReasonNone, false, st.HP
}
