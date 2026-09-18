package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// AssemblyConfig is everything the Saga runtime needs once configuration has
// been parsed: store and engine options, the wire prefix and stream, and the
// two durable consumers (completions from step workers, starts from Nest
// effects). The kit Mod fills it from viper.
type AssemblyConfig struct {
	Store       MongoStoreOptions
	Engine      Options
	Prefix      string
	Stream      fnats.JetStreamConfig
	Completions CompletionConsumerConfig
	Starts      NestStartConsumerConfig
	// NestResults is the consumer for native Nest steps' completion effects.
	// Left zero it is derived from Starts (same effect stream and prefix,
	// its own durable), because a deployment that has native steps and a
	// deployment that does not are configured the same way — the effects
	// simply never arrive in the second one (RR-20260917-07).
	NestResults NestCompletionConsumerConfig
}

// nestResultsConfig fills NestResults from Starts where it is empty, so an
// AssemblyConfig written before this consumer existed still gets it.
func (c AssemblyConfig) nestResultsConfig() NestCompletionConsumerConfig {
	config := c.NestResults
	if config.Stream == "" {
		config.Stream = c.Starts.Stream
	}
	if config.EffectPrefix == "" {
		config.EffectPrefix = c.Starts.EffectPrefix
	}
	if config.Durable == "" {
		config.Durable = defaultNestResultDurable(c.Starts.Durable)
	}
	if config.AckWait == 0 {
		config.AckWait = c.Starts.AckWait
	}
	if config.ProcessTimeout == 0 {
		config.ProcessTimeout = c.Starts.ProcessTimeout
	}
	if config.MaxDeliver == 0 {
		config.MaxDeliver = c.Starts.MaxDeliver
	}
	if config.MaxAckPending == 0 {
		config.MaxAckPending = c.Starts.MaxAckPending
	}
	if config.NakBackoffMin == 0 {
		config.NakBackoffMin = c.Starts.NakBackoffMin
	}
	if config.NakBackoffMax == 0 {
		config.NakBackoffMax = c.Starts.NakBackoffMax
	}
	return config
}

// defaultNestResultDurable derives a durable that cannot collide with the
// start consumer's: two consumers sharing one durable share one cursor, and
// each message would reach only one of them.
func defaultNestResultDurable(startDurable string) string {
	if startDurable == "" {
		return "roost-saga-nest-result"
	}
	return startDurable + "-result"
}

// Assembly owns the Saga runtime's construction and lifecycle: store,
// transport and engine at Provide time; infrastructure, the two durable
// consumers and the engine loop at Start; drain-then-stop at Stop. The kit
// Mod only parses configuration, publishes Engine and forwards calls (P3b).
type Assembly struct {
	Store     *MongoStore
	Transport *JetStreamPublisher
	Engine    *Engine

	cfg AssemblyConfig

	lifecycleMu sync.Mutex
	stateMu     sync.RWMutex
	resultSub   fnats.IJetStreamSubscription
	startSub    fnats.IJetStreamSubscription
	nestResults fnats.IJetStreamSubscription
	cancel      context.CancelFunc
	done        chan struct{}
	errMu       sync.RWMutex
	runErr      error
	running     atomic.Bool
}

// Assemble builds store, transport and engine and registers the definitions.
// Nothing touches Mongo or JetStream yet.
func Assemble(mongo fmongo.IMongo, jetStream fnats.IJetStream, cfg AssemblyConfig, definitions ...Definition) (*Assembly, error) {
	if mongo == nil {
		return nil, errors.New("saga: mongo client is required")
	}
	if jetStream == nil {
		return nil, errors.New("saga: jetstream client is required")
	}
	store, err := NewMongoStore(mongo, cfg.Store)
	if err != nil {
		return nil, err
	}
	transport, err := NewJetStreamPublisher(jetStream, cfg.Prefix)
	if err != nil {
		return nil, err
	}
	engine, err := NewEngine(store, transport, cfg.Engine)
	if err != nil {
		return nil, err
	}
	for i := range definitions {
		if err := engine.Register(definitions[i]); err != nil {
			return nil, fmt.Errorf("saga: register definition: %w", err)
		}
	}
	return &Assembly{Store: store, Transport: transport, Engine: engine, cfg: cfg}, nil
}

// Start ensures the Mongo infrastructure and the stream, subscribes the
// completion and Nest-start consumers and runs the engine loop in the
// background. ctx bounds the infrastructure calls only; the loop runs until
// Stop. A second Start while running is a no-op.
func (a *Assembly) Start(ctx context.Context) error {
	if a == nil || a.Engine == nil || a.Store == nil || a.Transport == nil {
		return errors.New("saga: not assembled")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.running.Load() {
		return nil
	}
	if err := a.Store.EnsureInfrastructure(ctx); err != nil {
		return err
	}
	jetStream := a.Transport.Client()
	if err := jetStream.EnsureStream(ctx, a.cfg.Stream); err != nil {
		return fmt.Errorf("saga: ensure stream: %w", err)
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	resultSub, err := SubscribeCompletions(runCtx, jetStream, a.cfg.Completions, a.Engine)
	if err != nil {
		runCancel()
		return fmt.Errorf("saga: subscribe completions: %w", err)
	}
	startSub, err := SubscribeNestStarts(runCtx, jetStream, a.cfg.Starts, a.Engine)
	if err != nil {
		resultSub.Drain()
		runCancel()
		return fmt.Errorf("saga: subscribe Nest starts: %w", err)
	}
	// The third consumer: completions a native Nest step committed as an
	// effect. Without it those results reach nobody and the saga waits out
	// every timeout (RR-20260917-07). A failure here tears down the two that
	// already started, like every other partial start.
	nestResultSub, err := SubscribeNestCompletions(runCtx, jetStream, a.cfg.nestResultsConfig(), a.Engine)
	if err != nil {
		startSub.Drain()
		resultSub.Drain()
		runCancel()
		return fmt.Errorf("saga: subscribe Nest completions: %w", err)
	}
	done := make(chan struct{})
	a.stateMu.Lock()
	a.cancel, a.resultSub, a.startSub, a.nestResults, a.done = runCancel, resultSub, startSub, nestResultSub, done
	a.stateMu.Unlock()
	a.errMu.Lock()
	a.runErr = nil
	a.errMu.Unlock()
	a.running.Store(true)
	go func() {
		defer a.running.Store(false)
		defer close(done)
		err := a.Engine.Run(runCtx)
		if err != nil && runCtx.Err() == nil {
			a.errMu.Lock()
			a.runErr = err
			a.errMu.Unlock()
		}
	}()
	return nil
}

// Stop drains both consumers (waiting for their closure within ctx), cancels
// the loop, stops the engine and waits for the loop to exit. When the drain
// does not finish in time the consumers are stopped hard and ctx's error is
// returned.
func (a *Assembly) Stop(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.stateMu.RLock()
	resultSub, startSub, nestResultSub, runCancel, done := a.resultSub, a.startSub, a.nestResults, a.cancel, a.done
	a.stateMu.RUnlock()
	subs := []fnats.IJetStreamSubscription{resultSub, startSub, nestResultSub}
	if err := drainSubscriptions(ctx, subs); err != nil {
		for _, sub := range subs {
			if sub != nil {
				sub.Stop()
			}
		}
		if runCancel != nil {
			runCancel()
		}
		return err
	}
	a.stateMu.Lock()
	a.resultSub, a.startSub, a.nestResults, a.cancel = nil, nil, nil, nil
	a.stateMu.Unlock()
	if runCancel != nil {
		runCancel()
	}
	if a.Engine != nil {
		if err := a.Engine.Stop(ctx); err != nil {
			return err
		}
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Running reports whether the engine loop is up.
func (a *Assembly) Running() bool { return a != nil && a.running.Load() }

// ConsumersClosed reports whether either durable consumer has stopped —
// a running loop with a dead consumer is not healthy.
func (a *Assembly) ConsumersClosed() bool {
	if a == nil {
		return true
	}
	a.stateMu.RLock()
	resultSub, startSub := a.resultSub, a.startSub
	a.stateMu.RUnlock()
	return subscriptionClosed(resultSub) || subscriptionClosed(startSub)
}

// RunError is the error the engine loop exited with, if it exited on its own.
func (a *Assembly) RunError() error {
	if a == nil {
		return nil
	}
	a.errMu.RLock()
	defer a.errMu.RUnlock()
	return a.runErr
}

func drainSubscriptions(ctx context.Context, subscriptions []fnats.IJetStreamSubscription) error {
	for _, sub := range subscriptions {
		if sub != nil {
			sub.Drain()
		}
	}
	for _, sub := range subscriptions {
		if sub == nil {
			continue
		}
		select {
		case <-sub.Closed():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
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
