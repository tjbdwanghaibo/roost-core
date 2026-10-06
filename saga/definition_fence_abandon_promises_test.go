package saga

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
)

// RR-20261005-NC-250：协调器因定义缺失把记录 fence 到 ManualRequired 时，若当前步骤正在重试退避（已派发过、
// 还没有结果，Attempt > 0、OperationKey 已清空），这个操作同样被放弃了。SAGA.md「原生步骤执行契约」第 4 条把
// “定义缺失”列为放弃关闭：tombstone 记“放弃关闭”、删掉仍排队的命令，之后到达的成功告警。
//
// 旧行为：定义缺失分支只关闭 record.OperationKey——在等结果（Waiting）时正确，退避中它是空串，不写 tombstone、
// 不删排队命令。较早尝试晚到的成功（Mongo 步骤的 handler 在截止前开始、之后提交；原生步骤的 completion effect
// 发布延迟）被协调器以 ErrNotWaiting 当作“无从知晓”丢弃，不告警；滚动升级期间修好定义后 Resume，已完成步骤
// 进入补偿，这一步不在 CompletedSteps 里、不会被补偿，也没有人被告知。同形态的截止分支与人工 Compensate
// 在 U-0280 已经按 Attempt > 0 补了关闭。
func TestDefinitionFenceDuringBackoffAbandonsTheOperation(t *testing.T) {
	for _, phase := range []Phase{PhaseForward, PhaseCompensate} {
		t.Run(phase.String(), func(t *testing.T) {
			ctx := context.Background()
			store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "nc250"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			publish := PublishFunc(func(context.Context, Command) error { return nil })
			// withDefinition 是正常协调器；withoutDefinition 是滚动升级中还没有这个定义版本的协调器。
			withDefinition, err := NewEngine(store, publish, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := withDefinition.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			withoutDefinition, err := NewEngine(store, publish, DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}

			now := time.Now().UTC().Truncate(time.Millisecond)
			record := Record{ID: "nc250-" + phase.String(), Type: "rally", DefinitionVersion: 1, BusinessKey: "nc250-" + phase.String(),
				Status: StatusPending, Phase: PhaseForward, Step: 1, CompletedSteps: 1, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if phase == PhaseCompensate {
				record.Status, record.Phase, record.Step = StatusCompensating, PhaseCompensate, 0
			}
			if err := store.Create(ctx, record); err != nil {
				t.Fatal(err)
			}
			get := func() Record {
				t.Helper()
				current, err := store.Get(ctx, record.ID)
				if err != nil {
					t.Fatal(err)
				}
				return current
			}

			// 派发第一次尝试；它的命令留在 outbox（发布一直失败，或者已经发出去、步骤还在执行）。
			if err := withDefinition.processClaimed(ctx, get(), now); err != nil {
				t.Fatal(err)
			}
			waiting := get()
			first := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: record.ID, Success: true}
			// 第一次尝试超时：还有重试额度，进入退避（Attempt=1，OperationKey 清空）。
			if err := withDefinition.processClaimed(ctx, waiting, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			backoff := get()
			if backoff.Status == StatusWaiting || backoff.Attempt != 1 || backoff.OperationKey != "" {
				t.Fatalf("precondition: want the step in retry backoff, got %+v", backoff)
			}
			// 退避到期，被没有定义的协调器领到：fence 到 ManualRequired。
			if err := withoutDefinition.processClaimed(ctx, backoff, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if fenced := get(); fenced.Status != StatusManualRequired {
				t.Fatalf("precondition: missing definition did not fence the record: %+v", fenced)
			}

			// 关闭操作会删掉它仍排队的命令；没有关闭时，被放弃的尝试仍会被发布出去。
			queued, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "nc250", Now: now.Add(time.Hour), LeaseDuration: time.Minute, Limit: 8})
			if err != nil {
				t.Fatal(err)
			}
			if len(queued) != 0 {
				t.Errorf("the fenced operation still has %d queued command(s) (first %s): the definition fence did not close the operation it abandoned", len(queued), queued[0].Command.ID)
			}

			// 第一次尝试其实生效了，它的成功现在才到。
			alarmsBefore := counterValue("saga.completion.late_after_abandon_total")
			_, completeErr := withDefinition.Complete(ctx, first)
			if completeErr != nil {
				t.Errorf("late success after the definition fence = %v, want nil (acknowledged and alarmed): ErrNotWaiting means the coordinator has no record of the abandoned operation and drops the effect silently", completeErr)
			}
			if errors.Is(completeErr, ErrNotWaiting) || withDefinition.Stats().LateAfterAbandon != 1 || counterValue("saga.completion.late_after_abandon_total")-alarmsBefore != 1 {
				t.Errorf("a step that took effect after the definition fence abandoned it was not alarmed: LateAfterAbandon=%d, counter grew by %d, want 1 and 1",
					withDefinition.Stats().LateAfterAbandon, counterValue("saga.completion.late_after_abandon_total")-alarmsBefore)
			}
			if current := get(); current.Status != StatusManualRequired || current.CompletedSteps != record.CompletedSteps {
				t.Errorf("the late success changed the fenced saga: %+v", current)
			}
		})
	}
}
