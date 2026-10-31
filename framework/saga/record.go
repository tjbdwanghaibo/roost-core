// Package saga 提供与存储无关的编排状态机，用于跨多个事务域的持久业务操作。
package saga

import (
	"fmt"
	"strings"
	"time"
)

// Status 是 saga 记录的状态。Completed / Compensated / Failed / ManualRequired 是终态。
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

// Terminal 报告状态是否为终态。
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

// Phase 是 saga 的推进方向：正向执行或补偿。
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

// RetryWindow 是这一步一个操作从第一次派发到最后一次尝试截止的最长计划时间：MaxAttempts 次超时，加其间 MaxAttempts−1 次
// 退避（按上限 BackoffMax 计）。同一操作在这段时间里都可能有尝试被参与方执行，参与方的回执必须比它活得久。
// 它是按计划算的：协调器停机、分区积压、命令在流里滞留都会把实际的最后一次尝试推得更晚（见 CheckReceiptRetention）。
func (s Step) RetryWindow() time.Duration {
	attempts := time.Duration(max(s.MaxAttempts, 1))
	return s.Timeout*attempts + s.BackoffMax*(attempts-1)
}

// ReceiptRetentionFactor 是回执保留期相对步骤重试窗口的最小倍数：保留期至少是最长窗口的这么多倍。多出的那一份窗口是余量，
// 吸收协调器调度滞后、短暂停机或换主、参与方积压与时钟偏差；更长的停顿超出 saga.receipt_retention − 窗口时，至多一次不再
// 由回执保证（KNOWN-LIMITS“Saga”）。
const ReceiptRetentionFactor = 2

// CheckReceiptRetention 检查一个已补齐预算的定义（Engine.Register 之前 StepBudgets.Resolve 过）的每一步：参与方回执保留期
// retention 不短于 ReceiptRetentionFactor × 这一步的 RetryWindow。参与方 Complete 时会剪掉早于 now − retention 的回执
// （Mongo 步骤由 TTL 删除），回执被剪之后同一操作的下一次尝试会再执行一次业务（RV-20261030-02）；配置在 Register 与
// 配置检查时就拒绝，不在运行期间才暴露。
func CheckReceiptRetention(definition Definition, retention time.Duration) error {
	for _, step := range definition.Steps {
		if window := step.RetryWindow(); retention < ReceiptRetentionFactor*window {
			return fmt.Errorf("%w: saga %s/%d step %q: receipt retention %v must be at least %d × the step's retry window %v "+
				"(timeout %v × %d attempts + backoff max %v × %d): participants prune receipts older than the retention, "+
				"and a retry of the same operation after that takes effect again",
				ErrInvalidDefinition, definition.Type, definition.Version, step.Name, retention, ReceiptRetentionFactor, window,
				step.Timeout, max(step.MaxAttempts, 1), step.BackoffMax, max(step.MaxAttempts, 1)-1)
		}
	}
	return nil
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
//
// 覆盖先按 {Type, Step} 原样查找，查不到再按全小写查找：配置键经 viper 一律转成小写，kit 的
// saga Mod 没拿到定义时（kitsaga.NewMod() 无参数、定义之后经 Register 登记）只能以小写键保存覆盖，
// 原样查找会让大小写混写的类型 / 步骤名的覆盖静默不生效。
func (budgets StepBudgets) Resolve(definition Definition) Definition {
	builtin := DefaultStepBudget()
	steps := make([]Step, len(definition.Steps))
	for i, step := range definition.Steps {
		override, ok := budgets.Overrides[StepKey{Type: definition.Type, Step: step.Name}]
		if !ok {
			override = budgets.Overrides[StepKey{Type: strings.ToLower(definition.Type), Step: strings.ToLower(step.Name)}]
		}
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

// Definition 是一个 saga 类型某个版本的步骤定义。
type Definition struct {
	Type    string
	Version uint32
	Steps   []Step
}

// Validate 校验类型名、版本、步骤数量与每个步骤的名字、主题、超时和重试预算，步骤名不得重复。
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

// Record 是一个 saga 实例的持久状态。
type Record struct {
	ID                string
	Type              string
	DefinitionVersion uint32
	BusinessKey       string
	// StartDigest 是规范化原始启动意图的 SHA-256，不随步骤 Data、Resume 或截止时间变更。
	StartDigest    string
	Status         Status
	Phase          Phase
	Step           int
	CompletedSteps int
	Attempt        uint32
	// Incarnation 记 Resume 的代际，并编进 CommandID：Resume 会重置 Attempt，若不换代，恢复后的操作会复用同一 saga
	// 上一生的 CommandID，撞上陈旧的 completion 回执，被当成重复而卡住。
	Incarnation uint32
	// LateStep 是放弃之后才生效、还没补偿的正向步骤号 + 1，0 表示没有（saga 方向 ④，方向 C）。协调器放弃了第 s 步
	// （超时用尽、saga 截止、人工 Compensate、定义缺失），这一步的某次尝试却在截止前生效了，成功在放弃之后才送达：
	// 它不在 CompletedSteps 里，补偿的倒序不会撤销它。协调器记下它，下一个补偿先补它（nextCompensation）。
	// 正向一次只开一个操作、放弃即离开正向，所以最多一个；恒有 LateStep-1 >= CompletedSteps。
	LateStep int
	// LateData 是那次迟到成功的 Completion.Data，补偿第 LateStep-1 步的命令载荷；它的补偿结果不进入 Data 链。
	LateData     []byte
	Version      uint64
	Data         []byte
	LastError    string
	OperationKey string
	CommandID    string
	NextRunAt    time.Time
	DeadlineAt   time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// StartAttempt 是开启这一轮的启动尝试标识（设计 §7.3）：之后的版本原样带着。
	StartAttempt string
	// Lifecycle 是这一笔 saga 的生命周期标识：协调器新建记录时随机生成（newLifecycle），之后的版本、Resume、人工补偿、
	// 重启重放都原样带着，并编进每个操作键（operationKey）。saga ID 由类型与业务键派生，终态过了保留期被剪掉后同一业务键
	// 可以再开一笔；没有它，新一笔的操作键与 CommandID 与上一笔相同，参与方会按上一笔的成功回执回放、不执行业务，
	// 迟到的上一笔结果也会被当成新一笔的结论（RV-20261030-01）。同一笔之内它不变，所以 Resume 之后的去重语义不受影响。
	Lifecycle string
}

func (r Record) Clone() Record {
	r.Data = append([]byte(nil), r.Data...)
	if r.LateData != nil {
		r.LateData = append([]byte(nil), r.LateData...)
	}
	return r
}

func (r Record) Validate() error {
	waitingFields := r.OperationKey != "" && len(r.OperationKey) <= 192 && r.CommandID != "" && len(r.CommandID) <= 192 && r.Attempt > 0
	if !validSubjectToken(r.ID, 128) || !validLifecycle(r.Lifecycle) || strings.TrimSpace(r.Type) != r.Type || r.Type == "" || len(r.Type) > 128 || r.DefinitionVersion == 0 || strings.TrimSpace(r.BusinessKey) != r.BusinessKey || r.BusinessKey == "" || len(r.BusinessKey) > 512 || len(r.Data) > 4<<20 || len(r.LastError) > 4096 || r.Status < StatusPending || r.Status > StatusManualRequired || r.Phase < PhaseForward || r.Phase > PhaseCompensate || r.Version == 0 || r.Step < 0 || r.CompletedSteps < 0 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || (!r.Status.Terminal() && r.NextRunAt.IsZero()) || (r.Status == StatusWaiting) != waitingFields || (r.Status != StatusWaiting && (r.OperationKey != "" || r.CommandID != "")) || (r.Status == StatusCompensating && (r.Phase != PhaseCompensate || (r.CompletedSteps == 0 && r.LateStep == 0))) || (r.Status == StatusCompensated && r.CompletedSteps != 0) || !r.lateStepValid() {
		return ErrInvalidRecord
	}
	return nil
}

// lateStepValid 检查迟到步骤（LateStep / LateData，saga 方向 ④）的不变量：没有迟到步骤时没有载荷；有时它在已完成前缀之外，
// 记录在补偿方向或等运维处理（ManualRequired），不会在 Completed / Compensated / Failed 上。
func (r Record) lateStepValid() bool {
	if r.LateStep == 0 {
		return len(r.LateData) == 0
	}
	return r.LateStep > 0 && r.LateStep-1 >= r.CompletedSteps && len(r.LateData) <= 4<<20 &&
		(r.Phase == PhaseCompensate || r.Status == StatusManualRequired) &&
		r.Status != StatusCompleted && r.Status != StatusCompensated && r.Status != StatusFailed
}

// Command 是派发给步骤执行方的一次尝试；ID 唯一标识这次尝试，IdempotencyKey 标识所属操作。
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
	// Partition 是派发它的协调器分区：参与方把结果发回 <prefix>.<Partition>.in.res。
	Partition uint16 `json:"partition,omitempty"`
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

// Completion 是步骤执行方对一次尝试的结果：成功、拒绝（不可重试失败）或可重试失败。
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

// Validate 校验身份字段与结果组合：成功不能带错误或可重试标记，失败必须带错误。
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

// validLifecycle 校验生命周期标识：非空、至多 64 字节、能放进 subject token，且不含操作键的分隔符 ':'。
func validLifecycle(lifecycle string) bool {
	return validSubjectToken(lifecycle, 64) && !strings.Contains(lifecycle, ":")
}

func validSubjectToken(token string, maxLength int) bool {
	if token == "" || len(token) > maxLength || strings.ContainsAny(token, ".*> \t\r\n") {
		return false
	}
	return true
}

// StartRequest 是启动一个 saga 的意图。saga ID 由 (Type, BusinessKey) 派生（IDOf，选项 ID-A），不由业务指定；同一业务键
// 在终态保留期内只能有一笔，重复启动按启动摘要（类型、业务键、定义版本、Data、截止）判定“已存在”或身份冲突。
type StartRequest struct {
	Type              string    `json:"type"`
	BusinessKey       string    `json:"business_key"`
	DefinitionVersion uint32    `json:"definition_version"`
	Data              []byte    `json:"data,omitempty"`
	DeadlineAt        time.Time `json:"deadline_at,omitempty"`
	// Now 只作测试钩子：所有者以自己的时钟为准，不上线。
	Now time.Time `json:"-"`
}

// ResumeRequest 是 Resume 的参数：可设置新的截止时间，或显式清除截止时间。
type ResumeRequest struct {
	ID            string
	Now           time.Time
	DeadlineAt    time.Time
	ClearDeadline bool
}

// Query 是按类型、定义版本、状态与更新时间筛选记录的查询（List）。
type Query struct {
	Type              string
	DefinitionVersion uint32
	Statuses          []Status
	UpdatedBefore     time.Time
	Limit             int
}

// OperationClosure 是一个已关闭 operation 的关闭方式（记在分区状态的操作 tombstone 里）。
type OperationClosure uint8

const (
	// OperationClosureUnknown：没有 tombstone，或 tombstone 没有记录关闭方式。
	OperationClosureUnknown OperationClosure = iota
	// OperationClosedWithResult：协调器接收了这个 operation 的某次成功 completion 后关闭它。
	OperationClosedWithResult
	// OperationAbandoned：协调器没有接收成功就关闭了它（超时或可重试失败用尽、拒绝、saga 截止、
	// 人工 Compensate、定义缺失）；之后到达的成功说明那一步已生效却未被计入。
	OperationAbandoned
)

// definitionKey 是已登记定义的键（类型 + 版本）。
type definitionKey struct {
	typeName string
	version  uint32
}
