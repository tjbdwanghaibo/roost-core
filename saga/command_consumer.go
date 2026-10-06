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

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	kitnats "github.com/tjbdwanghaibo/roost-core/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// StepHandler runs inside a retryable MongoDB transaction. It may only mutate
// MongoDB through the supplied context. Network calls and other irreversible
// side effects must be emitted through a transactional outbox because the
// MongoDB driver is allowed to invoke a transaction callback again.
//
// 业务写经这个事务 ctx 写进同一个 Mongo 事务时，框架保证同一操作实例（Command.IdempotencyKey）的所有尝试里至多一次
// 生效（SAGA.md「原生步骤执行契约」）；业务按 IdempotencyKey 自己做幂等只是可选的纵深防御。不在事务里的副作用（调用另一个
// 服务）不受这个保证：被 fence 中止的尝试可能已经发出调用，这类步骤仍要按 IdempotencyKey 幂等。
type StepHandler func(context.Context, Command) (Completion, error)

// MongoCommandInbox 是 Mongo 步骤的收件箱（saga 方向 ②）。与原生步骤共用操作实例的 claim / 守卫 / 判定
// （stepOperationInbox，claim 与守卫在 `<collection>_claims`），回执仍以 CommandID 为 `_id` 写在 collection 里（格式不变）。
// 生效点是 handler 的 Mongo 事务：业务写、对自己 claim 的条件写（settleOwnClaim）与回执在同一事务提交。
type MongoCommandInbox struct {
	stepOperationInbox
	collection string
}

// CommandInboxOptions 配置 MongoCommandInbox。
type CommandInboxOptions struct {
	ReceiptTTL time.Duration
	// Owner 是这个收件箱实例在 claim 上的名字，同一操作实例的尝试据此区分租约持有者；缺省 saga-mongo-inbox-<随机 ID>。
	Owner string
	// LeaseDuration 是一次尝试的租约上限，实际租约再封顶到命令截止；缺省 1 分钟。
	LeaseDuration time.Duration
}

// mongoInboxClaimSuffix：Mongo 步骤的 claim / 守卫集合是收件箱集合名加这个后缀（与回执分开，回执格式不变）。
const mongoInboxClaimSuffix = "_claims"

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
		client: client, database: database, claimCollection: collection + mongoInboxClaimSuffix,
		owner: option.Owner, leaseDuration: option.LeaseDuration, receiptTTL: option.ReceiptTTL,
		now: time.Now, receipt: inbox.findReceipt,
	}
	return inbox, nil
}

func (i *MongoCommandInbox) EnsureInfrastructure(ctx context.Context) error {
	ttl := int64(i.receiptTTL / time.Second)
	if ttl <= 0 || ttl > int64(^uint32(0)>>1) {
		return fmt.Errorf("saga inbox: invalid receipt ttl %s", i.receiptTTL)
	}
	if err := i.collectionRef().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: int32(ttl)}}); err != nil {
		return err
	}
	return i.ensureClaimIndexes(ctx)
}

// Handle 执行一次 Mongo 步骤尝试（SAGA.md「原生步骤执行契约」）：
//
//  1. Reserve 事务（与原生步骤同一份判定）：本命令已有回执 → 回放；同一操作实例另一次尝试已成功（任何一生）或
//     本生已拒绝 → 回放那次的 completion（CommandID 是那次的），不执行；另一次尝试或同一命令的另一次投递持有有效租约
//     → errOperationAttemptInFlight；租约过期 → 接替；命令已过截止 → ErrCommandExpired；被接替 → errAttemptSuperseded。
//  2. 执行事务：handler → 对自己 claim 的条件写（租约未过期、未被接替）→ 插入回执。条件写不匹配 → errAttemptFenced，
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
		// 同一 CommandID 的回执已被别人先提交（升级前的进程不写 claim，只靠回执的唯一 _id 排他）：
		// 本次事务已中止，回放那份回执。
		//
		// 这里不交还、也不标记自己的 claim（发版前审查观察，列为观察不改）：它仍是 pending、租约有效，但读 claim 的
		// 每条路径都先看回执——同一命令重投在 reserveInTransaction 第 1 步读到回执回放并顺手标记 completed；同一操作
		// 实例的其他尝试经 attemptResult 读到这份回执，标记后回放，不会因为这份租约等待（resolveOtherAttempts 只对
		// 没有结论的尝试看租约）。交还租约没有可观察的差别，所以不加这一步。
		if replayed, readErr := i.readReceipt(ctx, command.ID, digest); readErr == nil {
			return replayed, true, nil
		}
	}
	if err != nil {
		// 事务没有提交（handler 失败、取消、fence）：交还租约，重投不必等它自然过期。提交结果未知时 claim 若已
		// completed，交还的条件不匹配，不改动；重投读到回执回放。
		if releaseErr := i.releaseLease(context.WithoutCancel(ctx), reservation); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("saga inbox: release unused step lease: %w", releaseErr))
		}
		return Completion{}, false, err
	}
	return completion, false, nil
}

// execute 是 Mongo 步骤的生效点：handler 的业务写、对自己 claim 的条件写与回执在同一个 Mongo 事务里提交。
func (i *MongoCommandInbox) execute(ctx context.Context, command Command, digest []byte, reservation Reservation, handler StepHandler) (Completion, error) {
	session, err := i.client.StartSession(ctx)
	if err != nil {
		return Completion{}, err
	}
	defer session.EndSession(ctx)
	var completion Completion
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		var handlerErr error
		// command is already private to this delivery; passing it by value avoids
		// another payload-sized allocation on the step hot path.
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
		if err := i.settleOwnClaim(txCtx, reservation, completion); err != nil {
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

// Replay returns a completion already committed for this exact command. It
// never invokes business code and is used to finish publishing after a step
// attempt deadline has elapsed.
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

type StepConsumerConfig struct {
	Stream, Durable, Topic string
	AckWait                time.Duration
	MaxDeliver             int
	MaxAckPending          int
	NakBackoffMin          time.Duration
	NakBackoffMax          time.Duration

	// Admit decides whether THIS process may run the command, and it is asked
	// before the consumer takes anything for it.
	//
	// The question exists because one durable is shared by every process of a
	// service: a command about some object is delivered to whichever consumer
	// is free, not to the one that owns the object. A process that may not
	// touch that object has to say so — and it has to say so before the
	// inbox reserves the command, because a reservation is a lease under this
	// process's name. Refusing inside the handler is too late: the rightful
	// owner then finds the command claimed by somebody else and cannot run it
	// until the lease expires, while the message bounces between consumers.
	//
	// A non-nil error is returned to the delivery unchanged, so the message is
	// nak'd with the consumer's backoff and offered again — to any consumer,
	// including the one that may run it. Admit must therefore be cheap, free
	// of side effects, and it must not refuse everywhere: a command no process
	// admits is redelivered until MaxDeliver.
	//
	// nil admits everything, which is what a single-process deployment wants.
	Admit func(context.Context, Command) error
}

func SubscribeMongoStep(ctx context.Context, client fnats.IJetStream, transport *JetStreamPublisher, inbox *MongoCommandInbox, config StepConsumerConfig, handler StepHandler) (fnats.IJetStreamSubscription, error) {
	if client == nil || transport == nil || inbox == nil || handler == nil || config.Stream == "" || config.Durable == "" || !validSubjectPath(config.Topic) {
		return nil, fmt.Errorf("saga: invalid step consumer configuration")
	}
	if config.AckWait <= 0 {
		config.AckWait = 30 * time.Second
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
		if message == nil {
			return kitnats.Permanent(ErrInvalidRecord)
		}
		if len(message.Data) > maxWireEnvelopeBytes {
			return kitnats.Permanent(ErrInvalidRecord)
		}
		var envelope commandEnvelope
		if err := json.Unmarshal(message.Data, &envelope); err != nil {
			logConsumerError("step decode", message, err)
			return kitnats.Permanent(err)
		}
		if envelope.Version != WireVersion {
			return kitnats.Permanent(ErrInvalidRecord)
		}
		command := envelope.Command
		if err := command.Validate(); err != nil {
			logConsumerError("step command", message, err)
			return kitnats.Permanent(err)
		}
		if !time.Now().Before(command.DeadlineAt) {
			// The coordinator owns timeout/retry. Do not begin new business work,
			// but replay a completion which committed before an earlier publish
			// failed so it is not needlessly re-executed as a new attempt.
			// 没有自己的回执时，同一操作实例另一次尝试已生效的成功可能在协调器退避期间被丢弃，而这条可能是最后一次
			// 尝试：ack 前把那次成功重发（与原生步骤相同，U-0280 复核 2）。
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

// SubscribeStep is retained for raw Mongo handlers.
// Deprecated: use SubscribeMongoStep or SubscribeDataEngineStep explicitly.
func SubscribeStep(ctx context.Context, client fnats.IJetStream, transport *JetStreamPublisher, inbox *MongoCommandInbox, config StepConsumerConfig, handler StepHandler) (fnats.IJetStreamSubscription, error) {
	return SubscribeMongoStep(ctx, client, transport, inbox, config, handler)
}

// SubscribeDataEngineStep coordinates duplicate deliveries, but never
// publishes the completion directly. A native handler must execute its Nest
// transaction with inbox.Bind(command, reservation) and
// EmitCompletion; obtain reservation explicitly with
// ReservationFromContext(ctx). This
// consumer acknowledges only after the authoritative Data Engine receipt is
// projected and replayable.
func SubscribeDataEngineStep(ctx context.Context, client fnats.IJetStream, transport *JetStreamPublisher, inbox *DataEngineStepInbox, config StepConsumerConfig, handler StepHandler) (fnats.IJetStreamSubscription, error) {
	if client == nil || transport == nil || inbox == nil || handler == nil || config.Stream == "" || config.Durable == "" || !validSubjectPath(config.Topic) {
		return nil, fmt.Errorf("saga: invalid dataengine step consumer configuration")
	}
	if config.AckWait <= 0 {
		config.AckWait = 30 * time.Second
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
	if !validDeliveryLimits(config.MaxDeliver, config.MaxAckPending, config.NakBackoffMin, config.NakBackoffMax) || inbox.options.LeaseDuration <= config.AckWait {
		return nil, fmt.Errorf("saga: unsafe dataengine step consumer limits")
	}
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
			return err
		}
		if !time.Now().Before(command.DeadlineAt) {
			// 过了截止时间：协调器已按超时自行进入下一次尝试或补偿，不再等这次尝试的回答，
			// 这里也不再开始业务。有回执就把 claim 标成完成后 ack（completion 本身随投影的
			// effect 送达协调器）；没有回执同样 ack。
			//
			// 无回执时 ack 不会丢掉“已执行、回执还没投影”的尝试：那次尝试的结果走 WAL → 投影 →
			// completion effect，从不依赖这条消息，ack 不影响它投影与送达。旧实现在这里返回
			// context.DeadlineExceeded，按退避一直 nak 到 MaxDeliver（默认约 8.7 天），长期占住
			// 共享 durable 的 MaxAckPending，新命令全部超时（U-0281）。读回执出错时仍返回错误重投：
			// 那时无法判断有没有回执。
			_, found, replayErr := inbox.Replay(messageCtx, command)
			if replayErr != nil {
				return replayErr
			}
			if !found {
				// 这次尝试不执行，但同一操作实例另一次尝试的成功可能在退避期间被协调器丢弃，而这条可能是
				// 最后一次尝试：ack 前把那次成功重发，协调器还在等就接收，已放弃就告警（U-0280 复核）。
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
		reservation, err := inbox.Reserve(processCtx, command)
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
				// 同一操作实例较早的一次尝试已经有结果（U-0280）：这次尝试不执行，把那次的 completion 经 saga
				// 结果流重发给协调器。那次尝试自己的 completion effect 可能在协调器退避期间到达、已被丢弃；
				// 协调器正在等这个操作实例，会按 IdempotencyKey 接收它，已接收过则按回执去重。
				metrics.IncCounter("saga.step.attempt_replayed_total", nil, 1)
				return transport.PublishCompletion(processCtx, reservation.Completion)
			}
			return nil
		}
		if !reservation.Duplicate {
			processCtx = withReservation(processCtx, reservation)
			// 原生 handler 的结果是它在 Nest 事务里 EmitCompletion 的那一份，随回执与 effect 一起提交；
			// 返回值不使用，也不校验（生成模板返回零值 Completion）。旧实现校验它，零值让每次成功执行后
			// 都多一次 nak 与重投，重投时才读到回执 ack。
			if _, err := handler(processCtx, command); err != nil {
				if errors.Is(err, coredata.ErrFencedEntityPending) {
					// 实体上还有一笔未确定结果的原生步骤（常见是本命令上一次投递的记录）：本次事务已整体
					// 回滚，交还租约，让屏障解除后的重投能立刻重新 Reserve（RR-20260926-30）。
					if releaseErr := inbox.releaseLease(context.WithoutCancel(messageCtx), reservation); releaseErr != nil {
						err = errors.Join(err, fmt.Errorf("saga: release unused step lease: %w", releaseErr))
					}
				}
				return err
			}
		}
		_, err = inbox.waitReplay(processCtx, command)
		return err
	})
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

// commandDigest is the identity the inbox compares on redelivery. A command
// that cannot be marshalled has no identity: returning an error here is what
// keeps every such command from collapsing onto sha256(nil) and being mistaken
// for a redelivery of any other (B-14).
func commandDigest(c Command) ([]byte, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("%w: command digest: %v", ErrInvalidRecord, err)
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}
