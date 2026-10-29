package saga

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// cold_copy.go 是可选的 saga 冷副本（设计 §8，选项 C-A，缺省关闭）：协调器进程把分区事件里的 saga 记录投影进
// Mongo `<saga.database>.<saga.cold_copy.collection>`，供 Get / GetByBusinessKey / List 在分区所有者内存里没有这条记录时
// 回落读取（终态过了 saga.terminal_retention 被剪掉之后的历史查询）。
//
// 冷副本只是读路径的补充，不是权威：
//   - 不参与唯一性与任何判定。启动判定、判定补查（submitIntent）、WaitTerminal 只看分区内存；
//   - 回落只在所有者回答“内存里没有这条”时发生；所有者不可用仍是 ErrPartitionUnavailable，不拿冷副本顶替；
//   - 它落后于内存（批量、异步），读到的可能不是最新版本。内存里有这个 ID 时永远以内存为准（List 用 Known / Held 排除）。
//
// 投影（ColdCopyProjector）：每条流一个 durable（ColdCopyDurable，状态 R1 内存：进度丢了只是从流里重投一遍），缺省只过滤
// ev.done（terminal_only）；一批至多 batch_size 条、最多等 max_wait，同一 saga 只写这批里最新的一条，一次 BulkWrite，
// 每条写是“更新才整文档替换”的管道 upsert（见 coldCopyNewer），写成功后逐条 ack，写失败逐条 nak 重投。
//
// 为什么逐条 ack、不用 AckAll（原型稿 §12 的教训）：前一个进程取走、没 ack 就死掉的消息在等 AckWait 重投；AckAll 确认本批
// 最后一条会把序号更小、还没写的那几条一并确认掉，冷副本就永远缺它们。这里每条投递只由写过它（或这批里同一 saga 更新版本）的
// 那次 BulkWrite 结算。
//
// 清理守卫：冷副本 durable 是分区清理的守卫之一（partition.purge）：不越过它的 ack floor；找不到它时不清理（R1 内存消费者随
// 承载它的服务端重启而消失，投影器发现后重建，期间不能删它还没投影的事件）。所以关掉冷副本后要由运维删掉这些 durable，
// 否则它们一直挡住清理（与下线步骤的 durable 同一规则，SAGA.md）。

// 冷副本参数的缺省值（与 wiring/saga 的 saga.cold_copy.* 声明相同）。
const (
	DefaultColdCopyCollection = "sagas"
	DefaultColdCopyBatchSize  = 256
	DefaultColdCopyMaxWait    = 50 * time.Millisecond
	// coldCopyAckWait 是投影 durable 的 AckWait：一批的写入期限（coldCopyWriteTimeout）加上排队，远小于它。
	coldCopyAckWait = 30 * time.Second
	// coldCopyWriteTimeout 是一次 BulkWrite 的期限；超时按写失败逐条 nak。
	coldCopyWriteTimeout = 10 * time.Second
	// coldCopyCheckEvery 是投影器核对自己的 durable 还在不在的间隔（不在就重建订阅）。
	coldCopyCheckEvery = 5 * time.Second
)

// ColdCopySettings 是冷副本的装配参数（saga.cold_copy.* 与 saga.database）。Client 为 nil 时不装配冷副本。
type ColdCopySettings struct {
	Client     fmongo.IMongo
	Database   string
	Collection string
	// TerminalOnly 只投影终态事件（ev.done）：写入量按笔一次（重开后再到终态时再一次）；在途记录由所有者内存回答。
	TerminalOnly bool
	// BatchSize 是一批最多的事件数（1～1000）；MaxWait 是一批从第一条起最多等多久。零值取缺省。
	BatchSize int
	MaxWait   time.Duration
}

func (s ColdCopySettings) withDefaults() ColdCopySettings {
	s.Database, s.Collection = strings.TrimSpace(s.Database), strings.TrimSpace(s.Collection)
	if s.Collection == "" {
		s.Collection = DefaultColdCopyCollection
	}
	if s.BatchSize <= 0 {
		s.BatchSize = DefaultColdCopyBatchSize
	}
	if s.MaxWait <= 0 {
		s.MaxWait = DefaultColdCopyMaxWait
	}
	return s
}

func (s ColdCopySettings) validate() error {
	switch {
	case s.Client == nil || s.Database == "":
		return errors.New("saga: cold copy needs a Mongo client and a database")
	case s.BatchSize > 1000:
		return fmt.Errorf("%w: cold copy batch size %d must be <= 1000", ErrInvalidDefinition, s.BatchSize)
	case s.MaxWait > 10*time.Second:
		return fmt.Errorf("%w: cold copy max wait %v must be <= 10s", ErrInvalidDefinition, s.MaxWait)
	}
	return nil
}

// ColdCopy 是冷副本集合的读写：Get / List 给 Engine 的回落读，write 给投影器。
type ColdCopy struct {
	client     fmongo.IMongo
	database   string
	collection string
}

// NewColdCopy 构造冷副本的读写（不访问 Mongo）。collection 为空取 DefaultColdCopyCollection。
func NewColdCopy(client fmongo.IMongo, database, collection string) (*ColdCopy, error) {
	settings := ColdCopySettings{Client: client, Database: database, Collection: collection}.withDefaults()
	if err := settings.validate(); err != nil {
		return nil, err
	}
	return &ColdCopy{client: client, database: settings.Database, collection: settings.Collection}, nil
}

func (c *ColdCopy) coll() fmongo.ICollection {
	return c.client.Database(c.database).Collection(c.collection)
}

// EnsureInfrastructure 建 List 用的索引（_id 由 Mongo 保证唯一；ID 由 (Type, BusinessKey) 派生，不另建业务键索引）。
// 由跑投影的协调器进程调用。
func (c *ColdCopy) EnsureInfrastructure(ctx context.Context) error {
	if err := c.coll().EnsureIndexes(ctx, []fmongo.IndexModel{
		{Keys: bson.D{{Key: "updated_at", Value: 1}, {Key: "_id", Value: 1}}, Name: "cold_updated"},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "updated_at", Value: 1}}, Name: "cold_status_updated"},
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "updated_at", Value: 1}}, Name: "cold_type_updated"},
		{Keys: bson.D{{Key: "type", Value: 1}, {Key: "definition_version", Value: 1}, {Key: "status", Value: 1}, {Key: "updated_at", Value: 1}}, Name: "cold_definition_status_updated"},
	}); err != nil {
		return fmt.Errorf("saga: cold copy indexes: %w", err)
	}
	return nil
}

// Get 读冷副本里的一条记录；没有返回 ErrNotFound。
func (c *ColdCopy) Get(ctx context.Context, id string) (Record, error) {
	var doc coldRecordDoc
	if err := c.coll().FindOne(ctx, bson.M{"_id": id}, &doc); err != nil {
		if errors.Is(err, fmongo.ErrNotFound) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("saga: cold copy get %s: %w", id, err)
	}
	return doc.validated()
}

// List 按 Query 读冷副本（按 updated_at、_id 升序，与 Engine.List 的顺序一致），从第 skip 条起至多 query.Limit 条。
// 校验不过的记录跳过并告警，不让整次查询失败。
func (c *ColdCopy) List(ctx context.Context, query Query, skip int) ([]Record, error) {
	if query.Limit <= 0 || query.Limit > 1000 || skip < 0 {
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
	var docs []coldRecordDoc
	if err := c.coll().Find(ctx, filter, &docs, fmongo.FindOption{Sort: bson.D{{Key: "updated_at", Value: 1}, {Key: "_id", Value: 1}},
		Skip: int64(skip), Limit: int64(query.Limit), BatchSize: int32(query.Limit)}); err != nil {
		return nil, fmt.Errorf("saga: cold copy list: %w", err)
	}
	out := make([]Record, 0, len(docs))
	for i := range docs {
		record, err := docs[i].validated()
		if err != nil {
			metrics.IncCounter("saga.cold_copy.corrupt_record_total", nil, 1)
			slog.Error("saga: skipped a corrupt cold copy record; repair or delete it", "id", docs[i].ID, "err", err)
			continue
		}
		out = append(out, record)
	}
	return out, nil
}

// write 把一批记录写进冷副本：一次 BulkWrite，每条是按 coldCopyNewer 门控的整文档替换 upsert。重投、乱序、多个协调器
// 进程同时投影都幂等：旧版本永远盖不住新版本。
func (c *ColdCopy) write(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	models := make([]fmongo.WriteModel, 0, len(records))
	for _, record := range records {
		models = append(models, coldCopyUpsert(record))
	}
	_, err := c.coll().BulkWrite(ctx, models)
	return err
}

// coldCopyUpsert 是一条记录的门控 upsert：只有 coldCopyNewer 成立时整文档替换，否则保留原文档。
func coldCopyUpsert(record Record) fmongo.WriteModel {
	doc := toColdRecordDoc(record)
	pipeline := bson.A{bson.M{"$replaceWith": bson.M{"$cond": bson.A{coldCopyNewer(doc), bson.M{"$literal": doc}, "$$ROOT"}}}}
	return fmongo.NewUpdateOneModel(bson.M{"_id": doc.ID}, pipeline, true)
}

// coldCopyNewer 是版本门：新文档盖住已有文档，当且仅当
//
//   - 它是更晚的一生（created_at 更大：终态过了保留期被剪掉之后，同一业务键可以再开一笔，ID 相同、版本从 1 重新开始），或
//   - 同一生（created_at 相同）里版本更大。
//
// 只比版本会让新一生的终态（版本常常小于旧一生的终态）永远写不进去。CreatedAt 只在启动时设定（newStartRecord），之后的
// Resume、补偿都不改它；两生之间至少隔一个 terminal_retention（≥ 1m），协调器时钟回拨超过它不在考虑范围内。
func coldCopyNewer(doc coldRecordDoc) bson.M {
	epoch := time.Unix(0, 0).UTC()
	return bson.M{"$or": bson.A{
		bson.M{"$gt": bson.A{doc.CreatedAt, bson.M{"$ifNull": bson.A{"$created_at", epoch}}}},
		bson.M{"$and": bson.A{
			bson.M{"$eq": bson.A{doc.CreatedAt, "$created_at"}},
			bson.M{"$gt": bson.A{int64(doc.Version), bson.M{"$ifNull": bson.A{"$version", int64(0)}}}},
		}},
	}}
}

// coldNewer 是 coldCopyNewer 在内存里的同一规则（一批里同一 saga 只留最新的一条）。
func coldNewer(a, b Record) bool {
	ac, bc := a.CreatedAt.Truncate(time.Millisecond), b.CreatedAt.Truncate(time.Millisecond)
	if !ac.Equal(bc) {
		return ac.After(bc)
	}
	return a.Version > b.Version
}

// coldRecordDoc 是冷副本的文档形状（原 mongo_store.go 的 recordDoc 去掉租约，加 start_attempt）。时间按 BSON 存到毫秒。
type coldRecordDoc struct {
	ID                string    `bson:"_id"`
	Type              string    `bson:"type"`
	DefinitionVersion uint32    `bson:"definition_version"`
	BusinessKey       string    `bson:"business_key"`
	StartDigest       string    `bson:"start_digest,omitempty"`
	StartAttempt      string    `bson:"start_attempt,omitempty"`
	Status            Status    `bson:"status"`
	Phase             Phase     `bson:"phase"`
	Step              int       `bson:"step"`
	CompletedSteps    int       `bson:"completed_steps"`
	Attempt           uint32    `bson:"attempt"`
	Incarnation       uint32    `bson:"incarnation,omitempty"`
	LateStep          int       `bson:"late_step,omitempty"`
	LateData          []byte    `bson:"late_data,omitempty"`
	Version           int64     `bson:"version"`
	Data              []byte    `bson:"data,omitempty"`
	LastError         string    `bson:"last_error,omitempty"`
	OperationKey      string    `bson:"operation_key,omitempty"`
	CommandID         string    `bson:"command_id,omitempty"`
	NextRunAt         time.Time `bson:"next_run_at,omitempty"`
	DeadlineAt        time.Time `bson:"deadline_at,omitempty"`
	CreatedAt         time.Time `bson:"created_at"`
	UpdatedAt         time.Time `bson:"updated_at"`
}

func toColdRecordDoc(r Record) coldRecordDoc {
	ms := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		return t.UTC().Truncate(time.Millisecond)
	}
	return coldRecordDoc{ID: r.ID, Type: r.Type, DefinitionVersion: r.DefinitionVersion, BusinessKey: r.BusinessKey, StartDigest: r.StartDigest,
		StartAttempt: r.StartAttempt, Status: r.Status, Phase: r.Phase, Step: r.Step, CompletedSteps: r.CompletedSteps, Attempt: r.Attempt,
		Incarnation: r.Incarnation, LateStep: r.LateStep, LateData: append([]byte(nil), r.LateData...), Version: int64(r.Version),
		Data: append([]byte(nil), r.Data...), LastError: r.LastError, OperationKey: r.OperationKey, CommandID: r.CommandID,
		NextRunAt: ms(r.NextRunAt), DeadlineAt: ms(r.DeadlineAt), CreatedAt: ms(r.CreatedAt), UpdatedAt: ms(r.UpdatedAt)}
}

func (d coldRecordDoc) validated() (Record, error) {
	var lateData []byte
	if len(d.LateData) > 0 {
		lateData = append([]byte(nil), d.LateData...)
	}
	record := Record{ID: d.ID, Type: d.Type, DefinitionVersion: d.DefinitionVersion, BusinessKey: d.BusinessKey, StartDigest: d.StartDigest,
		StartAttempt: d.StartAttempt, Status: d.Status, Phase: d.Phase, Step: d.Step, CompletedSteps: d.CompletedSteps, Attempt: d.Attempt,
		Incarnation: d.Incarnation, LateStep: d.LateStep, LateData: lateData, Version: uint64(max(d.Version, 0)),
		Data: append([]byte(nil), d.Data...), LastError: d.LastError, OperationKey: d.OperationKey, CommandID: d.CommandID,
		NextRunAt: d.NextRunAt, DeadlineAt: d.DeadlineAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
	if err := record.Validate(); err != nil {
		return Record{}, fmt.Errorf("saga: corrupt cold copy record %q: %w", d.ID, err)
	}
	return record, nil
}

// ---- 投影 ----

// coldCopyWriter 是投影器写冷副本的方式（生产是 ColdCopy.write；单元注入失败与计数）。
type coldCopyWriter func(context.Context, []Record) error

// coldItem 是投影器收下、还没结算的一条事件投递。
type coldItem struct {
	record Record
	settle func(error)
}

// ColdCopyProjector 在协调器进程里把分区事件投影进冷副本（每条流一个 durable，见文件头）。
type ColdCopyProjector struct {
	store    *ColdCopy
	js       fnats.IJetStream
	log      fnats.JetStreamLog
	layout   Layout
	settings ColdCopySettings
	write    coldCopyWriter

	items chan coldItem

	mu      sync.Mutex
	subs    map[string]fnats.IJetStreamSubscription
	cancel  context.CancelFunc
	workers sync.WaitGroup
	started bool
	stopped bool
}

// NewColdCopyProjector 构造投影器（不访问 NATS / Mongo）。log 用来核对 durable 还在不在（DurableAckFloors）。
func NewColdCopyProjector(store *ColdCopy, js fnats.IJetStream, log fnats.JetStreamLog, layout Layout, settings ColdCopySettings) (*ColdCopyProjector, error) {
	if store == nil || js == nil || log == nil {
		return nil, errors.New("saga: cold copy projector needs the cold copy, JetStream and the JetStream log")
	}
	settings.Client, settings.Database = store.client, store.database
	settings = settings.withDefaults()
	if err := settings.validate(); err != nil {
		return nil, err
	}
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	return &ColdCopyProjector{store: store, js: js, log: log, layout: layout, settings: settings, write: store.write,
		items: make(chan coldItem, settings.BatchSize), subs: map[string]fnats.IJetStreamSubscription{}}, nil
}

// filter 是投影 durable 的过滤：terminal_only 只要 ev.done，否则全部事件。
func (p *ColdCopyProjector) filter() string {
	if p.settings.TerminalOnly {
		return p.layout.DoneFilter()
	}
	return p.layout.AllEventsFilter()
}

func (p *ColdCopyProjector) consumerConfig(stream string) fnats.JetStreamConsumerConfig {
	name := ColdCopyDurable(stream)
	return fnats.JetStreamConsumerConfig{
		Stream: stream, Name: name, Durable: name, FilterSubject: p.filter(), DeliverPolicy: fnats.JetStreamDeliverAll,
		AckWait: coldCopyAckWait, MaxDeliver: 1_000_000, MaxAckPending: min(max(4*p.settings.BatchSize, 1024), 65_536),
		NakBackoffMin: 250 * time.Millisecond, NakBackoffMax: 10 * time.Second,
		// 进度丢了只是从流里还在的事件重投一遍（版本门让它幂等），所以状态 R1 内存：每条 ack 不是一次 R3 共识写。
		Replicas: 1, MemoryStorage: true,
	}
}

// Start 建索引、在每条流上订阅投影 durable，启动批写与 durable 核对。返回 nil 后一直运行到 Stop。只能调用一次。
func (p *ColdCopyProjector) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		return errors.New("saga: cold copy projector already started")
	}
	p.started = true
	p.mu.Unlock()
	if err := p.store.EnsureInfrastructure(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		p.batch(runCtx)
	}()
	for _, stream := range p.layout.Streams() {
		p.mu.Lock()
		err := p.subscribeLocked(ctx, stream)
		p.mu.Unlock()
		if err != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer stopCancel()
			return errors.Join(err, p.Stop(stopCtx))
		}
	}
	p.workers.Add(1)
	go func() {
		defer p.workers.Done()
		p.supervise(runCtx)
	}()
	return nil
}

func (p *ColdCopyProjector) subscribeLocked(ctx context.Context, stream string) error {
	sub, err := p.js.Subscribe(ctx, p.consumerConfig(stream), p.handle)
	if err != nil {
		return fmt.Errorf("saga: cold copy subscribe on %s: %w", stream, err)
	}
	p.subs[stream] = sub
	return nil
}

// handle 收下一条事件：坏信封 Term（与协调器重放同一判据）；其余推迟结算、交给批写，由写它的那次 BulkWrite 逐条结算。
func (p *ColdCopyProjector) handle(ctx context.Context, message *fnats.JetStreamMsg) error {
	record, err := p.decode(message)
	if err != nil {
		logConsumerError("cold copy decode", message, err)
		return rejectEnvelope("cold_copy", err)
	}
	var settle func(error)
	if message.DeferSettle != nil {
		settle = message.DeferSettle()
	}
	if settle == nil {
		// 不能推迟（订阅正在结束、测试替身）：同步写这一条，按结果结算。
		writeCtx, cancel := context.WithTimeout(ctx, coldCopyWriteTimeout)
		defer cancel()
		return p.write(writeCtx, []Record{record})
	}
	select {
	case p.items <- coldItem{record: record, settle: settle}:
		return nil
	case <-ctx.Done():
		return ctx.Err() // 传输按这个错误立即 nak；settle 之后不再生效
	}
}

// decode 取出事件里的 saga 记录（事件版本、分区、内容校验与协调器重放相同）。
func (p *ColdCopyProjector) decode(message *fnats.JetStreamMsg) (Record, error) {
	if message == nil || len(message.Data) > maxWireEnvelopeBytes {
		return Record{}, ErrInvalidRecord
	}
	k, ok := p.layout.partitionOfEvent(message.Subject)
	if !ok {
		return Record{}, fmt.Errorf("%w: %q is not a saga event subject", ErrInvalidRecord, message.Subject)
	}
	ev, err := decodeEvent(message.Data, p.layout, k)
	if err != nil {
		return Record{}, err
	}
	return ev.Saga.Record.Clone(), nil
}

// batch 攒批：至多 BatchSize 条，或从这批第一条起等 MaxWait；写完逐条结算。ctx 结束时把手里的投递全部 nak（重投给别的进程
// 或下次启动）后退出。
func (p *ColdCopyProjector) batch(ctx context.Context) {
	var pending []coldItem
	var timer *time.Timer
	var deadline <-chan time.Time
	flush := func() {
		if timer != nil {
			timer.Stop()
			timer, deadline = nil, nil
		}
		p.flush(ctx, pending)
		pending = pending[:0]
	}
	for {
		select {
		case item := <-p.items:
			pending = append(pending, item)
			if len(pending) == 1 {
				timer = time.NewTimer(p.settings.MaxWait)
				deadline = timer.C
			}
			if len(pending) >= p.settings.BatchSize {
				flush()
			}
		case <-deadline:
			timer, deadline = nil, nil
			flush()
		case <-ctx.Done():
			for drained := false; !drained; {
				select {
				case item := <-p.items:
					pending = append(pending, item)
				default:
					drained = true
				}
			}
			for _, item := range pending {
				item.settle(ErrClosed)
			}
			return
		}
	}
}

// flush 把一批写进冷副本：同一 saga 只写最新的一条（coldNewer），一次写入；成功逐条 ack，失败逐条 nak（按退避重投，下次
// 重写同样幂等）。
func (p *ColdCopyProjector) flush(ctx context.Context, items []coldItem) {
	if len(items) == 0 {
		return
	}
	latest := make(map[string]int, len(items))
	records := make([]Record, 0, len(items))
	for _, item := range items {
		if i, ok := latest[item.record.ID]; ok {
			if coldNewer(item.record, records[i]) {
				records[i] = item.record
			}
			continue
		}
		latest[item.record.ID] = len(records)
		records = append(records, item.record)
	}
	writeCtx, cancel := context.WithTimeout(ctx, coldCopyWriteTimeout)
	err := p.write(writeCtx, records)
	cancel()
	metrics.IncCounter("saga.cold_copy.events_total", nil, int64(len(items)))
	if err != nil {
		metrics.IncCounter("saga.cold_copy.write_failures_total", nil, 1)
		slog.Warn("saga: cold copy batch write failed; its events are redelivered", "events", len(items), "sagas", len(records), "err", err)
	} else {
		metrics.IncCounter("saga.cold_copy.writes_total", nil, int64(len(records)))
	}
	for _, item := range items {
		item.settle(err)
	}
}

// supervise 每 coldCopyCheckEvery 核对每条流上的投影：订阅意外结束、或 durable 不在了（R1 内存消费者随承载它的服务端重启
// 消失），就停掉旧订阅重新订阅（重建 durable，从流里还在的事件重投；这期间分区清理因为找不到它而停着，见 purge）。
func (p *ColdCopyProjector) supervise(ctx context.Context) {
	ticker := time.NewTicker(coldCopyCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, stream := range p.layout.Streams() {
			p.check(ctx, stream)
		}
	}
}

func (p *ColdCopyProjector) check(ctx context.Context, stream string) {
	checkCtx, cancel := context.WithTimeout(ctx, coldCopyCheckEvery)
	defer cancel()
	p.mu.Lock()
	sub := p.subs[stream]
	p.mu.Unlock()
	lost := sub == nil
	if sub != nil {
		select {
		case <-sub.Closed():
			lost = true
		default:
		}
	}
	if !lost {
		floors, err := p.log.DurableAckFloors(checkCtx, stream)
		if err != nil {
			return // 列不出消费者：下次再看（清理同样会跳过）
		}
		_, present := floors[ColdCopyDurable(stream)]
		lost = !present
	}
	if !lost {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || ctx.Err() != nil {
		return
	}
	if old := p.subs[stream]; old != nil {
		old.Stop()
		delete(p.subs, stream)
	}
	if err := p.subscribeLocked(checkCtx, stream); err != nil {
		slog.Warn("saga: cold copy durable is gone and could not be recreated yet; the partition purge waits for it", "stream", stream, "err", err)
		return
	}
	metrics.IncCounter("saga.cold_copy.resubscribed_total", nil, 1)
	slog.Warn("saga: cold copy durable was lost and has been recreated; remaining events are projected again", "stream", stream)
}

// Stop 排空订阅（在 ctx 内等推迟中的结算完成，批写会把它们写完），再停批写与核对。ctx 到期时强制停止订阅，手里没写的投递
// nak（由 JetStream 重投），返回 ctx 错误。重复调用返回 nil。等待时不持锁：核对 goroutine 要拿锁才能看到 stopped 退出。
func (p *ColdCopyProjector) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	subs := make([]fnats.IJetStreamSubscription, 0, len(p.subs))
	for stream, sub := range p.subs {
		subs = append(subs, sub)
		delete(p.subs, stream)
	}
	p.mu.Unlock()
	var err error
	for _, sub := range subs {
		sub.Drain()
	}
	for _, sub := range subs {
		select {
		case <-sub.Closed():
		case <-ctx.Done():
			sub.Stop()
			err = ctx.Err()
		}
	}
	if p.cancel != nil {
		p.cancel()
	}
	done := make(chan struct{})
	go func() { p.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	return err
}
