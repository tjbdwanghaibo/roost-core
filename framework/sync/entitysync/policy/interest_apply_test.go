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
// 订阅结算时，Enter / Leave / Show / Hide / Move / MoveView / SetObserverView / Apply / Resubscribe / ObserverView
// 与视野操作都立即返回。批次用 BandLayer（政策阶段结算时按距离档取层级的业务回调）钉住，不靠时序。
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
			{"Resubscribe", func() { _ = in.Resubscribe(1) }},
			{"Apply", func() { _ = in.Apply() }},
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
