package saga

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	coresaga "github.com/tjbdwanghaibo/cube-core/saga"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func dataEngineCommand(id, operation string, payload string) coresaga.Command {
	now := time.Now().UTC()
	return coresaga.Command{
		ID: id, IdempotencyKey: operation, SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "r-1", StepName: "reserve", Phase: coresaga.PhaseForward, Attempt: 1,
		Topic: "rally.reserve", Payload: []byte(payload), CreatedAt: now, DeadlineAt: now.Add(time.Minute),
	}
}

func TestDataEngineStepInboxReservesCommandIdentityAndAllowsNewAttempt(t *testing.T) {
	client := newDataEngineInboxMongo()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("command-1", "operation-1", "a")
	first, err := inbox.Reserve(context.Background(), command)
	if err != nil || first.Duplicate || first.Token != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	duplicate, err := inbox.Reserve(context.Background(), command)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	conflict := command
	conflict.Payload = []byte("different")
	if _, err := inbox.Reserve(context.Background(), conflict); !errors.Is(err, coresaga.ErrIdentityConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	newAttempt := command
	newAttempt.ID = "command-2"
	newAttempt.Attempt = 2
	if reservation, err := inbox.Reserve(context.Background(), newAttempt); err != nil || reservation.Duplicate {
		t.Fatalf("new attempt=%+v err=%v", reservation, err)
	}
}

func TestDataEngineStepInboxReplaysAuthoritativeReceiptAndCompletesClaim(t *testing.T) {
	client := newDataEngineInboxMongo()
	inbox, _ := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	command := dataEngineCommand("command-1", "operation-1", "a")
	if _, err := inbox.Reserve(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	completion := coresaga.Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true, Data: []byte("reserved"), CompletedAt: time.Now().UTC()}
	effect, _ := coresaga.NewCompletionEffect(completion)
	client.db.receipts.receipts[dataEngineStepNamespace+"/"+command.ID] = dataEngineReceipt{
		ID: dataEngineStepNamespace + "/" + command.ID, Digest: commandDigest(command), Payload: effect.Payload,
	}
	replayed, found, err := inbox.Replay(context.Background(), command)
	if err != nil || !found || replayed.CommandID != command.ID || string(replayed.Data) != "reserved" {
		t.Fatalf("completion=%+v found=%v err=%v", replayed, found, err)
	}
	reservation, err := inbox.Reserve(context.Background(), command)
	if err != nil || !reservation.Duplicate || reservation.Completion.CommandID != command.ID {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
}

type dataEngineInboxMongo struct{ db *dataEngineInboxDatabase }

func newDataEngineInboxMongo() *dataEngineInboxMongo {
	return &dataEngineInboxMongo{db: &dataEngineInboxDatabase{
		claims:   &dataEngineInboxCollection{claims: make(map[string]dataEngineClaim)},
		receipts: &dataEngineInboxCollection{receipts: make(map[string]dataEngineReceipt)},
	}}
}
func (mongo *dataEngineInboxMongo) Database(string) fmongo.IDatabase              { return mongo.db }
func (mongo *dataEngineInboxMongo) DatabaseForSid(string, int32) fmongo.IDatabase { return mongo.db }
func (*dataEngineInboxMongo) StartSession(context.Context) (fmongo.ISession, error) {
	return dataEngineInboxSession{}, nil
}
func (*dataEngineInboxMongo) Ping(context.Context) error  { return nil }
func (*dataEngineInboxMongo) Close(context.Context) error { return nil }

type dataEngineInboxSession struct{}

func (dataEngineInboxSession) WithTransaction(ctx context.Context, apply func(context.Context) error) error {
	return apply(ctx)
}
func (dataEngineInboxSession) EndSession(context.Context) {}

type dataEngineInboxDatabase struct {
	claims, receipts *dataEngineInboxCollection
}

func (*dataEngineInboxDatabase) Name() string { return "game" }
func (database *dataEngineInboxDatabase) Collection(name string) fmongo.ICollection {
	if name == dataEngineClaimCollection {
		return database.claims
	}
	return database.receipts
}
func (*dataEngineInboxDatabase) Drop(context.Context) error { return nil }

type dataEngineInboxCollection struct {
	mu       sync.Mutex
	claims   map[string]dataEngineClaim
	receipts map[string]dataEngineReceipt
}

func (collection *dataEngineInboxCollection) InsertOne(_ context.Context, value any) (string, error) {
	collection.mu.Lock()
	defer collection.mu.Unlock()
	claim, ok := value.(dataEngineClaim)
	if !ok {
		return "", errors.New("unexpected insert")
	}
	if _, exists := collection.claims[claim.ID]; exists {
		return "", fmongo.ErrDuplicateKey
	}
	collection.claims[claim.ID] = claim
	return claim.ID, nil
}
func (*dataEngineInboxCollection) InsertMany(context.Context, []any) ([]string, error) {
	return nil, errors.New("unused")
}
func (collection *dataEngineInboxCollection) FindOne(_ context.Context, filter any, result any) error {
	collection.mu.Lock()
	defer collection.mu.Unlock()
	id := filterID(filter)
	switch target := result.(type) {
	case *dataEngineClaim:
		value, ok := collection.claims[id]
		if !ok {
			return fmongo.ErrNotFound
		}
		*target = value
		return nil
	case *dataEngineReceipt:
		value, ok := collection.receipts[id]
		if !ok {
			return fmongo.ErrNotFound
		}
		*target = value
		return nil
	default:
		return errors.New("unexpected find result")
	}
}
func (*dataEngineInboxCollection) Find(context.Context, any, any, ...fmongo.FindOption) error {
	return errors.New("unused")
}
func (collection *dataEngineInboxCollection) UpdateOne(_ context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	collection.mu.Lock()
	defer collection.mu.Unlock()
	id := filterID(filter)
	claim, ok := collection.claims[id]
	if !ok {
		return &fmongo.UpdateResult{}, nil
	}
	set := update.(bson.M)["$set"].(bson.M)
	if status, ok := set["status"].(string); ok {
		claim.Status = status
	}
	if raw, ok := set["completion"].([]byte); ok {
		claim.Completion = append([]byte(nil), raw...)
	}
	collection.claims[id] = claim
	return &fmongo.UpdateResult{MatchedCount: 1, ModifiedCount: 1}, nil
}
func (*dataEngineInboxCollection) UpdateMany(context.Context, any, any) (*fmongo.UpdateResult, error) {
	return nil, errors.New("unused")
}
func (*dataEngineInboxCollection) ReplaceOne(context.Context, any, any) (*fmongo.UpdateResult, error) {
	return nil, errors.New("unused")
}
func (*dataEngineInboxCollection) DeleteOne(context.Context, any) (int64, error) {
	return 0, errors.New("unused")
}
func (*dataEngineInboxCollection) DeleteMany(context.Context, any) (int64, error) {
	return 0, errors.New("unused")
}
func (collection *dataEngineInboxCollection) FindOneAndUpdate(_ context.Context, filter any, _ any, result any, _ ...fmongo.FindOneAndUpdateOption) error {
	collection.mu.Lock()
	defer collection.mu.Unlock()
	id := filterID(filter)
	claim, ok := collection.claims[id]
	if !ok {
		return fmongo.ErrNotFound
	}
	claim.LeaseToken++
	collection.claims[id] = claim
	*result.(*dataEngineClaim) = claim
	return nil
}
func (*dataEngineInboxCollection) FindOneAndDelete(context.Context, any, any) error {
	return errors.New("unused")
}
func (*dataEngineInboxCollection) FindOneAndReplace(context.Context, any, any, any) error {
	return errors.New("unused")
}
func (*dataEngineInboxCollection) CountDocuments(context.Context, any) (int64, error) {
	return 0, errors.New("unused")
}
func (*dataEngineInboxCollection) Aggregate(context.Context, any, any) error {
	return errors.New("unused")
}
func (*dataEngineInboxCollection) BulkWrite(context.Context, []fmongo.WriteModel) (*fmongo.BulkWriteResult, error) {
	return nil, errors.New("unused")
}
func (*dataEngineInboxCollection) EnsureIndexes(context.Context, []fmongo.IndexModel) error {
	return nil
}
