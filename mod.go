package saga

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/cube-core/app"
	"github.com/tjbdwanghaibo/cube-core/health"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	coresaga "github.com/tjbdwanghaibo/cube-core/saga"
	"github.com/tjbdwanghaibo/cube-kit/mods"
)

type Mod struct {
	definitions []coresaga.Definition
	config      modConfig
	store       *MongoStore
	engine      *coresaga.Engine
	transport   *JetStreamPublisher
	resultSub   fnats.IJetStreamSubscription
	startSub    fnats.IJetStreamSubscription
	cancel      context.CancelFunc
	done        chan struct{}
	errMu       sync.RWMutex
	runErr      error
	running     atomic.Bool
}

type modConfig struct {
	store          MongoStoreOptions
	engine         coresaga.Options
	prefix         string
	stream         fnats.JetStreamConfig
	durable        string
	ackWait        time.Duration
	processTimeout time.Duration
	maxDeliver     int
	maxPending     int
	nakMin         time.Duration
	nakMax         time.Duration
	start          NestStartConsumerConfig
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
func (m *Mod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModMongo, mods.ModNatsJetStream, mods.ModHealth, mods.ModNestWAL}
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
	m.config = modConfig{
		store:  MongoStoreOptions{Database: stringDefault(cfg.GetString("saga.database"), "saga"), SagaCollection: cfg.GetString("saga.collections.sagas"), OutboxCollection: cfg.GetString("saga.collections.outbox"), CompletionCollection: cfg.GetString("saga.collections.completions"), OperationCollection: cfg.GetString("saga.collections.operations"), CompletionReceiptTTL: durationDefault(cfg.GetDuration("saga.completion_receipt_ttl"), 30*24*time.Hour)},
		engine: coresaga.Options{Owner: owner, CoordinatorWorkers: intDefault(cfg.GetInt("saga.coordinator_workers"), defaults.CoordinatorWorkers), PublisherWorkers: intDefault(cfg.GetInt("saga.publisher_workers"), defaults.PublisherWorkers), CoordinatorBatch: intDefault(cfg.GetInt("saga.coordinator_claim_batch"), defaults.CoordinatorBatch), PublisherBatch: intDefault(cfg.GetInt("saga.publisher_claim_batch"), defaults.PublisherBatch), LeaseDuration: durationDefault(cfg.GetDuration("saga.lease_duration"), defaults.LeaseDuration), StoreTimeout: durationDefault(cfg.GetDuration("saga.store_timeout"), defaults.StoreTimeout), PollInterval: durationDefault(cfg.GetDuration("saga.poll_interval"), defaults.PollInterval), PublishTimeout: durationDefault(cfg.GetDuration("saga.publish_timeout"), defaults.PublishTimeout), PublishBackoffMin: durationDefault(cfg.GetDuration("saga.publish_backoff_min"), defaults.PublishBackoffMin), PublishBackoffMax: durationDefault(cfg.GetDuration("saga.publish_backoff_max"), defaults.PublishBackoffMax), MaxPayloadBytes: intDefault(cfg.GetInt("saga.max_payload_bytes"), defaults.MaxPayloadBytes)},
		prefix: prefix, durable: durable, ackWait: durationDefault(cfg.GetDuration("saga.result_ack_wait"), 30*time.Second), processTimeout: durationDefault(cfg.GetDuration("saga.result_process_timeout"), defaults.StoreTimeout), maxDeliver: intDefault(cfg.GetInt("saga.result_max_deliver"), 25_000), maxPending: intDefault(cfg.GetInt("saga.result_max_ack_pending"), 256), nakMin: durationDefault(cfg.GetDuration("saga.result_nak_backoff_min"), 250*time.Millisecond), nakMax: durationDefault(cfg.GetDuration("saga.result_nak_backoff_max"), 30*time.Second),
		start: NestStartConsumerConfig{
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
		stream: fnats.JetStreamConfig{Name: stream, Subjects: []string{prefix + ".>"}, Storage: fnats.JetStreamStorageFile, MaxAge: durationDefault(cfg.GetDuration("saga.stream_max_age"), 7*24*time.Hour), Duplicates: durationDefault(cfg.GetDuration("saga.duplicate_window"), 10*time.Minute), Replicas: intDefault(cfg.GetInt("saga.replicas"), 1), MaxBytes: int64Default(cfg.GetInt64("saga.stream_max_bytes"), 8<<30)},
	}
	if m.config.store.CompletionReceiptTTL <= m.config.stream.MaxAge {
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
	store, err := NewMongoStore(mongoClient, m.config.store)
	if err != nil {
		return err
	}
	transport, err := NewJetStreamPublisher(jetStream, m.config.prefix)
	if err != nil {
		return err
	}
	engine, err := coresaga.NewEngine(store, transport, m.config.engine)
	if err != nil {
		return err
	}
	for i := range m.definitions {
		if err := engine.Register(m.definitions[i]); err != nil {
			return fmt.Errorf("saga mod: register definition: %w", err)
		}
	}
	m.store, m.transport, m.engine = store, transport, engine
	if err := registry.Register(mods.ModSaga, engine); err != nil {
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
	if m.engine == nil || m.store == nil || m.transport == nil {
		return fmt.Errorf("saga mod: not provided")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.store.EnsureInfrastructure(ctx); err != nil {
		return err
	}
	jetStream := m.transport.client
	if err := jetStream.EnsureStream(ctx, m.config.stream); err != nil {
		return fmt.Errorf("saga mod: ensure stream: %w", err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	sub, err := SubscribeCompletions(runCtx, jetStream, CompletionConsumerConfig{Stream: m.config.stream.Name, Durable: m.config.durable, SubjectPrefix: m.config.prefix, AckWait: m.config.ackWait, ProcessTimeout: m.config.processTimeout, MaxDeliver: m.config.maxDeliver, MaxAckPending: m.config.maxPending, NakBackoffMin: m.config.nakMin, NakBackoffMax: m.config.nakMax}, m.engine)
	if err != nil {
		runCancel()
		return fmt.Errorf("saga mod: subscribe completions: %w", err)
	}
	startSub, err := SubscribeNestStarts(runCtx, jetStream, m.config.start, m.engine)
	if err != nil {
		sub.Drain()
		runCancel()
		return fmt.Errorf("saga mod: subscribe Nest starts: %w", err)
	}
	m.cancel, m.resultSub, m.startSub, m.done = runCancel, sub, startSub, make(chan struct{})
	m.running.Store(true)
	go func() {
		defer m.running.Store(false)
		defer close(m.done)
		err := m.engine.Run(runCtx)
		if err != nil && runCtx.Err() == nil {
			m.errMu.Lock()
			m.runErr = err
			m.errMu.Unlock()
		}
	}()
	return nil
}

func (m *Mod) Stop() { _ = m.StopWithContext(context.Background()) }
func (m *Mod) StopWithContext(ctx context.Context) error {
	if m.resultSub != nil {
		m.resultSub.Drain()
		m.resultSub = nil
	}
	if m.startSub != nil {
		m.startSub.Drain()
		m.startSub = nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	if m.engine != nil {
		_ = m.engine.Stop(ctx)
	}
	if m.done != nil {
		select {
		case <-m.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (m *Mod) Engine() *coresaga.Engine       { return m.engine }
func (m *Mod) Store() *MongoStore             { return m.store }
func (m *Mod) Transport() *JetStreamPublisher { return m.transport }
func (m *Mod) checkHealth(ctx context.Context) health.Result {
	if m.engine == nil || !m.running.Load() {
		return health.Result{Status: health.StatusFail, Message: "not initialized"}
	}
	if subscriptionClosed(m.resultSub) || subscriptionClosed(m.startSub) {
		return health.Result{Status: health.StatusFail, Message: "durable consumer stopped"}
	}
	m.errMu.RLock()
	err := m.runErr
	m.errMu.RUnlock()
	if err != nil {
		return health.Result{Status: health.StatusFail, Message: "worker stopped", Err: err}
	}
	if err := m.store.Ping(ctx); err != nil {
		return health.Result{Status: health.StatusFail, Message: "MongoDB unavailable", Err: err}
	}
	stats := m.engine.Stats()
	return health.Result{Status: health.StatusOK, Message: fmt.Sprintf("running conflicts=%d duplicates=%d publish_failures=%d store_failures=%d worker_failures=%d manual_required=%d", stats.Conflicts, stats.Duplicates, stats.PublishFailures, stats.StoreFailures, stats.WorkerFailures, stats.ManualRequired)}
}

func subscriptionClosed(subscription fnats.IJetStreamSubscription) bool {
	if subscription == nil {
		return true
	}
	select {
	case <-subscription.Closed():
		return true
	default:
		return false
	}
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
