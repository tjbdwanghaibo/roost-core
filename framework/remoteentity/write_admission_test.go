// 写准入：冷准入跟随调用方取消、批次共用一个有界准入期限、权威锁版本与租约过期重试、写额度独立且不阻塞、操作预算含排队；
// 多个进程写同一实体时本地副本落后于权威（或停在 Fenced）先卸载重载、outbox 代发别的进程的提交不推进本地副本。

package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
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

func (a grantTestAuthority) GrantWrite(context.Context, int64, string) (WriteGrant, error) {
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

// sharedWriterNode 是共享模式下的一个写者进程：自己的 EntityManager + 正式 ManagerAccess（内存命中直接返回已加载实例，
// 未命中经 authorityLoader 从权威构造）+ 自己的 Remote Manager；Mongo 权威与 Redis 版本锁和另一个进程共用。
type sharedWriterNode struct {
	sid     int32
	mgr     *Manager
	manager *entity.EntityManager
	access  *entity.ManagerAccess
	loader  *authorityLoader
}

// newSharedWriterNodes 建两个共用同一权威（Mongo 替身 + Redis 版本锁替身）的进程，与 reloadFixture 同一 kind。
// wrap 为 nil 时直接用 MongoCommitter 作存储；否则由它给指定进程包一层存储替身。
func newSharedWriterNodes(t *testing.T, rawID int64, wrap func(int32, *MongoCommitter) StorageBackend, sids ...int32) []sharedWriterNode {
	t.Helper()
	return newWriterNodesOn(t, newRemoteMongoFake(), rawID, wrap, sids...)
}

// newWriterNodesOn 与 newSharedWriterNodes 相同，但各进程共用调用方给的 Mongo（例如计数包装）。sids 可以重复：同一个 sid
// 的两个进程即“同 sid 崩溃重启的短暂重叠”，用来模拟单实例前提被打破。
func newWriterNodesOn(t *testing.T, db fmongo.IMongo, rawID int64, wrap func(int32, *MongoCommitter) StorageBackend, sids ...int32) []sharedWriterNode {
	t.Helper()
	const kind entity.EntityKind = 125
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	redis := &authorityLockRedis{unlockEvalStub: newUnlockEvalStub()}
	nodes := make([]sharedWriterNode, 0, len(sids))
	for _, sid := range sids {
		store := newBoundMongoCommitter(db, "control", sid, 0)
		manager := entity.NewEntityManager()
		access := entity.NewManagerAccess(manager)
		loader := &authorityLoader{store: store, manager: manager, kind: kind, rawID: rawID}
		if _, err := access.ConfigureLoader(loader); err != nil {
			t.Fatal(err)
		}
		var storage StorageBackend = store
		if wrap != nil {
			storage = wrap(sid, store)
		}
		backend, err := NewBackend(access, storage)
		if err != nil {
			t.Fatal(err)
		}
		cfg := DefaultConfig()
		cfg.FinalizeRetryInterval = 10 * time.Millisecond
		// 所有权在两个进程之间来回转，热 marker 缓存会让刚被转回的一方在 TTL 内仍按旧所有者拒绝；每次准入都读权威。
		cfg.MarkerCacheTTL = time.Nanosecond
		mgr := NewManager(NewVersionedLockFactory(redis, store), cfg, sid)
		mgr.SetBackend(backend)
		mgr.SetOwnershipStore(store)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = mgr.StopFinalizer(ctx)
		})
		nodes = append(nodes, sharedWriterNode{sid: sid, mgr: mgr, manager: manager, access: access, loader: loader})
	}
	return nodes
}

// write 按 Nest 的顺序写一笔 Durability 0 事务：先准入 Remote 写批次，handler 再按 ID 取本进程当前托管的实例修改
// （a / b 为空表示不改），定稿、提交、关闭。返回准入时的租约版本与错误。
func (n sharedWriterNode) write(t *testing.T, ctx context.Context, id int64, a, b string, tx byte) (uint64, error) {
	t.Helper()
	batch, err := n.mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		return 0, err
	}
	entry := batch.(*remoteWriteBatch).entries[0]
	live, _ := n.manager.Get(id).(*reloadableFullDocEntity)
	if live == nil || entry.entity != entity.IThreadSafeRemoteEntity(live) {
		_ = batch.Abort(ctx, errors.New("probe"))
		_ = batch.Close(ctx)
		t.Fatalf("sid %d: the batch admitted instance %p but the handler would modify the managed instance %p", n.sid, entry.entity, live)
	}
	live.set(a, b)
	if err := batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(tx), "shared_write", "", true, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := batch.Commit(ctx); err != nil {
		_ = batch.Close(ctx)
		return entry.lease.BaseVersion, err
	}
	return entry.lease.BaseVersion, batch.Close(ctx)
}

// 共享模式两个进程交替写同一实体：A 写、B 写两次、A 再写。A 的内存副本停在自己最后一次写入的版本，权威已被 B 推进；
// A 的写准入必须换掉落后的副本、从权威重新加载后基于最新版本写入，此后连续写都成功，内容不丢 B 的修改。
// 修复前正式 loader（ManagerAccess.LoadRemoteEntity = Get，内存命中返回同一旧实例）让“落后就重载”成为空操作：
// A 的每一笔写都报 loaded version 1, authority 3（无缝大世界原型端到端发现）。
func TestSharedModeWriterReloadsALocalCopyLeftBehindByAnotherProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes := newSharedWriterNodes(t, 2301, nil, 1000, 1004)
	a, b := nodes[0], nodes[1]
	id := testRemoteFullIDWithKind(2301, 1, 125)
	if _, err := a.mgr.ClaimRemoteOwnership(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.mgr.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.write(t, ctx, id, "A1", "", 0x01); err != nil {
		t.Fatalf("A first write: %v", err)
	}
	stale, _ := a.manager.Get(id).(*reloadableFullDocEntity)
	if _, err := b.write(t, ctx, id, "", "B1", 0x02); err != nil {
		t.Fatalf("B first write: %v", err)
	}
	if _, err := b.write(t, ctx, id, "", "B2", 0x03); err != nil {
		t.Fatalf("B second write: %v", err)
	}
	base, err := a.write(t, ctx, id, "A2", "", 0x04)
	if err != nil {
		t.Fatalf("A write after B advanced the authority: %v", err)
	}
	if base != 3 {
		t.Fatalf("A wrote on base version %d, want the authority's latest 3", base)
	}
	if doc := a.loaderDocument(t, id); doc != "a=A2;b=B2" {
		t.Fatalf("Mongo document=%q, want A's write on top of B's latest %q", doc, "a=A2;b=B2")
	}
	if stale == nil || !stale.IsRemoved() {
		t.Fatal("the stale instance is still managed after A wrote on a reloaded copy")
	}
	for i, tx := range []byte{0x05, 0x06} {
		base, err := a.write(t, ctx, id, fmt.Sprintf("A%d", i+3), "", tx)
		if err != nil || base != uint64(4+i) {
			t.Fatalf("A follow-up write %d: base=%d err=%v, want base %d", i, base, err, 4+i)
		}
	}
	if base, err := b.write(t, ctx, id, "", "B3", 0x07); err != nil || base != 6 {
		t.Fatalf("B write after A caught up: base=%d err=%v, want base 6", base, err)
	}
	if doc := a.loaderDocument(t, id); doc != "a=A4;b=B3" {
		t.Fatalf("Mongo document=%q, want %q", doc, "a=A4;b=B3")
	}
}

func (n sharedWriterNode) loaderDocument(t *testing.T, id int64) string {
	t.Helper()
	doc, _ := newCollectionLookup(n.loader.store, fullDocCollection, id)
	return doc
}

// heldPublishStorage 让本进程的 outbox 发布在标记 Committed 之前停住：事务已持久（Applied）、还没发布，
// 另一个进程的发布协调者扫描共享 outbox 时会先拿到它。
type heldPublishStorage struct {
	*MongoCommitter
	entered chan struct{}
	release chan struct{}
}

func (s *heldPublishStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

// 每个进程的 outbox 协调者扫描的是共享 outbox 里全部未发布事务，包括别的进程写的。A 代发 B 的事务时，确认不能落到
// A 的内存实例上：那份实例没有 B 的修改，把版本推进到 B 的 NextVersion 会让它冒充最新，A 下一笔整文档写就覆盖掉 B
// 的修改（丢更新）。A 的副本必须保持落后，由写准入从权威重载。
func TestPublishingAnotherProcessesCommitDoesNotAdvanceTheLocalCopy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	held := &heldPublishStorage{entered: make(chan struct{}, 1), release: make(chan struct{})}
	nodes := newSharedWriterNodes(t, 2302, func(sid int32, store *MongoCommitter) StorageBackend {
		if sid != 1004 {
			return store
		}
		held.MongoCommitter = store
		return held
	}, 1000, 1004)
	a, b := nodes[0], nodes[1]
	id := testRemoteFullIDWithKind(2302, 1, 125)
	if _, err := a.mgr.ClaimRemoteOwnership(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.mgr.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.write(t, ctx, id, "A1", "", 0x11); err != nil {
		t.Fatalf("A first write: %v", err)
	}
	stale, _ := a.manager.Get(id).(*reloadableFullDocEntity)
	bDone := make(chan error, 1)
	go func() {
		_, err := b.write(t, ctx, id, "", "B1", 0x12)
		bDone <- err
	}()
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("B's commit never reached its outbox publish")
	}
	// B 的事务已 Applied、未发布：A 的协调者代发它。
	if err := a.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatalf("A publishing the shared outbox: %v", err)
	}
	if got := stale.EntityVersion(); got != 1 {
		t.Fatalf("A's local copy (still holding only A's write) claims version %d after A published B's commit, want it left at 1", got)
	}
	close(held.release)
	if err := <-bDone; err != nil {
		t.Fatalf("B write: %v", err)
	}
	if _, err := a.write(t, ctx, id, "A2", "", 0x13); err != nil {
		t.Fatalf("A write after B: %v", err)
	}
	if doc := a.loaderDocument(t, id); doc != "a=A2;b=B1" {
		t.Fatalf("Mongo document=%q, want A's write on top of B's %q (B's update was lost)", doc, "a=A2;b=B1")
	}
}

// 本地副本落后不只发生在共享模式：A 退出共享回到独占、或所有权转给 B 再转回 A 之后，A 内存里仍是 B 写之前的副本，
// 独占（owner-routed）写准入拿到的持久许可版本更新，同样要从权威重载后再写。
func TestOwnerRoutedWriterReloadsALocalCopyLeftBehindWhileItWasNotTheOnlyWriter(t *testing.T) {
	for i, path := range []string{"leave shared mode", "ownership transferred away and back"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rawID := int64(2303 + i)
			nodes := newSharedWriterNodes(t, rawID, nil, 1000, 1004)
			a, b := nodes[0], nodes[1]
			id := testRemoteFullIDWithKind(rawID, 1, 125)
			if _, err := a.mgr.ClaimRemoteOwnership(ctx, id); err != nil {
				t.Fatal(err)
			}
			if path == "leave shared mode" {
				if _, err := a.mgr.EnterRemoteSharedMode(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.write(t, ctx, id, "A1", "", 0x21); err != nil {
				t.Fatalf("A first write: %v", err)
			}
			if path != "leave shared mode" {
				if _, err := a.mgr.TransferRemoteOwnership(ctx, id, b.sid); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.write(t, ctx, id, "", "B1", 0x22); err != nil {
				t.Fatalf("B write: %v", err)
			}
			if path == "leave shared mode" {
				if _, err := a.mgr.LeaveRemoteSharedMode(ctx, id); err != nil {
					t.Fatal(err)
				}
			} else if _, err := b.mgr.TransferRemoteOwnership(ctx, id, a.sid); err != nil {
				t.Fatal(err)
			}
			base, err := a.write(t, ctx, id, "A2", "", 0x23)
			if err != nil || base != 2 {
				t.Fatalf("A owner-routed write after B wrote: base=%d err=%v, want base 2", base, err)
			}
			if doc := a.loaderDocument(t, id); doc != "a=A2;b=B1" {
				t.Fatalf("Mongo document=%q, want %q", doc, "a=A2;b=B1")
			}
			if base, err := a.write(t, ctx, id, "A3", "", 0x24); err != nil || base != 3 {
				t.Fatalf("A follow-up write: base=%d err=%v, want base 3", base, err)
			}
		})
	}
}

// 所有权转给 B、B 没写又转回 A：A 的副本版本与权威一致，但它在转出时被置为 Fenced，留在内存里不会自己恢复。
// 权威已把写许可交回 A，这份副本内容就是权威最后一次提交，写准入要换成从权威重载的实例，而不是永久按 fenced 拒绝。
func TestOwnershipTransferredBackWithoutWritesLeavesTheOwnerWritable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes := newSharedWriterNodes(t, 2305, nil, 1000, 1004)
	a, b := nodes[0], nodes[1]
	id := testRemoteFullIDWithKind(2305, 1, 125)
	if _, err := a.mgr.ClaimRemoteOwnership(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.write(t, ctx, id, "A1", "", 0x31); err != nil {
		t.Fatalf("A first write: %v", err)
	}
	if _, err := a.mgr.TransferRemoteOwnership(ctx, id, b.sid); err != nil {
		t.Fatal(err)
	}
	if _, err := b.mgr.TransferRemoteOwnership(ctx, id, a.sid); err != nil {
		t.Fatal(err)
	}
	if base, err := a.write(t, ctx, id, "A2", "", 0x32); err != nil || base != 1 {
		t.Fatalf("A write after the ownership came back: base=%d err=%v, want base 1", base, err)
	}
	if doc := a.loaderDocument(t, id); doc != "a=A2;b=" {
		t.Fatalf("Mongo document=%q, want %q", doc, "a=A2;b=")
	}
}

// 事务外独立删档在 EntityManager.Destroy 持有实体锁时准备 Remote 批次（dataengine admitRemoteEntityDelete 带
// entity.WithRemoteWriteTargetHeld）。这时落后的实例不能卸载重载：同一 goroutine 可重入实体锁，卸载会在删档中途把
// 实例从 EntityManager 换掉。写准入按版本冲突拒绝，实例留在原处；之后不持锁的写准入照常换掉它。
func TestStaleWriteTargetHeldByTheCallerIsRefusedNotUnloaded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes := newSharedWriterNodes(t, 2306, nil, 1000, 1004)
	a, b := nodes[0], nodes[1]
	id := testRemoteFullIDWithKind(2306, 1, 125)
	if _, err := a.mgr.ClaimRemoteOwnership(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.mgr.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.write(t, ctx, id, "A1", "", 0x41); err != nil {
		t.Fatalf("A first write: %v", err)
	}
	if _, err := b.write(t, ctx, id, "", "B1", 0x42); err != nil {
		t.Fatalf("B write: %v", err)
	}
	stale := a.manager.Get(id).(*reloadableFullDocEntity)
	mu := stale.GetMutex()
	mu.Lock()
	batch, err := a.mgr.PrepareRemoteWriteBatch(entity.WithRemoteWriteTargetHeld(ctx), []int64{id})
	mu.Unlock()
	if err == nil {
		_ = batch.Abort(ctx, errors.New("probe"))
		_ = batch.Close(ctx)
		t.Fatal("a stale target whose lock the caller holds was admitted")
	}
	if !errors.Is(err, entity.ErrRemoteVersionConflict) {
		t.Fatalf("err=%v, want the version conflict", err)
	}
	if stale.IsRemoved() || a.manager.Get(id) != entity.IThreadSafeEntity(stale) {
		t.Fatal("the stale instance was unloaded while the caller held its lock")
	}
	if base, err := a.write(t, ctx, id, "A2", "", 0x43); err != nil || base != 2 {
		t.Fatalf("A write without the held lock: base=%d err=%v, want base 2", base, err)
	}
}

// 正式 Nest 链路：两个进程各有 Nest 引擎（I/O 阶段准入 Remote 写批次，NewEngine 把业务池续行绑定给 Remote Manager），
// handler 按声明目标拿实例写。A、B、B、A 交替写同一共享实体：A 的写准入在 I/O 阶段发现副本落后，卸载经业务池执行，
// 重载后 handler 拿到的是新实例，回复成功、权威里两边的修改都在。
func TestSharedModeWritersAlternateThroughNest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes := newSharedWriterNodes(t, 2307, nil, 1000, 1004)
	id := testRemoteFullIDWithKind(2307, 1, 125)
	if _, err := nodes[0].mgr.ClaimRemoteOwnership(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes[0].mgr.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	type change struct{ a, b string }
	name := nest.NewHandlerName("shared_mode_alternate_write")
	engines := make([]*nest.NestMgr, len(nodes))
	for i, node := range nodes {
		engine := nest.NewEngine(nest.NestOptionWithGetter(node.access), nest.NestOptionWithRemoteEntityManager(node.mgr),
			nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
		engine.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
			c := params[0].(change)
			es[0].(*reloadableFullDocEntity).set(c.a, c.b)
			return es[0].(*reloadableFullDocEntity).EntityVersion(), nil
		}, nest.HandlerMeta{Rollback: nest.RollbackNone, Durability: nest.DurabilityMemory})
		if err := engine.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
		engines[i] = engine
	}
	write := func(node int, c change) {
		t.Helper()
		if _, err := engines[node].Request(ctx, name, id, nest.NewParams(c)); err != nil {
			t.Fatalf("sid %d write %+v: %v", nodes[node].sid, c, err)
		}
	}
	write(0, change{a: "A1"})
	stale := nodes[0].manager.Get(id).(*reloadableFullDocEntity)
	write(1, change{b: "B1"})
	write(1, change{b: "B2"})
	write(0, change{a: "A2"})
	if !stale.IsRemoved() {
		t.Fatal("A wrote without replacing its stale local copy")
	}
	if !stale.destroyedOnFast.Load() {
		t.Fatal("the stale local copy was unloaded outside the Nest business pool")
	}
	write(0, change{a: "A3"})
	if doc := nodes[0].loaderDocument(t, id); doc != "a=A3;b=B2" {
		t.Fatalf("Mongo document=%q, want %q", doc, "a=A3;b=B2")
	}
	if got := nodes[0].manager.Get(id).(*reloadableFullDocEntity).EntityVersion(); got != 5 {
		t.Fatalf("A's local copy version=%d after five writes, want 5", got)
	}
}

// ---- 独占写准入：稳定持有期间不取写许可、只读请求不碰 Mongo（维护者 2026-11-06） ----
//
// 前提：一个实体同一时刻只有一个 owner（App 单实例与 fail-stop 保证）。独占实体只在加载、取得归属、退出共享这三个时刻
// 向权威对齐一次；稳定持有期间本进程是唯一写者，内存就是最新，读写都不再向 Mongo 取写许可。前提被打破时由投影的
// 版本 / 代际条件兜底拒绝后到的写。

// countingMongo 按“集合.方法”计数经过它的 Mongo 命令（含事务内的），断言每次请求发了哪些命令；order 按发出顺序
// 记下同样的键，用来断言某条命令之后还有没有别的命令。
type countingMongo struct {
	fmongo.IMongo
	mu    sync.Mutex
	calls map[string]int
	order []string
}

func newCountingMongo() *countingMongo {
	return &countingMongo{IMongo: newRemoteMongoFake(), calls: make(map[string]int)}
}

func (m *countingMongo) Database(name string) fmongo.IDatabase {
	return countingDatabase{IDatabase: m.IMongo.Database(name), m: m}
}

func (m *countingMongo) DatabaseForSid(prefix string, sid int32) fmongo.IDatabase {
	return countingDatabase{IDatabase: m.IMongo.DatabaseForSid(prefix, sid), m: m}
}

func (m *countingMongo) note(collection, method string) {
	m.mu.Lock()
	m.calls[collection+"."+method]++
	m.order = append(m.order, collection+"."+method)
	m.mu.Unlock()
}

// sequence 返回第 from 条之后按顺序发出的命令。
func (m *countingMongo) sequence(from int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order[from:]...)
}

func (m *countingMongo) sequenceLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.order)
}

func (m *countingMongo) snapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.calls))
	for k, v := range m.calls {
		out[k] = v
	}
	return out
}

// since 返回 before 之后新增的命令（只列非零项）。
func (m *countingMongo) since(before map[string]int) map[string]int {
	out := make(map[string]int)
	for k, v := range m.snapshot() {
		if d := v - before[k]; d != 0 {
			out[k] = d
		}
	}
	return out
}

// mongoWriteMethods 是会改数据的命令；读命令（FindOne / Find / CountDocuments / Aggregate）不在其中。
var mongoWriteMethods = map[string]bool{
	"InsertOne": true, "InsertMany": true, "UpdateOne": true, "UpdateMany": true, "ReplaceOne": true,
	"DeleteOne": true, "DeleteMany": true, "FindOneAndUpdate": true, "FindOneAndDelete": true,
	"FindOneAndReplace": true, "BulkWrite": true, "EnsureIndexes": true,
}

func mongoWrites(calls map[string]int) map[string]int {
	out := make(map[string]int)
	for k, v := range calls {
		_, method, _ := strings.Cut(k, ".")
		if mongoWriteMethods[method] {
			out[k] = v
		}
	}
	return out
}

type countingDatabase struct {
	fmongo.IDatabase
	m *countingMongo
}

func (d countingDatabase) Collection(name string) fmongo.ICollection {
	return countingCollection{ICollection: d.IDatabase.Collection(name), m: d.m, name: name}
}

type countingCollection struct {
	fmongo.ICollection
	m    *countingMongo
	name string
}

func (c countingCollection) InsertOne(ctx context.Context, doc any) (string, error) {
	c.m.note(c.name, "InsertOne")
	return c.ICollection.InsertOne(ctx, doc)
}
func (c countingCollection) InsertMany(ctx context.Context, docs []any) ([]string, error) {
	c.m.note(c.name, "InsertMany")
	return c.ICollection.InsertMany(ctx, docs)
}
func (c countingCollection) FindOne(ctx context.Context, filter any, result any) error {
	c.m.note(c.name, "FindOne")
	return c.ICollection.FindOne(ctx, filter, result)
}
func (c countingCollection) Find(ctx context.Context, filter any, results any, opts ...fmongo.FindOption) error {
	c.m.note(c.name, "Find")
	return c.ICollection.Find(ctx, filter, results, opts...)
}
func (c countingCollection) UpdateOne(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	c.m.note(c.name, "UpdateOne")
	return c.ICollection.UpdateOne(ctx, filter, update)
}
func (c countingCollection) UpdateMany(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	c.m.note(c.name, "UpdateMany")
	return c.ICollection.UpdateMany(ctx, filter, update)
}
func (c countingCollection) ReplaceOne(ctx context.Context, filter any, replacement any) (*fmongo.UpdateResult, error) {
	c.m.note(c.name, "ReplaceOne")
	return c.ICollection.ReplaceOne(ctx, filter, replacement)
}
func (c countingCollection) DeleteOne(ctx context.Context, filter any) (int64, error) {
	c.m.note(c.name, "DeleteOne")
	return c.ICollection.DeleteOne(ctx, filter)
}
func (c countingCollection) DeleteMany(ctx context.Context, filter any) (int64, error) {
	c.m.note(c.name, "DeleteMany")
	return c.ICollection.DeleteMany(ctx, filter)
}
func (c countingCollection) FindOneAndUpdate(ctx context.Context, filter any, update any, result any, opts ...fmongo.FindOneAndUpdateOption) error {
	c.m.note(c.name, "FindOneAndUpdate")
	return c.ICollection.FindOneAndUpdate(ctx, filter, update, result, opts...)
}
func (c countingCollection) FindOneAndDelete(ctx context.Context, filter any, result any) error {
	c.m.note(c.name, "FindOneAndDelete")
	return c.ICollection.FindOneAndDelete(ctx, filter, result)
}
func (c countingCollection) FindOneAndReplace(ctx context.Context, filter any, replacement any, result any) error {
	c.m.note(c.name, "FindOneAndReplace")
	return c.ICollection.FindOneAndReplace(ctx, filter, replacement, result)
}
func (c countingCollection) CountDocuments(ctx context.Context, filter any) (int64, error) {
	c.m.note(c.name, "CountDocuments")
	return c.ICollection.CountDocuments(ctx, filter)
}
func (c countingCollection) Aggregate(ctx context.Context, pipeline any, results any) error {
	c.m.note(c.name, "Aggregate")
	return c.ICollection.Aggregate(ctx, pipeline, results)
}
func (c countingCollection) BulkWrite(ctx context.Context, models []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	c.m.note(c.name, "BulkWrite")
	return c.ICollection.BulkWrite(ctx, models)
}
func (c countingCollection) EnsureIndexes(ctx context.Context, indexes []fmongo.IndexModel) error {
	c.m.note(c.name, "EnsureIndexes")
	return c.ICollection.EnsureIndexes(ctx, indexes)
}

// 稳定持有的独占实体：第一次写（认领 + 对齐）之后，每次写只有提交本身的一次元数据 CAS（BulkWrite），
// 不再有写许可（_remote_entity_meta 上的 findAndModify），也不再读所有权记录。
func TestExclusiveOwnerWritesWithoutAWriteGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := newCountingMongo()
	a := newWriterNodesOn(t, db, 2311, nil, 1000)[0]
	id := testRemoteFullIDWithKind(2311, 1, 125)
	if _, err := a.write(t, ctx, id, "A1", "", 0x51); err != nil {
		t.Fatalf("first write (claim and align): %v", err)
	}
	before := db.snapshot()
	for i := range 5 {
		base, err := a.write(t, ctx, id, fmt.Sprintf("A%d", i+2), "", byte(0x52+i))
		if err != nil || base != uint64(i+1) {
			t.Fatalf("write %d: base=%d err=%v, want base %d", i+2, base, err, i+1)
		}
	}
	got := db.since(before)
	if n := got[remoteMetaCollection+".FindOneAndUpdate"]; n != 0 {
		t.Fatalf("5 writes on a stably held exclusive entity took %d write grants (findAndModify on %s), want 0; commands=%v", n, remoteMetaCollection, got)
	}
	if n := got[remoteMetaCollection+".FindOne"]; n != 0 {
		t.Fatalf("5 writes on a stably held exclusive entity read the ownership record %d times, want 0; commands=%v", n, got)
	}
	if n := got[remoteMetaCollection+".BulkWrite"]; n != 5 {
		t.Fatalf("metadata CAS commands=%d, want one per write (5); commands=%v", n, got)
	}
	if doc := a.loaderDocument(t, id); doc != "a=A6;b=" {
		t.Fatalf("Mongo document=%q, want %q", doc, "a=A6;b=")
	}
}

// exclusiveNestNode 是带 Nest 引擎的写者进程：write 改 a / b；read 不改任何持久字段，答“内容@版本”。
type exclusiveNestNode struct {
	sharedWriterNode
	engine *nest.NestMgr
}

var (
	exclusiveWriteHandler = nest.NewHandlerName("exclusive_owner_write")
	exclusiveReadHandler  = nest.NewHandlerName("exclusive_owner_read")
)

func startExclusiveNest(t *testing.T, node sharedWriterNode) exclusiveNestNode {
	t.Helper()
	engine := nest.NewEngine(nest.NestOptionWithGetter(node.access), nest.NestOptionWithRemoteEntityManager(node.mgr),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 1, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	meta := nest.HandlerMeta{Rollback: nest.RollbackNone, Durability: nest.DurabilityMemory}
	engine.MustRegisterHandlerWithMeta(exclusiveWriteHandler, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		change := params[0].([2]string)
		es[0].(*reloadableFullDocEntity).set(change[0], change[1])
		return nil, nil
	}, meta)
	engine.MustRegisterHandlerWithMeta(exclusiveReadHandler, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		e := es[0].(*reloadableFullDocEntity)
		e.readOnly.Store(true)
		e.mu.Lock()
		defer e.mu.Unlock()
		return fmt.Sprintf("a=%s;b=%s@%d", e.a, e.b, e.EntityVersion()), nil
	}, meta)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	return exclusiveNestNode{sharedWriterNode: node, engine: engine}
}

func (n exclusiveNestNode) writeNest(ctx context.Context, id int64, a, b string) error {
	_, err := n.engine.Request(ctx, exclusiveWriteHandler, id, nest.NewParams([2]string{a, b}))
	return err
}

func (n exclusiveNestNode) read(t *testing.T, ctx context.Context, id int64) string {
	t.Helper()
	got, err := n.engine.Request(ctx, exclusiveReadHandler, id, nest.NewParams())
	if err != nil {
		t.Fatalf("sid %d read: %v", n.sid, err)
	}
	return got.(string)
}

// 稳定持有期间的只读请求（handler 不改持久字段）：在本进程持实体锁读内存，不向 Mongo 发任何写，也不读所有权记录与数据文档。
func TestExclusiveReadOnlyRequestTouchesNoMongo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := newCountingMongo()
	node := startExclusiveNest(t, newWriterNodesOn(t, db, 2312, nil, 1000)[0])
	id := testRemoteFullIDWithKind(2312, 1, 125)
	if err := node.writeNest(ctx, id, "A1", ""); err != nil {
		t.Fatal(err)
	}
	before := db.snapshot()
	for range 5 {
		if got := node.read(t, ctx, id); got != "a=A1;b=@1" {
			t.Fatalf("read=%q, want %q", got, "a=A1;b=@1")
		}
	}
	got := db.since(before)
	if writes := mongoWrites(got); len(writes) != 0 {
		t.Fatalf("5 read-only requests on a stably held exclusive entity wrote to Mongo %v, want none; all commands=%v", writes, got)
	}
	for _, key := range []string{remoteMetaCollection + ".FindOne", fullDocCollection + ".FindOne"} {
		if got[key] != 0 {
			t.Fatalf("5 read-only requests issued %s %d times, want 0; all commands=%v", key, got[key], got)
		}
	}
}

// 本地可能落后的三个时刻——转走又转回、从共享退回独占、冷加载（含同 sid 重启接管与淘汰后再访问）——之后的第一次请求，
// 读和写都先向权威对齐：不用旧数据答复，也不在旧数据上写。
func TestExclusiveOwnerAlignsBeforeServingAfterItMayHaveFallenBehind(t *testing.T) {
	for i, path := range []string{"ownership transferred away and back", "leave shared mode", "restart with the same sid and eviction"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rawID := int64(2313 + i)
			sids := []int32{1000, 1004}
			if path == "restart with the same sid and eviction" {
				sids = []int32{1000, 1000}
			}
			raw := newWriterNodesOn(t, newRemoteMongoFake(), rawID, nil, sids...)
			a, b := startExclusiveNest(t, raw[0]), startExclusiveNest(t, raw[1])
			id := testRemoteFullIDWithKind(rawID, 1, 125)
			want := "a=A1;b=B1@2"
			switch path {
			case "ownership transferred away and back":
				if err := a.writeNest(ctx, id, "A1", ""); err != nil {
					t.Fatal(err)
				}
				if got := a.read(t, ctx, id); got != "a=A1;b=@1" {
					t.Fatalf("A read before the transfer=%q", got)
				}
				if _, err := a.mgr.TransferRemoteOwnership(ctx, id, b.sid); err != nil {
					t.Fatal(err)
				}
				if err := b.writeNest(ctx, id, "", "B1"); err != nil {
					t.Fatalf("B write after taking over: %v", err)
				}
				if _, err := b.mgr.TransferRemoteOwnership(ctx, id, a.sid); err != nil {
					t.Fatal(err)
				}
			case "leave shared mode":
				// A 先独占写过（已对齐、实例不变），进入共享后 B 写，A 退回独占：A 的实例仍是共享之前那份。
				if err := a.writeNest(ctx, id, "A1", ""); err != nil {
					t.Fatal(err)
				}
				if got := a.read(t, ctx, id); got != "a=A1;b=@1" {
					t.Fatalf("A read before entering shared mode=%q", got)
				}
				if _, err := a.mgr.EnterRemoteSharedMode(ctx, id); err != nil {
					t.Fatal(err)
				}
				if err := b.writeNest(ctx, id, "", "B1"); err != nil {
					t.Fatalf("B shared write: %v", err)
				}
				if _, err := a.mgr.LeaveRemoteSharedMode(ctx, id); err != nil {
					t.Fatal(err)
				}
				// 路由前置检查（与起服预加载同一步）读到的权威版本比 A 的实例新：不能把落后的实例记成已对齐。
				if owner, found, err := a.mgr.ServeRoutedRequest(ctx, id); err != nil || !found || owner != a.sid {
					t.Fatalf("route check after leaving shared mode: owner=%d found=%v err=%v", owner, found, err)
				}
			case "restart with the same sid and eviction":
				// b 是同 sid 重启后的新进程：a 的内存里停着 v1，b 冷加载。
				if err := a.writeNest(ctx, id, "A1", ""); err != nil {
					t.Fatal(err)
				}
				if got := b.read(t, ctx, id); got != "a=A1;b=@1" {
					t.Fatalf("restarted process cold read=%q, want %q", got, "a=A1;b=@1")
				}
				if err := b.writeNest(ctx, id, "", "B1"); err != nil {
					t.Fatal(err)
				}
				// 淘汰后再访问：b 的实例被仅内存卸载，下一次请求从权威重新加载。
				evicted := b.manager.Get(id).(*reloadableFullDocEntity)
				if err := b.mgr.backend.(entity.IRemoteEntityUnloader).UnloadRemoteEntity(ctx, evicted); err != nil {
					t.Fatal(err)
				}
				loads := b.loader.loads.Load()
				if got := b.read(t, ctx, id); got != want {
					t.Fatalf("read after eviction=%q, want %q", got, want)
				}
				if b.loader.loads.Load() != loads+1 {
					t.Fatalf("read after eviction did not reload from the authority (loads %d -> %d)", loads, b.loader.loads.Load())
				}
				if err := b.writeNest(ctx, id, "A2", ""); err != nil {
					t.Fatal(err)
				}
				if doc := b.loaderDocument(t, id); doc != "a=A2;b=B1" {
					t.Fatalf("Mongo document=%q, want %q", doc, "a=A2;b=B1")
				}
				return
			}
			if got := a.read(t, ctx, id); got != want {
				t.Fatalf("A answered %q after it may have fallen behind, want the authority's %q", got, want)
			}
			if err := a.writeNest(ctx, id, "A2", ""); err != nil {
				t.Fatalf("A write: %v", err)
			}
			if doc := a.loaderDocument(t, id); doc != "a=A2;b=B1" {
				t.Fatalf("Mongo document=%q, want A's write on top of B's %q", doc, "a=A2;b=B1")
			}
			if got := a.read(t, ctx, id); got != "a=A2;b=B1@3" {
				t.Fatalf("A read after its write=%q", got)
			}
		})
	}
}

// admitWithoutChange 走一次写准入但不提交（与只读请求同一准入），让实例在本进程加载并对齐。
func (n sharedWriterNode) admitWithoutChange(t *testing.T, ctx context.Context, id int64) {
	t.Helper()
	batch, err := n.mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		t.Fatalf("sid %d admission: %v", n.sid, err)
	}
	_ = batch.Abort(ctx, errors.New("no change"))
	if err := batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// 兜底：单实例前提被打破时（同 sid 的两个进程都认为自己是 owner，或权威的所有权代际已被别处推进），后到的写在投影的
// 元数据 CAS（_ver == BaseVersion、_owner_epoch、_owner_route）上被拒，权威数据不被覆盖；本进程的实例隔离、卸载，
// 下一次访问重新向权威对齐。
func TestExclusiveWriteFromABrokenPremiseIsRejectedAtProjection(t *testing.T) {
	t.Run("two processes with the same sid", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		nodes := newWriterNodesOn(t, newRemoteMongoFake(), 2321, nil, 1000, 1000)
		a1, a2 := nodes[0], nodes[1]
		id := testRemoteFullIDWithKind(2321, 1, 125)
		if _, err := a1.write(t, ctx, id, "A1", "", 0x61); err != nil {
			t.Fatal(err)
		}
		a2.admitWithoutChange(t, ctx, id)
		if _, err := a1.write(t, ctx, id, "A1b", "", 0x62); err != nil {
			t.Fatal(err)
		}
		stale := a2.manager.Get(id).(*reloadableFullDocEntity)
		base, err := a2.write(t, ctx, id, "", "A2", 0x63)
		if err == nil {
			t.Fatalf("the late process's write on base %d was accepted, want the projection to reject it; Mongo=%q", base, a2.loaderDocument(t, id))
		}
		if !errors.Is(err, entity.ErrRemoteVersionConflict) {
			t.Fatalf("late write err=%v, want the version conflict from the projection CAS", err)
		}
		if doc := a1.loaderDocument(t, id); doc != "a=A1b;b=" {
			t.Fatalf("Mongo document=%q after the rejected late write, want the first process's %q", doc, "a=A1b;b=")
		}
		if !stale.IsRemoved() {
			t.Fatal("the late process kept serving its rejected local copy")
		}
		if base, err := a2.write(t, ctx, id, "", "A2", 0x64); err != nil || base != 2 {
			t.Fatalf("late process write after realigning: base=%d err=%v, want base 2", base, err)
		}
		if doc := a1.loaderDocument(t, id); doc != "a=A1b;b=A2" {
			t.Fatalf("Mongo document=%q, want %q", doc, "a=A1b;b=A2")
		}
	})
	t.Run("ownership moved behind the owner", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		a := newWriterNodesOn(t, newRemoteMongoFake(), 2322, nil, 1000)[0]
		id := testRemoteFullIDWithKind(2322, 1, 125)
		if _, err := a.write(t, ctx, id, "A1", "", 0x65); err != nil {
			t.Fatal(err)
		}
		owner, _, err := a.loader.store.GetOwnership(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.loader.store.TransferExpected(ctx, id, owner, 2000); err != nil {
			t.Fatal(err)
		}
		if _, err := a.write(t, ctx, id, "A2", "", 0x66); !errors.Is(err, entity.ErrRemoteVersionConflict) && !errors.Is(err, entity.ErrRemoteFenced) {
			t.Fatalf("write after the ownership moved behind the owner: err=%v, want a rejection", err)
		}
		if doc := a.loaderDocument(t, id); doc != "a=A1;b=" {
			t.Fatalf("Mongo document=%q, want the untouched %q", doc, "a=A1;b=")
		}
		if _, err := a.write(t, ctx, id, "A3", "", 0x67); !errors.Is(err, entity.ErrRemoteFenced) {
			t.Fatalf("write after realigning: err=%v, want ErrRemoteFenced (the authority names sid 2000)", err)
		}
	})
}

// gatedPublishStorage 在 hold 打开时让本进程的 outbox 发布停在标记 Committed 之前（事务已 Applied、未发布）。
type gatedPublishStorage struct {
	*MongoCommitter
	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *gatedPublishStorage) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	if s.hold.Load() {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.MongoCommitter.MarkRemoteCommitPublished(ctx, id)
}

// 独占写不再带每次递增的写许可 fence，确认（acknowledgeRemoteCommit）不能再靠 fence 认出写者：同 sid 的另一个进程
// （outbox 归属按 sid 判定，它把这笔当作自己的立即发布）确认时，空闲的本地副本与这笔提交的 (BaseVersion, LockFence)
// 恰好相同。若据此推进，那份没有这笔修改的副本就冒充最新，下一笔整文档写覆盖掉它。写者只认“正持有写门、定稿了这笔
// 事务”的那个进程。
func TestPublishingAnotherIncarnationsCommitDoesNotAdvanceAnIdleCopy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gated := &gatedPublishStorage{entered: make(chan struct{}, 1), release: make(chan struct{})}
	first := true
	nodes := newWriterNodesOn(t, newRemoteMongoFake(), 2323, func(_ int32, store *MongoCommitter) StorageBackend {
		if !first {
			return store
		}
		first = false
		gated.MongoCommitter = store
		return gated
	}, 1000, 1000)
	a1, a2 := nodes[0], nodes[1]
	id := testRemoteFullIDWithKind(2323, 1, 125)
	if _, err := a1.write(t, ctx, id, "A1", "", 0x71); err != nil {
		t.Fatal(err)
	}
	a2.admitWithoutChange(t, ctx, id)
	idle := a2.manager.Get(id).(*reloadableFullDocEntity)
	gated.hold.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := a1.write(t, ctx, id, "A1b", "", 0x72)
		done <- err
	}()
	select {
	case <-gated.entered:
	case <-ctx.Done():
		t.Fatal("the first process's commit never reached its outbox publish")
	}
	if err := a2.mgr.RecoverOutbox(ctx); err != nil {
		t.Fatalf("the other incarnation publishing the shared outbox: %v", err)
	}
	if got := idle.EntityVersion(); got != 1 {
		t.Fatalf("the idle copy (holding only v1's content) claims version %d after another incarnation's commit was acknowledged on it, want 1", got)
	}
	gated.hold.Store(false)
	close(gated.release)
	if err := <-done; err != nil {
		t.Fatalf("first process write: %v", err)
	}
	_, _ = a2.write(t, ctx, id, "", "A2", 0x73)
	if doc := a1.loaderDocument(t, id); !strings.HasPrefix(doc, "a=A1b;") {
		t.Fatalf("Mongo document=%q: the idle copy's write overwrote the first process's a=A1b", doc)
	}
}

// 起服预加载与路由前置检查的冷加载也是对齐时刻（同一步 routedOwnership）：它读到的权威版本与加载的实例一致时记下
// 对齐，之后的第一次请求不再读权威、也不取写许可。
func TestRoutedColdLoadAlignsSoTheFirstRequestReadsNoAuthority(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := newCountingMongo()
	nodes := newWriterNodesOn(t, db, 2324, nil, 1000, 1000)
	old, restarted := nodes[0], nodes[1]
	id := testRemoteFullIDWithKind(2324, 1, 125)
	if _, err := old.write(t, ctx, id, "A1", "", 0x81); err != nil {
		t.Fatal(err)
	}
	if owner, found, err := restarted.mgr.ServeRoutedRequest(ctx, id); err != nil || !found || owner != restarted.sid {
		t.Fatalf("route check: owner=%d found=%v err=%v", owner, found, err)
	}
	before := db.snapshot()
	if base, err := restarted.write(t, ctx, id, "", "B1", 0x82); err != nil || base != 1 {
		t.Fatalf("first write after the routed cold load: base=%d err=%v", base, err)
	}
	got := db.since(before)
	if got[remoteMetaCollection+".FindOne"] != 0 || got[remoteMetaCollection+".FindOneAndUpdate"] != 0 {
		t.Fatalf("first write after the routed cold load read or granted on %s: %v, want only the commit", remoteMetaCollection, got)
	}
}

// 新实体的第一次写：首写认领插入的所有权记录就是对齐所需的权威状态（版本是初始值、代际是这次认领的），认领之后不再读
// 一次权威对齐（2026-11-06 起认领直接带回它）；认领本身也不先读一次再插入——准入刚读过“没有记录”，直接插入，撞上已有
// 记录才读回。这次准入在 _remote_entity_meta 上只剩确认“没有记录”的那一次读与认领的一次插入。
func TestExclusiveFirstWriteClaimNeedsNoAlignmentRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := newCountingMongo()
	a := newWriterNodesOn(t, db, 2325, nil, 1000)[0]
	// 测试 loader 冷加载时自己读一次所有权记录取版本（正式 loader 读的是数据文档）；让它走不计数的同一份数据，
	// 计数里只剩写准入发出的命令。
	a.loader.store = newBoundMongoCommitter(db.IMongo, "control", a.sid, 0)
	id := testRemoteFullIDWithKind(2325, 1, 125)
	from, before := db.sequenceLen(), db.snapshot()
	if base, err := a.write(t, ctx, id, "A1", "", 0x91); err != nil || base != 0 {
		t.Fatalf("first write: base=%d err=%v", base, err)
	}
	var meta []string
	for _, call := range db.sequence(from) {
		if collection, _, _ := strings.Cut(call, "."); collection == remoteMetaCollection {
			meta = append(meta, call)
		}
	}
	claim := slices.Index(meta, remoteMetaCollection+".InsertOne")
	if claim < 0 {
		t.Fatalf("first write did not claim the ownership record; %s commands=%v", remoteMetaCollection, meta)
	}
	for _, call := range meta[claim+1:] {
		if call == remoteMetaCollection+".FindOne" {
			t.Fatalf("first write read the authority again after its own claim (alignment read); %s commands in order=%v", remoteMetaCollection, meta)
		}
	}
	if n := db.since(before)[remoteMetaCollection+".FindOne"]; n != 1 {
		t.Fatalf("first write read the ownership record %d times, want 1 (the lookup that found none); %s commands in order=%v", n, remoteMetaCollection, meta)
	}
	if base, err := a.write(t, ctx, id, "A2", "", 0x92); err != nil || base != 1 {
		t.Fatalf("second write: base=%d err=%v", base, err)
	}
	if doc := a.loaderDocument(t, id); doc != "a=A2;b=" {
		t.Fatalf("Mongo document=%q, want %q", doc, "a=A2;b=")
	}
}

// 认领撞上已有的、本 sid 的记录，而且它的版本比本地实例新（这里：B 缓存了“没有记录”，同 sid 的 A 在此期间认领并写了
// 两笔）：认领带回的是那条记录的权威版本，准入按对齐路径先把落后的本地实例卸载、从权威重载，再在最新版本上写，
// 不在旧数据上写、不覆盖 A 的修改。
func TestExclusiveClaimOfAnExistingNewerRecordReloadsBeforeWriting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes := newWriterNodesOn(t, newRemoteMongoFake(), 2326, nil, 1000, 1000)
	a, b := nodes[0], nodes[1]
	id := testRemoteFullIDWithKind(2326, 1, 125)
	// B 先在本地有一份 v0 的实例，并缓存“没有所有权记录”（TTL 内准入不再读权威，直接认领）。
	if _, err := b.loader.LoadEntity(ctx, id, 125); err != nil {
		t.Fatal(err)
	}
	b.mgr.cfg.MarkerCacheTTL = time.Hour
	meta := entity.ResolveEntityID(id)
	wrapper := b.mgr.getOrCreate(meta.FullID, meta.Category, meta.Kind)
	if err := wrapper.refreshMarked(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found := wrapper.cachedOwnership(); found {
		t.Fatal("B cached an ownership record before anyone claimed")
	}
	wrapper.release()
	for i, value := range []string{"A1", "A2"} {
		if _, err := a.write(t, ctx, id, value, "", byte(0x93+i)); err != nil {
			t.Fatalf("A write %d: %v", i+1, err)
		}
	}
	loads := b.loader.loads.Load()
	base, err := b.write(t, ctx, id, "", "B1", 0x95)
	if err != nil || base != 2 {
		t.Fatalf("B write after claiming an existing record at v2: base=%d err=%v, want base 2", base, err)
	}
	if b.loader.loads.Load() != loads+1 {
		t.Fatalf("B did not reload its v0 copy from the authority before writing (loads %d -> %d)", loads, b.loader.loads.Load())
	}
	if doc := b.loaderDocument(t, id); doc != "a=A2;b=B1" {
		t.Fatalf("Mongo document=%q, want B's write on top of A's %q", doc, "a=A2;b=B1")
	}
}
