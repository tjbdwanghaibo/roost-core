package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

func syncTestManager(t *testing.T, e *rollbackTestEntity, mode entitysync.SyncMode) (*entitysync.Manager, chan []byte, *atomic.Int32) {
	t.Helper()
	packs := new(atomic.Int32)
	packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		packs.Add(1)
		return entity.CopyFrozenSyncPayload(1, []byte{byte(e.dao.Value)}), nil
	}
	e.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: e.ID(), Namespace: "test", Packer: entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}})
	frames := make(chan []byte, 32)
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: mode, Interval: time.Hour, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		frames <- append([]byte(nil), p...)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	m.BindSyncProducer()
	if err = m.Register(e.Sync()); err != nil {
		t.Fatal(err)
	}
	if err = m.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err = m.Subscribe(1, e.ID(), entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err = m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-frames
	packs.Store(0)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, frames, packs
}
func syncValue(t *testing.T, raw []byte) byte {
	t.Helper()
	f, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range f.Objects {
		for _, c := range o.Components {
			u, err := entitysync.DecodeSubjectUpdate(c.Data, 0)
			if err != nil {
				t.Fatal(err)
			}
			return u.Payload.BytesCopy()[0]
		}
	}
	t.Fatal("missing update")
	return 0
}

func TestNestSyncModesShareCommitAndUnlockBoundaries(t *testing.T) {
	for _, mode := range []entitysync.SyncMode{entitysync.ModePeriodic, entitysync.ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			_, e := newAsyncPilotEntity(t, 9900, 10)
			m, frames, packs := syncTestManager(t, e, mode)
			scope, closeScope := entity.NewGuardScope("sync-modes")
			defer closeScope()
			scope.Guard().RequireEntity(e)
			_, err := invokeWithTransaction(HandlerMeta{}, []entity.IThreadSafeEntity{e}, nil, "write", nil, nil, func() (any, error) { e.dao.Value = 11; e.MarkSyncDirty(1); return nil, nil }, m)
			if err != nil {
				t.Fatal(err)
			}
			if mode == entitysync.ModeOnChange && packs.Load() != 1 {
				t.Fatalf("locked capture count %d", packs.Load())
			}
			if mode == entitysync.ModePeriodic && packs.Load() != 0 {
				t.Fatal("periodic captured in handler")
			}
			if err = m.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-frames:
				t.Fatal("sent before all locks released")
			default:
			}
			scope.Guard().ReleaseAll()
			if err = m.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := syncValue(t, <-frames); got != 11 {
				t.Fatalf("value %d", got)
			}
			if packs.Load() != 1 {
				t.Fatalf("duplicate packing %d", packs.Load())
			}
		})
	}
}

func TestNestOnChangeRunsWithoutWaitingForInterval(t *testing.T) {
	id, e := newAsyncPilotEntity(t, 9901, 10)
	m, frames, _ := syncTestManager(t, e, entitysync.ModeOnChange)
	getter := newMockGetter()
	getter.Add(e)
	name := NewHandlerName("sync_modes_live")
	MustRegisterMemoryHandler(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		e.dao.Value++
		e.MarkSyncDirty(1)
		return nil, nil
	})
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithEntitySync(m), NestOptionWithWorkerNumAndMsgCap(1, 1, 8))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	if _, err := engine.Request(context.Background(), name, id, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-frames:
		if got := syncValue(t, raw); got != 11 {
			t.Fatalf("value %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waited for one-hour interval")
	}
}

func TestNestSyncRollbackPreservesOlderPending(t *testing.T) {
	_, closeContext := fctx.NewContext()
	defer closeContext()
	_, e := newAsyncPilotEntity(t, 9902, 10)
	m, frames, _ := syncTestManager(t, e, entitysync.ModeOnChange)
	e.MarkSyncDirty(2)
	scope, closeScope := entity.NewGuardScope("sync-rollback")
	defer closeScope()
	scope.Guard().RequireEntity(e)
	rejection := errors.New("reject")
	_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo}, []entity.IThreadSafeEntity{e}, nil, "reject", nil, nil, func() (any, error) {
		CurrentRollbackTx().DeferRollback(func() error { e.dao.Value = 10; return nil })
		e.dao.Value = 99
		e.MarkSyncDirty(1)
		return nil, rejection
	}, m)
	if !errors.Is(err, rejection) {
		t.Fatal(err)
	}
	scope.Guard().ReleaseAll()
	if err = m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := syncValue(t, <-frames); got != 10 {
		t.Fatalf("rollback leaked value %d", got)
	}
}

func TestNestPipelinedSyncWaitsForConfirmationAndUnlock(t *testing.T) {
	_, closeContext := fctx.NewContext()
	defer closeContext()
	_, e := newAsyncPilotEntity(t, 9903, 10)
	m, frames, packs := syncTestManager(t, e, entitysync.ModeOnChange)
	scope, closeScope := entity.NewGuardScope("sync-pipelined")
	defer closeScope()
	scope.Guard().RequireEntity(e)
	committer := newPipelinedTestCommitter(false)
	_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}, []entity.IThreadSafeEntity{e}, committer, "pipeline", func() {
		if packs.Load() != 1 {
			t.Errorf("capture did not run under lock")
		}
		scope.Guard().ReleaseAll()
		if err := m.Flush(context.Background()); err != nil {
			t.Error(err)
		}
		select {
		case <-frames:
			t.Error("sent before ticket confirmation")
		default:
		}
		committer.resolveAll(nil)
	}, nil, func() (any, error) { e.dao.Value++; e.MarkSyncDirty(1); return nil, MarkPersist(e.dao, 1) }, m)
	if err != nil {
		t.Fatal(err)
	}
	scope.Guard().ReleaseAll() // drains post-commit callbacks on the inline path
	if err = m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-frames:
		if got := syncValue(t, raw); got != 11 {
			t.Fatalf("value %d", got)
		}
	default:
		t.Fatal("confirmed value missing")
	}
}

func TestNestSyncRejectedCommitDoesNotFreezeOrPublish(t *testing.T) {
	_, endContext := fctx.NewContext()
	defer endContext()
	_, e := newAsyncPilotEntity(t, 9904, 10)
	m, frames, packs := syncTestManager(t, e, entitysync.ModeOnChange)
	scope, endScope := entity.NewGuardScope("rejected-sync")
	defer endScope()
	scope.Guard().RequireEntity(e)
	rejection := errors.New("wal refused")
	committer := &recordingCommitter{err: rejection}
	_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}, []entity.IThreadSafeEntity{e}, committer, "reject", nil, nil, func() (any, error) {
		CurrentRollbackTx().DeferRollback(func() error { e.dao.Value = 10; return nil })
		e.dao.Value = 99
		e.MarkSyncDirty(1)
		return nil, MarkPersist(e.dao, 1)
	}, m)
	if err == nil {
		t.Fatal("rejection lost")
	}
	scope.Guard().ReleaseAll()
	if err = m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if packs.Load() != 0 || e.Sync().PendingDirty() {
		t.Fatal("rejected change captured")
	}
	select {
	case <-frames:
		t.Fatal("rejected change delivered")
	default:
	}
}

func TestNestSyncTracksCastEntityUntilAdmission(t *testing.T) {
	id, a := newAsyncPilotEntity(t, 9905, 10)
	_, b := newAsyncPilotEntity(t, 9906, 20)
	bid := mustBuildCastID(t, 9906, castAllianceCategory, castAllianceKind)
	b.EntityBase = entity.NewEntityBase(bid, castAllianceCategory, false, castAllianceKind)
	m, frames, _ := syncTestManager(t, a, entitysync.ModeOnChange)
	b.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: bid, Packer: entity.SubjectSyncPackFunc{Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.CopyFrozenSyncPayload(1, []byte{byte(b.dao.Value)}), nil
	}, Delta: func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) {
		return entity.CopyFrozenSyncPayload(1, []byte{byte(b.dao.Value)}), nil
	}}})
	if err := m.Register(b.Sync()); err != nil {
		t.Fatal(err)
	}
	if err := m.Subscribe(1, bid, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-frames
	getter := newMockGetter()
	getter.Add(a)
	getter.Add(b)
	name := NewHandlerName("sync_modes_cast")
	MustRegisterMemoryHandler(name, func(_ []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		values, err := CastMulti(CastTarget{ID: bid})
		if err != nil {
			return nil, err
		}
		b.dao.Value = 21
		values[0].Base().MarkSyncDirty(1)
		ReleaseCast(values[0])
		if !entity.CurrentGuardScope().Guard().Guarded(bid) {
			return nil, errors.New("cast released before admission")
		}
		return nil, nil
	})
	engine := NewEngine(NestOptionWithGetter(getter), NestOptionWithEntitySync(m))
	if err := engine.Start(); err != nil {
		t.Fatal(err)
	}
	defer engine.Shutdown(context.Background())
	if _, err := engine.Request(context.Background(), name, id, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-frames:
		if got := syncValue(t, raw); got != 21 {
			t.Fatalf("cast value %d", got)
		}
	default:
		t.Fatal("cast change missing")
	}
}

func TestSyncRemoteConfirmationIsSeparateFromLocalCommit(t *testing.T) {
	_, endContext := fctx.NewContext()
	defer endContext()
	_, e := newAsyncPilotEntity(t, 9907, 10)
	m, frames, _ := syncTestManager(t, e, entitysync.ModeOnChange)
	scope, endScope := entity.NewGuardScope("remote-sync")
	defer endScope()
	scope.Guard().RequireEntity(e)
	batch := entity.BeginSyncMutation([]entity.IThreadSafeEntity{e}, m)
	defer batch.Finish(false)
	// 本测试直接模拟“本地事务已持久提交”；RR-20260926-32 起这个事实由提交路径显式记录
	// （remoteCommitted），finishRemoteWriteBatch 不再从 nil 错误推断已提交。
	msg := &Msg{RemoteWriteBatch: &remoteBatchIntegrationFake{}, remoteFinalized: true, remoteCommitted: true}
	pop := pushCurrentNestDispatchMsg(msg)
	defer pop()
	tx := NewRollbackTx(RollbackNone)
	tx.syncMutation = batch
	tx.AfterCommit(batch.Confirm)
	e.dao.Value = 11
	e.MarkSyncDirty(1)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	scope.Guard().ReleaseAll()
	batch.Release()
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames:
		t.Fatal("local commit bypassed remote acknowledgement")
	default:
	}
	if err := msg.finishRemoteWriteBatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-frames:
		if got := syncValue(t, raw); got != 11 {
			t.Fatalf("remote value %d", got)
		}
	default:
		t.Fatal("remote confirmation did not release sync")
	}
}

func TestPipelinedSyncReleasesDynamicLocksBeforeDurableWait(t *testing.T) {
	_, endContext := fctx.NewContext()
	defer endContext()
	_, a := newAsyncPilotEntity(t, 9908, 10)
	_, b := newAsyncPilotEntity(t, 9909, 20)
	m, _, _ := syncTestManager(t, a, entitysync.ModeOnChange)
	b.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: b.ID(), Packer: entity.SubjectSyncPackFunc{}})
	scope, endScope := entity.NewGuardScope("pipeline-dynamic-locks")
	defer endScope()
	scope.Guard().RequireEntity(a)
	committer := newPipelinedTestCommitter(false)
	unlocked := make(chan struct{})
	_, err := invokeWithTransaction(HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}, []entity.IThreadSafeEntity{a}, committer, "dynamic-locks", func() {
		scope.Guard().ReleaseEntity(a.ID())
		// 只有另一个 goroutine 确实取得 Cast 的锁后，才允许 WAL 确认。
		go func() { b.GetMutex().Lock(); b.GetMutex().Unlock(); close(unlocked); committer.resolveAll(nil) }()
	}, nil, func() (any, error) {
		scope.Guard().RequireEntity(b)
		entity.CurrentSyncMutation().Include([]entity.IThreadSafeEntity{b})
		a.dao.Value++
		a.MarkSyncDirty(1)
		return nil, MarkPersist(a.dao, 1)
	}, m)
	if err != nil {
		t.Fatal(err)
	}
	<-unlocked
	if b.LastCommitLSN() != 1 {
		t.Fatalf("dynamic LSN=%d", b.LastCommitLSN())
	}
	scope.Guard().ReleaseAll()
}
