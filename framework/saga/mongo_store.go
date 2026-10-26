package saga

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	defaultSagaCollection       = "_sagas"
	defaultCompletionCollection = "_saga_completions"
	defaultOperationCollection  = "_saga_operations"
	// sagaMetaCollection 存 saga 元数据（本库对应的权威 Redis 实例，见 RedisStore.EnsureInfrastructure）。
	sagaMetaCollection = "_saga_meta"
)

// MongoStoreOptions 配置 Mongo 冷副本的库、各集合名与 completion 回执 TTL。
type MongoStoreOptions struct {
	Database             string
	SagaCollection       string
	CompletionCollection string
	OperationCollection  string
	CompletionReceiptTTL time.Duration
}

// MongoStore 是协调器存储的 Mongo 冷副本：记录、completion 回执与 operation tombstone 分集合保存。权威在 RedisStore；
// 这里的内容只由 Store Service 的 saga 投影写入（按版本单调、幂等），读它的场景是 Redis 里已驱逐的记录（冷读、装回、
// 历史回查）与最终一致的运维查询（List）。它不实现 Store，没有领取、outbox 与转移写入。
type MongoStore struct {
	client fmongo.IMongo
	opts   MongoStoreOptions
}

// Ping 检查 Mongo 连接。
func (s *MongoStore) Ping(ctx context.Context) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("saga mongo: not initialized")
	}
	return s.client.Ping(ctx)
}

// NewMongoStore 构造 MongoStore；集合名与回执 TTL 缺省时取默认值。
func NewMongoStore(client fmongo.IMongo, options MongoStoreOptions) (*MongoStore, error) {
	options.Database = strings.TrimSpace(options.Database)
	if client == nil || options.Database == "" {
		return nil, fmt.Errorf("saga mongo: client and database are required")
	}
	if options.SagaCollection == "" {
		options.SagaCollection = defaultSagaCollection
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

// EnsureInfrastructure 建冷副本各集合的索引（业务键唯一索引是业务键永久唯一的依据，见 RedisStore.Create）。
func (s *MongoStore) EnsureInfrastructure(ctx context.Context) error {
	ttl := int64(s.opts.CompletionReceiptTTL / time.Second)
	if ttl <= 0 || ttl > int64(^uint32(0)>>1) {
		return fmt.Errorf("saga mongo: invalid completion receipt ttl %s", s.opts.CompletionReceiptTTL)
	}
	if err := s.sagas().EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "business_key", Value: 1}}, Name: "uniq_type_business", Unique: true},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "updated_at", Value: -1}}, Name: "operations"},
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "updated_at", Value: -1}}, Name: "operations_by_type"},
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "definition_version", Value: 1}, {Key: "status", Value: 1}, {Key: "updated_at", Value: -1}}, Name: "operations_by_definition"},
	}); err != nil {
		return fmt.Errorf("saga mongo: saga indexes: %w", err)
	}
	if err := s.completions().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: int32(ttl)}}); err != nil {
		return err
	}
	return s.operations().EnsureIndexes(ctx, []fmongo.IndexModel{{Keys: bson.D{{Key: "created_at", Value: 1}}, Name: "ttl_created_at", TTL: int32(ttl)}})
}

// recordExists 报告冷副本里是否已有同 id 或同 (type, business_key) 的记录（RedisStore.Create 的业务键守卫）。
func (s *MongoStore) recordExists(ctx context.Context, id, sagaType, businessKey string) (bool, error) {
	var doc struct {
		ID string `bson:"_id"`
	}
	err := s.sagas().FindOne(ctx, bson.M{"$or": bson.A{bson.M{"_id": id}, bson.M{"type": sagaType, "business_key": businessKey}}}, &doc)
	if errors.Is(err, fmongo.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
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
	out := make([]Record, 0, len(docs))
	for i := range docs {
		record, err := validatedRecord(docs[i])
		if err != nil {
			// 一条坏记录不让整次查询失败，运维面仍能读出其余记录；坏记录本身告警。
			reportCorruptRecord("list", docs[i].ID, err)
			continue
		}
		out = append(out, record)
	}
	return out, nil
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

// loadForReload 读一条记录在冷副本里的全部可装回状态：记录本身，以及它的 tombstone（字段 o:<操作> = 关闭方式|毫秒）与迟到告警
// 标记（字段 a:<操作>:<代际> = 毫秒），形状与 RedisStore 驻留记录上的字段相同。记录不在时返回 ErrNotFound。
func (s *MongoStore) loadForReload(ctx context.Context, id string) (Record, map[string]string, error) {
	record, err := s.Get(ctx, id)
	if err != nil {
		return Record{}, nil, err
	}
	var operations []operationDoc
	if err := s.operations().Find(ctx, bson.M{"_id": bson.M{"$regex": "^" + regexp.QuoteMeta(id) + ":"}, "saga_id": id}, &operations); err != nil {
		return Record{}, nil, err
	}
	fields := make(map[string]string)
	for _, operation := range operations {
		fields["o:"+operation.ID] = operation.Closure + "|" + strconv.FormatInt(millis(operation.CreatedAt), 10)
		for key, at := range operation.LateAlarms {
			if incarnation, ok := strings.CutPrefix(key, "r"); ok {
				fields["a:"+operation.ID+":"+incarnation] = strconv.FormatInt(millis(at), 10)
			}
		}
	}
	return record, fields, nil
}

func lateAlarmField(incarnation uint32) string {
	return "late_alarms.r" + strconv.FormatUint(uint64(incarnation), 10)
}

// reportCorruptRecord 告警一条校验不过的 saga 记录（手工改坏、不兼容的写者整体 Replace 丢了字段）：计
// saga.store.corrupt_record_total{op}，记点名记录的 ERROR。标签只有操作名，记录 id 只进日志。
func reportCorruptRecord(op, id string, err error) {
	metrics.IncCounter("saga.store.corrupt_record_total", metrics.Labels{"op": op}, 1)
	slog.Error("saga store: skipped a corrupt saga record; repair it", "op", op, "id", id, "err", err)
}

func (s *MongoStore) sagas() fmongo.ICollection {
	return s.client.Database(s.opts.Database).Collection(s.opts.SagaCollection)
}
func (s *MongoStore) meta() fmongo.ICollection {
	return s.client.Database(s.opts.Database).Collection(sagaMetaCollection)
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
	// 启动摘要属于不可变身份，完整 Replace 不能把它丢掉。
	StartDigest    string `bson:"start_digest,omitempty"`
	Status         Status `bson:"status"`
	Phase          Phase  `bson:"phase"`
	Step           int    `bson:"step"`
	CompletedSteps int    `bson:"completed_steps"`
	Attempt        uint32 `bson:"attempt"`
	// Resume 代际属于持久派发身份，遗漏会在重读后复用旧 CommandID。
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

type completionDoc struct {
	ID        string    `bson:"_id"`
	Digest    []byte    `bson:"digest"`
	CreatedAt time.Time `bson:"created_at"`
}

// operationDoc 是 operation tombstone。Closure 是关闭方式（"result" / "abandoned"），缺失时按 OperationClosureUnknown
// 处理（不告警）。LateAlarms 的键是 r<代际>，值是第一次告警的时间；缺失时第一次迟到成功照常告警并补上。
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
