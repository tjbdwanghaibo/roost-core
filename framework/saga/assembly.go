package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
)

// AssemblyConfig 是配置解析后 saga 运行时需要的全部内容：存储与引擎选项、线上前缀与流、两个持久消费者（步骤执行方的
// 结果、Nest effect 的启动）。由 wiring 的 Mod 填写。
type AssemblyConfig struct {
	Store       MongoStoreOptions
	Engine      Options
	Prefix      string
	Stream      fnats.JetStreamConfig
	Completions CompletionConsumerConfig
	Starts      NestStartConsumerConfig
	// NestResults 是原生 Nest 步骤 completion effect 的消费者。零值时由 Starts 推导（同一 effect 流与前缀、自己的
	// durable）：有原生步骤与没有原生步骤的部署配置相同，后者只是永远不会收到这些 effect。
	NestResults NestCompletionConsumerConfig
}

// nestResultsConfig 在 NestResults 为空时由 Starts 补齐。
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

// defaultNestResultDurable 推导一个不会与启动消费者冲突的 durable：共用 durable 的两个消费者共用一个游标，每条消息只会
// 到达其中一个。
func defaultNestResultDurable(startDurable string) string {
	if startDurable == "" {
		return "roost-saga-nest-result"
	}
	return startDurable + "-result"
}

// Assembly 在构造时建立 store、transport 和 engine；Start 建立基础设施、
// 普通完成、启动意图、原生 Nest 完成三个持久消费者，并启动协调循环。
// Stop 先排空消费者再停止循环；Kit 仅解析配置、发布能力和转发生命周期。
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

// Assemble 构建存储、传输与引擎并登记定义，此时还不访问 Mongo 与 JetStream。
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

// Start 确保 Mongo 基础设施与流存在，订阅结果与 Nest 启动消费者，在后台运行引擎循环。ctx 只约束基础设施调用；循环
// 运行到 Stop。运行中再次 Start 无效果。
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
	// 第三个消费者：原生 Nest 步骤作为 effect 提交的 completion。没有它这些结果无人接收，saga 要等完每次超时。这里失败
	// 时与其他部分启动失败一样，撤掉已启动的两个。
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

// Stop 排空三个消费者（在 ctx 内等它们关闭），取消循环、停止引擎并等待循环退出。排空未能及时完成时强制停止消费者，
// 返回 ctx 的错误。
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

// Running 报告引擎循环是否在运行。
func (a *Assembly) Running() bool { return a != nil && a.running.Load() }

// ConsumersClosed 检查三个必需消费者：缺失或任意一个退出都不是健康运行。
// 原生 Nest 完成消费者遗漏时，循环仍能运行但无法接收业务回执。
func (a *Assembly) ConsumersClosed() bool {
	if a == nil {
		return true
	}
	a.stateMu.RLock()
	resultSub, startSub, nestResults := a.resultSub, a.startSub, a.nestResults
	a.stateMu.RUnlock()
	return subscriptionClosed(resultSub) || subscriptionClosed(startSub) || subscriptionClosed(nestResults)
}

// RunError 返回引擎循环自行退出时的错误。
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
