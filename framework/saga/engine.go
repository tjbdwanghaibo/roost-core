package saga

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// engine.go 是协调器的外观（设计 docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md）：协调器的权威是
// JetStream 分区事件日志，状态在分区所有者的内存里，快照是 KV 清单 + 对象存储正文。参与方（原生 / Mongo 步骤）把结果直接发回
// 命令所属分区（step_consumer.go）。
//
// 角色：Options.Owned 列出本进程拥有的分区（由 saga.coordinators 静态分配，选项 P-A；进程级唯一由 App 单实例锁保证，崩溃重启的
// 短暂重叠由期望序号隔离、旧进程 fail-stop）。拥有分区的进程跑分区循环、回答查询、做准入；不拥有任何分区的进程只有发起方一侧
// （StartSaga、查询、WaitTerminal）。

// Options 配置协调器。零值字段取缺省（见 withDefaults）；跨字段规则在 NewEngine 校验。
type Options struct {
	Layout Layout
	// Owned 是本进程拥有的分区（0 ≤ k < Partitions，不重复）；空表示只做发起方。
	Owned []int
	// Writer 是本进程作为写者的身份（<sid>-<SingletonIncarnation 或随机串>），进事件的 MsgID 与 Writer 字段；空时随机生成。
	Writer string
	// Replicas 是流与桶的副本数（生产 3，由配置层强制）。缺省 1。
	Replicas int
	// StreamMaxBytes 是每条流的容量上限（满了拒新写，协调器 fail-stop 告警）。缺省 8 GiB。
	StreamMaxBytes int64
	// DuplicateWindow 是流的 MsgID 去重窗口，必须 ≥ 2 × PublishUnknownBudget。缺省 2m。
	DuplicateWindow time.Duration
	// MaxBatch 是一轮最多取的输入条数（1～1000）。缺省 256。
	MaxBatch int
	// Tick / WheelSlots 是分区时间轮的槽宽（1ms～100ms）与槽数。缺省 5ms / 1024。
	Tick       time.Duration
	WheelSlots int
	// SnapshotEveryEvents / SnapshotInterval：每这么多条事件，或到时间且有新事件，做一次快照 + 清理。缺省 2000 / 30s。
	SnapshotEveryEvents int
	SnapshotInterval    time.Duration
	// TerminalRetention：终态（Completed / Compensated / Failed）在内存与快照里留多久（业务键唯一性窗口，选项 W-A）。缺省 1h。
	TerminalRetention time.Duration
	// ReceiptRetention：参与方回执保留期（RR-A 守卫用），必须 ≥ TerminalRetention。缺省 168h。
	ReceiptRetention time.Duration
	// PublishTimeout：一次发布等 PubAck 的时长，必须 < PublishUnknownBudget 且 < 10s（服务端原子批超时）。缺省 3s。
	PublishTimeout time.Duration
	// PublishUnknownBudget：分区事件发布结果未知时重发判定的总时长，超过 fail-stop。缺省 30s（配置层另要求 ≥ 3 × 7s 选主停顿）。
	PublishUnknownBudget time.Duration
	// QueryTimeout / StartVerdictTimeout：查询所有者的超时；等判定多久后改为按 Get 补查。缺省 2s / 2s。
	QueryTimeout        time.Duration
	StartVerdictTimeout time.Duration
	// PurgeStallAlarm：某个 durable 卡住清理多久告警。缺省 10m。
	PurgeStallAlarm time.Duration
	// MaxPayloadBytes 是启动 Data 与结果 Data 的上限。缺省 64 KiB，最大 4 MiB。
	MaxPayloadBytes int
	// StepBudgets 在 Register 时补齐步骤预算（定义里没写的字段取配置，按步骤的覆盖优先）。
	StepBudgets StepBudgets
	// Admission 是按参与方投影积压的新启动准入（设计 §6）。
	Admission AdmissionOptions
	// ColdCopy 是可选的冷副本（设计 §8，缺省关闭）：非 nil 时 Get / GetByBusinessKey / List 在分区所有者内存里没有这条记录时
	// 回落读它（只读，不参与唯一性与任何判定），拥有分区的进程清理时把它的 durable 当守卫（找不到就不清理）。
	ColdCopy *ColdCopy
	// Failure 收分区 fail-stop（被隔离、发布结果未知超预算、流满等），一般是 App 的 RuntimeFailure；nil 时只停这个分区。
	Failure *app.RuntimeFailure
	// Clock 是协调器时钟（测试注入）；nil 用 time.Now。准入的过期判定也用它（time.Now 带单调读数）。
	Clock func() time.Time
}

// DefaultOptions 返回缺省选项（Layout 只填前缀与流名，分区 16、流 4）。
func DefaultOptions() Options {
	return Options{
		Layout:   Layout{Prefix: "roost.saga", Stream: "ROOST_SAGA", Partitions: 16, Groups: 4},
		Replicas: 1, StreamMaxBytes: 8 << 30, DuplicateWindow: 2 * time.Minute, MaxBatch: 256, Tick: 5 * time.Millisecond, WheelSlots: 1024,
		SnapshotEveryEvents: 2000, SnapshotInterval: 30 * time.Second, TerminalRetention: time.Hour, ReceiptRetention: 168 * time.Hour,
		PublishTimeout: 3 * time.Second, PublishUnknownBudget: 30 * time.Second, QueryTimeout: 2 * time.Second, StartVerdictTimeout: 2 * time.Second,
		PurgeStallAlarm: 10 * time.Minute, MaxPayloadBytes: 64 << 10, StepBudgets: StepBudgets{Defaults: DefaultStepBudget()},
		Admission: AdmissionOptions{Enabled: true},
	}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.Replicas <= 0 {
		o.Replicas = d.Replicas
	}
	if o.StreamMaxBytes <= 0 {
		o.StreamMaxBytes = d.StreamMaxBytes
	}
	if o.DuplicateWindow <= 0 {
		o.DuplicateWindow = d.DuplicateWindow
	}
	if o.MaxBatch <= 0 {
		o.MaxBatch = d.MaxBatch
	}
	if o.Tick <= 0 {
		o.Tick = d.Tick
	}
	if o.WheelSlots <= 0 {
		o.WheelSlots = d.WheelSlots
	}
	if o.SnapshotEveryEvents <= 0 {
		o.SnapshotEveryEvents = d.SnapshotEveryEvents
	}
	if o.SnapshotInterval <= 0 {
		o.SnapshotInterval = d.SnapshotInterval
	}
	if o.TerminalRetention <= 0 {
		o.TerminalRetention = d.TerminalRetention
	}
	if o.ReceiptRetention <= 0 {
		o.ReceiptRetention = d.ReceiptRetention
	}
	if o.PublishTimeout <= 0 {
		o.PublishTimeout = d.PublishTimeout
	}
	if o.PublishUnknownBudget <= 0 {
		o.PublishUnknownBudget = d.PublishUnknownBudget
	}
	if o.QueryTimeout <= 0 {
		o.QueryTimeout = d.QueryTimeout
	}
	if o.StartVerdictTimeout <= 0 {
		o.StartVerdictTimeout = d.StartVerdictTimeout
	}
	if o.PurgeStallAlarm <= 0 {
		o.PurgeStallAlarm = d.PurgeStallAlarm
	}
	if o.MaxPayloadBytes <= 0 {
		o.MaxPayloadBytes = d.MaxPayloadBytes
	}
	o.Admission = o.Admission.withDefaults()
	return o
}

// validate 是选项的跨字段规则（每条都点名字段与违反的约束）。分区数 / 流数的“每条流 ≤ 50 个分区”在 Layout.Validate 里
// （wiring/saga 的配置检查调用同一个函数）。
func (o Options) validate() error {
	if err := o.Layout.Validate(); err != nil {
		return err
	}
	seen := map[int]bool{}
	for _, k := range o.Owned {
		if k < 0 || k >= o.Layout.Partitions || seen[k] {
			return fmt.Errorf("%w: owned partition %d (partitions %d, listed twice: %v)", ErrInvalidDefinition, k, o.Layout.Partitions, seen[k])
		}
		seen[k] = true
	}
	if strings.ContainsAny(o.Writer, ".*> \t\r\n") || len(o.Writer) > 128 {
		return fmt.Errorf("%w: writer %q must be a subject token of at most 128 bytes", ErrInvalidDefinition, o.Writer)
	}
	switch {
	case o.MaxBatch > 1000:
		return fmt.Errorf("%w: max batch %d must be <= 1000", ErrInvalidDefinition, o.MaxBatch)
	case o.Tick < time.Millisecond || o.Tick > 100*time.Millisecond:
		return fmt.Errorf("%w: tick %v must be in [1ms, 100ms]", ErrInvalidDefinition, o.Tick)
	case o.PublishTimeout >= o.PublishUnknownBudget:
		return fmt.Errorf("%w: publish timeout %v must be < publish unknown budget %v", ErrInvalidDefinition, o.PublishTimeout, o.PublishUnknownBudget)
	case o.PublishTimeout >= 10*time.Second:
		return fmt.Errorf("%w: publish timeout %v must be < 10s (the server drops an atomic batch not committed within 10s)", ErrInvalidDefinition, o.PublishTimeout)
	case o.DuplicateWindow < 2*o.PublishUnknownBudget:
		return fmt.Errorf("%w: duplicate window %v must be >= 2 x publish unknown budget %v", ErrInvalidDefinition, o.DuplicateWindow, o.PublishUnknownBudget)
	case o.TerminalRetention > o.ReceiptRetention:
		return fmt.Errorf("%w: terminal retention %v must be <= receipt retention %v", ErrInvalidDefinition, o.TerminalRetention, o.ReceiptRetention)
	case o.MaxPayloadBytes > 4<<20:
		return fmt.Errorf("%w: max payload %d exceeds %d", ErrInvalidDefinition, o.MaxPayloadBytes, 4<<20)
	}
	if err := o.StepBudgets.Validate(); err != nil {
		return fmt.Errorf("saga: StepBudgets: %w", err)
	}
	return o.Admission.validate()
}

// Stats 是 JetStream 协调器的累计计数。
type Stats struct {
	Started, Dispatched, Completed, Compensated, Failed, ManualRequired    uint64
	Duplicates, LateAfterAbandon, StaleIncarnation, StaleAttempt, Reopened uint64
	// RejectedInputs：解码 / 校验不过而跳过的输入（saga.partition.rejected_input_total）。
	RejectedInputs uint64
	// AdmissionRejected：准入关闭时被拒的启动（只回复那次尝试，不写事件）。
	AdmissionRejected uint64
	// UnknownSaga：结果对应的 saga 不在分区里（过了终态保留期被剪掉）。
	UnknownSaga uint64
	// Events：已提交的事件数；PublishUnknown / PublishResolved：结果未知的发布次数与按 Duplicate / 最后一条判定为已提交的次数；
	// BatchIncomplete：10176 立即重发次数；PublishRetried：确定没写入的其他错误重发次数。
	Events, PublishUnknown, PublishResolved, BatchIncomplete, PublishRetried uint64
	Snapshots, SnapshotConflicts, Purges, Pruned, Replayed, FailStops        uint64
}

type engineCounters struct {
	started, dispatched, completed, compensated, failed, manualRequired      atomic.Uint64
	duplicates, lateAfterAbandon, staleIncarnation, staleAttempt, reopened   atomic.Uint64
	rejectedInputs, admissionRejected, unknownSaga                           atomic.Uint64
	events, publishUnknown, publishResolved, batchIncomplete, publishRetried atomic.Uint64
	snapshots, snapshotConflicts, purges, pruned, replayed, failStops        atomic.Uint64
}

func (c *engineCounters) add(d partitionCounts) {
	c.started.Add(d.started)
	c.dispatched.Add(d.dispatched)
	c.completed.Add(d.completed)
	c.compensated.Add(d.compensated)
	c.failed.Add(d.failed)
	c.manualRequired.Add(d.manualRequired)
	c.duplicates.Add(d.duplicates)
	c.lateAfterAbandon.Add(d.lateAfterAbandon)
	c.staleIncarnation.Add(d.staleIncarnation)
	c.staleAttempt.Add(d.staleAttempt)
	c.reopened.Add(d.reopened)
	c.rejectedInputs.Add(d.rejectedInputs)
	c.admissionRejected.Add(d.admissionRejected)
	c.unknownSaga.Add(d.unknownSaga)
}

// Engine 是 saga 协调器：拥有分区的进程跑分区循环；全部进程都有发起方 API。
type Engine struct {
	log    fnats.JetStreamLog
	raw    fnats.RawClient
	opts   Options
	layout Layout
	now    func() time.Time
	writer string
	// instance 是本实例的随机标识（crypto/rand.Text，128 位），启动 / 运维尝试标识的前缀；replyPrefix 是判定回复的 subject 前缀。
	instance    string
	replyPrefix string
	attemptSeq  atomic.Uint64

	mu          sync.RWMutex
	definitions map[definitionKey]Definition

	stats   engineCounters
	watch   *terminalWatch
	replies replyRouter
	hints   admissionHints
	gate    *admission
	hooks   engineHooks

	snapshots fnats.LogKV
	objects   fnats.LogObjects

	// ready 在 Start 成功后为真、Stop 开始时为假：发起方 API（要收判定回复）只在这期间可用。
	ready         atomic.Bool
	lifeMu        sync.Mutex
	started       bool
	stopped       bool
	partitions    []*partition
	follows       []fnats.LogFollow
	subscriptions []fnats.DrainSubscription
	stopAdmission func(context.Context) error
	failed        chan struct{}
	failOnce      sync.Once
	failCause     error
}

// engineHooks 只给本包测试用：发布之前回调（冻结旧写者、注入故障）；分区跳过一条输入时回调（单元按它还原“结果被拒的原因”）。
// 生产代码不设置。
type engineHooks struct {
	beforePublish func(k int)
	rejectedInput func(kind, reason string, err error)
}

// NewEngine 校验选项并构造协调器；Register 定义后调用 Start。log 与 raw 来自同一条 NATS 连接
// （driver.Assembly 的 Log 与 Client）。
func NewEngine(log fnats.JetStreamLog, raw fnats.RawClient, options Options) (*Engine, error) {
	if log == nil || raw == nil {
		return nil, fmt.Errorf("saga: JetStream log and raw NATS client are required")
	}
	options = options.withDefaults()
	if err := options.validate(); err != nil {
		return nil, err
	}
	instance := rand.Text()
	writer := options.Writer
	if writer == "" {
		writer = "saga-" + instance
	}
	now := options.Clock
	if now == nil {
		now = time.Now
	}
	e := &Engine{
		log: log, raw: raw, opts: options, layout: options.Layout, now: now, writer: writer,
		instance: instance, replyPrefix: "_INBOX.saga." + instance,
		definitions: map[definitionKey]Definition{}, watch: newTerminalWatch(),
		replies: replyRouter{waiting: map[string]chan verdictReply{}}, hints: admissionHints{freshness: options.Admission.HintFreshness},
		failed: make(chan struct{}),
	}
	if options.Admission.Enabled && len(options.Owned) > 0 {
		e.gate = newAdmission(options.Admission, now)
	}
	return e, nil
}

// Register 与 Engine.Register 相同：按 StepBudgets 补齐步骤预算再校验，同一类型同一版本只能登记一次。所有者处理启动意图与
// 到期时用这里的定义；发起方本地先查一次（没有定义直接 ErrDefinitionMissing，不写意图）。
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
	definition.Steps = slices.Clone(definition.Steps)
	e.definitions[key] = definition
	registerStartDataLimit(definition.Type, e.opts.MaxPayloadBytes)
	return nil
}

func (e *Engine) definition(name string, version uint32) (Definition, bool) {
	e.mu.RLock()
	d, ok := e.definitions[definitionKey{typeName: name, version: version}]
	e.mu.RUnlock()
	return d, ok
}

// admit 是所有者处理启动意图时的权威准入判定；gated=false 表示没开准入。
func (e *Engine) admit() (AdmissionState, bool) {
	if e.gate == nil {
		return AdmissionState{Open: true}, false
	}
	return e.gate.evaluate(), true
}

// AdmissionState 是本进程看到的准入状态：拥有分区的进程给出自己的判定；只做发起方的进程给出最近收到的提示（没有新鲜提示时
// 视为打开）。健康检查与测试用。
func (e *Engine) AdmissionState() AdmissionState {
	if e.gate != nil {
		return e.gate.evaluate()
	}
	if closed, reason := e.hints.closed(); closed {
		return AdmissionState{Open: false, Reason: reason, At: e.now()}
	}
	return AdmissionState{Open: true, At: e.now()}
}

// receiptsExpired 是选项 RR-A 的守卫：记录上次更新早于 now − ReceiptRetention + 最长步骤窗口时，重新执行的操作可能已经没有
// 参与方回执。
func (e *Engine) receiptsExpired(record Record, definition Definition, now time.Time) bool {
	var window time.Duration
	for _, step := range definition.Steps {
		attempts := time.Duration(max(step.MaxAttempts, 1))
		window = max(window, step.Timeout*attempts+step.BackoffMax*(attempts-1))
	}
	return record.UpdatedAt.Before(now.Add(-e.opts.ReceiptRetention + window))
}

func (e *Engine) reportFailure(err error) {
	e.failOnce.Do(func() {
		e.failCause = err
		close(e.failed)
	})
	if e.opts.Failure != nil {
		e.opts.Failure.Fail(err)
	}
}

// Failed 在任一分区 fail-stop 后关闭。
func (e *Engine) Failed() <-chan struct{} { return e.failed }

// FailureCause 返回第一个 fail-stop 的原因；还没有 fail-stop 时返回 nil。
func (e *Engine) FailureCause() error {
	select {
	case <-e.failed:
		return e.failCause // 写在 close(e.failed) 之前，读在它之后
	default:
		return nil
	}
}

// ---- 生命周期 ----

// ensureInfrastructure 建流与桶、登记 / 核对布局。全部进程都做（发起方也要往流里追加输入、watch 准入提示）。
func (e *Engine) ensureInfrastructure(ctx context.Context) error {
	for g, name := range e.layout.Streams() {
		if _, err := e.log.EnsureLogStream(ctx, fnats.LogStreamConfig{
			Name: name, Subjects: e.layout.StreamSubjects(g), Replicas: e.opts.Replicas,
			MaxBytes: e.opts.StreamMaxBytes, Duplicates: e.opts.DuplicateWindow,
		}); err != nil {
			return fmt.Errorf("saga: ensure stream %s: %w", name, err)
		}
	}
	snapshots, err := e.log.KeyValue(ctx, fnats.LogKVConfig{Bucket: e.layout.SnapshotBucket(), Replicas: e.opts.Replicas, History: 1})
	if err != nil {
		return err
	}
	objects, err := e.log.Objects(ctx, fnats.LogObjectConfig{Bucket: e.layout.ObjectBucket(), Replicas: e.opts.Replicas})
	if err != nil {
		return err
	}
	e.snapshots, e.objects = snapshots, objects
	want, _ := json.Marshal(layoutRecord{V: WireVersion, Layout: layoutOf(e.layout)})
	if _, err := snapshots.Create(ctx, layoutKey, want); err != nil {
		if !errors.Is(err, fnats.ErrLogKVConflict) {
			return fmt.Errorf("saga: register layout: %w", err)
		}
		entry, err := snapshots.Get(ctx, layoutKey)
		if err != nil {
			return fmt.Errorf("saga: read registered layout: %w", err)
		}
		var got layoutRecord
		if json.Unmarshal(entry.Value, &got) != nil || got.V != WireVersion || got.Layout != layoutOf(e.layout) {
			return fmt.Errorf("%w: registered %s, this process %+v", ErrLayoutMismatch, entry.Value, layoutOf(e.layout))
		}
	}
	return nil
}

// Start 建基础设施，重放并启动拥有的分区（先恢复再服务：返回 nil 时全部拥有的分区都已重放完、开始处理输入），订阅查询与
// 判定回复，启动准入与终态观察。失败时已启动的部分全部停掉。只能调用一次。
func (e *Engine) Start(ctx context.Context) (err error) {
	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()
	if e.started {
		return fmt.Errorf("saga: JetStream engine already started")
	}
	e.started = true
	defer func() {
		if err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = e.stopLocked(stopCtx)
		}
	}()
	if err := e.ensureInfrastructure(ctx); err != nil {
		return err
	}
	loads, err := e.log.KeyValue(ctx, loadBucketConfig(e.layout, e.opts.Replicas, e.opts.Admission))
	if err != nil {
		return fmt.Errorf("saga: admission bucket: %w", err)
	}
	hintFollow, err := loads.Watch(ctx, admitHintKey, e.hints.observe, func() {})
	if err != nil {
		return fmt.Errorf("saga: watch admission hints: %w", err)
	}
	e.follows = append(e.follows, hintFollow)
	if e.gate != nil {
		stop, err := e.gate.run(loads)
		if err != nil {
			return err
		}
		e.stopAdmission = stop
	}
	reply, err := e.raw.SubscribeBounded(e.replyPrefix+".>", fnats.PendingLimits{Messages: 8192, Bytes: 64 << 20, OnError: func(err error) {
		slog.Error("saga: verdict reply subscription error", "err", err)
	}}, func(msg *fnats.Msg) { e.replies.deliver(msg.Data) })
	if err != nil {
		return fmt.Errorf("saga: subscribe verdict replies: %w", err)
	}
	e.subscriptions = append(e.subscriptions, reply)
	terminals, err := e.followTerminals(ctx)
	if err != nil {
		return fmt.Errorf("saga: follow terminal events: %w", err)
	}
	e.follows = append(e.follows, terminals...)
	for _, k := range e.opts.Owned {
		p, err := e.replayPartition(ctx, k)
		if err != nil {
			return err
		}
		e.partitions = append(e.partitions, p)
	}
	if err := e.serveQueries(); err != nil {
		return err
	}
	for _, p := range e.partitions {
		if err := e.startPartition(ctx, p); err != nil {
			return err
		}
	}
	e.ready.Store(true)
	return nil
}

// startPartition 接上输入的 Follow（从重放读到的最后一条之后），起分区循环。
func (e *Engine) startPartition(ctx context.Context, p *partition) error {
	follow, err := e.log.Follow(ctx, p.stream, []string{e.layout.InputFilter(p.k)}, p.followFrom, func(msg fnats.LogMsg) {
		select {
		case p.inputs <- msg:
		case <-p.stopCh:
		}
	})
	if err != nil {
		return fmt.Errorf("saga: follow inputs of partition %d: %w", p.k, err)
	}
	p.follow = follow
	loopCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.run(loopCtx)
	return nil
}

// Stop 按三步停机：停分区循环与快照 goroutine、停输入 / 终态 / 准入的消费、退订查询与回复，在 ctx 内等它们退出；超时返回
// ctx 错误并保留未停完的部分，可以用新 ctx 再调用。重复调用返回 nil。
func (e *Engine) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.lifeMu.Lock()
	defer e.lifeMu.Unlock()
	return e.stopLocked(ctx)
}

func (e *Engine) stopLocked(ctx context.Context) error {
	if e.stopped {
		return nil
	}
	e.ready.Store(false)
	for _, p := range e.partitions {
		select {
		case <-p.stopCh:
		default:
			close(p.stopCh)
		}
		if p.cancel != nil {
			p.cancel()
		}
	}
	for _, p := range e.partitions {
		if p.follow != nil {
			if err := p.follow.Stop(ctx); err != nil {
				return err
			}
		}
		if p.cancel != nil {
			select {
			case <-p.done:
			case <-ctx.Done():
				return fmt.Errorf("saga: partition %d loop still running: %w", p.k, ctx.Err())
			}
		}
		if err := waitGroup(ctx, &p.workers); err != nil {
			return fmt.Errorf("saga: partition %d snapshot still running: %w", p.k, err)
		}
	}
	for len(e.follows) > 0 {
		if err := e.follows[0].Stop(ctx); err != nil {
			return err
		}
		e.follows = e.follows[1:]
	}
	if e.stopAdmission != nil {
		if err := e.stopAdmission(ctx); err != nil {
			return err
		}
		e.stopAdmission = nil
	}
	for len(e.subscriptions) > 0 {
		if err := e.subscriptions[0].DrainContext(ctx); err != nil {
			return err
		}
		e.subscriptions = e.subscriptions[1:]
	}
	e.stopped = true
	return nil
}

func waitGroup(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run 启动协调器并阻塞到 ctx 结束或某个分区 fail-stop，然后停机（停机预算 30s）。分区 fail-stop 时返回它的原因。
func (e *Engine) Run(ctx context.Context) error {
	if err := e.Start(ctx); err != nil {
		return err
	}
	var cause error
	select {
	case <-ctx.Done():
	case <-e.failed:
		for _, p := range e.partitions {
			select {
			case <-p.done:
				if p.failErr != nil && cause == nil {
					cause = p.failErr
				}
			default:
			}
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.Stop(stopCtx); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// Stats 返回累计计数。
func (e *Engine) Stats() Stats {
	c := &e.stats
	return Stats{
		Started: c.started.Load(), Dispatched: c.dispatched.Load(), Completed: c.completed.Load(), Compensated: c.compensated.Load(),
		Failed: c.failed.Load(), ManualRequired: c.manualRequired.Load(), Duplicates: c.duplicates.Load(),
		LateAfterAbandon: c.lateAfterAbandon.Load(), StaleIncarnation: c.staleIncarnation.Load(), StaleAttempt: c.staleAttempt.Load(),
		Reopened: c.reopened.Load(), RejectedInputs: c.rejectedInputs.Load(), AdmissionRejected: c.admissionRejected.Load(),
		UnknownSaga: c.unknownSaga.Load(), Events: c.events.Load(), PublishUnknown: c.publishUnknown.Load(),
		PublishResolved: c.publishResolved.Load(), BatchIncomplete: c.batchIncomplete.Load(), PublishRetried: c.publishRetried.Load(),
		Snapshots: c.snapshots.Load(), SnapshotConflicts: c.snapshotConflicts.Load(), Purges: c.purges.Load(), Pruned: c.pruned.Load(),
		Replayed: c.replayed.Load(), FailStops: c.failStops.Load(),
	}
}

// ---- 发起方 API ----

// StartSaga 追加一条启动意图并等分区所有者的判定（设计 §7）：接受或同一意图已存在 → 返回记录；同业务键不同意图 →
// ErrIdentityConflict；准入关闭 → ErrBackpressure（提示关闭时不写意图，写入后被拒同样返回它）；所有者缺定义 →
// ErrDefinitionMissing；ctx 到期仍无判定 → ErrStartUnconfirmed（意图已持久、结果未知，用同一请求重试安全）。
// saga ID 恒为 IDOf(Type, BusinessKey)（选项 ID-A）；request.Now 不用（时间以所有者为准）。
func (e *Engine) StartSaga(ctx context.Context, request StartRequest) (Record, error) {
	if _, ok := e.definition(request.Type, request.DefinitionVersion); !ok {
		return Record{}, fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, request.Type, request.DefinitionVersion)
	}
	request, err := prepareStart(request, e.opts.MaxPayloadBytes)
	if err != nil {
		return Record{}, err
	}
	digest, err := startIntentDigest(request)
	if err != nil {
		return Record{}, err
	}
	if closed, reason := e.hints.closed(); closed {
		return Record{}, errAdmissionClosed(reason)
	}
	id := IDOf(request.Type, request.BusinessKey)
	k := e.layout.PartitionOf(id)
	attempt := e.nextAttempt()
	request.Now = time.Time{}
	data, _ := json.Marshal(startInput{V: WireVersion, Request: request, Attempt: attempt, Digest: digest})
	return e.submitIntent(ctx, e.layout.StartSubject(k), "start."+id+"."+attempt, attempt, data, id, digest)
}

// Get 从分区所有者的内存读一条记录；内存里没有（还没启动，或终态过了保留期被剪掉）时，冷副本开启则回落读冷副本，否则
// 返回 ErrNotFound。所有者不在返回 ErrPartitionUnavailable（不拿冷副本顶替：内存里可能有更新的版本）。
func (e *Engine) Get(ctx context.Context, id string) (Record, error) {
	record, err := e.getOwned(ctx, id)
	if errors.Is(err, ErrNotFound) && e.opts.ColdCopy != nil {
		metrics.IncCounter("saga.cold_copy.fallback_reads_total", metrics.Labels{"op": "get"}, 1)
		return e.opts.ColdCopy.Get(ctx, strings.TrimSpace(id))
	}
	return record, err
}

// getOwned 只问分区所有者的内存（没有返回 ErrNotFound）。启动判定的补查与 WaitTerminal 用它：冷副本不参与任何判定。
func (e *Engine) getOwned(ctx context.Context, id string) (Record, error) {
	id = strings.TrimSpace(id)
	if !validSubjectToken(id, 128) {
		return Record{}, ErrInvalidRecord
	}
	reply, err := e.ask(ctx, e.layout.PartitionOf(id), queryRequest{Kind: "get", ID: id})
	if err != nil {
		return Record{}, err
	}
	if !reply.Found || reply.Record == nil {
		return Record{}, ErrNotFound
	}
	return *reply.Record, nil
}

// GetByBusinessKey = Get(IDOf(type, businessKey))。还没启动（或终态过了保留期、且冷副本关闭或没有它）返回 ErrNotFound。
func (e *Engine) GetByBusinessKey(ctx context.Context, sagaType, businessKey string) (Record, error) {
	return e.Get(ctx, IDOf(strings.TrimSpace(sagaType), strings.TrimSpace(businessKey)))
}

// List 向全部分区所有者散播查询并合并（按更新时间、ID 排序，截取 Limit）。覆盖非终态、保留期内的终态与全部 ManualRequired；
// 冷副本开启时再补上内存里没有的记录（更早的终态，listWithColdCopy）。有分区没回答返回 ErrPartitionUnavailable（点名），
// 不返回残缺结果。Limit 缺省 100、上限 1000。
func (e *Engine) List(ctx context.Context, query Query) ([]Record, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > 1000 {
		return nil, ErrInvalidRecord
	}
	if e.opts.ColdCopy != nil {
		return e.listWithColdCopy(ctx, query)
	}
	replies, err := e.askAll(ctx, queryRequest{Kind: "list", Query: &query})
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, reply := range replies {
		out = append(out, reply.Records...)
	}
	sortRecords(out)
	if len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

// Resume 向分区追加一条 Resume 意图并等判定。规则见 planResume，另有回执保留期守卫（ErrReceiptsExpired）。
// ctx 到期仍无判定 → ErrStartUnconfirmed（再发同一请求是安全的：已生效的 Resume 之后记录不再是可 Resume 的状态）。
func (e *Engine) Resume(ctx context.Context, request ResumeRequest) (Record, error) {
	request, _, err := prepareResume(request, e.now())
	if err != nil {
		return Record{}, err
	}
	return e.submitOperation(ctx, opInput{Kind: opResume, ID: request.ID, Resume: &request, Now: request.Now})
}

// Compensate 向分区追加一条人工补偿意图并等判定（planCompensate：已在补偿或已补偿原样返回，Completed 返回 ErrSagaCompleted）。
func (e *Engine) Compensate(ctx context.Context, id, reason string, now time.Time) (Record, error) {
	id = strings.TrimSpace(id)
	if !validSubjectToken(id, 128) || len(reason) > 4096 {
		return Record{}, ErrInvalidRecord
	}
	if now.IsZero() {
		now = e.now()
	}
	return e.submitOperation(ctx, opInput{Kind: opCompensate, ID: id, Reason: reason, Now: now.UTC()})
}

func (e *Engine) submitOperation(ctx context.Context, in opInput) (Record, error) {
	in.V = WireVersion
	in.Attempt = e.nextAttempt()
	data, _ := json.Marshal(in)
	k := e.layout.PartitionOf(in.ID)
	return e.submitIntent(ctx, e.layout.OpSubject(k), "op."+in.ID+"."+in.Attempt, in.Attempt, data, in.ID, "")
}

// Complete 把一份结果追加到所属分区的 in.res（MsgID res.<CommandID>），只承诺“已持久进入输入”；接收与否由分区循环判定。
// 框架自带的步骤消费者直接发布（publishCompletion），它留给自定义参与方。
func (e *Engine) Complete(ctx context.Context, completion Completion) error {
	if completion.Validate() != nil || len(completion.Data) > e.opts.MaxPayloadBytes {
		return ErrInvalidRecord
	}
	completion.CompletedAt = time.Time{}
	data, _ := json.Marshal(resultInput{V: WireVersion, Completion: completion})
	k := e.layout.PartitionOf(completion.SagaID)
	return e.publishInput(ctx, fnats.LogMsg{Subject: e.layout.ResultSubject(k), Data: data, MsgID: "res." + completion.CommandID}, false)
}
