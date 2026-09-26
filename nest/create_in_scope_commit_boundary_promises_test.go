package nest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
)

// RR-20260926-35：handler 内 CreateInScope 新建的实体属于当前事务。
// 提交确认前 Sync 不外发；回滚或拒绝时从 EntityManager 撤销、不持久、订阅者不收到它。

const createdInScopeKind entity.EntityKind = 241

func createdInScopePacker(e entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.CopyFrozenSyncPayload(1, []byte{byte(e.(*rollbackTestEntity).dao.Value)}), nil
	}
	return entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}
}

// createdInScopeBuildHook 让个别用例在新实例构造时插入同步点（RR-20260926-57 用它确定 GetOrCreate 的重试时机）。
var createdInScopeBuildHook atomic.Pointer[func()]

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: entity.EntityCategory(1),
		Kind:     createdInScopeKind,
		Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			if hook := createdInScopeBuildHook.Load(); hook != nil {
				(*hook)()
			}
			return &rollbackTestEntity{
				EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind),
				dao:        &rollbackTestDao{id: p.Id},
			}, nil
		},
		Sync: entity.EntitySyncBuilderParam{Enabled: true, Namespace: "test", PackerFactory: createdInScopePacker},
	})
}

// createdSubjects 解出一帧里每个 subject 携带的首字节内容；ObjectRemove 不带内容，不出现在结果里。
func createdSubjects(t *testing.T, raw []byte) map[int64]byte {
	t.Helper()
	f, err := entitysync.DecodeFrame(raw, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]byte{}
	for _, o := range f.Objects {
		for _, c := range o.Components {
			u, err := entitysync.DecodeSubjectUpdate(c.Data, 0)
			if err != nil {
				t.Fatal(err)
			}
			if b := u.Payload.BytesCopy(); len(b) > 0 {
				out[u.SubjectID] = b[0]
			}
		}
	}
	return out
}

// drainSubjects 收集已经交付的全部帧。Transport 在 Flush 内同步调用，Flush 返回后不会再有迟到的帧。
func drainSubjects(t *testing.T, frames chan []byte) map[int64]byte {
	t.Helper()
	out := map[int64]byte{}
	for {
		select {
		case raw := <-frames:
			for id, v := range createdSubjects(t, raw) {
				out[id] = v
			}
		default:
			return out
		}
	}
}

func newCreateInScopeSync(t *testing.T, watermark func() uint64) (*entitysync.Manager, chan []byte) {
	t.Helper()
	frames := make(chan []byte, 64)
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, DurableWatermark: watermark, Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, p []byte) error {
		frames <- append([]byte(nil), p...)
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, frames
}

type createInScopeFixture struct {
	manager   *entity.EntityManager
	access    *entity.ManagerAccess
	sync      *entitysync.Manager
	frames    chan []byte
	existing  *rollbackTestEntity
	existID   int64
	createdID int64
}

func newCreateInScopeFixture(t *testing.T, unique int64, watermark func() uint64) *createInScopeFixture {
	t.Helper()
	manager := entity.NewEntityManager()
	existID, existing := newAsyncPilotEntity(t, unique, 10)
	if err := manager.TryAdd(existing); err != nil {
		t.Fatal(err)
	}
	m, frames := newCreateInScopeSync(t, watermark)
	return &createInScopeFixture{
		manager: manager, access: entity.NewManagerAccess(manager), sync: m, frames: frames,
		existing: existing, existID: existID,
		createdID: mustBuildCastID(t, unique+1, entity.EntityCategory(1), createdInScopeKind),
	}
}

// createAndPublish 是业务在 handler 里的正式写法：用当前 Guard 作用域创建、登记同步并订阅，再改已有实体。
func (f *createInScopeFixture) createAndPublish(value int, lateSync bool) (*rollbackTestEntity, error) {
	param := &entity.EntityCreateParam{IsCreate: true, Id: f.createdID, Category: entity.EntityCategory(1), Kind: createdInScopeKind}
	if lateSync {
		param.Sync = &entity.EntitySyncCreateParam{Enabled: false}
	}
	value0, err := f.access.CreateInScope(entity.CurrentGuardScope(), param)
	if err != nil {
		return nil, err
	}
	created := value0.(*rollbackTestEntity)
	created.dao.Value = value
	if lateSync {
		created.EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: created.ID(), Namespace: "test", Packer: createdInScopePacker(created)})
	}
	if err := f.sync.Register(created.Sync()); err != nil {
		return nil, err
	}
	if err := f.sync.Subscribe(1, created.ID(), entity.SyncProfile{}); err != nil {
		return nil, err
	}
	f.existing.dao.Value = 11
	if err := MarkPersist(f.existing.dao, 1); err != nil {
		return nil, err
	}
	return created, MarkPersist(created.dao, 1)
}

// 场景一：pipelined 且未接水位。锁释放后、ticket 持久前，新实体与已有实体同样被提交屏障挡住。
func TestCreateInScopePipelinedWithoutWatermarkWaitsForDurable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lateSync bool
		unique   int64
	}{{"sync_at_build", false, 9700}, {"sync_enabled_after_create", true, 9710}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCreateInScopeFixture(t, tc.unique, nil)
			committer := newPipelinedTestCommitter(false)
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			name := NewHandlerName("create_in_scope_pipelined_" + tc.name)
			var created *rollbackTestEntity
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				var err error
				created, err = f.createAndPublish(77, tc.lateSync)
				return "ok", err
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityPipelined})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			defer committer.resolveAll(nil) // 失败提前返回时先放行 ticket，Shutdown 才能排空
			msg, ch := GenSyncMsg(MsgTypeSingle)
			msg.Name, msg.Tid = name.String(), f.existID
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			select {
			case <-committer.enqueued:
			case <-time.After(2 * time.Second):
				t.Fatal("transaction was not enqueued")
			}
			// 取得新实体的锁即证明 handler 已返回、准入后提前释放已经完成；此后只剩 ticket 持久化。
			mu := created.GetMutex()
			mu.Lock()
			mu.Unlock()
			if err := f.sync.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if early := drainSubjects(t, f.frames); len(early) != 0 {
				t.Errorf("content left before the commit ticket was durable (durableLSN=%d): %v (created=%d)", committer.DurableLSN(), early, f.createdID)
			}
			if created.Sync().SyncCommitReady() {
				t.Fatalf("created entity is outside the commit barrier before its ticket is durable (durableLSN=%d)", committer.DurableLSN())
			}
			committer.mu.Lock()
			persisted := false
			for _, mutation := range committer.records[0].Mutations {
				persisted = persisted || mutation.Key.ID == f.createdID
			}
			committer.mu.Unlock()
			if !persisted {
				t.Fatal("created entity is missing from the WAL record")
			}
			committer.resolveAll(nil)
			if got := stagedWait(t, ch); got != "ok" {
				t.Fatalf("reply=%v", got)
			}
			if !created.Sync().SyncCommitReady() {
				t.Fatal("durable transaction left the created entity behind its commit barrier")
			}
			if err := f.sync.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := drainSubjects(t, f.frames); got[f.createdID] != 77 {
				t.Fatalf("created entity was not replicated after the durable commit: %v", got)
			}
			if f.manager.Get(f.createdID) != created {
				t.Fatal("committed entity is no longer published")
			}
		})
	}
}

// 场景二：handler 报错回滚。新实体从 EntityManager 撤销、不产生持久化记录、订阅者看不到它，锁与 ID 可复用。
func TestCreateInScopeRevokedWhenHandlerFails(t *testing.T) {
	f := newCreateInScopeFixture(t, 9720, nil)
	committer := &recordingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := NewHandlerName("create_in_scope_handler_fails")
	boom := errors.New("business rejected after create")
	var created *rollbackTestEntity
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		var err error
		if created, err = f.createAndPublish(5, false); err != nil {
			return nil, err
		}
		f.existing.dao.Value = 99
		return nil, boom
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	if _, err := mgr.Request(context.Background(), name, f.existID, nil); !errors.Is(err, boom) {
		t.Fatalf("reply err=%v", err)
	}
	if f.existing.dao.Value != 10 {
		t.Fatalf("existing entity not rolled back: %d", f.existing.dao.Value)
	}
	if f.manager.Get(f.createdID) != nil {
		t.Fatal("entity created inside a rolled-back handler is still published in the EntityManager")
	}
	if !created.IsRemoved() {
		t.Fatal("revoked entity is not marked removed; a reader holding the pointer could still lock it")
	}
	if len(committer.record.Mutations) != 0 {
		t.Fatalf("rolled-back transaction reached the committer: %+v", committer.record.Mutations)
	}
	if err := f.sync.Flush(context.Background()); err != nil {
		t.Fatalf("flush after revocation: %v", err)
	}
	if got := drainSubjects(t, f.frames); len(got) != 0 {
		t.Fatalf("revoked entity was replicated: %v", got)
	}
	if subs := f.sync.Subscribers(f.createdID); len(subs) != 0 {
		t.Fatalf("revoked subject still has subscribers %v", subs)
	}
	// 撤销后同一 ID 可以重新创建（removing 标记与 LockManager 条目已在释放后回收）。
	again, err := f.manager.Create(&entity.EntityCreateParam{IsCreate: true, Id: f.createdID, Category: entity.EntityCategory(1), Kind: createdInScopeKind})
	if err != nil {
		t.Fatalf("id of a revoked entity cannot be reused: %v", err)
	}
	if again == entity.IThreadSafeEntity(created) {
		t.Fatal("revoked instance was republished")
	}
}

// 场景三：strict 提交被拒（接了水位，handler 持锁期间有并发 tick）。拒绝后新实体不外发、不留在内存。
func TestCreateInScopeRevokedWhenStrictCommitRejected(t *testing.T) {
	f := newCreateInScopeFixture(t, 9730, func() uint64 { return 0 })
	committer := &recordingCommitter{err: errors.New("commit rejected")}
	mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := NewHandlerName("create_in_scope_strict_rejected")
	concurrent := make(chan error, 1)
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if _, err := f.createAndPublish(55, false); err != nil {
			return nil, err
		}
		// 并发 tick：它要么在锁外等待新实体的锁，要么晚于撤销运行，两种顺序都不能发出新实体。
		go func() { concurrent <- f.sync.Flush(context.Background()) }()
		return "ok", nil
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	if _, err := mgr.Request(context.Background(), name, f.existID, nil); !errors.Is(err, ErrCommitRejected) {
		t.Fatalf("reply err=%v", err)
	}
	if err := <-concurrent; err != nil {
		t.Fatalf("concurrent flush: %v", err)
	}
	if err := f.sync.Flush(context.Background()); err != nil {
		t.Fatalf("flush after rejection: %v", err)
	}
	if got := drainSubjects(t, f.frames); len(got) != 0 {
		t.Fatalf("entity from a rejected transaction was replicated: %v (created=%d)", got, f.createdID)
	}
	if f.manager.Get(f.createdID) != nil {
		t.Fatal("entity from a rejected transaction is still published")
	}
	if f.existing.dao.Value != 10 {
		t.Fatalf("existing entity not rolled back: %d", f.existing.dao.Value)
	}
}

// 成功的 strict 提交：新实体随事务持久化，释放并确认后正常同步。
func TestCreateInScopeCommittedEntitySyncsAfterCommit(t *testing.T) {
	f := newCreateInScopeFixture(t, 9740, nil)
	committer := &recordingCommitter{}
	mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
	name := NewHandlerName("create_in_scope_committed")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		_, err := f.createAndPublish(42, false)
		return "ok", err
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())
	if _, err := mgr.Request(context.Background(), name, f.existID, nil); err != nil {
		t.Fatal(err)
	}
	persisted := false
	for _, mutation := range committer.record.Mutations {
		persisted = persisted || mutation.Key.ID == f.createdID
	}
	if !persisted {
		t.Fatalf("created entity missing from the commit record: %+v", committer.record.Mutations)
	}
	created := f.manager.Get(f.createdID)
	if created == nil || !created.Base().Sync().SyncCommitReady() {
		t.Fatal("committed entity is not published or still behind the commit barrier")
	}
	if err := f.sync.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := drainSubjects(t, f.frames); got[f.createdID] != 42 {
		t.Fatalf("committed entity was not replicated: %v", got)
	}
}

// 事务外创建行为不变：Create 与独立 Guard 作用域里的 CreateInScope 立即发布、立即可同步。
func TestCreateOutsideTransactionUnchanged(t *testing.T) {
	f := newCreateInScopeFixture(t, 9750, nil)
	viaCreate, err := f.manager.Create(&entity.EntityCreateParam{IsCreate: true, Id: f.createdID, Category: entity.EntityCategory(1), Kind: createdInScopeKind})
	if err != nil {
		t.Fatal(err)
	}
	scopedID := mustBuildCastID(t, 9752, entity.EntityCategory(1), createdInScopeKind)
	var viaScope entity.IThreadSafeEntity
	if err := entity.WithGuardScope("outside_nest", func(scope *entity.GuardScope) error {
		viaScope, err = f.manager.CreateInScope(scope, &entity.EntityCreateParam{IsCreate: true, Id: scopedID, Category: entity.EntityCategory(1), Kind: createdInScopeKind})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []entity.IThreadSafeEntity{viaCreate, viaScope} {
		e.(*rollbackTestEntity).dao.Value = 7
		if f.manager.Get(e.ID()) != e || !e.Base().Sync().SyncCommitReady() {
			t.Fatalf("entity %d created outside a transaction is not immediately published", e.ID())
		}
		if err := f.sync.Register(e.Base().Sync()); err != nil {
			t.Fatal(err)
		}
		if err := f.sync.Subscribe(1, e.ID(), entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.sync.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := drainSubjects(t, f.frames); got[viaCreate.ID()] != 7 || got[viaScope.ID()] != 7 {
		t.Fatalf("entities created outside a transaction were not replicated: %v", got)
	}
}
