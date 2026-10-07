package saga

import (
	"context"
	"errors"
	"testing"
)

// RR-20261006-47（F06-S5，维护者 2026-10-07 选 A：拒绝）：Completed 的 saga 不能再被人工 Compensate。
//
// 承诺：Completed 是业务结论（全部步骤已生效），按终态做业务的一方（发奖励通知、写对账）已经据此行动；人工 Compensate
// 对它返回 ErrSagaCompleted，记录原样不动。要撤销已完成的业务走业务自己的冲正流程。
//
// 旧行为：Engine.Compensate 只拒绝 Waiting 与“没有可补偿的步骤”，Completed（CompletedSteps = 步骤数）被带回 Compensating，
// 逐步撤销已完成的业务；不计 saga.reopened_total，SAGA.md 没写、没有用例（探针：status=compensating step=1 completed=2 err=<nil>）。
func TestManualCompensateRefusesACompletedSaga(t *testing.T) {
	ctx := context.Background()
	engine, store := idleEngine(t)
	seeded := seedRecord(t, store, "done", StatusCompleted, len(testDefinition().Steps))
	reopenedBefore := counterValue("saga.reopened_total")

	got, err := engine.Compensate(ctx, "done", "operator", seeded.UpdatedAt)
	if !errors.Is(err, ErrSagaCompleted) {
		t.Fatalf("Compensate on a Completed saga = (status=%s phase=%s step=%d completed=%d, err=%v), want ErrSagaCompleted: a completed saga is a business conclusion others already acted on",
			got.Status, got.Phase, got.Step, got.CompletedSteps, err)
	}
	after, err := store.Get(ctx, "done")
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusCompleted || after.Phase != PhaseForward || after.Version != seeded.Version || after.Incarnation != seeded.Incarnation {
		t.Fatalf("the refused Compensate changed the record: %+v", after)
	}
	if grown := counterValue("saga.reopened_total") - reopenedBefore; grown != 0 {
		t.Fatalf("saga.reopened_total grew by %d on a refused Compensate", grown)
	}
	// 守卫：其余终态与非终态的口径不变（Compensated 幂等返回、Failed 照常进入补偿由既有用例覆盖）。
	seedRecord(t, store, "undone", StatusCompensated, 0)
	if record, err := engine.Compensate(ctx, "undone", "operator", seeded.UpdatedAt); err != nil || record.Status != StatusCompensated {
		t.Fatalf("Compensate on a Compensated saga = (%s, %v), want it returned unchanged", record.Status, err)
	}
}
