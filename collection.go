package mongo

import (
	"context"
	"errors"
	"fmt"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// collection implements fmongo.ICollection.
type collection struct {
	coll   *mongo.Collection
	policy IndexMigrationPolicy
}

type IndexMigrationPolicy struct {
	AllowRecreate bool
}

func newCollection(coll *mongo.Collection, policy IndexMigrationPolicy) *collection {
	return &collection{coll: coll, policy: policy}
}

// --- CRUD ---

func (c *collection) InsertOne(ctx context.Context, doc any) (string, error) {
	result, err := c.coll.InsertOne(ctx, doc)
	if err != nil {
		return "", wrapError(err)
	}
	return stringifyID(result.InsertedID), nil
}

func (c *collection) InsertMany(ctx context.Context, docs []any) ([]string, error) {
	result, err := c.coll.InsertMany(ctx, docs)
	if err != nil {
		return nil, wrapError(err)
	}
	ids := make([]string, len(result.InsertedIDs))
	for i, id := range result.InsertedIDs {
		ids[i] = stringifyID(id)
	}
	return ids, nil
}

func (c *collection) FindOne(ctx context.Context, filter any, result any) error {
	err := c.coll.FindOne(ctx, filter).Decode(result)
	if err != nil {
		return wrapError(err)
	}
	return nil
}

func (c *collection) Find(ctx context.Context, filter any, results any, opts ...fmongo.FindOption) error {
	findOpts := options.Find()
	for _, opt := range opts {
		if opt.Sort != nil {
			findOpts.SetSort(opt.Sort)
		}
		if opt.Limit > 0 {
			findOpts.SetLimit(opt.Limit)
		}
		if opt.Skip > 0 {
			findOpts.SetSkip(opt.Skip)
		}
		if opt.BatchSize > 0 {
			findOpts.SetBatchSize(opt.BatchSize)
		}
	}
	cursor, err := c.coll.Find(ctx, filter, findOpts)
	if err != nil {
		return wrapError(err)
	}
	return wrapError(cursor.All(ctx, results))
}

func (c *collection) StreamFind(ctx context.Context, filter any, consume func([]byte) error, opts ...fmongo.FindOption) error {
	if consume == nil {
		return fmt.Errorf("mongo: nil stream consumer")
	}
	findOpts := options.Find()
	for _, opt := range opts {
		if opt.Sort != nil {
			findOpts.SetSort(opt.Sort)
		}
		if opt.Limit > 0 {
			findOpts.SetLimit(opt.Limit)
		}
		if opt.Skip > 0 {
			findOpts.SetSkip(opt.Skip)
		}
		if opt.BatchSize > 0 {
			findOpts.SetBatchSize(opt.BatchSize)
		}
	}
	cursor, err := c.coll.Find(ctx, filter, findOpts)
	if err != nil {
		return wrapError(err)
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		raw := append([]byte(nil), cursor.Current...)
		if err := consume(raw); err != nil {
			return err
		}
	}
	return wrapError(cursor.Err())
}

func (c *collection) UpdateOne(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	result, err := c.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return nil, wrapError(err)
	}
	return convertUpdateResult(result), nil
}

func (c *collection) UpdateMany(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	result, err := c.coll.UpdateMany(ctx, filter, update)
	if err != nil {
		return nil, wrapError(err)
	}
	return convertUpdateResult(result), nil
}

func (c *collection) ReplaceOne(ctx context.Context, filter any, replacement any) (*fmongo.UpdateResult, error) {
	result, err := c.coll.ReplaceOne(ctx, filter, replacement)
	if err != nil {
		return nil, wrapError(err)
	}
	return &fmongo.UpdateResult{
		MatchedCount:  result.MatchedCount,
		ModifiedCount: result.ModifiedCount,
		UpsertedCount: result.UpsertedCount,
		UpsertedID:    stringifyID(result.UpsertedID),
	}, nil
}

func (c *collection) DeleteOne(ctx context.Context, filter any) (int64, error) {
	result, err := c.coll.DeleteOne(ctx, filter)
	if err != nil {
		return 0, wrapError(err)
	}
	return result.DeletedCount, nil
}

func (c *collection) DeleteMany(ctx context.Context, filter any) (int64, error) {
	result, err := c.coll.DeleteMany(ctx, filter)
	if err != nil {
		return 0, wrapError(err)
	}
	return result.DeletedCount, nil
}

// --- FindAndModify ---

func (c *collection) FindOneAndUpdate(ctx context.Context, filter any, update any, result any, opts ...fmongo.FindOneAndUpdateOption) error {
	foOpts := options.FindOneAndUpdate()
	for _, opt := range opts {
		if opt.Upsert {
			foOpts.SetUpsert(true)
		}
		if opt.ReturnAfter {
			foOpts.SetReturnDocument(options.After)
		}
	}
	err := c.coll.FindOneAndUpdate(ctx, filter, update, foOpts).Decode(result)
	return wrapError(err)
}

func (c *collection) FindOneAndDelete(ctx context.Context, filter any, result any) error {
	err := c.coll.FindOneAndDelete(ctx, filter).Decode(result)
	return wrapError(err)
}

func (c *collection) FindOneAndReplace(ctx context.Context, filter any, replacement any, result any) error {
	err := c.coll.FindOneAndReplace(ctx, filter, replacement).Decode(result)
	return wrapError(err)
}

// --- Count ---

func (c *collection) CountDocuments(ctx context.Context, filter any) (int64, error) {
	count, err := c.coll.CountDocuments(ctx, filter)
	return count, wrapError(err)
}

// --- Aggregate ---

func (c *collection) Aggregate(ctx context.Context, pipeline any, results any) error {
	cursor, err := c.coll.Aggregate(ctx, pipeline)
	if err != nil {
		return wrapError(err)
	}
	return wrapError(cursor.All(ctx, results))
}

// --- Bulk ---

func (c *collection) BulkWrite(ctx context.Context, models []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	writeModels := make([]mongo.WriteModel, len(models))
	for i, m := range models {
		switch m.Type {
		case fmongo.WriteModelInsertOne:
			writeModels[i] = mongo.NewInsertOneModel().SetDocument(m.Document)
		case fmongo.WriteModelUpdateOne:
			wm := mongo.NewUpdateOneModel().SetFilter(m.Filter).SetUpdate(m.Update)
			if m.Upsert {
				wm.SetUpsert(true)
			}
			writeModels[i] = wm
		case fmongo.WriteModelReplaceOne:
			wm := mongo.NewReplaceOneModel().SetFilter(m.Filter).SetReplacement(m.Document)
			if m.Upsert {
				wm.SetUpsert(true)
			}
			writeModels[i] = wm
		case fmongo.WriteModelDeleteOne:
			writeModels[i] = mongo.NewDeleteOneModel().SetFilter(m.Filter)
		default:
			return nil, fmt.Errorf("mongo: unsupported bulk write model type %d at index %d", m.Type, i)
		}
	}

	result, err := c.coll.BulkWrite(ctx, writeModels)
	if err != nil {
		return nil, wrapError(err)
	}
	return &fmongo.BulkWriteResult{
		InsertedCount: result.InsertedCount,
		MatchedCount:  result.MatchedCount,
		ModifiedCount: result.ModifiedCount,
		DeletedCount:  result.DeletedCount,
		UpsertedCount: result.UpsertedCount,
	}, nil
}

// --- Index ---

func (c *collection) EnsureIndexes(ctx context.Context, indexes []fmongo.IndexModel) error {
	if len(indexes) == 0 {
		return nil
	}
	for _, idx := range indexes {
		if err := c.ensureIndex(ctx, idx); err != nil {
			return err
		}
	}
	return nil
}

func (c *collection) ensureIndex(ctx context.Context, idx fmongo.IndexModel) error {
	model := mongoIndexModel(idx)
	_, err := c.coll.Indexes().CreateOne(ctx, model)
	if err == nil {
		return nil
	}
	if !shouldRecreateIndexOnConflict(idx, c.policy) || idx.Name == "" || !isIndexDefinitionConflict(err) {
		return err
	}
	if dropErr := c.coll.Indexes().DropOne(ctx, idx.Name); dropErr != nil && !isIndexNotFound(dropErr) {
		return dropErr
	}
	_, err = c.coll.Indexes().CreateOne(ctx, model)
	return err
}

func shouldRecreateIndexOnConflict(idx fmongo.IndexModel, policy IndexMigrationPolicy) bool {
	if !policy.AllowRecreate {
		return false
	}
	switch idx.ConflictPolicy {
	case fmongo.IndexConflictRecreate:
		return true
	case fmongo.IndexConflictFail:
		return false
	default:
		return idx.RecreateOnConflict
	}
}

func mongoIndexModel(idx fmongo.IndexModel) mongo.IndexModel {
	model := mongo.IndexModel{
		Keys: idx.Keys,
	}
	indexOpts := options.Index()
	if idx.Name != "" {
		indexOpts.SetName(idx.Name)
	}
	if idx.Unique {
		indexOpts.SetUnique(true)
	}
	if idx.Sparse {
		indexOpts.SetSparse(true)
	}
	if idx.TTL > 0 {
		indexOpts.SetExpireAfterSeconds(int32(idx.TTL))
	}
	model.Options = indexOpts
	return model
}

func isIndexDefinitionConflict(err error) bool {
	var cmdErr mongo.CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	switch cmdErr.Code {
	case 85, 86:
		return true
	}
	switch cmdErr.Name {
	case "IndexOptionsConflict", "IndexKeySpecsConflict":
		return true
	default:
		return false
	}
}

func isIndexNotFound(err error) bool {
	var cmdErr mongo.CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	return cmdErr.Code == 27 || cmdErr.Name == "IndexNotFound"
}

// --- helpers ---

func convertUpdateResult(r *mongo.UpdateResult) *fmongo.UpdateResult {
	return &fmongo.UpdateResult{
		MatchedCount:  r.MatchedCount,
		ModifiedCount: r.ModifiedCount,
		UpsertedCount: r.UpsertedCount,
		UpsertedID:    stringifyID(r.UpsertedID),
	}
}

func stringifyID(id any) string {
	if id == nil {
		return ""
	}
	return fmt.Sprintf("%v", id)
}

func wrapError(err error) error {
	if err == nil {
		return nil
	}
	if err == mongo.ErrNoDocuments {
		return fmongo.ErrNotFound
	}
	if mongo.IsDuplicateKeyError(err) {
		return fmongo.ErrDuplicateKey
	}
	return err
}

// bson is imported for potential use by index keys
var _ = bson.D{}

var _ fmongo.ICollection = (*collection)(nil)
var _ fmongo.IStreamingCollection = (*collection)(nil)
