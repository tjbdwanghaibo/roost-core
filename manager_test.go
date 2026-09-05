package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	redis "github.com/tjbdwanghaibo/roost-core/redis"
)

type mockVersionedLock struct {
	mu       sync.Mutex
	acquired bool
	version  int64
	locks    int
	fence    uint64
}

func (l *mockVersionedLock) TryLock(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.acquired {
		return ErrVersionedLockNotAcquired
	}
	l.acquired = true
	l.locks++
	l.fence++
	return nil
}
func (l *mockVersionedLock) Lock(ctx context.Context) error { return l.TryLock(ctx) }
func (l *mockVersionedLock) Unlock(_ context.Context, version int64, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.acquired {
		return ErrVersionedLockNotOwned
	}
	l.acquired = false
	l.version = version
	return nil
}
func (l *mockVersionedLock) UnlockWithRetry(ctx context.Context, version int64, ttl time.Duration, _ int, _ time.Duration) error {
	return l.Unlock(ctx, version, ttl)
}
func (l *mockVersionedLock) Version() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.version
}
func (l *mockVersionedLock) IsAcquired() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquired
}
func (l *mockVersionedLock) Fence() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fence
}
func (l *mockVersionedLock) Touch(context.Context, time.Duration) error { return nil }
func (l *mockVersionedLock) Refresh(context.Context) error              { return nil }
func (l *mockVersionedLock) Close() error                               { return nil }

type mockVersionedLockFactory struct {
	mu    sync.Mutex
	locks map[int64]*mockVersionedLock
}

func newMockVersionedLockFactory() *mockVersionedLockFactory {
	return &mockVersionedLockFactory{locks: make(map[int64]*mockVersionedLock)}
}
func (f *mockVersionedLockFactory) NewVersionedLock(id int64, _ redis.VersionedLockOptions) redis.IVersionedLock {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := &mockVersionedLock{}
	f.locks[id] = l
	return l
}

type mockLoader struct {
	mu       sync.Mutex
	loaded   []int64
	loadErr  bool
	entities map[int64]entity.IThreadSafeRemoteEntity
}

func newMockLoader() *mockLoader {
	return &mockLoader{entities: make(map[int64]entity.IThreadSafeRemoteEntity)}
}
func (l *mockLoader) LoadRemoteEntity(_ context.Context, id int64, _ entity.EntityKind) (entity.IThreadSafeRemoteEntity, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadErr {
		return nil, errors.New("load failed")
	}
	l.loaded = append(l.loaded, id)
	return l.entities[id], nil
}

func (l *mockLoader) add(entities ...entity.IThreadSafeRemoteEntity) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, current := range entities {
		l.entities[current.GUId()] = current
	}
}

type mockMarkerStore struct {
	mu     sync.Mutex
	marks  map[int64]bool
	leases map[int64]entity.RemoteEntityMarkerLease
	err    error
	gets   int
}

func newMockMarkerStore() *mockMarkerStore {
	return &mockMarkerStore{marks: make(map[int64]bool), leases: make(map[int64]entity.RemoteEntityMarkerLease)}
}
func (s *mockMarkerStore) GetOwnership(_ context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.err != nil {
		return entity.RemoteEntityMarkerLease{}, false, s.err
	}
	lease, found := s.leases[id]
	return lease, found, nil
}
func (s *mockMarkerStore) ClaimOwnership(_ context.Context, id int64, owner int32) (entity.RemoteEntityMarkerLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lease, found := s.leases[id]; found {
		if lease.OwnerSid == owner {
			return lease, nil
		}
		return entity.RemoteEntityMarkerLease{}, errors.New("ownership conflict")
	}
	lease := entity.RemoteEntityMarkerLease{OwnerSid: owner, MarkerEpoch: 1, RouteEpoch: 1}
	s.marks[id], s.leases[id] = false, lease
	return lease, nil
}
func (s *mockMarkerStore) EnterSharedExpected(_ context.Context, id int64, expected entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.leases[id]
	current.Shared = s.marks[id]
	if current != expected || current.Shared {
		return entity.RemoteEntityMarkerLease{}, errors.New("marker CAS conflict")
	}
	next := entity.RemoteEntityMarkerLease{OwnerSid: expected.OwnerSid, MarkerEpoch: expected.MarkerEpoch + 1, RouteEpoch: max(expected.RouteEpoch, 1), Shared: true}
	s.marks[id], s.leases[id] = true, next
	return next, nil
}
func (s *mockMarkerStore) TransferExpected(_ context.Context, id int64, expected entity.RemoteEntityMarkerLease, owner int32) (entity.RemoteEntityMarkerLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.leases[id]
	current.Shared = s.marks[id]
	if s.err != nil || current != expected || expected.OwnerSid == owner {
		return entity.RemoteEntityMarkerLease{}, errors.New("ownership CAS conflict")
	}
	next := expected
	next.OwnerSid, next.MarkerEpoch, next.RouteEpoch = owner, next.MarkerEpoch+1, next.RouteEpoch+1
	s.leases[id], s.marks[id] = next, next.Shared
	return next, nil
}
func (s *mockMarkerStore) LeaveSharedExpected(_ context.Context, id int64, lease entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil || !s.marks[id] || !lease.Shared {
		return entity.RemoteEntityMarkerLease{}, errors.New("marker conflict")
	}
	next := entity.RemoteEntityMarkerLease{OwnerSid: lease.OwnerSid, MarkerEpoch: lease.MarkerEpoch + 1, RouteEpoch: max(lease.RouteEpoch, 1)}
	s.marks[id], s.leases[id] = false, next
	return next, nil
}

type testDirty struct{ dirty bool }

func (d *testDirty) Dirty() bool { return d.dirty }
func (d *testDirty) SelfClean()  { d.dirty = false }

type testRemoteEntity struct {
	entity.RemoteEntityBase
	dirty    testDirty
	dao      testRemoteDao
	buildErr error
}
type testRemoteDao struct {
	id    int64
	dirty *testDirty
}

func newTestRemoteEntity(id int64, category entity.EntityCategory, kind entity.EntityKind) *testRemoteEntity {
	e := &testRemoteEntity{}
	e.RemoteEntityBase.EntityBase = *entity.NewEntityBase(testRemoteFullIDWithKind(id, category, kind), category, false, kind)
	e.dao = testRemoteDao{id: e.ID(), dirty: &e.dirty}
	return e
}
func testRemoteFullIDWithKind(id int64, category entity.EntityCategory, kind entity.EntityKind) int64 {
	entity.MustRegisterEntityKindCategory(kind, category)
	fullID, err := entity.BuildEntityID(id, kind)
	if err != nil {
		panic(err)
	}
	return fullID
}
func (e *testRemoteEntity) Base() *entity.EntityBase              { return &e.RemoteEntityBase.EntityBase }
func (e *testRemoteEntity) OnDataChange([]byte, int64)            {}
func (e *testRemoteEntity) RangeDao(fn func(entity.DaoInterface)) { fn(&e.dao) }
func (d *testRemoteDao) Id() int64                                { return d.id }
func (d *testRemoteDao) SetId(id int64)                           { d.id = id }
func (d *testRemoteDao) DbName() string                           { return "test" }
func (d *testRemoteDao) CollName() string                         { return "remote" }
func (d *testRemoteDao) Dirty() entity.IDirty                     { return d.dirty }
func (d *testRemoteDao) CleanDirty()                              { d.dirty.SelfClean() }

func TestManagerCoalescesConcurrentWrapperCreation(t *testing.T) {
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	markers := newMockMarkerStore()
	mgr.SetOwnershipStore(markers)
	id := testRemoteFullIDWithKind(1301, 1, 1)
	const workers = 32
	wrappers := make([]*remoteEntityWrapper, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range wrappers {
		go func(i int) { defer wg.Done(); wrappers[i] = mgr.getOrCreate(id, 1, 1) }(i)
	}
	wg.Wait()
	for i := 1; i < len(wrappers); i++ {
		if wrappers[i] != wrappers[0] {
			t.Fatalf("wrapper %d differs from coalesced instance", i)
		}
	}
	for _, wrapper := range wrappers {
		wrapper.release()
	}
	markers.mu.Lock()
	gets := markers.gets
	markers.mu.Unlock()
	if gets != 1 {
		t.Fatalf("marker reads=%d, want 1", gets)
	}
}

func TestWrapperCapacityEvictsIdleEntry(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WrapperCapacity = 1
	cfg.WrapperIdleTTL = time.Hour
	mgr := newRemoteEntityManager(newMockVersionedLockFactory(), cfg, 1000)
	mgr.SetOwnershipStore(newMockMarkerStore())
	firstID := testRemoteFullIDWithKind(2301, 1, 1)
	secondID := testRemoteFullIDWithKind(2302, 1, 1)
	first := mgr.getOrCreate(firstID, 1, 1)
	if first == nil {
		t.Fatal("create first wrapper")
	}
	first.release()
	second := mgr.getOrCreate(secondID, 1, 1)
	if second == nil {
		t.Fatal("idle wrapper was not evicted")
	}
	defer second.release()
	if _, ok := mgr.get(firstID); ok || mgr.wrapperCount() != 1 {
		t.Fatalf("wrapper eviction failed: count=%d", mgr.wrapperCount())
	}
}

func TestVersionedLockFactory(t *testing.T) {
	lock := newMockVersionedLockFactory().NewVersionedLock(42, redis.DefaultVersionedLockOptions("e", 30*time.Second))
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(context.Background(), 5, time.Hour); err != nil {
		t.Fatal(err)
	}
	if lock.IsAcquired() || lock.Version() != 5 {
		t.Fatalf("invalid unlocked state")
	}
}

func TestVersionedLockErrors(t *testing.T) {
	lock := newMockVersionedLockFactory().NewVersionedLock(1, redis.DefaultVersionedLockOptions("e", time.Second))
	if err := lock.Unlock(context.Background(), 0, 0); !errors.Is(err, ErrVersionedLockNotOwned) {
		t.Fatalf("error=%v", err)
	}
	_ = lock.Lock(context.Background())
	if err := lock.Lock(context.Background()); err == nil {
		t.Fatal("expected double-acquire error")
	}
}

func TestVersionedLockInvalidConfigurationReturnsError(t *testing.T) {
	lock := newVersionedLock(nil, 1, redis.VersionedLockOptions{TTL: 0})
	if err := lock.TryLock(context.Background()); !errors.Is(err, ErrVersionedLockConfig) {
		t.Fatalf("TryLock=%v", err)
	}
	if err := lock.Unlock(context.Background(), 0, 0); !errors.Is(err, ErrVersionedLockConfig) {
		t.Fatalf("Unlock=%v", err)
	}
	if err := lock.Close(); !errors.Is(err, ErrVersionedLockConfig) {
		t.Fatalf("Close=%v", err)
	}
}

func TestVersionedLockTokenUsesCryptographicRandomness(t *testing.T) {
	first, second := generateToken(), generateToken()
	if first == "" || second == "" || first == second {
		t.Fatalf("invalid tokens %q %q", first, second)
	}
}

// unfencedLock is a versioned lock WITHOUT Fence(): what a foreign
// IVersionedLockFactory would hand out. It delegates everything else to the
// fenced mock so the only thing missing is the fence.
type unfencedLock struct{ inner *mockVersionedLock }

func (l *unfencedLock) TryLock(ctx context.Context) error { return l.inner.TryLock(ctx) }
func (l *unfencedLock) Lock(ctx context.Context) error    { return l.inner.Lock(ctx) }
func (l *unfencedLock) Unlock(ctx context.Context, version int64, ttl time.Duration) error {
	return l.inner.Unlock(ctx, version, ttl)
}
func (l *unfencedLock) UnlockWithRetry(ctx context.Context, version int64, ttl time.Duration, count int, interval time.Duration) error {
	return l.inner.UnlockWithRetry(ctx, version, ttl, count, interval)
}
func (l *unfencedLock) Version() int64   { return l.inner.Version() }
func (l *unfencedLock) IsAcquired() bool { return l.inner.IsAcquired() }
func (l *unfencedLock) Touch(ctx context.Context, d time.Duration) error {
	return l.inner.Touch(ctx, d)
}
func (l *unfencedLock) Refresh(ctx context.Context) error { return l.inner.Refresh(ctx) }
func (l *unfencedLock) Close() error                      { return l.inner.Close() }

type unfencedLockFactory struct{}

func (unfencedLockFactory) NewVersionedLock(int64, redis.VersionedLockOptions) redis.IVersionedLock {
	return &unfencedLock{inner: &mockVersionedLock{}}
}

// A lock that cannot fence must be refused where the factory is wired, not
// discovered one operation at a time. Remote Entity's write gate needs the
// fence a lock allocates on acquisition; with a plain versioned lock every
// shared operation is refused at run time with ErrRemoteFenced — which is
// fail-closed, but late, noisy, and indistinguishable from a real fence
// conflict in the metrics. FEATURE_LOGIC §4.2 item 7.
func TestManagerRefusesALockFactoryWithoutFences(t *testing.T) {
	mgr := newRemoteEntityManager(unfencedLockFactory{}, DefaultConfig(), 1000)
	if err := mgr.LockFactoryError(); err == nil {
		t.Fatal("a factory of unfenced locks was accepted at construction")
	}
	if w := mgr.getOrCreate(testRemoteFullIDWithKind(1302, 1, 1), 1, 1); w != nil {
		w.release()
		t.Fatal("a wrapper was created over an unfenced lock; every shared operation on it would be refused at run time")
	}
	fenced := newRemoteEntityManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	if err := fenced.LockFactoryError(); err != nil {
		t.Fatalf("a fenced factory was refused: %v", err)
	}
	if w := fenced.getOrCreate(testRemoteFullIDWithKind(1302, 1, 1), 1, 1); w == nil {
		t.Fatal("a fenced factory was refused")
	} else {
		w.release()
	}
}
