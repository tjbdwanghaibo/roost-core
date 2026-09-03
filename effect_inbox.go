package nestwal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var (
	ErrEffectIdentityConflict = errors.New("nestwal: effect identity conflict")
	ErrEffectInboxRequired    = errors.New("nestwal: effect inbox is required")
)

// EffectHandler must perform MongoDB work using the supplied context. The
// inbox receipt and handler writes then commit atomically in one transaction.
type EffectHandler func(context.Context, EffectEnvelope) error

type effectInboxReceipt struct {
	ID        string    `bson:"_id"`
	Digest    []byte    `bson:"digest"`
	CreatedAt time.Time `bson:"created_at"`
}

// MongoEffectInbox is the durable consumer-side half of exactly-once effects.
// JetStream MsgID deduplication remains an optimization; correctness comes from
// this receipt being committed atomically with the business side effect.
type MongoEffectInbox struct {
	client     fmongo.IMongo
	database   string
	collection string
	receiptTTL time.Duration
}

type EffectInboxOptions struct {
	// ReceiptTTL must exceed the maximum broker retention/redelivery period.
	// It bounds storage without weakening idempotence for deliverable messages.
	ReceiptTTL time.Duration
}

func NewMongoEffectInbox(client fmongo.IMongo, database, collection string, options ...EffectInboxOptions) (*MongoEffectInbox, error) {
	database = strings.TrimSpace(database)
	collection = strings.TrimSpace(collection)
	if client == nil || database == "" {
		return nil, fmt.Errorf("nestwal: mongo effect inbox client and database are required")
	}
	if collection == "" {
		collection = "_nest_effect_inbox"
	}
	receiptTTL := 30 * 24 * time.Hour
	if len(options) > 0 && options[0].ReceiptTTL > 0 {
		receiptTTL = options[0].ReceiptTTL
	}
	return &MongoEffectInbox{client: client, database: database, collection: collection, receiptTTL: receiptTTL}, nil
}

func (i *MongoEffectInbox) EnsureInfrastructure(ctx context.Context) error {
	if i == nil || i.client == nil {
		return ErrEffectInboxRequired
	}
	ttlSeconds := int64(i.receiptTTL / time.Second)
	if ttlSeconds <= 0 || ttlSeconds > int64(^uint32(0)>>1) {
		return fmt.Errorf("nestwal: invalid effect inbox receipt ttl %s", i.receiptTTL)
	}
	return i.client.Database(i.database).Collection(i.collection).EnsureIndexes(ctx, []fmongo.IndexModel{{
		Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: int32(ttlSeconds),
	}})
}

func (i *MongoEffectInbox) Handle(ctx context.Context, envelope EffectEnvelope, handler EffectHandler) (bool, error) {
	if i == nil || i.client == nil || envelope.EffectID == "" || handler == nil {
		return false, ErrEffectInboxRequired
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(raw)
	session, err := i.client.StartSession(ctx)
	if err != nil {
		return false, err
	}
	defer session.EndSession(ctx)
	duplicate := false
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		collection := i.client.Database(i.database).Collection(i.collection)
		var receipt effectInboxReceipt
		findErr := collection.FindOne(txCtx, bson.M{"_id": envelope.EffectID}, &receipt)
		if findErr == nil {
			if !bytes.Equal(receipt.Digest, digest[:]) {
				return ErrEffectIdentityConflict
			}
			duplicate = true
			return nil
		}
		if !errors.Is(findErr, fmongo.ErrNotFound) {
			return findErr
		}
		if err := handler(txCtx, envelope); err != nil {
			return err
		}
		_, err := collection.InsertOne(txCtx, effectInboxReceipt{ID: envelope.EffectID, Digest: append([]byte(nil), digest[:]...), CreatedAt: time.Now().UTC()})
		return err
	})
	return duplicate, err
}

type JetStreamEffectConsumerConfig struct {
	Stream        string
	Durable       string
	FilterSubject string
	AckWait       time.Duration
	MaxDeliver    int
}

// SubscribeJetStreamEffects installs a durable consumer that never invokes a
// MongoDB side effect twice for the same EffectID.
func SubscribeJetStreamEffects(ctx context.Context, client fnats.IJetStream, inbox *MongoEffectInbox, config JetStreamEffectConsumerConfig, handler EffectHandler) (fnats.IJetStreamSubscription, error) {
	if client == nil || inbox == nil || handler == nil || strings.TrimSpace(config.Stream) == "" || strings.TrimSpace(config.Durable) == "" || strings.TrimSpace(config.FilterSubject) == "" {
		return nil, fmt.Errorf("nestwal: invalid effect consumer configuration")
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		return nil, fmt.Errorf("nestwal: prepare effect inbox: %w", err)
	}
	return client.Subscribe(ctx, fnats.JetStreamConsumerConfig{
		Stream: config.Stream, Name: config.Durable, Durable: config.Durable,
		FilterSubject: config.FilterSubject, DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait: config.AckWait, MaxDeliver: config.MaxDeliver,
	}, func(messageCtx context.Context, message *fnats.JetStreamMsg) error {
		if message == nil || len(message.Data) == 0 {
			return fmt.Errorf("nestwal: empty effect message")
		}
		var envelope EffectEnvelope
		if err := json.Unmarshal(message.Data, &envelope); err != nil {
			return fmt.Errorf("nestwal: decode effect: %w", err)
		}
		_, err := inbox.Handle(messageCtx, envelope, handler)
		return err
	})
}
