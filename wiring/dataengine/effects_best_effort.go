package dataengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"github.com/tjbdwanghaibo/roost-core/infra/base/worker"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// best_effort effect（nest.EffectBestEffort，维护者 2026-11-05 “方案一”）的发送端与接收端。
//
// 发送：Nest 在一次写到达持久点之后（与 AfterCommit 同一时刻，规则见 nest.RollbackTx.Emit）调用 Mod.PublishBestEffort；
// 它只把 effect 放进容量 dataengine.effects.best_effort_queue 的队列（可能在快池上调用，不等待），一个后台 goroutine
// 编成与可靠 effect 相同的信封（nestwal.EffectEnvelope）经 NATS 核心发布（不经 JetStream、不写 outbox）。队列满、
// NATS 发布失败、进程在入队后发出前退出都丢这一条：至多一次。
//
// 主题：BestEffortSubject——<subject_prefix>_best_effort.<topic>。不能在 <subject_prefix>.> 之下：效果流按那个通配
// 收消息，核心发布到那里会被 JetStream 存进流、被可靠消费者当作可靠 effect 收走。
//
// 接收：SubscribeEffects 对 Delivery 为 best_effort 的消费者用 NATS 队列订阅（队列组 = Durable，同一服务的副本分摊），
// 没有 inbox、没有重投：handler 出错或 panic 只记日志与 dataengine.effect_consumer.handled_total{result="failed"}。

// BestEffortSubject 是 best_effort effect 在 NATS 上的主题；prefix 是 dataengine.effects.subject_prefix（去掉首尾点）。
func BestEffortSubject(prefix, topic string) string {
	return strings.Trim(prefix, ".") + "_best_effort." + strings.Trim(topic, ".")
}

const bestEffortMetric = "dataengine.effects.best_effort_total"

type bestEffortItem struct {
	transactionID string
	effect        corenest.Effect
}

// bestEffortPublisher 是 Mod 的 best_effort 发送队列：enqueue 不阻塞，run 逐条发布，stop 发完剩下的（在 ctx 内）。
type bestEffortPublisher struct {
	client fnats.IClient
	prefix string
	queue  chan bestEffortItem

	mu       sync.Mutex
	started  bool
	stopping bool
	stopped  chan struct{} // 后台 goroutine 退出后关闭；未启动时由 stop 关闭
	quit     chan struct{} // stop 关闭：run 发完队列里已有的后退出
}

func newBestEffortPublisher(client fnats.IClient, prefix string, capacity int) *bestEffortPublisher {
	if capacity <= 0 {
		capacity = 8192
	}
	return &bestEffortPublisher{client: client, prefix: prefix, queue: make(chan bestEffortItem, capacity),
		stopped: make(chan struct{}), quit: make(chan struct{})}
}

func (p *bestEffortPublisher) start() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.stopping {
		return
	}
	p.started = true
	go p.run()
}

// enqueue 把 effect 放进发送队列；满了或已在停机就丢弃并计数。只做非阻塞的 channel 发送，可在快池上调用。
func (p *bestEffortPublisher) enqueue(transactionID corenest.TransactionID, effects []corenest.Effect) {
	if p == nil {
		metrics.IncCounter(bestEffortMetric, metrics.Labels{"result": "unavailable"}, int64(len(effects)))
		slog.Warn("dataengine: best_effort effects dropped: this process has no NATS client to send them", "count", len(effects))
		return
	}
	p.mu.Lock()
	stopping := p.stopping
	p.mu.Unlock()
	id := transactionID.String()
	for _, effect := range effects {
		if stopping {
			metrics.IncCounter(bestEffortMetric, metrics.Labels{"result": "stopped"}, 1)
			continue
		}
		select {
		case p.queue <- bestEffortItem{transactionID: id, effect: effect}:
		default:
			metrics.IncCounter(bestEffortMetric, metrics.Labels{"result": "dropped"}, 1)
		}
	}
}

func (p *bestEffortPublisher) run() {
	defer close(p.stopped)
	for {
		select {
		case item := <-p.queue:
			p.publish(item)
		case <-p.quit:
			for {
				select {
				case item := <-p.queue:
					p.publish(item)
				default:
					return
				}
			}
		}
	}
}

func (p *bestEffortPublisher) publish(item bestEffortItem) {
	payload, err := json.Marshal(nestwal.EffectEnvelope{
		TransactionID: item.transactionID, EffectID: item.effect.ID, Topic: item.effect.Topic,
		Key: item.effect.Key, Headers: item.effect.Headers, Payload: item.effect.Payload,
	})
	if err == nil {
		err = p.client.Publish(BestEffortSubject(p.prefix, item.effect.Topic), payload)
	}
	if err != nil {
		metrics.IncCounter(bestEffortMetric, metrics.Labels{"result": "failed"}, 1)
		slog.Warn("dataengine: best_effort effect not sent", "topic", item.effect.Topic, "effect_id", item.effect.ID, "err", err)
		return
	}
	metrics.IncCounter(bestEffortMetric, metrics.Labels{"result": "published"}, 1)
}

// stop 不再接收新的 effect，在 ctx 内等后台 goroutine 发完队列里已有的。可重复调用；ctx 先结束返回 ctx 错误（剩下的
// 仍在后台继续发，NATS 连接随后由 NATS Mod 关闭）。
func (p *bestEffortPublisher) stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if !p.stopping {
		p.stopping = true
		close(p.quit)
		if !p.started {
			p.started = true
			close(p.stopped)
		}
	}
	p.mu.Unlock()
	select {
	case <-p.stopped:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dataengine: best_effort effects still sending: %w", ctx.Err())
	}
}

// subscribeBestEffort 是 SubscribeEffects 对 best_effort 消费者的实现：NATS 队列订阅，处理规则见文件头。
func subscribeBestEffort(client fnats.IClient, subject, durable string, consumer EffectConsumer, workers int, handler nestwal.EffectHandler) (*bestEffortSubscription, error) {
	ctx, cancel := context.WithCancel(context.Background())
	subscription := &bestEffortSubscription{
		durable: durable, ackWait: consumer.AckWait, handler: handler, ordered: consumer.Order != EffectOrderNone,
		ctx: ctx, cancel: cancel, closed: make(chan struct{}),
		inFlight: make(chan struct{}, consumer.MaxAckPending), running: make(chan struct{}, workers),
		keyed: worker.NewKeyedDispatcher(2 * consumer.MaxAckPending), workers: workers,
	}
	natsSubscription, err := client.QueueSubscribe(subject, durable, subscription.deliver)
	if err != nil {
		cancel()
		return nil, err
	}
	subscription.subscription = natsSubscription
	return subscription, nil
}

// bestEffortSubscription 实现 fnats.IJetStreamSubscription（与可靠消费者同一个返回类型，业务 Shutdown 里的 Drain 照写）。
type bestEffortSubscription struct {
	durable      string
	ackWait      time.Duration
	handler      nestwal.EffectHandler
	ordered      bool
	workers      int
	subscription fnats.ISubscription

	ctx    context.Context
	cancel context.CancelFunc

	inFlight chan struct{} // 已接纳、还没处理完的 effect，容量 MaxAckPending（满了回调等，nats.go 的待交付缓冲接着攒）
	running  chan struct{} // 在跑的 handler，容量 Workers
	keyed    *worker.KeyedDispatcher

	// acceptMu 保证“先关接纳、再等在途”（RV-20261107-02）：每个回调在 acceptMu 下检查 stopped 并 tasks.Add，
	// Stop / Drain 在 acceptMu 下置 stopped 之后才 tasks.Wait，所以 Wait 之后不会再有 Add，Closed 之前全部已接纳的
	// 回调（含 Workers ≤ 1 时在投递 goroutine 上就地执行的）都已返回。原先就地执行的回调不记入 tasks，Closed 在
	// handler 还在跑时就关闭；Stop 之后到达的消息也仍会调 handler。
	acceptMu sync.Mutex
	stopped  bool
	tasks    sync.WaitGroup

	closeOnce sync.Once
	closed    chan struct{}
}

// deliver 是 nats.go 的回调（每个订阅一个 goroutine 依次调用）。
func (s *bestEffortSubscription) deliver(message *fnats.Msg) {
	envelope, err := decodeBestEffort(message)
	if err != nil {
		metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": s.durable, "result": "poison"}, 1)
		slog.Error("dataengine effects: dropping a best_effort effect that cannot be decoded", "consumer", s.durable, "err", err)
		return
	}
	if !s.accept() {
		metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": s.durable, "result": "stopped"}, 1)
		return
	}
	if s.workers <= 1 {
		defer s.tasks.Done()
		s.process(envelope)
		return
	}
	select {
	case s.inFlight <- struct{}{}:
	case <-s.ctx.Done():
		s.tasks.Done()
		metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": s.durable, "result": "stopped"}, 1)
		return
	}
	key := envelope.Key
	if !s.ordered {
		key = "" // order=none：不按 Key 排队，只受 Workers 上限
	}
	run := func() {
		defer func() {
			<-s.inFlight
			s.tasks.Done()
		}()
		select {
		case s.running <- struct{}{}:
		case <-s.ctx.Done():
			metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": s.durable, "result": "stopped"}, 1)
			return
		}
		defer func() { <-s.running }()
		// 两个分支同时就绪时 select 随机选：Stop 之后排队的回调可能先拿到 running，这里再查一次，兑现“排队的不再处理”。
		if s.ctx.Err() != nil {
			metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": s.durable, "result": "stopped"}, 1)
			return
		}
		s.process(envelope)
	}
	if err := s.keyed.Dispatch(key, startEffectTask, run); err != nil {
		// 不会发生（派发上限是准入名额的两倍）；丢弃这一条。
		<-s.inFlight
		s.tasks.Done()
		metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": s.durable, "result": "failed"}, 1)
	}
}

// accept 接纳一条 effect：已停止接纳返回 false；否则记入在途（tasks），由调用方在处理结束时 Done。
func (s *bestEffortSubscription) accept() bool {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	if s.stopped {
		return false
	}
	s.tasks.Add(1)
	return true
}

// stopAccepting 关闭接纳；此后 tasks.Wait 只等已接纳的回调。可重复调用。
func (s *bestEffortSubscription) stopAccepting() {
	s.acceptMu.Lock()
	s.stopped = true
	s.acceptMu.Unlock()
}

// process 运行 handler（ctx 以 AckWait 为截止）；错误与 panic 只记日志与计数，没有重投。
func (s *bestEffortSubscription) process(envelope nestwal.EffectEnvelope) {
	ctx, cancel := context.WithTimeout(s.ctx, s.ackWait)
	defer cancel()
	err := func() (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("handler panic: %v", recovered)
			}
		}()
		return s.handler(ctx, envelope)
	}()
	if err != nil {
		metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": s.durable, "result": "failed"}, 1)
		slog.Warn("dataengine effects: best_effort handler failed; the effect is not redelivered", "consumer", s.durable,
			"effect_id", envelope.EffectID, "topic", envelope.Topic, "err", err)
		return
	}
	metrics.IncCounter("dataengine.effect_consumer.handled_total", metrics.Labels{"consumer": s.durable, "result": "ok"}, 1)
}

func decodeBestEffort(message *fnats.Msg) (nestwal.EffectEnvelope, error) {
	var envelope nestwal.EffectEnvelope
	if message == nil || len(message.Data) == 0 {
		return envelope, errors.New("empty effect message")
	}
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		return envelope, fmt.Errorf("decode effect: %w", err)
	}
	if envelope.EffectID == "" {
		return envelope, errors.New("effect envelope has no effect_id")
	}
	return envelope, nil
}

// Drain 退掉订阅兴趣、交完 nats.go 已缓冲的消息、等已接纳的 handler 跑完，然后关闭 Closed。不阻塞。
func (s *bestEffortSubscription) Drain() {
	go func() {
		if drainer, ok := s.subscription.(fnats.DrainSubscription); ok {
			if err := drainer.DrainContext(context.Background()); err != nil {
				slog.Warn("dataengine effects: best_effort subscription drain failed", "consumer", s.durable, "err", err)
				_ = s.subscription.Unsubscribe()
			}
		} else {
			_ = s.subscription.Unsubscribe()
		}
		// 排空（或退订）之后才关接纳：排空期间 nats.go 交来的缓冲消息照常处理。
		s.stopAccepting()
		s.tasks.Wait()
		s.close()
	}()
}

// Stop 先关接纳（此后到达的消息不再处理），再退订、取消在跑 handler 的 ctx（排队的不再处理），等全部已接纳的回调
// 返回后关闭 Closed。不阻塞。
func (s *bestEffortSubscription) Stop() {
	s.stopAccepting()
	_ = s.subscription.Unsubscribe()
	s.cancel()
	go func() {
		s.tasks.Wait()
		s.close()
	}()
}

func (s *bestEffortSubscription) Closed() <-chan struct{} { return s.closed }

func (s *bestEffortSubscription) close() {
	s.closeOnce.Do(func() {
		s.cancel()
		close(s.closed)
	})
}

var _ fnats.IJetStreamSubscription = (*bestEffortSubscription)(nil)
