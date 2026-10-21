// Interest 政策：块订阅与覆盖预算、排队事实的提交 / 回滚 / 拒绝、事实准入不等政策锁。

package policy

import (
	"context"
	"errors"
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
func flushInterest(t *testing.T, m *entitysync.Manager) {
	t.Helper()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestObservedBlocksKeepOnePositionAndSubscription(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m)
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
	flushInterest(t, m)
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
	flushInterest(t, m)
	if !subscribed(m, 1, 2) {
		t.Fatal("one remaining block was ignored")
	}
	setRelation(t, in, 1, 2)
	mustApply(t, in)
	if err := in.QueueObservedBlocks(e, nil); err != nil {
		t.Fatal(err)
	}
	flushInterest(t, m)
	if !subscribed(m, 1, 2) {
		t.Fatal("block removal dropped team subscription")
	}
	setRelation(t, in, 1)
	mustApply(t, in)
	if subscribed(m, 1, 2) {
		t.Fatal("last source not removed")
	}
}

func TestSpatialAndBlockHandoverDoesNotRecreateBaseline(t *testing.T) {
	transport := &sink{}
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	in := newInterest(t, m)
	player(t, m, 1)
	player(t, m, 2)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustEnter(t, in, 2, spatial.Point{X: 20, Y: 10})
	mustApply(t, in)
	flushInterest(t, m)
	before := transport.count(1)
	e := factEntity(t, 2)
	if err := in.QueueSpatial(e, spatial.Point{X: 900, Y: 900}, []int64{in.BlockAt(spatial.Point{X: 10, Y: 10})}); err != nil {
		t.Fatal(err)
	}
	flushInterest(t, m)
	if !subscribed(m, 1, 2) || transport.count(1) != before {
		t.Fatal("source handover recreated or removed baseline")
	}
}

func TestCoverageCommitRollbackRejectAndLifetime(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m)
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
		flushInterest(t, m)
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
		flushInterest(t, m)
		if len(in.ObservedBlocks(2)) != 0 {
			t.Fatal("coverage escaped before release")
		}
		mutation.Release()
		flushInterest(t, m)
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
	flushInterest(t, m)
	if subscribed(m, 1, 2) || len(in.ObservedBlocks(2)) != 0 {
		t.Fatal("old coverage resurrected after Hide/Show")
	}
}

func TestCoverageLateObserverBudgetAndVisibilityLimit(t *testing.T) {
	m := newManager(t)
	cfg := InterestConfig{Manager: m, AOI: interestConfig()}
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
	flushInterest(t, m)
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
}

func TestFactAdmissionDoesNotWaitForPolicyLock(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m)
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
}

func TestInflightFactsKeepBudgetAndPendingHeadDoesNotStarve(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m)
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
	m := newManager(t)
	in := newInterest(t, m)
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
}

func TestPublicationHoldsReleaseOnDiscardAndClose(t *testing.T) {
	for _, action := range []string{"rollback", "hide", "leave", "close"} {
		t.Run(action, func(t *testing.T) {
			m := newManager(t)
			in := newInterest(t, m)
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
			case "leave":
				mutation.Admit()
				mutation.Confirm()
				mutation.Release()
				if err := in.Leave(1, 1); err != nil {
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
}

func TestBatchLimitRetainsLaterEntityPublication(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m)
	in.batchSize = 1
	var entities []*queuedEntity
	for id := int64(1); id <= 2; id++ {
		mustEnter(t, in, id, spatial.Point{X: 10 * id, Y: 10})
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
}

type queuedEntity struct{ *entity.EntityBase }

func (e *queuedEntity) Base() *entity.EntityBase { return e.EntityBase }

func TestQueuedInterestFactsFollowCommitAndRollback(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m)
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
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(in.Visible(1)) != 1 {
		t.Fatal("unconfirmed movement applied")
	}
	first.Confirm()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(in.Visible(1)) != 0 {
		t.Fatal("confirmed movement not applied")
	}
	second := entity.BeginSyncMutation(es, m)
	if err := in.QueueMove(e, spatial.Point{X: 20, Y: 10}); err != nil {
		t.Fatal(err)
	}
	second.Finish(false)
	second.Release()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(in.Visible(1)) != 0 || len(in.facts) != 0 {
		t.Fatal("rolled back facts escaped or leaked")
	}
	if subscribed(m, 2, 1) {
		t.Fatal("rolled back movement escaped")
	}
}

func TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen(t *testing.T) {
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	in := newInterest(t, m)
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
	ctx := context.Background()
	t.Run("leave", func(t *testing.T) {
		frames := &sink{}
		m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: frames})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close(context.Background()) })
		in := newInterest(t, m)
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
		if err := m.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if frames.count(1) == before {
			t.Fatal("session 1 received nothing after the invalidated fact: the manager stopped emitting")
		}
		// 同一 id 重新进入：被 Leave 作废的旧位置不能套到它身上。
		mustEnter(t, in, 2, spatial.Point{X: 30, Y: 10})
		if err := m.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		if !subscribed(m, 1, 2) {
			t.Fatal("stale queued move applied to the re-entered id")
		}
	})

	t.Run("hide", func(t *testing.T) {
		m := newManager(t)
		in := newInterest(t, m)
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
}

// 结构性守卫：将来若又出现绕过 Leave / Hide 改写的移除路径，应用失败的事实报告一次后丢弃，不再让之后每一次 Flush 都失败。
func TestQueuedFactThatCannotApplyFailsOneFlushOnly(t *testing.T) {
	m := newManager(t)
	in := newInterest(t, m)
	player(t, m, 1)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustApply(t, in)
	in.mu.Lock()
	in.queueActive = true
	in.facts = append(in.facts, queuedInterestFact{id: 99, at: spatial.Point{X: 5, Y: 5}, condition: func() (bool, bool) { return true, false }})
	in.mu.Unlock()
	if err := m.Flush(context.Background()); err == nil {
		t.Fatal("unappliable fact not reported")
	}
	for round := 0; round < 2; round++ {
		if err := m.Flush(context.Background()); err != nil {
			t.Fatalf("flush %d still failing on a dropped fact: %v", round, err)
		}
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
	m := newManager(t)
	in := newInterest(t, m)
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
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	m := newManager(t)
	in := newInterest(t, m)
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
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !remote.Sync().SyncCommitReady() {
		t.Fatal("rejected commit still blocks the gate")
	}
	if visible := in.Visible(1); !slices.Contains(visible, int64(3)) {
		t.Fatalf("rejected Remote move was applied: observer 1 sees %v", visible)
	}
}

// D3：Drain 超时时，Interest 报出仍让它等待的条目——未确认提交的排队事实、尚未判定停滞的被拒订阅（带最后一次错误）。
func TestDrainTimeoutListsInterestStalls(t *testing.T) {
	// 周期远长于 ctx：每个被拒 pair 只被重说一次，仍计入 queuedPending。
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: &sink{}, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	in := newInterest(t, m)
	player(t, m, 1)
	player(t, m, 2)
	player(t, m, 3)
	mustEnter(t, in, 1, spatial.Point{X: 10, Y: 10})
	mustEnter(t, in, 2, spatial.Point{X: 900, Y: 900})
	mustEnter(t, in, 3, spatial.Point{X: 500, Y: 500})
	mustApply(t, in)
	m.CloseSession(2)

	// 玩家 2 的会话已关闭：挂给它的关系视野应用后订阅被拒。
	setRelation(t, in, 2, 1)

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
}
