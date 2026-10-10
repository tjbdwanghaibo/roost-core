package saga

import (
	"context"
	"testing"
	"time"
)

// 完成判定与代际变更按实际结果验证，不约束内部函数必须如何组织。
func TestJudgeCompletionIsTheUnifiedRule(t *testing.T) {
	const current uint32 = 2
	operation := operationKey("g", PhaseForward, 1)
	other := operationKey("g", PhaseForward, 0)
	waitingFor := commandID(operation, current, 3)
	positions := map[string]Record{
		"waiting":     {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusWaiting, OperationKey: operation, CommandID: waitingFor, Attempt: 3},
		"backoff":     {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusPending, Attempt: 3},
		"to dispatch": {ID: "g", Phase: PhaseForward, Step: 1, Status: StatusPending},
		"elsewhere":   {ID: "g", Phase: PhaseForward, Step: 0, Status: StatusWaiting, OperationKey: other, CommandID: commandID(other, current, 1), Attempt: 1},
	}
	incarnations := map[string]uint32{"older": current - 1, "same": current, "newer": current + 1}
	kinds := []string{"success", "refusal", "retryable of the waited attempt", "retryable of an earlier attempt"}

	// want 按方案第 1 节的表逐行写，不复用 judgeCompletion 的分支。
	want := func(kind, life, position string) completionVerdict {
		if life == "newer" {
			return verdictStaleIncarnation // 0
		}
		if kind == "success" {
			opened := (life == "same" && (position == "waiting" || position == "backoff")) || (life == "older" && position != "elsewhere")
			if opened {
				return verdictAccept // S
			}
			return verdictHistory // S'
		}
		if life == "older" {
			return verdictStaleIncarnation // R/F-old
		}
		if position != "waiting" {
			return verdictHistory // R/F-gone
		}
		if kind == "retryable of an earlier attempt" {
			return verdictStaleAttempt // F-stale
		}
		return verdictAccept // R / F
	}

	for position, base := range positions {
		for life, incarnation := range incarnations {
			for _, kind := range kinds {
				record := base
				record.Incarnation = current
				completion := Completion{SagaID: "g", IdempotencyKey: operation, CommandID: commandID(operation, incarnation, 3)}
				switch kind {
				case "success":
					completion.Success = true
				case "retryable of the waited attempt":
					completion.Retryable = true
					if life == "same" {
						completion.CommandID = waitingFor
					}
				case "retryable of an earlier attempt":
					completion.Retryable = true
					completion.CommandID = commandID(operation, incarnation, 2)
				}
				got := judgeCompletion(record, completion, commandIDIncarnation(operation, completion.CommandID))
				if expected := want(kind, life, position); got != expected {
					t.Errorf("%s from the %s incarnation, record %s: verdict %d, want %d", kind, life, position, got, expected)
				}
			}
		}
	}
}

func TestStepTransitionAloneDecidesTheIncarnation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	cases := []struct {
		name  string
		cause transitionCause
		phase Phase
		want  uint32
	}{
		{"timeout keeps the life", causeTimeout, PhaseForward, 3},
		{"dispatch keeps the life", causeDispatch, PhaseForward, 3},
		{"forward manual compensate keeps the life", causeManualCompensate, PhaseForward, 3},
		{"compensating manual compensate opens a life", causeManualCompensate, PhaseCompensate, 4},
		{"resume opens a life", causeResume, PhaseForward, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			before := Record{ID: "inc-1", Type: "gift", DefinitionVersion: 1, Status: StatusPending, Phase: tc.phase, Version: 1, Incarnation: 3, Data: []byte("{}"), CreatedAt: now, UpdatedAt: now, NextRunAt: now}
			if err := store.Create(ctx, before); err != nil {
				t.Fatal(err)
			}
			after := before.Clone()
			after.Version++
			after.Incarnation = 99
			written, _, err := (&Engine{store: store}).stepTransition(ctx, before, after, transition{cause: tc.cause})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.Get(ctx, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if written.Incarnation != tc.want || stored.Incarnation != tc.want {
				t.Fatalf("cause %d from incarnation 3 with after.Incarnation=99: returned %d, stored %d, want %d", tc.cause, written.Incarnation, stored.Incarnation, tc.want)
			}
		})
	}
}
