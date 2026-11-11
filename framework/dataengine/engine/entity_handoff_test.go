package engine

import (
	"context"
	"errors"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

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
