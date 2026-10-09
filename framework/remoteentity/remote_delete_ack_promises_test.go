package remoteentity

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// RR-20261006-01（Mirror 第 6 步本机替代时发现）：Remote 实体的删除提交到了权威，确认却失败。
//
// 删除随事务走 deferEntityDelete：准入之后实例就从内存删掉，实体管理器把它清空（ClearBase → ID 归零）；
// strict / pipelined 的 Remote 提交与确认在投影器里稍后进行。Manager 的登记还挂着这个被清空的实例，
// acknowledgeRemoteCommit 把删除提交交给它的 AcknowledgeRemoteCommit，生成代码按身份核对（commit.EntityID
// != e.GUId()）返回 "remote acknowledgement identity mismatch"。afterRemoteCommit 在确认处返回错误，
// 发布（L2 墓碑 + 推送删除）整个被跳过：调用方拿到“结果未知”，Mongo 已删除，L2 仍是删除前的快照，
// 只读方（Mirror）陈旧上限到了重新确认也只会从 L2 读回旧版本，直到 L2 TTL 过期。
//
// generatedAckEntity 用生成代码同一条确认规则（codegen/internal/entity/gen.go 的 AcknowledgeRemoteCommit 模板）。
type generatedAckEntity struct{ *testRemoteEntity }

func (e generatedAckEntity) AcknowledgeRemoteCommit(commit entity.RemoteCommit) error {
	if commit.EntityID != e.GUId() {
		return fmt.Errorf("Guild: remote acknowledgement identity mismatch")
	}
	return e.testRemoteEntity.AcknowledgeRemoteCommit(commit)
}

type recordedSnapshotDelete struct {
	key     entity.RemoteSnapshotKey
	version uint64
}

type recordingSnapshotPublisher struct {
	mu      sync.Mutex
	deletes []recordedSnapshotDelete
}

func (*recordingSnapshotPublisher) PublishRemoteSnapshot(context.Context, entity.RemoteSnapshotRecord) error {
	return nil
}

func (p *recordingSnapshotPublisher) DeleteRemoteSnapshot(_ context.Context, key entity.RemoteSnapshotKey, version uint64) error {
	p.mu.Lock()
	p.deletes = append(p.deletes, recordedSnapshotDelete{key: key, version: version})
	p.mu.Unlock()
	return nil
}

func (*recordingSnapshotPublisher) PublishRemoteInterest(context.Context, entity.RemoteSnapshotInterest, bool) error {
	return nil
}

// 删除提交确认时实例已被清空：提交照常成功、发布删除（缓存墓碑 + 推送），不把被清空的实例当成这个实体去确认。
func TestRemoteDeleteCommitPublishesItsTombstoneAfterTheInstanceIsCleared(t *testing.T) {
	// kind 241：本包测试已用 12、91～96、119～135、143、144、196～198、213、217、236、237、243～245、249～254，不能撞号。
	const kind entity.EntityKind = 241
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	ctx := context.Background()
	mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	loader := newRemoteTestLoader()
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	publisher := &recordingSnapshotPublisher{}
	mgr.snapshots.transport = publisher
	live := generatedAckEntity{newTestRemoteEntity(4101, 1, kind)}
	loader.add(live)
	id := live.GUId()
	key := entity.RemoteSnapshotKey{EntityID: id, Kind: kind, Scope: 1}

	batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	outcome := entity.NewRemoteTransactionOutcome(remoteTestTxID(41), "delete", "", true, 0)
	outcome.DeleteIntents = batchDeleteIntent(id)
	if err := batch.FinalizeLocked(outcome); err != nil {
		t.Fatal(err)
	}
	commits := batch.Commits()
	if len(commits) != 1 || !commits[0].Delete {
		t.Fatalf("commits=%+v, want one delete commit", commits)
	}
	// 事务的删除已经把实例从内存删掉：实体管理器对它执行的就是 ClearBase（ID 归零）。
	live.ClearBase()
	if live.GUId() == id {
		t.Fatal("ClearBase did not reset the instance; the test no longer models the destroyed entity")
	}
	receipts, err := batch.Commit(ctx)
	if err != nil {
		t.Fatalf("the delete reached the authority but its acknowledgement failed: %v", err)
	}
	if len(receipts) != 1 || receipts[0].EntityID != id {
		t.Fatalf("receipts=%+v", receipts)
	}
	publisher.mu.Lock()
	deletes := append([]recordedSnapshotDelete(nil), publisher.deletes...)
	publisher.mu.Unlock()
	if len(deletes) != 1 || deletes[0].key != key || deletes[0].version != commits[0].NextVersion {
		t.Fatalf("published deletes=%+v, want the tombstone of %v at version %d", deletes, key, commits[0].NextVersion)
	}
	if got, found, err := mgr.ReadRemoteSnapshot(ctx, key, entity.RemoteReadCached, 0); err != nil || found {
		t.Fatalf("after the delete the cache reads found=%v version=%d err=%v, want the tombstone", found, got.StateVersion, err)
	}
	if err := batch.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
