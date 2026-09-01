package dataengine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	"github.com/tjbdwanghaibo/cube-core/entity"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type mongoRemoteProjectionFake struct {
	stored  int
	applied int
}

func (fake *mongoRemoteProjectionFake) ApplyRemoteCommitsInTransaction(_ context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	fake.stored += len(commits)
	return nil, nil
}

func (fake *mongoRemoteProjectionFake) ApplyRemoteCommits(_ context.Context, _ entity.RemoteTransactionID, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	fake.applied += len(commits)
	return nil, nil
}

type mongoStoreFakeClient struct {
	db            *mongoStoreFakeDatabase
	startSessions int
}

func (c *mongoStoreFakeClient) Database(string) fmongo.IDatabase              { return c.db }
func (c *mongoStoreFakeClient) DatabaseForSid(string, int32) fmongo.IDatabase { return c.db }
func (c *mongoStoreFakeClient) StartSession(context.Context) (fmongo.ISession, error) {
	c.startSessions++
	return &mongoStoreFakeSession{}, nil
}
func (c *mongoStoreFakeClient) Ping(context.Context) error  { return nil }
func (c *mongoStoreFakeClient) Close(context.Context) error { return nil }

type mongoStoreFakeSession struct{}

func (*mongoStoreFakeSession) WithTransaction(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
func (*mongoStoreFakeSession) EndSession(context.Context) {}

type mongoStoreFakeDatabase struct {
	collections map[string]*mongoStoreFakeCollection
}

func (d *mongoStoreFakeDatabase) Name() string { return "game" }
func (d *mongoStoreFakeDatabase) Collection(name string) fmongo.ICollection {
	if d.collections == nil {
		d.collections = make(map[string]*mongoStoreFakeCollection)
	}
	if d.collections[name] == nil {
		d.collections[name] = &mongoStoreFakeCollection{}
	}
	return d.collections[name]
}
func (*mongoStoreFakeDatabase) Drop(context.Context) error { return nil }

type mongoStoreFakeCollection struct {
	mu                  sync.Mutex
	updateResult        *fmongo.UpdateResult
	updateErr           error
	findOneAndUpdateErr error
	findOneAndUpdateDoc *outboxDocument
	findDoc             any
	findDocs            []outboxDocument
	findRaw             []bson.Raw
	findTransactions    []transactionDocument
	findErr             error
	deleteCount         int64
	count               int64
	lastFilter          any
	lastUpdate          any
	inserted            []any
	insertErr           error
	bulkModels          []fmongo.WriteModel
	bulkResult          *fmongo.BulkWriteResult
	bulkErr             error
	ensureIndexes       []fmongo.IndexModel
}

func (c *mongoStoreFakeCollection) InsertOne(_ context.Context, doc any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inserted = append(c.inserted, doc)
	return "id", c.insertErr
}
func (*mongoStoreFakeCollection) InsertMany(context.Context, []any) ([]string, error) {
	return nil, nil
}
func (c *mongoStoreFakeCollection) FindOne(_ context.Context, filter any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastFilter = filter
	if c.findErr != nil {
		return c.findErr
	}
	if c.findDoc == nil {
		return fmongo.ErrNotFound
	}
	raw, err := bson.Marshal(c.findDoc)
	if err != nil {
		return err
	}
	return bson.Unmarshal(raw, result)
}
func (c *mongoStoreFakeCollection) Find(_ context.Context, filter any, results any, _ ...fmongo.FindOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastFilter = filter
	if c.findErr != nil {
		return c.findErr
	}
	if out, ok := results.(*[]outboxDocument); ok {
		*out = append([]outboxDocument(nil), c.findDocs...)
	}
	if out, ok := results.(*[]bson.Raw); ok {
		*out = append([]bson.Raw(nil), c.findRaw...)
	}
	if out, ok := results.(*[]transactionDocument); ok {
		*out = append([]transactionDocument(nil), c.findTransactions...)
	}
	return nil
}
func (c *mongoStoreFakeCollection) UpdateOne(_ context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastFilter, c.lastUpdate = filter, update
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	if c.updateResult == nil {
		return &fmongo.UpdateResult{}, nil
	}
	return c.updateResult, nil
}
func (*mongoStoreFakeCollection) UpdateMany(context.Context, any, any) (*fmongo.UpdateResult, error) {
	return nil, nil
}
func (*mongoStoreFakeCollection) ReplaceOne(context.Context, any, any) (*fmongo.UpdateResult, error) {
	return nil, nil
}
func (c *mongoStoreFakeCollection) DeleteOne(context.Context, any) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deleteCount, nil
}
func (*mongoStoreFakeCollection) DeleteMany(context.Context, any) (int64, error) { return 0, nil }
func (c *mongoStoreFakeCollection) FindOneAndUpdate(_ context.Context, filter any, update any, result any, _ ...fmongo.FindOneAndUpdateOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastFilter, c.lastUpdate = filter, update
	if c.findOneAndUpdateErr == nil && c.findOneAndUpdateDoc != nil {
		if out, ok := result.(*outboxDocument); ok {
			*out = *c.findOneAndUpdateDoc
		}
	}
	return c.findOneAndUpdateErr
}
func (*mongoStoreFakeCollection) FindOneAndDelete(context.Context, any, any) error { return nil }
func (*mongoStoreFakeCollection) FindOneAndReplace(context.Context, any, any, any) error {
	return nil
}
func (c *mongoStoreFakeCollection) CountDocuments(context.Context, any) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count, nil
}
func (*mongoStoreFakeCollection) Aggregate(context.Context, any, any) error { return nil }
func (c *mongoStoreFakeCollection) BulkWrite(_ context.Context, models []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bulkModels = append(c.bulkModels, models...)
	if c.bulkErr != nil {
		return nil, c.bulkErr
	}
	if c.bulkResult != nil {
		result := *c.bulkResult
		return &result, nil
	}
	return &fmongo.BulkWriteResult{MatchedCount: int64(len(models))}, nil
}
func (c *mongoStoreFakeCollection) EnsureIndexes(_ context.Context, indexes []fmongo.IndexModel) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ensureIndexes = append(c.ensureIndexes, indexes...)
	return nil
}

func newMongoStoreTest(t *testing.T) (*MongoStore, *mongoStoreFakeClient, *mongoStoreFakeCollection) {
	t.Helper()
	db := &mongoStoreFakeDatabase{}
	client := &mongoStoreFakeClient{db: db}
	store, err := NewMongoStore(client, MongoStoreConfig{DefaultDatabase: "game", ServerID: 3, TransactionReceiptTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	collection := db.Collection("heroes").(*mongoStoreFakeCollection)
	return store, client, collection
}

func testMutationRecord(kind coredata.MutationKind) coredata.CommitRecord {
	var id coredata.TransactionID
	id[15] = 9
	mutation := coredata.Mutation{
		Key:             coredata.DocumentKey{Database: "game", Resource: "heroes", ID: 7},
		Kind:            kind,
		ExpectedVersion: 4,
		NextVersion:     5,
		Mask:            1,
		Schema:          2,
		Codec:           "bson-v2",
	}
	if kind == coredata.MutationPut {
		mutation.Data, _ = bson.Marshal(bson.M{"_id": int64(7), "level": int32(5)})
	}
	if kind == coredata.MutationPatch {
		mutation.Patch.SetBSON, _ = bson.Marshal(bson.D{{Key: "level", Value: int32(5)}})
	}
	return coredata.CommitRecord{ID: id, Mutations: []coredata.Mutation{mutation}}
}

func TestMongoStorePatchExactVersionAndReplay(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	collection.updateResult = &fmongo.UpdateResult{MatchedCount: 1, ModifiedCount: 1}
	record := testMutationRecord(coredata.MutationPatch)
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if client.startSessions != 0 {
		t.Fatalf("single mutation fast path started %d sessions", client.startSessions)
	}
	filter := collection.lastFilter.(bson.M)
	if filter["_id"] != int64(7) || filter["_version"] != uint64(4) {
		t.Fatalf("filter=%v", filter)
	}
	set := collection.lastUpdate.(bson.M)["$set"].(bson.M)
	if set["level"] != int32(5) || set["_version"] != uint64(5) {
		t.Fatalf("patch set=%#v", set)
	}

	collection.updateResult = &fmongo.UpdateResult{}
	collection.findDoc = bson.M{"_id": int64(7), "_version": uint64(5), "_last_tx": record.ID.String()}
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
}

func TestMongoStorePatchRejectsWrongOrMissingBase(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	collection.updateResult = &fmongo.UpdateResult{}
	collection.findDoc = bson.M{"_id": int64(7), "_version": uint64(3), "_last_tx": "other"}
	if err := store.Project(context.Background(), testMutationRecord(coredata.MutationPatch)); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("wrong base err=%v", err)
	}
	collection.findDoc = nil
	collection.findErr = fmongo.ErrNotFound
	if err := store.Project(context.Background(), testMutationRecord(coredata.MutationPatch)); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("missing base err=%v", err)
	}
}

func TestMongoStoreProjectBatchUsesOneTransactionAndDurableMarkers(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	first := testMutationRecord(coredata.MutationPatch)
	second := testMutationRecord(coredata.MutationPatch)
	second.ID[15] = 10
	second.Mutations[0].ExpectedVersion = 5
	second.Mutations[0].NextVersion = 6
	collection.bulkResult = &fmongo.BulkWriteResult{MatchedCount: 2, ModifiedCount: 2}

	if err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{first, second}); err != nil {
		t.Fatal(err)
	}
	if client.startSessions != 1 {
		t.Fatalf("sessions=%d, want 1", client.startSessions)
	}
	if len(collection.bulkModels) != 2 {
		t.Fatalf("mutation bulk models=%d, want 2", len(collection.bulkModels))
	}
	markers := client.db.Collection(transactionCollection).(*mongoStoreFakeCollection)
	if len(markers.bulkModels) != 2 {
		t.Fatalf("transaction marker bulk models=%d, want 2", len(markers.bulkModels))
	}
}

func TestMongoStoreProjectBatchRollsBackOnCASMismatch(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	first := testMutationRecord(coredata.MutationPatch)
	second := testMutationRecord(coredata.MutationPatch)
	second.ID[15] = 10
	second.Mutations[0].ExpectedVersion = 5
	second.Mutations[0].NextVersion = 6
	collection.bulkResult = &fmongo.BulkWriteResult{MatchedCount: 1, ModifiedCount: 1}

	if err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{first, second}); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("err=%v, want ErrProjectionConflict", err)
	}
}

func TestMongoStoreProjectBatchValidatesBeforeCheckingEligibility(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	record := testMutationRecord(coredata.MutationPatch)
	record.Mutations[0].Key.Resource = ""
	record.Effects = []coredata.Effect{{ID: "special", Topic: "special"}}
	if err := coredata.ValidateCommitRecord(record); err == nil {
		t.Fatal("test record unexpectedly passed validation")
	}

	err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{record})
	if err == nil || errors.Is(err, errProjectionBatchUnsupported) {
		t.Fatalf("err=%v, want validation error before unsupported classification", err)
	}
	if client.startSessions != 0 || len(collection.bulkModels) != 0 {
		t.Fatalf("validation failure started sessions=%d mutation writes=%d", client.startSessions, len(collection.bulkModels))
	}
}

func TestMongoStoreProjectBatchRejectsUnsupportedBeforeSessionOrWrites(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	record := testMutationRecord(coredata.MutationPatch)
	record.Effects = []coredata.Effect{{ID: "special", Topic: "special"}}

	err := store.ProjectBatch(context.Background(), []coredata.CommitRecord{record})
	if !errors.Is(err, errProjectionBatchUnsupported) {
		t.Fatalf("err=%v, want errProjectionBatchUnsupported", err)
	}
	markers := client.db.Collection(transactionCollection).(*mongoStoreFakeCollection)
	if client.startSessions != 0 || len(collection.bulkModels) != 0 || len(markers.bulkModels) != 0 {
		t.Fatalf("unsupported batch started sessions=%d mutation writes=%d marker writes=%d",
			client.startSessions, len(collection.bulkModels), len(markers.bulkModels))
	}
}

func TestMongoStoreDeleteWritesVersionedTombstone(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	collection.updateResult = &fmongo.UpdateResult{MatchedCount: 1}
	if err := store.Project(context.Background(), testMutationRecord(coredata.MutationDelete)); err != nil {
		t.Fatal(err)
	}
	update := collection.lastUpdate.(bson.M)
	set := update["$set"].(bson.M)
	if set["_deleted"] != true || set["_version"] != uint64(5) {
		t.Fatalf("delete update=%v", update)
	}
}

func TestMongoStoreMigrationConflictBecomesObsoleteNoop(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	collection.findOneAndUpdateErr = fmongo.ErrDuplicateKey
	collection.findDoc = projectionMeta{Version: 12, LastTx: "another-transaction"}
	record := testMutationRecord(coredata.MutationPut)
	record.Handler = MigrationHandler
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatalf("obsolete migration must be acknowledged for repository reload: %v", err)
	}
}

func TestMongoStoreProjectsRemoteAndOrdinaryMutationInOneSession(t *testing.T) {
	const kind entity.EntityKind = 241
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	entityID, err := entity.BuildEntityID(77, kind)
	if err != nil {
		t.Fatal(err)
	}
	store, client, _ := newMongoStoreTest(t)
	remoteProjection := &mongoRemoteProjectionFake{}
	if err := store.SetRemoteProjection(remoteProjection, remoteProjection); err != nil {
		t.Fatal(err)
	}
	var txID coredata.TransactionID
	txID[15] = 44
	remote := entity.RemoteCommit{
		TransactionID: entity.RemoteTransactionID(txID), EntityID: entityID, Kind: kind,
		BaseVersion: 1, NextVersion: 2, MarkerEpoch: 1, RouteEpoch: 1, Schema: 1, Codec: 1,
		Mutations: []entity.RemoteDataMutation{{Collection: "heroes", ID: entityID, Version: 2, Mask: 1, Data: []byte("remote")}},
	}
	ordinary, _ := bson.Marshal(bson.M{"_id": int64(5), "value": "ordinary"})
	record := coredata.CommitRecord{ID: txID, Mutations: []coredata.Mutation{
		{Key: coredata.DocumentKey{Resource: "heroes", ID: entityID}, Kind: coredata.MutationPut, ExpectedVersion: 1, NextVersion: 2, Remote: &remote},
		{Key: coredata.DocumentKey{Database: "game", Resource: "mail", ID: 5}, Kind: coredata.MutationPut, ExpectedVersion: 0, NextVersion: 1, Data: ordinary},
	}}
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if client.startSessions != 1 || remoteProjection.stored != 1 || remoteProjection.applied != 1 {
		t.Fatalf("sessions=%d stored=%d applied=%d", client.startSessions, remoteProjection.stored, remoteProjection.applied)
	}
}

func TestMongoStoreOlderPutCannotReviveTombstone(t *testing.T) {
	store, _, collection := newMongoStoreTest(t)
	collection.findOneAndUpdateErr = fmongo.ErrDuplicateKey
	collection.findDoc = bson.M{"_id": int64(7), "_version": uint64(6), "_deleted": true, "_last_tx": "other"}
	if err := store.Project(context.Background(), testMutationRecord(coredata.MutationPut)); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("err=%v, want ErrProjectionConflict", err)
	}
}

func TestMongoStoreMultiRecordUsesTransactionAndStagesEffectsReceipts(t *testing.T) {
	store, client, collection := newMongoStoreTest(t)
	collection.updateResult = &fmongo.UpdateResult{MatchedCount: 1}
	record := testMutationRecord(coredata.MutationPatch)
	record.Effects = []coredata.Effect{{ID: "effect-1", Topic: "hero.changed", Payload: []byte{1}}}
	record.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "step-1", Digest: []byte{2}}}
	client.db.Collection(transactionCollection).(*mongoStoreFakeCollection).findErr = fmongo.ErrNotFound
	if err := store.Project(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if client.startSessions != 1 {
		t.Fatalf("sessions=%d, want 1", client.startSessions)
	}
	if len(client.db.Collection(outboxCollection).(*mongoStoreFakeCollection).inserted) != 1 || len(client.db.Collection(receiptCollection).(*mongoStoreFakeCollection).inserted) != 1 || len(client.db.Collection(transactionCollection).(*mongoStoreFakeCollection).inserted) != 1 {
		t.Fatal("transaction did not stage effect, receipt, and transaction identity")
	}
}

func TestMongoStoreReceiptIdentityIncludesCompletionPayload(t *testing.T) {
	store, client, _ := newMongoStoreTest(t)
	collection := client.db.Collection(receiptCollection).(*mongoStoreFakeCollection)
	collection.insertErr = fmongo.ErrDuplicateKey
	collection.findDoc = receiptDocument{Digest: []byte{1}, Payload: []byte("different")}
	err := store.stageReceipt(context.Background(), "tx-1", coredata.Receipt{
		Namespace: "saga-step", ID: "command-1", Digest: []byte{1}, Payload: []byte("completion"),
	})
	if !errors.Is(err, ErrReceiptIdentity) {
		t.Fatalf("err=%v", err)
	}
}
