package dataengine

import (
	"context"
	"errors"
	"fmt"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

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

var _ coredata.Store = (*MongoStore)(nil)
