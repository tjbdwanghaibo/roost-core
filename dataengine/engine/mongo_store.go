package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	TransactionCollection = "_dataengine_transactions"
	OutboxCollection      = "_dataengine_outbox"
	ReceiptCollection     = "_dataengine_receipts"
)

var (
	ErrProjectionConflict         = errors.New("dataengine mongo: fatal projection version conflict")
	ErrTransactionIdentity        = errors.New("dataengine mongo: transaction identity conflict")
	ErrReceiptIdentity            = errors.New("dataengine mongo: receipt identity conflict")
	ErrRemoteProjection           = errors.New("dataengine mongo: remote mutation requires remote projector")
	errProjectionBatchUnsupported = errors.New("dataengine mongo: projection batch is unsupported")

	// ErrProjectionBatchNeedsPerRecord asks the projector to re-project a
	// batch one record at a time. A batch cannot tell which of its records
	// failed to match, and "did not match" is ambiguous: it is a genuine
	// version conflict for a fresh record, but an already-applied record
	// replayed after a lost WAL acknowledgement looks identical. Only the
	// single-record path can distinguish them (it compares the stored version
	// and _last_tx), so the batch defers instead of guessing — guessing
	// "conflict" is what used to fence the process on a benign replay.
	ErrProjectionBatchNeedsPerRecord = errors.New("dataengine mongo: projection batch must be retried per record")
)

type MongoStoreConfig struct {
	DefaultDatabase       string
	ServerID              int32
	TransactionReceiptTTL time.Duration
	ReceiptTTL            time.Duration
}

// MongoStore projects canonical WAL mutations with exact version predicates.
// A single ordinary mutation uses a one-document fast path; every transaction
// involving multiple documents, effects, or receipts uses one Mongo session.
type MongoStore struct {
	client        fmongo.IMongo
	cfg           MongoStoreConfig
	now           func() time.Time
	remoteStore   RemoteProjectionStore
	remoteApplier entity.RemoteCommitApplier
}

type RemoteProjectionStore interface {
	ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error)
}

func (store *MongoStore) SetRemoteProjection(remoteStore RemoteProjectionStore, applier entity.RemoteCommitApplier) error {
	if store == nil || remoteStore == nil || applier == nil {
		return errors.New("dataengine mongo: remote store and applier are required")
	}
	store.remoteStore, store.remoteApplier = remoteStore, applier
	return nil
}

func NewMongoStore(client fmongo.IMongo, cfg MongoStoreConfig) (*MongoStore, error) {
	if client == nil {
		return nil, errors.New("dataengine mongo: client is required")
	}
	cfg.DefaultDatabase = strings.TrimSpace(cfg.DefaultDatabase)
	if cfg.DefaultDatabase == "" {
		return nil, errors.New("dataengine mongo: default database is required")
	}
	if cfg.TransactionReceiptTTL <= 0 {
		cfg.TransactionReceiptTTL = 30 * 24 * time.Hour
	}
	if cfg.ReceiptTTL <= 0 {
		cfg.ReceiptTTL = 30 * 24 * time.Hour
	}
	return &MongoStore{client: client, cfg: cfg, now: time.Now}, nil
}

func (store *MongoStore) EnsureInfrastructure(ctx context.Context) error {
	if store == nil || store.client == nil {
		return errors.New("dataengine mongo: store is not configured")
	}
	transactionTTL, err := ttlSeconds(store.cfg.TransactionReceiptTTL)
	if err != nil {
		return err
	}
	db := store.client.Database(store.cfg.DefaultDatabase)
	if err := db.Collection(TransactionCollection).EnsureIndexes(ctx, []fmongo.IndexModel{{
		Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: transactionTTL,
	}}); err != nil {
		return err
	}
	if err := db.Collection(ReceiptCollection).EnsureIndexes(ctx, []fmongo.IndexModel{{
		Keys: bson.D{{Key: "expires_at", Value: 1}}, Name: "ttl_expires_at", ExpireAt: true, RecreateOnConflict: true,
	}}); err != nil {
		return err
	}
	return db.Collection(OutboxCollection).EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "available_at", Value: 1}, {Key: "lease_until", Value: 1}}, Name: "claim_due"},
		{Keys: bson.D{{Key: "effect_id", Value: 1}}, Name: "uniq_effect", Unique: true},
		// The backlog probe reads the oldest staged effect. Without this the
		// sort scans the whole collection, so the probe would get slower in
		// exact proportion to the backlog it exists to measure.
		{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "backlog_oldest"},
	})
}

func ttlSeconds(ttl time.Duration) (int32, error) {
	seconds := int64(ttl / time.Second)
	if seconds <= 0 || seconds > int64(^uint32(0)>>1) {
		return 0, fmt.Errorf("dataengine mongo: invalid ttl %s", ttl)
	}
	return int32(seconds), nil
}

func (store *MongoStore) applyMutation(ctx context.Context, txID string, mutation coredata.Mutation, inTransaction bool) error {
	if mutation.Remote != nil {
		return ErrRemoteProjection
	}
	coll := store.collection(mutation.Key)
	switch mutation.Kind {
	case coredata.MutationPut:
		return store.applyPut(ctx, coll, txID, mutation, inTransaction)
	case coredata.MutationPatch:
		return store.applyPatch(ctx, coll, txID, mutation)
	case coredata.MutationDelete:
		return store.applyDelete(ctx, coll, txID, mutation)
	default:
		return coredata.ErrInvalidMutationKind
	}
}

func (store *MongoStore) applyPut(ctx context.Context, coll fmongo.ICollection, txID string, mutation coredata.Mutation, inTransaction bool) error {
	var doc bson.M
	if err := bson.Unmarshal(mutation.Data, &doc); err != nil {
		return err
	}
	if id, ok := documentInt64(doc["_id"]); !ok || id != mutation.Key.ID {
		return coredata.ErrInvalidDocumentKey
	}
	doc["_id"] = mutation.Key.ID
	doc["_version"] = mutation.NextVersion
	doc["_schema"] = mutation.Schema
	doc["_last_tx"] = txID
	delete(doc, "_deleted")
	delete(doc, "_deleted_at")
	// The aggregation update is a true replacement and still supports upsert;
	// unlike $set it cannot retain fields removed by a migration.
	pipeline := bson.A{bson.M{"$replaceWith": doc}}
	var after bson.M
	err := coll.FindOneAndUpdate(ctx, mutationFilter(mutation), pipeline, &after, fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true})
	if err == nil {
		return nil
	}
	// 多文档路径已在写入前检查持久事务 marker。重复键会中止 Mongo
	// transaction，不能继续在这个 session 中查询；此时缺少 marker 的 Put
	// 不属于已提交重放，应直接报告冲突并让整个事务回滚。
	if inTransaction && errors.Is(err, fmongo.ErrDuplicateKey) {
		return fmt.Errorf("%w: Put duplicate key %s/%s/%d expected=%d next=%d", ErrProjectionConflict,
			mutation.Key.Database, mutation.Key.Resource, mutation.Key.ID, mutation.ExpectedVersion, mutation.NextVersion)
	}
	if errors.Is(err, fmongo.ErrDuplicateKey) || errors.Is(err, fmongo.ErrNotFound) {
		return store.classifyNoMatch(ctx, coll, txID, mutation)
	}
	return err
}

func (store *MongoStore) applyPatch(ctx context.Context, coll fmongo.ICollection, txID string, mutation coredata.Mutation) error {
	update, err := patchUpdate(txID, mutation)
	if err != nil {
		return err
	}
	result, err := coll.UpdateOne(ctx, mutationFilter(mutation), update)
	if err != nil {
		return err
	}
	if result != nil && result.MatchedCount == 1 {
		return nil
	}
	return store.classifyNoMatch(ctx, coll, txID, mutation)
}

func (store *MongoStore) applyDelete(ctx context.Context, coll fmongo.ICollection, txID string, mutation coredata.Mutation) error {
	update := bson.M{"$set": bson.M{
		"_version":    mutation.NextVersion,
		"_schema":     mutation.Schema,
		"_last_tx":    txID,
		"_deleted":    true,
		"_deleted_at": store.now().UTC(),
	}}
	result, err := coll.UpdateOne(ctx, mutationFilter(mutation), update)
	if err != nil {
		return err
	}
	if result != nil && result.MatchedCount == 1 {
		return nil
	}
	return store.classifyNoMatch(ctx, coll, txID, mutation)
}

func mutationFilter(mutation coredata.Mutation) bson.M {
	if mutation.ExpectedVersion == 0 {
		return bson.M{"_id": mutation.Key.ID, "_version": bson.M{"$exists": false}}
	}
	return bson.M{"_id": mutation.Key.ID, "_version": mutation.ExpectedVersion}
}

func patchUpdate(txID string, mutation coredata.Mutation) (bson.M, error) {
	set := bson.M{"_version": mutation.NextVersion, "_schema": mutation.Schema, "_last_tx": txID}
	if len(mutation.Patch.SetBSON) != 0 {
		raw := bson.Raw(mutation.Patch.SetBSON)
		elements, err := raw.Elements()
		if err != nil {
			return nil, err
		}
		for _, element := range elements {
			var value any
			wrapped, err := bson.Marshal(bson.D{{Key: "v", Value: element.Value()}})
			if err != nil {
				return nil, err
			}
			var decoded bson.M
			if err := bson.Unmarshal(wrapped, &decoded); err != nil {
				return nil, err
			}
			value = decoded["v"]
			set[element.Key()] = value
		}
	}
	unset := bson.M{"_deleted": "", "_deleted_at": ""}
	for _, path := range mutation.Patch.Unset {
		unset[path] = ""
	}
	return bson.M{"$set": set, "$unset": unset}, nil
}

type projectionMeta struct {
	Version uint64 `bson:"_version"`
	LastTx  string `bson:"_last_tx"`
}

func (store *MongoStore) classifyNoMatch(ctx context.Context, coll fmongo.ICollection, txID string, mutation coredata.Mutation) error {
	var meta projectionMeta
	err := coll.FindOne(ctx, bson.M{"_id": mutation.Key.ID}, &meta)
	if err == nil && meta.Version == mutation.NextVersion && meta.LastTx == txID {
		return nil
	}
	if err != nil && !errors.Is(err, fmongo.ErrNotFound) {
		return err
	}
	return fmt.Errorf("%w: %s/%s/%d expected=%d next=%d stored=%d", ErrProjectionConflict,
		mutation.Key.Database, mutation.Key.Resource, mutation.Key.ID, mutation.ExpectedVersion, mutation.NextVersion, meta.Version)
}

func (store *MongoStore) collection(key coredata.DocumentKey) fmongo.ICollection {
	database := key.Database
	if database == "" {
		database = store.cfg.DefaultDatabase
	}
	if key.Scope == coredata.DatabaseServer {
		return store.client.DatabaseForSid(database, store.cfg.ServerID).Collection(key.Resource)
	}
	return store.client.Database(database).Collection(key.Resource)
}

type transactionDocument struct {
	ID        string    `bson:"_id"`
	Digest    []byte    `bson:"digest"`
	CreatedAt time.Time `bson:"created_at"`
	Skipped   bool      `bson:"skipped,omitempty"`
}

func (store *MongoStore) insertTransactionMarker(ctx context.Context, document transactionDocument) error {
	_, err := store.client.Database(store.cfg.DefaultDatabase).Collection(TransactionCollection).InsertOne(ctx, document)
	if !errors.Is(err, fmongo.ErrDuplicateKey) {
		return err
	}
	applied, _, checkErr := store.checkTransaction(ctx, document.ID, document.Digest)
	if checkErr != nil {
		return checkErr
	}
	if applied {
		return nil
	}
	return err
}

func (store *MongoStore) leaseFencesMatch(ctx context.Context, receipts []coredata.Receipt) (bool, error) {
	now := store.now().UTC()
	for i := range receipts {
		fence, control, err := coredata.DecodeLeaseFenceReceipt(receipts[i])
		if err != nil {
			return false, err
		}
		if !control {
			continue
		}
		// The predicate comes from the fence itself, not from a filter
		// assembled here: the document is written by another package, and two
		// independent spellings of the same schema is how a rename turns every
		// fenced transaction into a silent no-op.
		var found bson.M
		err = store.client.Database(fence.Database).Collection(fence.Resource).
			FindOne(ctx, fence.Predicate(now), &found)
		if errors.Is(err, fmongo.ErrNotFound) {
			// A stale lease is the expected outcome here, but so is a schema
			// drift that makes the predicate unsatisfiable — and the two look
			// identical from inside. Count it so a drift shows up as every
			// fenced transaction skipping at once instead of as silence.
			metrics.IncCounter("dataengine.fence.skipped.total", metrics.Labels{"resource": fence.Resource}, 1)
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (store *MongoStore) checkTransaction(ctx context.Context, txID string, digest []byte) (bool, bool, error) {
	var stored transactionDocument
	err := store.client.Database(store.cfg.DefaultDatabase).Collection(TransactionCollection).FindOne(ctx, bson.M{"_id": txID}, &stored)
	if errors.Is(err, fmongo.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !bytes.Equal(stored.Digest, digest) {
		return false, false, ErrTransactionIdentity
	}
	return true, stored.Skipped, nil
}

type receiptDocument struct {
	ID            string    `bson:"_id"`
	Namespace     string    `bson:"namespace"`
	ReceiptID     string    `bson:"receipt_id"`
	TransactionID string    `bson:"transaction_id"`
	Digest        []byte    `bson:"digest"`
	Payload       []byte    `bson:"payload,omitempty"`
	ExpiresAt     time.Time `bson:"expires_at"`
	CreatedAt     time.Time `bson:"created_at"`
}

func (store *MongoStore) stageReceipt(ctx context.Context, txID string, receipt coredata.Receipt) error {
	expiresAt := time.Unix(0, receipt.ExpiresAt).UTC()
	if receipt.ExpiresAt == 0 {
		expiresAt = store.now().UTC().Add(store.cfg.ReceiptTTL)
	}
	doc := receiptDocument{
		ID: receipt.Namespace + "/" + receipt.ID, Namespace: receipt.Namespace, ReceiptID: receipt.ID,
		TransactionID: txID, Digest: append([]byte(nil), receipt.Digest...), Payload: append([]byte(nil), receipt.Payload...),
		ExpiresAt: expiresAt, CreatedAt: store.now().UTC(),
	}
	_, err := store.client.Database(store.cfg.DefaultDatabase).Collection(ReceiptCollection).InsertOne(ctx, doc)
	if !errors.Is(err, fmongo.ErrDuplicateKey) {
		return err
	}
	var stored receiptDocument
	if findErr := store.client.Database(store.cfg.DefaultDatabase).Collection(ReceiptCollection).FindOne(ctx, bson.M{"_id": doc.ID}, &stored); findErr != nil {
		return errors.Join(err, findErr)
	}
	if !bytes.Equal(stored.Digest, doc.Digest) || !bytes.Equal(stored.Payload, doc.Payload) {
		return ErrReceiptIdentity
	}
	return nil
}

type outboxDocument struct {
	ID            string            `bson:"_id"`
	EffectID      string            `bson:"effect_id"`
	TransactionID string            `bson:"transaction_id"`
	Topic         string            `bson:"topic"`
	Key           string            `bson:"key,omitempty"`
	Payload       []byte            `bson:"payload,omitempty"`
	Headers       map[string]string `bson:"headers,omitempty"`
	AvailableAt   time.Time         `bson:"available_at"`
	LeaseOwner    string            `bson:"lease_owner,omitempty"`
	LeaseUntil    time.Time         `bson:"lease_until"`
	LeaseToken    uint64            `bson:"lease_token"`
	Attempt       uint32            `bson:"attempt"`
	LastError     string            `bson:"last_error,omitempty"`
	CreatedAt     time.Time         `bson:"created_at"`
}

func (store *MongoStore) stageEffect(ctx context.Context, txID string, effect coredata.Effect) error {
	availableAt := time.Unix(0, effect.AvailableAt).UTC()
	if effect.AvailableAt == 0 {
		availableAt = store.now().UTC()
	}
	doc := outboxDocument{
		ID: effect.ID, EffectID: effect.ID, TransactionID: txID, Topic: effect.Topic, Key: effect.Key,
		Payload: append([]byte(nil), effect.Payload...), Headers: cloneHeaders(effect.Headers),
		AvailableAt: availableAt, CreatedAt: store.now().UTC(),
	}
	_, err := store.client.Database(store.cfg.DefaultDatabase).Collection(OutboxCollection).InsertOne(ctx, doc)
	if !errors.Is(err, fmongo.ErrDuplicateKey) {
		return err
	}
	var stored outboxDocument
	if findErr := store.client.Database(store.cfg.DefaultDatabase).Collection(OutboxCollection).FindOne(ctx, bson.M{"_id": doc.ID}, &stored); findErr != nil {
		return errors.Join(err, findErr)
	}
	if stored.TransactionID != txID || stored.Topic != effect.Topic {
		return ErrTransactionIdentity
	}
	return nil
}

func digestRecord(record coredata.CommitRecord) ([]byte, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	return digest[:], nil
}

func documentKeyLess(left, right coredata.DocumentKey) bool {
	if left.Database != right.Database {
		return left.Database < right.Database
	}
	if left.Scope != right.Scope {
		return left.Scope < right.Scope
	}
	if left.Resource != right.Resource {
		return left.Resource < right.Resource
	}
	return left.ID < right.ID
}

func documentInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case int:
		return int64(typed), true
	default:
		return 0, false
	}
}

func cloneHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	cloned := make(map[string]string, len(headers))
	for key, value := range headers {
		cloned[key] = value
	}
	return cloned
}
