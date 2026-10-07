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
	resultStream := stringDefault(c.ResultEffectStream, c.StartEffectStream)
	resultPrefix := stringDefault(c.ResultEffectPrefix, c.StartEffectPrefix)
	m.config = coresaga.AssemblyConfig{
		Store: coresaga.MongoStoreOptions{Database: c.Database, SagaCollection: c.Collections.Sagas, OutboxCollection: c.Collections.Outbox,
			CompletionCollection: c.Collections.Completions, OperationCollection: c.Collections.Operations, CompletionReceiptTTL: c.CompletionReceiptTTL},
		Engine: coresaga.Options{Owner: owner, CoordinatorWorkers: c.CoordinatorWorkers, PublisherWorkers: c.PublisherWorkers,
			CoordinatorBatch: c.CoordinatorClaimBatch, PublisherBatch: c.PublisherClaimBatch, LeaseDuration: c.LeaseDuration,
			StoreTimeout: c.StoreTimeout, PollInterval: c.PollInterval, PublishTimeout: c.PublishTimeout,
			PublishBackoffMin: c.PublishBackoffMin, PublishBackoffMax: c.PublishBackoffMax, MaxPayloadBytes: c.MaxPayloadBytes,
			StepBudgets: defaults.StepBudgets},
		Prefix: c.SubjectPrefix,
		Stream: fnats.JetStreamConfig{Name: c.Stream, Subjects: []string{c.SubjectPrefix + ".>"}, Storage: fnats.JetStreamStorageFile,
			MaxAge: c.StreamMaxAge, Duplicates: c.DuplicateWindow, Replicas: c.Replicas, MaxBytes: c.StreamMaxBytes},
		Completions: coresaga.CompletionConsumerConfig{
			Stream: c.Stream, Durable: c.ResultDurable, SubjectPrefix: c.SubjectPrefix,
			AckWait: c.Result.AckWait, ProcessTimeout: c.Result.ProcessTimeout, MaxDeliver: c.Result.MaxDeliver,
			MaxAckPending: c.Result.MaxAckPending, NakBackoffMin: c.Result.NakBackoffMin, NakBackoffMax: c.Result.NakBackoffMax,
		},
		// Native Nest steps commit their completion as an effect, so it
		// arrives on the effect stream rather than the saga stream. Its
		// consumer shares the start consumer's stream and prefix and takes
		// its own durable; core derives the rest when these are empty
		// (RR-20260917-07).
		NestResults: coresaga.NestCompletionConsumerConfig{
			Stream: resultStream, Durable: c.ResultEffectDurable, EffectPrefix: resultPrefix,
			AckWait: c.ResultEffect.AckWait, ProcessTimeout: c.ResultEffect.ProcessTimeout, MaxDeliver: c.ResultEffect.MaxDeliver,
			MaxAckPending: c.ResultEffect.MaxAckPending, NakBackoffMin: c.ResultEffect.NakBackoffMin, NakBackoffMax: c.ResultEffect.NakBackoffMax,
		},
		Starts: coresaga.NestStartConsumerConfig{
			Stream: c.StartEffectStream, Durable: c.StartEffectDurable, EffectPrefix: c.StartEffectPrefix,
			AckWait: c.StartEffect.AckWait, ProcessTimeout: c.StartEffect.ProcessTimeout, MaxDeliver: c.StartEffect.MaxDeliver,
			MaxAckPending: c.StartEffect.MaxAckPending, NakBackoffMin: c.StartEffect.NakBackoffMin, NakBackoffMax: c.StartEffect.NakBackoffMax,
		},
	}
	budgets, err := stepBudgets(c.StepDefaults, c.Steps, m.definitions)
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
// 三样：命令主题前缀、saga 流名与 saga 库名。业务的步骤消费者（game-demo 的赠礼 saga）用它，不直接读 viper、
// 不另抄一份缺省值（A4 ① 收尾）。
func StreamSettings(cfg *viper.Viper) (subjectPrefix, stream, database string, err error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return "", "", "", fmt.Errorf("saga: %w", err)
	}
	return settings.Saga.SubjectPrefix, settings.Saga.Stream, settings.Saga.Database, nil
}
