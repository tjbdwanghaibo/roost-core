package cellworld

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func replicaNodes() (*Node, *Node) {
	makeNode := func(sid int32) *Node {
		n := &Node{cfg: NodeConfig{SID: sid, ReplicaCacheSize: 1024, MaxViewEntities: 50, WitnessBatch: 16, ReplicaRefresh: 250 * time.Millisecond}}
		n.sentReplica.init()
		n.receivedReplica.init([]int32{101, 102, 201})
		return n
	}
	return makeNode(101), makeNode(201)
}
func replicaWire(t *testing.T, from, to *Node, m *wireMsg, now time.Time) *wireMsg {
	t.Helper()
	m.From = from.cfg.SID
	raw, err := encodeMsg(from.encodeReplica(to.cfg.SID, m, now))
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeMsg(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestReplicaDeltaClearsZeroFieldsAndRejectsGaps(t *testing.T) {
	a, b := replicaNodes()
	now := time.Now()
	s := EntityState{ID: 1, Kind: KindNPC, Ver: Version{Epoch: 1, Seq: 1}, X: 20, Y: 30, VX: 40, VY: 50, HP: 60, Idle: true, Panel: Panel{MaxHP: 70}}
	message := func(s EntityState) *wireMsg {
		return &wireMsg{Kind: kindGhost, Ghost: &ghostBatch{Updates: []EntityState{s}}}
	}
	first := replicaWire(t, a, b, message(s), now)
	if _, err := b.decodeReplica(first); err != nil {
		t.Fatal(err)
	}
	zero := EntityState{ID: 1, Kind: KindNPC, Ver: Version{Epoch: 1, Seq: 2}}
	delta := replicaWire(t, a, b, message(zero), now.Add(time.Millisecond))
	if len(delta.Deltas) != 1 || delta.Deltas[0].Full != nil {
		t.Fatal("expected field delta")
	}
	got, err := b.decodeReplica(delta)
	if err != nil || got[0].Ghost.Updates[0] != zero {
		t.Fatalf("zero delta: %+v %v", got, err)
	}
	lost := zero
	lost.Ver.Seq = 3
	lost.X = 99
	_ = replicaWire(t, a, b, message(lost), now.Add(2*time.Millisecond))
	next := lost
	next.Ver.Seq = 4
	next.Y = 88
	gap := replicaWire(t, a, b, message(next), now.Add(3*time.Millisecond))
	if _, err = b.decodeReplica(gap); !errors.Is(err, errReplicaGap) {
		t.Fatalf("gap=%v", err)
	}
	repaired := replicaWire(t, a, b, message(next), now.Add(time.Second))
	got, err = b.decodeReplica(repaired)
	if err != nil || got[0].Ghost.Updates[0] != next {
		t.Fatalf("repair: %+v %v", got, err)
	}
	if _, err = b.decodeReplica(first); err != nil {
		t.Fatal(err)
	}
	key := replicaKey{a.cfg.SID, kindGhost, 1}
	if b.receivedReplica.peer(a.cfg.SID).states[key].state != next {
		t.Fatal("old full regressed the decoding baseline")
	}
}
func TestWitnessMembershipDeltaAndSharedEncoding(t *testing.T) {
	a, b := replicaNodes()
	now := time.Unix(1_700_000_000, 100*int64(time.Millisecond)) // 固定时间：刷新相位边界不能落在两份消息之间
	entity := func(id int64) EntityState {
		return EntityState{ID: id, Kind: KindNPC, Ver: Version{Epoch: 1, Seq: 1}, HP: 100}
	}
	view := func(observer int64, tick uint64, ids ...int64) *witnessMsg {
		w := &witnessMsg{Observer: observer, Epoch: 1, Tick: tick, Self: entity(observer)}
		for _, id := range ids {
			w.Entities = append(w.Entities, entity(id))
		}
		return w
	}
	first := replicaWire(t, a, b, &wireMsg{Kind: kindWitness, Witness: view(1, 1, 10, 11), Witnesses: []*witnessMsg{view(2, 1, 10, 11)}}, now)
	if len(first.Deltas) != 2 {
		t.Fatalf("shared objects encoded %d times", len(first.Deltas))
	}
	got, err := b.decodeReplica(first)
	if err != nil || len(got) != 2 {
		t.Fatalf("first: %v %v", got, err)
	}
	next := replicaWire(t, a, b, &wireMsg{Kind: kindWitness, Witness: view(1, 2, 11, 12), Witnesses: []*witnessMsg{view(2, 2, 10, 11)}}, now.Add(time.Millisecond))
	if !reflect.DeepEqual(next.Witness.Added, []int64{12}) || !reflect.DeepEqual(next.Witness.Removed, []int64{10}) || len(next.Deltas) != 1 {
		t.Fatalf("membership was not incremental: %+v", next)
	}
	got, err = b.decodeReplica(next)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0].Witness.Entities, []EntityState{entity(11), entity(12)}) {
		t.Fatal("wrong reconstructed view")
	}
	_, cold := replicaNodes()
	if _, err = cold.decodeReplica(next); !errors.Is(err, errReplicaGap) {
		t.Fatalf("restart trusted missing baseline: %v", err)
	}
	// Receiver requested repair: sender forgets this peer, and the next normal view is self-contained.
	a.sentReplica.clearPeer(b.cfg.SID)
	full := replicaWire(t, a, b, &wireMsg{Kind: kindWitness, Witness: view(1, 3, 11, 12)}, now.Add(2*time.Millisecond))
	if _, err = cold.decodeReplica(full); err != nil {
		t.Fatal(err)
	}
}

// 同一个实体的全量节拍与观察者全量节拍可能不同。整份视野必须自包含，
// 不能要求冷接收端先拿到另一份消息，即使基线重置请求也全部丢了。
func TestPeriodicFullWitnessIsSelfContained(t *testing.T) {
	a, b := replicaNodes()
	now := time.Now()
	s := EntityState{ID: 10, Kind: KindNPC, Ver: Version{Epoch: 1, Seq: 1}, HP: 90}
	makeView := func(id int64, tick uint64) *wireMsg {
		return &wireMsg{Kind: kindWitness, Witness: &witnessMsg{Observer: id, Epoch: 1, Tick: tick, Self: s, Entities: []EntityState{s}}}
	}
	_ = replicaWire(t, a, b, makeView(1, 1), now)
	_ = replicaWire(t, a, b, makeView(2, 1), now.Add(240*time.Millisecond))
	periodic := replicaWire(t, a, b, makeView(1, 2), now.Add(260*time.Millisecond))
	if _, err := b.decodeReplica(periodic); err != nil {
		t.Fatalf("periodic full depends on other views: %v", err)
	}
}

// 同一个对象 / 观察者每个刷新窗口只请求一次修复；新出现的缺口照常请求，窗口过后可以重发，别的 peer 不受影响。
func TestReplicaGapRequestsOneRepairPerRefreshWindow(t *testing.T) {
	_, n := replicaNodes()
	n.controls = make(chan queuedSend, 8)
	now := time.Now()
	gap := &replicaGap{kind: kindWitness, states: []int64{7, 8}, views: []int64{1}}
	for i := range 100 {
		accepted := n.requestReplicaRepair(101, gap, now.Add(time.Duration(i)*time.Millisecond))
		if accepted != (i == 0) {
			t.Fatalf("request %d: accepted %v", i, accepted)
		}
	}
	if len(n.controls) != 1 {
		t.Fatalf("queued %d repairs", len(n.controls))
	}
	if r := (<-n.controls).out.msg.ReplicaReset; r == nil || r.Kind != kindWitness || !slices.Equal(r.States, []int64{7, 8}) || !slices.Equal(r.Views, []int64{1}) {
		t.Fatalf("repair request %+v", r)
	}
	if !n.requestReplicaRepair(101, &replicaGap{kind: kindWitness, states: []int64{7, 9}}, now) {
		t.Fatal("a new object in the gap was not requested")
	}
	if r := (<-n.controls).out.msg.ReplicaReset; !slices.Equal(r.States, []int64{9}) || len(r.Views) != 0 {
		t.Fatalf("already requested object asked again: %+v", r)
	}
	if !n.requestReplicaRepair(101, gap, now.Add(250*time.Millisecond)) {
		t.Fatal("lost reset cannot retry")
	}
	if !n.requestReplicaRepair(102, gap, now) {
		t.Fatal("another peer was throttled")
	}
}

// 有缺口的消息只拒绝解不出的部分：增量对不上的对象从接收缓存删掉、引用它的视野不交付（成员表照常推进），成员表对不上的
// 观察者不交付；其余视野照常交付。发送方只忘掉请求里列出的基线，之后这些视野与对象以全量恢复。
func TestReplicaGapDeliversOnlyCompleteViews(t *testing.T) {
	a, b := replicaNodes()
	now := time.Unix(1_700_000_000, 100*int64(time.Millisecond))
	state := func(id int64, seq uint64) EntityState {
		return EntityState{ID: id, Kind: KindNPC, X: int64(seq), HP: 100, Ver: Version{Epoch: 1, Seq: seq}}
	}
	view := func(observer int64, tick uint64, members ...EntityState) *witnessMsg {
		return &witnessMsg{Observer: observer, Epoch: 1, Tick: tick, Self: EntityState{ID: observer, Kind: KindPlayer, Ver: Version{Epoch: 1, Seq: 1}}, Entities: members}
	}
	batch := func(views ...*witnessMsg) *wireMsg {
		return &wireMsg{Kind: kindWitness, Witness: views[0], Witnesses: views[1:]}
	}
	at := func(ms int) time.Time { return now.Add(time.Duration(ms) * time.Millisecond) }
	if _, err := b.decodeReplica(replicaWire(t, a, b, batch(view(1, 1, state(1, 1), state(2, 1)), view(2, 1, state(3, 1), state(4, 1)), view(3, 1, state(1, 1))), at(0))); err != nil {
		t.Fatal(err)
	}
	_ = replicaWire(t, a, b, batch(view(1, 2, state(1, 2), state(2, 1))), at(1)) // 丢失：观察者 1 的第 2 拍与对象 1 的增量
	got, err := b.decodeReplica(replicaWire(t, a, b, batch(view(1, 3, state(1, 3), state(2, 1)), view(2, 2, state(3, 2), state(4, 1)), view(3, 2, state(1, 3))), at(2)))
	var gap *replicaGap
	if !errors.As(err, &gap) || !slices.Equal(gap.states, []int64{1}) || !slices.Equal(gap.views, []int64{1}) {
		t.Fatalf("gap = %v", err)
	}
	if len(got) != 1 || got[0].Witness.Observer != 2 || !reflect.DeepEqual(got[0].Witness.Entities, []EntityState{state(3, 2), state(4, 1)}) {
		t.Fatalf("delivered %+v", got)
	}
	if _, ok := b.receivedReplica.peer(a.cfg.SID).states[replicaKey{a.cfg.SID, kindWitness, 1}]; ok {
		t.Fatal("object with a broken delta kept its stale baseline")
	}
	if v := b.receivedReplica.peer(a.cfg.SID).views[replicaKey{a.cfg.SID, kindWitness, 3}]; v.tick != 2 {
		t.Fatalf("membership of a view waiting for one object did not advance: %+v", v)
	}
	a.sentReplica.forget(b.cfg.SID, &replicaReset{Kind: kindWitness, States: gap.states, Views: gap.views})
	repaired := replicaWire(t, a, b, batch(view(1, 4, state(1, 3), state(2, 1)), view(2, 3, state(3, 2), state(4, 1)), view(3, 3, state(1, 3))), at(3))
	fulls := 0
	for _, d := range repaired.Deltas {
		if d.Full != nil {
			fulls++
		}
	}
	if fulls != 2 || !repaired.Witness.FullMembership || repaired.Witnesses[0].FullMembership || repaired.Witnesses[1].FullMembership {
		t.Fatalf("repair resent %d full objects, full memberships %v/%v/%v", fulls, repaired.Witness.FullMembership, repaired.Witnesses[0].FullMembership, repaired.Witnesses[1].FullMembership)
	}
	got, err = b.decodeReplica(repaired)
	if err != nil || len(got) != 3 || !reflect.DeepEqual(got[2].Witness.Entities, []EntityState{state(1, 3)}) {
		t.Fatalf("after repair: %v %+v", err, got)
	}
}

func TestUnchangedGhostPulseIsDeliveredBeforeFullRefresh(t *testing.T) {
	a, b := replicaNodes()
	now := time.Now()
	state := EntityState{ID: 7, Kind: KindNPC, X: 10, HP: 10, Ver: Version{Epoch: 1, Seq: 2}}
	msg := &wireMsg{Kind: kindGhost, Ghost: &ghostBatch{Updates: []EntityState{state}}}
	if _, err := b.decodeReplica(replicaWire(t, a, b, msg, now)); err != nil {
		t.Fatal(err)
	}
	pulse := replicaWire(t, a, b, msg, now.Add(50*time.Millisecond))
	got, err := b.decodeReplica(pulse)
	if err != nil || len(got) != 1 || len(got[0].Ghost.Updates) != 1 || got[0].Ghost.Updates[0] != state {
		t.Fatalf("unchanged heartbeat was lost by delta encoding: %v %v", got, err)
	}
	_, cold := replicaNodes()
	if _, err := cold.decodeReplica(pulse); !errors.Is(err, errReplicaGap) {
		t.Fatalf("cold heartbeat did not require a baseline: %v", err)
	}
}

func TestIdleActorKeepsContentVersionAndRefreshesGhost(t *testing.T) {
	grid := worldGrid(t, 2, 1)
	a := CellID{}
	b := CellID{Col: 1}
	na := plainNode(t, grid, a, map[int64]*plainActor{})
	nb := plainNode(t, grid, b, map[int64]*plainActor{})
	st := ActorState{EntityState: EntityState{ID: npcID(911), Kind: KindNPC, X: 9950, Y: 3000, HP: 10, Ver: Version{Epoch: 1, Seq: 8}}, Role: RoleReal}
	now := time.Now()
	r := NewRegion()
	r.locals.set(st.ID, localRec{St: st.EntityState, Role: RoleReal})
	remote := NewRegion()
	for i := range 100 {
		at := now.Add(time.Duration(i) * 50 * time.Millisecond)
		fx := na.stepActor(&st, at, 50*time.Millisecond)
		if st.Ver.Seq != 8 {
			t.Fatalf("static beat %d changed content version to %d", i, st.Ver.Seq)
		}
		var out []outgoing
		na.applyReports(r, []ActorReport{*fx.report}, at, &out)
		for _, o := range out {
			if o.sid == nb.cfg.SID && o.msg.Ghost != nil {
				nb.applyGhostBatch(remote, na.cfg.SID, o.msg.Ghost, at)
			}
		}
	}
	g, ok := remote.ghosts.get(st.ID)
	if !ok || now.Add(5*time.Second).Sub(time.Unix(0, g.HeardAt)) > nb.cfg.GhostTTL {
		t.Fatal("unchanged live ghost lost its heartbeat")
	}
	st.VX = 100
	na.stepActor(&st, now.Add(5*time.Second), 50*time.Millisecond)
	if st.Ver.Seq != 9 {
		t.Fatal("movement did not advance content version")
	}
}

func BenchmarkActorBeat(b *testing.B) {
	grid := worldGrid(b, 2, 1)
	for _, velocity := range []int64{0, 100} {
		b.Run(fmt.Sprintf("vx_%d", velocity), func(b *testing.B) {
			n := plainNode(b, grid, CellID{}, map[int64]*plainActor{})
			base := ActorState{EntityState: EntityState{ID: npcID(2), Kind: KindNPC, X: 3000, Y: 3000, VX: velocity, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
			now := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				st := base
				fx := n.stepActor(&st, now, 50*time.Millisecond)
				if fx.report == nil {
					b.Fatal("missing report")
				}
			}
		})
	}
}

// 一批 16 个观察者共享 64 个对象，覆盖大多数内容静止、视野成员不变的接收热路径。
func BenchmarkWitnessDecode(b *testing.B) {
	a, receiver := replicaNodes()
	receiver.cfg.MaxViewEntities = 64
	now := time.Now()
	var views []*witnessMsg
	for observer := int64(1); observer <= 16; observer++ {
		v := &witnessMsg{Observer: observer, Epoch: 1, Tick: 1, Self: EntityState{ID: observer, Kind: KindPlayer, Ver: Version{Epoch: 1, Seq: 1}}}
		for id := int64(100); id < 164; id++ {
			v.Entities = append(v.Entities, EntityState{ID: id, Kind: KindNPC, Ver: Version{Epoch: 1, Seq: 1}, X: id, HP: 100})
		}
		views = append(views, v)
	}
	input := &wireMsg{Kind: kindWitness, From: a.cfg.SID, Witness: views[0], Witnesses: views[1:]}
	if _, err := receiver.decodeReplica(a.encodeReplica(receiver.cfg.SID, input, now)); err != nil {
		b.Fatal(err)
	}
	for _, v := range views {
		v.Tick = 2
	}
	delta := a.encodeReplica(receiver.cfg.SID, input, now.Add(time.Millisecond))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := receiver.decodeReplica(delta); err != nil {
			b.Fatal(err)
		}
		delta.Witness.BaseTick, delta.Witness.Tick = delta.Witness.Tick, delta.Witness.Tick+1
		for _, v := range delta.Witnesses {
			v.BaseTick, v.Tick = v.Tick, v.Tick+1
		}
	}
}

// 一个 Cell 的区域心跳算 witness：7500 见方、半径 800、上限 64，250 个玩家 Real 与 2850 个 NPC（容量档单格的规模）。
func BenchmarkRegionWitnesses(b *testing.B) {
	rect := spatial.Rect{Max: spatial.Point{X: 7500, Y: 7500}}
	n := &Node{cfg: NodeConfig{ViewRadius: 800, MaxViewEntities: 64, IndexBlock: 800}, indexBounds: rect}
	r := NewRegion()
	rnd := rand.New(rand.NewPCG(5, 6))
	for i := int64(1); i <= 3100; i++ {
		st := EntityState{ID: npcID(i), Kind: KindNPC, X: rnd.Int64N(7500), Y: rnd.Int64N(7500), HP: 100, Ver: Version{Epoch: 1, Seq: 1}}
		if i <= 250 {
			st.ID, st.Kind, st.Base = actorID(i), KindPlayer, 1
		}
		r.locals.m[st.ID] = localRec{St: st, Role: RoleReal}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var out []outgoing
		n.witnesses(r, &beatView{}, &out)
		if len(out) != 250 {
			b.Fatalf("%d witnesses", len(out))
		}
	}
}

// 丢一批 witness：接收方只为这一批涉及的观察者与对象请求修复，之后的全量条目以这一批的成员数为界，不把整个 peer
// 退回全量，也不反复放大。4 批各 16 个观察者，第 k 批看对象 [48k+1, 48k+64]（相邻批共享 16 个），每拍每批 4 个对象移动；
// 周期刷新设为 1 小时，只看修复带来的全量。修复请求经接收方的出站控制队列、编码后交给发送方的 receive。
func TestReplicaLossResetsOnlyWhatItCovers(t *testing.T) {
	a, b := replicaNodes()
	for _, n := range []*Node{a, b} {
		n.cfg.ReplicaRefresh, n.cfg.MaxViewEntities, n.cfg.ReplicaCacheSize, n.cfg.MaxWireBytes = time.Hour, 64, 1<<16, 1<<20
	}
	b.controls = make(chan queuedSend, 64)
	const batches, perBatch, stride, span = 4, 16, 48, 64
	truth := map[int64]EntityState{}
	for id := int64(1); id <= stride*(batches-1)+span; id++ {
		truth[id] = EntityState{ID: id, Kind: KindNPC, X: id, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}
	}
	base := time.Unix(1_700_000_000, 0)
	observer := func(k, o int) int64 { return int64(10_000 + k*perBatch + o) }
	tick := func(n int, drop int) (fulls int) {
		now := base.Add(time.Duration(n) * 50 * time.Millisecond)
		for k := range batches {
			for j := range 4 {
				id := int64(k*stride + 1 + (n*4+j)%span)
				s := truth[id]
				s.X, s.Ver.Seq = s.X+1, s.Ver.Seq+1
				truth[id] = s
			}
		}
		for k := range batches {
			var views []*witnessMsg
			for o := range perBatch {
				v := &witnessMsg{Observer: observer(k, o), Epoch: 1, Tick: uint64(n), Self: EntityState{ID: observer(k, o), Kind: KindPlayer, Ver: Version{Epoch: 1, Seq: 1}}}
				for id := int64(k*stride + 1); id <= int64(k*stride+span); id++ {
					v.Entities = append(v.Entities, truth[id])
				}
				views = append(views, v)
			}
			m := a.encodeReplica(b.cfg.SID, &wireMsg{Kind: kindWitness, From: a.cfg.SID, Witness: views[0], Witnesses: views[1:]}, now)
			for _, d := range m.Deltas {
				if d.Full != nil {
					fulls++
				}
			}
			if k == drop {
				continue
			}
			data, err := encodeMsg(m)
			if err != nil {
				t.Fatal(err)
			}
			b.receiveFrom(a.cfg.SID, data)
		}
		for len(b.controls) > 0 {
			repair := (<-b.controls).out.msg
			repair.From = b.cfg.SID
			data, err := encodeMsg(repair)
			if err != nil {
				t.Fatal(err)
			}
			a.receiveFrom(b.cfg.SID, data)
		}
		return fulls
	}
	for n := 1; n <= 4; n++ {
		tick(n, -1)
	}
	if got := tick(5, -1); got != 0 {
		t.Fatalf("steady state sent %d full objects", got)
	}
	tick(6, 0) // 第 0 批丢失
	after, last := 0, 0
	for n := 7; n <= 16; n++ {
		last = tick(n, -1)
		after += last
	}
	t.Logf("full objects in the 10 ticks after one lost batch: %d (last tick %d)", after, last)
	if after > span+16 || last != 0 {
		t.Fatalf("one lost batch caused %d full objects afterwards (last tick %d); its views hold %d", after, last, span)
	}
	for k := range batches {
		for o := range perBatch {
			if v := b.receivedReplica.peer(a.cfg.SID).views[replicaKey{a.cfg.SID, kindWitness, observer(k, o)}]; v.tick != 16 {
				t.Fatalf("observer %d did not recover: tick %d", observer(k, o), v.tick)
			}
		}
	}
	for id, s := range truth {
		if got := b.receivedReplica.peer(a.cfg.SID).states[replicaKey{a.cfg.SID, kindWitness, id}].state; got != s {
			t.Fatalf("object %d decoded as %+v, sender has %+v", id, got, s)
		}
	}
}

// 同一刷新区块里的观察者对齐全量刷新的相位：入场时刻错开的 16 个观察者共享 64 个对象，稳态每个刷新周期只发一次
// 成员全量，被强制全量的对象在一条消息里只编一次。
func TestNearbyObserversRefreshTogether(t *testing.T) {
	a, b := replicaNodes()
	a.cfg.ViewRadius, b.cfg.MaxViewEntities = 800, 64
	base := time.Unix(1_700_000_000, 0)
	var entities []EntityState
	for id := int64(1); id <= 64; id++ {
		entities = append(entities, EntityState{ID: id, Kind: KindNPC, X: 100 + id, Y: 200, HP: 100, Ver: Version{Epoch: 1, Seq: 1}})
	}
	fulls := 0
	for n := 0; n < 40; n++ {
		var views []*witnessMsg
		for o := range 16 {
			if o%5 > n { // 入场时刻错开 0～4 拍
				continue
			}
			self := EntityState{ID: int64(10_000 + o), Kind: KindPlayer, X: 300 + int64(o)*50, Y: 400, Base: 1, Ver: Version{Epoch: 1, Seq: 1}}
			views = append(views, &witnessMsg{Observer: self.ID, Epoch: 1, Tick: uint64(n + 1), Self: self, Entities: entities})
		}
		m := replicaWire(t, a, b, &wireMsg{Kind: kindWitness, Witness: views[0], Witnesses: views[1:]}, base.Add(time.Duration(n)*50*time.Millisecond))
		if _, err := b.decodeReplica(m); err != nil {
			t.Fatal(err)
		}
		if n >= 20 {
			for _, d := range m.Deltas {
				if d.Full != nil {
					fulls++
				}
			}
		}
	}
	// 后 20 拍是 4 个刷新周期：对齐时每周期 64 个全量，留一个周期的余量。
	t.Logf("full objects in 4 refresh periods: %d", fulls)
	if fulls > 5*64 {
		t.Fatalf("staggered observers resent %d full objects in 4 refresh periods", fulls)
	}
}

// witness 按观察者所在的刷新区块排序：ID 交错的两群观察者在输出里各自连续（同一批消息里是相邻的观察者）。
func TestWitnessesGroupObserversByArea(t *testing.T) {
	rect := spatial.Rect{Max: spatial.Point{X: 7500, Y: 7500}}
	n := &Node{cfg: NodeConfig{ViewRadius: 800, MaxViewEntities: 64, IndexBlock: 800}, indexBounds: rect}
	r := NewRegion()
	for i := int64(1); i <= 32; i++ {
		at := spatial.Point{X: 500 + i, Y: 500}
		if i%2 == 0 {
			at = spatial.Point{X: 6000 + i, Y: 6000}
		}
		r.locals.m[actorID(i)] = localRec{St: EntityState{ID: actorID(i), Kind: KindPlayer, Base: 1, X: at.X, Y: at.Y, HP: 100, Ver: Version{Epoch: 1, Seq: 1}}, Role: RoleReal}
	}
	var out []outgoing
	n.witnesses(r, &beatView{}, &out)
	if len(out) != 32 {
		t.Fatalf("%d witnesses", len(out))
	}
	for i, o := range out {
		near := o.msg.Witness.Self.X < 3000
		if near != (i < 16) {
			t.Fatalf("witness %d (observer at %d) breaks the grouping", i, o.msg.Witness.Self.X)
		}
	}
}

// ---- Base 按来源 Cell 分订阅、并行解码（D13 C）----

// witnessNest 是 Base 接收测试的 Nest 替身：只记下投给 Base 的 witness（来源、观察者、Tick），不执行 handler。
// hold 非 nil 时，来自 hold 来源的投递在 Dispatch 里等 release 关闭（模拟这个来源的解码 / 投递很慢）。
type witnessNest struct {
	nest.Client
	mu      sync.Mutex
	seen    map[int32][]uint64
	hold    int32
	entered chan struct{}
	release chan struct{}
	// count 非 nil 时只计数（基准用）：累计到 want 时往 done 发一个信号。
	count, want *atomic.Int64
	done        chan struct{}
}

func (w *witnessNest) Dispatch(_ context.Context, _ nest.HandlerName, _ int64, params nest.Params, _ ...nest.SendOpt) error {
	m, ok := params[0].(*wireMsg)
	if !ok || m.Kind != kindWitness {
		return nil
	}
	if w.hold != 0 && m.From == w.hold {
		select {
		case w.entered <- struct{}{}:
		default:
		}
		<-w.release
	}
	if w.count != nil {
		if n := w.count.Add(1); n == w.want.Load() {
			w.done <- struct{}{}
		}
		return nil
	}
	runtime.Gosched() // 让不同来源的回调交错
	w.mu.Lock()
	w.seen[m.From] = append(w.seen[m.From], m.Witness.Tick)
	w.mu.Unlock()
	return nil
}

func (w *witnessNest) ticks(from int32) []uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.seen[from])
}

// startWitnessBase 起一个只托管玩家的 Node（sid 900）在 lb 上收件，返回它与停止函数。
func startWitnessBase(t testing.TB, grid *Grid, lb Transport, sink *witnessNest) (*Node, func()) {
	t.Helper()
	if sink.seen == nil {
		sink.seen = map[int32][]uint64{}
	}
	n, err := NewNode(NodeConfig{World: "w", SID: 900, Grid: grid, Transport: lb, Nest: sink,
		Destroy:      func(context.Context, fentity.IThreadSafeEntity) error { return nil },
		PlayerEntity: func(actor int64) int64 { return actor }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	select {
	case <-n.Ready():
	case err := <-done:
		cancel()
		t.Fatalf("run: %v", err)
	}
	return n, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}
}

// witnessSender 是一个格的发件端：只用 Node 的正式 publish（增量编码、按来源选主题），不起 Cell。
func witnessSender(grid *Grid, cell CellID, lb Transport) *Node {
	n := &Node{cfg: NodeConfig{World: "w", SID: grid.SID(cell), Grid: grid, Transport: lb}.withDefaults(), cell: cell, hostsCell: true}
	n.sentReplica.init()
	return n
}

// sendWitness 让格 from 给 Base 900 发观察者 observer 第 tick 拍的 witness：8 个对象（状态增量），成员表不变
// （成员增量）。
func sendWitness(from *Node, observer int64, tick uint64) error {
	v := &witnessMsg{Observer: observer, Epoch: 1, Tick: tick, Self: EntityState{ID: observer, Kind: KindPlayer, Base: 900, Ver: Version{Epoch: 1, Seq: 1}}}
	for i := int64(1); i <= 8; i++ {
		// 第 i 个对象每 8 拍动一次，版本每拍推进：多数是只带版本的空增量，少数带位置。
		x := i*1000 + (int64(tick)+8-i)/8
		v.Entities = append(v.Entities, EntityState{ID: int64(from.cfg.SID)*1000 + i, Kind: KindNPC, X: x, HP: 100, Ver: Version{Epoch: 1, Seq: tick}})
	}
	return from.publish(outgoing{sid: 900, msg: &wireMsg{Kind: kindWitness, Witness: v}})
}

// 同一来源 Cell 的 witness 按发布顺序解码、投递：每个来源的 Tick 恰好是 1..N 递增，没有一条因并行解码乱序而报缺口。
// 四个格并发发件，Base 的四个来源订阅并发解码。
func TestBaseDecodesEachSourceCellInOrder(t *testing.T) {
	grid := worldGrid(t, 4, 1)
	lb := newLoopback()
	sink := &witnessNest{}
	base, stop := startWitnessBase(t, grid, lb, sink)
	defer stop()
	const ticks = 400
	var senders sync.WaitGroup
	for _, cell := range grid.Cells() {
		from := witnessSender(grid, cell, lb)
		senders.Go(func() {
			for tick := uint64(1); tick <= ticks; tick++ {
				if err := sendWitness(from, int64(from.cfg.SID), tick); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	senders.Wait()
	eventually(t, 10*time.Second, "every source delivered", func() (bool, string) {
		for _, cell := range grid.Cells() {
			if got := len(sink.ticks(grid.SID(cell))); got < ticks {
				return false, fmt.Sprintf("sid %d delivered %d of %d", grid.SID(cell), got, ticks)
			}
		}
		return true, ""
	})
	for _, cell := range grid.Cells() {
		got := sink.ticks(grid.SID(cell))
		for i, tick := range got {
			if tick != uint64(i+1) {
				t.Fatalf("sid %d: delivery %d has tick %d (out of order)", grid.SID(cell), i, tick)
			}
		}
	}
	if s := base.Stats(); s.ReplicaGaps != 0 || s.DecodeErrors != 0 {
		t.Fatalf("gaps %d, decode errors %d", s.ReplicaGaps, s.DecodeErrors)
	}
}

// 一个来源 Cell 的解码 / 投递卡住，不挡别的来源：慢来源的一条 witness 停在回调里时，另一格的 witness 照常解码、投递；
// 放开之后慢来源的那条也到达。分订阅之前所有来源共用一个订阅回调，这里会卡住。
func TestSlowSourceCellDoesNotBlockOthers(t *testing.T) {
	grid := worldGrid(t, 2, 1)
	lb := newLoopback()
	slow, fast := CellID{0, 0}, CellID{1, 0}
	sink := &witnessNest{hold: grid.SID(slow), entered: make(chan struct{}, 1), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(sink.release) }) }
	base, stop := startWitnessBase(t, grid, lb, sink)
	defer stop()
	defer release()
	slowSender, fastSender := witnessSender(grid, slow, lb), witnessSender(grid, fast, lb)
	if err := sendWitness(slowSender, 1, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow source's witness never reached dispatch")
	}
	for tick := uint64(1); tick <= 50; tick++ {
		if err := sendWitness(fastSender, 2, tick); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 5*time.Second, "fast source delivered while the slow one is stuck", func() (bool, string) {
		got := len(sink.ticks(grid.SID(fast)))
		return got == 50, fmt.Sprintf("fast delivered %d of 50 (replica gaps %d)", got, base.Stats().ReplicaGaps)
	})
	release()
	eventually(t, 5*time.Second, "slow source delivered after release", func() (bool, string) {
		got := len(sink.ticks(grid.SID(slow)))
		return got == 1, fmt.Sprintf("slow delivered %d", got)
	})
}

// 主题只是路由：来源主题上 From 不是那一格的、共用主题上冒充格的消息按坏消息丢弃，不推进任何来源的基线。
func TestReceiveRejectsMessagesOffTheirSourceLane(t *testing.T) {
	grid := worldGrid(t, 2, 1)
	lb := newLoopback()
	sink := &witnessNest{}
	base, stop := startWitnessBase(t, grid, lb, sink)
	defer stop()
	a, b := grid.SID(CellID{0, 0}), grid.SID(CellID{1, 0})
	sender := witnessSender(grid, CellID{0, 0}, lb)
	m := sender.encodeReplica(900, &wireMsg{Kind: kindWitness, From: a, Witness: &witnessMsg{Observer: 1, Epoch: 1, Tick: 1,
		Self: EntityState{ID: 1, Kind: KindPlayer, Ver: Version{Epoch: 1, Seq: 1}}}}, time.Now())
	data, err := encodeMsg(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{SubjectFromCell("w", 900, b), SubjectFromBase("w", 900)} {
		if err := lb.Publish(subject, data); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 5*time.Second, "both off-lane messages rejected", func() (bool, string) {
		s := base.Stats()
		return s.DecodeErrors == 2, fmt.Sprintf("decode errors %d", s.DecodeErrors)
	})
	if err := lb.Publish(SubjectFromCell("w", 900, a), data); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "on-lane message delivered", func() (bool, string) {
		return len(sink.ticks(a)) == 1, fmt.Sprintf("delivered %v", sink.ticks(a))
	})
	if got := sink.ticks(b); len(got) != 0 {
		t.Fatalf("off-lane message delivered as sid %d: %v", b, got)
	}
}

// 接收缓存按来源分片之后，容量上限仍是全部来源合计：超限时清空所有分片（与分片前清空整个缓存相同），计数归零。
func TestReceivedReplicaBoundCoversAllSources(t *testing.T) {
	a, b := replicaNodes()
	b.cfg.ReplicaCacheSize = 64
	other := &Node{cfg: NodeConfig{SID: 201, ReplicaCacheSize: 1024, MaxViewEntities: 50, WitnessBatch: 16, ReplicaRefresh: 250 * time.Millisecond}}
	other.sentReplica.init()
	b.cfg.SID = 301
	b.receivedReplica.init([]int32{101, 201})
	now := time.Now()
	ghost := func(from *Node, first int64) {
		var updates []EntityState
		for id := first; id < first+40; id++ {
			updates = append(updates, EntityState{ID: id, Kind: KindNPC, HP: 1, Ver: Version{Epoch: 1, Seq: 1}})
		}
		m := replicaWire(t, from, b, &wireMsg{Kind: kindGhost, Ghost: &ghostBatch{Updates: updates}}, now)
		if _, err := b.decodeReplica(m); err != nil {
			t.Fatal(err)
		}
	}
	ghost(a, 1)
	if got := b.receivedReplica.size.Load(); got != 40 {
		t.Fatalf("size after the first source: %d", got)
	}
	ghost(other, 1000) // 合计 80 ≥ 64：两个分片都清空
	if got := b.receivedReplica.size.Load(); got != 0 {
		t.Fatalf("size after crossing the bound: %d", got)
	}
	for _, sid := range []int32{101, 201} {
		if c := b.receivedReplica.peer(sid); len(c.states)+len(c.views) != 0 {
			t.Fatalf("source %d kept %d entries", sid, len(c.states)+len(c.views))
		}
	}
	if _, err := b.decodeReplica(&wireMsg{Kind: kindGhost, From: 555, Replica: true, Ghost: &ghostBatch{}}); !errors.Is(err, errBadMessage) {
		t.Fatalf("replica state from a non-cell source: %v", err)
	}
}

// captureTransport 记下 publish 选的主题与编码好的字节，不投递（基准预先编码用）。
type captureTransport struct {
	subject string
	data    []byte
}

func (c *captureTransport) Publish(subject string, data []byte) error {
	c.subject, c.data = subject, data
	return nil
}
func (*captureTransport) Subscribe(string, func([]byte)) (func() error, error) {
	return func() error { return nil }, nil
}

// Base 接收吞吐（D13 C 前后对照）：sources 个格各给 Base 发一批 16 名观察者 × 64 个对象的 witness，每条都是全量
// （成员表全量 + 成员状态全量：千人档全量条目约占 80%，这里取上限），一轮等 Base 把全部 16 × sources 份视野解码、展开、
// 投给 Nest（替身只计数）才算完。消息预先编码，计时只含 Base 的收件路径（传输回调 → decodeMsg → decodeReplica →
// 派发）。分订阅之前各来源在同一个回调里串行，一轮的时间随来源数线性增长；分订阅之后各来源并行，随可用核数摊开。
func BenchmarkBaseReceiveSources(b *testing.B) {
	for _, sources := range []int{1, 4} {
		b.Run(fmt.Sprintf("sources_%d", sources), func(b *testing.B) {
			grid := worldGrid(b, int32(sources), 1)
			lb := newLoopback()
			var count, want atomic.Int64
			sink := &witnessNest{count: &count, want: &want, done: make(chan struct{}, 1)}
			base, stop := startWitnessBase(b, grid, lb, sink)
			defer stop()
			type encoded struct {
				subject string
				data    []byte
			}
			senders := make([]*Node, 0, sources)
			captures := make([]*captureTransport, 0, sources)
			for _, cell := range grid.Cells() {
				capture := &captureTransport{}
				sender := witnessSender(grid, cell, capture)
				sender.cfg.ReplicaRefresh = time.Nanosecond // 每条都发全量
				senders, captures = append(senders, sender), append(captures, capture)
			}
			tick := uint64(0)
			const pool = 256
			var rounds [][]encoded
			refill := func() {
				rounds = rounds[:0]
				for range pool {
					tick++
					var round []encoded
					for i, sender := range senders {
						var views []*witnessMsg
						for o := int64(1); o <= 16; o++ {
							observer := int64(sender.cfg.SID)*100 + o
							v := &witnessMsg{Observer: observer, Epoch: 1, Tick: tick, Self: EntityState{ID: observer, Kind: KindPlayer, Base: 900, Ver: Version{Epoch: 1, Seq: 1}}}
							for id := int64(1); id <= 64; id++ {
								v.Entities = append(v.Entities, EntityState{ID: int64(sender.cfg.SID)*1000 + id, Kind: KindNPC, X: id + int64(tick), HP: 100, Ver: Version{Epoch: 1, Seq: tick}})
							}
							views = append(views, v)
						}
						if err := sender.publish(outgoing{sid: 900, msg: &wireMsg{Kind: kindWitness, Witness: views[0], Witnesses: views[1:]}}); err != nil {
							b.Fatal(err)
						}
						round = append(round, encoded{captures[i].subject, captures[i].data})
					}
					rounds = append(rounds, round)
				}
			}
			refill()
			next := 0
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if next == len(rounds) {
					b.StopTimer()
					refill()
					next = 0
					b.StartTimer()
				}
				count.Store(0)
				want.Store(int64(16 * sources))
				for _, m := range rounds[next] {
					if err := lb.Publish(m.subject, m.data); err != nil {
						b.Fatal(err)
					}
				}
				next++
				<-sink.done
			}
			b.StopTimer()
			if s := base.Stats(); s.ReplicaGaps != 0 || s.DecodeErrors != 0 {
				b.Fatalf("gaps %d, decode errors %d", s.ReplicaGaps, s.DecodeErrors)
			}
			b.ReportMetric(float64(16*sources)*float64(b.N)/b.Elapsed().Seconds(), "witness/s")
		})
	}
}
