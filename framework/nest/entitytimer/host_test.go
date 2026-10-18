package entitytimer

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/timer"
)

// 相同宿主、存量节点与周期触发负载，用于比较 Tick 的调度视图管理成本。
func BenchmarkHostPeriodicTick(b *testing.B) {
	for _, count := range []int{1, 128} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			now := time.Unix(1_800_000_000, 0)
			d := NewDriver(Config{Name: "bench.timer", Handler: nest.NewHandlerName("bench_tick")})
			d.SetClock(func() time.Time { return now })
			host := d.NewHost(newOwner(301), newMemStore(), func(h *Handlers) {
				h.Register(typeIncome, func(Fire) time.Duration { return time.Second })
			})
			if err := host.OnInitFinish(true); err != nil {
				b.Fatal(err)
			}
			for range count {
				host.NewTimer(time.Second, typeIncome, 0, 0, nil)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				now = now.Add(time.Second)
				host.Tick(now)
			}
		})
	}
}

// memStore 是测试里的“DAO”：节点 map 与种子。OnDestroy 之后它留着，就是实体卸载后存储里的样子。
// 在 Nest 事务里写入时登记一次整份快照的逆操作，回滚即还原（生成 DAO 的 mutator 也是随事务回滚）。
type memStore struct {
	nodes map[int64]timer.Node
	seed  int64
}

func newMemStore() *memStore { return &memStore{nodes: make(map[int64]timer.Node)} }

func (s *memStore) undo() {
	nodes, seed := maps.Clone(s.nodes), s.seed
	nest.RecordUndo(s, 1, func() error {
		s.nodes, s.seed = nodes, seed
		return nil
	})
}

func (s *memStore) SavedTimers() []timer.Node {
	out := make([]timer.Node, 0, len(s.nodes))
	for _, node := range s.nodes {
		node.Payload = slices.Clone(node.Payload)
		out = append(out, node)
	}
	return out
}
func (s *memStore) TimerSeed() int64 { return s.seed }
func (s *memStore) PutTimer(node timer.Node) {
	s.undo()
	s.nodes[node.ID] = timer.Node{ID: node.ID, Type: node.Type, Priority: node.Priority, Param1: node.Param1, Param2: node.Param2,
		Payload: slices.Clone(node.Payload), End: time.UnixMilli(node.End.UnixMilli()), Delay: node.Delay}
}
func (s *memStore) DeleteTimer(id int64) { s.undo(); delete(s.nodes, id) }
func (s *memStore) SetTimerSeed(seed int64) {
	s.undo()
	s.seed = seed
}

const (
	typeBuff   int32 = 1
	typeIncome int32 = 2
)

// fires 记录回调收到的 Fire；income 回调按 interval 续期（周期定时器）。
type fires struct {
	got      []Fire
	interval time.Duration
}

func (f *fires) register(handlers *Handlers) {
	handlers.Register(typeBuff, func(fire Fire) time.Duration { f.got = append(f.got, fire); return 0 })
	handlers.Register(typeIncome, func(fire Fire) time.Duration { f.got = append(f.got, fire); return f.interval })
}

// tickDelivered 扮演生成的 Tick handler：取出驱动投递的消息，在“锁内”调用 host.Tick。
func tickDelivered(t *testing.T, d *Driver, client *recordingClient, host *Host) int {
	t.Helper()
	d.DeliverDue(context.Background())
	sent := client.take()
	for _, tick := range sent {
		if tick.id != host.owner.ID() {
			t.Fatalf("tick for %d delivered to the host of %d", tick.id, host.owner.ID())
		}
		host.Tick(time.UnixMilli(tick.now))
	}
	return len(sent)
}

// 在线：到期由驱动投递一次，回调拿到原定期限与触发时刻、不是补触发；一次性定时器结束后索引清空。
func TestHostFiresAnOnlineDeadlineOnce(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	owner := newOwner(101)
	live.set(owner)
	store := newMemStore()
	f := &fires{}
	host := d.NewHost(owner, store, f.register)
	if err := host.OnInitFinish(true); err != nil {
		t.Fatal(err)
	}
	armedAt := clk.Now()
	id := host.NewTimer(5*time.Second, typeBuff, 7, 0, []byte("double-gold"))
	if id != 1 {
		t.Fatalf("first id = %d", id)
	}
	if due, ok := d.DueOf(owner); !ok || !due.Equal(armedAt.Add(5*time.Second)) {
		t.Fatalf("indexed due = %v %v", due, ok)
	}
	clk.Advance(4 * time.Second)
	if tickDelivered(t, d, client, host) != 0 {
		t.Fatal("delivered before the deadline")
	}
	clk.Advance(1500 * time.Millisecond)
	if tickDelivered(t, d, client, host) != 1 {
		t.Fatal("not delivered at the deadline")
	}
	if len(f.got) != 1 {
		t.Fatalf("fired %d times", len(f.got))
	}
	fire := f.got[0]
	if fire.ID != id || fire.Type != typeBuff || fire.Param1 != 7 || string(fire.Payload) != "double-gold" ||
		!fire.Deadline.Equal(armedAt.Add(5*time.Second)) || !fire.Now.Equal(clk.Now()) || fire.Overdue || !fire.LoadedAt.IsZero() ||
		fire.Interval != 5*time.Second || fire.Late() != 500*time.Millisecond {
		t.Fatalf("fire = %+v", fire)
	}
	if len(store.nodes) != 0 || d.Pending() != 0 {
		t.Fatalf("after a one-shot fired: nodes %d, pending %d", len(store.nodes), d.Pending())
	}
	clk.Advance(time.Hour)
	if tickDelivered(t, d, client, host) != 0 || len(f.got) != 1 {
		t.Fatal("a finished timer was delivered again")
	}
}

// 建造完成接着派兵、抵达后取消超时：回调内的组件操作必须与当前 Tick 使用同一调度视图。
func TestHostCallbackOperationsShareTheCurrentTick(t *testing.T) {
	t.Run("schedule next stage", func(t *testing.T) {
		d, clk, client, live := newTestDriver(t)
		owner := newOwner(201)
		live.set(owner)
		store := newMemStore()
		var host *Host
		fired := 0
		host = d.NewHost(owner, store, func(h *Handlers) {
			h.Register(typeBuff, func(Fire) time.Duration {
				clk.Advance(time.Second) // 后续任务从它实际登记的业务时间起算。
				host.NewTimer(time.Minute, typeIncome, 0, 0, nil)
				return 0
			})
			h.Register(typeIncome, func(Fire) time.Duration { fired++; return 0 })
		})
		if err := host.OnInitFinish(true); err != nil {
			t.Fatal(err)
		}
		host.NewTimer(time.Second, typeBuff, 0, 0, nil)
		clk.Advance(time.Second)
		tickDelivered(t, d, client, host)
		if due, ok := d.DueOf(owner); !ok || !due.Equal(clk.Now().Add(time.Minute)) {
			t.Fatalf("next-stage timer persisted=%d but driver due=%v present=%v", len(store.nodes), due, ok)
		}
		clk.Advance(time.Minute)
		tickDelivered(t, d, client, host)
		if fired != 1 {
			t.Fatalf("next stage fired %d times, want 1", fired)
		}
	})
	t.Run("cancel another due stage", func(t *testing.T) {
		d, clk, client, live := newTestDriver(t)
		owner := newOwner(202)
		live.set(owner)
		store := newMemStore()
		var host *Host
		var canceledID int64
		fired := 0
		host = d.NewHost(owner, store, func(h *Handlers) {
			h.Register(typeBuff, func(Fire) time.Duration {
				if !host.RemoveTimer(canceledID) {
					t.Error("cancel refused a live timer")
				}
				return 0
			})
			h.Register(typeIncome, func(Fire) time.Duration { fired++; return 0 })
		})
		if err := host.OnInitFinish(true); err != nil {
			t.Fatal(err)
		}
		host.NewTimer(time.Second, typeBuff, 0, 0, nil)
		canceledID = host.NewTimer(time.Second, typeIncome, 0, 0, nil)
		clk.Advance(time.Second)
		tickDelivered(t, d, client, host)
		if fired != 0 {
			t.Fatalf("canceled stage still fired %d times", fired)
		}
	})
	t.Run("reschedule current stage", func(t *testing.T) {
		d, clk, client, live := newTestDriver(t)
		owner := newOwner(203)
		live.set(owner)
		store := newMemStore()
		var host *Host
		calls := 0
		host = d.NewHost(owner, store, func(h *Handlers) {
			h.Register(typeBuff, func(fire Fire) time.Duration {
				calls++
				if calls == 1 && !host.ChangeTimer(fire.ID, time.Minute) {
					t.Error("current stage was not found")
				}
				return 0
			})
		})
		if err := host.OnInitFinish(true); err != nil {
			t.Fatal(err)
		}
		host.NewTimer(time.Second, typeBuff, 0, 0, nil)
		clk.Advance(time.Second)
		tickDelivered(t, d, client, host)
		if due, ok := d.DueOf(owner); !ok || !due.Equal(clk.Now().Add(time.Minute)) {
			t.Fatalf("rescheduled due=%v present=%v", due, ok)
		}
		clk.Advance(time.Minute)
		tickDelivered(t, d, client, host)
		if calls != 2 || len(store.nodes) != 0 {
			t.Fatalf("calls=%d nodes=%d", calls, len(store.nodes))
		}
	})
	// 同一次 Tick 都已到期的另一阶段被回调改期：本次不按旧期限触发，索引报告新期限。
	t.Run("reschedule another due stage", func(t *testing.T) {
		d, clk, client, live := newTestDriver(t)
		owner := newOwner(206)
		live.set(owner)
		store := newMemStore()
		var host *Host
		var delayedID int64
		fired := 0
		host = d.NewHost(owner, store, func(h *Handlers) {
			h.Register(typeBuff, func(Fire) time.Duration {
				if !host.ChangeTimer(delayedID, time.Minute) {
					t.Error("reschedule refused a live timer")
				}
				return 0
			})
			h.Register(typeIncome, func(Fire) time.Duration { fired++; return 0 })
		})
		if err := host.OnInitFinish(true); err != nil {
			t.Fatal(err)
		}
		host.NewTimer(time.Second, typeBuff, 0, 0, nil)
		delayedID = host.NewTimer(time.Second, typeIncome, 0, 0, nil)
		clk.Advance(time.Second)
		tickDelivered(t, d, client, host)
		if fired != 0 {
			t.Fatalf("rescheduled stage fired %d times at its old deadline", fired)
		}
		if due, ok := d.DueOf(owner); !ok || !due.Equal(clk.Now().Add(time.Minute)) {
			t.Fatalf("rescheduled due=%v present=%v", due, ok)
		}
		clk.Advance(time.Minute)
		tickDelivered(t, d, client, host)
		if fired != 1 || len(store.nodes) != 0 {
			t.Fatalf("fired=%d nodes=%d after the new deadline", fired, len(store.nodes))
		}
	})
	t.Run("panic releases transient view", func(t *testing.T) {
		d, clk, _, live := newTestDriver(t)
		owner := newOwner(204)
		live.set(owner)
		store := newMemStore()
		fired := 0
		host := d.NewHost(owner, store, func(h *Handlers) {
			h.Register(typeBuff, func(Fire) time.Duration { panic("business panic") })
			h.Register(typeIncome, func(Fire) time.Duration { fired++; return 0 })
		})
		if err := host.OnInitFinish(true); err != nil {
			t.Fatal(err)
		}
		host.NewTimer(time.Second, typeBuff, 0, 0, nil)
		clk.Advance(time.Second)
		func() { defer func() { _ = recover() }(); host.Tick(clk.Now()) }()
		id := host.NewTimer(time.Second, typeIncome, 0, 0, nil)
		if _, ok := store.nodes[id]; !ok {
			t.Fatal("new timer remained trapped in the failed tick")
		}
		clk.Advance(time.Second)
		host.Tick(clk.Now())
		if fired != 1 {
			t.Fatalf("new callback fired %d times", fired)
		}
	})
	t.Run("rollback restores the previous stage", func(t *testing.T) {
		d, clk, _, live := newTestDriver(t)
		owner := newOwner(205)
		live.set(owner)
		store := newMemStore()
		var host *Host
		host = d.NewHost(owner, store, func(h *Handlers) {
			h.Register(typeBuff, func(Fire) time.Duration { host.NewTimer(time.Minute, typeIncome, 0, 0, nil); return 0 })
			h.Register(typeIncome, func(Fire) time.Duration { return 0 })
		})
		if err := host.OnInitFinish(true); err != nil {
			t.Fatal(err)
		}
		id := host.NewTimer(time.Second, typeBuff, 0, 0, nil)
		clk.Advance(time.Second)
		if _, err := nest.RunIsolatedTransaction(context.Background(), nullCommitter{}, "reject-stage", func() (any, error) { host.Tick(clk.Now()); return nil, errBusiness }); err == nil {
			t.Fatal("transaction did not reject")
		}
		if len(store.nodes) != 1 || store.nodes[id].Type != typeBuff || host.active != nil {
			t.Fatal("rollback kept the next stage or its transient scheduler")
		}
		if due, ok := d.DueOf(owner); !ok || !due.Equal(clk.Now()) {
			t.Fatalf("rollback changed index: %v %v", due, ok)
		}
		if _, err := nest.RunIsolatedTransaction(context.Background(), nullCommitter{}, "retry-stage", func() (any, error) { host.Tick(clk.Now()); return nil, nil }); err != nil {
			t.Fatal(err)
		}
		if due, ok := d.DueOf(owner); !ok || !due.Equal(clk.Now().Add(time.Minute)) {
			t.Fatalf("retry did not publish next stage: %v %v", due, ok)
		}
	})
}

// 周期定时器按回调返回值续期，从触发时刻起算。
func TestHostRearmsAPeriodicTimerByItsReturnValue(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	owner := newOwner(102)
	live.set(owner)
	store := newMemStore()
	f := &fires{interval: time.Minute}
	host := d.NewHost(owner, store, f.register)
	if err := host.OnInitFinish(true); err != nil {
		t.Fatal(err)
	}
	host.NewTimer(time.Minute, typeIncome, 0, 0, nil)
	for round := 1; round <= 3; round++ {
		clk.Advance(time.Minute + 200*time.Millisecond) // 投递晚 200ms：下一次从实际触发时刻起算
		if tickDelivered(t, d, client, host) != 1 || len(f.got) != round {
			t.Fatalf("round %d: fired %d", round, len(f.got))
		}
		if due, ok := d.DueOf(owner); !ok || !due.Equal(clk.Now().Add(time.Minute)) {
			t.Fatalf("round %d: next due %v %v, want now+1m", round, due, ok)
		}
	}
}

// 离线：实体卸载期间错过多个周期，加载后只触发一次，Fire 带原定期限、触发时刻、加载时刻与补触发标志；
// 回调返回的间隔从触发时刻起算，框架不补算、不循环补触发。
func TestHostFiresAnOverdueTimerOnceAfterLoad(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	store := newMemStore()
	first := newOwner(103)
	live.set(first)
	f := &fires{interval: time.Minute}
	host := d.NewHost(first, store, f.register)
	if err := host.OnInitFinish(true); err != nil {
		t.Fatal(err)
	}
	host.NewTimer(time.Minute, typeIncome, 0, 0, nil)
	deadline := clk.Now().Add(time.Minute)

	// 卸载：实例离开驱动，节点留在存储里。
	first.SetRemoved()
	host.OnDestroy()
	live.del(103)
	clk.Advance(10*time.Minute + 30*time.Second) // 错过十个周期
	if tickDelivered(t, d, client, host) != 0 {
		t.Fatal("delivered to an unloaded entity")
	}

	// 加载：同一份存储、新实例。
	second := newOwner(103)
	loadedAt := clk.Now()
	reloaded := d.NewHost(second, store, f.register)
	if err := reloaded.OnInitFinish(false); err != nil {
		t.Fatal(err)
	}
	live.set(second)
	if due, ok := d.DueOf(second); !ok || !due.Equal(deadline) {
		t.Fatalf("after load the index holds %v %v, want the overdue deadline %v", due, ok, deadline)
	}
	if tickDelivered(t, d, client, reloaded) != 1 {
		t.Fatal("the overdue timer was not delivered after load")
	}
	if len(f.got) != 1 {
		t.Fatalf("fired %d times after missing ten periods, want exactly 1", len(f.got))
	}
	fire := f.got[0]
	if !fire.Overdue || !fire.Deadline.Equal(deadline) || !fire.Now.Equal(loadedAt) || !fire.LoadedAt.Equal(loadedAt) || fire.Interval != time.Minute {
		t.Fatalf("catch-up fire = %+v (deadline %v, loaded %v)", fire, deadline, loadedAt)
	}
	if missed := int64(fire.Now.Sub(fire.Deadline) / fire.Interval); missed != 9 {
		t.Fatalf("the business can count the missed periods from the fire: got %d, want 9", missed)
	}
	if due, ok := d.DueOf(second); !ok || !due.Equal(loadedAt.Add(time.Minute)) {
		t.Fatalf("next due %v %v, want one interval after the catch-up", due, ok)
	}
	// 下一次是在线触发，不再是补触发。
	clk.Advance(time.Minute)
	if tickDelivered(t, d, client, reloaded) != 1 || len(f.got) != 2 || f.got[1].Overdue {
		t.Fatalf("second fire = %+v", f.got)
	}
}

// 重建后不复用 ID：种子随 upsert 持久，取消掉的节点的 ID 也不会再分配。
func TestHostDoesNotReuseIDsAcrossReload(t *testing.T) {
	d, _, _, live := newTestDriver(t)
	store := newMemStore()
	owner := newOwner(104)
	live.set(owner)
	host := d.NewHost(owner, store, (&fires{}).register)
	if err := host.OnInitFinish(true); err != nil {
		t.Fatal(err)
	}
	a := host.NewTimer(time.Minute, typeBuff, 0, 0, nil)
	b := host.NewTimer(time.Minute, typeBuff, 0, 0, nil)
	if !host.RemoveTimer(a) || !host.RemoveTimer(b) || len(store.nodes) != 0 {
		t.Fatal("remove")
	}
	host.OnDestroy()
	reloaded := d.NewHost(newOwner(104), store, (&fires{}).register)
	if err := reloaded.OnInitFinish(false); err != nil {
		t.Fatal(err)
	}
	if c := reloaded.NewTimer(time.Minute, typeBuff, 0, 0, nil); c != 3 {
		t.Fatalf("after reload the next id is %d, want 3 (ids 1 and 2 were handed out before)", c)
	}
}

// 同一实体多个定时器：索引里是最早的那个，增删改之后跟着变。
func TestHostIndexesTheEarliestOfSeveralTimers(t *testing.T) {
	d, clk, _, live := newTestDriver(t)
	owner := newOwner(105)
	live.set(owner)
	host := d.NewHost(owner, newMemStore(), (&fires{}).register)
	if err := host.OnInitFinish(true); err != nil {
		t.Fatal(err)
	}
	now := clk.Now()
	expect := func(want time.Duration) {
		t.Helper()
		due, ok := d.DueOf(owner)
		if want == 0 {
			if ok {
				t.Fatalf("indexed %v, want nothing", due)
			}
			return
		}
		if !ok || !due.Equal(now.Add(want)) {
			t.Fatalf("indexed %v %v, want now+%v", due, ok, want)
		}
	}
	thirty := host.NewTimer(30*time.Second, typeBuff, 0, 0, nil)
	ten := host.NewTimer(10*time.Second, typeBuff, 0, 0, nil)
	twenty := host.NewTimerAt(now.Add(20*time.Second), typeBuff, 0, 0, nil)
	expect(10 * time.Second)
	host.ChangeTimer(ten, 40*time.Second)
	expect(20 * time.Second)
	host.RemoveTimer(twenty)
	expect(30 * time.Second)
	if order := host.Timers(); len(order) != 2 || order[0].ID != thirty || order[1].ID != ten {
		t.Fatalf("Timers() = %+v", order)
	}
	host.RemoveTimer(thirty)
	host.RemoveTimer(ten)
	expect(0)
	if past := host.NewTimerAt(now.Add(-time.Hour), typeBuff, 0, 0, nil); past == 0 {
		t.Fatal("NewTimerAt in the past refused")
	}
	expect(time.Millisecond)
}

// 回调登记错误在实体初始化时报出，不是到期时才发现。
func TestHostReportsBadHandlerRegistrationAtInit(t *testing.T) {
	d, _, _, _ := newTestDriver(t)
	host := d.NewHost(newOwner(106), newMemStore(), func(handlers *Handlers) {
		handlers.Register(0, func(Fire) time.Duration { return 0 })
		handlers.Register(3, func(Fire) time.Duration { return 0 })
		handlers.Register(3, func(Fire) time.Duration { return 0 })
	})
	err := host.OnInitFinish(false)
	if err == nil || !strings.Contains(err.Error(), "must be positive") || !strings.Contains(err.Error(), "registered twice") {
		t.Fatalf("OnInitFinish = %v", err)
	}
	if host.NewTimer(time.Second, 3, 0, 0, nil) != 0 {
		t.Fatal("a host with a registration error armed a timer")
	}
}
