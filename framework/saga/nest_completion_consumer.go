package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	kitnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
)

// NestCompletionConsumerConfig 配置把原生步骤的事务性 completion effect 转成 Engine.Complete 的持久消费者。
//
// 原生 Nest 步骤（SubscribeDataEngineStep + inbox.Bind + EmitCompletion）把 completion 作为 Nest effect 提交，它经
// DataEngine outbox 到达 EFFECT 流的 `<EffectPrefix>.saga.result.<sagaID>`，而不是普通结果消费者监听的 saga 流。
// 没有这个消费者就没人读它：saga 一直等待、步骤超时重投、收件箱永远回放同一份回执。
//
// 同一逻辑服务的全部协调器副本共用 Durable，每个 saga 由其中一个完成。
type NestCompletionConsumerConfig struct {
	Stream         string
	Durable        string
	EffectPrefix   string
	AckWait        time.Duration
	ProcessTimeout time.Duration
	MaxDeliver     int
	MaxAckPending  int
	NakBackoffMin  time.Duration
	NakBackoffMax  time.Duration
}

// Completer 是本消费者驱动的那一半 Engine。
type Completer interface {
	Complete(context.Context, Completion) (Record, error)
}

// SubscribeNestCompletions 安装这个消费者，与 SubscribeNestStarts 对称：同一个流、同一个前缀，各自的 durable 与主题。
func SubscribeNestCompletions(ctx context.Context, client fnats.IJetStream, config NestCompletionConsumerConfig, completer Completer) (fnats.IJetStreamSubscription, error) {
	if client == nil || completer == nil {
		return nil, fmt.Errorf("saga: Nest completion subscriber dependencies are required")
	}
	config.Stream = strings.TrimSpace(config.Stream)
	config.Durable = strings.TrimSpace(config.Durable)
	config.EffectPrefix = strings.Trim(strings.TrimSpace(config.EffectPrefix), ".")
	if config.Stream == "" || config.Durable == "" || !validSubjectPath(config.EffectPrefix) {
		return nil, fmt.Errorf("saga: Nest completion stream, durable and effect prefix are required")
	}
	if config.AckWait <= 0 {
		config.AckWait = 30 * time.Second
	}
	if config.ProcessTimeout <= 0 {
		config.ProcessTimeout = 10 * time.Second
	}
	if config.MaxDeliver <= 0 {
		config.MaxDeliver = 25_000
	}
	if config.MaxAckPending <= 0 {
		config.MaxAckPending = 256
	}
	if config.NakBackoffMin <= 0 {
		config.NakBackoffMin = 250 * time.Millisecond
	}
	if config.NakBackoffMax < config.NakBackoffMin {
		config.NakBackoffMax = 30 * time.Second
	}
	if config.ProcessTimeout >= config.AckWait || !validDeliveryLimits(config.MaxDeliver, config.MaxAckPending, config.NakBackoffMin, config.NakBackoffMax) {
		return nil, fmt.Errorf("saga: unsafe Nest completion consumer limits")
	}
	// 每个 saga id 一个 subject，所以过滤是通配——不同于只有一个 subject 的启动主题。
	return client.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream:        config.Stream,
		Name:          config.Durable,
		Durable:       config.Durable,
		FilterSubject: config.EffectPrefix + "." + strings.TrimSuffix(CompletionEffectTopicPrefix, ".") + ".>",
		DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait:       config.AckWait,
		MaxDeliver:    config.MaxDeliver,
		MaxAckPending: config.MaxAckPending,
		NakBackoffMin: config.NakBackoffMin,
		NakBackoffMax: config.NakBackoffMax,
	}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		processCtx, cancel := context.WithTimeout(messageCtx, config.ProcessTimeout)
		err := handleNestCompletion(processCtx, message, completer)
		cancel()
		if err != nil {
			logConsumerError("Nest completion", message, err)
		}
		return err
	})
}

func handleNestCompletion(ctx context.Context, message *fnats.JetStreamMsg, completer Completer) error {
	if message == nil || completer == nil {
		return rejectEnvelope("native_result", ErrInvalidRecord)
	}
	if len(message.Data) > maxWireEnvelopeBytes {
		return rejectEnvelope("native_result", ErrInvalidRecord)
	}
	var envelope nestwal.EffectEnvelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		return rejectEnvelope("native_result", err)
	}
	if envelope.EffectID == "" || !strings.HasPrefix(envelope.Topic, CompletionEffectTopicPrefix) {
		return rejectEnvelope("native_result", ErrInvalidRecord)
	}
	completion, err := DecodeCompletionEffect(envelope.Payload)
	if err != nil {
		return rejectEnvelope("native_result", err)
	}
	// 逐跳计时：completion effect PubAck → 协调器结果消费者开始处理（投递 + 在本 durable 后排队）。
	hoptrace.SinceMark("h11.completion.deliver", "effect-published/", envelope.EffectID)
	// 宽 subject 订阅不证明信封内路由正确，必须先核对目标 saga。
	// 在调用 Complete 前拒绝，避免异键消息推进状态或写入幂等回执。
	if envelope.Topic != CompletionEffectTopicPrefix+completion.SagaID {
		return rejectEnvelope("native_result", ErrInvalidRecord)
	}
	completeStart := hoptrace.Now()
	_, err = completer.Complete(ctx, completion)
	hoptrace.Since("h11.completion.handle", completeStart)
	if err == nil {
		return nil
	}
	// 本协调器已不拥有的 saga、或已经往前走的步骤的 completion 不是需要重试的投递：effect 流会保留它直到 ack，永远
	// 重投会让消费者卡在一条过期消息后面。Engine.Complete 对已记下的回执幂等，所以到达 ErrNotWaiting 说明结果已计入。
	if isTerminalCompletionError(err) {
		return kitnats.Permanent(err)
	}
	return err
}

// isTerminalCompletionError 列出重投无法改善的结果，两个结果消费者（原生 effect 流与普通结果流）共用它。ErrIdentityConflict：同一 CommandID 已记下另一份结果，回执是持久的，重投不会变。
//
// ErrDefinitionMissing 不在其中：Complete 只在记录正等着这个操作时才查定义，这时没有定义是滚动发布里的暂时状态——
// 派发它的进程有这个定义版本，处理结果的协调器还没升级，定义会随新进程上线。它按可重试错误 nak 退避：定义上线后
// 重投被接收；一直不来时，步骤超时后没有定义的协调器把记录 fence 到 ManualRequired（放弃关闭），之后的
// 重投按迟到成功 ack，MaxDeliver 兜底。若在这里 Term，滚动发布中先到的结果会丢，记录只能等超时重派或被 fence。
func isTerminalCompletionError(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotWaiting), errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalidRecord), errors.Is(err, ErrIdentityConflict):
		return true
	default:
		return false
	}
}

var _ Completer = (*Engine)(nil)
