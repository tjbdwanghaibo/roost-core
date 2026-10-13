// 写准入：冷准入跟随调用方取消、批次共用一个有界准入期限、权威锁版本与租约过期重试、写额度独立且不阻塞、操作预算含排队。

package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

type contextOwnershipStore struct {
	*mockMarkerStore
	mu        sync.Mutex
	deadlines []time.Time
	entered   chan context.Context
	release   chan struct{}
}

func (s *contextOwnershipStore) GetOwnership(ctx context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	deadline, _ := ctx.Deadline()
	s.mu.Lock()
	s.deadlines = append(s.deadlines, deadline)
	s.mu.Unlock()
	if s.entered != nil {
		s.entered <- ctx
		select {
		case <-ctx.Done():
			return entity.RemoteEntityMarkerLease{}, false, ctx.Err()
		case <-s.release:
			return entity.RemoteEntityMarkerLease{}, false, errors.New("test released")
		}
	}
	return s.mockMarkerStore.GetOwnership(ctx, id)
}

var registerAdmissionKind = sync.OnceFunc(func() {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: 236, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
})

func admissionManager(t *testing.T, store *contextOwnershipStore, budget time.Duration) (*Manager, []int64) {
	t.Helper()
	registerAdmissionKind()
	cfg := DefaultConfig()
	cfg.OpTimeout = budget
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	loader := newRemoteTestLoader()
	ids := make([]int64, 3)
	for i := range ids {
		live := newTestRemoteEntity(int64(7310+i), 1, 236)
		loader.add(live)
		ids[i] = live.GUId()
	}
	m.SetBackend(loader)
	m.SetOwnershipStore(store)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.StopFinalizer(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return m, ids
}
func TestColdRemoteAdmissionHonorsCallerCancellation(t *testing.T) {
	for _, ownership := range []bool{false, true} {
		name := "write"
		if ownership {
			name = "ownership"
		}
		t.Run(name, func(t *testing.T) {
			store := &contextOwnershipStore{mockMarkerStore: newMockMarkerStore(), entered: make(chan context.Context, 4), release: make(chan struct{})}
			defer close(store.release)
			m, ids := admissionManager(t, store, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if ownership {
					_, err := m.ClaimRemoteOwnership(ctx, ids[0])
					done <- err
				} else {
					_, err := m.PrepareRemoteWriteBatch(ctx, ids[:1])
					done <- err
				}
			}()
			observed := awaitChan(t, store.entered, "initial ownership read")
			cancel()
			select {
			case <-observed.Done():
			case <-time.After(150 * time.Millisecond):
				t.Fatal("cold ownership read detached from caller cancellation")
			}
			if err := awaitChan(t, done, "canceled admission"); !errors.Is(err, context.Canceled) {
				t.Fatalf("admission=%v", err)
			}
		})
	}
}
func TestRemoteBatchSharesOneBoundedAdmissionDeadline(t *testing.T) {
	store := &contextOwnershipStore{mockMarkerStore: newMockMarkerStore()}
	m, ids := admissionManager(t, store, 100*time.Millisecond)
	parent, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	started := time.Now()
	batch, err := m.PrepareRemoteWriteBatch(parent, ids)
	if err != nil {
		t.Fatal(err)
	}
	_ = batch.Abort(context.Background(), errors.New("test complete"))
	if err := batch.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.deadlines) != len(ids) {
		t.Fatalf("authority reads=%d want=%d (cold construction must not perform I/O)", len(store.deadlines), len(ids))
	}
	first := store.deadlines[0]
	if first.After(started.Add(150 * time.Millisecond)) {
		t.Fatalf("caller deadline bypassed operation budget: %v", first.Sub(started))
	}
	for _, deadline := range store.deadlines[1:] {
		if !deadline.Equal(first) {
			t.Fatal("each entity received a fresh operation budget")
		}
	}
}

type grantTestAuthority struct {
	WriteAuthority
	grant func() (WriteGrant, error)
}

func (a grantTestAuthority) GrantWrite(context.Context, int64, string, int32) (WriteGrant, error) {
	return a.grant()
}

type authorityLockRedis struct {
	*unlockEvalStub
	loseBeforeRefresh bool
}

func (r *authorityLockRedis) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	if script != versionedRefreshLua && script != versionedAbandonLua {
		return r.unlockEvalStub.Eval(ctx, script, keys, args...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if script == versionedRefreshLua && r.loseBeforeRefresh {
		r.hash["owner"] = "replacement"
	}
	if r.hash["owner"] != args[0].(string) {
		return int64(0), nil
	}
	if script == versionedAbandonLua {
		delete(r.hash, "owner")
	}
	return int64(1), nil
}
func TestAuthorityLockUsesDurableVersionAndCleansFailedAdmission(t *testing.T) {
	for _, mode := range []string{"success", "authority unavailable", "redis replaced"} {
		t.Run(mode, func(t *testing.T) {
			r := &authorityLockRedis{unlockEvalStub: newUnlockEvalStub(), loseBeforeRefresh: mode == "redis replaced"}
			r.hash["version"] = "1"
			unavailable := errors.New("authority unavailable")
			a := grantTestAuthority{grant: func() (WriteGrant, error) {
				if mode == "authority unavailable" {
					return WriteGrant{}, unavailable
				}
				return WriteGrant{Version: 8, Fence: 123}, nil
			}}
			lock := NewVersionedLockFactory(r, a).NewVersionedLock(101, fredis.VersionedLockOptions{TTL: time.Second}).(*versionedLock)
			err := lock.TryLock(context.Background())
			switch mode {
			case "success":
				if err != nil || lock.Version() != 8 || lock.Fence() != 123 || !lock.IsAcquired() {
					t.Fatalf("lock=%+v err=%v", lock, err)
				}
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			case "authority unavailable":
				if !errors.Is(err, unavailable) || lock.IsAcquired() || r.hash["owner"] != "" || r.hash["version"] != "1" {
					t.Fatalf("err=%v hash=%v", err, r.hash)
				}
			case "redis replaced":
				if !errors.Is(err, ErrVersionedLockExpired) || lock.IsAcquired() || r.hash["owner"] != "replacement" {
					t.Fatalf("err=%v hash=%v", err, r.hash)
				}
			}
		})
	}
}

func TestAuthorityLockRetriesKnownLeaseExpiryBeforeBusinessAdmission(t *testing.T) {
	r := &authorityLockRedis{unlockEvalStub: newUnlockEvalStub()}
	grants := 0
	a := grantTestAuthority{grant: func() (WriteGrant, error) {
		grants++
		if grants == 1 {
			r.mu.Lock()
			delete(r.hash, "owner")
			r.mu.Unlock()
		}
		return WriteGrant{Version: 8, Fence: uint64(grants)}, nil
	}}
	lock := NewVersionedLockFactory(r, a).NewVersionedLock(101, fredis.VersionedLockOptions{TTL: time.Second, RetryCount: 1, RetryInterval: time.Millisecond}).(*versionedLock)
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if grants != 2 || lock.Fence() != 2 || !lock.IsAcquired() {
		t.Fatalf("grants=%d fence=%d held=%v", grants, lock.Fence(), lock.IsAcquired())
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteWriteBudgetIsIndependentAndNonBlocking(t *testing.T) {
	registerAdmissionKind()
	cfg := DefaultConfig()
	cfg.MaxConcurrentWrites = 8
	cfg.AsyncFinalizeCapacity = 64
	m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	loader := newRemoteTestLoader()
	ids := make([]int64, 64)
	for i := range ids {
		e := newTestRemoteEntity(int64(8200+i), 1, 236)
		loader.add(e)
		ids[i] = e.GUId()
	}
	m.SetBackend(loader)
	m.SetOwnershipStore(newMockMarkerStore())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.StopFinalizer(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		batch entity.RemoteWriteBatch
		err   error
	}
	results := make(chan result, len(ids))
	var group sync.WaitGroup
	for _, id := range ids {
		group.Go(func() { batch, err := m.PrepareRemoteWriteBatch(ctx, []int64{id}); results <- result{batch, err} })
	}
	group.Wait()
	close(results)
	var accepted []entity.RemoteWriteBatch
	for result := range results {
		if result.err == nil {
			accepted = append(accepted, result.batch)
		} else if !errors.Is(result.err, entity.ErrRemoteOverloaded) {
			t.Errorf("unexpected refusal: %v", result.err)
		}
	}
	stats := m.Stats()
	if len(accepted) != 8 || stats.WritesInFlight != 8 || stats.WriteRejected != 56 || stats.WriteLimit != 8 || stats.Wrappers != 8 {
		t.Errorf("accepted=%d stats=%+v", len(accepted), stats)
	}
	for _, batch := range accepted {
		if err := batch.Abort(ctx, errors.New("test")); err != nil {
			t.Error(err)
		}
		if err := batch.Close(ctx); err != nil {
			t.Error(err)
		}
		_ = batch.Close(ctx)
	}
	if m.Stats().WritesInFlight != 0 {
		t.Fatal("Close leaked write capacity")
	}
	batch, err := m.PrepareRemoteWriteBatch(ctx, ids[:1])
	if err != nil {
		t.Fatal(err)
	}
	_ = batch.Abort(ctx, errors.New("test"))
	_ = batch.Close(ctx)
	// 失败发生于资源预算之后，仍必须归还额度。
	missing := testRemoteFullIDWithKind(8999, 1, 236)
	if _, err := m.PrepareRemoteWriteBatch(ctx, []int64{missing}); err == nil {
		t.Fatal("missing entity unexpectedly prepared")
	}
	if m.Stats().WritesInFlight != 0 {
		t.Fatal("failed Prepare leaked write capacity")
	}
}

func TestRemoteWriteBudgetConfigCompatibility(t *testing.T) {
	for _, tc := range []struct{ limit, finalize, want int }{{0, 17, 17}, {8, 17, 8}, {32, 17, 17}, {0, 0, 4096}} {
		cfg := DefaultConfig()
		cfg.MaxConcurrentWrites = tc.limit
		cfg.AsyncFinalizeCapacity = tc.finalize
		m := NewManager(newMockVersionedLockFactory(), cfg, 1000)
		if got := m.Stats().WriteLimit; got != tc.want {
			t.Fatalf("config=%+v effective=%d", tc, got)
		}
	}
}

// OpTimeout 是一次远端写的总预算，在每实体写闸上排队也算在里面（与 ownership.go 的加锁路径同一判据：先设 deadline 再排队）；
// 同一实体上排队的 N 个写者各自在预算内返回。
// 历史：RR-20260920-08。
func TestAQueuedWriterIsBoundedByTheOperationBudget(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OpTimeout = 150 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())
	wrapper := mgr.getOrCreate(testRemoteFullIDWithKind(4242, 1, 1), 1, 1)
	if wrapper == nil {
		t.Fatal("no wrapper")
	}

	// Somebody else holds the entity's write gate and is not letting go: a
	// slow commit, a distributed lock still being acquired, anything.
	wrapper.writeGate <- struct{}{}
	defer func() { <-wrapper.writeGate }()

	// The caller has no deadline of its own, which is the case the budget is
	// supposed to cover. Run it off the test goroutine: without the budget
	// this call never returns, and a hanging test says less than a failing
	// one.
	type result struct {
		err    error
		waited time.Duration
	}
	done := make(chan result, 1)
	go func() {
		started := time.Now()
		_, err := wrapper.beginWrite(context.Background())
		done <- result{err: err, waited: time.Since(started)}
	}()
	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("a writer got in while the gate was held")
		}
		if got.waited > 4*cfg.OpTimeout {
			t.Fatalf("a queued writer waited %s with an operation budget of %s; the budget does not cover the wait", got.waited, cfg.OpTimeout)
		}
		if !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("a writer that ran out of budget reported %v, want a deadline", got.err)
		}
	case <-time.After(4 * cfg.OpTimeout):
		t.Fatalf("a queued writer did not return within %s of an operation budget of %s; nothing bounds the wait", 4*cfg.OpTimeout, cfg.OpTimeout)
	}
}

// 排队本身不能把总耗时做成 N 倍：N 个写者各自都要在预算内回来。
func TestConcurrentWritersEachReturnWithinTheBudget(t *testing.T) {
	const writers = 8
	cfg := DefaultConfig()
	cfg.OpTimeout = 150 * time.Millisecond
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())
	wrapper := mgr.getOrCreate(testRemoteFullIDWithKind(4343, 1, 1), 1, 1)
	if wrapper == nil {
		t.Fatal("no wrapper")
	}
	wrapper.writeGate <- struct{}{}
	defer func() { <-wrapper.writeGate }()

	var wg sync.WaitGroup
	worst := make([]time.Duration, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			started := time.Now()
			_, _ = wrapper.beginWrite(context.Background())
			worst[slot] = time.Since(started)
		}(i)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(6 * cfg.OpTimeout):
		t.Fatalf("%d queued writers did not all return within %s of a %s budget", writers, 6*cfg.OpTimeout, cfg.OpTimeout)
	}
	for slot, waited := range worst {
		if waited > 4*cfg.OpTimeout {
			t.Fatalf("writer %d waited %s with a budget of %s; the wait grows with the queue", slot, waited, cfg.OpTimeout)
		}
	}
}

// 调用方自带的更短 deadline 仍然优先——预算是上界，不是下界。
func TestACallerDeadlineShorterThanTheBudgetStillWins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OpTimeout = 2 * time.Second
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())
	wrapper := mgr.getOrCreate(testRemoteFullIDWithKind(4444, 1, 1), 1, 1)
	if wrapper == nil {
		t.Fatal("no wrapper")
	}
	wrapper.writeGate <- struct{}{}
	defer func() { <-wrapper.writeGate }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := wrapper.beginWrite(ctx); err == nil {
		t.Fatal("a writer got in while the gate was held")
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("the caller's 100ms deadline was ignored: waited %s", waited)
	}
}
