package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func stepN(d *Driver, n int64) {
	for range n {
		d.step(context.Background())
	}
}

// 档位规则：只接受固定档位；首拍落在启动后 (0, 周期]，之后每周期一次，触发格 ≡ 相位 (mod 本档格数)；
// 相位 = hash(实体 ID)，同档实体均匀分布在周期内的各格。
func TestTiersFireOnTheirPhaseOncePerPeriod(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		for _, bad := range []time.Duration{0, 10 * time.Millisecond, 150 * time.Millisecond, 3 * time.Second, 2 * time.Minute} {
			if _, err := tierOf(bad); !errors.Is(err, ErrUnsupportedPeriod) {
				t.Fatalf("period %v accepted: %v", bad, err)
			}
		}
		d, client, _, live := newTestDriver(t, discard)
		const perTier = 40
		type item struct {
			reg   *Reg
			start int64
		}
		var items []item
		stepN(d, 7) // 启动不在第 0 格
		for ti, p := range Periods {
			for i := range perTier {
				e := newHBEntity(t, d, int64(ti*1000+i+1), kindShort)
				live.Store(e.ID(), e)
				r, err := e.host.Register("p", p, func(Beat) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
				r.Start()
				items = append(items, item{reg: r, start: d.frame()})
			}
		}
		horizon := 2*tierFrames[numTiers-1] + 3
		fires := make(map[*Reg][]int64)
		for range horizon {
			d.step(context.Background())
			for _, b := range client.take() {
				fires[b.reg] = append(fires[b.reg], b.frame)
				execute(t, b) // 立即执行，不留在途
			}
		}
		for _, it := range items {
			r := it.reg
			frames := tierFrames[r.tier]
			cell := cellOf(r.host.hash, r.tier)
			var want []int64
			for f := it.start + 1; f <= it.start+horizon; f++ {
				if f%frames == cell {
					want = append(want, f)
				}
			}
			if fmt.Sprint(fires[r]) != fmt.Sprint(want) {
				t.Fatalf("entity %d tier %v: fired at %v, want %v", r.host.id, r.tier.period(), fires[r], want)
			}
			if first := want[0] - it.start; first <= 0 || first > frames {
				t.Fatalf("first beat %d frames after start, want within (0, %d]", first, frames)
			}
		}

		// 相位打散：1 万个实体同在 100ms 档（10 格），每格数量接近平均。
		counts := make([]int, tierFrames[1])
		for i := range 10_000 {
			counts[cellOf(phaseHash(mustID(t, int64(100_000+i), kindShort)), 1)]++
		}
		for cell, n := range counts {
			if n < 900 || n > 1100 {
				t.Fatalf("100ms cell %d holds %d of 10000 entities (want about 1000): %v", cell, n, counts)
			}
		}
	})
}

// 至多一拍在途：上一拍没执行完时到期的拍跳过、不累积、不补发；跳过计数交给下一拍的 Beat.Skipped；
// Warn 按间隔汇总（间隔内只计数）。准入失败（ErrQueueFull）同样按跳拍处理并清在途。
func TestInflightBeatSkipsCountsAndWarnsAtALimitedRate(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		logs := &countingHandler{}
		d, client, _, live := newTestDriver(t, slog.New(logs))
		e := newHBEntity(t, d, 1, kindShort)
		live.Store(e.ID(), e)
		var beats []Beat
		r, err := e.host.Register("move", 50*time.Millisecond, func(b Beat) error { beats = append(beats, b); return nil })
		if err != nil {
			t.Fatal(err)
		}
		r.Start()

		stepN(d, 5*5) // 5 个周期：第一拍投出后一直不执行
		sent := client.take()
		if len(sent) != 1 {
			t.Fatalf("delivered %d beats while the first was still in flight, want 1", len(sent))
		}
		if st := d.Stats(); st.SkippedInflight != 4 || st.Delivered != 1 {
			t.Fatalf("stats %+v, want 4 skipped and 1 delivered", st)
		}
		t0 := time.Unix(100, 0)
		d.flush(t0)
		if logs.count() != 1 {
			t.Fatalf("warns after the first flush = %d, want 1", logs.count())
		}
		stepN(d, 5*3) // 再跳 3 拍：间隔内不再打
		d.flush(t0.Add(time.Second))
		if logs.count() != 1 {
			t.Fatalf("warns inside the interval = %d, want still 1", logs.count())
		}
		d.flush(t0.Add(DefaultWarnInterval))
		if logs.count() != 2 {
			t.Fatalf("warns after the interval = %d, want 2", logs.count())
		}

		execute(t, sent[0])
		if len(beats) != 1 || beats[0].Skipped != 7 || !beats[0].First {
			t.Fatalf("beat after 7 skips = %+v", beats)
		}
		// 执行完之后下一个到期格照常投递一次。
		stepN(d, 5)
		if got := client.take(); len(got) != 1 {
			t.Fatalf("delivered %d after the in-flight beat ran, want 1", len(got))
		}

		// 准入失败：清在途、计 queue_full，每个周期至多投一次、不在本格重试。
		client.err = nest.ErrQueueFull
		stepN(d, 5*4)
		if got := client.take(); len(got) != 0 {
			// 上一拍还在途（上面投出的没执行）：先执行掉它。
			t.Fatalf("unexpected deliveries %d while a beat is in flight", len(got))
		}
		r.inflight.Store(false)
		stepN(d, 5*4)
		if got := client.take(); len(got) != 4 {
			t.Fatalf("refused admissions per 4 periods = %d, want 4 (one per period, no retries)", len(got))
		}
		if st := d.Stats(); st.SkippedQueueFull != 4 || r.inflight.Load() {
			t.Fatalf("after refused admissions: stats %+v inflight %v", st, r.inflight.Load())
		}
	})
}

// 在途超过 StallAfter 视为丢失、重投一次；两份消息至多一份执行回调。
func TestStalledBeatIsRedeliveredAndRunsOnce(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		d, client, _, live := newTestDriver(t, discard)
		e := newHBEntity(t, d, 1, kindShort)
		live.Store(e.ID(), e)
		runs := 0
		r, _ := e.host.Register("x", time.Second, func(Beat) error { runs++; return nil })
		r.Start()
		// 每秒一个到期格；记下每次投递发生在第几秒。
		var at []int
		for sec := 1; sec <= 80; sec++ {
			stepN(d, 100)
			for range client.take() {
				at = append(at, sec)
			}
		}
		// 首投在第 1 秒；一直不执行：10s 后重投，之后 20s、40s（翻倍），再之后上限 80s。
		if fmt.Sprint(at) != "[1 11 31 71]" {
			t.Fatalf("deliveries of a beat that never runs at seconds %v, want [1 11 31 71]", at)
		}
		if st := d.Stats(); st.Stalled != 3 {
			t.Fatalf("stats %+v", st)
		}
		// 积压的几份消息先后执行：只有第一份执行回调。
		for range at {
			_ = d.runBeat(e, r)
		}
		if runs != 1 || d.Stats().DroppedDuplicate != int64(len(at)-1) {
			t.Fatalf("callback ran %d times (stats %+v), want once", runs, d.Stats())
		}
		// 执行之后阈值复位：再卡住时 10s 后重投。
		at = at[:0]
		for sec := 1; sec <= 12; sec++ {
			stepN(d, 100)
			for range client.take() {
				at = append(at, sec)
			}
		}
		if fmt.Sprint(at) != "[1 11]" {
			t.Fatalf("after a beat ran, deliveries at %v, want [1 11]", at)
		}
	})
}

// 换档：生效后按新档位所在的格触发；Elapsed 从上次执行起算，Period 是新档。
func TestSetPeriodMovesToTheNewTier(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		d, client, clk, live := newTestDriver(t, discard)
		e := newHBEntity(t, d, 3, kindShort)
		live.Store(e.ID(), e)
		var beats []Beat
		r, _ := e.host.Register("x", 100*time.Millisecond, func(b Beat) error { beats = append(beats, b); return nil })
		r.Start()
		run := func(frames int64) []int64 {
			var at []int64
			for range frames {
				d.step(context.Background())
				clk.Advance(Resolution)
				for _, b := range client.take() {
					at = append(at, b.frame)
					execute(t, b)
				}
			}
			return at
		}
		if at := run(30); len(at) != 3 {
			t.Fatalf("100ms beats in 300ms = %v", at)
		}
		if err := r.SetPeriod(150 * time.Millisecond); !errors.Is(err, ErrUnsupportedPeriod) {
			t.Fatalf("SetPeriod(150ms) = %v", err)
		}
		if err := r.SetPeriod(time.Second); err != nil {
			t.Fatal(err)
		}
		at := run(300)
		if len(at) != 3 {
			t.Fatalf("1s beats in 3s = %v", at)
		}
		for _, f := range at {
			if f%100 != cellOf(r.host.hash, 4) {
				t.Fatalf("1s beat at frame %d, want cell %d", f, cellOf(r.host.hash, 4))
			}
		}
		last := beats[len(beats)-1]
		if last.Period != time.Second || last.Elapsed != time.Second || last.First {
			t.Fatalf("beat after the change = %+v", last)
		}
		// 停止状态下换档只记下，下次 Start 用它。
		r.Stop()
		_ = r.SetPeriod(50 * time.Millisecond)
		if d.Registered() != 0 {
			t.Fatal("stopped registration is still on the ring")
		}
		r.Start()
		if at := run(20); len(at) != 4 {
			t.Fatalf("50ms beats in 200ms after restart = %v", at)
		}
		if !beats[len(beats)-4].First {
			t.Fatal("first beat after restart is not First")
		}
	})
}

// 多登记不合并：同一实体的两个同档登记落在同一格，各自一条消息、各自的回调与经过时间。
func TestRegistrationsOfOneEntityAreDeliveredSeparately(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		d, client, _, live := newTestDriver(t, discard)
		e := newHBEntity(t, d, 4, kindShort)
		live.Store(e.ID(), e)
		var a, b int
		ra, _ := e.host.Register("a", 100*time.Millisecond, func(Beat) error { a++; return nil })
		rb, _ := e.host.Register("b", 100*time.Millisecond, func(Beat) error { b++; return nil }, WithDurability(nest.DurabilityStrict))
		rc, _ := e.host.Register("c", time.Second, func(Beat) error { return nil })
		if _, err := e.host.Register("a", time.Second, func(Beat) error { return nil }); !errors.Is(err, ErrDuplicateName) {
			t.Fatalf("duplicate name = %v", err)
		}
		ra.Start()
		rb.Start()
		rc.Start()
		perFrame := map[int64][]sentBeat{}
		for range 100 {
			d.step(context.Background())
			for _, s := range client.take() {
				perFrame[s.frame] = append(perFrame[s.frame], s)
				execute(t, s)
			}
		}
		if a != 10 || b != 10 {
			t.Fatalf("callbacks a=%d b=%d in 1s, want 10 each", a, b)
		}
		for f, sent := range perFrame {
			regs := map[*Reg]bool{}
			for _, s := range sent {
				if regs[s.reg] {
					t.Fatalf("frame %d delivered %q twice", f, s.reg.name)
				}
				regs[s.reg] = true
			}
			if regs[ra] != regs[rb] {
				t.Fatalf("same-tier registrations of one entity landed in different cells at frame %d", f)
			}
		}
		for _, s := range perFrame[func() int64 {
			for f := range perFrame {
				return f
			}
			return 0
		}()] {
			want := "heartbeat.beat.async"
			if s.reg == rb {
				want = "heartbeat.beat.strict"
			}
			if s.handler != want {
				t.Fatalf("registration %q dispatched to %s, want %s", s.reg.name, s.handler, want)
			}
		}
	})
}

// 投递前核对实例：已 OnDestroy、已卸载、被同 ID 新实例取代、还没发布的都不投，不唤醒不在内存的实体。
// 执行时核对：目标不是登记所属的实例（卸载后同 ID 重载）时按过期丢弃，新实例上不执行旧登记的回调。
func TestInstanceChecksDropStaleBeats(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		d, client, _, live := newTestDriver(t, discard)
		oldE := newHBEntity(t, d, 5, kindShort)
		oldRuns := 0
		r, _ := oldE.host.Register("x", 50*time.Millisecond, func(Beat) error { oldRuns++; return nil })
		r.Start()
		stepN(d, 5)
		if d.Stats().DroppedNotLoaded != 1 || len(client.take()) != 0 {
			t.Fatalf("unpublished instance got a beat: %+v", d.Stats())
		}
		live.Store(oldE.ID(), oldE)
		stepN(d, 5)
		inFlight := client.take()
		if len(inFlight) != 1 {
			t.Fatalf("delivered %d", len(inFlight))
		}
		// 卸载并在同 ID 上重载：新实例自己登记、启动。
		oldE.SetRemoved()
		oldE.OnDestroy(entity.DestroyReasonMemoryUnload)
		newE := newHBEntity(t, d, 5, kindShort)
		live.Store(newE.ID(), newE)
		newRuns := 0
		nr, _ := newE.host.Register("x", 50*time.Millisecond, func(Beat) error { newRuns++; return nil })
		nr.Start()
		// 旧实例的在途拍到达新实例：不执行任何回调。
		if err := d.runBeat(newE, inFlight[0].reg); err != nil {
			t.Fatal(err)
		}
		if oldRuns != 0 || newRuns != 0 || d.Stats().DroppedStale != 1 {
			t.Fatalf("stale beat ran: old %d new %d stats %+v", oldRuns, newRuns, d.Stats())
		}
		// 旧登记已离开环；OnDestroy 之后的启停被忽略。
		r.Start()
		stepN(d, 5)
		sent := client.take()
		if len(sent) != 1 || sent[0].reg != nr {
			t.Fatalf("after reload delivered %v", sent)
		}
		execute(t, sent[0])
		if newRuns != 1 || d.Registered() != 1 {
			t.Fatalf("new instance runs %d, registered %d", newRuns, d.Registered())
		}
		if _, err := oldE.host.Register("y", time.Second, func(Beat) error { return nil }); !errors.Is(err, ErrHostClosed) {
			t.Fatalf("register after OnDestroy = %v", err)
		}
		// 被同 ID 的另一实例取代（新实例已发布、旧实例还没 OnDestroy）：投递前丢弃。
		third := newHBEntity(t, d, 6, kindShort)
		tr, _ := third.host.Register("x", 50*time.Millisecond, func(Beat) error { return nil })
		tr.Start()
		live.Store(third.ID(), newHBEntity(t, d, 6, kindShort))
		before := d.Stats().DroppedStale
		stepN(d, 5)
		for _, s := range client.take() {
			if s.reg == tr {
				t.Fatal("beat delivered to a replaced instance")
			}
		}
		if d.Stats().DroppedStale != before+1 {
			t.Fatalf("replaced instance not counted stale: %+v", d.Stats())
		}
	})
}

// Close 顺序：Stop 等正在交出的一批结束；批里还没投的不再投、清在途；Stop 返回后不再 Dispatch；
// Stop 之后 OnDestroy 仍可安全调用。
func TestStopWaitsForTheBatchAndDispatchesNothingAfter(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		d, client, _, live := newTestDriver(t, discard)
		client.entered = make(chan struct{})
		// 一个实体 50 个同档登记：同一格到期，一批 50 拍。
		e := newHBEntity(t, d, 7000, kindShort)
		live.Store(e.ID(), e)
		ents := []*hbEntity{e}
		var regs []*Reg
		for i := range 50 {
			r, _ := e.host.Register(fmt.Sprint("x", i), 50*time.Millisecond, func(Beat) error { return nil })
			r.Start()
			regs = append(regs, r)
		}
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
		<-client.entered // 第一条 Dispatch 正在进行，它在驱动停止（ctx 取消）后才返回
		if err := d.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		after := len(client.take())
		inflight := 0
		for _, r := range regs {
			if r.inflight.Load() {
				inflight++
			}
		}
		// 单目标：正在进行的那一条已投出、仍在途，其余不投、清在途。批量：50 拍是同一次调用，驱动停止后它整批不准入、
		// 全部清在途。
		want := 1
		if testDelivery == DeliveryBatch {
			want = 0
		}
		if after != want || inflight != want {
			t.Fatalf("Stop during a batch: %d dispatched, %d left in flight; want %d (only the call in progress may have been sent; the rest must not be sent and must clear their flag)", after, inflight, want)
		}
		// 停止后不再投递：即使有到期的登记。
		time.Sleep(3 * Resolution)
		if n := len(client.take()); n != 0 {
			t.Fatalf("%d dispatches after Stop returned", n)
		}
		for _, e := range ents {
			e.OnDestroy(entity.DestroyReasonMemoryUnload)
		}
		if d.Registered() != 0 {
			t.Fatalf("registered after OnDestroy = %d", d.Registered())
		}
		if err := d.Stop(context.Background()); err != nil {
			t.Fatalf("second Stop = %v", err)
		}
	})
}

// -race：业务在实体锁内并发启停 / 换档，驱动同时推进、回调同时执行。
func TestConcurrentChangesStepsAndBeats(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		d, client, _, live := newTestDriver(t, discard)
		const n = 64
		type ent struct {
			e  *hbEntity
			mu sync.Mutex
			r  *Reg
		}
		ents := make([]*ent, n)
		for i := range ents {
			e := newHBEntity(t, d, int64(9000+i), kindShort)
			live.Store(e.ID(), e)
			r, _ := e.host.Register("x", Periods[i%3], func(Beat) error { return nil })
			ents[i] = &ent{e: e, r: r}
		}
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for w := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := 0; ; k++ {
					select {
					case <-stop:
						return
					default:
					}
					x := ents[(k*7+w)%n]
					x.mu.Lock()
					switch k % 3 {
					case 0:
						x.r.Start()
					case 1:
						_ = x.r.SetPeriod(Periods[k%4])
					default:
						x.r.Stop()
					}
					x.mu.Unlock()
				}
			}()
		}
		for range 2000 {
			d.step(context.Background())
			for _, s := range client.take() {
				for _, y := range ents {
					if y.r == s.reg {
						y.mu.Lock()
						execute(t, s)
						y.mu.Unlock()
					}
				}
			}
		}
		close(stop)
		wg.Wait()
		for _, x := range ents {
			x.e.OnDestroy(entity.DestroyReasonMemoryUnload)
		}
		if d.Registered() != 0 {
			t.Fatalf("registered after destroying all = %d", d.Registered())
		}
	})
}

// 投递方式开关：缺省是 Nest 批量作业（维护者 2026-10-22 决定 B：DefaultDelivery = DeliveryBatch，空值与空串同此），
// 单目标 Dispatch 是可选项；配置值只认 dispatch / batch；批量方式要求客户端实现 nest.TryBatchDispatcher（不静默
// 退回）；第一次 Start 之后不能再改。
// 变异核对：SetDelivery 不检查“已启动”时 Start 之后改方式成功，本用例变红；DefaultDelivery 改回 DeliveryDispatch
// （或 ParseDelivery 的空串分支返回 DeliveryDispatch）时第一处断言变红。
func TestDeliveryIsChosenOnceBeforeStart(t *testing.T) {
	if DefaultDelivery != DeliveryBatch {
		t.Fatalf("DefaultDelivery = %q, want batch (maintainer decision 2026-10-22)", DefaultDelivery)
	}
	if d := NewDriver(Config{Name: "default"}); d.Delivery() != DeliveryBatch {
		t.Fatalf("default delivery %q, want batch", d.Delivery())
	}
	for in, want := range map[string]Delivery{"": DeliveryBatch, " ": DeliveryBatch, "dispatch": DeliveryDispatch, " Batch ": DeliveryBatch} {
		if got, err := ParseDelivery(in); err != nil || got != want {
			t.Fatalf("ParseDelivery(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseDelivery("broadcast"); err == nil {
		t.Fatal("ParseDelivery accepted an unknown delivery")
	}

	// 只实现 Client 的客户端：批量方式（显式或缺省）在 Bind / SetDelivery 时报错，不静默退回单目标。
	type clientOnly struct{ nest.Client }
	lookup := func(int64) entity.IThreadSafeEntity { return nil }
	for _, delivery := range []Delivery{DeliveryBatch, ""} {
		batch := NewDriver(Config{Name: "batch", Delivery: delivery})
		if err := batch.Bind(clientOnly{&fakeClient{}}, lookup); err == nil || !strings.Contains(err.Error(), "TryBatchDispatcher") {
			t.Fatalf("Bind a client without try-batch support in delivery %q = %v", delivery, err)
		}
	}
	d := NewDriver(Config{Name: "switch", Delivery: DeliveryDispatch})
	if err := d.Bind(clientOnly{&fakeClient{}}, lookup); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDelivery(DeliveryBatch); err == nil || d.Delivery() != DeliveryDispatch {
		t.Fatalf("SetDelivery(batch) on a client without try-batch support = %v, delivery %q", err, d.Delivery())
	}
	if err := d.Bind(&fakeClient{}, lookup); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDelivery(DeliveryBatch); err != nil || d.Delivery() != DeliveryBatch {
		t.Fatalf("SetDelivery(batch) before Start = %v, delivery %q", err, d.Delivery())
	}
	if _, ok := d.deliver.(*batchDeliverer); !ok {
		t.Fatalf("deliverer %T after SetDelivery(batch), want *batchDeliverer", d.deliver)
	}
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDelivery(DeliveryDispatch); err == nil {
		t.Fatal("SetDelivery changed the delivery of a running driver")
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDelivery(DeliveryDispatch); err == nil || d.Delivery() != DeliveryBatch {
		t.Fatalf("SetDelivery after the driver had started = %v, delivery %q", err, d.Delivery())
	}
}

// 慢派发 trace 在 handler 执行期间对实现了 fmt.GoStringer 的参数（*Reg）调用 GoString：只能读构造后不变的字段，不能
// 与回调对在途 / 跳拍计数的原子写竞争。红在 -race 下：去掉 Reg.GoString 时 %#v 按反射打印，本用例报数据竞争。
func TestRegFormatsWithoutReadingMutableState(t *testing.T) {
	d, _, _, _ := newTestDriver(t, discard)
	e := newHBEntity(t, d, 9100, kindShort)
	r, err := e.host.Register("fmt", 50*time.Millisecond, func(Beat) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			r.inflight.Store(true)
			r.skipped.Add(1)
		}
	}()
	for range 1000 {
		if got := fmt.Sprintf("%#v", r); !strings.Contains(got, `"fmt"`) {
			t.Fatalf("%%#v of a registration = %s", got)
		}
	}
	<-done
}

// 实例已移除（卸载 / 删档，IsRemoved）而组件没调 Host.OnDestroy：驱动在登记到期时发现就撤销整个宿主，环上不再有它的
// 登记、驱动不再持有这个实例；每个登记只计一次 dropped{removed}，之后的启停与登记都被拒绝。
func TestARemovedInstanceLeavesTheDriver(t *testing.T) {
	forEachDelivery(t, func(t *testing.T) {
		d, client, _, live := newTestDriver(t, discard)
		e := &hbEntity{EntityBase: entity.NewEntityBase(mustID(t, 40, kindShort), entity.EntityCategoryOther, true, kindShort)}
		e.host = d.NewHost(e)
		live.Store(e.ID(), e)
		runs := 0
		fast, _ := e.host.Register("fast", 50*time.Millisecond, func(Beat) error { runs++; return nil })
		slow, _ := e.host.Register("slow", time.Second, func(Beat) error { runs++; return nil })
		fast.Start()
		slow.Start()
		stepN(d, 5)
		for _, b := range client.take() {
			execute(t, b)
		}
		if runs == 0 || d.Registered() != 2 {
			t.Fatalf("before removal: runs %d registered %d", runs, d.Registered())
		}
		// 卸载流程标记了移除，但组件漏调 OnDestroy（e.host 不被撤销）。
		e.SetRemoved()
		stepN(d, 200)
		if got := client.take(); len(got) != 0 {
			t.Fatalf("a removed instance got %d beats", len(got))
		}
		// 第一个到期的登记发现移除就撤销整个宿主：同一格里一起到期的登记各计一次，之后不再到期、不再计数。
		st := d.Stats()
		if st.Registered != 0 || st.DroppedRemoved < 1 || st.DroppedRemoved > 2 {
			t.Fatalf("removed instance still on the driver: registered %d dropped removed %d (want 0 / at most one per registration)",
				st.Registered, st.DroppedRemoved)
		}
		stepN(d, 200)
		if again := d.Stats().DroppedRemoved; again != st.DroppedRemoved {
			t.Fatalf("a removed instance kept being counted: %d then %d", st.DroppedRemoved, again)
		}
		if _, err := e.host.Register("late", time.Second, func(Beat) error { return nil }); !errors.Is(err, ErrHostClosed) {
			t.Fatalf("register on a removed instance = %v, want ErrHostClosed", err)
		}
	})
}
