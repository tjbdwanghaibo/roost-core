package saga

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/nest"
)

type memoryStore struct {
	mu       sync.Mutex
	records  map[string]Record
	keys     map[string]string
	outbox   map[string]OutboxRecord
	receipts map[string]Completion
	closed   map[string]string
}

func newMemoryStore() *memoryStore {
	return &memoryStore{records: map[string]Record{}, keys: map[string]string{}, outbox: map[string]OutboxRecord{}, receipts: map[string]Completion{}, closed: map[string]string{}}
}
func (s *memoryStore) Create(_ context.Context, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.Type + "/" + r.BusinessKey
	if _, ok := s.keys[key]; ok {
		return ErrAlreadyExists
	}
	s.records[r.ID] = r.Clone()
	s.keys[key] = r.ID
	return nil
}
func (s *memoryStore) Get(_ context.Context, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r.Clone(), nil
}
func (s *memoryStore) GetByBusinessKey(_ context.Context, typ, key string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.keys[typ+"/"+key]
	if !ok {
		return Record{}, ErrNotFound
	}
	return s.records[id].Clone(), nil
}
func (s *memoryStore) List(_ context.Context, q Query) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, min(q.Limit, len(s.records)))
	for _, r := range s.records {
		if q.Type != "" && r.Type != q.Type {
			continue
		}
		if q.DefinitionVersion != 0 && r.DefinitionVersion != q.DefinitionVersion {
			continue
		}
		if len(q.Statuses) > 0 {
			matched := false
			for _, status := range q.Statuses {
				matched = matched || r.Status == status
			}
			if !matched {
				continue
			}
		}
		if !q.UpdatedBefore.IsZero() && !r.UpdatedAt.Before(q.UpdatedBefore) {
			continue
		}
		out = append(out, r.Clone())
		if len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}
func (s *memoryStore) CompletionRecorded(_ context.Context, completion Completion) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, ok := s.receipts[completion.CommandID]
	if !ok {
		if sagaID, closed := s.closed[completion.IdempotencyKey]; closed {
			if sagaID != completion.SagaID {
				return false, ErrIdentityConflict
			}
			return true, nil
		}
		return false, nil
	}
	if prior.Success != completion.Success || prior.Retryable != completion.Retryable || prior.Error != completion.Error || string(prior.Data) != string(completion.Data) {
		return false, ErrIdentityConflict
	}
	return true, nil
}
func (s *memoryStore) ClaimDue(_ context.Context, q ClaimRequest) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, q.Limit)
	for id, r := range s.records {
		if len(out) >= q.Limit {
			break
		}
		if r.Status.Terminal() || r.NextRunAt.After(q.Now) || (!r.Lease.Until.IsZero() && r.Lease.Until.After(q.Now)) {
			continue
		}
		r.Lease = Lease{Owner: q.Owner, Token: r.Lease.Token + 1, Until: q.Now.Add(q.LeaseDuration)}
		s.records[id] = r
		out = append(out, r.Clone())
	}
	return out, nil
}
func (s *memoryStore) Apply(_ context.Context, q ApplyRequest) (ApplyOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.records[q.After.ID]
	if !ok {
		return 0, ErrNotFound
	}
	if current.Version != q.ExpectedVersion {
		return 0, ErrConflict
	}
	if q.ExpectedLease.Owner != "" && (current.Lease.Owner != q.ExpectedLease.Owner || current.Lease.Token != q.ExpectedLease.Token) {
		return 0, ErrConflict
	}
	if q.Receipt != nil {
		if prior, ok := s.receipts[q.Receipt.CommandID]; ok {
			if prior.Success != q.Receipt.Success || prior.Retryable != q.Receipt.Retryable || prior.Error != q.Receipt.Error || string(prior.Data) != string(q.Receipt.Data) {
				return 0, ErrIdentityConflict
			}
			return ApplyDuplicate, nil
		}
	}
	s.records[q.After.ID] = q.After.Clone()
	if q.Outbox != nil {
		for id, pending := range s.outbox {
			if pending.Command.IdempotencyKey == q.Outbox.Command.IdempotencyKey {
				delete(s.outbox, id)
			}
		}
		s.outbox[q.Outbox.Command.ID] = q.Outbox.Clone()
	}
	if q.Receipt != nil {
		s.receipts[q.Receipt.CommandID] = *q.Receipt
	}
	if q.CloseOperation != "" {
		s.closed[q.CloseOperation] = q.After.ID
		for id, item := range s.outbox {
			if item.Command.IdempotencyKey == q.CloseOperation {
				delete(s.outbox, id)
			}
		}
	}
	return ApplyApplied, nil
}
func (s *memoryStore) ClaimOutbox(_ context.Context, q ClaimRequest) ([]OutboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]OutboxRecord, 0, q.Limit)
	for id, r := range s.outbox {
		if len(out) >= q.Limit {
			break
		}
		if r.NextAttemptAt.After(q.Now) || (!r.Lease.Until.IsZero() && r.Lease.Until.After(q.Now)) {
			continue
		}
		r.Lease = Lease{Owner: q.Owner, Token: r.Lease.Token + 1, Until: q.Now.Add(q.LeaseDuration)}
		s.outbox[id] = r
		out = append(out, r.Clone())
	}
	return out, nil
}
func (s *memoryStore) AckOutbox(_ context.Context, id string, l Lease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.outbox[id]
	if !ok || r.Lease.Owner != l.Owner || r.Lease.Token != l.Token {
		return ErrConflict
	}
	delete(s.outbox, id)
	return nil
}
func (s *memoryStore) NackOutbox(_ context.Context, id string, l Lease, next time.Time, last string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.outbox[id]
	if !ok || r.Lease.Owner != l.Owner || r.Lease.Token != l.Token {
		return ErrConflict
	}
	r.Attempt++
	r.NextAttemptAt = next
	r.Lease = Lease{}
	s.outbox[id] = r
	return nil
}

func testDefinition() Definition {
	return Definition{Type: "rally", Version: 1, Steps: []Step{{Name: "reserve", ForwardTopic: "reserve", CompensateTopic: "reserve.cancel", Timeout: time.Second, MaxAttempts: 2, BackoffMin: time.Millisecond, BackoffMax: 10 * time.Millisecond}, {Name: "march", ForwardTopic: "march", CompensateTopic: "march.cancel", Timeout: time.Second, MaxAttempts: 2, BackoffMin: time.Millisecond, BackoffMax: 10 * time.Millisecond}}}
}

func newTestEngine(t *testing.T) (*Engine, *memoryStore, <-chan Command, context.CancelFunc) {
	t.Helper()
	store := newMemoryStore()
	commands := make(chan Command, 16)
	options := DefaultOptions()
	options.CoordinatorWorkers = 1
	options.PublisherWorkers = 1
	options.PollInterval = time.Millisecond
	options.LeaseDuration = time.Second
	options.StoreTimeout = 100 * time.Millisecond
	options.PublishTimeout = 100 * time.Millisecond
	engine, err := NewEngine(store, PublishFunc(func(_ context.Context, c Command) error { commands <- c; return nil }), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = engine.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		_ = engine.Stop(stopCtx)
	})
	return engine, store, commands, cancel
}

func waitCommand(t *testing.T, commands <-chan Command) Command {
	t.Helper()
	select {
	case command := <-commands:
		return command
	case <-time.After(2 * time.Second):
		t.Fatal("command timeout")
		return Command{}
	}
}

func TestEngineCompletesForwardSteps(t *testing.T) {
	engine, _, commands, _ := newTestEngine(t)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("initial")})
	if err != nil {
		t.Fatal(err)
	}
	first := waitCommand(t, commands)
	if first.Step != 0 || first.Phase != PhaseForward {
		t.Fatalf("unexpected first command: %+v", first)
	}
	if _, err = engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true, Data: []byte("reserved")}); err != nil {
		t.Fatal(err)
	}
	second := waitCommand(t, commands)
	if second.Step != 1 || string(second.Payload) != "reserved" {
		t.Fatalf("unexpected second command: %+v", second)
	}
	final, err := engine.Complete(context.Background(), Completion{CommandID: second.ID, IdempotencyKey: second.IdempotencyKey, SagaID: record.ID, Success: true, Data: []byte("done")})
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusCompleted || final.CompletedSteps != 2 || string(final.Data) != "done" {
		t.Fatalf("unexpected final record: %+v", final)
	}
}

func TestEngineCompensatesInReverseOrder(t *testing.T) {
	engine, _, commands, _ := newTestEngine(t)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-2"})
	if err != nil {
		t.Fatal(err)
	}
	first := waitCommand(t, commands)
	_, err = engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	second := waitCommand(t, commands)
	_, err = engine.Complete(context.Background(), Completion{CommandID: second.ID, IdempotencyKey: second.IdempotencyKey, SagaID: record.ID, Success: false, Retryable: false, Error: "world rejected"})
	if err != nil {
		t.Fatal(err)
	}
	compensate := waitCommand(t, commands)
	if compensate.Step != 0 || compensate.Phase != PhaseCompensate || compensate.Topic != "reserve.cancel" {
		t.Fatalf("unexpected compensation: %+v", compensate)
	}
	final, err := engine.Complete(context.Background(), Completion{CommandID: compensate.ID, IdempotencyKey: compensate.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusCompensated {
		t.Fatalf("status=%v", final.Status)
	}
}

func TestStartSagaIsIdempotentByBusinessKey(t *testing.T) {
	engine, _, _, _ := newTestEngine(t)
	one, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	two, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if one.ID != two.ID {
		t.Fatalf("ids differ: %s %s", one.ID, two.ID)
	}
}

func TestStartSagaRejectsBusinessKeyWithDifferentIntent(t *testing.T) {
	engine, _, _, _ := newTestEngine(t)
	if _, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "identity", Data: []byte("one")}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "identity", Data: []byte("two")}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("err=%v", err)
	}
}

func TestDefinitionVersionsRunSideBySideAndArePartOfIdentity(t *testing.T) {
	engine, _, commands, _ := newTestEngine(t)
	v2 := testDefinition()
	v2.Version = 2
	v2.Steps[0].ForwardTopic = "reserve.v2"
	if err := engine.Register(v2); err != nil {
		t.Fatal(err)
	}
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 2, BusinessKey: "versioned"})
	if err != nil {
		t.Fatal(err)
	}
	command := waitCommand(t, commands)
	if record.DefinitionVersion != 2 || command.DefinitionVersion != 2 || command.Topic != "reserve.v2" {
		t.Fatalf("record=%+v command=%+v", record, command)
	}
	if _, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "versioned"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("definition version identity err=%v", err)
	}
}

func TestMissingDefinitionVersionIsFencedForManualRepair(t *testing.T) {
	engine, store, _, _ := newTestEngine(t)
	now := time.Now().UTC()
	record := Record{ID: "missing-version", Type: "rally", DefinitionVersion: 99, BusinessKey: "missing-version", Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	engine.signal(engine.dueKick)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.Get(context.Background(), record.ID)
		if err == nil && current.Status == StatusManualRequired {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("missing definition was not fenced")
}

func TestStartSagaIdentityUsesStorageSafeDeadlinePrecision(t *testing.T) {
	engine, _, _, _ := newTestEngine(t)
	deadline := time.Date(2026, 8, 24, 20, 0, 0, 987654321, time.FixedZone("test", 8*60*60))
	one, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "deadline-precision", DeadlineAt: deadline})
	if err != nil {
		t.Fatal(err)
	}
	two, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "deadline-precision", DeadlineAt: deadline})
	if err != nil || one.ID != two.ID {
		t.Fatalf("redelivery was not idempotent: one=%+v two=%+v err=%v", one, two, err)
	}
	if got := one.DeadlineAt.Nanosecond(); got != 987000000 {
		t.Fatalf("deadline precision=%d", got)
	}
}

func TestNestStartEffectHasStableIdentity(t *testing.T) {
	one, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("one")})
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("two")})
	if err != nil {
		t.Fatal(err)
	}
	if one.ID == two.ID || one.Topic != StartEffectTopic {
		t.Fatalf("different intents must have different effect identities: %+v %+v", one, two)
	}
	replay, err := NewStartEffect(StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "r-1", Data: []byte("one"), Now: time.Now()})
	if err != nil || replay.ID != one.ID {
		t.Fatalf("exact intent identity is not stable: %+v err=%v", replay, err)
	}
	decoded, err := DecodeStartEffect(one.Payload)
	if err != nil || decoded.BusinessKey != "r-1" {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
}

func TestNestStartEffectRejectsUnknownWireVersion(t *testing.T) {
	if _, err := DecodeStartEffect([]byte(`{"version":2,"start":{"Type":"rally","BusinessKey":"r-1"}}`)); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("err=%v", err)
	}
}

func TestNestCompletionEffectHasStableCommandIdentityAndWireVersion(t *testing.T) {
	completion := Completion{CommandID: "command-1", IdempotencyKey: "operation-1", SagaID: "saga-1", Success: true, Data: []byte("done")}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		t.Fatal(err)
	}
	if effect.ID != "saga-completion:command-1" || effect.Topic != "saga.result.saga-1" {
		t.Fatalf("effect=%+v", effect)
	}
	decoded, err := DecodeCompletionEffect(effect.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.CommandID != completion.CommandID || string(decoded.Data) != "done" {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestBindCommandRequiresActiveNestTransactionAndCallerTTL(t *testing.T) {
	now := time.Now().UTC()
	command := Command{ID: "command-1", IdempotencyKey: "operation-1", SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1, BusinessKey: "r-1", StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "rally.reserve", CreatedAt: now, DeadlineAt: now.Add(time.Minute)}
	if err := BindCommand(command, now.Add(time.Hour)); !errors.Is(err, nest.ErrTransactionClosed) {
		t.Fatalf("err=%v", err)
	}
	if err := BindCommand(command, time.Time{}); !errors.Is(err, nest.ErrTransactionClosed) {
		t.Fatalf("zero expiry err=%v", err)
	}
}

func TestLateDuplicateCompletionIsAcknowledgedAfterNextStep(t *testing.T) {
	engine, _, commands, _ := newTestEngine(t)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "late"})
	if err != nil {
		t.Fatal(err)
	}
	first := waitCommand(t, commands)
	completion := Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true, Data: []byte("reserved")}
	if _, err = engine.Complete(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	_ = waitCommand(t, commands)
	if _, err = engine.Complete(context.Background(), completion); err != nil {
		t.Fatalf("late duplicate was not acknowledged: %v", err)
	}
	obsolete := completion
	obsolete.CommandID = "unseen-retry-delivery"
	if _, err = engine.Complete(context.Background(), obsolete); err != nil {
		t.Fatalf("closed operation result was not acknowledged: %v", err)
	}
}

func TestRetryableCompletionCreatesNewDeliveryForSameOperation(t *testing.T) {
	engine, _, commands, _ := newTestEngine(t)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	first := waitCommand(t, commands)
	if _, err = engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: false, Retryable: true, Error: "busy"}); err != nil {
		t.Fatal(err)
	}
	second := waitCommand(t, commands)
	if second.ID == first.ID || second.IdempotencyKey != first.IdempotencyKey || second.Attempt != 2 {
		t.Fatalf("unexpected retry: first=%+v second=%+v", first, second)
	}
	after, err := engine.Complete(context.Background(), Completion{CommandID: second.ID, IdempotencyKey: second.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.Step != 1 {
		t.Fatalf("retry did not advance: %+v", after)
	}
}

func TestCompletionCannotInjectRemoteClockIntoState(t *testing.T) {
	engine, _, commands, _ := newTestEngine(t)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "clock"})
	if err != nil {
		t.Fatal(err)
	}
	command := waitCommand(t, commands)
	after, err := engine.Complete(context.Background(), Completion{CommandID: command.ID, IdempotencyKey: command.IdempotencyKey, SagaID: record.ID, Success: true, CompletedAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("remote clock entered coordinator state: %s", after.UpdatedAt)
	}
}

func TestLateSuccessfulStepIsIncludedInCompensation(t *testing.T) {
	engine, _, commands, _ := newTestEngine(t)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "deadline", DeadlineAt: time.Now().Add(50 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	first := waitCommand(t, commands)
	time.Sleep(60 * time.Millisecond)
	after, err := engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusCompensating || after.CompletedSteps != 1 || after.Step != 0 {
		t.Fatalf("late success escaped compensation: %+v", after)
	}
	command := waitCommand(t, commands)
	if command.Phase != PhaseCompensate {
		t.Fatalf("command=%+v", command)
	}
}

func TestResumeRequiresExplicitExpiredDeadlinePolicy(t *testing.T) {
	engine, store, _, _ := newTestEngine(t)
	now := time.Now().UTC()
	record := Record{ID: "expired", Type: "rally", DefinitionVersion: 1, BusinessKey: "expired", Status: StatusFailed, Phase: PhaseForward, Step: 0, Version: 1, DeadlineAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute)}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Resume(context.Background(), ResumeRequest{ID: record.ID, Now: now}); !errors.Is(err, ErrDeadlineExpired) {
		t.Fatalf("err=%v", err)
	}
	resumed, err := engine.Resume(context.Background(), ResumeRequest{ID: record.ID, Now: now, ClearDeadline: true})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != StatusPending || !resumed.DeadlineAt.IsZero() {
		t.Fatalf("resumed=%+v", resumed)
	}
}

func TestResumeMintsCommandIDsDisjointFromPreviousLife(t *testing.T) {
	// Regression: Resume resets Attempt, so the first redispatch used to mint
	// the same CommandID as the first attempt of the failed life. A stale
	// completion receipt under that ID then blocked the resumed operation as
	// a false duplicate. Incarnation keeps every life's identifiers disjoint.
	engine, _, commands, _ := newTestEngine(t)
	record, err := engine.StartSaga(context.Background(), StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "resume-ids"})
	if err != nil {
		t.Fatal(err)
	}
	first := waitCommand(t, commands)
	failed, err := engine.Complete(context.Background(), Completion{CommandID: first.ID, IdempotencyKey: first.IdempotencyKey, SagaID: record.ID, Success: false, Error: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != StatusFailed {
		t.Fatalf("expected failed record, got %+v", failed)
	}

	resumed, err := engine.Resume(context.Background(), ResumeRequest{ID: record.ID})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Incarnation != 1 {
		t.Fatalf("expected incarnation 1, got %d", resumed.Incarnation)
	}
	redispatched := waitCommand(t, commands)
	if redispatched.ID == first.ID {
		t.Fatalf("resumed dispatch reused CommandID %q from the failed life", first.ID)
	}
	if redispatched.IdempotencyKey != first.IdempotencyKey {
		t.Fatalf("operation identity must survive resume: %q != %q", redispatched.IdempotencyKey, first.IdempotencyKey)
	}
	// The resumed completion must apply as fresh progress, not a duplicate.
	after, err := engine.Complete(context.Background(), Completion{CommandID: redispatched.ID, IdempotencyKey: redispatched.IdempotencyKey, SagaID: record.ID, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.CompletedSteps != 1 {
		t.Fatalf("resumed completion did not advance the saga: %+v", after)
	}
}

func TestDefinitionValidation(t *testing.T) {
	tests := []Definition{{}, {Type: "x", Version: 1, Steps: []Step{{Name: "x"}}}, {Type: "x", Version: 1, Steps: []Step{{Name: "x", ForwardTopic: "x", Timeout: time.Second, MaxAttempts: 1, BackoffMin: time.Second, BackoffMax: time.Second}, {Name: "x", ForwardTopic: "y", Timeout: time.Second, MaxAttempts: 1, BackoffMin: time.Second, BackoffMax: time.Second}}}}
	for i, test := range tests {
		if err := test.Validate(); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestDefinitionRejectsUnsafeTransportSubject(t *testing.T) {
	definition := testDefinition()
	definition.Steps[0].ForwardTopic = "reserve.>"
	if err := definition.Validate(); err == nil {
		t.Fatal("wildcard topic accepted")
	}
	definition = testDefinition()
	definition.Steps[0].ForwardTopic = "reserve..troops"
	if err := definition.Validate(); err == nil {
		t.Fatal("empty subject token accepted")
	}
}

func TestEngineRejectsPublisherBatchWhichCanOutliveLease(t *testing.T) {
	options := DefaultOptions()
	options.PublisherBatch = 5
	if _, err := NewEngine(newMemoryStore(), PublishFunc(func(context.Context, Command) error { return nil }), options); err == nil {
		t.Fatal("unsafe publisher batch accepted")
	}
}

func TestStartSagaRejectsUnsafeExplicitID(t *testing.T) {
	engine, _, _, _ := newTestEngine(t)
	if _, err := engine.StartSaga(context.Background(), StartRequest{ID: "bad.>", Type: "rally", DefinitionVersion: 1, BusinessKey: "unsafe-id"}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("err=%v", err)
	}
}

func TestStoreContractClosesOperationAndRemovesStaleOutbox(t *testing.T) {
	store := newMemoryStore()
	now := time.Now()
	record := Record{ID: "s", Type: "t", DefinitionVersion: 1, BusinessKey: "b", Status: StatusWaiting, Phase: PhaseForward, Step: 0, Attempt: 2, Version: 1, OperationKey: "op", CommandID: "op:2", NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	store.outbox["old"] = OutboxRecord{Command: Command{ID: "old", IdempotencyKey: "op"}}
	store.outbox["other"] = OutboxRecord{Command: Command{ID: "other", IdempotencyKey: "other"}}
	after := record
	after.Version = 2
	after.Status = StatusCompleted
	after.OperationKey = ""
	after.CommandID = ""
	after.NextRunAt = time.Time{}
	if _, err := store.Apply(context.Background(), ApplyRequest{ExpectedVersion: 1, After: after, CloseOperation: "op"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.outbox["old"]; ok {
		t.Fatal("stale outbox survived operation close")
	}
	if _, ok := store.outbox["other"]; !ok {
		t.Fatal("unrelated outbox was removed")
	}
}

func TestStoreContractNewAttemptSupersedesQueuedAttempt(t *testing.T) {
	store := newMemoryStore()
	now := time.Now().UTC()
	record := Record{ID: "s", Type: "t", DefinitionVersion: 1, BusinessKey: "b", Status: StatusPending, Phase: PhaseForward, Step: 0, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	store.outbox["op:1"] = OutboxRecord{Command: Command{ID: "op:1", IdempotencyKey: "op"}}
	after := record
	after.Version++
	after.Status = StatusWaiting
	after.Attempt = 2
	after.OperationKey = "op"
	after.CommandID = "op:2"
	_, err := store.Apply(context.Background(), ApplyRequest{ExpectedVersion: record.Version, After: after, Outbox: &OutboxRecord{Command: Command{ID: "op:2", IdempotencyKey: "op"}, NextAttemptAt: now, CreatedAt: now}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := store.outbox["op:1"]; exists {
		t.Fatal("superseded attempt remains queued")
	}
	if _, exists := store.outbox["op:2"]; !exists {
		t.Fatal("new attempt was not queued")
	}
}

func BenchmarkNewID(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if id := NewID(); len(id) != 32 {
			b.Fatal(id)
		}
	}
}
func BenchmarkMemoryStoreApply(b *testing.B) {
	store := newMemoryStore()
	now := time.Now()
	r := Record{ID: "id", Type: "t", DefinitionVersion: 1, BusinessKey: "k", Status: StatusPending, Phase: PhaseForward, Version: 1, CreatedAt: now, UpdatedAt: now, NextRunAt: now}
	if err := store.Create(context.Background(), r); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		current, _ := store.Get(context.Background(), "id")
		after := current
		after.Version++
		after.UpdatedAt = now
		if _, err := store.Apply(context.Background(), ApplyRequest{ExpectedVersion: current.Version, After: after}); err != nil {
			b.Fatal(fmt.Errorf("apply: %w", err))
		}
	}
}

var _ Store = (*memoryStore)(nil)
var _ = errors.Is
