package remoteentity

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// writeCommitPayloads 在全部实体 CAS 成功后按实际集合合并写入。
// 同一集合保留原提交顺序；调用方的 Mongo 事务保证跨集合失败整体回滚。
func (s *MongoCommitter) writeCommitPayloads(ctx context.Context, commits []entity.RemoteCommit) error {
	type collectionKey struct{ database, collection string }
	type collectionWrites struct {
		key        collectionKey
		collection fmongo.ICollection
		models     []fmongo.WriteModel
	}
	positions := make(map[collectionKey]int)
	var groups []collectionWrites
	add := func(db fmongo.IDatabase, name string, model fmongo.WriteModel) {
		key := collectionKey{db.Name(), name}
		pos, ok := positions[key]
		if !ok {
			pos = len(groups)
			positions[key] = pos
			groups = append(groups, collectionWrites{key: key, collection: db.Collection(name)})
		}
		groups[pos].models = append(groups[pos].models, model)
	}
	for _, commit := range commits {
		for _, mutation := range commit.Mutations {
			doc := bson.M{"_id": mutation.ID, "_ver": commit.NextVersion, "_marker_epoch": commit.MarkerEpoch, "_route_epoch": commit.RouteEpoch, "_lock_fence": commit.LockFence, "data": append([]byte(nil), mutation.Data...)}
			add(s.dataDB(mutation.Database, mutation.DatabaseScope), mutation.Collection, fmongo.NewReplaceOneModel(bson.M{"_id": mutation.ID}, doc, true))
		}
		for _, item := range commit.Deletes {
			add(s.dataDB(item.Database, item.DatabaseScope), item.Collection, fmongo.NewDeleteOneModel(bson.M{"_id": item.ID}))
		}
		for _, snapshot := range commit.Snapshots {
			key := remoteSnapshotStorageKey(snapshot.Key)
			doc := bson.M{"_id": key, "key": snapshot.Key, "state_version": snapshot.StateVersion, "base_version": snapshot.BaseVersion, "marker_epoch": snapshot.MarkerEpoch, "route_epoch": snapshot.RouteEpoch, "schema": snapshot.Schema, "codec": snapshot.Codec, "checksum": snapshot.Checksum, "full": snapshot.Full, "data": append([]byte(nil), snapshot.Data...)}
			add(s.controlDB(), remoteSnapshotCollection, fmongo.NewReplaceOneModel(bson.M{"_id": key}, doc, true))
		}
		for _, key := range commit.Invalidations {
			add(s.controlDB(), remoteSnapshotCollection, fmongo.NewDeleteOneModel(bson.M{"_id": remoteSnapshotStorageKey(key)}))
		}
	}
	for _, group := range groups {
		if _, err := group.collection.BulkWrite(ctx, group.models); err != nil {
			return fmt.Errorf("remote payload %s.%s: %w", group.key.database, group.key.collection, err)
		}
	}
	return nil
}
