package saga

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

type Options struct {
	Owner              string
	CoordinatorWorkers int
	PublisherWorkers   int
	CoordinatorBatch   int
	PublisherBatch     int
	LeaseDuration      time.Duration
	StoreTimeout       time.Duration
	PollInterval       time.Duration
	PublishTimeout     time.Duration
	PublishBackoffMin  time.Duration
	PublishBackoffMax  time.Duration
	MaxPayloadBytes    int
	// StepBudgets 在 Register 时补齐步骤的超时与重试预算（配置默认值 + 按步骤覆盖，见 StepBudgets）。
	StepBudgets StepBudgets
}

func DefaultOptions() Options {
	return Options{
		Owner: "saga-" + NewID(), CoordinatorWorkers: 4, PublisherWorkers: 4,
		CoordinatorBatch: 3, PublisherBatch: 1, LeaseDuration: 15 * time.Second, StoreTimeout: 3 * time.Second, PollInterval: 100 * time.Millisecond,
		PublishTimeout: 3 * time.Second, PublishBackoffMin: 50 * time.Millisecond, PublishBackoffMax: 5 * time.Second,
		MaxPayloadBytes: 64 << 10, StepBudgets: StepBudgets{Defaults: DefaultStepBudget()},
	}
}

type StartRequest struct {
	ID                string    `json:"id,omitempty"`
	Type              string    `json:"type"`
	BusinessKey       string    `json:"business_key"`
	DefinitionVersion uint32    `json:"definition_version"`
	Data              []byte    `json:"data,omitempty"`
	DeadlineAt        time.Time `json:"deadline_at,omitempty"`
	Now               time.Time `json:"-"`
}

type ResumeRequest struct {
	ID            string
	Now           time.Time
	DeadlineAt    time.Time
	ClearDeadline bool
}

type Stats struct {
	Started, Dispatched, Completed, Compensated, Failed, ManualRequired   uint64
	Conflicts, Duplicates, PublishFailures, StoreFailures, WorkerFailures uint64
	// LateAfterAbandon 计协调器放弃一个步骤之后才到达的成功 completion（U-0280）：那一步已经生效，
	// 却不在 CompletedSteps 里。按（操作，代际）计一次（正向由回执去重，补偿方向由 LateSuccessAlarmStore，B1）。
	// 正向的由协调器自动补偿（saga 方向 ④，记 WARN）；补偿方向的只告警（ERROR），需要运维核对（TROUBLESHOOTING T-226）。
	LateAfterAbandon uint64
	// StaleIncarnation 计被协调器拒收的旧一生结果（B1）：Resume 或补偿方向的人工 Compensate 进入新一生之后，
	// 上一生某次尝试的拒绝 / 失败才到达。收件箱也不回放旧一生的拒绝，新一生照常执行，这些结果只计数。
	StaleIncarnation uint64
	// StaleAttempt 计被协调器拒收的同一生较早尝试的可重试失败（saga 方向 ③）：协调器在等之后的一次尝试，较早那次的
	// 可重试失败只说明那一次没有生效，之后的尝试照常执行，它不能推进记录。只计数、记 WARN。
	StaleAttempt uint64
	// Reopened 计 saga 被重开的次数（saga 方向 ④ 的可观测性，维护者第十三轮）：已结束的 Failed / Compensated 因为迟到的
	// 正向成功被带回补偿（reason=late_success），或 ManualRequired 期间记下的迟到步骤在运维 Resume / Compensate 时被补偿
	// （reason=resume / compensate）。指标 saga.reopened_total{saga_type,from_status,reason}，每次记 WARN。
	// 重开的 saga 补偿完会再次到达终态，Completed / Compensated / Failed 按到达终态的次数计，同一 saga 会再计一次。
	Reopened uint64
}

type Engine struct {
	store     Store
	publisher Publisher
	opts      Options

	mu          sync.RWMutex
	definitions map[definitionKey]Definition
	runMu       sync.Mutex
	running     bool
	cancel      context.CancelFunc
	done        chan struct{}
	dueKick     chan struct{}
	outboxKick  chan struct{}

	started, dispatched, completed, compensated, failed, manualRequired atomic.Uint64
	conflicts, duplicates, publishFailures                              atomic.Uint64
	storeFailures, workerFailures, lateAfterAbandon                     atomic.Uint64
	staleIncarnation, staleAttempt, reopened                            atomic.Uint64
}

func NewEngine(store Store, publisher Publisher, options Options) (*Engine, error) {
	if store == nil || publisher == nil {
		return nil, fmt.Errorf("saga: store and publisher are required")
	}
	defaults := DefaultOptions()
	if strings.TrimSpace(options.Owner) == "" {
		options.Owner = defaults.Owner
	}
	if options.CoordinatorWorkers <= 0 {
		options.CoordinatorWorkers = defaults.CoordinatorWorkers
	}
	if options.PublisherWorkers <= 0 {
		options.PublisherWorkers = defaults.PublisherWorkers
	}
	if options.CoordinatorBatch <= 0 {
		options.CoordinatorBatch = defaults.CoordinatorBatch
	}
	if options.PublisherBatch <= 0 {
		options.PublisherBatch = defaults.PublisherBatch
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = defaults.LeaseDuration
	}
	if options.StoreTimeout <= 0 {
		options.StoreTimeout = defaults.StoreTimeout
	}
	if options.PollInterval <= 0 {
		options.PollInterval = defaults.PollInterval
	}
	if options.PublishTimeout <= 0 {
		options.PublishTimeout = defaults.PublishTimeout
	}
	if options.PublishBackoffMin <= 0 {
		options.PublishBackoffMin = defaults.PublishBackoffMin
	}
	if options.PublishBackoffMax < options.PublishBackoffMin {
		options.PublishBackoffMax = defaults.PublishBackoffMax
	}
	if options.MaxPayloadBytes <= 0 {
		options.MaxPayloadBytes = defaults.MaxPayloadBytes
	}
	// Every rejected configuration names the offending field and the violated
	// budget: these cross-parameter lease constraints are the ones operators
	// cannot eyeball, and a single opaque "unsafe limits" answer forced a
	// source dive per misconfiguration.
	if options.StoreTimeout >= options.LeaseDuration {
		return nil, fmt.Errorf("saga: StoreTimeout (%v) must be < LeaseDuration (%v)", options.StoreTimeout, options.LeaseDuration)
	}
	if len(options.Owner) > 256 {
		return nil, fmt.Errorf("saga: Owner length %d exceeds 256", len(options.Owner))
	}
	if options.CoordinatorWorkers > 1024 {
		return nil, fmt.Errorf("saga: CoordinatorWorkers %d exceeds 1024", options.CoordinatorWorkers)
	}
	if options.PublisherWorkers > 1024 {
		return nil, fmt.Errorf("saga: PublisherWorkers %d exceeds 1024", options.PublisherWorkers)
	}
	if options.CoordinatorBatch > 4096 {
		return nil, fmt.Errorf("saga: CoordinatorBatch %d exceeds 4096", options.CoordinatorBatch)
	}
	if options.PublisherBatch > 4096 {
		return nil, fmt.Errorf("saga: PublisherBatch %d exceeds 4096", options.PublisherBatch)
	}
	if err := options.StepBudgets.Validate(); err != nil {
		return nil, fmt.Errorf("saga: StepBudgets: %w", err)
	}
	if options.MaxPayloadBytes > 4<<20 {
		return nil, fmt.Errorf("saga: MaxPayloadBytes %d exceeds %d", options.MaxPayloadBytes, 4<<20)
	}
	if options.LeaseDuration <= options.PublishTimeout {
		return nil, fmt.Errorf("saga: LeaseDuration (%v) must be > PublishTimeout (%v)", options.LeaseDuration, options.PublishTimeout)
	}
	leaseAfterClaim := options.LeaseDuration - options.StoreTimeout
	coordinatorBudget := leaseAfterClaim / time.Duration(options.CoordinatorBatch)
	publisherBudget := leaseAfterClaim / time.Duration(options.PublisherBatch)
	if options.StoreTimeout >= coordinatorBudget {
		return nil, fmt.Errorf("saga: coordinator budget exhausted: (LeaseDuration-StoreTimeout)/CoordinatorBatch = %v must be > StoreTimeout (%v)", coordinatorBudget, options.StoreTimeout)
	}
	if options.PublishTimeout >= publisherBudget {
		return nil, fmt.Errorf("saga: publisher budget exhausted: (LeaseDuration-StoreTimeout)/PublisherBatch = %v must be > PublishTimeout (%v)", publisherBudget, options.PublishTimeout)
	}
	if options.StoreTimeout >= publisherBudget-options.PublishTimeout {
		return nil, fmt.Errorf("saga: publisher budget exhausted: (LeaseDuration-StoreTimeout)/PublisherBatch - PublishTimeout = %v must be > StoreTimeout (%v)", publisherBudget-options.PublishTimeout, options.StoreTimeout)
	}
	return &Engine{store: store, publisher: publisher, opts: options, definitions: make(map[definitionKey]Definition), dueKick: make(chan struct{}, 1), outboxKick: make(chan struct{}, 1)}, nil
}

type definitionKey struct {
	typeName string
	version  uint32
}

// Register 先按 Options.StepBudgets 补齐步骤预算（定义里没写的字段取配置默认值，配置的按步骤覆盖优先），
// 再校验。补齐后的定义是协调器实际使用的定义。
func (e *Engine) Register(definition Definition) error {
	definition = e.opts.StepBudgets.Resolve(definition)
	if err := definition.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	key := definitionKey{typeName: definition.Type, version: definition.Version}
	if _, exists := e.definitions[key]; exists {
		return fmt.Errorf("%w: %s/%d", ErrAlreadyExists, definition.Type, definition.Version)
	}
	definition.Steps = append([]Step(nil), definition.Steps...)
	e.definitions[key] = definition
	return nil
}

// StartSaga 持久创建业务意图；相同身份重投返回当前进度，不重置已执行的步骤。
// 启动摘要与可变运行数据分离，旧记录不能证明原始身份时返回 ErrIdentityConflict。
func (e *Engine) StartSaga(ctx context.Context, request StartRequest) (Record, error) {
	if _, ok := e.definition(request.Type, request.DefinitionVersion); !ok {
		return Record{}, fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, request.Type, request.DefinitionVersion)
	}
	request.BusinessKey = strings.TrimSpace(request.BusinessKey)
	request.ID = strings.TrimSpace(request.ID)
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	if request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > e.opts.MaxPayloadBytes {
		return Record{}, ErrInvalidRecord
	}
	now := request.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	id := request.ID
	if id == "" {
		id = NewID()
	}
	record := Record{ID: id, Type: request.Type, DefinitionVersion: request.DefinitionVersion, BusinessKey: request.BusinessKey, Status: StatusPending, Phase: PhaseForward, Step: 0, Version: 1, Data: append([]byte(nil), request.Data...), NextRunAt: now, DeadlineAt: request.DeadlineAt, CreatedAt: now, UpdatedAt: now}
	// RR-20261005-NC-39：Data/DeadlineAt 是运行状态，原始启动身份必须独立持久保存。
	intentDigest, err := startIntentDigest(request)
	if err != nil {
		return Record{}, err
	}
	record.StartDigest = intentDigest
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	if err := e.store.Create(ctx, record); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			existing, getErr := e.store.GetByBusinessKey(ctx, request.Type, request.BusinessKey)
			// 只有尚未推进的旧记录能用初始状态判断；已推进记录无法恢复原始意图，明确拒绝。
			// 不用当前业务数据伪造回填摘要，否则仍会把另一个请求误认成历史重投。
			sameIntent := existing.StartDigest == intentDigest
			if existing.StartDigest == "" {
				sameIntent = existing.Version == 1 && existing.Status == StatusPending && existing.Phase == PhaseForward && existing.Step == 0 && existing.CompletedSteps == 0 && existing.Attempt == 0 && existing.Incarnation == 0 && bytes.Equal(request.Data, existing.Data) && request.DeadlineAt.Equal(canonicalDeadline(existing.DeadlineAt))
			}
			if getErr == nil && request.DefinitionVersion == existing.DefinitionVersion && (request.ID == "" || request.ID == existing.ID) && sameIntent {
				return existing, nil
			}
			if getErr == nil {
				return Record{}, ErrIdentityConflict
			}
		}
		return Record{}, err
	}
	e.started.Add(1)
	e.signal(e.dueKick)
	return record.Clone(), nil
}

// startIntentDigest 使用规范化后的启动请求；ID 单独校验，Now 只用于首次调度。
// JSON 的 omitempty 保持既有 nil/空 Data 等价；UTC 毫秒截止时间与 Mongo 精度一致。
func startIntentDigest(request StartRequest) (string, error) {
	// 固定摘要格式，不因以后 StartRequest 增加选项而改变既存记录的身份。
	intent := struct {
		Type              string    `json:"type"`
		BusinessKey       string    `json:"business_key"`
		DefinitionVersion uint32    `json:"definition_version"`
		Data              []byte    `json:"data,omitempty"`
		DeadlineAt        time.Time `json:"deadline_at,omitempty"`
	}{request.Type, request.BusinessKey, request.DefinitionVersion, request.Data, request.DeadlineAt}
	raw, err := json.Marshal(intent)
	if err != nil {
		return "", fmt.Errorf("%w: start intent digest: %v", ErrInvalidRecord, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (e *Engine) Get(ctx context.Context, id string) (Record, error) { return e.store.Get(ctx, id) }
func (e *Engine) List(ctx context.Context, query Query) ([]Record, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > 1000 {
		return nil, ErrInvalidRecord
	}
	return e.store.List(ctx, query)
}

// Resume retries a terminal failure after an operator or automated repair has
// removed its cause. Completed and compensated Sagas cannot be resumed.
func (e *Engine) Resume(ctx context.Context, request ResumeRequest) (Record, error) {
	request.ID = strings.TrimSpace(request.ID)
	if !validSubjectToken(request.ID, 128) || (request.ClearDeadline && !request.DeadlineAt.IsZero()) {
		return Record{}, ErrInvalidRecord
	}
	now := request.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	deadline := canonicalDeadline(request.DeadlineAt)
	if !deadline.IsZero() && !deadline.After(now) {
		return Record{}, ErrDeadlineExpired
	}
	for attempts := 0; attempts < 8; attempts++ {
		record, err := e.store.Get(ctx, request.ID)
		if err != nil {
			return Record{}, err
		}
		if record.Status != StatusFailed && record.Status != StatusManualRequired {
			return Record{}, fmt.Errorf("saga: status %d cannot resume", record.Status)
		}
		if _, ok := e.definition(record.Type, record.DefinitionVersion); !ok {
			return Record{}, fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, record.Type, record.DefinitionVersion)
		}
		after := record.Clone()
		after.Version++
		after.UpdatedAt = now
		after.Attempt = 0
		after.LastError = ""
		after.CommandID = ""
		after.OperationKey = ""
		after.NextRunAt = now
		if request.ClearDeadline {
			after.DeadlineAt = time.Time{}
		} else if !deadline.IsZero() {
			after.DeadlineAt = deadline
		} else if after.Phase == PhaseForward && !after.DeadlineAt.IsZero() && !now.Before(after.DeadlineAt) {
			return Record{}, ErrDeadlineExpired
		}
		clearLease(&after)
		if record.Phase == PhaseCompensate || record.CompletedSteps > 0 || record.LateStep > 0 {
			// 补偿：先补放弃后才生效的那一步（saga 方向 ④），再按倒序补已完成的前缀。
			after = nextCompensation(after, now)
		} else {
			after.Phase = PhaseForward
			after.Status = StatusPending
		}
		// 新一生（Incarnation+1）由 stepTransition 开：之后的 CommandID 与 Resume 之前的所有回执不相交。
		written, _, err := e.stepTransition(ctx, record, after, transition{cause: causeResume})
		if errors.Is(err, ErrConflict) {
			e.conflicts.Add(1)
			continue
		}
		if err != nil {
			return Record{}, err
		}
		if record.LateStep > 0 {
			e.reportReopen(record, written, reopenResume)
		}
		e.signal(e.dueKick)
		return written, nil
	}
	return Record{}, ErrConflict
}

// Compensate requests semantic rollback of every completed step. It is safe to
// call repeatedly; already compensating or compensated records are returned.
func (e *Engine) Compensate(ctx context.Context, id, reason string, now time.Time) (Record, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for attempts := 0; attempts < 8; attempts++ {
		record, err := e.store.Get(ctx, id)
		if err != nil {
			return Record{}, err
		}
		if record.Status == StatusCompensated || record.Status == StatusCompensating {
			return record, nil
		}
		if record.Status == StatusWaiting {
			return Record{}, fmt.Errorf("saga: cannot force compensation while a step result is in flight")
		}
		if record.CompletedSteps == 0 && record.LateStep == 0 {
			return Record{}, fmt.Errorf("saga: no completed steps to compensate")
		}
		// 当前步骤正在重试退避时人工补偿放弃了它；补偿方向停下的记录进入新一生（B1）。两者都由 stepTransition 决定。
		written, _, err := e.stepTransition(ctx, record, e.beginCompensation(record, reason, now), transition{cause: causeManualCompensate})
		if errors.Is(err, ErrConflict) {
			e.conflicts.Add(1)
			continue
		}
		if err != nil {
			return Record{}, err
		}
		if record.LateStep > 0 {
			e.reportReopen(record, written, reopenCompensate)
		}
		e.signal(e.dueKick)
		return written, nil
	}
	return Record{}, ErrConflict
}

// Complete 接收一次尝试的结果。协调器对每个操作（saga + 方向 + 步骤，即 IdempotencyKey）记着当前代际
// （Record.Incarnation，Resume / 补偿方向的人工 Compensate 递增）与当前等待的尝试；completion 的代际从
// CommandID 解析（commandIDIncarnation）。判定顺序（B1，对齐 SAGA.md「原生步骤执行契约」第 3 条）：
//
//  1. 旧一生（或比记录还新、不可能由协调器产生）的拒绝 / 失败：不接收，只计数（StaleIncarnation）。
//     收件箱同样不回放旧一生的拒绝，新一生的尝试照常执行。
//  2. 旧一生的成功：记录正停在这个操作上（在等它，或 Resume 后还没派发、新一生的尝试在退避）就接收为该操作的
//     结果——成功在任何一生里都不重做，新一生的尝试在收件箱里看到它也只会回放。
//  3. 同一生、记录在等这个操作：成功与拒绝（操作的结论）从哪次尝试来都接收——收件箱不让之后的尝试再执行，下一次尝试
//     回放的正是较早那次的 completion；可重试失败（尝试的结论）只接收正在等的那次尝试的（saga 方向 ③）：较早尝试的
//     可重试失败只说明那一次没生效，正在等的尝试照常执行，接收它会用后一次的尝试计数判用尽、放弃正在执行的尝试（O-S5-7）。
//  4. 其余按回执与 tombstone 判断重复或“放弃后迟到的成功”（completeNotWaiting）。
func (e *Engine) Complete(ctx context.Context, completion Completion) (Record, error) {
	if completion.Validate() != nil {
		return Record{}, ErrInvalidRecord
	}
	if len(completion.Data) > e.opts.MaxPayloadBytes {
		return Record{}, ErrInvalidRecord
	}
	// Coordinator time owns ordering, deadlines and receipt TTL. A remote step
	// clock must not move Saga state backwards or expire deduplication records.
	completion.CompletedAt = time.Now().UTC()
	incarnation := commandIDIncarnation(completion.IdempotencyKey, completion.CommandID)
	for attempts := 0; attempts < 8; attempts++ {
		record, err := e.store.Get(ctx, completion.SagaID)
		if err != nil {
			return Record{}, err
		}
		var accept bool
		switch {
		case incarnation != record.Incarnation && (!completion.Success || incarnation > record.Incarnation):
			e.reportStaleIncarnation(record, completion, incarnation)
			return record, nil
		case incarnation != record.Incarnation:
			accept = positionedAt(record, completion.IdempotencyKey)
		default:
			accept = record.Status == StatusWaiting && record.OperationKey == completion.IdempotencyKey
			if accept && completion.Retryable && completion.CommandID != record.CommandID {
				e.reportStaleAttempt(record, completion)
				return record, nil
			}
		}
		if !accept {
			written, err := e.completeNotWaiting(ctx, record, completion, incarnation)
			if errors.Is(err, ErrConflict) {
				e.conflicts.Add(1)
				continue
			}
			return written, err
		}
		definition, ok := e.definition(record.Type, record.DefinitionVersion)
		if !ok {
			return Record{}, fmt.Errorf("%w: %s", ErrDefinitionMissing, record.Type)
		}
		after := e.applyCompletion(record, definition, completion)
		_, outcome, err := e.stepTransition(ctx, record, after, transition{cause: causeResult, receipt: &completion})
		if errors.Is(err, ErrConflict) {
			e.conflicts.Add(1)
			continue
		}
		if err != nil {
			return Record{}, err
		}
		if outcome == ApplyDuplicate {
			e.duplicates.Add(1)
			return e.store.Get(ctx, record.ID)
		}
		if completion.Success && compensatingLateStep(record) {
			// 重开（reportReopen）的另一端：迟到的那一步补偿完，运维按 saga_id 把两条日志对上。
			slog.Info("saga: compensated the step that took effect after the coordinator abandoned it",
				"saga_id", record.ID, "saga_type", record.Type, "late_step", record.Step, "status", after.Status.String(), "version", after.Version)
		}
		e.countTerminal(after.Status)
		e.signal(e.dueKick)
		return after.Clone(), nil
	}
	return Record{}, ErrConflict
}

// positionedAt 判断记录是否正停在这个操作上：在等它的某次尝试，或者下一步就要派发它（Resume 之后还没派发、
// 本生的尝试在退避）。
func positionedAt(record Record, operation string) bool {
	switch record.Status {
	case StatusWaiting:
		return record.OperationKey == operation
	case StatusPending, StatusCompensating:
		return operationKey(record.ID, record.Phase, record.Step) == operation
	default:
		return false
	}
}

// completeNotWaiting 处理协调器不接收的 completion：已记录的按重复确认；放弃关闭之后才到的正向成功交给
// compensateLateStep 补偿那一步（saga 方向 ④），补偿方向的只告警，按（操作，completion 的代际）只告警一次（B1）；
// 协调器既不在等、也没有记录的返回 ErrNotWaiting。写记录冲突时返回 ErrConflict，由 Complete 重读重试。
func (e *Engine) completeNotWaiting(ctx context.Context, record Record, completion Completion, incarnation uint32) (Record, error) {
	history, err := e.completionHistory(ctx, completion)
	if err != nil {
		return Record{}, err
	}
	if !history.Recorded {
		return Record{}, ErrNotWaiting
	}
	if completion.Success && !history.Receipt && history.Closure == OperationAbandoned {
		if step, ok := lateForwardStep(record, completion.IdempotencyKey); ok {
			return e.compensateLateStep(ctx, record, completion, step)
		}
		first, err := e.markLateSuccessAlarm(ctx, completion, incarnation)
		if err != nil {
			return Record{}, err
		}
		if first {
			e.reportLateAfterAbandon(record, completion)
			return record, nil
		}
	}
	e.duplicates.Add(1)
	return record, nil
}

// markLateSuccessAlarm 在 tombstone 上记下“这个操作在这一代际放弃后迟到的成功已告警”，first=true 表示第一次。
// 同一个已生效的成功会经 effect 重投、过期投递的回放、JetStream 重投多次送达，告警按步骤计，不按送达计。
// Store 没实现 LateSuccessAlarmStore 时每次都告警（宁可重复，不丢告警）。
func (e *Engine) markLateSuccessAlarm(ctx context.Context, completion Completion, incarnation uint32) (bool, error) {
	if marker, ok := e.store.(LateSuccessAlarmStore); ok {
		return marker.MarkLateSuccessAlarm(ctx, completion, incarnation)
	}
	return true, nil
}

// lateForwardStep 判断一份放弃后迟到的成功能否由协调器补偿（saga 方向 ④），返回那一步的步骤号。只处理正向操作，
// 且记录已离开正向（补偿方向、Failed，或等运维的 ManualRequired）。下面几种按 4.3 的论证不会发生，出现时退回只告警：
// 记录还在正向非终态、已记着另一个迟到步骤、或这一步在已完成前缀之内。
func lateForwardStep(record Record, operation string) (int, bool) {
	phase, step, ok := parseOperationKey(record.ID, operation)
	if !ok || phase != PhaseForward || step < record.CompletedSteps || (record.LateStep != 0 && record.LateStep != step+1) {
		return 0, false
	}
	return step, record.Phase == PhaseCompensate || record.Status == StatusFailed || record.Status == StatusManualRequired
}

// compensateLateStep 是方向 C：第 step 步在协调器放弃之后生效了，把它补偿掉。一个 Store 事务里记下这份 completion 的回执、
// 把 tombstone 改为带结果关闭（stepTransition 收到成功回执时的既有行为；之后的送达按回执去重），并在记录上写 LateStep / LateData：
//
//   - 记录在两个补偿之间（Compensating 且还没派发）或已经终态（Failed / Compensated）：立即转去补偿这一步，终态被重开；
//   - 某个补偿在等结果或在重试退避：不打断它（它可能生效），它接收结果后 nextCompensation 先补这一步；
//   - ManualRequired：只记下，运维 Resume / Compensate 时先补这一步（等运维的记录不自动跑）。
func (e *Engine) compensateLateStep(ctx context.Context, record Record, completion Completion, step int) (Record, error) {
	now := completion.CompletedAt
	after := record.Clone()
	after.Version++
	after.UpdatedAt = now
	after.LateStep = step + 1
	after.LateData = append([]byte(nil), completion.Data...)
	clearLease(&after)
	if openOperation(record) == "" && record.Status != StatusManualRequired {
		after.LastError = fmt.Sprintf("step %d took effect after the coordinator abandoned it", step)
		after = nextCompensation(after, now)
	}
	written, outcome, err := e.stepTransition(ctx, record, after, transition{cause: causeLateSuccess, receipt: &completion})
	if err != nil {
		return Record{}, err
	}
	if outcome == ApplyDuplicate {
		e.duplicates.Add(1)
		return e.store.Get(ctx, record.ID)
	}
	e.reportLateCompensation(record, completion)
	if record.Status == StatusFailed || record.Status == StatusCompensated {
		e.reportReopen(record, written, reopenLateSuccess)
	}
	e.signal(e.dueKick)
	return written.Clone(), nil
}

// saga.reopened_total 的 reason 标签：重开从哪里来。取值固定三个，保持低基数。
const (
	// reopenLateSuccess：已结束的 Failed / Compensated 收到放弃后迟到的正向成功，协调器立即把它带回补偿（compensateLateStep）。
	reopenLateSuccess = "late_success"
	// reopenResume / reopenCompensate：ManualRequired 期间记下的迟到步骤（只记 LateStep、不自动跑），运维 Resume / Compensate 时先补它。
	reopenResume     = "resume"
	reopenCompensate = "compensate"
)

// reportReopen 记一次 saga 重开（saga 方向 ④ 的可观测性，维护者第十三轮）：before 是重开前的记录（它的状态就是原终态），
// after 是已写入的记录（正在补偿迟到那一步）。saga 自己没有向业务推送终态的机制，按终态做业务的一方靠 Get / List 读记录，
// 识别方法见 SAGA.md「运维观察」：记下读到终态时的 Version，之后 Version 变大就是记录被改过（重开或 Resume）。
// 标签只有 saga 类型、原状态与原因，saga id 与步骤号只进日志。
func (e *Engine) reportReopen(before, after Record, reason string) {
	e.reopened.Add(1)
	from := before.Status.String()
	metrics.IncCounter("saga.reopened_total", metrics.Labels{"saga_type": before.Type, "from_status": from, "reason": reason}, 1)
	slog.Warn("saga: reopened to compensate a step that took effect after the coordinator abandoned it",
		"saga_id", before.ID, "saga_type", before.Type, "from_status", from, "reason", reason,
		"late_step", after.LateStep-1, "status", after.Status.String(), "version", after.Version)
}

// nextCompensation 选下一个要补偿的步骤：先补放弃后才生效的那一步（LateStep，saga 方向 ④），再按 CompletedSteps 倒序；
// 都补完是 Compensated。开始补偿（compensationState）、补偿成功（applyCompletion）、Resume、迟到成功到达都经这里，
// 选择规则只有这一处。after 的版本、时间、错误由调用方设置。
func nextCompensation(after Record, now time.Time) Record {
	after.Phase = PhaseCompensate
	after.Attempt = 0
	after.CommandID = ""
	after.OperationKey = ""
	switch {
	case after.LateStep > 0:
		after.Status = StatusCompensating
		after.Step = after.LateStep - 1
		after.NextRunAt = now
	case after.CompletedSteps > 0:
		after.Status = StatusCompensating
		after.Step = after.CompletedSteps - 1
		after.NextRunAt = now
	default:
		after.Status = StatusCompensated
		after.NextRunAt = time.Time{}
	}
	return after
}

// compensatingLateStep 表示记录当前的补偿操作是迟到的那一步（不是已完成前缀里的一步）。
func compensatingLateStep(record Record) bool {
	return record.Phase == PhaseCompensate && record.LateStep > 0 && record.Step == record.LateStep-1
}

// reportLateCompensation 记一次由协调器自动补偿的迟到成功（saga 方向 ④）：仍计 late_after_abandon（phase=forward），
// 但已经有了归宿，只记 WARN。
func (e *Engine) reportLateCompensation(record Record, completion Completion) {
	e.lateAfterAbandon.Add(1)
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.late_after_abandon_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Warn("saga: step succeeded after the coordinator abandoned it; compensating it",
		"saga_id", completion.SagaID, "saga_type", record.Type, "status", record.Status.String(), "step", step,
		"command_id", completion.CommandID, "operation", completion.IdempotencyKey)
}

// reportStaleAttempt 记一份被拒收的同一生较早尝试的可重试失败（saga 方向 ③）：协调器在等之后的那次尝试。
func (e *Engine) reportStaleAttempt(record Record, completion Completion) {
	e.staleAttempt.Add(1)
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.stale_attempt_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Warn("saga: ignored a retryable failure from an earlier attempt; waiting for the current attempt",
		"saga_id", completion.SagaID, "saga_type", record.Type, "phase", phase, "step", step,
		"command_id", completion.CommandID, "waiting_for", record.CommandID, "error", completion.Error)
}

// reportStaleIncarnation 记一份被拒收的旧一生结果（B1）。这是 Resume 之后的正常现象，不是故障：只计数、记 WARN。
func (e *Engine) reportStaleIncarnation(record Record, completion Completion, incarnation uint32) {
	e.staleIncarnation.Add(1)
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.stale_incarnation_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Warn("saga: ignored a step result from an earlier incarnation of the saga",
		"saga_id", completion.SagaID, "saga_type", record.Type, "phase", phase, "step", step, "command_id", completion.CommandID,
		"result_incarnation", incarnation, "incarnation", record.Incarnation, "success", completion.Success, "error", completion.Error)
}

// completionHistory 用 Store 的可选扩展区分重复结果与放弃后到达的成功；没有扩展时退回
// CompletionRecorded，一律按重复处理。
func (e *Engine) completionHistory(ctx context.Context, completion Completion) (CompletionHistory, error) {
	if history, ok := e.store.(CompletionHistoryStore); ok {
		return history.CompletionHistory(ctx, completion)
	}
	recorded, err := e.store.CompletionRecorded(ctx, completion)
	return CompletionHistory{Recorded: recorded}, err
}

// reportLateAfterAbandon 告警一份协调器不能自己处理的“放弃之后才到的成功”（U-0280 C'）：补偿方向的操作（正向的由
// compensateLateStep 补偿，saga 方向 ④），以及 lateForwardStep 判为不应发生的情形。运维按 TROUBLESHOOTING T-226 核对；
// 补偿方向停在 ManualRequired 时 Resume，新一生的同一补偿会回放这次成功而不是再执行一次。标签只有 saga 类型与方向，保持低基数。
func (e *Engine) reportLateAfterAbandon(record Record, completion Completion) {
	e.lateAfterAbandon.Add(1)
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.late_after_abandon_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Error("saga: step succeeded after the coordinator abandoned it; the coordinator cannot account for it",
		"saga_id", completion.SagaID, "saga_type", record.Type, "status", record.Status.String(), "phase", phase, "step", step,
		"command_id", completion.CommandID, "operation", completion.IdempotencyKey)
}

// operationPosition 从 operationKey（sagaID:phase:step，见 operationKey）取出方向与步骤号，用于告警标签与日志。
func operationPosition(operation string) (phase, step string) {
	cut := strings.LastIndexByte(operation, ':')
	if cut <= 0 {
		return "unknown", ""
	}
	rest, step := operation[:cut], operation[cut+1:]
	switch rest[strings.LastIndexByte(rest, ':')+1:] {
	case strconv.Itoa(int(PhaseForward)):
		return PhaseForward.String(), step
	case strconv.Itoa(int(PhaseCompensate)):
		return PhaseCompensate.String(), step
	default:
		return "unknown", step
	}
}

func (e *Engine) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.runMu.Lock()
	if e.running {
		e.runMu.Unlock()
		return fmt.Errorf("saga: engine already running")
	}
	ctx, cancel := context.WithCancel(ctx)
	e.running, e.cancel, e.done = true, cancel, make(chan struct{})
	done := e.done
	e.runMu.Unlock()
	defer func() { e.runMu.Lock(); e.running = false; close(done); e.runMu.Unlock() }()

	var wg sync.WaitGroup
	for i := 0; i < e.opts.CoordinatorWorkers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.coordinatorLoop(ctx) }()
	}
	for i := 0; i < e.opts.PublisherWorkers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.publisherLoop(ctx) }()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (e *Engine) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.runMu.Lock()
	cancel, done := e.cancel, e.done
	e.runMu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) Stats() Stats {
	return Stats{Started: e.started.Load(), Dispatched: e.dispatched.Load(), Completed: e.completed.Load(), Compensated: e.compensated.Load(), Failed: e.failed.Load(), ManualRequired: e.manualRequired.Load(), Conflicts: e.conflicts.Load(), Duplicates: e.duplicates.Load(), PublishFailures: e.publishFailures.Load(), StoreFailures: e.storeFailures.Load(), WorkerFailures: e.workerFailures.Load(), LateAfterAbandon: e.lateAfterAbandon.Load(), StaleIncarnation: e.staleIncarnation.Load(), StaleAttempt: e.staleAttempt.Load(), Reopened: e.reopened.Load()}
}

func (e *Engine) coordinatorLoop(ctx context.Context) {
	for ctx.Err() == nil {
		now := time.Now().UTC()
		storeCtx, cancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
		records, err := e.store.ClaimDue(storeCtx, ClaimRequest{Owner: e.opts.Owner, Now: now, LeaseDuration: e.opts.LeaseDuration, Limit: e.opts.CoordinatorBatch})
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				e.storeFailures.Add(1)
				slog.Error("saga: claim due", "err", err)
			}
			e.wait(ctx, e.dueKick)
			continue
		}
		if len(records) == 0 {
			e.wait(ctx, e.dueKick)
			continue
		}
		for i := range records {
			storeCtx, cancel = context.WithTimeout(ctx, e.opts.StoreTimeout)
			err = e.processClaimed(storeCtx, records[i], time.Now().UTC())
			cancel()
			if err != nil {
				if errors.Is(err, ErrConflict) {
					e.conflicts.Add(1)
					continue
				}
				if errors.Is(err, context.Canceled) {
					continue
				}
				e.workerFailures.Add(1)
				slog.Error("saga: process", "id", records[i].ID, "err", err)
			}
		}
	}
}

func (e *Engine) publisherLoop(ctx context.Context) {
	for ctx.Err() == nil {
		now := time.Now().UTC()
		storeCtx, cancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
		items, err := e.store.ClaimOutbox(storeCtx, ClaimRequest{Owner: e.opts.Owner, Now: now, LeaseDuration: e.opts.LeaseDuration, Limit: e.opts.PublisherBatch})
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				e.storeFailures.Add(1)
				slog.Error("saga: claim outbox", "err", err)
			}
			e.wait(ctx, e.outboxKick)
			continue
		}
		if len(items) == 0 {
			e.wait(ctx, e.outboxKick)
			continue
		}
		for i := range items {
			publishCtx, cancel := context.WithTimeout(ctx, e.opts.PublishTimeout)
			err := e.publisher.PublishSagaCommand(publishCtx, items[i].Command.Clone())
			cancel()
			if err != nil {
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					continue
				}
				e.publishFailures.Add(1)
				next := now.Add(backoff(items[i].Command.ID, items[i].Attempt+1, e.opts.PublishBackoffMin, e.opts.PublishBackoffMax))
				storeCtx, storeCancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
				nackErr := e.store.NackOutbox(storeCtx, items[i].Command.ID, items[i].Lease, next, err.Error())
				storeCancel()
				if errors.Is(nackErr, ErrConflict) {
					e.conflicts.Add(1)
				} else if nackErr != nil && !errors.Is(nackErr, context.Canceled) {
					e.storeFailures.Add(1)
					slog.Error("saga: nack outbox", "id", items[i].Command.ID, "err", nackErr)
				}
				continue
			}
			storeCtx, storeCancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
			ackErr := e.store.AckOutbox(storeCtx, items[i].Command.ID, items[i].Lease)
			storeCancel()
			if errors.Is(ackErr, ErrConflict) {
				e.conflicts.Add(1)
			} else if ackErr != nil && !errors.Is(ackErr, context.Canceled) {
				e.storeFailures.Add(1)
				slog.Error("saga: ack outbox", "id", items[i].Command.ID, "err", ackErr)
			}
		}
	}
}

func (e *Engine) processClaimed(ctx context.Context, record Record, now time.Time) error {
	definition, ok := e.definition(record.Type, record.DefinitionVersion)
	if !ok {
		after := record.Clone()
		after.Status = StatusManualRequired
		after.LastError = fmt.Sprintf("definition not registered: %s/%d", record.Type, record.DefinitionVersion)
		after.Version++
		after.UpdatedAt = now
		after.NextRunAt = time.Time{}
		after.CommandID = ""
		after.OperationKey = ""
		clearLease(&after)
		// RR-20261005-NC-250：在等结果或正在重试退避的操作都被放弃（stepTransition 关闭它）。
		_, _, err := e.stepTransition(ctx, record, after, transition{cause: causeDefinitionMissing, fenced: true})
		if err == nil {
			e.countTerminal(after.Status)
		}
		return err
	}
	if !record.DeadlineAt.IsZero() && !now.Before(record.DeadlineAt) && record.Phase == PhaseForward {
		after := e.beginCompensation(record, "saga deadline exceeded", now)
		_, _, err := e.stepTransition(ctx, record, after, transition{cause: causeDeadline, fenced: true})
		if err == nil {
			e.countTerminal(after.Status)
			e.signal(e.dueKick)
		}
		return err
	}
	if record.Status == StatusWaiting {
		after := e.retryOrCompensate(record, definition, "step result timeout", now)
		_, _, err := e.stepTransition(ctx, record, after, transition{cause: causeTimeout, fenced: true})
		if err == nil {
			e.countTerminal(after.Status)
			e.signal(e.dueKick)
		}
		return err
	}
	step, ok := stepFor(record, definition)
	if !ok {
		after := record.Clone()
		after.Status = StatusManualRequired
		after.LastError = "invalid saga step"
		after.Version++
		after.UpdatedAt = now
		clearLease(&after)
		_, _, err := e.stepTransition(ctx, record, after, transition{cause: causeInvalidStep, fenced: true})
		if err == nil {
			e.failed.Add(1)
			e.manualRequired.Add(1)
		}
		return err
	}
	after := record.Clone()
	after.Status = StatusWaiting
	after.Attempt++
	after.Version++
	after.UpdatedAt = now
	after.NextRunAt = now.Add(step.Timeout)
	after.OperationKey = operationKey(record.ID, record.Phase, record.Step)
	after.CommandID = commandID(after.OperationKey, after.Incarnation, after.Attempt)
	clearLease(&after)
	topic := step.ForwardTopic
	if record.Phase == PhaseCompensate {
		topic = step.CompensateTopic
		if topic == "" {
			topic = step.ForwardTopic + ".compensate"
		}
	}
	payload := record.Data
	if compensatingLateStep(record) {
		// 补偿迟到生效的那一步，载荷是它自己的正向成功 Data（saga 方向 ④）。
		payload = record.LateData
	}
	command := Command{ID: after.CommandID, IdempotencyKey: after.OperationKey, SagaID: record.ID, SagaType: record.Type, DefinitionVersion: record.DefinitionVersion, BusinessKey: record.BusinessKey, Step: record.Step, StepName: step.Name, Phase: record.Phase, Attempt: after.Attempt, Topic: topic, Payload: append([]byte(nil), payload...), DeadlineAt: after.NextRunAt, CreatedAt: now}
	outbox := &OutboxRecord{Command: command, NextAttemptAt: now, CreatedAt: now}
	_, _, err := e.stepTransition(ctx, record, after, transition{cause: causeDispatch, fenced: true, outbox: outbox})
	if err == nil {
		e.dispatched.Add(1)
		e.signal(e.outboxKick)
	}
	return err
}

func (e *Engine) applyCompletion(record Record, definition Definition, result Completion) Record {
	now := result.CompletedAt
	after := record.Clone()
	after.Version++
	after.UpdatedAt = now
	after.CommandID = ""
	after.OperationKey = ""
	clearLease(&after)
	if result.Success {
		late := compensatingLateStep(record)
		if result.Data != nil && !late {
			after.Data = append(after.Data[:0], result.Data...)
		}
		after.Attempt = 0
		after.LastError = ""
		if record.Phase == PhaseForward {
			after.CompletedSteps = record.Step + 1
			after.Step++
			if !record.DeadlineAt.IsZero() && !now.Before(record.DeadlineAt) {
				return compensationState(after, "saga deadline exceeded after step completion", now)
			}
			if after.Step >= len(definition.Steps) {
				after.Status = StatusCompleted
				after.NextRunAt = time.Time{}
			} else {
				after.Status = StatusPending
				after.NextRunAt = now
			}
		} else {
			if late {
				// 迟到的那一步补偿完了（saga 方向 ④）：它不在已完成前缀里，CompletedSteps 不变；它的结果不进入 Data 链。
				after.LateStep, after.LateData = 0, nil
			} else {
				after.CompletedSteps--
			}
			after = nextCompensation(after, now)
		}
		return after
	}
	if !result.Retryable {
		if after.Phase == PhaseCompensate {
			after.Status = StatusManualRequired
			after.NextRunAt = time.Time{}
			return after
		}
		return compensationState(after, result.Error, now)
	}
	return e.retryState(after, definition, result.Error, now)
}

// retryOrCompensate is the transition from a record as stored (the claim
// loop's timeout path): one version step, then the retry-or-compensate
// decision. applyCompletion, which has already advanced the version, calls
// retryState directly (U-0225, see beginCompensation).
func (e *Engine) retryOrCompensate(record Record, definition Definition, reason string, now time.Time) Record {
	after := record.Clone()
	after.Version = record.Version + 1
	return e.retryState(after, definition, reason, now)
}

// retryState schedules another attempt of the current step while attempts
// remain, and otherwise moves to compensation (or manual_required when the
// failing step was itself a compensation). after's version is already
// advanced by the caller.
func (e *Engine) retryState(after Record, definition Definition, reason string, now time.Time) Record {
	record := after
	after.UpdatedAt = now
	after.LastError = reason
	after.CommandID = ""
	after.OperationKey = ""
	clearLease(&after)
	step, ok := stepFor(record, definition)
	if !ok {
		after.Status = StatusManualRequired
		after.NextRunAt = time.Time{}
		return after
	}
	if after.Attempt < step.MaxAttempts {
		if after.Phase == PhaseForward {
			after.Status = StatusPending
		} else {
			after.Status = StatusCompensating
		}
		after.NextRunAt = now.Add(backoff(record.ID, after.Attempt, step.BackoffMin, step.BackoffMax))
		return after
	}
	if after.Phase == PhaseCompensate {
		after.Status = StatusManualRequired
		after.NextRunAt = time.Time{}
		return after
	}
	return compensationState(after, reason, now)
}

// beginCompensation is the transition from a record as stored: it advances
// the version once and then turns the record into its compensation state.
// Callers that have already advanced the version (applyCompletion,
// retryOrCompensate) use compensationState directly — beginCompensation on
// an already-advanced clone produced expected+2, which MongoStore.Apply
// rejects as an invalid record, so no step refusal or exhausted retry ever
// reached compensation on Mongo (U-0225; the memory store used in tests only
// compares the current version and never saw it).
func (e *Engine) beginCompensation(record Record, reason string, now time.Time) Record {
	after := record.Clone()
	after.Version = record.Version + 1
	return compensationState(after, reason, now)
}

// compensationState fills in the compensation (or terminal failure) state on
// a record whose version the caller has already advanced.
func compensationState(after Record, reason string, now time.Time) Record {
	after.UpdatedAt = now
	after.LastError = reason
	after.Attempt = 0
	after.CommandID = ""
	after.OperationKey = ""
	clearLease(&after)
	if after.CompletedSteps == 0 && after.LateStep == 0 {
		after.Status = StatusFailed
		after.NextRunAt = time.Time{}
		return after
	}
	return nextCompensation(after, now)
}

func stepFor(record Record, definition Definition) (Step, bool) {
	if record.Step < 0 || record.Step >= len(definition.Steps) {
		return Step{}, false
	}
	return definition.Steps[record.Step], true
}
func operationKey(id string, phase Phase, step int) string {
	return fmt.Sprintf("%s:%d:%d", id, phase, step)
}

// parseOperationKey 是 operationKey 的逆：取出这个 saga 的操作的方向与步骤号；不是这个 saga 的操作返回 false。
func parseOperationKey(id, operation string) (Phase, int, bool) {
	rest, ok := strings.CutPrefix(operation, id+":")
	if !ok {
		return 0, 0, false
	}
	phaseText, stepText, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, 0, false
	}
	phase, err := strconv.Atoi(phaseText)
	if err != nil || (Phase(phase) != PhaseForward && Phase(phase) != PhaseCompensate) {
		return 0, 0, false
	}
	step, err := strconv.Atoi(stepText)
	if err != nil || step < 0 {
		return 0, 0, false
	}
	return Phase(phase), step, true
}

// commandID names one dispatch attempt. Incarnation 0 keeps the historical
// "operationKey:attempt" format so records and receipts that predate the
// field survive an upgrade unchanged; resumed records carry a non-zero
// incarnation and therefore mint identifiers disjoint from every earlier life.
func commandID(operationKey string, incarnation, attempt uint32) string {
	if incarnation == 0 {
		return fmt.Sprintf("%s:%d", operationKey, attempt)
	}
	return fmt.Sprintf("%s:r%d:%d", operationKey, incarnation, attempt)
}

// commandIDIncarnation 是 commandID 的逆：从 operationKey:attempt / operationKey:rN:attempt 取出代际 N。
// 不是这个格式的 CommandID（测试或手工命令）按第 0 代处理。协调器核对 completion 的代际（B1）与原生收件箱
// 判断拒绝是否同一生（U-0280）都用它，两边对同一个 ID 必须得出同一个代际。
func commandIDIncarnation(operationKey, commandID string) uint32 {
	rest, ok := strings.CutPrefix(commandID, operationKey+":r")
	if !ok {
		return 0
	}
	digits, _, ok := strings.Cut(rest, ":")
	if !ok {
		return 0
	}
	value, err := strconv.ParseUint(digits, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(value)
}
func clearLease(record *Record) { record.Lease = Lease{} }
func canonicalDeadline(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	// BSON datetime and many SQL backends persist milliseconds. Canonicalizing
	// before identity comparison keeps exact start-intent redelivery idempotent.
	return value.UTC().Truncate(time.Millisecond)
}

func (e *Engine) definition(name string, version uint32) (Definition, bool) {
	e.mu.RLock()
	d, ok := e.definitions[definitionKey{typeName: name, version: version}]
	e.mu.RUnlock()
	return d, ok
}
func (e *Engine) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func (e *Engine) wait(ctx context.Context, kick <-chan struct{}) {
	timer := time.NewTimer(e.opts.PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-kick:
	case <-timer.C:
	}
}
func (e *Engine) countTerminal(status Status) {
	switch status {
	case StatusCompleted:
		e.completed.Add(1)
	case StatusCompensated:
		e.compensated.Add(1)
	case StatusFailed:
		e.failed.Add(1)
	case StatusManualRequired:
		e.failed.Add(1)
		e.manualRequired.Add(1)
	}
}

func backoff(key string, attempt uint32, minimum, maximum time.Duration) time.Duration {
	if minimum <= 0 {
		minimum = time.Millisecond
	}
	if maximum < minimum {
		maximum = minimum
	}
	d := minimum
	for i := uint32(1); i < attempt && d < maximum; i++ {
		if d > maximum/2 {
			d = maximum
			break
		}
		d *= 2
	}
	if d > maximum {
		d = maximum
	}
	if d <= 1 {
		return d
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	var raw [4]byte
	binaryPutUint32(raw[:], attempt)
	_, _ = h.Write(raw[:])
	half := d / 2
	return half + time.Duration(uint64(h.Sum32())%uint64(d-half+1))
}
func binaryPutUint32(dst []byte, value uint32) {
	dst[0] = byte(value >> 24)
	dst[1] = byte(value >> 16)
	dst[2] = byte(value >> 8)
	dst[3] = byte(value)
}
