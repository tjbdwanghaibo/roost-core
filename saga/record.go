// Package saga provides the storage-independent orchestration state machine
// for durable business operations spanning multiple transaction domains.
package saga

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

type Status uint8

const (
	StatusPending Status = iota + 1
	StatusWaiting
	StatusCompensating
	StatusCompleted
	StatusCompensated
	StatusFailed
	StatusManualRequired
)

func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusCompensated || s == StatusFailed || s == StatusManualRequired
}

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusWaiting:
		return "waiting"
	case StatusCompensating:
		return "compensating"
	case StatusCompleted:
		return "completed"
	case StatusCompensated:
		return "compensated"
	case StatusFailed:
		return "failed"
	case StatusManualRequired:
		return "manual_required"
	default:
		return "invalid"
	}
}

type Phase uint8

const (
	PhaseForward Phase = iota + 1
	PhaseCompensate
)

func (p Phase) String() string {
	if p == PhaseForward {
		return "forward"
	}
	if p == PhaseCompensate {
		return "compensate"
	}
	return "invalid"
}

// Step 的 Timeout / MaxAttempts / BackoffMin / BackoffMax 是它的超时与重试预算：一次操作（同一步骤
// 同一方向）最多派发 MaxAttempts 次尝试，每次等 Timeout，相邻两次之间按 BackoffMin..BackoffMax 退避。
// 零值字段在 Engine.Register 时由 Options.StepBudgets 补齐（配置提供的默认值与按步骤覆盖，见 StepBudgets）。
type Step struct {
	Name            string
	ForwardTopic    string
	CompensateTopic string
	Timeout         time.Duration
	MaxAttempts     uint32
	BackoffMin      time.Duration
	BackoffMax      time.Duration
}

// StepBudget 是一个步骤的超时与重试预算；零值字段表示“未指定”。
type StepBudget struct {
	Timeout     time.Duration
	MaxAttempts uint32
	BackoffMin  time.Duration
	BackoffMax  time.Duration
}

// DefaultStepBudget 是框架内置的步骤预算：5 次尝试、每次 5s、退避 100ms..5s。
func DefaultStepBudget() StepBudget {
	return StepBudget{Timeout: 5 * time.Second, MaxAttempts: 5, BackoffMin: 100 * time.Millisecond, BackoffMax: 5 * time.Second}
}

// StepKey 指定一个步骤：saga 类型（Definition.Type）与步骤名（Step.Name）。
type StepKey struct {
	Type string
	Step string
}

// StepBudgets 把配置里的步骤预算应用到定义上（kit 的 saga Mod 从 saga.step_defaults 与
// saga.steps.<type>.<step> 读取）。每个字段的取值顺序：
//
//  1. Overrides[{Type, Step}] 里该字段非零 → 用它（运维按步骤覆盖，优先于代码）；
//  2. 定义里该字段非零 → 用它；
//  3. Defaults 里该字段非零 → 用它；
//  4. DefaultStepBudget()。
//
// 覆盖整个 Definition 的所有版本：同一类型同一步骤名在新旧版本里用同一份覆盖。
type StepBudgets struct {
	Defaults  StepBudget
	Overrides map[StepKey]StepBudget
}

// Resolve 返回补齐预算后的定义副本，不校验结果（Engine.Register 会校验）。
func (budgets StepBudgets) Resolve(definition Definition) Definition {
	builtin := DefaultStepBudget()
	steps := make([]Step, len(definition.Steps))
	for i, step := range definition.Steps {
		override := budgets.Overrides[StepKey{Type: definition.Type, Step: step.Name}]
		step.Timeout = firstDuration(override.Timeout, step.Timeout, budgets.Defaults.Timeout, builtin.Timeout)
		step.MaxAttempts = firstAttempts(override.MaxAttempts, step.MaxAttempts, budgets.Defaults.MaxAttempts, builtin.MaxAttempts)
		step.BackoffMin = firstDuration(override.BackoffMin, step.BackoffMin, budgets.Defaults.BackoffMin, builtin.BackoffMin)
		step.BackoffMax = firstDuration(override.BackoffMax, step.BackoffMax, budgets.Defaults.BackoffMax, builtin.BackoffMax)
		steps[i] = step
	}
	definition.Steps = steps
	return definition
}

// Validate 拒绝不可能成立的默认值与覆盖（负数、超过 1000 次、退避上限小于下限）。零值字段合法，表示“未指定”。
func (budgets StepBudgets) Validate() error {
	check := func(name string, budget StepBudget) error {
		if budget.Timeout < 0 || budget.BackoffMin < 0 || budget.BackoffMax < 0 || budget.MaxAttempts > 1000 ||
			(budget.BackoffMin > 0 && budget.BackoffMax > 0 && budget.BackoffMax < budget.BackoffMin) {
			return fmt.Errorf("%w: step budget %s %+v", ErrInvalidDefinition, name, budget)
		}
		return nil
	}
	if err := check("defaults", budgets.Defaults); err != nil {
		return err
	}
	for key, budget := range budgets.Overrides {
		if strings.TrimSpace(key.Type) == "" || strings.TrimSpace(key.Step) == "" {
			return fmt.Errorf("%w: step budget override without saga type or step name", ErrInvalidDefinition)
		}
		if err := check(key.Type+"/"+key.Step, budget); err != nil {
			return err
		}
	}
	return nil
}

func firstDuration(values ...time.Duration) time.Duration {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func firstAttempts(values ...uint32) uint32 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

type Definition struct {
	Type    string
	Version uint32
	Steps   []Step
}

func (d Definition) Validate() error {
	if strings.TrimSpace(d.Type) != d.Type || d.Type == "" || len(d.Type) > 128 || d.Version == 0 || len(d.Steps) == 0 || len(d.Steps) > 256 {
		return ErrInvalidDefinition
	}
	seen := make(map[string]struct{}, len(d.Steps))
	for i := range d.Steps {
		s := d.Steps[i]
		if strings.TrimSpace(s.Name) != s.Name || s.Name == "" || len(s.Name) > 128 || !validSubject(s.ForwardTopic) || (s.CompensateTopic != "" && !validSubject(s.CompensateTopic)) || (s.CompensateTopic == "" && !validSubject(s.ForwardTopic+".compensate")) || s.Timeout <= 0 || s.MaxAttempts == 0 || s.MaxAttempts > 1000 || s.BackoffMin <= 0 || s.BackoffMax < s.BackoffMin {
			return fmt.Errorf("%w: step %d", ErrInvalidDefinition, i)
		}
		if _, exists := seen[s.Name]; exists {
			return fmt.Errorf("%w: duplicate step %q", ErrInvalidDefinition, s.Name)
		}
		seen[s.Name] = struct{}{}
	}
	return nil
}

type Lease struct {
	Owner string
	Token uint64
	Until time.Time
}

type Record struct {
	ID                string
	Type              string
	DefinitionVersion uint32
	BusinessKey       string
	// StartDigest 是规范化原始启动意图的 SHA-256，不随步骤 Data、Resume 或截止时间变更。
	// 旧记录可为空；自定义 Store 必须在 Create/Get/Apply/Clone 链完整保留（NC-39）。
	StartDigest    string
	Status         Status
	Phase          Phase
	Step           int
	CompletedSteps int
	Attempt        uint32
	// Incarnation counts Resume generations. It is folded into CommandID so a
	// resumed operation can never reuse a CommandID from a previous life of
	// the same Saga (Resume resets Attempt), which would collide with stale
	// completion receipts and stall the Saga as a false duplicate.
	Incarnation  uint32
	Version      uint64
	Data         []byte
	LastError    string
	OperationKey string
	CommandID    string
	NextRunAt    time.Time
	DeadlineAt   time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Lease        Lease
}

func (r Record) Clone() Record {
	r.Data = append([]byte(nil), r.Data...)
	return r
}

func (r Record) Validate() error {
	waitingFields := r.OperationKey != "" && len(r.OperationKey) <= 192 && r.CommandID != "" && len(r.CommandID) <= 192 && r.Attempt > 0
	leaseValid := (r.Lease.Owner == "" && r.Lease.Token == 0 && r.Lease.Until.IsZero()) || (strings.TrimSpace(r.Lease.Owner) != "" && len(r.Lease.Owner) <= 256 && r.Lease.Token > 0 && !r.Lease.Until.IsZero())
	if !validSubjectToken(r.ID, 128) || strings.TrimSpace(r.Type) != r.Type || r.Type == "" || len(r.Type) > 128 || r.DefinitionVersion == 0 || strings.TrimSpace(r.BusinessKey) != r.BusinessKey || r.BusinessKey == "" || len(r.BusinessKey) > 512 || len(r.Data) > 4<<20 || len(r.LastError) > 4096 || r.Status < StatusPending || r.Status > StatusManualRequired || r.Phase < PhaseForward || r.Phase > PhaseCompensate || r.Version == 0 || r.Step < 0 || r.CompletedSteps < 0 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || (!r.Status.Terminal() && r.NextRunAt.IsZero()) || (r.Status == StatusWaiting) != waitingFields || (r.Status != StatusWaiting && (r.OperationKey != "" || r.CommandID != "")) || (r.Status == StatusCompensating && (r.Phase != PhaseCompensate || r.CompletedSteps == 0)) || (r.Status == StatusCompensated && r.CompletedSteps != 0) || !leaseValid {
		return ErrInvalidRecord
	}
	return nil
}

type Command struct {
	ID                string    `json:"id"`
	IdempotencyKey    string    `json:"idempotency_key"`
	SagaID            string    `json:"saga_id"`
	SagaType          string    `json:"saga_type"`
	DefinitionVersion uint32    `json:"definition_version"`
	BusinessKey       string    `json:"business_key"`
	Step              int       `json:"step"`
	StepName          string    `json:"step_name"`
	Phase             Phase     `json:"phase"`
	Attempt           uint32    `json:"attempt"`
	Topic             string    `json:"topic"`
	Payload           []byte    `json:"payload,omitempty"`
	DeadlineAt        time.Time `json:"deadline_at"`
	CreatedAt         time.Time `json:"created_at"`
}

func (c Command) Clone() Command {
	c.Payload = append([]byte(nil), c.Payload...)
	return c
}

func (c Command) Validate() error {
	if len(c.ID) > 192 || c.ID == "" || len(c.IdempotencyKey) > 192 || c.IdempotencyKey == "" || !validSubjectToken(c.SagaID, 128) || strings.TrimSpace(c.SagaType) != c.SagaType || len(c.SagaType) > 128 || c.SagaType == "" || c.DefinitionVersion == 0 || strings.TrimSpace(c.BusinessKey) != c.BusinessKey || len(c.BusinessKey) > 512 || c.BusinessKey == "" || c.Step < 0 || strings.TrimSpace(c.StepName) != c.StepName || len(c.StepName) > 128 || c.StepName == "" || c.Phase < PhaseForward || c.Phase > PhaseCompensate || c.Attempt == 0 || !validSubject(c.Topic) || len(c.Payload) > 4<<20 || c.DeadlineAt.IsZero() || c.CreatedAt.IsZero() {
		return ErrInvalidRecord
	}
	return nil
}

type Completion struct {
	CommandID      string    `json:"command_id"`
	IdempotencyKey string    `json:"idempotency_key"`
	SagaID         string    `json:"saga_id"`
	Success        bool      `json:"success"`
	Retryable      bool      `json:"retryable,omitempty"`
	Data           []byte    `json:"data,omitempty"`
	Error          string    `json:"error,omitempty"`
	CompletedAt    time.Time `json:"completed_at,omitempty"`
}

func (c Completion) Validate() error {
	invalidOutcome := (c.Success && (c.Retryable || c.Error != "")) || (!c.Success && strings.TrimSpace(c.Error) == "")
	if c.CommandID == "" || len(c.CommandID) > 192 || c.IdempotencyKey == "" || len(c.IdempotencyKey) > 192 || !validSubjectToken(c.SagaID, 128) || len(c.Error) > 4096 || len(c.Data) > 4<<20 || invalidOutcome {
		return ErrInvalidRecord
	}
	return nil
}

func validSubject(subject string) bool {
	if subject == "" || len(subject) > 256 || strings.TrimSpace(subject) != subject {
		return false
	}
	for _, token := range strings.Split(subject, ".") {
		if !validSubjectToken(token, 128) {
			return false
		}
	}
	return true
}

func validSubjectToken(token string, maxLength int) bool {
	if token == "" || len(token) > maxLength || strings.ContainsAny(token, ".*> \t\r\n") {
		return false
	}
	return true
}

type OutboxRecord struct {
	Command       Command
	Attempt       uint32
	NextAttemptAt time.Time
	Lease         Lease
	CreatedAt     time.Time
}

func (o OutboxRecord) Clone() OutboxRecord {
	o.Command = o.Command.Clone()
	return o
}

var idState struct {
	prefix [8]byte
	seq    atomic.Uint64
}

func init() {
	if _, err := rand.Read(idState.prefix[:]); err != nil {
		binary.BigEndian.PutUint64(idState.prefix[:], uint64(time.Now().UnixNano()))
	}
}

func NewID() string {
	var raw [16]byte
	var encoded [32]byte
	copy(raw[:8], idState.prefix[:])
	binary.BigEndian.PutUint64(raw[8:], idState.seq.Add(1))
	hex.Encode(encoded[:], raw[:])
	return string(encoded[:])
}
