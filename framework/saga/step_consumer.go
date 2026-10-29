package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

// step_consumer.go 是参与方的步骤消费者（设计 §5）：命令是 JetStream 协调器分区里的命令事件
// （<prefix>.<k>.ev.cmd.<topic>），每条流一个 durable（同名，过滤全部分区的这个主题）。执行后把结果直接发到命令所属分区的
// in.res（MsgID res.<CommandID>），PubAck 之后才 ack 命令；没有 outbox、没有租约、不经 DataEngine effect。
//
// 结算规则（原生与 Mongo 相同）：
//   - 坏信封 → Term + saga.consumer.rejected_total（启动中继同一口径）；
//   - Admit 拒绝 → 原样返回（nak，留给能执行它的进程）；
//   - 执行 / 回放出错 → 结果未知，nak 退避重投，重投时读到回执就回放；
//   - 过期未执行 → 直接 ack，计 saga.step.expired_unexecuted_total；
//   - 执行或回放 → 发结果，PubAck 之后 ack；发布失败 → nak（重投回放同一回执，同一 MsgID 在去重窗口内被服务端丢弃，协调器也按回执去重）。

// DefaultStepAckWait 是步骤消费者不写 AckWait 时的取值；生成的与 demo 的步骤消费者都用它。步骤 Timeout 必须短于它，
// 否则 JetStream 会在 handler 还在跑时把命令重投给另一个消费者；kit 的 saga Mod 在启动前按它校验 saga.step_defaults /
// saga.steps 与定义里的步骤超时。
const DefaultStepAckWait = 30 * time.Second

// maxWireEnvelopeBytes 是消费者接受的一条消息的上限（超过按坏信封 Term）。
const maxWireEnvelopeBytes = 8 << 20

// StepHandler 是 Mongo 步骤的业务：在可重试的 MongoDB 事务内运行，只能经传入的 ctx 修改 MongoDB（驱动允许再次调用事务回调）。
//
// 业务写经这个事务 ctx 写进同一个 Mongo 事务时，框架保证同一操作实例（Command.IdempotencyKey）的所有尝试里至多一次生效：
// 回执文档与业务写在同一事务里，同一操作的两次尝试并发时 Mongo 写冲突让后者整体重跑、读到前者的回执回放（MongoStepInbox）。
// 不在事务里的副作用（调用另一个服务、发邮件）不受这个保证：被回滚的尝试可能已经发出调用，这类步骤仍要按 IdempotencyKey 幂等。
type StepHandler func(context.Context, Command) (Completion, error)

// StepConsumerConfig 配置一个步骤的命令消费者：每条 saga 流上一个同名 durable，过滤全部分区的这个主题。
type StepConsumerConfig struct {
	// Durable 是消费者名（同一服务的全部副本共用）；Topic 是步骤的正向或补偿主题。
	Durable, Topic string
	// AckWait 不写取 DefaultStepAckWait。必须长于路由到这个主题的每个步骤的 Timeout；一次处理的时长以它为界。
	AckWait       time.Duration
	MaxDeliver    int
	MaxAckPending int
	NakBackoffMin time.Duration
	NakBackoffMax time.Duration

	// Admit 判定本进程能否执行这条命令，在执行之前询问。
	//
	// 一个服务的所有进程共享一个 durable：关于某个对象的命令投给空闲的消费者，而不是对象的所有者。不能碰这个对象的进程
	// 必须说“不”。非 nil 错误原样返回给投递，消息按消费者的退避 nak 后重新提供给任意消费者（包括能执行它的那个）。所以
	// Admit 必须廉价、无副作用，且不能处处拒绝：没有任何进程准入的命令会一直重投到 MaxDeliver。nil 表示全部准入。
	Admit func(context.Context, Command) error

	// NativeWorkers 是原生步骤消费者同时在途的处理数（每个订阅一份）：一次处理含一次 Nest 请求（WAL 落盘），逐条串行会把
	// 整条流的吞吐压到一次落盘一条；同一实体的事务仍由 Nest 实体锁串行。不写取 min(32, MaxAckPending)，不能超过 MaxAckPending。
	NativeWorkers int
	// MongoWorkers 是 Mongo 步骤的并发上限；0/1 串行，不能超过 MaxAckPending。启用后业务 handler 必须允许不同命令并发。
	MongoWorkers int
	// ReceiptRetention 是回执保留期（saga.receipt_retention）：原生步骤 Complete 顺手剪掉更早的回执，Mongo 步骤回执的 TTL
	// 由 MongoStepInbox 自己的保留期决定。不写取 DefaultStepReceiptRetention。
	ReceiptRetention time.Duration
}

// defaultStepWorkers 是消费者在途处理数的缺省上限（每个订阅一份）：原生步骤的一次处理含一次 Nest 请求（WAL 落盘），
// 回调逐条串行会把整条流的步骤吞吐压到一次 fsync 一条；同一实体的事务仍由 Nest 实体锁串行。
const defaultStepWorkers = 32

// stepConsumerDefaults 补齐消费者的投递参数并校验。workers 是在途处理上限（0 取缺省，不超过 MaxAckPending）。
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

// SubscribeNativeStep 订阅原生步骤命令：每条命令经 RunNativeStep 运行步骤体（Nest handler 里 Begin 查回执、Complete 写回执），
// 按结论发结果或直接 ack。流来自布局；NativeWorkers 是在途处理上限。
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

// SubscribeMongoStep 订阅 Mongo 步骤命令：每条命令经
// MongoStepInbox.Handle 在一个 Mongo 事务里查回执、执行 handler、写回执，按结论发结果或直接 ack。MongoWorkers 是在途处理上限
// （缺省 1，串行；业务 handler 允许不同命令并发时再调大）。
func SubscribeMongoStep(ctx context.Context, js fnats.IJetStream, layout Layout, inbox *MongoStepInbox, config StepConsumerConfig, handler StepHandler) ([]fnats.IJetStreamSubscription, error) {
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

// publishCompletion 把一份结果追加到分区 partition 的 in.res（MsgID res.<CommandID>，与 Engine.Complete 同一形状），
// 只承诺“已持久进入输入”（PubAck）；接收与否由分区循环判定。CompletedAt 不上线（协调器用自己的时钟）。
func publishCompletion(ctx context.Context, js fnats.IJetStream, layout Layout, completion Completion, partition int) error {
	if partition < 0 || partition >= layout.Partitions {
		return fmt.Errorf("%w: result for partition %d of %d", ErrInvalidRecord, partition, layout.Partitions)
	}
	completion.CompletedAt = time.Time{}
	if err := completion.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(resultInput{V: WireVersion, Completion: completion})
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
	case ev.V != WireVersion:
		return Command{}, fmt.Errorf("%w: command event wire version %d, want %d", ErrInvalidRecord, ev.V, WireVersion)
	case ev.Command == nil:
		return Command{}, errors.Join(ErrInvalidRecord, errors.New("saga: event carries no command"))
	case ev.Command.Validate() != nil, ev.Command.SagaID != ev.Saga.Record.ID, ev.Part < 0, int(ev.Command.Partition) != ev.Part:
		return Command{}, fmt.Errorf("%w: command event for saga %s on partition %d", ErrInvalidRecord, ev.Saga.Record.ID, ev.Part)
	}
	return *ev.Command, nil
}

func validSubjectPath(subject string) bool {
	if subject == "" || len(subject) > 256 || strings.TrimSpace(subject) != subject || strings.ContainsAny(subject, "*> \t\r\n") {
		return false
	}
	for _, token := range strings.Split(subject, ".") {
		if token == "" {
			return false
		}
	}
	return true
}

func validDeliveryLimits(maxDeliver, maxAckPending int, nakMin, nakMax time.Duration) bool {
	return maxDeliver > 0 && maxDeliver <= 1_000_000 && maxAckPending > 0 && maxAckPending <= 65_536 && nakMin > 0 && nakMax >= nakMin && nakMax <= 24*time.Hour
}

// withConsumerWorkers 把处理放进有界的在途槽；结算仍由原订阅负责，Drain 等待所有延迟结算。同一操作的并发由实体锁与回执
// （原生）或 Mongo 事务写冲突（Mongo 步骤）裁决，不在这里另建按 ID 的锁或重试队列。
func withConsumerWorkers(workers int, handler fnats.JetStreamHandler) fnats.JetStreamHandler {
	if workers <= 1 {
		return handler
	}
	slots := make(chan struct{}, workers)
	return func(ctx context.Context, message *fnats.JetStreamMsg) error {
		if message == nil || message.DeferSettle == nil {
			return handler(ctx, message)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			<-slots
			return err
		}
		settle := message.DeferSettle()
		if settle == nil {
			defer func() { <-slots }()
			return handler(ctx, message)
		}
		go func() {
			var err error
			defer func() {
				if value := recover(); value != nil {
					err = fmt.Errorf("saga: consumer handler panic: %v", value)
				}
				settle(err)
				<-slots
			}()
			err = handler(ctx, message)
		}()
		return nil
	}
}
