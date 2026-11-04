package cellworld

import "sync/atomic"

// NodeStats 是 Node 的累计计数快照；Reals / Ghosts 是最近一次区域心跳时本格的数量。
type NodeStats struct {
	Ticks, MsgsOut, BytesOut, MsgsIn, BytesIn       uint64
	OutboxDrops, DispatchErrors, DestroyErrors      uint64
	DecodeErrors, PublishErrors                     uint64
	StaleDrops, Conflicts                           uint64
	HandoffsStarted, HandoffsAccepted               uint64
	HandoffsRejected, HandoffsAborted               uint64
	OfferRetransmits, OffersReceived, InputsDropped uint64
	DamageForwarded, DamageRetries, DamageUnknown   uint64
	LeaseExpired                                    uint64
	ViewsAccepted, ViewsStale, OwnerChanges         uint64
	Reenters, InputsResent, EnterRefusals, Lost     uint64
	Writebacks, MirrorErrors                        uint64
	// ActorBeats：执行过的角色 / NPC 格内心跳数。TierIdle / TierFast：NPC 降档 / 升档（已提交）次数。PeriodErrors：换档失败
	// （心跳登记已撤销，实体正在销毁）。IdleNPCs 是最近一次区域心跳时本格慢档的 NPC Real 数。
	ActorBeats, TierIdle, TierFast, PeriodErrors uint64
	Reals, Ghosts, IdleNPCs                      uint64
	// 面板（第 3 项）：Cell 侧 PanelsApplied（真正应用）/ PanelsDuplicate（重复或过期）/ PanelRetries（收到时不是 Real）；
	// Base 侧 PanelsSent（含重发）/ PanelUnknown（超过 PanelDeadline 未确认）。
	PanelsApplied, PanelsDuplicate, PanelRetries uint64
	PanelsSent, PanelUnknown                     uint64
	// 队伍面板（第 4 项）：GroupSnapsSent / GroupSnapsIn（发出 / 收到的队员快照，收到的按版本进 mirror）、GroupNoticesSent /
	// GroupAdopted / GroupStale（成员变化通知：发出、采纳、版本不新或不是同一组而不采纳）、GroupResyncs（请求重读成员表）、
	// GroupViewErrors（组关系视野的 Interest 调用失败，下一拍重设）。
	GroupSnapsSent, GroupSnapsIn               uint64
	GroupNoticesSent, GroupAdopted, GroupStale uint64
	GroupResyncs, GroupViewErrors              uint64
}

type nodeCounters struct {
	ticks, msgsOut, bytesOut, msgsIn, bytesIn       atomic.Uint64
	outboxDrops, dispatchErrors, destroyErrors      atomic.Uint64
	decodeErrors, publishErrors                     atomic.Uint64
	staleDrops, conflicts                           atomic.Uint64
	handoffsStarted, handoffsAccepted               atomic.Uint64
	handoffsRejected, handoffsAborted               atomic.Uint64
	offerRetransmits, offersReceived, inputsDropped atomic.Uint64
	damageForwarded, damageRetries, damageUnknown   atomic.Uint64
	leaseExpired                                    atomic.Uint64
	viewsAccepted, viewsStale, ownerChanges         atomic.Uint64
	reenters, inputsResent, enterRefusals, lost     atomic.Uint64
	writebacks, mirrorErrors                        atomic.Uint64
	actorBeats, tierIdle, tierFast, periodErrors    atomic.Uint64
	reals, ghosts, idleNPCs                         atomic.Uint64
	panelsApplied, panelsDuplicate, panelRetries    atomic.Uint64
	panelsSent, panelUnknown                        atomic.Uint64
	groupSnapsSent, groupSnapsIn                    atomic.Uint64
	groupNoticesSent, groupAdopted, groupStale      atomic.Uint64
	groupResyncs, groupViewErrors                   atomic.Uint64
}

// Stats 返回计数快照，任何 goroutine 可调用。
func (n *Node) Stats() NodeStats {
	s := &n.stats
	return NodeStats{
		Ticks: s.ticks.Load(), MsgsOut: s.msgsOut.Load(), BytesOut: s.bytesOut.Load(), MsgsIn: s.msgsIn.Load(), BytesIn: s.bytesIn.Load(),
		OutboxDrops: s.outboxDrops.Load(), DispatchErrors: s.dispatchErrors.Load(), DestroyErrors: s.destroyErrors.Load(),
		DecodeErrors: s.decodeErrors.Load(), PublishErrors: s.publishErrors.Load(),
		StaleDrops: s.staleDrops.Load(), Conflicts: s.conflicts.Load(),
		HandoffsStarted: s.handoffsStarted.Load(), HandoffsAccepted: s.handoffsAccepted.Load(),
		HandoffsRejected: s.handoffsRejected.Load(), HandoffsAborted: s.handoffsAborted.Load(),
		OfferRetransmits: s.offerRetransmits.Load(), OffersReceived: s.offersReceived.Load(), InputsDropped: s.inputsDropped.Load(),
		DamageForwarded: s.damageForwarded.Load(), DamageRetries: s.damageRetries.Load(), DamageUnknown: s.damageUnknown.Load(),
		LeaseExpired:  s.leaseExpired.Load(),
		ViewsAccepted: s.viewsAccepted.Load(), ViewsStale: s.viewsStale.Load(), OwnerChanges: s.ownerChanges.Load(),
		Reenters: s.reenters.Load(), InputsResent: s.inputsResent.Load(), EnterRefusals: s.enterRefusals.Load(), Lost: s.lost.Load(),
		Writebacks: s.writebacks.Load(), MirrorErrors: s.mirrorErrors.Load(),
		ActorBeats: s.actorBeats.Load(), TierIdle: s.tierIdle.Load(), TierFast: s.tierFast.Load(), PeriodErrors: s.periodErrors.Load(),
		Reals: s.reals.Load(), Ghosts: s.ghosts.Load(), IdleNPCs: s.idleNPCs.Load(),
		PanelsApplied: s.panelsApplied.Load(), PanelsDuplicate: s.panelsDuplicate.Load(), PanelRetries: s.panelRetries.Load(),
		PanelsSent: s.panelsSent.Load(), PanelUnknown: s.panelUnknown.Load(),
		GroupSnapsSent: s.groupSnapsSent.Load(), GroupSnapsIn: s.groupSnapsIn.Load(),
		GroupNoticesSent: s.groupNoticesSent.Load(), GroupAdopted: s.groupAdopted.Load(), GroupStale: s.groupStale.Load(),
		GroupResyncs: s.groupResyncs.Load(), GroupViewErrors: s.groupViewErrors.Load(),
	}
}
