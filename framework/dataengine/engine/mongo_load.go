package engine

import (
	"context"
	"errors"
	"fmt"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// ReadConsistent 在一个 Mongo 会话事务内执行 read，得到一致快照；驱动可能重调 read。
func (store *MongoStore) ReadConsistent(ctx context.Context, read func(context.Context) error) error {
	if store == nil || store.client == nil || read == nil {
		return coredata.ErrStoreRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session, err := store.client.StartSession(ctx)
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	return session.WithTransaction(ctx, read)
}

func (store *MongoStore) Load(ctx context.Context, spec coredata.LoadSpec) ([]coredata.RawDocument, error) {
	docs := make([]coredata.RawDocument, 0)
	err := store.StreamLoad(ctx, spec, func(doc coredata.RawDocument) error {
		docs = append(docs, doc)
		return nil
	})
	return docs, err
}

// StreamLoad 流式读取未删除的文档（spec.IncludeDeleted 时连墓碑一起）；集合支持流式游标时逐条交给 consume，
// 否则整批读出后逐条交付。
func (store *MongoStore) StreamLoad(ctx context.Context, spec coredata.LoadSpec, consume func(coredata.RawDocument) error) error {
	if store == nil || store.client == nil {
		return coredata.ErrStoreRequired
	}
	if spec.Resource == "" || consume == nil {
		return errors.New("dataengine mongo: invalid load request")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := coredata.DocumentKey{Database: spec.Database, Scope: spec.Scope, Resource: spec.Resource}
	collection := store.collection(key)
	filter := activeLoadFilter(spec.Filter)
	if spec.IncludeDeleted {
		filter = bson.M(spec.Filter)
		if filter == nil {
			filter = bson.M{}
		}
	}
	consumeRaw := func(raw []byte) error {
		doc, err := decodeRawDocument(key, bson.Raw(raw))
		if err != nil {
			return err
		}
		return consume(doc)
	}
	options := fmongo.FindOption{BatchSize: int32(spec.BatchSize)}
	if streaming, ok := collection.(fmongo.IStreamingCollection); ok {
		return streaming.StreamFind(ctx, filter, consumeRaw, options)
	}
	var rawDocs []bson.Raw
	if err := collection.Find(ctx, filter, &rawDocs, options); err != nil {
		return err
	}
	for _, raw := range rawDocs {
		if err := consumeRaw(raw); err != nil {
			return err
		}
	}
	return nil
}

// decodeRawDocument 解出文档元数据；带 _ver 的 Remote 版本信封以 _ver 作为版本。
func decodeRawDocument(key coredata.DocumentKey, raw bson.Raw) (coredata.RawDocument, error) {
	var metadata struct {
		ID            int64   `bson:"_id"`
		Version       uint64  `bson:"_version"`
		RemoteVersion *uint64 `bson:"_ver"`
		Schema        uint32  `bson:"_schema"`
		MarkerEpoch   uint64  `bson:"_marker_epoch"`
		LockFence     uint64  `bson:"_lock_fence"`
		RouteEpoch    uint64  `bson:"_route_epoch"`
		Deleted       bool    `bson:"_deleted"`
	}
	if err := bson.Unmarshal(raw, &metadata); err != nil {
		return coredata.RawDocument{}, err
	}
	if metadata.ID == 0 {
		return coredata.RawDocument{}, errors.New("dataengine mongo: loaded document has zero _id")
	}
	key.ID = metadata.ID
	enveloped := metadata.RemoteVersion != nil
	if enveloped {
		metadata.Version = *metadata.RemoteVersion
	}
	return coredata.RawDocument{
		Key: key, Version: metadata.Version, Schema: metadata.Schema, Deleted: metadata.Deleted,
		Data: append([]byte(nil), raw...), MarkerEpoch: metadata.MarkerEpoch,
		LockFence: metadata.LockFence, RouteEpoch: metadata.RouteEpoch, Enveloped: enveloped,
	}, nil
}

func activeLoadFilter(filter map[string]any) bson.M {
	active := bson.M{"_deleted": bson.M{"$ne": true}}
	if len(filter) == 0 {
		return active
	}
	return bson.M{"$and": bson.A{bson.M(filter), active}}
}

// persistedPayload 返回 DAO 的持久化 payload 与 schema：普通文档就是整文档，
// Remote 信封取其 data 字段。
func persistedPayload(doc coredata.RawDocument) ([]byte, uint32, error) {
	if !doc.Enveloped {
		return append([]byte(nil), doc.Data...), doc.Schema, nil
	}
	var envelope struct {
		Data []byte `bson:"data"`
	}
	if err := bson.Unmarshal(doc.Data, &envelope); err != nil {
		return nil, 0, err
	}
	if len(envelope.Data) == 0 {
		return nil, 0, fmt.Errorf("empty data envelope")
	}
	var metadata struct {
		Schema uint32 `bson:"_schema"`
	}
	if err := bson.Unmarshal(envelope.Data, &metadata); err != nil {
		return nil, 0, err
	}
	return append([]byte(nil), envelope.Data...), metadata.Schema, nil
}

// RemoteEntityDeleted 报告 Remote 托管实体是否删过档：Remote 删档物理删除数据文档，墓碑只在 Remote 元数据里，
// 由绑定的 Remote 原子存储回答（D10）。没有绑定 Remote 投影、或存储不提供该查询时返回 false。
// 可在 ReadConsistent 的回调里用同一个会话 ctx 调用（Remote 投影同样在本 Store 的事务里写元数据）。
func (store *MongoStore) RemoteEntityDeleted(ctx context.Context, id int64) (bool, error) {
	if store == nil {
		return false, nil
	}
	reader, ok := store.remoteStore.(interface {
		RemoteEntityDeleted(context.Context, int64) (bool, error)
	})
	if !ok {
		return false, nil
	}
	return reader.RemoteEntityDeleted(ctx, id)
}

var _ coredata.Store = (*MongoStore)(nil)
