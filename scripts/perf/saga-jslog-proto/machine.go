package jslogproto

import "fmt"

// Budget 是步骤预算（与现行 saga.step_defaults 同义）：每次尝试等 Timeout，一次操作最多 MaxAttempts 次，相邻两次退避。
type Budget struct {
	TimeoutMs    int64
	MaxAttempts  int
	BackoffMinMs int64
	BackoffMaxMs int64
}

func DefaultBudget() Budget {
	return Budget{TimeoutMs: 5000, MaxAttempts: 5, BackoffMinMs: 100, BackoffMaxMs: 5000}
}

func (b Budget) backoff(attempt int) int64 {
	d := b.BackoffMinMs
	for i := 1; i < attempt && d < b.BackoffMaxMs; i++ {
		d *= 2
	}
	if d > b.BackoffMaxMs {
		d = b.BackoffMaxMs
	}
	return d
}

// Outcome 是协调器对一次输入的判定，供统计与测试断言。
type Outcome int

const (
	OutcomeIgnored        Outcome = iota // 不认识的 saga、过期 / 不在等的结果
	OutcomeAccepted                      // 改变了状态
	OutcomeDuplicate                     // 操作已带结果关闭
	OutcomeStaleAttempt                  // 较早尝试的可重试失败
	OutcomeLateCompensate                // 放弃之后迟到的正向成功：补偿这一步（必要时重开终态）
	OutcomeLateAlarm                     // 放弃之后迟到的补偿成功：只告警
)

func (o Outcome) String() string {
	return [...]string{"ignored", "accepted", "duplicate", "stale_attempt", "late_compensate", "late_alarm"}[o]
}

// machine 是纯状态机：输入一份 saga 副本，就地修改，返回要派发的命令（可为空）。不做 I/O，重放与测试共用。
type machine struct{ budget Budget }

func stepCommand(s *Saga, phase Phase, step int) (action string, account int64, refuse bool) {
	switch {
	case step == 0 && phase == PhaseForward:
		return "debit", s.Data.From, false
	case step == 0 && phase == PhaseCompensate:
		return "credit", s.Data.From, false
	case step == 1 && phase == PhaseForward:
		return "credit", s.Data.To, s.Data.RefuseCredit
	default: // step 1 补偿
		return "debit", s.Data.To, false
	}
}

const stepCount = 2

func (m machine) start(id string, part int, data TransferData, now int64) (*Saga, *Command) {
	s := &Saga{ID: id, Part: part, Status: StatusRunning, Data: data, Phase: PhaseForward, CreatedAt: now}
	return s, m.dispatch(s, PhaseForward, 0, now)
}

// dispatch 开一次新尝试（同一操作 Attempt+1）或开一个新操作（Attempt 从 1 起）。
func (m machine) dispatch(s *Saga, phase Phase, step int, now int64) *Command {
	if s.Phase != phase || s.Step != step {
		s.Phase, s.Step, s.Attempt = phase, step, 0
	}
	s.Attempt++
	op := OpKey(s.ID, phase, step)
	s.Waiting = true
	s.CmdID = CmdID(op, s.Attempt)
	s.DeadlineAt = now + m.budget.TimeoutMs
	s.NextRunAt = 0
	action, account, refuse := stepCommand(s, phase, step)
	return &Command{SagaID: s.ID, Part: s.Part, Op: op, CmdID: s.CmdID, Phase: phase, Step: step, Attempt: s.Attempt,
		Action: action, Account: account, Amount: s.Data.Amount, Refuse: refuse, DeadlineAt: s.DeadlineAt}
}

func (s *Saga) close(op, how string) {
	if s.Closed == nil {
		s.Closed = map[string]string{}
	}
	s.Closed[op] = how
}

func (s *Saga) currentOp() string { return OpKey(s.ID, s.Phase, s.Step) }

// idle 表示当前没有尝试在途、也没有排着的重试（补偿之间或终态）。
func (s *Saga) idle() bool { return !s.Waiting && s.NextRunAt == 0 }

func removeStep(steps []int, step int) []int {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i] == step {
			return append(steps[:i], steps[i+1:]...)
		}
	}
	return steps
}

// nextCompensation 从 Completed 末尾取下一步补偿；没有就到终态。
func (m machine) nextCompensation(s *Saga, now int64) *Command {
	s.Waiting, s.CmdID, s.DeadlineAt, s.NextRunAt = false, "", 0, 0
	if len(s.Completed) == 0 {
		if s.Status == StatusCompensating {
			s.Status = StatusCompensated
		}
		return nil
	}
	s.Status = StatusCompensating
	return m.dispatch(s, PhaseCompensate, s.Completed[len(s.Completed)-1], now)
}

// abandon 关闭当前操作（放弃关闭）：正向转补偿（没有已生效步骤则 Failed），补偿方向停在 ManualRequired。
func (m machine) abandon(s *Saga, now int64) *Command {
	s.close(s.currentOp(), ClosedAbandoned)
	s.Waiting, s.CmdID, s.DeadlineAt, s.NextRunAt = false, "", 0, 0
	if s.Phase == PhaseCompensate {
		s.Status = StatusManual
		return nil
	}
	if len(s.Completed) == 0 {
		s.Status = StatusFailed
		return nil
	}
	s.Status = StatusCompensating
	return m.nextCompensation(s, now)
}

// complete 按现行“协调器接收结果的统一规则”判定一条结果（judgeCompletion 的原型版本）：
//   - 成功是操作的结论：操作开着（在等，或退避中 Attempt≥1）就接收；已带结果关闭是重复；放弃关闭之后到达的正向成功补偿这一步；
//   - 拒绝只在正等着这个操作时接收；可重试失败只接收正在等的那次尝试的。
func (m machine) complete(s *Saga, c Completion, now int64) (Outcome, *Command) {
	op := c.Op
	open := op == s.currentOp() && !s.Status.Terminal() && (s.Waiting || s.Attempt >= 1 && s.NextRunAt > 0)
	if !open {
		switch s.Closed[op] {
		case ClosedWithResult:
			return OutcomeDuplicate, nil
		case ClosedAbandoned:
			if !c.Success {
				return OutcomeIgnored, nil
			}
			if c.Phase == PhaseCompensate {
				s.LateAlarm++
				s.close(op, ClosedWithResult)
				return OutcomeLateAlarm, nil
			}
			// 正向第 s 步在放弃之后生效：记带结果关闭、放回 Completed，在下一个边界补偿它；终态被重开。
			s.close(op, ClosedWithResult)
			s.Completed = append(s.Completed, c.Step)
			switch {
			case s.Status == StatusFailed || s.Status == StatusCompensated:
				s.Reopened++
				s.Status = StatusCompensating
				return OutcomeLateCompensate, m.nextCompensation(s, now)
			case s.Status == StatusCompensating && s.idle():
				return OutcomeLateCompensate, m.nextCompensation(s, now)
			default: // 补偿在途 / 退避：它结束后从 Completed 末尾先补这一步；ManualRequired 只记下
				return OutcomeLateCompensate, nil
			}
		}
		return OutcomeIgnored, nil
	}
	switch {
	case c.Success:
		s.close(op, ClosedWithResult)
		s.Waiting, s.CmdID, s.DeadlineAt, s.NextRunAt = false, "", 0, 0
		if s.Phase == PhaseForward {
			s.Completed = append(s.Completed, s.Step)
			if s.Status == StatusRunning {
				if s.Step+1 < stepCount {
					return OutcomeAccepted, m.dispatch(s, PhaseForward, s.Step+1, now)
				}
				s.Status = StatusCompleted
				return OutcomeAccepted, nil
			}
			return OutcomeAccepted, m.nextCompensation(s, now)
		}
		s.Completed = removeStep(s.Completed, s.Step)
		return OutcomeAccepted, m.nextCompensation(s, now)
	case !c.Retryable:
		if !s.Waiting {
			return OutcomeIgnored, nil // 退避中的拒绝由下一次尝试回放
		}
		return OutcomeAccepted, m.abandon(s, now)
	default:
		if !s.Waiting || c.CmdID != s.CmdID {
			return OutcomeStaleAttempt, nil
		}
		return OutcomeAccepted, m.retryOrAbandon(s, now)
	}
}

func (m machine) retryOrAbandon(s *Saga, now int64) *Command {
	if s.Attempt >= m.budget.MaxAttempts {
		return m.abandon(s, now)
	}
	s.Waiting, s.CmdID, s.DeadlineAt = false, "", 0
	s.NextRunAt = now + m.budget.backoff(s.Attempt)
	return nil
}

// timer 处理到期：尝试超时 → 退避或放弃；退避到期 → 下一次尝试。不到期（陈旧的轮条目）不改状态。
func (m machine) timer(s *Saga, now int64) (bool, *Command) {
	switch {
	case s.Waiting && now >= s.DeadlineAt:
		return true, m.retryOrAbandon(s, now)
	case !s.Waiting && s.NextRunAt > 0 && now >= s.NextRunAt:
		return true, m.dispatch(s, s.Phase, s.Step, now)
	}
	return false, nil
}

// dueAt 是这笔 saga 下一次需要定时器检查的时刻（0 = 不需要）。
func (s *Saga) dueAt() int64 {
	if s.Waiting {
		return s.DeadlineAt
	}
	return s.NextRunAt
}

func (s *Saga) String() string {
	return fmt.Sprintf("%s v%d %s %s%d a%d w=%v completed=%v", s.ID, s.Version, s.Status, s.Phase, s.Step, s.Attempt, s.Waiting, s.Completed)
}
