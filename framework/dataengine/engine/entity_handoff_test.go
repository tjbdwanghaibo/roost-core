package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 交出实体（Runtime.HandOffEntity）的契约与它依据的现状。

// unconfirmedProjection 把一条记录做成“已投影、checkpoint 未确认”：记录已准入、已落库，WaitEntityProjection 已经放行
// （UnloadEntity 等的就是它），WAL 的 checkpoint 却没有盖过它——进程在两者之间停下，或 checkpoint 写失败。
// 返回的 reopen 模拟进程重启：关掉当前 Projector 与 WAL，在同一个 WAL 目录上重新打开。
func unconfirmedProjection(t *testing.T, record coredata.CommitRecord) (projector *Projector, heroes *mongotest.Collection, reopen func() *Projector) {
	t.Helper()
	store, _, heroes := newMongoStoreTest(t)
	seedHero(t, heroes, 4)
	dir := t.TempDir()
	open := func() (*Projector, *nestwal.WAL) {
		wal, err := nestwal.Open(nestwal.DefaultOptions(dir))
		if err != nil {
			t.Fatal(err)
		}
		opened, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: false, ManualReplay: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = opened.Close(context.Background())
			_ = wal.Close(context.Background())
		})
		return opened, wal
	}
	projector, wal := open()
	ctx := context.Background()
	if err := projector.Commit(ctx, record); err != nil {
		t.Fatal(err)
	}
	projector.TransactionReleased(record.ID)
	lost := errors.New("checkpoint lost")
	projector.OverrideAck(func(context.Context, corenest.CommitFence) error { return lost })
	if err := projector.Flush(ctx); !errors.Is(err, lost) {
		t.Fatalf("flush with a lost checkpoint = %v, want the checkpoint error", err)
	}
	if doc, _ := heroes.Lookup(int64(7)); doc["_version"] != int64(5) || doc["_last_tx"] != record.ID.String() {
		t.Fatalf("premise: the record must be projected, stored=%v", doc)
	}
	if err := projector.WaitEntityProjection(ctx, 7); err != nil {
		t.Fatalf("premise: the entity's projection wait must already pass, got %v", err)
	}
	if stats := projector.Stats(); stats.WALUnacked != 1 {
		t.Fatalf("premise: the record must still be unconfirmed, stats=%+v", stats)
	}
	projector.OverrideAck(wal.Ack)
	return projector, heroes, func() *Projector {
		if err := projector.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if err := wal.Close(ctx); err != nil {
			t.Fatal(err)
		}
		reopened, _ := open()
		return reopened
	}
}

// anotherWriterAdvances 是接手这份文档的另一个进程写的下一版：版本 5 → 6，末次事务是它自己的。
func anotherWriterAdvances(t *testing.T, heroes *mongotest.Collection) {
	t.Helper()
	result, err := heroes.UpdateOne(context.Background(), bson.M{"_id": int64(7), "_version": int64(5)},
		bson.M{"$set": bson.M{"_version": int64(6), "_last_tx": "another-process", "level": int32(9)}})
	if err != nil || result.MatchedCount != 1 {
		t.Fatalf("another writer's update: %+v, %v", result, err)
	}
}

// 刻画现状（FEATURE-2026-11-11-player-rehome §3.2“为什么要等 checkpoint”的依据）：WaitEntityProjection 放行不等于
// checkpoint 已盖过这条记录。单 mutation 快路记录不写事务标记，重放时只靠文档上的 _version 与 _last_tx 认出“已应用”；
// 另一个写者推进文档之后两者都对不上，重放得到 fatal 的 ErrProjectionConflict——同一进程的下一轮回放会 fail-stop，
// 重启后的启动恢复（Runtime.Start 的排空）同样失败、进程起不来。没有别的写者时重放是幂等的；写事务标记的记录
// （多 mutation、带回执 / effect）先查标记，文档被推进后重放仍然安全。
func TestUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument(t *testing.T) {
	ctx := context.Background()
	fastPath := testMutationRecord(coredata.MutationPatch)
	if !projectsOnFastPath(fastPath) {
		t.Fatal("premise: a single local mutation projects on the fast path")
	}

	t.Run("no_other_writer_replays_idempotently", func(t *testing.T) {
		projector, heroes, _ := unconfirmedProjection(t, fastPath)
		if err := projector.Flush(ctx); err != nil {
			t.Fatalf("replay of an applied record nobody else touched = %v", err)
		}
		if doc, _ := heroes.Lookup(int64(7)); doc["_version"] != int64(5) || doc["level"] != int32(5) {
			t.Fatalf("stored after the replay=%v", doc)
		}
		if stats := projector.Stats(); stats.WALUnacked != 0 {
			t.Fatalf("the replay did not confirm the record: %+v", stats)
		}
	})

	t.Run("fast_path_record_in_the_same_process_is_fatal", func(t *testing.T) {
		projector, heroes, _ := unconfirmedProjection(t, fastPath)
		anotherWriterAdvances(t, heroes)
		if err := projector.Flush(ctx); !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("replay after another writer advanced the document = %v, want ErrProjectionConflict", err)
		}
		if projector.fatal() == nil {
			t.Fatal("the conflict did not fence the projector")
		}
		next := testMutationRecord(coredata.MutationPatch)
		next.ID[14] = 1
		if err := projector.Commit(ctx, next); !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("admission after the conflict = %v, want the fatal conflict", err)
		}
		if doc, _ := heroes.Lookup(int64(7)); doc["_version"] != int64(6) || doc["level"] != int32(9) {
			t.Fatalf("the other writer's version was overwritten: %v", doc)
		}
	})

	t.Run("fast_path_record_after_a_restart_is_fatal", func(t *testing.T) {
		_, heroes, reopen := unconfirmedProjection(t, fastPath)
		anotherWriterAdvances(t, heroes)
		restarted := reopen()
		// Runtime.Start 的启动排空就是这一次 Flush；fatal 时不重试，启动失败。
		if err := restarted.Flush(ctx); !errors.Is(err, ErrProjectionConflict) {
			t.Fatalf("startup replay after another writer advanced the document = %v, want ErrProjectionConflict", err)
		}
		if restarted.fatal() == nil {
			t.Fatal("the conflict did not fence the restarted projector")
		}
	})

	t.Run("record_with_a_transaction_marker_replays_safely", func(t *testing.T) {
		marked := testMutationRecord(coredata.MutationPatch)
		marked.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "step-1", Digest: []byte{2}}}
		if projectsOnFastPath(marked) {
			t.Fatal("premise: a record with a receipt does not project on the fast path")
		}
		_, heroes, reopen := unconfirmedProjection(t, marked)
		anotherWriterAdvances(t, heroes)
		restarted := reopen()
		if err := restarted.Flush(ctx); err != nil {
			t.Fatalf("replay of a marked record after another writer advanced the document = %v, want nil", err)
		}
		if doc, _ := heroes.Lookup(int64(7)); doc["_version"] != int64(6) || doc["level"] != int32(9) {
			t.Fatalf("the replay touched the other writer's version: %v", doc)
		}
	})
}

// 以下用例用 entity_unload_test.go 的夹具：正式 Nest（业务池、锁内 WAL 准入、undo 回滚）、真实文件 WAL、共享冷加载、
// Mongo 替身、手动回放的投影器（记录准入之后、Flush 之前一直在途）。

type handOffOutcome struct {
	handOff *EntityHandOff
	err     error
}

func (f *unloadFixture) handOffAsync(timeout time.Duration) <-chan handOffOutcome {
	result := make(chan handOffOutcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(f.ctx, timeout)
		defer cancel()
		handOff, err := f.runtime.HandOffEntity(ctx, f.id)
		result <- handOffOutcome{handOff, err}
	}()
	return result
}

func receiveHandOff(t *testing.T, outcome <-chan handOffOutcome) handOffOutcome {
	t.Helper()
	select {
	case got := <-outcome:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("HandOffEntity did not return")
		return handOffOutcome{}
	}
}

// refusedAsHandedOff：写入在 WAL 之前被交出屏障拒绝——不要重试的 ErrEntityHandedOff，而不是可重试的 ErrEntityUnloading。
func refusedAsHandedOff(err error) bool {
	return errors.Is(err, ErrEntityHandedOff) && errors.Is(err, corenest.ErrCommitRejected) && !errors.Is(err, coredata.ErrEntityUnloading)
}

// 交出的前半段与运维卸载相同：已提交、未投影的修改先落库再卸载，期间的写入以可重试的 ErrEntityUnloading 拒绝。
// 不同的是返回之后屏障保持：写入（先把实体冷加载回来、再在 WAL 准入处）以 ErrEntityHandedOff 拒绝并回滚，库不再被
// 本进程改动；Release 卸载交出期间加载回来的实例、撤屏障，之后照常冷加载、照常写。
func TestEntityHandOffKeepsRefusingWritesUntilReleased(t *testing.T) {
	f := newUnloadFixture(t, 9201)
	if err := f.levelUpRequest(); err != nil {
		t.Fatal(err)
	}
	outcome := f.handOffAsync(5 * time.Second)
	f.waitBarrier(1)
	// 交出进行中：结论未定（超时会撤销屏障），写入拿到的是可重试的错误。
	if err := f.levelUpRequest(); !errors.Is(err, coredata.ErrEntityUnloading) || errors.Is(err, ErrEntityHandedOff) {
		t.Fatalf("write while the hand-off is in progress = %v, want the retryable ErrEntityUnloading", err)
	}
	select {
	case got := <-outcome:
		t.Fatalf("HandOffEntity returned before the admitted write was projected: %+v, %v", got.handOff, got.err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	got := receiveHandOff(t, outcome)
	if got.err != nil || got.handOff == nil || !got.handOff.Resident || !got.handOff.Unloaded || got.handOff.Drained != 1 || got.handOff.AlreadyHandedOff {
		t.Fatalf("HandOffEntity = %+v, %v; want resident, unloaded, drained=1", got.handOff, got.err)
	}
	if f.manager.Get(f.id) != nil {
		t.Fatal("the entity is still resident after the hand-off")
	}
	if level, version := f.storedState(); level != 2 || version != 5 {
		t.Fatalf("stored after the hand-off: level=%d version=%d, want 2/5", level, version)
	}

	// 返回之后：写入仍被拒绝。
	if err := f.levelUpRequest(); !refusedAsHandedOff(err) {
		t.Fatalf("write after HandOffEntity returned = %v, want ErrEntityHandedOff (with ErrCommitRejected, not the retryable ErrEntityUnloading)", err)
	}
	if level, version, ok := f.heroState(); !ok || level != 2 || version != 5 {
		t.Fatalf("the refused write was not rolled back on the instance it cold-loaded: level=%d version=%d resident=%v", level, version, ok)
	}
	if stats := f.projector.Stats(); stats.Committed != 1 || stats.WALUnacked != 0 || stats.HandedOffEntities != 1 ||
		stats.HandOffAdmissionRejected != 1 || stats.UnloadingEntities != 0 || stats.UnloadAdmissionRejected != 1 {
		t.Fatalf("stats while handed off = %+v", stats)
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	if level, version := f.storedState(); level != 2 || version != 5 {
		t.Fatalf("this process changed the document after handing the entity off: level=%d version=%d", level, version)
	}

	// Release：交出期间加载回来的实例可能已落后于库，先卸载再撤屏障。
	if err := got.handOff.Release(f.ctx); err != nil {
		t.Fatalf("Release = %v", err)
	}
	if f.manager.Get(f.id) != nil {
		t.Fatal("Release kept the instance that was loaded while the entity was handed off")
	}
	if stats := f.projector.Stats(); stats.HandedOffEntities != 0 || stats.UnloadingEntities != 0 {
		t.Fatalf("barriers after Release = %+v", stats)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after Release: %v", err)
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	if level, version := f.storedState(); level != 3 || version != 6 {
		t.Fatalf("stored after the write that followed Release: level=%d version=%d, want 3/6", level, version)
	}
}

// 实体不在内存里也立屏障：之后把它冷加载回来的写入、直接写它的记录（新建、删除走的都是同一处准入）都在 WAL 之前被拒。
func TestEntityHandOffBarsAnEntityThatIsNotLoaded(t *testing.T) {
	f := newUnloadFixture(t, 9202)
	if got, err := f.runtime.UnloadEntity(f.ctx, f.id); err != nil || !got.Unloaded {
		t.Fatalf("premise: unload = %+v, %v", got, err)
	}
	handOff, err := f.runtime.HandOffEntity(f.ctx, f.id)
	if err != nil || handOff == nil || handOff.Resident || handOff.Unloaded || handOff.Drained != 0 {
		t.Fatalf("HandOffEntity of an entity that is not loaded = %+v, %v", handOff, err)
	}
	if stats := f.projector.Stats(); stats.HandedOffEntities != 1 {
		t.Fatalf("no barrier was raised for an entity that is not loaded: %+v", stats)
	}
	if err := f.levelUpRequest(); !refusedAsHandedOff(err) {
		t.Fatalf("write that cold-loads the handed-off entity = %v, want ErrEntityHandedOff", err)
	}
	if level, version, ok := f.heroState(); !ok || level != 1 || version != 4 {
		t.Fatalf("after the refused write: level=%d version=%d resident=%v, want the stored 1/4", level, version, ok)
	}

	// 一个本进程从没见过的 ID：交出之后新建它的记录同样被拒，没有进 WAL。
	other, err := entity.BuildEntityID(9299, unloadTestKind)
	if err != nil {
		t.Fatal(err)
	}
	otherHandOff, err := f.runtime.HandOffEntity(f.ctx, other)
	if err != nil || otherHandOff == nil || otherHandOff.Resident {
		t.Fatalf("HandOffEntity of an unknown entity = %+v, %v", otherHandOff, err)
	}
	create := testMutationRecord(coredata.MutationPut)
	create.Mutations[0].Key = coredata.DocumentKey{Database: testDatabase, Resource: unloadTestCollection, ID: other}
	create.Mutations[0].ExpectedVersion, create.Mutations[0].NextVersion = 0, 1
	create.Mutations[0].Data, _ = bson.Marshal(bson.M{"_id": other, "level": int32(1)})
	if err := f.projector.Commit(f.ctx, create); !errors.Is(err, ErrEntityHandedOff) {
		t.Fatalf("record creating a handed-off entity = %v, want ErrEntityHandedOff", err)
	}
	if stats := f.projector.Stats(); stats.Committed != 0 || stats.WALUnacked != 0 {
		t.Fatalf("a refused record reached the WAL: %+v", stats)
	}

	for _, held := range []*EntityHandOff{handOff, otherHandOff} {
		if err := held.Release(f.ctx); err != nil {
			t.Fatalf("Release = %v", err)
		}
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after Release: %v", err)
	}
	if err := f.projector.Commit(f.ctx, create); err != nil {
		t.Fatalf("create after Release: %v", err)
	}
	f.projector.TransactionReleased(create.ID)
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	if level, version := f.storedState(); level != 2 || version != 5 {
		t.Fatalf("stored after Release: level=%d version=%d, want 2/5", level, version)
	}
}

// 等 checkpoint：实体的记录已投影、checkpoint 没盖过它时交出不返回（TestUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument
// 说明了为什么：下一个写者推进文档之后重放这条记录是 fatal）。checkpoint 确认之后才返回；此后另一个写者推进文档，
// 本进程再回放也没有这个实体的记录可重放。在期限内等不到时明确失败、实体不受影响、屏障撤销。
func TestEntityHandOffWaitsForTheCheckpointToCoverItsRecords(t *testing.T) {
	lost := errors.New("checkpoint lost")
	// projectedButUnconfirmed：一笔写入已落库，checkpoint 写失败。
	projectedButUnconfirmed := func(t *testing.T, uniqueID int64) *unloadFixture {
		f := newUnloadFixture(t, uniqueID)
		if err := f.levelUpRequest(); err != nil {
			t.Fatal(err)
		}
		f.projector.OverrideAck(func(context.Context, corenest.CommitFence) error { return lost })
		if err := f.projector.Flush(f.ctx); !errors.Is(err, lost) {
			t.Fatalf("flush with a lost checkpoint = %v", err)
		}
		if level, version := f.storedState(); level != 2 || version != 5 {
			t.Fatalf("premise: the write must be projected, stored=%d/%d", level, version)
		}
		if err := f.projector.WaitEntityProjection(f.ctx, f.id); err != nil {
			t.Fatalf("premise: the projection wait must already pass, got %v", err)
		}
		return f
	}

	t.Run("returns_only_after_the_checkpoint", func(t *testing.T) {
		f := projectedButUnconfirmed(t, 9203)
		outcome := f.handOffAsync(5 * time.Second)
		select {
		case got := <-outcome:
			t.Fatalf("HandOffEntity returned while the checkpoint does not cover the entity's projected record (%+v, %v): "+
				"replaying it after the next owner writes is a fatal projection conflict", got.handOff, got.err)
		case <-time.After(100 * time.Millisecond):
		}
		f.waitBarrier(1)
		if err := f.levelUpRequest(); !errors.Is(err, coredata.ErrEntityUnloading) {
			t.Fatalf("write while the hand-off waits for the checkpoint = %v, want ErrEntityUnloading", err)
		}
		f.projector.OverrideAck(f.projector.wal.Ack)
		if err := f.projector.Flush(f.ctx); err != nil {
			t.Fatal(err)
		}
		got := receiveHandOff(t, outcome)
		if got.err != nil || got.handOff == nil || !got.handOff.Unloaded || got.handOff.Drained != 0 {
			t.Fatalf("HandOffEntity after the checkpoint = %+v, %v", got.handOff, got.err)
		}
		// 下一个写者推进文档；本进程的回放里已经没有这个实体的记录。
		if _, err := f.docs.UpdateOne(f.ctx, bson.M{"_id": f.id}, bson.M{"$set": bson.M{"_version": int64(6), "_last_tx": "another-process", "level": int32(9)}}); err != nil {
			t.Fatal(err)
		}
		if err := f.projector.Flush(f.ctx); err != nil || f.projector.fatal() != nil {
			t.Fatalf("replay after the next owner advanced the document = %v (fatal=%v)", err, f.projector.fatal())
		}
		if stats := f.projector.Stats(); stats.WALUnacked != 0 || stats.FatalProjectionConflicts != 0 {
			t.Fatalf("stats = %+v", stats)
		}
	})

	t.Run("fails_at_once_when_the_projector_is_fenced", func(t *testing.T) {
		f := projectedButUnconfirmed(t, 9214)
		outcome := f.handOffAsync(5 * time.Second)
		f.waitBarrier(1)
		// 本进程不会再确认任何记录：等待立即以可判别的 fatal 结束，而不是等到期限。
		f.projector.isFatalProjection(fmt.Errorf("%w: injected", ErrProjectionConflict))
		got := receiveHandOff(t, outcome)
		if !errors.Is(got.err, ErrProjectionConflict) || errors.Is(got.err, ErrEntityUnloadTimeout) || got.handOff != nil {
			t.Fatalf("HandOffEntity on a fenced projector = %+v, %v; want the fatal conflict", got.handOff, got.err)
		}
		if stats := f.projector.Stats(); stats.UnloadingEntities != 0 || stats.HandedOffEntities != 0 {
			t.Fatalf("a barrier outlived the failed hand-off: %+v", stats)
		}
	})

	t.Run("times_out_without_touching_the_entity", func(t *testing.T) {
		f := projectedButUnconfirmed(t, 9204)
		got := receiveHandOff(t, f.handOffAsync(100*time.Millisecond))
		if !errors.Is(got.err, ErrEntityUnloadTimeout) || errors.Is(got.err, context.DeadlineExceeded) || got.handOff != nil {
			t.Fatalf("HandOffEntity with an unconfirmed record = %+v, %v; want ErrEntityUnloadTimeout (a known outcome, not a ctx error)", got.handOff, got.err)
		}
		if _, _, ok := f.heroState(); !ok {
			t.Fatal("a timed-out hand-off removed the entity")
		}
		if stats := f.projector.Stats(); stats.UnloadingEntities != 0 || stats.HandedOffEntities != 0 {
			t.Fatalf("a barrier outlived the failed hand-off: %+v", stats)
		}
		if err := f.levelUpRequest(); err != nil {
			t.Fatalf("write after the failed hand-off: %v", err)
		}
		f.projector.OverrideAck(f.projector.wal.Ack)
		if err := f.projector.Flush(f.ctx); err != nil {
			t.Fatal(err)
		}
		if level, version := f.storedState(); level != 3 || version != 6 {
			t.Fatalf("stored = %d/%d, want 3/6", level, version)
		}
	})
}

// 重复交出是幂等的：返回同一份交出（AlreadyHandedOff），期间被加载回来的实例再卸载一次；任一句柄 Release 即解除，
// 再 Release 什么也不做。旧句柄不能解除之后的另一次交出。nil 句柄（交出失败时的返回值）可以直接 Release。
func TestEntityHandOffRepeatedAndReleasedTwice(t *testing.T) {
	f := newUnloadFixture(t, 9205)
	first, err := f.runtime.HandOffEntity(f.ctx, f.id)
	if err != nil || first == nil || first.AlreadyHandedOff {
		t.Fatalf("first hand-off = %+v, %v", first, err)
	}
	if _, err := f.access.Get(f.ctx, f.id, entity.EntityCategoryNone); err != nil || f.manager.Get(f.id) == nil {
		t.Fatalf("premise: a read loads the handed-off entity back, err=%v", err)
	}
	second, err := f.runtime.HandOffEntity(f.ctx, f.id)
	if err != nil || second == nil || !second.AlreadyHandedOff || !second.Resident || !second.Unloaded {
		t.Fatalf("repeated hand-off = %+v, %v; want the same hand-off with the reloaded instance unloaded again", second, err)
	}
	if f.manager.Get(f.id) != nil {
		t.Fatal("the repeated hand-off left the reloaded instance resident")
	}
	if stats := f.projector.Stats(); stats.HandedOffEntities != 1 || stats.UnloadingEntities != 0 {
		t.Fatalf("barriers after the repeated hand-off = %+v", stats)
	}

	if err := second.Release(f.ctx); err != nil {
		t.Fatalf("Release through the second handle = %v", err)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after Release: %v", err)
	}
	for _, again := range []*EntityHandOff{second, nil} {
		if err := again.Release(f.ctx); err != nil {
			t.Fatalf("Release of an already released (or nil) hand-off = %v", err)
		}
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after releasing twice: %v", err)
	}
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}

	// 新的一次交出不被旧句柄解除：first 那份交出是经 second 解除的，first 自己还没有 Release 过。
	third, err := f.runtime.HandOffEntity(f.ctx, f.id)
	if err != nil || third == nil || third.AlreadyHandedOff {
		t.Fatalf("hand-off after the release = %+v, %v", third, err)
	}
	if err := first.Release(f.ctx); err != nil {
		t.Fatalf("stale Release = %v", err)
	}
	if err := f.levelUpRequest(); !refusedAsHandedOff(err) {
		t.Fatalf("write after a stale handle's Release = %v, want the newer hand-off to still refuse it", err)
	}
	if err := third.Release(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after the last Release: %v", err)
	}
}

// 明确的拒绝，都不留屏障：ctx 已取消；业务 worker 上调用（会等待，fail-fast 而不是死锁，Release 同样）；等待中 ctx 被取消；
// 不是持久化的实体；不是完整 Entity ID；Remote 托管实体（归属由 Remote 运行时的迁移负责）。
func TestEntityHandOffRefusals(t *testing.T) {
	f := newUnloadFixture(t, 9206)
	noBarrier := func(when string) {
		t.Helper()
		if stats := f.projector.Stats(); stats.UnloadingEntities != 0 || stats.HandedOffEntities != 0 {
			t.Fatalf("%s left a barrier: %+v", when, stats)
		}
		if f.manager.Get(f.id) == nil {
			t.Fatalf("%s unloaded the entity", when)
		}
	}

	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if handOff, err := f.runtime.HandOffEntity(canceled, f.id); !errors.Is(err, context.Canceled) || handOff != nil {
		t.Fatalf("HandOffEntity with a canceled ctx = %+v, %v; want context.Canceled and nothing done", handOff, err)
	}
	noBarrier("a canceled hand-off")

	var onWorker error
	var workerHandOff *EntityHandOff
	if err := f.scheduler.RunLocal(f.ctx, func() { workerHandOff, onWorker = f.runtime.HandOffEntity(f.ctx, f.id) }); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(onWorker, fctx.ErrBlockingInBusinessWorker) || workerHandOff != nil {
		t.Fatalf("HandOffEntity on a business worker = %+v, %v; want ErrBlockingInBusinessWorker", workerHandOff, onWorker)
	}
	noBarrier("a hand-off refused on a business worker")

	// 等投影时 ctx 被取消（不是到期）：同样是确定的“没有交出”。
	if err := f.levelUpRequest(); err != nil {
		t.Fatal(err)
	}
	waiting, stop := context.WithCancel(f.ctx)
	interrupted := make(chan handOffOutcome, 1)
	go func() {
		handOff, err := f.runtime.HandOffEntity(waiting, f.id)
		interrupted <- handOffOutcome{handOff, err}
	}()
	f.waitBarrier(1)
	stop()
	if got := receiveHandOff(t, interrupted); !errors.Is(got.err, ErrEntityUnloadTimeout) || got.handOff != nil {
		t.Fatalf("HandOffEntity canceled while waiting for the projection = %+v, %v; want ErrEntityUnloadTimeout", got.handOff, got.err)
	}
	noBarrier("a hand-off canceled while waiting")
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}

	f.manager.Get(f.id).(*unloadHero).persist = false
	if handOff, err := f.runtime.HandOffEntity(f.ctx, f.id); !errors.Is(err, ErrEntityUnloadNotPersistent) || handOff != nil {
		t.Fatalf("HandOffEntity of a non-persistent entity = %+v, %v; want ErrEntityUnloadNotPersistent", handOff, err)
	}
	noBarrier("a refused hand-off of a non-persistent entity")
	f.manager.Get(f.id).(*unloadHero).persist = true

	// 不是完整 Entity ID（只传了唯一号）：屏障会立在没有记录会写的 ID 上，拒绝。
	if handOff, err := f.runtime.HandOffEntity(f.ctx, 9206); !errors.Is(err, entity.ErrInvalidEntityID) || handOff != nil {
		t.Fatalf("HandOffEntity of a bare unique id = %+v, %v; want ErrInvalidEntityID", handOff, err)
	}
	noBarrier("a refused hand-off of an invalid id")

	remoteID, err := entity.BuildEntityID(9206, unloadRemoteTestKind)
	if err != nil {
		t.Fatal(err)
	}
	if handOff, err := f.runtime.HandOffEntity(f.ctx, remoteID); !errors.Is(err, ErrEntityHandOffUnsupported) || handOff != nil {
		t.Fatalf("HandOffEntity of a remote-managed entity = %+v, %v; want ErrEntityHandOffUnsupported", handOff, err)
	}
	noBarrier("a refused hand-off of a remote-managed entity")

	// Release 也会等待：业务 worker 上拒绝、屏障保持，之后在业务池外照常解除。
	handOff, err := f.runtime.HandOffEntity(f.ctx, f.id)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnWorker error
	if err := f.scheduler.RunLocal(f.ctx, func() { releaseOnWorker = handOff.Release(f.ctx) }); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(releaseOnWorker, fctx.ErrBlockingInBusinessWorker) {
		t.Fatalf("Release on a business worker = %v, want ErrBlockingInBusinessWorker", releaseOnWorker)
	}
	if err := f.levelUpRequest(); !refusedAsHandedOff(err) {
		t.Fatalf("write after a Release that was refused on a business worker = %v, want ErrEntityHandedOff", err)
	}
	if err := handOff.Release(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.levelUpRequest(); err != nil {
		t.Fatalf("write after Release: %v", err)
	}
}

// 与运维卸载（entity.unload）互斥而不互相破坏：一方进行中另一方按忙拒绝；交出之后对加载回来的实例做普通卸载，
// 卸载返回时撤的是它自己的屏障，交出屏障还在。
func TestEntityHandOffAndOrdinaryUnload(t *testing.T) {
	t.Run("hand_off_during_an_unload_is_busy", func(t *testing.T) {
		f := newUnloadFixture(t, 9207)
		release, err := f.projector.beginEntityUnload(f.id)
		if err != nil {
			t.Fatal(err)
		}
		if handOff, err := f.runtime.HandOffEntity(f.ctx, f.id); !errors.Is(err, ErrEntityUnloadBusy) || handOff != nil {
			t.Fatalf("HandOffEntity during an unload = %+v, %v; want ErrEntityUnloadBusy", handOff, err)
		}
		release()
		if stats := f.projector.Stats(); stats.HandedOffEntities != 0 || f.manager.Get(f.id) == nil {
			t.Fatalf("the refused hand-off changed something: %+v resident=%v", stats, f.manager.Get(f.id) != nil)
		}
	})
	t.Run("unload_during_a_hand_off_is_busy", func(t *testing.T) {
		f := newUnloadFixture(t, 9208)
		if err := f.levelUpRequest(); err != nil {
			t.Fatal(err)
		}
		outcome := f.handOffAsync(5 * time.Second)
		f.waitBarrier(1)
		if _, err := f.runtime.UnloadEntity(f.ctx, f.id); !errors.Is(err, ErrEntityUnloadBusy) {
			t.Fatalf("UnloadEntity during a hand-off = %v, want ErrEntityUnloadBusy", err)
		}
		if err := f.projector.Flush(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := receiveHandOff(t, outcome); got.err != nil || got.handOff == nil || !got.handOff.Unloaded {
			t.Fatalf("the hand-off was disturbed by the refused unload: %+v, %v", got.handOff, got.err)
		}
		if err := f.levelUpRequest(); !refusedAsHandedOff(err) {
			t.Fatalf("write after the hand-off = %v, want ErrEntityHandedOff", err)
		}
	})
	t.Run("unload_after_a_hand_off_keeps_the_barrier", func(t *testing.T) {
		f := newUnloadFixture(t, 9209)
		handOff, err := f.runtime.HandOffEntity(f.ctx, f.id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.access.Get(f.ctx, f.id, entity.EntityCategoryNone); err != nil {
			t.Fatal(err)
		}
		if got, err := f.runtime.UnloadEntity(f.ctx, f.id); err != nil || !got.Resident || !got.Unloaded {
			t.Fatalf("UnloadEntity of the reloaded instance = %+v, %v", got, err)
		}
		if stats := f.projector.Stats(); stats.HandedOffEntities != 1 || stats.UnloadingEntities != 0 {
			t.Fatalf("barriers after the ordinary unload = %+v, want the hand-off still held", stats)
		}
		if err := f.levelUpRequest(); !refusedAsHandedOff(err) {
			t.Fatalf("write after an ordinary unload of a handed-off entity = %v, want ErrEntityHandedOff", err)
		}
		if err := handOff.Release(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := f.levelUpRequest(); err != nil {
			t.Fatalf("write after Release: %v", err)
		}
	})
}

// 并发写入下交出（-race）：投影在后台回放。交出返回时，此前成功的每一笔写入都在库里，此后没有一笔写入成功；
// Release 之后写入恢复，最终库里的值等于成功的写入数。
func TestEntityHandOffUnderConcurrentWrites(t *testing.T) {
	f := newUnloadFixture(t, 9210)
	stopReplay := make(chan struct{})
	var replay sync.WaitGroup
	replay.Add(1)
	go func() {
		// 与生产的后台投影循环同一个入口：遇到仍持有实体锁的事务就停下、稍后再来。
		defer replay.Done()
		for {
			select {
			case <-stopReplay:
				return
			default:
			}
			if _, err := f.projector.ReplayPass(f.ctx); err != nil && !errors.Is(err, errProjectorTransactionHeld) {
				t.Errorf("replay: %v", err)
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	var succeeded, refused atomic.Int32
	stopWriters := make(chan struct{})
	var writers sync.WaitGroup
	for range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stopWriters:
					return
				default:
				}
				switch err := f.levelUpRequest(); {
				case err == nil:
					succeeded.Add(1)
				case refusedAsHandedOff(err):
					refused.Add(1)
				case errors.Is(err, coredata.ErrEntityUnloading):
				default:
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	waitFor := func(what string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFor("some writes to succeed", func() bool { return succeeded.Load() >= 20 })

	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	handOff, err := f.runtime.HandOffEntity(ctx, f.id)
	if err != nil || handOff == nil {
		t.Fatalf("HandOffEntity under load = %+v, %v", handOff, err)
	}
	level, version := f.storedState()
	time.Sleep(20 * time.Millisecond) // 屏障之前已提交的写入此刻都已回到调用方并计数
	admitted := succeeded.Load()
	if level != 1+admitted || version != uint64(4+admitted) {
		t.Fatalf("stored when HandOffEntity returned = %d/%d, want every one of the %d acknowledged writes (%d/%d)", level, version, admitted, 1+admitted, 4+admitted)
	}
	waitFor("writes refused as handed off", func() bool { return refused.Load() > 0 })
	time.Sleep(20 * time.Millisecond)
	if got := succeeded.Load(); got != admitted {
		t.Fatalf("%d write(s) were admitted after HandOffEntity returned", got-admitted)
	}
	if level, version := f.storedState(); level != 1+admitted || version != uint64(4+admitted) {
		t.Fatalf("the document changed while the entity was handed off: %d/%d", level, version)
	}

	if err := handOff.Release(ctx); err != nil {
		t.Fatalf("Release under load = %v", err)
	}
	waitFor("writes to resume after Release", func() bool { return succeeded.Load() > admitted })
	close(stopWriters)
	writers.Wait()
	close(stopReplay)
	replay.Wait()
	if err := f.projector.Flush(f.ctx); err != nil {
		t.Fatal(err)
	}
	total := succeeded.Load()
	if level, version := f.storedState(); level != 1+total || version != uint64(4+total) {
		t.Fatalf("stored at the end = %d/%d, want %d/%d", level, version, 1+total, 4+total)
	}
}

// 停机：持有屏障时 Runtime.Shutdown 照常排空并结束（屏障只是内存里的登记，不挡停机），之后 Release 什么也不用做、
// 新的交出被拒绝；交出还在等待（投影或 checkpoint）时投影器关闭，交出以 ErrRuntimeStopped 明确失败、不留屏障。
func TestEntityHandOffAcrossShutdown(t *testing.T) {
	t.Run("barrier_held_at_shutdown", func(t *testing.T) {
		f := newUnloadFixture(t, 9211)
		if err := f.levelUpRequest(); err != nil {
			t.Fatal(err)
		}
		outcome := f.handOffAsync(5 * time.Second)
		f.waitBarrier(1)
		if err := f.projector.Flush(f.ctx); err != nil {
			t.Fatal(err)
		}
		got := receiveHandOff(t, outcome)
		if got.err != nil {
			t.Fatal(got.err)
		}
		if err := f.scheduler.Shutdown(f.ctx); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
		defer cancel()
		if err := f.runtime.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown while a hand-off barrier is held = %v", err)
		}
		if level, version := f.storedState(); level != 2 || version != 5 {
			t.Fatalf("stored after shutdown = %d/%d, want 2/5", level, version)
		}
		if err := got.handOff.Release(ctx); err != nil {
			t.Fatalf("Release after shutdown = %v", err)
		}
		if handOff, err := f.runtime.HandOffEntity(ctx, f.id); !errors.Is(err, coredata.ErrRecoveryIncomplete) || handOff != nil {
			t.Fatalf("HandOffEntity after shutdown = %+v, %v; want ErrRecoveryIncomplete", handOff, err)
		}
	})
	t.Run("stopped_while_waiting_for_the_projection", func(t *testing.T) {
		f := newUnloadFixture(t, 9212)
		if err := f.levelUpRequest(); err != nil {
			t.Fatal(err)
		}
		outcome := f.handOffAsync(5 * time.Second)
		f.waitBarrier(1)
		if err := f.projector.Close(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := receiveHandOff(t, outcome); !errors.Is(got.err, ErrRuntimeStopped) || got.handOff != nil {
			t.Fatalf("HandOffEntity interrupted by shutdown = %+v, %v; want ErrRuntimeStopped", got.handOff, got.err)
		}
		if stats := f.projector.Stats(); stats.UnloadingEntities != 0 || stats.HandedOffEntities != 0 {
			t.Fatalf("barriers after the interrupted hand-off = %+v", stats)
		}
	})
	t.Run("stopped_while_waiting_for_the_checkpoint", func(t *testing.T) {
		f := newUnloadFixture(t, 9213)
		if err := f.levelUpRequest(); err != nil {
			t.Fatal(err)
		}
		lost := errors.New("checkpoint lost")
		f.projector.OverrideAck(func(context.Context, corenest.CommitFence) error { return lost })
		if err := f.projector.Flush(f.ctx); !errors.Is(err, lost) {
			t.Fatalf("flush = %v", err)
		}
		outcome := f.handOffAsync(5 * time.Second)
		select {
		case got := <-outcome:
			t.Fatalf("premise: the hand-off must be waiting for the checkpoint, got %+v, %v", got.handOff, got.err)
		case <-time.After(50 * time.Millisecond):
		}
		if err := f.projector.Close(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := receiveHandOff(t, outcome); !errors.Is(got.err, ErrRuntimeStopped) || got.handOff != nil {
			t.Fatalf("HandOffEntity interrupted by shutdown while waiting for the checkpoint = %+v, %v; want ErrRuntimeStopped", got.handOff, got.err)
		}
		if stats := f.projector.Stats(); stats.UnloadingEntities != 0 || stats.HandedOffEntities != 0 {
			t.Fatalf("barriers after the interrupted hand-off = %+v", stats)
		}
	})
}

// 交出时实体还有 Sync 订阅者（调用方没有先断开）：卸载触发卸载后重载（unload_resync），订阅者收到库里的全量，实例重新
// 常驻——但它是只读的，写入仍在 WAL 之前被拒。Release 把这个实例卸载（它可能已落后于下一个写者写进库的内容），订阅者
// 再收到一次从库重载的全量，之后写入恢复。
func TestEntityHandOffWithSubscribersReloadsButKeepsRefusingWrites(t *testing.T) {
	f := newUnloadResyncFixture(t, 2433)
	ctx := context.Background()
	patch := func(sequence byte, from uint64, level int32) coredata.CommitRecord {
		record := testMutationRecord(coredata.MutationPatch)
		record.ID[14] = sequence
		record.Mutations[0].Key = coredata.DocumentKey{Database: testDatabase, Resource: unloadResyncCollection, ID: f.id}
		record.Mutations[0].ExpectedVersion, record.Mutations[0].NextVersion, record.Mutations[0].Schema = from, from+1, 1
		record.Mutations[0].Patch.SetBSON, _ = bson.Marshal(bson.D{{Key: "level", Value: level}})
		return record
	}

	handOff, err := f.runtime.HandOffEntity(ctx, f.id)
	if err != nil || handOff == nil || !handOff.Resident || !handOff.Unloaded {
		t.Fatalf("HandOffEntity = %+v, %v", handOff, err)
	}
	correction := f.next("the entity was handed off with a subscriber attached, but the subscriber got neither a reload nor a remove")
	if len(correction.Objects) != 1 || correction.Objects[0].Operation != frame.ObjectUpdate || f.level(correction.Objects[0]) != 1 {
		t.Fatalf("frame after the hand-off = %+v, want the stored level 1 as an update", correction.Objects)
	}
	reloaded := f.manager.Get(f.id)
	if reloaded == nil || reloaded == entity.IThreadSafeEntity(f.resident) {
		t.Fatal("premise: the subscriber's reload made a new instance resident")
	}
	if err := f.project.Commit(ctx, patch(1, 4, 2)); !errors.Is(err, ErrEntityHandedOff) {
		t.Fatalf("write to the instance reloaded for subscribers = %v, want ErrEntityHandedOff", err)
	}

	// 下一个写者在这期间把文档推进到 level 7。
	if _, err := f.docs.UpdateOne(ctx, bson.M{"_id": f.id}, bson.M{"$set": bson.M{"level": int32(7), "_version": uint64(5)}}); err != nil {
		t.Fatal(err)
	}
	if err := handOff.Release(ctx); err != nil {
		t.Fatalf("Release = %v", err)
	}
	fresh := f.next("Release unloaded the stale instance, but the subscriber was not resynced from Mongo")
	if len(fresh.Objects) != 1 || f.level(fresh.Objects[0]) != 7 {
		t.Fatalf("frame after Release = %+v, want the stored level 7", fresh.Objects)
	}
	if current := f.manager.Get(f.id); current == nil || current == reloaded {
		t.Fatal("Release kept the instance that was loaded while the entity was handed off")
	}
	write := patch(2, 5, 8)
	if err := f.project.Commit(ctx, write); err != nil {
		t.Fatalf("write after Release = %v", err)
	}
	f.project.TransactionReleased(write.ID)
	if err := f.project.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if doc, _ := f.docs.Lookup(f.id); doc["_version"] != int64(6) || doc["level"] != int32(8) {
		t.Fatalf("stored after the write that followed Release = %v", doc)
	}
}
