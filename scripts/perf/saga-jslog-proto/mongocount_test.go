package jslogproto

// 计数包装：数参与方投影实际发出的 Mongo 命令（每次集合方法调用、会话开启各计一次往返），用于“投影器每条记录几次往返”。
// 只包了集合与会话入口；StreamFind 不暴露（加载退回 Find，各计一次）。

import (
	"context"
	"sync/atomic"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
)

type countingMongo struct {
	fmongo.IMongo
	calls *atomic.Uint64
}

func (m countingMongo) Database(name string) fmongo.IDatabase {
	return countingDB{IDatabase: m.IMongo.Database(name), calls: m.calls}
}

func (m countingMongo) StartSession(ctx context.Context) (fmongo.ISession, error) {
	m.calls.Add(1)
	return m.IMongo.StartSession(ctx)
}

type countingDB struct {
	fmongo.IDatabase
	calls *atomic.Uint64
}

func (d countingDB) Collection(name string) fmongo.ICollection {
	return countingColl{ICollection: d.IDatabase.Collection(name), calls: d.calls}
}

type countingColl struct {
	fmongo.ICollection
	calls *atomic.Uint64
}

func (c countingColl) InsertOne(ctx context.Context, doc any) (string, error) {
	c.calls.Add(1)
	return c.ICollection.InsertOne(ctx, doc)
}
func (c countingColl) InsertMany(ctx context.Context, docs []any) ([]string, error) {
	c.calls.Add(1)
	return c.ICollection.InsertMany(ctx, docs)
}
func (c countingColl) FindOne(ctx context.Context, filter any, result any) error {
	c.calls.Add(1)
	return c.ICollection.FindOne(ctx, filter, result)
}
func (c countingColl) Find(ctx context.Context, filter any, results any, opts ...fmongo.FindOption) error {
	c.calls.Add(1)
	return c.ICollection.Find(ctx, filter, results, opts...)
}
func (c countingColl) UpdateOne(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	c.calls.Add(1)
	return c.ICollection.UpdateOne(ctx, filter, update)
}
func (c countingColl) UpdateMany(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	c.calls.Add(1)
	return c.ICollection.UpdateMany(ctx, filter, update)
}
func (c countingColl) ReplaceOne(ctx context.Context, filter any, replacement any) (*fmongo.UpdateResult, error) {
	c.calls.Add(1)
	return c.ICollection.ReplaceOne(ctx, filter, replacement)
}
func (c countingColl) DeleteOne(ctx context.Context, filter any) (int64, error) {
	c.calls.Add(1)
	return c.ICollection.DeleteOne(ctx, filter)
}
func (c countingColl) DeleteMany(ctx context.Context, filter any) (int64, error) {
	c.calls.Add(1)
	return c.ICollection.DeleteMany(ctx, filter)
}
func (c countingColl) FindOneAndUpdate(ctx context.Context, filter any, update any, result any, opts ...fmongo.FindOneAndUpdateOption) error {
	c.calls.Add(1)
	return c.ICollection.FindOneAndUpdate(ctx, filter, update, result, opts...)
}
func (c countingColl) FindOneAndDelete(ctx context.Context, filter any, result any) error {
	c.calls.Add(1)
	return c.ICollection.FindOneAndDelete(ctx, filter, result)
}
func (c countingColl) FindOneAndReplace(ctx context.Context, filter any, replacement any, result any) error {
	c.calls.Add(1)
	return c.ICollection.FindOneAndReplace(ctx, filter, replacement, result)
}
func (c countingColl) CountDocuments(ctx context.Context, filter any) (int64, error) {
	c.calls.Add(1)
	return c.ICollection.CountDocuments(ctx, filter)
}
func (c countingColl) Aggregate(ctx context.Context, pipeline any, results any) error {
	c.calls.Add(1)
	return c.ICollection.Aggregate(ctx, pipeline, results)
}
func (c countingColl) BulkWrite(ctx context.Context, models []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	c.calls.Add(1)
	return c.ICollection.BulkWrite(ctx, models)
}
