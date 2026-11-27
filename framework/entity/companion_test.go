package entity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
	"github.com/tjbdwanghaibo/roost-core/infra/base/lock"
)

const (
	companionTestP EntityKind = 143
	companionTestC EntityKind = 144
)

type companionTestPrincipal struct{ *mgrTestEntity }
type companionTestChild struct{ *mgrTestEntity }

func companionFixture(t *testing.T) *EntityManager {
	t.Helper()
	isolateEntityRegistry(t)
	old := companionRegistry.Load()
	t.Cleanup(func() { companionRegistry.Store(old) })
	for _, kind := range []EntityKind{companionTestP, companionTestC} {
		RegisterEntityBuilder(&EntityBuilderParam{Kind: kind, Category: testEntityCategory, NoPersist: true, Builder: func(p *EntityCreateParam) (IThreadSafeEntity, error) {
			e := &mgrTestEntity{EntityBase: NewEntityBaseWithMutex(p.Id, p.Category, true, p.Mutex, p.Kind)}
			if p.Kind == companionTestP {
				return &companionTestPrincipal{e}, nil
			}
			return &companionTestChild{e}, nil
		}})
	}
	if err := RegisterCompanionKind[*companionTestPrincipal, *companionTestChild](companionTestP, companionTestC); err != nil {
		t.Fatal(err)
	}
	return NewEntityManager()
}
func makeCompanion(t *testing.T, m *EntityManager, p EntityCreateParam) IThreadSafeEntity {
	t.Helper()
	v, e := m.Create(&p)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func testPair(t *testing.T, m *EntityManager) (IThreadSafeEntity, IThreadSafeEntity) {
	t.Helper()
	p := makeCompanion(t, m, EntityCreateParam{Kind: companionTestP, UniqueID: 71, IsCreate: true})
	c := makeCompanion(t, m, EntityCreateParam{Kind: companionTestC, CompanionOf: p.ID(), IsCreate: true})
	return p, c
}
func withCompanionView(t *testing.T, members []IThreadSafeEntity, stage func(*CompanionChange) error, fn func()) {
	t.Helper()
	gs, done := NewGuardScope(entitypermit.System(), "test")
	defer done()
	for _, v := range members {
		if !gs.Guard().TryRequireEntity(entitypermit.System(), v) {
			t.Fatal("logical guard")
		}
	}
	restore := PushCompanionScope(entitypermit.System(), members, stage)
	defer restore()
	fn()
}
func TestCompanionIdentityAndExplicitComposition(t *testing.T) {
	m := companionFixture(t)
	p := makeCompanion(t, m, EntityCreateParam{Kind: companionTestP, UniqueID: 12345, IsCreate: true})
	id, e := CompanionID(p.ID(), companionTestC)
	if e != nil || GetUniqueIDFromEntityID(id) != p.UniqueID() {
		t.Fatal(id, e)
	}
	back, e := PrincipalID(id)
	if e != nil || back != p.ID() {
		t.Fatal(back, e)
	}
	for _, bad := range []int64{0, -1, p.ID() ^ 1, p.ID() ^ (1 << EntityRemoteShift), id} {
		if _, e := CompanionID(bad, companionTestC); e == nil {
			t.Fatalf("accepted malformed %d", bad)
		}
	}
	if e := RegisterCompanionKind[*companionTestChild, *companionTestPrincipal](companionTestC, companionTestP); e == nil {
		t.Fatal("accepted cycle")
	}
	for _, args := range []EntityCreateParam{{Kind: companionTestP, CompanionOf: p.ID()}, {Kind: companionTestC, Id: p.ID(), CompanionOf: p.ID()}, {Kind: companionTestC, UniqueID: 12, CompanionOf: p.ID()}, {Kind: companionTestC, CompanionOf: id}, {Kind: companionTestC, CompanionOf: p.ID() + 1<<EntityIDTagBits}} {
		args.IsCreate = true
		if v, e := m.Create(&args); v != nil || e == nil || m.Len() != 1 {
			t.Fatalf("wrong identity published: %+v %v", args, e)
		}
	}
	c := makeCompanion(t, m, EntityCreateParam{Kind: companionTestC, UniqueID: p.UniqueID(), IsCreate: true})
	if c.ID() != id || SnapshotCompanionDomain(c).Root != c {
		t.Fatal("same number auto-attached")
	}
	if m.AttachCompanion(id) == nil {
		t.Fatal("unproven pair attached")
	}
	withCompanionView(t, []IThreadSafeEntity{p}, nil, func() {
		_, e := GetCompanion[*companionTestChild]()
		if !errors.Is(e, ErrCompanionScope) {
			t.Fatalf("unrelated peer reported as declared: %v", e)
		}
	})
	if err := m.DeclareDetachedCompanion(p.ID(), id); err != nil {
		t.Fatal(err)
	}
	withCompanionView(t, []IThreadSafeEntity{p}, nil, func() {
		v, e := GetCompanion[*companionTestChild]()
		if v != nil || !errors.Is(e, ErrCompanionDetached) {
			t.Fatal(v, e)
		}
	})
	if e := m.AttachCompanion(id); e != nil {
		t.Fatal(e)
	}
	withCompanionView(t, []IThreadSafeEntity{p, c}, nil, func() {
		v, e := GetCompanion[*companionTestChild]()
		if v != c || e != nil {
			t.Fatal(v, e)
		}
		if _, e = GetPrincipal[*companionTestPrincipal](); e == nil {
			t.Fatal("parent got itself")
		}
		nested, done := NewGuardScope(entitypermit.System(), "isolated")
		_ = nested
		_, e = GetCompanion[*companionTestChild]()
		done()
		if e == nil {
			t.Fatal("scope leaked")
		}
	})
	withCompanionView(t, []IThreadSafeEntity{c, p}, nil, func() {
		v, e := GetPrincipal[*companionTestPrincipal]()
		if v != p || e != nil {
			t.Fatal(v, e)
		}
		if _, e = GetCompanion[*companionTestChild](); e == nil {
			t.Fatal("child got itself")
		}
	})
	if _, e := GetPrincipal[*companionTestPrincipal](); e == nil {
		t.Fatal("getter outside handler")
	}
}
func TestCompanionStageRollbackCommitAndReload(t *testing.T) {
	m := companionFixture(t)
	p, c := testPair(t, m)
	pid, cid := p.ID(), c.ID()
	before := SnapshotCompanionDomain(c)
	if before.Root != p || !before.Valid() {
		t.Fatal("bad initial closure")
	}
	for _, commit := range []bool{false, true} {
		var ch *CompanionChange
		withCompanionView(t, []IThreadSafeEntity{p, c}, func(change *CompanionChange) error { ch = change; return nil }, func() {
			if e := m.DetachCompanion(cid); !errors.Is(e, ErrCompanionScope) {
				t.Fatal("manager bypass", e)
			}
			if e := DetachCompanion(cid); e != nil {
				t.Fatal(e)
			}
			if e := DetachCompanion(cid); !errors.Is(e, ErrCompanionBusy) {
				t.Fatal("duplicate staged", e)
			}
			if !before.Valid() {
				t.Fatal("changed before commit")
			}
			if commit {
				ch.Apply()
			}
			ch.Release()
		})
		if before.Valid() == commit {
			t.Fatal("wrong change visibility", commit)
		}
	}
	if e := m.Destroy(context.Background(), c, DestroyReasonMemoryUnload, false); e != nil {
		t.Fatal(e)
	}
	fresh := makeCompanion(t, m, EntityCreateParam{Kind: companionTestC, Id: cid})
	if SnapshotCompanionDomain(fresh).Root != fresh || SnapshotCompanionDomain(c).Valid() {
		t.Fatal("reloaded/retired authorization")
	}
	if e := m.AttachCompanion(cid); e == nil {
		t.Fatal("cold reload inferred a declaration")
	}
	if e := m.DeclareDetachedCompanion(pid, cid); e != nil {
		t.Fatal(e)
	}
	if e := m.AttachCompanion(cid); e != nil {
		t.Fatal(e)
	}
	s := SnapshotCompanionDomain(fresh)
	if e := m.Destroy(context.Background(), p, DestroyReasonCommon, false); e != nil {
		t.Fatal(e)
	}
	if s.Valid() || SnapshotCompanionDomain(fresh).Root != fresh {
		t.Fatal("retirement not synchronized")
	}
	newP := makeCompanion(t, m, EntityCreateParam{Kind: companionTestP, Id: pid, IsCreate: true})
	if m.AttachCompanion(cid) == nil {
		t.Fatal("replacement principal inherited authority")
	}
	if err := m.DeclareDetachedCompanion(pid, cid); err != nil {
		t.Fatal(err)
	}
	if m.AttachCompanion(cid) != nil || SnapshotCompanionDomain(fresh).Root != newP {
		t.Fatal("explicit rebind failed")
	}
	if m.TryAdd(newP) == nil || NewEntityManager().TryAdd(newP) == nil {
		t.Fatal("same instance managed twice")
	}
	other := NewEntityManager()
	second := makeCompanion(t, other, EntityCreateParam{Kind: companionTestC, Id: cid})
	if SnapshotCompanionDomain(second).Root != second || other.AttachCompanion(cid) == nil {
		t.Fatal("cross-manager implicit authority")
	}
}

func TestCompanionDeleteVersusExplicitColdRestore(t *testing.T) {
	m := companionFixture(t)
	p, c := testPair(t, m)
	pid, cid := p.ID(), c.ID()
	if e := m.DetachCompanion(cid); e != nil {
		t.Fatal(e)
	}
	destroy := func(e IThreadSafeEntity, r EntityDestroyReason) {
		t.Helper()
		if err := m.Destroy(context.Background(), e, r, false); err != nil {
			t.Fatal(err)
		}
	}
	destroy(c, DestroyReasonCommon)
	c = makeCompanion(t, m, EntityCreateParam{IsCreate: true, Kind: companionTestC, Id: cid})
	if m.AttachCompanion(cid) == nil {
		t.Fatal("new unrelated child inherited a deleted declaration")
	}
	destroy(c, DestroyReasonCommon)
	c = makeCompanion(t, m, EntityCreateParam{IsCreate: true, Kind: companionTestC, CompanionOf: pid})
	if SnapshotCompanionDomain(c).Root != p {
		t.Fatal("new explicit child inherited deleted detached state")
	}
	destroy(c, DestroyReasonMemoryUnload)
	cold := makeCompanion(t, m, EntityCreateParam{Kind: companionTestC, CompanionOf: pid})
	if SnapshotCompanionDomain(cold).Root != cold {
		t.Fatal("restored declaration auto-attached")
	}
	if e := m.AttachCompanion(cid); e != nil {
		t.Fatal(e)
	}
	destroy(p, DestroyReasonMemoryUnload)
	np := makeCompanion(t, m, EntityCreateParam{Kind: companionTestP, Id: pid})
	if SnapshotCompanionDomain(cold).Root != cold || m.AttachCompanion(cid) != nil || SnapshotCompanionDomain(cold).Root != np {
		t.Fatal("live child must explicitly rebind after principal reload")
	}
	if e := m.DetachCompanion(cid); e != nil {
		t.Fatal(e)
	}
	destroy(np, DestroyReasonCommon)
	_ = makeCompanion(t, m, EntityCreateParam{IsCreate: true, Kind: companionTestP, Id: pid})
	if m.AttachCompanion(cid) == nil {
		t.Fatal("detached live child retained a deleted principal's authority")
	}
}

// Wrap a NON-reentrant native mutex, with a deterministic old-waiter edge.
type countedEntrance struct {
	lock.Mutex
	calls   atomic.Int64
	waiting atomic.Pointer[chan struct{}]
}

func (l *countedEntrance) Lock() {
	l.calls.Add(1)
	if c := l.waiting.Swap(nil); c != nil {
		close(*c)
	}
	l.Mutex.Lock()
}
func (l *countedEntrance) TryLock() bool { l.calls.Add(1); return l.Mutex.TryLock() }
func TestCompanionNativeDedupAndOldWaiter(t *testing.T) {
	m := companionFixture(t)
	pid, _ := BuildEntityID(73, companionTestP)
	cid, _ := CompanionID(pid, companionTestC)
	pr := &countedEntrance{Mutex: lock.NewDefaultMutex(pid)}
	cr := &countedEntrance{Mutex: lock.NewDefaultMutex(cid)}
	p := makeCompanion(t, m, EntityCreateParam{Kind: companionTestP, Id: pid, IsCreate: true, Mutex: pr})
	c := makeCompanion(t, m, EntityCreateParam{Kind: companionTestC, CompanionOf: pid, IsCreate: true, Mutex: cr})
	a, b := pr.calls.Load(), cr.calls.Load()
	withCompanionView(t, []IThreadSafeEntity{p, c}, nil, func() {
		if pr.calls.Load() != a+1 || cr.calls.Load() != b {
			t.Fatalf("redundant native call root=%d child=%d", pr.calls.Load()-a, cr.calls.Load()-b)
		}
	})
	if e := m.DetachCompanion(cid); e != nil {
		t.Fatal(e)
	}
	view := c.GetMutex()
	assertNativeLockDenied(t, view, cid)
	stable := testSystemMutex(c)
	stable.Lock()
	started := make(chan struct{})
	cr.waiting.Store(&started)
	acquired := make(chan struct{})
	go func() { stable.Lock(); close(acquired); stable.Unlock() }()
	<-started
	if e := m.AttachCompanion(cid); e != nil {
		t.Fatal(e)
	}
	if stable != testSystemMutex(c) || view != c.GetMutex() {
		t.Fatal("domain transition replaced the stable system entrance or public view")
	}
	assertNativeLockDenied(t, view, cid)
	assertNativeLockDenied(t, p.GetMutex(), pid)
	testSystemMutex(p).Lock()
	stable.Unlock()
	select {
	case <-acquired:
		t.Fatal("old waiter entered changed domain")
	case <-time.After(12 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Millisecond)
	defer cancel()
	miss := make(chan error, 1)
	go func() { miss <- m.Destroy(ctx, c, DestroyReasonCommon, false) }()
	if e := <-miss; !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("remove bypassed root: %v", e)
	}
	testSystemMutex(p).Unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("queued waiter stranded")
	}
}

type rejectCompanionCapture struct{ inspect func(IThreadSafeEntity) }

func (cap rejectCompanionCapture) CaptureCreatedEntity(e IThreadSafeEntity, _ func()) error {
	cap.inspect(e)
	return errors.New("test reject creation")
}
func (rejectCompanionCapture) BeginCreatedEntityBuild() func(map[string]DaoInterface, error) error {
	return nil
}
func (rejectCompanionCapture) CreatedEntityLockBusy(int64) error { return ErrCompanionBusy }

func TestCompanionAtomicCreateAndCaptureFailure(t *testing.T) {
	m := companionFixture(t)
	p := makeCompanion(t, m, EntityCreateParam{Kind: companionTestP, UniqueID: 9001, IsCreate: true})
	cid, _ := CompanionID(p.ID(), companionTestC)
	testSystemMutex(p).Lock()
	failed := make(chan error, 1)
	go func() {
		_, err := m.Create(&EntityCreateParam{Kind: companionTestC, CompanionOf: p.ID(), IsCreate: true})
		failed <- err
	}()
	if err := <-failed; !errors.Is(err, ErrCompanionBusy) || m.Get(cid) != nil || len(SnapshotCompanionDomain(p).Members) != 0 {
		t.Fatal("busy root partially published", err)
	}
	testSystemMutex(p).Unlock()
	var stale IThreadSafeEntity
	scope, done := NewGuardScope(entitypermit.System(), "capture")
	scope.Guard().BindCreatedEntityCapturer(entitypermit.System(), rejectCompanionCapture{func(e IThreadSafeEntity) {
		stale = e
		s := SnapshotCompanionDomain(e)
		if s.Root != p || !s.Valid() || m.Get(cid) != e {
			t.Fatal("published before attached")
		}
		blocked := make(chan bool, 1)
		go func() {
			if testSystemMutex(e).TryLock() {
				testSystemMutex(e).Unlock()
				blocked <- false
				return
			}
			blocked <- true
		}()
		if !<-blocked {
			t.Fatal("published without lock domain")
		}
	}})
	v, err := m.CreateInScope(scope, &EntityCreateParam{Kind: companionTestC, CompanionOf: p.ID(), IsCreate: true})
	done()
	if v != nil || err == nil || stale == nil || SnapshotCompanionDomain(stale).Valid() || m.Len() != 1 || len(SnapshotCompanionDomain(p).Members) != 0 {
		t.Fatal("residue after rejection", err)
	}
	fresh := makeCompanion(t, m, EntityCreateParam{Kind: companionTestC, CompanionOf: p.ID(), IsCreate: true})
	if SnapshotCompanionDomain(fresh).Root != p {
		t.Fatal("failed create left detached tombstone")
	}
	if err := m.TryAdd(stale); err == nil {
		t.Fatal("re-added retired instance")
	}
}

func TestCompanionSyncSnapshotRejectsForeignSameIDGuard(t *testing.T) {
	ma := companionFixture(t)
	_, a := testPair(t, ma)
	mb := NewEntityManager()
	p, b := testPair(t, mb)
	b.Base().EnableSync(EntitySyncCreateParam{Enabled: true})
	state := b.Base().Sync()
	testSystemMutex(p).Lock()
	started := make(chan struct{})
	captured := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		scope, exit := NewGuardScope(entitypermit.System(), "foreign")
		defer exit()
		scope.Guard().RequireEntity(entitypermit.System(), a)
		close(started)
		done <- state.withEntityLock(func() error { close(captured); return nil })
	}()
	<-started
	select {
	case <-captured:
		t.Fatal("foreign same-ID guard bypassed snapshot lock")
	case <-time.After(15 * time.Millisecond):
	}
	testSystemMutex(p).Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCompanionScopedChangeCannotBypassRollback(t *testing.T) {
	m := companionFixture(t)
	p, c := testPair(t, m)
	id := c.ID()
	g := BorrowIndependentGuard(entitypermit.System())
	g.RequireEntity(entitypermit.System(), p)
	if e := m.DetachCompanion(id); !errors.Is(e, ErrCompanionScope) {
		t.Fatalf("independent guard bypass: %v", e)
	}
	EntityGuardRelease(entitypermit.System(), g)
	withCompanionView(t, []IThreadSafeEntity{p, c}, func(*CompanionChange) error { return errors.New("rejected before admission") }, func() {
		if DetachCompanion(id) == nil {
			t.Fatal("stage unexpectedly accepted")
		}
	})
	if SnapshotCompanionDomain(c).Root != p || m.DetachCompanion(id) != nil {
		t.Fatal("staging failure changed state or leaked reservation")
	}
}

func TestCompanionDirectDomainChurn(t *testing.T) {
	m := companionFixture(t)
	p, c := testPair(t, m)
	cid := c.ID()
	var v int
	var wg sync.WaitGroup
	wg.Add(2)
	for _, e := range []IThreadSafeEntity{p, c} {
		go func() {
			defer wg.Done()
			for range 1200 {
				testSystemMutex(e).Lock()
				if SnapshotCompanionDomain(e).Root == p {
					v++
				}
				testSystemMutex(e).Unlock()
			}
		}()
	}
	for range 180 {
		if e := m.DetachCompanion(cid); e == nil {
			for {
				if e = m.AttachCompanion(cid); e == nil {
					break
				}
				if !errors.Is(e, ErrCompanionBusy) {
					t.Fatal(e)
				}
			}
		} else if !errors.Is(e, ErrCompanionBusy) {
			t.Fatal(e)
		}
	}
	wg.Wait()
	if v < 1200 {
		t.Fatal("lost protected writes", v)
	}
}
