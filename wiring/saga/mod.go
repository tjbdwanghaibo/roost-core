package saga

import (
	"context"
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
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"
)

// Mod parses configuration, looks up the Mongo, authority Redis and JetStream
// capabilities, hands them to core's saga.Assemble and forwards lifecycle
// calls. The coordinator's authoritative store is the authority Redis
// (redis_authority.*, saga.redis.*); Mongo holds the cold copy the Store
// Service projects into. Consumer subscription, engine loop and
// drain-then-stop live in core.
type Mod struct {
	definitions []coresaga.Definition
	config      coresaga.AssemblyConfig
	asm         *coresaga.Assembly
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

// DependsOn names Mods: Mongo, the authority Redis and NATS. JetStream is the
// NATS Mod's capability and health is a registry built-in — neither is a Mod
// name, and app resolves dependencies by Mod name.
func (m *Mod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModMongo, mods.ModRedisAuthority, mods.ModNats}
}

func (m *Mod) OptionalDependsOn() []app.ModName {
	return []app.ModName{mods.ModDataEngine}
}

// Locations 是 saga Mod 按配置使用的持久位置：权威 Redis 上的键前缀与分区（Redis 连接见 redis_authority.*）、Mongo
// 冷副本（库与集合，没写的集合名为空、由 core 取缺省）与 JetStream 流名、主题前缀。
type Locations struct {
	Redis         coresaga.RedisStoreOptions
	Store         coresaga.MongoStoreOptions
	Stream        string
	SubjectPrefix string
}

// LoadLocations 按 saga Mod 的声明读 Locations（与 Init 同一个结构体）：运维工具（cmd/sidmerge 的“全部 saga
// 终态”检查）与 Store Service 用它找 saga 记录，不另抄缺省。
func LoadLocations(cfg *viper.Viper) (Locations, error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return Locations{}, fmt.Errorf("saga: %w", err)
	}
	return settings.locations(), nil
}

func (settings *config) locations() Locations {
	c := settings.Saga
	return Locations{
		Redis: coresaga.RedisStoreOptions{KeyPrefix: c.Redis.KeyPrefix, Partitions: c.Redis.Partitions, WaitAOFTimeout: c.Redis.WaitAOFTimeout,
			TerminalRetention: c.Redis.TerminalRetention, BusinessKeyGuard: c.Redis.BusinessKeyGuard, MaxProjectionBacklog: c.Redis.MaxProjectionBacklog},
		Store: coresaga.MongoStoreOptions{Database: c.Database, SagaCollection: c.Collections.Sagas,
			CompletionCollection: c.Collections.Completions, OperationCollection: c.Collections.Operations, CompletionReceiptTTL: c.CompletionReceiptTTL},
		Stream: c.Stream, SubjectPrefix: c.SubjectPrefix,
	}
}

// concurrency 是本 Mod 在权威 Redis 连接池上的并发上限：协调与发布循环各占一条连接，三类消费者的每个在途处理各占一条。
func (settings *config) concurrency() int {
	c := settings.Saga
	return c.CoordinatorWorkers + c.PublisherWorkers + c.ResultWorkers + c.ResultEffectWorkers + c.StartEffectWorkers
}

// ConfigSchema 声明 saga.*。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	c := settings.Saga
	defaults := coresaga.DefaultOptions()
	owner := c.Owner
	if owner == "" {
		owner = fmt.Sprintf("saga-%d-%s", settings.Sid, coresaga.NewID())
	}
	effects, err := kitdataengine.LoadEffectsConfig(cfg)
	if err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	startStream, startPrefix, resultStream, resultPrefix := effectStreams(&settings, effects)
	locations := settings.locations()
	m.config = coresaga.AssemblyConfig{
		Store: locations.Store,
		Redis: locations.Redis,
		Engine: coresaga.Options{Owner: owner, CoordinatorWorkers: c.CoordinatorWorkers, PublisherWorkers: c.PublisherWorkers,
			CoordinatorBatch: c.CoordinatorClaimBatch, PublisherBatch: c.PublisherClaimBatch, LeaseDuration: c.LeaseDuration,
			StoreTimeout: c.StoreTimeout, PollInterval: c.PollInterval, PublishTimeout: c.PublishTimeout,
			PublishBackoffMin: c.PublishBackoffMin, PublishBackoffMax: c.PublishBackoffMax, MaxPayloadBytes: settings.SagaPayloadLimit(),
			StepBudgets: defaults.StepBudgets},
		Prefix: c.SubjectPrefix,
		Stream: fnats.JetStreamConfig{Name: c.Stream, Subjects: []string{c.SubjectPrefix + ".>"}, Storage: fnats.JetStreamStorageFile,
			MaxAge: c.StreamMaxAge, Duplicates: c.DuplicateWindow, Replicas: c.Replicas, MaxBytes: c.StreamMaxBytes},
		Completions: coresaga.CompletionConsumerConfig{
			Workers: c.ResultWorkers, Stream: c.Stream, Durable: c.ResultDurable, SubjectPrefix: c.SubjectPrefix,
			AckWait: c.Result.AckWait, ProcessTimeout: c.Result.ProcessTimeout, MaxDeliver: c.Result.MaxDeliver,
			MaxAckPending: c.Result.MaxAckPending, NakBackoffMin: c.Result.NakBackoffMin, NakBackoffMax: c.Result.NakBackoffMax,
		},
		// Native Nest steps commit their completion as an effect, so it
		// arrives on the effect stream rather than the saga stream. Its
		// consumer shares the start consumer's stream and prefix and takes
		// its own durable; core derives the rest when these are empty.
		NestResults: coresaga.NestCompletionConsumerConfig{
			Workers: c.ResultEffectWorkers, Stream: resultStream, Durable: c.ResultEffectDurable, EffectPrefix: resultPrefix,
			AckWait: c.ResultEffect.AckWait, ProcessTimeout: c.ResultEffect.ProcessTimeout, MaxDeliver: c.ResultEffect.MaxDeliver,
			MaxAckPending: c.ResultEffect.MaxAckPending, NakBackoffMin: c.ResultEffect.NakBackoffMin, NakBackoffMax: c.ResultEffect.NakBackoffMax,
		},
		Starts: coresaga.NestStartConsumerConfig{
			Workers: c.StartEffectWorkers, Stream: startStream, Durable: c.StartEffectDurable, EffectPrefix: startPrefix,
			AckWait: c.StartEffect.AckWait, ProcessTimeout: c.StartEffect.ProcessTimeout, MaxDeliver: c.StartEffect.MaxDeliver,
			MaxAckPending: c.StartEffect.MaxAckPending, NakBackoffMin: c.StartEffect.NakBackoffMin, NakBackoffMax: c.StartEffect.NakBackoffMax,
		},
	}
	budgets, err := stepBudgets(c.StepDefaults, c.Steps, m.definitions)
	if err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	m.config.Engine.StepBudgets = budgets
	if err := checkStepTimeouts(budgets, m.definitions); err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	if m.config.Store.CompletionReceiptTTL <= m.config.Stream.MaxAge {
		return fmt.Errorf("saga: completion receipt ttl must exceed stream max age")
	}
	return nil
}

// effectStreams 解析 saga 订阅的两条效果流（Nest start、原生步骤结果）：不写取 dataengine.effects 的值，结果流不写取
// start 流。Init 装配消费者与配置检查（checkEffectRetention）用同一个解析。
func effectStreams(settings *config, effects kitdataengine.EffectsConfig) (startStream, startPrefix, resultStream, resultPrefix string) {
	c := &settings.Saga
	startStream = stringDefault(c.StartEffectStream, effects.Stream)
	startPrefix = stringDefault(c.StartEffectPrefix, effects.Prefix())
	resultStream = stringDefault(c.ResultEffectStream, startStream)
	resultPrefix = stringDefault(c.ResultEffectPrefix, startPrefix)
	return startStream, startPrefix, resultStream, resultPrefix
}

// CheckConfigAcrossMods 是 saga 读别的 Mod 配置键的规则（App 在配置检查时调用：启动前任何 Mod Init 之前、
// --check-config 与 doctor 的 config-check 同一处）：
//
//   - checkAuthorityPool：同一进程注册了权威 Redis Mod 时，redis_authority.pool_size 不小于本 Mod 的并发之和（不够时
//     并发的消费者、循环排队等连接，存储超时被等连接吃掉）。
//   - checkEffectRetention：完成回执要比效果流里的结果活得久。不看本进程是否注册了 DataEngine——只跑协调器的进程
//     读的仍是按 dataengine.effects 解析的那条流，它的保留期由别的进程的 DataEngine 按同一份配置建。
//   - 同一进程注册了 DataEngine 时，saga.start_effect_* 写了就必须与 dataengine.effects 相同：Nest 的 saga start 由
//     DataEngine 发布到 dataengine.effects 那条流，本 Mod 的 start 消费者订阅 saga.start_effect_*，两边写得不同，启动时
//     只会得到 JetStream 的 "stream not found" 或永远收不到 start。这里点名两个键并给出改法。没有 DataEngine 的进程
//     不比较（显式值就是它要订阅的流）。
func (m *Mod) CheckConfigAcrossMods(cfg *viper.Viper, registered func(app.ModName) bool) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return err
	}
	effects, err := kitdataengine.LoadEffectsConfig(cfg)
	if err != nil {
		return err
	}
	errs := []error{checkEffectRetention(&settings, effects)}
	if registered(mods.ModRedisAuthority) {
		errs = append(errs, checkAuthorityPool(cfg, &settings))
	}
	if !registered(mods.ModDataEngine) {
		return errors.Join(errs...)
	}
	if stream := strings.TrimSpace(settings.Saga.StartEffectStream); stream != "" && stream != effects.Stream {
		errs = append(errs, effectKeyMismatch("saga.start_effect_stream", stream, "dataengine.effects.stream", effects.Stream))
	}
	if prefix := strings.Trim(strings.TrimSpace(settings.Saga.StartEffectPrefix), "."); prefix != "" && prefix != effects.Prefix() {
		errs = append(errs, effectKeyMismatch("saga.start_effect_prefix", settings.Saga.StartEffectPrefix, "dataengine.effects.subject_prefix", effects.Prefix()))
	}
	return errors.Join(errs...)
}

// checkAuthorityPool：权威 Redis 连接池要容得下本进程同时在用的连接（saga 的并发，见 concurrency）。
func checkAuthorityPool(cfg *viper.Viper, settings *config) error {
	authority, err := kitredis.LoadAuthorityConfig(cfg)
	if err != nil {
		return err
	}
	if need := settings.concurrency(); authority.PoolSize < need {
		return fmt.Errorf("redis_authority.pool_size (%d) is smaller than the saga concurrency on it (%d = coordinator_workers + publisher_workers + "+
			"result_workers + result_effect_workers + start_effect_workers); raise the pool or lower the workers", authority.PoolSize, need)
	}
	return nil
}

func effectKeyMismatch(sagaKey, sagaValue, effectKey, effectValue string) error {
	return fmt.Errorf("%s (%q) differs from %s (%q): this process's Data Engine publishes Nest saga starts on the effect stream, "+
		"and the saga start consumer would subscribe elsewhere (startup fails with \"stream not found\" or never sees a start); "+
		"delete %s so it follows %s, or set both to the same value", sagaKey, sagaValue, effectKey, effectValue, sagaKey, effectKey)
}

// checkEffectRetention 是跨 Mod 校验（维护者决定）：原生 Nest 步骤的完成结果在 DataEngine 的效果流上，回执必须比
// 流里的结果活得久，否则回执过期后再投递的结果协调器分不清是已计入的重复还是放弃后生效的成功，只能 Term。saga 流的
// 同一前提由 Init 校验；这里只在结果效果流就是 DataEngine 效果流（与 Init 同一个解析）时比较，别的流的保留期不归这份
// 配置管。
func checkEffectRetention(settings *config, effects kitdataengine.EffectsConfig) error {
	_, _, resultStream, _ := effectStreams(settings, effects)
	if strings.TrimSpace(resultStream) != effects.Stream {
		return nil
	}
	if ttl := settings.Saga.CompletionReceiptTTL; ttl <= effects.MaxAge {
		return fmt.Errorf("saga.completion_receipt_ttl (%s) must exceed dataengine.effects.max_age (%s): "+
			"native step results are kept on the effect stream %s that long, and a result redelivered after its "+
			"completion receipt expired can no longer be told from a late success and is terminated; "+
			"raise saga.completion_receipt_ttl or lower dataengine.effects.max_age",
			ttl, effects.MaxAge, effects.Stream)
	}
	return nil
}

func (m *Mod) Provide(registry *app.Registry) error {
	if registry == nil {
		return fmt.Errorf("saga mod: nil registry")
	}
	mongoClient, ok := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
	if !ok || mongoClient == nil {
		return fmt.Errorf("saga mod: capability %q not found", mods.ModMongo)
	}
	authority, ok := app.Lookup[fredis.IRedis](registry, mods.ModRedisAuthority)
	if !ok || authority == nil {
		return fmt.Errorf("saga mod: capability %q not found; add wiring/redis.NewAuthorityMod()", mods.ModRedisAuthority)
	}
	jetStream, ok := app.Lookup[fnats.IJetStream](registry, mods.ModNatsJetStream)
	if !ok || jetStream == nil {
		return fmt.Errorf("saga mod: capability %q not found", mods.ModNatsJetStream)
	}
	if failure, ok := app.Lookup[*app.RuntimeFailure](registry, mods.ModRuntimeFailure); ok {
		m.config.Failure = failure
	}
	asm, err := coresaga.Assemble(mongoClient, authority, jetStream, m.config, m.definitions...)
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

func (m *Mod) Start() error {
	if m == nil || m.asm == nil {
		return fmt.Errorf("saga mod: not provided")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

func (m *Mod) Engine() *coresaga.Engine {
	if m == nil || m.asm == nil {
		return nil
	}
	return m.asm.Engine
}

// Store 返回协调器的权威存储（Redis）；Mongo 冷副本经 Store().Copy() 取。
func (m *Mod) Store() *coresaga.RedisStore {
	if m == nil || m.asm == nil {
		return nil
	}
	return m.asm.Store
}
func (m *Mod) Transport() *coresaga.JetStreamPublisher {
	if m == nil || m.asm == nil {
		return nil
	}
	return m.asm.Transport
}

func (m *Mod) checkHealth(ctx context.Context) health.Result {
	if m == nil || m.asm == nil || !m.asm.Running() {
		return health.Result{Status: health.StatusFail, Message: "not initialized"}
	}
	if m.asm.ConsumersClosed() {
		return health.Result{Status: health.StatusFail, Message: "durable consumer stopped"}
	}
	if err := m.asm.RunError(); err != nil {
		return health.Result{Status: health.StatusFail, Message: "worker stopped", Err: err}
	}
	if err := m.asm.Store.Ping(ctx); err != nil {
		return health.Result{Status: health.StatusFail, Message: "authority Redis or MongoDB copy unavailable", Err: err}
	}
	stats := m.asm.Engine.Stats()
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("running conflicts=%d duplicates=%d publish_failures=%d store_failures=%d worker_failures=%d manual_required=%d late_after_abandon=%d reopened=%d", stats.Conflicts, stats.Duplicates, stats.PublishFailures, stats.StoreFailures, stats.WorkerFailures, stats.ManualRequired, stats.LateAfterAbandon, stats.Reopened)}
}

func stringDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var _ app.Mod = (*Mod)(nil)
var _ app.ModOptionalDependencyProvider = (*Mod)(nil)

// StreamSettings 按 saga Mod 的声明（整份 saga.*，与 Mod Init 读的同一个结构体）返回步骤消费者与协调器会合要用的
// 三样：命令主题前缀、saga 流名与 saga 库名。步骤运行时 Steps 用它；业务自己组装步骤消费者时同样经它，不直接读
// viper、不另抄一份缺省值。
func StreamSettings(cfg *viper.Viper) (subjectPrefix, stream, database string, err error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", "", "", fmt.Errorf("saga: %w", err)
	}
	return settings.Saga.SubjectPrefix, settings.Saga.Stream, settings.Saga.Database, nil
}
