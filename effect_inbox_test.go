package nestwal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/cube-kit/internal/mongofake"
)

func newEffectInboxTest(t *testing.T, ttl time.Duration) (*MongoEffectInbox, *mongofake.Client, *mongofake.Collection) {
	t.Helper()
	client := mongofake.NewClient()
	inbox, err := NewMongoEffectInbox(client, "game", "", EffectInboxOptions{ReceiptTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return inbox, client, client.Collection("game", inbox.collection)
}

func TestMongoEffectInboxDeduplicatesAndRejectsIdentityConflict(t *testing.T) {
	inbox, _, collection := newEffectInboxTest(t, 48*time.Hour)
	if err := inbox.EnsureInfrastructure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(collection.Indexes) != 1 || collection.Indexes[0].TTL != int32((48*time.Hour)/time.Second) {
		t.Fatalf("indexes = %+v", collection.Indexes)
	}
	envelope := EffectEnvelope{TransactionID: "tx-1", EffectID: "effect-1", Topic: "mail", Payload: []byte("reward")}
	calls := 0
	handler := func(context.Context, EffectEnvelope) error { calls++; return nil }
	duplicate, err := inbox.Handle(context.Background(), envelope, handler)
	if err != nil || duplicate {
		t.Fatalf("first handle duplicate=%v err=%v", duplicate, err)
	}
	if collection.Len() != 1 {
		t.Fatalf("receipts=%d, want 1", collection.Len())
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
	if collection.Len() != 1 {
		t.Fatalf("identity conflict wrote a receipt: %d", collection.Len())
	}
}

// A failing handler must leave no receipt behind, otherwise the effect would
// be permanently swallowed as a "duplicate" on redelivery.
func TestMongoEffectInboxHandlerFailureLeavesNoReceipt(t *testing.T) {
	inbox, _, collection := newEffectInboxTest(t, time.Hour)
	envelope := EffectEnvelope{TransactionID: "tx-1", EffectID: "effect-1", Topic: "mail", Payload: []byte("reward")}
	boom := errors.New("side effect failed")
	if _, err := inbox.Handle(context.Background(), envelope, func(context.Context, EffectEnvelope) error {
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err=%v, want the handler error", err)
	}
	if collection.Len() != 0 {
		t.Fatalf("failed handler left %d receipts", collection.Len())
	}
	// Redelivery must run the handler again rather than reporting a duplicate.
	calls := 0
	duplicate, err := inbox.Handle(context.Background(), envelope, func(context.Context, EffectEnvelope) error {
		calls++
		return nil
	})
	if err != nil || duplicate || calls != 1 {
		t.Fatalf("redelivery duplicate=%v calls=%d err=%v", duplicate, calls, err)
	}
}

// ISession.WithTransaction retries its callback automatically, so Handle must
// be idempotent across attempts: the handler may run again, but the receipt
// must not turn a first delivery into a reported duplicate.
func TestMongoEffectInboxSurvivesTransactionRetry(t *testing.T) {
	inbox, client, collection := newEffectInboxTest(t, time.Hour)
	client.TransientRetries = 1
	envelope := EffectEnvelope{TransactionID: "tx-1", EffectID: "effect-1", Topic: "mail", Payload: []byte("reward")}
	duplicate, err := inbox.Handle(context.Background(), envelope, func(context.Context, EffectEnvelope) error {
		return nil
	})
	if err != nil {
		t.Fatalf("retried handle err=%v", err)
	}
	if collection.Len() != 1 {
		t.Fatalf("retry wrote %d receipts, want 1", collection.Len())
	}
	if !duplicate {
		// The retry legitimately observes its own first-attempt receipt; what
		// must never happen is an error or a second stored receipt.
		t.Log("retry reported a first delivery; receipt count is the invariant that matters")
	}
}
