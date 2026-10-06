package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"

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
		if err := retryDuringElection(ctx, startupElectionRetry, func() error { return c.ensureIndex(ctx, idx) }); err != nil {
			return err
		}
	}
	return nil
}

// electionRetry 是启动期索引创建遇到副本集选举时的有界重试（O-M6-5，维护者第十二轮决定）。
//
// EnsureIndexes 只在启动时调用（DataEngine、Remote owner、saga、效果收件箱建索引），createIndexes /
// dropIndexes 不在驱动的可重试读写之列：主节点让位时在途的命令得到 InterruptedDueToReplStateChange，
// 新主选出之前打到旧主的得到 NotWritablePrimary，进程直接启动失败（Mirror 第 6 步本机替代里 owner
// 在 S5 之后撞上过一次）。普通读写由驱动的可重试读写吸收，这里只补 DDL。索引创建是幂等的，重做安全。
//
// 次数与间隔都有上限：attempts 次、每次之间 interval，用完返回最后一次的错误并写明是选举没有结束；
// ctx 先到期则以 ctx 为准，同样点名。只认选举相关的服务端错误码，其他错误原样立即返回。
type electionRetry struct {
	attempts int
	interval time.Duration
}

// startupElectionRetry：10 次、间隔 1s，覆盖一次正常选举（让位到新主通常几秒）；
// 调用方的启动期限（DataEngine startup_timeout、remote_entity.op_timeout 缺省都是 30s）在它之上。
var startupElectionRetry = electionRetry{attempts: 10, interval: time.Second}

// electionErrorCodes 是副本集换主期间服务端返回的错误码：InterruptedDueToReplStateChange (11602)、
// NotWritablePrimary (10107)、NotPrimaryNoSecondaryOk (13435)、NotPrimaryOrSecondary (13436)、
// PrimarySteppedDown (189)、ShutdownInProgress (91)、InterruptedAtShutdown (11600)。
// electionRetryMetric 计每一次因换主错误而失败的索引创建尝试（含最后一次放弃的）。启动时非零说明
// 撞上了选举；放弃时启动失败，错误点名选举。导出名 mongo_ensure_index_election_retries_total。
const electionRetryMetric = "mongo.ensure_index.election_retries.total"

var electionErrorCodes = []int{11602, 10107, 13435, 13436, 189, 91, 11600}

func isElectionError(err error) bool {
	var server mongo.ServerError
	if !errors.As(err, &server) {
		return false
	}
	for _, code := range electionErrorCodes {
		if server.HasErrorCode(code) {
			return true
		}
	}
	return false
}

func retryDuringElection(ctx context.Context, policy electionRetry, op func() error) error {
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || !isElectionError(err) {
			return err
		}
		metrics.IncCounter(electionRetryMetric, nil, 1)
		if attempt >= policy.attempts {
			return fmt.Errorf("mongo: replica set primary election did not settle after %d attempts %s apart: %w",
				policy.attempts, policy.interval, err)
		}
		timer := time.NewTimer(policy.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("mongo: replica set primary election did not settle before the deadline (attempt %d of %d): %w; last error: %w",
				attempt, policy.attempts, ctx.Err(), err)
		case <-timer.C:
		}
	}
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
	if idx.ExpireAt {
		indexOpts.SetExpireAfterSeconds(0)
	} else if idx.TTL > 0 {
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

// wrapError maps driver errors onto the fmongo sentinels. A duplicate key keeps
// the driver error in its chain: errors.Is(err, fmongo.ErrDuplicateKey) still
// holds, and the WriteException / server labels stay reachable through
// errors.As (mongo.LabeledError, mongo.ServerError) — including the
// TransientTransactionError label that mongo.Session.WithTransaction retries
// on. Replacing it with the bare sentinel used to discard them
// (RR-20260926-34). ErrNoDocuments carries no server state and maps to the bare
// fmongo.ErrNotFound as before.
func wrapError(err error) error {
	if err == nil {
		return nil
	}
	if err == mongo.ErrNoDocuments {
		return fmongo.ErrNotFound
	}
	if mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("%w: %w", fmongo.ErrDuplicateKey, err)
	}
	return err
}

// bson is imported for potential use by index keys
var _ = bson.D{}

var _ fmongo.ICollection = (*collection)(nil)
var _ fmongo.IStreamingCollection = (*collection)(nil)
