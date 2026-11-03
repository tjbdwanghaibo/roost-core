package cellworld

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func (h *harness) attack(at CellID, attacker, target int64, seq uint64, amount int32) HitID {
	hit := HitID{Attacker: attacker, Seq: seq}
	h.cell(at).handle(&wireMsg{Kind: kindAttack, From: testBaseSID, Attack: &attackReq{Hit: hit, Target: target, Amount: amount}}, h.now)
	return hit
}

func (h *harness) results(hit HitID) []*attackResultMsg {
	var out []*attackResultMsg
	for _, m := range h.baseMsgs(kindAttackResult) {
		if m.AttackResult.Hit == hit {
			out = append(out, m.AttackResult)
		}
	}
	return out
}

func (h *harness) hp(id int64) int32 {
	_, e, ok := h.realAt(id)
	if !ok {
		h.t.Fatalf("entity %d has no Real", id)
	}
	return e.st.HP
}

// A 区玩家打 B 区边界带内的目标：A 用 Ghost 判距离并转发给 B 结算；所有消息重复投递、同一攻击再发一次，
// 伤害只结算一次，重复请求得到“已结算（重复）”。
func TestDamageAcrossBoundaryAppliedOnce(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 960, Y: 500}, 100)
	h.spawn(cellB, EntityState{ID: 7, X: 1030, Y: 500, HP: 100})
	h.step(nil) // 互发 Ghost
	if got := h.roles(7); got[cellA] != "ghost" {
		t.Fatalf("target roles = %v, want ghost in A", got)
	}
	hit := h.attack(cellA, 1, 7, 1, 10)
	h.pump(func(m netMsg) verdict { // Cell 之间的消息全部重复；发给 Base 的不重复，便于数结论条数
		if m.to == testBaseSID {
			return pass
		}
		return dup
	})
	if hp := h.hp(7); hp != 90 {
		t.Fatalf("hp = %d, want 90", hp)
	}
	res := h.results(hit)
	if len(res) != 1 || res[0].Outcome != OutcomeApplied || res[0].Duplicate {
		t.Fatalf("results = %+v, want one applied", res)
	}
	h.attack(cellA, 1, 7, 1, 10) // Base 重发同一攻击
	h.pump(nil)
	if hp := h.hp(7); hp != 90 || h.hits[hit] != 1 {
		t.Fatalf("hp = %d, applications = %d after duplicate attack", hp, h.hits[hit])
	}
	res = h.results(hit)
	if len(res) != 2 || !res[1].Duplicate {
		t.Fatalf("results = %+v, want second marked duplicate", res)
	}
}

// 超出距离与看不见的目标：明确拒绝，不转发。
func TestDamageRejectedOutOfRangeOrInvisible(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 900, Y: 500}, 100)
	h.spawn(cellB, EntityState{ID: 7, X: 1050, Y: 500, HP: 100})
	h.step(nil)
	far := h.attack(cellA, 1, 7, 1, 10)
	unknown := h.attack(cellA, 1, 99, 2, 10)
	h.pump(nil)
	if r := h.results(far); len(r) != 1 || r[0].Outcome != OutcomeRejected || r[0].Reason != ReasonOutOfRange {
		t.Fatalf("far = %+v", r)
	}
	if r := h.results(unknown); len(r) != 1 || r[0].Reason != ReasonNotVisible {
		t.Fatalf("unknown = %+v", r)
	}
	if s := h.cell(cellA).Stats(); s.DamageForwarded != 0 {
		t.Fatalf("forwarded %d", s.DamageForwarded)
	}
}

// 目标在攻击途中迁移：B 的 Offer 扣住时，A（攻击者所在格，也是交接目标）只有 Ghost，B 已冻结只回“重试”；
// Offer 放行、A 安装后下一次重试在本格结算。只结算一次。
func TestDamageWhileTargetMigratesRetriesToNewOwner(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 960, Y: 500}, 100)
	h.spawn(cellB, EntityState{ID: 7, X: 1010, Y: 520, VX: -400, HP: 100}) // 向 A 走
	holdOffers := func(m netMsg) verdict {
		if m.msg.Kind == kindOffer {
			return hold
		}
		return pass
	}
	h.step(holdOffers) // x=990（B 的迟滞内）
	h.step(holdOffers) // x=970：B 冻结，Offer 扣住
	if got := h.roles(7); got[cellB] != "frozen" || got[cellA] != "ghost" {
		t.Fatalf("roles = %v, want B frozen, A ghost", got)
	}
	hit := h.attack(cellA, 1, 7, 1, 10)
	for range 4 {
		h.step(holdOffers) // 来回“重试”
	}
	if h.hits[hit] != 0 || len(h.results(hit)) != 0 {
		t.Fatal("damage settled while the target was frozen")
	}
	h.release()
	h.pump(nil)
	for range 3 {
		h.step(nil)
	}
	if at, _, _ := h.realAt(7); at != cellA {
		t.Fatalf("target Real at %v, want A", at)
	}
	res := h.results(hit)
	if len(res) != 1 || res[0].Outcome != OutcomeApplied || h.hits[hit] != 1 || h.hp(7) != 90 {
		t.Fatalf("results = %+v, applications = %d, hp = %d", res, h.hits[hit], h.hp(7))
	}
	if s := h.cell(cellA).Stats(); s.DamageRetries == 0 {
		t.Fatal("expected retries while target was frozen")
	}
}

// B 结算后回复丢失，随后目标迁到 A：去重表随交接带走，A 的重试在新 owner 上命中去重，报“已结算（重复）”，HP 只扣一次。
func TestDamageDedupTravelsWithHandoff(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 960, Y: 500}, 100)
	h.spawn(cellB, EntityState{ID: 7, X: 1030, Y: 500, HP: 100})
	h.step(nil)
	hit := h.attack(cellA, 1, 7, 1, 10)
	firstReply := true
	h.pump(func(m netMsg) verdict {
		if m.msg.Kind == kindDamageReply && firstReply {
			firstReply = false
			return drop
		}
		return pass
	})
	if h.hits[hit] != 1 || len(h.results(hit)) != 0 {
		t.Fatalf("applications = %d results = %d before handoff", h.hits[hit], len(h.results(hit)))
	}
	// 目标往 A 走：1030 → 970 越出 B 的迟滞。
	h.cell(cellB).ents[7].st.VX = -400
	dropDamage := func(m netMsg) verdict { // 交接完成前 A 的重试都丢掉，保证重试落在新 owner 上
		if m.msg.Kind == kindDamage {
			return drop
		}
		return pass
	}
	for range 3 {
		h.step(dropDamage)
	}
	if at, _, _ := h.realAt(7); at != cellA {
		t.Fatalf("target Real at %v, want A", at)
	}
	for range 6 {
		h.step(nil)
	}
	res := h.results(hit)
	if len(res) != 1 || res[0].Outcome != OutcomeApplied || !res[0].Duplicate {
		t.Fatalf("results = %+v, want one applied duplicate", res)
	}
	if h.hits[hit] != 1 || h.hp(7) != 90 {
		t.Fatalf("applications = %d hp = %d", h.hits[hit], h.hp(7))
	}
}

// 回复一直丢：期限到报“结果未知”；它确实已经结算过（Unknown 不能当成没结算）。
func TestDamageUnknownAfterDeadline(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), nil)
	h.enterPlayer(1, spatial.Point{X: 960, Y: 500}, 100)
	h.spawn(cellB, EntityState{ID: 7, X: 1030, Y: 500, HP: 100})
	h.step(nil)
	hit := h.attack(cellA, 1, 7, 1, 10)
	noReplies := func(m netMsg) verdict {
		if m.msg.Kind == kindDamageReply {
			return drop
		}
		return pass
	}
	for range 45 { // 2.25s > AttackDeadline 2s
		h.step(noReplies)
	}
	res := h.results(hit)
	if len(res) != 1 || res[0].Outcome != OutcomeUnknown {
		t.Fatalf("results = %+v, want one unknown", res)
	}
	if h.hits[hit] != 1 || h.hp(7) != 90 {
		t.Fatalf("applications = %d hp = %d, want applied exactly once", h.hits[hit], h.hp(7))
	}
}

// 去重表满（窗口内同一目标挨打超过上限）时拒绝新伤害，而不是淘汰旧记录——淘汰会让迟到的重试被再结算一次。
func TestDamageBusyWhenDedupTableFull(t *testing.T) {
	h := newHarness(t, testGrid1x2(t), func(cfg *CellConfig) { cfg.MaxAppliedHits = 2 })
	h.enterPlayer(1, spatial.Point{X: 960, Y: 500}, 100)
	h.spawn(cellA, EntityState{ID: 7, X: 970, Y: 500, HP: 100})
	h.attack(cellA, 1, 7, 1, 10)
	h.attack(cellA, 1, 7, 2, 10)
	third := h.attack(cellA, 1, 7, 3, 10)
	h.pump(nil)
	if r := h.results(third); len(r) != 1 || r[0].Reason != ReasonBusy {
		t.Fatalf("third = %+v, want busy", r)
	}
	if hp := h.hp(7); hp != 80 {
		t.Fatalf("hp = %d", hp)
	}
}
