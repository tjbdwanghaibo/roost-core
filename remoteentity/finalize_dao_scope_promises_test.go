package remoteentity

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260927-09（OPEN-ITEMS A10）：RR-20260926-45 只在装配期按注册的 DaoBuilders 拦下“remote=managed + dbscope=sid”。
// 手写实体注册的 DAO 工厂与实际持有的 DAO 不一致（漏报 sid DAO）时装配校验看不到，而 Remote 提交按 mutation 自带的
// DatabaseScope 选库（mongo_payload.go → dataDB 的 sid 分支），又回到“提交按提交方 sid、加载按本服 sid”的不一致。
// 承诺：提交路径在 WAL 准入之前（FinalizeLocked）拒绝带 dbscope=sid 的 mutation / delete，错误可 errors.Is
// ErrRemoteManagedServerScopedDAO，已定稿的条目回滚、批次不产生任何提交。投影 / 重放路径不拒绝（见修复记录）。

// serverScopedCommitEntity 的注册 DAO 工厂不声明 DbScope（装配校验放行），实际提交却带 dbscope=sid。
type serverScopedCommitEntity struct {
	*testRemoteEntity
}

func (e *serverScopedCommitEntity) BuildRemoteCommitLocked(lease entity.RemoteWriteLease, outcome entity.RemoteTransactionOutcome) (entity.RemoteCommit, error) {
	commit, err := e.testRemoteEntity.BuildRemoteCommitLocked(lease, outcome)
	for i := range commit.Mutations {
		commit.Mutations[i].DatabaseScope = uint8(entity.DatabaseServer)
	}
	for i := range commit.Deletes {
		commit.Deletes[i].DatabaseScope = uint8(entity.DatabaseServer)
	}
	return commit, err
}

func TestFinalizeLockedRejectsServerScopedRemoteData(t *testing.T) {
	for i, deleteCommit := range []bool{false, true} {
		name := map[bool]string{false: "update", true: "delete"}[deleteCommit]
		t.Run(name, func(t *testing.T) {
			const kind entity.EntityKind = 143
			entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
			mgr := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
			loader := newRemoteTestLoader()
			mgr.SetBackend(loader)
			mgr.SetOwnershipStore(newMockMarkerStore())
			live := &serverScopedCommitEntity{newTestRemoteEntity(2431+int64(i), 1, kind)}
			live.dirty.set(true)
			loader.add(live)

			ctx := context.Background()
			batch, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{live.GUId()})
			if err != nil {
				t.Fatal(err)
			}
			outcome := entity.NewRemoteTransactionOutcome(remoteTestTxID(byte(43+i)), "sid", "", true, 2)
			if deleteCommit {
				outcome.DeleteIntents = batchDeleteIntent(live.GUId())
			}
			err = batch.FinalizeLocked(outcome)
			if err == nil {
				t.Fatalf("FinalizeLocked accepted a remote-managed %s commit whose data uses dbscope=sid; commits=%+v", name, batch.Commits())
			}
			if !errors.Is(err, ErrRemoteManagedServerScopedDAO) || !errors.Is(err, entity.ErrRemoteManagedServerScopedDAO) {
				t.Fatalf("FinalizeLocked = %v, want errors.Is ErrRemoteManagedServerScopedDAO", err)
			}
			if commits := batch.Commits(); len(commits) != 0 {
				t.Fatalf("a rejected FinalizeLocked left commits for the WAL: %+v", commits)
			}
			if err := batch.Abort(ctx, errors.New("test complete")); err != nil {
				t.Fatal(err)
			}
			if err := batch.Close(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
