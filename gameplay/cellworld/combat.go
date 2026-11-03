package cellworld

import (
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 跨格交互（v1 只有伤害），见 FEATURE-2026-11-03-seamless-world §5.4。
//
// 结算方永远是目标的 Real：攻击者所在格只做前置检查（攻击者是本格 Real、目标在本格视野里且在距离内），
// 目标不在本格是 Real 时把请求转发给它所知的 owner，并在 AttackDeadline 内按 AttackRetry 重发。
// 结算方按 HitID 去重，去重表随交接带走，所以重复、迟到、改投新 owner 的请求都只结算一次。
// 待决请求属于攻击者所在格（不随攻击者迁移），由它向 Base 报结论。

// onAttack 处理 Base 发来的攻击。
func (c *Cell) onAttack(from int32, req *attackReq, now time.Time) {
	result := func(outcome DamageOutcome, reason RejectReason, duplicate bool, hp int32) {
		c.send(from, &wireMsg{Kind: kindAttackResult, AttackResult: &attackResultMsg{
			Hit: req.Hit, Target: req.Target, Outcome: outcome, Reason: reason, Duplicate: duplicate, HP: hp,
		}})
	}
	if _, pending := c.attacks[req.Hit]; pending {
		return // 同一攻击已在途，结论到了会报
	}
	attacker, ok := c.ents[req.Hit.Attacker]
	if !ok || attacker.role != roleReal {
		result(OutcomeRejected, ReasonNotOwner, false, 0)
		return
	}
	target, ok := c.ents[req.Target]
	if !ok {
		result(OutcomeRejected, ReasonNotVisible, false, 0)
		return
	}
	if !spatial.WithinDistance(attacker.st.Pos(), target.st.Pos(), c.cfg.AttackRange) {
		result(OutcomeRejected, ReasonOutOfRange, false, target.st.HP)
		return
	}
	if target.role == roleReal {
		outcome, reason, duplicate := c.applyDamage(target, req.Hit, req.Amount, now)
		result(outcome, reason, duplicate, target.st.HP)
		return
	}
	p := &pendingAttack{
		base: from, hit: req.Hit, target: req.Target, amount: req.Amount,
		targetCell: c.ownerHint(target), deadline: now.Add(c.cfg.AttackDeadline),
	}
	c.attacks[req.Hit] = p
	c.stats.damageForwarded.Add(1)
	c.sendDamage(p, now)
}

// ownerHint 是本格所知的、非 Real 记录的 owner：Ghost 的来源格或 Frozen 的交接目标。
func (c *Cell) ownerHint(e *entity) CellID {
	if e.role == roleFrozen {
		return e.handoff.target
	}
	return e.owner
}

// sendDamage 把待决伤害发给 targetCell；targetCell 是本格时就地查（目标可能刚迁进来）。
func (c *Cell) sendDamage(p *pendingAttack, now time.Time) {
	p.nextSend = now.Add(c.cfg.AttackRetry)
	if p.targetCell != c.id {
		c.send(c.grid.SID(p.targetCell), &wireMsg{Kind: kindDamage, Damage: &damageReq{Hit: p.hit, Target: p.target, Amount: p.amount}})
		return
	}
	target, ok := c.ents[p.target]
	if !ok {
		return // 不认识：等下一次重试或到期
	}
	if target.role != roleReal {
		p.targetCell = c.ownerHint(target)
		return
	}
	outcome, reason, duplicate := c.applyDamage(target, p.hit, p.amount, now)
	c.finishAttack(p, outcome, reason, duplicate, target.st.HP)
}

func (c *Cell) finishAttack(p *pendingAttack, outcome DamageOutcome, reason RejectReason, duplicate bool, hp int32) {
	delete(c.attacks, p.hit)
	if outcome == OutcomeUnknown {
		c.stats.damageUnknown.Add(1)
	}
	c.send(p.base, &wireMsg{Kind: kindAttackResult, AttackResult: &attackResultMsg{
		Hit: p.hit, Target: p.target, Outcome: outcome, Reason: reason, Duplicate: duplicate, HP: hp,
	}})
}

// onDamage：本格是目标 Real 就结算（去重），否则回“重试”与所知 owner。
func (c *Cell) onDamage(from int32, req *damageReq, now time.Time) {
	reply := &damageReply{Hit: req.Hit, Target: req.Target}
	target, ok := c.ents[req.Target]
	switch {
	case ok && target.role == roleReal:
		reply.Outcome, reply.Reason, reply.Duplicate = c.applyDamage(target, req.Hit, req.Amount, now)
		reply.HP = target.st.HP
	case ok:
		reply.Outcome, reply.Hint = OutcomeRetry, c.grid.SID(c.ownerHint(target))
	default:
		reply.Outcome = OutcomeRetry
	}
	c.send(from, &wireMsg{Kind: kindDamageReply, DamageReply: reply})
}

// onDamageReply：结论报给 Base；“重试”按提示改投，下一拍到点再发。
func (c *Cell) onDamageReply(from int32, r *damageReply, now time.Time) {
	p, ok := c.attacks[r.Hit]
	if !ok {
		c.stats.staleDrops.Add(1) // 重复回复或已到期
		return
	}
	switch r.Outcome {
	case OutcomeApplied, OutcomeRejected:
		c.finishAttack(p, r.Outcome, r.Reason, r.Duplicate, r.HP)
	case OutcomeRetry:
		c.stats.damageRetries.Add(1)
		if hint, ok := c.grid.CellOf(r.Hint); ok {
			p.targetCell = hint
		}
	}
}

// retryAttacks 每拍检查待决伤害：到期报 Unknown，到点重发（请求或回复丢失都靠它恢复，去重保证不重复结算）。
func (c *Cell) retryAttacks(now time.Time) {
	for _, p := range c.attacks {
		switch {
		case !now.Before(p.deadline):
			c.finishAttack(p, OutcomeUnknown, ReasonNone, false, 0)
		case !now.Before(p.nextSend):
			c.sendDamage(p, now)
		}
	}
}

// applyDamage 在目标 Real 上结算一次伤害。命中去重表返回重复；去重窗口内已满时拒绝（不淘汰旧记录，否则迟到的
// 重试会被再结算一次）。
func (c *Cell) applyDamage(target *entity, hit HitID, amount int32, now time.Time) (DamageOutcome, RejectReason, bool) {
	c.pruneApplied(target, now)
	if slices.ContainsFunc(target.applied, func(a appliedHit) bool { return a.Hit == hit }) {
		return OutcomeApplied, ReasonNone, true
	}
	if target.st.HP <= 0 {
		return OutcomeRejected, ReasonDead, false
	}
	if len(target.applied) >= c.cfg.MaxAppliedHits {
		return OutcomeRejected, ReasonBusy, false
	}
	target.st.HP = max(0, target.st.HP-max(0, amount))
	target.applied = append(target.applied, appliedHit{Hit: hit, At: now.UnixNano()})
	if c.cfg.Hooks.DamageApplied != nil {
		c.cfg.Hooks.DamageApplied(c.id, target.st.ID, hit, target.st.HP)
	}
	return OutcomeApplied, ReasonNone, false
}

// pruneApplied 删掉超出去重窗口的记录。
func (c *Cell) pruneApplied(e *entity, now time.Time) {
	cutoff := now.Add(-c.cfg.DedupWindow).UnixNano()
	e.applied = slices.DeleteFunc(e.applied, func(a appliedHit) bool { return a.At < cutoff })
}
