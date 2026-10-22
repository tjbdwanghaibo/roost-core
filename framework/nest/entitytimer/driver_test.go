package entitytimer

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// fakeClock 是可控的业务时钟：测试推进它，再显式调用 DeliverDue，事件顺序由测试决定。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.UnixMilli(1_800_000_000_000)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// testOwner 是只有身份的实体：驱动只读 ID、IsRemoved 与实例身份。
type testOwner struct{ *entity.EntityBase }

func (o *testOwner) Base() *entity.EntityBase { return o.EntityBase }

func newOwner(id int64) *testOwner {
	return &testOwner{EntityBase: entity.NewEntityBase(id, entity.EntityCategoryOther, false)}
}

// recordingClient 记下每次投递；fail 非空时准入失败。
type recordingClient struct {
	mu    sync.Mutex
	sent  []sentTick
	fail  error
	calls atomic.Int64
}

type sentTick struct {
	handler string
	id      int64
	params  int // Tick 消息不带参数（handler 按自己冻结的业务时间触发）
}

func (c *recordingClient) Dispatch(_ context.Context, name nest.HandlerName, id int64, params nest.Params, _ ...nest.SendOpt) error {
	c.calls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.sent = append(c.sent, sentTick{handler: name.String(), id: id, params: len(params)})
	return nil
}

func (c *recordingClient) take() []sentTick {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.sent
	c.sent = nil
	return out
}

var errUnused = errors.New("unused in entitytimer tests")

func (*recordingClient) Request(context.Context, nest.HandlerName, int64, nest.Params, ...nest.SendOpt) (any, error) {
	return nil, errUnused
}
func (*recordingClient) DispatchBroadcast(context.Context, nest.HandlerName, []int64, nest.Params, ...nest.SendOpt) error {
	return errUnused
}
func (*recordingClient) DispatchMulti(context.Context, nest.HandlerName, []int64, nest.Params, ...nest.SendOpt) error {
	return errUnused
}
func (*recordingClient) RequestMulti(context.Context, nest.HandlerName, []int64, nest.Params, ...nest.SendOpt) (any, error) {
	return nil, errUnused
}
func (*recordingClient) DispatchMultiGroup(context.Context, nest.HandlerName, [][]int64, nest.Params, ...nest.SendOpt) error {
	return errUnused
}
func (*recordingClient) RequestMultiGroup(context.Context, nest.HandlerName, [][]int64, nest.Params, ...nest.SendOpt) (any, error) {
	return nil, errUnused
}

// liveSet 是 Lookup：测试决定哪个实例是某 ID 当前发布的那个。
type liveSet struct {
	mu   sync.Mutex
	byID map[int64]entity.IThreadSafeEntity
}

func (l *liveSet) set(value entity.IThreadSafeEntity) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byID == nil {
		l.byID = make(map[int64]entity.IThreadSafeEntity)
	}
	l.byID[value.ID()] = value
}

func (l *liveSet) del(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byID, id)
}

func (l *liveSet) lookup(id int64) entity.IThreadSafeEntity {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byID[id]
}

func newTestDriver(t *testing.T) (*Driver, *fakeClock, *recordingClient, *liveSet) {
	t.Helper()
	d := NewDriver(Config{Name: "test.timer", Handler: nest.NewHandlerName("handlerTickTestTimer"), Redeliver: 10 * time.Second})
	clk := newFakeClock()
	d.SetClock(clk.Now)
	client := &recordingClient{}
	live := &liveSet{}
	if err := d.Bind(client, live.lookup); err != nil {
		t.Fatal(err)
	}
	return d, clk, client, live
}

func ids(sent []sentTick) []int64 {
	out := make([]int64, 0, len(sent))
	for _, s := range sent {
		out = append(out, s.id)
	}
	return out
}

// 只给到期的实例投递，且只投一次；投递携带驱动此刻的业务时间（毫秒）。
func TestDriverDeliversOnlyDueEntitiesOnce(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	a, b, c := newOwner(1), newOwner(2), newOwner(3)
	for _, o := range []*testOwner{a, b, c} {
		live.set(o)
	}
	sa, sb, sc := d.newSlot(a), d.newSlot(b), d.newSlot(c)
	start := clk.Now()
	d.report(sa, 1, start.Add(5*time.Second))
	d.report(sb, 1, start.Add(time.Minute))
	d.report(sc, 1, time.Time{}) // 没有期限：不进索引
	if got := d.Pending(); got != 2 {
		t.Fatalf("pending = %d, want 2 (an owner with no deadline is not indexed)", got)
	}
	if n := d.DeliverDue(context.Background()); n != 0 || len(client.take()) != 0 {
		t.Fatalf("delivered %d before anything was due", n)
	}
	clk.Advance(5 * time.Second)
	if n := d.DeliverDue(context.Background()); n != 1 {
		t.Fatalf("delivered %d at the first deadline, want 1", n)
	}
	sent := client.take()
	if len(sent) != 1 || sent[0].id != 1 || sent[0].handler != "handlerTickTestTimer" || sent[0].params != 0 {
		t.Fatalf("sent = %+v, want one tick to entity 1 without parameters", sent)
	}
	// 待报告期间不重复投递。
	clk.Advance(time.Second)
	if n := d.DeliverDue(context.Background()); n != 0 {
		t.Fatalf("delivered %d again while the first tick was unreported", n)
	}
	if _, ok := d.DueOf(a); ok {
		t.Fatalf("an in-flight owner still reports a deadline")
	}
}

// Tick 提交后的报告把条目换回新期限；报告之前没有回音就按 Redeliver 重投。
func TestDriverReportClearsInflightAndRedeliversWhenSilent(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	a := newOwner(7)
	live.set(a)
	s := d.newSlot(a)
	d.report(s, 1, clk.Now())
	if d.DeliverDue(context.Background()) != 1 {
		t.Fatal("first delivery")
	}
	client.take()
	// handler 提交：新的最早期限 30s 之后。
	d.report(s, 2, clk.Now().Add(30*time.Second))
	if due, ok := d.DueOf(a); !ok || !due.Equal(clk.Now().Add(30*time.Second)) {
		t.Fatalf("due after the report = %v %v", due, ok)
	}
	clk.Advance(30 * time.Second)
	if d.DeliverDue(context.Background()) != 1 {
		t.Fatal("second delivery")
	}
	client.take()
	// 这次 handler 回滚了：没有报告。Redeliver（10s）之前不重投，之后重投一次。
	clk.Advance(9 * time.Second)
	if d.DeliverDue(context.Background()) != 0 {
		t.Fatal("redelivered before Redeliver elapsed")
	}
	clk.Advance(time.Second)
	if n := d.DeliverDue(context.Background()); n != 1 {
		t.Fatalf("redelivered %d after Redeliver, want 1", n)
	}
	if sent := client.take(); len(sent) != 1 || sent[0].id != 7 {
		t.Fatalf("redelivery = %+v", sent)
	}
}

// 同一实例的报告按序号排定：晚到的旧报告（AfterCommit 不保证跨事务的执行顺序）不覆盖新报告。
func TestDriverDropsStaleReports(t *testing.T) {
	d, clk, _, live := newTestDriver(t)
	a := newOwner(9)
	live.set(a)
	s := d.newSlot(a)
	early, late := clk.Now().Add(time.Second), clk.Now().Add(time.Hour)
	d.report(s, 2, early)
	d.report(s, 1, late) // 旧事务的回调晚到
	if due, ok := d.DueOf(a); !ok || !due.Equal(early) {
		t.Fatalf("due = %v %v, want the newer report %v", due, ok, early)
	}
	d.report(s, 3, time.Time{})
	d.report(s, 2, late)
	if d.Pending() != 0 {
		t.Fatalf("a stale report re-armed an owner whose newest report cleared it")
	}
}

// 实例销毁（卸载 / 删档）之后：不再投递，之后（晚到的 AfterCommit）的报告也不能把它放回索引。
func TestDriverNeverDeliversToAClosedOwner(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	a := newOwner(11)
	live.set(a)
	s := d.newSlot(a)
	d.report(s, 1, clk.Now().Add(time.Second))
	d.close(s)
	live.del(11)
	d.report(s, 2, clk.Now().Add(time.Second))
	clk.Advance(time.Hour)
	if n := d.DeliverDue(context.Background()); n != 0 || len(client.take()) != 0 || d.Pending() != 0 {
		t.Fatalf("delivered %d to a destroyed owner (pending %d)", n, d.Pending())
	}
}

// 到期时核对实例：已移除（Destroy 在锁内先 SetRemoved，OnDestroy 在解锁后）或被同 ID 的新实例取代时丢弃；
// 查不到实例（加载中、尚未发布）时照常投递。
func TestDriverValidatesTheOwnerBeforeDelivering(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	removed, replaced, loading := newOwner(21), newOwner(22), newOwner(23)
	live.set(removed)
	live.set(newOwner(22)) // 22 已经是另一个实例
	for _, o := range []*testOwner{removed, replaced, loading} {
		d.report(d.newSlot(o), 1, clk.Now())
	}
	removed.SetRemoved()
	if n := d.DeliverDue(context.Background()); n != 1 {
		t.Fatalf("delivered %d, want only the owner that is still loading", n)
	}
	if got := ids(client.take()); len(got) != 1 || got[0] != 23 {
		t.Fatalf("delivered to %v, want [23]", got)
	}
	if d.Pending() != 1 {
		t.Fatalf("pending = %d, want only the in-flight delivery", d.Pending())
	}
}

// 准入失败（队列满）按 dispatchRetry 重试，不等满 Redeliver；期间有新报告时以报告为准。
func TestDriverRetriesRefusedAdmission(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	a := newOwner(31)
	live.set(a)
	s := d.newSlot(a)
	d.report(s, 1, clk.Now())
	client.fail = nest.ErrQueueFull
	if d.DeliverDue(context.Background()) != 0 {
		t.Fatal("a refused dispatch counted as delivered")
	}
	if wake, ok := d.NextDue(); !ok || !wake.Equal(clk.Now().Add(dispatchRetry)) {
		t.Fatalf("next wake = %v %v, want the admission retry %v", wake, ok, clk.Now().Add(dispatchRetry))
	}
	client.fail = nil
	clk.Advance(dispatchRetry)
	if d.DeliverDue(context.Background()) != 1 {
		t.Fatal("retry after the refused admission")
	}
}

// 同一时刻到期的多个实例一次投完，顺序按实体 ID。
func TestDriverDeliversSimultaneousDeadlinesInIDOrder(t *testing.T) {
	d, clk, client, live := newTestDriver(t)
	for _, id := range []int64{44, 41, 43, 42} {
		o := newOwner(id)
		live.set(o)
		d.report(d.newSlot(o), 1, clk.Now().Add(time.Second))
	}
	clk.Advance(time.Second)
	if n := d.DeliverDue(context.Background()); n != 4 {
		t.Fatalf("delivered %d", n)
	}
	if got := fmt.Sprint(ids(client.take())); got != "[41 42 43 44]" {
		t.Fatalf("order = %s", got)
	}
}

// 循环：期限变早时被叫醒，到期才投递；Stop 等循环退出。
func TestDriverLoopWakesForAnEarlierDeadline(t *testing.T) {
	d := NewDriver(Config{Name: "loop.timer", Handler: nest.NewHandlerName("handlerTickLoopTimer"), MaxWait: time.Hour})
	client := &recordingClient{}
	live := &liveSet{}
	if err := d.Bind(client, live.lookup); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	a := newOwner(51)
	live.set(a)
	s := d.newSlot(a)
	// 业务时钟用真实时间；MaxWait 一小时，所以这里能投递只可能是因为报告叫醒了循环。
	d.report(s, 1, d.Now())
	deadline := time.Now().Add(5 * time.Second)
	for client.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the loop did not wake up for a deadline that is already due")
		}
		time.Sleep(time.Millisecond)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// 并发：加载（首次报告）/ 卸载（close）/ 期限变化（report）/ 投递（循环与 DeliverDue）交错，-race 下无竞态；
// 最后全部实例关闭后索引为空，关闭之后没有投递。
func TestDriverConcurrentReportsClosesAndDeliveries(t *testing.T) {
	d := NewDriver(Config{Name: "race.timer", Handler: nest.NewHandlerName("handlerTickRaceTimer"), Redeliver: time.Millisecond, MaxWait: time.Millisecond})
	client := &recordingClient{}
	live := &liveSet{}
	if err := d.Bind(client, live.lookup); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	const owners = 64
	var wg sync.WaitGroup
	var closed sync.Map
	for i := range owners {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(id))
			o := newOwner(id)
			live.set(o)
			s := d.newSlot(o)
			for seq := uint64(1); seq <= 200; seq++ {
				due := time.Time{}
				if rng.Intn(4) != 0 {
					due = d.Now().Add(time.Duration(rng.Intn(3)) * time.Millisecond)
				}
				// 偶尔报告乱序到达。
				if seq > 1 && rng.Intn(5) == 0 {
					d.report(s, seq-1, due)
				}
				d.report(s, seq, due)
				if rng.Intn(10) == 0 {
					d.DeliverDue(context.Background())
				}
			}
			o.SetRemoved()
			live.del(id)
			d.close(s)
			closed.Store(id, true)
			d.report(s, 1000, d.Now()) // 晚到的提交回调
		}(int64(1000 + i))
	}
	wg.Wait()
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.Pending() != 0 {
		t.Fatalf("pending = %d after every owner closed", d.Pending())
	}
	before := client.calls.Load()
	d.DeliverDue(context.Background())
	if client.calls.Load() != before {
		t.Fatal("delivered after every owner closed")
	}
}
