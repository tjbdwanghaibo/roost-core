package saga

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// Mod 解析 saga.*，取 NATS 的三项能力（nats.log、nats.raw、nats.jetstream，同一条连接）交给 core 的 saga.Assemble，并转发
// 生命周期。协调器的权威是 JetStream 分区事件日志：本进程 sid 在 saga.coordinators 里时拥有一部分分区（静态分配，选项 P-A），
// 跑分区循环、回答查询、做准入；不在列表里的进程只做发起方（StartSaga、查询、WaitTerminal）。每个装了本 Mod 的进程都跑
// 启动中继（Nest 事务里提交的启动意图 → 分区）。
//
// 写者身份（进事件的 MsgID）是 <sid>-<单实例锁本次启动的 token>：每次进程启动不同，崩溃重启的短暂重叠里新旧两个进程写同一
// 分区时，期望序号让其中一个被隔离并 fail-stop。没有启用单实例锁的进程（开发环境）用 <sid>-<随机串>。
type Mod struct {
	definitions []coresaga.Definition
	config      coresaga.AssemblyConfig
	sid         int32
	asm         *coresaga.Assembly
	// coldCopy 是 saga.cold_copy.* 解析后的参数（enabled=false 时为 nil）；Mongo 客户端在 Provide 时填。
	coldCopy *coresaga.ColdCopySettings
}

func NewMod(definitions ...coresaga.Definition) *Mod {
	return &Mod{definitions: append([]coresaga.Definition(nil), definitions...)}
}

// CombineDefinitions flattens generated per-Saga version groups for NewMod.
func CombineDefinitions(groups ...[]coresaga.Definition) []coresaga.Definition {
	total := 0
	for i := range groups {
		total += len(groups[i])
	}
	definitions := make([]coresaga.Definition, 0, total)
	for i := range groups {
		definitions = append(definitions, groups[i]...)
	}
	return definitions
}

func (m *Mod) Name() app.ModName { return mods.ModSaga }

// DependsOn 只有 NATS：协调器的全部状态在 JetStream 里。health 是 Registry 内建能力，不是 Mod 名。
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModNats} }

// OptionalDependsOn：同进程有 DataEngine 时在它之后启动——启动中继订阅的效果流由 DataEngine 建；同进程有 Mongo 时在它之后
// 启动——冷副本（saga.cold_copy.enabled）读写 Mongo（开启冷副本却没有 Mongo 由 CheckConfigAcrossMods 拒绝）。
func (m *Mod) OptionalDependsOn() []app.ModName {
	return []app.ModName{mods.ModDataEngine, mods.ModMongo}
}

// ConfigSchema 声明 saga.*。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	c := &settings.Saga
	coordinators, err := settings.coordinators()
	if err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	effects, err := kitdataengine.LoadEffectsConfig(cfg)
	if err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	layout := settings.layout()
	m.sid = settings.Sid
	m.config = coresaga.AssemblyConfig{
		Engine: coresaga.Options{
			Layout: layout, Owned: ownedPartitions(coordinators, settings.Sid, layout.Partitions),
			Replicas: c.Replicas, StreamMaxBytes: c.StreamMaxBytes, DuplicateWindow: c.DuplicateWindow, MaxBatch: c.MaxBatch, Tick: c.Tick,
			SnapshotEveryEvents: c.SnapshotEveryEvents, SnapshotInterval: c.SnapshotInterval,
			TerminalRetention: c.TerminalRetention, ReceiptRetention: c.ReceiptRetention,
			PublishTimeout: c.PublishTimeout, PublishUnknownBudget: c.PublishUnknownBudget,
			QueryTimeout: c.QueryTimeout, StartVerdictTimeout: c.StartVerdictTimeout, PurgeStallAlarm: c.PurgeStallAlarm,
			MaxPayloadBytes: settings.SagaPayloadLimit(), Admission: settings.admission(),
		},
		Starts: coresaga.NestStartConsumerConfig{
			Workers: c.StartEffectWorkers, Stream: stringDefault(c.StartEffectStream, effects.Stream), Durable: c.StartEffectDurable,
			EffectPrefix: stringDefault(c.StartEffectPrefix, effects.Prefix()),
			AckWait:      c.StartEffect.AckWait, ProcessTimeout: c.StartEffect.ProcessTimeout, MaxDeliver: c.StartEffect.MaxDeliver,
			MaxAckPending: c.StartEffect.MaxAckPending, NakBackoffMin: c.StartEffect.NakBackoffMin, NakBackoffMax: c.StartEffect.NakBackoffMax,
		},
	}
	budgets, err := stepBudgets(c.StepDefaults, c.Steps, m.definitions)
	if err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	m.config.Engine.StepBudgets = budgets
	if cold := c.ColdCopy; cold.Enabled {
		m.coldCopy = &coresaga.ColdCopySettings{Database: c.Database, Collection: cold.Collection, TerminalOnly: cold.TerminalOnly,
			BatchSize: cold.BatchSize, MaxWait: cold.MaxWait}
	}
	return checkStepTimeouts(budgets, m.definitions)
}

// CheckConfigAcrossMods 是 saga 读别的 Mod 配置键的规则（App 在配置检查时调用：启动前任何 Mod Init 之前、
// --check-config 与 doctor 的 config-check 同一处）：
//
//   - 同一进程注册了 DataEngine 时，saga.start_effect_* 写了就必须与 dataengine.effects 相同：Nest 的 saga start 由
//     DataEngine 发布到 dataengine.effects 那条流，启动中继订阅 saga.start_effect_*，两边写得不同，启动时只会得到
//     JetStream 的 "stream not found" 或永远收不到 start。
//   - 同一进程注册了 DataEngine 且准入开启时，dataengine.projection.max_unacked_records 必须 > 0 且 ≥ 4 × saga.admission.high_records：
//     准入只拦新启动，在途推进照常产生投影积压，硬上限是最后一道闸（到上限 DataEngine 在 WAL 准入前拒绝，命令 nak 重试）。
//   - saga.cold_copy.enabled 时本进程必须注册 Mongo Mod（冷副本的读写都在 saga.database）。
func (m *Mod) CheckConfigAcrossMods(cfg *viper.Viper, registered func(app.ModName) bool) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return err
	}
	if settings.Saga.ColdCopy.Enabled && !registered(mods.ModMongo) {
		return fmt.Errorf("saga.cold_copy.enabled needs the mongo Mod in this process: the cold copy is written to and read from %s.%s",
			settings.Saga.Database, settings.Saga.ColdCopy.Collection)
	}
	if !registered(mods.ModDataEngine) {
		return nil
	}
	effects, err := kitdataengine.LoadEffectsConfig(cfg)
	if err != nil {
		return err
	}
	var errs []error
	if stream := strings.TrimSpace(settings.Saga.StartEffectStream); stream != "" && stream != effects.Stream {
		errs = append(errs, effectKeyMismatch("saga.start_effect_stream", stream, "dataengine.effects.stream", effects.Stream))
	}
	if prefix := strings.Trim(strings.TrimSpace(settings.Saga.StartEffectPrefix), "."); prefix != "" && prefix != effects.Prefix() {
		errs = append(errs, effectKeyMismatch("saga.start_effect_prefix", settings.Saga.StartEffectPrefix, "dataengine.effects.subject_prefix", effects.Prefix()))
	}
	if settings.Saga.Admission.Enabled {
		maxUnacked, err := kitdataengine.ProjectionMaxUnackedRecords(cfg)
		if err != nil {
			return err
		}
		if high := settings.Saga.Admission.HighRecords; maxUnacked <= 0 || maxUnacked < 4*high {
			errs = append(errs, fmt.Errorf("dataengine.projection.max_unacked_records (%d) must be set and at least 4 × saga.admission.high_records (%d = %d): "+
				"admission only refuses new sagas, so in-flight steps keep adding projection backlog and the hard cap is the last stop", maxUnacked, high, 4*high))
		}
	}
	return errors.Join(errs...)
}

func effectKeyMismatch(sagaKey, sagaValue, effectKey, effectValue string) error {
	return fmt.Errorf("%s (%q) differs from %s (%q): this process's Data Engine publishes Nest saga starts on the effect stream, "+
		"and the saga start relay would subscribe elsewhere (startup fails with \"stream not found\" or never sees a start); "+
		"delete %s so it follows %s, or set both to the same value", sagaKey, sagaValue, effectKey, effectValue, sagaKey, effectKey)
}

func (m *Mod) Provide(registry *app.Registry) error {
	if registry == nil {
		return fmt.Errorf("saga mod: nil registry")
	}
	log, ok := app.Lookup[fnats.JetStreamLog](registry, mods.ModNatsLog)
	if !ok || log == nil {
		return fmt.Errorf("saga mod: capability %q not found", mods.ModNatsLog)
	}
	raw, ok := app.Lookup[fnats.RawClient](registry, mods.ModNatsRaw)
	if !ok || raw == nil {
		return fmt.Errorf("saga mod: capability %q not found", mods.ModNatsRaw)
	}
	jetStream, ok := app.Lookup[fnats.IJetStream](registry, mods.ModNatsJetStream)
	if !ok || jetStream == nil {
		return fmt.Errorf("saga mod: capability %q not found", mods.ModNatsJetStream)
	}
	if failure, ok := app.Lookup[*app.RuntimeFailure](registry, mods.ModRuntimeFailure); ok {
		m.config.Engine.Failure = failure
	}
	m.config.Engine.Writer = m.writer(registry)
	if m.coldCopy != nil {
		client, ok := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
		if !ok || client == nil {
			return fmt.Errorf("saga mod: saga.cold_copy.enabled but capability %q not found", mods.ModMongo)
		}
		cold := *m.coldCopy
		cold.Client = client
		m.config.ColdCopy = &cold
	}
	asm, err := coresaga.Assemble(log, raw, jetStream, m.config, m.definitions...)
	if err != nil {
		return err
	}
	m.asm = asm
	if err := registry.Register(mods.ModSaga, asm.Engine); err != nil {
		return err
	}
	healthRegistry, ok := app.Lookup[*health.Registry](registry, mods.ModHealth)
	if !ok || healthRegistry == nil {
		return fmt.Errorf("saga mod: capability %q not found", mods.ModHealth)
	}
	healthRegistry.Register("saga", health.CheckerFunc(m.checkHealth))
	return nil
}

// writer 是本进程的写者身份：<sid>-<单实例锁 token>，没有单实例锁时 <sid>-<随机串>。每次启动都不同。
func (m *Mod) writer(registry *app.Registry) string {
	if incarnation, ok := app.Lookup[app.SingletonIncarnation](registry, mods.ModSingletonIncarnation); ok && incarnation.Token != "" {
		return fmt.Sprintf("%d-%s", m.sid, incarnation.Token)
	}
	return fmt.Sprintf("%d-%s", m.sid, rand.Text())
}

// Start 启动协调器（先恢复再服务：拥有的分区重放完才返回）与启动中继。重放时长随日志尾巴与快照大小变，上限 2 分钟（与 App
// 的启动预算同一量级）。
func (m *Mod) Start() error {
	if m == nil || m.asm == nil {
		return fmt.Errorf("saga mod: not provided")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return m.asm.Start(ctx)
}

func (m *Mod) Stop() { _ = m.StopWithContext(context.Background()) }
func (m *Mod) StopWithContext(ctx context.Context) error {
	if m == nil || m.asm == nil {
		return nil
	}
	return m.asm.Stop(ctx)
}

// Engine 返回协调器（发起方 API：StartSaga、Get、GetByBusinessKey、List、Resume、Compensate、Complete、WaitTerminal）。
func (m *Mod) Engine() *coresaga.Engine {
	if m == nil || m.asm == nil {
		return nil
	}
	return m.asm.Engine
}

func (m *Mod) checkHealth(context.Context) health.Result {
	if m == nil || m.asm == nil || !m.asm.Running() {
		return health.Result{Status: health.StatusFail, Message: "not running"}
	}
	if err := m.asm.RunError(); err != nil {
		return health.Result{Status: health.StatusFail, Message: "coordinator failed", Err: err}
	}
	if m.asm.ConsumersClosed() {
		return health.Result{Status: health.StatusFail, Message: "start relay stopped"}
	}
	stats, admission := m.asm.Engine.Stats(), m.asm.Engine.AdmissionState()
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("running partitions=%d cold_copy=%t admission_open=%t manual_required=%d late_after_abandon=%d reopened=%d unknown_saga=%d publish_unknown=%d",
		len(m.config.Engine.Owned), m.coldCopy != nil, admission.Open, stats.ManualRequired, stats.LateAfterAbandon, stats.Reopened, stats.UnknownSaga, stats.PublishUnknown)}
}

func stringDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var _ app.Mod = (*Mod)(nil)
var _ app.ModOptionalDependencyProvider = (*Mod)(nil)

// Participant 是步骤一侧（参与方）与协调器会合要用的配置：分区布局（命令在哪些流上、结果发回哪个分区）、流与桶的副本数、
// Mongo 步骤收件箱所在的库、回执保留期与准入信号参数。步骤运行时 Steps 用它；业务自己组装步骤消费者时同样经它，不直接读
// viper、不另抄一份缺省值。
type Participant struct {
	Layout           coresaga.Layout
	Replicas         int
	Database         string
	ReceiptRetention time.Duration
	Admission        coresaga.AdmissionOptions
	// SignalInterval 是 LoadPublisher 发布积压信号的间隔（saga.admission.signal_interval）。
	SignalInterval time.Duration
	// Sid 是本进程的 sid（准入信号的键名用）。
	Sid int32
	// StepWindow 是缺省步骤预算窗口：saga.step_defaults（缺省取 coresaga.DefaultStepBudget）的
	// MaxAttempts × Timeout 加尝试之间的退避。参与方启动探针拿它比 transactionLifetimeLimitSeconds（设计 T-A）。
	StepWindow time.Duration
}

// ParticipantSettings 按 saga Mod 的声明（整份 saga.*，与 Mod Init 读的同一个结构体）返回 Participant。
func ParticipantSettings(cfg *viper.Viper) (Participant, error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return Participant{}, fmt.Errorf("saga: %w", err)
	}
	return Participant{
		Layout: settings.layout(), Replicas: settings.Saga.Replicas, Database: settings.Saga.Database,
		ReceiptRetention: settings.Saga.ReceiptRetention, Admission: settings.admission(),
		SignalInterval: settings.Saga.Admission.SignalInterval, Sid: settings.Sid,
		StepWindow: stepWindow(settings.Saga.StepDefaults),
	}, nil
}

// stepWindow 是按 saga.step_defaults 补齐框架缺省后的一个步骤从第一次派发到最后一次尝试超时的时长：
// MaxAttempts × Timeout + 第 i 次重试前的退避 min(BackoffMin × 2^(i-1), BackoffMax) 之和。
func stepWindow(defaults stepBudgetConfig) time.Duration {
	budget := coresaga.DefaultStepBudget()
	if defaults.Timeout > 0 {
		budget.Timeout = defaults.Timeout
	}
	if defaults.MaxAttempts > 0 {
		budget.MaxAttempts = uint32(defaults.MaxAttempts)
	}
	if defaults.BackoffMin > 0 {
		budget.BackoffMin = defaults.BackoffMin
	}
	if defaults.BackoffMax > 0 {
		budget.BackoffMax = defaults.BackoffMax
	}
	window := time.Duration(budget.MaxAttempts) * budget.Timeout
	backoff := budget.BackoffMin
	for range int(budget.MaxAttempts) - 1 {
		window += min(backoff, budget.BackoffMax)
		backoff *= 2
	}
	return window
}
