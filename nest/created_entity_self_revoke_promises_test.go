package nest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20260927-21（第五轮审计 N1，RR-81 引入）：handler 内 RunIsolatedTransaction 新建 X 后让独立事务失败，独立事务回滚撤销 X，
// 撤销收尾挂在同一个 Guard 上、要等整个 handler 释放才执行；外层随后再新建同一个 X。RR-81 的分支把 TryAdd 撞上的 removing
// 当成“别的持有者尚未交还”的暂时冲突：可回滚事务整条回滚重排，而这个冲突在每次执行里都会重现，handler 空转到重排上限
// （审计探针 attempts=401），最终回复还按判别表属于“可重试”；memory handler 的冲突文案写 locked by another holder，
// 实际是它自己。
//
// 承诺：本 handler 自己撤销、收尾挂在当前 Guard 上的同 ID 再建是确定失败——不重排，错误可 errors.Is(entity.ErrEntityRemoved)，
// 说明是本 handler 内已撤销的同 ID；不带 ErrLockTimeout / ErrCreatedEntityLockConflict。别的持有者的撤销 / Destroy 收尾
// 保持 RR-81（见 created_entity_revoke_window_promises_test.go、created_entity_removal_window_shapes_promises_test.go）。
// 探针形状取自审计原文：每次执行都先走嵌套撤销，再在外层新建。

func TestOuterCreateAfterNestedRevokeInSameGuardFailsDeterministically(t *testing.T) {
	for i, tc := range []struct {
		name string
		meta HandlerMeta
	}{
		{"state_strict", HandlerMeta{Rollback: RollbackState, Durability: DurabilityStrict}},
		{"memory", HandlerMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := entity.NewEntityManager()
			pilots := addPilots(t, manager, 38120+int64(i)*10, 1)
			access := entity.NewManagerAccess(manager)
			x := mustBuildCastID(t, 38125+int64(i)*10, entity.EntityCategory(1), createdInScopeKind)
			committer := &recordingCommitter{}
			mgr := NewEngine(NestOptionWithGetter(access), NestOptionWithTransactionCommitter(committer), NestOptionWithWorkerNumAndMsgCap(1, 1, 16))
			nestedBoom := errors.New("nested transaction failed after creating X")
			var runs int
			var isoErr, outerErr error
			name := NewHandlerName("rr20260927_21_self_revoke_" + tc.name)
			mgr.MustRegisterHandlerWithMeta(name, func([]entity.IThreadSafeEntity, []any, ...HandlerOption) (any, error) {
				runs++
				_, isoErr = RunIsolatedTransaction(context.Background(), committer, "rr20260927_21_iso", func() (any, error) {
					if _, err := access.Create(createParam(x)); err != nil {
						return nil, err
					}
					return nil, nestedBoom
				})
				if _, outerErr = access.Create(createParam(x)); outerErr != nil {
					return nil, outerErr
				}
				return "ok", nil
			}, tc.meta)
			if err := mgr.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mgr.Shutdown(context.Background()) }()

			_, err := mgr.Request(context.Background(), name, pilots[0], nil)
			if !errors.Is(isoErr, nestedBoom) {
				t.Fatalf("fixture: nested transaction err=%v, want its own failure", isoErr)
			}
			if runs != 1 {
				t.Fatalf("%s handler ran %d times: a create that collides with this handler's own revoke was requeued (reply %v)", tc.name, runs, err)
			}
			for where, got := range map[string]error{"outer Create": outerErr, "reply": err} {
				if !errors.Is(got, entity.ErrEntityRemoved) || errors.Is(got, ErrLockTimeout) || errors.Is(got, ErrCreatedEntityLockConflict) {
					t.Fatalf("%s: %s = %v; want errors.Is entity.ErrEntityRemoved without ErrLockTimeout / ErrCreatedEntityLockConflict", tc.name, where, got)
				}
				if msg := got.Error(); !strings.Contains(msg, "revoked earlier in this handler") || strings.Contains(msg, "another holder") {
					t.Fatalf("%s: %s text %q must say the id was revoked earlier in this handler, not blame another holder", tc.name, where, msg)
				}
			}
			if got := manager.Get(x); got != nil {
				t.Fatalf("X published although both creations failed: %v", got)
			}
			// 收尾随 handler 的 Guard 释放完成：同一 ID 之后可以重新创建。
			again, err := access.Create(createParam(x))
			if err != nil || manager.Get(x) != again {
				t.Fatalf("re-create after the handler released: %v", err)
			}
		})
	}
}
