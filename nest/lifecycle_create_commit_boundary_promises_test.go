package nest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-35 复核：业务在 handler 里新建实体的正式入口是生成 Lifecycle 的 Create / GetOrCreate，
// 它们经 ManagerAccess.Create → EntityManager.Create 自建短 Guard 作用域。handler 内调用它们就是事务内，
// 必须与 CreateInScope 进入同一提交边界；真正在 Nest 之外调用时保持立即发布。

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
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
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
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
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
			mgr := NewEngine(NestOptionWithGetter(f.access), NestOptionWithTransactionCommitter(committer), NestOptionWithEntitySync(f.sync), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
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
