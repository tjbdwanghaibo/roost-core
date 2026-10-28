package saga

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
)

// Options 配置协调器：租约身份、协调 / 发布 worker 数与批量、租约与存储超时、轮询间隔、发布退避、载荷上限与步骤预算。
// 零值字段取 DefaultOptions 的值；跨参数的租约约束在 NewEngine 校验。
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

// DefaultOptions 返回默认配置（Owner 每次生成新的随机身份）。
func DefaultOptions() Options {
	return Options{
		Owner: "saga-" + NewID(), CoordinatorWorkers: 4, PublisherWorkers: 4,
		CoordinatorBatch: 3, PublisherBatch: 1, LeaseDuration: 15 * time.Second, StoreTimeout: 3 * time.Second, PollInterval: 100 * time.Millisecond,
		PublishTimeout: 3 * time.Second, PublishBackoffMin: 50 * time.Millisecond, PublishBackoffMax: 5 * time.Second,
		MaxPayloadBytes: 64 << 10, StepBudgets: StepBudgets{Defaults: DefaultStepBudget()},
	}
}

// StartRequest 是启动一个 saga 的意图；同一 (Type, BusinessKey) 只能有一份，重复启动按启动摘要判定幂等或冲突。
type StartRequest struct {
	ID                string    `json:"id,omitempty"`
	Type              string    `json:"type"`
	BusinessKey       string    `json:"business_key"`
	DefinitionVersion uint32    `json:"definition_version"`
	Data              []byte    `json:"data,omitempty"`
	DeadlineAt        time.Time `json:"deadline_at,omitempty"`
	Now               time.Time `json:"-"`
}

// ResumeRequest 是 Resume 的参数：可设置新的截止时间，或显式清除截止时间。
type ResumeRequest struct {
	ID            string
	Now           time.Time
	DeadlineAt    time.Time
	ClearDeadline bool
}

// Stats 是协调器的累计计数。
type Stats struct {
	Started, Dispatched, Completed, Compensated, Failed, ManualRequired   uint64
	Conflicts, Duplicates, PublishFailures, StoreFailures, WorkerFailures uint64
	// LateAfterAbandon 计协调器放弃一个步骤之后才到达的成功 completion：那一步已经生效，
	// 却不在 CompletedSteps 里。按（操作，代际）计一次（正向由回执去重，补偿方向由 LateSuccessAlarmStore）。
	// 正向的由协调器自动补偿（saga 方向 ④，记 WARN）；补偿方向的只告警（ERROR），需要运维核对（TROUBLESHOOTING T-226）。
	LateAfterAbandon uint64
	// StaleIncarnation 计被协调器拒收的旧一生结果：Resume 或补偿方向的人工 Compensate 进入新一生之后，
	// 上一生某次尝试的拒绝 / 失败才到达。收件箱也不回放旧一生的拒绝，新一生照常执行，这些结果只计数。
	StaleIncarnation uint64
	// StaleAttempt 计被协调器拒收的同一生较早尝试的可重试失败（saga 方向 ③）：协调器在等之后的一次尝试，较早那次的
	// 可重试失败只说明那一次没有生效，之后的尝试照常执行，它不能推进记录。只计数、记 WARN。
	StaleAttempt uint64
	// Reopened 计 saga 被重开的次数（saga 方向 ④ 的可观测性）：已结束的 Failed / Compensated 因为迟到的
	// 正向成功被带回补偿（reason=late_success），或 ManualRequired 期间记下的迟到步骤在运维 Resume / Compensate 时被补偿
	// （reason=resume / compensate）。指标 saga.reopened_total{saga_type,from_status,reason}，每次记 WARN。
	// 重开的 saga 补偿完会再次到达终态，Completed / Compensated / Failed 按到达终态的次数计，同一 saga 会再计一次。
	Reopened uint64
}

// Engine 是 saga 协调器：按定义推进记录（派发步骤、接收结果、超时重试、补偿），经 Store 持久化，经 Publisher 发出命令。
// 多个进程可以同时运行，记录由租约与版本 fence 保证同一时刻只有一个协调者推进。
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

// NewEngine 校验配置并构造协调器；Register 定义后调用 Run 开始推进。
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
	// 每个被拒的配置都点名出错的字段与违反的预算：这些跨参数的租约约束运维无法一眼看出，笼统的“限制不安全”会让每次
	// 配置错误都要翻源码。
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
	// EmitStart 在 Nest 事务里就按这个上限拒绝。
	registerStartDataLimit(definition.Type, e.opts.MaxPayloadBytes)
	return nil
}

// StartSaga 持久创建业务意图；相同身份重投返回当前进度，不重置已执行的步骤。
// 启动摘要与可变运行数据分离，旧记录不能证明原始身份时返回 ErrIdentityConflict。
func (e *Engine) StartSaga(ctx context.Context, request StartRequest) (Record, error) {
	if _, ok := e.definition(request.Type, request.DefinitionVersion); !ok {
		return Record{}, fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, request.Type, request.DefinitionVersion)
	}
	request, err := prepareStart(request, e.opts.MaxPayloadBytes)
	if err != nil {
		return Record{}, err
	}
	now := request.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	id := request.ID
	if id == "" {
		id = NewID()
	}
	record, err := newStartRecord(request, id, now)
	if err != nil {
		return Record{}, err
	}
	intentDigest := record.StartDigest
	hoptrace.Mark("timeline/start.begin/", record.ID)
	createStart := hoptrace.Now()
	err = e.store.Create(ctx, record)
	hoptrace.Since("h00.start.create", createStart)
	if err != nil {
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
	hoptrace.Mark("timeline/start.end/", record.ID)
	e.started.Add(1)
	e.signal(e.dueKick)
	return record.Clone(), nil
}

// Get 读取一条 saga 记录。
func (e *Engine) Get(ctx context.Context, id string) (Record, error) { return e.store.Get(ctx, id) }

// GetByBusinessKey 按 (类型, 业务键) 读取记录：同一对只能有一份 saga（StartSaga 的唯一性），所以发起方用自己给的
// 业务键就能查到结果，不需要知道 saga ID（经 EmitStart 的事务性 outbox 启动时 ID 由启动消费者分配）。还没启动
// （启动意图仍在 outbox / 流里）时返回 ErrNotFound，调用方按“尚未开始”处理而不是失败。
func (e *Engine) GetByBusinessKey(ctx context.Context, sagaType, businessKey string) (Record, error) {
	return e.store.GetByBusinessKey(ctx, strings.TrimSpace(sagaType), strings.TrimSpace(businessKey))
}

// List 按查询列出记录，Limit 缺省 100、上限 1000。
func (e *Engine) List(ctx context.Context, query Query) ([]Record, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > 1000 {
		return nil, ErrInvalidRecord
	}
	return e.store.List(ctx, query)
}

// Resume 在运维或自动修复消除原因之后重试一个失败终态。Completed 与 Compensated 的 saga 不能 Resume。
func (e *Engine) Resume(ctx context.Context, request ResumeRequest) (Record, error) {
	request, deadline, err := prepareResume(request, time.Now())
	if err != nil {
		return Record{}, err
	}
	for attempts := 0; attempts < 8; attempts++ {
		record, err := e.store.Get(ctx, request.ID)
		if err != nil {
			return Record{}, err
		}
		_, defined := e.definition(record.Type, record.DefinitionVersion)
		after, err := planResume(record, defined, request, deadline)
		if err != nil {
			return Record{}, err
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

// Compensate 请求对每个已完成步骤做语义回滚。可重复调用；已在补偿或已补偿的记录原样返回。Completed 的 saga 返回
// ErrSagaCompleted。
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
		// Completed 是业务结论，按终态做业务的一方已据此行动；撤销已完成的业务走业务自己的冲正流程，
		// 不能带回 Compensating 逐步撤销（维护者 2026-10-07 选 A）。规则全在 planCompensate。
		after, write, err := planCompensate(record, reason, now)
		if !write {
			return after, err
		}
		// 当前步骤正在重试退避时人工补偿放弃了它；补偿方向停下的记录进入新一生（B1）。两者都由 planTransition 决定。
		written, _, err := e.stepTransition(ctx, record, after, transition{cause: causeManualCompensate})
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

// Complete 接收一次尝试的结果。接收与否只由 judgeCompletion 判定（统一规则，见 docs/framework/SAGA.md）：成功是操作的结论，只要这个操作还没有带结果关闭就接收；
// 拒绝是本生这个操作的结论、可重试失败是一次尝试的结论，二者只在协调器正等着它们时接收。
func (e *Engine) Complete(ctx context.Context, completion Completion) (Record, error) {
	if completion.Validate() != nil {
		return Record{}, ErrInvalidRecord
	}
	if len(completion.Data) > e.opts.MaxPayloadBytes {
		return Record{}, ErrInvalidRecord
	}
	// 排序、截止与回执 TTL 都以协调器时间为准；远端步骤的时钟不能让 saga 状态倒退，也不能让去重记录过期。
	completion.CompletedAt = time.Now().UTC()
	incarnation := commandIDIncarnation(completion.IdempotencyKey, completion.CommandID)
	for attempts := 0; attempts < 8; attempts++ {
		// 这次读不是可省的预读：判定（judgeCompletion）与目标状态（applyCompletion）都从它算出，写入按它的版本做条件
		// 替换（乐观并发，冲突即重读重试）。Store.Apply 的事务不再读记录，只核对版本，所以它是唯一的那次读。
		hoptrace.Mark("timeline/complete.begin/", completion.CommandID)
		getStart := hoptrace.Now()
		record, err := e.store.Get(ctx, completion.SagaID)
		hoptrace.Since("h11.complete.get", getStart)
		if err != nil {
			return Record{}, err
		}
		switch judgeCompletion(record, completion, incarnation) {
		case verdictStaleIncarnation:
			e.reportStaleIncarnation(record, completion, incarnation)
			return record, nil
		case verdictStaleAttempt:
			e.reportStaleAttempt(record, completion)
			return record, nil
		case verdictHistory:
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
		applyStart := hoptrace.Now()
		_, outcome, err := e.stepTransition(ctx, record, after, transition{cause: causeResult, receipt: &completion})
		hoptrace.Since("h11.complete.apply", applyStart)
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
		hoptrace.Mark("timeline/complete.end/", completion.CommandID)
		e.countTerminal(after.Status)
		e.signal(e.dueKick)
		return after.Clone(), nil
	}
	return Record{}, ErrConflict
}

// completeNotWaiting 处理协调器不接收的 completion：已记录的按重复确认；放弃关闭之后才到的正向成功交给
// compensateLateStep 补偿那一步（saga 方向 ④），补偿方向的只告警，按（操作，completion 的代际）只告警一次（B1）；
// 协调器既不在等、也没有记录的返回 ErrNotWaiting。写记录冲突时返回 ErrConflict，由 Complete 重读重试。
func (e *Engine) completeNotWaiting(ctx context.Context, record Record, completion Completion, incarnation uint32) (Record, error) {
	history, err := e.completionHistory(ctx, completion)
	if err != nil {
		return Record{}, err
	}
	verdict, step := judgeHistory(record, completion, history)
	switch verdict {
	case historyNotWaiting:
		return Record{}, ErrNotWaiting
	case historyLateStep:
		return e.compensateLateStep(ctx, record, completion, step)
	case historyLateAlarm:
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

// compensateLateStep 是 saga 方向 ④：第 step 步在协调器放弃之后生效了，把它补偿掉。一个 Store 事务里记下这份 completion 的回执、
// 把 tombstone 改为带结果关闭（planTransition 收到成功回执时的既有行为；之后的送达按回执去重），并在记录上写 LateStep / LateData
// （目标状态见 lateStepState）。
func (e *Engine) compensateLateStep(ctx context.Context, record Record, completion Completion, step int) (Record, error) {
	after := lateStepState(record, completion, step)
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
	reportReopen(before, after, reason)
}

// reportReopen 是重开的指标与日志（两种协调器共用；计数由调用方加）。
func reportReopen(before, after Record, reason string) {
	from := before.Status.String()
	metrics.IncCounter("saga.reopened_total", metrics.Labels{"saga_type": before.Type, "from_status": from, "reason": reason}, 1)
	slog.Warn("saga: reopened to compensate a step that took effect after the coordinator abandoned it",
		"saga_id", before.ID, "saga_type", before.Type, "from_status", from, "reason", reason,
		"late_step", after.LateStep-1, "status", after.Status.String(), "version", after.Version)
}

// reportLateCompensation 记一次由协调器自动补偿的迟到成功（saga 方向 ④）：仍计 late_after_abandon（phase=forward），
// 但已经有了归宿，只记 WARN。
func (e *Engine) reportLateCompensation(record Record, completion Completion) {
	e.lateAfterAbandon.Add(1)
	reportLateCompensation(record, completion)
}

func reportLateCompensation(record Record, completion Completion) {
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.late_after_abandon_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Warn("saga: step succeeded after the coordinator abandoned it; compensating it",
		"saga_id", completion.SagaID, "saga_type", record.Type, "status", record.Status.String(), "step", step,
		"command_id", completion.CommandID, "operation", completion.IdempotencyKey)
}

// reportStaleAttempt 记一份被拒收的同一生较早尝试的可重试失败（saga 方向 ③）：协调器在等之后的那次尝试。
func (e *Engine) reportStaleAttempt(record Record, completion Completion) {
	e.staleAttempt.Add(1)
	reportStaleAttempt(record, completion)
}

func reportStaleAttempt(record Record, completion Completion) {
	phase, step := operationPosition(completion.IdempotencyKey)
	metrics.IncCounter("saga.completion.stale_attempt_total", metrics.Labels{"saga_type": record.Type, "phase": phase}, 1)
	slog.Warn("saga: ignored a retryable failure from an earlier attempt; waiting for the current attempt",
		"saga_id", completion.SagaID, "saga_type", record.Type, "phase", phase, "step", step,
		"command_id", completion.CommandID, "waiting_for", record.CommandID, "error", completion.Error)
}

// reportStaleIncarnation 记一份被拒收的旧一生结果（B1）。这是 Resume 之后的正常现象，不是故障：只计数、记 WARN。
func (e *Engine) reportStaleIncarnation(record Record, completion Completion, incarnation uint32) {
	e.staleIncarnation.Add(1)
	reportStaleIncarnation(record, completion, incarnation)
}

func reportStaleIncarnation(record Record, completion Completion, incarnation uint32) {
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

// reportLateAfterAbandon 告警一份协调器不能自己处理的“放弃之后才到的成功”：补偿方向的操作（正向的由
// compensateLateStep 补偿，saga 方向 ④），以及 lateForwardStep 判为不应发生的情形。运维按 TROUBLESHOOTING T-226 核对；
// 补偿方向停在 ManualRequired 时 Resume，新一生的同一补偿会回放这次成功而不是再执行一次。标签只有 saga 类型与方向，保持低基数。
func (e *Engine) reportLateAfterAbandon(record Record, completion Completion) {
	e.lateAfterAbandon.Add(1)
	reportLateAfterAbandon(record, completion)
}

func reportLateAfterAbandon(record Record, completion Completion) {
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

// Run 启动协调与发布 worker，阻塞到 ctx 取消或 Stop，并等 worker 全部退出。同一时刻只能运行一次。
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
	for range e.opts.CoordinatorWorkers {
		wg.Add(1)
		go func() { defer wg.Done(); e.coordinatorLoop(ctx) }()
	}
	for range e.opts.PublisherWorkers {
		wg.Add(1)
		go func() { defer wg.Done(); e.publisherLoop(ctx) }()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// Stop 取消 Run 并在 ctx 内等它退出；超时返回 ctx 错误，用新 ctx 重试会再次等待。未运行时返回 nil。
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

// Stats 返回累计计数。
func (e *Engine) Stats() Stats {
	return Stats{Started: e.started.Load(), Dispatched: e.dispatched.Load(), Completed: e.completed.Load(), Compensated: e.compensated.Load(), Failed: e.failed.Load(), ManualRequired: e.manualRequired.Load(), Conflicts: e.conflicts.Load(), Duplicates: e.duplicates.Load(), PublishFailures: e.publishFailures.Load(), StoreFailures: e.storeFailures.Load(), WorkerFailures: e.workerFailures.Load(), LateAfterAbandon: e.lateAfterAbandon.Load(), StaleIncarnation: e.staleIncarnation.Load(), StaleAttempt: e.staleAttempt.Load(), Reopened: e.reopened.Load()}
}

func (e *Engine) coordinatorLoop(ctx context.Context) {
	for ctx.Err() == nil {
		now := time.Now().UTC()
		storeCtx, cancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
		claimStart := hoptrace.Now()
		records, err := e.store.ClaimDue(storeCtx, ClaimRequest{Owner: e.opts.Owner, Now: now, LeaseDuration: e.opts.LeaseDuration, Limit: e.opts.CoordinatorBatch})
		cancel()
		if len(records) > 0 {
			hoptrace.Since("h01.coord.claim", claimStart)
		}
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
			// 逐跳计时：派发事务提交 → 发布器开始发布（outbox 排队与领取），发布本身（同步等 PubAck）。
			hoptrace.SinceMark("h03.outbox.wait", "dispatched/", items[i].Command.ID)
			hoptrace.Mark("timeline/publish.begin/", items[i].Command.ID)
			publishStart := hoptrace.Now()
			publishCtx, cancel := context.WithTimeout(ctx, e.opts.PublishTimeout)
			err := e.publisher.PublishSagaCommand(publishCtx, items[i].Command.Clone())
			cancel()
			hoptrace.Since("h03.outbox.publish", publishStart)
			hoptrace.Mark("timeline/publish.end/", items[i].Command.ID)
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
			hoptrace.Mark("published/", items[i].Command.ID)
			ackStart := hoptrace.Now()
			storeCtx, storeCancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
			ackErr := e.store.AckOutbox(storeCtx, items[i].Command.ID, items[i].Lease)
			storeCancel()
			hoptrace.Since("h03.outbox.ack", ackStart)
			if errors.Is(ackErr, ErrConflict) {
				e.conflicts.Add(1)
			} else if ackErr != nil && !errors.Is(ackErr, context.Canceled) {
				e.storeFailures.Add(1)
				slog.Error("saga: ack outbox", "id", items[i].Command.ID, "err", ackErr)
			}
		}
	}
}

// processClaimed 处理一条已领取（持有租约）的到期记录：判定全在 planDue（与 JetStream 协调器共用），这里只负责写入
// （带租约 fence）、计数与唤醒。
func (e *Engine) processClaimed(ctx context.Context, record Record, now time.Time) error {
	definition, defined := e.definition(record.Type, record.DefinitionVersion)
	plan := planDue(record, definition, defined, now)
	t := transition{cause: plan.cause, fenced: true}
	if plan.cause != causeDispatch {
		// 定义缺失时在等结果或正在重试退避的操作都被放弃（planTransition 关闭它）。
		_, _, err := e.stepTransition(ctx, record, plan.after, t)
		if err != nil {
			return err
		}
		switch plan.cause {
		case causeDeadline, causeTimeout:
			e.countTerminal(plan.after.Status)
			e.signal(e.dueKick)
		case causeInvalidStep:
			e.failed.Add(1)
			e.manualRequired.Add(1)
		default:
			e.countTerminal(plan.after.Status)
		}
		return nil
	}
	command := *plan.command
	t.outbox = &OutboxRecord{Command: command, NextAttemptAt: now, CreatedAt: now}
	if !record.NextRunAt.IsZero() && hoptrace.Enabled() {
		// 逐跳计时：记录到期（启动、上一步结果接收、重试退避结束）→ 协调器领取并开始派发。
		hoptrace.Observe("h01.coord.wake", time.Since(record.NextRunAt))
	}
	// 只在诊断时关联交易与命令；不把高基数身份写入生产指标。
	if hoptrace.Enabled() {
		hoptrace.Mark("timeline/link/"+record.ID+"/", command.ID)
	}
	hoptrace.Mark("timeline/dispatch.begin/", command.ID)
	dispatchStart := hoptrace.Now()
	_, _, err := e.stepTransition(ctx, record, plan.after, t)
	hoptrace.Since("h02.dispatch.apply", dispatchStart)
	if err == nil {
		hoptrace.Mark("timeline/dispatch.end/", command.ID)
		hoptrace.Mark("dispatched/", command.ID)
		e.dispatched.Add(1)
		e.signal(e.outboxKick)
	}
	return err
}

// applyCompletion / retryOrCompensate 转到 transition.go 的同名纯函数；方法形式留给现有测试调用。
func (e *Engine) applyCompletion(record Record, definition Definition, result Completion) Record {
	return applyCompletion(record, definition, result)
}

func (e *Engine) retryOrCompensate(record Record, definition Definition, reason string, now time.Time) Record {
	return retryOrCompensate(record, definition, reason, now)
}

// stepTransition 把 before → after 写成一次记录转移，是 Redis 引擎写记录（Store.Apply）的唯一入口：代际与关闭由
// planTransition 决定，ApplyRequest 只在这里构造、只在这里交给 Store，调用方拿到的是写入的记录，拿不到可改写的请求
// （例如把 CloseOperation 清空后再写入）。fenced 时带 before 的租约。
func (e *Engine) stepTransition(ctx context.Context, before, after Record, t transition) (Record, ApplyOutcome, error) {
	after, closeOperation := planTransition(before, after, t)
	request := ApplyRequest{ExpectedVersion: before.Version, After: after, Outbox: t.outbox, Receipt: t.receipt, CloseOperation: closeOperation}
	if t.fenced {
		request.ExpectedLease = before.Lease
	}
	outcome, err := e.store.Apply(ctx, request)
	return request.After, outcome, err
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
