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
	// 却不在 CompletedSteps 里、不会被补偿。按（操作，代际）计一次（Store 实现 LateSuccessAlarmStore 时，B1），
	// 每次记 ERROR，需要运维核对（见 TROUBLESHOOTING T-226）。
	LateAfterAbandon uint64
	// StaleIncarnation 计被协调器拒收的旧一生结果（B1）：Resume 或补偿方向的人工 Compensate 进入新一生之后，
	// 上一生某次尝试的拒绝 / 失败才到达。收件箱也不回放旧一生的拒绝，新一生照常执行，这些结果只计数。
	StaleIncarnation uint64
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
	staleIncarnation                                                    atomic.Uint64
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
		if record.Phase == PhaseCompensate || record.CompletedSteps > 0 {
			after.Phase = PhaseCompensate
			after.Status = StatusCompensating
			after.Step = after.CompletedSteps - 1
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
		if record.CompletedSteps == 0 {
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
//  3. 同一生、记录在等这个操作：接收（同一生较早尝试的成功、拒绝、可重试失败都算，与收件箱一致）。
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
		}
		if !accept {
			return e.completeNotWaiting(ctx, record, completion, incarnation)
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

// completeNotWaiting 处理协调器不接收的 completion：已记录的按重复确认；放弃关闭之后才到的成功告警，
// 按（操作，completion 的代际）只告警一次（B1）；协调器既不在等、也没有记录的返回 ErrNotWaiting。
func (e *Engine) completeNotWaiting(ctx context.Context, record Record, completion Completion, incarnation uint32) (Record, error) {
	history, err := e.completionHistory(ctx, completion)
	if err != nil {
		return Record{}, err
	}
	if !history.Recorded {
		return Record{}, ErrNotWaiting
	}
	if completion.Success && !history.Receipt && history.Closure == OperationAbandoned {
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

// reportLateAfterAbandon 处理“协调器放弃之后才到的成功”（U-0280 C'）：步骤已经生效，但协调器在超时用尽
// 或 saga 截止时已经关闭了它，CompletedSteps 不含它、补偿也不会撤销它。这里只告警，不重开终态、不自动
// 补偿；运维按 TROUBLESHOOTING T-226 核对业务数据，Failed / ManualRequired 的 saga 可以 Resume——新一生
// 的同一步骤会回放这次成功而不是再执行一次。标签只有 saga 类型与方向，保持低基数。
func (e *Engine) reportLateAfterAbandon(record Record, completion Completion) {
	e.lateAfterAbandon.Add(1)
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.late_after_abandon_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Error("saga: step succeeded after the coordinator abandoned it; the effect is not compensated",
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
	return Stats{Started: e.started.Load(), Dispatched: e.dispatched.Load(), Completed: e.completed.Load(), Compensated: e.compensated.Load(), Failed: e.failed.Load(), ManualRequired: e.manualRequired.Load(), Conflicts: e.conflicts.Load(), Duplicates: e.duplicates.Load(), PublishFailures: e.publishFailures.Load(), StoreFailures: e.storeFailures.Load(), WorkerFailures: e.workerFailures.Load(), LateAfterAbandon: e.lateAfterAbandon.Load(), StaleIncarnation: e.staleIncarnation.Load()}
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
	command := Command{ID: after.CommandID, IdempotencyKey: after.OperationKey, SagaID: record.ID, SagaType: record.Type, DefinitionVersion: record.DefinitionVersion, BusinessKey: record.BusinessKey, Step: record.Step, StepName: step.Name, Phase: record.Phase, Attempt: after.Attempt, Topic: topic, Payload: append([]byte(nil), record.Data...), DeadlineAt: after.NextRunAt, CreatedAt: now}
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
		if result.Data != nil {
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
			after.CompletedSteps--
			if after.CompletedSteps <= 0 {
				after.CompletedSteps = 0
				after.Status = StatusCompensated
				after.NextRunAt = time.Time{}
			} else {
				after.Step = after.CompletedSteps - 1
				after.Status = StatusCompensating
				after.NextRunAt = now
			}
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
	if after.CompletedSteps == 0 {
		after.Status = StatusFailed
		after.NextRunAt = time.Time{}
		return after
	}
	after.Status = StatusCompensating
	after.Phase = PhaseCompensate
	after.Step = after.CompletedSteps - 1
	after.NextRunAt = now
	return after
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
