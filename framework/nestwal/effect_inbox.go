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

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

var (
	// ErrEffectIdentityConflict 表示同一 EffectID 再次到达但内容摘要不同。
	ErrEffectIdentityConflict = errors.New("nestwal: effect identity conflict")
	// ErrEffectInboxRequired 表示 inbox、EffectID 或 handler 缺失。
	ErrEffectInboxRequired = errors.New("nestwal: effect inbox is required")
)

// EffectHandler 必须用传入的 ctx 执行 MongoDB 操作，这样 inbox 回执与 handler 的写入在同一事务中原子提交。
type EffectHandler func(context.Context, EffectEnvelope) error

type effectInboxReceipt struct {
	ID        string    `bson:"_id"`
	Digest    []byte    `bson:"digest"`
	CreatedAt time.Time `bson:"created_at"`
}

// MongoEffectInbox 是外部效果“恰好一次”在消费端的持久部分。JetStream MsgID 去重只是优化，
// 正确性来自回执与业务副作用在同一 Mongo 事务中原子提交。
type MongoEffectInbox struct {
	client     fmongo.IMongo
	database   string
	collection string
	receiptTTL time.Duration
}

type EffectInboxOptions struct {
	// ReceiptTTL 必须长于消息队列的最长保留 / 重投期：它只限制存储，不削弱仍可能投递的消息的幂等性。
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

// Handle 在一个 Mongo 事务内查回执：已存在且摘要相同返回 duplicate=true 不调用 handler，摘要不同
// 返回 ErrEffectIdentityConflict；不存在则调用 handler 并写入回执。
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

// SubscribeJetStreamEffects 安装一个持久消费者，经 MongoEffectInbox 保证同一 EffectID 的
// MongoDB 副作用至多执行一次。
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
