package saga

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	coresaga "github.com/tjbdwanghaibo/cube-core/saga"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMongoCommandInboxExecutesStepOnceForMessageRedelivery(t *testing.T) {
	client := newInboxMongoFake()
	inbox, err := NewMongoCommandInbox(client, "game", "")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		calls.Add(1)
		return coresaga.Completion{Success: true, Data: []byte("reserved")}, nil
	}
	now := time.Now()
	command := coresaga.Command{ID: "delivery-1", IdempotencyKey: "saga:forward:0", SagaID: "saga", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Step: 0, StepName: "reserve", Phase: coresaga.PhaseForward, Attempt: 1, Topic: "reserve", Payload: []byte("input"), CreatedAt: now, DeadlineAt: now.Add(time.Second)}
	first, duplicate, err := inbox.Handle(context.Background(), command, handler)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate || first.CommandID != "delivery-1" {
		t.Fatalf("unexpected first result: %+v duplicate=%v", first, duplicate)
	}
	second, duplicate, err := inbox.Handle(context.Background(), command, handler)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate || second.CommandID != "delivery-1" || string(second.Data) != "reserved" {
		t.Fatalf("unexpected duplicate result: %+v duplicate=%v", second, duplicate)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls=%d", calls.Load())
	}
}

func TestMongoCommandInboxAllowsNewSagaAttempt(t *testing.T) {
	client := newInboxMongoFake()
	inbox, _ := NewMongoCommandInbox(client, "game", "")
	var calls atomic.Int32
	handler := func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		attempt := calls.Add(1)
		if attempt == 1 {
			return coresaga.Completion{Retryable: true, Error: "busy"}, nil
		}
		return coresaga.Completion{Success: true}, nil
	}
	now := time.Now()
	command := coresaga.Command{ID: "attempt-1", IdempotencyKey: "stable-op", SagaID: "saga", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-2", Step: 0, StepName: "reserve", Phase: coresaga.PhaseForward, Attempt: 1, Topic: "reserve", CreatedAt: now, DeadlineAt: now.Add(time.Second)}
	first, _, err := inbox.Handle(context.Background(), command, handler)
	if err != nil || !first.Retryable {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	command.ID = "attempt-2"
	command.Attempt = 2
	second, duplicate, err := inbox.Handle(context.Background(), command, handler)
	if err != nil || duplicate || !second.Success {
		t.Fatalf("second=%+v duplicate=%v err=%v", second, duplicate, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler calls=%d", calls.Load())
	}
}

func TestMongoCommandInboxRejectsCommandIDReuse(t *testing.T) {
	client := newInboxMongoFake()
	inbox, _ := NewMongoCommandInbox(client, "game", "")
	handler := func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		return coresaga.Completion{Success: true}, nil
	}
	now := time.Now()
	command := coresaga.Command{ID: "one", IdempotencyKey: "op", SagaID: "saga", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-3", Step: 0, StepName: "reserve", Phase: coresaga.PhaseForward, Attempt: 1, Topic: "reserve", Payload: []byte("a"), CreatedAt: now, DeadlineAt: now.Add(time.Second)}
	if _, _, err := inbox.Handle(context.Background(), command, handler); err != nil {
		t.Fatal(err)
	}
	command.Payload = []byte("different")
	if _, _, err := inbox.Handle(context.Background(), command, handler); !errors.Is(err, coresaga.ErrIdentityConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestStorageDigestsUseStableOperationIdentity(t *testing.T) {
	one := coresaga.Completion{CommandID: "delivery-1", IdempotencyKey: "op", SagaID: "saga", Success: true, Data: []byte("x"), CompletedAt: time.Now()}
	two := one
	two.CompletedAt = two.CompletedAt.Add(time.Hour)
	if string(completionDigest(one)) != string(completionDigest(two)) {
		t.Fatal("completion timestamp changed identity")
	}
	two.CommandID = "delivery-2"
	if string(completionDigest(one)) == string(completionDigest(two)) {
		t.Fatal("different command IDs shared a receipt identity")
	}
	two = one
	two.Data = []byte("y")
	if string(completionDigest(one)) == string(completionDigest(two)) {
		t.Fatal("business result did not change identity")
	}
}

type inboxMongoFake struct{ db *inboxDatabaseFake }

func newInboxMongoFake() *inboxMongoFake {
	return &inboxMongoFake{db: &inboxDatabaseFake{collections: map[string]*inboxCollectionFake{}}}
}
func (m *inboxMongoFake) Database(string) fmongo.IDatabase              { return m.db }
func (m *inboxMongoFake) DatabaseForSid(string, int32) fmongo.IDatabase { return m.db }
func (m *inboxMongoFake) StartSession(context.Context) (fmongo.ISession, error) {
	return inboxSessionFake{}, nil
}
func (m *inboxMongoFake) Ping(context.Context) error  { return nil }
func (m *inboxMongoFake) Close(context.Context) error { return nil }

type inboxSessionFake struct{}

func (inboxSessionFake) WithTransaction(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}
func (inboxSessionFake) EndSession(context.Context) {}

type inboxDatabaseFake struct {
	mu          sync.Mutex
	collections map[string]*inboxCollectionFake
}

func (d *inboxDatabaseFake) Name() string               { return "game" }
func (d *inboxDatabaseFake) Drop(context.Context) error { return nil }
func (d *inboxDatabaseFake) Collection(name string) fmongo.ICollection {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.collections[name]
	if c == nil {
		c = &inboxCollectionFake{receipts: map[string]commandReceiptDoc{}}
		d.collections[name] = c
	}
	return c
}

type inboxCollectionFake struct {
	mu       sync.Mutex
	receipts map[string]commandReceiptDoc
}

func (c *inboxCollectionFake) InsertOne(_ context.Context, doc any) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	receipt, ok := doc.(commandReceiptDoc)
	if !ok {
		return "", errors.New("unexpected document")
	}
	if _, exists := c.receipts[receipt.ID]; exists {
		return "", fmongo.ErrDuplicateKey
	}
	c.receipts[receipt.ID] = receipt
	return receipt.ID, nil
}
func (c *inboxCollectionFake) FindOne(_ context.Context, filter any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := filterID(filter)
	receipt, ok := c.receipts[id]
	if !ok {
		return fmongo.ErrNotFound
	}
	target, ok := result.(*commandReceiptDoc)
	if !ok {
		return errors.New("unexpected result")
	}
	*target = receipt
	return nil
}
func (c *inboxCollectionFake) EnsureIndexes(context.Context, []fmongo.IndexModel) error { return nil }
func (c *inboxCollectionFake) InsertMany(context.Context, []any) ([]string, error) {
	return nil, errors.New("unsupported")
}
func (c *inboxCollectionFake) Find(context.Context, any, any, ...fmongo.FindOption) error {
	return errors.New("unsupported")
}
func (c *inboxCollectionFake) UpdateOne(_ context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := filterID(filter)
	receipt, exists := c.receipts[id]
	if !exists {
		return &fmongo.UpdateResult{}, nil
	}
	values, ok := update.(bson.M)
	if !ok {
		return nil, errors.New("unexpected update")
	}
	set, ok := values["$set"].(bson.M)
	if !ok {
		return nil, errors.New("unexpected set")
	}
	if raw, ok := set["completion"].([]byte); ok {
		receipt.Completion = append([]byte(nil), raw...)
	}
	if createdAt, ok := set["created_at"].(time.Time); ok {
		receipt.CreatedAt = createdAt
	}
	c.receipts[id] = receipt
	return &fmongo.UpdateResult{MatchedCount: 1, ModifiedCount: 1}, nil
}
func (c *inboxCollectionFake) UpdateMany(context.Context, any, any) (*fmongo.UpdateResult, error) {
	return nil, errors.New("unsupported")
}
func (c *inboxCollectionFake) ReplaceOne(context.Context, any, any) (*fmongo.UpdateResult, error) {
	return nil, errors.New("unsupported")
}
func (c *inboxCollectionFake) DeleteOne(context.Context, any) (int64, error) {
	return 0, errors.New("unsupported")
}
func (c *inboxCollectionFake) DeleteMany(context.Context, any) (int64, error) {
	return 0, errors.New("unsupported")
}
func (c *inboxCollectionFake) FindOneAndUpdate(context.Context, any, any, any, ...fmongo.FindOneAndUpdateOption) error {
	return errors.New("unsupported")
}
func (c *inboxCollectionFake) FindOneAndDelete(context.Context, any, any) error {
	return errors.New("unsupported")
}
func (c *inboxCollectionFake) FindOneAndReplace(context.Context, any, any, any) error {
	return errors.New("unsupported")
}
func (c *inboxCollectionFake) CountDocuments(context.Context, any) (int64, error) {
	return 0, errors.New("unsupported")
}
func (c *inboxCollectionFake) Aggregate(context.Context, any, any) error {
	return errors.New("unsupported")
}
func (c *inboxCollectionFake) BulkWrite(context.Context, []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	return nil, errors.New("unsupported")
}
func filterID(filter any) string {
	if values, ok := filter.(bson.M); ok {
		if id, ok := values["_id"].(string); ok {
			return id
		}
	}
	if values, ok := filter.(map[string]any); ok {
		if id, ok := values["_id"].(string); ok {
			return id
		}
	}
	return ""
}

var _ fmongo.IMongo = (*inboxMongoFake)(nil)
var _ fmongo.IDatabase = (*inboxDatabaseFake)(nil)
var _ fmongo.ICollection = (*inboxCollectionFake)(nil)
