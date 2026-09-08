package dataengine

import (
	"context"
	"errors"
	"fmt"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var ErrOutboxLeaseConflict = errors.New("dataengine outbox: lease conflict")

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
	return store.store.client.Database(store.store.cfg.DefaultDatabase).Collection(outboxCollection)
}

func (store *MongoOutboxStore) Claim(ctx context.Context, owner string, now time.Time, limit int, leaseDuration time.Duration) ([]OutboxItem, error) {
	if store == nil || store.store == nil || owner == "" || limit <= 0 || leaseDuration <= 0 {
		return nil, errors.New("dataengine outbox: invalid claim")
	}
	filter := bson.M{
		"available_at": bson.M{"$lte": now},
		"$or": bson.A{
			bson.M{"lease_until": bson.M{"$lte": now}},
			bson.M{"lease_until": bson.M{"$exists": false}},
		},
	}
	var candidates []outboxDocument
	if err := store.collection().Find(ctx, filter, &candidates, fmongo.FindOption{
		Sort: bson.D{{Key: "available_at", Value: 1}, {Key: "_id", Value: 1}}, Limit: int64(limit), BatchSize: int32(limit),
	}); err != nil {
		return nil, err
	}
	items := make([]OutboxItem, 0, len(candidates))
	for i := range candidates {
		candidate := &candidates[i]
		claimFilter := bson.M{
			"_id":          candidate.ID,
			"available_at": bson.M{"$lte": now},
			"lease_token":  candidate.LeaseToken,
			"$or": bson.A{
				bson.M{"lease_until": bson.M{"$lte": now}},
				bson.M{"lease_until": bson.M{"$exists": false}},
			},
		}
		update := bson.M{
			"$set": bson.M{"lease_owner": owner, "lease_until": now.Add(leaseDuration)},
			"$inc": bson.M{"lease_token": 1},
		}
		var claimed outboxDocument
		err := store.collection().FindOneAndUpdate(ctx, claimFilter, update, &claimed, fmongo.FindOneAndUpdateOption{ReturnAfter: true})
		if errors.Is(err, fmongo.ErrNotFound) {
			continue
		}
		if err != nil {
			return items, err
		}
		items = append(items, claimed.item())
	}
	return items, nil
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
