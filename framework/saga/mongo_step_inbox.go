package saga

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// mongo_step_inbox.go 是 Mongo 步骤收件箱（设计 §5.2，选项 M-A）：按操作实例
// （Command.IdempotencyKey）一份回执文档，查回执、执行 handler、写回执在同一个 Mongo 事务里。
//
// 同一操作的两次尝试并发时，两笔事务写同一个回执文档：Mongo 的写冲突让后者整体重跑（驱动按 TransientTransactionError 重试
// 回调），重跑读到前者已提交的回执就回放；后者的业务写随它的事务回滚。所以事务内的业务写至多生效一次，不需要旧收件箱的
// Reserve 事务、操作状态文档与租约。事务外的副作用（调用另一个服务）仍可能被执行两次，必须按 IdempotencyKey 幂等（现行要求）。

// mongoStepReceiptMaxRuns 是一次 Handle 因“回执被并发写入”（插入撞上已提交的同 _id）或“写前截止已过”重跑的次数上限。
// 正常情况下写冲突由驱动在事务内重试，这里只兜住驱动报成 DuplicateKey 的那一种与截止重判。
const mongoStepReceiptMaxRuns = 3

// MongoStepInbox 是 Mongo 步骤的回执存储与执行入口。
type MongoStepInbox struct {
	client     fmongo.IMongo
	database   string
	collection string
	retention  time.Duration
	now        func() time.Time
}

// mongoStepReceiptDoc 是一个操作实例的回执文档（_id = IdempotencyKey）。Retryable 是 handler 交来的可重试失败：它只对写它的
// 那次尝试（CommandID 相同）回放——协调器对同一个 CommandID 只接受一种结论——不挡同一操作的下一次尝试。
type mongoStepReceiptDoc struct {
	ID          string    `bson:"_id"`
	CommandID   string    `bson:"command_id"`
	Incarnation uint32    `bson:"incarnation"`
	Success     bool      `bson:"success"`
	Retryable   bool      `bson:"retryable,omitempty"`
	Error       string    `bson:"error,omitempty"`
	Data        []byte    `bson:"data,omitempty"`
	At          time.Time `bson:"at"`
}

// NewMongoStepInbox 构造收件箱。collection 必填（wiring/saga 的 Steps 用 `_<group>_step_inbox`）；
// retention ≤ 0 取 DefaultStepReceiptRetention，回执文档按 at + retention 由 TTL 索引删除。
func NewMongoStepInbox(client fmongo.IMongo, database, collection string, retention time.Duration) (*MongoStepInbox, error) {
	database, collection = strings.TrimSpace(database), strings.TrimSpace(collection)
	if client == nil || database == "" || collection == "" {
		return nil, errors.New("saga mongo step inbox: client, database and collection are required")
	}
	if retention <= 0 {
		retention = DefaultStepReceiptRetention
	}
	if seconds := retention / time.Second; seconds <= 0 || seconds > time.Duration(^uint32(0)>>1) {
		return nil, fmt.Errorf("saga mongo step inbox: invalid receipt retention %s", retention)
	}
	return &MongoStepInbox{client: client, database: database, collection: collection, retention: retention, now: time.Now}, nil
}

// EnsureInfrastructure 建回执集合的 TTL 索引（at + retention）。
func (i *MongoStepInbox) EnsureInfrastructure(ctx context.Context) error {
	return i.receipts().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "at", Value: 1}}, Name: "ttl_at", TTL: int32(i.retention / time.Second)}})
}

// Handle 执行一次 Mongo 步骤尝试，一个 Mongo 事务里：
//
//  1. 读回执：成功（任何代际）、本代际的拒绝、或本命令自己的可重试失败 → 回放，不执行；
//  2. 命令已过截止 → 过期未执行；
//  3. 执行 handler（事务 ctx，截止为命令截止）；
//  4. 写回执（没有就插入，有旧一生的拒绝或别的尝试的可重试失败就按原值条件替换），写前再查一次截止；过了截止中止事务
//     （业务写回滚）后重跑，重跑走第 1 步回放或第 2 步过期。
//
// 返回错误 = 结果未知或确定没提交（handler 失败、取消、提交结果未知）：调用方 nak，重投时第 1 步读到回执就回放。
func (i *MongoStepInbox) Handle(ctx context.Context, command Command, handler StepHandler) (StepOutcome, error) {
	if i == nil || i.client == nil || handler == nil {
		return StepOutcome{}, ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return StepOutcome{}, err
	}
	var err error
	for range mongoStepReceiptMaxRuns {
		var outcome StepOutcome
		outcome, err = i.handleOnce(ctx, command, handler)
		if errors.Is(err, fmongo.ErrDuplicateKey) || errors.Is(err, errMongoStepRerun) {
			continue
		}
		return outcome, err
	}
	return StepOutcome{}, err
}

// errMongoStepRerun 让 Handle 重跑一次（写前截止已过、或条件替换没匹配上）：事务已中止，业务写已回滚。
var errMongoStepRerun = errors.New("saga mongo step inbox: rerun the attempt")

func (i *MongoStepInbox) handleOnce(ctx context.Context, command Command, handler StepHandler) (StepOutcome, error) {
	session, err := i.client.StartSession(ctx)
	if err != nil {
		return StepOutcome{}, err
	}
	defer session.EndSession(ctx)
	incarnation := commandIDIncarnation(command.IdempotencyKey, command.ID)
	var outcome StepOutcome
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		outcome = StepOutcome{}
		var previous mongoStepReceiptDoc
		err := i.receipts().FindOne(txCtx, bson.M{"_id": command.IdempotencyKey}, &previous)
		found := err == nil
		if err != nil && !errors.Is(err, fmongo.ErrNotFound) {
			return err
		}
		if found && previous.replays(command, incarnation) {
			outcome = StepOutcome{Kind: StepReplayed, Completion: previous.completion(command)}
			return nil
		}
		if !i.now().Before(command.DeadlineAt) {
			outcome = StepOutcome{Kind: StepExpired}
			return nil
		}
		handlerCtx, cancel := context.WithDeadline(txCtx, command.DeadlineAt)
		completion, err := handler(handlerCtx, command)
		cancel()
		if err != nil {
			return err
		}
		completion.CommandID, completion.IdempotencyKey, completion.SagaID = command.ID, command.IdempotencyKey, command.SagaID
		if err := completion.Validate(); err != nil {
			return err
		}
		// 截止检查放在写回执之前（与旧收件箱“条件在事务最后一次写时检查”同一位置）：过了截止就不让这次执行生效。
		if !i.now().Before(command.DeadlineAt) {
			return errMongoStepRerun
		}
		doc := mongoStepReceiptDoc{
			ID: command.IdempotencyKey, CommandID: command.ID, Incarnation: incarnation,
			Success: completion.Success, Retryable: completion.Retryable, Error: completion.Error, Data: completion.Data, At: i.now().UTC(),
		}
		if !found {
			if _, err := i.receipts().InsertOne(txCtx, doc); err != nil {
				return err
			}
		} else {
			// 按读到的旧值条件替换：快照之后有别的事务改了它，Mongo 报写冲突让整个回调重跑；没匹配上同样重跑。
			result, err := i.receipts().ReplaceOne(txCtx, bson.M{"_id": command.IdempotencyKey, "command_id": previous.CommandID, "at": previous.At}, doc)
			if err != nil {
				return err
			}
			if result.MatchedCount != 1 {
				return errMongoStepRerun
			}
		}
		completion.CompletedAt = time.Time{}
		outcome = StepOutcome{Kind: StepExecuted, Completion: completion}
		return nil
	})
	if err != nil {
		return StepOutcome{}, err
	}
	return outcome, nil
}

// replays 是回放规则：成功对任何尝试回放；业务拒绝只对同一代际回放（补偿方向重开的新一生重新执行）；可重试失败只对写它的
// 那次尝试回放。
func (d mongoStepReceiptDoc) replays(command Command, incarnation uint32) bool {
	switch {
	case d.Success:
		return true
	case d.Retryable:
		return d.CommandID == command.ID
	default:
		return d.Incarnation == incarnation
	}
}

func (d mongoStepReceiptDoc) completion(command Command) Completion {
	return Completion{
		CommandID: d.CommandID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID,
		Success: d.Success, Retryable: d.Retryable, Error: d.Error, Data: d.Data,
	}
}

func (i *MongoStepInbox) receipts() fmongo.ICollection {
	return i.client.Database(i.database).Collection(i.collection)
}
