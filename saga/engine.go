package saga

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	Owner              string
	CoordinatorWorkers int
	PublisherWorkers   int
	CoordinatorBatch   int
	PublisherBatch     int
	LeaseDuration      time.Duration
	StoreTimeout       time.Duration
	PollInterval       time.Duration
	PublishTimeout     time.Duration
	PublishBackoffMin  time.Duration
	PublishBackoffMax  time.Duration
	MaxPayloadBytes    int
}

func DefaultOptions() Options {
	return Options{
		Owner: "saga-" + NewID(), CoordinatorWorkers: 4, PublisherWorkers: 4,
		CoordinatorBatch: 3, PublisherBatch: 1, LeaseDuration: 15 * time.Second, StoreTimeout: 3 * time.Second, PollInterval: 100 * time.Millisecond,
		PublishTimeout: 3 * time.Second, PublishBackoffMin: 50 * time.Millisecond, PublishBackoffMax: 5 * time.Second,
		MaxPayloadBytes: 64 << 10,
	}
}

type StartRequest struct {
	ID                string    `json:"id,omitempty"`
	Type              string    `json:"type"`
	BusinessKey       string    `json:"business_key"`
	DefinitionVersion uint32    `json:"definition_version"`
	Data              []byte    `json:"data,omitempty"`
	DeadlineAt        time.Time `json:"deadline_at,omitempty"`
	Now               time.Time `json:"-"`
}

type ResumeRequest struct {
	ID            string
	Now           time.Time
	DeadlineAt    time.Time
	ClearDeadline bool
}

type Stats struct {
	Started, Dispatched, Completed, Compensated, Failed, ManualRequired   uint64
	Conflicts, Duplicates, PublishFailures, StoreFailures, WorkerFailures uint64
}

type Engine struct {
	store     Store
	publisher Publisher
	opts      Options

	mu          sync.RWMutex
	definitions map[definitionKey]Definition
	runMu       sync.Mutex
	running     bool
	cancel      context.CancelFunc
	done        chan struct{}
	dueKick     chan struct{}
	outboxKick  chan struct{}

	started, dispatched, completed, compensated, failed, manualRequired atomic.Uint64
	conflicts, duplicates, publishFailures                              atomic.Uint64
	storeFailures, workerFailures                                       atomic.Uint64
}

func NewEngine(store Store, publisher Publisher, options Options) (*Engine, error) {
	if store == nil || publisher == nil {
		return nil, fmt.Errorf("saga: store and publisher are required")
	}
	defaults := DefaultOptions()
	if strings.TrimSpace(options.Owner) == "" {
		options.Owner = defaults.Owner
	}
	if options.CoordinatorWorkers <= 0 {
		options.CoordinatorWorkers = defaults.CoordinatorWorkers
	}
	if options.PublisherWorkers <= 0 {
		options.PublisherWorkers = defaults.PublisherWorkers
	}
	if options.CoordinatorBatch <= 0 {
		options.CoordinatorBatch = defaults.CoordinatorBatch
	}
	if options.PublisherBatch <= 0 {
		options.PublisherBatch = defaults.PublisherBatch
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = defaults.LeaseDuration
	}
	if options.StoreTimeout <= 0 {
		options.StoreTimeout = defaults.StoreTimeout
	}
	if options.PollInterval <= 0 {
		options.PollInterval = defaults.PollInterval
	}
	if options.PublishTimeout <= 0 {
		options.PublishTimeout = defaults.PublishTimeout
	}
	if options.PublishBackoffMin <= 0 {
		options.PublishBackoffMin = defaults.PublishBackoffMin
	}
	if options.PublishBackoffMax < options.PublishBackoffMin {
		options.PublishBackoffMax = defaults.PublishBackoffMax
	}
	if options.MaxPayloadBytes <= 0 {
		options.MaxPayloadBytes = defaults.MaxPayloadBytes
	}
	if options.StoreTimeout >= options.LeaseDuration {
		return nil, fmt.Errorf("saga: unsafe engine limits")
	}
	leaseAfterClaim := options.LeaseDuration - options.StoreTimeout
	coordinatorBudget := leaseAfterClaim / time.Duration(options.CoordinatorBatch)
	publisherBudget := leaseAfterClaim / time.Duration(options.PublisherBatch)
	unsafePublisherBudget := options.PublishTimeout >= publisherBudget || options.StoreTimeout >= publisherBudget-options.PublishTimeout
	if len(options.Owner) > 256 || options.CoordinatorWorkers > 1024 || options.PublisherWorkers > 1024 || options.CoordinatorBatch > 4096 || options.PublisherBatch > 4096 || options.MaxPayloadBytes > 4<<20 || options.LeaseDuration <= options.PublishTimeout || options.StoreTimeout >= coordinatorBudget || unsafePublisherBudget {
		return nil, fmt.Errorf("saga: unsafe engine limits")
	}
	return &Engine{store: store, publisher: publisher, opts: options, definitions: make(map[definitionKey]Definition), dueKick: make(chan struct{}, 1), outboxKick: make(chan struct{}, 1)}, nil
}

type definitionKey struct {
	typeName string
	version  uint32
}

func (e *Engine) Register(definition Definition) error {
	if err := definition.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	key := definitionKey{typeName: definition.Type, version: definition.Version}
	if _, exists := e.definitions[key]; exists {
		return fmt.Errorf("%w: %s/%d", ErrAlreadyExists, definition.Type, definition.Version)
	}
	definition.Steps = append([]Step(nil), definition.Steps...)
	e.definitions[key] = definition
	return nil
}

func (e *Engine) StartSaga(ctx context.Context, request StartRequest) (Record, error) {
	if _, ok := e.definition(request.Type, request.DefinitionVersion); !ok {
		return Record{}, fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, request.Type, request.DefinitionVersion)
	}
	request.BusinessKey = strings.TrimSpace(request.BusinessKey)
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	if request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > e.opts.MaxPayloadBytes {
		return Record{}, ErrInvalidRecord
	}
	now := request.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	id := strings.TrimSpace(request.ID)
	if id == "" {
		id = NewID()
	}
	record := Record{ID: id, Type: request.Type, DefinitionVersion: request.DefinitionVersion, BusinessKey: request.BusinessKey, Status: StatusPending, Phase: PhaseForward, Step: 0, Version: 1, Data: append([]byte(nil), request.Data...), NextRunAt: now, DeadlineAt: request.DeadlineAt, CreatedAt: now, UpdatedAt: now}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	if err := e.store.Create(ctx, record); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			existing, getErr := e.store.GetByBusinessKey(ctx, request.Type, request.BusinessKey)
			if getErr == nil && request.DefinitionVersion == existing.DefinitionVersion && (request.ID == "" || request.ID == existing.ID) && bytes.Equal(request.Data, existing.Data) && request.DeadlineAt.Equal(canonicalDeadline(existing.DeadlineAt)) {
				return existing, nil
			}
			if getErr == nil {
				return Record{}, ErrIdentityConflict
			}
		}
		return Record{}, err
	}
	e.started.Add(1)
	e.signal(e.dueKick)
	return record.Clone(), nil
}

func (e *Engine) Get(ctx context.Context, id string) (Record, error) { return e.store.Get(ctx, id) }
func (e *Engine) List(ctx context.Context, query Query) ([]Record, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > 1000 {
		return nil, ErrInvalidRecord
	}
	return e.store.List(ctx, query)
}

// Resume retries a terminal failure after an operator or automated repair has
// removed its cause. Completed and compensated Sagas cannot be resumed.
func (e *Engine) Resume(ctx context.Context, request ResumeRequest) (Record, error) {
	request.ID = strings.TrimSpace(request.ID)
	if !validSubjectToken(request.ID, 128) || (request.ClearDeadline && !request.DeadlineAt.IsZero()) {
		return Record{}, ErrInvalidRecord
	}
	now := request.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	deadline := canonicalDeadline(request.DeadlineAt)
	if !deadline.IsZero() && !deadline.After(now) {
		return Record{}, ErrDeadlineExpired
	}
	for attempts := 0; attempts < 8; attempts++ {
		record, err := e.store.Get(ctx, request.ID)
		if err != nil {
			return Record{}, err
		}
		if record.Status != StatusFailed && record.Status != StatusManualRequired {
			return Record{}, fmt.Errorf("saga: status %d cannot resume", record.Status)
		}
		if _, ok := e.definition(record.Type, record.DefinitionVersion); !ok {
			return Record{}, fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, record.Type, record.DefinitionVersion)
		}
		after := record.Clone()
		after.Version++
		after.UpdatedAt = now
		after.Attempt = 0
		// A new incarnation keeps future CommandIDs disjoint from every
		// receipt recorded before this resume; see Record.Incarnation.
		after.Incarnation++
		after.LastError = ""
		after.CommandID = ""
		after.OperationKey = ""
		after.NextRunAt = now
		if request.ClearDeadline {
			after.DeadlineAt = time.Time{}
		} else if !deadline.IsZero() {
			after.DeadlineAt = deadline
		} else if after.Phase == PhaseForward && !after.DeadlineAt.IsZero() && !now.Before(after.DeadlineAt) {
			return Record{}, ErrDeadlineExpired
		}
		clearLease(&after)
		if record.Phase == PhaseCompensate || record.CompletedSteps > 0 {
			after.Phase = PhaseCompensate
			after.Status = StatusCompensating
			after.Step = after.CompletedSteps - 1
		} else {
			after.Phase = PhaseForward
			after.Status = StatusPending
		}
		_, err = e.store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: after})
		if errors.Is(err, ErrConflict) {
			e.conflicts.Add(1)
			continue
		}
		if err != nil {
			return Record{}, err
		}
		e.signal(e.dueKick)
		return after, nil
	}
	return Record{}, ErrConflict
}

// Compensate requests semantic rollback of every completed step. It is safe to
// call repeatedly; already compensating or compensated records are returned.
func (e *Engine) Compensate(ctx context.Context, id, reason string, now time.Time) (Record, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	for attempts := 0; attempts < 8; attempts++ {
		record, err := e.store.Get(ctx, id)
		if err != nil {
			return Record{}, err
		}
		if record.Status == StatusCompensated || record.Status == StatusCompensating {
			return record, nil
		}
		if record.Status == StatusWaiting {
			return Record{}, fmt.Errorf("saga: cannot force compensation while a step result is in flight")
		}
		if record.CompletedSteps == 0 {
			return Record{}, fmt.Errorf("saga: no completed steps to compensate")
		}
		after := e.beginCompensation(record, reason, now)
		_, err = e.store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: after})
		if errors.Is(err, ErrConflict) {
			e.conflicts.Add(1)
			continue
		}
		if err != nil {
			return Record{}, err
		}
		e.signal(e.dueKick)
		return after, nil
	}
	return Record{}, ErrConflict
}

func (e *Engine) Complete(ctx context.Context, completion Completion) (Record, error) {
	if completion.Validate() != nil {
		return Record{}, ErrInvalidRecord
	}
	if len(completion.Data) > e.opts.MaxPayloadBytes {
		return Record{}, ErrInvalidRecord
	}
	// Coordinator time owns ordering, deadlines and receipt TTL. A remote step
	// clock must not move Saga state backwards or expire deduplication records.
	completion.CompletedAt = time.Now().UTC()
	for attempts := 0; attempts < 8; attempts++ {
		record, err := e.store.Get(ctx, completion.SagaID)
		if err != nil {
			return Record{}, err
		}
		if record.Status != StatusWaiting || record.OperationKey != completion.IdempotencyKey {
			recorded, receiptErr := e.store.CompletionRecorded(ctx, completion)
			if receiptErr != nil {
				return Record{}, receiptErr
			}
			if recorded {
				e.duplicates.Add(1)
				return record, nil
			}
			return Record{}, ErrNotWaiting
		}
		definition, ok := e.definition(record.Type, record.DefinitionVersion)
		if !ok {
			return Record{}, fmt.Errorf("%w: %s", ErrDefinitionMissing, record.Type)
		}
		after := e.applyCompletion(record, definition, completion)
		outcome, err := e.store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: after, Receipt: &completion, CloseOperation: closedOperation(record, after)})
		if errors.Is(err, ErrConflict) {
			e.conflicts.Add(1)
			continue
		}
		if err != nil {
			return Record{}, err
		}
		if outcome == ApplyDuplicate {
			e.duplicates.Add(1)
			return e.store.Get(ctx, record.ID)
		}
		e.countTerminal(after.Status)
		e.signal(e.dueKick)
		return after.Clone(), nil
	}
	return Record{}, ErrConflict
}

func (e *Engine) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.runMu.Lock()
	if e.running {
		e.runMu.Unlock()
		return fmt.Errorf("saga: engine already running")
	}
	ctx, cancel := context.WithCancel(ctx)
	e.running, e.cancel, e.done = true, cancel, make(chan struct{})
	done := e.done
	e.runMu.Unlock()
	defer func() { e.runMu.Lock(); e.running = false; close(done); e.runMu.Unlock() }()

	var wg sync.WaitGroup
	for i := 0; i < e.opts.CoordinatorWorkers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.coordinatorLoop(ctx) }()
	}
	for i := 0; i < e.opts.PublisherWorkers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.publisherLoop(ctx) }()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (e *Engine) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.runMu.Lock()
	cancel, done := e.cancel, e.done
	e.runMu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) Stats() Stats {
	return Stats{Started: e.started.Load(), Dispatched: e.dispatched.Load(), Completed: e.completed.Load(), Compensated: e.compensated.Load(), Failed: e.failed.Load(), ManualRequired: e.manualRequired.Load(), Conflicts: e.conflicts.Load(), Duplicates: e.duplicates.Load(), PublishFailures: e.publishFailures.Load(), StoreFailures: e.storeFailures.Load(), WorkerFailures: e.workerFailures.Load()}
}

func (e *Engine) coordinatorLoop(ctx context.Context) {
	for ctx.Err() == nil {
		now := time.Now().UTC()
		storeCtx, cancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
		records, err := e.store.ClaimDue(storeCtx, ClaimRequest{Owner: e.opts.Owner, Now: now, LeaseDuration: e.opts.LeaseDuration, Limit: e.opts.CoordinatorBatch})
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				e.storeFailures.Add(1)
				slog.Error("saga: claim due", "err", err)
			}
			e.wait(ctx, e.dueKick)
			continue
		}
		if len(records) == 0 {
			e.wait(ctx, e.dueKick)
			continue
		}
		for i := range records {
			storeCtx, cancel = context.WithTimeout(ctx, e.opts.StoreTimeout)
			err = e.processClaimed(storeCtx, records[i], time.Now().UTC())
			cancel()
			if err != nil {
				if errors.Is(err, ErrConflict) {
					e.conflicts.Add(1)
					continue
				}
				if errors.Is(err, context.Canceled) {
					continue
				}
				e.workerFailures.Add(1)
				slog.Error("saga: process", "id", records[i].ID, "err", err)
			}
		}
	}
}

func (e *Engine) publisherLoop(ctx context.Context) {
	for ctx.Err() == nil {
		now := time.Now().UTC()
		storeCtx, cancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
		items, err := e.store.ClaimOutbox(storeCtx, ClaimRequest{Owner: e.opts.Owner, Now: now, LeaseDuration: e.opts.LeaseDuration, Limit: e.opts.PublisherBatch})
		cancel()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				e.storeFailures.Add(1)
				slog.Error("saga: claim outbox", "err", err)
			}
			e.wait(ctx, e.outboxKick)
			continue
		}
		if len(items) == 0 {
			e.wait(ctx, e.outboxKick)
			continue
		}
		for i := range items {
			publishCtx, cancel := context.WithTimeout(ctx, e.opts.PublishTimeout)
			err := e.publisher.PublishSagaCommand(publishCtx, items[i].Command.Clone())
			cancel()
			if err != nil {
				if errors.Is(err, context.Canceled) && ctx.Err() != nil {
					continue
				}
				e.publishFailures.Add(1)
				next := now.Add(backoff(items[i].Command.ID, items[i].Attempt+1, e.opts.PublishBackoffMin, e.opts.PublishBackoffMax))
				storeCtx, storeCancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
				nackErr := e.store.NackOutbox(storeCtx, items[i].Command.ID, items[i].Lease, next, err.Error())
				storeCancel()
				if errors.Is(nackErr, ErrConflict) {
					e.conflicts.Add(1)
				} else if nackErr != nil && !errors.Is(nackErr, context.Canceled) {
					e.storeFailures.Add(1)
					slog.Error("saga: nack outbox", "id", items[i].Command.ID, "err", nackErr)
				}
				continue
			}
			storeCtx, storeCancel := context.WithTimeout(ctx, e.opts.StoreTimeout)
			ackErr := e.store.AckOutbox(storeCtx, items[i].Command.ID, items[i].Lease)
			storeCancel()
			if errors.Is(ackErr, ErrConflict) {
				e.conflicts.Add(1)
			} else if ackErr != nil && !errors.Is(ackErr, context.Canceled) {
				e.storeFailures.Add(1)
				slog.Error("saga: ack outbox", "id", items[i].Command.ID, "err", ackErr)
			}
		}
	}
}

func (e *Engine) processClaimed(ctx context.Context, record Record, now time.Time) error {
	definition, ok := e.definition(record.Type, record.DefinitionVersion)
	if !ok {
		after := record.Clone()
		after.Status = StatusManualRequired
		after.LastError = fmt.Sprintf("definition not registered: %s/%d", record.Type, record.DefinitionVersion)
		after.Version++
		after.UpdatedAt = now
		after.NextRunAt = time.Time{}
		after.CommandID = ""
		after.OperationKey = ""
		clearLease(&after)
		_, err := e.store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, ExpectedLease: record.Lease, After: after, CloseOperation: record.OperationKey})
		if err == nil {
			e.countTerminal(after.Status)
		}
		return err
	}
	if !record.DeadlineAt.IsZero() && !now.Before(record.DeadlineAt) && record.Phase == PhaseForward {
		after := e.beginCompensation(record, "saga deadline exceeded", now)
		_, err := e.store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, ExpectedLease: record.Lease, After: after, CloseOperation: closedOperation(record, after)})
		if err == nil {
			e.countTerminal(after.Status)
			e.signal(e.dueKick)
		}
		return err
	}
	if record.Status == StatusWaiting {
		after := e.retryOrCompensate(record, definition, "step result timeout", now)
		_, err := e.store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, ExpectedLease: record.Lease, After: after, CloseOperation: closedOperation(record, after)})
		if err == nil {
			e.countTerminal(after.Status)
			e.signal(e.dueKick)
		}
		return err
	}
	step, ok := stepFor(record, definition)
	if !ok {
		after := record.Clone()
		after.Status = StatusManualRequired
		after.LastError = "invalid saga step"
		after.Version++
		after.UpdatedAt = now
		clearLease(&after)
		_, err := e.store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, ExpectedLease: record.Lease, After: after})
		if err == nil {
			e.failed.Add(1)
			e.manualRequired.Add(1)
		}
		return err
	}
	after := record.Clone()
	after.Status = StatusWaiting
	after.Attempt++
	after.Version++
	after.UpdatedAt = now
	after.NextRunAt = now.Add(step.Timeout)
	after.OperationKey = operationKey(record.ID, record.Phase, record.Step)
	after.CommandID = commandID(after.OperationKey, after.Incarnation, after.Attempt)
	clearLease(&after)
	topic := step.ForwardTopic
	if record.Phase == PhaseCompensate {
		topic = step.CompensateTopic
		if topic == "" {
			topic = step.ForwardTopic + ".compensate"
		}
	}
	command := Command{ID: after.CommandID, IdempotencyKey: after.OperationKey, SagaID: record.ID, SagaType: record.Type, DefinitionVersion: record.DefinitionVersion, BusinessKey: record.BusinessKey, Step: record.Step, StepName: step.Name, Phase: record.Phase, Attempt: after.Attempt, Topic: topic, Payload: append([]byte(nil), record.Data...), DeadlineAt: after.NextRunAt, CreatedAt: now}
	outbox := &OutboxRecord{Command: command, NextAttemptAt: now, CreatedAt: now}
	_, err := e.store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, ExpectedLease: record.Lease, After: after, Outbox: outbox})
	if err == nil {
		e.dispatched.Add(1)
		e.signal(e.outboxKick)
	}
	return err
}

func (e *Engine) applyCompletion(record Record, definition Definition, result Completion) Record {
	now := result.CompletedAt
	after := record.Clone()
	after.Version++
	after.UpdatedAt = now
	after.CommandID = ""
	after.OperationKey = ""
	clearLease(&after)
	if result.Success {
		if result.Data != nil {
			after.Data = append(after.Data[:0], result.Data...)
		}
		after.Attempt = 0
		after.LastError = ""
		if record.Phase == PhaseForward {
			after.CompletedSteps = record.Step + 1
			after.Step++
			if !record.DeadlineAt.IsZero() && !now.Before(record.DeadlineAt) {
				return e.beginCompensation(after, "saga deadline exceeded after step completion", now)
			}
			if after.Step >= len(definition.Steps) {
				after.Status = StatusCompleted
				after.NextRunAt = time.Time{}
			} else {
				after.Status = StatusPending
				after.NextRunAt = now
			}
		} else {
			after.CompletedSteps--
			if after.CompletedSteps <= 0 {
				after.CompletedSteps = 0
				after.Status = StatusCompensated
				after.NextRunAt = time.Time{}
			} else {
				after.Step = after.CompletedSteps - 1
				after.Status = StatusCompensating
				after.NextRunAt = now
			}
		}
		return after
	}
	if !result.Retryable {
		if after.Phase == PhaseCompensate {
			after.Status = StatusManualRequired
			after.NextRunAt = time.Time{}
			return after
		}
		return e.beginCompensation(after, result.Error, now)
	}
	return e.retryOrCompensate(after, definition, result.Error, now)
}

func (e *Engine) retryOrCompensate(record Record, definition Definition, reason string, now time.Time) Record {
	after := record.Clone()
	after.Version = max(after.Version, record.Version+1)
	after.UpdatedAt = now
	after.LastError = reason
	after.CommandID = ""
	after.OperationKey = ""
	clearLease(&after)
	step, ok := stepFor(record, definition)
	if !ok {
		after.Status = StatusManualRequired
		after.NextRunAt = time.Time{}
		return after
	}
	if after.Attempt < step.MaxAttempts {
		if after.Phase == PhaseForward {
			after.Status = StatusPending
		} else {
			after.Status = StatusCompensating
		}
		after.NextRunAt = now.Add(backoff(record.ID, after.Attempt, step.BackoffMin, step.BackoffMax))
		return after
	}
	if after.Phase == PhaseCompensate {
		after.Status = StatusManualRequired
		after.NextRunAt = time.Time{}
		return after
	}
	return e.beginCompensation(after, reason, now)
}

func (e *Engine) beginCompensation(record Record, reason string, now time.Time) Record {
	after := record.Clone()
	after.Version = max(after.Version, record.Version+1)
	after.UpdatedAt = now
	after.LastError = reason
	after.Attempt = 0
	after.CommandID = ""
	after.OperationKey = ""
	clearLease(&after)
	if after.CompletedSteps == 0 {
		after.Status = StatusFailed
		after.NextRunAt = time.Time{}
		return after
	}
	after.Status = StatusCompensating
	after.Phase = PhaseCompensate
	after.Step = after.CompletedSteps - 1
	after.NextRunAt = now
	return after
}

func stepFor(record Record, definition Definition) (Step, bool) {
	if record.Step < 0 || record.Step >= len(definition.Steps) {
		return Step{}, false
	}
	return definition.Steps[record.Step], true
}
func operationKey(id string, phase Phase, step int) string {
	return fmt.Sprintf("%s:%d:%d", id, phase, step)
}

// commandID names one dispatch attempt. Incarnation 0 keeps the historical
// "operationKey:attempt" format so records and receipts that predate the
// field survive an upgrade unchanged; resumed records carry a non-zero
// incarnation and therefore mint identifiers disjoint from every earlier life.
func commandID(operationKey string, incarnation, attempt uint32) string {
	if incarnation == 0 {
		return fmt.Sprintf("%s:%d", operationKey, attempt)
	}
	return fmt.Sprintf("%s:r%d:%d", operationKey, incarnation, attempt)
}
func clearLease(record *Record) { record.Lease = Lease{} }
func canonicalDeadline(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	// BSON datetime and many SQL backends persist milliseconds. Canonicalizing
	// before identity comparison keeps exact start-intent redelivery idempotent.
	return value.UTC().Truncate(time.Millisecond)
}
func closedOperation(before, after Record) string {
	if before.OperationKey == "" {
		return ""
	}
	sameStep := before.Phase == after.Phase && before.Step == after.Step
	if sameStep && (after.Status == StatusPending || after.Status == StatusCompensating) {
		return ""
	}
	return before.OperationKey
}
func (e *Engine) definition(name string, version uint32) (Definition, bool) {
	e.mu.RLock()
	d, ok := e.definitions[definitionKey{typeName: name, version: version}]
	e.mu.RUnlock()
	return d, ok
}
func (e *Engine) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
func (e *Engine) wait(ctx context.Context, kick <-chan struct{}) {
	timer := time.NewTimer(e.opts.PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-kick:
	case <-timer.C:
	}
}
func (e *Engine) countTerminal(status Status) {
	switch status {
	case StatusCompleted:
		e.completed.Add(1)
	case StatusCompensated:
		e.compensated.Add(1)
	case StatusFailed:
		e.failed.Add(1)
	case StatusManualRequired:
		e.failed.Add(1)
		e.manualRequired.Add(1)
	}
}

func backoff(key string, attempt uint32, minimum, maximum time.Duration) time.Duration {
	if minimum <= 0 {
		minimum = time.Millisecond
	}
	if maximum < minimum {
		maximum = minimum
	}
	d := minimum
	for i := uint32(1); i < attempt && d < maximum; i++ {
		if d > maximum/2 {
			d = maximum
			break
		}
		d *= 2
	}
	if d > maximum {
		d = maximum
	}
	if d <= 1 {
		return d
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	var raw [4]byte
	binaryPutUint32(raw[:], attempt)
	_, _ = h.Write(raw[:])
	half := d / 2
	return half + time.Duration(uint64(h.Sum32())%uint64(d-half+1))
}
func binaryPutUint32(dst []byte, value uint32) {
	dst[0] = byte(value >> 24)
	dst[1] = byte(value >> 16)
	dst[2] = byte(value >> 8)
	dst[3] = byte(value)
}
