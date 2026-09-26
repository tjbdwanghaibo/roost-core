package engine

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20260926-30：原生 saga 步骤（本地 mutation + lease fence receipt）已在 Nest 内存提交并准入 WAL，
// 投影时租约已过期，整笔记录被跳过；旧实现仍然准入同一实体上以该内存为基础的下一笔事务，
// 它按内存版本 5→6 投影、Mongo 仍是 4，判 fatal 冲突，Projector fence，重启回放同一冲突起不来。
// 承诺：
//   - 原生步骤准入后、投影结果确定前，同实体的其他事务在 WAL 准入处以可重试的
//     coredata.ErrFencedEntityPending 拒绝；步骤投影成功、或被跳过且内存实体已驱逐后恢复；
//   - 屏障随投影成功 / 驱逐完成 / discard / fatal / 关闭解除，不泄漏；
//   - 同一步骤的重投（新 token 的另一条记录）只被挡到屏障解除为止；
//   - 被跳过后受影响的常驻实体被驱逐（不持久化），驱逐在 Nest 快池执行；重启恢复通过。

const fencedClaim = "saga-step/cmd-slow"

var fencedDigest = bytes.Repeat([]byte{5}, 32)

// fencedDebit 是原生步骤记录：hero 7 从 4 到 5，带 token 的 lease fence。
func fencedDebit(t *testing.T, id byte, token uint64) coredata.CommitRecord {
	t.Helper()
	record := testMutationRecord(coredata.MutationPatch)
	record.ID[15] = id
	record.Receipts = append(record.Receipts, leaseFenceReceipt(t, fencedClaim, "worker-1", token, fencedDigest))
	return record
}

// heroWrite 是同一实体上的普通事务 expected -> expected+1。
func heroWrite(id byte, expected uint64) coredata.CommitRecord {
	record := testMutationRecord(coredata.MutationPatch)
	record.ID[15] = id
	record.Mutations[0].ExpectedVersion, record.Mutations[0].NextVersion = expected, expected+1
	record.Mutations[0].Patch.SetBSON, _ = bson.Marshal(bson.D{{Key: "level", Value: int32(expected + 1)}})
	return record
}

// commitReleased 模拟 Nest：锁内准入、释放后通知。
func commitReleased(projector *Projector, record coredata.CommitRecord) error {
	err := projector.Commit(context.Background(), record)
	if err == nil {
		projector.TransactionReleased(record.ID)
	}
	return err
}

type evictionRecorder struct {
	calls  atomic.Int32
	failed chan struct{}
	fail   atomic.Bool
}

func (recorder *evictionRecorder) evict(_ context.Context, ids []int64) error {
	recorder.calls.Add(1)
	if recorder.fail.Load() {
		select {
		case recorder.failed <- struct{}{}:
		default:
		}
		return errors.New("injected eviction failure")
	}
	return nil
}

func fencedProjector(t *testing.T, leaseUntil time.Time) (*Projector, *evictionRecorder, *mongotest.Collection, *nestwal.WAL) {
	t.Helper()
	store, client, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	seedClaim(t, client, fencedClaim, "worker-1", 7, fencedDigest, leaseUntil)
	projector, wal := manualProjector(t, store)
	recorder := &evictionRecorder{failed: make(chan struct{}, 1)}
	projector.evictEntities = recorder.evict
	return projector, recorder, collection, wal
}

func TestFencedLocalStepSkippedAfterLeaseExpiryDoesNotFenceNextTransaction(t *testing.T) {
	projector, recorder, collection, _ := fencedProjector(t, time.Now().UTC().Add(-time.Second))
	ctx := context.Background()
	if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil { // 4 -> 5，Nest 内存已经应用
		t.Fatal(err)
	}
	nextErr := commitReleased(projector, heroWrite(10, 5)) // 同一实体，内存版本 5 -> 6

	flushErr := projector.Flush(ctx)
	if errors.Is(flushErr, ErrProjectionConflict) || projector.Stats().FatalProjectionConflicts != 0 {
		t.Fatalf("same-entity transaction after a skipped fenced step fenced the projector: next admission=%v flush=%v", nextErr, flushErr)
	}
	if !errors.Is(nextErr, coredata.ErrFencedEntityPending) {
		t.Fatalf("same-entity transaction behind an unprojected lease-fenced step: admission=%v, want ErrFencedEntityPending", nextErr)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(4) {
		t.Fatalf("skipped step changed Mongo: %v", doc)
	}
	// 冷加载屏障覆盖驱逐：等到驱逐完成才返回，此后实体从 Mongo（v4）重载。
	if err := projector.WaitEntityProjection(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if recorder.calls.Load() != 1 {
		t.Fatalf("evictions=%d, want 1", recorder.calls.Load())
	}
	stats := projector.Stats()
	if stats.FencedEntities != 0 || stats.StaleEvictions != 1 || stats.FencedAdmissionRejected != 1 {
		t.Fatalf("stats after eviction=%+v", stats)
	}
	// 屏障解除后，以 Mongo 为基础（重载后的内存）的下一笔事务正常准入与投影。
	if err := commitReleased(projector, heroWrite(11, 4)); err != nil {
		t.Fatalf("admission after the eviction: %v", err)
	}
	if err := projector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(5) || doc["level"] != int32(5) {
		t.Fatalf("Mongo after the reloaded transaction: %v", doc)
	}
}

func TestFencedLocalStepAppliedReleasesBarrierWithoutEviction(t *testing.T) {
	projector, recorder, collection, _ := fencedProjector(t, time.Now().UTC().Add(time.Hour))
	ctx := context.Background()
	if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil {
		t.Fatal(err)
	}
	if err := commitReleased(projector, heroWrite(10, 5)); !errors.Is(err, coredata.ErrFencedEntityPending) {
		t.Fatalf("admission while the step is unprojected=%v", err)
	}
	if err := projector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := projector.WaitEntityProjection(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if recorder.calls.Load() != 0 || projector.Stats().FencedEntities != 0 {
		t.Fatalf("applied step: evictions=%d stats=%+v", recorder.calls.Load(), projector.Stats())
	}
	if err := commitReleased(projector, heroWrite(10, 5)); err != nil {
		t.Fatalf("retry after the step applied: %v", err)
	}
	if err := projector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(6) {
		t.Fatalf("Mongo=%v, want version 6", doc)
	}
}

// 同一步骤的重投拿到新 token，是另一条记录：它被挡到上一条的结果确定为止，而不是永远。
func TestFencedStepRedeliveryWaitsOnlyUntilThePreviousRecordSettles(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	// 第一条记录 token 7；重投已经接管到 token 8，所以第一条投影时会被跳过。
	seedClaim(t, client, fencedClaim, "worker-1", 8, fencedDigest, time.Now().UTC().Add(time.Hour))
	projector, _ := manualProjector(t, store)
	recorder := &evictionRecorder{}
	projector.evictEntities = recorder.evict
	ctx := context.Background()
	if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil {
		t.Fatal(err)
	}
	redelivery := fencedDebit(t, 12, 8)
	if err := commitReleased(projector, redelivery); !errors.Is(err, coredata.ErrFencedEntityPending) {
		t.Fatalf("redelivered step over an unsettled record=%v", err)
	}
	if err := projector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := projector.WaitEntityProjection(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err := commitReleased(projector, redelivery); err != nil {
		t.Fatalf("redelivered step after the barrier lifted: %v", err)
	}
	if err := projector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(5) {
		t.Fatalf("redelivered step did not apply exactly once: %v", doc)
	}
}

func TestFencedStepBarrierIsReleasedOnEveryExit(t *testing.T) {
	t.Run("discard", func(t *testing.T) {
		projector, _, _, wal := fencedProjector(t, time.Now().UTC().Add(time.Hour))
		if err := wal.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := commitReleased(projector, fencedDebit(t, 9, 7)); err == nil {
			t.Fatal("append on a closed WAL succeeded")
		}
		if got := projector.Stats().FencedEntities; got != 0 {
			t.Fatalf("discarded admission leaked %d fenced entities", got)
		}
	})
	t.Run("fatal", func(t *testing.T) {
		projector, _, _, _ := fencedProjector(t, time.Now().UTC().Add(time.Hour))
		conflict := heroWrite(8, 1)
		conflict.Mutations[0].Key.ID = 8 // 不存在的文档：fatal 冲突，排在原生步骤之前
		if err := commitReleased(projector, conflict); err != nil {
			t.Fatal(err)
		}
		if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil {
			t.Fatal(err)
		}
		if err := projector.Flush(context.Background()); !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("flush=%v, want fatal conflict", err)
		}
		if got := projector.Stats().FencedEntities; got != 0 {
			t.Fatalf("fatal left %d fenced entities", got)
		}
		if err := projector.WaitEntityProjection(context.Background(), 7); !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("waiter after fatal=%v", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		projector, _, _, _ := fencedProjector(t, time.Now().UTC().Add(time.Hour))
		if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil {
			t.Fatal(err)
		}
		if err := projector.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := projector.Stats().FencedEntities; got != 0 {
			t.Fatalf("close left %d fenced entities", got)
		}
	})
	t.Run("eviction retries keep the barrier until they succeed", func(t *testing.T) {
		projector, recorder, _, _ := fencedProjector(t, time.Now().UTC().Add(-time.Second))
		recorder.fail.Store(true)
		if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil {
			t.Fatal(err)
		}
		if err := projector.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		awaitChan(t, recorder.failed, "the first eviction attempt to fail")
		if err := commitReleased(projector, heroWrite(10, 4)); !errors.Is(err, coredata.ErrFencedEntityPending) {
			t.Fatalf("admission while the eviction is failing=%v", err)
		}
		recorder.fail.Store(false)
		if err := projector.WaitEntityProjection(context.Background(), 7); err != nil {
			t.Fatal(err)
		}
		if err := commitReleased(projector, heroWrite(10, 4)); err != nil {
			t.Fatalf("admission after the eviction succeeded: %v", err)
		}
	})
}

// 重启：被跳过的步骤仍在 WAL 里、投影前进程退出。屏障保证它之后没有同实体记录，
// 启动恢复把它跳过即可；之后实体从 Mongo（v4）加载，下一笔事务正常投影。
func TestFencedStepSkippedDuringStartupRecoveryNeedsNoEviction(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	seedClaim(t, client, fencedClaim, "worker-1", 7, fencedDigest, time.Now().UTC().Add(-time.Second))
	options := nestwal.DefaultOptions(t.TempDir())
	options.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := commitReleased(first, fencedDebit(t, 9, 7)); err != nil {
		t.Fatal(err)
	}
	if err := commitReleased(first, heroWrite(10, 5)); !errors.Is(err, coredata.ErrFencedEntityPending) {
		t.Fatalf("dependent record admitted before the crash: %v", err)
	}
	if err := first.Close(context.Background()); err != nil { // 不 Flush：模拟投影前退出
		t.Fatal(err)
	}
	reopened, err := nestwal.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	second, err := NewProjector(reopened, store, ProjectorOptions{CloseWAL: false, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	if err := second.Flush(context.Background()); err != nil {
		t.Fatalf("startup recovery: %v", err)
	}
	if stats := second.Stats(); stats.FatalProjectionConflicts != 0 || stats.StaleEvictions != 0 {
		t.Fatalf("recovery stats=%+v", stats)
	}
	if err := commitReleased(second, heroWrite(11, 4)); err != nil {
		t.Fatal(err)
	}
	if err := second.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(5) {
		t.Fatalf("Mongo after restart=%v", doc)
	}
}

type evictionTestEntity struct {
	*entity.EntityBase
	destroyedOnFast *atomic.Int32
}

func (value *evictionTestEntity) Base() *entity.EntityBase { return value.EntityBase }
func (value *evictionTestEntity) OnDestroy(entity.EntityDestroyReason) {
	if fctx.InFastWorker() {
		value.destroyedOnFast.Add(1)
	}
}

// Runtime 驱逐：Nest 构造时把 RunLocal 绑定给作为 committer 的 Projector，驱逐在快池持锁执行，
// 常驻实体被移出内存（不删库）。
func TestSkippedFencedStepEvictsTheResidentEntityOnTheFastPool(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	seedHero(t, collection, 4)
	seedClaim(t, client, fencedClaim, "worker-1", 7, fencedDigest, time.Now().UTC().Add(-time.Second))
	projector, _ := manualProjector(t, store)
	manager := entity.NewEntityManager()
	access := entity.NewManagerAccess(manager)
	onFast := &atomic.Int32{}
	manager.Add(&evictionTestEntity{EntityBase: entity.NewEntityBase(7, entity.EntityCategory(1), false, deleteTestKind), destroyedOnFast: onFast})
	runtime := &Runtime{Projector: projector, access: access}
	projector.evictEntities = runtime.evictStaleEntities
	scheduler := corenest.NewEngine(corenest.NestOptionWithTransactionCommitter(projector), corenest.NestOptionWithGetter(access))
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scheduler.Shutdown(context.Background()) })

	if err := commitReleased(projector, fencedDebit(t, 9, 7)); err != nil {
		t.Fatal(err)
	}
	if err := projector.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := projector.WaitEntityProjection(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if manager.Get(7) != nil {
		t.Fatal("entity carrying the skipped step is still resident")
	}
	if onFast.Load() != 1 {
		t.Fatalf("eviction ran on the fast pool %d times, want 1", onFast.Load())
	}
	if doc, _ := collection.Lookup(int64(7)); doc["_version"] != int64(4) {
		t.Fatalf("eviction persisted something: %v", doc)
	}
}
