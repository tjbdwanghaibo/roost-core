package saga

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	dataEngineClaimCollection   = "_dataengine_inbox_claims"
	dataEngineReceiptCollection = "_dataengine_receipts"
	dataEngineStepNamespace     = "saga-step"
	// The projector matches on this exact value, so it is taken from the
	// shared fence contract rather than spelled out again here.
	claimStatusPending   = coredata.LeaseFenceStatusPending
	claimStatusCompleted = "completed"
	// claimStatusSuperseded 标记“同一操作实例的较新尝试已经接替这次尝试”：这次尝试的租约已过期、
	// 还没有回执，接替时 lease_token 同时加一，所以它在 WAL 里未投影的记录投影时 fence 不再匹配，
	// 被跳过（U-0280）。被接替的尝试永远不会再生效。
	claimStatusSuperseded = "superseded"

	// 操作实例守卫文档与 claim 同集合，命名空间不同：同一操作实例（saga + 步骤 + 方向）的各个尝试
	// 在 Reserve 事务里都写它，Mongo 的写冲突让这些 Reserve 串行化（U-0280）。
	dataEngineOperationNamespace = "saga-step-op"
	claimStatusOperationGuard    = "operation"
	// maxOperationAttempts 是一次 Reserve 扫描同一操作实例 claim 的上限。协调器每一生最多 1000 次
	// 尝试（Definition.Validate），超过这个数说明数据异常，拒绝而不是做无界扫描。
	maxOperationAttempts = 4096
)

// ErrCommandExpired 表示命令已过 DeadlineAt：协调器已不再等这次尝试，收件箱不再为它分配租约。
var ErrCommandExpired = errors.New("saga: step command is past its deadline")

// errOperationAttemptInFlight 表示同一操作实例的另一次尝试还持有有效租约、结果未定。
// 消费者把它当作可重试错误返回（nak 后重投），重投时再判断回放、接替还是执行。
var errOperationAttemptInFlight = errors.New("saga: another attempt of this step operation holds a live lease")

// errAttemptSuperseded 表示这次尝试已被同一操作实例的较新尝试接替，永远不会执行。
var errAttemptSuperseded = errors.New("saga: step attempt was superseded by a newer attempt")

type DataEngineStepInboxOptions struct {
	Owner         string
	LeaseDuration time.Duration
	ReceiptTTL    time.Duration
	PollInterval  time.Duration
}

type DataEngineStepInbox struct {
	client   fmongo.IMongo
	database string
	options  DataEngineStepInboxOptions
	now      func() time.Time
}

type Reservation struct {
	Token     uint64
	Duplicate bool
	// Completion 非空时是这个操作实例已有的结果：可能是本命令自己的回执，也可能是同一操作实例
	// 较早一次尝试的回执（Completion.CommandID 与当前命令不同，消费者把它重发给协调器）。
	Completion Completion
	commandID  string
	owner      string
	digest     []byte
}

type reservationContextKey struct{}

func withReservation(ctx context.Context, reservation Reservation) context.Context {
	return context.WithValue(ctx, reservationContextKey{}, reservation)
}

// ReservationFromContext returns the lease fence allocated for the current
// synchronous step delivery. Business adapters pass it explicitly to Bind;
// it must not be copied into detached goroutines as ambient context.
func ReservationFromContext(ctx context.Context) (Reservation, bool) {
	if ctx == nil {
		return Reservation{}, false
	}
	reservation, ok := ctx.Value(reservationContextKey{}).(Reservation)
	return reservation, ok && reservation.Token > 0 && !reservation.Duplicate && reservation.commandID != "" && reservation.owner != "" && len(reservation.digest) > 0
}

// dataEngineClaim 的 updated_at 也由投影事务按 coredata.LeaseFenceFieldUpdatedAt 条件写入，
// 使投影与 reserveInTransaction 的过期接管写同一文档、在 Mongo 里串行化（RR-20260926-30 §6）。
//
// OperationKey / Incarnation 是 U-0280 增加的字段：同一操作实例（Command.IdempotencyKey）的各次尝试
// 按 operation_key 找到彼此；Incarnation 是 Resume 代际，只用来决定旧一生的拒绝要不要回放。
// 升级前写的 claim 没有这两个字段，新 Reserve 看不到它们（混跑语义见 SAGA.md「原生步骤执行契约」）。
type dataEngineClaim struct {
	ID           string    `bson:"_id"`
	Namespace    string    `bson:"namespace"`
	CommandID    string    `bson:"command_id"`
	OperationKey string    `bson:"operation_key,omitempty"`
	Incarnation  uint32    `bson:"incarnation,omitempty"`
	Digest       []byte    `bson:"digest"`
	Owner        string    `bson:"owner"`
	LeaseUntil   time.Time `bson:"lease_until"`
	LeaseToken   uint64    `bson:"lease_token"`
	Status       string    `bson:"status"`
	Completion   []byte    `bson:"completion,omitempty"`
	SupersededBy string    `bson:"superseded_by,omitempty"`
	CreatedAt    time.Time `bson:"created_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
	ExpiresAt    time.Time `bson:"expires_at"`
}

type dataEngineReceipt struct {
	ID      string `bson:"_id"`
	Digest  []byte `bson:"digest"`
	Payload []byte `bson:"payload"`
}

func NewDataEngineStepInbox(client fmongo.IMongo, database string, options DataEngineStepInboxOptions) (*DataEngineStepInbox, error) {
	if client == nil || database == "" || options.Owner == "" {
		return nil, errors.New("saga dataengine inbox: client, database and owner are required")
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = time.Minute
	}
	if options.ReceiptTTL <= 0 {
		options.ReceiptTTL = 30 * 24 * time.Hour
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 25 * time.Millisecond
	}
	return &DataEngineStepInbox{client: client, database: database, options: options, now: time.Now}, nil
}

func (inbox *DataEngineStepInbox) EnsureInfrastructure(ctx context.Context) error {
	if inbox == nil || inbox.client == nil {
		return ErrInvalidRecord
	}
	return inbox.claims().EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "lease_until", Value: 1}}, Name: "claim_expired"},
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "command_id", Value: 1}}, Name: "uniq_command", Unique: true},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Name: "ttl_expires_at", ExpireAt: true, RecreateOnConflict: true},
		// 同一操作实例的尝试互相查找（U-0280）。
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "operation_key", Value: 1}}, Name: "by_operation"},
	})
}

// Bind is called from inside the native Nest handler. The inbox owns the
// validated receipt retention policy; core Saga only binds the supplied time.
func (inbox *DataEngineStepInbox) Bind(command Command, reservations ...Reservation) error {
	if inbox == nil || inbox.options.ReceiptTTL <= 0 {
		return ErrInvalidRecord
	}
	if len(reservations) != 1 || reservations[0].Token == 0 || reservations[0].Duplicate {
		return fmt.Errorf("saga dataengine inbox: an active reservation is required")
	}
	reservation := reservations[0]
	digest, err := commandDigest(command)
	if err != nil {
		return err
	}
	if reservation.commandID != command.ID || reservation.owner != inbox.options.Owner || !bytes.Equal(reservation.digest, digest) {
		return fmt.Errorf("saga dataengine inbox: reservation does not match command identity")
	}
	fence := coredata.LeaseFence{
		Database: inbox.database, Resource: dataEngineClaimCollection,
		DocumentID: dataEngineStepNamespace + "/" + command.ID,
		Owner:      reservation.owner, Token: reservation.Token, Digest: append([]byte(nil), digest...),
	}
	return BindCommand(command, inbox.now().UTC().Add(inbox.options.ReceiptTTL), fence)
}

func (inbox *DataEngineStepInbox) Reserve(ctx context.Context, command Command) (Reservation, error) {
	if inbox == nil || inbox.client == nil {
		return Reservation{}, ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return Reservation{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	digest, err := commandDigest(command)
	if err != nil {
		return Reservation{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		session, err := inbox.client.StartSession(ctx)
		if err != nil {
			return Reservation{}, err
		}
		var reservation Reservation
		err = session.WithTransaction(ctx, func(txCtx context.Context) error {
			var reserveErr error
			reservation, reserveErr = inbox.reserveInTransaction(txCtx, command, digest)
			return reserveErr
		})
		session.EndSession(ctx)
		if errors.Is(err, fmongo.ErrDuplicateKey) {
			continue
		}
		return reservation, err
	}
	return Reservation{}, fmongo.ErrDuplicateKey
}

// reserveInTransaction 在一个 Mongo 事务里决定这次投递要做什么（U-0280「原生步骤执行契约」）：
//
//  1. 本命令已有回执：回放（Duplicate + Completion）。
//  2. 本命令已有 claim：租约有效 → Duplicate（同一尝试的重投，等它的回执）；被接替 → errAttemptSuperseded；
//     租约过期 → 先看同一操作实例的其他尝试，再接管自己的 claim。
//  3. 要新建或接管 claim 时先写操作实例守卫，让同一操作实例的并发 Reserve 串行化。
//  4. 同一操作实例的其他尝试：有成功回执（任何一生）或本生的拒绝回执 → 回放那次的 completion，不执行；
//     可重试失败 → 忽略；仍 pending 且租约有效 → errOperationAttemptInFlight（等它有结论）；
//     pending 且租约过期 → 接替（status=superseded、lease_token+1），它在 WAL 里未投影的记录随后被 fence 跳过。
//  5. 以上都没有：写自己的 claim 并执行。
//
// 租约封顶到命令截止时间：lease_until = min(now+LeaseDuration, Command.DeadlineAt)。协调器只在截止后
// 才判这次尝试超时、放弃或发出下一次尝试，所以截止后才投影的记录（崩溃重启后的 WAL 重放、投影积压）
// 一律被跳过——一次尝试只可能在协调器等它的窗口内生效。
func (inbox *DataEngineStepInbox) reserveInTransaction(ctx context.Context, command Command, digest []byte) (Reservation, error) {
	commandID := command.ID
	if completion, found, err := inbox.readReceipt(ctx, commandID, digest); err != nil || found {
		if found {
			// 回执是权威，claim 只是协调行：标记失败不改变 Reserve 的结论（真实服务端会因写错误中止事务并由驱动重跑）。
			// 但不能静默吞掉——将来出现确定性失败时会一直重跑到超时，日志与计数是唯一线索（RR-20260927-16）。
			if markErr := inbox.markCompleted(ctx, commandID, completion); markErr != nil {
				metrics.IncCounter("saga.step_inbox.mark_completed_error_total", nil, 1)
				slog.Warn("saga dataengine inbox: mark claim completed failed; the receipt stays authoritative",
					"command_id", commandID, "err", markErr)
			}
		}
		return Reservation{Duplicate: found, Completion: completion}, err
	}
	now := inbox.now().UTC()
	if !now.Before(command.DeadlineAt) {
		return Reservation{}, ErrCommandExpired
	}
	leaseUntil := now.Add(inbox.options.LeaseDuration)
	if command.DeadlineAt.Before(leaseUntil) {
		leaseUntil = command.DeadlineAt
	}
	claimID := dataEngineStepNamespace + "/" + commandID
	var claim dataEngineClaim
	err := inbox.claims().FindOne(ctx, bson.M{"_id": claimID}, &claim)
	if err != nil && !errors.Is(err, fmongo.ErrNotFound) {
		return Reservation{}, err
	}
	ownClaim := err == nil
	if ownClaim {
		if !bytes.Equal(claim.Digest, digest) {
			return Reservation{}, ErrIdentityConflict
		}
		switch claim.Status {
		case claimStatusCompleted:
			completion, err := DecodeCompletionEffect(claim.Completion)
			return Reservation{Token: claim.LeaseToken, Duplicate: true, Completion: completion}, err
		case claimStatusSuperseded:
			return Reservation{}, errAttemptSuperseded
		case claimStatusPending:
		default:
			return Reservation{}, ErrConflict
		}
		if claim.LeaseUntil.After(now) {
			return Reservation{Token: claim.LeaseToken, Duplicate: true}, nil
		}
	}
	// 到这里这次投递要么新建 claim，要么接管自己过期的 claim：先写守卫，再看同一操作实例的其他尝试。
	if err := inbox.guardOperation(ctx, command.IdempotencyKey, now); err != nil {
		return Reservation{}, err
	}
	if replay, found, err := inbox.resolveOtherAttempts(ctx, command, now); err != nil || found {
		return replay, err
	}
	if !ownClaim {
		claim = dataEngineClaim{
			ID: claimID, Namespace: dataEngineStepNamespace, CommandID: commandID,
			OperationKey: command.IdempotencyKey, Incarnation: commandIncarnation(command),
			Digest: append([]byte(nil), digest...), Owner: inbox.options.Owner, LeaseUntil: leaseUntil, LeaseToken: 1,
			Status: claimStatusPending, CreatedAt: now, UpdatedAt: now,
			ExpiresAt: now.Add(inbox.options.ReceiptTTL),
		}
		if _, err := inbox.claims().InsertOne(ctx, claim); err != nil {
			return Reservation{}, err
		}
		return inbox.activeReservation(commandID, digest, 1), nil
	}
	filter := bson.M{"_id": claimID, "digest": digest, "status": claimStatusPending, "lease_token": claim.LeaseToken, "lease_until": bson.M{"$lte": now}}
	update := bson.M{"$set": bson.M{
		"owner": inbox.options.Owner, "lease_until": leaseUntil, "updated_at": now,
		"operation_key": command.IdempotencyKey, "incarnation": commandIncarnation(command),
	}, "$inc": bson.M{"lease_token": 1}}
	var renewed dataEngineClaim
	if err := inbox.claims().FindOneAndUpdate(ctx, filter, update, &renewed, fmongo.FindOneAndUpdateOption{ReturnAfter: true}); err != nil {
		if errors.Is(err, fmongo.ErrNotFound) {
			return Reservation{Token: claim.LeaseToken, Duplicate: true}, nil
		}
		return Reservation{}, err
	}
	return inbox.activeReservation(commandID, digest, renewed.LeaseToken), nil
}

// guardOperation 写操作实例守卫文档。同一操作实例的两个 Reserve 事务都写它，Mongo 只让一个提交，
// 另一个以写冲突（TransientTransactionError）重跑，重跑时就能看到前者写下的 claim；首次创建时
// 并发 upsert 撞唯一键，由 Reserve 的外层循环重试。
func (inbox *DataEngineStepInbox) guardOperation(ctx context.Context, operationKey string, now time.Time) error {
	guardID := dataEngineOperationNamespace + "/" + operationKey
	update := bson.M{
		"$set": bson.M{
			"namespace": dataEngineOperationNamespace, "command_id": operationKey, "status": claimStatusOperationGuard,
			"updated_at": now, "expires_at": now.Add(inbox.options.ReceiptTTL),
		},
		"$inc": bson.M{"seq": 1},
	}
	var guard bson.M
	return inbox.claims().FindOneAndUpdate(ctx, bson.M{"_id": guardID}, update, &guard, fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true})
}

// resolveOtherAttempts 处理同一操作实例的其他尝试（见 reserveInTransaction 第 4 步）。found=true 时
// 返回的 Reservation 是回放；err 为 errOperationAttemptInFlight 时调用方稍后重试。
func (inbox *DataEngineStepInbox) resolveOtherAttempts(ctx context.Context, command Command, now time.Time) (Reservation, bool, error) {
	var claims []dataEngineClaim
	filter := bson.M{"namespace": dataEngineStepNamespace, "operation_key": command.IdempotencyKey}
	if err := inbox.claims().Find(ctx, filter, &claims, fmongo.FindOption{Limit: maxOperationAttempts + 1}); err != nil {
		return Reservation{}, false, err
	}
	if len(claims) > maxOperationAttempts {
		return Reservation{}, false, fmt.Errorf("%w: operation %s has more than %d attempts", ErrConflict, command.IdempotencyKey, maxOperationAttempts)
	}
	incarnation := commandIncarnation(command)
	var refusal *Completion
	var live []dataEngineClaim
	for i := range claims {
		other := claims[i]
		if other.CommandID == command.ID || other.Status == claimStatusSuperseded {
			continue
		}
		completion, settled, err := inbox.attemptResult(ctx, other)
		if err != nil {
			return Reservation{}, false, err
		}
		switch {
		case settled && completion.Success:
			// 成功在任何一生里都不重做：Resume 之后的新一生遇到旧一生已生效的步骤，同样回放它。
			return Reservation{Duplicate: true, Completion: completion}, true, nil
		case settled && !completion.Retryable:
			// 业务拒绝只在同一生里回放；Resume 的目的就是在修复原因之后重新执行。
			if other.Incarnation == incarnation && refusal == nil {
				refusal = &completion
			}
		case settled:
			// 可重试失败：那次尝试已有结论且没有生效，允许新尝试执行。
		default:
			live = append(live, other)
		}
	}
	if refusal != nil {
		return Reservation{Duplicate: true, Completion: *refusal}, true, nil
	}
	for _, other := range live {
		if other.LeaseUntil.After(now) {
			return Reservation{}, false, errOperationAttemptInFlight
		}
		if err := inbox.supersede(ctx, other, command.ID, now); err != nil {
			return Reservation{}, false, err
		}
	}
	return Reservation{}, false, nil
}

// operationSuccess 找同一操作实例另一次尝试已经生效的成功（任何一生）。只读：不写守卫、不接替，
// 供不会执行的投递（过期、认领前过截止）在 ack 前把成功重发给协调器（见 replayOperationSuccess）。
func (inbox *DataEngineStepInbox) operationSuccess(ctx context.Context, command Command) (Completion, bool, error) {
	var claims []dataEngineClaim
	filter := bson.M{"namespace": dataEngineStepNamespace, "operation_key": command.IdempotencyKey}
	if err := inbox.claims().Find(ctx, filter, &claims, fmongo.FindOption{Limit: maxOperationAttempts + 1}); err != nil {
		return Completion{}, false, err
	}
	for i := range claims {
		other := claims[i]
		if other.CommandID == command.ID || other.Status == claimStatusSuperseded {
			continue
		}
		completion, settled, err := inbox.attemptResult(ctx, other)
		if err != nil {
			return Completion{}, false, err
		}
		if settled && completion.Success {
			return completion, true, nil
		}
	}
	return Completion{}, false, nil
}

// attemptResult 读另一次尝试的结论：claim 已标 completed 就用它保存的 completion；仍 pending 时看回执
// （投影已写回执、claim 还没来得及标记），有回执顺手标记。settled=false 表示还没有结论。
func (inbox *DataEngineStepInbox) attemptResult(ctx context.Context, claim dataEngineClaim) (Completion, bool, error) {
	switch claim.Status {
	case claimStatusCompleted:
		completion, err := DecodeCompletionEffect(claim.Completion)
		return completion, err == nil, err
	case claimStatusPending:
	default:
		return Completion{}, false, ErrConflict
	}
	completion, found, err := inbox.readReceipt(ctx, claim.CommandID, claim.Digest)
	if err != nil || !found {
		return Completion{}, false, err
	}
	if err := inbox.markCompleted(ctx, claim.CommandID, completion); err != nil {
		return Completion{}, false, err
	}
	return completion, true, nil
}

// supersede 让一次租约已过期、还没有回执的尝试永远失效：status 改为 superseded 且 lease_token 加一。
// 它与投影事务的 fence 确认写同一文档，二者只能有一个提交：投影先提交，这里的事务重跑后会读到回执；
// 这里先提交，投影的 fence 不再匹配，记录被跳过（受影响实体按 RR-20260926-30 驱逐重载）。
func (inbox *DataEngineStepInbox) supersede(ctx context.Context, claim dataEngineClaim, by string, now time.Time) error {
	filter := bson.M{"_id": claim.ID, "status": claimStatusPending, "lease_token": claim.LeaseToken, "lease_until": bson.M{"$lte": now}}
	update := bson.M{"$set": bson.M{"status": claimStatusSuperseded, "superseded_by": by, "updated_at": now}, "$inc": bson.M{"lease_token": 1}}
	result, err := inbox.claims().UpdateOne(ctx, filter, update)
	if err != nil {
		return err
	}
	if result == nil || result.MatchedCount != 1 {
		return ErrConflict
	}
	metrics.IncCounter("saga.step_inbox.superseded_total", nil, 1)
	return nil
}

// commandIncarnation 从协调器生成的 CommandID（operationKey:attempt 或 operationKey:rN:attempt）取出
// Resume 代际；不是这个格式的 CommandID（测试或手工命令）按第 0 代处理。解析与协调器核对 completion 代际
// 用同一个函数（commandIDIncarnation，B1），两边对“同一生”的判断不会分叉。
func commandIncarnation(command Command) uint32 {
	return commandIDIncarnation(command.IdempotencyKey, command.ID)
}

// releaseLease 交还本次投递刚拿到、但没有用上的租约：把 lease_until 设为现在，只对仍属于这个
// owner/token 的 pending claim 生效。只在 handler 以 coredata.ErrFencedEntityPending 失败时调用——
// 该错误说明这次 Nest 事务在 WAL 准入前整体回滚，没有带本 token 的记录进入 WAL；不交还的话，
// 之后的重投都会读到“租约有效”的 Duplicate，一直等到本次租约自然过期（RR-20260926-30）。
// 即使 handler 另有一笔已准入的记录用了本 token，交还后它最多在投影时被跳过，被跳过的原生步骤
// 会驱逐受影响的内存实体，不会产生重复副作用。
func (inbox *DataEngineStepInbox) releaseLease(ctx context.Context, reservation Reservation) error {
	if inbox == nil || inbox.client == nil || reservation.Duplicate || reservation.Token == 0 || reservation.commandID == "" {
		return nil
	}
	now := inbox.now().UTC()
	filter := bson.M{
		"_id": dataEngineStepNamespace + "/" + reservation.commandID, "digest": reservation.digest,
		"owner": reservation.owner, "lease_token": reservation.Token, "status": claimStatusPending,
	}
	_, err := inbox.claims().UpdateOne(ctx, filter, bson.M{"$set": bson.M{"lease_until": now, "updated_at": now}})
	return err
}

func (inbox *DataEngineStepInbox) activeReservation(commandID string, digest []byte, token uint64) Reservation {
	return Reservation{
		Token: token, commandID: commandID, owner: inbox.options.Owner,
		digest: append([]byte(nil), digest...),
	}
}

func (inbox *DataEngineStepInbox) Replay(ctx context.Context, command Command) (Completion, bool, error) {
	if inbox == nil || inbox.client == nil {
		return Completion{}, false, ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return Completion{}, false, err
	}
	digest, err := commandDigest(command)
	if err != nil {
		return Completion{}, false, err
	}
	completion, found, err := inbox.readReceipt(ctx, command.ID, digest)
	if err != nil || !found {
		return completion, found, err
	}
	if err := inbox.markCompleted(ctx, command.ID, completion); err != nil {
		return Completion{}, false, err
	}
	return completion, true, nil
}

func (inbox *DataEngineStepInbox) readReceipt(ctx context.Context, commandID string, digest []byte) (Completion, bool, error) {
	var receipt dataEngineReceipt
	err := inbox.receipts().FindOne(ctx, bson.M{"_id": dataEngineStepNamespace + "/" + commandID}, &receipt)
	if errors.Is(err, fmongo.ErrNotFound) {
		return Completion{}, false, nil
	}
	if err != nil {
		return Completion{}, false, err
	}
	if !bytes.Equal(receipt.Digest, digest) {
		return Completion{}, false, ErrIdentityConflict
	}
	completion, err := DecodeCompletionEffect(receipt.Payload)
	if err != nil {
		return Completion{}, false, err
	}
	return completion, true, nil
}

func (inbox *DataEngineStepInbox) markCompleted(ctx context.Context, commandID string, completion Completion) error {
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		return err
	}
	now := inbox.now().UTC()
	result, err := inbox.claims().UpdateOne(ctx, bson.M{"_id": dataEngineStepNamespace + "/" + commandID}, bson.M{"$set": bson.M{
		"status": claimStatusCompleted, "completion": effect.Payload, "lease_until": time.Unix(0, 0).UTC(), "updated_at": now,
		"expires_at": now.Add(inbox.options.ReceiptTTL),
	}})
	if err != nil {
		return err
	}
	// A receipt may survive a claim cleanup/migration. It remains authoritative
	// and replayable even when no coordination row exists.
	if result == nil || result.MatchedCount == 0 {
		return nil
	}
	return nil
}

func (inbox *DataEngineStepInbox) waitReplay(ctx context.Context, command Command) (Completion, error) {
	ticker := time.NewTicker(inbox.options.PollInterval)
	defer ticker.Stop()
	for {
		completion, found, err := inbox.Replay(ctx, command)
		if err != nil || found {
			return completion, err
		}
		select {
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (inbox *DataEngineStepInbox) claims() fmongo.ICollection {
	return inbox.client.Database(inbox.database).Collection(dataEngineClaimCollection)
}

func (inbox *DataEngineStepInbox) receipts() fmongo.ICollection {
	return inbox.client.Database(inbox.database).Collection(dataEngineReceiptCollection)
}

func (reservation Reservation) String() string {
	return fmt.Sprintf("token=%d duplicate=%t", reservation.Token, reservation.Duplicate)
}
