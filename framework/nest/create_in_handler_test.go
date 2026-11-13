// handler 内新建实体（CreateInScope 与生成 Lifecycle 两个入口）随事务提交才外发，回滚 / 拒绝时撤销。

package nest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
)

// handler 内 CreateInScope 新建的实体属于当前事务：提交确认前 Sync 不外发；回滚或拒绝时从 EntityManager 撤销、
// 不持久、订阅者不收到它。
// 历史：RR-20260926-35。

const createdInScopeKind entity.EntityKind = 241

func createdInScopePacker(e entity.IThreadSafeEntity) entity.SubjectSyncPacker {
	packed := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		return entity.CopyFrozenSyncPayload(1, []byte{byte(e.(*rollbackTestEntity).dao.Value)}), nil
	}
	return entity.SubjectSyncPackFunc{Snapshot: packed, Delta: func(p entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return packed(p) }}
}

// createdInScopeBuildHook 让个别用例在新实例构造时插入同步点（确定 GetOrCreate 的重试时机）。
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
	out := map[int64]byte{}
	for _, o := range received(t, raw).Objects {
		for _, c := range o.Components {
			u := c.Update
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
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Mode: entitysync.ModePeriodic, Interval: time.Hour, DurableWatermark: watermark, Transport: decodingTransport(frames)})
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
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
	mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
	mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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
	mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
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

var errGeneratedShapeNotFound = errors.New("created_in_scope lifecycle: Entity not found")

// generatedShapeLifecycle 与 codegen/internal/roost/add_workflow.go 生成的 <Entity>Lifecycle 的
// Get / Create / GetOrCreate 逐行同形（持有 *entity.ManagerAccess，按 UniqueID 创建）。
type generatedShapeLifecycle struct {
	access *entity.ManagerAccess
}

func (lifecycle *generatedShapeLifecycle) Get(ctx context.Context, uniqueID int64) (*rollbackTestEntity, error) {
	fullID, err := entity.BuildEntityID(uniqueID, createdInScopeKind)
	if err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Get(ctx, fullID, entity.MustEntityCategoryOfKind(createdInScopeKind))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errGeneratedShapeNotFound
	}
	typed, ok := value.(*rollbackTestEntity)
	if !ok {
		return nil, fmt.Errorf("created_in_scope lifecycle: Entity %d has type %T", fullID, value)
	}
	return typed, nil
}

func (lifecycle *generatedShapeLifecycle) Create(ctx context.Context, uniqueID int64) (*rollbackTestEntity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := lifecycle.access.Create(&entity.EntityCreateParam{
		IsCreate: true,
		UniqueID: uniqueID,
		Kind:     createdInScopeKind,
		Category: entity.MustEntityCategoryOfKind(createdInScopeKind),
	})
	if err != nil {
		return nil, err
	}
	typed, ok := value.(*rollbackTestEntity)
	if !ok {
		return nil, fmt.Errorf("created_in_scope lifecycle: created Entity has type %T", value)
	}
	return typed, nil
}

func (lifecycle *generatedShapeLifecycle) GetOrCreate(ctx context.Context, uniqueID int64) (*rollbackTestEntity, bool, error) {
	for attempt := 1; ; attempt++ {
		value, err := lifecycle.Get(ctx, uniqueID)
		if err == nil {
			return value, false, nil
		}
		if !errors.Is(err, errGeneratedShapeNotFound) {
			return nil, false, err
		}
		value, err = lifecycle.Create(ctx, uniqueID)
		if errors.Is(err, entity.ErrEntityExists) {
			value, err = lifecycle.Get(ctx, uniqueID)
			return value, false, err
		}
		if errors.Is(err, entity.ErrEntityRemoved) && attempt < 3 {
			continue
		}
		return value, err == nil, err
	}
}

// lifecycleEntries 是 handler 内新建实体的两个生成入口。
var lifecycleEntries = []string{"lifecycle_create", "lifecycle_get_or_create"}

// createViaLifecycle 与 createAndPublish 相同，只是用生成 Lifecycle 创建。
func (f *createInScopeFixture) createViaLifecycle(entry string, unique int64, value int) (*rollbackTestEntity, error) {
	lifecycle := &generatedShapeLifecycle{access: f.access}
	ctx := nestBaseContext()
	var created *rollbackTestEntity
	var err error
	switch entry {
	case "lifecycle_create":
		created, err = lifecycle.Create(ctx, unique)
	case "lifecycle_get_or_create":
		var isNew bool
		created, isNew, err = lifecycle.GetOrCreate(ctx, unique)
		if err == nil && !isNew {
			err = errors.New("GetOrCreate did not take the create branch")
		}
	}
	if err != nil {
		return nil, err
	}
	created.dao.Value = value
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

// 场景一：pipelined 且未接水位，经生成 Lifecycle 新建的实体在 ticket 持久前不外发。
func TestLifecycleCreateInHandlerWaitsForDurable(t *testing.T) {
	for i, entry := range lifecycleEntries {
		t.Run(entry, func(t *testing.T) {
			unique := int64(9760 + 10*i)
			f := newCreateInScopeFixture(t, unique, nil)
			committer := newPipelinedTestCommitter(false)
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("lifecycle_pipelined_" + entry)
			var created *rollbackTestEntity
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				var err error
				created, err = f.createViaLifecycle(entry, unique+1, 77)
				return "ok", err
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityPipelined})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			defer committer.resolveAll(nil)
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
			// 已有实体的锁可取得即证明 handler 已返回、准入后提前释放已完成（新实体在旧实现里早已解锁）。
			mu := f.existing.GetMutex()
			mu.Lock()
			mu.Unlock()
			if err := f.sync.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if early := drainSubjects(t, f.frames); len(early) != 0 {
				t.Errorf("content left before the commit ticket was durable (durableLSN=%d): %v (created=%d)", committer.DurableLSN(), early, f.createdID)
			}
			if created.Sync().SyncCommitReady() {
				t.Fatalf("lifecycle-created entity is outside the commit barrier before its ticket is durable")
			}
			committer.resolveAll(nil)
			if got := stagedWait(t, ch); got != "ok" {
				t.Fatalf("reply=%v", got)
			}
			if err := f.sync.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := drainSubjects(t, f.frames); got[f.createdID] != 77 {
				t.Fatalf("lifecycle-created entity was not replicated after the durable commit: %v", got)
			}
		})
	}
}

// 场景二：handler 报错回滚，经生成 Lifecycle 新建的实体被撤销。
func TestLifecycleCreateInHandlerRevokedWhenHandlerFails(t *testing.T) {
	for i, entry := range lifecycleEntries {
		t.Run(entry, func(t *testing.T) {
			unique := int64(9780 + 10*i)
			f := newCreateInScopeFixture(t, unique, nil)
			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("lifecycle_fails_" + entry)
			boom := errors.New("business rejected after create")
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if _, err := f.createViaLifecycle(entry, unique+1, 5); err != nil {
					return nil, err
				}
				return nil, boom
			}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			if _, err := mgr.Request(context.Background(), name, f.existID, nil); !errors.Is(err, boom) {
				t.Fatalf("reply err=%v", err)
			}
			if f.manager.Get(f.createdID) != nil {
				t.Fatal("entity created through the generated lifecycle inside a rolled-back handler is still published")
			}
			if err := f.sync.Flush(context.Background()); err != nil {
				t.Fatalf("flush after revocation: %v", err)
			}
			if got := drainSubjects(t, f.frames); len(got) != 0 {
				t.Fatalf("revoked entity was replicated: %v", got)
			}
			if _, err := (&generatedShapeLifecycle{access: f.access}).Create(context.Background(), unique+1); err != nil {
				t.Fatalf("id of a revoked entity cannot be reused: %v", err)
			}
		})
	}
}

// 场景三：strict 提交被拒（接了水位，handler 持锁期间有并发 tick）。
func TestLifecycleCreateInHandlerRevokedWhenStrictCommitRejected(t *testing.T) {
	for i, entry := range lifecycleEntries {
		t.Run(entry, func(t *testing.T) {
			unique := int64(9800 + 10*i)
			f := newCreateInScopeFixture(t, unique, func() uint64 { return 0 })
			committer := &recordingCommitter{err: errors.New("commit rejected")}
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			name := NewHandlerName("lifecycle_rejected_" + entry)
			concurrent := make(chan error, 1)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if _, err := f.createViaLifecycle(entry, unique+1, 55); err != nil {
					return nil, err
				}
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
		})
	}
}

// Nest 之外：生成 Lifecycle 的 Create / GetOrCreate 立即发布、立即可同步（登录 / 创角端点的现状）。
func TestLifecycleCreateOutsideNestUnchanged(t *testing.T) {
	f := newCreateInScopeFixture(t, 9830, nil)
	lifecycle := &generatedShapeLifecycle{access: f.access}
	viaCreate, err := lifecycle.Create(context.Background(), 9831)
	if err != nil {
		t.Fatal(err)
	}
	viaGetOrCreate, isNew, err := lifecycle.GetOrCreate(context.Background(), 9832)
	if err != nil || !isNew {
		t.Fatalf("GetOrCreate: new=%v err=%v", isNew, err)
	}
	for _, e := range []*rollbackTestEntity{viaCreate, viaGetOrCreate} {
		e.dao.Value = 8
		if f.manager.Get(e.ID()) != entity.IThreadSafeEntity(e) || !e.Sync().SyncCommitReady() {
			t.Fatalf("entity %d created outside Nest is not immediately published", e.ID())
		}
		if !e.GetMutex().TryLock() {
			t.Fatalf("entity %d created outside Nest is still locked", e.ID())
		}
		e.GetMutex().Unlock()
		if err := f.sync.Register(e.Sync()); err != nil {
			t.Fatal(err)
		}
		if err := f.sync.Subscribe(1, e.ID(), entity.SyncProfile{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.sync.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := drainSubjects(t, f.frames); got[viaCreate.ID()] != 8 || got[viaGetOrCreate.ID()] != 8 {
		t.Fatalf("entities created outside Nest were not replicated: %v", got)
	}
}

func TestLifecycleGetOrCreateRetriesWhileRevokeFinishes(t *testing.T) {
	manager := entity.NewEntityManager()
	ids := addPilots(t, manager, 9950, 1)
	access := entity.NewManagerAccess(manager)
	lifecycle := &generatedShapeLifecycle{access: access}
	const unique = 9955
	inWindow := make(chan struct{})
	bRetried := make(chan struct{})
	aDone := make(chan struct{})
	var builds atomic.Int64
	hook := func() {
		if builds.Add(1) == 3 {
			close(bRetried)
			select {
			case <-aDone:
			case <-time.After(2 * time.Second):
			}
		}
	}
	createdInScopeBuildHook.Store(&hook)
	t.Cleanup(func() { createdInScopeBuildHook.Store(nil) })

	mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
	boom := errors.New("boom")
	name := NewHandlerName("rr57_revoke_then_fail")
	mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
		if _, err := lifecycle.Create(nestBaseContext(), unique); err != nil {
			return nil, err
		}
		entity.CurrentGuardScope().Guard().AppendPostRelease(func() {
			close(inWindow)
			select {
			case <-bRetried:
			case <-time.After(2 * time.Second):
			}
		})
		return nil, boom
	}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict})
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mgr.Shutdown(context.Background())

	aErr := make(chan error, 1)
	go func() { _, err := mgr.Request(context.Background(), name, ids[0], nil); aErr <- err }()
	<-inWindow
	type result struct {
		value   *rollbackTestEntity
		created bool
		err     error
	}
	bOut := make(chan result, 1)
	go func() {
		value, created, err := lifecycle.GetOrCreate(context.Background(), unique)
		bOut <- result{value, created, err}
	}()
	if err := <-aErr; !errors.Is(err, boom) {
		t.Fatalf("A: want boom, got %v", err)
	}
	close(aDone)
	b := <-bOut
	if b.err != nil || !b.created || b.value == nil {
		t.Fatalf("GetOrCreate while the previous instance's revoke was finishing: created=%v err=%v (ErrEntityRemoved=%v); want a new entity", b.created, b.err, errors.Is(b.err, entity.ErrEntityRemoved))
	}
	if got := manager.Get(b.value.ID()); got != b.value {
		t.Fatalf("GetOrCreate returned an entity that is not the published one: %v", got)
	}
	if !tryLockElsewhere(b.value) {
		t.Fatal("the entity created outside Nest is still locked")
	}
}

// 失败的新建不能留下持久写：builder、组件 Init、OnInitFinish 已用新实体的 DAO 登记了持久修改，随后构建失败（或构建
// 成功、发布撞上同 ID），实体没有发布；业务吞掉创建错误继续执行时，外层事务照常提交别的修改，但不得带上失败实体
// 的 DAO——否则库里会有一个“创建失败却已有数据”的实体。构建还动了自身 DAO 以外的持久状态时不能逐项撤销，整条回滚。

const failedCreateKind entity.EntityKind = 197

const failedCreateComponentType entity.ComponentType = 9701

var errFailedCreateRejected = errors.New("fixture: entity initialization rejected")

// failedCreateStage 是新建在哪一步失败；每一步都先写新实体的 DAO 再判断。
type failedCreateStage int

const (
	failedCreateInBuilder failedCreateStage = iota
	failedCreateInComponentInit
	failedCreateInOnInitFinish
	failedCreateNever
)

func (stage failedCreateStage) String() string {
	return [...]string{"builder", "component Init", "OnInitFinish", "none"}[stage]
}

// failedCreatePlan 按实体 ID 决定构建在哪一步失败；sideEffect 在 builder 里、失败之前运行（写别的实体等）。
type failedCreatePlan struct {
	stage      failedCreateStage
	sideEffect func() error
}

var failedCreatePlans sync.Map // int64 -> failedCreatePlan

func planFailedCreate(t *testing.T, id int64, plan failedCreatePlan) {
	t.Helper()
	failedCreatePlans.Store(id, plan)
	t.Cleanup(func() { failedCreatePlans.Delete(id) })
}

// writeFailedCreateDAO 是组件 / 实体初始化里的一次持久 setter。
func writeFailedCreateDAO(dao *rollbackTestDao, value int) error {
	dao.Value = value
	return MarkPersist(dao, 1)
}

type failedCreateEntity struct {
	*entity.EntityBase
	dao   *rollbackTestDao
	comps entity.ComponentManager
	stage failedCreateStage
}

func (e *failedCreateEntity) Base() *entity.EntityBase { return e.EntityBase }

func (e *failedCreateEntity) RangeDao(f func(entity.DaoInterface)) { f(e.dao) }

func (e *failedCreateEntity) OnInitFinish(*entity.EntityCreateParam) error {
	if err := writeFailedCreateDAO(e.dao, 97); err != nil {
		return err
	}
	if e.stage == failedCreateInOnInitFinish {
		return errFailedCreateRejected
	}
	return nil
}

type failedCreateComponent struct{ owner *failedCreateEntity }

func (*failedCreateComponent) Name() string                         { return "failed_create" }
func (*failedCreateComponent) OnDestroy(entity.EntityDestroyReason) {}
func (c *failedCreateComponent) OnInitFinish(*entity.EntityCreateParam, bool) error {
	if err := writeFailedCreateDAO(c.owner.dao, 98); err != nil {
		return err
	}
	if c.owner.stage == failedCreateInComponentInit {
		return errFailedCreateRejected
	}
	return nil
}

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: 1, Kind: failedCreateKind,
		DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return &rollbackTestDao{} }},
		Builder: func(p *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			plan := failedCreatePlan{stage: failedCreateNever}
			if stored, ok := failedCreatePlans.Load(p.Id); ok {
				plan = stored.(failedCreatePlan)
			}
			e := &failedCreateEntity{
				EntityBase: entity.NewEntityBaseWithMutex(p.Id, p.Category, false, p.Mutex, p.Kind),
				dao:        p.Dao["rollback_test"].(*rollbackTestDao),
				comps:      entity.NewComponentManager(),
				stage:      plan.stage,
			}
			e.comps.Set(failedCreateComponentType, &failedCreateComponent{owner: e})
			if err := writeFailedCreateDAO(e.dao, 99); err != nil {
				return nil, err
			}
			if plan.sideEffect != nil {
				if err := plan.sideEffect(); err != nil {
					return nil, err
				}
			}
			if plan.stage == failedCreateInBuilder {
				return nil, errFailedCreateRejected
			}
			if err := e.comps.InitAll(p, p.IsCreate); err != nil {
				return nil, err
			}
			return e, nil
		},
	})
}

func failedCreateParam(id int64) *entity.EntityCreateParam {
	return &entity.EntityCreateParam{IsCreate: true, Id: id, Category: 1, Kind: failedCreateKind}
}

// assertNoMutationFor 断言提交记录里没有该实体的文档。
func assertNoMutationFor(t *testing.T, committer *recordingCommitter, id int64) {
	t.Helper()
	for _, mutation := range committer.record.Mutations {
		if mutation.Key.ID == id {
			t.Fatalf("the failed creation of %d reached the commit: key=%+v version %d->%d payload=%s",
				id, mutation.Key, mutation.ExpectedVersion, mutation.NextVersion, mutation.Data)
		}
	}
}

// failedCreateEngine 起一个 Nest，注册 handler：在 pilot 上新建 created；业务吞掉创建错误时把 pilot 的值改成 5 并
// 正常返回，propagate 时原样返回创建错误。
func failedCreateEngine(t *testing.T, manager *entity.EntityManager, committer *recordingCommitter, meta HandlerMeta, created int64, propagate bool) (*NestMgr, HandlerName, *error) {
	t.Helper()
	mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(manager)), NestOptionWithTransactionCommitter(committer))
	name := NewHandlerName(fmt.Sprintf("failed_create_%d_%t", created, propagate))
	var createErr error
	mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
		_, createErr = manager.Create(failedCreateParam(created))
		if createErr == nil {
			return nil, errors.New("fixture: the creation was expected to fail")
		}
		if propagate {
			return nil, createErr
		}
		pilot := es[0].(*rollbackTestEntity)
		return "handled", writeFailedCreateDAO(pilot.dao, 5)
	}, meta)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })
	return mgr, name, &createErr
}

func TestAFailedCreateLeavesNoPersistentWrite(t *testing.T) {
	strict := HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}
	for i, stage := range []failedCreateStage{failedCreateInBuilder, failedCreateInComponentInit, failedCreateInOnInitFinish} {
		base := int64(39300 + 20*i)
		t.Run(stage.String(), func(t *testing.T) {
			t.Run("isolated transaction, the error is handled", func(t *testing.T) {
				manager := entity.NewEntityManager()
				id := mustBuildCastID(t, base, 1, failedCreateKind)
				planFailedCreate(t, id, failedCreatePlan{stage: stage})
				committer := &recordingCommitter{}
				err := entity.WithGuardScope("failed_create_isolated", func(*entity.GuardScope) error {
					_, err := RunIsolatedTransaction(context.Background(), committer, "failed_create_isolated", func() (any, error) {
						if _, err := manager.Create(failedCreateParam(id)); !errors.Is(err, errFailedCreateRejected) {
							t.Errorf("create err=%v, want the initialization failure", err)
						}
						return nil, nil
					})
					return err
				})
				if err != nil {
					t.Fatalf("transaction err=%v", err)
				}
				if manager.Get(id) != nil {
					t.Fatal("the failed entity was published")
				}
				assertNoMutationFor(t, committer, id)
			})

			t.Run("nest handler handles the error and goes on", func(t *testing.T) {
				manager := entity.NewEntityManager()
				pilot := addPilots(t, manager, base+1, 1)[0]
				id := mustBuildCastID(t, base+2, 1, failedCreateKind)
				planFailedCreate(t, id, failedCreatePlan{stage: stage})
				committer := &recordingCommitter{}
				mgr, name, createErr := failedCreateEngine(t, manager, committer, strict, id, false)
				if _, err := mgr.Request(context.Background(), name, pilot, nil); err != nil {
					t.Fatalf("request err=%v, want the handler's own result (it handled the failed creation)", err)
				}
				if !errors.Is(*createErr, errFailedCreateRejected) {
					t.Fatalf("create err=%v", *createErr)
				}
				if manager.Get(id) != nil {
					t.Fatal("the failed entity was published")
				}
				assertNoMutationFor(t, committer, id)
				if len(committer.record.Mutations) != 1 || committer.record.Mutations[0].Key.ID != pilot {
					t.Fatalf("committed %+v, want only the pilot's change made after the failed creation", committer.record.Mutations)
				}
			})

			t.Run("nest handler propagates the error", func(t *testing.T) {
				manager := entity.NewEntityManager()
				pilot := addPilots(t, manager, base+3, 1)[0]
				id := mustBuildCastID(t, base+4, 1, failedCreateKind)
				planFailedCreate(t, id, failedCreatePlan{stage: stage})
				committer := &recordingCommitter{}
				mgr, name, _ := failedCreateEngine(t, manager, committer, strict, id, true)
				if _, err := mgr.Request(context.Background(), name, pilot, nil); !errors.Is(err, errFailedCreateRejected) {
					t.Fatalf("request err=%v, want the initialization failure", err)
				}
				if len(committer.record.Mutations) != 0 {
					t.Fatalf("a rolled-back handler reached the committer: %+v", committer.record.Mutations)
				}
			})
		})
	}

	// memory 事务（有 RollbackTx、durability=memory）在提交点拒绝任何持久登记；失败新建的登记撤销后，只改非持久状态的
	// handler 照常成功，不因一个没有发布的实体被拒。
	t.Run("memory transaction handles the error", func(t *testing.T) {
		manager := entity.NewEntityManager()
		pilot := addPilots(t, manager, 39380, 1)[0]
		id := mustBuildCastID(t, 39381, 1, failedCreateKind)
		planFailedCreate(t, id, failedCreatePlan{stage: failedCreateInBuilder})
		committer := &recordingCommitter{}
		mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(manager)), NestOptionWithTransactionCommitter(committer))
		name := NewHandlerName("failed_create_memory")
		mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
			if _, err := manager.Create(failedCreateParam(id)); !errors.Is(err, errFailedCreateRejected) {
				return nil, fmt.Errorf("fixture: create err=%v", err)
			}
			return "handled", nil
		}, HandlerMeta{Rollback: RollbackState, Durability: DurabilityMemory})
		if err := mgr.Start(); err != nil {
			t.Fatal(err)
		}
		defer mgr.Shutdown(context.Background())
		if _, err := mgr.Request(context.Background(), name, pilot, nil); err != nil {
			t.Fatalf("request err=%v, want success: the failed entity's DAO is not this handler's persistent write", err)
		}
		assertNoMutationFor(t, committer, id)
	})

	// 对照：同一 kind 构建成功时，新实体照 R15 由创建它的事务写齐（0→1 整文档）。
	t.Run("a successful create is still stored by its transaction", func(t *testing.T) {
		manager := entity.NewEntityManager()
		pilot := addPilots(t, manager, 39390, 1)[0]
		id := mustBuildCastID(t, 39391, 1, failedCreateKind)
		committer := &recordingCommitter{}
		mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(manager)), NestOptionWithTransactionCommitter(committer))
		name := NewHandlerName("failed_create_control_success")
		mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
			_, err := manager.Create(failedCreateParam(id))
			return nil, err
		}, strict)
		if err := mgr.Start(); err != nil {
			t.Fatal(err)
		}
		defer mgr.Shutdown(context.Background())
		if _, err := mgr.Request(context.Background(), name, pilot, nil); err != nil {
			t.Fatal(err)
		}
		if manager.Get(id) == nil {
			t.Fatal("the created entity is not published")
		}
		var stored bool
		for _, mutation := range committer.record.Mutations {
			stored = stored || (mutation.Key.ID == id && mutation.ExpectedVersion == 0 && mutation.NextVersion == 1)
		}
		if !stored {
			t.Fatalf("the created entity is not in its creating commit: %+v", committer.record.Mutations)
		}
	})
}

// 构建成功、发布撞上同 ID 的已有实例（ErrEntityExists，生成 GetOrCreate 据此改为读取）：这次构建登记的 DAO 修改同样
// 撤销，已有实例不受影响，handler 的其他修改照常提交。
func TestACreateThatLosesToAnExistingIDLeavesNoPersistentWrite(t *testing.T) {
	manager := entity.NewEntityManager()
	pilot := addPilots(t, manager, 39400, 1)[0]
	id := mustBuildCastID(t, 39401, 1, failedCreateKind)
	existing := &rollbackTestEntity{EntityBase: entity.NewEntityBase(id, 1, false, failedCreateKind), dao: &rollbackTestDao{id: id, Value: 1}}
	if err := manager.TryAdd(existing); err != nil {
		t.Fatal(err)
	}
	committer := &recordingCommitter{}
	mgr, name, createErr := failedCreateEngine(t, manager, committer, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, id, false)
	if _, err := mgr.Request(context.Background(), name, pilot, nil); err != nil {
		t.Fatalf("request err=%v", err)
	}
	if !errors.Is(*createErr, entity.ErrEntityExists) {
		t.Fatalf("create err=%v, want ErrEntityExists", *createErr)
	}
	if manager.Get(id) != entity.IThreadSafeEntity(existing) || existing.dao.Value != 1 {
		t.Fatal("the existing instance was replaced or changed")
	}
	assertNoMutationFor(t, committer, id)
	if len(committer.record.Mutations) != 1 || committer.record.Mutations[0].Key.ID != pilot {
		t.Fatalf("committed %+v, want only the pilot's change", committer.record.Mutations)
	}
}

// 构建在失败之前还写了别的持久状态（另一个实体的 DAO、outbox 效果）：这些不能逐项撤销，业务吞掉创建错误也整条回滚，
// 回复带 ErrFailedCreateLeftEffects 与原始失败，提交记录不产生。
func TestAFailedCreateThatTouchedOtherStateRollsBackTheTransaction(t *testing.T) {
	for i, tc := range []struct {
		name   string
		effect func(other *rollbackTestEntity) error
	}{
		{"another entity's DAO", func(other *rollbackTestEntity) error { return writeFailedCreateDAO(other.dao, 77) }},
		{"an outbox effect", func(*rollbackTestEntity) error { return Emit(Effect{Topic: "failed_create.welcome"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pilot := addPilots(t, manager, int64(39410+10*i), 1)[0]
			pilotEntity := manager.Get(pilot).(*rollbackTestEntity)
			id := mustBuildCastID(t, int64(39411+10*i), 1, failedCreateKind)
			planFailedCreate(t, id, failedCreatePlan{stage: failedCreateInBuilder, sideEffect: func() error { return tc.effect(pilotEntity) }})
			committer := &recordingCommitter{}
			mgr, name, createErr := failedCreateEngine(t, manager, committer, HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}, id, false)
			_, err := mgr.Request(context.Background(), name, pilot, nil)
			if !errors.Is(err, ErrFailedCreateLeftEffects) || !errors.Is(err, errFailedCreateRejected) {
				t.Fatalf("request err=%v, want ErrFailedCreateLeftEffects with the initialization failure", err)
			}
			if !errors.Is(*createErr, ErrFailedCreateLeftEffects) || !errors.Is(*createErr, errFailedCreateRejected) {
				t.Fatalf("create err=%v, want both sentinels so the business sees why", *createErr)
			}
			if len(committer.record.Mutations) != 0 || len(committer.record.Effects) != 0 {
				t.Fatalf("a transaction with an unrevocable failed creation reached the committer: %+v", committer.record)
			}
			if pilotEntity.dao.Value != 1 {
				t.Fatalf("pilot value=%d, want the rollback to restore 1", pilotEntity.dao.Value)
			}
		})
	}
}

// effectRecordingCommitter 是自动持久的 pipelined committer，同时实现 BestEffortEffectPublisher：strict / async 走
// Commit，pipelined 走 Enqueue，三者都记进 records；交给 PublishBestEffort 的 effect 记进 published。
type effectRecordingCommitter struct {
	*pipelinedTestCommitter
	publishMu sync.Mutex
	published []Effect
}

func (c *effectRecordingCommitter) PublishBestEffort(_ TransactionID, effects []Effect) {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	c.published = append(c.published, effects...)
}

func (c *effectRecordingCommitter) snapshot() (records []CommitRecord, published []Effect) {
	c.mu.Lock()
	records = append(records, c.records...)
	c.mu.Unlock()
	c.publishMu.Lock()
	published = append(published, c.published...)
	c.publishMu.Unlock()
	return records, published
}

// RV-20261106-01：失败新建在构建里 Emit 的 effect（reliable 与 best_effort 同样）属于一次没有发生的新建，不能逐项撤销，
// 业务吞掉创建错误也整条回滚：回复带 ErrFailedCreateLeftEffects，不产生提交记录，也不经 publisher 发出
// best_effort effect。覆盖三个失败阶段与 strict / async / pipelined；最后一组是构建成功的对照（effect 照常发出），
// 证明这里能观察到发布。
func TestAFailedCreateDoesNotPublishItsEffects(t *testing.T) {
	stages := []failedCreateStage{failedCreateInBuilder, failedCreateInComponentInit, failedCreateInOnInitFinish, failedCreateNever}
	durabilities := []DurabilityPolicy{DurabilityStrict, DurabilityAsync, DurabilityPipelined}
	deliveries := []EffectDelivery{EffectReliable, EffectBestEffort}
	n := int64(0)
	for _, stage := range stages {
		for _, durability := range durabilities {
			for _, delivery := range deliveries {
				n++
				base := 39700 + 2*n
				t.Run(fmt.Sprintf("%s/%s/%s", stage, durability, delivery), func(t *testing.T) {
					manager := entity.NewEntityManager()
					pilot := addPilots(t, manager, base, 1)[0]
					pilotEntity := manager.Get(pilot).(*rollbackTestEntity)
					id := mustBuildCastID(t, base+1, 1, failedCreateKind)
					planFailedCreate(t, id, failedCreatePlan{stage: stage, sideEffect: func() error {
						return Emit(Effect{Topic: "failed_create.created", Key: fmt.Sprint(id), Delivery: delivery, Payload: []byte("created")})
					}})
					committer := &effectRecordingCommitter{pipelinedTestCommitter: newPipelinedTestCommitter(true)}
					mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(manager)), NestOptionWithTransactionCommitter(committer))
					name := NewHandlerName(fmt.Sprintf("failed_create_effect_%d", n))
					mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
						_, err := manager.Create(failedCreateParam(id))
						if stage == failedCreateNever {
							if err != nil {
								return nil, err
							}
						} else if !errors.Is(err, errFailedCreateRejected) {
							return nil, fmt.Errorf("fixture: create err=%v, want the initialization failure", err)
						}
						// 业务吞掉创建错误，继续改 pilot。
						return "handled", writeFailedCreateDAO(pilotEntity.dao, 5)
					}, HandlerMeta{Rollback: RollbackState, Durability: durability})
					if err := mgr.Start(); err != nil {
						t.Fatal(err)
					}
					defer mgr.Shutdown(context.Background())
					_, err := mgr.Request(context.Background(), name, pilot, nil)
					// 三种级别的提交后回调（含 best_effort 发布）都在回复之前执行：strict / async（有 best_effort 时升到
					// strict）在锁内提交之后，pipelined 第一阶段在本 worker 等到票据持久之后。
					records, published := committer.snapshot()
					if stage == failedCreateNever {
						if err != nil || manager.Get(id) == nil {
							t.Fatalf("control: err=%v visible=%v", err, manager.Get(id) != nil)
						}
						if delivery == EffectBestEffort && len(published) != 1 {
							t.Fatalf("control: published %+v, want the created entity's best_effort effect", published)
						}
						return
					}
					if !errors.Is(err, ErrFailedCreateLeftEffects) || !errors.Is(err, errFailedCreateRejected) {
						t.Fatalf("request err=%v published=%+v records=%d entityVisible=%v; want ErrFailedCreateLeftEffects with the initialization failure",
							err, published, len(records), manager.Get(id) != nil)
					}
					if len(published) != 0 || len(records) != 0 {
						t.Fatalf("a failed creation's effect escaped: published=%+v records=%+v", published, records)
					}
					if manager.Get(id) != nil || pilotEntity.dao.Value != 1 {
						t.Fatalf("visible=%v pilot value=%d, want the whole transaction rolled back", manager.Get(id) != nil, pilotEntity.dao.Value)
					}
				})
			}
		}
	}
}

// declaredRemoteBatch 是只声明了 ids 的 Remote 写批次替身（strict 下 Commit 成功、没有 Remote 提交内容）。
type declaredRemoteBatch struct {
	stagedRemoteBatch
	ids []int64
}

func (b *declaredRemoteBatch) EntityIDs() []int64 { return append([]int64(nil), b.ids...) }

// handler 里新建一个没有声明进本消息的 Remote 托管实体（本消息声明的是另一个 Remote 实体，所以带 Remote 批次）：
// 新实体不在批次里，没有它的分布式所有权保护，Remote 持久写只能走消息声明的批次（REMOTE_ENTITY“写准入”），与 Cast 对
// 未声明 Remote 目标的拒绝（refuseUndeclaredRemoteTargets）同一结论。新建返回 ErrDurableRemoteWriteUnsupported，发布被
// 撤销；业务吞掉错误时整条事务回滚（捕获失败），批次中止、不提交。修前 CaptureEntities 只看消息带不带批次，新实体的
// DAO 由本地投影以普通文档写进 Remote 集合（真实依赖用例 scripts/testdata/remoteflow create_unchanged_test.go in_handler）。
func TestCreateInHandlerRefusesARemoteEntityTheMessageDidNotDeclare(t *testing.T) {
	for _, swallow := range []bool{false, true} {
		t.Run(fmt.Sprintf("swallow=%v", swallow), func(t *testing.T) {
			unique := int64(36760)
			if swallow {
				unique += 10
			}
			manager := entity.NewEntityManager()
			access := entity.NewManagerAccess(manager)
			declared := mustBuildCastID(t, unique, entity.EntityCategoryRemote, remoteRecreatableKind)
			undeclared := mustBuildCastID(t, unique+1, entity.EntityCategoryRemote, remoteRecreatableKind)
			param := func(id int64) *entity.EntityCreateParam {
				return &entity.EntityCreateParam{IsCreate: true, Id: id, Category: entity.EntityCategoryRemote, Kind: remoteRecreatableKind}
			}
			if _, err := access.Create(param(declared)); err != nil { // Nest 之外创建并发布
				t.Fatal(err)
			}
			var batches []*declaredRemoteBatch
			var commits atomic.Int32
			remote := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) {
				batch := &declaredRemoteBatch{stagedRemoteBatch: stagedRemoteBatch{commit: func() { commits.Add(1) }}, ids: []int64{declared}}
				batches = append(batches, batch)
				return batch, nil
			}}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithRemoteEntityManager(remote), NestOptionWithTransactionCommitter(&recordingCommitter{}), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}, WorkerPoolConfig{}))
			meta := HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}
			noop := NewHandlerName(fmt.Sprintf("remote_undeclared_create_noop_%d", unique))
			mgr.MustRegisterHandlerWithMeta(noop, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				return "ok", nil
			}, meta)
			name := NewHandlerName(fmt.Sprintf("remote_undeclared_create_%d", unique))
			var createErr error
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				_, createErr = access.Create(param(undeclared))
				if swallow {
					return "ok", nil
				}
				return nil, createErr
			}, meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			// 对照：声明的 Remote 目标照常在批次里捕获并提交。
			if reply, err := mgr.Request(context.Background(), noop, declared, nil); err != nil || reply != "ok" || commits.Load() != 1 {
				t.Fatalf("premise: a strict message on the declared Remote entity reply=%v err=%v batch commits=%d, want ok with one commit", reply, err, commits.Load())
			}
			reply, err := mgr.Request(context.Background(), name, declared, nil)
			if !errors.Is(createErr, ErrDurableRemoteWriteUnsupported) {
				t.Fatalf("create of an undeclared Remote entity err=%v, want ErrDurableRemoteWriteUnsupported", createErr)
			}
			if err == nil {
				t.Fatalf("handler reply=%v with no error: the transaction that created an undeclared Remote entity must not commit", reply)
			}
			if got := manager.Get(undeclared); got != nil {
				t.Fatalf("the undeclared Remote entity stayed published after the refusal: %p", got)
			}
			if commits.Load() != 1 {
				t.Fatalf("batch commits=%d, want only the premise's commit", commits.Load())
			}
			if last := batches[len(batches)-1]; !last.aborted.Load() || last.closed.Load() != 1 {
				t.Fatalf("refused message's batch aborted=%v closed=%d, want aborted and closed once", last.aborted.Load(), last.closed.Load())
			}
		})
	}
}
