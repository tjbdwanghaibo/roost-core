package saga

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
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
	kitnats "github.com/tjbdwanghaibo/roost-kit/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// StepHandler runs inside a retryable MongoDB transaction. It may only mutate
// MongoDB through the supplied context. Network calls and other irreversible
// side effects must be emitted through a transactional outbox because the
// MongoDB driver is allowed to invoke a transaction callback again.
type StepHandler func(context.Context, coresaga.Command) (coresaga.Completion, error)

type MongoCommandInbox struct {
	client               fmongo.IMongo
	database, collection string
	receiptTTL           time.Duration
}

type CommandInboxOptions struct{ ReceiptTTL time.Duration }

func NewMongoCommandInbox(client fmongo.IMongo, database, collection string, options ...CommandInboxOptions) (*MongoCommandInbox, error) {
	database = strings.TrimSpace(database)
	collection = strings.TrimSpace(collection)
	if client == nil || database == "" {
		return nil, fmt.Errorf("saga inbox: client and database are required")
	}
	if collection == "" {
		collection = "_saga_step_inbox"
	}
	ttl := 30 * 24 * time.Hour
	if len(options) > 0 && options[0].ReceiptTTL > 0 {
		ttl = options[0].ReceiptTTL
	}
	return &MongoCommandInbox{client: client, database: database, collection: collection, receiptTTL: ttl}, nil
}

func (i *MongoCommandInbox) EnsureInfrastructure(ctx context.Context) error {
	ttl := int64(i.receiptTTL / time.Second)
	if ttl <= 0 || ttl > int64(^uint32(0)>>1) {
		return fmt.Errorf("saga inbox: invalid receipt ttl %s", i.receiptTTL)
	}
	return i.collectionRef().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: int32(ttl)}})
}

func (i *MongoCommandInbox) Handle(ctx context.Context, command coresaga.Command, handler StepHandler) (coresaga.Completion, bool, error) {
	if i == nil || i.client == nil || handler == nil {
		return coresaga.Completion{}, false, coresaga.ErrInvalidRecord
	}
	if err := command.Validate(); err != nil {
		return coresaga.Completion{}, false, err
	}
	digest := commandDigest(command)
	session, err := i.client.StartSession(ctx)
	if err != nil {
		return coresaga.Completion{}, false, err
	}
	defer session.EndSession(ctx)
	var completion coresaga.Completion
	duplicate := false
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		duplicate = false
		var receipt commandReceiptDoc
		findErr := i.collectionRef().FindOne(txCtx, bson.M{"_id": command.ID}, &receipt)
		if findErr == nil {
			if !bytes.Equal(receipt.Digest, digest) {
				return coresaga.ErrIdentityConflict
			}
			if err := json.Unmarshal(receipt.Completion, &completion); err != nil {
				return err
			}
			duplicate = true
			return nil
		}
		if !errors.Is(findErr, fmongo.ErrNotFound) {
			return findErr
		}
		// Reserve CommandID before invoking the handler. Mongo's unique _id
		// index serializes overlapping redeliveries before either can execute
		// business writes; the reservation is invisible until this transaction
		// commits and is rolled back together with a handler failure.
		createdAt := time.Now().UTC()
		if _, insertErr := i.collectionRef().InsertOne(txCtx, commandReceiptDoc{ID: command.ID, Digest: digest, CreatedAt: createdAt}); insertErr != nil {
			return insertErr
		}
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
		updated, updateErr := i.collectionRef().UpdateOne(txCtx, bson.M{"_id": command.ID}, bson.M{"$set": bson.M{"completion": raw, "created_at": createdAt}})
		if updateErr != nil {
			return updateErr
		}
		if updated == nil || updated.MatchedCount != 1 {
			return coresaga.ErrConflict
		}
		return nil
	})
	if errors.Is(err, fmongo.ErrDuplicateKey) {
		// A concurrent transaction reserved the same CommandID first. Once its
		// commit becomes visible, replay the durable completion without running
		// the handler again.
		completion, err = i.readReceipt(ctx, command.ID, digest)
		if err == nil {
			duplicate = true
		}
	}
	if err != nil {
		return coresaga.Completion{}, false, err
	}
	// A redelivery has a new delivery ID but the same idempotent operation.
	completion.CommandID = command.ID
	return completion, duplicate, nil
}

func (i *MongoCommandInbox) readReceipt(ctx context.Context, commandID string, digest []byte) (coresaga.Completion, error) {
	var receipt commandReceiptDoc
	if err := i.collectionRef().FindOne(ctx, bson.M{"_id": commandID}, &receipt); err != nil {
		return coresaga.Completion{}, err
	}
	if !bytes.Equal(receipt.Digest, digest) || len(receipt.Completion) == 0 {
		return coresaga.Completion{}, coresaga.ErrIdentityConflict
	}
	var completion coresaga.Completion
	if err := json.Unmarshal(receipt.Completion, &completion); err != nil {
		return coresaga.Completion{}, err
	}
	return completion, nil
}

// Replay returns a completion already committed for this exact command. It
// never invokes business code and is used to finish publishing after a step
// attempt deadline has elapsed.
func (i *MongoCommandInbox) Replay(ctx context.Context, command coresaga.Command) (coresaga.Completion, bool, error) {
	if i == nil || i.client == nil {
		return coresaga.Completion{}, false, coresaga.ErrInvalidRecord
	}
	completion, err := i.readReceipt(ctx, command.ID, commandDigest(command))
	if errors.Is(err, fmongo.ErrNotFound) {
		return coresaga.Completion{}, false, nil
	}
	if err != nil {
		return coresaga.Completion{}, false, err
	}
	return completion, true, nil
}

type StepConsumerConfig struct {
	Stream, Durable, Topic string
	AckWait                time.Duration
	MaxDeliver             int
	MaxAckPending          int
	NakBackoffMin          time.Duration
	NakBackoffMax          time.Duration
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
			return kitnats.Permanent(coresaga.ErrInvalidRecord)
		}
		if len(message.Data) > maxWireEnvelopeBytes {
			return kitnats.Permanent(coresaga.ErrInvalidRecord)
		}
		var envelope commandEnvelope
		if err := json.Unmarshal(message.Data, &envelope); err != nil {
			logConsumerError("step decode", message, err)
			return kitnats.Permanent(err)
		}
		if envelope.Version != coresaga.WireVersion {
			return kitnats.Permanent(coresaga.ErrInvalidRecord)
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
			replayCtx, cancel := context.WithTimeout(messageCtx, 3*time.Second)
			completion, found, err := inbox.Replay(replayCtx, command)
			if err == nil && found {
				err = transport.PublishCompletion(replayCtx, completion)
			}
			cancel()
			if err != nil {
				logConsumerError("stale step completion replay", message, err)
			}
			return err
		}
		processCtx, cancel := context.WithDeadline(messageCtx, command.DeadlineAt)
		completion, _, err := inbox.Handle(processCtx, command, handler)
		if err != nil {
			cancel()
			logConsumerError("step", message, err)
			return err
		}
		err = transport.PublishCompletion(processCtx, completion)
		cancel()
		if err != nil {
			logConsumerError("step completion publish", message, err)
		}
		return err
	})
}

// SubscribeStep is retained for raw Mongo handlers.
// Deprecated: use SubscribeMongoStep or SubscribeDataEngineStep explicitly.
func SubscribeStep(ctx context.Context, client fnats.IJetStream, transport *JetStreamPublisher, inbox *MongoCommandInbox, config StepConsumerConfig, handler StepHandler) (fnats.IJetStreamSubscription, error) {
	return SubscribeMongoStep(ctx, client, transport, inbox, config, handler)
}

// SubscribeDataEngineStep coordinates duplicate deliveries, but never
// publishes the completion directly. A native handler must execute its Nest
// transaction with inbox.Bind(command, reservation) and
// coresaga.EmitCompletion; obtain reservation explicitly with
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
			_, found, replayErr := inbox.Replay(messageCtx, command)
			if replayErr != nil {
				return replayErr
			}
			if found {
				return nil
			}
			return context.DeadlineExceeded
		}
		processCtx, cancel := context.WithDeadline(messageCtx, command.DeadlineAt)
		defer cancel()
		reservation, err := inbox.Reserve(processCtx, command)
		if err != nil {
			return err
		}
		if reservation.Duplicate && reservation.Completion.CommandID != "" {
			return nil
		}
		if !reservation.Duplicate {
			processCtx = withReservation(processCtx, reservation)
			completion, err := handler(processCtx, command)
			if err != nil {
				return err
			}
			completion.CommandID, completion.IdempotencyKey, completion.SagaID = command.ID, command.IdempotencyKey, command.SagaID
			if err := completion.Validate(); err != nil {
				return err
			}
		}
		_, err = inbox.waitReplay(processCtx, command)
		return err
	})
}

func decodeStepCommand(message *fnats.JetStreamMsg) (coresaga.Command, error) {
	if message == nil || len(message.Data) > maxWireEnvelopeBytes {
		return coresaga.Command{}, coresaga.ErrInvalidRecord
	}
	var envelope commandEnvelope
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		return coresaga.Command{}, err
	}
	if envelope.Version != coresaga.WireVersion || envelope.Command.Validate() != nil {
		return coresaga.Command{}, coresaga.ErrInvalidRecord
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
func commandDigest(c coresaga.Command) []byte {
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return sum[:]
}
