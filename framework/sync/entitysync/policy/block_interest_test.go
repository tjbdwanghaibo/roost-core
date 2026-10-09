package policy

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
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
	in := newInterest(t, m, "team")
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
	in.Relation("team").Set(1, []int64{2})
	mustApply(t, in)
	if err := in.QueueObservedBlocks(e, nil); err != nil {
		t.Fatal(err)
	}
	flushInterest(t, m)
	if !subscribed(m, 1, 2) {
		t.Fatal("block removal dropped team subscription")
	}
	in.Relation("team").Clear(1)
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
	if err := in.QueueSpatial(e, spatial.Point{X: 900, Y: 900}, true, []int64{in.BlockAt(spatial.Point{X: 10, Y: 10})}); err != nil {
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
		if err := in.QueueSpatial(e, spatial.Point{X: 800, Y: 800}, true, near); err != nil {
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
	go func() { done <- in.QueueMove(e, spatial.Point{X: 20, Y: 10}, true) }()
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
	in.profile = func(int) entity.SyncProfile {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return entity.SyncProfile{}
	}
	if err := in.QueueMove(factEntity(t, 1), spatial.Point{X: 30, Y: 10}, true); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- in.applyQueued() }()
	<-entered
	err := in.QueueMove(factEntity(t, 2), spatial.Point{X: 40, Y: 10}, true)
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
	if err := in.QueueMove(e, spatial.Point{X: 800, Y: 800}, true); err != nil {
		t.Fatal(err)
	}
	if err := in.QueueMove(factEntity(t, 2), spatial.Point{X: 50, Y: 10}, true); err != nil {
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
		if err := a.AddObserver(id, observers[id]); err != nil {
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
	if err := in.QueueMove(e, spatial.Point{X: 900, Y: 900}, true); err != nil {
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
				if err := in.Leave(1); err != nil {
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
		if err := in.QueueMove(e, spatial.Point{X: 800, Y: 800}, true); err != nil {
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
