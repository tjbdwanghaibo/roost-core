package nest

import (
	"context"
	"errors"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260930-20：handler 返回业务错误（事务回滚）后，释放实体时 OnEntityRelease 钩子 panic，回复必须仍以业务错误为主
// （errors.Is(err, 业务错误) 成立），hook 的 panic 以 errors.Join 附加在错误链上（errors.Is(err, hookErr) 同样成立），
// 且不带 ErrAfterCommitFailed——什么都没提交。
//
// 旧行为：dispatchLoadedEntities 的 `defer release()` 直接把 hook 的 panic 抛出，函数的返回值（在途的业务错误）随之丢失，
// runNestLogic 的 recover 再把 err 整个换成 hook 错误，回复只剩 `release hook failed`，调用方无法判别是业务拒绝还是释放失败。
// 已提交路径（handler 成功、提交后 hook panic）的 ErrAfterCommitFailed 语义由 local_after_commit_sentinel_promises_test.go 钉住。

var errRolledBackBusiness = errors.New("business rejected")

func TestRolledBackReleaseHookPanicKeepsBusinessError(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"memory", HandlerMeta{}},
		{"strict", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityStrict}},
		{"pipelined", HandlerMeta{Rollback: RollbackUndo, Durability: DurabilityPipelined}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLocalHookFixture(t, 10060+int64(i))
			var committer TransactionCommitter
			recording := &recordingCommitter{}
			switch tc.meta.Durability {
			case DurabilityPipelined:
				committer = newPipelinedTestCommitter(true)
			default:
				committer = recording
			}
			mgr := NewEngine(NestOptionWithGetter(entity.NewManagerAccess(f.owner)), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerPools(WorkerPoolConfig{Workers: 1, QueueCap: 16}, WorkerPoolConfig{}))
			name := NewHandlerName("rr20_rolled_back_release_hook_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				if tc.meta.Rollback == RollbackUndo {
					old := f.e.dao.Value
					RecordUndo(f.e.dao, 1, func() error { f.e.dao.Value = old; return nil })
					f.e.dao.Value++
				}
				f.armed = true
				return nil, errRolledBackBusiness
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer mgr.Shutdown(context.Background())
			_, err := mgr.Request(context.Background(), name, f.id, nil)
			if f.armed {
				t.Fatal("premise: the release hook did not run")
			}
			if tc.meta.Rollback == RollbackUndo && f.e.dao.Value != 10 {
				t.Fatalf("value=%d, want rollback to 10", f.e.dao.Value)
			}
			if len(recording.record.Mutations) != 0 {
				t.Fatalf("fixture: %d mutations committed for a rolled-back transaction", len(recording.record.Mutations))
			}
			if !errors.Is(err, errRolledBackBusiness) {
				t.Fatalf("%s reply=%v: the business error must be kept when the release hook panics after a rollback", tc.name, err)
			}
			if !errors.Is(err, f.hookErr) {
				t.Fatalf("%s reply=%v: the release hook panic must stay on the error chain", tc.name, err)
			}
			if errors.Is(err, ErrAfterCommitFailed) {
				t.Fatalf("%s reply=%v: nothing was committed, ErrAfterCommitFailed must not be reported", tc.name, err)
			}
		})
	}
}
