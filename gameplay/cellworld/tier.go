package cellworld

import (
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// NPC 心跳分档（维护者 2026-11-03 决定 D1 = B）：附近没有玩家的 NPC 心跳降到 NodeConfig.IdlePeriod，有玩家靠近时回到
// Period。只降格内移动心跳（cell.actor）的档位，不暂停 AI、不停移动；与“无观察者暂停 AI”设计稿
// （FEATURE-2026-10-22-ai-idle-without-observers）是两件事，那份设计稿本轮不实施。
//
// 判据（区域心跳 retier，Scene 锁内，每个 Period 一次）：
//
//   - 观察者 = 本格的玩家 Real / Frozen 与邻格玩家的 Ghost。W + H ≤ G（配置校验）保证离本格 NPC W 以内的邻格玩家一定
//     在本格有 Ghost，所以只看区域表就够，不需要问邻格。
//   - NPC “有人在附近” = 某个观察者离它最近报告的位置不超过 W + drift；drift = (|VX| + |VY|) × 它当前档位周期，补上慢档
//     报告位置的陈旧（慢档 NPC 一个慢拍才报告一次）。
//   - 升档：有人在附近且 NPC 在慢档 → 立刻请求升档。降档：快档 NPC 连续 IdleAfter 没人在附近 → 请求降档（时间滞后，
//     玩家在 W 边上来回时只升一次、不降）。请求经出站队列投给 NPC 自己的 handler（cellworld_tier），区域不锁 NPC；
//     请求丢了，tierRetry 之后再发；NPC 的 handler 幂等（档位已是目标值就不动）。
//
// 档位存在 NPC 的 DAO（EntityState.Idle），随事务回滚，随 Ghost 与交接传出：
//
//   - 慢档一拍按 Period 分步积分、每步判交接（advance）：越界就停在那一步冻结，冻结点离本格至多 H + 一个 Period 的
//     位移，与快档相同，不会一步跨过边界带（交接与旧格留 Ghost 的几何不因降档改变）。
//   - 冻结中一律快档（按 OfferRetry 重传、到 HandoffAbort 放弃都按快档计时），Idle 标记保留：Offer 带着它，新格以慢档
//     装入（不因交接回到快档再等一次 IdleAfter）；新格的区域在下一拍按自己的观察者决定要不要升档。被拒恢复为 Real 时
//     回到慢档。
//   - 邻格的 Ghost：慢档 NPC 一个慢拍才更新一次，Ghost 的过期时间放宽为 GhostTTL + IdlePeriod（按接收方自己的配置；
//     所有进程读同一份分区配置）。
//   - 玩家的 SceneActor 恒为快档。伤害、交接协议、去重与档位无关：慢档只改变心跳的频率。

var hTier = nest.NewHandlerName("cellworld_tier")

// tierRetries 是同一 NPC 两次换档请求之间至少隔几个 Period（请求丢失时重发，不每拍重发）。
const tierRetries = 5

// beatPeriod 是角色当前应有的心跳档位：只有慢档（Idle）的 NPC Real 用 IdlePeriod；冻结中、玩家、未启用降档时一律 Period。
func (n *Node) beatPeriod(st ActorState) time.Duration {
	if n.cfg.IdlePeriod != 0 && st.Idle && st.Kind == KindNPC && st.Role == RoleReal {
		return n.cfg.IdlePeriod
	}
	return n.cfg.Period
}

// setState 写回角色状态；档位（beatPeriod）随之变化时在同一事务里给心跳换档（提交后生效，回滚作废）。换档失败只可能是
// 心跳登记已撤销（实体正在销毁），计数，不让事务失败。
func (n *Node) setState(a Actor, before, after ActorState) {
	a.SetCellState(after)
	if p := n.beatPeriod(after); p != n.beatPeriod(before) {
		if err := a.SetCellPeriod(p); err != nil {
			n.stats.periodErrors.Add(1)
		}
	}
}

// advance 积分 Real 的一拍：把当前档位的周期按 Period 分步，每步之后判交接；某一步越界（且不在被拒后的退避期内）就在
// 那一步冻结、返回 true，剩下的位移不再走（冻结后不积分）。静止的角色只判一次。
func (n *Node) advance(st *ActorState, nowNano int64, fx *actorEffects) bool {
	steps := 1
	if st.VX != 0 || st.VY != 0 {
		steps = int(n.beatPeriod(*st) / n.cfg.Period)
	}
	for range steps {
		n.integrate(st)
		if target, ok := n.cfg.Grid.ShouldHandoff(n.cell, st.Pos()); ok && nowNano >= st.RetryAfter {
			n.freeze(st, target, nowNano, fx)
			return true
		}
	}
	return false
}

// drift 是 NPC 在当前档位一个周期里最多走多远（|VX| + |VY| 是欧氏位移的上界）。
func (n *Node) drift(st EntityState, period time.Duration) int64 {
	return (abs64(st.VX) + abs64(st.VY)) * int64(period) / int64(time.Second)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// retier 是区域心跳里的档位判断（见文件头）：更新每个 NPC Real 的“无人起点”与上次请求时间（区域表，事务内），需要换档
// 的投给 NPC 的 handler（提交后经出站队列）。只在状态变化时写区域表，没人在附近、已经慢档的 NPC 每拍不产生写。
func (n *Node) retier(r *Region, now time.Time, fx *txEffects) {
	if n.cfg.IdlePeriod == 0 {
		return
	}
	nowNano := now.UnixNano()
	near := n.nearPlayers(r)
	retry := int64(tierRetries) * int64(n.cfg.Period)
	var idle uint64
	for id, l := range r.locals.m {
		if l.St.Kind != KindNPC || l.Role != RoleReal {
			continue
		}
		period := n.cfg.Period
		if l.St.Idle {
			idle++
			period = n.cfg.IdlePeriod
		}
		next := l
		switch {
		case near.within(l.St.Pos(), n.cfg.WakeRadius+n.drift(l.St, period)):
			next.QuietSince = 0
			if l.St.Idle && nowNano-l.TierAsked >= retry {
				next.TierAsked = nowNano
				fx.follows = append(fx.follows, job{kind: jobDispatch, handler: hTier, ids: []int64{id}, params: nest.Params{false}})
			}
		case l.St.Idle:
		case l.QuietSince == 0:
			next.QuietSince = nowNano
		case nowNano-l.QuietSince >= int64(n.cfg.IdleAfter) && nowNano-l.TierAsked >= retry:
			next.TierAsked = nowNano
			fx.follows = append(fx.follows, job{kind: jobDispatch, handler: hTier, ids: []int64{id}, params: nest.Params{true}})
		}
		if next.QuietSince != l.QuietSince || next.TierAsked != l.TierAsked {
			r.locals.set(id, next)
		}
	}
	n.stats.idleNPCs.Store(idle)
}

// playerBuckets 是本拍观察者（玩家）位置的分桶：桶边长 size，查询半径不超过 size 时只看周围 3×3 个桶。一拍现建、用完即弃。
type playerBuckets struct {
	size    int64
	buckets map[[2]int64][]spatial.Point
}

// nearPlayers 收集本格的观察者：本格玩家 Real / Frozen 与邻格玩家的 Ghost。桶边长取 W + 本格 NPC 里最大的 drift。
func (n *Node) nearPlayers(r *Region) playerBuckets {
	var points []spatial.Point
	var maxDrift int64
	for _, l := range r.locals.m {
		switch {
		case l.St.Kind == KindPlayer && (l.Role == RoleReal || l.Role == RoleFrozen):
			points = append(points, l.St.Pos())
		case l.St.Kind == KindNPC && l.Role == RoleReal:
			period := n.cfg.Period
			if l.St.Idle {
				period = n.cfg.IdlePeriod
			}
			maxDrift = max(maxDrift, n.drift(l.St, period))
		}
	}
	for id, g := range r.ghosts.m {
		if _, local := r.locals.m[id]; !local && g.St.Kind == KindPlayer {
			points = append(points, g.St.Pos())
		}
	}
	b := playerBuckets{size: n.cfg.WakeRadius + maxDrift}
	if len(points) == 0 {
		return b
	}
	b.buckets = make(map[[2]int64][]spatial.Point, len(points))
	for _, p := range points {
		k := b.key(p)
		b.buckets[k] = append(b.buckets[k], p)
	}
	return b
}

func (b playerBuckets) key(p spatial.Point) [2]int64 {
	return [2]int64{floorDiv(p.X, b.size), floorDiv(p.Y, b.size)}
}

func floorDiv(v, d int64) int64 {
	q := v / d
	if v%d != 0 && v < 0 {
		q--
	}
	return q
}

// within 报告 p 的 radius（≤ size）以内有没有观察者。
func (b playerBuckets) within(p spatial.Point, radius int64) bool {
	if len(b.buckets) == 0 {
		return false
	}
	k := b.key(p)
	for dx := int64(-1); dx <= 1; dx++ {
		for dy := int64(-1); dy <= 1; dy++ {
			for _, q := range b.buckets[[2]int64{k[0] + dx, k[1] + dy}] {
				if spatial.WithinDistance(p, q, radius) {
					return true
				}
			}
		}
	}
	return false
}

// handleTier 是区域的换档请求（只锁 NPC）：NPC 仍是本格 Real 且档位不同才换，版本前进，提交后报告区域。冻结中、已交出、
// 已不在本进程的请求丢弃（区域按下一份报告重新判断）。
func (n *Node) handleTier(es []fentity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
	a := actorAt(es, 0)
	idle, _ := params[0].(bool)
	if a == nil {
		return nil, nil
	}
	before := a.CellState()
	if before.Kind != KindNPC || before.Role != RoleReal || before.Idle == idle {
		return nil, nil
	}
	st := before
	st.Idle = idle
	st.Ver.Seq++
	n.setState(a, before, st)
	counter := &n.stats.tierFast
	if idle {
		counter = &n.stats.tierIdle
	}
	fx := &txEffects{reports: []ActorReport{{State: st.EntityState, Role: st.Role}}}
	fx.hooks = append(fx.hooks, func() { counter.Add(1) })
	n.registerCommit(fx)
	return nil, nil
}
