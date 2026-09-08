package saga

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	defaultSagaCollection       = "_sagas"
	defaultOutboxCollection     = "_saga_outbox"
	defaultCompletionCollection = "_saga_completions"
	defaultOperationCollection  = "_saga_operations"
)

type MongoStoreOptions struct {
	Database             string
	SagaCollection       string
	OutboxCollection     string
	CompletionCollection string
	OperationCollection  string
	CompletionReceiptTTL time.Duration
}

type MongoStore struct {
	client fmongo.IMongo
	opts   MongoStoreOptions
}

func (s *MongoStore) Ping(ctx context.Context) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("saga mongo: not initialized")
	}
	return s.client.Ping(ctx)
}

func NewMongoStore(client fmongo.IMongo, options MongoStoreOptions) (*MongoStore, error) {
	options.Database = strings.TrimSpace(options.Database)
	if client == nil || options.Database == "" {
		return nil, fmt.Errorf("saga mongo: client and database are required")
	}
	if options.SagaCollection == "" {
		options.SagaCollection = defaultSagaCollection
	}
	if options.OutboxCollection == "" {
		options.OutboxCollection = defaultOutboxCollection
	}
	if options.CompletionCollection == "" {
		options.CompletionCollection = defaultCompletionCollection
	}
	if options.OperationCollection == "" {
		options.OperationCollection = defaultOperationCollection
	}
	if options.CompletionReceiptTTL <= 0 {
		options.CompletionReceiptTTL = 30 * 24 * time.Hour
	}
	return &MongoStore{client: client, opts: options}, nil
}

func (s *MongoStore) EnsureInfrastructure(ctx context.Context) error {
	ttl := int64(s.opts.CompletionReceiptTTL / time.Second)
	if ttl <= 0 || ttl > int64(^uint32(0)>>1) {
		return fmt.Errorf("saga mongo: invalid completion receipt ttl %s", s.opts.CompletionReceiptTTL)
	}
	if err := s.sagas().EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "business_key", Value: 1}}, Name: "uniq_type_business", Unique: true},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "next_run_at", Value: 1}, {Key: "lease_until", Value: 1}}, Name: "claim_due"},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "updated_at", Value: -1}}, Name: "operations"},
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "updated_at", Value: -1}}, Name: "operations_by_type"},
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "definition_version", Value: 1}, {Key: "status", Value: 1}, {Key: "updated_at", Value: -1}}, Name: "operations_by_definition"},
	}); err != nil {
		return fmt.Errorf("saga mongo: saga indexes: %w", err)
	}
	if err := s.outbox().EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "lease_until", Value: 1}}, Name: "claim_due"},
		{Keys: bson.D{{Key: "saga_id", Value: 1}, {Key: "created_at", Value: 1}}, Name: "by_saga"},
		{Keys: bson.D{{Key: "command.idempotency_key", Value: 1}}, Name: "by_operation"},
	}); err != nil {
		return fmt.Errorf("saga mongo: outbox indexes: %w", err)
	}
	if err := s.completions().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: int32(ttl)}}); err != nil {
		return err
	}
	return s.operations().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: int32(ttl)}})
}

func (s *MongoStore) Create(ctx context.Context, record coresaga.Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	_, err := s.sagas().InsertOne(ctx, toRecordDoc(record))
	if errors.Is(err, fmongo.ErrDuplicateKey) {
		return coresaga.ErrAlreadyExists
	}
	return err
}

func (s *MongoStore) Get(ctx context.Context, id string) (coresaga.Record, error) {
	var doc recordDoc
	if err := s.sagas().FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
		return coresaga.Record{}, mapNotFound(err)
	}
	return validatedRecord(doc)
}

func (s *MongoStore) GetByBusinessKey(ctx context.Context, sagaType, businessKey string) (coresaga.Record, error) {
	var doc recordDoc
	if err := s.sagas().FindOne(ctx, bson.M{"type": sagaType, "business_key": businessKey}, &doc); err != nil {
		return coresaga.Record{}, mapNotFound(err)
	}
	return validatedRecord(doc)
}

func (s *MongoStore) List(ctx context.Context, query coresaga.Query) ([]coresaga.Record, error) {
	if query.Limit <= 0 || query.Limit > 1000 {
		return nil, coresaga.ErrInvalidRecord
	}
	filter := bson.M{}
	if query.Type != "" {
		filter["type"] = query.Type
	}
	if query.DefinitionVersion != 0 {
		filter["definition_version"] = query.DefinitionVersion
	}
	if len(query.Statuses) > 0 {
		filter["status"] = bson.M{"$in": query.Statuses}
	}
	if !query.UpdatedBefore.IsZero() {
		filter["updated_at"] = bson.M{"$lt": query.UpdatedBefore}
	}
	var docs []recordDoc
	if err := s.sagas().Find(ctx, filter, &docs, fmongo.FindOption{Sort: bson.D{{Key: "updated_at", Value: -1}, {Key: "_id", Value: 1}}, Limit: int64(query.Limit), BatchSize: int32(query.Limit)}); err != nil {
		return nil, err
	}
	out := make([]coresaga.Record, len(docs))
	for i := range docs {
		record, err := validatedRecord(docs[i])
		if err != nil {
			return nil, err
		}
		out[i] = record
	}
	return out, nil
}

func (s *MongoStore) CompletionRecorded(ctx context.Context, completion coresaga.Completion) (bool, error) {
	if err := completion.Validate(); err != nil {
		return false, coresaga.ErrInvalidRecord
	}
	var existing completionDoc
	err := s.completions().FindOne(ctx, bson.M{"_id": completion.CommandID}, &existing)
	if errors.Is(err, fmongo.ErrNotFound) {
		var operation operationDoc
		opErr := s.operations().FindOne(ctx, bson.M{"_id": completion.IdempotencyKey}, &operation)
		if errors.Is(opErr, fmongo.ErrNotFound) {
			return false, nil
		}
		if opErr != nil {
			return false, opErr
		}
		if operation.SagaID != completion.SagaID {
			return false, coresaga.ErrIdentityConflict
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Equal(existing.Digest, completionDigest(completion)) {
		return false, coresaga.ErrIdentityConflict
	}
	return true, nil
}

func (s *MongoStore) ClaimDue(ctx context.Context, request coresaga.ClaimRequest) ([]coresaga.Record, error) {
	if err := validateClaim(request); err != nil {
		return nil, err
	}
	filter := bson.M{"status": bson.M{"$in": []coresaga.Status{coresaga.StatusPending, coresaga.StatusWaiting, coresaga.StatusCompensating}}, "next_run_at": bson.M{"$lte": request.Now}, "lease_until": bson.M{"$lte": request.Now}}
	var candidates []recordDoc
	if err := s.sagas().Find(ctx, filter, &candidates, fmongo.FindOption{Sort: bson.D{{Key: "next_run_at", Value: 1}, {Key: "_id", Value: 1}}, Limit: int64(request.Limit), BatchSize: int32(request.Limit)}); err != nil {
		return nil, err
	}
	out := make([]coresaga.Record, 0, len(candidates))
	for i := range candidates {
		claimFilter := bson.M{"_id": candidates[i].ID, "version": candidates[i].Version, "lease_until": bson.M{"$lte": request.Now}}
		update := bson.M{"$set": bson.M{"lease_owner": request.Owner, "lease_until": request.Now.Add(request.LeaseDuration)}, "$inc": bson.M{"lease_token": 1}}
		var claimed recordDoc
		err := s.sagas().FindOneAndUpdate(ctx, claimFilter, update, &claimed, fmongo.FindOneAndUpdateOption{ReturnAfter: true})
		if errors.Is(err, fmongo.ErrNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		record, validateErr := validatedRecord(claimed)
		if validateErr != nil {
			return out, validateErr
		}
		out = append(out, record)
	}
	return out, nil
}

func (s *MongoStore) Apply(ctx context.Context, request coresaga.ApplyRequest) (coresaga.ApplyOutcome, error) {
	if request.ExpectedVersion == 0 || request.After.Version != request.ExpectedVersion+1 {
		return 0, coresaga.ErrInvalidRecord
	}
	if err := request.After.Validate(); err != nil {
		return 0, err
	}
	if request.Outbox == nil && request.Receipt == nil && request.CloseOperation == "" {
		result, err := s.sagas().ReplaceOne(ctx, applyFilter(request), toRecordDoc(request.After))
		if err != nil {
			return 0, err
		}
		if result.MatchedCount != 1 {
			return 0, coresaga.ErrConflict
		}
		return coresaga.ApplyApplied, nil
	}
	session, err := s.client.StartSession(ctx)
	if err != nil {
		return 0, err
	}
	defer session.EndSession(ctx)
	outcome := coresaga.ApplyApplied
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		// The driver may invoke this callback more than once for a transient
		// transaction error; never leak an outcome from an earlier attempt.
		outcome = coresaga.ApplyApplied
		if request.Receipt != nil {
			digest := completionDigest(*request.Receipt)
			var existing completionDoc
			findErr := s.completions().FindOne(txCtx, bson.M{"_id": request.Receipt.CommandID}, &existing)
			if findErr == nil {
				if !bytes.Equal(existing.Digest, digest) {
					return coresaga.ErrIdentityConflict
				}
				outcome = coresaga.ApplyDuplicate
				return nil
			}
			if !errors.Is(findErr, fmongo.ErrNotFound) {
				return findErr
			}
		}
		if request.CloseOperation != "" {
			var existing operationDoc
			findErr := s.operations().FindOne(txCtx, bson.M{"_id": request.CloseOperation}, &existing)
			if findErr == nil && existing.SagaID != request.After.ID {
				return coresaga.ErrIdentityConflict
			}
			if findErr != nil && !errors.Is(findErr, fmongo.ErrNotFound) {
				return findErr
			}
		}
		result, replaceErr := s.sagas().ReplaceOne(txCtx, applyFilter(request), toRecordDoc(request.After))
		if replaceErr != nil {
			return replaceErr
		}
		if result.MatchedCount != 1 {
			return coresaga.ErrConflict
		}
		if request.Outbox != nil {
			if _, deleteErr := s.outbox().DeleteMany(txCtx, bson.M{"command.idempotency_key": request.Outbox.Command.IdempotencyKey}); deleteErr != nil {
				return deleteErr
			}
			if _, insertErr := s.outbox().InsertOne(txCtx, toOutboxDoc(*request.Outbox)); insertErr != nil {
				return insertErr
			}
		}
		if request.Receipt != nil {
			if _, insertErr := s.completions().InsertOne(txCtx, completionDoc{ID: request.Receipt.CommandID, Digest: completionDigest(*request.Receipt), CreatedAt: request.Receipt.CompletedAt}); insertErr != nil {
				return insertErr
			}
		}
		if request.CloseOperation != "" {
			var existing operationDoc
			findErr := s.operations().FindOne(txCtx, bson.M{"_id": request.CloseOperation}, &existing)
			if errors.Is(findErr, fmongo.ErrNotFound) {
				if _, insertErr := s.operations().InsertOne(txCtx, operationDoc{ID: request.CloseOperation, SagaID: request.After.ID, CreatedAt: request.After.UpdatedAt}); insertErr != nil {
					return insertErr
				}
			}
			if _, deleteErr := s.outbox().DeleteMany(txCtx, bson.M{"command.idempotency_key": request.CloseOperation}); deleteErr != nil {
				return deleteErr
			}
		}
		return nil
	})
	return outcome, err
}

func (s *MongoStore) ClaimOutbox(ctx context.Context, request coresaga.ClaimRequest) ([]coresaga.OutboxRecord, error) {
	if err := validateClaim(request); err != nil {
		return nil, err
	}
	filter := bson.M{"next_attempt_at": bson.M{"$lte": request.Now}, "lease_until": bson.M{"$lte": request.Now}}
	var candidates []outboxDoc
	if err := s.outbox().Find(ctx, filter, &candidates, fmongo.FindOption{Sort: bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "_id", Value: 1}}, Limit: int64(request.Limit), BatchSize: int32(request.Limit)}); err != nil {
		return nil, err
	}
	out := make([]coresaga.OutboxRecord, 0, len(candidates))
	for i := range candidates {
		claimFilter := bson.M{"_id": candidates[i].ID, "lease_until": bson.M{"$lte": request.Now}}
		update := bson.M{"$set": bson.M{"lease_owner": request.Owner, "lease_until": request.Now.Add(request.LeaseDuration)}, "$inc": bson.M{"lease_token": 1}}
		var claimed outboxDoc
		err := s.outbox().FindOneAndUpdate(ctx, claimFilter, update, &claimed, fmongo.FindOneAndUpdateOption{ReturnAfter: true})
		if errors.Is(err, fmongo.ErrNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		record := claimed.record()
		if err := record.Command.Validate(); err != nil || record.NextAttemptAt.IsZero() || record.CreatedAt.IsZero() {
			return out, coresaga.ErrInvalidRecord
		}
		out = append(out, record)
	}
	return out, nil
}

func (s *MongoStore) AckOutbox(ctx context.Context, id string, lease coresaga.Lease) error {
	deleted, err := s.outbox().DeleteOne(ctx, bson.M{"_id": id, "lease_owner": lease.Owner, "lease_token": lease.Token})
	if err != nil {
		return err
	}
	if deleted != 1 {
		return coresaga.ErrConflict
	}
	return nil
}
func (s *MongoStore) NackOutbox(ctx context.Context, id string, lease coresaga.Lease, next time.Time, lastError string) error {
	update := bson.M{"$set": bson.M{"next_attempt_at": next, "last_error": lastError, "lease_until": time.Unix(0, 0).UTC()}, "$unset": bson.M{"lease_owner": ""}, "$inc": bson.M{"attempt": 1}}
	result, err := s.outbox().UpdateOne(ctx, bson.M{"_id": id, "lease_owner": lease.Owner, "lease_token": lease.Token}, update)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return coresaga.ErrConflict
	}
	return nil
}

func (s *MongoStore) sagas() fmongo.ICollection {
	return s.client.Database(s.opts.Database).Collection(s.opts.SagaCollection)
}
func (s *MongoStore) outbox() fmongo.ICollection {
	return s.client.Database(s.opts.Database).Collection(s.opts.OutboxCollection)
}
func (s *MongoStore) completions() fmongo.ICollection {
	return s.client.Database(s.opts.Database).Collection(s.opts.CompletionCollection)
}
func (s *MongoStore) operations() fmongo.ICollection {
	return s.client.Database(s.opts.Database).Collection(s.opts.OperationCollection)
}

type recordDoc struct {
	ID                string          `bson:"_id"`
	Type              string          `bson:"type"`
	DefinitionVersion uint32          `bson:"definition_version"`
	BusinessKey       string          `bson:"business_key"`
	Status            coresaga.Status `bson:"status"`
	Phase             coresaga.Phase  `bson:"phase"`
	Step              int             `bson:"step"`
	CompletedSteps    int             `bson:"completed_steps"`
	Attempt           uint32          `bson:"attempt"`
	Version           uint64          `bson:"version"`
	Data              []byte          `bson:"data,omitempty"`
	LastError         string          `bson:"last_error,omitempty"`
	OperationKey      string          `bson:"operation_key,omitempty"`
	CommandID         string          `bson:"command_id,omitempty"`
	NextRunAt         time.Time       `bson:"next_run_at,omitempty"`
	DeadlineAt        time.Time       `bson:"deadline_at,omitempty"`
	CreatedAt         time.Time       `bson:"created_at"`
	UpdatedAt         time.Time       `bson:"updated_at"`
	LeaseOwner        string          `bson:"lease_owner,omitempty"`
	LeaseToken        uint64          `bson:"lease_token,omitempty"`
	LeaseUntil        time.Time       `bson:"lease_until,omitempty"`
}

func toRecordDoc(r coresaga.Record) recordDoc {
	leaseUntil := r.Lease.Until
	if leaseUntil.IsZero() {
		leaseUntil = time.Unix(0, 0).UTC()
	}
	return recordDoc{ID: r.ID, Type: r.Type, DefinitionVersion: r.DefinitionVersion, BusinessKey: r.BusinessKey, Status: r.Status, Phase: r.Phase, Step: r.Step, CompletedSteps: r.CompletedSteps, Attempt: r.Attempt, Version: r.Version, Data: append([]byte(nil), r.Data...), LastError: r.LastError, OperationKey: r.OperationKey, CommandID: r.CommandID, NextRunAt: r.NextRunAt, DeadlineAt: r.DeadlineAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LeaseOwner: r.Lease.Owner, LeaseToken: r.Lease.Token, LeaseUntil: leaseUntil}
}
func (d recordDoc) record() coresaga.Record {
	lease := coresaga.Lease{Owner: d.LeaseOwner, Token: d.LeaseToken, Until: d.LeaseUntil}
	if lease.Owner == "" {
		lease = coresaga.Lease{}
	}
	return coresaga.Record{ID: d.ID, Type: d.Type, DefinitionVersion: d.DefinitionVersion, BusinessKey: d.BusinessKey, Status: d.Status, Phase: d.Phase, Step: d.Step, CompletedSteps: d.CompletedSteps, Attempt: d.Attempt, Version: d.Version, Data: append([]byte(nil), d.Data...), LastError: d.LastError, OperationKey: d.OperationKey, CommandID: d.CommandID, NextRunAt: d.NextRunAt, DeadlineAt: d.DeadlineAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Lease: lease}
}

func validatedRecord(doc recordDoc) (coresaga.Record, error) {
	record := doc.record()
	if err := record.Validate(); err != nil {
		return coresaga.Record{}, fmt.Errorf("saga mongo: corrupt record %q: %w", doc.ID, err)
	}
	return record, nil
}

type commandDoc struct {
	ID                string         `bson:"id"`
	IdempotencyKey    string         `bson:"idempotency_key"`
	SagaID            string         `bson:"saga_id"`
	SagaType          string         `bson:"saga_type"`
	DefinitionVersion uint32         `bson:"definition_version"`
	BusinessKey       string         `bson:"business_key"`
	Step              int            `bson:"step"`
	StepName          string         `bson:"step_name"`
	Phase             coresaga.Phase `bson:"phase"`
	Attempt           uint32         `bson:"attempt"`
	Topic             string         `bson:"topic"`
	Payload           []byte         `bson:"payload,omitempty"`
	DeadlineAt        time.Time      `bson:"deadline_at"`
	CreatedAt         time.Time      `bson:"created_at"`
}
type outboxDoc struct {
	ID            string     `bson:"_id"`
	SagaID        string     `bson:"saga_id"`
	Command       commandDoc `bson:"command"`
	Attempt       uint32     `bson:"attempt"`
	NextAttemptAt time.Time  `bson:"next_attempt_at"`
	CreatedAt     time.Time  `bson:"created_at"`
	LastError     string     `bson:"last_error,omitempty"`
	LeaseOwner    string     `bson:"lease_owner,omitempty"`
	LeaseToken    uint64     `bson:"lease_token,omitempty"`
	LeaseUntil    time.Time  `bson:"lease_until,omitempty"`
}

func toCommandDoc(c coresaga.Command) commandDoc {
	return commandDoc{ID: c.ID, IdempotencyKey: c.IdempotencyKey, SagaID: c.SagaID, SagaType: c.SagaType, DefinitionVersion: c.DefinitionVersion, BusinessKey: c.BusinessKey, Step: c.Step, StepName: c.StepName, Phase: c.Phase, Attempt: c.Attempt, Topic: c.Topic, Payload: append([]byte(nil), c.Payload...), DeadlineAt: c.DeadlineAt, CreatedAt: c.CreatedAt}
}
func (d commandDoc) command() coresaga.Command {
	return coresaga.Command{ID: d.ID, IdempotencyKey: d.IdempotencyKey, SagaID: d.SagaID, SagaType: d.SagaType, DefinitionVersion: d.DefinitionVersion, BusinessKey: d.BusinessKey, Step: d.Step, StepName: d.StepName, Phase: d.Phase, Attempt: d.Attempt, Topic: d.Topic, Payload: append([]byte(nil), d.Payload...), DeadlineAt: d.DeadlineAt, CreatedAt: d.CreatedAt}
}
func toOutboxDoc(o coresaga.OutboxRecord) outboxDoc {
	leaseUntil := o.Lease.Until
	if leaseUntil.IsZero() {
		leaseUntil = time.Unix(0, 0).UTC()
	}
	return outboxDoc{ID: o.Command.ID, SagaID: o.Command.SagaID, Command: toCommandDoc(o.Command), Attempt: o.Attempt, NextAttemptAt: o.NextAttemptAt, CreatedAt: o.CreatedAt, LeaseOwner: o.Lease.Owner, LeaseToken: o.Lease.Token, LeaseUntil: leaseUntil}
}
func (d outboxDoc) record() coresaga.OutboxRecord {
	return coresaga.OutboxRecord{Command: d.Command.command(), Attempt: d.Attempt, NextAttemptAt: d.NextAttemptAt, CreatedAt: d.CreatedAt, Lease: coresaga.Lease{Owner: d.LeaseOwner, Token: d.LeaseToken, Until: d.LeaseUntil}}
}

type completionDoc struct {
	ID        string    `bson:"_id"`
	Digest    []byte    `bson:"digest"`
	CreatedAt time.Time `bson:"created_at"`
}
type operationDoc struct {
	ID        string    `bson:"_id"`
	SagaID    string    `bson:"saga_id"`
	CreatedAt time.Time `bson:"created_at"`
}

func applyFilter(r coresaga.ApplyRequest) bson.M {
	f := bson.M{"_id": r.After.ID, "version": r.ExpectedVersion}
	if r.ExpectedLease.Owner != "" {
		f["lease_owner"] = r.ExpectedLease.Owner
		f["lease_token"] = r.ExpectedLease.Token
	}
	return f
}
func validateClaim(r coresaga.ClaimRequest) error {
	if strings.TrimSpace(r.Owner) == "" || r.Now.IsZero() || r.LeaseDuration <= 0 || r.Limit <= 0 {
		return coresaga.ErrInvalidRecord
	}
	return nil
}
func mapNotFound(err error) error {
	if errors.Is(err, fmongo.ErrNotFound) {
		return coresaga.ErrNotFound
	}
	return err
}
func completionDigest(c coresaga.Completion) []byte {
	stable := struct {
		Command   string `json:"command_id"`
		Key       string `json:"idempotency_key"`
		Saga      string `json:"saga_id"`
		Success   bool   `json:"success"`
		Retryable bool   `json:"retryable"`
		Data      []byte `json:"data"`
		Error     string `json:"error"`
	}{c.CommandID, c.IdempotencyKey, c.SagaID, c.Success, c.Retryable, c.Data, c.Error}
	raw, _ := json.Marshal(stable)
	sum := sha256.Sum256(raw)
	return sum[:]
}

var _ coresaga.Store = (*MongoStore)(nil)
