package remoteentity

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
)

// OPEN-ITEMS B09：RR-20260926-19 端到端——历史 WAL 里“Remote + 过期 lease fence”的混合记录。
//
// 新写入在准入处已被 engine.ErrRemoteLeaseFenceUnsupported 拒绝（用例先证明这一点），只剩升级前写进 WAL 的历史记录会走到投影。
// 这里绕过准入直接把记录追加进真实 nestwal WAL，再由 engine.Projector + engine.MongoStore（mongotest，与 MongoCommitter 同一个库）
// 投影：lease 条件写不命中 → 同一 Mongo 事务写入 digest 绑定的 Remote Rejected 记录与 skipped marker → 通知真实 Manager。
//
// 验证：WAL ack、无 fatal；持久状态与 tracker 都是 Rejected；gate / 写额度释放；被拒绝的旧实例经正式 ManagerAccess 仅内存卸载，
// 重载后等于权威且可写，被拒绝的内存修改没有进入 Mongo（沿用 RR-39 的 assertReloadedFromAuthority）。
func TestHistoricalLeaseFencedRemoteRecordRejectsAndReloads(t *testing.T) {
	f, live := newReloadFixture(t, 1957)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	store, err := engine.NewMongoStore(f.store.mongo, engine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 1000, TransactionReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetRemoteProjection(f.store, f.mgr); err != nil {
		t.Fatal(err)
	}
	walOptions := nestwal.DefaultOptions(t.TempDir())
	walOptions.GroupCommitInterval = time.Millisecond
	wal, err := nestwal.Open(walOptions)
	if err != nil {
		t.Fatal(err)
	}
	walOwned := true
	t.Cleanup(func() {
		if walOwned {
			_ = wal.Close(context.Background())
		}
	})

	tx := remoteTestTxID(0xD1)
	batch := f.prepareRejected(t, live, tx, uint8(nest.DurabilityAsync.Record()))
	record := remoteCommitRecord(nest.DurabilityAsync.Record(), batch.Commits())
	// 指向不存在的 saga 步骤 claim：条件写 matched=0，即租约已失效。
	fence, err := coredata.NewLeaseFenceReceipt(coredata.LeaseFence{
		Database: "game", Resource: "_dataengine_inbox_claims", DocumentID: "saga-step/historical",
		Owner: "worker-old", Token: 3, Digest: bytes.Repeat([]byte{7}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	record.Receipts = []coredata.Receipt{fence}
	if err = coredata.ValidateCommitRecord(record); err != nil {
		t.Fatalf("premise: historical record must be a valid WAL record: %v", err)
	}
	if _, err = wal.Append(ctx, record); err != nil {
		t.Fatal(err)
	}

	projector, err := engine.NewProjector(wal, store, engine.ProjectorOptions{
		CloseWAL: true, IdlePoll: 20 * time.Millisecond, RetryMin: 5 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	walOwned = false
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = projector.Close(closing)
	})
	// 前提：同样的组合现在走正式准入会在写 WAL 前被拒绝。
	probe := record
	probe.ID = coredata.TransactionID(remoteTestTxID(0xD2))
	if err = projector.Commit(ctx, probe); !errors.Is(err, engine.ErrRemoteLeaseFenceUnsupported) {
		t.Fatalf("premise: admission of Remote + lease fence = %v, want ErrRemoteLeaseFenceUnsupported", err)
	}

	if _, err = batch.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = projector.Flush(ctx); err != nil {
		t.Fatalf("projector flush: %v", err)
	}
	if stats := projector.Stats(); stats.WALUnacked != 0 || stats.FatalProjectionConflicts != 0 || stats.ProjectionFailures != 0 {
		t.Fatalf("projector stats=%+v, want the skipped record acknowledged without failures", stats)
	}
	if status, err := f.store.CommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitRejected {
		t.Fatalf("durable status=%+v err=%v, want Rejected", status, err)
	}
	if status, err := f.mgr.RemoteCommitStatus(ctx, tx); err != nil || status.State != entity.RemoteCommitRejected {
		t.Fatalf("tracker status=%+v err=%v, want Rejected", status, err)
	}
	f.assertReloadedFromAuthority(t, live)
	if active := f.mgr.Stats().ActiveTransactions; active != 0 {
		t.Fatalf("active transactions=%d after rejection and the next write", active)
	}
}
