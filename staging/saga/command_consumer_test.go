package saga

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
	"github.com/tjbdwanghaibo/roost-kit/mongo/mongotest"
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

func newInboxMongoFake() *mongotest.Client { return mongotest.NewClient() }
