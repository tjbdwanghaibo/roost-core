package cellworld

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
)

// 队伍面板（维护者 2026-11-04 第 4 项 = A）：分区 Scene 里不同 Base 进程上、彼此不在视野里的队友互相看见位置与 HP。
//
//	成员表   权威在 game（demo 是 Remote 托管的 Team 实体）；每个 Base 在 Player 上缓存一份（GroupPlayer，带版本 Ver）。
//	         game 改了成员表就在那笔事务里 SetGroup 自己、NotifyGroup 别人；缓存版本为 0（Base 重启、Player 重新加载）
//	         或每 GroupResyncEvery 经 GroupResync 从权威重读，兜住丢失的通知。
//	快照     每名队员的 Base 心跳每 GroupEvery 把它自己的 SceneActor 状态（本进程 mirror 里最新的一份 witness 里的自己，
//	         带 Real 的版本）发给其他队员所在的 Base（不发给自己的 sid：同 Base 的队员本来就在本进程的 mirror 里）。
//	收件     收到的快照按版本喂本进程的 Sync mirror（policy.Mirrors，保留 GroupHold = 3 个周期，过期即删）。它与 witness
//	         喂的是同一个 mirror、同一个 subject（队友的 SceneActor ID）：队友在视野里时两路按版本取新的，客户端只持有一份。
//	面板     每名玩家一个组关系视野（Interest 的 relation view，NodeConfig.GroupView）：成员是其他队员的 SceneActor，玩家
//	         挂接它，不论距离都订阅队友；在空间视野里的队友与它合并成一份订阅。视野按缓存的成员表在 Base 心跳里重设
//	         （成员表变了的下一拍；幂等，事务回滚了下一拍再设）。
//
// 一致性：成员表是权威的一份带版本的快照，通知只在“同一组、缓存版本非 0、通知版本更新”时采纳（迟到 / 重复的旧通知不改
// 任何东西；版本 0 的缓存只认重读）。快照、通知、重读都是至多一次的消息：丢失由下一个周期、重读兜底，面板最多晚
// GroupResyncEvery 收敛。收件方不核对快照的发送者是否真是本进程某个玩家的队友（原型：集群内互信）；不相干的快照只会
// 进 mirror、没有视野订阅它，GroupHold 后过期。

// Group 是玩家所在的组（队伍）的成员表：Ver 是权威每次改成员表时 +1 的版本（只前进），ID 为 0 表示不在组里。
type Group struct {
	ID      int64         `msgpack:"i"`
	Ver     uint64        `msgpack:"v"`
	Members []GroupMember `msgpack:"m,omitempty"`
}

// GroupMember 是一名成员：Player 实体 ID、它在分区 Scene 里的 SceneActor ID、它的 Base 的 sid（静态归属）。
type GroupMember struct {
	Player int64 `msgpack:"p"`
	Actor  int64 `msgpack:"a"`
	SID    int32 `msgpack:"s"`
}

// GroupPlayer 是参加队伍面板的 Base 玩家：CellGroup / SetCellGroup 读写 Player 上缓存的成员表（经 DAO setter：事务回滚
// 即还原），只在实体自己的 Nest 锁内调用。
type GroupPlayer interface {
	BasePlayer
	CellGroup() Group
	SetCellGroup(Group)
}

// member 返回 player 在 g 里的那一项。
func (g Group) member(player int64) (GroupMember, bool) {
	i := slices.IndexFunc(g.Members, func(m GroupMember) bool { return m.Player == player })
	if i < 0 {
		return GroupMember{}, false
	}
	return g.Members[i], true
}

// SetGroup 在调用方当前的 Player 事务里（game 改成员表的那笔事务，例如 [Team, Player]）写入玩家缓存的成员表（经
// SetCellGroup：回滚即撤销）。提交之后的下一个 Base 心跳按它重设组关系视野、把自己的快照发给新成员的 Base。不发同步请求。
func (n *Node) SetGroup(p GroupPlayer, g Group) {
	p.SetCellGroup(g)
	st := p.BaseState()
	st.GroupSentAt = 0                     // 成员可能变了：下一拍就发，不等一个周期
	st.GroupSyncAt = time.Now().UnixNano() // 刚从权威读到：下一次重读从现在起算
	p.SetBaseState(st)
}

// NotifyGroup 在调用方当前事务提交之后，把成员表 g 发给 g 的成员与 former（这次离开的成员）所在的 Base（含本进程：
// 同 Base 的其他队员也要更新）。收件 Base 上缓存着同一组、版本非 0 且更旧的玩家采纳它，不在 g 里的清空。
func (n *Node) NotifyGroup(g Group, former []GroupMember) {
	to := map[int32][]int64{}
	for _, m := range append(slices.Clone(g.Members), former...) {
		if m.Player != 0 && m.SID != 0 && !slices.Contains(to[m.SID], m.Player) {
			to[m.SID] = append(to[m.SID], m.Player)
		}
	}
	fx := &txEffects{}
	for sid, players := range to {
		n.stats.groupNoticesSent.Add(1)
		fx.send(sid, &wireMsg{Kind: kindGroupNotice, GroupNotice: &groupNotice{Group: g, To: players}})
	}
	n.registerCommit(fx)
}

// onGroupNotice 在收件玩家的 Player 事务里采纳一份成员表通知：只在缓存的是同一组、缓存版本非 0（0 = 成员未知，只认
// 重读）且通知的版本更新时采纳；玩家不在新表里就清空（离队、被解散）。
func (n *Node) onGroupNotice(p GroupPlayer, st *BaseState, g Group) {
	cur := p.CellGroup()
	if cur.ID != g.ID || cur.Ver == 0 || g.Ver <= cur.Ver {
		n.stats.groupStale.Add(1)
		return
	}
	if _, in := g.member(p.ID()); !in {
		g = Group{}
	}
	p.SetCellGroup(g)
	st.GroupSentAt, st.GroupSyncAt = 0, time.Now().UnixNano()
	n.stats.groupAdopted.Add(1)
}

// groupBeat 是 Player 的 Base 心跳里队伍面板的一段（玩家在世界里时）：按缓存的成员表重设组关系视野，需要时请求重读，
// 到周期就把自己的快照发给其他队员的 Base。
func (n *Node) groupBeat(p GroupPlayer, st *BaseState, now time.Time, fx *txEffects) {
	g := p.CellGroup()
	if st.Shown && (g.ID != st.GroupViewID || g.Ver != st.GroupViewVer || n.groupViewDrifted(p.ID(), g)) {
		n.setGroupView(p.ID(), st.Actor, g)
		st.GroupViewID, st.GroupViewVer = g.ID, g.Ver
	}
	n.expireMirrors(now)
	if g.ID == 0 {
		return
	}
	nowNano := now.UnixNano()
	// 重读：成员未知（版本 0）时一个周期一次，平时每 GroupResyncEvery 一次。投递在出站 goroutine 上、只做准入。
	if resync := n.cfg.GroupResync; resync != nil {
		since := nowNano - st.GroupSyncAt
		if (g.Ver == 0 && since >= int64(n.cfg.GroupEvery)) || since >= int64(n.cfg.GroupResyncEvery) {
			st.GroupSyncAt = nowNano
			n.stats.groupResyncs.Add(1)
			group, player := g.ID, p.ID()
			fx.follows = append(fx.follows, job{kind: jobCall, call: func(ctx context.Context) {
				if err := resync(ctx, group, player); err != nil {
					n.stats.dispatchErrors.Add(1)
				}
			}})
		}
	}
	if nowNano-st.GroupSentAt < int64(n.cfg.GroupEvery) {
		return
	}
	self, _, ok := n.cfg.Mirrors.Get(st.Actor)
	if !ok || self.Kind != KindPlayer {
		return // 还没有自己的 witness（刚进入、owner 格崩溃中）：没有可发的状态
	}
	st.GroupSentAt = nowNano
	sent := map[int32]bool{n.cfg.SID: true}
	for _, m := range g.Members {
		if m.Player == p.ID() || sent[m.SID] {
			continue
		}
		sent[m.SID] = true
		n.stats.groupSnapsSent.Add(1)
		fx.send(m.SID, &wireMsg{Kind: kindGroupSnap, GroupSnap: &groupSnap{Group: g.ID, Ver: g.Ver, State: self}})
	}
}

// groupOthers 是 g 里除 playerEntity 之外的队员的 SceneActor（升序）。
func groupOthers(g Group, playerEntity int64) []int64 {
	others := make([]int64, 0, len(g.Members))
	for _, m := range g.Members {
		if m.Player != playerEntity && m.Actor != 0 {
			others = append(others, m.Actor)
		}
	}
	slices.Sort(others)
	return others
}

// groupViewDrifted 报告组关系视野的成员是否与成员表不一致。Interest.Hide 会把目标从所有关系视野里拿掉：队友的 mirror
// 过期删除（对方 Base 停了、队友离开世界）时就是这样，它的快照重新到来、mirror 重新出现时要在这里加回去。
func (n *Node) groupViewDrifted(playerEntity int64, g Group) bool {
	if _, in := g.member(playerEntity); g.ID == 0 || !in {
		return false
	}
	members, ok := n.cfg.Interest.ViewMembers(n.cfg.GroupView(playerEntity))
	return !ok || !slices.Equal(members, groupOthers(g, playerEntity))
}

// setGroupView 按成员表 g 设玩家的组关系视野：不在组里时删掉，否则成员是其他队员的 SceneActor、玩家挂接它。在 handler
// 里直接调用 Interest（只取它的短锁、幂等）：事务回滚时 BaseState 里记的视野版本也回滚，下一拍再设一次。
func (n *Node) setGroupView(playerEntity, actor int64, g Group) {
	if _, in := g.member(playerEntity); g.ID == 0 || !in {
		n.removeGroupView(playerEntity)
		return
	}
	view := n.cfg.GroupView(playerEntity)
	others := groupOthers(g, playerEntity)
	err := n.cfg.Interest.DefineRelationView(view, n.cfg.GroupLayer)
	if errors.Is(err, policy.ErrViewExists) {
		err = nil
	}
	if err == nil {
		err = n.cfg.Interest.SetViewMembers(view, others)
	}
	if err == nil {
		err = n.cfg.Interest.AttachView(n.cfg.Session(actor), view)
	}
	if err != nil {
		n.stats.groupViewErrors.Add(1)
	}
}

// removeGroupView 删掉玩家的组关系视野（没有就什么都不做）：挂接它的玩家不再经它订阅队友。
func (n *Node) removeGroupView(playerEntity int64) {
	if err := n.cfg.Interest.RemoveView(n.cfg.GroupView(playerEntity)); err != nil && !errors.Is(err, policy.ErrViewUnknown) {
		n.stats.groupViewErrors.Add(1)
	}
}

// onGroupSnap 是收到的队员快照（传输回调上，不经 Nest：快照不属于本进程的任何实体，mirror 按版本幂等）：按版本喂
// mirror，保留 GroupHold。
func (n *Node) onGroupSnap(m *groupSnap) {
	s := m.State
	if n.cfg.GroupView == nil || n.cfg.Mirrors == nil || s.ID == 0 || s.Kind != KindPlayer {
		n.stats.decodeErrors.Add(1)
		return
	}
	n.stats.groupSnapsIn.Add(1)
	namespace, kind := n.cfg.MirrorKind(s)
	if _, err := n.cfg.Mirrors.Apply(policy.MirrorItem[EntityState]{
		ID: s.ID, Namespace: namespace, Kind: kind, Version: policy.MirrorVersion{Epoch: s.Ver.Epoch, Seq: s.Ver.Seq},
		Content: s, Hold: n.cfg.GroupHold,
	}, time.Now()); err != nil {
		n.stats.mirrorErrors.Add(1)
	}
}
