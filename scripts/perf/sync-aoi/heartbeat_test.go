package main

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 心跳模式的改值速率与全局循环相同：Nest 模式压测端每秒 20 个 tick、每 tick 改 count 个实体；心跳模式每个移动者
// 每拍一次改值。
func TestHeartbeatMoversMatchTheLoopChangeRate(t *testing.T) {
	for _, tc := range []struct {
		entities, dirty int
		period          time.Duration
		movers, want    int
	}{
		{10000, 1, 100 * time.Millisecond, 0, 200}, // 100 个/tick × 20/s = 2000/s，每个移动者 10 拍/s
		{10000, 1, 50 * time.Millisecond, 0, 100},
		{10000, 1, time.Second, 0, 2000},
		{100, 100, time.Second, 0, 100}, // 封顶为实体数
		{10000, 1, 100 * time.Millisecond, 7, 7},
	} {
		c := config{Entities: tc.entities, Dirty: tc.dirty, MovePeriod: tc.period, Movers: tc.movers}
		if got := moversFor(c); got != tc.want {
			t.Errorf("moversFor(%d entities, %d%%, %v, override %d) = %d, want %d", tc.entities, tc.dirty, tc.period, tc.movers, got, tc.want)
		}
	}
	c := config{Entities: 10000}
	picked := moverIndexes(c, 200)
	sorted := slices.Clone(picked)
	slices.Sort(sorted)
	if len(slices.Compact(sorted)) != 200 {
		t.Fatalf("mover indexes repeat: %v", picked)
	}
}

// 客户端对心跳模式只能核对巡逻带（位置取决于心跳时机）；全局循环仍按 Step 逐项核对。
func TestClientVerifiesThePatrolBandPerMoveDriver(t *testing.T) {
	c := config{Entities: 100, MoveAmplitude: 48}
	id := int64(7)
	base := patrolBase(id, c)
	loop := position(id, 5, c)
	if base.Y != loop.Y || loop.X < base.X || loop.X > base.X+48 {
		t.Fatalf("loop position %+v is off the band starting at %+v", loop, base)
	}
	for _, tc := range []struct {
		driver string
		at     spatial.Point
		step   int64
		ok     bool
	}{
		{moveDriverLoop, loop, 5, true},
		{moveDriverLoop, spatial.Point{X: loop.X + 1, Y: loop.Y}, 5, false}, // 全局循环：差一格也不行
		{moveDriverHeartbeat, spatial.Point{X: base.X + 17, Y: base.Y}, 3, true},
		{moveDriverHeartbeat, spatial.Point{X: base.X + 48, Y: base.Y}, 3, true},
		{moveDriverHeartbeat, spatial.Point{X: base.X + 49, Y: base.Y}, 3, false},
		{moveDriverHeartbeat, spatial.Point{X: base.X - 1, Y: base.Y}, 3, false},
		{moveDriverHeartbeat, spatial.Point{X: base.X, Y: base.Y + 1}, 3, false},
	} {
		c.MoveDriver = tc.driver
		if got := verifyPatrol(component{ID: id, Step: tc.step, X: tc.at.X, Y: tc.at.Y}, c); got != tc.ok {
			t.Errorf("%s at %+v step %d: accepted %v, want %v", tc.driver, tc.at, tc.step, got, tc.ok)
		}
	}
	for leg := range uint64(200) {
		current := base.X + int64(leg%49)
		if got := nextTarget(id, leg, base, 48, current); got.Y != base.Y || got.X < base.X || got.X > base.X+48 || got.X == current {
			t.Fatalf("leg %d from x=%d: target %+v (band %+v + 48)", leg, current, got, base)
		}
	}
}

// 端到端（几百毫秒、16 个实体，不是性能测量）：真实 Nest + 心跳驱动 + 同步 Manager + Interest，移动者各自巡逻，
// 模拟客户端按心跳模式逐帧核对（版本链、巡逻带、HP 与 Step 一致、最终可见集合）。另外按最终位置核对 AOI：进入与
// 离开半径相同（无滞回）、上限够大时，每个玩家看到的就是半径内的其他实体——位置没有经 QueueMove 进 AOI 时这里不成立。
func TestHeartbeatPatrolReachesClientsAndTheAOI(t *testing.T) {
	c := config{Nest: true, Mode: "periodic", ApplyMode: "sync", Players: 2, Entities: 16, Ticks: 10, Hz: 20, Dirty: 50, Padding: 8, Batches: 1,
		Visible: 32, Block: 150, EnterRadius: 40, LeaveRadius: 40, MoveAmplitude: 48,
		MoveDriver: moveDriverHeartbeat, MovePeriod: 50 * time.Millisecond, MoveSpeed: 400, Output: "unused", Role: "server"}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	conns := make(map[int64]net.Conn)
	results := make(map[int64]chan clientResult)
	for _, id := range observerIDs(c) {
		server, client := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		conns[id] = server
		done := make(chan clientResult, 1)
		results[id] = done
		go func() { defer client.Close(); done <- consume(client, c, func() {}) }()
	}
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Interval: 50 * time.Millisecond, Limits: frameLimits(c), Transport: entitysync.TransportFunc(func(_ context.Context, sid entitysync.SessionID, raw []byte) error {
		conn := conns[int64(sid)]
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		return writePacket(conn, packetFrame, int(loadTick.Load()), time.Now().UnixNano(), raw)
	})})
	if err != nil {
		t.Fatal(err)
	}
	interest, err := policy.NewInterest(policy.InterestConfig{Manager: manager, AOI: policy.AOIConfig{Bounds: worldBounds(c), BlockSize: c.Block, EnterRadius: c.EnterRadius, LeaveRadius: c.LeaveRadius, MaxVisible: c.Visible - 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer interest.Close()
	players := map[int64]bool{}
	for _, id := range observerIDs(c) {
		players[id] = true
	}
	states := make([]*subject, c.Entities)
	for i := range states {
		id := workloadID(int64(i+1), c)
		at := position(id, 0, c)
		s := &subject{data: component{ID: id, X: at.X, Y: at.Y, HP: 100, Extra: make([]byte, c.Padding)}}
		for j := range s.data.Extra {
			s.data.Extra[j] = byte(id % 251)
		}
		s.state = entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "aoi-load", Packer: s})
		s.EntityBase = entity.NewEntityBase(id, entity.EntityCategory(4), true, loadEntityKind)
		s.SetSyncState(s.state)
		states[i] = s
		if err := manager.Register(s.state); err != nil {
			t.Fatal(err)
		}
		if players[id] {
			if err := manager.OpenSession(entitysync.SessionID(id)); err != nil {
				t.Fatal(err)
			}
			err = interest.Enter(id, id, at, policy.ObserverView{})
		} else {
			err = interest.Show(id, at)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if r := interest.Apply(); len(r) > 0 {
		t.Fatalf("initial refusal %v", r[0])
	}
	moving, err := newHeartbeatLoad(states, c, interest)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(moving.movers); got != 8 { // 16 × 50% = 8 个/tick × 20/s ÷ 20 拍/s
		t.Fatalf("movers %d, want 8", got)
	}
	engine, getter, err := newLoadNest(states, c, manager, interest, &splitTimings{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	ctx := context.Background()
	loadTick.Store(0)
	for range 5 {
		if err := manager.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := moving.start(ctx, engine, getter); err != nil {
		t.Fatal(err)
	}
	for tick := 1; tick <= 12; tick++ { // 约 0.6 秒
		loadTick.Store(int64(tick))
		time.Sleep(50 * time.Millisecond)
	}
	if err := moving.stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := moving.verdict(); err != nil {
		t.Fatalf("%v; report %+v", err, moving.report())
	}
	// AOI 与最终位置一致（位置都经 QueueMove 进了 AOI）。
	moved := 0
	for _, s := range moving.movers {
		if s.data.Step > 0 {
			moved++
		}
		if !verifyPatrol(s.data, c) {
			t.Fatalf("mover %d left its band: %+v", s.data.ID, s.data)
		}
	}
	if moved != len(moving.movers) {
		t.Fatalf("%d of %d movers changed position in the measured ticks", moved, len(moving.movers))
	}
	for id := range players {
		var self spatial.Point
		var want []int64
		for _, s := range states {
			if s.data.ID == id {
				self = spatial.Point{X: s.data.X, Y: s.data.Y}
			}
		}
		for _, s := range states {
			if s.data.ID != id && spatial.WithinDistance(self, spatial.Point{X: s.data.X, Y: s.data.Y}, c.EnterRadius) {
				want = append(want, s.data.ID)
			}
		}
		got := slices.DeleteFunc(interest.Visible(id), func(v int64) bool { return v == id })
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("player %d at %+v sees %v, want the entities within %d at their final positions %v", id, self, got, c.EnterRadius, want)
		}
	}
	samples, deltas := 0, 0
	for id, conn := range conns {
		raw, _ := json.Marshal(interest.Seen(id))
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := writePacket(conn, packetFinish, 0, 0, raw); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-results[id]:
			if r.err != nil {
				t.Fatalf("client %d rejected the heartbeat stream: %v", id, r.err)
			}
			samples += len(r.changed)
			deltas += len(r.live)
		case <-time.After(5 * time.Second):
			t.Fatalf("client %d did not finish", id)
		}
	}
	report := moving.report()
	t.Logf("movers %d, client samples %d (deltas %d), heartbeat %+v", len(moving.movers), samples, deltas, report)
	if samples == 0 || deltas == 0 {
		t.Fatalf("client latency samples %d, of which deltas %d: heartbeat changes did not reach visible clients as updates", samples, deltas)
	}
}
