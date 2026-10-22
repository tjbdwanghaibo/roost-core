// Interest 政策：块订阅与覆盖预算、排队事实的提交 / 回滚 / 拒绝、事实准入不等政策锁。

package policy

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

func factEntity(t *testing.T, id int64) *queuedEntity {
	t.Helper()
	e := &queuedEntity{entity.NewEntityBase(id, entity.EntityCategory(4), true)}
	e.SetSyncState(subjectState(t, id))
	return e
}

// applyModes：排队事实的语义用例对两种应用模式各跑一遍，断言相同；“何时应用”由 flushInterest / stepApplier /
// applyQueued 按模式处理（同步模式由 Flush 的政策阶段应用，异步模式由应用线程应用）。
var applyModes = []ApplyMode{ApplySync, ApplyAsync}

func forEachApplyMode(t *testing.T, run func(t *testing.T, mode ApplyMode)) {
	t.Helper()
	for _, mode := range applyModes {
		t.Run(mode.String(), func(t *testing.T) { run(t, mode) })
	}
}

// flushInterest 走一次 Flush 应用并结算已确认的排队事实：异步模式先让应用线程把它们应用完（与后台应用线程互斥，
// 不改变谁先谁后的结果），同步模式由这次 Flush 的政策阶段应用；未确认的事实仍留在队列里。
func flushInterest(t *testing.T, in *Interest, m *entitysync.Manager) {
	t.Helper()
	stepApplier(t, in)
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestObservedBlocksKeepOnePositionAndSubscription(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		for _, id := range []int64{1, 2} {
			player(t, m, id)
		}
		mustEnter(t, in, 1, spatial.Point{X: 140, Y: 140})
		original := spatial.Point{X: 900, Y: 900}
		if err := in.Show(2, original); err != nil {
			t.Fatal(err)
		}
		e := factEntity(t, 2)
		blocks := []int64{in.BlockAt(spatial.Point{X: 10, Y: 10}), in.BlockAt(spatial.Point{X: 160, Y: 10})}
		expected := slices.Clone(blocks)
		if err := in.QueueObservedBlocks(e, append(blocks, blocks[0])); err != nil {
			t.Fatal(err)
		}
		blocks[0] = in.BlockAt(original)
		flushInterest(t, in, m)
		if got := m.Subscribers(2); !slices.Equal(got, []entitysync.SessionID{1}) {
			t.Fatalf("subscribers=%v", got)
		}
		if in.aoi.subjects[2] != original {
			t.Fatal("coverage moved physical position")
		}
		if got := in.aoi.index.QueryBlockIndex(in.BlockAt(original)); !slices.Contains(got, int64(2)) {
			t.Fatal("lost owner block")
		}
		if got := in.aoi.index.QueryBlockIndex(expected[0]); slices.Contains(got, int64(2)) {
			t.Fatal("coverage duplicated physical position")
		}
		if !slices.Equal(in.ObservedBlocks(2), expected) {
			t.Fatal("caller mutated coverage")
		}
		if err := in.QueueObservedBlocks(e, expected[1:]); err != nil {
			t.Fatal(err)
		}
		flushInterest(t, in, m)
		if !subscribed(m, 1, 2) {
			t.Fatal("one remaining block was ignored")
		}
		setRelation(t, in, 1, 2)
		mustApply(t, in)
		if err := in.QueueObservedBlocks(e, nil); err != nil {
			t.Fatal(err)
		}
		flushInterest(t, in, m)
		if !subscribed(m, 1, 2) {
			t.Fatal("block removal dropped team subscription")
		}
		setRelation(t, in, 1)
		mustApply(t, in)
		if subscribed(m, 1, 2) {
			t.Fatal("last source not removed")
		}
	})
}

func TestSpatialAndBlockHandoverDoesNotRecreateBaseline(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		transport := &sink{}
		m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close(context.Background())
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
		mustApply(t, in)
		flushInterest(t, in, m)
		before := transport.count(1)
		e := factEntity(t, 2)
		if err := in.QueueSpatial(e, spatial.Point{X: 900, Y: 900}, []int64{in.BlockAt(spatial.Point{X: 10, Y: 10})}); err != nil {
			t.Fatal(err)
		}
		flushInterest(t, in, m)
		if !subscribed(m, 1, 2) || transport.count(1) != before {
			t.Fatal("source handover recreated or removed baseline")
		}
	})
}

func TestCoverageCommitRollbackRejectAndLifetime(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
		e := factEntity(t, 2)
		near := []int64{in.BlockAt(spatial.Point{X: 10, Y: 10})}
		for _, outcome := range []string{"rollback", "reject", "commit"} {
			mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
			if err := in.QueueSpatial(e, spatial.Point{X: 800, Y: 800}, near); err != nil {
				t.Fatal(err)
			}
			flushInterest(t, in, m)
			if len(in.ObservedBlocks(2)) != 0 {
				t.Fatal("unconfirmed coverage escaped")
			}
			switch outcome {
			case "rollback":
				mutation.Finish(false)
			case "reject":
				mutation.Admit()
				mutation.RejectEntities([]int64{2})
			case "commit":
				mutation.Admit()
				mutation.Confirm()
			}
			flushInterest(t, in, m)
			if len(in.ObservedBlocks(2)) != 0 {
				t.Fatal("coverage escaped before release")
			}
			mutation.Release()
			flushInterest(t, in, m)
			if (outcome == "commit") != subscribed(m, 1, 2) {
				t.Fatalf("outcome=%s subscription mismatch", outcome)
			}
		}
		if err := in.QueueObservedBlocks(e, near); err != nil {
			t.Fatal(err)
		}
		if err := in.Hide(2); err != nil {
			t.Fatal(err)
		}
		if err := in.Show(2, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
		flushInterest(t, in, m)
		if subscribed(m, 1, 2) || len(in.ObservedBlocks(2)) != 0 {
			t.Fatal("old coverage resurrected after Hide/Show")
		}
	})
}

func TestCoverageLateObserverBudgetAndVisibilityLimit(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		cfg := InterestConfig{Manager: m, ApplyMode: mode, AOI: interestConfig()}
		cfg.AOI.MaxObservedBlocks = 1
		cfg.AOI.MaxVisible = 1
		in, err := NewInterest(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		player(t, m, 1)
		for _, id := range []int64{2, 3} {
			player(t, m, id)
			if err := in.Show(id, spatial.Point{X: 900, Y: 900}); err != nil {
				t.Fatal(err)
			}
			e := factEntity(t, id)
			if err := in.QueueObservedBlocks(e, []int64{0, 1}); !errors.Is(err, ErrInterestBudget) {
				t.Fatalf("budget=%v", err)
			}
			if err := in.QueueObservedBlocks(e, []int64{-1}); !errors.Is(err, spatial.ErrInvalidBounds) {
				t.Fatalf("invalid block=%v", err)
			}
			if err := in.QueueObservedBlocks(e, []int64{0, 0}); err != nil {
				t.Fatal(err)
			}
		}
		flushInterest(t, in, m)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustApply(t, in)
		if !subscribed(m, 1, 2) || !subscribed(m, 1, 3) {
			t.Fatal("late observer or spatial cap suppressed explicit coverage")
		}
		if err := in.Move(1, spatial.Point{X: 900, Y: 100}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, in)
		if subscribed(m, 1, 2) || subscribed(m, 1, 3) {
			t.Fatal("observer leaving blocks retained coverage")
		}
	})
}

func TestFactAdmissionDoesNotWaitForPolicyLock(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		e := factEntity(t, 1)
		in.mu.Lock()
		done := make(chan error, 1)
		go func() { done <- in.QueueMove(e, spatial.Point{X: 20, Y: 10}) }()
		select {
		case err := <-done:
			in.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			in.mu.Unlock()
			<-done
			t.Fatal("QueueMove waited for AOI computation lock")
		}
	})
}

func TestInflightFactsKeepBudgetAndPendingHeadDoesNotStarve(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
		in.maxQueuedFacts = 1
		entered, release := make(chan struct{}), make(chan struct{})
		player(t, m, 1)
		in.bandLayer = func(int) entity.SyncProfile {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
			return entity.SyncProfile{}
		}
		if err := in.QueueMove(factEntity(t, 1), spatial.Point{X: 30, Y: 10}); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- in.applyQueued() }()
		<-entered
		err := in.QueueMove(factEntity(t, 2), spatial.Point{X: 40, Y: 10})
		close(release)
		if applyErr := <-done; applyErr != nil {
			t.Fatal(applyErr)
		}
		if !errors.Is(err, ErrInterestQueueFull) {
			t.Fatalf("inflight escaped budget: %v", err)
		}
		in.maxQueuedFacts = 4
		in.batchSize = 1
		e := factEntity(t, 1)
		mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
		defer func() { mutation.Finish(false); mutation.Release() }()
		if err := in.QueueMove(e, spatial.Point{X: 800, Y: 800}); err != nil {
			t.Fatal(err)
		}
		if err := in.QueueMove(factEntity(t, 2), spatial.Point{X: 50, Y: 10}); err != nil {
			t.Fatal(err)
		}
		if err := in.applyQueued(); err != nil {
			t.Fatal(err)
		}
		if in.aoi.subjects[2].X != 50 {
			t.Fatal("unconfirmed prefix starved independent ID")
		}
	})
}

func TestParallelObserverBatchesAgainstDistanceReference(t *testing.T) {
	a := interestFixture(t, AOIConfig{EnterRadius: 100, LeaveRadius: 130, BlockSize: 50})
	positions := make(map[int64]spatial.Point)
	observers := make(map[int64]spatial.Point)
	visible := make(map[int64]map[int64]bool)
	rng := rand.New(rand.NewPCG(7, 11))
	point := func() spatial.Point { return spatial.Point{X: rng.Int64N(1000), Y: rng.Int64N(1000)} }
	for id := int64(1); id <= 80; id++ {
		positions[id] = point()
		if err := a.AddSubject(id, positions[id]); err != nil {
			t.Fatal(err)
		}
	}
	for id := int64(1); id <= 20; id++ {
		observers[id] = positions[id]
		visible[id] = make(map[int64]bool)
		if err := a.AddObserver(id, id, observers[id], ObserverView{}); err != nil {
			t.Fatal(err)
		}
		for _, sid := range a.Visible(id) {
			visible[id][sid] = true
		}
	}
	for range 40 {
		a.beginBatch()
		// 每轮每个subject最多一次移动，参考模型独立计算最终半径/滞回关系。
		for id := int64(1); id <= 80; id++ {
			positions[id] = point()
			if err := a.MoveSubject(id, positions[id]); err != nil {
				t.Fatal(err)
			}
		}
		a.endBatch(4)
		for oid, at := range observers {
			var expected []int64
			next := make(map[int64]bool)
			for sid, pos := range positions {
				radius := int64(100)
				if visible[oid][sid] {
					radius = 130
				}
				if sid != oid && spatial.WithinDistance(at, pos, radius) {
					next[sid] = true
					expected = append(expected, sid)
				}
			}
			slices.Sort(expected)
			if !slices.Equal(a.Visible(oid), expected) {
				t.Fatalf("observer %d mismatch", oid)
			}
			visible[oid] = next
		}
		a.Flush()
	}
}

// 已确认内容不能越过尚未应用的空间事实。否则分批或并发入队时会向旧
// AOI订阅发送新位置，而退出/进入关系要等下一轮才应用。
func TestQueuedSpatialFactBlocksContentUntilPolicyApplied(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		e := factEntity(t, 1)
		if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
		prepared, err := e.Sync().PrepareViews(nil, []entity.SyncProfile{{}})
		if prepared != nil {
			prepared.Abort()
		}
		if !errors.Is(err, entity.ErrSyncCommitPending) {
			t.Fatalf("content overtook pending spatial fact: %v", err)
		}
		if err := in.applyQueued(); err != nil {
			t.Fatal(err)
		}
		prepared, err = e.Sync().PrepareViews(nil, []entity.SyncProfile{{}})
		if err != nil {
			t.Fatal(err)
		}
		prepared.Abort()
	})
}

func TestPublicationHoldsReleaseOnDiscardAndClose(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		for _, action := range []string{"rollback", "hide", "leave", "close"} {
			t.Run(action, func(t *testing.T) {
				m := newManager(t)
				in := newInterestMode(t, m, mode)
				mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
				e := factEntity(t, 1)
				mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
				if err := in.QueueObservedBlocks(e, []int64{0}); err != nil {
					t.Fatal(err)
				}
				switch action {
				case "rollback":
					mutation.Finish(false)
					mutation.Release()
					if err := in.applyQueued(); err != nil {
						t.Fatal(err)
					}
				case "hide":
					mutation.Admit()
					mutation.Confirm()
					mutation.Release()
					if err := in.Hide(1); err != nil {
						t.Fatal(err)
					}
					// 被 Hide 作废的事实在下一次结算（同一 Flush 的捕获之前）释放屏障。
					if err := in.applyQueued(); err != nil {
						t.Fatal(err)
					}
				case "leave":
					mutation.Admit()
					mutation.Confirm()
					mutation.Release()
					if err := in.Leave(1, 1); err != nil {
						t.Fatal(err)
					}
					if err := in.applyQueued(); err != nil {
						t.Fatal(err)
					}
				case "close":
					mutation.Admit()
					mutation.Confirm()
					mutation.Release()
					in.Close()
				}
				prepared, err := e.Sync().PrepareViews(nil, []entity.SyncProfile{{}})
				if err != nil {
					t.Fatalf("publication hold leaked on %s: %v", action, err)
				}
				prepared.Abort()
			})
		}
	})
}

func TestBatchLimitRetainsLaterEntityPublication(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		in.batchSize = 1
		// 同步模式一次政策阶段在预算内排空日志，只有预算用完才停在批次边界：预算设到最小，让它在第一批之后停。
		in.syncBudget = time.Nanosecond
		manualApplier(in)
		// 两条 Enter 排在事实前面：切点只落在前缀边界，预算只够一个单位时 Enter 也占一个单位（命令计入单位）。
		for id := int64(1); id <= 2; id++ {
			mustEnter(t, in, id, spatial.Point{X: 10 * id, Y: 10})
		}
		var entities []*queuedEntity
		for id := int64(1); id <= 2; id++ {
			e := factEntity(t, id)
			entities = append(entities, e)
			if err := in.QueueMove(e, spatial.Point{X: 800, Y: 800}); err != nil {
				t.Fatal(err)
			}
		}
		if err := in.applyQueued(); err != nil {
			t.Fatal(err)
		}
		first, err := entities[0].Sync().PrepareViews(nil, []entity.SyncProfile{{}})
		if err != nil {
			t.Fatal(err)
		}
		first.Abort()
		second, err := entities[1].Sync().PrepareViews(nil, []entity.SyncProfile{{}})
		if second != nil {
			second.Abort()
		}
		if !errors.Is(err, entity.ErrSyncCommitPending) {
			t.Fatalf("deferred entity published: %v", err)
		}
		if err := in.applyQueued(); err != nil {
			t.Fatal(err)
		}
		second, err = entities[1].Sync().PrepareViews(nil, []entity.SyncProfile{{}})
		if err != nil {
			t.Fatal(err)
		}
		second.Abort()
	})
}

type queuedEntity struct{ *entity.EntityBase }

func (e *queuedEntity) Base() *entity.EntityBase { return e.EntityBase }

func TestQueuedInterestFactsFollowCommitAndRollback(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		if err := in.Enter(1, 1, spatial.Point{X: 10, Y: 10}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		if err := in.Enter(2, 2, spatial.Point{X: 20, Y: 10}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, in)
		e := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
		e.SetSyncState(subjectState(t, 2))
		es := []entity.IThreadSafeEntity{e}
		first := entity.BeginSyncMutation(es, m)
		if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
		first.Admit()
		first.Release()
		flushInterest(t, in, m)
		if len(in.Visible(1)) != 1 {
			t.Fatal("unconfirmed movement applied")
		}
		first.Confirm()
		flushInterest(t, in, m)
		if len(in.Visible(1)) != 0 {
			t.Fatal("confirmed movement not applied")
		}
		second := entity.BeginSyncMutation(es, m)
		if err := in.QueueMove(e, spatial.Point{X: 20, Y: 10}); err != nil {
			t.Fatal(err)
		}
		second.Finish(false)
		second.Release()
		flushInterest(t, in, m)
		if len(in.Visible(1)) != 0 || queuedFacts(in) != 0 {
			t.Fatal("rolled back facts escaped or leaked")
		}
		if subscribed(m, 2, 1) {
			t.Fatal("rolled back movement escaped")
		}
	})
}

func TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, Interval: 5 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
		mustApply(t, in)
		m.CloseSession(2)

		// 会话 2 已关闭：挂给它的关系视野由政策阶段应用，订阅一直被拒。
		setRelation(t, in, 2, 1)

		stalledBefore := counterValue("entitysync_interest_retry_stalled_total")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.Drain(ctx); err != nil {
			t.Fatalf("Drain with a pair whose session is closed for good: %v (retry=%d)", err, retryLen(in))
		}
		if got := counterValue("entitysync_interest_retry_stalled_total") - stalledBefore; got != 1 {
			t.Fatalf("stalled retry counted %d times, want 1", got)
		}
		if retryLen(in) != 1 {
			t.Fatalf("stalled pair left the retry list: retry=%d", retryLen(in))
		}
		// 再排一轮 Drain 也不再计数（每个 pair 只告警一次）。
		if err := m.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if got := counterValue("entitysync_interest_retry_stalled_total") - stalledBefore; got != 1 {
			t.Fatalf("stalled retry re-counted: %d", got)
		}
		// 会话重开：停滞的 pair 仍被重说，订阅成功。
		if err := m.OpenSession(2); err != nil {
			t.Fatal(err)
		}
		if !subscribed(m, 2, 1) {
			t.Fatal("stalled pair was dropped: reopened session never subscribed")
		}
		if retryLen(in) != 0 {
			t.Fatalf("accepted pair still in retry: %d", retryLen(in))
		}
	})
}

// injectFact 绕过 queueFact 把一条已确认的事实直接放进日志（不唤醒应用线程）；代际取目标当前已应用的代际。
func injectFact(in *Interest, id int64, at spatial.Point) {
	in.mu.Lock()
	gen := in.placed[id]
	in.mu.Unlock()
	r := &in.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = true
	r.facts++
	r.log = append(r.log, logEntry{fact: queuedInterestFact{id: id, gen: gen, at: at, condition: func() (bool, bool) { return true, false }}})
}

// queuedFacts 是门控事实的预算占用（日志里的、应用中的与已应用未结算的）；loggedFacts 只数还在日志里的。
func queuedFacts(in *Interest) int {
	in.registry.mu.Lock()
	defer in.registry.mu.Unlock()
	return in.registry.facts
}

func loggedFacts(in *Interest) int {
	in.registry.mu.Lock()
	defer in.registry.mu.Unlock()
	n := 0
	for _, entry := range in.registry.log {
		if entry.op == nil {
			n++
		}
	}
	return n
}

func retryLen(in *Interest) int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.retry)
}

func counterValue(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

func TestQueuedFactsInvalidatedByLeaveOrHideDoNotWedgeFlush(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		ctx := context.Background()
		t.Run("leave", func(t *testing.T) {
			frames := &sink{}
			m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: frames})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.Close(context.Background()) })
			in := newInterestMode(t, m, mode)
			player(t, m, 1)
			player(t, m, 2)
			player(t, m, 4)
			mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
			mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
			mustApply(t, in)
			e := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
			e.SetSyncState(subjectState(t, 2))
			batch := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
			if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}); err != nil {
				t.Fatal(err)
			}
			batch.Admit()
			batch.Confirm()
			batch.Release()
			// 2 挂着一个看 1 的关系视野（尚未应用）。
			setRelation(t, in, 2, 1)
			// 事实已就绪、尚未应用：此刻业务让 2 离开。
			if err := in.Leave(2, 2); err != nil {
				t.Fatal(err)
			}
			for round := 0; round < 3; round++ {
				stepApplier(t, in)
				if err := m.Flush(ctx); err != nil {
					t.Fatalf("flush %d after Leave with a ready queued fact: %v", round, err)
				}
			}
			if subscribed(m, 2, 1) {
				t.Fatal("a relation view attached before Leave resurrected 2's interest in 1")
			}
			// 其他 subject 照常出帧：4 走进 1 的视野，会话 1 要收到它的快照。
			before := frames.count(1)
			mustEnter(t, in, 4, spatial.Point{X: 15, Y: 10})
			flushInterest(t, in, m)
			if frames.count(1) == before {
				t.Fatal("session 1 received nothing after the invalidated fact: the manager stopped emitting")
			}
			// 同一 id 重新进入：被 Leave 作废的旧位置不能套到它身上。
			mustEnter(t, in, 2, spatial.Point{X: 30, Y: 10})
			flushInterest(t, in, m)
			if !subscribed(m, 1, 2) {
				t.Fatal("stale queued move applied to the re-entered id")
			}
		})

		t.Run("hide", func(t *testing.T) {
			m := newManager(t)
			in := newInterestMode(t, m, mode)
			player(t, m, 1)
			player(t, m, 3)
			mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
			if err := in.Show(3, spatial.Point{X: 20, Y: 10}); err != nil {
				t.Fatal(err)
			}
			mustApply(t, in)
			e := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
			e.SetSyncState(subjectState(t, 3))
			batch := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
			if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}); err != nil {
				t.Fatal(err)
			}
			// 玩家 3 挂着一个看 1 的关系视野：Hide 只把实体 3 从被观察方移走，玩家 3 的挂接不因 Hide 失效。
			setRelation(t, in, 3, 1)
			batch.Admit()
			batch.Confirm()
			batch.Release()
			if err := in.Hide(3); err != nil {
				t.Fatal(err)
			}
			for round := 0; round < 3; round++ {
				stepApplier(t, in)
				if err := m.Flush(ctx); err != nil {
					t.Fatalf("flush %d after Hide with a ready queued fact: %v", round, err)
				}
			}
			if !subscribed(m, 3, 1) {
				t.Fatal("a relation view Hide did not touch was lost")
			}
			if err := m.Stop(ctx); err != nil {
				t.Fatalf("Stop after Hide with a ready queued fact: %v", err)
			}
		})
	})
}

// 结构性守卫：将来若又出现绕过 Leave / Hide 改写的移除路径，应用失败的事实报告一次后丢弃，不再让之后每一次 Flush 都失败。
func TestQueuedFactThatCannotApplyFailsOneFlushOnly(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustApply(t, in)
		// 一条应用时才失败的事实（边界外，绕过了登记侧的检查）：应用方丢弃它并攒下错误，由下一次 Flush 的结算返回一次。
		injectFact(in, 1, spatial.Point{X: -5, Y: -5})
		stepApplier(t, in)
		if err := m.Flush(context.Background()); err == nil {
			t.Fatal("unappliable fact not reported")
		}
		for round := 0; round < 2; round++ {
			if err := m.Flush(context.Background()); err != nil {
				t.Fatalf("flush %d still failing on a dropped fact: %v", round, err)
			}
		}
	})
}

// 异步模式：Flush 只结算已经应用好的结果：排队事实的 AOI 应用由应用线程做，不在 Manager 的 Flush 里。
// 后台应用线程停掉（Enter 等命令也会启动它，不停掉它就可能在断言之前把注入的
// 事实应用掉），直接把一条已确认的事实放进日志；Flush 之后事实必须仍在日志里、AOI 里的位置不变、订阅没变。
// 应用一步之后，下一次 Flush 才结算并下发。
func TestFlushSettlesButDoesNotApplyQueuedFacts(t *testing.T) {
	m := newManager(t)
	in := newInterestMode(t, m, ApplyAsync)
	manualApplier(in)
	player(t, m, 1)
	player(t, m, 2)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
	mustApply(t, in)
	flushInterest(t, in, m)
	injectFact(in, 2, spatial.Point{X: 20, Y: 10})
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	in.mu.Lock()
	at := in.aoi.subjects[2]
	in.mu.Unlock()
	queued := loggedFacts(in)
	if at != (spatial.Point{X: 900, Y: 900}) || queued != 1 {
		t.Fatalf("Flush applied a queued fact on its own path: position=%v queued=%d", at, queued)
	}
	if slices.Contains(m.Subscribers(2), entitysync.SessionID(1)) {
		t.Fatal("Flush subscribed through an unapplied fact")
	}
	stepApplier(t, in)
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Subscribers(2), entitysync.SessionID(1)) {
		t.Fatal("applied fact not settled by the next Flush")
	}
}

func mustEnter(t *testing.T, in *Interest, id int64, at spatial.Point) {
	t.Helper()
	if err := in.Enter(id, id, at, ObserverView{}); err != nil {
		t.Fatal(err)
	}
}

func (s *sink) count(session entitysync.SessionID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames[session]
}

// 一笔事务同时修改本地实体与 Remote 实体，本地部分已持久提交、Remote 部分被权威持久拒绝：finalizer 按 Remote 批次的实体 ID 调用
// RejectEntities，本地实体排队的移动事实照常应用到 AOI；只有被拒绝的 Remote 实体的事实被丢弃。
// 历史：RR-20260926-58。
func TestRemoteRejectKeepsCommittedLocalInterestFact(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		player(t, m, 3)
		for id, x := range map[int64]int64{1: 10, 2: 20, 3: 30} {
			if err := in.Enter(id, id, spatial.Point{X: x, Y: 10}, ObserverView{}); err != nil {
				t.Fatal(err)
			}
		}
		mustApply(t, in)
		if got := len(in.Visible(1)); got != 2 {
			t.Fatalf("setup: observer 1 sees %d subject(s), want 2", got)
		}
		local := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
		local.SetSyncState(subjectState(t, 2))
		remote := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
		remote.SetSyncState(subjectState(t, 3))
		mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{local, remote}, m)
		if err := in.QueueMove(local, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
		if err := in.QueueMove(remote, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
		mutation.Admit()
		mutation.Release()
		// Remote 确认无结论 → finalizer 拿到持久拒绝：只拒绝 Remote 批次里的实体。
		mutation.RejectEntities([]int64{remote.ID()})
		flushInterest(t, in, m)
		if !local.Sync().SyncCommitReady() || !remote.Sync().SyncCommitReady() {
			t.Fatalf("gates still frozen: local=%v remote=%v", local.Sync().SyncCommitReady(), remote.Sync().SyncCommitReady())
		}
		visible := in.Visible(1)
		if slices.Contains(visible, int64(2)) {
			t.Fatalf("local entity 2 moved to (900,900) in durably committed memory, but its AOI move was discarded by the Remote rejection: observer 1 still sees %v", visible)
		}
		if !slices.Contains(visible, int64(3)) {
			t.Fatalf("rejected Remote entity 3's move was applied: observer 1 sees %v, want 3 still in range", visible)
		}
	})
}

// 没有 SubjectSyncState 的实体只能经 SyncConditionFor 的回退路径看整笔提交：RejectEntities 之后按实体 ID 判断，
// 被拒绝的 Remote 实体的事实丢弃，已提交的本地实体的事实就绪。
func TestRejectEntitiesJudgesEntitiesWithoutSyncStateByID(t *testing.T) {
	m := newManager(t)
	_, closeScope := entity.NewGuardScope("reject-entities")
	defer closeScope()
	local := &queuedEntity{entity.NewEntityBase(2, entity.EntityCategory(4), true)}
	remote := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
	subject := &queuedEntity{entity.NewEntityBase(4, entity.EntityCategory(4), true)}
	subject.SetSyncState(subjectState(t, 4))
	mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{subject}, m)
	localFacts, remoteFacts := entity.SyncConditionFor(local), entity.SyncConditionFor(remote)
	mutation.Admit()
	mutation.Release()
	if ready, _ := localFacts(); ready {
		t.Fatal("facts ready before any durable conclusion")
	}
	mutation.RejectEntities([]int64{remote.ID()})
	mutation.Finish(false)
	if ready, discarded := localFacts(); !ready || discarded {
		t.Fatalf("committed local entity facts ready=%v discarded=%v, want ready", ready, discarded)
	}
	if ready, discarded := remoteFacts(); ready || !discarded {
		t.Fatalf("rejected Remote entity facts ready=%v discarded=%v, want discarded", ready, discarded)
	}
	if !subject.Sync().SyncCommitReady() {
		t.Fatal("committed entity gate still frozen")
	}
}

// 纯 Remote 事务被拒绝（批次覆盖 mutation 的全部实体）：行为与 Reject 相同——门不再阻塞、本提交的事实全部丢弃。
func TestPureRemoteRejectDiscardsItsInterestFacts(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 3)
		if err := in.Enter(1, 1, spatial.Point{X: 10, Y: 10}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		if err := in.Enter(3, 3, spatial.Point{X: 30, Y: 10}, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, in)
		remote := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
		remote.SetSyncState(subjectState(t, 3))
		mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{remote}, m)
		if err := in.QueueMove(remote, spatial.Point{X: 900, Y: 900}); err != nil {
			t.Fatal(err)
		}
		mutation.Admit()
		mutation.Release()
		mutation.RejectEntities([]int64{remote.ID()})
		flushInterest(t, in, m)
		if !remote.Sync().SyncCommitReady() {
			t.Fatal("rejected commit still blocks the gate")
		}
		if visible := in.Visible(1); !slices.Contains(visible, int64(3)) {
			t.Fatalf("rejected Remote move was applied: observer 1 sees %v", visible)
		}
	})
}

// D3：Drain 超时时，Interest 报出仍让它等待的条目——未确认提交的排队事实、尚未判定停滞的被拒订阅（带最后一次错误）。
func TestDrainTimeoutListsInterestStalls(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		// 周期远长于 ctx：每个被拒 pair 只被重说一次，仍计入 queuedPending。
		m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, Interval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		player(t, m, 3)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
		mustEnter(t, in, 3, spatial.Point{X: 500, Y: 500})
		mustApply(t, in)
		m.CloseSession(2)

		// 玩家 2 的会话已关闭：挂给它的关系视野应用后订阅被拒（异步模式由应用线程应用，这里先走一步，结果确定）。
		setRelation(t, in, 2, 1)
		stepApplier(t, in)

		// 实体 3 的提交一直没确认：事实留在队列里。
		pendingEntity := &queuedEntity{entity.NewEntityBase(3, entity.EntityCategory(4), true)}
		pendingEntity.SetSyncState(subjectState(t, 3))
		unconfirmed := entity.BeginSyncMutation([]entity.IThreadSafeEntity{pendingEntity}, m)
		if err := in.QueueMove(pendingEntity, spatial.Point{X: 500, Y: 510}); err != nil {
			t.Fatal(err)
		}
		unconfirmed.Admit()
		t.Cleanup(unconfirmed.Release)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		err = m.Drain(ctx)
		var stalled *entitysync.DrainStalledError
		if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &stalled) {
			t.Fatalf("drain: %v", err)
		}
		var queued, refused *entitysync.DrainStall
		for i := range stalled.Stalls {
			stall := &stalled.Stalls[i]
			switch {
			case stall.Source == "interest" && stall.Reason == "commit_pending" && stall.Subject == 3:
				queued = stall
			case stall.Source == "interest" && stall.Reason == "subscribe_refused" && stall.Subject == 1 && stall.Session == 2:
				refused = stall
			}
		}
		if queued == nil || refused == nil {
			t.Fatalf("interest stalls missing: %v", err)
		}
		if refused.Err == nil || refused.Waited <= 0 || !strings.Contains(refused.Detail, "observer 2, refused 1 time") {
			t.Fatalf("refused stall = %+v", *refused)
		}
	})
}

// Drain 诊断与登记并发：诊断用的事实计数只在登记侧锁内读取。用例先走到“已应用、未结算、
// 没有排队事实”的状态（applied-unsettled）——只有这个状态下诊断才会去比较事实计数，已结算的状态会短路那次读取，
// 在那里跑通不能说明没有竞争。随后一边 QueueMove 一边取诊断，-race 下锁外读取会报数据竞争。
// 两种应用模式都走手动应用（不启动后台应用线程），事实只登记、不被应用，诊断看到的是确定的两种状态之一。
func TestDrainStallsReadsFactCountUnderTheRegistryLock(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		manualApplier(in)
		e := factEntity(t, 2)
		if err := in.Show(e.ID(), spatial.Point{X: 10, Y: 10}); err != nil {
			t.Fatal(err)
		}
		in.applyStep()
		if stalls, _ := in.drainStalls(8); len(stalls) != 1 || stalls[0].Reason != "applied_unsettled" {
			t.Fatalf("applied but unsettled: stalls = %+v", stalls)
		}
		in.maxQueuedFacts = 10000
		const moves = 2000
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range moves {
				if err := in.QueueMove(e, spatial.Point{X: 20, Y: 20}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		for range moves {
			stalls, total := in.drainStalls(8)
			// 每次诊断取的是一个一致的快照：要么还没有排队事实（applied_unsettled），要么有（queued_fact，且不再报
			// applied_unsettled）。
			if total != len(stalls) || len(stalls) != 1 {
				t.Fatalf("stalls = %+v (total %d)", stalls, total)
			}
			if reason := stalls[0].Reason; reason != "applied_unsettled" && reason != "queued_fact" {
				t.Fatalf("stall reason = %q", reason)
			}
		}
		<-done
		if stalls, _ := in.drainStalls(8); len(stalls) != 1 || stalls[0].Reason != "queued_fact" || stalls[0].Subject != e.ID() {
			t.Fatalf("after %d queued moves: stalls = %+v", moves, stalls)
		}
	})
}

// stepApplier 让应用线程走一步（与后台应用线程互斥于 Interest.mu，可以同时存在）。同步模式没有应用线程，
// 事实由下一次 Flush 的政策阶段应用，这里什么也不做。
func stepApplier(t *testing.T, in *Interest) {
	t.Helper()
	if in.applyMode == ApplySync {
		return
	}
	for {
		_, more, _ := in.applyStep()
		if !more {
			return
		}
	}
}

// applyQueued 是“应用一批并结算”的测试替身：异步模式应用线程走一步，再按 Flush 的政策阶段结算；同步模式就是
// Flush 的政策阶段本身（应用一批并结算）。
func (in *Interest) applyQueued() error {
	if in.applyMode == ApplyAsync {
		in.applyStep()
	}
	return in.settleQueued()
}

// manualApplier 让这个异步模式的 Interest 不启动后台应用线程，由用例自己调 applyStep 一步一步走（断言单步的取批
// 规则时用）。同步模式本来就没有应用线程。
func manualApplier(in *Interest) {
	if in.applyMode == ApplySync {
		return
	}
	in.registry.mu.Lock()
	started := in.applierStarted
	in.applierStarted = true
	in.registry.mu.Unlock()
	if !started {
		close(in.applierDone)
		return
	}
	// 之前的登记已经启动了后台应用线程：停掉它（Close 之后再关一次 applierStop 用新的通道）。
	close(in.applierStop)
	<-in.applierDone
	in.applierStop = make(chan struct{})
}

// 后台应用线程与 Manager 周期 Flush 同时运行：回滚的移动（落在观察者 W 身边）从不生效——W 的会话一帧也没收到；
// 已提交的移动按顺序生效，最后的位置就是最后一笔提交的位置；Drain 等到在途事实全部应用并结算。
func TestApplierNeverAppliesRolledBackFactsAndKeepsOrder(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		transport := &sink{}
		m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: transport, Interval: 2 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		in := newInterestMode(t, m, mode)
		const mover, watcher = int64(2), int64(9)
		player(t, m, mover)
		if err := m.OpenSession(entitysync.SessionID(watcher)); err != nil {
			t.Fatal(err)
		}
		trap := spatial.Point{X: 900, Y: 900}
		if err := in.Watch(watcher, trap, ObserverView{}); err != nil {
			t.Fatal(err)
		}
		if err := in.Show(mover, spatial.Point{X: 100, Y: 100}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, in)
		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		e := factEntity(t, mover)
		rng := rand.New(rand.NewPCG(7, 11))
		last := spatial.Point{X: 100, Y: 100}
		for step := range 400 {
			mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
			commit := rng.IntN(3) != 0
			at := trap
			if commit {
				at = spatial.Point{X: 100 + int64(step%50)*4, Y: 100}
			}
			if err := in.QueueMove(e, at); err != nil {
				t.Fatal(err)
			}
			if commit {
				mutation.Admit()
				mutation.Confirm()
				last = at
			} else {
				mutation.Finish(false)
			}
			mutation.Release()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		if err := m.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		in.mu.Lock()
		at := in.aoi.subjects[mover]
		in.mu.Unlock()
		queued, inflight := loggedFacts(in), queuedFacts(in)
		if at != last || queued != 0 || inflight != 0 {
			t.Fatalf("after Drain: position=%v want %v, queued=%d inflight=%d", at, last, queued, inflight)
		}
		if frames := transport.count(entitysync.SessionID(watcher)); frames != 0 {
			t.Fatalf("a rolled back move reached the watcher: %d frame(s)", frames)
		}
	})
}

// 同一实体的事实按入队顺序生效：后一笔先确认也要等前一笔；前一笔确认后两笔按顺序应用，最终在后一笔的位置。
func TestApplierKeepsPerEntityOrderAcrossOutOfOrderConfirms(t *testing.T) {
	forEachApplyMode(t, func(t *testing.T, mode ApplyMode) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		manualApplier(in)
		player(t, m, 2)
		if err := in.Show(2, spatial.Point{X: 100, Y: 100}); err != nil {
			t.Fatal(err)
		}
		mustApply(t, in)
		e := factEntity(t, 2)
		first := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
		if err := in.QueueMove(e, spatial.Point{X: 200, Y: 100}); err != nil {
			t.Fatal(err)
		}
		first.Admit()
		first.Release()
		second := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
		if err := in.QueueMove(e, spatial.Point{X: 300, Y: 100}); err != nil {
			t.Fatal(err)
		}
		second.Admit()
		second.Confirm()
		second.Release()
		if err := in.applyQueued(); err != nil {
			t.Fatal(err)
		}
		if at := in.aoi.subjects[2]; at != (spatial.Point{X: 100, Y: 100}) {
			t.Fatalf("a later fact overtook an unconfirmed earlier one: %v", at)
		}
		first.Confirm()
		if err := in.applyQueued(); err != nil {
			t.Fatal(err)
		}
		if at := in.aoi.subjects[2]; at != (spatial.Point{X: 300, Y: 100}) {
			t.Fatalf("facts not applied in order: %v", at)
		}
	})
}

// 异步模式的背压：已应用、未结算的事实仍占队列预算；队列满时 QueueMove 返回 ErrInterestQueueFull，结算之后恢复。
func TestAppliedButUnsettledFactsKeepTheQueueBudget(t *testing.T) {
	m := newManager(t)
	cfg := InterestConfig{Manager: m, AOI: interestConfig(), MaxQueuedFacts: 2, ApplyMode: ApplyAsync}
	in, err := NewInterest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.Close)
	manualApplier(in)
	player(t, m, 2)
	if err := in.Show(2, spatial.Point{X: 100, Y: 100}); err != nil {
		t.Fatal(err)
	}
	mustApply(t, in)
	e := factEntity(t, 2)
	for x := int64(200); x <= 300; x += 100 {
		if err := in.QueueMove(e, spatial.Point{X: x, Y: 100}); err != nil {
			t.Fatal(err)
		}
	}
	stepApplier(t, in)
	if err := in.QueueMove(e, spatial.Point{X: 400, Y: 100}); !errors.Is(err, ErrInterestQueueFull) {
		t.Fatalf("applied but unsettled facts left the budget: %v", err)
	}
	flushInterest(t, in, m)
	if err := in.QueueMove(e, spatial.Point{X: 400, Y: 100}); err != nil {
		t.Fatalf("settled facts still hold the budget: %v", err)
	}
}

// 异步模式：已应用、未结算的事实仍挡住内容发布：内容与它带来的进出视野要在同一次结算之后、同一帧里出现。
func TestAppliedFactHoldsContentUntilSettled(t *testing.T) {
	m := newManager(t)
	in := newInterestMode(t, m, ApplyAsync)
	manualApplier(in)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	e := factEntity(t, 1)
	if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}); err != nil {
		t.Fatal(err)
	}
	stepApplier(t, in)
	prepared, err := e.Sync().PrepareViews(nil, []entity.SyncProfile{{}})
	if prepared != nil {
		prepared.Abort()
	}
	if !errors.Is(err, entity.ErrSyncCommitPending) {
		t.Fatalf("content published before the applied fact was settled: %v", err)
	}
	if err := in.settleQueued(); err != nil {
		t.Fatal(err)
	}
	prepared, err = e.Sync().PrepareViews(nil, []entity.SyncProfile{{}})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Abort()
}

// ---- 两种应用模式 ----

// 同步模式的视野上限在构造时检查：显式超过主视野 + SyncApplyMaxExtraViews 拒绝构造并提示改用 ApplyAsync；零值取这个上限
// （主视野 + 8 个额外视野可以，第 9 个额外视野 ErrViewLimit）。异步模式不受此限（零值 64）。未知模式拒绝构造。
func TestSyncApplyModeCapsViewsPerPlayer(t *testing.T) {
	config := func(t *testing.T, mode ApplyMode, views int) InterestConfig {
		return InterestConfig{Manager: newManager(t), AOI: interestConfig(), ApplyMode: mode, MaxViewsPerPlayer: views}
	}
	_, err := NewInterest(config(t, ApplySync, SyncApplyMaxViewsPerPlayer+1))
	if !errors.Is(err, ErrInterestConfig) || !strings.Contains(err.Error(), "ApplyAsync") {
		t.Fatalf("ApplySync with %d views per player: %v, want ErrInterestConfig pointing at ApplyAsync", SyncApplyMaxViewsPerPlayer+1, err)
	}
	if _, err := NewInterest(config(t, ApplyMode(9), 0)); !errors.Is(err, ErrInterestConfig) {
		t.Fatalf("unknown apply mode accepted: %v", err)
	}
	attachExtra := func(t *testing.T, cfg InterestConfig, extra int) error {
		in, err := NewInterest(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(in.Close)
		player(t, cfg.Manager, 1)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		for id := ViewID(1); id <= ViewID(extra); id++ {
			if err := in.DefineRelationView(id, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			if err := in.AttachView(1, id); err != nil {
				return fmt.Errorf("extra view %d: %w", id, err)
			}
		}
		return nil
	}
	for _, views := range []int{0, SyncApplyMaxViewsPerPlayer} {
		if err := attachExtra(t, config(t, ApplySync, views), SyncApplyMaxExtraViews); err != nil {
			t.Fatalf("ApplySync MaxViewsPerPlayer=%d: main view + %d extra refused: %v", views, SyncApplyMaxExtraViews, err)
		}
		if err := attachExtra(t, config(t, ApplySync, views), SyncApplyMaxExtraViews+1); !errors.Is(err, ErrViewLimit) {
			t.Fatalf("ApplySync MaxViewsPerPlayer=%d: extra view %d: %v, want ErrViewLimit", views, SyncApplyMaxExtraViews+1, err)
		}
	}
	if err := attachExtra(t, config(t, ApplyAsync, 0), SyncApplyMaxExtraViews+1); err != nil {
		t.Fatalf("ApplyAsync limited to the sync cap: %v", err)
	}
	if err := attachExtra(t, config(t, ApplyAsync, DefaultMaxViewsPerPlayer), 16); err != nil {
		t.Fatalf("ApplyAsync with %d views per player: %v", DefaultMaxViewsPerPlayer, err)
	}
}

// 两种模式“结果何时可见”不同：同步模式下提交之后的下一次 Flush 就应用并结算（不启动应用线程）；异步模式下 Flush 只结算
// 应用线程已经应用好的事实（应用线程没走到时 Flush 之后还看不到），要看到全部结果用 Drain。
func TestApplyModeDecidesWhenACommittedMoveIsVisible(t *testing.T) {
	setup := func(t *testing.T, mode ApplyMode) (*entitysync.Manager, *Interest, *queuedEntity) {
		m := newManager(t)
		in := newInterestMode(t, m, mode)
		player(t, m, 1)
		player(t, m, 2)
		mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
		mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
		mustApply(t, in)
		if err := m.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		return m, in, factEntity(t, 2)
	}
	commitMove := func(t *testing.T, m *entitysync.Manager, in *Interest, e *queuedEntity) {
		mutation := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
		if err := in.QueueMove(e, spatial.Point{X: 20, Y: 10}); err != nil {
			t.Fatal(err)
		}
		mutation.Admit()
		mutation.Confirm()
		mutation.Release()
	}
	sees := func(m *entitysync.Manager) bool { return slices.Contains(m.Subscribers(2), entitysync.SessionID(1)) }
	t.Run("sync/flush", func(t *testing.T) {
		m, in, e := setup(t, ApplySync)
		commitMove(t, m, in, e)
		if err := m.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !sees(m) || !slices.Contains(in.Visible(1), int64(2)) {
			t.Fatalf("ApplySync: the Flush after the commit did not apply the move: subscribers=%v visible=%v", m.Subscribers(2), in.Visible(1))
		}
		in.registry.mu.Lock()
		started := in.applierStarted
		in.registry.mu.Unlock()
		if started {
			t.Fatal("ApplySync started an applier thread")
		}
	})
	t.Run("async/flush", func(t *testing.T) {
		m, in, e := setup(t, ApplyAsync)
		manualApplier(in) // 应用线程不在后台抢先，“Flush 之后还看不到”是确定的
		commitMove(t, m, in, e)
		if err := m.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if sees(m) {
			t.Fatal("ApplyAsync: Flush applied a queued move on its own path")
		}
		stepApplier(t, in)
		if err := m.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !sees(m) {
			t.Fatal("ApplyAsync: an applied move was not settled by the next Flush")
		}
	})
	t.Run("async/drain", func(t *testing.T) {
		m, in, e := setup(t, ApplyAsync)
		commitMove(t, m, in, e)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if !sees(m) {
			t.Fatal("ApplyAsync: Drain returned before the committed move was applied and settled")
		}
	})
}
