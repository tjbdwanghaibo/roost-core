package driver

import (
	"context"
	"errors"
	"fmt"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"

	gojs "github.com/nats-io/nats.go/jetstream"
)

// JetStreamClient 是同一连接上的 JetStream。已关闭判据在 client 上（见 Client 的类型注释）：
// 入口经 admit 检查，nats.go 的失败经 client.wrapError 翻译（Close 之后一律 fnats.ErrClosed）。
type JetStreamClient struct {
	js     gojs.JetStream
	client *Client
}

func NewJetStreamClient(client *Client) (*JetStreamClient, error) {
	if client == nil || client.natsConn() == nil {
		return nil, errors.New("nats jetstream: client is nil")
	}
	js, err := gojs.New(client.natsConn())
	if err != nil {
		return nil, err
	}
	return &JetStreamClient{js: js, client: client}, nil
}

// admit 先报未初始化，再查连接是否已关闭（没有 client 的只在测试里出现，不查）。
func (c *JetStreamClient) admit() error {
	if c == nil || c.js == nil {
		return errors.New("nats jetstream: not initialized")
	}
	if c.client == nil {
		return nil
	}
	return c.client.admit()
}

func (c *JetStreamClient) EnsureStream(ctx context.Context, cfg fnats.JetStreamConfig) error {
	if err := c.admit(); err != nil {
		return err
	}
	_, err := c.js.CreateOrUpdateStream(ctx, toJetStreamStreamConfig(cfg))
	return c.client.wrapError(err)
}

func (c *JetStreamClient) Publish(ctx context.Context, subject string, data []byte, opts fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	if err := c.admit(); err != nil {
		return fnats.JetStreamPublishAck{}, err
	}
	publishOpts := make([]gojs.PublishOpt, 0, 1)
	if opts.MsgID != "" {
		publishOpts = append(publishOpts, gojs.WithMsgID(opts.MsgID))
	}
	ack, err := c.js.Publish(ctx, subject, data, publishOpts...)
	if err != nil {
		return fnats.JetStreamPublishAck{}, c.client.wrapError(err)
	}
	if ack == nil {
		return fnats.JetStreamPublishAck{}, nil
	}
	return fnats.JetStreamPublishAck{
		Stream:    ack.Stream,
		Sequence:  ack.Sequence,
		Duplicate: ack.Duplicate,
	}, nil
}

func (c *JetStreamClient) Subscribe(ctx context.Context, cfg fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	if err := c.admit(); err != nil {
		return nil, err
	}
	if handler == nil {
		return nil, errors.New("nats jetstream: handler is nil")
	}
	consumer, err := c.js.CreateOrUpdateConsumer(ctx, cfg.Stream, toJetStreamConsumerConfig(cfg))
	if err != nil {
		return nil, consumerConfigError(cfg, c.client.wrapError(err))
	}
	handlerCtx, cancel := context.WithCancel(context.Background())
	sub := &jetStreamSubscription{cancel: cancel, closed: make(chan struct{})}
	cc, err := consumer.Consume(func(msg gojs.Msg) {
		sub.deliver(handlerCtx, handler, msg, cfg)
	})
	if err != nil {
		cancel()
		return nil, c.client.wrapError(err)
	}
	sub.watch(cc)
	return sub, nil
}

// consumerConfigError 给建立 / 更新持久消费者的失败带上流与消费者名（否则启动失败的报错不说是哪个 durable）。
//
// Subscribe 用 CreateOrUpdateConsumer：同名 durable 已存在时原地更新。MaxDeliver、MaxAckPending、AckWait、
// FilterSubject 这类可变字段直接改成新值、投递进度保留——新版本改了这些参数，启动即生效，不会静默沿用旧值；
// DeliverPolicy、AckPolicy、ReplayPolicy、起始序号 / 时间等不可变字段与已存在的 durable 不同时，服务器拒绝
// （API 错误 10012，描述为 “… can not be updated”），进程启动失败。这种冲突这里点名处置：停掉所有使用它的
// 进程、删除 durable 再启动——新 durable 按 DeliverPolicy 重新开始，旧的投递进度丢失（DeliverAll 会从流里最早
// 的消息重投，消费者的幂等收件箱负责去重）。2026-10-18 用 nats-server 2.14.5 实测，见 real_nats_integration_test.go。
func consumerConfigError(cfg fnats.JetStreamConsumerConfig, err error) error {
	name := cfg.Durable
	if name == "" {
		name = cfg.Name
	}
	var apiErr *gojs.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == gojs.JSErrCodeConsumerCreate &&
		(strings.Contains(apiErr.Description, "can not be updated") || strings.Contains(apiErr.Description, "can not update")) {
		return fmt.Errorf("nats jetstream: consumer %q on stream %q already exists with a setting that cannot be changed in place; "+
			"stop every process that uses it, delete it (`nats consumer rm %s %s`) and start again — the new consumer starts over "+
			"from its deliver policy and the old delivery position is lost: %w", name, cfg.Stream, cfg.Stream, name, err)
	}
	return fmt.Errorf("nats jetstream: consumer %q on stream %q: %w", name, cfg.Stream, err)
}

// settleJetStreamDelivery acknowledges, redelivers, or terminates one delivery
// according to the handler outcome. A settle call that fails is not fatal —
// the broker redelivers after AckWait, which is exactly the at-least-once
// contract — but it must be visible: a broken ack path (closed connection,
// consumer deleted underneath us) looks identical to "the handler keeps
// failing" if the error is dropped, and the redelivery loop it produces has
// no other symptom.
func settleJetStreamDelivery(msg gojs.Msg, wrapped *fnats.JetStreamMsg, cfg fnats.JetStreamConsumerConfig, handlerErr error) {
	op := "ack"
	var settleErr error
	switch {
	case handlerErr == nil:
		settleErr = msg.Ack()
	default:
		if reason := terminalReason(handlerErr, cfg.MaxDeliver, wrapped.NumDelivered); reason != "" {
			slog.Error("nats jetstream: terminating failed delivery", "subject", wrapped.Subject, "stream", wrapped.Stream, "consumer", wrapped.Consumer, "stream_sequence", wrapped.StreamSeq, "deliveries", wrapped.NumDelivered, "reason", reason, "err", handlerErr)
			metrics.IncCounter("nats.jetstream.terminal.total", metrics.Labels{"reason": reason}, 1)
			op, settleErr = "term", msg.Term()
		} else if delay := nakBackoff(cfg, wrapped.NumDelivered); delay > 0 {
			op, settleErr = "nak_delay", msg.NakWithDelay(delay)
		} else {
			op, settleErr = "nak", msg.Nak()
		}
	}
	if settleErr == nil {
		return
	}
	metrics.IncCounter("nats.jetstream.settle_failures.total", metrics.Labels{"op": op}, 1)
	slog.Warn("nats jetstream: settling delivery failed; broker will redeliver after ack wait", "op", op, "subject", wrapped.Subject, "stream", wrapped.Stream, "consumer", wrapped.Consumer, "stream_sequence", wrapped.StreamSeq, "deliveries", wrapped.NumDelivered, "err", settleErr)
}

func terminalReason(err error, maxDeliver int, deliveries uint64) string {
	if fnats.IsPermanent(err) {
		return "permanent"
	}
	if err != nil && maxDeliver > 0 && deliveries >= uint64(maxDeliver) {
		return "max_deliver"
	}
	return ""
}

func invokeJetStreamHandler(ctx context.Context, handler fnats.JetStreamHandler, msg *fnats.JetStreamMsg) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("nats jetstream: handler panic: %v", recovered)
		}
	}()
	return handler(ctx, msg)
}

func nakBackoff(config fnats.JetStreamConsumerConfig, deliveries uint64) time.Duration {
	minimum, maximum := config.NakBackoffMin, config.NakBackoffMax
	if minimum <= 0 {
		return 0
	}
	if maximum < minimum {
		maximum = minimum
	}
	delay := minimum
	for attempt := uint64(1); attempt < deliveries && delay < maximum; attempt++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func toJetStreamStreamConfig(cfg fnats.JetStreamConfig) gojs.StreamConfig {
	return gojs.StreamConfig{
		Name:       cfg.Name,
		Subjects:   append([]string(nil), cfg.Subjects...),
		Storage:    toJetStreamStorage(cfg.Storage),
		MaxAge:     cfg.MaxAge,
		Duplicates: cfg.Duplicates,
		Replicas:   cfg.Replicas,
		MaxBytes:   cfg.MaxBytes,
	}
}

func toJetStreamConsumerConfig(cfg fnats.JetStreamConsumerConfig) gojs.ConsumerConfig {
	name := cfg.Name
	if name == "" {
		name = cfg.Durable
	}
	durable := cfg.Durable
	if durable == "" {
		durable = name
	}
	return gojs.ConsumerConfig{
		Name:          name,
		Durable:       durable,
		FilterSubject: cfg.FilterSubject,
		DeliverPolicy: toJetStreamDeliverPolicy(cfg.DeliverPolicy),
		AckPolicy:     gojs.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    cfg.MaxDeliver,
		MaxAckPending: cfg.MaxAckPending,
	}
}

func toJetStreamStorage(storage fnats.JetStreamStorage) gojs.StorageType {
	switch storage {
	case fnats.JetStreamStorageMemory:
		return gojs.MemoryStorage
	default:
		return gojs.FileStorage
	}
}

func toJetStreamDeliverPolicy(policy fnats.JetStreamDeliverPolicy) gojs.DeliverPolicy {
	switch policy {
	case fnats.JetStreamDeliverNew:
		return gojs.DeliverNewPolicy
	default:
		return gojs.DeliverAllPolicy
	}
}

func jetStreamMsg(msg gojs.Msg) *fnats.JetStreamMsg {
	wrapped := &fnats.JetStreamMsg{
		Subject:    msg.Subject(),
		Data:       append([]byte(nil), msg.Data()...),
		InProgress: msg.InProgress,
	}
	meta, err := msg.Metadata()
	if err == nil && meta != nil {
		wrapped.Stream = meta.Stream
		wrapped.Consumer = meta.Consumer
		wrapped.StreamSeq = meta.Sequence.Stream
		wrapped.ConsumerSeq = meta.Sequence.Consumer
		wrapped.NumDelivered = meta.NumDelivered
	}
	return wrapped
}

// jetStreamSubscription 是一个 durable 的消费句柄。nats.go 在一个 goroutine 里逐条调用回调；handler 可以用
// JetStreamMsg.DeferSettle 把结算推迟到后台，这里记着推迟中的投递数（pending）。
//
// 结束分两步：nats.go 的 ConsumeContext 关闭（不再有回调）记为 consumeEnded；它关闭且 pending 归零时 finish——
// 取消 handler 的 ctx、关闭 closed。所以 Closed 表示“没有在途回调，也没有未结算的推迟投递”，Drain 的调用方等它就等到
// 后台结算完成。
type jetStreamSubscription struct {
	cc         gojs.ConsumeContext
	cancel     context.CancelFunc
	mu         sync.Mutex
	cancelOnce sync.Once
	draining   bool
	stopped    bool

	// 下面三项受 mu 保护。closed 只由 finish 关闭一次（finished 记录）。
	pending      int
	consumeEnded bool
	finished     bool
	closed       chan struct{}
}

// newJetStreamSubscription 包装一个已开始消费的 ConsumeContext 并开始等它结束。
func newJetStreamSubscription(cc gojs.ConsumeContext, cancel context.CancelFunc) *jetStreamSubscription {
	sub := &jetStreamSubscription{cancel: cancel, closed: make(chan struct{})}
	sub.watch(cc)
	return sub
}

// watch 记下 ConsumeContext，并在它关闭（nats.go 不再调用回调）时记 consumeEnded。
func (s *jetStreamSubscription) watch(cc gojs.ConsumeContext) {
	s.mu.Lock()
	s.cc = cc
	s.mu.Unlock()
	closed := cc.Closed()
	go func() {
		<-closed
		s.mu.Lock()
		s.consumeEnded = true
		s.mu.Unlock()
		s.maybeFinish()
	}()
}

// deliver 处理一条投递：调用 handler，按返回值结算；handler 推迟了结算时交给 deferredDelivery。
func (s *jetStreamSubscription) deliver(ctx context.Context, handler fnats.JetStreamHandler, msg gojs.Msg, cfg fnats.JetStreamConsumerConfig) {
	wrapped := jetStreamMsg(msg)
	delivery := &deferredDelivery{sub: s, msg: msg, wrapped: wrapped, cfg: cfg}
	wrapped.DeferSettle = delivery.deferSettle
	handlerErr := invokeJetStreamHandler(ctx, handler, wrapped)
	if delivery.handlerReturned(handlerErr) {
		return
	}
	settleJetStreamDelivery(msg, wrapped, cfg, handlerErr)
}

// beginDeferred 登记一个推迟的结算；订阅已在结束时拒绝（handler 照旧同步处理）。
func (s *jetStreamSubscription) beginDeferred() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumeEnded || s.finished {
		return false
	}
	s.pending++
	return true
}

// endDeferred 是一个推迟的结算完成。
func (s *jetStreamSubscription) endDeferred() {
	s.mu.Lock()
	s.pending--
	s.mu.Unlock()
	s.maybeFinish()
}

// maybeFinish 在 ConsumeContext 已关闭、且没有推迟中的结算时结束订阅：取消 handler 的 ctx（Drain 之后它要活到这里），
// 关闭 closed。只做一次。
func (s *jetStreamSubscription) maybeFinish() {
	s.mu.Lock()
	if !s.consumeEnded || s.pending > 0 || s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.stopped = true
	s.mu.Unlock()
	s.cancelHandler()
	close(s.closed)
}

// deferredDelivery 是一条投递的结算权：同步路径（handler 不调用 DeferSettle）在回调返回时结算；推迟路径由 settle 或
// handler 的错误返回二者先到的那个结算，恰好一次。
type deferredDelivery struct {
	sub     *jetStreamSubscription
	msg     gojs.Msg
	wrapped *fnats.JetStreamMsg
	cfg     fnats.JetStreamConsumerConfig

	mu    sync.Mutex
	state deliveryState
}

type deliveryState uint8

const (
	deliveryInline   deliveryState = iota // 没有推迟：回调返回时按返回值结算
	deliveryDeferred                      // 已推迟，等 settle（或 handler 的错误返回）
	deliverySettled                       // 已结算
)

func (d *deferredDelivery) deferSettle() func(error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state != deliveryInline || !d.sub.beginDeferred() {
		return nil
	}
	d.state = deliveryDeferred
	return d.settle
}

// handlerReturned 在 handler 返回后调用；true 表示结算已归推迟路径（或已因 handler 的错误立即完成）。
func (d *deferredDelivery) handlerReturned(handlerErr error) bool {
	d.mu.Lock()
	switch {
	case d.state == deliveryInline:
		d.mu.Unlock()
		return false
	case d.state == deliveryDeferred && handlerErr != nil:
		d.mu.Unlock()
		d.settle(handlerErr)
		return true
	default:
		d.mu.Unlock()
		return true
	}
}

func (d *deferredDelivery) settle(err error) {
	d.mu.Lock()
	if d.state != deliveryDeferred {
		d.mu.Unlock()
		return
	}
	d.state = deliverySettled
	d.mu.Unlock()
	settleJetStreamDelivery(d.msg, d.wrapped, d.cfg, err)
	d.sub.endDeferred()
}

func (s *jetStreamSubscription) Stop() {
	if s != nil && s.cc != nil {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		s.stopped = true
		s.mu.Unlock()
		s.cancelHandler()
		s.cc.Stop()
	}
}

func (s *jetStreamSubscription) Drain() {
	if s != nil && s.cc != nil {
		s.mu.Lock()
		if s.stopped || s.draining {
			s.mu.Unlock()
			return
		}
		s.draining = true
		s.mu.Unlock()
		// handler 的 ctx 要活到缓冲里的消息处理完、推迟的结算也完成（maybeFinish 取消它）。
		s.cc.Drain()
	}
}

func (s *jetStreamSubscription) cancelHandler() {
	if s.cancel != nil {
		s.cancelOnce.Do(s.cancel)
	}
}

// Closed 在 ConsumeContext 关闭且推迟的结算全部完成后关闭。
func (s *jetStreamSubscription) Closed() <-chan struct{} {
	if s == nil || s.cc == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.closed
}

var _ fnats.IJetStream = (*JetStreamClient)(nil)
