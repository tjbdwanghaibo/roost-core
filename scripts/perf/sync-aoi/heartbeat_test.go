package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
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
//
// 测量窗口按事件结束：每个移动者在 loadTick > 0 时至少写过两次位置（有心跳间隔样本）才停，不按固定的 0.6 秒。
// 驱动的核对用确定的计数（heartbeatAccountingHolds），不要求零跳拍：上一拍在下一拍到期时还没执行就跳过这一拍
// （inflight），是驱动在宿主调度迟到时的设计行为（每个登记至多一拍在途、迟到不补投，见 gameplay/heartbeat 的
// TestInflightBeatSkipsCountsAndWarnsAtALimitedRate）。零跳拍是压测的有效性门槛，由正式压测的 verdict 判定；
// 这里在负载高的宿主上曾因 50ms 档迟到出现 inflight 跳拍而失败（2026-11-04，投递 106 = 执行 106）。
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
	// 压测端每 50ms 推进一个 tick（tick 是负载节拍，不是同步手段）；至少 12 个 tick，之后等每个移动者在测量窗口里
	// 写过两次位置，至多 200 个 tick（约 10 秒）。
	allMovedTwice := func() bool {
		for _, s := range moving.movers {
			if s.mover.measured.Load() < 2 {
				return false
			}
		}
		return true
	}
	for tick := 1; tick <= 12 || !allMovedTwice(); tick++ {
		if tick > 200 {
			counts := make([]int64, 0, len(moving.movers))
			for _, s := range moving.movers {
				counts = append(counts, s.mover.measured.Load())
			}
			t.Fatalf("movers did not each move twice in 200 ticks: measured moves %v; report %+v", counts, moving.report())
		}
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
	if err := heartbeatAccountingHolds(moving); err != nil {
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

// heartbeatAccountingHolds 是驱动停下、Nest 排空之后与调度时机无关的核对：投递出去的每一拍都执行了，或在执行侧
// 按“已停止 / 重复”丢弃（没有丢失的拍）；没有准入失败、卡住与动作失败；测量窗口里确实有心跳改值。本用例的实体不卸载、
// 不被替换、查找表固定、不是 Remote 托管，其余丢弃原因不会出现。inflight 跳拍不在其中：它只在上一拍还在途时发生，
// 在途的那一拍照样计在投递 = 执行里。
func heartbeatAccountingHolds(l *heartbeatLoad) error {
	s := l.driver.Stats()
	if n := s.DroppedRemoved + s.DroppedStale + s.DroppedNotLoaded + s.DroppedNotOwner + s.StoppedShared; n > 0 {
		return fmt.Errorf("heartbeat dropped %d beats for reasons this fixture never produces", n)
	}
	if s.Delivered != s.Executed+s.DroppedStopped+s.DroppedDuplicate {
		return fmt.Errorf("heartbeat delivered %d beats but executed %d (dropped stopped %d, duplicate %d): a delivered beat was lost",
			s.Delivered, s.Executed, s.DroppedStopped, s.DroppedDuplicate)
	}
	if s.SkippedQueueFull+s.SkippedRejected+s.Stalled > 0 {
		return fmt.Errorf("heartbeat admission failed or stalled (queue_full %d, rejected %d, stalled %d)", s.SkippedQueueFull, s.SkippedRejected, s.Stalled)
	}
	if n := l.stats.failed.Load(); n > 0 {
		first := ""
		if p := l.stats.firstErr.Load(); p != nil {
			first = *p
		}
		return fmt.Errorf("%d move action failures, first: %s", n, first)
	}
	l.stats.mu.Lock()
	samples := len(l.stats.intervals)
	l.stats.mu.Unlock()
	if s.Executed == 0 || samples == 0 {
		return errors.New("heartbeat mode measured no beats")
	}
	return nil
}

// -heartbeat-delivery 透传到驱动（KNOWN-LIMITS“心跳模式的投递方式没有开关”）：空串是驱动缺省（批量作业），dispatch / batch
// 按值设定，记进 server.json 的 heartbeat.delivery；未知值与全局循环模式下给值都在校验时拒绝。
func TestHeartbeatDeliveryFlagReachesTheDriver(t *testing.T) {
	base := config{Nest: true, Mode: "periodic", ApplyMode: "sync", Players: 2, Entities: 16, Ticks: 10, Hz: 20, Dirty: 50, Padding: 8, Batches: 1,
		Visible: 32, Block: 150, EnterRadius: 40, LeaveRadius: 40, MoveAmplitude: 48,
		MoveDriver: moveDriverHeartbeat, MovePeriod: 50 * time.Millisecond, MoveSpeed: 400, Output: "unused", Role: "server"}
	for _, tc := range []struct {
		flag string
		want heartbeat.Delivery
	}{{"", heartbeat.DefaultDelivery}, {"dispatch", heartbeat.DeliveryDispatch}, {"batch", heartbeat.DeliveryBatch}} {
		c := base
		c.HeartbeatDelivery = tc.flag
		if err := c.validate(); err != nil {
			t.Fatalf("-heartbeat-delivery=%q: %v", tc.flag, err)
		}
		driver, err := newLoadDriver(c)
		if err != nil {
			t.Fatal(err)
		}
		load := &heartbeatLoad{driver: driver, stats: &moveStats{}, c: c}
		if got := driver.Delivery(); got != tc.want {
			t.Fatalf("-heartbeat-delivery=%q: driver delivery %q, want %q", tc.flag, got, tc.want)
		}
		if got := load.report()["delivery"]; got != string(tc.want) {
			t.Fatalf("-heartbeat-delivery=%q: report delivery %v, want %q", tc.flag, got, tc.want)
		}
	}
	c := base
	c.HeartbeatDelivery = "fanout"
	if err := c.validate(); err == nil {
		t.Fatal("an unknown delivery was accepted")
	}
	c = base
	c.MoveDriver, c.HeartbeatDelivery = moveDriverLoop, "dispatch"
	if err := c.validate(); err == nil {
		t.Fatal("-heartbeat-delivery was accepted with the loop driver, where it does nothing")
	}
}
