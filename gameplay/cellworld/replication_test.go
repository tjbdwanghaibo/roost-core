package cellworld

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func replicaNodes() (*Node, *Node) {
	makeNode := func(sid int32) *Node {
		n := &Node{cfg: NodeConfig{SID: sid, ReplicaCacheSize: 1024, MaxViewEntities: 50, WitnessBatch: 16, ReplicaRefresh: 250 * time.Millisecond}}
		n.sentReplica.init()
		n.receivedReplica.init()
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
	if b.receivedReplica.states[key].state != next {
		t.Fatal("old full regressed the decoding baseline")
	}
}
func TestWitnessMembershipDeltaAndSharedEncoding(t *testing.T) {
	a, b := replicaNodes()
	now := time.Now()
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

func TestReplicaGapRequestsOneRepairPerRefreshWindow(t *testing.T) {
	_, n := replicaNodes()
	n.controls = make(chan queuedSend, 8)
	now := time.Now()
	for i := range 100 {
		accepted := n.requestReplicaReset(101, now.Add(time.Duration(i)*time.Millisecond))
		if accepted != (i == 0) {
			t.Fatalf("request %d: accepted %v", i, accepted)
		}
	}
	if len(n.controls) != 1 {
		t.Fatalf("queued %d repairs", len(n.controls))
	}
	if !n.requestReplicaReset(101, now.Add(250*time.Millisecond)) {
		t.Fatal("lost reset cannot retry")
	}
	if !n.requestReplicaReset(102, now) {
		t.Fatal("another peer was throttled")
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
