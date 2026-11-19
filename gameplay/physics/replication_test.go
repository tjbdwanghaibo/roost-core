package physics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/chunksync"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

func TestViewCadencePromotionRemovalAndFieldRestoration(t *testing.T) {
	bodies := []physx.Body{{ID: 1, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}, {ID: 2, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}}
	states := []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}, {ID: 2, Rotation: physx.Quat{0, 0, 0, 1}, Position: physx.Vec3{12, 0, 0}, Velocity: physx.Vec3{1, 0, 0}}}
	policy := DefaultReplicationPolicy()
	plan := func(tick uint64, previous map[uint64]ViewEntry, grabs map[uint64]uint64) ViewPlan {
		t.Helper()
		s, err := QuantizedSnapshot(Frame{Epoch: 1, Tick: tick, States: states})
		if err != nil {
			t.Fatal(err)
		}
		p, err := PlanView(s, states, bodies, nil, 1, grabs, previous, policy)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := plan(1, nil, nil)
	if first.Entries[2].Class != "far" || !bytes.Equal(first.Entries[2].Object.Data[15:33], make([]byte, 18)) {
		t.Fatal("far fields not trimmed")
	}
	states[1].Position[0] = 13
	deferred := plan(4, first.Entries, nil)
	if deferred.Entries[2].Tick != 1 || deferred.Deferred["far"] != 1 {
		t.Fatal("far cadence")
	}
	refreshed := plan(11, first.Entries, nil)
	if refreshed.Entries[2].Tick != 11 {
		t.Fatal("far did not refresh")
	}
	states[1].Position[0] = 2
	near := plan(12, refreshed.Entries, nil)
	if near.Entries[2].Tick != 12 || near.Entries[2].Class != "near" || bytes.Equal(near.Entries[2].Object.Data[15:33], make([]byte, 18)) {
		t.Fatal("promotion did not restore velocity immediately")
	}
	states[1].Position[0] = 13
	grabbed := plan(13, first.Entries, map[uint64]uint64{2: 1})
	if grabbed.Entries[2].Class != "near" {
		t.Fatal("reverse grab not prioritized")
	}
	states[1].Position[0] = 25.1
	migrated := plan(4, first.Entries, nil)
	if migrated.Entries[2].Tick != 4 || migrated.Entries[2].Object.Chunk.X != 1 {
		t.Fatal("migration deferred")
	}
	states = states[:1]
	left := plan(5, migrated.Entries, nil)
	if len(left.Entries) != 1 || len(left.Snapshot.Objects) != 1 {
		t.Fatal("removal deferred")
	}
	// 暂存计划不能污染 ACK 基线。
	if first.Entries[2].Tick != 1 || first.Entries[2].Object.Chunk.X != 0 {
		t.Fatal("baseline mutated")
	}
}

func TestViewLargeFastBodiesAndMotionCalibration(t *testing.T) {
	bodies := []physx.Body{{ID: 1, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}, {ID: 2, Shape: physx.Box, Motion: physx.Kinematic, Dimensions: physx.Vec3{1, 1, 1}}}
	states := []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}, {ID: 2, Rotation: physx.Quat{0, 0, 0, 1}, Position: physx.Vec3{12, 0, 0}}}
	run := func(tick uint64, previous map[uint64]ViewEntry) ViewPlan {
		s, _ := QuantizedSnapshot(Frame{Epoch: 1, Tick: tick, States: states})
		p, err := PlanView(s, states, bodies, []MotionTrack{{ID: 2}}, 1, nil, previous, DefaultReplicationPolicy())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := run(1, nil)
	if first.Entries[2].Class != "motion" {
		t.Fatal("motion profile")
	}
	states[1].Position[0] = 13
	held := run(49, first.Entries)
	if held.Entries[2].Tick != 1 {
		t.Fatal("early calibration")
	}
	updated := run(51, first.Entries)
	if updated.Entries[2].Tick != 51 {
		t.Fatal("missing calibration")
	}
	states[1].Velocity[0] = -100
	fast := run(52, updated.Entries)
	if fast.Entries[2].Class != "near" {
		t.Fatal("fast approaching body throttled")
	}
	states[1].Velocity[0] = 0
	bodies[1].Dimensions[0] = 10
	large := run(52, updated.Entries)
	if large.Entries[2].Class != "near" {
		t.Fatal("large nearby surface throttled")
	}
}

func TestSharedReplicationFrameOwnsItsInputs(t *testing.T) {
	bodies := []physx.Body{{ID: 1, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}, {ID: 2, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}}
	states := []physx.State{{ID: 1, Rotation: physx.Quat{0, 0, 0, 1}}, {ID: 2, Position: physx.Vec3{11, 0, 0}, Rotation: physx.Quat{0, 0, 0, 1}}}
	snapshot, _ := QuantizedSnapshot(Frame{Epoch: 1, Tick: 1, States: states})
	scene := NewReplicationScene(bodies, nil)
	frame := scene.Frame(states)
	bodies[1].Dimensions[0] = 100
	states[1].Position[0] = 1
	for i := 0; i < 2; i++ {
		p, err := frame.Plan(snapshot, 1, nil, nil, DefaultReplicationPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if p.Entries[2].Class != "far" {
			t.Fatal("shared index aliased mutable inputs")
		}
	}
	next := scene.Frame(states)
	p, err := next.Plan(snapshot, 1, nil, nil, DefaultReplicationPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[2].Class != "near" {
		t.Fatal("next frame did not refresh index")
	}
}

// 帧索引按 ID 查找：输入无序或同 ID 重复时与按 ID 覆盖写入的结果相同（同 ID 取最后一个）；
// Frame 与 AdoptFrame 都不修改调用方切片。
func TestReplicationFrameLookupMatchesLastWrite(t *testing.T) {
	state := func(id uint64, x float32) physx.State {
		return physx.State{ID: id, Position: physx.Vec3{x}, Rotation: physx.Quat{0, 0, 0, 1}}
	}
	inputs := [][]physx.State{
		nil,
		{state(3, 1), state(7, 2), state(9, 3)},
		{state(9, 1), state(3, 2), state(7, 3), state(3, 4), state(5, 5), state(9, 6)},
		{state(4, 1), state(4, 2), state(4, 3)},
	}
	scene := NewReplicationScene(nil, nil)
	for _, input := range inputs {
		want := make(map[uint64]physx.State)
		for _, s := range input {
			want[s.ID] = s
		}
		original := slices.Clone(input)
		for _, adopt := range []bool{false, true} {
			frame := scene.Frame(input)
			if adopt {
				frame = scene.AdoptFrame(input)
			}
			if !slices.Equal(input, original) {
				t.Fatalf("adopt=%v modified the caller slice", adopt)
			}
			for id := uint64(0); id <= 10; id++ {
				got, ok := frame.State(id)
				w, exists := want[id]
				if ok != exists || got != w {
					t.Fatalf("adopt=%v id=%d: got %v,%v want %v,%v", adopt, id, got, ok, w, exists)
				}
			}
		}
	}
}

func BenchmarkRecipientViewIndex(b *testing.B) {
	bodies := CrowdScene()
	states := InitialFrame(1, bodies).States
	snapshot, _ := QuantizedSnapshot(Frame{Epoch: 1, Tick: 1, States: states})
	cached := NewReplicationScene(bodies, nil).Frame(states)
	for _, shared := range []bool{false, true} {
		name := "rebuild"
		if shared {
			name = "shared"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var err error
				if shared {
					_, err = cached.Plan(snapshot, 51, nil, nil, DefaultReplicationPolicy())
				} else {
					_, err = PlanView(snapshot, states, bodies, nil, 51, nil, nil, DefaultReplicationPolicy())
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// 固定场景走完整房间复制链路（量化快照、AOI、视图分档、字段增量、私有确认），
// 逐帧比较载荷与视图账本。分配优化不得改变任何一帧的字节或分档结果；
// 复用 ACK 后换下的视野与账本（VisibleInto/PlanInto）必须与每次新建的结果相同。
func TestRoomReplicationPipelineStaysByteIdentical(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprint("reuse=", reuse), func(t *testing.T) { checkRoomReplicationPipeline(t, reuse) })
	}
}

func checkRoomReplicationPipeline(t *testing.T, reuse bool) {
	bodies := CrowdScene()
	tracks := []MotionTrack{{ID: 1, PeriodSeconds: 4, Axis: physx.Vec3{0, 1, 0}, AngularSpeed: 1}}
	base := InitialFrame(9, bodies).States
	scene := NewReplicationScene(bodies, tracks)
	type member struct {
		body              uint64
		cursor            chunksync.Cursor
		pending           *chunksync.Plan
		payload           []byte
		view, nextView    map[uint64]ViewEntry
		visible, nextSeen map[uint64]bool
		spareView         map[uint64]ViewEntry
		spareSeen         map[uint64]bool
	}
	members := []*member{{body: 51}, {body: 55}, {body: 60}, {body: 70}}
	digest := sha256.New()
	write := func(values ...any) { fmt.Fprintln(digest, values...) }
	polls := 0
	coverage := map[string]int{}
	var seenObjects, viewObjects []chunksync.Object
	for tick := uint64(1); tick <= 90; tick++ {
		states := slices.Clone(base)
		for i := range states {
			s := &states[i]
			phase := float64(tick)/7 + float64(s.ID)
			// 部分刚体做大幅往返，穿越 AOI 半径、分档边界和 25m Chunk。
			amplitude := float32(1 + s.ID%5*4)
			s.Position[0] += amplitude * float32(math.Sin(phase))
			s.Position[2] += amplitude * float32(math.Cos(phase*.7))
			s.Velocity = physx.Vec3{amplitude * float32(math.Cos(phase)) / 7, 0, -amplitude * float32(math.Sin(phase*.7)) / 10}
			half := float32(phase) / 2
			s.Rotation = physx.Quat{0, float32(math.Sin(float64(half))), 0, float32(math.Cos(float64(half)))}
			s.Sleeping = s.ID%11 == 0 && tick%13 < 4
			s.InputSequence, s.InputGeneration, s.InputActive = tick, 3, s.ID%2 == 0
		}
		frame := Frame{Epoch: 9, Tick: tick, States: states}
		spatial, err := NewQuantizedSpatialFrame(frame, 10)
		if err != nil {
			t.Fatal(err)
		}
		view := scene.Frame(states)
		grabs := map[uint64]uint64{}
		if tick%20 < 10 {
			grabs[51] = 3
			grabs[4] = 55
		}
		for k, m := range members {
			if (tick+uint64(k))%2 != 0 {
				continue
			}
			polls++
			if m.pending != nil {
				if polls%5 == 0 { // 未确认：重发同一载荷。
					digest.Write(m.payload)
					continue
				}
				if err = m.cursor.Commit(m.pending); err != nil {
					t.Fatal(err)
				}
				if reuse {
					m.spareView, m.spareSeen = m.view, m.visible
				}
				m.view, m.visible, m.pending = m.nextView, m.nextSeen, nil
			}
			snapshot, visible, err := spatial.VisibleInto(seenObjects, m.spareSeen, m.body, 12, 2, m.visible)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := view.PlanInto(viewObjects, m.spareView, snapshot, m.body, grabs, m.view, DefaultReplicationPolicy())
			if err != nil {
				t.Fatal(err)
			}
			m.spareView, m.spareSeen = nil, nil
			p, err := m.cursor.PlanFields(plan.Snapshot, QuantizedLayout())
			if err != nil {
				t.Fatal(err)
			}
			if reuse { // 与 Poll 一样，组帧后两张临时对象表即可给下一个成员复用。
				seenObjects, viewObjects = snapshot.Objects, plan.Snapshot.Objects
			}
			var own InputAck
			if state, ok := view.State(m.body); ok {
				own = InputAck{Sequence: state.InputSequence, Generation: state.InputGeneration, Active: state.InputActive}
			}
			m.pending, m.nextView, m.nextSeen = p, plan.Entries, visible
			m.payload = RoomPayload(p.Data, own)
			digest.Write(m.payload)
			for _, id := range slices.Sorted(maps.Keys(plan.Entries)) {
				e := plan.Entries[id]
				write(id, e.Tick, e.Class, e.Object.Chunk, e.Object.Data, visible[id])
			}
			for _, class := range slices.Sorted(maps.Keys(plan.Samples)) {
				write("sample", class, plan.Samples[class])
				coverage[class]++
			}
			for _, class := range slices.Sorted(maps.Keys(plan.Deferred)) {
				write("deferred", class, plan.Deferred[class])
				coverage["deferred"]++
			}
			for id := range m.visible {
				if !visible[id] {
					coverage["removed"]++
				}
			}
			for id, e := range plan.Entries {
				if old, ok := m.view[id]; ok && old.Object.Chunk != e.Object.Chunk {
					coverage["migrated"]++
				}
			}
			write("visible", len(visible))
		}
	}
	for _, need := range []string{"near", "middle", "far", "motion", "deferred", "removed", "migrated"} {
		if coverage[need] == 0 {
			t.Fatalf("scenario does not cover %s: %v", need, coverage)
		}
	}
	// RP3 只改了私有头的版本字节：把 roomMagic 临时改回 "RP\x02" 时，这里得到的仍是 RP2 时期的摘要
	// f862c6e98746cdb23e8e98b7bb9d5421cb824cbaea64027eb8d552d5d1d34790（2026-11-19 实测）。
	// 场景里没有传送，所以对象字节、分档与账本都不变。
	const want = "dea81b0bf0f155580923890568e3b8b84c03ed1760e01b1b789d823e7ce6ca0c"
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Fatalf("replication bytes changed: %s", got)
	}
}

// 传送标记按接收方各自的视图账本置位：近 / 中 / 远三档接收方、以及传送那一帧没有收帧的接收方，
// 都在传送后发给它的第一帧里看到 ViewTeleported 和当前位置，下一帧清掉，之后不再出现；
// 没被确认的计划不改账本，重新计划时标记还在。
func TestTeleportMarkReachesEveryReceiverOnce(t *testing.T) {
	const target = 1
	bodies := []physx.Body{{ID: target, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}}}
	states := []physx.State{{ID: target, Rotation: physx.Quat{0, 0, 0, 1}, Velocity: physx.Vec3{0, 0, 1.5}}}
	// 观察者离目标 3m / 8m / 20m，分别落在近、中、远档；late 也在远档，但传送那一帧不收帧。
	observers := []struct {
		name  string
		id    uint64
		x     float32
		class string
	}{{"near", 10, 3, "near"}, {"middle", 11, 8, "middle"}, {"far", 12, 20, "far"}, {"late", 13, -20, "far"}}
	for _, o := range observers {
		bodies = append(bodies, physx.Body{ID: o.id, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}})
		states = append(states, physx.State{ID: o.id, Rotation: physx.Quat{0, 0, 0, 1}, Position: physx.Vec3{o.x, 0, 0}})
	}
	scene := NewReplicationScene(bodies, nil)
	type receiver struct {
		ledger  map[uint64]ViewEntry // 已确认的视图账本
		cursor  chunksync.Cursor
		replica RoomReplica
		marks   int // 客户端解码后看到目标带 ViewTeleported 的帧数
	}
	receivers := map[uint64]*receiver{}
	for _, o := range observers {
		receivers[o.id] = &receiver{}
	}
	plan := func(tick, observer uint64) ViewPlan {
		t.Helper()
		snapshot, err := QuantizedSnapshot(Frame{Epoch: 1, Tick: tick, States: states})
		if err != nil {
			t.Fatal(err)
		}
		p, err := scene.Frame(states).Plan(snapshot, observer, nil, receivers[observer].ledger, DefaultReplicationPolicy())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// deliver 走完整的下发与确认：字段增量、RP3 私有头、客户端副本解码，然后才采纳账本。
	deliver := func(tick, observer uint64) ViewEntry {
		t.Helper()
		r := receivers[observer]
		p := plan(tick, observer)
		fields, err := r.cursor.PlanFields(p.Snapshot, QuantizedLayout())
		if err != nil {
			t.Fatal(err)
		}
		if err = r.replica.Apply(RoomPayload(fields.Data, InputAck{})); err != nil {
			t.Fatal(err)
		}
		if err = r.cursor.Commit(fields); err != nil {
			t.Fatal(err)
		}
		r.ledger = p.Entries
		if r.replica.Profiles[target]&ViewTeleported != 0 {
			r.marks++
		}
		return p.Entries[target]
	}
	marked := func(e ViewEntry) bool { return e.Object.Data[33]&ViewTeleported != 0 }
	position := func(observer uint64) physx.Vec3 { return receivers[observer].replica.States[target].Position }

	for _, o := range observers {
		if e := deliver(1, o.id); e.Class != o.class || marked(e) {
			t.Fatalf("%s: first sight must be class %s without a mark, got %s %v", o.name, o.class, e.Class, marked(e))
		}
	}
	// tick 2：目标被传送（同一 Chunk、各观察者的档位不变，排除跨 Chunk 与升档这两条已有的刷新路径）。
	states[0].Position, states[0].Velocity, states[0].Teleports = physx.Vec3{1, 0, 1}, physx.Vec3{}, 1
	for _, o := range observers[:3] {
		// 计划了但没确认（Poll 的响应丢了）：账本不变，再计划一次标记仍在，账本本身没被改。
		lost := plan(2, o.id)
		if !marked(lost.Entries[target]) || lost.Entries[target].Tick != 2 {
			t.Fatalf("%s: unacknowledged plan lacks the mark", o.name)
		}
		if base := receivers[o.id].ledger[target]; marked(base) || base.Teleports != 0 || base.Tick != 1 {
			t.Fatalf("%s: planning modified the acknowledged ledger: %+v", o.name, base)
		}
		e := deliver(2, o.id)
		if !marked(e) || e.Tick != 2 || e.Class != o.class {
			t.Fatalf("%s: teleport frame must refresh with the mark: tick %d class %s mark %v", o.name, e.Tick, e.Class, marked(e))
		}
		if got := position(o.id); got != (physx.Vec3{1, 0, 1}) {
			t.Fatalf("%s: marked frame carries position %v", o.name, got)
		}
		if receivers[o.id].replica.Profiles[target]&ViewDisplayOnly != 0 != (o.class != "near") {
			t.Fatalf("%s: the mark changed the profile bits: %b", o.name, receivers[o.id].replica.Profiles[target])
		}
	}
	// tick 3：标记清掉（远档本来要等到 tick 11 才刷新，这里为了清标记刷新一次），位置不变。
	for _, o := range observers[:3] {
		if e := deliver(3, o.id); marked(e) || e.Tick != 3 {
			t.Fatalf("%s: mark not cleared on the next frame: tick %d mark %v", o.name, e.Tick, marked(e))
		}
		if receivers[o.id].replica.Profiles[target]&ViewTeleported != 0 {
			t.Fatalf("%s: client still sees the mark", o.name)
		}
	}
	// tick 4：一切照旧——远档与中档回到各自的周期（暂缓），不再因为那次传送多发。
	states[0].Position[2] = 1.25
	for _, o := range observers[:3] {
		e := deliver(4, o.id)
		if marked(e) || (e.Tick == 4) != (o.class == "near") {
			t.Fatalf("%s: after the mark, tick %d mark %v", o.name, e.Tick, marked(e))
		}
	}
	// late 从 tick 1 之后一直没收帧，期间目标又被传送了一次：它的下一帧带一次标记和最新位置。
	states[0].Position, states[0].Teleports = physx.Vec3{2, 0, 2}, 2
	if e := deliver(5, 13); !marked(e) || e.Tick != 5 || position(13) != (physx.Vec3{2, 0, 2}) {
		t.Fatalf("late receiver missed the teleport: tick %d mark %v position %v", e.Tick, marked(e), position(13))
	}
	if e := deliver(6, 13); marked(e) {
		t.Fatal("late receiver: mark repeated")
	}
	// 其余接收方也各自再看到第二次传送，一次。
	for _, o := range observers[:3] {
		if e := deliver(5, o.id); !marked(e) {
			t.Fatalf("%s: second teleport not marked", o.name)
		}
		for tick := uint64(6); tick <= 30; tick++ {
			if e := deliver(tick, o.id); marked(e) {
				t.Fatalf("%s: mark came back at tick %d", o.name, tick)
			}
		}
	}
	for tick := uint64(7); tick <= 30; tick++ {
		deliver(tick, 13)
	}
	for _, o := range observers {
		want := 2
		if o.name == "late" {
			want = 1 // 两次传送之间没收过帧，只需要知道“位置不连续”一次。
		}
		if got := receivers[o.id].marks; got != want {
			t.Fatalf("%s: client saw the mark in %d frames, want %d", o.name, got, want)
		}
	}
	// 新进入视野的接收方没有旧位置，不需要标记。
	receivers[99] = &receiver{}
	bodies = append(bodies, physx.Body{ID: 99, Shape: physx.Sphere, Dimensions: physx.Vec3{.5}})
	states = append(states, physx.State{ID: 99, Rotation: physx.Quat{0, 0, 0, 1}, Position: physx.Vec3{4, 0, 0}})
	scene = NewReplicationScene(bodies, nil)
	if e := deliver(31, 99); marked(e) || e.Teleports != 2 {
		t.Fatalf("newcomer: mark %v, ledger count %d", marked(e), e.Teleports)
	}
	// 关闭分档（policy.Enabled=false）时同样置位、同样只一次。
	states[0].Teleports = 3
	off := ReplicationPolicy{}
	snapshot, _ := QuantizedSnapshot(Frame{Epoch: 1, Tick: 32, States: states})
	first, err := scene.Frame(states).Plan(snapshot, 99, nil, receivers[99].ledger, off)
	if err != nil || !marked(first.Entries[target]) {
		t.Fatalf("mark missing with tiers disabled: %v", err)
	}
	snapshot.Tick = 33
	second, err := scene.Frame(states).Plan(snapshot, 99, nil, first.Entries, off)
	if err != nil || marked(second.Entries[target]) {
		t.Fatalf("mark repeated with tiers disabled: %v", err)
	}
}
