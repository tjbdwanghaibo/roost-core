package saga

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/health"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/kit/dataengine"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
)

// Mod parses configuration, looks up the Mongo and JetStream capabilities,
// hands them to core's saga.Assemble and forwards lifecycle calls. Consumer
// subscription, engine loop and drain-then-stop live in core (P3b).
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

// DependsOn names Mods: Mongo and NATS. JetStream is the NATS Mod's
// capability and health is a registry built-in — neither is a Mod name, and
// app resolves dependencies by Mod name (roost-codegen U-0025).
func (m *Mod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModMongo, mods.ModNats}
}

func (m *Mod) OptionalDependsOn() []app.ModName {
	return []app.ModName{mods.ModDataEngine}
}

func (m *Mod) Init(cfg *viper.Viper) error {
	if cfg == nil {
		cfg = viper.New()
	}
	defaults := coresaga.DefaultOptions()
	// 严格读取（维护者决定 A4）：写错类型的值不再被读成 0 / 纳秒后取默认，返回前一并报出。
	read := app.NewConfigReader(cfg)
	owner := cfg.GetString("saga.owner")
	if owner == "" {
		owner = fmt.Sprintf("saga-%d-%s", cfg.GetInt32("sid"), coresaga.NewID())
	}
	prefix := cfg.GetString("saga.subject_prefix")
	if prefix == "" {
		prefix = "roost.saga"
	}
	stream := cfg.GetString("saga.stream")
	if stream == "" {
		stream = "ROOST_SAGA"
	}
	durable := cfg.GetString("saga.result_durable")
	if durable == "" {
		durable = "roost-saga-coordinator"
	}
	m.config = coresaga.AssemblyConfig{
		Store:  coresaga.MongoStoreOptions{Database: stringDefault(cfg.GetString("saga.database"), "saga"), SagaCollection: cfg.GetString("saga.collections.sagas"), OutboxCollection: cfg.GetString("saga.collections.outbox"), CompletionCollection: cfg.GetString("saga.collections.completions"), OperationCollection: cfg.GetString("saga.collections.operations"), CompletionReceiptTTL: durationDefault(read.Duration("saga.completion_receipt_ttl"), 30*24*time.Hour)},
		Engine: coresaga.Options{Owner: owner, CoordinatorWorkers: intDefault(read.Int("saga.coordinator_workers"), defaults.CoordinatorWorkers), PublisherWorkers: intDefault(read.Int("saga.publisher_workers"), defaults.PublisherWorkers), CoordinatorBatch: intDefault(read.Int("saga.coordinator_claim_batch"), defaults.CoordinatorBatch), PublisherBatch: intDefault(read.Int("saga.publisher_claim_batch"), defaults.PublisherBatch), LeaseDuration: durationDefault(read.Duration("saga.lease_duration"), defaults.LeaseDuration), StoreTimeout: durationDefault(read.Duration("saga.store_timeout"), defaults.StoreTimeout), PollInterval: durationDefault(read.Duration("saga.poll_interval"), defaults.PollInterval), PublishTimeout: durationDefault(read.Duration("saga.publish_timeout"), defaults.PublishTimeout), PublishBackoffMin: durationDefault(read.Duration("saga.publish_backoff_min"), defaults.PublishBackoffMin), PublishBackoffMax: durationDefault(read.Duration("saga.publish_backoff_max"), defaults.PublishBackoffMax), MaxPayloadBytes: intDefault(read.Int("saga.max_payload_bytes"), defaults.MaxPayloadBytes)},
		Prefix: prefix,
		Stream: fnats.JetStreamConfig{Name: stream, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile, MaxAge: durationDefault(read.Duration("saga.stream_max_age"), 7*24*time.Hour), Duplicates: durationDefault(read.Duration("saga.duplicate_window"), 10*time.Minute), Replicas: intDefault(read.Int("saga.replicas"), 1), MaxBytes: int64Default(read.Int64("saga.stream_max_bytes"), 8<<30)},
		Completions: coresaga.CompletionConsumerConfig{
			Stream: stream, Durable: durable, SubjectPrefix: prefix,
			AckWait:        durationDefault(read.Duration("saga.result_ack_wait"), 30*time.Second),
			ProcessTimeout: durationDefault(read.Duration("saga.result_process_timeout"), defaults.StoreTimeout),
			MaxDeliver:     intDefault(read.Int("saga.result_max_deliver"), 25_000),
			MaxAckPending:  intDefault(read.Int("saga.result_max_ack_pending"), 256),
			NakBackoffMin:  durationDefault(read.Duration("saga.result_nak_backoff_min"), 250*time.Millisecond),
			NakBackoffMax:  durationDefault(read.Duration("saga.result_nak_backoff_max"), 30*time.Second),
		},
		// Native Nest steps commit their completion as an effect, so it
		// arrives on the effect stream rather than the saga stream. Its
		// consumer shares the start consumer's stream and prefix and takes
		// its own durable; core derives the rest when these are empty
		// (RR-20260917-07).
		NestResults: coresaga.NestCompletionConsumerConfig{
			Stream:         stringDefault(cfg.GetString("saga.result_effect_stream"), stringDefault(cfg.GetString("saga.start_effect_stream"), "ROOST_EFFECTS")),
			Durable:        cfg.GetString("saga.result_effect_durable"),
			EffectPrefix:   stringDefault(cfg.GetString("saga.result_effect_prefix"), stringDefault(cfg.GetString("saga.start_effect_prefix"), "roost.effect")),
			AckWait:        durationDefault(read.Duration("saga.result_effect_ack_wait"), 30*time.Second),
			ProcessTimeout: durationDefault(read.Duration("saga.result_effect_process_timeout"), defaults.StoreTimeout),
			MaxDeliver:     intDefault(read.Int("saga.result_effect_max_deliver"), 25_000),
			MaxAckPending:  intDefault(read.Int("saga.result_effect_max_ack_pending"), 256),
			NakBackoffMin:  durationDefault(read.Duration("saga.result_effect_nak_backoff_min"), 250*time.Millisecond),
			NakBackoffMax:  durationDefault(read.Duration("saga.result_effect_nak_backoff_max"), 30*time.Second),
		},
		Starts: coresaga.NestStartConsumerConfig{
			Stream:         stringDefault(cfg.GetString("saga.start_effect_stream"), "ROOST_EFFECTS"),
			Durable:        stringDefault(cfg.GetString("saga.start_effect_durable"), "roost-saga-start"),
			EffectPrefix:   stringDefault(cfg.GetString("saga.start_effect_prefix"), "roost.effect"),
			AckWait:        durationDefault(read.Duration("saga.start_effect_ack_wait"), 30*time.Second),
			ProcessTimeout: durationDefault(read.Duration("saga.start_effect_process_timeout"), defaults.StoreTimeout),
			MaxDeliver:     intDefault(read.Int("saga.start_effect_max_deliver"), 25_000),
			MaxAckPending:  intDefault(read.Int("saga.start_effect_max_ack_pending"), 256),
			NakBackoffMin:  durationDefault(read.Duration("saga.start_effect_nak_backoff_min"), 250*time.Millisecond),
			NakBackoffMax:  durationDefault(read.Duration("saga.start_effect_nak_backoff_max"), 30*time.Second),
		},
	}
	if err := read.Err(); err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	budgets, err := StepBudgetsFromConfig(cfg, m.definitions...)
	if err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	m.config.Engine.StepBudgets = budgets
	if m.config.Store.CompletionReceiptTTL <= m.config.Stream.MaxAge {
		return fmt.Errorf("saga: completion receipt ttl must exceed stream max age")
	}
	return m.checkEffectRetention(cfg)
}

// checkEffectRetention 是 O-S5-2 的跨 Mod 校验（维护者第十二轮决定）：原生 Nest 步骤的完成结果在
// DataEngine 的效果流上，回执必须比流里的结果活得久，否则回执过期后再投递的结果协调器分不清是
// 已计入的重复还是放弃后生效的成功，只能 Term。saga 流的同一前提上面已经校验；这里只在结果效果流
// 就是 DataEngine 效果流（按 DataEngine Mod 的读法解析的流名）时比较，别的流的保留期不归这份配置管。
func (m *Mod) checkEffectRetention(cfg *viper.Viper) error {
	effectStream, effectMaxAge, err := kitdataengine.EffectStreamRetention(cfg)
	if err != nil {
		return fmt.Errorf("saga: %w", err)
	}
	if strings.TrimSpace(m.config.NestResults.Stream) != effectStream {
		return nil
	}
	if ttl := m.config.Store.CompletionReceiptTTL; ttl <= effectMaxAge {
		return fmt.Errorf("saga: saga.completion_receipt_ttl (%s) must exceed dataengine.effects.max_age (%s): "+
			"native step results are kept on the effect stream %s that long, and a result redelivered after its "+
			"completion receipt expired can no longer be told from a late success and is terminated; "+
			"raise saga.completion_receipt_ttl or lower dataengine.effects.max_age",
			ttl, effectMaxAge, effectStream)
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
	jetStream, ok := app.Lookup[fnats.IJetStream](registry, mods.ModNatsJetStream)
	if !ok || jetStream == nil {
		return fmt.Errorf("saga mod: capability %q not found", mods.ModNatsJetStream)
	}
	asm, err := coresaga.Assemble(mongoClient, jetStream, m.config, m.definitions...)
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
func (m *Mod) Store() *coresaga.MongoStore {
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
		return health.Result{Status: health.StatusFail, Message: "MongoDB unavailable", Err: err}
	}
	stats := m.asm.Engine.Stats()
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("running conflicts=%d duplicates=%d publish_failures=%d store_failures=%d worker_failures=%d manual_required=%d late_after_abandon=%d", stats.Conflicts, stats.Duplicates, stats.PublishFailures, stats.StoreFailures, stats.WorkerFailures, stats.ManualRequired, stats.LateAfterAbandon)}
}

func durationDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}
func intDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}
func int64Default(value, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	return value
}
func stringDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

var _ app.Mod = (*Mod)(nil)
var _ app.ModOptionalDependencyProvider = (*Mod)(nil)
