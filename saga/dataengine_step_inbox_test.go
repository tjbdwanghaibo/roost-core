package saga

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func dataEngineCommand(id, operation string, payload string) Command {
	now := time.Now().UTC()
	return Command{
		ID: id, IdempotencyKey: operation, SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "r-1", StepName: "reserve", Phase: PhaseForward, Attempt: 1,
		Topic: "rally.reserve", Payload: []byte(payload), CreatedAt: now, DeadlineAt: now.Add(time.Minute),
	}
}

type stepFenceCommitter struct{ record coredata.CommitRecord }

func (committer *stepFenceCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	committer.record = coredata.CloneCommitRecord(record)
	return nil
}

func TestDataEngineStepBindCarriesExplicitReservationFence(t *testing.T) {
	inbox, err := NewDataEngineStepInbox(newDataEngineInboxMongo(), "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("command-fenced", "operation-fenced", "payload")
	reservation := inbox.activeReservation(command.IdempotencyKey, command.ID, mustCommandDigest(t, command), 9)
	ctx := withReservation(context.Background(), reservation)
	extracted, ok := ReservationFromContext(ctx)
	if !ok || extracted.Token != reservation.Token {
		t.Fatalf("reservation=%+v ok=%t", extracted, ok)
	}
	committer := &stepFenceCommitter{}
	_, err = corenest.RunIsolatedTransaction(ctx, committer, "saga-fence-test", func() (any, error) {
		return nil, inbox.Bind(command, extracted)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(committer.record.Receipts) != 2 {
		t.Fatalf("receipts=%+v", committer.record.Receipts)
	}
	fence, control, err := coredata.DecodeLeaseFenceReceipt(committer.record.Receipts[0])
	if err != nil || !control || fence.Owner != "worker-1" || fence.Token != 9 || fence.Resource != dataEngineOperationCollection || fence.DocumentID != command.IdempotencyKey || !bytes.Equal(fence.Digest, mustCommandDigest(t, command)) {
		t.Fatalf("fence=%+v control=%t err=%v", fence, control, err)
	}
}

func TestDataEngineStepBindRejectsReservationFromAnotherCommand(t *testing.T) {
	inbox, err := NewDataEngineStepInbox(newDataEngineInboxMongo(), "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	reserved := dataEngineCommand("command-a", "operation-a", "payload-a")
	other := dataEngineCommand("command-b", "operation-b", "payload-b")
	reservation := inbox.activeReservation(reserved.IdempotencyKey, reserved.ID, mustCommandDigest(t, reserved), 1)
	committer := &stepFenceCommitter{}
	_, err = corenest.RunIsolatedTransaction(context.Background(), committer, "saga-fence-test", func() (any, error) {
		return nil, inbox.Bind(other, reservation)
	})
	if err == nil {
		t.Fatal("reservation from another command was accepted")
	}
	if !committer.record.Empty() {
		t.Fatalf("mismatched reservation committed record=%+v", committer.record)
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
	if _, err := inbox.Reserve(context.Background(), conflict); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	// U-0280：同一操作实例（IdempotencyKey）的另一次尝试，在第一次尝试的租约有效时不能开始，
	// 只能等它有结论；租约过期（封顶在第一次尝试的截止时间）后接替它，第一次尝试从此不会再执行。
	newAttempt := command
	newAttempt.ID = "command-2"
	newAttempt.Attempt = 2
	newAttempt.DeadlineAt = command.DeadlineAt.Add(time.Minute)
	if reservation, err := inbox.Reserve(context.Background(), newAttempt); !errors.Is(err, errOperationAttemptInFlight) {
		t.Fatalf("new attempt during the first attempt's lease=%+v err=%v, want errOperationAttemptInFlight", reservation, err)
	}
	inbox.now = func() time.Time { return command.DeadlineAt }
	if reservation, err := inbox.Reserve(context.Background(), newAttempt); err != nil || reservation.Duplicate {
		t.Fatalf("new attempt after the first attempt's deadline=%+v err=%v", reservation, err)
	}
	// 状态文档：第二次尝试是当前尝试（token 2，第一次尝试的 fence 从此不再匹配），第一次尝试记为被它接替。
	state := inboxOperation(t, client, command.IdempotencyKey)
	if state.CommandID != newAttempt.ID || state.LeaseToken != 2 || state.Status != operationStatusPending ||
		len(state.Superseded) != 1 || state.Superseded[0].CommandID != command.ID || state.Superseded[0].By != newAttempt.ID {
		t.Fatalf("operation state after the first attempt was superseded=%+v", state)
	}
	if _, err := inbox.Reserve(context.Background(), command); !errors.Is(err, errAttemptSuperseded) && !errors.Is(err, ErrCommandExpired) {
		t.Fatalf("redelivery of the superseded first attempt err=%v, want errAttemptSuperseded or ErrCommandExpired", err)
	}
}

func TestDataEngineStepInboxReplaysAuthoritativeReceiptAndCompletesClaim(t *testing.T) {
	client := newDataEngineInboxMongo()
	inbox, _ := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	command := dataEngineCommand("command-1", "operation-1", "a")
	if _, err := inbox.Reserve(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	completion := Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID, Success: true, Data: []byte("reserved"), CompletedAt: time.Now().UTC()}
	effect, _ := NewCompletionEffect(completion)
	if err := inboxReceipts(client).Seed(dataEngineReceipt{
		ID: dataEngineStepNamespace + "/" + command.ID, Digest: mustCommandDigest(t, command), Payload: effect.Payload,
	}); err != nil {
		t.Fatal(err)
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

func TestDataEngineStepInboxUsesAbsoluteOperationExpiry(t *testing.T) {
	client := newDataEngineInboxMongo()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	operations := inboxOperations(client)
	// 每个操作一份状态文档，所有读写按 _id：只有过期清理一个索引（状态文档方案，claims 集合的四个索引已删除）。
	if len(operations.Indexes) != 1 || !operations.HasIndex("expires_at") {
		t.Fatalf("operation indexes=%+v, want only the expiry index", operations.Indexes)
	}
	expiry := operations.Indexes[0]
	if !expiry.ExpireAt || expiry.TTL != 0 || !expiry.RecreateOnConflict {
		t.Fatalf("operation expiry index=%+v", expiry)
	}
}

func newDataEngineInboxMongo() *mongotest.Client { return mongotest.NewClient() }

func inboxOperations(client *mongotest.Client) *mongotest.Collection {
	return client.Collection("game", dataEngineOperationCollection)
}

// inboxOperation 读原生收件箱里一个操作实例的状态文档。
func inboxOperation(t *testing.T, client *mongotest.Client, operationKey string) stepOperation {
	t.Helper()
	var state stepOperation
	if err := inboxOperations(client).FindOne(context.Background(), bson.M{"_id": operationKey}, &state); err != nil {
		t.Fatalf("operation %s: %v", operationKey, err)
	}
	return state
}

func inboxReceipts(client *mongotest.Client) *mongotest.Collection {
	return client.Collection("game", dataEngineReceiptCollection)
}

// The projector lives in another package and queries the operation state document this
// package writes. Nothing in the compiler ties the two together, and a
// mismatch is silent by construction: an unsatisfiable predicate looks exactly
// like a legitimately stale lease, so every fenced transaction would be
// acknowledged as a skipped no-op with no error and no failing test.
//
// This closes that gap by round-tripping a real state document through BSON and
// evaluating the real predicate against it, then verifying the predicate
// rejects every single-field deviation. A rename or a type change on either
// side fails here.
func TestDataEngineOperationStateSatisfiesProjectorFencePredicate(t *testing.T) {
	client := mongotest.NewClient()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{
		Owner: "worker-1", LeaseDuration: time.Minute, ReceiptTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	command := dataEngineCommand("command-fence", "operation-fence", "payload")
	reservation, err := inbox.Reserve(context.Background(), command)
	if err != nil || reservation.Token == 0 || reservation.Duplicate {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}

	// Bind produces the fence the projector will later evaluate.
	committer := &stepFenceCommitter{}
	ctx := withReservation(context.Background(), reservation)
	extracted, _ := ReservationFromContext(ctx)
	if _, err := corenest.RunIsolatedTransaction(ctx, committer, "saga-fence-contract", func() (any, error) {
		return nil, inbox.Bind(command, extracted)
	}); err != nil {
		t.Fatal(err)
	}
	fence, control, err := coredata.DecodeLeaseFenceReceipt(committer.record.Receipts[0])
	if err != nil || !control {
		t.Fatalf("fence=%+v control=%v err=%v", fence, control, err)
	}

	operations := inboxOperations(client)
	now := time.Now().UTC()
	var found bson.M
	if err := operations.FindOne(context.Background(), fence.Predicate(now), &found); err != nil {
		stored, _ := operations.Lookup(fence.DocumentID)
		t.Fatalf("the projector's predicate does not match the operation state this package writes: %v\npredicate=%v\nstored=%v",
			err, fence.Predicate(now), stored)
	}

	// Every field of the predicate must be load-bearing: if any deviation
	// still matched, the fence would not actually be fencing anything.
	for name, mutate := range map[string]func(*coredata.LeaseFence){
		"other owner":  func(f *coredata.LeaseFence) { f.Owner = "worker-2" },
		"other token":  func(f *coredata.LeaseFence) { f.Token++ },
		"other digest": func(f *coredata.LeaseFence) { f.Digest = bytes.Repeat([]byte{9}, len(f.Digest)) },
		"other document": func(f *coredata.LeaseFence) {
			f.DocumentID = "other-operation"
		},
	} {
		deviated := fence
		mutate(&deviated)
		if err := operations.FindOne(context.Background(), deviated.Predicate(now), &found); !errors.Is(err, fmongo.ErrNotFound) {
			t.Fatalf("%s: predicate still matched (err=%v)", name, err)
		}
	}

	// An expired lease must stop matching without touching the document.
	if err := operations.FindOne(context.Background(), fence.Predicate(now.Add(2*time.Minute)), &found); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("expired lease still matched: %v", err)
	}

	// A settled attempt must stop matching: the status value is shared with
	// the projector precisely so this transition is respected.
	completion := Completion{
		CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID,
		Success: true, CompletedAt: now,
	}
	if err := inbox.markCompleted(context.Background(), command.IdempotencyKey, command.ID, completion); err != nil {
		t.Fatal(err)
	}
	if err := operations.FindOne(context.Background(), fence.Predicate(now), &found); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("settled attempt still matched the pending predicate: %v", err)
	}
}
