package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// step_consumer.go 是参与方新路径的步骤消费者（设计 §5，第 3 批并存、未接线）：命令是 JetStream 协调器分区里的命令事件
// （<prefix>.<k>.ev.cmd.<topic>），每条流一个 durable（同名，过滤全部分区的这个主题）。执行后把结果直接发到命令所属分区的
// in.res（MsgID res.<CommandID>），PubAck 之后才 ack 命令；没有 outbox、没有租约、不经 DataEngine effect。
//
// 结算规则（原生与 Mongo 相同）：
//   - 坏信封 → Term + saga.consumer.rejected_total（与旧消费者同一口径）；
//   - Admit 拒绝 → 原样返回（nak，留给能执行它的进程）；
//   - 执行 / 回放出错 → 结果未知，nak 退避重投，重投时读到回执就回放；
//   - 过期未执行 → 直接 ack，计 saga.step.expired_unexecuted_total；
//   - 执行或回放 → 发结果，PubAck 之后 ack；发布失败 → nak（重投回放同一回执，同一 MsgID 在去重窗口内被服务端丢弃，协调器也按回执去重）。

// defaultStepWorkers 是新路径消费者在途处理数的缺省上限（每个订阅一份）：原生步骤的一次处理含一次 Nest 请求（WAL 落盘），
// 回调逐条串行会把整条流的步骤吞吐压到一次 fsync 一条；同一实体的事务仍由 Nest 实体锁串行。
const defaultStepWorkers = 32

// stepConsumerDefaults 补齐新路径消费者的投递参数并校验。workers 是在途处理上限（0 取缺省，不超过 MaxAckPending）。
func stepConsumerDefaults(config StepConsumerConfig, workers int) (StepConsumerConfig, int, error) {
	if config.Durable == "" || !validSubjectPath(config.Topic) {
		return config, 0, fmt.Errorf("saga: invalid step consumer configuration")
	}
	if config.AckWait <= 0 {
		config.AckWait = DefaultStepAckWait
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
	if config.ReceiptRetention <= 0 {
		config.ReceiptRetention = DefaultStepReceiptRetention
	}
	if workers <= 0 {
		workers = min(defaultStepWorkers, config.MaxAckPending)
	}
	if workers > config.MaxAckPending || !validDeliveryLimits(config.MaxDeliver, config.MaxAckPending, config.NakBackoffMin, config.NakBackoffMax) {
		return config, 0, fmt.Errorf("saga: unsafe step consumer limits")
	}
	return config, workers, nil
}

// subscribeStepStreams 在布局的每条流上建同名 durable（过滤全部分区的这个主题），返回全部订阅；中途失败时停掉已建的。
func subscribeStepStreams(ctx context.Context, js fnats.IJetStream, layout Layout, config StepConsumerConfig, handler fnats.JetStreamHandler) ([]fnats.IJetStreamSubscription, error) {
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	subs := make([]fnats.IJetStreamSubscription, 0, layout.Groups)
	for _, stream := range layout.Streams() {
		sub, err := js.Subscribe(ctx, fnats.JetStreamConsumerConfig{
			Stream: stream, Name: config.Durable, Durable: config.Durable, FilterSubject: layout.CommandFilter(config.Topic),
			DeliverPolicy: fnats.JetStreamDeliverAll, AckWait: config.AckWait, MaxDeliver: config.MaxDeliver,
			MaxAckPending: config.MaxAckPending, NakBackoffMin: config.NakBackoffMin, NakBackoffMax: config.NakBackoffMax,
		}, handler)
		if err != nil {
			for _, started := range subs {
				started.Stop()
			}
			return nil, fmt.Errorf("saga: subscribe %s on %s: %w", config.Durable, stream, err)
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// SubscribeNativeStep 订阅原生步骤命令（新路径）：每条命令经 RunNativeStep 运行步骤体（Nest handler 里 Begin 查回执、
// Complete 写回执），按结论发结果或直接 ack。config.Stream 与 ReceiptWaiters 不用（流来自布局；不再等投影），
// NativeWorkers 是在途处理上限。
//
// 一次处理的时长以 AckWait 为界（不是命令截止：过了截止的命令也要跑一次，读到成功回执要重发——现行“过期投递重发成功”）。
func SubscribeNativeStep(ctx context.Context, js fnats.IJetStream, layout Layout, config StepConsumerConfig, body NativeStepBody) ([]fnats.IJetStreamSubscription, error) {
	if js == nil || body == nil {
		return nil, fmt.Errorf("saga: invalid native step consumer configuration")
	}
	config, workers, err := stepConsumerDefaults(config, config.NativeWorkers)
	if err != nil {
		return nil, err
	}
	consume := func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		command, err := decodeCommandEvent(message)
		if err != nil {
			logConsumerError("native step decode", message, err)
			return rejectEnvelope("native_step", err)
		}
		if config.Admit != nil {
			if err := config.Admit(messageCtx, command); err != nil {
				return err
			}
		}
		processCtx, cancel := context.WithTimeout(messageCtx, config.AckWait)
		defer cancel()
		outcome, err := RunNativeStep(processCtx, command, config.ReceiptRetention, body)
		if err != nil {
			logConsumerError("native step", message, err)
			return err
		}
		return settleStepOutcome(messageCtx, js, layout, command, outcome, message)
	}
	return subscribeStepStreams(ctx, js, layout, config, withConsumerWorkers(workers, consume))
}

// SubscribeJetStreamMongoStep 订阅 Mongo 步骤命令（新路径，暂名：第 4 批切换时改回 SubscribeMongoStep）：每条命令经
// MongoStepInbox.Handle 在一个 Mongo 事务里查回执、执行 handler、写回执，按结论发结果或直接 ack。MongoWorkers 是在途处理上限
// （缺省 1，串行；业务 handler 允许不同命令并发时再调大）。
func SubscribeJetStreamMongoStep(ctx context.Context, js fnats.IJetStream, layout Layout, inbox *MongoStepInbox, config StepConsumerConfig, handler StepHandler) ([]fnats.IJetStreamSubscription, error) {
	if js == nil || inbox == nil || handler == nil {
		return nil, fmt.Errorf("saga: invalid mongo step consumer configuration")
	}
	workers := config.MongoWorkers
	if workers <= 0 {
		workers = 1
	}
	config, workers, err := stepConsumerDefaults(config, workers)
	if err != nil {
		return nil, err
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		return nil, err
	}
	consume := func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		command, err := decodeCommandEvent(message)
		if err != nil {
			logConsumerError("mongo step decode", message, err)
			return rejectEnvelope("mongo_step", err)
		}
		if config.Admit != nil {
			if err := config.Admit(messageCtx, command); err != nil {
				return err
			}
		}
		processCtx, cancel := context.WithTimeout(messageCtx, config.AckWait)
		defer cancel()
		outcome, err := inbox.Handle(processCtx, command, handler)
		if err != nil {
			logConsumerError("mongo step", message, err)
			return err
		}
		return settleStepOutcome(messageCtx, js, layout, command, outcome, message)
	}
	return subscribeStepStreams(ctx, js, layout, config, withConsumerWorkers(workers, consume))
}

// settleStepOutcome 按一次运行的结论收尾：过期未执行直接 ack；执行或回放把结果发回命令所属分区，PubAck 后 ack。
func settleStepOutcome(ctx context.Context, js fnats.IJetStream, layout Layout, command Command, outcome StepOutcome, message *fnats.JetStreamMsg) error {
	switch outcome.Kind {
	case StepExpired:
		metrics.IncCounter("saga.step.expired_unexecuted_total", nil, 1)
		slog.Info("saga: step command expired before it ran; acknowledged without executing",
			"command_id", command.ID, "saga_id", command.SagaID, "deadline_at", command.DeadlineAt)
		return nil
	case StepReplayed:
		metrics.IncCounter("saga.step.attempt_replayed_total", nil, 1)
	case StepExecuted:
	default:
		return fmt.Errorf("saga: step %s ended without an outcome", command.ID)
	}
	publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := publishCompletion(publishCtx, js, layout, outcome.Completion, int(command.Partition)); err != nil {
		logConsumerError("step result publish", message, err)
		return err
	}
	return nil
}

// publishCompletion 把一份结果追加到分区 partition 的 in.res（MsgID res.<CommandID>，与 JetStreamEngine.Complete 同一形状），
// 只承诺“已持久进入输入”（PubAck）；接收与否由分区循环判定。CompletedAt 不上线（协调器用自己的时钟）。
func publishCompletion(ctx context.Context, js fnats.IJetStream, layout Layout, completion Completion, partition int) error {
	if partition < 0 || partition >= layout.Partitions {
		return fmt.Errorf("%w: result for partition %d of %d", ErrInvalidRecord, partition, layout.Partitions)
	}
	completion.CompletedAt = time.Time{}
	if err := completion.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(resultInput{V: LogWireVersion, Completion: completion})
	if err != nil {
		return err
	}
	_, err = js.Publish(ctx, layout.ResultSubject(partition), data, fnats.JetStreamPublishOptions{MsgID: "res." + completion.CommandID})
	return err
}

// decodeCommandEvent 从分区命令事件里取出命令：事件版本、命令合法、命令属于事件里的 saga 与分区。错误都是坏信封（Term）。
// 消费者不需要事件里的 saga 状态，只校验它与命令一致。
func decodeCommandEvent(message *fnats.JetStreamMsg) (Command, error) {
	if message == nil || len(message.Data) > maxWireEnvelopeBytes {
		return Command{}, ErrInvalidRecord
	}
	var ev logEvent
	if err := json.Unmarshal(message.Data, &ev); err != nil {
		return Command{}, fmt.Errorf("%w: command event: %v", ErrInvalidRecord, err)
	}
	switch {
	case ev.V != LogWireVersion:
		return Command{}, fmt.Errorf("%w: command event wire version %d, want %d", ErrInvalidRecord, ev.V, LogWireVersion)
	case ev.Command == nil:
		return Command{}, errors.Join(ErrInvalidRecord, errors.New("saga: event carries no command"))
	case ev.Command.Validate() != nil, ev.Command.SagaID != ev.Saga.Record.ID, ev.Part < 0, int(ev.Command.Partition) != ev.Part:
		return Command{}, fmt.Errorf("%w: command event for saga %s on partition %d", ErrInvalidRecord, ev.Saga.Record.ID, ev.Part)
	}
	return *ev.Command, nil
}
