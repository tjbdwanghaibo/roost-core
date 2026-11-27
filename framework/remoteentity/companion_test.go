package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/internal/entitypermit"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// Test the public dispatch boundary with a real batch, an in-memory Mongo authority
// and the existing Redis version-lock substitute. Do not synthesize marker leases.
const companionPlayerKind entity.EntityKind = 209
const companionActivityKind entity.EntityKind = 210

type companionPlayer struct{ *entity.EntityBase }

func (p *companionPlayer) Base() *entity.EntityBase { return p.EntityBase }

type companionActivity struct{ *testRemoteEntity }

type companionActivityDAO struct{ *testRemoteDao }

func (a *companionActivity) RangeDao(fn func(entity.DaoInterface)) { fn(companionActivityDAO{&a.dao}) }
func (d companionActivityDAO) CaptureRollback(tx *nest.RollbackTx) error {
	dirty := d.dirty.Dirty()
	tx.DeferRollback(func() error { d.dirty.set(dirty); return nil })
	return nil
}

var registerRemoteCompanionFixture = sync.OnceFunc(func() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{Kind: companionPlayerKind, Category: entity.EntityCategoryPlayer, NoPersist: true, Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
		return &companionPlayer{entity.NewEntityBaseWithMutex(p.Id, p.Category, true, p.Mutex, p.Kind)}, nil
	}})
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{Kind: companionActivityKind, Category: entity.EntityCategoryPlayerScoped, NoPersist: true, RemotePolicy: entity.RemotePolicyManaged, Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
		r := &testRemoteEntity{}
		r.RemoteEntityBase.EntityBase = *entity.NewEntityBaseWithMutex(p.Id, p.Category, true, p.Mutex, p.Kind)
		r.dao = testRemoteDao{id: p.Id, dirty: &r.dirty}
		return &companionActivity{r}, nil
	}})
	if err := entity.RegisterCompanionKind[*companionPlayer, *companionActivity](companionPlayerKind, companionActivityKind); err != nil {
		panic(err)
	}
})

func remoteCompanionFixture(t *testing.T) (sharedWriterNode, *companionPlayer, *companionActivity) {
	t.Helper()
	registerRemoteCompanionFixture()
	if entity.RemoteKindShared(companionActivityKind) {
		t.Fatal("Activity starts locally exclusive, not ownership=shared")
	}
	n := newWriterNodes(t, writerNodesSetup{db: newRemoteMongoFake(), database: "control", redis: &authorityLockRedis{unlockEvalStub: newUnlockEvalStub()}, kind: companionActivityKind}, 3411, nil, 1000)[0]
	p, err := n.manager.Create(&entity.EntityCreateParam{IsCreate: true, Kind: companionPlayerKind, UniqueID: 3411})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh remote child cannot attach on publication. Its declared, detached
	// instance needs a separate admitted Attach; neither its ID nor LocalOwned suffices.
	if _, err := n.manager.Create(&entity.EntityCreateParam{IsCreate: true, Kind: companionActivityKind, CompanionOf: p.ID()}); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("fresh composed remote child: %v", err)
	}
	a, err := n.manager.Create(&entity.EntityCreateParam{IsCreate: true, Kind: companionActivityKind, CompanionOf: p.ID(), CompanionDetached: true})
	if err != nil {
		t.Fatal(err)
	}
	return n, p.(*companionPlayer), a.(*companionActivity)
}

type companionBatchProof interface {
	AllowsExclusiveCompanionLocked(entity.IThreadSafeRemoteEntity) bool
	ConfirmEmptyCompanionLocked() bool
}

func TestCompanionLeaseIsExactActiveAndExclusive(t *testing.T) {
	n, p, a := remoteCompanionFixture(t)
	ctx := t.Context()
	id := a.ID()
	batch, err := n.mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close(ctx)
	proof, ok := batch.(companionBatchProof)
	if !ok {
		t.Fatal("real remote batch lacks companion admission proof")
	}
	mu := entity.SystemMutex(entitypermit.System(), a)
	mu.Lock()
	if !proof.AllowsExclusiveCompanionLocked(a) || proof.ConfirmEmptyCompanionLocked() {
		t.Fatal("expected active exclusive proof, not a finalized outcome")
	}
	mu.Unlock()
	// Another current local-owned object can have the very same ID and marker vector.
	// The batch confers authority only on the instance Manager actually admitted.
	other := entity.NewEntityManager()
	_, err = other.Create(&entity.EntityCreateParam{Kind: companionPlayerKind, Id: p.ID()})
	if err != nil {
		t.Fatal(err)
	}
	otherValue, err := other.Create(&entity.EntityCreateParam{Kind: companionActivityKind, Id: id, CompanionOf: p.ID()})
	if err != nil {
		t.Fatal(err)
	}
	foreign := otherValue.(*companionActivity)
	if err = foreign.SetRemoteVersionVector(a.RemoteVersionVector()); err != nil {
		t.Fatal(err)
	}
	if err = foreign.TransitionRemoteOwnership(entity.RemoteOwnershipLocalOwned); err != nil {
		t.Fatal(err)
	}
	foreignMu := entity.SystemMutex(entitypermit.System(), foreign)
	foreignMu.Lock()
	wrong := proof.AllowsExclusiveCompanionLocked(foreign)
	foreignMu.Unlock()
	if wrong {
		t.Fatal("batch authorized a different instance")
	}
	empty, err := n.mgr.PrepareRemoteWriteBatch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	undeclared := empty.(companionBatchProof).AllowsExclusiveCompanionLocked(a)
	mu.Unlock()
	if closeErr := empty.Close(ctx); closeErr != nil {
		t.Fatal(closeErr)
	}
	if undeclared {
		t.Fatal("undeclared LocalOwned member received a proof")
	}
	if err = n.manager.AttachCompanion(id); err == nil {
		t.Fatal("raw manager must not attach a remote entity")
	}
	mu.Lock()
	err = batch.FinalizeLocked(entity.NewRemoteTransactionOutcome(remoteTestTxID(195), "relation", "", true, 0))
	if err != nil {
		t.Fatal(err)
	}
	if proof.AllowsExclusiveCompanionLocked(a) || !proof.ConfirmEmptyCompanionLocked() || proof.ConfirmEmptyCompanionLocked() {
		t.Fatal("grant must end at finalization; empty confirmation must be single use")
	}
	mu.Unlock()
	// Even an empty confirmed batch pins the ownership gate until Close, not merely until return from the handler.
	blocked, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	_, err = n.mgr.EnterRemoteSharedMode(blocked, id)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transition before Close: %v", err)
	}
	if err = batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = n.mgr.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	shared, err := n.mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close(ctx)
	mu.Lock()
	ok = shared.(companionBatchProof).AllowsExclusiveCompanionLocked(a)
	mu.Unlock()
	if ok || a.RemoteOwnershipState() != entity.RemoteOwnershipShared {
		t.Fatal("shared batch cannot authorize exclusive companion getter")
	}
}

func TestCompanionRemoteActivityLifecycle(t *testing.T) {
	n, p, a := remoteCompanionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	id := a.ID()
	store := &companionTransferBarrier{MongoCommitter: n.loader.store, entered: make(chan struct{}), resume: make(chan struct{})}
	n.mgr.SetOwnershipStore(store)
	engine := nest.NewEngine(nest.NestOptionWithGetter(n.access), nest.NestOptionWithRemoteEntityManager(n.mgr), nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 2, QueueCap: 16}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	meta := nest.HandlerMeta{Rollback: nest.RollbackState, Durability: nest.DurabilityMemory}
	attach := nest.NewHandlerName("companion_remote_attach")
	detach := nest.NewHandlerName("companion_remote_detach")
	mixed := nest.NewHandlerName("companion_remote_mixed_change")
	engine.MustRegisterHandlerWithMeta(mixed, func(_ []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		v, err := entity.GetCompanion[*companionActivity]()
		if err != nil {
			return nil, err
		}
		v.dirty.set(true)
		return nil, entity.DetachCompanion(id)
	}, meta)
	principal := nest.NewHandlerName("companion_remote_principal")
	engine.MustRegisterHandlerWithMeta(principal, func(_ []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		value, err := entity.GetPrincipal[*companionPlayer]()
		if err != nil {
			return nil, err
		}
		if value != p || !nest.CurrentRemoteWriteBatchContains(id) {
			return nil, errors.New("wrong principal or no activity grant")
		}
		return nil, nil
	}, meta)
	write := nest.NewHandlerName("companion_remote_write")
	multi := nest.NewHandlerName("companion_remote_multi")
	engine.MustRegisterHandlerWithMeta(attach, func(_ []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		return nil, entity.AttachCompanion(id)
	}, meta)
	engine.MustRegisterHandlerWithMeta(detach, func(_ []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		return nil, entity.DetachCompanion(id)
	}, meta)
	engine.MustRegisterHandlerWithMeta(write, func(_ []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		v, e := entity.GetCompanion[*companionActivity]()
		if e != nil {
			return nil, e
		}
		if v != a || !nest.CurrentRemoteWriteBatchContains(id) {
			return nil, errors.New("not the admitted Activity")
		}
		v.dirty.set(true)
		return nil, nil
	}, meta)
	engine.MustRegisterHandlerWithMeta(multi, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		if len(es) != 2 || es[0] != p || es[1] != a || !nest.CurrentRemoteWriteBatchContains(id) {
			return nil, fmt.Errorf("wrong explicit multi targets: %v", es)
		}
		if v, e := entity.GetCompanion[*companionActivity](); v != nil || e == nil {
			return nil, errors.New("shared implicit getter should be closed")
		}
		a.dirty.set(true)
		return nil, nil
	}, meta)
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Shutdown(context.Background()) })
	request := func(h nest.HandlerName, target int64) error {
		_, e := engine.Request(ctx, h, target, nest.NewParams())
		return e
	}
	if err := request(write, p.ID()); err == nil {
		t.Fatal("detached getter before attach")
	}
	if err := n.manager.AttachCompanion(id); err == nil {
		t.Fatal("manager granted unproved attach")
	}
	if err := request(attach, id); err != nil {
		t.Fatalf("exclusive attach via handler: %v", err)
	}
	if err := request(principal, id); err != nil {
		t.Fatalf("exclusive child to principal: %v", err)
	}
	if err := request(write, p.ID()); err != nil {
		t.Fatalf("exclusive companion write: %v", err)
	}
	if a.EntityVersion() != 1 {
		t.Fatalf("exclusive version=%d", a.EntityVersion())
	}
	if err := request(mixed, p.ID()); !errors.Is(err, nest.ErrCompanionChangeUnsupported) {
		t.Fatalf("remote persistence plus detach: %v", err)
	}
	if a.dirty.Dirty() || a.EntityVersion() != 1 || entity.SnapshotCompanionDomain(a).Root != p {
		t.Fatal("rejected mixed change left partial state")
	}
	if err := n.manager.DetachCompanion(id); err == nil {
		t.Fatal("raw manager detached a remote member")
	}
	before, ok, err := store.ReadAuthorityState(ctx, id)
	if err != nil || !ok {
		t.Fatalf("missing committed authority: %v, %v", ok, err)
	}
	for name, op := range map[string]func() (entity.RemoteEntityMarkerLease, error){"share": func() (entity.RemoteEntityMarkerLease, error) { return n.mgr.EnterRemoteSharedMode(ctx, id) }, "transfer": func() (entity.RemoteEntityMarkerLease, error) { return n.mgr.TransferRemoteOwnership(ctx, id, 2000) }} {
		if _, err := op(); !errors.Is(err, entity.ErrRemoteOwnerTransition) {
			t.Fatalf("%s while attached: %v", name, err)
		}
	}
	after, found, err := store.ReadAuthorityState(ctx, id)
	if err != nil || !found || after.Ownership != before.Ownership || after.Version != 1 {
		t.Fatalf("refused conversions changed authoritative state: %+v, %v", after, err)
	}
	if err := request(detach, id); err != nil {
		t.Fatalf("exclusive detach: %v", err)
	}
	if err := request(write, p.ID()); err == nil {
		t.Fatal("detached getter before sharing")
	}
	if _, err := n.mgr.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := request(attach, id); err == nil {
		t.Fatal("shared attach")
	}
	if err := request(write, p.ID()); err == nil {
		t.Fatal("implicit shared write")
	}
	if _, err := engine.RequestMulti(ctx, multi, []int64{p.ID(), id}, nest.NewParams()); err != nil {
		t.Fatalf("explicit shared multi write: %v", err)
	}
	if a.EntityVersion() != 2 {
		t.Fatalf("shared version=%d", a.EntityVersion())
	}
	if _, err := n.mgr.LeaveRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := request(write, p.ID()); err == nil {
		t.Fatal("auto-attached on leaving shared")
	}
	if err := request(attach, id); err != nil {
		t.Fatalf("exclusive reattach: %v", err)
	}
	if err := request(write, p.ID()); err != nil {
		t.Fatalf("composed write after reattach: %v", err)
	}
	if a.EntityVersion() != 3 {
		t.Fatalf("reattached version=%d", a.EntityVersion())
	}
	if err := request(detach, id); err != nil {
		t.Fatal(err)
	}
	// Pause the CAS, not the business handler: the transferring manager must
	// withhold new grants until the authority has answered and the gate is released.
	resume := sync.OnceFunc(func() { close(store.resume) })
	defer resume()
	done := make(chan error, 1)
	go func() { _, err := n.mgr.TransferRemoteOwnership(ctx, id, 2000); done <- err }()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if a.RemoteOwnershipState() != entity.RemoteOwnershipDraining {
		t.Fatal("expected draining before authority CAS")
	}
	waiting, stop := context.WithTimeout(ctx, 40*time.Millisecond)
	pending, err := n.mgr.PrepareRemoteWriteBatch(waiting, []int64{id})
	stop()
	if pending != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new write while transfer pending: %v %v", pending, err)
	}
	if err = request(write, p.ID()); !errors.Is(err, entity.ErrCompanionDetached) {
		t.Fatalf("principal getter while transfer pending: %v", err)
	}
	resume()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = request(attach, id); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("reattach by former owner after transfer: %v", err)
	}
}

type companionTransferBarrier struct {
	*MongoCommitter
	entered, resume chan struct{}
}

func (s *companionTransferBarrier) TransferExpected(ctx context.Context, id int64, expected entity.RemoteEntityMarkerLease, sid int32) (entity.RemoteEntityMarkerLease, error) {
	close(s.entered)
	select {
	case <-s.resume:
		return s.MongoCommitter.TransferExpected(ctx, id, expected, sid)
	case <-ctx.Done():
		return entity.RemoteEntityMarkerLease{}, ctx.Err()
	}
}

func TestCompanionLeaseDoesNotFollowReload(t *testing.T) {
	n, p, old := remoteCompanionFixture(t)
	ctx := t.Context()
	id := old.ID()
	if !old.Touch() {
		t.Fatal("no current activity")
	}
	defer old.UnTouch()
	b, err := n.mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	if err = n.access.UnloadRemoteEntity(ctx, old); err != nil {
		t.Fatal(err)
	}
	next, err := n.manager.Create(&entity.EntityCreateParam{Kind: companionActivityKind, Id: id, CompanionOf: p.ID()})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []entity.IThreadSafeRemoteEntity{old, next.(*companionActivity)} {
		mu := entity.SystemMutex(entitypermit.System(), e)
		mu.Lock()
		ok := b.(companionBatchProof).AllowsExclusiveCompanionLocked(e)
		mu.Unlock()
		if ok {
			t.Fatal("retired batch must authorize neither removed instance nor replacement")
		}
	}
}
