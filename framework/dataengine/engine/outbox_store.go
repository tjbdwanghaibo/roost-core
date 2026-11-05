package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// ErrOutboxLeaseConflict 表示 Ack/Nack 时 effect 的租约已不属于本 worker（过期后被他人认领或已删除）。
var ErrOutboxLeaseConflict = errors.New("dataengine outbox: lease conflict")

// OutboxLease 是一次认领的身份：owner 加单调递增的 token。
type OutboxLease struct {
	Owner string
	Token uint64
}

type OutboxItem struct {
	TransactionID string
	Effect        coredata.Effect
	Lease         OutboxLease
	Attempt       uint32
	LastError     string
	CreatedAt     time.Time
}

type OutboxBacklog struct {
	Pending   int64
	OldestAge time.Duration
}

// OutboxStore 是 outbox 的租约式队列：Claim 认领到期且无有效租约的 effect，
// Ack/Nack 必须带认领时的租约。
type OutboxStore interface {
	Claim(context.Context, string, time.Time, int, time.Duration) ([]OutboxItem, error)
	Ack(context.Context, string, OutboxLease) error
	Nack(context.Context, string, OutboxLease, time.Time, string) error
	Backlog(context.Context, time.Time) (OutboxBacklog, error)
}

type MongoOutboxStore struct {
	store *MongoStore
}

func NewMongoOutboxStore(store *MongoStore) (*MongoOutboxStore, error) {
	if store == nil || store.client == nil {
		return nil, errors.New("dataengine outbox: mongo store is required")
	}
	return &MongoOutboxStore{store: store}, nil
}

func (store *MongoOutboxStore) collection() fmongo.ICollection {
	return store.store.client.Database(store.store.cfg.DefaultDatabase).Collection(OutboxCollection)
}

// Claim 认领至多 limit 条到期且无有效租约的 effect，固定三次往返：按 available_at 查出候选；一次条件 UpdateMany 给其中
// 仍满足“到期且无有效租约”的文档写上本 worker 的租约、本次认领的标记（lease_claim）并推进 token；再按候选 _id 与
// 标记读回真正认领到的文档（按 available_at, _id 排序）。条件在服务端逐文档原子判定：两次查询之间被别的 worker
// 认领（租约有效）、Nack 推迟（available_at 未到）或已 Ack 删除的候选不匹配、不被改写，租约语义与逐条 CAS 相同；
// 读回只认本次的标记，别处的认领不会被当成自己的。原先每个候选一次 FindOneAndUpdate，一批 64 条要 65 次往返，
// 多个 worker 还在同一批候选上逐条撞车，发布速率被单条往返时延封顶（2026-11-05 联盟 Join 实测约 40 条/s）。
func (store *MongoOutboxStore) Claim(ctx context.Context, owner string, now time.Time, limit int, leaseDuration time.Duration) ([]OutboxItem, error) {
	if store == nil || store.store == nil || owner == "" || limit <= 0 || leaseDuration <= 0 {
		return nil, errors.New("dataengine outbox: invalid claim")
	}
	claimable := func() bson.A {
		return bson.A{
			bson.M{"lease_until": bson.M{"$lte": now}},
			bson.M{"lease_until": bson.M{"$exists": false}},
		}
	}
	var candidates []outboxDocument
	if err := store.collection().Find(ctx, bson.M{"available_at": bson.M{"$lte": now}, "$or": claimable()}, &candidates, fmongo.FindOption{
		Sort: bson.D{{Key: "available_at", Value: 1}, {Key: "_id", Value: 1}}, Limit: int64(limit), BatchSize: int32(limit),
	}); err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	ids := make(bson.A, len(candidates))
	for i := range candidates {
		ids[i] = candidates[i].ID
	}
	claimID, err := newOutboxClaimID()
	if err != nil {
		return nil, err
	}
	result, err := store.collection().UpdateMany(ctx,
		bson.M{"_id": bson.M{"$in": ids}, "available_at": bson.M{"$lte": now}, "$or": claimable()},
		bson.M{
			"$set": bson.M{"lease_owner": owner, "lease_until": now.Add(leaseDuration), "lease_claim": claimID},
			"$inc": bson.M{"lease_token": 1},
		})
	if err != nil {
		return nil, err
	}
	if result == nil || result.ModifiedCount == 0 {
		return nil, nil
	}
	var claimed []outboxDocument
	if err := store.collection().Find(ctx, bson.M{"_id": bson.M{"$in": ids}, "lease_claim": claimID}, &claimed, fmongo.FindOption{
		Sort: bson.D{{Key: "available_at", Value: 1}, {Key: "_id", Value: 1}}, BatchSize: int32(len(ids)),
	}); err != nil {
		// 已写上的租约到期后由下一次认领接手（至少一次投递不受影响）。
		return nil, err
	}
	items := make([]OutboxItem, 0, len(claimed))
	for i := range claimed {
		items = append(items, claimed[i].item())
	}
	return items, nil
}

// newOutboxClaimID 是一次认领的标记：128 位随机数，只用来读回本次 UpdateMany 改写的文档。
func newOutboxClaimID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("dataengine outbox: claim id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (store *MongoOutboxStore) Ack(ctx context.Context, effectID string, lease OutboxLease) error {
	deleted, err := store.collection().DeleteOne(ctx, bson.M{"_id": effectID, "lease_owner": lease.Owner, "lease_token": lease.Token})
	if err != nil {
		return err
	}
	if deleted != 1 {
		return ErrOutboxLeaseConflict
	}
	return nil
}

// OutboxAck 是一条已投递 effect 的确认：effect ID 加认领时的租约。
type OutboxAck struct {
	EffectID string
	Lease    OutboxLease
}

// OutboxBatchAcker 是 OutboxStore 的可选能力：一次往返确认一批已投递的 effect。OutboxWorker 发现存储实现它时
// 一批只确认一次，否则逐条 Ack。
type OutboxBatchAcker interface {
	// AckMany 删除租约仍属于调用方的那些 effect，返回删除条数；有的租约已不属于调用方（过期后被他人认领或已删除）
	// 时其余照删，并返回 ErrOutboxLeaseConflict。
	AckMany(context.Context, []OutboxAck) (int64, error)
}

// AckMany 用一次 BulkWrite 删除一批 effect（条件不匹配的删除不报错，不会中断后面的条目），每条仍按 _id + 租约 owner / token 条件删除（与 Ack 相同）。
func (store *MongoOutboxStore) AckMany(ctx context.Context, acks []OutboxAck) (int64, error) {
	if len(acks) == 0 {
		return 0, nil
	}
	models := make([]fmongo.WriteModel, 0, len(acks))
	for _, ack := range acks {
		models = append(models, fmongo.NewDeleteOneModel(bson.M{"_id": ack.EffectID, "lease_owner": ack.Lease.Owner, "lease_token": ack.Lease.Token}))
	}
	result, err := store.collection().BulkWrite(ctx, models)
	if err != nil {
		return 0, err
	}
	var deleted int64
	if result != nil {
		deleted = result.DeletedCount
	}
	if deleted != int64(len(acks)) {
		return deleted, fmt.Errorf("%w: %d of %d effects no longer leased to %s", ErrOutboxLeaseConflict, int64(len(acks))-deleted, len(acks), acks[0].Lease.Owner)
	}
	return deleted, nil
}

var _ OutboxBatchAcker = (*MongoOutboxStore)(nil)

func (store *MongoOutboxStore) Nack(ctx context.Context, effectID string, lease OutboxLease, next time.Time, lastError string) error {
	update := bson.M{
		"$set":   bson.M{"available_at": next, "last_error": lastError, "lease_until": time.Unix(0, 0).UTC()},
		"$unset": bson.M{"lease_owner": ""},
		"$inc":   bson.M{"attempt": 1},
	}
	result, err := store.collection().UpdateOne(ctx, bson.M{"_id": effectID, "lease_owner": lease.Owner, "lease_token": lease.Token}, update)
	if err != nil {
		return err
	}
	if result == nil || result.MatchedCount != 1 {
		return fmt.Errorf("%w: effect=%s", ErrOutboxLeaseConflict, effectID)
	}
	return nil
}

func (store *MongoOutboxStore) Backlog(ctx context.Context, now time.Time) (OutboxBacklog, error) {
	collection := store.collection()
	pending, err := collection.CountDocuments(ctx, bson.M{})
	if err != nil {
		return OutboxBacklog{}, err
	}
	backlog := OutboxBacklog{Pending: pending}
	if pending == 0 {
		return backlog, nil
	}
	var oldest []outboxDocument
	if err := collection.Find(ctx, bson.M{}, &oldest, fmongo.FindOption{
		Sort: bson.D{{Key: "created_at", Value: 1}}, Limit: 1, BatchSize: 1,
	}); err != nil {
		return OutboxBacklog{}, err
	}
	if len(oldest) > 0 && !oldest[0].CreatedAt.IsZero() && now.After(oldest[0].CreatedAt) {
		backlog.OldestAge = now.Sub(oldest[0].CreatedAt)
	}
	return backlog, nil
}

func (doc outboxDocument) item() OutboxItem {
	return OutboxItem{
		TransactionID: doc.TransactionID,
		Effect: coredata.Effect{
			ID: doc.EffectID, Topic: doc.Topic, Key: doc.Key, Payload: append([]byte(nil), doc.Payload...),
			Headers: cloneHeaders(doc.Headers), AvailableAt: doc.AvailableAt.UnixNano(),
		},
		Lease:   OutboxLease{Owner: doc.LeaseOwner, Token: doc.LeaseToken},
		Attempt: doc.Attempt, LastError: doc.LastError, CreatedAt: doc.CreatedAt,
	}
}
