package cellworld

import (
	"slices"
	"time"
)

// 交接（handoff）状态机，见 FEATURE-2026-11-03-seamless-world §5.3。
//
// 不变量：同一 (实体, epoch) 至多一个格取得 Real；旧格先冻结（失去 Real）再发 Offer，新格收到 Offer 才安装，
// 所以任一时刻至多一个 Real。一个 epoch 只对应一个目标格，重传只发给同一个目标。旧格得不到结论时只删除、
// 不恢复（无法证明目标没装），代价是目标不可达超过 HandoffAbort 时实体丢失、由 Base 重新进入补回。

// beginHandoff 冻结本格的 Real 并向 target 发 Offer。epoch 在这里 +1：交接后的状态、去重表与输入序号整份带走。
func (c *Cell) beginHandoff(e *entity, target CellID, now time.Time) {
	c.realChanged(e, false)
	epoch := e.st.Ver.Epoch + 1
	e.st.Ver = Version{Epoch: epoch, Seq: e.st.Ver.Seq + 1}
	e.role = roleFrozen
	c.seen[e.st.ID] = seenEpoch{epoch: epoch, accepted: true, at: now}
	c.pruneApplied(e, now)
	offer := &handoffOffer{
		State: e.st, From: c.id, LastInput: e.lastInput,
		GhostedTo: slices.Clone(e.ghostedTo), Applied: slices.Clone(e.applied),
	}
	data, err := encodeMsg(&wireMsg{Kind: kindOffer, From: c.sid, Offer: offer})
	if err != nil { // 编码失败只可能是程序错误；仍保持冻结，由超时收尾
		c.stats.publishErrors.Add(1)
	}
	e.handoff = &pendingHandoff{target: target, epoch: epoch, startedAt: now, lastSent: now, payload: data}
	c.stats.handoffsStarted.Add(1)
	if data != nil {
		c.sendRaw(c.grid.SID(target), data)
	}
}

// retransmitOrAbort 在每拍检查冻结中的交接：到期放弃，否则按 OfferRetry 重传同一份 Offer。
func (c *Cell) retransmitOrAbort(e *entity, now time.Time) {
	p := e.handoff
	if now.Sub(p.startedAt) >= c.cfg.HandoffAbort {
		c.stats.handoffsAborted.Add(1)
		if c.cfg.Hooks.HandoffAborted != nil {
			c.cfg.Hooks.HandoffAborted(c.id, e.st.ID, p.epoch)
		}
		if e.st.Kind == KindPlayer && e.st.Base != 0 {
			c.send(e.st.Base, &wireMsg{Kind: kindLost, Lost: &lostNotice{State: e.st}})
		}
		c.tombs[e.st.ID] = tombstone{ver: e.st.Ver, until: now.Add(2 * c.cfg.GhostTTL)}
		c.removeEntity(e)
		return
	}
	if now.Sub(p.lastSent) >= c.cfg.OfferRetry && p.payload != nil {
		p.lastSent = now
		c.stats.offerRetransmits.Add(1)
		c.sendRaw(c.grid.SID(p.target), p.payload)
	}
}

// onOffer 是目标格的决定。回复“已接受”的含义是“epoch E 已有人接手，你可以放手”，所以本格已经越过 E
// （E 被本格接受后又交接走了）同样回“已接受”；回复幂等，重复 Offer 不会重装。
func (c *Cell) onOffer(from int32, o *handoffOffer, now time.Time) {
	c.stats.offersReceived.Add(1)
	st := o.State
	epoch := st.Ver.Epoch
	reply := func(accepted bool) {
		c.send(from, &wireMsg{Kind: kindOfferReply, OfferReply: &handoffReply{ID: st.ID, Epoch: epoch, Accepted: accepted}})
	}
	if st.ID == 0 || epoch == 0 {
		return
	}
	e, known := c.ents[st.ID]
	if known {
		switch {
		case e.role == roleFrozen && epoch > e.handoff.epoch:
			// 实体已经从目标格再交接回本格：只有接受过本格 epoch 的目标才能发出更大的 epoch，隐式完成后按新 Offer 处理。
			c.completeHandoff(e, now)
			e, known = c.ents[st.ID]
		case (e.role == roleReal || e.role == roleFrozen) && e.st.Ver.Epoch >= epoch:
			reply(true)
			return
		case e.st.Ver.Epoch > epoch:
			reply(true)
			return
		}
	}
	if s, ok := c.seen[st.ID]; ok {
		switch {
		case s.epoch > epoch:
			reply(true)
			return
		case s.epoch == epoch:
			reply(s.accepted)
			return
		}
	}
	if known && e.st.Ver.Epoch >= epoch {
		// 本地只有同 epoch 的 Ghost 而没有 seen 记录：按协议不可达（同 epoch 的状态只能来自接受了它的格）；
		// 保守拒绝并记下，不冒险装出第二个 Real。
		c.seen[st.ID] = seenEpoch{epoch: epoch, accepted: false, at: now}
		reply(false)
		return
	}
	target, inWorld := c.grid.CellAt(st.Pos())
	if !inWorld || target != c.id || c.realCount() >= c.cfg.MaxReals {
		c.seen[st.ID] = seenEpoch{epoch: epoch, accepted: false, at: now}
		reply(false)
		return
	}
	ghostedTo := make([]CellID, 0, len(o.GhostedTo)+1)
	for _, id := range append(slices.Clone(o.GhostedTo), o.From) {
		if id != c.id && c.grid.Contains(id) && !slices.Contains(ghostedTo, id) {
			ghostedTo = append(ghostedTo, id)
		}
	}
	c.installReal(st, now, o.LastInput, slices.Clone(o.Applied), ghostedTo)
	reply(true)
}

// onOfferReply 只处理与本格当前冻结 epoch、目标都一致的回复；其余（重复、迟到、过期）丢弃。
func (c *Cell) onOfferReply(from int32, r *handoffReply, now time.Time) {
	e, ok := c.ents[r.ID]
	if !ok || e.role != roleFrozen || e.handoff.epoch != r.Epoch || c.grid.SID(e.handoff.target) != from {
		c.stats.staleDrops.Add(1)
		return
	}
	if r.Accepted {
		c.completeHandoff(e, now)
		return
	}
	// 被拒：目标承诺以后不接受这个 epoch（seen 记录），本格以该 epoch 恢复为 Real，退避后用下一个 epoch 再试。
	p := e.handoff
	e.handoff = nil
	e.role = roleReal
	e.retryAfter = now.Add(c.cfg.RejectBackoff)
	c.stats.handoffsRejected.Add(1)
	c.realChanged(e, true)
	if c.cfg.Hooks.HandoffDone != nil {
		c.cfg.Hooks.HandoffDone(c.id, e.st.ID, p.epoch, false, now.Sub(p.startedAt))
	}
}

// completeHandoff 在得到“已接受”（显式或隐式）后收尾：仍在本格边界带内就降为 Ghost（owner = 目标），否则删除。
func (c *Cell) completeHandoff(e *entity, now time.Time) {
	p := e.handoff
	if p == nil {
		return
	}
	c.stats.handoffsAccepted.Add(1)
	if c.cfg.Hooks.HandoffDone != nil {
		c.cfg.Hooks.HandoffDone(c.id, e.st.ID, p.epoch, true, now.Sub(p.startedAt))
	}
	e.handoff = nil
	e.ghostedTo, e.applied, e.lastInput = nil, nil, 0
	if c.grid.InBand(c.id, e.st.Pos()) {
		e.role, e.owner, e.heardAt = roleGhost, p.target, now
		return
	}
	c.tombs[e.st.ID] = tombstone{ver: e.st.Ver, until: now.Add(2 * c.cfg.GhostTTL)}
	c.removeEntity(e)
}
