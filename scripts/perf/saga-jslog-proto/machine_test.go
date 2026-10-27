package jslogproto

import "testing"

// 纯状态机：现行“协调器接收结果的统一规则”在原型里的对应（不需要 NATS）。
func TestMachineCompletionRules(t *testing.T) {
	m := machine{budget: Budget{TimeoutMs: 100, MaxAttempts: 2, BackoffMinMs: 10, BackoffMaxMs: 10}}
	data := TransferData{From: 1, To: 2, Amount: 5}
	res := func(s *Saga, success, retryable bool, cmdID string) Completion {
		return Completion{V: 1, SagaID: s.ID, Op: s.currentOp(), CmdID: cmdID, Phase: s.Phase, Step: s.Step, Success: success, Retryable: retryable}
	}

	t.Run("forward_then_complete_then_duplicate", func(t *testing.T) {
		s, cmd := m.start("a", 0, data, 0)
		first := res(s, true, false, cmd.CmdID)
		if o, next := m.complete(s, first, 1); o != OutcomeAccepted || next == nil || next.Step != 1 {
			t.Fatalf("step 0 success: %v %+v", o, next)
		}
		if o, _ := m.complete(s, first, 2); o != OutcomeDuplicate {
			t.Fatalf("redelivered step 0 success: %v, want duplicate", o)
		}
		if o, _ := m.complete(s, res(s, true, false, s.CmdID), 3); o != OutcomeAccepted || s.Status != StatusCompleted {
			t.Fatalf("step 1 success: %v %s", o, s)
		}
	})
	t.Run("stale_retryable_failure_and_success_in_backoff", func(t *testing.T) {
		s, cmd := m.start("b", 0, data, 0)
		old := cmd.CmdID
		if changed, _ := m.timer(s, 100); !changed || s.Waiting || s.NextRunAt == 0 {
			t.Fatalf("attempt timeout should schedule a retry: %s", s)
		}
		// 退避中：拒绝不接收，较早尝试的可重试失败不接收，成功接收（RR-20261006-42）。
		if o, _ := m.complete(s, res(s, false, false, old), 101); o != OutcomeIgnored {
			t.Fatalf("refusal in backoff: %v", o)
		}
		if o, _ := m.complete(s, res(s, false, true, old), 102); o != OutcomeStaleAttempt {
			t.Fatalf("stale retryable failure: %v", o)
		}
		if o, next := m.complete(s, res(s, true, false, old), 103); o != OutcomeAccepted || next == nil || next.Step != 1 {
			t.Fatalf("success in backoff: %v %+v", o, next)
		}
	})
	t.Run("late_success_after_abandon_reopens_failed", func(t *testing.T) {
		s, cmd := m.start("c", 0, data, 0)
		late := res(s, true, false, cmd.CmdID)
		m.timer(s, 100)        // 第 1 次超时 → 退避
		m.timer(s, 200)        // 退避到期 → 第 2 次
		_, _ = m.timer(s, 400) // 第 2 次超时 → 用尽 → 没有已生效步骤 → Failed
		if s.Status != StatusFailed {
			t.Fatalf("want failed after exhausting attempts: %s", s)
		}
		o, next := m.complete(s, late, 500)
		if o != OutcomeLateCompensate || s.Status != StatusCompensating || s.Reopened != 1 || next == nil || next.Phase != PhaseCompensate || next.Step != 0 {
			t.Fatalf("late success: %v %s %+v", o, s, next)
		}
		if o, _ := m.complete(s, late, 501); o != OutcomeDuplicate {
			t.Fatalf("late success again: %v", o)
		}
		if o, _ := m.complete(s, res(s, true, false, next.CmdID), 502); o != OutcomeAccepted || s.Status != StatusCompensated {
			t.Fatalf("compensation of the late step: %v %s", o, s)
		}
	})
	t.Run("refusal_compensates_then_compensation_refusal_is_manual", func(t *testing.T) {
		s, cmd := m.start("d", 0, TransferData{From: 1, To: 2, Amount: 5, RefuseCredit: true}, 0)
		m.complete(s, res(s, true, false, cmd.CmdID), 1)
		o, next := m.complete(s, res(s, false, false, s.CmdID), 2)
		if o != OutcomeAccepted || s.Status != StatusCompensating || next == nil || next.Action != "credit" || next.Account != 1 {
			t.Fatalf("refusal at step 1: %v %s %+v", o, s, next)
		}
		if o, _ := m.complete(s, res(s, false, false, next.CmdID), 3); o != OutcomeAccepted || s.Status != StatusManual {
			t.Fatalf("compensation refused: %v %s", o, s)
		}
	})
}

func TestWheelDeliversDueEntriesOnce(t *testing.T) {
	w := newWheel(5, 8, 1000)
	w.schedule("a", 1003)
	w.schedule("b", 1100) // 多转几圈
	w.schedule("c", 900)  // 已过期：落在当前槽
	got := w.advance(1010, nil)
	if len(got) != 2 {
		t.Fatalf("due at 1010: %v", got)
	}
	if got := w.advance(1099, nil); len(got) != 0 {
		t.Fatalf("due at 1099: %v", got)
	}
	if got := w.advance(1200, nil); len(got) != 1 || got[0] != "b" || w.size() != 0 {
		t.Fatalf("due at 1200: %v size=%d", got, w.size())
	}
	// 同一 tick 内还没到期的条目：下一次 advance 必须交出，不能等一整圈（原型第一版的缺陷，迟到成功用例因此偶发失败）。
	w.schedule("d", 1204) // tick 240
	if got := w.advance(1201, nil); len(got) != 0 {
		t.Fatalf("due at 1201: %v", got)
	}
	if got := w.advance(1206, nil); len(got) != 1 || got[0] != "d" {
		t.Fatalf("entry in the current tick was skipped: due at 1206 = %v", got)
	}
}
