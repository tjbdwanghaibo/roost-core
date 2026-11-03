package cellworld

import (
	"slices"
	"time"
)

// Ghost 通道：Real 的 owner 每拍把边界带内的 Real 完整状态发给相关邻格（按目标格合批、分块），离开边界带
// 发显式删除；接收端按版本只接受更新的状态，删除留墓碑，超过 GhostTTL 没更新的 Ghost 自行删除。
// 全量而非增量：任一条消息丢失都会被下一拍覆盖，不需要可靠传输与基线（原型简化，带宽见 §6）。

// publishGhost 把一个 Real 的本拍状态放进各目标格的批次，并对上一拍发过、这拍不再需要的格发删除。
func (c *Cell) publishGhost(e *entity) {
	targets := c.grid.GhostTargets(c.id, e.st.Pos(), c.scratch[:0])
	for _, target := range targets {
		b := c.batchFor(target)
		b.Updates = append(b.Updates, e.st)
		c.maybeFlush(target, b)
	}
	for _, old := range e.ghostedTo {
		if !slices.Contains(targets, old) {
			b := c.batchFor(old)
			b.Removes = append(b.Removes, ghostRemove{ID: e.st.ID, Ver: e.st.Ver})
			c.maybeFlush(old, b)
		}
	}
	e.ghostedTo = append(e.ghostedTo[:0], targets...)
	c.scratch = targets[:0]
}

func (c *Cell) batchFor(target CellID) *ghostBatch {
	b := c.batches[target]
	if b == nil {
		b = &ghostBatch{}
		c.batches[target] = b
	}
	return b
}

// maybeFlush 在批次达到 GhostChunk 时立即发出，限制单条消息体积。
func (c *Cell) maybeFlush(target CellID, b *ghostBatch) {
	if len(b.Updates)+len(b.Removes) >= c.cfg.GhostChunk {
		c.sendGhosts(target, b, time.Now())
	}
}

func (c *Cell) flushGhosts(now time.Time) {
	for target, b := range c.batches {
		if len(b.Updates)+len(b.Removes) > 0 {
			c.sendGhosts(target, b, now)
		}
	}
}

func (c *Cell) sendGhosts(target CellID, b *ghostBatch, now time.Time) {
	c.send(c.grid.SID(target), &wireMsg{Kind: kindGhost, SentAt: now.UnixNano(), Ghost: b})
	b.Updates, b.Removes = b.Updates[:0], b.Removes[:0]
}

// onGhostBatch 应用一个邻格发来的 Ghost 批次。
func (c *Cell) onGhostBatch(from int32, sentAt int64, b *ghostBatch, now time.Time) {
	fromCell, ok := c.grid.CellOf(from)
	if !ok || fromCell == c.id {
		c.stats.staleDrops.Add(1)
		return
	}
	for _, s := range b.Updates {
		c.applyGhost(fromCell, s, now)
	}
	for _, r := range b.Removes {
		c.applyGhostRemove(r, now)
	}
	if c.cfg.Hooks.GhostApplied != nil && sentAt != 0 {
		c.cfg.Hooks.GhostApplied(c.id, now.Sub(time.Unix(0, sentAt)), len(b.Updates)+len(b.Removes))
	}
}

func (c *Cell) applyGhost(fromCell CellID, s EntityState, now time.Time) {
	if s.ID == 0 {
		return
	}
	e, ok := c.ents[s.ID]
	if !ok {
		if t, dead := c.tombs[s.ID]; dead && !s.Ver.Newer(t.ver) {
			c.stats.staleDrops.Add(1)
			return
		}
		delete(c.tombs, s.ID)
		e = &entity{role: roleGhost}
		c.ents[s.ID] = e
		c.setGhost(e, fromCell, s, now)
		return
	}
	switch e.role {
	case roleGhost:
		if !s.Ver.Newer(e.st.Ver) {
			c.stats.staleDrops.Add(1)
			return
		}
		c.setGhost(e, fromCell, s, now)
	case roleReal:
		// 本格是 Real 时别处来的状态一定是旧的（交接前的旧 owner 迟到消息）。比本格更新的 epoch 说明出现了
		// 第二个 Real，协议保证不会发生；计数供测试断言。
		if s.Ver.Epoch > e.st.Ver.Epoch {
			c.stats.conflicts.Add(1)
		}
		c.stats.staleDrops.Add(1)
	case roleFrozen:
		// 只有 epoch ≥ 交接 epoch 的 Real 能发出这样的状态，它证明目标格已经接受交接（隐式确认）。
		if s.Ver.Epoch >= e.handoff.epoch {
			c.completeHandoff(e, now)
			if e2, still := c.ents[s.ID]; still && e2.role == roleGhost && s.Ver.Newer(e2.st.Ver) {
				c.setGhost(e2, fromCell, s, now)
			} else if !still {
				c.applyGhost(fromCell, s, now) // 已删除（带外）但新 owner 仍向本格发：按新 Ghost 建
			}
			return
		}
		c.stats.staleDrops.Add(1)
	}
}

func (c *Cell) setGhost(e *entity, owner CellID, s EntityState, now time.Time) {
	e.st, e.owner, e.heardAt = s, owner, now
	c.reindex(e)
}

func (c *Cell) applyGhostRemove(r ghostRemove, now time.Time) {
	if e, ok := c.ents[r.ID]; ok {
		if e.role != roleGhost || !r.Ver.AtLeast(e.st.Ver) {
			c.stats.staleDrops.Add(1)
			return
		}
		c.removeEntity(e)
	}
	if t, ok := c.tombs[r.ID]; !ok || r.Ver.Newer(t.ver) {
		c.tombs[r.ID] = tombstone{ver: r.Ver, until: now.Add(2 * c.cfg.GhostTTL)}
	}
}
