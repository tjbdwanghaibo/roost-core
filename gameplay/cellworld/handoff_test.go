package cellworld

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 交接状态机在 v2 执行模型（角色是 Nest 实体、交接以事务为边界）下的细粒度用例：v1 handoff_test 的每一条按新模型改写，
// 断言含义不变——同一实体同一时刻至多一个 Real、同一 (实体, epoch) 只取得一次、epoch 严格递增（realChecker 在每次
// 提交后的身份变化时检查），消息丢失 / 重复 / 扣住只推迟结论、不双写。

// waitFor 轮询条件（只用于等异步执行完成，不断言耗时）。
func waitFor(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (w *world) roleOf(cell CellID, id int64) (EntitySnapshot, bool) {
	for _, s := range w.inspect(cell) {
		if s.State.ID == id {
			return s, true
		}
	}
	return EntitySnapshot{}, false
}

func (w *world) sumStats(pick func(NodeStats) uint64) uint64 {
	var total uint64
	for _, p := range w.cells {
		total += pick(p.node.Stats())
	}
	return total
}

// NPC 越过边界：新格成为 Real，旧格（仍在边界带内）留下 owner = 新格的 Ghost。
func TestHandoffMovesRealAndLeavesGhost(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	id := npcID(11)
	w.spawn(a, EntityState{ID: id, X: 9950, Y: 5000, VX: 300, HP: 100})
	waitFor(t, 5*time.Second, "npc real in B", func() bool { s, ok := w.roleOf(b, id); return ok && s.Role == "real" })
	s, ok := w.roleOf(a, id)
	if !ok || s.Role != "ghost" || s.Owner != b {
		t.Fatalf("A holds %+v (known %v), want a ghost owned by B", s, ok)
	}
	if got, _ := w.roleOf(b, id); got.State.Ver.Epoch != 2 {
		t.Fatalf("B real at epoch %d, want 2", got.State.Ver.Epoch)
	}
}

// 迟滞：在边界两侧 ±80（< H=100）来回，不交接。
func TestHandoffHysteresisAbsorbsJitter(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	w.enter(1, spatial.Point{X: 9960, Y: 5000})
	for i := range 6 {
		vx := int64(400)
		if i%2 == 1 {
			vx = -400
		}
		w.move(1, vx, 0)
		w.waitView(1, 5*time.Second, "turn", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10060
			}
			return v.Self.X <= 9960
		})
	}
	w.move(1, 0, 0)
	if started := w.sumStats(func(s NodeStats) uint64 { return s.HandoffsStarted }); started != 0 {
		t.Fatalf("%d handoffs inside the hysteresis band", started)
	}
	if sid, epoch := w.owner(1); sid != w.grid.SID(CellID{0, 0}) || epoch != 1 {
		t.Fatalf("owner sid %d epoch %d, want A epoch 1", sid, epoch)
	}
}

// 前 3 个 Offer 与前 2 个回复丢失，其余格间消息全部重复：交接照样完成，只有一个 Real。
func TestHandoffSurvivesLostAndDuplicatedMessages(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	var offers, replies atomic.Int32
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		switch m.Kind {
		case kindOffer:
			return offers.Add(1) <= 3, true, 0
		case kindOfferReply:
			return replies.Add(1) <= 2, true, 0
		case kindGhost, kindDamage, kindDamageReply:
			return false, true, 0
		}
		return false, false, 0
	})
	w := startWorld(t, spec)
	// 丢 3 个 Offer = 冻结期间多等 3 个 OfferRetry（100ms）：witness 间隔的上界随之放宽。
	scenarioStraightCrossing(t, w, 2, 3*100*time.Millisecond+contMaxGap)
	if got := w.sumStats(func(s NodeStats) uint64 { return s.OfferRetransmits }); got < 3 {
		t.Fatalf("offer retransmits %d, want >= 3", got)
	}
}

// 回复全部丢失：旧格从新格发来的 Ghost（epoch ≥ 交接 epoch）隐式得到结论，不放弃。
func TestHandoffAckLossResolvedByGhostUpdate(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) { return m.Kind == kindOfferReply, false, 0 })
	w := startWorld(t, spec)
	scenarioStraightCrossing(t, w, 2, 300*time.Millisecond)
	if aborted := w.sumStats(func(s NodeStats) uint64 { return s.HandoffsAborted }); aborted != 0 {
		t.Fatalf("%d handoffs aborted although ghosts confirm them", aborted)
	}
	if accepted := w.cells[CellID{0, 0}].node.Stats().HandoffsAccepted; accepted == 0 {
		t.Fatal("A never concluded its handoff")
	}
}

// 超时只删不恢复：目标格收不到 Offer（前 1.2s 全部丢掉）时旧格在 HandoffAbort 后删除记录、告诉 Base；之后 Base 按最后
// 位置重进（新格按快照建出 SceneActor，epoch 更大）；全程至多一个 Real。
func TestHandoffAbortDeletesWithoutResuming(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	var start atomic.Int64
	start.Store(time.Now().UnixNano())
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		return m.Kind == kindOffer && time.Since(time.Unix(0, start.Load())) < 1200*time.Millisecond, false, 0
	})
	spec.cellMod = func(cfg *NodeConfig) { cfg.HandoffAbort = 600 * time.Millisecond }
	w := startWorld(t, spec)
	b := CellID{1, 0}
	w.enter(1, spatial.Point{X: 9950, Y: 5000})
	start.Store(time.Now().UnixNano())
	w.move(1, 1000, 0)
	waitFor(t, 3*time.Second, "abort", func() bool { return w.sumStats(func(s NodeStats) uint64 { return s.HandoffsAborted }) > 0 })
	w.waitView(1, 5*time.Second, "view from B after re-enter", func(v View) bool { return v.Cell == w.grid.SID(b) })
	st := w.baseState(1)
	if st.LostEpoch == 0 || st.Epoch <= st.LostEpoch {
		t.Fatalf("base state %+v: want a lost epoch and a later owner epoch", st)
	}
	if s := w.base.node.Stats(); s.Lost == 0 || s.Reenters == 0 {
		t.Fatalf("base stats %+v, want lost and re-enter", s)
	}
	w.move(1, 0, 0)
	time.Sleep(3 * worldPeriod)
	w.assertSingleReal(actorID(1), b)
}

// 被拒恢复并用下一个 epoch 重试：B 满员（MaxReals 1，已有一个 NPC）时拒绝交接，A 以原 epoch 恢复为 Real，退避后用更大的
// epoch 再试；始终只有 A 一个 Real。
func TestHandoffRejectedResumesAndRetriesWithNextEpoch(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	spec.cellMod = func(cfg *NodeConfig) {
		if cfg.SID == 102 {
			cfg.MaxReals = 1
		}
		cfg.RejectBackoff = 200 * time.Millisecond
	}
	w := startWorld(t, spec)
	a := CellID{0, 0}
	w.spawn(CellID{1, 0}, EntityState{ID: npcID(99), X: 15000, Y: 5000, HP: 100})
	w.enter(1, spatial.Point{X: 9950, Y: 5000})
	w.move(1, 1000, 0)
	waitFor(t, 5*time.Second, "two rejections", func() bool { return w.cells[a].node.Stats().HandoffsRejected >= 2 })
	w.move(1, 0, 0)
	time.Sleep(4 * worldPeriod)
	s, ok := w.roleOf(a, actorID(1))
	if !ok || s.Role != "real" || s.State.Ver.Epoch < 3 {
		t.Fatalf("A holds %+v (known %v), want Real at epoch >= 3 after retries", s, ok)
	}
	w.assertSingleReal(actorID(1), a)
}

// 回复扣住（400ms）时来回越界：冻结窗口与回程交接交错，结论仍只给一个格 Real。
func TestHandoffConcurrentCrossingBack(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		if m.Kind == kindOfferReply {
			return false, false, 400 * time.Millisecond
		}
		return false, false, 0
	})
	w := startWorld(t, spec)
	w.enter(1, spatial.Point{X: 9800, Y: 5000})
	for i := range 6 {
		vx := int64(2000)
		if i%2 == 1 {
			vx = -2000
		}
		w.move(1, vx, 0)
		w.waitView(1, 5*time.Second, "turn", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10250
			}
			return v.Self.X <= 9750
		})
	}
	w.move(1, 0, 0)
	time.Sleep(time.Second)
	sid, _ := w.owner(1)
	cell, _ := w.grid.CellOf(sid)
	w.assertSingleReal(actorID(1), cell)
}

// 斜穿四格的交接链 A → B → D：先沿 +x 进 B，再沿 +y 进 D；epoch 3，D 是唯一 Real。
func TestHandoffChainAcrossCorner(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 2)
	w := startWorld(t, spec)
	d := CellID{1, 1}
	w.enter(1, spatial.Point{X: 9500, Y: 9000})
	w.move(1, 1000, 0)
	w.waitView(1, 5*time.Second, "in B", func(v View) bool { return v.Self.X >= 10500 })
	w.move(1, 0, 1000)
	w.waitView(1, 5*time.Second, "in D", func(v View) bool { return v.Self.Y >= 10500 })
	w.move(1, 0, 0)
	time.Sleep(3 * worldPeriod)
	if sid, epoch := w.owner(1); sid != w.grid.SID(d) || epoch != 3 {
		t.Fatalf("owner sid %d epoch %d, want D epoch 3", sid, epoch)
	}
	w.assertSingleReal(actorID(1), d)
}

// Enter 幂等（重复的 Enter 不造第二个实例、不换 epoch），本格知道 Real 在别处（存活 Ghost）时拒绝并提示 owner。
func TestEnterIsIdempotentAndRefusesWhenRealElsewhere(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) { return false, m.Kind == kindEnter, 0 })
	w := startWorld(t, spec)
	a, b := CellID{0, 0}, CellID{1, 0}
	w.enter(1, spatial.Point{X: 9800, Y: 5000})
	time.Sleep(4 * worldPeriod)
	if sid, epoch := w.owner(1); sid != w.grid.SID(a) || epoch != 1 {
		t.Fatalf("owner sid %d epoch %d after a duplicated enter, want A epoch 1", sid, epoch)
	}
	// 绕过 Base 直接让 B 进入同一个角色：B 有它的存活 Ghost，必须拒绝。
	before := w.base.node.Stats().EnterRefusals
	w.base.node.enqueue(job{kind: jobSend, send: outgoing{sid: w.grid.SID(b), msg: &wireMsg{Kind: kindEnter, Enter: &enterReq{
		State: EntityState{ID: actorID(1), Kind: KindPlayer, X: 10500, Y: 5000, HP: 100}, EpochHint: 5,
	}}}})
	waitFor(t, 3*time.Second, "refusal", func() bool { return w.base.node.Stats().EnterRefusals > before })
	time.Sleep(2 * worldPeriod)
	w.assertSingleReal(actorID(1), a)
}

// Region 的 Ghost 表：只接受更新的版本，删除留墓碑挡住迟到的旧版本，更新的版本让它重新出现。（纯函数用例，不在事务里：
// undo 不登记。）
func TestGhostDropsStaleAndHonoursTombstones(t *testing.T) {
	g := worldGrid(t, 2, 1)
	a := CellID{0, 0}
	n := &Node{cfg: NodeConfig{Grid: g, GhostTTL: time.Second}.withDefaults(), cell: a, hostsCell: true}
	r := NewRegion()
	now := time.Unix(1000, 0)
	id := npcID(5)
	at := func(seq uint64, epoch uint64) EntityState {
		return EntityState{ID: id, Kind: KindNPC, X: 10050, Y: 5000, HP: 100, Ver: Version{Epoch: epoch, Seq: seq}}
	}
	apply := func(b *ghostBatch) { n.applyGhostBatch(r, g.SID(CellID{1, 0}), b, now) }
	apply(&ghostBatch{Updates: []EntityState{at(5, 1)}})
	apply(&ghostBatch{Updates: []EntityState{at(4, 1)}})
	if gr, _ := r.ghosts.get(id); gr.St.Ver.Seq != 5 {
		t.Fatalf("stale update applied: %+v", gr.St.Ver)
	}
	apply(&ghostBatch{Removes: []ghostRemove{{ID: id, Ver: Version{Epoch: 1, Seq: 6}}}})
	if _, ok := r.ghosts.get(id); ok {
		t.Fatal("ghost survived its remove")
	}
	apply(&ghostBatch{Updates: []EntityState{at(6, 1)}})
	if _, ok := r.ghosts.get(id); ok {
		t.Fatal("late update resurrected a removed ghost")
	}
	apply(&ghostBatch{Updates: []EntityState{at(1, 2)}})
	if gr, ok := r.ghosts.get(id); !ok || gr.St.Ver.Epoch != 2 {
		t.Fatalf("newer epoch not applied after the tombstone: %+v %v", gr, ok)
	}
	if n.stats.staleDrops.Load() != 2 {
		t.Fatalf("stale drops %d, want 2", n.stats.staleDrops.Load())
	}
}
