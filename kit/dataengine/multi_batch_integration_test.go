//go:build integration

package dataengine

import (
	"context"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func realLocalMultiRecords(t *testing.T, fx *realFixture) []coredata.CommitRecord {
	var records []coredata.CommitRecord
	for i := range 4 {
		// 两个 Entity，各有两个 DAO；跨集合且持续修改同一文档。
		var mutations []coredata.Mutation
		for _, collection := range []string{"a_wallets", "z_inventories"} {
			for _, id := range []int64{1, 2} {
				mutations = append(mutations, realPut(t, fx.database, collection, id, uint64(i), uint64(i+1), bson.M{"value": i + 1}))
			}
		}
		records = append(records, realRecord(byte(120+i), mutations))
	}
	return records
}

// manualRealProjector 返回不启动后台循环的 Projector，由测试逐步调用 ReplayPass / Flush。
// RR-20260926-29：RR-17 后 Close 会拒绝后续 ReplayPass，不能再用“先 Close 停循环”的办法。
func manualRealProjector(t *testing.T, wal *nestwal.WAL, store engine.ProjectionStore) *engine.Projector {
	t.Helper()
	p, err := engine.NewProjector(wal, store, engine.ProjectorOptions{CloseWAL: false, CheckpointInterval: time.Hour, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func TestRealLocalMultiBatchOrderIdentityAndLostCheckpoint(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	records := realLocalMultiRecords(t, fx)
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wal.Close(context.Background()) }()
	p := manualRealProjector(t, wal, fx.runtime.Store)
	for _, r := range records {
		if _, err = wal.Append(fx.context(), r); err != nil {
			t.Fatal(err)
		}
	}
	ackLost := errors.New("injected lost checkpoint after batch")
	p.OverrideAck(func(context.Context, corenest.CommitFence) error { return ackLost })
	if n, err := p.ReplayPass(fx.context()); n != 4 || !errors.Is(err, ackLost) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, c := range []string{"a_wallets", "z_inventories"} {
		for _, id := range []int64{1, 2} {
			assertDocumentVersion(t, fx, c, id, 4)
		}
	}
	assertCollectionCount(t, fx, engine.TransactionCollection, 4)
	assertWALReplayCount(t, wal, 4)
	if err = wal.Close(fx.context()); err != nil {
		t.Fatal(err)
	}
	wal, err = nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	// 已提交 marker 必须允许在当前版本 4 时重放版本 1..4。
	p = manualRealProjector(t, wal, fx.runtime.Store)
	if err = p.Flush(fx.context()); err != nil {
		t.Fatal(err)
	}
	assertWALReplayCount(t, wal, 0)
	assertCollectionCount(t, fx, engine.TransactionCollection, 4)
	changed := coredata.CloneCommitRecord(records[0])
	changed.Handler = "different-payload"
	if err = fx.runtime.Store.ProjectBatch(fx.context(), []coredata.CommitRecord{changed, records[1]}); !errors.Is(err, engine.ErrTransactionIdentity) {
		t.Fatalf("identity=%v", err)
	}
}

func TestRealLocalMultiBatchLateConflictRollsBackThenAcknowledgesPrefix(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	records := realLocalMultiRecords(t, fx)
	// 第三笔的最后一个 DAO 冲突，前两笔与第四笔均不可提前提交。
	records[2].Mutations[3].ExpectedVersion = 8
	records[2].Mutations[3].NextVersion = 9
	if err := fx.runtime.Store.ProjectBatch(fx.context(), records); !errors.Is(err, engine.ErrProjectionBatchNeedsPerRecord) {
		t.Fatalf("batch=%v", err)
	}
	for _, c := range []string{"a_wallets", "z_inventories", engine.TransactionCollection} {
		assertCollectionCount(t, fx, c, 0)
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer wal.Close(context.Background())
	p := manualRealProjector(t, wal, fx.runtime.Store)
	for _, r := range records {
		if _, err = wal.Append(fx.context(), r); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := p.ReplayPass(fx.context()); n != 2 || !errors.Is(err, engine.ErrProjectionConflict) {
		t.Fatalf("fallback n=%d err=%v", n, err)
	}
	for _, c := range []string{"a_wallets", "z_inventories"} {
		for _, id := range []int64{1, 2} {
			assertDocumentVersion(t, fx, c, id, 2)
		}
	}
	assertCollectionCount(t, fx, engine.TransactionCollection, 2)
	assertWALReplayCount(t, wal, 2)
	if p.Stats().FatalProjectionConflicts != 1 {
		t.Fatalf("stats=%+v", p.Stats())
	}
}
