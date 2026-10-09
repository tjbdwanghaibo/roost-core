package remoteentity

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// 恢复入口没有写者重投帮忙：一整页历史坏记录不能饿死后面的正常事务。
func TestOutboxFailedFirstPageDoesNotStarveLaterTransaction(t *testing.T) {
	fixture, _, batch := prepareReplayBatch(t, nil)
	defer batch.Abort(context.Background(), nil)
	defer batch.Close(context.Background())
	defer fixture.StopFinalizer(context.Background())
	seed := batch.Commits()[0]
	db := newRemoteMongoFake()
	storage := NewMongoCommitter(db, "outbox_pages", 1000, 0)
	backend, err := NewBackend(newRemoteTestLoader(), storage)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	manager.SetBackend(backend)
	defer manager.StopFinalizer(context.Background())
	// 同一毫秒创建，必须用 _id 作为稳定的第二排序键。
	created := time.UnixMilli(1000)
	var last entity.RemoteTransactionID
	for i := 1; i <= 257; i++ {
		var id entity.RemoteTransactionID
		binary.BigEndian.PutUint64(id[8:], uint64(i))
		commit := seed.Clone()
		commit.TransactionID = id
		receipt := mongoRemoteReceipt{TransactionID: id[:], EntityID: commit.EntityID, StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch, RouteEpoch: commit.RouteEpoch, LockFence: commit.LockFence}
		doc := mongoRemoteTransaction{ID: id.String(), State: uint8(entity.RemoteCommitApplied), CreatedAt: created, Receipts: []mongoRemoteReceipt{receipt}}
		if i == 257 {
			doc.Commits = []entity.RemoteCommit{commit}
			last = id
		}
		if _, err := db.Collection("outbox_pages", remoteTxCollection).InsertOne(context.Background(), doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.RecoverOutbox(context.Background()); err == nil {
		t.Fatal("corrupt pending records were hidden")
	}
	status, err := storage.CommitStatus(context.Background(), last)
	if err != nil || status.State != entity.RemoteCommitCommitted {
		t.Fatalf("later transaction starved: state=%v err=%v", status.State, err)
	}
	pending, err := storage.PendingRemoteCommits(context.Background(), 1000)
	if err != nil || len(pending) != 256 {
		t.Fatalf("failed records must remain pending: count=%d err=%v", len(pending), err)
	}
}
