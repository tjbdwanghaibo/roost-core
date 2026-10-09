package saga

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 步骤收件箱的“操作实例”协调，原生步骤（DataEngineStepInbox）与 Mongo 步骤（MongoCommandInbox）共用
// （SAGA.md「原生步骤执行契约」；方案 docs/feature/SAGA-OPERATION-STATE-DOC-2026-10-06.md）。
//
// 每个操作实例（Command.IdempotencyKey）只有一份状态文档 stepOperation：当前尝试（谁持有租约、token、到期、
// 结论）、本生的拒绝（有界）与截止未到的被接替尝试（有界）。Reserve 在一个 Mongo 事务里读回执与这份文档、
// 做判定，需要时以 version 做 CAS 写回；同一操作的并发 Reserve 在这份文档上写冲突串行化。
// 尝试只有在它的生效点对这份文档做条件写（当前尝试是它、token 相同、pending、租约未过期）成功时才生效：
// 原生步骤的生效点是 DataEngine 投影事务（lease fence 直接匹配文档的顶层字段），Mongo 步骤的生效点是
// handler 的 Mongo 事务（settleOwnAttempt）。接替与生效点写同一份文档，二者只能有一个提交。
//
// 判定只读这一份文档：尝试次数、Resume 次数都不进入判定，没有按操作的查询，也没有上限
// （取代了 RR-20261006-15 / -16 的 claim 扫描与上限）。

const (
	// operationStatusPending 必须与投影匹配的值相同，所以取自共享的 fence 契约：当前尝试结论未知、可能持有租约。
	operationStatusPending = coredata.LeaseFenceStatusPending
	// operationStatusSettled：当前尝试的结论已写进 result / completion。
	operationStatusSettled = "settled"

	operationResultSuccess   = "success"
	operationResultRefused   = "refused"
	operationResultRetryable = "retryable"

	// maxRememberedRefusalLives：拒绝只保留代际最大的两生（论证见方案 4.5）。更老一生的拒绝被剪掉时记
	// refusals_dropped_through，那一生的投递不再执行。
	maxRememberedRefusalLives = 2
	// maxRememberedSuperseded：被接替的尝试最多记最近的这么多条（方案 4.4）。只有截止未到的那些需要记住，正常运行里
	// 至多一两条；按接替先后剪掉最早的，被剪掉的早已过了截止。
	maxRememberedSuperseded = 16
)

// ErrCommandExpired 表示命令已过 DeadlineAt：协调器已不再等这次尝试，收件箱不再为它分配租约。
var ErrCommandExpired = errors.New("saga: step command is past its deadline")

// errOperationAttemptInFlight 表示同一操作实例的另一次尝试（或同一命令的另一次投递）还持有有效租约、结果未定。
// 消费者把它当作可重试错误返回（nak 后重投），重投时再判断回放、接替还是执行。
var errOperationAttemptInFlight = errors.New("saga: another attempt of this step operation holds a live lease")

// errAttemptSuperseded 表示这次尝试已被同一操作实例的较新尝试接替，永远不会执行。
var errAttemptSuperseded = errors.New("saga: step attempt was superseded by a newer attempt")

// errAttemptFenced 表示 Mongo 步骤的 handler 事务在提交前对状态文档的条件写没有匹配：租约已过期（命令截止已过）
// 或已被较新的尝试接替。事务整体中止，这次尝试没有生效。
var errAttemptFenced = errors.New("saga: step attempt lost its lease before it could commit")

// stepOperationInbox 是两种收件箱共用的状态文档读写与判定。
type stepOperationInbox struct {
	client              fmongo.IMongo
	database            string
	operationCollection string
	owner               string
	leaseDuration       time.Duration
	receiptTTL          time.Duration
	now                 func() time.Time
	// receipt 读这个收件箱的权威回执：found=false 表示没有；摘要不同返回 ErrIdentityConflict。
	receipt func(ctx context.Context, commandID string, digest []byte) (Completion, bool, error)
}

type Reservation struct {
	Token     uint64
	Duplicate bool
	// Completion 非空时是这个操作实例已有的结果：可能是本命令自己的回执，也可能是同一操作实例
	// 另一次尝试的结果（Completion.CommandID 与当前命令不同，消费者把它重发给协调器）。
	Completion   Completion
	operationKey string
	commandID    string
	owner        string
	digest       []byte
}

// stepOperation 是一个操作实例的状态文档。当前尝试的字段在顶层，字段名与 coredata.LeaseFence 的谓词一致：
// 原生投影事务按 coredata.LeaseFence.Predicate 对它做条件写（确认写 updated_at），与 Reserve 的接替写同一文档、
// 在 Mongo 里串行化（RR-20260926-30 §6）。
type stepOperation struct {
	ID      string `bson:"_id"` // Command.IdempotencyKey
	Version uint64 `bson:"version"`

	CommandID   string    `bson:"command_id"`
	Incarnation uint32    `bson:"incarnation"`
	Digest      []byte    `bson:"digest"`
	Owner       string    `bson:"owner"`
	LeaseToken  uint64    `bson:"lease_token"`
	LeaseUntil  time.Time `bson:"lease_until"`
	DeadlineAt  time.Time `bson:"deadline_at"`
	Status      string    `bson:"status"`
	Result      string    `bson:"result,omitempty"`
	Completion  []byte    `bson:"completion,omitempty"`

	// Refusals 以 "r<代际>" 为键，只留代际最大的 maxRememberedRefusalLives 生。
	Refusals map[string]operationRefusal `bson:"refusals,omitempty"`
	// RefusalsDroppedThrough 是被剪掉的拒绝里最大的代际；nil 表示没剪过。
	RefusalsDroppedThrough *uint32 `bson:"refusals_dropped_through,omitempty"`
	// Superseded 是最近被接替的尝试（最多 maxRememberedSuperseded 条）。
	Superseded []supersededAttempt `bson:"superseded,omitempty"`

	CreatedAt time.Time `bson:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"`
	ExpiresAt time.Time `bson:"expires_at"`
}

type operationRefusal struct {
	CommandID  string `bson:"command_id"`
	Completion []byte `bson:"completion"`
}

type supersededAttempt struct {
	CommandID  string    `bson:"command_id"`
	By         string    `bson:"by"`
	DeadlineAt time.Time `bson:"deadline_at"`
}

func refusalKey(incarnation uint32) string { return "r" + strconv.FormatUint(uint64(incarnation), 10) }

func (o *stepOperationInbox) ensureOperationIndexes(ctx context.Context) error {
	return o.operations().EnsureIndexes(ctx, []fmongo.IndexModel{
		// 所有读写都按 _id；只需要过期清理。
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Name: "ttl_expires_at", ExpireAt: true, RecreateOnConflict: true},
	})
}

// reserve 在一个 Mongo 事务里决定这次投递做什么（reserveInTransaction）；首次创建状态文档时并发插入撞唯一键，重试一次。
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

// reserveInTransaction 按方案 3.1 的表依次判定（编号即表中的行）：
//
//  1. 本命令已有回执：回放（并结算仍 pending 的自己）。
//  2. 命令已过截止：ErrCommandExpired。
//  3. 还没有状态文档：新建，本命令执行。
//  4. 本命令已被接替：errAttemptSuperseded。
//     5～8. 本命令就是当前尝试：摘要不同 → ErrIdentityConflict；已结算 → 回放；租约有效 → Duplicate（同一命令在途）；
//     租约过期 → 重新取得租约并执行。
//     9～15. 另一次尝试是当前尝试：它已投影未结算先结算；有成功（任何一生）→ 回放；有本生的拒绝 → 回放；这一生的拒绝
//     可能已被剪掉 → errAttemptSuperseded；它的租约有效 → errOperationAttemptInFlight；过期 → 接替；其余 → 执行。
//
// 租约封顶到命令截止时间：lease_until = min(now+LeaseDuration, Command.DeadlineAt)。协调器只在截止后才判这次尝试超时、
// 放弃或发出下一次尝试，所以截止后才到生效点的尝试（崩溃重启后的 WAL 重放、投影积压、Mongo 事务在写冲突重试里拖过截止）
// 一律不生效——一次尝试只可能在协调器等它的窗口内生效。
func (o *stepOperationInbox) reserveInTransaction(ctx context.Context, command Command, digest []byte) (Reservation, error) {
	key := command.IdempotencyKey
	if completion, found, err := o.receipt(ctx, command.ID, digest); err != nil || found {
		if found {
			// 回执是权威，状态文档只是协调行：结算失败不改变 Reserve 的结论（真实服务端会因写错误中止事务并由驱动重跑）。
			// 但不能静默吞掉——将来出现确定性失败时会一直重跑到超时，日志与计数是唯一线索（RR-20260927-16）。
			if markErr := o.markCompleted(ctx, key, command.ID, completion); markErr != nil {
				metrics.IncCounter("saga.step_inbox.mark_completed_error_total", nil, 1)
				slog.Warn("saga step inbox: mark operation attempt settled failed; the receipt stays authoritative",
					"command_id", command.ID, "err", markErr)
			}
		}
		return Reservation{Duplicate: found, Completion: completion}, err
	}
	now := o.now().UTC()
	if !now.Before(command.DeadlineAt) {
		return Reservation{}, ErrCommandExpired
	}
	state, exists, err := o.loadOperation(ctx, key)
	if err != nil {
		return Reservation{}, err
	}
	if !exists {
		return o.createOperation(ctx, command, digest, now)
	}
	if state.supersededAttempt(command.ID) {
		return Reservation{}, errAttemptSuperseded
	}
	if state.CommandID == command.ID {
		if !bytes.Equal(state.Digest, digest) {
			return Reservation{}, ErrIdentityConflict
		}
		switch state.Status {
		case operationStatusSettled:
			completion, err := DecodeCompletionEffect(state.Completion)
			return Reservation{Token: state.LeaseToken, Duplicate: true, Completion: completion}, err
		case operationStatusPending:
		default:
			return Reservation{}, ErrConflict
		}
		if state.LeaseUntil.After(now) {
			return Reservation{Token: state.LeaseToken, Duplicate: true}, nil
		}
		return o.grantLease(ctx, state, command, digest, now, false)
	}
	if state.Status == operationStatusPending {
		// 第 9 步：当前尝试已有回执（原生：投影已提交，还没结算）就先结算，再按它的结论判定。
		completion, found, err := o.receipt(ctx, state.CommandID, state.Digest)
		if err != nil {
			return Reservation{}, err
		}
		if found {
			if state, err = o.settleFromReceipt(ctx, state, completion, now); err != nil {
				return Reservation{}, err
			}
		}
	}
	if state.Status == operationStatusSettled && state.Result == operationResultSuccess {
		// 成功在任何一生里都不重做：Resume 之后的新一生遇到旧一生已生效的步骤，同样回放它。
		completion, err := DecodeCompletionEffect(state.Completion)
		return Reservation{Duplicate: true, Completion: completion}, err
	}
	incarnation := commandIncarnation(command)
	if refusal, ok := state.Refusals[refusalKey(incarnation)]; ok {
		// 业务拒绝只在同一生里回放；Resume 的目的就是在修复原因之后重新执行。
		completion, err := DecodeCompletionEffect(refusal.Completion)
		return Reservation{Duplicate: true, Completion: completion}, err
	}
	if state.RefusalsDroppedThrough != nil && incarnation <= *state.RefusalsDroppedThrough {
		// 这一生的拒绝可能已被剪掉，不能证明它没被拒绝过；协调器已前进至少两生，不再接收它的结果（方案 4.5）。
		return Reservation{}, errAttemptSuperseded
	}
	if state.Status == operationStatusPending {
		if state.LeaseUntil.After(now) {
			return Reservation{}, errOperationAttemptInFlight
		}
		return o.grantLease(ctx, state, command, digest, now, true)
	}
	// 当前尝试是可重试失败，或别的生的拒绝：那次尝试已有结论且没有生效，这次执行。
	return o.grantLease(ctx, state, command, digest, now, false)
}

func (o *stepOperationInbox) loadOperation(ctx context.Context, key string) (stepOperation, bool, error) {
	var state stepOperation
	err := o.operations().FindOne(ctx, bson.M{"_id": key}, &state)
	if errors.Is(err, fmongo.ErrNotFound) {
		return stepOperation{}, false, nil
	}
	return state, err == nil, err
}

// createOperation 写这个操作的第一份状态文档：本命令是当前尝试，token 1。
func (o *stepOperationInbox) createOperation(ctx context.Context, command Command, digest []byte, now time.Time) (Reservation, error) {
	state := stepOperation{
		ID: command.IdempotencyKey, Version: 1,
		CommandID: command.ID, Incarnation: commandIncarnation(command), Digest: append([]byte(nil), digest...),
		Owner: o.owner, LeaseToken: 1, LeaseUntil: o.leaseUntil(command, now), DeadlineAt: command.DeadlineAt.UTC(),
		Status:    operationStatusPending,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(o.receiptTTL),
	}
	if _, err := o.operations().InsertOne(ctx, state); err != nil {
		return Reservation{}, err
	}
	return o.activeReservation(command.IdempotencyKey, command.ID, digest, 1), nil
}

// grantLease 把本命令设为当前尝试并授予新租约（token 加一，旧尝试的生效点从此不再匹配）。takeover=true 表示
// 接替一次租约过期、结论未知的其他尝试：把它记进 superseded，它的迟到投递不再执行。
// 写入以读到的 version 做 CAS；在事务里与并发写同一文档写冲突，输家整笔重跑、重新判定。
func (o *stepOperationInbox) grantLease(ctx context.Context, state stepOperation, command Command, digest []byte, now time.Time, takeover bool) (Reservation, error) {
	superseded := slices.Clone(state.Superseded)
	if takeover {
		// 被接替的尝试记下来，它的迟到投递不再执行；只留最近的 maxRememberedSuperseded 条（方案 4.4）。
		superseded = append(superseded, supersededAttempt{CommandID: state.CommandID, By: command.ID, DeadlineAt: state.DeadlineAt})
		if len(superseded) > maxRememberedSuperseded {
			superseded = superseded[len(superseded)-maxRememberedSuperseded:]
		}
	}
	refusals, droppedThrough := pruneRefusals(state.Refusals, state.RefusalsDroppedThrough)
	token := state.LeaseToken + 1
	set := bson.M{
		"version": state.Version + 1, "command_id": command.ID, "incarnation": commandIncarnation(command),
		"digest": append([]byte(nil), digest...), "owner": o.owner, "lease_token": token,
		"lease_until": o.leaseUntil(command, now), "deadline_at": command.DeadlineAt.UTC(), "status": operationStatusPending,
		"superseded": superseded, "refusals": refusals, "updated_at": now, "expires_at": now.Add(o.receiptTTL),
	}
	if droppedThrough != nil {
		set["refusals_dropped_through"] = *droppedThrough
	}
	update := bson.M{"$set": set, "$unset": bson.M{"result": "", "completion": ""}}
	result, err := o.operations().UpdateOne(ctx, bson.M{"_id": state.ID, "version": state.Version}, update)
	if err != nil {
		return Reservation{}, err
	}
	if result == nil || result.MatchedCount != 1 {
		return Reservation{}, ErrConflict
	}
	if takeover {
		metrics.IncCounter("saga.step_inbox.superseded_total", nil, 1)
	}
	return o.activeReservation(state.ID, command.ID, digest, token), nil
}

func (o *stepOperationInbox) leaseUntil(command Command, now time.Time) time.Time {
	leaseUntil := now.Add(o.leaseDuration)
	if command.DeadlineAt.Before(leaseUntil) {
		leaseUntil = command.DeadlineAt.UTC()
	}
	return leaseUntil
}

// pruneRefusals 只留代际最大的 maxRememberedRefusalLives 生，返回剪枝后的表与被剪掉的最大代际（没剪且以前也没剪过时为 nil）。
func pruneRefusals(refusals map[string]operationRefusal, droppedThrough *uint32) (map[string]operationRefusal, *uint32) {
	kept := make(map[string]operationRefusal, len(refusals))
	if len(refusals) <= maxRememberedRefusalLives {
		for key, refusal := range refusals {
			kept[key] = refusal
		}
		return kept, droppedThrough
	}
	lives := make([]uint32, 0, len(refusals))
	byLife := make(map[uint32]string, len(refusals))
	for key := range refusals {
		life, err := strconv.ParseUint(key[1:], 10, 32)
		if err != nil {
			continue
		}
		lives = append(lives, uint32(life))
		byLife[uint32(life)] = key
	}
	slices.Sort(lives)
	cut := len(lives) - maxRememberedRefusalLives
	for i, life := range lives {
		if i < cut {
			if droppedThrough == nil || life > *droppedThrough {
				dropped := life
				droppedThrough = &dropped
			}
			continue
		}
		kept[byLife[life]] = refusals[byLife[life]]
	}
	return kept, droppedThrough
}

func (state stepOperation) supersededAttempt(commandID string) bool {
	for _, attempt := range state.Superseded {
		if attempt.CommandID == commandID {
			return true
		}
	}
	return false
}

// settleFromReceipt 在 Reserve 里结算一次已有回执、状态仍 pending 的当前尝试（原生：投影已提交、还没结算），返回结算后的状态。
func (o *stepOperationInbox) settleFromReceipt(ctx context.Context, state stepOperation, completion Completion, now time.Time) (stepOperation, error) {
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		return stepOperation{}, err
	}
	result, err := o.operations().UpdateOne(ctx, bson.M{"_id": state.ID, "version": state.Version}, settleUpdate(state.Incarnation, state.CommandID, completion, effect.Payload, now, o.receiptTTL))
	if err != nil {
		return stepOperation{}, err
	}
	if result == nil || result.MatchedCount != 1 {
		return stepOperation{}, ErrConflict
	}
	state.Version++
	state.Status, state.Result, state.Completion = operationStatusSettled, operationResult(completion), effect.Payload
	if state.Result == operationResultRefused {
		refusals := make(map[string]operationRefusal, len(state.Refusals)+1)
		for key, refusal := range state.Refusals {
			refusals[key] = refusal
		}
		refusals[refusalKey(state.Incarnation)] = operationRefusal{CommandID: state.CommandID, Completion: effect.Payload}
		state.Refusals = refusals
	}
	return state, nil
}

// settleUpdate 是写结论的更新：当前尝试 settled，存下 completion，租约置为过去；拒绝同时记进本生的 refusals。
func settleUpdate(incarnation uint32, commandID string, completion Completion, payload []byte, now time.Time, ttl time.Duration) bson.M {
	set := bson.M{
		"status": operationStatusSettled, "result": operationResult(completion), "completion": payload,
		"lease_until": time.Unix(0, 0).UTC(), "updated_at": now, "expires_at": now.Add(ttl),
	}
	if operationResult(completion) == operationResultRefused {
		set["refusals."+refusalKey(incarnation)] = operationRefusal{CommandID: commandID, Completion: payload}
	}
	return bson.M{"$set": set, "$inc": bson.M{"version": 1}}
}

// operationResult 是一份 completion 的结论。
func operationResult(completion Completion) string {
	switch {
	case completion.Success:
		return operationResultSuccess
	case completion.Retryable:
		return operationResultRetryable
	default:
		return operationResultRefused
	}
}

// operationSuccess 找同一操作实例另一次尝试已经生效的成功（任何一生）。只读判定、不授予租约，供不会执行的投递
// （过期、认领前过截止、被 fence、被接替）在 ack 前把成功重发给协调器（见 replayOperationSuccess）。
// 当前尝试已投影、还没结算时顺手结算（与 Reserve 第 9 步相同）。
func (o *stepOperationInbox) operationSuccess(ctx context.Context, command Command) (Completion, bool, error) {
	state, exists, err := o.loadOperation(ctx, command.IdempotencyKey)
	if err != nil || !exists || state.CommandID == command.ID {
		return Completion{}, false, err
	}
	switch state.Status {
	case operationStatusSettled:
		if state.Result != operationResultSuccess {
			return Completion{}, false, nil
		}
		completion, err := DecodeCompletionEffect(state.Completion)
		return completion, err == nil, err
	case operationStatusPending:
	default:
		return Completion{}, false, ErrConflict
	}
	completion, found, err := o.receipt(ctx, state.CommandID, state.Digest)
	if err != nil || !found {
		return Completion{}, false, err
	}
	if err := o.markCompleted(ctx, state.ID, state.CommandID, completion); err != nil {
		return Completion{}, false, err
	}
	return completion, completion.Success, nil
}

// settleOwnAttempt 是 Mongo 步骤的生效点（saga 方向 ②）：在 handler 的 Mongo 事务里、业务写之后，对状态文档做条件写
// （当前尝试是自己、owner、token、pending、租约未过期）并存下 completion。不匹配说明租约已过期（命令截止已过）或已被
// 较新的尝试接替，返回 errAttemptFenced，调用方让整笔事务中止。条件在事务里最后一次写时检查，剩下的窗口是提交本身的
// 延迟，与原生投影的条件写相同。
func (o *stepOperationInbox) settleOwnAttempt(ctx context.Context, reservation Reservation, completion Completion) error {
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		return err
	}
	now := o.now().UTC()
	filter := bson.M{
		"_id": reservation.operationKey, "command_id": reservation.commandID, "digest": reservation.digest, "owner": reservation.owner,
		"lease_token": reservation.Token, "status": operationStatusPending, "lease_until": bson.M{"$gt": now},
	}
	incarnation := commandIDIncarnation(reservation.operationKey, reservation.commandID)
	result, err := o.operations().UpdateOne(ctx, filter, settleUpdate(incarnation, reservation.commandID, completion, effect.Payload, now, o.receiptTTL))
	if err != nil {
		return err
	}
	if result == nil || result.MatchedCount != 1 {
		return errAttemptFenced
	}
	return nil
}

// markCompleted 在读到一次尝试的权威回执之后写下它的结论（原生步骤：投影写回执，结论由之后的 Reserve / Replay 写）。
// 只对仍是当前尝试、仍 pending 的状态生效；已结算或已不是当前尝试时什么都不做（回执仍是权威、可回放）。
func (o *stepOperationInbox) markCompleted(ctx context.Context, operationKey, commandID string, completion Completion) error {
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		return err
	}
	now := o.now().UTC()
	filter := bson.M{"_id": operationKey, "command_id": commandID, "status": operationStatusPending}
	_, err = o.operations().UpdateOne(ctx, filter, settleUpdate(commandIDIncarnation(operationKey, commandID), commandID, completion, effect.Payload, now, o.receiptTTL))
	return err
}

// commandIncarnation 从协调器生成的 CommandID（operationKey:attempt 或 operationKey:rN:attempt）取出
// Resume 代际；不是这个格式的 CommandID（测试或手工命令）按第 0 代处理。解析与协调器核对 completion 代际
// 用同一个函数（commandIDIncarnation，B1），两边对“同一生”的判断不会分叉。
func commandIncarnation(command Command) uint32 {
	return commandIDIncarnation(command.IdempotencyKey, command.ID)
}

// releaseLease 交还本次投递刚拿到、但没有用上的租约：把 lease_until 设为现在，只对仍是当前尝试、仍属于这个
// owner/token 的 pending 状态生效。调用方要确定这个租约下没有、也不会再有生效的写：原生步骤只在 handler 以
// coredata.ErrFencedEntityPending 失败时调用（Nest 事务在 WAL 准入前整体回滚，RR-20260926-30）；Mongo 步骤在
// handler 事务失败后调用（事务没有提交；提交结果未知时状态若已 settled，条件不匹配，不改动）。不交还的话，
// 之后的重投都会读到“租约有效”，一直等到本次租约自然过期。
func (o *stepOperationInbox) releaseLease(ctx context.Context, reservation Reservation) error {
	if o == nil || o.client == nil || reservation.Duplicate || reservation.Token == 0 || reservation.commandID == "" {
		return nil
	}
	now := o.now().UTC()
	filter := bson.M{
		"_id": reservation.operationKey, "command_id": reservation.commandID, "digest": reservation.digest,
		"owner": reservation.owner, "lease_token": reservation.Token, "status": operationStatusPending,
	}
	_, err := o.operations().UpdateOne(ctx, filter, bson.M{"$set": bson.M{"lease_until": now, "updated_at": now}, "$inc": bson.M{"version": 1}})
	return err
}

func (o *stepOperationInbox) activeReservation(operationKey, commandID string, digest []byte, token uint64) Reservation {
	return Reservation{
		Token: token, operationKey: operationKey, commandID: commandID, owner: o.owner,
		digest: append([]byte(nil), digest...),
	}
}

func (o *stepOperationInbox) operations() fmongo.ICollection {
	return o.client.Database(o.database).Collection(o.operationCollection)
}

func (reservation Reservation) String() string {
	return fmt.Sprintf("token=%d duplicate=%t", reservation.Token, reservation.Duplicate)
}
