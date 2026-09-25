package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Project 先检查幂等身份，再选择单文档 CAS 或 Mongo 原子事务。
// Remote、receipt 和 outbox 必须与普通 DAO 一起提交，不能拆成独立写入。
func (store *MongoStore) Project(ctx context.Context, record coredata.CommitRecord) error {
	if store == nil || store.client == nil {
		return errors.New("dataengine mongo: store is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := coredata.ValidateCommitRecord(record); err != nil {
		return err
	}
	if len(record.Mutations) == 1 && len(record.Effects) == 0 && len(record.Receipts) == 0 && record.Mutations[0].Remote == nil {
		err := store.applyMutation(ctx, record.ID.String(), record.Mutations[0], false)
		if record.Handler == MigrationHandler && errors.Is(err, ErrProjectionConflict) {
			// A concurrent writer made this migration record obsolete. It must
			// still advance the WAL checkpoint; the repository reloads and either
			// observes the migrated schema or submits one new CAS attempt.
			return nil
		}
		return err
	}
	digest, err := digestRecord(record)
	if err != nil {
		return err
	}
	session, err := store.client.StartSession(ctx)
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	mutations := append([]coredata.Mutation(nil), record.Mutations...)
	slices.SortFunc(mutations, func(left, right coredata.Mutation) int {
		if documentKeyLess(left.Key, right.Key) {
			return -1
		}
		if documentKeyLess(right.Key, left.Key) {
			return 1
		}
		return 0
	})
	remote := make([]entity.RemoteCommit, 0, len(mutations))
	ordinary := make([]coredata.Mutation, 0, len(mutations))
	for i := range mutations {
		if mutations[i].Remote == nil {
			ordinary = append(ordinary, mutations[i])
			continue
		}
		if mutations[i].Remote.TransactionID != entity.RemoteTransactionID(record.ID) {
			return fmt.Errorf("dataengine mongo: remote transaction identity mismatch at mutation %d", i)
		}
		remote = append(remote, mutations[i].Remote.Clone())
	}
	if len(remote) > 0 && (store.remoteStore == nil || store.remoteApplier == nil) {
		return ErrRemoteProjection
	}
	transactionSkipped := false
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		// The Mongo driver may retry this callback. Recompute the outcome from
		// the durable marker on every invocation rather than retaining a prior
		// callback's transient value.
		transactionSkipped = false
		alreadyApplied, skipped, err := store.checkTransaction(txCtx, record.ID.String(), digest)
		if err != nil || alreadyApplied {
			transactionSkipped = skipped
			return err
		}
		fencesMatch, err := store.leaseFencesMatch(txCtx, record.Receipts)
		if err != nil {
			return err
		}
		if !fencesMatch {
			transactionSkipped = true
			return store.insertTransactionMarker(txCtx, transactionDocument{
				ID: record.ID.String(), Digest: digest, CreatedAt: store.now().UTC(), Skipped: true,
			})
		}
		for i := range ordinary {
			if err := store.applyMutation(txCtx, record.ID.String(), ordinary[i], true); err != nil {
				return fmt.Errorf("dataengine mongo: mutation %d: %w", i, err)
			}
		}
		if len(remote) > 0 {
			if _, err := store.remoteStore.ApplyRemoteCommitsInTransaction(txCtx, remote); err != nil {
				if errors.Is(err, fmongo.ErrVersionConflict) || errors.Is(err, entity.ErrRemoteVersionConflict) {
					err = errors.Join(ErrProjectionConflict, err)
				}
				return fmt.Errorf("dataengine mongo: remote projection: %w", err)
			}
		}
		for i := range record.Receipts {
			if record.Receipts[i].Namespace == coredata.LeaseFenceReceiptNamespace {
				continue
			}
			if err := store.stageReceipt(txCtx, record.ID.String(), record.Receipts[i]); err != nil {
				return fmt.Errorf("dataengine mongo: receipt %d: %w", i, err)
			}
		}
		for i := range record.Effects {
			if err := store.stageEffect(txCtx, record.ID.String(), record.Effects[i]); err != nil {
				return fmt.Errorf("dataengine mongo: effect %d: %w", i, err)
			}
		}
		err = store.insertTransactionMarker(txCtx, transactionDocument{
			ID: record.ID.String(), Digest: digest, CreatedAt: store.now().UTC(),
		})
		return err
	})
	if err != nil {
		return err
	}
	if transactionSkipped {
		return nil
	}
	if len(remote) > 0 {
		if _, err := store.remoteApplier.ApplyRemoteCommits(ctx, entity.RemoteTransactionID(record.ID), remote); err != nil {
			return fmt.Errorf("dataengine mongo: remote publication: %w", err)
		}
	}
	return nil
}

// SupportsMultiMutationBatch 保留旧 BatchProjectionStore 实现的兼容边界。
func (*MongoStore) SupportsMultiMutationBatch() bool { return true }

// 只有两个 Remote 适配器都声明并发安全，正式 Store 才开放独立实体并行。
// 自定义适配器未声明时保留其原有串行调用契约。
func (store *MongoStore) SupportsRemoteParallelProjection() bool {
	type concurrentRemote interface{ SupportsConcurrentRemoteCommits() bool }
	persistence, ok := store.remoteStore.(concurrentRemote)
	if !ok || !persistence.SupportsConcurrentRemoteCommits() {
		return false
	}
	publisher, ok := store.remoteApplier.(concurrentRemote)
	return ok && publisher.SupportsConcurrentRemoteCommits()
}

// ProjectBatch atomically projects a bounded batch of local DAO records.
// Ordered bulk writes preserve each document's WAL version order and remove one round-trip per
// WAL record, while transaction markers make a committed-but-unacknowledged
// batch idempotent across restart. Records with effects, receipts, remote
// mutations, or migrations retain the per-record projection path.
func (store *MongoStore) ProjectBatch(ctx context.Context, records []coredata.CommitRecord) error {
	if store == nil || store.client == nil {
		return errors.New("dataengine mongo: store is not configured")
	}
	if len(records) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	digests := make(map[string][]byte, len(records))
	for i := range records {
		record := records[i]
		if err := coredata.ValidateCommitRecord(record); err != nil {
			return err
		}
		if !isLocalBatchRecord(record) {
			return errProjectionBatchUnsupported
		}
		digest, err := digestRecord(record)
		if err != nil {
			return err
		}
		id := record.ID.String()
		if previous, exists := digests[id]; exists && !bytes.Equal(previous, digest) {
			return ErrTransactionIdentity
		}
		digests[id] = digest
	}
	session, err := store.client.StartSession(ctx)
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	return session.WithTransaction(ctx, func(txCtx context.Context) error {
		ids := make([]string, 0, len(records))
		for i := range records {
			ids = append(ids, records[i].ID.String())
		}
		transactionColl := store.client.Database(store.cfg.DefaultDatabase).Collection(TransactionCollection)
		var existing []transactionDocument
		if err := transactionColl.Find(txCtx, bson.M{"_id": bson.M{"$in": ids}}, &existing, fmongo.FindOption{BatchSize: int32(min(len(ids), 4096))}); err != nil {
			return err
		}
		applied := make(map[string]struct{}, len(existing))
		for i := range existing {
			want, ok := digests[existing[i].ID]
			if !ok || !bytes.Equal(existing[i].Digest, want) {
				return ErrTransactionIdentity
			}
			applied[existing[i].ID] = struct{}{}
		}

		type mutationGroup struct {
			collection fmongo.ICollection
			models     []fmongo.WriteModel
		}
		groups := make(map[string]*mutationGroup)
		var groupKeys []string
		markers := make([]fmongo.WriteModel, 0, len(records)-len(applied))
		for i := range records {
			record := records[i]
			txID := record.ID.String()
			if _, ok := applied[txID]; ok {
				continue
			}
			// 不按文档重新排序跨业务的 mutation；同一文档的版本必须沿 WAL 前进。
			for _, mutation := range record.Mutations {
				model, err := store.batchMutationModel(txID, mutation)
				if err != nil {
					return err
				}
				groupKey := batchCollectionKey(store, mutation.Key)
				group := groups[groupKey]
				if group == nil {
					group = &mutationGroup{collection: store.collection(mutation.Key)}
					groups[groupKey] = group
					groupKeys = append(groupKeys, groupKey)
				}
				group.models = append(group.models, model)
			}
			markers = append(markers, fmongo.NewInsertOneModel(transactionDocument{
				ID: txID, Digest: digests[txID], CreatedAt: store.now().UTC(),
			}))
		}
		if len(markers) == 0 {
			return nil
		}
		slices.Sort(groupKeys)
		for _, key := range groupKeys {
			group := groups[key]
			result, err := group.collection.BulkWrite(txCtx, group.models)
			if err != nil {
				// An upsert that collides on _id is what an already-applied
				// Put looks like: its exact-version filter matches nothing, so
				// the upsert tries to insert a document that already exists.
				if errors.Is(err, fmongo.ErrDuplicateKey) {
					return fmt.Errorf("%w: batch duplicate key", ErrProjectionBatchNeedsPerRecord)
				}
				return err
			}
			if result == nil || result.MatchedCount+result.UpsertedCount != int64(len(group.models)) {
				return fmt.Errorf("%w: batch matched=%d upserted=%d expected=%d", ErrProjectionBatchNeedsPerRecord,
					bulkMatched(result), bulkUpserted(result), len(group.models))
			}
		}
		_, err := transactionColl.BulkWrite(txCtx, markers)
		if errors.Is(err, fmongo.ErrDuplicateKey) {
			return ErrTransactionIdentity
		}
		return err
	})
}

func (store *MongoStore) batchMutationModel(txID string, mutation coredata.Mutation) (fmongo.WriteModel, error) {
	switch mutation.Kind {
	case coredata.MutationPut:
		var doc bson.M
		if err := bson.Unmarshal(mutation.Data, &doc); err != nil {
			return fmongo.WriteModel{}, err
		}
		if id, ok := documentInt64(doc["_id"]); !ok || id != mutation.Key.ID {
			return fmongo.WriteModel{}, coredata.ErrInvalidDocumentKey
		}
		doc["_id"] = mutation.Key.ID
		doc["_version"] = mutation.NextVersion
		doc["_schema"] = mutation.Schema
		doc["_last_tx"] = txID
		delete(doc, "_deleted")
		delete(doc, "_deleted_at")
		return fmongo.NewReplaceOneModel(mutationFilter(mutation), doc, true), nil
	case coredata.MutationPatch:
		update, err := patchUpdate(txID, mutation)
		if err != nil {
			return fmongo.WriteModel{}, err
		}
		return fmongo.NewUpdateOneModel(mutationFilter(mutation), update, false), nil
	case coredata.MutationDelete:
		update := bson.M{"$set": bson.M{
			"_version": mutation.NextVersion, "_schema": mutation.Schema, "_last_tx": txID,
			"_deleted": true, "_deleted_at": store.now().UTC(),
		}}
		return fmongo.NewUpdateOneModel(mutationFilter(mutation), update, false), nil
	default:
		return fmongo.WriteModel{}, coredata.ErrInvalidMutationKind
	}
}

func batchCollectionKey(store *MongoStore, key coredata.DocumentKey) string {
	database := key.Database
	if database == "" {
		database = store.cfg.DefaultDatabase
	}
	return fmt.Sprintf("%d/%s/%s", key.Scope, database, key.Resource)
}

func bulkMatched(result *fmongo.BulkWriteResult) int64 {
	if result == nil {
		return 0
	}
	return result.MatchedCount
}

func bulkUpserted(result *fmongo.BulkWriteResult) int64 {
	if result == nil {
		return 0
	}
	return result.UpsertedCount
}
