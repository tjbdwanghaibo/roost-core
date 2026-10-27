package jslogproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
)

// 参与方：真实 Nest 实体 + nestwal。步骤在一个 Nest 事务里改余额并写操作回执（opKey → 结果），WAL 落盘后
// 把结果直接发回协调器分区，PubAck 之后才 ack 命令。
//
// 两种持久方式：
//   - ParticipantOptions.Mongo 非空（投影打开，现行口径）：DataEngine 的 MongoStore + Projector（缺省选项）异步把 WAL
//     投影进 Mongo、ack 后 WAL 可裁剪；启动时先 Flush 把 WAL 尾巴投影完，再从 Mongo 加载实体（现行“先排空 WAL 再加载”）。
//   - Mongo 为空（投影关闭，对照组）：Projector 用 ManualReplay、Applier 丢弃，WAL 从不 ack；重启时从整段 WAL 重建实体。
// 替身（与生产的差别，报告逐项写明）：
//   - 实体与 DAO 手写（与 framework/nest 的外部测试同形），不经代码生成；补丁路径 bal / r.<opKey>。
//   - 回执放在实体文档里（按 opKey），代替现行的 _dataengine_step_operations 状态文档 + 回执集合 + lease fence。

const (
	accountKind     entity.EntityKind     = 231
	accountCategory entity.EntityCategory = entity.EntityCategoryPlayer
	accountMask     uint64                = 1
)

var registerKindOnce sync.Once

func accountEntityID(n int64) int64 {
	registerKindOnce.Do(func() { entity.MustRegisterEntityKindCategory(accountKind, accountCategory) })
	id, err := entity.BuildEntityID(n+1, accountKind)
	if err != nil {
		panic(err)
	}
	return id
}

// Receipt 是操作实例的结论（成功或业务拒绝）；可重试失败与过期不留回执。
type Receipt struct {
	CmdID   string `bson:"c" json:"c"`
	Success bool   `bson:"s" json:"s"`
	Reason  string `bson:"x,omitempty" json:"x,omitempty"`
	At      int64  `bson:"t" json:"t"`
}

type accountDao struct {
	id       int64
	db       string
	Tracker  dataengine.Tracker
	Balance  int64
	Receipts map[string]Receipt
}

func (d *accountDao) Id() int64                         { return d.id }
func (d *accountDao) SetId(id int64)                    { d.id = id }
func (d *accountDao) DbName() string                    { return d.db }
func (d *accountDao) CollName() string                  { return "account" }
func (d *accountDao) Dirty() entity.IDirty              { return &d.Tracker }
func (d *accountDao) CleanDirty()                       { d.Tracker.SelfClean() }
func (d *accountDao) DirtyTracker() *dataengine.Tracker { return &d.Tracker }

type accountDoc struct {
	ID       int64              `bson:"_id"`
	Balance  int64              `bson:"bal"`
	Receipts map[string]Receipt `bson:"r"`
}

func (d *accountDao) CaptureRollbackState() ([]byte, error) {
	return bson.Marshal(accountDoc{Balance: d.Balance, Receipts: d.Receipts})
}

func (d *accountDao) RestoreRollbackState(raw []byte) error {
	var doc accountDoc
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	d.Balance, d.Receipts = doc.Balance, doc.Receipts
	return nil
}

func (d *accountDao) key() dataengine.DocumentKey {
	return dataengine.DocumentKey{Database: d.DbName(), Resource: d.CollName(), ID: d.id}
}

// PrepareMutation：第一次写是整文档 Put，之后是字段补丁（$set bal / r.<opKey>，$unset 过期回执）。
func (d *accountDao) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error) {
	version := d.Tracker.Version()
	m := dataengine.Mutation{Key: d.key(), ExpectedVersion: version, NextVersion: version + 1, Mask: change.Mask, Schema: 1, Codec: "bson"}
	if version == 0 {
		raw, err := bson.Marshal(accountDoc{ID: d.id, Balance: d.Balance, Receipts: d.Receipts})
		if err != nil {
			return m, err
		}
		m.Kind, m.Data = dataengine.MutationPut, raw
		return m, nil
	}
	set := bson.D{}
	for path, value := range change.Set {
		set = append(set, bson.E{Key: path, Value: value})
	}
	raw, err := bson.Marshal(set)
	if err != nil {
		return m, err
	}
	m.Kind = dataengine.MutationPatch
	m.Patch = dataengine.FieldPatch{SetBSON: raw, Unset: append([]string(nil), change.Unset...)}
	return m, nil
}

func (d *accountDao) AcceptMutation(m dataengine.Mutation) error {
	return d.Tracker.AcceptVersion(m.ExpectedVersion, m.NextVersion)
}

type accountEntity struct {
	*entity.EntityBase
	dao *accountDao
}

func (e *accountEntity) Base() *entity.EntityBase { return e.EntityBase }
func (e *accountEntity) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.dao)
	}
}

type ParticipantOptions struct {
	Layout     Layout
	WALDir     string
	Accounts   int
	InitialBal int64
	Workers    int
	Durability nest.DurabilityPolicy // DurabilityStrict 或 DurabilityPipelined
	Durable    string                // JetStream durable 名
	// Mongo 非空时打开 DataEngine 投影（Database 是本轮唯一库名，账户文档在 <Database>.account）。
	Mongo    fmongo.IMongo
	Database string
	// ReceiptRetention：回执在实体里保留多久（之后同一操作的迟到投递按过期处理）。
	ReceiptRetention time.Duration
	Log              *slog.Logger

	// 以下只给测试 / 变异用。
	DisableReceiptCheck bool                             // 变异：不查回执，重投就重执行
	RedeliverRate       float64                          // 发完结果后以这个概率 Nak（模拟“结果已发、ack 前崩溃”）
	DupResultRate       float64                          // 以这个概率换一个 MsgID 再发一次同一结果（重复结果）
	DelayResult         func(cmd *Command) time.Duration // 提交后、发结果前等待（迟到成功）
	Now                 func() int64
}

type ParticipantStats struct {
	Executed, Replayed, Refused, Expired, ResultPubs, NestErrors, Redelivered atomic.Uint64
	// 诊断：Nest 调用（含 WAL 落盘）与结果发布（到 PubAck）的累计耗时。
	NestNanos, PubNanos atomic.Uint64
}

type Participant struct {
	nc        *nats.Conn
	js        jetstream.JetStream
	opts      ParticipantOptions
	manager   *entity.EntityManager
	mgr       *nest.NestMgr
	wal       *nestwal.WAL
	projector *engine.Projector
	store     *engine.MongoStore
	ccs       []jetstream.ConsumeContext
	work      chan jetstream.Msg
	wg        sync.WaitGroup
	cancel    context.CancelFunc
	Stats     ParticipantStats
	// RebuiltRecords 是启动时从 WAL 重放的记录数，RebuildTime 是重建耗时。
	RebuiltRecords int
	RebuildTime    time.Duration
}

var (
	stepHandler = nest.NewHandlerName("sagajs_step")
	readHandler = nest.NewHandlerName("sagajs_read")
	openHandler = nest.NewHandlerName("sagajs_open")
)

type discardStore struct{}

func (discardStore) Project(context.Context, dataengine.CommitRecord) error { return nil }

// stepResult 是 Nest handler 的返回：已有回执（回放）或本次结论；Expired 表示过了截止且没有回执，不执行。
type stepResult struct {
	Receipt  Receipt
	Replayed bool
	Expired  bool
}

func NewParticipant(nc *nats.Conn, js jetstream.JetStream, opts ParticipantOptions) (*Participant, error) {
	if opts.Workers == 0 {
		opts.Workers = 16
	}
	if opts.Durability == 0 {
		opts.Durability = nest.DurabilityStrict
	}
	if opts.ReceiptRetention == 0 {
		opts.ReceiptRetention = 10 * time.Minute
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() int64 { return time.Now().UnixMilli() }
	}
	if opts.Durable == "" {
		opts.Durable = "part_acct"
	}
	if opts.Database == "" {
		opts.Database = "sagajs"
	}
	p := &Participant{nc: nc, js: js, opts: opts, manager: entity.NewEntityManager()}
	wal, err := nestwal.Open(nestwal.DefaultOptions(opts.WALDir))
	if err != nil {
		return nil, err
	}
	p.wal = wal
	begin := time.Now()
	var docs map[int64]*rebuiltDoc
	var projector *engine.Projector
	if opts.Mongo != nil {
		projector, docs, err = p.openProjected(wal)
	} else {
		docs, err = rebuildFromWAL(wal)
		if err == nil {
			projector, err = engine.NewProjector(wal, discardStore{}, engine.ProjectorOptions{CloseWAL: true, ManualReplay: true})
		}
	}
	if err != nil {
		_ = wal.Close(context.Background())
		return nil, fmt.Errorf("recover participant state: %w", err)
	}
	p.RebuildTime = time.Since(begin)
	for _, d := range docs {
		p.RebuiltRecords += int(d.version)
	}
	p.projector = projector
	for n := int64(0); n < int64(opts.Accounts); n++ {
		id := accountEntityID(n)
		dao := &accountDao{id: id, db: opts.Database, Receipts: map[string]Receipt{}}
		if d, ok := docs[id]; ok {
			dao.Balance, dao.Receipts = d.balance, d.receipts
			dao.Tracker.SetVersion(d.version)
		}
		e := &accountEntity{EntityBase: entity.NewEntityBase(id, accountCategory, false, accountKind), dao: dao}
		if err := p.manager.TryAdd(e); err != nil {
			return nil, err
		}
	}
	p.mgr = nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(p.manager)), nest.NestOptionWithTransactionCommitter(projector),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: opts.Workers, QueueCap: 4096}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	p.mgr.MustRegisterHandlerWithMeta(stepHandler, p.step, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: opts.Durability})
	p.mgr.MustRegisterHandlerWithMeta(openHandler, p.open, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	p.mgr.MustRegisterHandlerWithMeta(readHandler, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		return es[0].(*accountEntity).dao.Balance, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackNone, Durability: nest.DurabilityMemory})
	if err := p.mgr.Start(); err != nil {
		return nil, err
	}
	// 开户：版本 0 的实体写一次整文档（重建出来的跳过）。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var openErr atomic.Value
	for n := int64(0); n < int64(opts.Accounts); n++ {
		if _, ok := docs[accountEntityID(n)]; ok {
			continue
		}
		wg.Add(1)
		go func(n int64) {
			defer wg.Done()
			if _, err := p.mgr.Request(ctx, openHandler, accountEntityID(n), nest.Params{opts.InitialBal}); err != nil {
				openErr.Store(err)
			}
		}(n)
	}
	wg.Wait()
	if v := openErr.Load(); v != nil {
		return nil, v.(error)
	}
	return p, nil
}

func (p *Participant) open(es []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
	dao := es[0].(*accountEntity).dao
	old := dao.Balance
	nest.RecordUndo(dao, accountMask, func() error { dao.Balance = old; return nil })
	dao.Balance = args[0].(int64)
	return nil, nest.MarkPersist(dao, accountMask)
}

// step 是参与方的 Nest handler：同一实体的事务串行，所以同一操作的多次尝试在这里天然排队，后到的读到先到的回执。
func (p *Participant) step(es []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
	dao := es[0].(*accountEntity).dao
	cmd := args[0].(*Command)
	now := p.opts.Now()
	if r, ok := dao.Receipts[cmd.Op]; ok && !p.opts.DisableReceiptCheck {
		return stepResult{Receipt: r, Replayed: true}, nil
	}
	if now >= cmd.DeadlineAt {
		return stepResult{Expired: true}, nil
	}
	receipt := Receipt{CmdID: cmd.CmdID, Success: true, At: now}
	newBal := dao.Balance
	switch {
	case cmd.Action == "credit" && cmd.Refuse:
		receipt.Success, receipt.Reason = false, "credit refused"
	case cmd.Action == "credit":
		newBal += cmd.Amount
	case dao.Balance < cmd.Amount:
		receipt.Success, receipt.Reason = false, "insufficient balance"
	default:
		newBal -= cmd.Amount
	}
	if newBal != dao.Balance {
		old := dao.Balance
		nest.RecordUndo(dao, accountMask, func() error { dao.Balance = old; return nil })
		dao.Balance = newBal
		if err := nest.MarkPersistSet(dao, accountMask, "bal", newBal); err != nil {
			return nil, err
		}
	}
	op := cmd.Op
	prev, had := dao.Receipts[op]
	nest.RecordUndo(dao, accountMask, func() error {
		if had {
			dao.Receipts[op] = prev
		} else {
			delete(dao.Receipts, op)
		}
		return nil
	})
	dao.Receipts[op] = receipt
	if err := nest.MarkPersistSet(dao, accountMask, "r."+op, receipt); err != nil {
		return nil, err
	}
	// 顺手清掉过期回执（同一事务、同一条 WAL 记录）。
	cutoff := now - p.opts.ReceiptRetention.Milliseconds()
	for key, r := range dao.Receipts {
		if r.At < cutoff {
			r, key := r, key
			nest.RecordUndo(dao, accountMask, func() error { dao.Receipts[key] = r; return nil })
			delete(dao.Receipts, key)
			if err := nest.MarkPersistUnset(dao, accountMask, "r."+key); err != nil {
				return nil, err
			}
		}
	}
	return stepResult{Receipt: receipt}, nil
}

// openProjected 按现行 DataEngine 路径打开投影：MongoStore（本轮唯一库、恒等库名表）+ 缺省 Projector，
// 先 Flush 把 WAL 里未投影的尾巴写进 Mongo，再从 Mongo 加载全部账户（版本取 _version）。
func (p *Participant) openProjected(wal *nestwal.WAL) (*engine.Projector, map[int64]*rebuiltDoc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	databases, _, err := dataengine.NewDatabaseMap(dataengine.DatabaseMapConfig{Declared: dataengine.DeclaredDatabases()})
	if err != nil {
		return nil, nil, err
	}
	store, err := engine.NewMongoStore(p.opts.Mongo, engine.MongoStoreConfig{DefaultDatabase: p.opts.Database, Databases: databases})
	if err != nil {
		return nil, nil, err
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		return nil, nil, err
	}
	projector, err := engine.NewProjector(wal, store, engine.DefaultProjectorOptions())
	if err != nil {
		return nil, nil, err
	}
	if err := projector.Flush(ctx); err != nil {
		_ = projector.Close(ctx)
		return nil, nil, fmt.Errorf("drain WAL into mongo: %w", err)
	}
	docs := map[int64]*rebuiltDoc{}
	raws, err := store.Load(ctx, dataengine.LoadSpec{Database: p.opts.Database, Resource: "account"})
	if err != nil {
		_ = projector.Close(ctx)
		return nil, nil, err
	}
	for _, raw := range raws {
		var doc accountDoc
		if err := bson.Unmarshal(raw.Data, &doc); err != nil {
			_ = projector.Close(ctx)
			return nil, nil, err
		}
		d := &rebuiltDoc{balance: doc.Balance, receipts: doc.Receipts, version: raw.Version}
		if d.receipts == nil {
			d.receipts = map[string]Receipt{}
		}
		docs[raw.Key.ID] = d
	}
	p.store = store
	return projector, docs, nil
}

// ProjectionBacklog 是参与方 WAL 里已准入、尚未投影确认的记录数（投影关闭时恒为全部记录，无意义）。
func (p *Participant) ProjectionBacklog() uint64 { return p.projector.Stats().WALUnacked }

// FlushProjection 把已准入的记录全部投影进 Mongo。
func (p *Participant) FlushProjection(ctx context.Context) error { return p.projector.Flush(ctx) }

// MongoBalances 读 Mongo 里的账户投影（冷副本核对用）。
func (p *Participant) MongoBalances(ctx context.Context) (map[int64]int64, error) {
	if p.store == nil {
		return nil, errors.New("projection is off")
	}
	raws, err := p.store.Load(ctx, dataengine.LoadSpec{Database: p.opts.Database, Resource: "account"})
	if err != nil {
		return nil, err
	}
	out := map[int64]int64{}
	for _, raw := range raws {
		var doc accountDoc
		if err := bson.Unmarshal(raw.Data, &doc); err != nil {
			return nil, err
		}
		out[raw.Key.ID] = doc.Balance
	}
	return out, nil
}

type rebuiltDoc struct {
	balance  int64
	receipts map[string]Receipt
	version  uint64
}

// rebuildFromWAL 按 WAL 顺序重放账户 mutation（Put 整文档 / Patch 补丁），得到每个实体的最新状态与版本。
func rebuildFromWAL(wal *nestwal.WAL) (map[int64]*rebuiltDoc, error) {
	docs := map[int64]*rebuiltDoc{}
	err := wal.Replay(context.Background(), func(_ nest.CommitFence, record nest.CommitRecord) error {
		for _, m := range record.Mutations {
			if m.Key.Resource != "account" {
				continue
			}
			d := docs[m.Key.ID]
			if d == nil {
				d = &rebuiltDoc{receipts: map[string]Receipt{}}
				docs[m.Key.ID] = d
			}
			if m.ExpectedVersion != d.version {
				return fmt.Errorf("account %d: WAL version gap %d → %d", m.Key.ID, d.version, m.ExpectedVersion)
			}
			switch m.Kind {
			case dataengine.MutationPut:
				var doc accountDoc
				if err := bson.Unmarshal(m.Data, &doc); err != nil {
					return err
				}
				d.balance = doc.Balance
				if doc.Receipts != nil {
					d.receipts = doc.Receipts
				}
			case dataengine.MutationPatch:
				elements, err := bson.Raw(m.Patch.SetBSON).Elements()
				if err != nil {
					return err
				}
				for _, e := range elements {
					switch key := e.Key(); {
					case key == "bal":
						d.balance = e.Value().Int64()
					case strings.HasPrefix(key, "r."):
						var r Receipt
						if err := e.Value().Unmarshal(&r); err != nil {
							return err
						}
						d.receipts[strings.TrimPrefix(key, "r.")] = r
					}
				}
				for _, key := range m.Patch.Unset {
					delete(d.receipts, strings.TrimPrefix(key, "r."))
				}
			}
			d.version = m.NextVersion
		}
		return nil
	})
	return docs, err
}

// Start 建（或复用）durable 消费者并开始处理命令。
func (p *Participant) Start(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.work = make(chan jetstream.Msg, p.opts.Workers*4)
	for i := 0; i < p.opts.Workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for msg := range p.work {
				p.handle(runCtx, msg)
			}
		}()
	}
	// 每条流一个同名 durable 消费者（分区分到几条流时）。
	for _, name := range p.opts.Layout.Streams() {
		cons, err := p.js.CreateOrUpdateConsumer(ctx, name, jetstream.ConsumerConfig{
			Durable: p.opts.Durable, FilterSubject: p.opts.Layout.DestFilter("acct"), AckPolicy: jetstream.AckExplicitPolicy,
			AckWait: 30 * time.Second, MaxAckPending: 8192, DeliverPolicy: jetstream.DeliverAllPolicy, MaxDeliver: -1,
		})
		if err != nil {
			return err
		}
		cc, err := cons.Consume(func(msg jetstream.Msg) { p.work <- msg }, jetstream.PullMaxMessages(p.opts.Workers*4))
		if err != nil {
			return err
		}
		p.ccs = append(p.ccs, cc)
	}
	return nil
}

func (p *Participant) handle(ctx context.Context, msg jetstream.Msg) {
	var ev Event
	if err := json.Unmarshal(msg.Data(), &ev); err != nil || ev.V != WireVersion || ev.Cmd == nil {
		_ = msg.Term() // 坏信封是确定性错误
		return
	}
	cmd := ev.Cmd
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	nestBegin := time.Now()
	out, err := p.mgr.Request(reqCtx, stepHandler, accountEntityID(cmd.Account), nest.Params{cmd})
	p.Stats.NestNanos.Add(uint64(time.Since(nestBegin)))
	cancel()
	if err != nil {
		p.Stats.NestErrors.Add(1)
		_ = msg.NakWithDelay(100 * time.Millisecond)
		return
	}
	res := out.(stepResult)
	if res.Expired {
		p.Stats.Expired.Add(1)
		_ = msg.Ack()
		return
	}
	switch {
	case res.Replayed:
		p.Stats.Replayed.Add(1)
	case res.Receipt.Success:
		p.Stats.Executed.Add(1)
	default:
		p.Stats.Refused.Add(1)
	}
	if p.opts.DelayResult != nil {
		if d := p.opts.DelayResult(cmd); d > 0 {
			time.Sleep(d)
		}
	}
	completion := Completion{V: WireVersion, SagaID: cmd.SagaID, Op: cmd.Op, CmdID: res.Receipt.CmdID, Phase: cmd.Phase, Step: cmd.Step,
		Success: res.Receipt.Success, Reason: res.Receipt.Reason}
	data, _ := json.Marshal(completion)
	subject := p.opts.Layout.ResultSubject(cmd.Part)
	pubBegin := time.Now()
	defer func() { p.Stats.PubNanos.Add(uint64(time.Since(pubBegin))) }()
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = p.js.Publish(pctx, subject, data, jetstream.WithMsgID("res."+cmd.CmdID))
		cancel()
		if err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return
	}
	p.Stats.ResultPubs.Add(1)
	if p.opts.DupResultRate > 0 && rand.Float64() < p.opts.DupResultRate {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if _, err := p.js.Publish(pctx, subject, data, jetstream.WithMsgID(fmt.Sprintf("res.%s.dup%d", cmd.CmdID, rand.Int64()))); err == nil {
			p.Stats.ResultPubs.Add(1)
		}
		cancel()
	}
	if p.opts.RedeliverRate > 0 && rand.Float64() < p.opts.RedeliverRate {
		p.Stats.Redelivered.Add(1)
		_ = msg.Nak()
		return
	}
	_ = msg.Ack()
}

// Balance 经 Nest 读一个账户（内存事务，与步骤串行）。
func (p *Participant) Balance(ctx context.Context, account int64) (int64, error) {
	out, err := p.mgr.Request(ctx, readHandler, accountEntityID(account), nil)
	if err != nil {
		return 0, err
	}
	return out.(int64), nil
}

// WALSyncs 返回 WAL 的累计 fsync 次数（每步本地 fsync 的分子）。
func (p *Participant) WALSyncs() uint64 { return p.wal.Stats().Syncs }

// Stop 停止消费并关闭 Nest 与 WAL（不 ack WAL：重启时从整段 WAL 重建）。
func (p *Participant) Stop() {
	for _, cc := range p.ccs {
		cc.Stop()
		<-cc.Closed()
	}
	if p.work != nil {
		close(p.work)
	}
	p.wg.Wait()
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.mgr.Shutdown(ctx)
	if err := p.projector.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
		p.opts.Log.Warn("projector close", "err", err)
	}
}
