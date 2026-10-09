package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	kitnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
)

// NestStartConsumerConfig configures the shared durable consumer which turns
// transactional Nest effects into Saga records. All coordinator replicas in
// one logical service must use the same Durable value.
type NestStartConsumerConfig struct {
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

type Starter interface {
	StartSaga(context.Context, StartRequest) (Record, error)
}

func SubscribeNestStarts(ctx context.Context, client fnats.IJetStream, config NestStartConsumerConfig, starter Starter) (fnats.IJetStreamSubscription, error) {
	if client == nil || starter == nil {
		return nil, fmt.Errorf("saga: Nest start subscriber dependencies are required")
	}
	config.Stream = strings.TrimSpace(config.Stream)
	config.Durable = strings.TrimSpace(config.Durable)
	config.EffectPrefix = strings.Trim(strings.TrimSpace(config.EffectPrefix), ".")
	if config.Stream == "" || config.Durable == "" || !validSubjectPath(config.EffectPrefix) {
		return nil, fmt.Errorf("saga: Nest start stream, durable and effect prefix are required")
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
		return nil, fmt.Errorf("saga: unsafe Nest start consumer limits")
	}
	return client.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream:        config.Stream,
		Name:          config.Durable,
		Durable:       config.Durable,
		FilterSubject: config.EffectPrefix + "." + StartEffectTopic,
		DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait:       config.AckWait,
		MaxDeliver:    config.MaxDeliver,
		MaxAckPending: config.MaxAckPending,
		NakBackoffMin: config.NakBackoffMin,
		NakBackoffMax: config.NakBackoffMax,
	}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		processCtx, cancel := context.WithTimeout(messageCtx, config.ProcessTimeout)
		err := handleNestStart(processCtx, message, starter)
		cancel()
		if err != nil {
			logConsumerError("Nest start", message, err)
		}
		return err
	})
}

func handleNestStart(ctx context.Context, message *fnats.JetStreamMsg, starter Starter) error {
	if message == nil || starter == nil {
		return rejectEnvelope("start", ErrInvalidRecord)
	}
	if len(message.Data) > maxWireEnvelopeBytes {
		return rejectEnvelope("start", ErrInvalidRecord)
	}
	var envelope nestwal.EffectEnvelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		return rejectEnvelope("start", err)
	}
	if envelope.EffectID == "" || envelope.Topic != StartEffectTopic {
		return rejectEnvelope("start", ErrInvalidRecord)
	}
	request, err := DecodeStartEffect(envelope.Payload)
	if err != nil {
		return rejectEnvelope("start", err)
	}
	_, err = starter.StartSaga(ctx, request)
	if reason := startRefusalReason(err); reason != "" {
		reportStartRefused(request, reason, err)
		return kitnats.Permanent(err)
	}
	return err
}

// startRefusalReason 给出重投永远不会改变的 StartSaga 错误（RR-20261006-43）：ErrInvalidRecord（Data 超过 MaxPayloadBytes、
// 业务键或 ID 不合法）与 ErrIdentityConflict（同一 (type, business_key) 已有另一份意图，启动摘要是持久的）。之前它们与暂时错误
// 一样 nak 退避，约 8.7 天后才被 MaxDeliver Term，saga 从未创建、没有指标。ErrDefinitionMissing 不在其中：与结果消费者同一口径，
// 它是滚动发布中协调器还没升级的暂时状态，定义上线后重投被接收。
func startRefusalReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrIdentityConflict):
		return "identity_conflict"
	case errors.Is(err, ErrInvalidRecord):
		return "invalid_record"
	default:
		return ""
	}
}

// reportStartRefused 告警一份被永久拒绝的启动意图：业务的 Nest 事务已经提交，saga 不会创建，需要人核对。
// 标签只有 saga 类型与原因；业务键与 saga id 只进日志。
func reportStartRefused(request StartRequest, reason string, err error) {
	metrics.IncCounter("saga.start.rejected_total", metrics.Labels{"saga_type": request.Type, "reason": reason}, 1)
	slog.Error("saga: start intent permanently refused; the saga was not created",
		"saga_type", request.Type, "saga_id", request.ID, "business_key", request.BusinessKey, "definition_version", request.DefinitionVersion,
		"data_bytes", len(request.Data), "reason", reason, "err", err)
}

var _ Starter = (*Engine)(nil)

// rejectEnvelope 是五个 saga 消费者（Mongo 步骤、原生步骤、普通结果流、原生结果流、Nest 启动）对坏信封的同一处理
// （RR-20261006-44）：空消息、超长帧、JSON 损坏、未知 WireVersion、内容校验不过都是确定性错误，重投不会变好——计
// saga.consumer.rejected_total{consumer}，返回 Permanent 让驱动 Term。之前原生步骤消费者把它们当普通错误 nak 到 MaxDeliver
// （缺省 25000 次、退避封顶 30s，约 8.7 天），期间占着共享 durable 的 MaxAckPending 名额；另外四个虽然 Term，但没有指标。
// ERROR 日志由调用方的 logConsumerError 记（Nest 启动与原生结果流在订阅回调里统一记）。
func rejectEnvelope(consumer string, err error) error {
	metrics.IncCounter("saga.consumer.rejected_total", metrics.Labels{"consumer": consumer}, 1)
	return kitnats.Permanent(err)
}

func logConsumerError(kind string, message *fnats.JetStreamMsg, err error) {
	if message == nil {
		slog.Error("saga: consumer rejected nil message", "kind", kind, "err", err)
		return
	}
	slog.Error("saga: consumer processing failed", "kind", kind, "subject", message.Subject, "stream_sequence", message.StreamSeq, "deliveries", message.NumDelivered, "err", err)
}
