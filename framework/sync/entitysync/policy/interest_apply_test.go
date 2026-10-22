// Interest 的应用顺序与线程契约：排队事实与直接操作按调用顺序生效（重新进入、重新显示、直接放置不被更早的
// 排队移动拖回旧位置）；同步应用模式一次 Flush 排空已确认的积压；业务线程上的直接操作不等 AOI 批次。

package policy

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// commitQueuedMove 在一个完整的提交门里排一条移动并确认提交：事实已就绪、还没应用。
func commitQueuedMove(t *testing.T, m *entitysync.Manager, in *Interest, e *queuedEntity, to spatial.Point) {
	t.Helper()
	mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
	if err := in.QueueMove(e, to); err != nil {
		t.Fatal(err)
	}
	mutation.Admit()
	mutation.Confirm()
	mutation.Release()
}

// spatialViewAt 读空间视野在 AOI 里的视点（只用于失败信息）。
func spatialViewAt(in *Interest, handle int64) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	if observer := in.aoi.observers[handle]; observer != nil {
		return fmt.Sprintf("%v", observer.at)
	}
	return "absent"
}

func mainViewAt(in *Interest, playerID int64) string {
	main := in.registry.mainOf(playerID)
	if main == nil {
		return "no main view"
	}
	return spatialViewAt(in, main.handle)
}

func subjectAt(in *Interest, id int64) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	if at, ok := in.aoi.subjects[id]; ok {
		return fmt.Sprintf("%v", at)
	}
	return "absent"
}

// 一条已提交、还没应用的移动早于之后的 Leave → Enter / Hide → Show / 再次 Enter：结果必须与“先应用这条移动，
// 再做之后的操作”相同——之后的放置决定实体与跟随它的视野在哪里，旧移动不能把它们拖回旧目标。
// 历史：D19（d2c2cdce）把 Leave / Hide 对排队事实的改写改成“保留跟随者那一半”后，Enter / Show 之后才应用的旧事实
// 仍按旧目标移动新的跟随视野（学习分支 learn/main-review-20261021 KNOWN-LIMITS“main 近期提交审查”第 1 条）。
func TestPlacementAfterAQueuedMoveIsNotDraggedBack(t *testing.T) {
	far := spatial.Point{X: 900, Y: 900}
	back := spatial.Point{X: 30, Y: 10}
	drive := func(t *testing.T, in *Interest, m *entitysync.Manager) {
		t.Helper()
		for round := 0; round < 3; round++ {
			flushInterest(t, in, m)
		}
	}
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		t.Run("leave_then_enter", func(t *testing.T) {
			m := newManager(t)
			in := newInterestMode(t, m, mode)
			player(t, m, 1)
			player(t, m, 2)
			mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
			mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
			mustApply(t, in)
			commitQueuedMove(t, m, in, factEntity(t, 2), far)
			if err := in.Leave(2, 2); err != nil {
				t.Fatal(err)
			}
			mustEnter(t, in, 2, back)
			drive(t, in, m)
			if !slices.Contains(in.Visible(2), int64(1)) || !subscribed(m, 2, 1) || !subscribed(m, 1, 2) {
				t.Fatalf("%s: rejoined at %v but entity 2 at %s, main view at %s; player 2 sees %v, subscribed 2→1 %v, 1→2 %v",
					mode, back, subjectAt(in, 2), mainViewAt(in, 2), in.Visible(2), subscribed(m, 2, 1), subscribed(m, 1, 2))
			}
		})
		t.Run("hide_then_show", func(t *testing.T) {
			m := newManager(t)
			in := newInterestMode(t, m, mode)
			player(t, m, 1)
			player(t, m, 5)
			if err := m.Register(subjectState(t, 3)); err != nil {
				t.Fatal(err)
			}
			mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
			if err := in.Show(3, spatial.Point{X: 20, Y: 10}); err != nil {
				t.Fatal(err)
			}
			// 单位 3 的视野挂给玩家 5：玩家 5 经它看见实体 1。
			const unitView ViewID = 30
			mustView(t, in.DefineSpatialView(unitView, spatial.Point{X: 20, Y: 10}, ObserverView{}, 3))
			mustView(t, in.AttachView(5, unitView))
			mustApply(t, in)
			commitQueuedMove(t, m, in, factEntity(t, 3), far)
			if err := in.Hide(3); err != nil {
				t.Fatal(err)
			}
			if err := in.Show(3, back); err != nil {
				t.Fatal(err)
			}
			drive(t, in, m)
			in.registry.mu.Lock()
			handle := in.registry.views[unitView].handle
			in.registry.mu.Unlock()
			if !slices.Contains(in.ViewVisible(unitView), int64(1)) || !subscribed(m, 5, 1) {
				t.Fatalf("%s: shown again at %v but entity 3 at %s, its view at %s; the view sees %v, subscribed 5→1 %v",
					mode, back, subjectAt(in, 3), spatialViewAt(in, handle), in.ViewVisible(unitView), subscribed(m, 5, 1))
			}
		})
		t.Run("enter_again", func(t *testing.T) {
			m := newManager(t)
			in := newInterestMode(t, m, mode)
			player(t, m, 1)
			player(t, m, 2)
			mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
			mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
			mustApply(t, in)
			commitQueuedMove(t, m, in, factEntity(t, 2), far)
			// 已登记的玩家再次 Enter（重连）按这次的位置重新登记。
			mustEnter(t, in, 2, back)
			drive(t, in, m)
			if !slices.Contains(in.Visible(2), int64(1)) || !subscribed(m, 2, 1) || !subscribed(m, 1, 2) {
				t.Fatalf("%s: entered again at %v but entity 2 at %s, main view at %s; player 2 sees %v, subscribed 2→1 %v, 1→2 %v",
					mode, back, subjectAt(in, 2), mainViewAt(in, 2), in.Visible(2), subscribed(m, 2, 1), subscribed(m, 1, 2))
			}
		})
	})
}

// 同步应用模式下，一次 Flush（periodic 的一个周期）把此前已确认的事实全部应用并结算，积压超过 BatchSize 也一样：
// BatchSize 是一批的大小，不是每个周期的吞吐上限。（耗时预算用完时可以留到下一周期，这里的积压远小于预算。）
// 历史：学习分支 learn/main-review-20261021 KNOWN-LIMITS“main 近期提交审查”第 5 条。
func TestSyncApplyOneFlushDrainsTheCommittedBacklog(t *testing.T) {
	m := newManager(t)
	in := newInterestMode(t, m, ApplySync)
	in.batchSize = 2
	player(t, m, 1)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	movers := []int64{11, 12, 13, 14, 15}
	for _, id := range movers {
		if err := in.Show(id, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
	}
	mustApply(t, in)
	for i, id := range movers {
		commitQueuedMove(t, m, in, factEntity(t, id), spatial.Point{X: 20 + int64(i), Y: 10})
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	visible := in.Visible(1)
	var missing []int64
	for _, id := range movers {
		if !slices.Contains(visible, id) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("ApplySync: one Flush applied %d of %d committed moves (BatchSize %d); still queued: %v",
			len(movers)-len(missing), len(movers), in.batchSize, missing)
	}
}

// 业务线程（Nest 快池 worker、提交后回调）上调用的 Interest 入口不等 AOI 批次：政策阶段持有应用侧状态做空间计算与
// 订阅结算时，Enter / Leave / Show / Hide / Move / MoveView / SetObserverView / Resubscribe / ObserverView 与视野
// 操作都立即返回。（显式 Apply 是“在调用方线程排空并结算”，按设计会等这一步，不在快池上调用，不在此列。）批次用 BandLayer（政策阶段结算时按距离档取层级的业务回调）钉住，不靠时序。
// 历史：学习分支 learn/main-review-20261021 KNOWN-LIMITS“main 近期提交审查”第 6 条（实测阻塞 15～46ms）。
func TestDirectCallsDoNotWaitForTheAOIBatch(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		type pin struct{ entered, release chan struct{} }
		var armed atomic.Pointer[pin]
		m := newManager(t)
		in, err := NewInterest(InterestConfig{
			Manager: m, ApplyMode: mode,
			AOI: AOIConfig{
				Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
				EnterRadius: 120, LeaveRadius: 150,
			},
			BandLayer: func(band int) entity.SyncProfile {
				if p := armed.Swap(nil); p != nil {
					close(p.entered)
					<-p.release
				}
				return DefaultBandProfile(band)
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(in.Close)
		manualApplier(in) // 异步模式由用例走应用线程的一步，事件留给 Flush 结算
		for id := int64(1); id <= 7; id++ {
			player(t, m, id)
		}
		player(t, m, 20)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 3, spatial.Point{X: 500, Y: 500})
		mustEnter(t, in, 5, spatial.Point{X: 700, Y: 700})
		for id, at := range map[int64]spatial.Point{2: {X: 900, Y: 900}, 7: {X: 800, Y: 800}} {
			if err := in.Show(id, at); err != nil {
				t.Fatal(err)
			}
		}
		mustApply(t, in)
		// 一条已提交的移动把 2 带进玩家 1 的视野：结算它时经 BandLayer 取层级。
		commitQueuedMove(t, m, in, factEntity(t, 2), spatial.Point{X: 20, Y: 10})
		stepApplier(t, in)
		p := &pin{entered: make(chan struct{}), release: make(chan struct{})}
		armed.Store(p)
		flushed := make(chan error, 1)
		go func() { flushed <- m.Flush(context.Background()) }()
		select {
		case <-p.entered:
		case <-time.After(10 * time.Second):
			close(p.release)
			t.Fatal("the policy phase never reached the pinned BandLayer")
		}

		calls := []struct {
			name string
			call func()
		}{
			{"Enter", func() { _ = in.Enter(4, 4, spatial.Point{X: 600, Y: 600}, ObserverView{}) }},
			{"Leave", func() { _ = in.Leave(5, 5) }},
			{"Show", func() { _ = in.Show(6, spatial.Point{X: 650, Y: 650}) }},
			{"Hide", func() { _ = in.Hide(7) }},
			{"Move", func() { _ = in.Move(3, spatial.Point{X: 510, Y: 500}) }},
			{"MoveView", func() { _ = in.MoveView(1, spatial.Point{X: 12, Y: 10}) }},
			{"SetObserverView", func() { _, _ = in.SetObserverView(1, ObserverView{}) }},
			{"ObserverView", func() { _, _ = in.ObserverView(1) }},
			{"Resubscribe", func() { in.Resubscribe(1) }},
			{"DefineSpatialView+AttachView", func() {
				_ = in.DefineSpatialView(40, spatial.Point{X: 100, Y: 100}, ObserverView{}, 0)
				_ = in.AttachView(20, 40)
			}},
		}
		done := make([]chan struct{}, len(calls))
		for i, c := range calls {
			done[i] = make(chan struct{})
			go func() {
				defer close(done[i])
				c.call()
			}()
		}
		// 批次一直钉住：拿应用侧锁的调用在放开之前不会返回；不拿的在微秒内返回。界限只是上限，不是概率等待。
		deadline := time.After(5 * time.Second)
		expired := false
		var blocked []string
		for i, c := range calls {
			if !expired {
				select {
				case <-done[i]:
					continue
				case <-deadline:
					expired = true
				}
			}
			select {
			case <-done[i]:
			default:
				blocked = append(blocked, c.name)
			}
		}
		close(p.release)
		if err := <-flushed; err != nil {
			t.Fatal(err)
		}
		for i := range calls {
			<-done[i]
		}
		if len(blocked) > 0 {
			t.Fatalf("%s: while the policy phase held the AOI batch, these calls did not return: %v", mode, blocked)
		}
	})
}

// Leave 之后、下一次结算之前业务关闭又重开了会话并重新 Enter（断线重连、顶号）：Manager 在 CloseSession 时丢掉了
// 这个会话的订阅，所以同一次结算里重新持有的 pair 要重新说给 Manager，而不是当作“一直订阅着”什么也不做。
func TestLeaveAndEnterInOneSettleSubscribeTheReopenedSessionAgain(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
		flushInterest(t, in, m)
		if !subscribed(m, 2, 1) || !subscribed(m, 2, 2) {
			t.Fatal("setup: player 2 does not see 1 and itself")
		}
		if err := in.Leave(2, 2); err != nil {
			t.Fatal(err)
		}
		m.CloseSession(entitysync.SessionID(2))
		if err := m.OpenSession(entitysync.SessionID(2)); err != nil {
			t.Fatal(err)
		}
		mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
		flushInterest(t, in, m)
		if !subscribed(m, 2, 1) || !subscribed(m, 2, 2) || !subscribed(m, 1, 2) {
			t.Fatalf("%s: after Leave → reopen → Enter in one settle: 2 sees 1 %v, 2 sees itself %v, 1 sees 2 %v",
				mode, subscribed(m, 2, 1), subscribed(m, 2, 2), subscribed(m, 1, 2))
		}
	})
}

// 政策阶段里结算被拒的订阅交给 InterestConfig.OnRefusal（显式 Apply 的拒绝仍由返回值给出）。
func TestPolicyPhaseRefusalsReachOnRefusal(t *testing.T) {
	m := newManager(t)
	var got []Refusal
	in, err := NewInterest(InterestConfig{
		Manager: m,
		AOI: AOIConfig{
			Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
			EnterRadius: 120, LeaveRadius: 150,
		},
		OnRefusal: func(refusal Refusal) { got = append(got, refusal) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Close)
	player(t, m, 1)
	// 实体 7 没在 Manager 登记：玩家 1 经距离看见它时订阅被拒。
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	if err := in.Show(7, spatial.Point{X: 20, Y: 10}); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Observer != 1 || got[0].Subject != 7 || got[0].Retry || got[0].Err == nil {
		t.Fatalf("OnRefusal got %+v, want one first refusal of (1, 7)", got)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[1].Retry {
		t.Fatalf("the retry was not reported as a retry: %+v", got)
	}
}

// 还没确认的排队移动被同一目标之后的 Leave → Enter 越过（撤除与放置不等提交）：它后来确认了也作废——目标与主视野
// 留在 Enter 给的位置，不被拖到那条移动的目标。
func TestAnUnconfirmedMoveOvertakenByLeaveAndEnterIsDropped(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
		flushInterest(t, in, m)
		e := factEntity(t, 2)
		mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
		if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
		mutation.Admit()
		// 提交还没确认：Leave → Enter 先生效。
		if err := in.Leave(2, 2); err != nil {
			t.Fatal(err)
		}
		back := spatial.Point{X: 30, Y: 10}
		mustEnter(t, in, 2, back)
		flushInterest(t, in, m)
		mutation.Confirm()
		mutation.Release()
		for round := 0; round < 3; round++ {
			flushInterest(t, in, m)
		}
		if subjectAt(in, 2) != fmt.Sprintf("%v", back) || !slices.Contains(in.Visible(2), int64(1)) || queuedFacts(in) != 0 {
			t.Fatalf("%s: entity 2 at %s, main view at %s, player 2 sees %v, facts left %d",
				mode, subjectAt(in, 2), mainViewAt(in, 2), in.Visible(2), queuedFacts(in))
		}
	})
}

// Drain 的政策钩子（Pending）把“应用方已执行、还没结算”的条目算作没做完：异步模式下应用线程可能在 Manager 的
// 某次 Flush 结算之后、Drain 检查 Pending 之前执行完命令，这时 held 里的变化还没说给 Manager，Drain 不能返回。
// 用例让应用线程走一步（命令已执行、未结算），直接问 Pending，不依赖时序；随后一次 Flush 结算后 Pending 才为假。
// 历史：单写者重构（e5b382a7）后 TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen/async 在负载下偶发失败。
func TestPendingCountsAppliedButUnsettledCommands(t *testing.T) {
	m := newManager(t)
	in := newInterestMode(t, m, ApplyAsync)
	manualApplier(in)
	player(t, m, 1)
	player(t, m, 2)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
	mustApply(t, in)
	if in.queuedPending() {
		t.Fatal("setup: an applied and settled interest still reports pending work")
	}
	// 关系视野让 1 看见远处的 2：应用线程执行命令，结算要等下一次 Flush。
	setRelation(t, in, 1, 2)
	stepApplier(t, in)
	if !in.queuedPending() {
		t.Fatalf("the applier executed the relation view but Pending is false: Drain would return before the subscribe is said (subscribers of 2: %v)", m.Subscribers(2))
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if in.queuedPending() || !slices.Contains(m.Subscribers(2), entitysync.SessionID(1)) {
		t.Fatalf("after the settling Flush: pending=%v subscribers of 2=%v", in.queuedPending(), m.Subscribers(2))
	}
}

// ---- ApplySync 政策阶段的耗时预算：执行日志 + 结算一起计入（方案 §12，维护者 2026-10-22 决定 A）----

// budgetHarness 给同步模式的政策阶段装一套可控的耗时，不靠真实 sleep：Interest 的时钟 = 起点 + 每条已应用事实
// perFact + 每个进入事件 perEvent。进入事件在结算里按距离档取层级（BandLayer）时计数，所以 perEvent 是结算的耗时。
// 时钟只在政策阶段（持 mu 的 Flush goroutine）里读，直接读应用侧状态；用例在两次 Flush 之间用 spent 读。
type budgetHarness struct {
	in                *Interest
	m                 *entitysync.Manager
	perFact, perEvent time.Duration
	queued            int
	events            atomic.Int64
	base              time.Time
}

func newBudgetHarness(t *testing.T, batchSize int, perFact, perEvent time.Duration) *budgetHarness {
	t.Helper()
	h := &budgetHarness{m: newManager(t), perFact: perFact, perEvent: perEvent, base: time.Unix(1_700_000_000, 0)}
	in, err := NewInterest(InterestConfig{
		Manager: h.m, ApplyMode: ApplySync, BatchSize: batchSize,
		AOI: AOIConfig{
			Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 150,
			EnterRadius: 120, LeaveRadius: 150,
		},
		BandLayer: func(band int) entity.SyncProfile {
			h.events.Add(1)
			return DefaultBandProfile(band)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Close)
	h.in = in
	player(t, h.m, 1)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustApply(t, in)
	h.events.Store(0)
	in.now = func() time.Time { return h.base.Add(h.costLocked()) }
	return h
}

// costLocked 是到目前为止的累计耗时：已应用的事实（登记过的 − 还在日志 / 应用中 / 已应用未结算的 + 已应用未结算的）
// 与已结算的进入事件。调用方持有 mu。
func (h *budgetHarness) costLocked() time.Duration {
	h.in.registry.mu.Lock()
	facts := h.in.registry.facts
	h.in.registry.mu.Unlock()
	applied := h.queued - facts + len(h.in.applied)
	return time.Duration(applied)*h.perFact + time.Duration(h.events.Load())*h.perEvent
}

func (h *budgetHarness) spent() time.Duration {
	h.in.mu.Lock()
	defer h.in.mu.Unlock()
	return h.costLocked()
}

// show 在 Manager 登记目标并把它放在远处（观察者 1 看不见），结算一次。
func (h *budgetHarness) show(t *testing.T, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if err := h.m.Register(subjectState(t, id)); err != nil {
			t.Fatal(err)
		}
		if err := h.in.Show(id, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
	}
	mustApply(t, h.in)
	h.events.Store(0)
}

func (h *budgetHarness) move(t *testing.T, id int64, to spatial.Point) {
	t.Helper()
	commitQueuedMove(t, h.m, h.in, factEntity(t, id), to)
	h.queued++
}

// flush 走一次 Flush，返回这次政策阶段按注入耗时花掉的时间。
func (h *budgetHarness) flush(t *testing.T) time.Duration {
	t.Helper()
	before := h.spent()
	if err := h.m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h.spent() - before
}

// warmUp 让政策阶段先测到一步与一次结算的耗时（估计来自之前的步与结算；启动后的第一次没有估计，见方案 §12）。
func (h *budgetHarness) warmUp(t *testing.T, ids ...int64) {
	t.Helper()
	h.show(t, ids...)
	for i, id := range ids {
		h.move(t, id, spatial.Point{X: 20 + int64(i), Y: 60})
	}
	h.flush(t)
	if left := queuedFacts(h.in); left != 0 {
		t.Fatalf("warm-up left %d facts", left)
	}
}

func near(i int) spatial.Point { return spatial.Point{X: 20 + int64(i), Y: 10} }

// 预算内做得完的积压，一次政策阶段全部做完（与没有预算时相同），不记预算停止。
func TestSyncApplyBudgetFinishesWhatFitsInOnePolicyPhase(t *testing.T) {
	h := newBudgetHarness(t, 2, time.Millisecond, time.Millisecond)
	movers := []int64{11, 12, 13, 14, 15}
	h.show(t, movers...)
	for i, id := range movers {
		h.move(t, id, near(i))
	}
	stops := counterValue(budgetStopMetric)
	spent := h.flush(t)
	if left := queuedFacts(h.in); left != 0 || spent > h.in.syncBudget {
		t.Fatalf("one Flush left %d of %d facts and spent %v (budget %v)", left, len(movers), spent, h.in.syncBudget)
	}
	for i, id := range movers {
		if got := subjectAt(h.in, id); got != fmt.Sprintf("%v", near(i)) {
			t.Fatalf("entity %d at %s, want %v", id, got, near(i))
		}
	}
	if got := counterValue(budgetStopMetric) - stops; got != 0 {
		t.Fatalf("budget stops = %d, want 0", got)
	}
}

// 预计超预算时政策阶段停在预算内（执行 + 结算 ≤ 预算），剩下的留给后面的 Flush；最终全部应用，同一目标的两次移动
// 按提交顺序生效，任何一次 Flush 之后都不会退回更早的位置。
func TestSyncApplyBudgetStopsWithinBudgetAndKeepsOrder(t *testing.T) {
	h := newBudgetHarness(t, 4, time.Millisecond, time.Millisecond)
	h.warmUp(t, 901, 902)
	const n = 30
	movers := make([]int64, n)
	for i := range movers {
		movers[i] = int64(100 + i)
	}
	h.show(t, movers...)
	second := func(i int) spatial.Point { return spatial.Point{X: 20 + int64(i), Y: 40} }
	for i, id := range movers {
		h.move(t, id, near(i))
	}
	for i, id := range movers {
		h.move(t, id, second(i))
	}
	stage := make([]int, n) // 0 远处，1 第一次移动，2 第二次移动
	stops := counterValue(budgetStopMetric)
	flushes := 0
	for queuedFacts(h.in) > 0 {
		flushes++
		if flushes > 20 {
			t.Fatalf("no progress: %d facts left after %d Flushes", queuedFacts(h.in), flushes-1)
		}
		spent := h.flush(t)
		if spent > h.in.syncBudget {
			t.Fatalf("Flush %d: the policy phase spent %v (steps + settle), over the budget %v; %d facts left",
				flushes, spent, h.in.syncBudget, queuedFacts(h.in))
		}
		for i, id := range movers {
			now := 0
			switch subjectAt(h.in, id) {
			case fmt.Sprintf("%v", near(i)):
				now = 1
			case fmt.Sprintf("%v", second(i)):
				now = 2
			}
			if now < stage[i] {
				t.Fatalf("Flush %d: entity %d went back from stage %d to %d", flushes, id, stage[i], now)
			}
			stage[i] = now
		}
	}
	if flushes < 2 {
		t.Fatalf("%d facts at 2ms each (step + settle) fit in one Flush with a %v budget: the budget did not stop", 2*n, h.in.syncBudget)
	}
	if got := counterValue(budgetStopMetric) - stops; got == 0 {
		t.Fatal("the policy phase stopped on budget but budget stops did not count it")
	}
	for i, id := range movers {
		if stage[i] != 2 {
			t.Fatalf("entity %d ended at %s, want the second move %v", id, subjectAt(h.in, id), second(i))
		}
	}
}

// 一条事实本身就超过预算时仍然前进：每次政策阶段至少执行一个单位，批切到一条（BatchSize 是 4），只有这一步可以
// 超出预算；之后的留给下一次 Flush。
func TestSyncApplyBudgetStillProgressesWhenOneStepExceedsIt(t *testing.T) {
	h := newBudgetHarness(t, 4, 30*time.Millisecond, 0)
	h.warmUp(t, 901)
	movers := []int64{11, 12, 13, 14, 15, 16}
	h.show(t, movers...)
	for i, id := range movers {
		h.move(t, id, near(i))
	}
	for flush := 1; flush <= len(movers); flush++ {
		before := queuedFacts(h.in)
		h.flush(t)
		if applied := before - queuedFacts(h.in); applied != 1 {
			t.Fatalf("Flush %d applied %d facts at 30ms each (budget %v, BatchSize 4): want the step cut to exactly one",
				flush, applied, h.in.syncBudget)
		}
	}
	if left := queuedFacts(h.in); left != 0 {
		t.Fatalf("%d facts left after %d Flushes", left, len(movers))
	}
}

// 结算的耗时计入预算：执行很便宜、结算很贵（每个进入事件 2ms）时，执行部分必须给结算留出时间，不能先把预算
// 用在执行上、再在预算之外结算。
func TestSyncApplyBudgetCountsTheSettle(t *testing.T) {
	h := newBudgetHarness(t, 64, 100*time.Microsecond, 2*time.Millisecond)
	h.warmUp(t, 901, 902)
	movers := make([]int64, 40)
	for i := range movers {
		movers[i] = int64(100 + i)
	}
	h.show(t, movers...)
	for i, id := range movers {
		h.move(t, id, near(i))
	}
	for flush := 1; queuedFacts(h.in) > 0; flush++ {
		if flush > 20 {
			t.Fatalf("no progress: %d facts left", queuedFacts(h.in))
		}
		before := queuedFacts(h.in)
		if spent := h.flush(t); spent > h.in.syncBudget {
			t.Fatalf("Flush %d applied %d facts and spent %v with the settle (budget %v): the settle was not reserved",
				flush, before-queuedFacts(h.in), spent, h.in.syncBudget)
		}
	}
}

// planSyncStep 的规则逐条：没有估计时不限单位数；放得下就按“剩余 /（执行 + 结算）每单位”取；放不下时第一步仍执行
// 一个单位、之后停止；结算预留随已执行未结算的单位数增长，下限是预算的 1/10。
func TestPlanSyncStepRules(t *testing.T) {
	const ms = time.Millisecond
	budget := 25 * ms
	cases := []struct {
		name      string
		elapsed   time.Duration
		unsettled int
		first     bool
		costs     applyCosts
		limit     int
		run       bool
	}{
		{"no estimate yet, first step", 0, 0, true, applyCosts{}, 0, true},
		{"no estimate yet, room left", 5 * ms, 3, false, applyCosts{}, 0, true},
		{"no estimate yet, only the reserve left", 23 * ms, 3, false, applyCosts{}, 0, false},
		{"fits: (25 - 0 - 2.5) / 2", 0, 0, true, applyCosts{unit: ms, settle: ms}, 11, true},
		{"reserve grows with unsettled units: (25 - 8 - 8) / 2", 8 * ms, 8, false, applyCosts{unit: ms, settle: ms}, 4, true},
		{"next unit plus its settle does not fit: stop", 12 * ms, 12, false, applyCosts{unit: ms, settle: ms}, 0, false},
		{"one unit exceeds the budget: first step still runs one", 0, 0, true, applyCosts{unit: 30 * ms}, 1, true},
		{"one unit exceeds the budget: no second step", 30 * ms, 1, false, applyCosts{unit: 30 * ms}, 0, false},
		{"settle alone exceeds the budget: first step runs one", 0, 40, true, applyCosts{unit: ms, settle: ms}, 1, true},
	}
	for _, c := range cases {
		limit, run := planSyncStep(budget, c.elapsed, c.unsettled, c.first, c.costs)
		if limit != c.limit || run != c.run {
			t.Errorf("%s: planSyncStep = (%d, %v), want (%d, %v)", c.name, limit, run, c.limit, c.run)
		}
	}
}

// ---- 一步的切点：纯前缀（方案 §11.2，维护者 2026-10-22 决定 A）----

// 切点用例里的目标与位置：a、d、b、e 先放在远处；b 被直接 Show 到 shownB，再由提交 X 移到 movedB。
const (
	cutA, cutD, cutB, cutE = int64(2), int64(3), int64(4), int64(5)
)

var (
	cutFar = spatial.Point{X: 900, Y: 900}
	shownB = spatial.Point{X: 500, Y: 500}
	cutTo  = map[int64]spatial.Point{cutA: {X: 20, Y: 10}, cutD: {X: 30, Y: 10}, cutB: {X: 40, Y: 10}, cutE: {X: 50, Y: 10}}
)

// queueAcrossHideShow 排出日志 [move a (X), move d, Hide b, Show b, move b (X), move e]：提交 X（守卫作用域里同时
// 持 a、b）先排 a 的移动；X 还在 handler 里时，另一笔提交排 d 的移动，业务线程直接 Hide(b) → Show(b, shownB)
// （接入层、别的实体的提交后回调都不经 b 的实体锁）；X 再排 b 的移动并确认；最后另一笔提交排 e 的移动。
// 全部事实已确认、还没应用。返回排下的事实数。
func queueAcrossHideShow(t *testing.T, m *entitysync.Manager, in *Interest) int {
	t.Helper()
	a, b := factEntity(t, cutA), factEntity(t, cutB)
	queuedA, queueB, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, closeScope := entity.NewGuardScope("commit-x")
		defer closeScope()
		x := entity.BeginSyncMutation([]entity.IThreadSafeEntity{a, b}, m)
		if err := in.QueueMove(a, cutTo[cutA]); err != nil {
			t.Error(err)
		}
		if entity.CurrentSyncMutation() == nil {
			t.Error("setup: commit X has no current sync mutation, its facts would not share a commit")
		}
		close(queuedA)
		<-queueB
		if err := in.QueueMove(b, cutTo[cutB]); err != nil {
			t.Error(err)
		}
		x.Admit()
		x.Confirm()
		x.Release()
	}()
	<-queuedA
	commitQueuedMove(t, m, in, factEntity(t, cutD), cutTo[cutD])
	if err := in.Hide(cutB); err != nil {
		t.Fatal(err)
	}
	if err := in.Show(cutB, shownB); err != nil {
		t.Fatal(err)
	}
	close(queueB)
	<-done
	commitQueuedMove(t, m, in, factEntity(t, cutE), cutTo[cutE])
	return 4
}

// showCutSubjects 在 Manager 登记 a、d、b、e，把它们放在远处并结算（观察者 1 看不见它们）。
func showCutSubjects(t *testing.T, m *entitysync.Manager, in *Interest) {
	t.Helper()
	for _, id := range []int64{cutA, cutD, cutB, cutE} {
		if err := m.Register(subjectState(t, id)); err != nil {
			t.Fatal(err)
		}
		if err := in.Show(id, cutFar); err != nil {
			t.Fatal(err)
		}
	}
	mustApply(t, in)
}

// checkCutResult 检查全部条目执行完之后的位置：b 在 X 的移动目标（不是 Show 的位置），其余在各自的目标。
func checkCutResult(t *testing.T, in *Interest, how string) {
	t.Helper()
	if got := subjectAt(in, cutB); got != fmt.Sprintf("%v", cutTo[cutB]) {
		t.Fatalf("%s: b at %s after log order [move a (X), move d, Hide b, Show b, move b (X), move e], want %v",
			how, got, cutTo[cutB])
	}
	for _, id := range []int64{cutA, cutD, cutE} {
		if got := subjectAt(in, id); got != fmt.Sprintf("%v", cutTo[id]) {
			t.Fatalf("%s: entity %d at %s, want %v", how, id, got, cutTo[id])
		}
	}
	if left := queuedFacts(in); left != 0 {
		t.Fatalf("%s: %d facts left", how, left)
	}
}

// checkFirstCut 检查第一次切开的位置：X 整笔在这一步里（b 已在 X 的移动目标，提交没被切开），e 还没取（切点落在
// X 结束之后的第一个边界，而不是更远）。
func checkFirstCut(t *testing.T, what, b, e string) {
	t.Helper()
	if b != fmt.Sprintf("%v", cutTo[cutB]) {
		t.Fatalf("%s left b at %s: commit X was cut after its move of a, want its move of b %v in the same step", what, b, cutTo[cutB])
	}
	if e != fmt.Sprintf("%v", cutFar) {
		t.Fatalf("%s reached e (at %s): the cut must land at the first boundary after X ends", what, e)
	}
}

// 一步到了上限（BatchSize 或 ApplySync 的预算单位）之后，切点只落在已取用的提交全部结束之后：切点之前全部执行、
// 之后全部留下（纯前缀）。日志 [move a (X), move d, Hide b, Show b, move b (X), move e] 在第一条之后就到了上限，
// X 还没结束，所以 move d、Hide b、Show b、move b 都在这一步里按序执行，切点落在 move e 之前。
// 历史：旧规则在 move d 处切开、却仍取用同一提交的 move b（“同一提交不切开”），它越过留下的 Hide / Show，按代际
// 作废，b 停在 Show 的位置（单写者重构 eed9c870 引入；§12 的预算让同步模式的切点更多）。
func TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands(t *testing.T) {
	// BatchSize 1：先单走一步（切点规则与模式无关），再按模式排空。
	t.Run("batch_size", func(t *testing.T) {
		forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
			m := newManager(t)
			in := newInterestMode(t, m, mode)
			manualApplier(in)
			in.batchSize = 1
			player(t, m, 1)
			mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
			showCutSubjects(t, m, in)
			queueAcrossHideShow(t, m, in)
			in.applyStep()
			bAfterStep, eAfterStep := subjectAt(in, cutB), subjectAt(in, cutE)
			for round := 0; round < 4; round++ {
				flushInterest(t, in, m)
			}
			checkCutResult(t, in, fmt.Sprintf("%s, BatchSize 1", mode))
			checkFirstCut(t, fmt.Sprintf("%s, BatchSize 1: the first step", mode), bAfterStep, eAfterStep)
		})
	})
	// 显式 Apply 在调用方线程排空，同一条规则。
	t.Run("explicit_apply", func(t *testing.T) {
		forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
			m := newManager(t)
			in := newInterestMode(t, m, mode)
			manualApplier(in)
			in.batchSize = 1
			player(t, m, 1)
			mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
			showCutSubjects(t, m, in)
			queueAcrossHideShow(t, m, in)
			mustApply(t, in)
			checkCutResult(t, in, fmt.Sprintf("%s, Apply with BatchSize 1", mode))
		})
	})
	// ApplySync 的预算只够一个单位（每条事实 30ms，预算 25ms），BatchSize 不限：切点来自预算。异步模式与显式 Apply
	// 不按预算切开。
	t.Run("budget", func(t *testing.T) {
		h := newBudgetHarness(t, 64, 30*time.Millisecond, 0)
		h.warmUp(t, 901)
		h.show(t, cutA, cutD, cutB, cutE)
		h.queued += queueAcrossHideShow(t, h.m, h.in)
		h.flush(t)
		bAfterFirst, eAfterFirst := subjectAt(h.in, cutB), subjectAt(h.in, cutE)
		for flush := 2; queuedFacts(h.in) > 0; flush++ {
			if flush > 10 {
				t.Fatalf("no progress: %d facts left", queuedFacts(h.in))
			}
			h.flush(t)
		}
		checkCutResult(t, h.in, "ApplySync, budget of one unit")
		checkFirstCut(t, "ApplySync, budget of one unit: the first Flush", bAfterFirst, eAfterFirst)
	})
}

// commitInScope 在一个 Nest 守卫作用域里（一笔有身份的提交）依次排 es[i] 到 to[i] 的移动并确认、放行。
func commitInScope(t *testing.T, m *entitysync.Manager, in *Interest, es []*queuedEntity, to []spatial.Point) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, closeScope := entity.NewGuardScope("commit")
		defer closeScope()
		locked := make([]entity.IThreadSafeEntity, len(es))
		for i, e := range es {
			locked[i] = e
		}
		mutation := entity.BeginSyncMutation(locked, m)
		for i, e := range es {
			if err := in.QueueMove(e, to[i]); err != nil {
				t.Error(err)
			}
		}
		mutation.Admit()
		mutation.Confirm()
		mutation.Release()
	}()
	<-done
}

// 一笔提交只要有一条事实还在等（它的实体前面还有没确认的提交），这一步就一条也不取用它的事实，整笔留在原位等，
// 不切成两半；它留下的事实又挡住后面同一目标的事实，那些事实所属的提交也整笔等（连带）。排在后面的命令照常执行，
// 不等它们。前面的提交确认之后，等着的提交在同一步里一起应用，按日志顺序。
// （纯前缀切开要求切点落在已取用提交全部结束之后；取用部分就绪的提交会把它切开，见方案 §11.2。）
//
// 日志：[move b (W，未确认)] [move a、move b (X，已确认)] [move g、move a (Z，已确认)] [Show c]。X 的 b 排在 W 之后，
// 所以 X 整笔等；Z 的 a 排在 X 的 a 之后，不能越过它，所以 Z 也整笔等（g 也不动）。
func TestACommitWithAWaitingFactIsNotTakenInPart(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		manualApplier(in)
		player(t, m, 1)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		const a, b, c, g = int64(2), int64(4), int64(6), int64(8)
		for _, id := range []int64{a, b, c, g} {
			if err := m.Register(subjectState(t, id)); err != nil {
				t.Fatal(err)
			}
			if err := in.Show(id, cutFar); err != nil {
				t.Fatal(err)
			}
		}
		mustApply(t, in)
		ea, eb, eg := factEntity(t, a), factEntity(t, b), factEntity(t, g)
		w := entity.BeginSyncMutation([]entity.IThreadSafeEntity{eb}, m)
		if err := in.QueueMove(eb, spatial.Point{X: 35, Y: 10}); err != nil {
			t.Fatal(err)
		}
		w.Admit()
		commitInScope(t, m, in, []*queuedEntity{ea, eb}, []spatial.Point{{X: 20, Y: 10}, {X: 40, Y: 10}})
		commitInScope(t, m, in, []*queuedEntity{eg, ea}, []spatial.Point{{X: 60, Y: 10}, {X: 25, Y: 10}})
		if err := in.Show(c, spatial.Point{X: 50, Y: 10}); err != nil {
			t.Fatal(err)
		}
		in.applyStep()
		if gotA, gotB, gotG, gotC := subjectAt(in, a), subjectAt(in, b), subjectAt(in, g), subjectAt(in, c); gotA != fmt.Sprintf("%v", cutFar) ||
			gotB != fmt.Sprintf("%v", cutFar) || gotG != fmt.Sprintf("%v", cutFar) || gotC != "{50 10}" {
			t.Fatalf("%s: while X's move of b waits for W: a at %s, b at %s, g at %s (want X and Z held back whole at %v); the later Show put c at %s (want {50 10})",
				mode, gotA, gotB, gotG, cutFar, gotC)
		}
		w.Confirm()
		w.Release()
		in.applyStep()
		if gotA, gotB, gotG := subjectAt(in, a), subjectAt(in, b), subjectAt(in, g); gotA != "{25 10}" || gotB != "{40 10}" || gotG != "{60 10}" {
			t.Fatalf("%s: after W confirmed, one step left a at %s, b at %s, g at %s; want W, X, Z in log order: a {25 10}, b {40 10}, g {60 10}",
				mode, gotA, gotB, gotG)
		}
		flushInterest(t, in, m)
		if left := queuedFacts(in); left != 0 {
			t.Fatalf("%s: %d facts left", mode, left)
		}
	})
}

// 命令也计入同步模式的预算单位：单位上限到了以后，切点同样可以落在命令之前（纯前缀的任何边界），只有命令的一段
// 日志也按预算分到几步，不再整段在一步里执行。
func TestUnitLimitStopsBeforeACommandToo(t *testing.T) {
	m := newManager(t)
	in := newInterestMode(t, m, ApplySync)
	for _, id := range []int64{2, 3, 4} {
		if err := in.Show(id, spatial.Point{X: 100 * id, Y: 10}); err != nil {
			t.Fatal(err)
		}
	}
	in.mu.Lock()
	_, more, _ := in.runLogLocked(1)
	in.mu.Unlock()
	if got := []string{subjectAt(in, 2), subjectAt(in, 3), subjectAt(in, 4)}; got[0] != "{200 10}" || got[1] != "absent" || got[2] != "absent" || !more {
		t.Fatalf("a step limited to one unit over [Show 2, Show 3, Show 4] placed %v (more %v), want only 2 placed and more to run", got, more)
	}
}
