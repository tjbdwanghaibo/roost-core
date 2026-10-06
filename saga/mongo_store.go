package saga

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
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

func (s *MongoStore) Create(ctx context.Context, record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	_, err := s.sagas().InsertOne(ctx, toRecordDoc(record))
	if errors.Is(err, fmongo.ErrDuplicateKey) {
		return ErrAlreadyExists
	}
	return err
}

func (s *MongoStore) Get(ctx context.Context, id string) (Record, error) {
	var doc recordDoc
	if err := s.sagas().FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
		return Record{}, mapNotFound(err)
	}
	return validatedRecord(doc)
}

func (s *MongoStore) GetByBusinessKey(ctx context.Context, sagaType, businessKey string) (Record, error) {
	var doc recordDoc
	if err := s.sagas().FindOne(ctx, bson.M{"type": sagaType, "business_key": businessKey}, &doc); err != nil {
		return Record{}, mapNotFound(err)
	}
	return validatedRecord(doc)
}

func (s *MongoStore) List(ctx context.Context, query Query) ([]Record, error) {
	if query.Limit <= 0 || query.Limit > 1000 {
		return nil, ErrInvalidRecord
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
	out := make([]Record, len(docs))
	for i := range docs {
		record, err := validatedRecord(docs[i])
		if err != nil {
			return nil, err
		}
		out[i] = record
	}
	return out, nil
}

func (s *MongoStore) CompletionRecorded(ctx context.Context, completion Completion) (bool, error) {
	history, err := s.CompletionHistory(ctx, completion)
	return history.Recorded, err
}

// CompletionHistory 先查这个 CommandID 的 completion receipt，再查 operation tombstone 与它的关闭方式。
func (s *MongoStore) CompletionHistory(ctx context.Context, completion Completion) (CompletionHistory, error) {
	if err := completion.Validate(); err != nil {
		return CompletionHistory{}, ErrInvalidRecord
	}
	var existing completionDoc
	err := s.completions().FindOne(ctx, bson.M{"_id": completion.CommandID}, &existing)
	if errors.Is(err, fmongo.ErrNotFound) {
		var operation operationDoc
		opErr := s.operations().FindOne(ctx, bson.M{"_id": completion.IdempotencyKey}, &operation)
		if errors.Is(opErr, fmongo.ErrNotFound) {
			return CompletionHistory{}, nil
		}
		if opErr != nil {
			return CompletionHistory{}, opErr
		}
		if operation.SagaID != completion.SagaID {
			return CompletionHistory{}, ErrIdentityConflict
		}
		return CompletionHistory{Recorded: true, Closure: operation.closure()}, nil
	}
	if err != nil {
		return CompletionHistory{}, err
	}
	digest, err := completionDigest(completion)
	if err != nil {
		return CompletionHistory{}, err
	}
	if !bytes.Equal(existing.Digest, digest) {
		return CompletionHistory{}, ErrIdentityConflict
	}
	return CompletionHistory{Recorded: true, Receipt: true}, nil
}

// MarkLateSuccessAlarm 在 tombstone 的 late_alarms 子文档里记 r<代际>（B1）。条件更新只匹配还没有这个
// 键的 tombstone，单文档原子：并发送达的同一个迟到成功只有一次 MatchedCount == 1。
func (s *MongoStore) MarkLateSuccessAlarm(ctx context.Context, completion Completion, incarnation uint32) (bool, error) {
	if err := completion.Validate(); err != nil {
		return false, ErrInvalidRecord
	}
	field := lateAlarmField(incarnation)
	filter := bson.M{"_id": completion.IdempotencyKey, "saga_id": completion.SagaID, field: bson.M{"$exists": false}}
	result, err := s.operations().UpdateOne(ctx, filter, bson.M{"$set": bson.M{field: time.Now().UTC()}})
	if err != nil {
		return false, err
	}
	return result != nil && result.MatchedCount == 1, nil
}

func lateAlarmField(incarnation uint32) string {
	return "late_alarms.r" + strconv.FormatUint(uint64(incarnation), 10)
}

func (s *MongoStore) ClaimDue(ctx context.Context, request ClaimRequest) ([]Record, error) {
	if err := validateClaim(request); err != nil {
		return nil, err
	}
	filter := bson.M{"status": bson.M{"$in": []Status{StatusPending, StatusWaiting, StatusCompensating}}, "next_run_at": bson.M{"$lte": request.Now}, "lease_until": bson.M{"$lte": request.Now}}
	var candidates []recordDoc
	if err := s.sagas().Find(ctx, filter, &candidates, fmongo.FindOption{Sort: bson.D{{Key: "next_run_at", Value: 1}, {Key: "_id", Value: 1}}, Limit: int64(request.Limit), BatchSize: int32(request.Limit)}); err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(candidates))
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

func (s *MongoStore) Apply(ctx context.Context, request ApplyRequest) (ApplyOutcome, error) {
	if request.ExpectedVersion == 0 || request.After.Version != request.ExpectedVersion+1 {
		return 0, ErrInvalidRecord
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
			return 0, ErrConflict
		}
		return ApplyApplied, nil
	}
	var receiptDigest []byte
	if request.Receipt != nil {
		digest, err := completionDigest(*request.Receipt)
		if err != nil {
			return 0, err
		}
		receiptDigest = digest
	}
	session, err := s.client.StartSession(ctx)
	if err != nil {
		return 0, err
	}
	defer session.EndSession(ctx)
	outcome := ApplyApplied
	err = session.WithTransaction(ctx, func(txCtx context.Context) error {
		// The driver may invoke this callback more than once for a transient
		// transaction error; never leak an outcome from an earlier attempt.
		outcome = ApplyApplied
		if request.Receipt != nil {
			digest := receiptDigest
			var existing completionDoc
			findErr := s.completions().FindOne(txCtx, bson.M{"_id": request.Receipt.CommandID}, &existing)
			if findErr == nil {
				if !bytes.Equal(existing.Digest, digest) {
					return ErrIdentityConflict
				}
				outcome = ApplyDuplicate
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
				return ErrIdentityConflict
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
			return ErrConflict
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
			if _, insertErr := s.completions().InsertOne(txCtx, completionDoc{ID: request.Receipt.CommandID, Digest: receiptDigest, CreatedAt: request.Receipt.CompletedAt}); insertErr != nil {
				return insertErr
			}
		}
		if request.CloseOperation != "" {
			var existing operationDoc
			findErr := s.operations().FindOne(txCtx, bson.M{"_id": request.CloseOperation}, &existing)
			// 只有接收了成功才算“带结果关闭”：协调器只把成功计入 CompletedSteps、纳入补偿。以失败关闭
			// （可重试失败用尽、拒绝）与超时用尽一样没有记下任何生效的结果，之后到达的成功仍要告警。
			closure := operationClosureAbandoned
			if request.Receipt != nil && request.Receipt.Success {
				closure = operationClosureResult
			}
			switch {
			case errors.Is(findErr, fmongo.ErrNotFound):
				if _, insertErr := s.operations().InsertOne(txCtx, operationDoc{ID: request.CloseOperation, SagaID: request.After.ID, Closure: closure, CreatedAt: request.After.UpdatedAt}); insertErr != nil {
					return insertErr
				}
			case findErr == nil && closure == operationClosureResult && existing.Closure != operationClosureResult:
				// 放弃过的 operation 在 Resume 后的新一生里带结果关闭：以结果为准，之后到达的旧尝试结果按重复处理。
				if _, updateErr := s.operations().UpdateOne(txCtx, bson.M{"_id": request.CloseOperation}, bson.M{"$set": bson.M{"closure": closure}}); updateErr != nil {
					return updateErr
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

func (s *MongoStore) ClaimOutbox(ctx context.Context, request ClaimRequest) ([]OutboxRecord, error) {
	if err := validateClaim(request); err != nil {
		return nil, err
	}
	filter := bson.M{"next_attempt_at": bson.M{"$lte": request.Now}, "lease_until": bson.M{"$lte": request.Now}}
	var candidates []outboxDoc
	if err := s.outbox().Find(ctx, filter, &candidates, fmongo.FindOption{Sort: bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "_id", Value: 1}}, Limit: int64(request.Limit), BatchSize: int32(request.Limit)}); err != nil {
		return nil, err
	}
	out := make([]OutboxRecord, 0, len(candidates))
	for i := range candidates {
		// RR-20261005-NC-41：Find 后另一发布者可能 Nack 并延后重试；领取时复查期限。
		claimFilter := bson.M{"_id": candidates[i].ID, "next_attempt_at": bson.M{"$lte": request.Now}, "lease_until": bson.M{"$lte": request.Now}}
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
			return out, ErrInvalidRecord
		}
		out = append(out, record)
	}
	return out, nil
}

func (s *MongoStore) AckOutbox(ctx context.Context, id string, lease Lease) error {
	deleted, err := s.outbox().DeleteOne(ctx, bson.M{"_id": id, "lease_owner": lease.Owner, "lease_token": lease.Token})
	if err != nil {
		return err
	}
	if deleted != 1 {
		return ErrConflict
	}
	return nil
}
func (s *MongoStore) NackOutbox(ctx context.Context, id string, lease Lease, next time.Time, lastError string) error {
	update := bson.M{"$set": bson.M{"next_attempt_at": next, "last_error": lastError, "lease_until": time.Unix(0, 0).UTC()}, "$unset": bson.M{"lease_owner": ""}, "$inc": bson.M{"attempt": 1}}
	result, err := s.outbox().UpdateOne(ctx, bson.M{"_id": id, "lease_owner": lease.Owner, "lease_token": lease.Token}, update)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return ErrConflict
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
	ID                string `bson:"_id"`
	Type              string `bson:"type"`
	DefinitionVersion uint32 `bson:"definition_version"`
	BusinessKey       string `bson:"business_key"`
	// RR-20261005-NC-39：启动摘要属于不可变身份，完整 Replace 不能把它丢掉。
	StartDigest    string `bson:"start_digest,omitempty"`
	Status         Status `bson:"status"`
	Phase          Phase  `bson:"phase"`
	Step           int    `bson:"step"`
	CompletedSteps int    `bson:"completed_steps"`
	Attempt        uint32 `bson:"attempt"`
	// RR-20261005-NC-38：Resume 代际属于持久派发身份，遗漏会在重读后复用旧 CommandID。
	Incarnation uint32 `bson:"incarnation,omitempty"`
	// saga 方向 ④：放弃后才生效、待补偿的正向步骤号 + 1 与它的载荷；正常记录没有这两个字段。
	LateStep     int       `bson:"late_step,omitempty"`
	LateData     []byte    `bson:"late_data,omitempty"`
	Version      uint64    `bson:"version"`
	Data         []byte    `bson:"data,omitempty"`
	LastError    string    `bson:"last_error,omitempty"`
	OperationKey string    `bson:"operation_key,omitempty"`
	CommandID    string    `bson:"command_id,omitempty"`
	NextRunAt    time.Time `bson:"next_run_at,omitempty"`
	DeadlineAt   time.Time `bson:"deadline_at,omitempty"`
	CreatedAt    time.Time `bson:"created_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
	LeaseOwner   string    `bson:"lease_owner,omitempty"`
	LeaseToken   uint64    `bson:"lease_token,omitempty"`
	LeaseUntil   time.Time `bson:"lease_until,omitempty"`
}

func toRecordDoc(r Record) recordDoc {
	leaseUntil := r.Lease.Until
	if leaseUntil.IsZero() {
		leaseUntil = time.Unix(0, 0).UTC()
	}
	return recordDoc{ID: r.ID, Type: r.Type, DefinitionVersion: r.DefinitionVersion, BusinessKey: r.BusinessKey, StartDigest: r.StartDigest, Status: r.Status, Phase: r.Phase, Step: r.Step, CompletedSteps: r.CompletedSteps, Attempt: r.Attempt, Incarnation: r.Incarnation, LateStep: r.LateStep, LateData: append([]byte(nil), r.LateData...), Version: r.Version, Data: append([]byte(nil), r.Data...), LastError: r.LastError, OperationKey: r.OperationKey, CommandID: r.CommandID, NextRunAt: r.NextRunAt, DeadlineAt: r.DeadlineAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LeaseOwner: r.Lease.Owner, LeaseToken: r.Lease.Token, LeaseUntil: leaseUntil}
}
func (d recordDoc) record() Record {
	lease := Lease{Owner: d.LeaseOwner, Token: d.LeaseToken, Until: d.LeaseUntil}
	if lease.Owner == "" {
		lease = Lease{}
	}
	return Record{ID: d.ID, Type: d.Type, DefinitionVersion: d.DefinitionVersion, BusinessKey: d.BusinessKey, StartDigest: d.StartDigest, Status: d.Status, Phase: d.Phase, Step: d.Step, CompletedSteps: d.CompletedSteps, Attempt: d.Attempt, Incarnation: d.Incarnation, LateStep: d.LateStep, LateData: d.lateData(), Version: d.Version, Data: append([]byte(nil), d.Data...), LastError: d.LastError, OperationKey: d.OperationKey, CommandID: d.CommandID, NextRunAt: d.NextRunAt, DeadlineAt: d.DeadlineAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Lease: lease}
}

// lateData 把空载荷读成 nil：Record.Validate 要求没有迟到步骤时 LateData 为空，迟到成功的 Data 本身也可以为空。
func (d recordDoc) lateData() []byte {
	if len(d.LateData) == 0 {
		return nil
	}
	return append([]byte(nil), d.LateData...)
}

func validatedRecord(doc recordDoc) (Record, error) {
	record := doc.record()
	if err := record.Validate(); err != nil {
		return Record{}, fmt.Errorf("saga mongo: corrupt record %q: %w", doc.ID, err)
	}
	return record, nil
}

type commandDoc struct {
	ID                string    `bson:"id"`
	IdempotencyKey    string    `bson:"idempotency_key"`
	SagaID            string    `bson:"saga_id"`
	SagaType          string    `bson:"saga_type"`
	DefinitionVersion uint32    `bson:"definition_version"`
	BusinessKey       string    `bson:"business_key"`
	Step              int       `bson:"step"`
	StepName          string    `bson:"step_name"`
	Phase             Phase     `bson:"phase"`
	Attempt           uint32    `bson:"attempt"`
	Topic             string    `bson:"topic"`
	Payload           []byte    `bson:"payload,omitempty"`
	DeadlineAt        time.Time `bson:"deadline_at"`
	CreatedAt         time.Time `bson:"created_at"`
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

func toCommandDoc(c Command) commandDoc {
	return commandDoc{ID: c.ID, IdempotencyKey: c.IdempotencyKey, SagaID: c.SagaID, SagaType: c.SagaType, DefinitionVersion: c.DefinitionVersion, BusinessKey: c.BusinessKey, Step: c.Step, StepName: c.StepName, Phase: c.Phase, Attempt: c.Attempt, Topic: c.Topic, Payload: append([]byte(nil), c.Payload...), DeadlineAt: c.DeadlineAt, CreatedAt: c.CreatedAt}
}
func (d commandDoc) command() Command {
	return Command{ID: d.ID, IdempotencyKey: d.IdempotencyKey, SagaID: d.SagaID, SagaType: d.SagaType, DefinitionVersion: d.DefinitionVersion, BusinessKey: d.BusinessKey, Step: d.Step, StepName: d.StepName, Phase: d.Phase, Attempt: d.Attempt, Topic: d.Topic, Payload: append([]byte(nil), d.Payload...), DeadlineAt: d.DeadlineAt, CreatedAt: d.CreatedAt}
}
func toOutboxDoc(o OutboxRecord) outboxDoc {
	leaseUntil := o.Lease.Until
	if leaseUntil.IsZero() {
		leaseUntil = time.Unix(0, 0).UTC()
	}
	return outboxDoc{ID: o.Command.ID, SagaID: o.Command.SagaID, Command: toCommandDoc(o.Command), Attempt: o.Attempt, NextAttemptAt: o.NextAttemptAt, CreatedAt: o.CreatedAt, LeaseOwner: o.Lease.Owner, LeaseToken: o.Lease.Token, LeaseUntil: leaseUntil}
}
func (d outboxDoc) record() OutboxRecord {
	return OutboxRecord{Command: d.Command.command(), Attempt: d.Attempt, NextAttemptAt: d.NextAttemptAt, CreatedAt: d.CreatedAt, Lease: Lease{Owner: d.LeaseOwner, Token: d.LeaseToken, Until: d.LeaseUntil}}
}

type completionDoc struct {
	ID        string    `bson:"_id"`
	Digest    []byte    `bson:"digest"`
	CreatedAt time.Time `bson:"created_at"`
}

// operationDoc 是 operation tombstone。Closure 是 U-0280 增加的字段（"result" / "abandoned"），
// 之前写的 tombstone 没有它，按 OperationClosureUnknown 处理（不告警）。LateAlarms 是 B1 增加的字段：
// 键 r<代际>，值是第一次告警的时间；之前写的 tombstone 没有它，第一次迟到成功照常告警并补上。
type operationDoc struct {
	ID         string               `bson:"_id"`
	SagaID     string               `bson:"saga_id"`
	Closure    string               `bson:"closure,omitempty"`
	LateAlarms map[string]time.Time `bson:"late_alarms,omitempty"`
	CreatedAt  time.Time            `bson:"created_at"`
}

const (
	operationClosureResult    = "result"
	operationClosureAbandoned = "abandoned"
)

func (d operationDoc) closure() OperationClosure {
	switch d.Closure {
	case operationClosureResult:
		return OperationClosedWithResult
	case operationClosureAbandoned:
		return OperationAbandoned
	default:
		return OperationClosureUnknown
	}
}

func applyFilter(r ApplyRequest) bson.M {
	f := bson.M{"_id": r.After.ID, "version": r.ExpectedVersion}
	if r.ExpectedLease.Owner != "" {
		f["lease_owner"] = r.ExpectedLease.Owner
		f["lease_token"] = r.ExpectedLease.Token
	}
	return f
}
func validateClaim(r ClaimRequest) error {
	if strings.TrimSpace(r.Owner) == "" || r.Now.IsZero() || r.LeaseDuration <= 0 || r.Limit <= 0 {
		return ErrInvalidRecord
	}
	return nil
}
func mapNotFound(err error) error {
	if errors.Is(err, fmongo.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
func completionDigest(c Completion) ([]byte, error) {
	stable := struct {
		Command   string `json:"command_id"`
		Key       string `json:"idempotency_key"`
		Saga      string `json:"saga_id"`
		Success   bool   `json:"success"`
		Retryable bool   `json:"retryable"`
		Data      []byte `json:"data"`
		Error     string `json:"error"`
	}{c.CommandID, c.IdempotencyKey, c.SagaID, c.Success, c.Retryable, c.Data, c.Error}
	raw, err := json.Marshal(stable)
	if err != nil {
		return nil, fmt.Errorf("%w: completion digest: %v", ErrInvalidRecord, err)
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

var _ Store = (*MongoStore)(nil)
var _ CompletionHistoryStore = (*MongoStore)(nil)
var _ LateSuccessAlarmStore = (*MongoStore)(nil)
