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
	Reals, Ghosts                                   uint64
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
	reals, ghosts                                   atomic.Uint64
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
		Reals: s.reals.Load(), Ghosts: s.ghosts.Load(),
	}
}
