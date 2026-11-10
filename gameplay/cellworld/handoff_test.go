package cellworld

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 交接状态机在 v2 执行模型（角色是 Nest 实体、交接以事务为边界）下的细粒度用例：v1 handoff_test 的每一条按新模型改写，
// 断言含义不变——同一实体同一时刻至多一个 Real、同一 (实体, epoch) 只取得一次、epoch 严格递增（realChecker 在每次
// 提交后的身份变化时检查），消息丢失 / 重复 / 扣住只推迟结论、不双写。

// waitFor 轮询条件（只用于等异步执行完成，不断言耗时）；超时没有诊断，需要诊断的用 eventually。
func waitFor(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	eventually(t, timeout, what, func() (bool, string) { return cond(), "" })
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
	waitFor(t, settleWithin, "npc real in B", func() bool { s, ok := w.roleOf(b, id); return ok && s.Role == "real" })
	// B 装入 Real 与 A 收到 OfferReply、由冻结转为 Ghost 是两个格上的两笔事务：B 刚成为 Real 时 A 可能仍是 frozen（协议内
	// 的正常窗口，至多一个 Real 由 realChecker 全程检查）。等 A 收尾，再断言它留下的是 owner = B 的 Ghost。
	waitFor(t, settleWithin, "A leaves its frozen state", func() bool { s, ok := w.roleOf(a, id); return !ok || s.Role != "frozen" })
	s, ok := w.roleOf(a, id)
	if !ok || s.Role != "ghost" || s.Owner != b {
		t.Fatalf("A holds %+v (known %v), want a ghost owned by B", s, ok)
	}
	if got, _ := w.roleOf(b, id); got.State.Ver.Epoch != 2 {
		t.Fatalf("B real at epoch %d, want 2", got.State.Ver.Epoch)
	}
}

// 迟滞：在边界两侧来回（越过边界 60，远小于 H），不交接。
//
// 换向看 witness：看到玩家到了换向点才发下一条移动意图，从那一刻到新速度在格里生效（witness 一跳、测试轮询、输入一跳）
// 玩家照常走；位置按心跳实际经过的时间积分，负载下还可能赶上一拍被夹到上限（max(1s, Period)）的经过时间。所以本用例的
// H 取 900（G 相应取 2000，满足 R + H ≤ G）：换向点离交接线 840，400/s 下约 2.1s，盖过一拍上限（400）加上控制延迟。
// 承诺与 H 的取值无关——越界不超过 H 就不交接；H=100 时换向余量只有 40（100ms），控制延迟一长玩家就真的走出了迟滞带
// （此时交接是正确行为），见 docs/maintenance/evidence/cellworld-realclock-flakes-20261110。
func TestHandoffHysteresisAbsorbsJitter(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	spec.grid = worldGridBand(t, 2, 1, 2000, 900)
	w := startWorld(t, spec)
	w.enter(1, spatial.Point{X: 9960, Y: 5000})
	w.resetViews(1)
	for i := range 6 {
		vx := int64(400)
		if i%2 == 1 {
			vx = -400
		}
		w.move(1, vx, 0)
		w.waitView(1, settleWithin, "turn", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10060
			}
			return v.Self.X <= 9960
		})
	}
	w.move(1, 0, 0)
	if started := w.sumStats(func(s NodeStats) uint64 { return s.HandoffsStarted }); started != 0 {
		// 越过交接线后交接是正确行为：先用 witness 里玩家走到的最远处区分“用例失去控制”与“迟滞带内交接”。
		edge := int64(10000) + w.grid.Hysteresis()
		var peak int64
		for _, v := range w.viewsOf(1) {
			peak = max(peak, v.Self.X)
		}
		if peak >= edge {
			t.Fatalf("%d handoffs, but the player reached x=%d past the hysteresis edge %d: control latency carried it out of the band", started, peak, edge)
		}
		t.Fatalf("%d handoffs inside the hysteresis band (player peaked at x=%d, edge %d)", started, peak, edge)
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
	// 丢 3 个 Offer = 冻结期间多等 3 个 OfferRetry（100ms）：消息段的上界随之加 300ms。
	scenarioStraightCrossing(t, w, 2, gapRule{beats: contGap.beats, edge: contGap.edge, transit: transitCap + 3*100*time.Millisecond})
	if got := w.sumStats(func(s NodeStats) uint64 { return s.OfferRetransmits }); got < 3 {
		t.Fatalf("offer retransmits %d, want >= 3", got)
	}
}

// 回复全部丢失：旧格从新格发来的 Ghost（epoch ≥ 交接 epoch）隐式得到结论，不放弃。
func TestHandoffAckLossResolvedByGhostUpdate(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) { return m.Kind == kindOfferReply, false, 0 })
	w := startWorld(t, spec)
	scenarioStraightCrossing(t, w, 2, contGap) // 回复丢失不影响新格装入，间隔与无故障时相同
	if aborted := w.sumStats(func(s NodeStats) uint64 { return s.HandoffsAborted }); aborted != 0 {
		t.Fatalf("%d handoffs aborted although ghosts confirm them", aborted)
	}
	if accepted := w.cells[CellID{0, 0}].node.Stats().HandoffsAccepted; accepted == 0 {
		t.Fatal("A never concluded its handoff")
	}
}

// 超时只删不恢复：目标格收不到 Offer（第一次放弃之前全部丢掉）时旧格在 HandoffAbort 后删除记录、告诉 Base；之后 Base
// 按最后位置重进（新格按快照建出 SceneActor，epoch 更大）；全程至多一个 Real。
//
// 丢 Offer 的窗口按状态而不是墙钟划定（原来是“前 1.2s”）：负载下冻结 + HandoffAbort 走完可能超过 1.2s，重传的 Offer
// 就会送达、交接成功，用例等不到放弃。现在丢到旧格真的放弃为止，之后的消息（重进）照常。
func TestHandoffAbortDeletesWithoutResuming(t *testing.T) {
	spec, lb := loopbackSpec(t, 2, 1)
	var started atomic.Pointer[world]
	aborted := func() bool {
		w := started.Load()
		return w != nil && w.sumStats(func(s NodeStats) uint64 { return s.HandoffsAborted }) > 0
	}
	lb.setFault(func(_ string, m *wireMsg) (bool, bool, time.Duration) {
		return m.Kind == kindOffer && !aborted(), false, 0
	})
	spec.cellMod = func(cfg *NodeConfig) { cfg.HandoffAbort = 600 * time.Millisecond }
	w := startWorld(t, spec)
	started.Store(w)
	b := CellID{1, 0}
	w.enter(1, spatial.Point{X: 9950, Y: 5000})
	w.move(1, 1000, 0)
	waitFor(t, settleWithin, "abort", aborted)
	w.waitView(1, settleWithin, "view from B after re-enter", func(v View) bool { return v.Cell == w.grid.SID(b) })
	st := w.baseState(1)
	if st.LostEpoch == 0 || st.Epoch <= st.LostEpoch {
		t.Fatalf("base state %+v: want a lost epoch and a later owner epoch", st)
	}
	if s := w.base.node.Stats(); s.Lost == 0 || s.Reenters == 0 {
		t.Fatalf("base stats %+v, want lost and re-enter", s)
	}
	w.move(1, 0, 0)
	w.assertSingleReal(actorID(1), b) // 等到结论成立（有期限）
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
	waitFor(t, settleWithin, "two rejections", func() bool { return w.cells[a].node.Stats().HandoffsRejected >= 2 })
	w.move(1, 0, 0)
	// 停下时可能还有一次重试在路上（冻结中）：等 A 以更大的 epoch 恢复为 Real。
	eventually(t, settleWithin, "A real again after the retries", func() (bool, string) {
		s, ok := w.roleOf(a, actorID(1))
		return ok && s.Role == "real" && s.State.Ver.Epoch >= 3, fmt.Sprintf("A holds %+v (known %v), want Real at epoch >= 3 after retries", s, ok)
	})
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
		w.waitView(1, settleWithin, "turn", func(v View) bool {
			if vx > 0 {
				return v.Self.X >= 10250
			}
			return v.Self.X <= 9750
		})
	}
	w.move(1, 0, 0)
	sid, _ := w.waitSettled(1, settleWithin) // 回复扣住 400ms：停下时最后一次交接还没收尾
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
	w.waitView(1, settleWithin, "in B", func(v View) bool { return v.Self.X >= 10500 })
	w.move(1, 0, 1000)
	w.waitView(1, settleWithin, "in D", func(v View) bool { return v.Self.Y >= 10500 })
	w.move(1, 0, 0)
	// 原来 sleep 3 拍后读 Base 的 owner：Base 换 owner 靠 D 的 witness，负载下 3 拍不一定够。等交接收尾（Base 的 owner、
	// 各格角色与最近的 witness 一致），再断言 owner 是 D、epoch 3。
	if sid, epoch := w.waitSettled(1, settleWithin); sid != w.grid.SID(d) || epoch != 3 {
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
	w.waitBeats(a, 4) // 重复的 Enter 与原件同时投递：再过 4 拍（按 A 实际执行的拍数），它若造出第二个实例或换了 epoch 就看得见
	if sid, epoch := w.owner(1); sid != w.grid.SID(a) || epoch != 1 {
		t.Fatalf("owner sid %d epoch %d after a duplicated enter, want A epoch 1", sid, epoch)
	}
	// 绕过 Base 直接让 B 进入同一个角色：B 有它的存活 Ghost，必须拒绝。
	before := w.base.node.Stats().EnterRefusals
	w.base.node.enqueue(job{kind: jobSend, send: outgoing{sid: w.grid.SID(b), msg: &wireMsg{Kind: kindEnter, Enter: &enterReq{
		State: EntityState{ID: actorID(1), Kind: KindPlayer, X: 10500, Y: 5000, HP: 100}, EpochHint: 5,
	}}}})
	waitFor(t, settleWithin, "refusal", func() bool { return w.base.node.Stats().EnterRefusals > before })
	w.waitBeats(b, 2) // 拒绝之后 B 若仍错误地装入，2 拍内会在区域表里出现（realChecker 也在每次提交后检查）
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

// plainActor 是不在 Nest 事务里、没有心跳的角色替身：直接调用 Node 的 handler 逻辑（onOffer / onEnter）的单元用例用。
type plainActor struct {
	*fentity.EntityBase
	st ActorState
}

func (a *plainActor) Base() *fentity.EntityBase             { return a.EntityBase }
func (a *plainActor) CellState() ActorState                 { return a.st }
func (a *plainActor) SetCellState(st ActorState)            { a.st = st }
func (a *plainActor) StartCell() error                      { return nil }
func (a *plainActor) SetCellPeriod(time.Duration) error     { return nil }
func (a *plainActor) OnDestroy(fentity.EntityDestroyReason) {}

// plainNode 是 cell 格的 Node（不运行：只直接调用积分与 handler 逻辑），CreateActor 建 plainActor 并记在 created 里。
func plainNode(t testing.TB, grid *Grid, cell CellID, created map[int64]*plainActor) *Node {
	n := &Node{cfg: NodeConfig{Grid: grid, Cell: &cell, ViewRadius: worldViewRadius}.withDefaults(), cell: cell, hostsCell: true}
	n.rect = grid.Rect(cell)
	n.cfg.CreateActor = func(id int64, kind EntityKind) (Actor, error) {
		a := &plainActor{EntityBase: fentity.NewEntityBase(id, fentity.EntityCategoryOther, true, kindTActor)}
		created[id] = a
		return a, nil
	}
	return n
}

// 位置零头随交接带走（线上格式 v8）：旧格在越界的那一步冻结，零头写进 Offer，经编码 / 解码到目标格装入时接着用；交接前后
// 合起来的位移与 速度 × 积分时间逐千分位相同（冻结中不积分）。被拒恢复 Real 时零头不动；进场（Enter）从整数位置开始、
// 零头为 0，幂等重进不动已有的零头；线上的坏零头当作 0。
func TestHandoffCarriesTheSubUnitRemainder(t *testing.T) {
	grid := worldGrid(t, 2, 1)
	a, b := CellID{0, 0}, CellID{1, 0}
	created := map[int64]*plainActor{}
	na, nb := plainNode(t, grid, a, created), plainNode(t, grid, b, created)
	now := time.Now()
	const start = 10090
	st := ActorState{EntityState: EntityState{ID: npcID(50), Kind: KindNPC, X: start, Y: 5000, VX: 7, VY: 3, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	var ms int64
	var fx actorEffects
	for st.Role == RoleReal {
		fx = na.stepActor(&st, now, 37*time.Millisecond)
		ms += 37
	}
	if st.Role != RoleFrozen || len(fx.sends) != 1 || fx.sends[0].msg.Kind != kindOffer {
		t.Fatalf("crossing the freeze line: %v, sends %+v", st.Role, fx.sends)
	}
	if st.FracX == 0 || st.FracY == 0 {
		t.Fatalf("froze with remainder (%d, %d); pick a case that leaves one on both axes", st.FracX, st.FracY)
	}
	raw, err := encodeMsg(fx.sends[0].msg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeMsg(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.Offer.FracX != st.FracX || m.Offer.FracY != st.FracY {
		t.Fatalf("offer carries remainder (%d, %d), want (%d, %d)", m.Offer.FracX, m.Offer.FracY, st.FracX, st.FracY)
	}

	// 被拒：同一 epoch 恢复为 Real，零头不动。
	frozen := &plainActor{EntityBase: fentity.NewEntityBase(st.ID, fentity.EntityCategoryOther, true, kindTActor), st: st}
	na.onOfferReply(NewRegion(), frozen, grid.SID(b), &handoffReply{ID: st.ID, Epoch: st.Handoff.Epoch, Accepted: false}, now, &txEffects{})
	if frozen.st.Role != RoleReal || frozen.st.FracX != st.FracX || frozen.st.FracY != st.FracY {
		t.Fatalf("after a refused offer: %v remainder (%d, %d), want real with (%d, %d)", frozen.st.Role, frozen.st.FracX, frozen.st.FracY, st.FracX, st.FracY)
	}

	// 接受：目标格装入，接着积分。
	if err := nb.onOffer(NewRegion(), nil, grid.SID(a), m.Offer, now, &txEffects{}); err != nil {
		t.Fatal(err)
	}
	moved := created[st.ID]
	if moved == nil || moved.st.Role != RoleReal || moved.st.FracX != st.FracX || moved.st.FracY != st.FracY {
		t.Fatalf("installed on (1,0): %+v, want real with remainder (%d, %d)", moved, st.FracX, st.FracY)
	}
	for range 20 {
		nb.stepActor(&moved.st, now, 37*time.Millisecond)
		ms += 37
	}
	if got, want := (moved.st.X-start)*milli+int64(moved.st.FracX), 7*ms; got != want {
		t.Fatalf("x walked %d thousandths across the handoff, want %d (7/s for %dms)", got, want, ms)
	}
	if got, want := (moved.st.Y-5000)*milli+int64(moved.st.FracY), 3*ms; got != want {
		t.Fatalf("y walked %d thousandths across the handoff, want %d", got, want)
	}

	// 坏零头（不在 (-1000, 1000) 里）当作 0。
	bad := *m.Offer
	bad.State.ID, bad.State.Ver.Epoch, bad.FracX, bad.FracY = npcID(51), 2, 1000, -1500
	if err := nb.onOffer(NewRegion(), nil, grid.SID(a), &bad, now, &txEffects{}); err != nil {
		t.Fatal(err)
	}
	if got := created[npcID(51)]; got == nil || got.st.FracX != 0 || got.st.FracY != 0 {
		t.Fatalf("an offer with remainder (1000, -1500) installed %+v, want (0, 0)", got)
	}

	// 进场：从整数位置开始；幂等重进（本格已是 Real）不动零头。
	r := NewRegion()
	enter := &enterReq{State: EntityState{ID: actorID(52), X: 3000, Y: 4000, HP: 100}, Entry: 1}
	if err := na.onEnter(r, nil, 1001, enter, now, &txEffects{}); err != nil {
		t.Fatal(err)
	}
	entered := created[actorID(52)]
	if entered == nil || entered.st.Role != RoleReal || entered.st.FracX != 0 || entered.st.FracY != 0 {
		t.Fatalf("entered: %+v, want real with no remainder", entered)
	}
	entered.st.VX = 10
	na.stepActor(&entered.st, now, 30*time.Millisecond)
	if entered.st.FracX != 300 {
		t.Fatalf("remainder %d after 30ms at 10/s, want 300", entered.st.FracX)
	}
	if err := na.onEnter(r, entered, 1001, enter, now, &txEffects{}); err != nil {
		t.Fatal(err)
	}
	if entered.st.FracX != 300 || entered.st.X != 3000 {
		t.Fatalf("an idempotent enter changed the position to %d %+d", entered.st.X, entered.st.FracX)
	}
}
