package cellworld

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

var (
	cellA = CellID{0, 0}
	cellB = CellID{1, 0}
)

// 正常交接：越出本格超过迟滞才交接；交接后 B 是唯一 Real（epoch+1），A 留 Ghost；离开 A 的边界带后 A 的 Ghost 被删除。
func TestHandoffMovesRealAndLeavesGhost(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.spawn(cellA, EntityState{ID: 7, X: 980, Y: 500, VX: 400, HP: 100})

	h.step(nil) // x=1000：已在 B 的格里，但在迟滞内，仍由 A 负责并向 B 发 Ghost
	if got := h.roles(7); got[cellA] != "real" || got[cellB] != "ghost" {
		t.Fatalf("after first step roles = %v, want A real, B ghost", got)
	}
	h.step(nil) // x=1020：越出迟滞，交接
	at, e, ok := h.realAt(7)
	if !ok || at != cellB || e.st.Ver.Epoch != 2 {
		t.Fatalf("Real after crossing = %v %+v %v, want B at epoch 2", at, e, ok)
	}
	if got := h.roles(7); got[cellA] != "ghost" {
		t.Fatalf("A role after handoff = %v, want ghost", got)
	}
	if s := h.cell(cellA).Stats(); s.HandoffsStarted != 1 || s.HandoffsAccepted != 1 {
		t.Fatalf("A stats = %+v", s)
	}
	for range 6 { // 走到 x=1140，超出 A 的边界带（x < 1100）
		h.step(nil)
	}
	if got := h.roles(7); len(got) != 1 || got[cellB] != "real" {
		t.Fatalf("roles after leaving A's band = %v, want only B real", got)
	}
}

// 迟滞：在边界两侧来回抖动、幅度小于迟滞时不交接。
func TestHandoffHysteresisAbsorbsJitter(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 995, Y: 500}, 100)
	seq := uint64(0)
	input := func(at CellID, vx int64) {
		seq++
		h.cell(at).handle(&wireMsg{Kind: kindInput, From: testBaseSID, Input: &inputReq{ID: 1, Seq: seq, VX: vx}}, h.now)
	}
	for i := range 20 {
		vx := int64(200) // 每拍 10
		if i%2 == 1 {
			vx = -200
		}
		at, _, _ := h.realAt(1)
		input(at, vx)
		h.step(nil)
	}
	if s := h.cell(cellA).Stats(); s.HandoffsStarted != 0 {
		t.Fatalf("jitter within hysteresis started %d handoffs", s.HandoffsStarted)
	}
}

// 丢失与重复：前 3 次 Offer 丢失、前 2 次回复丢失、其余全部重复投递，Ghost 也丢（排除隐式确认）。
// 结论只推迟，不双写：B 只安装一次，任一时刻至多一个 Real。
func TestHandoffSurvivesLostAndDuplicatedMessages(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.spawn(cellA, EntityState{ID: 7, X: 980, Y: 500, VX: 400, HP: 100})
	offers, replies := 0, 0
	filter := func(m netMsg) verdict {
		switch m.msg.Kind {
		case kindOffer:
			offers++
			if offers <= 3 {
				return drop
			}
		case kindOfferReply:
			replies++
			if replies <= 2 {
				return drop
			}
		case kindGhost:
			if m.msg.From == h.grid.SID(cellB) {
				return drop
			}
		}
		return dup
	}
	for range 20 {
		h.step(filter)
	}
	at, e, ok := h.realAt(7)
	if !ok || at != cellB || e.st.Ver.Epoch != 2 {
		t.Fatalf("Real = %v %+v %v, want B epoch 2", at, e, ok)
	}
	if n := h.real.acquisitions(cellB); n != 1 {
		t.Fatalf("B acquired Real %d times, want 1", n)
	}
	s := h.cell(cellA).Stats()
	if s.HandoffsAccepted != 1 || s.OfferRetransmits < 3 {
		t.Fatalf("A stats = %+v, want 1 accepted and >= 3 retransmits", s)
	}
}

// 回复全部丢失时，B 发来的 epoch ≥ 交接 epoch 的 Ghost 更新就是确认（只有新 Real 能发出它）。
func TestHandoffAckLossResolvedByGhostUpdate(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.spawn(cellA, EntityState{ID: 7, X: 980, Y: 500, VX: 400, HP: 100})
	noReplies := func(m netMsg) verdict {
		if m.msg.Kind == kindOfferReply {
			return drop
		}
		return pass
	}
	for range 4 {
		h.step(noReplies)
	}
	if got := h.roles(7); got[cellA] != "ghost" || got[cellB] != "real" {
		t.Fatalf("roles = %v, want A ghost (implicitly acknowledged), B real", got)
	}
	if s := h.cell(cellA).Stats(); s.HandoffsAccepted != 1 || s.HandoffsAborted != 0 {
		t.Fatalf("A stats = %+v", s)
	}
}

// 超时：Offer 一直到不了，A 到 HandoffAbort 删除记录（不恢复为 Real）并通知 Base；扣住的 Offer 之后才到，
// B 安装——仍然只有一个 Real；Base 按最后位置重进时 B 幂等返回 owner，不产生第二个 Real。
func TestHandoffAbortDeletesWithoutResuming(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 980, Y: 500}, 100)
	h.cell(cellA).handle(&wireMsg{Kind: kindInput, From: testBaseSID, Input: &inputReq{ID: 1, Seq: 1, VX: 400}}, h.now)
	first := true
	filter := func(m netMsg) verdict {
		if m.msg.Kind == kindOffer {
			if first {
				first = false
				return hold
			}
			return drop
		}
		return pass
	}
	for range 70 { // 3.5s > HandoffAbort 3s
		h.step(filter)
	}
	if got := h.roles(1); len(got) != 0 {
		t.Fatalf("roles after abort = %v, want none", got)
	}
	lost := h.baseMsgs(kindLost)
	if len(lost) != 1 || lost[0].Lost.State.Ver.Epoch != 2 {
		t.Fatalf("lost notices = %d, want one at epoch 2", len(lost))
	}
	if s := h.cell(cellA).Stats(); s.HandoffsAborted != 1 {
		t.Fatalf("A stats = %+v", s)
	}
	h.release() // 迟到的 Offer
	h.pump(nil)
	at, e, ok := h.realAt(1)
	if !ok || at != cellB || e.st.Ver.Epoch != 2 {
		t.Fatalf("after late offer Real = %v %v, want B epoch 2", at, ok)
	}
	// Base 收到 Lost 后按最后位置重进（epoch 提示 = 3）：B 已是 Real，幂等。
	last := lost[0].Lost.State
	h.cell(cellB).handle(&wireMsg{Kind: kindEnter, From: testBaseSID, Enter: &enterReq{State: EntityState{ID: 1, X: last.X, Y: last.Y, HP: last.HP}, EpochHint: 3}}, h.now)
	h.pump(nil)
	if n := h.real.acquisitions(cellB); n != 1 {
		t.Fatalf("B acquired %d times, want 1", n)
	}
}

// 超时且 Offer 真的丢了：没有 Real；Base 重进按 epoch 提示建新 Real，epoch 仍单调。
func TestHandoffAbortThenReenterCreatesNextEpoch(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 980, Y: 500}, 100)
	h.cell(cellA).handle(&wireMsg{Kind: kindInput, From: testBaseSID, Input: &inputReq{ID: 1, Seq: 1, VX: 400}}, h.now)
	noOffers := func(m netMsg) verdict {
		if m.msg.Kind == kindOffer {
			return drop
		}
		return pass
	}
	for range 70 {
		h.step(noOffers)
	}
	if _, _, ok := h.realAt(1); ok {
		t.Fatal("Real survived abort")
	}
	last := h.baseMsgs(kindLost)[0].Lost.State
	h.cell(cellB).handle(&wireMsg{Kind: kindEnter, From: testBaseSID, Enter: &enterReq{State: EntityState{ID: 1, X: last.X, Y: last.Y, HP: last.HP}, EpochHint: last.Ver.Epoch + 1}}, h.now)
	h.pump(nil)
	at, e, ok := h.realAt(1)
	if !ok || at != cellB || e.st.Ver.Epoch != 3 {
		t.Fatalf("after re-enter Real = %v %v, want B epoch 3", at, ok)
	}
}

// 拒绝：B 满了就拒绝，A 以交接 epoch 恢复为 Real、退避后用下一个 epoch 再试；重复投递旧 Offer 不会让 B 改口安装。
func TestHandoffRejectedResumesAndRetriesWithNextEpoch(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), func(cfg *CellConfig) {
		if cfg.Cell == cellB {
			cfg.MaxReals = 1
		}
	})
	h.spawn(cellB, EntityState{ID: 50, X: 1500, Y: 500, HP: 100}) // 占满 B
	h.spawn(cellA, EntityState{ID: 7, X: 980, Y: 500, VX: 400, HP: 100})
	var firstOffer []netMsg
	capture := func(m netMsg) verdict {
		if m.msg.Kind == kindOffer && len(firstOffer) == 0 {
			firstOffer = append(firstOffer, m)
		}
		return pass
	}
	h.step(capture)
	h.step(capture) // 交接 → 被拒 → 恢复
	at, e, ok := h.realAt(7)
	if !ok || at != cellA || e.st.Ver.Epoch != 2 {
		t.Fatalf("after reject Real = %v %v, want A epoch 2", at, ok)
	}
	h.deliver(firstOffer) // 重复的旧 Offer
	h.pump(nil)
	if at, _, _ := h.realAt(7); at != cellA {
		t.Fatalf("duplicate stale offer moved Real to %v", at)
	}
	for range 12 { // 退避 500ms 之后再试
		h.step(nil)
	}
	_, e, _ = h.realAt(7)
	if e.st.Ver.Epoch != 3 {
		t.Fatalf("epoch after second attempt = %d, want 3", e.st.Ver.Epoch)
	}
	if s := h.cell(cellA).Stats(); s.HandoffsRejected != 2 {
		t.Fatalf("A stats = %+v, want 2 rejections", s)
	}
	if n := h.real.acquisitions(cellB); n != 1 { // 只有 NPC 50
		t.Fatalf("B acquired %d Reals, want 1", n)
	}
}

// 并发两次越界（来回）：A→B 的回复被扣住、B 的 Ghost 也到不了 A，玩家又走回 A。B 发来 epoch 3 的 Offer
// 证明 B 接受过 epoch 2：A 先完成 2、再安装 3；迟到的回复 2 被忽略。
func TestHandoffConcurrentCrossingBack(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 980, Y: 500}, 100)
	h.cell(cellA).handle(&wireMsg{Kind: kindInput, From: testBaseSID, Input: &inputReq{ID: 1, Seq: 1, VX: 400}}, h.now)
	firstReply := true
	filter := func(m netMsg) verdict {
		switch {
		case m.msg.Kind == kindOfferReply && m.to == h.grid.SID(cellA) && firstReply:
			firstReply = false
			return hold
		case m.msg.Kind == kindOfferReply && m.to == h.grid.SID(cellA):
			return drop // A 的重传得到的回复也丢，只剩 B 的 Offer 能让 A 得到结论
		case m.msg.Kind == kindGhost && m.msg.From == h.grid.SID(cellB):
			return drop
		}
		return pass
	}
	h.step(filter)
	h.step(filter) // x=1020：A→B
	if at, _, _ := h.realAt(1); at != cellB {
		t.Fatalf("Real = %v, want B", at)
	}
	h.cell(cellB).handle(&wireMsg{Kind: kindInput, From: testBaseSID, Input: &inputReq{ID: 1, Seq: 2, VX: -400}}, h.now)
	for range 3 { // 1020 → 960，越出 B 的迟滞，B→A
		h.step(filter)
	}
	at, e, ok := h.realAt(1)
	if !ok || at != cellA || e.st.Ver.Epoch != 3 {
		t.Fatalf("Real after crossing back = %v %v, want A epoch 3", at, ok)
	}
	h.release()
	h.pump(nil)
	if at, e, _ := h.realAt(1); at != cellA || e.st.Ver.Epoch != 3 {
		t.Fatalf("late reply changed Real to %v", at)
	}
	if got := h.roles(1); got[cellB] != "ghost" {
		t.Fatalf("B role = %v, want ghost", got)
	}
}

// 斜穿四格交点、交接链 A→B→D：A 的第一次回复被扣住、B 的 Ghost 到不了 A；A 的重传到达时 B 已冻结交给 D
// （epoch 3 ≥ 2），B 回“已接受”，A 得到结论。最终 D 是唯一 Real，epoch 3。
func TestHandoffChainAcrossCorner(t *testing.T) {
	g := testGrid2x2(t)
	h := newHarness(t, g, nil)
	a, b, d := CellID{0, 0}, CellID{1, 0}, CellID{1, 1}
	h.enterPlayer(1, spatial.Point{X: 980, Y: 960}, 100)
	h.cell(a).handle(&wireMsg{Kind: kindInput, From: testBaseSID, Input: &inputReq{ID: 1, Seq: 1, VX: 400}}, h.now)
	// 发往 A 的 Ghost 全丢（排除隐式确认）；B 冻结交给 D 之前发往 A 的回复也全丢（第一条扣住）。
	firstReply, blockReplies := true, true
	filter := func(m netMsg) verdict {
		switch {
		case m.msg.Kind == kindGhost && m.to == g.SID(a):
			return drop
		case m.msg.Kind == kindOfferReply && m.to == g.SID(a) && blockReplies:
			if firstReply {
				firstReply = false
				return hold
			}
			return drop
		}
		return pass
	}
	h.step(filter)
	h.step(filter)
	if at, _, _ := h.realAt(1); at != b {
		t.Fatalf("Real = %v, want B", at)
	}
	h.cell(b).handle(&wireMsg{Kind: kindInput, From: testBaseSID, Input: &inputReq{ID: 1, Seq: 2, VY: 400}}, h.now)
	for range 3 { // y 960 → 1020：B→D
		h.step(filter)
	}
	if got := h.roles(1); got[a] != "frozen" || got[d] != "real" {
		t.Fatalf("roles = %v, want A still frozen at epoch 2 and D real", got)
	}
	blockReplies = false
	for range 4 { // A 的重传到 B：B 记录的 epoch 3 > 2，回“已接受”
		h.step(filter)
	}
	at, e, ok := h.realAt(1)
	if !ok || at != d || e.st.Ver.Epoch != 3 {
		t.Fatalf("Real = %v %v, want D epoch 3", at, ok)
	}
	if got := h.roles(1); got[a] == "frozen" || got[b] == "frozen" {
		t.Fatalf("handoff left frozen records: %v", got)
	}
	h.release()
	h.pump(nil)
	if at, _, _ := h.realAt(1); at != d {
		t.Fatalf("late reply moved Real to %v", at)
	}
}

// 进入：本格已是 Real 时幂等；本格只有存活 Ghost（Real 在别处）时拒绝并提示 owner。
func TestEnterIsIdempotentAndRefusesWhenRealElsewhere(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 1010, Y: 500}, 100) // 在 B
	h.step(nil)                                           // B 向 A 发 Ghost
	h.enterPlayer(1, spatial.Point{X: 1010, Y: 500}, 100) // 重复进入
	if n := h.real.acquisitions(cellB); n != 1 {
		t.Fatalf("duplicate enter acquired %d times", n)
	}
	// 发给 A（A 有存活 Ghost）：拒绝并提示 B。
	h.cell(cellA).handle(&wireMsg{Kind: kindEnter, From: testBaseSID, Enter: &enterReq{State: EntityState{ID: 1, X: 990, Y: 500, HP: 100}, EpochHint: 2}}, h.now)
	h.pump(nil)
	refused := h.baseMsgs(kindEnterRefused)
	if len(refused) != 1 || refused[0].EnterRefused.Hint != h.grid.SID(cellB) {
		t.Fatalf("refusals = %+v, want one hinting B", refused)
	}
	if n := h.real.acquisitions(cellA); n != 0 {
		t.Fatalf("A acquired a second Real")
	}
}

// Ghost 版本序：旧版本（含重复）丢弃，删除留墓碑，旧更新不能让被删的 Ghost 复活；更新的版本可以重建。
func TestGhostDropsStaleAndHonoursTombstones(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	a := h.cell(cellA)
	from := h.grid.SID(cellB)
	ghost := func(ver Version, x int64) *wireMsg {
		return &wireMsg{Kind: kindGhost, From: from, Ghost: &ghostBatch{Updates: []EntityState{{ID: 9, X: x, Y: 500, HP: 10, Ver: ver}}}}
	}
	a.handle(ghost(Version{1, 5}, 1050), h.now)
	a.handle(ghost(Version{1, 4}, 1060), h.now) // 旧
	a.handle(ghost(Version{1, 5}, 1070), h.now) // 重复
	if x := a.ents[9].st.X; x != 1050 {
		t.Fatalf("stale ghost applied: x=%d", x)
	}
	a.handle(&wireMsg{Kind: kindGhost, From: from, Ghost: &ghostBatch{Removes: []ghostRemove{{ID: 9, Ver: Version{1, 6}}}}}, h.now)
	a.handle(ghost(Version{1, 5}, 1080), h.now) // 删除前的旧更新迟到
	if _, ok := a.ents[9]; ok {
		t.Fatal("stale update resurrected a removed ghost")
	}
	a.handle(ghost(Version{2, 1}, 1090), h.now) // 新 epoch 回到边界带
	if e, ok := a.ents[9]; !ok || e.st.X != 1090 {
		t.Fatal("newer ghost not re-created")
	}
	if s := a.Stats(); s.StaleDrops != 3 {
		t.Fatalf("stale drops = %d, want 3", s.StaleDrops)
	}
}
