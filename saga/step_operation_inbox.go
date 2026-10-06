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

// 步骤收件箱的“操作实例”协调，原生步骤（DataEngineStepInbox）与 Mongo 步骤（MongoCommandInbox）共用
// （SAGA.md「原生步骤执行契约」；U-0280 为原生步骤建立，saga 方向 ②把 Mongo 步骤纳入同一份代码）。
//
// 每次尝试在 Reserve 事务里拿一份 claim（`saga-step/<CommandID>`，租约封顶到命令截止），同一操作实例
// （Command.IdempotencyKey）的各次尝试按 operation_key 找到彼此，并都写守卫文档 `saga-step-op/<IdempotencyKey>`
// 串行化。尝试只有在它的生效点对自己的 claim 做条件写（owner、token、pending、租约未过期）成功时才生效：
// 原生步骤的生效点是 DataEngine 投影事务，Mongo 步骤的生效点是 handler 的 Mongo 事务（settleOwnClaim）。
// 接替（supersede）写的是同一个 claim 文档，与生效点的条件写只能有一个提交。
//
// 两种收件箱的差别只在 claim 所在集合与回执（receipt）怎么读：原生回执由投影写进 `_dataengine_receipts`，
// Mongo 回执由 handler 事务写进收件箱自己的集合。

const (
	stepClaimNamespace = "saga-step"
	// claimStatusPending 必须与投影匹配的值相同，所以取自共享的 fence 契约。
	claimStatusPending   = coredata.LeaseFenceStatusPending
	claimStatusCompleted = "completed"
	// claimStatusSuperseded 标记“同一操作实例的较新尝试已经接替这次尝试”：这次尝试的租约已过期、
	// 还没有回执，接替时 lease_token 同时加一，所以它的生效点的条件写不再匹配（原生：WAL 里未投影的
	// 记录投影时被跳过；Mongo：handler 事务中止）。被接替的尝试永远不会再生效。
	claimStatusSuperseded = "superseded"

	// 操作实例守卫文档与 claim 同集合，命名空间不同：同一操作实例的各个尝试在 Reserve 事务里都写它，
	// Mongo 的写冲突让这些 Reserve 串行化（U-0280）。
	stepOperationNamespace    = "saga-step-op"
	claimStatusOperationGuard = "operation"
	// claim 的 outcome：标记 completed 时按结论写入，让 Reserve 只读取会影响判定的 claim（RR-20261006-15）。
	// 没有 outcome 的 completed claim 是升级前（或混跑中的旧进程）写的，按“可能有影响”读取。
	claimOutcomeSuccess   = "success"
	claimOutcomeRefused   = "refused"
	claimOutcomeRetryable = "retryable"

	// maxDecisiveOperationClaims 是一次扫描同一操作实例“有影响的 claim”的上限，超过说明数据异常，拒绝而不是做无界
	// 扫描。有影响的 claim 有界、与尝试次数和 Resume 次数无关（RR-20261006-15，证明见 operationClaimsFilter）：
	// 旧实现按全部 claim 计数，协调器每生最多 1000 次尝试、Resume 不限次数，几次 Resume 之后同一步骤就永远 Reserve 不了。
	maxDecisiveOperationClaims = 4096
	// maxLegacyOperationClaims 是同一操作实例不带 outcome 的 completed claim（升级前、或混跑中的旧进程写的）的上限，
	// 与 maxDecisiveOperationClaims 分开计数（RR-20261006-16）。旧进程按“这个操作的全部 claim”计数、看到 4096 份时
	// 还会再写一份，所以升级前就卡住的操作恰好有 4097 份不带 outcome 的 claim，和新写的 claim 共用 4096 的上限时
	// 升级后照样卡住；混跑中旧进程还可能给新进程留下的 pending claim 补标 completed（原生步骤的回执先于标记）。
	// 取 2×4096 留出这部分余量，超过同样说明数据异常。
	maxLegacyOperationClaims = 2 * maxDecisiveOperationClaims
)

// ErrCommandExpired 表示命令已过 DeadlineAt：协调器已不再等这次尝试，收件箱不再为它分配租约。
var ErrCommandExpired = errors.New("saga: step command is past its deadline")

// errOperationAttemptInFlight 表示同一操作实例的另一次尝试（或同一命令的另一次投递）还持有有效租约、结果未定。
// 消费者把它当作可重试错误返回（nak 后重投），重投时再判断回放、接替还是执行。
var errOperationAttemptInFlight = errors.New("saga: another attempt of this step operation holds a live lease")

// errAttemptSuperseded 表示这次尝试已被同一操作实例的较新尝试接替，永远不会执行。
var errAttemptSuperseded = errors.New("saga: step attempt was superseded by a newer attempt")

// errAttemptFenced 表示 Mongo 步骤的 handler 事务在提交前对自己 claim 的条件写没有匹配：租约已过期（命令截止已过）
// 或已被较新的尝试接替。事务整体中止，这次尝试没有生效。
var errAttemptFenced = errors.New("saga: step attempt lost its lease before it could commit")

// stepOperationInbox 是两种收件箱共用的 claim / 守卫 / 判定。
type stepOperationInbox struct {
	client          fmongo.IMongo
	database        string
	claimCollection string
	owner           string
	leaseDuration   time.Duration
	receiptTTL      time.Duration
	now             func() time.Time
	// receipt 读这个收件箱的权威回执：found=false 表示没有；摘要不同返回 ErrIdentityConflict。
	receipt func(ctx context.Context, commandID string, digest []byte) (Completion, bool, error)
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

// stepClaim 的 updated_at 也由原生投影事务按 coredata.LeaseFenceFieldUpdatedAt 条件写入，
// 使投影与 reserveInTransaction 的过期接管写同一文档、在 Mongo 里串行化（RR-20260926-30 §6）。
//
// OperationKey / Incarnation 是 U-0280 增加的字段：同一操作实例（Command.IdempotencyKey）的各次尝试
// 按 operation_key 找到彼此；Incarnation 是 Resume 代际，只用来决定旧一生的拒绝要不要回放。
// 升级前写的 claim 没有这两个字段，新 Reserve 看不到它们（混跑语义见 SAGA.md「原生步骤执行契约」）。
type stepClaim struct {
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
	// Outcome 是 completed claim 的结论（claimOutcome*，RR-20261006-15）；升级前写的 claim 没有。
	Outcome   string    `bson:"outcome,omitempty"`
	CreatedAt time.Time `bson:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"`
	ExpiresAt time.Time `bson:"expires_at"`
}

func stepClaimID(commandID string) string { return stepClaimNamespace + "/" + commandID }

func (o *stepOperationInbox) ensureClaimIndexes(ctx context.Context) error {
	return o.claims().EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "lease_until", Value: 1}}, Name: "claim_expired"},
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "command_id", Value: 1}}, Name: "uniq_command", Unique: true},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Name: "ttl_expires_at", ExpireAt: true, RecreateOnConflict: true},
		// 同一操作实例的尝试互相查找（U-0280）。修前进程的查询只按这两个字段，滚动升级期间仍要用它。
		{Keys: bson.D{{Key: "namespace", Value: 1}, {Key: "operation_key", Value: 1}}, Name: "by_operation"},
		// operationClaimsFilter 的每个 $or 分支都是这个索引的前缀上的等值或区间（pending 只用到 status；成功、旧 claim
		// 用到 outcome；本生拒绝用到 incarnation），服务端只碰有影响的 claim，与累积的可重试失败、被接替的尝试、旧一生
		// 的拒绝数量无关（RR-20261006-15 复核补修）。只有 by_operation 时服务端要取出这个操作的全部 claim 逐个过滤，
		// 检查的文档数随尝试与 Resume 线性增长；$or 还让优化器在集合里只有少数操作时改选 claim_expired 或 uniq_command。
		{Keys: bson.D{
			{Key: "namespace", Value: 1}, {Key: "operation_key", Value: 1}, {Key: "status", Value: 1}, {Key: "outcome", Value: 1}, {Key: "incarnation", Value: 1},
		}, Name: "by_operation_decision"},
	})
}

// reserve 在一个 Mongo 事务里决定这次投递做什么（reserveInTransaction）；首次创建守卫或 claim 时并发 upsert / insert
// 撞唯一键，重试一次。
func (o *stepOperationInbox) reserve(ctx context.Context, command Command, digest []byte) (Reservation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempt := 0; attempt < 2; attempt++ {
		session, err := o.client.StartSession(ctx)
		if err != nil {
			return Reservation{}, err
		}
		var reservation Reservation
		err = session.WithTransaction(ctx, func(txCtx context.Context) error {
			var reserveErr error
			reservation, reserveErr = o.reserveInTransaction(txCtx, command, digest)
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

// reserveInTransaction 在一个 Mongo 事务里决定这次投递要做什么（SAGA.md「原生步骤执行契约」）：
//
//  1. 本命令已有回执：回放（Duplicate + Completion）。
//  2. 本命令已有 claim：租约有效 → Duplicate（同一尝试的重投，等它的回执）；被接替 → errAttemptSuperseded；
//     租约过期 → 先看同一操作实例的其他尝试，再接管自己的 claim。
//  3. 要新建或接管 claim 时先写操作实例守卫，让同一操作实例的并发 Reserve 串行化。
//  4. 同一操作实例的其他尝试：有成功回执（任何一生）或本生的拒绝回执 → 回放那次的 completion，不执行；
//     可重试失败 → 忽略；仍 pending 且租约有效 → errOperationAttemptInFlight（等它有结论）；
//     pending 且租约过期 → 接替（status=superseded、lease_token+1），它的生效点随后不再匹配。
//  5. 以上都没有：写自己的 claim 并执行。
//
// 租约封顶到命令截止时间：lease_until = min(now+LeaseDuration, Command.DeadlineAt)。协调器只在截止后
// 才判这次尝试超时、放弃或发出下一次尝试，所以截止后才到生效点的尝试（崩溃重启后的 WAL 重放、投影积压、
// Mongo 事务在写冲突重试里拖过截止）一律不生效——一次尝试只可能在协调器等它的窗口内生效。
func (o *stepOperationInbox) reserveInTransaction(ctx context.Context, command Command, digest []byte) (Reservation, error) {
	commandID := command.ID
	if completion, found, err := o.receipt(ctx, commandID, digest); err != nil || found {
		if found {
			// 回执是权威，claim 只是协调行：标记失败不改变 Reserve 的结论（真实服务端会因写错误中止事务并由驱动重跑）。
			// 但不能静默吞掉——将来出现确定性失败时会一直重跑到超时，日志与计数是唯一线索（RR-20260927-16）。
			if markErr := o.markCompleted(ctx, commandID, completion); markErr != nil {
				metrics.IncCounter("saga.step_inbox.mark_completed_error_total", nil, 1)
				slog.Warn("saga step inbox: mark claim completed failed; the receipt stays authoritative",
					"command_id", commandID, "err", markErr)
			}
		}
		return Reservation{Duplicate: found, Completion: completion}, err
	}
	now := o.now().UTC()
	if !now.Before(command.DeadlineAt) {
		return Reservation{}, ErrCommandExpired
	}
	leaseUntil := now.Add(o.leaseDuration)
	if command.DeadlineAt.Before(leaseUntil) {
		leaseUntil = command.DeadlineAt
	}
	claimID := stepClaimID(commandID)
	var claim stepClaim
	err := o.claims().FindOne(ctx, bson.M{"_id": claimID}, &claim)
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
	if err := o.guardOperation(ctx, command.IdempotencyKey, now); err != nil {
		return Reservation{}, err
	}
	if replay, found, err := o.resolveOtherAttempts(ctx, command, now); err != nil || found {
		return replay, err
	}
	if !ownClaim {
		claim = stepClaim{
			ID: claimID, Namespace: stepClaimNamespace, CommandID: commandID,
			OperationKey: command.IdempotencyKey, Incarnation: commandIncarnation(command),
			Digest: append([]byte(nil), digest...), Owner: o.owner, LeaseUntil: leaseUntil, LeaseToken: 1,
			Status: claimStatusPending, CreatedAt: now, UpdatedAt: now,
			ExpiresAt: now.Add(o.receiptTTL),
		}
		if _, err := o.claims().InsertOne(ctx, claim); err != nil {
			return Reservation{}, err
		}
		return o.activeReservation(commandID, digest, 1), nil
	}
	filter := bson.M{"_id": claimID, "digest": digest, "status": claimStatusPending, "lease_token": claim.LeaseToken, "lease_until": bson.M{"$lte": now}}
	update := bson.M{"$set": bson.M{
		"owner": o.owner, "lease_until": leaseUntil, "updated_at": now,
		"operation_key": command.IdempotencyKey, "incarnation": commandIncarnation(command),
	}, "$inc": bson.M{"lease_token": 1}}
	var renewed stepClaim
	if err := o.claims().FindOneAndUpdate(ctx, filter, update, &renewed, fmongo.FindOneAndUpdateOption{ReturnAfter: true}); err != nil {
		if errors.Is(err, fmongo.ErrNotFound) {
			return Reservation{Token: claim.LeaseToken, Duplicate: true}, nil
		}
		return Reservation{}, err
	}
	return o.activeReservation(commandID, digest, renewed.LeaseToken), nil
}

// guardOperation 写操作实例守卫文档。同一操作实例的两个 Reserve 事务都写它，Mongo 只让一个提交，
// 另一个以写冲突（TransientTransactionError）重跑，重跑时就能看到前者写下的 claim；首次创建时
// 并发 upsert 撞唯一键，由 reserve 的外层循环重试。
func (o *stepOperationInbox) guardOperation(ctx context.Context, operationKey string, now time.Time) error {
	guardID := stepOperationNamespace + "/" + operationKey
	update := bson.M{
		"$set": bson.M{
			"namespace": stepOperationNamespace, "command_id": operationKey, "status": claimStatusOperationGuard,
			"updated_at": now, "expires_at": now.Add(o.receiptTTL),
		},
		"$inc": bson.M{"seq": 1},
	}
	var guard bson.M
	return o.claims().FindOneAndUpdate(ctx, bson.M{"_id": guardID}, update, &guard, fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true})
}

// resolveOtherAttempts 处理同一操作实例的其他尝试（见 reserveInTransaction 第 4 步）。found=true 时
// 返回的 Reservation 是回放；err 为 errOperationAttemptInFlight 时调用方稍后重试。
func (o *stepOperationInbox) resolveOtherAttempts(ctx context.Context, command Command, now time.Time) (Reservation, bool, error) {
	incarnation := commandIncarnation(command)
	claims, err := o.operationClaims(ctx, command.IdempotencyKey, true, incarnation)
	if err != nil {
		return Reservation{}, false, err
	}
	var refusal *Completion
	var live []stepClaim
	for i := range claims {
		other := claims[i]
		if other.CommandID == command.ID || other.Status == claimStatusSuperseded {
			continue
		}
		completion, settled, err := o.attemptResult(ctx, other)
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
		if err := o.supersede(ctx, other, command.ID, now); err != nil {
			return Reservation{}, false, err
		}
	}
	return Reservation{}, false, nil
}

// operationClaims 读同一操作实例里会影响判定的 claim（见 operationClaimsFilter）。refusalsOf 是要回放拒绝的那一生；
// sameLifeRefusals=false 时不读任何拒绝（operationSuccess）。
func (o *stepOperationInbox) operationClaims(ctx context.Context, operationKey string, sameLifeRefusals bool, refusalsOf uint32) ([]stepClaim, error) {
	var claims []stepClaim
	filter := operationClaimsFilter(operationKey, sameLifeRefusals, refusalsOf)
	// 两类各有上限：取到 Limit 份说明至少一类超了；没取满时两类的份数都是准确的。
	if err := o.claims().Find(ctx, filter, &claims, fmongo.FindOption{Limit: maxDecisiveOperationClaims + maxLegacyOperationClaims + 1}); err != nil {
		return nil, err
	}
	legacy := 0
	for i := range claims {
		if claims[i].Status == claimStatusCompleted && claims[i].Outcome == "" {
			legacy++
		}
	}
	if legacy > maxLegacyOperationClaims {
		return nil, fmt.Errorf("%w: operation %s has more than %d attempts completed before the claim outcome was recorded", ErrConflict, operationKey, maxLegacyOperationClaims)
	}
	if len(claims)-legacy > maxDecisiveOperationClaims {
		return nil, fmt.Errorf("%w: operation %s has more than %d undecided, successful or same-life refused attempts", ErrConflict, operationKey, maxDecisiveOperationClaims)
	}
	return claims, nil
}

// operationClaimsFilter 选出同一操作实例里有影响的 claim（RR-20261006-15）：
//
//   - pending：还没结论的尝试（在途要等，过期要接替），以及原生投影已写回执、还没标记的尝试；
//   - 成功（任何一生）：要回放；
//   - refusalsOf 这一生的拒绝：同一生里回放；
//   - 没有 outcome 的 completed：升级前写的，结论只能读 completion 才知道。
//
// 可重试失败、被接替的尝试、旧一生的拒绝不影响判定（resolveOtherAttempts 跳过它们），也就不读取——它们随尝试与
// Resume 无界累积，正是旧实现超过上限的来源。
//
// 有界性：一次 Reserve 只有在处理完其他尝试之后才写自己的 claim（接替全部过期的 pending，有在途的就不写），守卫让同一
// 操作实例的 Reserve 串行，所以任何时刻没有结论的 pending 至多一份，加上结论已写、尚未被下一次 Reserve 标记的少数几份；
// 一个操作至多生效一次（U-0280），成功至多一份；同一生出现拒绝之后，后来的尝试回放它、不再写 claim，拒绝至多一份；
// 没有 outcome 的只有升级前与混跑期间写的，受旧进程自己的上限约束、单独计数（maxLegacyOperationClaims，RR-20261006-16），
// 并随 TTL 消失。所以带 outcome 的结果集是个小常数，与尝试次数、Resume 次数无关，两个上限都只在数据异常时触发。
//
// 第 0 代的 incarnation：新建 claim 时带 omitempty 不写这个字段，接管自己过期的 claim 时 $set 写成 0，两种都要匹配。
func operationClaimsFilter(operationKey string, sameLifeRefusals bool, refusalsOf uint32) bson.M {
	decisive := bson.A{
		bson.M{"status": claimStatusPending},
		bson.M{"status": claimStatusCompleted, "outcome": claimOutcomeSuccess},
		bson.M{"status": claimStatusCompleted, "outcome": bson.M{"$exists": false}},
	}
	if sameLifeRefusals {
		decisive = append(decisive, bson.M{"status": claimStatusCompleted, "outcome": claimOutcomeRefused, "incarnation": refusalsOf})
		if refusalsOf == 0 {
			// 第 0 代的另一种写法单列一支，不写成嵌套的 $or：嵌套 $or 让优化器给这一支改选 claim_expired，
			// 扫这个集合全部 completed claim（RR-20261006-15 复核补修，explain 实测）。
			decisive = append(decisive, bson.M{"status": claimStatusCompleted, "outcome": claimOutcomeRefused, "incarnation": bson.M{"$exists": false}})
		}
	}
	return bson.M{"namespace": stepClaimNamespace, "operation_key": operationKey, "$or": decisive}
}

// claimOutcome 是一份 completion 写进 claim 的结论。
func claimOutcome(completion Completion) string {
	switch {
	case completion.Success:
		return claimOutcomeSuccess
	case completion.Retryable:
		return claimOutcomeRetryable
	default:
		return claimOutcomeRefused
	}
}

// operationSuccess 找同一操作实例另一次尝试已经生效的成功（任何一生）。只读：不写守卫、不接替，
// 供不会执行的投递（过期、认领前过截止、被 fence）在 ack 前把成功重发给协调器（见 replayOperationSuccess）。
func (o *stepOperationInbox) operationSuccess(ctx context.Context, command Command) (Completion, bool, error) {
	// 旧实现在这里用同一个 Limit 截断而不报错，尝试多了可能看不到已经生效的成功（RR-20261006-15）；
	// 现在只读有影响的 claim，并与 Reserve 一样在超过上限时报错。
	claims, err := o.operationClaims(ctx, command.IdempotencyKey, false, 0)
	if err != nil {
		return Completion{}, false, err
	}
	for i := range claims {
		other := claims[i]
		if other.CommandID == command.ID || other.Status == claimStatusSuperseded {
			continue
		}
		completion, settled, err := o.attemptResult(ctx, other)
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
// （原生：投影已写回执、claim 还没来得及标记），有回执顺手标记。settled=false 表示还没有结论。
func (o *stepOperationInbox) attemptResult(ctx context.Context, claim stepClaim) (Completion, bool, error) {
	switch claim.Status {
	case claimStatusCompleted:
		completion, err := DecodeCompletionEffect(claim.Completion)
		return completion, err == nil, err
	case claimStatusPending:
	default:
		return Completion{}, false, ErrConflict
	}
	completion, found, err := o.receipt(ctx, claim.CommandID, claim.Digest)
	if err != nil || !found {
		return Completion{}, false, err
	}
	if err := o.markCompleted(ctx, claim.CommandID, completion); err != nil {
		return Completion{}, false, err
	}
	return completion, true, nil
}

// supersede 让一次租约已过期、还没有回执的尝试永远失效：status 改为 superseded 且 lease_token 加一。
// 它与那次尝试生效点的条件写写同一文档，二者只能有一个提交：生效点先提交，这里的事务重跑后会读到结论；
// 这里先提交，生效点的条件写不再匹配（原生记录投影时被跳过，受影响实体按 RR-20260926-30 驱逐重载；
// Mongo 步骤的 handler 事务中止）。
func (o *stepOperationInbox) supersede(ctx context.Context, claim stepClaim, by string, now time.Time) error {
	filter := bson.M{"_id": claim.ID, "status": claimStatusPending, "lease_token": claim.LeaseToken, "lease_until": bson.M{"$lte": now}}
	update := bson.M{"$set": bson.M{"status": claimStatusSuperseded, "superseded_by": by, "updated_at": now}, "$inc": bson.M{"lease_token": 1}}
	result, err := o.claims().UpdateOne(ctx, filter, update)
	if err != nil {
		return err
	}
	if result == nil || result.MatchedCount != 1 {
		return ErrConflict
	}
	metrics.IncCounter("saga.step_inbox.superseded_total", nil, 1)
	return nil
}

// settleOwnClaim 是 Mongo 步骤的生效点（saga 方向 ②）：在 handler 的 Mongo 事务里、业务写之后，对自己的 claim 做
// 条件写（owner、token、pending、租约未过期）并存下 completion。不匹配说明租约已过期（命令截止已过）或已被较新的
// 尝试接替，返回 errAttemptFenced，调用方让整笔事务中止。条件在事务里最后一次写时检查，剩下的窗口是提交本身的延迟，
// 与原生投影的条件写相同。
func (o *stepOperationInbox) settleOwnClaim(ctx context.Context, reservation Reservation, completion Completion) error {
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		return err
	}
	now := o.now().UTC()
	filter := bson.M{
		"_id": stepClaimID(reservation.commandID), "digest": reservation.digest, "owner": reservation.owner,
		"lease_token": reservation.Token, "status": claimStatusPending, "lease_until": bson.M{"$gt": now},
	}
	result, err := o.claims().UpdateOne(ctx, filter, bson.M{"$set": bson.M{
		"status": claimStatusCompleted, "outcome": claimOutcome(completion), "completion": effect.Payload, "lease_until": time.Unix(0, 0).UTC(), "updated_at": now,
		"expires_at": now.Add(o.receiptTTL),
	}})
	if err != nil {
		return err
	}
	if result == nil || result.MatchedCount != 1 {
		return errAttemptFenced
	}
	return nil
}

// commandIncarnation 从协调器生成的 CommandID（operationKey:attempt 或 operationKey:rN:attempt）取出
// Resume 代际；不是这个格式的 CommandID（测试或手工命令）按第 0 代处理。解析与协调器核对 completion 代际
// 用同一个函数（commandIDIncarnation，B1），两边对“同一生”的判断不会分叉。
func commandIncarnation(command Command) uint32 {
	return commandIDIncarnation(command.IdempotencyKey, command.ID)
}

// releaseLease 交还本次投递刚拿到、但没有用上的租约：把 lease_until 设为现在，只对仍属于这个
// owner/token 的 pending claim 生效。调用方要确定这个租约下没有、也不会再有生效的写：原生步骤只在 handler 以
// coredata.ErrFencedEntityPending 失败时调用（Nest 事务在 WAL 准入前整体回滚，RR-20260926-30）；Mongo 步骤在
// handler 事务失败后调用（事务没有提交；提交结果未知时 claim 若已 completed，条件不匹配，不改动）。不交还的话，
// 之后的重投都会读到“租约有效”，一直等到本次租约自然过期。
func (o *stepOperationInbox) releaseLease(ctx context.Context, reservation Reservation) error {
	if o == nil || o.client == nil || reservation.Duplicate || reservation.Token == 0 || reservation.commandID == "" {
		return nil
	}
	now := o.now().UTC()
	filter := bson.M{
		"_id": stepClaimID(reservation.commandID), "digest": reservation.digest,
		"owner": reservation.owner, "lease_token": reservation.Token, "status": claimStatusPending,
	}
	_, err := o.claims().UpdateOne(ctx, filter, bson.M{"$set": bson.M{"lease_until": now, "updated_at": now}})
	return err
}

func (o *stepOperationInbox) activeReservation(commandID string, digest []byte, token uint64) Reservation {
	return Reservation{
		Token: token, commandID: commandID, owner: o.owner,
		digest: append([]byte(nil), digest...),
	}
}

func (o *stepOperationInbox) markCompleted(ctx context.Context, commandID string, completion Completion) error {
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		return err
	}
	now := o.now().UTC()
	// claim 不存在（回执比 claim 活得久、或是升级前写的回执）时不报错：回执仍是权威、可回放。
	_, err = o.claims().UpdateOne(ctx, bson.M{"_id": stepClaimID(commandID)}, bson.M{"$set": bson.M{
		"status": claimStatusCompleted, "outcome": claimOutcome(completion), "completion": effect.Payload, "lease_until": time.Unix(0, 0).UTC(), "updated_at": now,
		"expires_at": now.Add(o.receiptTTL),
	}})
	return err
}

func (o *stepOperationInbox) claims() fmongo.ICollection {
	return o.client.Database(o.database).Collection(o.claimCollection)
}

func (reservation Reservation) String() string {
	return fmt.Sprintf("token=%d duplicate=%t", reservation.Token, reservation.Duplicate)
}
