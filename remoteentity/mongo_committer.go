package remoteentity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
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
	// DataEngine 已原子落库后会经发布路径再次调用这里。持久回执已存在时，
	// majority 读取并校验 digest 即可重放，不再为只读回执开一次 Mongo 事务。
	txID, digest, err := validateRemoteCommitBatch(commits)
	if err != nil {
		return nil, err
	}
	if existing, found, err := s.loadTransaction(ctx, txID); err != nil {
		return nil, err
	} else if found {
		return committedRemoteReceipts(txID, digest, existing)
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
			if existing, ok, loadErr := s.loadTransaction(ctx, txID); loadErr == nil && ok {
				return committedRemoteReceipts(txID, digest, existing)
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
		return committedRemoteReceipts(txID, digest, existing)
	}
	if !errors.Is(findErr, fmongo.ErrNotFound) {
		return nil, findErr
	}
	receipts, err := s.applyCommitMetadata(ctx, commits)
	if err != nil {
		return nil, err
	}
	if err := s.writeCommitPayloads(ctx, commits); err != nil {
		return nil, err
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

// 已提交与新许可并不冲突：返回原回执是幂等重放，不是一次新的写入。
func committedRemoteReceipts(txID entity.RemoteTransactionID, digest []byte, doc mongoRemoteTransaction) ([]entity.RemoteCommitReceipt, error) {
	if !bytes.Equal(doc.Digest, digest) {
		return nil, fmt.Errorf("%w: transaction id reused with different commits", entity.ErrRemoteRejected)
	}
	status := remoteStatusFromMongoTransaction(txID, doc)
	switch status.State {
	case entity.RemoteCommitApplied, entity.RemoteCommitPublished, entity.RemoteCommitCommitted:
		return append([]entity.RemoteCommitReceipt(nil), status.Receipts...), nil
	default:
		return nil, fmt.Errorf("%w: %s", entity.ErrRemoteRejected, status.Cause)
	}
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
		// Encodability is part of validity here: a commit whose counters do
		// not fit a BSON number fails deterministically at projection time,
		// and by then the record is durable and startup recovery replays it
		// forever (RR-20260920-01).
		if err := validateRemoteCommitEncodable(commits[i]); err != nil {
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
	count, err := s.controlDB().Collection(remoteMetaCollection).CountDocuments(ctx, bson.M{"_authority": bson.M{"$ne": true}})
	if err != nil {
		return err
	}
	if count != 0 {
		return ErrRemoteAuthorityInvalid
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
		StateVersion uint64                `bson:"state_version"`
		BaseVersion  uint64                `bson:"base_version"`
		MarkerEpoch  uint64                `bson:"marker_epoch"`
		RouteEpoch   uint64                `bson:"route_epoch"`
		Schema       uint32                `bson:"schema"`
		Codec        uint16                `bson:"codec"`
		Checksum     entity.RemoteChecksum `bson:"checksum"`
		Full         bool                  `bson:"full"`
		Data         []byte                `bson:"data"`
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

// 准入必须先创建所有权并取得许可；在同一 Mongo 事务内一次校验多个实体。
// MatchedCount 必须等于实体数；任一许可/版本失效都会使整个事务回滚。
func (s *MongoCommitter) applyCommitMetadata(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	receipts := make([]entity.RemoteCommitReceipt, 0, len(commits))

	models := make([]fmongo.WriteModel, 0, len(commits))
	for _, commit := range commits {
		filter := authorityCommitFilter(commit)
		filter["_id"], filter["_ver"] = commit.EntityID, commit.BaseVersion
		models = append(models, fmongo.NewUpdateOneModel(filter, remoteMetadataUpdate(commit), false))
		receipts = append(receipts, remoteCommitReceipt(commit))
	}
	result, err := s.controlDB().Collection(remoteMetaCollection).BulkWrite(ctx, models)
	if err != nil {
		return nil, err
	}
	if result == nil || result.MatchedCount != int64(len(commits)) {
		return nil, fmongo.ErrVersionConflict
	}
	return receipts, nil
}
func authorityCommitFilter(commit entity.RemoteCommit) bson.M {
	return bson.M{"_authority": true, "_owner_epoch": commit.MarkerEpoch, "_owner_route": commit.RouteEpoch, "_grant_fence": commit.LockFence, "_grant_token": bson.M{"$ne": ""}}
}
func remoteMetadataUpdate(commit entity.RemoteCommit) bson.M {
	return bson.M{"$set": bson.M{"_ver": commit.NextVersion, "_marker_epoch": commit.MarkerEpoch, "_route_epoch": commit.RouteEpoch, "_lock_fence": commit.LockFence, "_deleted": commit.Delete}}
}
func remoteCommitReceipt(commit entity.RemoteCommit) entity.RemoteCommitReceipt {
	return entity.RemoteCommitReceipt{TransactionID: commit.TransactionID, EntityID: commit.EntityID, StateVersion: commit.NextVersion, MarkerEpoch: commit.MarkerEpoch, LockFence: commit.LockFence, RouteEpoch: commit.RouteEpoch, CommittedAt: time.Now().UnixNano()}
}

// SupportsConcurrentRemoteCommits 允许不同 Entity 的独立 Mongo 事务并行；
// 相同 Entity 仍必须按版本顺序提交，事务身份与最新许可校验保持不变。
func (*MongoCommitter) SupportsConcurrentRemoteCommits() bool { return true }

// RejectRemoteCommitsInTransaction 将租约失效的结论与 DataEngine skipped 标记原子持久化。
// 重放只能复用相同内容的拒绝，不能覆盖已经提交的事务。
func (s *MongoCommitter) RejectRemoteCommitsInTransaction(ctx context.Context, commits []entity.RemoteCommit, cause string) error {
	if s == nil || s.mongo == nil || s.database == "" {
		return entity.ErrRemoteWriteCapabilityDisabled
	}
	id, digest, err := validateRemoteCommitBatch(commits)
	if err != nil {
		return err
	}
	collection := s.controlDB().Collection(remoteTxCollection)
	var existing mongoRemoteTransaction
	err = collection.FindOne(ctx, bson.M{"_id": id.String()}, &existing)
	if err == nil {
		if bytes.Equal(existing.Digest, digest) && existing.State == uint8(entity.RemoteCommitRejected) {
			return nil
		}
		return fmt.Errorf("%w: rejection conflicts with existing remote transaction", entity.ErrRemoteRejected)
	}
	if !errors.Is(err, fmongo.ErrNotFound) {
		return err
	}
	_, err = collection.InsertOne(ctx, mongoRemoteTransaction{ID: id.String(), State: uint8(entity.RemoteCommitRejected), Digest: digest, Cause: cause, CreatedAt: time.Now().UTC()})
	return err
}
