package saga

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
	"github.com/tjbdwanghaibo/roost-kit/mongo/mongotest"
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
	reservation := inbox.activeReservation(command.ID, commandDigest(command), 9)
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
	if err != nil || !control || fence.Owner != "worker-1" || fence.Token != 9 || fence.DocumentID != "saga-step/command-fenced" || !bytes.Equal(fence.Digest, commandDigest(command)) {
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
	reservation := inbox.activeReservation(reserved.ID, commandDigest(reserved), 1)
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
	if err := inboxReceipts(client).Seed(dataEngineReceipt{
		ID: dataEngineStepNamespace + "/" + command.ID, Digest: commandDigest(command), Payload: effect.Payload,
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

func TestDataEngineStepInboxUsesAbsoluteClaimExpiry(t *testing.T) {
	client := newDataEngineInboxMongo()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	claims := inboxClaims(client)
	if len(claims.Indexes) != 3 {
		t.Fatalf("claim indexes=%d, want 3", len(claims.Indexes))
	}
	// The claim path and the command-identity uniqueness both depend on their
	// index existing, so assert them by shape rather than by position alone.
	if !claims.HasIndex("status", "lease_until") || !claims.HasIndex("namespace", "command_id") {
		t.Fatalf("claim indexes=%+v", claims.Indexes)
	}
	expiry := claims.Indexes[2]
	if !expiry.ExpireAt || expiry.TTL != 0 || !expiry.RecreateOnConflict {
		t.Fatalf("claim expiry index=%+v", expiry)
	}
}

func newDataEngineInboxMongo() *mongotest.Client { return mongotest.NewClient() }

func inboxClaims(client *mongotest.Client) *mongotest.Collection {
	return client.Collection("game", dataEngineClaimCollection)
}

func inboxReceipts(client *mongotest.Client) *mongotest.Collection {
	return client.Collection("game", dataEngineReceiptCollection)
}

// The projector lives in another package and queries the claim document this
// package writes. Nothing in the compiler ties the two together, and a
// mismatch is silent by construction: an unsatisfiable predicate looks exactly
// like a legitimately stale lease, so every fenced transaction would be
// acknowledged as a skipped no-op with no error and no failing test.
//
// This closes that gap by round-tripping a real claim through BSON and
// evaluating the real predicate against it, then verifying the predicate
// rejects every single-field deviation. A rename or a type change on either
// side fails here.
func TestDataEngineClaimSatisfiesProjectorFencePredicate(t *testing.T) {
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

	claims := inboxClaims(client)
	now := time.Now().UTC()
	var found bson.M
	if err := claims.FindOne(context.Background(), fence.Predicate(now), &found); err != nil {
		stored, _ := claims.Lookup(fence.DocumentID)
		t.Fatalf("the projector's predicate does not match the claim this package writes: %v\npredicate=%v\nstored=%v",
			err, fence.Predicate(now), stored)
	}

	// Every field of the predicate must be load-bearing: if any deviation
	// still matched, the fence would not actually be fencing anything.
	for name, mutate := range map[string]func(*coredata.LeaseFence){
		"other owner":  func(f *coredata.LeaseFence) { f.Owner = "worker-2" },
		"other token":  func(f *coredata.LeaseFence) { f.Token++ },
		"other digest": func(f *coredata.LeaseFence) { f.Digest = bytes.Repeat([]byte{9}, len(f.Digest)) },
		"other document": func(f *coredata.LeaseFence) {
			f.DocumentID = dataEngineStepNamespace + "/other-command"
		},
	} {
		deviated := fence
		mutate(&deviated)
		if err := claims.FindOne(context.Background(), deviated.Predicate(now), &found); !errors.Is(err, fmongo.ErrNotFound) {
			t.Fatalf("%s: predicate still matched (err=%v)", name, err)
		}
	}

	// An expired lease must stop matching without touching the document.
	if err := claims.FindOne(context.Background(), fence.Predicate(now.Add(2*time.Minute)), &found); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("expired lease still matched: %v", err)
	}

	// A completed claim must stop matching: the status value is shared with
	// the projector precisely so this transition is respected.
	completion := coresaga.Completion{
		CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: command.SagaID,
		Success: true, CompletedAt: now,
	}
	if err := inbox.markCompleted(context.Background(), command.ID, completion); err != nil {
		t.Fatal(err)
	}
	if err := claims.FindOne(context.Background(), fence.Predicate(now), &found); !errors.Is(err, fmongo.ErrNotFound) {
		t.Fatalf("completed claim still matched the pending predicate: %v", err)
	}
}
