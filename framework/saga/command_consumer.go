package saga

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// StepHandler 在可重试的 MongoDB 事务内运行，只能经传入的 ctx 修改 MongoDB。网络调用等不可逆副作用必须经事务性
// outbox 发出：MongoDB 驱动允许再次调用事务回调。
//
// 业务写经这个事务 ctx 写进同一个 Mongo 事务时，框架保证同一操作实例（Command.IdempotencyKey）的所有尝试里至多一次
// 生效（SAGA.md「原生步骤执行契约」）；业务按 IdempotencyKey 自己做幂等只是可选的纵深防御。不在事务里的副作用（调用另一个
// 服务）不受这个保证：被 fence 中止的尝试可能已经发出调用，这类步骤仍要按 IdempotencyKey 幂等。
type StepHandler func(context.Context, Command) (Completion, error)

// MongoCommandInbox 是 Mongo 步骤的收件箱（saga 方向 ②）。与原生步骤共用操作实例的状态文档与判定
// （stepOperationInbox，状态文档在 `<collection>_operations`），回执仍以 CommandID 为 `_id` 写在 collection 里（格式不变）。
// 生效点是 handler 的 Mongo 事务：业务写、对状态文档的条件写（settleOwnAttempt）与回执在同一事务提交。
type MongoCommandInbox struct {
	stepOperationInbox
	collection string
}

// CommandInboxOptions 配置 MongoCommandInbox。
type CommandInboxOptions struct {
	ReceiptTTL time.Duration
	// Owner 是这个收件箱实例在状态文档租约上的名字，同一操作实例的尝试据此区分租约持有者；缺省 saga-mongo-inbox-<随机 ID>。
	Owner string
	// LeaseDuration 是一次尝试的租约上限，实际租约再封顶到命令截止；缺省 1 分钟。
	LeaseDuration time.Duration
}

// mongoInboxOperationSuffix：Mongo 步骤的操作状态文档集合是收件箱集合名加这个后缀（与回执分开，回执格式不变）。
const mongoInboxOperationSuffix = "_operations"

// NewMongoCommandInbox 构造 Mongo 步骤收件箱；回执 TTL、租约持有者名与租约上限取 CommandInboxOptions 或缺省值。
func NewMongoCommandInbox(client fmongo.IMongo, database, collection string, options ...CommandInboxOptions) (*MongoCommandInbox, error) {
	database = strings.TrimSpace(database)
	collection = strings.TrimSpace(collection)
	if client == nil || database == "" {
		return nil, fmt.Errorf("saga inbox: client and database are required")
	}
	if collection == "" {
		collection = "_saga_step_inbox"
	}
	var option CommandInboxOptions
	if len(options) > 0 {
		option = options[0]
	}
	if option.ReceiptTTL <= 0 {
		option.ReceiptTTL = 30 * 24 * time.Hour
	}
	if strings.TrimSpace(option.Owner) == "" {
		option.Owner = "saga-mongo-inbox-" + NewID()
	}
	if option.LeaseDuration <= 0 {
		option.LeaseDuration = time.Minute
	}
	inbox := &MongoCommandInbox{collection: collection}
	inbox.stepOperationInbox = stepOperationInbox{
		client: client, database: database, operationCollection: collection + mongoInboxOperationSuffix,
		owner: option.Owner, leaseDuration: option.LeaseDuration, receiptTTL: option.ReceiptTTL,
		now: time.Now, receipt: inbox.findReceipt,
	}
	return inbox, nil
}

// EnsureInfrastructure 建回执集合的 TTL 索引与操作状态文档集合的索引。
func (i *MongoCommandInbox) EnsureInfrastructure(ctx context.Context) error {
	ttl := int64(i.receiptTTL / time.Second)
	if ttl <= 0 || ttl > int64(^uint32(0)>>1) {
		return fmt.Errorf("saga inbox: invalid receipt ttl %s", i.receiptTTL)
	}
	if err := i.collectionRef().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: int32(ttl)}}); err != nil {
		return err
	}
	return i.ensureOperationIndexes(ctx)
}

// Handle 执行一次 Mongo 步骤尝试（SAGA.md「原生步骤执行契约」）：
//
//  1. Reserve 事务（与原生步骤同一份判定）：本命令已有回执 → 回放；同一操作实例另一次尝试已成功（任何一生）或
//     本生已拒绝 → 回放那次的 completion（CommandID 是那次的），不执行；另一次尝试或同一命令的另一次投递持有有效租约
//     → errOperationAttemptInFlight；租约过期 → 接替；命令已过截止 → ErrCommandExpired；被接替 → errAttemptSuperseded。
//  2. 执行事务：handler → 对状态文档的条件写（仍是当前尝试、租约未过期）→ 插入回执。条件写不匹配 → errAttemptFenced，
//     事务中止，业务写随之回滚。
//
// 执行事务失败时交还租约，重投能立刻重试。duplicate=true 表示返回的是已有结果的回放。
func (i *MongoCommandInbox) Handle(ctx context.Context, command Command, handler StepHandler) (Completion, bool, error) {
	if i == nil || i.client == nil || handler == nil {
		return Completion{}, false, ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return Completion{}, false, err
	}
	digest, err := commandDigest(command)
	if err != nil {
		return Completion{}, false, err
	}
	reservation, err := i.reserve(ctx, command, digest)
	if err != nil {
		return Completion{}, false, err
	}
	if reservation.Duplicate {
		if reservation.Completion.CommandID != "" {
			return reservation.Completion, true, nil
		}
		// 同一命令的另一次投递持有有效租约：它会自己发布结果；这次投递 nak，重投时读回执。
		return Completion{}, false, errOperationAttemptInFlight
	}
	completion, err := i.execute(ctx, command, digest, reservation, handler)
	if errors.Is(err, fmongo.ErrDuplicateKey) {
		// 同一 CommandID 的回执已被别人先提交：本次事务已中止，回放那份回执。正常不可达（同一命令的两次投递由租约
		// 串行，后一次在 Reserve 第 1 步就读到回执），留作纵深防御。这里不交还租约：读状态文档的每条路径都先看回执
		// （本命令重投在 Reserve 第 1 步回放并结算；其他尝试在第 9 步读到当前尝试的回执，结算后回放），不会因为这份租约等待。
		if replayed, readErr := i.readReceipt(ctx, command.ID, digest); readErr == nil {
			return replayed, true, nil
		}
	}
	if err != nil {
		// 事务没有提交（handler 失败、取消、fence）：交还租约，重投不必等它自然过期。提交结果未知时状态若已
		// settled，交还的条件不匹配，不改动；重投读到回执回放。
		if releaseErr := i.releaseLease(context.WithoutCancel(ctx), reservation); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("saga inbox: release unused step lease: %w", releaseErr))
		}
		return Completion{}, false, err
	}
	return completion, false, nil
}

// execute 是 Mongo 步骤的生效点：handler 的业务写、对状态文档的条件写与回执在同一个 Mongo 事务里提交。
func (i *MongoCommandInbox) execute(ctx context.Context, command Command, digest []byte, reservation Reservation, handler StepHandler) (Completion, error) {
	session, err := i.client.StartSession(ctx)
	if err != nil {
		return Completion{}, err
	}
	defer session.EndSession(ctx)
	var completion Completion
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		var handlerErr error
		// command 已归本次投递私有，按值传递，步骤热路径上不再多一次载荷大小的分配。
		completion, handlerErr = handler(txCtx, command)
		if handlerErr != nil {
			return handlerErr
		}
		completion.CommandID = command.ID
		completion.IdempotencyKey = command.IdempotencyKey
		completion.SagaID = command.SagaID
		if completion.CompletedAt.IsZero() {
			completion.CompletedAt = time.Now().UTC()
		}
		if err := completion.Validate(); err != nil {
			return err
		}
		raw, marshalErr := json.Marshal(completion)
		if marshalErr != nil {
			return marshalErr
		}
		// fence 放在业务写之后、提交之前：检查与提交之间只剩提交本身的延迟。
		if err := i.settleOwnAttempt(txCtx, reservation, completion); err != nil {
			return err
		}
		_, insertErr := i.collectionRef().InsertOne(txCtx, commandReceiptDoc{ID: command.ID, Digest: digest, Completion: raw, CreatedAt: time.Now().UTC()})
		return insertErr
	})
	return completion, err
}

// findReceipt 是 stepOperationInbox 读回执的入口：没有回执 found=false。
func (i *MongoCommandInbox) findReceipt(ctx context.Context, commandID string, digest []byte) (Completion, bool, error) {
	completion, err := i.readReceipt(ctx, commandID, digest)
	if errors.Is(err, fmongo.ErrNotFound) {
		return Completion{}, false, nil
	}
	return completion, err == nil, err
}

func (i *MongoCommandInbox) readReceipt(ctx context.Context, commandID string, digest []byte) (Completion, error) {
	var receipt commandReceiptDoc
	if err := i.collectionRef().FindOne(ctx, bson.M{"_id": commandID}, &receipt); err != nil {
		return Completion{}, err
	}
	if !bytes.Equal(receipt.Digest, digest) || len(receipt.Completion) == 0 {
		return Completion{}, ErrIdentityConflict
	}
	var completion Completion
	if err := json.Unmarshal(receipt.Completion, &completion); err != nil {
		return Completion{}, err
	}
	return completion, nil
}

// Replay 返回这条命令已提交的 completion。从不调用业务代码，用于步骤尝试截止之后补完发布。
func (i *MongoCommandInbox) Replay(ctx context.Context, command Command) (Completion, bool, error) {
	if i == nil || i.client == nil {
		return Completion{}, false, ErrInvalidRecord
	}
	digest, err := commandDigest(command)
	if err != nil {
		return Completion{}, false, err
	}
	return i.findReceipt(ctx, command.ID, digest)
}

// DefaultStepAckWait 是步骤消费者不写 AckWait 时的取值；生成的与 demo 的步骤消费者都用它。一次尝试的处理以命令截止
// （派发时刻 + 步骤 Timeout）为界，步骤 Timeout 必须短于它，否则 JetStream 会在 handler 还在跑时把命令重投给另一个
// 消费者；kit 的 saga Mod 在启动前按它校验 saga.step_defaults / saga.steps 与定义里的步骤超时。
const DefaultStepAckWait = 30 * time.Second

// StepConsumerConfig 配置步骤命令的 JetStream 持久消费者。
type StepConsumerConfig struct {
	Stream, Durable, Topic string
	// AckWait 不写取 DefaultStepAckWait。必须长于路由到这个主题的每个步骤的 Timeout；saga Mod 的启动校验按
	// DefaultStepAckWait 判，自己写更短的 AckWait 时要相应缩短步骤 Timeout。
	AckWait       time.Duration
	MaxDeliver    int
	MaxAckPending int
	NakBackoffMin time.Duration
	NakBackoffMax time.Duration

	// Admit 判定本进程能否执行这条命令，在消费者为它取得任何东西之前询问。
	//
	// 一个服务的所有进程共享一个 durable：关于某个对象的命令投给空闲的消费者，而不是对象的所有者。不能碰这个对象的
	// 进程必须说“不”，而且要在收件箱预留命令之前说——预留是以本进程名义持有的租约。在 handler 里拒绝就太晚了：
	// 真正的所有者会发现命令被别人领取，要等租约过期才能执行，消息则在消费者之间来回弹。
	//
	// 非 nil 错误原样返回给投递，消息按消费者的退避 nak 后重新提供给任意消费者（包括能执行它的那个）。所以 Admit 必须
	// 廉价、无副作用，且不能处处拒绝：没有任何进程准入的命令会一直重投到 MaxDeliver。
	//
	// nil 表示全部准入，单进程部署正需要这样。
	Admit func(context.Context, Command) error

	// ReceiptWaiters 只用于原生步骤消费者（SubscribeDataEngineStep）：同时在后台“等回执可见、结算状态文档、再 ack”的
	// 投递数上限（每个订阅一份）。步骤体返回后回调不在这里等回执投影，而是推迟这条消息的结算、立刻处理 durable 上的
	// 下一条命令；后台满了回调就等一个空位，与逐条等待同样回压。不写取 min(32, MaxAckPending)，不能超过 MaxAckPending。
	// 传输不支持推迟结算（JetStreamMsg.DeferSettle 为 nil，例如测试替身）时退回在回调里等。
	ReceiptWaiters int
}

// defaultReceiptWaiters 是 StepConsumerConfig.ReceiptWaiters 的缺省上限：正常负载下同时在等回执的投递只有几条
// （单条等待约等于一次投影，几十毫秒），32 已远超；它限制的是投影停顿时后台 goroutine 与轮询的规模。
const defaultReceiptWaiters = 32

// SubscribeMongoStep 订阅 Mongo 步骤命令：解码与校验信封（坏信封 Term + 告警），过了截止时间只回放已有结果，否则经
// 收件箱执行 handler 并发布 completion。
func SubscribeMongoStep(ctx context.Context, client fnats.IJetStream, transport *JetStreamPublisher, inbox *MongoCommandInbox, config StepConsumerConfig, handler StepHandler) (fnats.IJetStreamSubscription, error) {
	if client == nil || transport == nil || inbox == nil || handler == nil || config.Stream == "" || config.Durable == "" || !validSubjectPath(config.Topic) {
		return nil, fmt.Errorf("saga: invalid step consumer configuration")
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
	if !validDeliveryLimits(config.MaxDeliver, config.MaxAckPending, config.NakBackoffMin, config.NakBackoffMax) {
		return nil, fmt.Errorf("saga: unsafe step consumer limits")
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		return nil, err
	}
	subject := transport.prefix + ".command." + strings.Trim(config.Topic, ".")
	return client.Subscribe(ctx, fnats.JetStreamConsumerConfig{Stream: config.Stream, Name: config.Durable, Durable: config.Durable, FilterSubject: subject, DeliverPolicy: fnats.JetStreamDeliverAll, AckWait: config.AckWait, MaxDeliver: config.MaxDeliver, MaxAckPending: config.MaxAckPending, NakBackoffMin: config.NakBackoffMin, NakBackoffMax: config.NakBackoffMax}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		command, err := decodeStepCommand(message)
		if err != nil {
			logConsumerError("step decode", message, err)
			return rejectEnvelope("mongo_step", err)
		}
		if !time.Now().Before(command.DeadlineAt) {
			// 超时与重试归协调器。不再开始新的业务，但要回放此前已提交、只是发布失败的 completion，免得它作为新尝试
			// 被无谓地重新执行。没有自己的回执时，同一操作实例另一次尝试已生效的成功可能没有送达协调器，而这条可能是
			// 最后一次尝试：ack 前把那次成功重发（与原生步骤相同）。
			replayCtx, cancel := context.WithTimeout(messageCtx, 3*time.Second)
			defer cancel()
			completion, found, err := inbox.Replay(replayCtx, command)
			switch {
			case err == nil && found:
				err = transport.PublishCompletion(replayCtx, completion)
			case err == nil:
				err = ackUnexecutedAttempt(replayCtx, &inbox.stepOperationInbox, transport, command, ErrCommandExpired)
			}
			if err != nil {
				logConsumerError("stale step completion replay", message, err)
			}
			return err
		}
		if config.Admit != nil {
			if err := config.Admit(messageCtx, command); err != nil {
				return err
			}
		}
		processCtx, cancel := context.WithDeadline(messageCtx, command.DeadlineAt)
		defer cancel()
		completion, _, err := inbox.Handle(processCtx, command, handler)
		switch {
		case errors.Is(err, ErrCommandExpired), errors.Is(err, errAttemptFenced), errors.Is(err, errAttemptSuperseded):
			// 这次尝试不会（再）生效：截止已过、提交前失去租约，或已被较新的尝试接替。ack 前重发同一操作实例已生效的成功。
			// 用 messageCtx：processCtx 的截止就是命令截止，此刻可能已经过了。
			if replayErr := ackUnexecutedAttempt(messageCtx, &inbox.stepOperationInbox, transport, command, err); replayErr != nil {
				logConsumerError("step completion replay", message, replayErr)
				return replayErr
			}
			return nil
		case err != nil:
			// 包括 errOperationAttemptInFlight（另一次尝试还持有有效租约）与被命令截止打断的执行事务：nak，
			// 重投时再判断；过了截止的重投走上面的过期分支（提交结果未知时回执已在，过期分支回放它）。
			if !errors.Is(err, errOperationAttemptInFlight) {
				logConsumerError("step", message, err)
			}
			return err
		}
		// completion 可能是同一操作实例另一次尝试的结果（回放，CommandID 是那次的）：原样发布，协调器按它去重、核对代际。
		err = transport.PublishCompletion(processCtx, completion)
		if err != nil {
			logConsumerError("step completion publish", message, err)
		}
		return err
	})
}

// ackUnexecutedAttempt 处理一次不会执行的 Mongo 步骤投递：先把同一操作实例已生效的成功经 saga 结果流重发，
// 没有可重发的就计 saga.step.expired_unexecuted_total；返回 nil 表示可以 ack。
func ackUnexecutedAttempt(ctx context.Context, inbox *stepOperationInbox, transport *JetStreamPublisher, command Command, reason error) error {
	replayed, err := replayOperationSuccess(ctx, inbox, transport, command)
	if err != nil {
		return err
	}
	if !replayed {
		metrics.IncCounter("saga.step.expired_unexecuted_total", nil, 1)
		slog.Info("saga: step attempt will not run; acknowledged without executing",
			"command_id", command.ID, "saga_id", command.SagaID, "reason", reason)
	}
	return nil
}

// SubscribeDataEngineStep 订阅原生（DataEngine）步骤命令：协调重复投递，但从不直接发布 completion。原生 handler 必须用
// inbox.Bind(command, reservation) 执行它的 Nest 事务并 EmitCompletion，reservation 显式经 ReservationFromContext(ctx)
// 取得。消费者只在权威 DataEngine 回执已投影、可回放之后才 ack。
func SubscribeDataEngineStep(ctx context.Context, client fnats.IJetStream, transport *JetStreamPublisher, inbox *DataEngineStepInbox, config StepConsumerConfig, handler StepHandler) (fnats.IJetStreamSubscription, error) {
	if client == nil || transport == nil || inbox == nil || handler == nil || config.Stream == "" || config.Durable == "" || !validSubjectPath(config.Topic) {
		return nil, fmt.Errorf("saga: invalid dataengine step consumer configuration")
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
	if config.ReceiptWaiters <= 0 {
		config.ReceiptWaiters = min(defaultReceiptWaiters, config.MaxAckPending)
	}
	if !validDeliveryLimits(config.MaxDeliver, config.MaxAckPending, config.NakBackoffMin, config.NakBackoffMax) || inbox.options.LeaseDuration <= config.AckWait || config.ReceiptWaiters > config.MaxAckPending {
		return nil, fmt.Errorf("saga: unsafe dataengine step consumer limits")
	}
	// 后台等回执的空位：一个订阅一份，容量即同时等待的上限。
	receiptWaiters := make(chan struct{}, config.ReceiptWaiters)
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		return nil, err
	}
	subject := transport.prefix + ".command." + strings.Trim(config.Topic, ".")
	return client.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream: config.Stream, Name: config.Durable, Durable: config.Durable, FilterSubject: subject,
		DeliverPolicy: fnats.JetStreamDeliverAll, AckWait: config.AckWait, MaxDeliver: config.MaxDeliver,
		MaxAckPending: config.MaxAckPending, NakBackoffMin: config.NakBackoffMin, NakBackoffMax: config.NakBackoffMax,
	}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		command, err := decodeStepCommand(message)
		if err != nil {
			// 坏信封与 Mongo 步骤消费者同一口径：Term + 告警，不 nak 到 MaxDeliver。
			logConsumerError("native step decode", message, err)
			return rejectEnvelope("native_step", err)
		}
		// 逐跳计时：命令 PubAck → 本回调开始（JetStream 投递 + 在本 durable 的串行回调后排队），以及整个回调占住
		// durable 的时间。
		hoptrace.SinceMark("h04.step.deliver", "published/", command.ID)
		callbackStart := hoptrace.Now()
		defer hoptrace.Since("h04.step.callback", callbackStart)
		if !time.Now().Before(command.DeadlineAt) {
			// 过了截止时间：协调器已按超时自行进入下一次尝试或补偿，不再等这次尝试的回答，
			// 这里也不再开始业务。有回执就结算状态文档后 ack（completion 本身随投影的
			// effect 送达协调器）；没有回执同样 ack。
			//
			// 无回执时 ack 不会丢掉“已执行、回执还没投影”的尝试：那次尝试的结果走 WAL → 投影 →
			// completion effect，从不依赖这条消息，ack 不影响它投影与送达。若返回错误按退避一直 nak 到 MaxDeliver
			// （默认约 8.7 天），会长期占住共享 durable 的 MaxAckPending，新命令全部超时。读回执出错时仍返回错误重投：
			// 那时无法判断有没有回执。
			_, found, replayErr := inbox.Replay(messageCtx, command)
			if replayErr != nil {
				return replayErr
			}
			if !found {
				// 这次尝试不执行，但同一操作实例另一次尝试的成功可能没有送达协调器，而这条可能是
				// 最后一次尝试：ack 前把那次成功重发，协调器还在等就接收，已放弃就告警。
				replayed, err := replayOperationSuccess(messageCtx, &inbox.stepOperationInbox, transport, command)
				if err != nil {
					return err
				}
				if !replayed {
					metrics.IncCounter("saga.step.expired_unexecuted_total", nil, 1)
					slog.Info("saga: step command expired before it ran; acknowledged without executing",
						"command_id", command.ID, "saga_id", command.SagaID, "deadline_at", command.DeadlineAt)
				}
			}
			return nil
		}
		if config.Admit != nil {
			if err := config.Admit(messageCtx, command); err != nil {
				return err
			}
		}
		processCtx, cancel := context.WithDeadline(messageCtx, command.DeadlineAt)
		defer cancel()
		reserveStart := hoptrace.Now()
		reservation, err := inbox.Reserve(processCtx, command)
		hoptrace.Since("h05.step.reserve", reserveStart)
		switch {
		case errors.Is(err, ErrCommandExpired):
			// 认领前过了截止时间：不执行，ack；与上面的过期分支一样先重发同一操作实例已生效的成功。
			// 用 messageCtx：processCtx 的截止就是命令截止，此刻已经过了。
			replayed, replayErr := replayOperationSuccess(messageCtx, &inbox.stepOperationInbox, transport, command)
			if replayErr != nil {
				return replayErr
			}
			if !replayed {
				metrics.IncCounter("saga.step.expired_unexecuted_total", nil, 1)
				slog.Info("saga: step attempt will not run; acknowledged without executing",
					"command_id", command.ID, "saga_id", command.SagaID, "reason", err)
			}
			return nil
		case errors.Is(err, errAttemptSuperseded):
			// 这次尝试已被同一操作实例的较新尝试接替：它永远不会执行，ack（接替者负责回放或执行）。
			metrics.IncCounter("saga.step.expired_unexecuted_total", nil, 1)
			slog.Info("saga: step attempt will not run; acknowledged without executing",
				"command_id", command.ID, "saga_id", command.SagaID, "reason", err)
			return nil
		case err != nil:
			// 包括 errOperationAttemptInFlight：同一操作实例的另一次尝试还持有有效租约，nak 后重投时再判断。
			return err
		}
		if reservation.Duplicate && reservation.Completion.CommandID != "" {
			if reservation.Completion.CommandID != command.ID {
				// 同一操作实例较早的一次尝试已经有结果：这次尝试不执行，把那次的 completion 经 saga
				// 结果流重发给协调器。那次尝试自己的 completion effect 可能还没送达或已丢失；
				// 协调器正在等这个操作实例，会按 IdempotencyKey 接收它，已接收过则按回执去重。
				metrics.IncCounter("saga.step.attempt_replayed_total", nil, 1)
				return transport.PublishCompletion(processCtx, reservation.Completion)
			}
			return nil
		}
		if !reservation.Duplicate {
			processCtx = withReservation(processCtx, reservation)
			// 原生 handler 的结果是它在 Nest 事务里 EmitCompletion 的那一份，随回执与 effect 一起提交；
			// 返回值不使用，也不校验（生成模板返回零值 Completion；校验它会让每次成功执行后多一次 nak 与重投）。
			handlerStart := hoptrace.Now()
			_, err := handler(processCtx, command)
			hoptrace.Since("h06.step.handler", handlerStart)
			if err != nil {
				if errors.Is(err, coredata.ErrFencedEntityPending) {
					// 实体上还有一笔未确定结果的原生步骤（常见是本命令上一次投递的记录）：本次事务已整体
					// 回滚，交还租约，让屏障解除后的重投能立刻重新 Reserve。
					if releaseErr := inbox.releaseLease(context.WithoutCancel(messageCtx), reservation); releaseErr != nil {
						err = errors.Join(err, fmt.Errorf("saga: release unused step lease: %w", releaseErr))
					}
				}
				return err
			}
		}
		return awaitReceiptAndSettle(messageCtx, processCtx, message, inbox, command, receiptWaiters)
	})
}

// awaitReceiptAndSettle 是原生步骤消费者的最后一段：等本命令的回执投影可见、结算状态文档（Replay 里的 markCompleted），
// 然后 ack。回执只经 WAL → 投影产生，这段等待不影响结果本身，只决定何时 ack。
//
// 在回调里等会占住 durable 的串行回调（nats.go 逐条调用），一条命令的投影停顿就挡住同一 durable 后面的全部命令；
// 所以这里推迟这条消息的结算（JetStreamMsg.DeferSettle），把等待交给后台、回调立即返回：
//   - 有界：同时等待的数目不超过 waiters 的容量（ReceiptWaiters ≤ MaxAckPending）；满了在回调里等空位，截止前没等到
//     与修前一样返回截止错误（nak）。推迟中的消息仍未确认，broker 侧还受 MaxAckPending 约束。
//   - 结算与修前相同：回执可见 → ack；等到截止（命令截止，步骤超时短于 AckWait）或订阅 Stop（handler ctx 取消）→
//     返回的错误按退避 nak，重投走过期分支回放回执。租约、fence、恰好一次都不依赖 ack 的时机。
//   - Drain：订阅的 Closed 等全部推迟的结算完成，Steps.Drain / 调用方等到的就是后台 ack 完成。
//   - 传输不支持推迟（DeferSettle 为 nil，或订阅正在结束返回 nil）时在回调里等，行为与修前相同。
func awaitReceiptAndSettle(messageCtx, processCtx context.Context, message *fnats.JetStreamMsg, inbox *DataEngineStepInbox, command Command, waiters chan struct{}) error {
	if message.DeferSettle == nil {
		return waitReceipt(processCtx, inbox, command)
	}
	select {
	case waiters <- struct{}{}:
	case <-processCtx.Done():
		return processCtx.Err()
	}
	settle := message.DeferSettle()
	if settle == nil {
		<-waiters
		return waitReceipt(processCtx, inbox, command)
	}
	go func() {
		defer func() { <-waiters }()
		// processCtx 随回调返回被取消；后台按同一个截止重新派生，父 ctx 仍是 handler 的（Stop 时取消）。
		waitCtx, cancel := context.WithDeadline(messageCtx, command.DeadlineAt)
		defer cancel()
		settle(waitReceipt(waitCtx, inbox, command))
	}()
	return nil
}

// waitReceipt 等本命令的回执可见并结算状态文档。
func waitReceipt(ctx context.Context, inbox *DataEngineStepInbox, command Command) error {
	waitStart := hoptrace.Now()
	_, err := inbox.waitReplay(ctx, command)
	hoptrace.Since("h12.step.wait_receipt", waitStart)
	return err
}

// replayOperationSuccess 把同一操作实例另一次尝试已生效的成功经 saga 结果流重发给协调器，供不会执行的
// 投递在 ack 前调用。协调器已接收过那次成功则按回执去重；还在等这个操作则接收；已放弃则告警。
func replayOperationSuccess(ctx context.Context, inbox *stepOperationInbox, transport *JetStreamPublisher, command Command) (bool, error) {
	completion, found, err := inbox.operationSuccess(ctx, command)
	if err != nil || !found {
		return false, err
	}
	metrics.IncCounter("saga.step.attempt_replayed_total", nil, 1)
	return true, transport.PublishCompletion(ctx, completion)
}

// decodeStepCommand 解开步骤命令消息；错误都是坏信封，两个步骤消费者都以 rejectEnvelope Term。
func decodeStepCommand(message *fnats.JetStreamMsg) (Command, error) {
	if message == nil || len(message.Data) > maxWireEnvelopeBytes {
		return Command{}, ErrInvalidRecord
	}
	var envelope commandEnvelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		return Command{}, err
	}
	if envelope.Version != WireVersion || envelope.Command.Validate() != nil {
		return Command{}, ErrInvalidRecord
	}
	return envelope.Command, nil
}

type commandReceiptDoc struct {
	ID         string    `bson:"_id"`
	Digest     []byte    `bson:"digest"`
	Completion []byte    `bson:"completion"`
	CreatedAt  time.Time `bson:"created_at"`
}

func (i *MongoCommandInbox) collectionRef() fmongo.ICollection {
	return i.client.Database(i.database).Collection(i.collection)
}

// commandDigest 是收件箱在重投时比较的身份。无法序列化的命令没有身份：在这里返回错误，才不会让所有这类命令都塌缩到
// sha256(nil)、被误认作任意另一条命令的重投。
//
// 时间字段按时刻比较：摘要前统一换成 UTC。协调器写的就是 UTC（所以已有摘要不变），但命令可能经业务自己的通道转交
// （RunHandedOver 的调用方，例如 demo 用 bus 的 MessagePack，解出来是本地时区）：同一时刻换个时区，JSON 文本就不同，
// 修前同一命令 ID 在消费者与转交两条路径上互相 ErrIdentityConflict，消费者那条消息读不到自己的回执、nak 到 MaxDeliver。
func commandDigest(c Command) ([]byte, error) {
	c.DeadlineAt, c.CreatedAt = c.DeadlineAt.UTC(), c.CreatedAt.UTC()
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("%w: command digest: %v", ErrInvalidRecord, err)
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}
