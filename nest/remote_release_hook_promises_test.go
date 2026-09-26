package nest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260926-32：Remote 事务持久提交之后 Entity release hook panic，已提交批次必须 Commit。
//
// finishRemoteWriteBatch 旧实现按 dispatchErr 的错误类型推断“是否已提交”：只有
// ErrAfterCommitFailed 被当作提交后错误。release hook 的 panic 由 dispatchLoadedEntities
// 的 defer release() 抛出，经 runNestLogic 变成普通 dispatchErr，于是 WAL 已写入的事务
// 被 Abort——远端内存回滚、事务标 Rejected、postRemoteCommit（Sync Confirm）被跳过，
// 与 RR-13/14 同症状。承诺：以 Msg 上显式记录的“持久提交成功”决定 Commit / Abort，
// 提交后的错误只作为回复里的错误报告；未提交的失败仍 Abort。
func TestRemoteReleaseHookPanicAfterDurableCommitStillCommits(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failHandler bool
	}{
		{name: "hook panic after durable commit"},
		{name: "handler failure before commit", failHandler: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unique := int64(9970)
			if tc.failHandler {
				unique = 9980
			}
			localID, e := newAsyncPilotEntity(t, unique, 10)
			owner := entity.NewEntityManager()
			if err := owner.TryAdd(e); err != nil {
				t.Fatal(err)
			}
			armed := false
			defer owner.RegisterOnEntityRelease(func(entity.IThreadSafeEntity) {
				if armed {
					armed = false
					panic(errors.New("release hook failed"))
				}
			})()
			getter := newMockGetter()
			remoteID := mustBuildCastID(t, unique+1, entity.EntityCategoryRemote, nestRemoteManagedKind)
			getter.Add(newMockEntity(remoteID, entity.EntityCategoryRemote))
			getter.Add(e)
			batch := &stagedRemoteBatch{}
			committed := false
			batch.commit = func() { committed = true }
			manager := stagedRemoteManager{prepare: func(context.Context) (entity.RemoteWriteBatch, error) { return batch, nil }}
			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(getter), NestOptionWithRemoteEntityManager(manager), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			name := NewHandlerName("remote_release_hook_panic")
			mgr.MustRegisterHandlerWithMeta(name, func(es []entity.IThreadSafeEntity, _ []any, _ ...HandlerOption) (any, error) {
				old := e.dao.Value
				RecordUndo(e.dao, 1, func() error { e.dao.Value = old; return nil })
				e.dao.Value++
				if tc.failHandler {
					return nil, errors.New("business refused")
				}
				armed = true
				return "ok", MarkPersist(e.dao, 1)
			}, HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict})
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			msg, ch := GenSyncMsg(MsgTypeMulti)
			msg.Name = name.String()
			msg.Tids = []int64{remoteID, localID}
			msg.Cost = true
			msg.HasRemote = true
			if err := mgr.dispatcher.TrySendMsg(msg); err != nil {
				t.Fatal(err)
			}
			got := stagedWait(t, ch)
			replyErr, _ := got.(error)
			if tc.failHandler {
				if committed || !batch.aborted.Load() || len(committer.record.Mutations) != 0 || e.dao.Value != 10 {
					t.Fatalf("uncommitted failure: reply=%v Commit=%v Abort=%v mutations=%d value=%d", got, committed, batch.aborted.Load(), len(committer.record.Mutations), e.dao.Value)
				}
				return
			}
			if len(committer.record.Mutations) == 0 {
				t.Fatalf("transaction was not durably committed: reply=%v", got)
			}
			if !committed || batch.aborted.Load() {
				t.Fatalf("remote batch after durable commit + release hook panic: Commit=%v Abort=%v reply=%v", committed, batch.aborted.Load(), got)
			}
			if replyErr == nil || !strings.Contains(replyErr.Error(), "release hook failed") {
				t.Fatalf("post-commit hook failure must be reported, reply=%v", got)
			}
			if e.dao.Value != 11 {
				t.Fatalf("committed local value rolled back: %d", e.dao.Value)
			}
		})
	}
}
