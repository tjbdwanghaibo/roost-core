package saga

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// idleEngine is an engine whose coordinator loop is not running, so records
// stay exactly where the test put them.
func idleEngine(t *testing.T) (*Engine, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	return engine, store
}

func seedRecord(t *testing.T, store *memoryStore, id string, status Status, completed int) Record {
	t.Helper()
	now := time.Now().UTC()
	record := Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: "key-" + id, Status: status, Phase: PhaseForward, Step: completed, CompletedSteps: completed, Version: 3, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if status == StatusWaiting {
		record.OperationKey, record.CommandID, record.Attempt = "op", "cmd", 1
	}
	if status == StatusCompensating {
		record.Phase = PhaseCompensate
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	return record
}

// The engine's public entry points guard the state machine: only some
// statuses may resume or compensate, definitions must be known, and requests
// are bounded. Each refusal is pinned; the coordinator loop is not running so
// the refusal is the only thing that can happen.
func TestEngineRegisterAndStartRefuseUnknownOrDuplicateDefinitions(t *testing.T) {
	engine, _ := idleEngine(t)
	if err := engine.Register(testDefinition()); !errors.Is(err, ErrAlreadyExists) || !strings.Contains(err.Error(), "rally/1") {
		t.Fatalf("duplicate Register = %v", err)
	}
	if _, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 2, BusinessKey: "b"}); !errors.Is(err, ErrDefinitionMissing) || !strings.Contains(err.Error(), "rally/2") {
		t.Fatalf("unknown definition version = %v", err)
	}
	for name, req := range map[string]StartRequest{
		"blank business key":        {Type: "rally", DefinitionVersion: 1, BusinessKey: "   "},
		"business key too long":     {Type: "rally", DefinitionVersion: 1, BusinessKey: strings.Repeat("k", 513)},
		"data over MaxPayloadBytes": {Type: "rally", DefinitionVersion: 1, BusinessKey: "b", Data: make([]byte, DefaultOptions().MaxPayloadBytes+1)},
	} {
		if _, err := engine.StartSaga(context.Background(), req); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("%s = %v, want ErrInvalidRecord", name, err)
		}
	}
	if _, err := engine.List(context.Background(), Query{Limit: 1001}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("List limit 1001 = %v", err)
	}
	if _, err := engine.List(context.Background(), Query{Limit: 1000}); err != nil {
		t.Fatalf("List limit 1000 = %v", err)
	}
}

func TestEngineResumeRefusesEachIllegalRequest(t *testing.T) {
	engine, store := idleEngine(t)
	seedRecord(t, store, "pending", StatusPending, 0)
	seedRecord(t, store, "done", StatusCompleted, 2)
	failed := seedRecord(t, store, "failed", StatusFailed, 1)
	orphan := seedRecord(t, store, "orphan", StatusFailed, 0)
	orphan.DefinitionVersion = 9
	if _, err := store.Apply(context.Background(), ApplyRequest{ExpectedVersion: orphan.Version, After: func() Record { o := orphan; o.Version++; return o }()}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	cases := []struct {
		name    string
		request ResumeRequest
		want    error
		text    string
	}{
		{"id with wildcard", ResumeRequest{ID: "bad.>"}, ErrInvalidRecord, ""},
		{"clear-deadline together with a deadline", ResumeRequest{ID: "failed", ClearDeadline: true, DeadlineAt: now.Add(time.Hour)}, ErrInvalidRecord, ""},
		{"deadline already passed", ResumeRequest{ID: "failed", Now: now, DeadlineAt: now.Add(-time.Second)}, ErrDeadlineExpired, ""},
		{"pending cannot resume", ResumeRequest{ID: "pending"}, nil, "status 1 cannot resume"},
		{"completed cannot resume", ResumeRequest{ID: "done"}, nil, "status 4 cannot resume"},
		{"definition gone", ResumeRequest{ID: "orphan"}, ErrDefinitionMissing, "rally/9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := engine.Resume(ctx, tc.request)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Resume = %v, want %v %q", err, tc.want, tc.text)
			}
		})
	}
	resumed, err := engine.Resume(ctx, ResumeRequest{ID: "failed", Now: now})
	if err != nil {
		t.Fatalf("legal resume: %v", err)
	}
	if resumed.Incarnation != failed.Incarnation+1 || resumed.Status != StatusCompensating || resumed.Phase != PhaseCompensate {
		t.Fatalf("resumed failed saga with completed steps = %+v, want a new incarnation compensating", resumed)
	}
}

func TestEngineCompensateRefusesInFlightAndNothingToUndo(t *testing.T) {
	engine, store := idleEngine(t)
	seedRecord(t, store, "waiting", StatusWaiting, 1)
	seedRecord(t, store, "fresh", StatusPending, 0)
	already := seedRecord(t, store, "already", StatusCompensating, 1)
	ctx := context.Background()
	if _, err := engine.Compensate(ctx, "waiting", "operator", time.Time{}); err == nil || !strings.Contains(err.Error(), "cannot force compensation while a step result is in flight") {
		t.Fatalf("Compensate while waiting = %v", err)
	}
	if _, err := engine.Compensate(ctx, "fresh", "operator", time.Time{}); err == nil || !strings.Contains(err.Error(), "no completed steps to compensate") {
		t.Fatalf("Compensate with nothing done = %v", err)
	}
	got, err := engine.Compensate(ctx, "already", "operator", time.Time{})
	if err != nil || got.Version != already.Version {
		t.Fatalf("Compensate on an already compensating saga must be a no-op: %+v, %v", got, err)
	}
	if _, err := engine.Complete(ctx, Completion{CommandID: "c", IdempotencyKey: "k", SagaID: "waiting", Success: true, Data: make([]byte, DefaultOptions().MaxPayloadBytes+1)}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Complete with data over MaxPayloadBytes = %v", err)
	}
	if _, err := engine.Complete(ctx, Completion{CommandID: "c", IdempotencyKey: "k", SagaID: "waiting", Success: false}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Complete failing validation = %v", err)
	}
}
