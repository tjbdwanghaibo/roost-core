package cellworld

import (
	"slices"
	"sort"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// Region 是一个格的区域表，由该格的 Scene 实体持有，只在 Scene 的 Nest 锁内读写（Scene 的 handler 与 scene.cell 心跳）：
//
//	locals   本格角色 / NPC 最近报告的状态与身份（角色心跳提交后经 Node 报告；权威在角色实体的 DAO 上，这里是索引）
//	ghosts   邻格 Real 的只读镜像（不是实体）
//	seen     本格见过的每个实体的最高交接 epoch 与是否接受（交接幂等）
//	tombs    删除墓碑（迟到的旧 Ghost 不能复活）
//	attacks  本格发起、等目标所在格结论的跨格伤害
//
// 它是手写 DAO：每次写都经 undoMap 向当前 Nest 事务登记逆操作（同一事务里同一个键只登记一次，回滚还原到事务开始时
// 的值），所以 Scene handler 回滚时区域表一起还原——“交接提交成功才算移交”里 seen 的接受记录就靠它。值都是值类型，
// 写入后不原地修改。空间索引不常驻：每拍从 locals / ghosts 现建（A1：组件需要的索引在一次调用内构建）。
type Region struct {
	locals  undoMap[int64, localRec]
	ghosts  undoMap[int64, ghostRec]
	seen    undoMap[int64, seenEpoch]
	tombs   undoMap[int64, tombstone]
	attacks undoMap[HitID, pendingAttack]
	tick    uint64
}

// 区域各张表在事务里的 undo 字段号（undoMap 的 field）。
const (
	undoLocals uint64 = iota + 1
	undoGhosts
	undoSeen
	undoTombs
	undoAttacks
)

// NewRegion 构造空区域表。
func NewRegion() *Region {
	r := &Region{}
	r.locals.init(undoLocals)
	r.ghosts.init(undoGhosts)
	r.seen.init(undoSeen)
	r.tombs.init(undoTombs)
	r.attacks.init(undoAttacks)
	return r
}

// localRec 是本格一个角色 / NPC 的索引记录。GhostedTo 是上一次向哪些邻格发过 Ghost。
type localRec struct {
	St        EntityState
	Role      Role
	Handoff   *HandoffState
	GhostedTo []CellID
	// QuietSince：NPC 从什么时候起附近没有玩家（0 = 有人在附近或还没判断）；TierAsked：上次向它请求换档的时间。见 tier.go。
	QuietSince, TierAsked int64
}

type ghostRec struct {
	St      EntityState
	Owner   CellID
	HeardAt int64
}

type seenEpoch struct {
	Epoch    uint64
	Accepted bool
	At       int64
}

type tombstone struct {
	Ver   Version
	Until int64
}

type pendingAttack struct {
	Base       int32
	Hit        HitID
	Target     int64
	Amount     int32
	TargetCell CellID
	Deadline   int64
	NextSend   int64
}

// RegionHost 是持有 Region 的 Scene 实体。
type RegionHost interface {
	CellRegion() *Region
}

// Len 返回区域里的本格记录数与 Ghost 数（测试与诊断；只在 Scene 锁内调用）。
func (r *Region) Len() (locals, ghosts int) { return len(r.locals.m), len(r.ghosts.m) }

// undoMap 是登记逆操作的 map。在 rollback=undo 的 Nest 事务里，每个键第一次被改时登记“还原成事务开始时的值”；
// 不在事务里（单元测试直接调用）时不登记。
type undoMap[K comparable, V any] struct {
	m     map[K]V
	field uint64
}

func (u *undoMap[K, V]) init(field uint64) {
	u.m = make(map[K]V)
	u.field = field
}

func (u *undoMap[K, V]) get(k K) (V, bool) {
	v, ok := u.m[k]
	return v, ok
}

func (u *undoMap[K, V]) remember(k K) {
	old, had := u.m[k]
	nest.RecordUndoToken(u, u.field, k, func() error {
		if had {
			u.m[k] = old
		} else {
			delete(u.m, k)
		}
		return nil
	})
}

func (u *undoMap[K, V]) set(k K, v V) {
	u.remember(k)
	u.m[k] = v
}

func (u *undoMap[K, V]) del(k K) {
	if _, ok := u.m[k]; !ok {
		return
	}
	u.remember(k)
	delete(u.m, k)
}

// ---- 报告：本格角色提交后的状态 ----

// applyReports 应用一批本格角色的报告：更新索引，Real 按位置向邻格发 Ghost（按目标格合批），不再需要的邻格发删除。
func (n *Node) applyReports(r *Region, reports []ActorReport, now time.Time, out *[]outgoing) {
	batches := map[CellID]*ghostBatch{}
	batch := func(target CellID) *ghostBatch {
		b := batches[target]
		if b == nil {
			b = &ghostBatch{}
			batches[target] = b
		}
		return b
	}
	nowNano := now.UnixNano()
	for _, rep := range reports {
		id := rep.State.ID
		prev, known := r.locals.get(id)
		if !known && !rep.Removed {
			// 本地记录只由装入（install，在锁 Scene 的事务里直接写区域表）建立；经出站队列异步到达的报告找不到记录，说明
			// 交接结论 / 离场已在锁 Scene 的事务里直接删掉了它（报告晚于结论到达）。丢弃：重建出的记录没有实体、不会再被
			// 清掉（例如冻结报告晚到会留下一条永久的 frozen，挡住新 owner 的 Ghost）。
			n.stats.staleDrops.Add(1)
			continue
		}
		if known {
			// 同一角色的两次提交后回调可能乱序到达：旧一代（已交出又交回来之前）的、版本与身份都没变的报告丢弃。
			if rep.State.Ver.Epoch < prev.St.Ver.Epoch {
				continue
			}
			if !rep.Removed && !rep.State.Ver.Newer(prev.St.Ver) && rep.Role == prev.Role {
				continue
			}
		}
		if rep.Removed {
			r.locals.del(id)
			if rep.Tomb {
				for _, target := range prev.GhostedTo {
					b := batch(target)
					b.Removes = append(b.Removes, ghostRemove{ID: id, Ver: rep.State.Ver})
				}
				r.tombs.set(id, tombstone{Ver: rep.State.Ver, Until: nowNano + 2*int64(n.cfg.GhostTTL)})
				continue
			}
			// 交出：仍在本格边界带内就留一份 Ghost（owner = 交接目标），新 owner 的 Ghost 更新随后覆盖它。
			if rep.Handoff != nil && n.cfg.Grid.InBand(n.cell, rep.State.Pos()) {
				if g, ok := r.ghosts.get(id); !ok || rep.State.Ver.Newer(g.St.Ver) {
					r.ghosts.set(id, ghostRec{St: rep.State, Owner: rep.Handoff.Target, HeardAt: nowNano})
				}
			} else {
				r.tombs.set(id, tombstone{Ver: rep.State.Ver, Until: nowNano + 2*int64(n.cfg.GhostTTL)})
			}
			continue
		}
		rec := localRec{St: rep.State, Role: rep.Role, Handoff: rep.Handoff, GhostedTo: prev.GhostedTo, QuietSince: prev.QuietSince, TierAsked: prev.TierAsked}
		if !known {
			// 新装的 Real：第一次发 Ghost 时对全部邻格里不再需要的发删除，清掉旧 owner 留在它们那里的镜像。
			rec.GhostedTo = n.cfg.Grid.Neighbors(n.cell)
		}
		if rep.Role == RoleFrozen && rep.Handoff != nil {
			r.seen.set(id, seenEpoch{Epoch: rep.Handoff.Epoch, Accepted: true, At: nowNano})
		}
		if rep.Role == RoleReal {
			targets := n.cfg.Grid.GhostTargets(n.cell, rep.State.Pos(), nil)
			for _, target := range targets {
				b := batch(target)
				b.Updates = append(b.Updates, rep.State)
			}
			for _, old := range rec.GhostedTo {
				if !slices.Contains(targets, old) {
					b := batch(old)
					b.Removes = append(b.Removes, ghostRemove{ID: id, Ver: rep.State.Ver})
				}
			}
			rec.GhostedTo = targets
		}
		r.locals.set(id, rec)
		r.ghosts.del(id)
	}
	n.flushGhostBatches(batches, now, out)
}

// flushGhostBatches 按 GhostChunk 切块，生成发往各邻格的 Ghost 消息（提交后发出）。
func (n *Node) flushGhostBatches(batches map[CellID]*ghostBatch, now time.Time, out *[]outgoing) {
	targets := make([]CellID, 0, len(batches))
	for target := range batches {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Row != targets[j].Row {
			return targets[i].Row < targets[j].Row
		}
		return targets[i].Col < targets[j].Col
	})
	chunk := n.cfg.GhostChunk
	for _, target := range targets {
		b := batches[target]
		for len(b.Updates)+len(b.Removes) > 0 {
			part := &ghostBatch{}
			for len(part.Updates)+len(part.Removes) < chunk && len(b.Updates) > 0 {
				part.Updates = append(part.Updates, b.Updates[0])
				b.Updates = b.Updates[1:]
			}
			for len(part.Updates)+len(part.Removes) < chunk && len(b.Removes) > 0 {
				part.Removes = append(part.Removes, b.Removes[0])
				b.Removes = b.Removes[1:]
			}
			*out = append(*out, outgoing{sid: n.cfg.Grid.SID(target), msg: &wireMsg{Kind: kindGhost, SentAt: now.UnixNano(), Ghost: part}})
		}
	}
}

// ---- Ghost：邻格发来的镜像 ----

// applyGhostBatch 应用一个邻格发来的 Ghost 批次。本格的角色正在把自己交给发送方、而发送方已经发来 epoch ≥ 交接
// epoch 的状态时，说明交接已被接受（只有接受了它的格才能发出这个 epoch）：返回需要隐式完成的角色 ID（由 Node 投给
// 角色的 handler，Scene handler 里不能锁一个没声明的角色）。
func (n *Node) applyGhostBatch(r *Region, from int32, b *ghostBatch, now time.Time) (implicit []implicitDone) {
	fromCell, ok := n.cfg.Grid.CellOf(from)
	if !ok || fromCell == n.cell {
		n.stats.staleDrops.Add(1)
		return nil
	}
	nowNano := now.UnixNano()
	for _, s := range b.Updates {
		if s.ID == 0 {
			continue
		}
		if loc, isLocal := r.locals.get(s.ID); isLocal {
			switch loc.Role {
			case RoleReal:
				// 本格是 Real 时别处来的状态一定是旧的；更新的 epoch 意味着第二个 Real，协议保证不会发生（计数供测试断言）。
				if s.Ver.Epoch > loc.St.Ver.Epoch {
					n.stats.conflicts.Add(1)
				}
				n.stats.staleDrops.Add(1)
				continue
			case RoleFrozen:
				if loc.Handoff != nil && s.Ver.Epoch >= loc.Handoff.Epoch && fromCell == loc.Handoff.Target {
					implicit = append(implicit, implicitDone{id: s.ID, epoch: loc.Handoff.Epoch, from: from})
				}
			}
		}
		if t, dead := r.tombs.get(s.ID); dead && !s.Ver.Newer(t.Ver) {
			n.stats.staleDrops.Add(1)
			continue
		}
		if g, ok := r.ghosts.get(s.ID); ok && !s.Ver.Newer(g.St.Ver) {
			n.stats.staleDrops.Add(1)
			continue
		}
		r.tombs.del(s.ID)
		r.ghosts.set(s.ID, ghostRec{St: s, Owner: fromCell, HeardAt: nowNano})
	}
	for _, rm := range b.Removes {
		if g, ok := r.ghosts.get(rm.ID); ok {
			if rm.Ver.Newer(g.St.Ver) || rm.Ver == g.St.Ver {
				r.ghosts.del(rm.ID)
			} else {
				n.stats.staleDrops.Add(1)
				continue
			}
		}
		if t, ok := r.tombs.get(rm.ID); !ok || rm.Ver.Newer(t.Ver) {
			r.tombs.set(rm.ID, tombstone{Ver: rm.Ver, Until: nowNano + 2*int64(n.cfg.GhostTTL)})
		}
	}
	return implicit
}

type implicitDone struct {
	id    int64
	epoch uint64
	from  int32
}

// ---- 心跳：过期、伤害重试、witness ----

// regionBeat 是 scene.cell 心跳：过期清理 → 跨格伤害重试 / 到期 → NPC 档位（tier.go）→ 给本格每个玩家 Real 算 witness。
func (n *Node) regionBeat(r *Region, now time.Time, fx *txEffects) {
	out := &fx.sends
	r.tick++
	nowNano := now.UnixNano()
	for id, g := range r.ghosts.m {
		ttl := int64(n.cfg.GhostTTL)
		if g.St.Idle {
			ttl += int64(n.cfg.IdlePeriod) // 慢档 NPC 一个慢拍才更新一次 Ghost
		}
		if nowNano-g.HeardAt > ttl {
			r.ghosts.del(id)
		}
	}
	for id, t := range r.tombs.m {
		if nowNano > t.Until {
			r.tombs.del(id)
		}
	}
	for id, s := range r.seen.m {
		if nowNano-s.At > int64(n.cfg.SeenTTL) {
			if _, local := r.locals.get(id); !local {
				r.seen.del(id)
			}
		}
	}
	for hit, p := range r.attacks.m {
		switch {
		case nowNano >= p.Deadline:
			r.attacks.del(hit)
			n.stats.damageUnknown.Add(1)
			*out = append(*out, outgoing{sid: p.Base, msg: &wireMsg{Kind: kindAttackResult, AttackResult: &attackResultMsg{Hit: p.Hit, Target: p.Target, Outcome: OutcomeUnknown}}})
		case nowNano >= p.NextSend:
			p.NextSend = nowNano + int64(n.cfg.AttackRetry)
			r.attacks.set(hit, p)
			n.stats.damageRetries.Add(1)
			*out = append(*out, n.damageTo(p))
		}
	}
	view := &beatView{}
	n.retier(r, view, now, fx)
	n.witnesses(r, view, out)
	var reals, ghosts uint64
	for _, l := range r.locals.m {
		if l.Role == RoleReal {
			reals++
		}
	}
	ghosts = uint64(len(r.ghosts.m))
	n.stats.reals.Store(reals)
	n.stats.ghosts.Store(ghosts)
}

// beatView 是一次区域心跳里现建的空间索引：本格记录与 Ghost（本格边界带以内的），witness 与 NPC 档位判断共用，
// 第一次用到时才建、心跳结束即弃（A1：索引不常驻）。
type beatView struct {
	built  bool
	index  *spatial.BlockIndex
	states map[int64]EntityState
}

func (v *beatView) get(n *Node, r *Region) (*spatial.BlockIndex, map[int64]EntityState) {
	if v.built {
		return v.index, v.states
	}
	v.built = true
	index, err := spatial.NewBlockIndex(n.indexBounds, n.cfg.IndexBlock)
	if err != nil {
		return nil, nil // indexBounds 与 IndexBlock 在 NewNode 时已校验为正，不会走到这里
	}
	states := make(map[int64]EntityState, len(r.locals.m)+len(r.ghosts.m))
	for id, l := range r.locals.m {
		if n.indexBounds.Contains(l.St.Pos()) {
			states[id] = l.St
			index.Add(id, l.St.Pos())
		}
	}
	for id, g := range r.ghosts.m {
		if _, local := states[id]; local || !n.indexBounds.Contains(g.St.Pos()) {
			continue
		}
		states[id] = g.St
		index.Add(id, g.St.Pos())
	}
	v.index, v.states = index, states
	return index, states
}

// witnesses 给本格每个玩家 Real 推一份 witness：半径 R 内的本格记录与 Ghost（含自己），超出上限按距离取最近的。
func (n *Node) witnesses(r *Region, view *beatView, out *[]outgoing) {
	var observers []localRec
	for _, l := range r.locals.m {
		if l.Role == RoleReal && l.St.Kind == KindPlayer && l.St.Base != 0 {
			observers = append(observers, l)
		}
	}
	if len(observers) == 0 {
		return
	}
	index, states := view.get(n, r)
	if index == nil {
		return
	}
	sort.Slice(observers, func(i, j int) bool { return observers[i].St.ID < observers[j].St.ID })
	for _, o := range observers {
		center := o.St.Pos()
		var seen []EntityState
		for _, id := range index.QueryRadius(center, n.cfg.ViewRadius) {
			s := states[id]
			if spatial.WithinDistance(center, s.Pos(), n.cfg.ViewRadius) {
				seen = append(seen, s)
			}
		}
		truncated := false
		if len(seen) > n.cfg.MaxViewEntities {
			sort.Slice(seen, func(i, j int) bool {
				return spatial.DistanceSquared(center, seen[i].Pos()) < spatial.DistanceSquared(center, seen[j].Pos())
			})
			seen, truncated = seen[:n.cfg.MaxViewEntities], true
		}
		*out = append(*out, outgoing{sid: o.St.Base, msg: &wireMsg{Kind: kindWitness, Witness: &witnessMsg{
			Observer: o.St.ID, Epoch: o.St.Ver.Epoch, Tick: r.tick, Self: o.St, Entities: seen, Truncated: truncated,
		}}})
	}
}

// ---- 查询 ----

// lookup 返回本格（self）所知的某个实体：本格记录（owner 是 self，冻结中是交接目标）或 Ghost（owner 是来源格）。
func (r *Region) lookup(id int64, self CellID) (EntityState, Role, CellID, bool) {
	if l, ok := r.locals.get(id); ok {
		owner := self
		if l.Handoff != nil {
			owner = l.Handoff.Target
		}
		return l.St, l.Role, owner, true
	}
	if g, ok := r.ghosts.get(id); ok {
		return g.St, RoleNone, g.Owner, true
	}
	return EntityState{}, RoleNone, CellID{}, false
}

// nextEpoch 返回本格为新 Real（进入 / 生成）取的 epoch：比本格见过的一切都大，且不小于 hint。
func (r *Region) nextEpoch(id int64, hint uint64) uint64 {
	next := max(hint, 1)
	if s, ok := r.seen.get(id); ok {
		next = max(next, s.Epoch+1)
	}
	if l, ok := r.locals.get(id); ok {
		next = max(next, l.St.Ver.Epoch+1)
	}
	if g, ok := r.ghosts.get(id); ok {
		next = max(next, g.St.Ver.Epoch+1)
	}
	if t, ok := r.tombs.get(id); ok {
		next = max(next, t.Ver.Epoch+1)
	}
	return next
}

// activeCount 是本格 Real + Frozen 的记录数（容量判断）。
func (r *Region) activeCount() int {
	count := 0
	for _, l := range r.locals.m {
		if l.Role == RoleReal || l.Role == RoleFrozen {
			count++
		}
	}
	return count
}
