package saga

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/health"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
	"github.com/tjbdwanghaibo/roost-kit/mods"
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
		Store:  coresaga.MongoStoreOptions{Database: stringDefault(cfg.GetString("saga.database"), "saga"), SagaCollection: cfg.GetString("saga.collections.sagas"), OutboxCollection: cfg.GetString("saga.collections.outbox"), CompletionCollection: cfg.GetString("saga.collections.completions"), OperationCollection: cfg.GetString("saga.collections.operations"), CompletionReceiptTTL: durationDefault(cfg.GetDuration("saga.completion_receipt_ttl"), 30*24*time.Hour)},
		Engine: coresaga.Options{Owner: owner, CoordinatorWorkers: intDefault(cfg.GetInt("saga.coordinator_workers"), defaults.CoordinatorWorkers), PublisherWorkers: intDefault(cfg.GetInt("saga.publisher_workers"), defaults.PublisherWorkers), CoordinatorBatch: intDefault(cfg.GetInt("saga.coordinator_claim_batch"), defaults.CoordinatorBatch), PublisherBatch: intDefault(cfg.GetInt("saga.publisher_claim_batch"), defaults.PublisherBatch), LeaseDuration: durationDefault(cfg.GetDuration("saga.lease_duration"), defaults.LeaseDuration), StoreTimeout: durationDefault(cfg.GetDuration("saga.store_timeout"), defaults.StoreTimeout), PollInterval: durationDefault(cfg.GetDuration("saga.poll_interval"), defaults.PollInterval), PublishTimeout: durationDefault(cfg.GetDuration("saga.publish_timeout"), defaults.PublishTimeout), PublishBackoffMin: durationDefault(cfg.GetDuration("saga.publish_backoff_min"), defaults.PublishBackoffMin), PublishBackoffMax: durationDefault(cfg.GetDuration("saga.publish_backoff_max"), defaults.PublishBackoffMax), MaxPayloadBytes: intDefault(cfg.GetInt("saga.max_payload_bytes"), defaults.MaxPayloadBytes)},
		Prefix: prefix,
		Stream: fnats.JetStreamConfig{Name: stream, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile, MaxAge: durationDefault(cfg.GetDuration("saga.stream_max_age"), 7*24*time.Hour), Duplicates: durationDefault(cfg.GetDuration("saga.duplicate_window"), 10*time.Minute), Replicas: intDefault(cfg.GetInt("saga.replicas"), 1), MaxBytes: int64Default(cfg.GetInt64("saga.stream_max_bytes"), 8<<30)},
		Completions: coresaga.CompletionConsumerConfig{
			Stream: stream, Durable: durable, SubjectPrefix: prefix,
			AckWait:        durationDefault(cfg.GetDuration("saga.result_ack_wait"), 30*time.Second),
			ProcessTimeout: durationDefault(cfg.GetDuration("saga.result_process_timeout"), defaults.StoreTimeout),
			MaxDeliver:     intDefault(cfg.GetInt("saga.result_max_deliver"), 25_000),
			MaxAckPending:  intDefault(cfg.GetInt("saga.result_max_ack_pending"), 256),
			NakBackoffMin:  durationDefault(cfg.GetDuration("saga.result_nak_backoff_min"), 250*time.Millisecond),
			NakBackoffMax:  durationDefault(cfg.GetDuration("saga.result_nak_backoff_max"), 30*time.Second),
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
			AckWait:        durationDefault(cfg.GetDuration("saga.result_effect_ack_wait"), 30*time.Second),
			ProcessTimeout: durationDefault(cfg.GetDuration("saga.result_effect_process_timeout"), defaults.StoreTimeout),
			MaxDeliver:     intDefault(cfg.GetInt("saga.result_effect_max_deliver"), 25_000),
			MaxAckPending:  intDefault(cfg.GetInt("saga.result_effect_max_ack_pending"), 256),
			NakBackoffMin:  durationDefault(cfg.GetDuration("saga.result_effect_nak_backoff_min"), 250*time.Millisecond),
			NakBackoffMax:  durationDefault(cfg.GetDuration("saga.result_effect_nak_backoff_max"), 30*time.Second),
		},
		Starts: coresaga.NestStartConsumerConfig{
			Stream:         stringDefault(cfg.GetString("saga.start_effect_stream"), "ROOST_EFFECTS"),
			Durable:        stringDefault(cfg.GetString("saga.start_effect_durable"), "roost-saga-start"),
			EffectPrefix:   stringDefault(cfg.GetString("saga.start_effect_prefix"), "roost.effect"),
			AckWait:        durationDefault(cfg.GetDuration("saga.start_effect_ack_wait"), 30*time.Second),
			ProcessTimeout: durationDefault(cfg.GetDuration("saga.start_effect_process_timeout"), defaults.StoreTimeout),
			MaxDeliver:     intDefault(cfg.GetInt("saga.start_effect_max_deliver"), 25_000),
			MaxAckPending:  intDefault(cfg.GetInt("saga.start_effect_max_ack_pending"), 256),
			NakBackoffMin:  durationDefault(cfg.GetDuration("saga.start_effect_nak_backoff_min"), 250*time.Millisecond),
			NakBackoffMax:  durationDefault(cfg.GetDuration("saga.start_effect_nak_backoff_max"), 30*time.Second),
		},
	}
	if m.config.Store.CompletionReceiptTTL <= m.config.Stream.MaxAge {
		return fmt.Errorf("saga: completion receipt ttl must exceed stream max age")
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
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("running conflicts=%d duplicates=%d publish_failures=%d store_failures=%d worker_failures=%d manual_required=%d", stats.Conflicts, stats.Duplicates, stats.PublishFailures, stats.StoreFailures, stats.WorkerFailures, stats.ManualRequired)}
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
