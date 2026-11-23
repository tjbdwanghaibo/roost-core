package nestgame

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

func settleUnits(t *testing.T, n int) []*Unit {
	t.Helper()
	RegisterEntity()
	access := entity.NewManagerAccess(entity.NewEntityManager())
	units := make([]*Unit, n)
	for i := range units {
		id, err := entity.BuildEntityID(int64(i+1), kindUnit)
		if err != nil {
			t.Fatal(err)
		}
		u, err := access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: kindUnit, Id: id})
		if err != nil {
			t.Fatal(err)
		}
		units[i] = u.(*Unit)
	}
	return units
}

func TestSettleStrictCapMembersAndValues(t *testing.T) {
	if sessionObjectLimit != 100 {
		t.Fatalf("fixture session cap = %d, want 100", sessionObjectLimit)
	}
	load := &syncGameLoad{units: settleUnits(t, sessionObjectLimit+6), clients: make([]syncLoadClient, 1)}
	values := make(map[int64]syncClientObject)
	for i, u := range load.units {
		x, hp := int64(i+1), int64(1000+i)
		u.state.SetX(x)
		u.state.SetHP(hp)
		values[u.ID()] = syncClientObject{value: syncClientValue{X: x, HP: hp}}
	}
	held := make(map[int64]syncClientObject)
	load.clients[0].objects = held
	for _, total := range []int{5, sessionObjectLimit, sessionObjectLimit + 5} {
		clear(held)
		w := settleWanted{self: load.units[0].ID(), covered: make(map[int64]struct{})}
		for _, u := range load.units[:total] {
			w.covered[u.ID()] = struct{}{}
		}
		// 同一对象还经距离和 self 可见；不能重复计数，封顶时只指定 self 必选。
		w.visible = []int64{load.units[0].ID(), load.units[1].ID()}
		n := min(total, sessionObjectLimit)
		for _, u := range load.units[:n] {
			held[u.ID()] = values[u.ID()]
		}
		check := func(ok bool) settleMismatch {
			t.Helper()
			m, got := load.checkClient(0, w, true)
			if got != ok || m.seen != total || m.want != n {
				t.Fatalf("total=%d want=%v count=%d/%d result=%v", total, ok, m.got, m.want, got)
			}
			return m
		}
		check(true)
		delete(held, w.self)
		missing := check(false)
		msg := missing.detail(func(int64) bool { return false })
		if missing.got != n-1 || !strings.Contains(msg, fmt.Sprintf("missing pinned self=%d(subscribed=false)", w.self)) {
			t.Fatalf("total=%d: %s", total, msg)
		}
		held[w.self] = values[w.self]
		id := load.units[n-1].ID()
		delete(held, id)
		m := check(false)
		if m.want-m.got != 1 {
			t.Fatalf("short=%d", m.want-m.got)
		}
		lookups := 0
		for k := range held {
			delete(held, k)
		} // 终止详情必须来自核对时的数据，不能重取客户端
		msg = m.detail(func(int64) bool { lookups++; return true })
		if lookups != min(4, total-(n-1)) || !strings.Contains(msg, "subscribed=true") || strings.Contains(msg, "missing pinned self") {
			t.Fatalf("queries=%d: %s", lookups, msg)
		}
		for _, u := range load.units[:n-1] {
			held[u.ID()] = values[u.ID()]
		}
		for _, bad := range []int64{0, load.units[total].ID()} {
			held[bad] = syncClientObject{}
			m := check(false) // 等数量错成员，包括不可解码的 ID，不能访问 DAO 越界
			if msg := m.detail(func(int64) bool { return false }); !strings.Contains(msg, fmt.Sprintf("extra=[%d]", bad)) {
				t.Fatalf("wrong member %d: %s", bad, msg)
			}
			delete(held, bad)
		}
		held[id] = values[id]
		for key, obj := range held { // 任意一个对象落后都不能通过，不只查首项或 self
			x, hp := obj.value.X, obj.value.HP
			for _, v := range []syncClientValue{{X: x - 1, HP: hp}, {X: x, HP: hp - 1}} {
				held[key] = syncClientObject{value: v}
				m := check(false)
				if !m.valueMismatch || m.id != key || m.expected != [2]int64{x, hp} || m.actual != [2]int64{v.X, v.HP} {
					t.Fatalf("total=%d: lost DAO mismatch for subject %d: want=%v got=%v", total, key, m.expected, m.actual)
				}
			}
			held[key] = obj
		}
		check(true)
	}
}

// 同一批 105 个合法 ID，持有数同为 100 时，仅遗漏普通对象和遗漏自己的结果必须不同。
func TestSettleFullSubsetRequiresPinnedSelf(t *testing.T) {
	load := &syncGameLoad{units: settleUnits(t, 105), clients: make([]syncLoadClient, 1)}
	w := settleWanted{self: load.units[0].ID(), covered: make(map[int64]struct{})}
	for _, u := range load.units {
		w.covered[u.ID()] = struct{}{}
	}
	held := make(map[int64]syncClientObject)
	load.clients[0].objects = held
	for _, u := range load.units[5:] { // 旧正例：末尾 100 个全合法、值也全对，但恰好没有自己的 Unit。
		held[u.ID()] = syncClientObject{}
	}
	m, ok := load.checkClient(0, w, true)
	if ok || m.seen != 105 || m.want != 100 || m.got != 100 {
		t.Fatalf("missing self in full legal subset: ok=%v seen/want/got=%d/%d/%d", ok, m.seen, m.want, m.got)
	}
	delete(held, load.units[5].ID())
	held[w.self] = syncClientObject{}
	if _, ok := load.checkClient(0, w, true); !ok { // 自己 + 最后 99 个，而非固定的前 100 个。
		t.Fatal("rejected self with 99 legal non-pinned objects")
	}
	// 诊断使用失败时的快照；其余合法暂缓项不能挤掉 self 证据或造成无界订户查询。
	queries := make(map[int64]int)
	msg := m.detail(func(id int64) bool { queries[id]++; return id == w.self })
	for id, n := range queries {
		if n != 1 {
			t.Fatalf("duplicate diagnostic lookup of %d: %s", id, msg)
		}
	}
	if len(queries) != 4 || queries[w.self] != 1 || !strings.Contains(msg, fmt.Sprintf("missing pinned self=%d(subscribed=true)", w.self)) || !strings.Contains(msg, "extra=[]") {
		t.Fatalf("missing pinned diagnostic or lookup bound: queries=%v: %s", queries, msg)
	}
	held[load.units[1].ID()] = syncClientObject{}
	if m, ok := load.checkClient(0, w, true); ok || m.got != 101 || m.want != 100 {
		t.Fatalf("101 legal objects including self: ok=%v want/got=%d/%d", ok, m.want, m.got)
	}
	delete(held, load.units[104].ID())
	if _, ok := load.checkClient(0, w, true); !ok { // 换掉另一个非钉住对象后仍合法。
		t.Fatal("rejected an alternative legal subset retaining self")
	}
}

type testSettleNetwork struct {
	loadFrontend
	failure func() error
}

func (n testSettleNetwork) currentFailure() error { return n.failure() }

func TestSettleAllClientsDeadlineAndNetwork(t *testing.T) {
	load := &syncGameLoad{units: settleUnits(t, 4), self: true, clients: make([]syncLoadClient, 4)}
	for i := range load.clients {
		load.clients[i].objects = map[int64]syncClientObject{load.units[i].ID(): {}}
	}
	netErr := errors.New("test read failed")
	for _, failAt := range []int{1, 2} {
		calls := 0
		n := testSettleNetwork{failure: func() error {
			calls++
			if calls == failAt {
				return netErr
			}
			return nil
		}}
		if _, err := load.waitViews(context.Background(), n, t.Logf); !errors.Is(err, netErr) {
			t.Fatalf("matching objects masked network failure at check %d: %v", failAt, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	calls := 0
	net := testSettleNetwork{failure: func() error {
		calls++
		if calls == 2 {
			<-ctx.Done()
		}
		return nil
	}}
	if _, err := load.waitViews(ctx, net, t.Logf); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("accepted success past deadline: %v", err)
	}
	ctx = context.Background()
	check := func(full bool, count, behind, short int) {
		t.Helper()
		r, err := load.scanViews(ctx, full)
		if err != nil || r.checked != count || r.behind != behind || r.short != short {
			t.Fatalf("full=%v: got %+v err=%v", full, r, err)
		}
	}
	check(false, 4, 0, 0)
	load.clients[3].objects[load.units[3].ID()] = syncClientObject{value: syncClientValue{HP: 1}}
	check(false, 4, 1, 0) // 最后一名客户端错也不成功
	clear(load.clients[1].objects)
	check(false, 2, 1, 1)
	check(true, 4, 2, 1) // 等数量错值也算 behind，但不算 short
}

// realSettleScene 用正式生成的 Unit/DAO 和真正 Manager/Interest；不启后台调度，
// 每次 Flush 是一个可观察的政策阶段。小 sessionLimit 能在十来个对象上实际触发收紧和放宽。
func realSettleScene(t *testing.T, players, entities, sessionLimit int) *syncGameLoad {
	t.Helper()
	l := &syncGameLoad{units: settleUnits(t, entities), config: gameConfig{Players: players, Entities: entities, ObservedBlocks: 4}, clients: make([]syncLoadClient, players), sessions: make(map[int64]entitysync.SessionID), bounds: spatial.Rect{Max: spatial.Point{X: 1800, Y: 1800}}}
	var err error
	l.layout, err = spatial.NewBlockLayout(l.bounds, loadBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	l.manager, err = entitysync.NewManager(entitysync.ManagerConfig{Transport: spatialSink{}, Limits: frame.Limits{MaxObjects: sessionLimit}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.manager.Close(context.Background()) })
	l.interest, err = policy.NewInterest(policy.InterestConfig{Manager: l.manager, AOI: policy.AOIConfig{Bounds: l.bounds, BlockSize: loadBlockSize, EnterRadius: 150, LeaveRadius: loadLeaveRadius, MaxVisible: 49}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.interest.Close)
	for i, u := range l.units {
		enableUnitSync(u)
		if err := l.manager.Register(u.Sync()); err != nil {
			t.Fatal(err)
		}
		if i < players {
			sid := entitysync.SessionID(101 + i)
			if err := l.openSession(i, sid); err != nil {
				t.Fatal(err)
			}
			if err := l.manager.ReadySession(sid); err != nil {
				t.Fatal(err)
			}
			if err := l.interest.Enter(int64(sid), u.ID(), l.position(u), policy.ObserverView{}); err != nil {
				t.Fatal(err)
			}
		} else if err := l.interest.Show(u.ID(), l.position(u)); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// 给全部客户端放入权威 Seen∪self，同一条严格核对应通过；若数量、任一成员或值不同即失败。
// 在这两个小场景里 Seen 始终不超过夹具缺省的 sessionObjectLimit，因而这是集合相等，不是子集检查。
func compareRealSeen(t *testing.T, load *syncGameLoad) {
	t.Helper()
	for i := range load.clients {
		id := load.units[i].ID()
		observer, _ := load.observerOf(id)
		seen := append(load.interest.Seen(observer), id)
		held := make(map[int64]syncClientObject)
		for _, id := range seen {
			s := load.units[entity.GetUniqueIDFromEntityID(id)-1].state
			held[id] = syncClientObject{value: syncClientValue{X: s.GetX(), HP: s.GetHP()}}
		}
		if len(held) > sessionObjectLimit {
			t.Fatal("test cannot compare full equality above the session limit")
		}
		load.clients[i].objects = held
	}
	r, err := load.scanViews(context.Background(), true)
	if err != nil || r.first != nil || r.checked != len(load.clients) {
		t.Fatalf("Seen agreement: behind=%d first=%+v err=%v", r.behind, r.first, err)
	}
}

func TestSettleSeenBlockBoundaryAndCommittedRedeclaration(t *testing.T) {
	l := realSettleScene(t, 9, 100, sessionObjectLimit)
	compareRealSeen(t, l)
	watcher, target := l.units[2], l.units[8] // x=260,440 in this fixture
	observer, _ := l.observerOf(watcher.ID())
	sees := func(want bool) {
		t.Helper()
		if got := slices.Contains(l.interest.Seen(observer), target.ID()); got != want {
			t.Fatalf("watcher at %v sees target at %v = %v, want %v", l.position(watcher), l.position(target), got, want)
		}
		compareRealSeen(t, l)
	}
	sees(false)
	block := func(x int64) int64 { return l.interest.BlockAt(spatial.Point{X: x, Y: 200}) }
	// 距离进入半径 150；声明 x=450 的块，只有视野右端（含边界）到达 450 时才看见。
	if err := l.interest.QueueObservedBlocks(target, []int64{block(450), block(450)}); err != nil {
		t.Fatal(err)
	}
	watcher.state.SetX(4) // 268+180=448
	if err := l.interest.QueueMove(watcher, l.position(watcher)); err != nil {
		t.Fatal(err)
	}
	sees(false)
	watcher.state.SetX(5) // 270+180=450
	if err := l.interest.QueueMove(watcher, l.position(watcher)); err != nil {
		t.Fatal(err)
	}
	sees(true)
	if err := l.interest.QueueObservedBlocks(target, []int64{block(150), block(300), block(450), block(300)}); err != nil {
		t.Fatal(err)
	}
	sees(true)
	// 目标从 x=440 跨到 450；同一提交重新声明覆盖。未确认的事实仍不可见，确认后旧覆盖全部退出。
	mut := entity.BeginSyncMutation([]entity.IThreadSafeEntity{target}, l.manager)
	target.state.SetX(5)
	if err := l.interest.QueueSpatial(target, l.position(target), []int64{block(900)}); err != nil {
		t.Fatal(err)
	}
	mut.Admit()
	if !slices.Contains(l.interest.Seen(observer), target.ID()) {
		t.Fatal("unconfirmed coverage replaced live held")
	}
	mut.Confirm()
	mut.Release()
	// 不先 Apply/Seen：把客户端留在旧集合，核对必须发现已确认的重新声明，而非跨轮复用旧块。
	r, err := l.scanViews(context.Background(), true)
	if err != nil || r.first == nil {
		t.Fatalf("changed view passed stale clients: %+v err=%v", r, err)
	}
	sees(false)
}

func TestSettleSeenActuallyTightensAndRelaxes(t *testing.T) {
	l := realSettleScene(t, 2, 10, 4)
	compareRealSeen(t, l)
	id, _ := l.observerOf(l.units[0].ID())
	initial := len(l.interest.Visible(id))
	for range 5 {
		if err := l.manager.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tight := len(l.interest.Visible(id))
	if tight >= initial || l.manager.Counters().OverLimitDeferred == 0 {
		t.Fatalf("no real tightening: visible %d -> %d counters=%+v", initial, tight, l.manager.Counters())
	}
	compareRealSeen(t, l)
	old := maps.Clone(l.clients[0].objects)
	// 只推进结算，不再 Flush 发新暂缓；在真正发生冷却放宽的那一步对照。
	deadline := time.Now().Add(3 * time.Second)
	for len(l.interest.Visible(id)) <= tight {
		if time.Now().After(deadline) {
			t.Fatalf("view never relaxed beyond %d", tight)
		}
		time.Sleep(10 * time.Millisecond)
		l.interest.Apply()
	}
	r, err := l.scanViews(context.Background(), true)
	if err != nil || r.first == nil {
		t.Fatalf("tightened set still passed after relaxation: %+v %v", r, err)
	}
	compareRealSeen(t, l)
	if len(old) >= len(l.clients[0].objects) {
		t.Fatal("relaxation did not change authoritative held")
	}
}

func TestSettleFlushWindow(t *testing.T) {
	before := entitysync.ManagerCounters{FlushCalls: 9, FlushDuration: 18 * time.Millisecond}
	after := entitysync.ManagerCounters{FlushCalls: 11, FlushDuration: 27 * time.Millisecond, LastFlushDuration: time.Second}
	if f := flushBetween(before, after); f.calls != 2 || f.ms != 9 || f.avgMS != 4.5 {
		t.Fatalf("not a window delta: %+v", f)
	}
	if f := flushBetween(after, after); f != (settleFlush{}) {
		t.Fatalf("zero calls: %+v", f)
	}
}
