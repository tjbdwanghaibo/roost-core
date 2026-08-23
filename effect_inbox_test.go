package nestwal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type effectInboxMongoFake struct {
	db *effectInboxDatabaseFake
}

func (m *effectInboxMongoFake) Database(string) fmongo.IDatabase              { return m.db }
func (m *effectInboxMongoFake) DatabaseForSid(string, int32) fmongo.IDatabase { return m.db }
func (m *effectInboxMongoFake) StartSession(context.Context) (fmongo.ISession, error) {
	return effectInboxSessionFake{}, nil
}
func (m *effectInboxMongoFake) Ping(context.Context) error  { return nil }
func (m *effectInboxMongoFake) Close(context.Context) error { return nil }

type effectInboxSessionFake struct{}

func (effectInboxSessionFake) WithTransaction(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
func (effectInboxSessionFake) EndSession(context.Context) {}

type effectInboxDatabaseFake struct{ collection *effectInboxCollectionFake }

func (d *effectInboxDatabaseFake) Name() string { return "game" }
func (d *effectInboxDatabaseFake) Collection(string) fmongo.ICollection {
	return d.collection
}
func (d *effectInboxDatabaseFake) Drop(context.Context) error { return nil }

type effectInboxCollectionFake struct {
	mu       sync.Mutex
	receipts map[string]effectInboxReceipt
	indexes  []fmongo.IndexModel
}

func (c *effectInboxCollectionFake) InsertOne(_ context.Context, document any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	receipt := document.(effectInboxReceipt)
	if _, exists := c.receipts[receipt.ID]; exists {
		return "", fmongo.ErrDuplicateKey
	}
	c.receipts[receipt.ID] = receipt
	return receipt.ID, nil
}
func (c *effectInboxCollectionFake) InsertMany(context.Context, []any) ([]string, error) {
	panic("unused")
}
func (c *effectInboxCollectionFake) FindOne(_ context.Context, filter any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := filter.(bson.M)["_id"].(string)
	receipt, exists := c.receipts[id]
	if !exists {
		return fmongo.ErrNotFound
	}
	*(result.(*effectInboxReceipt)) = receipt
	return nil
}
func (c *effectInboxCollectionFake) Find(context.Context, any, any, ...fmongo.FindOption) error {
	panic("unused")
}
func (c *effectInboxCollectionFake) UpdateOne(context.Context, any, any) (*fmongo.UpdateResult, error) {
	panic("unused")
}
func (c *effectInboxCollectionFake) UpdateMany(context.Context, any, any) (*fmongo.UpdateResult, error) {
	panic("unused")
}
func (c *effectInboxCollectionFake) ReplaceOne(context.Context, any, any) (*fmongo.UpdateResult, error) {
	panic("unused")
}
func (c *effectInboxCollectionFake) DeleteOne(context.Context, any) (int64, error) {
	panic("unused")
}
func (c *effectInboxCollectionFake) DeleteMany(context.Context, any) (int64, error) {
	panic("unused")
}
func (c *effectInboxCollectionFake) FindOneAndUpdate(context.Context, any, any, any, ...fmongo.FindOneAndUpdateOption) error {
	panic("unused")
}
func (c *effectInboxCollectionFake) FindOneAndDelete(context.Context, any, any) error {
	panic("unused")
}
func (c *effectInboxCollectionFake) FindOneAndReplace(context.Context, any, any, any) error {
	panic("unused")
}
func (c *effectInboxCollectionFake) CountDocuments(context.Context, any) (int64, error) {
	panic("unused")
}
func (c *effectInboxCollectionFake) Aggregate(context.Context, any, any) error { panic("unused") }
func (c *effectInboxCollectionFake) BulkWrite(context.Context, []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	panic("unused")
}
func (c *effectInboxCollectionFake) EnsureIndexes(_ context.Context, indexes []fmongo.IndexModel) error {
	c.mu.Lock()
	c.indexes = append([]fmongo.IndexModel(nil), indexes...)
	c.mu.Unlock()
	return nil
}

func TestMongoEffectInboxDeduplicatesAndRejectsIdentityConflict(t *testing.T) {
	collection := &effectInboxCollectionFake{receipts: make(map[string]effectInboxReceipt)}
	inbox, err := NewMongoEffectInbox(&effectInboxMongoFake{db: &effectInboxDatabaseFake{collection: collection}}, "game", "", EffectInboxOptions{ReceiptTTL: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(collection.indexes) != 1 || collection.indexes[0].TTL != int32((48*time.Hour)/time.Second) {
		t.Fatalf("indexes = %+v", collection.indexes)
	}
	envelope := EffectEnvelope{TransactionID: "tx-1", EffectID: "effect-1", Topic: "mail", Payload: []byte("reward")}
	calls := 0
	handler := func(context.Context, EffectEnvelope) error { calls++; return nil }
	duplicate, err := inbox.Handle(context.Background(), envelope, handler)
	if err != nil || duplicate {
		t.Fatalf("first handle duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = inbox.Handle(context.Background(), envelope, handler)
	if err != nil || !duplicate || calls != 1 {
		t.Fatalf("second handle duplicate=%v calls=%d err=%v", duplicate, calls, err)
	}
	conflict := envelope
	conflict.Payload = []byte("different")
	if _, err := inbox.Handle(context.Background(), conflict, handler); !errors.Is(err, ErrEffectIdentityConflict) {
		t.Fatalf("identity conflict error = %v", err)
	}
}
