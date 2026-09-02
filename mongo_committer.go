package remoteentity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/cube-core/entity"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	remoteMetaCollection     = "_remote_entity_meta"
	remoteTxCollection       = "_remote_entity_transactions"
	remoteSnapshotCollection = "_remote_entity_snapshots"
)

// MongoCommitter is the authoritative Remote Entity commit implementation.
// Entity CAS metadata, DAO documents, immutable snapshots, and idempotency
// status are committed in one MongoDB transaction.
type MongoCommitter struct {
	mongo          fmongo.IMongo
	database       string
	serverID       int32
	transactionTTL time.Duration
}

// AtomicCommitStore applies Remote Entity commits inside a MongoDB transaction
// owned by another infrastructure component. Implementations must not start or
// commit a nested session and must be idempotent by RemoteTransactionID.
type AtomicCommitStore interface {
	ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error)
}

func NewMongoCommitter(mongo fmongo.IMongo, database string, serverID int32, transactionTTL time.Duration) *MongoCommitter {
	return &MongoCommitter{mongo: mongo, database: database, serverID: serverID, transactionTTL: transactionTTL}
}

type mongoRemoteReceipt struct {
	TransactionID []byte `bson:"transaction_id"`
	EntityID      int64  `bson:"entity_id"`
	StateVersion  uint64 `bson:"state_version"`
	MarkerEpoch   uint64 `bson:"marker_epoch"`
	LockFence     uint64 `bson:"lock_fence"`
	RouteEpoch    uint64 `bson:"route_epoch"`
	CommittedAt   int64  `bson:"committed_at"`
}

type mongoRemoteTransaction struct {
	ID        string                `bson:"_id"`
	State     uint8                 `bson:"state"`
	Receipts  []mongoRemoteReceipt  `bson:"receipts"`
	Commits   []entity.RemoteCommit `bson:"commits"`
	Digest    []byte                `bson:"digest"`
	Cause     string                `bson:"cause,omitempty"`
	CreatedAt time.Time             `bson:"created_at"`
	ExpiresAt *time.Time            `bson:"expires_at,omitempty"`
}

func (s *MongoCommitter) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	receipts, err := s.CommitRemoteBatch(ctx, []entity.RemoteCommit{commit})
	if err != nil {
		return entity.RemoteCommitReceipt{}, err
	}
	if len(receipts) != 1 {
		return entity.RemoteCommitReceipt{}, entity.ErrRemotePersistenceIndeterminate
	}
	return receipts[0], nil
}

func (s *MongoCommitter) CommitRemoteBatch(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	if s == nil || s.mongo == nil || s.database == "" || len(commits) == 0 {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := s.mongo.StartSession(ctx)
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)
	var receipts []entity.RemoteCommitReceipt
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		var applyErr error
		receipts, applyErr = s.ApplyRemoteCommitsInTransaction(txCtx, commits)
		return applyErr
	})
	if err != nil {
		if errors.Is(err, fmongo.ErrVersionConflict) || errors.Is(err, fmongo.ErrDuplicateKey) {
			txID, digest, validationErr := validateRemoteCommitBatch(commits)
			if validationErr != nil {
				return nil, validationErr
			}
			if existing, ok, loadErr := s.loadTransaction(ctx, txID); loadErr == nil && ok {
				if !bytes.Equal(existing.Digest, digest) {
					return nil, fmt.Errorf("%w: transaction id reused with different commits", entity.ErrRemoteRejected)
				}
				status := remoteStatusFromMongoTransaction(txID, existing)
				if status.State == entity.RemoteCommitApplied || status.State == entity.RemoteCommitPublished || status.State == entity.RemoteCommitCommitted {
					return append([]entity.RemoteCommitReceipt(nil), status.Receipts...), nil
				}
			}
			return nil, entity.ErrRemoteVersionConflict
		}
		return nil, err
	}
	return receipts, nil
}

// ApplyRemoteCommitsInTransaction performs the authoritative CAS, data writes,
// snapshots and outbox receipt using the caller's MongoDB session context.
func (s *MongoCommitter) ApplyRemoteCommitsInTransaction(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	if s == nil || s.mongo == nil || s.database == "" || len(commits) == 0 {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	txID, digest, err := validateRemoteCommitBatch(commits)
	if err != nil {
		return nil, err
	}
	var existing mongoRemoteTransaction
	findErr := s.controlDB().Collection(remoteTxCollection).FindOne(ctx, bson.M{"_id": txID.String()}, &existing)
	if findErr == nil {
		if !bytes.Equal(existing.Digest, digest) {
			return nil, fmt.Errorf("%w: transaction id reused with different commits", entity.ErrRemoteRejected)
		}
		status := remoteStatusFromMongoTransaction(txID, existing)
		if status.State == entity.RemoteCommitApplied || status.State == entity.RemoteCommitPublished || status.State == entity.RemoteCommitCommitted {
			return append([]entity.RemoteCommitReceipt(nil), status.Receipts...), nil
		}
		return nil, fmt.Errorf("%w: %s", entity.ErrRemoteRejected, status.Cause)
	}
	if !errors.Is(findErr, fmongo.ErrNotFound) {
		return nil, findErr
	}
	receipts := make([]entity.RemoteCommitReceipt, 0, len(commits))
	for i := range commits {
		receipt, err := s.applyCommit(ctx, commits[i])
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	doc := mongoRemoteTransaction{ID: txID.String(), State: uint8(entity.RemoteCommitApplied), Digest: append([]byte(nil), digest...), CreatedAt: time.Now().UTC()}
	for i := range commits {
		doc.Commits = append(doc.Commits, commits[i].Clone())
	}
	for _, receipt := range receipts {
		doc.Receipts = append(doc.Receipts, encodeMongoReceipt(receipt))
	}
	if _, err := s.controlDB().Collection(remoteTxCollection).InsertOne(ctx, doc); err != nil {
		return nil, err
	}
	return receipts, nil
}

func validateRemoteCommitBatch(commits []entity.RemoteCommit) (entity.RemoteTransactionID, []byte, error) {
	if len(commits) == 0 {
		return entity.RemoteTransactionID{}, nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	txID := commits[0].TransactionID
	seenEntities := make(map[int64]struct{}, len(commits))
	for i := range commits {
		if commits[i].TransactionID != txID {
			return entity.RemoteTransactionID{}, nil, fmt.Errorf("%w: transaction mismatch", entity.ErrRemoteRejected)
		}
		if err := commits[i].Validate(); err != nil {
			return entity.RemoteTransactionID{}, nil, err
		}
		if _, exists := seenEntities[commits[i].EntityID]; exists {
			return entity.RemoteTransactionID{}, nil, fmt.Errorf("%w: duplicate entity %d in transaction", entity.ErrRemoteRejected, commits[i].EntityID)
		}
		seenEntities[commits[i].EntityID] = struct{}{}
	}
	digest, err := remoteCommitBatchDigest(commits)
	return txID, digest, err
}

func (s *MongoCommitter) applyCommit(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	meta := s.controlDB().Collection(remoteMetaCollection)
	filter := bson.M{"_id": commit.EntityID, "_ver": commit.BaseVersion, "$or": bson.A{
		bson.M{"_marker_epoch": bson.M{"$lt": commit.MarkerEpoch}},
		bson.M{"_marker_epoch": commit.MarkerEpoch, "_route_epoch": bson.M{"$lt": commit.RouteEpoch}},
		bson.M{"_marker_epoch": commit.MarkerEpoch, "_route_epoch": commit.RouteEpoch, "_lock_fence": bson.M{"$lte": commit.LockFence}},
	}}
	update := bson.M{"$set": bson.M{"_ver": commit.NextVersion, "_marker_epoch": commit.MarkerEpoch, "_route_epoch": commit.RouteEpoch, "_lock_fence": commit.LockFence, "_deleted": commit.Delete}}
	result, err := meta.UpdateOne(ctx, filter, update)
	if err != nil {
		return entity.RemoteCommitReceipt{}, err
	}
	if result == nil || result.MatchedCount == 0 {
		if commit.BaseVersion != 0 {
			return entity.RemoteCommitReceipt{}, fmongo.ErrVersionConflict
		}
		_, err = meta.InsertOne(ctx, bson.M{"_id": commit.EntityID, "_ver": commit.NextVersion, "_marker_epoch": commit.MarkerEpoch, "_route_epoch": commit.RouteEpoch, "_lock_fence": commit.LockFence, "_deleted": commit.Delete})
		if err != nil {
			return entity.RemoteCommitReceipt{}, err
		}
	}
	for _, mutation := range commit.Mutations {
		doc := bson.M{"_id": mutation.ID, "_ver": commit.NextVersion, "_marker_epoch": commit.MarkerEpoch, "_route_epoch": commit.RouteEpoch, "_lock_fence": commit.LockFence, "data": append([]byte(nil), mutation.Data...)}
		if _, err := s.dataDB(mutation.Database, mutation.DatabaseScope).Collection(mutation.Collection).BulkWrite(ctx, []fmongo.WriteModel{fmongo.NewReplaceOneModel(bson.M{"_id": mutation.ID}, doc, true)}); err != nil {
			return entity.RemoteCommitReceipt{}, err
		}
	}
	for _, item := range commit.Deletes {
		if _, err := s.dataDB(item.Database, item.DatabaseScope).Collection(item.Collection).DeleteOne(ctx, bson.M{"_id": item.ID}); err != nil {
			return entity.RemoteCommitReceipt{}, err
		}
	}
	for _, snapshot := range commit.Snapshots {
		doc := bson.M{"_id": remoteSnapshotStorageKey(snapshot.Key), "key": snapshot.Key, "state_version": snapshot.StateVersion, "base_version": snapshot.BaseVersion, "marker_epoch": snapshot.MarkerEpoch, "route_epoch": snapshot.RouteEpoch, "schema": snapshot.Schema, "codec": snapshot.Codec, "checksum": snapshot.Checksum, "full": snapshot.Full, "data": append([]byte(nil), snapshot.Data...)}
		if _, err := s.controlDB().Collection(remoteSnapshotCollection).BulkWrite(ctx, []fmongo.WriteModel{fmongo.NewReplaceOneModel(bson.M{"_id": remoteSnapshotStorageKey(snapshot.Key)}, doc, true)}); err != nil {
			return entity.RemoteCommitReceipt{}, err
		}
	}
	for _, key := range commit.Invalidations {
		if _, err := s.controlDB().Collection(remoteSnapshotCollection).DeleteOne(ctx, bson.M{"_id": remoteSnapshotStorageKey(key)}); err != nil {
			return entity.RemoteCommitReceipt{}, err
		}
	}
	return entity.RemoteCommitReceipt{TransactionID: commit.TransactionID, EntityID: commit.EntityID, StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch, LockFence: commit.LockFence, RouteEpoch: commit.RouteEpoch, CommittedAt: time.Now().UnixNano()}, nil
}

func (s *MongoCommitter) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	if s == nil || s.mongo == nil || id.IsZero() {
		return entity.RemoteCommitStatus{}, entity.ErrRemoteRejected
	}
	status, ok, err := s.loadStatus(ctx, id)
	if err != nil {
		return entity.RemoteCommitStatus{}, err
	}
	if !ok {
		return entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitUnknown}, nil
	}
	return status, nil
}

func (s *MongoCommitter) EnsureRemoteStorage(ctx context.Context) error {
	if s == nil || s.mongo == nil || s.database == "" {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	ttl := s.transactionTTL
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	if err := s.controlDB().Collection(remoteTxCollection).EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "state", Value: 1}, {Key: "created_at", Value: 1}}, Name: "state_created_at"},
		// Keep the historical index name so EnsureIndexes replaces deployments
		// that expired Applied outbox records by created_at. Only acknowledged
		// records receive expires_at; unpublished commits must never age out.
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Name: "created_at_ttl", Sparse: true, TTL: remoteTransactionTTLSeconds(ttl), RecreateOnConflict: true},
	}); err != nil {
		return fmt.Errorf("remote_entity: ensure transaction indexes: %w", err)
	}
	return nil
}

func (s *MongoCommitter) loadStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, bool, error) {
	doc, ok, err := s.loadTransaction(ctx, id)
	if err != nil || !ok {
		return entity.RemoteCommitStatus{}, ok, err
	}
	return remoteStatusFromMongoTransaction(id, doc), true, nil
}

func (s *MongoCommitter) loadTransaction(ctx context.Context, id entity.RemoteTransactionID) (mongoRemoteTransaction, bool, error) {
	var doc mongoRemoteTransaction
	err := s.controlDB().Collection(remoteTxCollection).FindOne(ctx, bson.M{"_id": id.String()}, &doc)
	if errors.Is(err, fmongo.ErrNotFound) {
		return mongoRemoteTransaction{}, false, nil
	}
	if err != nil {
		return mongoRemoteTransaction{}, false, err
	}
	return doc, true, nil
}

func remoteStatusFromMongoTransaction(id entity.RemoteTransactionID, doc mongoRemoteTransaction) entity.RemoteCommitStatus {
	status := entity.RemoteCommitStatus{TransactionID: id, State: entity.RemoteCommitState(doc.State), Cause: doc.Cause}
	for _, receipt := range doc.Receipts {
		status.Receipts = append(status.Receipts, decodeMongoReceipt(receipt))
	}
	for i := range doc.Commits {
		status.Commits = append(status.Commits, doc.Commits[i].Clone())
	}
	return status
}

func remoteCommitBatchDigest(commits []entity.RemoteCommit) ([]byte, error) {
	raw, err := json.Marshal(commits)
	if err != nil {
		return nil, fmt.Errorf("remote_entity: encode transaction digest: %w", err)
	}
	digest := sha256.Sum256(raw)
	return digest[:], nil
}

func (s *MongoCommitter) PendingRemoteCommits(ctx context.Context, limit int) ([]entity.RemoteCommitStatus, error) {
	if s == nil || s.mongo == nil {
		return nil, entity.ErrRemoteWriteCapabilityDisabled
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var docs []mongoRemoteTransaction
	err := s.controlDB().Collection(remoteTxCollection).Find(ctx, bson.M{"state": uint8(entity.RemoteCommitApplied)}, &docs, fmongo.FindOption{Sort: bson.D{{Key: "created_at", Value: 1}}, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	statuses := make([]entity.RemoteCommitStatus, 0, len(docs))
	for _, doc := range docs {
		if len(doc.Receipts) == 0 {
			return nil, fmt.Errorf("remote_entity: corrupt outbox transaction %q", doc.ID)
		}
		status := entity.RemoteCommitStatus{State: entity.RemoteCommitApplied, Cause: doc.Cause}
		for _, receipt := range doc.Receipts {
			decoded := decodeMongoReceipt(receipt)
			status.Receipts = append(status.Receipts, decoded)
			status.TransactionID = decoded.TransactionID
		}
		for i := range doc.Commits {
			status.Commits = append(status.Commits, doc.Commits[i].Clone())
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (s *MongoCommitter) MarkRemoteCommitPublished(ctx context.Context, id entity.RemoteTransactionID) error {
	now := time.Now().UTC()
	result, err := s.controlDB().Collection(remoteTxCollection).UpdateOne(ctx, bson.M{"_id": id.String(), "state": uint8(entity.RemoteCommitApplied)}, bson.M{"$set": bson.M{"state": uint8(entity.RemoteCommitCommitted), "published_at": now.UnixNano(), "expires_at": now}})
	if err != nil {
		return err
	}
	if result == nil || result.MatchedCount == 0 {
		status, ok, err := s.loadStatus(ctx, id)
		if err != nil {
			return err
		}
		if !ok || status.State != entity.RemoteCommitCommitted {
			return entity.ErrRemotePersistenceIndeterminate
		}
	}
	return nil
}

func remoteTransactionTTLSeconds(ttl time.Duration) int32 {
	seconds := ttl / time.Second
	if seconds < 1 {
		return 1
	}
	const maxInt32 = int64(^uint32(0) >> 1)
	if seconds > time.Duration(maxInt32) {
		return int32(maxInt32)
	}
	return int32(seconds)
}

func (s *MongoCommitter) LoadRemoteSnapshot(ctx context.Context, key entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, minVersion uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	var doc struct {
		StateVersion uint64 `bson:"state_version"`
		BaseVersion  uint64 `bson:"base_version"`
		MarkerEpoch  uint64 `bson:"marker_epoch"`
		RouteEpoch   uint64 `bson:"route_epoch"`
		Schema       uint32 `bson:"schema"`
		Codec        uint16 `bson:"codec"`
		Checksum     uint64 `bson:"checksum"`
		Full         bool   `bson:"full"`
		Data         []byte `bson:"data"`
	}
	err := s.controlDB().Collection(remoteSnapshotCollection).FindOne(ctx, bson.M{"_id": remoteSnapshotStorageKey(key), "state_version": bson.M{"$gte": minVersion}}, &doc)
	if errors.Is(err, fmongo.ErrNotFound) {
		return entity.RemoteSnapshotEnvelope{}, false, nil
	}
	if err != nil {
		return entity.RemoteSnapshotEnvelope{}, false, err
	}
	return entity.RemoteSnapshotEnvelope{Key: key, StateVersion: doc.StateVersion, BaseVersion: doc.BaseVersion, MarkerEpoch: doc.MarkerEpoch, RouteEpoch: doc.RouteEpoch, Schema: doc.Schema, Codec: doc.Codec, Checksum: doc.Checksum, Full: doc.Full, Payload: entity.TakeFrozenRemoteSnapshotPayload(doc.Data)}, true, nil
}

func (s *MongoCommitter) controlDB() fmongo.IDatabase { return s.mongo.Database(s.database) }

func (s *MongoCommitter) dataDB(name string, scope uint8) fmongo.IDatabase {
	if name == "" {
		name = s.database
	}
	if scope == 1 {
		return s.mongo.DatabaseForSid(name, s.serverID)
	}
	return s.mongo.Database(name)
}

func remoteSnapshotStorageKey(key entity.RemoteSnapshotKey) string {
	return fmt.Sprintf("%d:%d:%d:%d:%d", key.Tenant, key.Kind, key.EntityID, key.Scope, key.Policy)
}

func encodeMongoReceipt(r entity.RemoteCommitReceipt) mongoRemoteReceipt {
	return mongoRemoteReceipt{TransactionID: append([]byte(nil), r.TransactionID[:]...), EntityID: r.EntityID, StateVersion: r.StateVersion, MarkerEpoch: r.MarkerEpoch, LockFence: r.LockFence, RouteEpoch: r.RouteEpoch, CommittedAt: r.CommittedAt}
}

func decodeMongoReceipt(r mongoRemoteReceipt) entity.RemoteCommitReceipt {
	var id entity.RemoteTransactionID
	copy(id[:], r.TransactionID)
	return entity.RemoteCommitReceipt{TransactionID: id, EntityID: r.EntityID, StateVersion: r.StateVersion, MarkerEpoch: r.MarkerEpoch, LockFence: r.LockFence, RouteEpoch: r.RouteEpoch, CommittedAt: r.CommittedAt}
}

var _ entity.IRemoteAtomicBatchCommitter = (*MongoCommitter)(nil)
var _ entity.IRemoteSnapshotLoader = (*MongoCommitter)(nil)
var _ entity.IRemoteCommitOutbox = (*MongoCommitter)(nil)
var _ entity.IRemoteStorageInitializer = (*MongoCommitter)(nil)
var _ AtomicCommitStore = (*MongoCommitter)(nil)
