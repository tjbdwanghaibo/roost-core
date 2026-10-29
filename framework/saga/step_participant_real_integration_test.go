//go:build integration

package saga

// 参与方新路径（第 3 批，未接线）在真实依赖上的用例：JetStream 协调器（NewEngine）+ 新步骤消费者
// （SubscribeNativeStep / SubscribeMongoStep）。原生参与方是真实的 Nest 引擎 + nestwal WAL（strict）+ 手写的账户实体
// （DAO 形状与生成 DAO 相同：字段补丁、回执 map 的逆操作按键登记），回执经 StepReceipts 适配放在实体文档的 r.<操作> 里。
//
//   - TestRealJetStreamNativeStepsEndToEnd：正向、业务拒绝后的补偿、“Nest 已提交但步骤体报错”的重投回放；另有一个从来没有命令的
//     空闲步骤 durable，核对它不挡分区清理（设计 §16 第 1 条）。只要私有集群。
//   - TestRealJetStreamParticipantKill9：参与方子进程开 DataEngine 投影（共享隔离 Mongo，唯一库），10% 的步骤在 Nest 提交后报
//     “结果未知”（重投必须回放），跑一批转账 saga 时 kill -9，
//     同一 WAL 重启（先排空 WAL 进 Mongo 再加载），全部到终态后核对 Mongo 里每个账户的余额 = 按终态算出的预期：任何一个操作
//     生效两次（重复扣款 / 重复退款）都会让余额对不上。
//   - TestRealMongoStepInboxSerializesConcurrentAttempts：同一操作的两次尝试并发进 Mongo 事务，写冲突让后者重跑并回放，业务写
//     恰好一份。
//   - TestRealJetStreamLoadPublisherDrivesAdmission：LoadPublisher 的积压关闭准入、回落重开、Remove 后不再计入。
//
// 环境：ROOST_NATS_CLUSTER_URL（scripts/nats-cluster.sh 起的私有集群）；Mongo 用例另要 ROOST_DATAENGINE_IT_MONGO_URI（source
// ~/.roost-it/roost-dataengine-it/env.sh，运行期间持有 $ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock），库名
// roost_sagajs3_<pid>_<ns>，用后删除。Mongo 地址只经环境变量传给子进程，不打印。
// 变异：ROOST_SAGA_MUTATE=noreceipt 让账户 handler 的 Begin 看不到已有回执，kill -9 与端到端用例必须变红；
// ROOST_SAGA_MUTATE=noadmit 让协调器不做准入（原型 SAGAJS_MUTATE=noadmit 的迁移，设计 §11.2），TestRealJetStreamLoadPublisherDrivesAdmission
// 必须变红。
// 长停机：ROOST_SAGA_JS_RESTART_DELAY=60s 让 kill -9 之后过 60s 再拉起参与方（协调器在这期间按步骤超时反复重试）。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// ---- 账户实体（手写，形状同生成 DAO）----

const (
	ptAccountKind     entity.EntityKind     = 232
	ptAccountCategory entity.EntityCategory = entity.EntityCategoryPlayer
	ptBalanceMask     uint64                = 1
	ptReceiptsMask    uint64                = 2
	ptInitialBalance  int64                 = 1_000_000
)

var ptKindOnce sync.Once

func ptAccountID(n int64) int64 {
	ptKindOnce.Do(func() { entity.MustRegisterEntityKindCategory(ptAccountKind, ptAccountCategory) })
	id, err := entity.BuildEntityID(n+1, ptAccountKind)
	if err != nil {
		panic(err)
	}
	return id
}

type ptAccountDao struct {
	id       int64
	db       string
	Tracker  dataengine.Tracker
	Balance  int64
	Receipts map[string][]byte
}

type ptAccountDoc struct {
	ID       int64             `bson:"_id"`
	Balance  int64             `bson:"bal"`
	Receipts map[string][]byte `bson:"r"`
}

func (d *ptAccountDao) Id() int64                         { return d.id }
func (d *ptAccountDao) SetId(id int64)                    { d.id = id }
func (d *ptAccountDao) DbName() string                    { return d.db }
func (d *ptAccountDao) CollName() string                  { return "account" }
func (d *ptAccountDao) Dirty() entity.IDirty              { return &d.Tracker }
func (d *ptAccountDao) CleanDirty()                       { d.Tracker.SelfClean() }
func (d *ptAccountDao) DirtyTracker() *dataengine.Tracker { return &d.Tracker }

func (d *ptAccountDao) CaptureRollbackState() ([]byte, error) {
	return bson.Marshal(ptAccountDoc{Balance: d.Balance, Receipts: d.Receipts})
}

func (d *ptAccountDao) RestoreRollbackState(raw []byte) error {
	var doc ptAccountDoc
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	d.Balance, d.Receipts = doc.Balance, doc.Receipts
	return nil
}

// PrepareMutation：第一次写是整文档 Put，之后是字段补丁（$set bal / r.<操作>，$unset 剪掉的回执）。
func (d *ptAccountDao) PrepareMutation(change nest.PersistChange) (dataengine.Mutation, error) {
	version := d.Tracker.Version()
	m := dataengine.Mutation{Key: dataengine.DocumentKey{Database: d.db, Resource: d.CollName(), ID: d.id}, ExpectedVersion: version, NextVersion: version + 1, Mask: change.Mask, Schema: 1, Codec: "bson"}
	if version == 0 {
		raw, err := bson.Marshal(ptAccountDoc{ID: d.id, Balance: d.Balance, Receipts: d.Receipts})
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

func (d *ptAccountDao) AcceptMutation(m dataengine.Mutation) error {
	return d.Tracker.AcceptVersion(m.ExpectedVersion, m.NextVersion)
}

type ptAccount struct {
	*entity.EntityBase
	dao *ptAccountDao
}

func (e *ptAccount) Base() *entity.EntityBase { return e.EntityBase }
func (e *ptAccount) RangeDao(f func(entity.DaoInterface)) {
	if f != nil {
		f(e.dao)
	}
}

// SagaStepReceipts 是生成适配的形状：回执 map 的读写经“mutator”（逆操作按操作键登记、字段补丁 r.<操作>）。
func (e *ptAccount) SagaStepReceipts() StepReceipts { return ptReceipts{e.dao} }

type ptReceipts struct{ dao *ptAccountDao }

func (r ptReceipts) StepReceipt(operation string) ([]byte, bool) {
	raw, ok := r.dao.Receipts[operation]
	return raw, ok
}

func (r ptReceipts) PutStepReceipt(operation string, encoded []byte) {
	d := r.dao
	prev, had := d.Receipts[operation]
	nest.RecordUndoToken(d, ptReceiptsMask, operation, func() error {
		if had {
			d.Receipts[operation] = prev
		} else {
			delete(d.Receipts, operation)
		}
		return nil
	})
	d.Receipts[operation] = encoded
	if err := nest.MarkPersistSet(d, ptReceiptsMask, "r."+operation, encoded); err != nil {
		panic(err)
	}
}

func (r ptReceipts) DeleteStepReceipt(operation string) {
	d := r.dao
	prev, had := d.Receipts[operation]
	if !had {
		return
	}
	nest.RecordUndoToken(d, ptReceiptsMask, operation, func() error { d.Receipts[operation] = prev; return nil })
	delete(d.Receipts, operation)
	if err := nest.MarkPersistUnset(d, ptReceiptsMask, "r."+operation); err != nil {
		panic(err)
	}
}

func (r ptReceipts) RangeStepReceipts(f func(string, []byte) bool) {
	for operation, raw := range r.dao.Receipts {
		if !f(operation, raw) {
			return
		}
	}
}

// ---- 参与方：Nest + WAL（+ 可选 DataEngine 投影）+ 原生步骤消费者 ----

// ptTransfer 是转账 saga 的 Data（命令载荷）：debit 从 From 扣、credit 给 To 加；Refuse 时 credit 拒绝（触发补偿）。
type ptTransfer struct {
	From   int64 `json:"from"`
	To     int64 `json:"to"`
	Amount int64 `json:"amount"`
	Refuse bool  `json:"refuse,omitempty"`
}

// ptDefinition 是转账 saga：两步原生步骤。预算宽（参与方 kill -9 重启要几秒，不能让协调器先放弃那一步）。
func ptDefinition() Definition {
	step := func(name string) Step {
		return Step{Name: name, ForwardTopic: "pt." + name, CompensateTopic: "pt." + name + ".cancel", Timeout: 2 * time.Second, MaxAttempts: 40, BackoffMin: 50 * time.Millisecond, BackoffMax: 300 * time.Millisecond}
	}
	return Definition{Type: "pt", Version: 1, Steps: []Step{step("debit"), step("credit")}}
}

var ptTopics = []string{"pt.debit", "pt.debit.cancel", "pt.credit", "pt.credit.cancel"}

var (
	ptStepHandler = nest.NewHandlerName("saga_pt_step")
	ptReadHandler = nest.NewHandlerName("saga_pt_read")
)

type discardProjection struct{}

func (discardProjection) Project(context.Context, dataengine.CommitRecord) error { return nil }

type ptParticipant struct {
	manager   *entity.EntityManager
	mgr       *nest.NestMgr
	wal       *nestwal.WAL
	projector *engine.Projector
	subs      []fnats.IJetStreamSubscription
	// executed 计业务真正执行（Begin 判定执行）的次数；unknownAfterCommit 注入“Nest 已提交、步骤体却报错”的概率。
	executed           atomic.Int64
	unknownAfterCommit float64
	injected           atomic.Int64
}

// openPtParticipant 打开参与方：mongo 非空时走 DataEngine 投影（先排空 WAL 尾巴进 Mongo，再从 Mongo 加载账户——现行“先恢复再
// 服务”），否则投影丢弃、账户全新（单进程用例）。
func openPtParticipant(walDir string, mongo fmongo.IMongo, database string, accounts int) (*ptParticipant, error) {
	return openPtParticipantWith(walDir, mongo, database, accounts, nest.DurabilityStrict)
}

// openPtParticipantWith 同 openPtParticipant，步骤 handler 用给定的持久化策略（压测的 pipelined 档用 DurabilityPipelined）。
func openPtParticipantWith(walDir string, mongo fmongo.IMongo, database string, accounts int, durability nest.DurabilityPolicy) (*ptParticipant, error) {
	p := &ptParticipant{manager: entity.NewEntityManager()}
	wal, err := nestwal.Open(nestwal.DefaultOptions(walDir))
	if err != nil {
		return nil, err
	}
	p.wal = wal
	docs := map[int64]ptAccountDoc{}
	versions := map[int64]uint64{}
	if mongo == nil {
		p.projector, err = engine.NewProjector(wal, discardProjection{}, engine.ProjectorOptions{CloseWAL: true, ManualReplay: true})
	} else {
		p.projector, err = openPtProjection(wal, mongo, database, docs, versions)
	}
	if err != nil {
		_ = wal.Close(context.Background())
		return nil, err
	}
	for n := range int64(accounts) {
		id := ptAccountID(n)
		dao := &ptAccountDao{id: id, db: database, Receipts: map[string][]byte{}}
		if doc, ok := docs[id]; ok {
			dao.Balance = doc.Balance
			if doc.Receipts != nil {
				dao.Receipts = doc.Receipts
			}
			dao.Tracker.SetVersion(versions[id])
		}
		if err := p.manager.TryAdd(&ptAccount{EntityBase: entity.NewEntityBase(id, ptAccountCategory, false, ptAccountKind), dao: dao}); err != nil {
			return nil, err
		}
	}
	p.mgr = nest.NewEngine(nest.NestOptionWithGetter(entity.NewManagerAccess(p.manager)), nest.NestOptionWithTransactionCommitter(p.projector),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 8, QueueCap: 4096}, nest.WorkerPoolConfig{}, nest.WorkerPoolConfig{}))
	p.mgr.MustRegisterHandlerWithMeta(ptStepHandler, p.step, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: durability})
	open := nest.NewHandlerName("saga_pt_open")
	p.mgr.MustRegisterHandlerWithMeta(open, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		dao := es[0].(*ptAccount).dao
		dao.Balance = ptInitialBalance
		nest.RecordUndo(dao, ptBalanceMask, func() error { dao.Balance = 0; return nil })
		return nil, nest.MarkPersist(dao, ptBalanceMask)
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityStrict})
	p.mgr.MustRegisterHandlerWithMeta(ptReadHandler, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		return es[0].(*ptAccount).dao.Balance, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackNone, Durability: nest.DurabilityMemory})
	if err := p.mgr.Start(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for n := range int64(accounts) {
		if _, ok := docs[ptAccountID(n)]; ok {
			continue
		}
		if _, err := p.mgr.Request(ctx, open, ptAccountID(n), nil); err != nil {
			return nil, fmt.Errorf("open account %d: %w", n, err)
		}
	}
	return p, nil
}

func openPtProjection(wal *nestwal.WAL, mongo fmongo.IMongo, database string, docs map[int64]ptAccountDoc, versions map[int64]uint64) (*engine.Projector, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	databases, _, err := dataengine.NewDatabaseMap(dataengine.DatabaseMapConfig{Declared: dataengine.DeclaredDatabases()})
	if err != nil {
		return nil, err
	}
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: database, Databases: databases})
	if err != nil {
		return nil, err
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		return nil, err
	}
	projector, err := engine.NewProjector(wal, store, engine.DefaultProjectorOptions())
	if err != nil {
		return nil, err
	}
	// 先恢复再服务：WAL 尾巴投影完才加载。被 kill -9 的进程可能留下没结束的 Mongo 事务（服务端 transactionLifetimeLimitSeconds
	// 才回收，期间同文档写冲突），所以重试到成功（上限 3 分钟；设计 §10 T-A 第 3 条，wiring 侧的有界重试在第 4 批）。
	for deadline := time.Now().Add(3 * time.Minute); ; {
		flushCtx, flushCancel := context.WithTimeout(ctx, 30*time.Second)
		err := projector.Flush(flushCtx)
		flushCancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = projector.Close(ctx)
			return nil, fmt.Errorf("drain WAL into mongo: %w", err)
		}
		fmt.Fprintln(os.Stderr, "participant: draining the WAL into Mongo failed; retrying:", err)
		time.Sleep(time.Second)
	}
	raws, err := store.Load(ctx, dataengine.LoadSpec{Database: database, Resource: "account"})
	if err != nil {
		_ = projector.Close(ctx)
		return nil, err
	}
	for _, raw := range raws {
		var doc ptAccountDoc
		if err := bson.Unmarshal(raw.Data, &doc); err != nil {
			_ = projector.Close(ctx)
			return nil, err
		}
		docs[raw.Key.ID], versions[raw.Key.ID] = doc, raw.Version
	}
	return projector, nil
}

// step 是账户的原生步骤 handler：生成包装的形状（先 Begin，回放 / 过期时不执行业务），之后扣或加余额并 Complete。
func (p *ptParticipant) step(es []entity.IThreadSafeEntity, args []any, _ ...nest.HandlerOption) (any, error) {
	account := es[0].(*ptAccount)
	step, amount, credit := args[0].(NativeStep), args[1].(int64), args[2].(bool)
	var host StepReceiptHost = account
	if os.Getenv("ROOST_SAGA_MUTATE") == "noreceipt" {
		host = blindReceiptHost{account} // 变异：Begin 看不到已有回执，重投就重执行
	}
	if replayed, err := step.Begin(host); replayed || err != nil {
		return nil, err
	}
	p.executed.Add(1)
	dao := account.dao
	if !credit && dao.Balance < amount {
		return nil, step.Complete(false, "insufficient balance")
	}
	if credit && step.Command().Phase == PhaseForward && ptPayload(step.Command()).Refuse {
		return nil, step.Complete(false, "credit refused")
	}
	old := dao.Balance
	nest.RecordUndo(dao, ptBalanceMask, func() error { dao.Balance = old; return nil })
	if credit {
		dao.Balance += amount
	} else {
		dao.Balance -= amount
	}
	if err := nest.MarkPersistSet(dao, ptBalanceMask, "bal", dao.Balance); err != nil {
		return nil, err
	}
	return nil, step.Complete(true, "")
}

// blindReceiptHost 只给变异用：读回执永远“没有”，写照常。
type blindReceiptHost struct{ account *ptAccount }

func (h blindReceiptHost) SagaStepReceipts() StepReceipts {
	return blindReceipts{ptReceipts{h.account.dao}}
}

type blindReceipts struct{ ptReceipts }

func (blindReceipts) StepReceipt(string) ([]byte, bool) { return nil, false }

func ptPayload(command Command) ptTransfer {
	var transfer ptTransfer
	_ = json.Unmarshal(command.Payload, &transfer)
	return transfer
}

// body 是转账的步骤体：按主题选账户与方向，经 Nest 请求执行 handler。按 unknownAfterCommit 的概率在 Nest 成功之后报错（结果未知：
// 消费者 nak，重投时 Begin 回放）。
func (p *ptParticipant) body(ctx context.Context, step NativeStep) error {
	command := step.Command()
	var transfer ptTransfer
	if err := json.Unmarshal(command.Payload, &transfer); err != nil || transfer.Amount <= 0 {
		return fmt.Errorf("pt: bad payload %q: %v", command.Payload, err)
	}
	account, credit := transfer.From, false
	switch command.Topic {
	case "pt.debit.cancel":
		credit = true
	case "pt.credit":
		account, credit = transfer.To, true
	case "pt.credit.cancel":
		account = transfer.To
	}
	if _, err := p.mgr.Request(ctx, ptStepHandler, ptAccountID(account), nest.Params{step, transfer.Amount, credit}); err != nil {
		return err
	}
	if p.unknownAfterCommit > 0 && rand.Float64() < p.unknownAfterCommit {
		p.injected.Add(1)
		return errors.New("pt: injected unknown outcome after the Nest commit")
	}
	return nil
}

func (p *ptParticipant) subscribe(ctx context.Context, js fnats.IJetStream, layout Layout) error {
	for _, topic := range ptTopics {
		subs, err := SubscribeNativeStep(ctx, js, layout, StepConsumerConfig{
			Durable: "pt-" + strings.ReplaceAll(topic, ".", "-"), Topic: topic, AckWait: 10 * time.Second,
			NakBackoffMin: 50 * time.Millisecond, NakBackoffMax: 200 * time.Millisecond, ReceiptRetention: time.Hour,
		}, p.body)
		if err != nil {
			return err
		}
		p.subs = append(p.subs, subs...)
	}
	return nil
}

// close 按三步停：停消费（Drain 等在途处理）、停 Nest、把 WAL 投影完并关闭。
func (p *ptParticipant) close() error {
	for _, sub := range p.subs {
		sub.Drain()
	}
	for _, sub := range p.subs {
		select {
		case <-sub.Closed():
		case <-time.After(20 * time.Second):
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = p.mgr.Shutdown(ctx)
	if err := p.projector.Flush(ctx); err != nil {
		return err
	}
	return p.projector.Close(ctx)
}

// balance 经 Nest 读一个账户（与步骤事务串行，读写之间有同步）。
func (p *ptParticipant) balance(t *testing.T, n int64) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := p.mgr.Request(ctx, ptReadHandler, ptAccountID(n), nil)
	if err != nil {
		t.Fatal(err)
	}
	return out.(int64)
}

// ---- 共用：连接、协调器、发起与核对 ----

func ptJetStream(t *testing.T, url string) (realJSConn, fnats.IJetStream) {
	t.Helper()
	conn := dialRealJS(t, url)
	js, err := ndriver.NewJetStreamClient(conn.client)
	if err != nil {
		t.Fatal(err)
	}
	return conn, js
}

func startPtCoordinator(t *testing.T, conn realJSConn, layout Layout, configure func(*Options)) *Engine {
	t.Helper()
	options := realEngineOptions(layout, allPartitions(layout.Partitions))
	if configure != nil {
		configure(&options)
	}
	e, err := NewEngine(conn.log, conn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Register(ptDefinition()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	})
	return e
}

// ptStartAll 并发发起 n 笔转账（每 5 笔一笔 credit 被拒），返回业务键 → 转账。onStarted 在每笔启动后调用（kill -9 的时机）。
func ptStartAll(t *testing.T, e *Engine, n, workers, accounts int, onStarted func(int)) map[string]ptTransfer {
	t.Helper()
	transfers := map[string]ptTransfer{}
	var mu sync.Mutex
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				transfer := ptTransfer{From: int64(i % accounts), To: int64((i*7 + 3) % accounts), Amount: int64(i + 1), Refuse: i%5 == 4}
				if transfer.To == transfer.From {
					transfer.To = (transfer.To + 1) % int64(accounts)
				}
				bk := "t-" + strconv.Itoa(i)
				data, _ := json.Marshal(transfer)
				for {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					_, err := e.StartSaga(ctx, StartRequest{Type: "pt", BusinessKey: bk, DefinitionVersion: 1, Data: data})
					cancel()
					if err == nil || errors.Is(err, ErrStartUnconfirmed) {
						break
					}
					if !errors.Is(err, ErrBackpressure) {
						t.Errorf("start %s: %v", bk, err)
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
				mu.Lock()
				transfers[bk] = transfer
				mu.Unlock()
				if onStarted != nil {
					onStarted(i)
				}
			}
		}()
	}
	wg.Wait()
	return transfers
}

// ptExpectedBalances 按终态算每个账户的预期余额：Completed = 扣一次、加一次；Compensated = 扣了又退（净零）。其他终态失败。
func ptExpectedBalances(t *testing.T, e *Engine, transfers map[string]ptTransfer, accounts int, budget time.Duration) []int64 {
	t.Helper()
	want := make([]int64, accounts)
	for i := range want {
		want[i] = ptInitialBalance
	}
	statuses := map[Status]int{}
	deadline := time.Now().Add(budget)
	for bk, transfer := range transfers {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		final, err := e.WaitTerminal(ctx, IDOf("pt", bk))
		cancel()
		if err != nil {
			// 诊断：所有者眼里的当前记录与协调器计数（WaitTerminal 漏看终态时这里会是终态）。
			getCtx, getCancel := context.WithTimeout(context.Background(), 5*time.Second)
			record, getErr := e.Get(getCtx, IDOf("pt", bk))
			getCancel()
			t.Fatalf("saga %s (%+v) not terminal: %v; owner has %+v (%v); stats %+v", bk, transfer, err, record, getErr, e.Stats())
		}
		statuses[final.Status]++
		switch {
		case final.Status == StatusCompleted && !transfer.Refuse:
			want[transfer.From] -= transfer.Amount
			want[transfer.To] += transfer.Amount
		case final.Status == StatusCompensated && transfer.Refuse:
		default:
			t.Errorf("saga %s (%+v) ended %s (%s)", bk, transfer, final.Status, final.LastError)
		}
	}
	t.Logf("terminal statuses: %v", statuses)
	return want
}

// ---- 用例 ----

func TestRealJetStreamNativeStepsEndToEnd(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 4, 2)
	conn, js := ptJetStream(t, url)
	owner := startPtCoordinator(t, conn, layout, func(o *Options) { o.SnapshotEveryEvents = 16 })
	const accounts = 8
	participant, err := openPtParticipant(t.TempDir(), nil, "pt", accounts)
	if err != nil {
		t.Fatal(err)
	}
	participant.unknownAfterCommit = 0.2
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := participant.subscribe(ctx, js, layout); err != nil {
		t.Fatal(err)
	}
	// 一个从来没有命令的步骤 durable（下线前的步骤、低频主题）：它不能挡住分区清理。
	idle, err := SubscribeNativeStep(ctx, js, layout, StepConsumerConfig{Durable: "pt-idle", Topic: "pt.idle"}, participant.body)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, sub := range idle {
			sub.Stop()
		}
		_ = participant.close()
	})

	transfers := ptStartAll(t, owner, 60, 6, accounts, nil)
	want := ptExpectedBalances(t, owner, transfers, accounts, 60*time.Second)
	for n := range int64(accounts) {
		if got := participant.balance(t, n); got != want[n] {
			t.Errorf("account %d balance %d, want %d", n, got, want[n])
		}
	}
	if participant.injected.Load() == 0 {
		t.Fatal("no unknown outcome was injected; the replay path was not exercised")
	}
	// 60 笔、每笔两步正向（拒绝的再加一步补偿），业务执行次数恰好等于操作数：注入的结果未知全部由回放收尾。
	operations := int64(0)
	for _, transfer := range transfers {
		operations += 2
		if transfer.Refuse {
			operations++
		}
	}
	if got := participant.executed.Load(); got != operations {
		t.Fatalf("business executed %d times for %d operations (injected %d unknown outcomes)", got, operations, participant.injected.Load())
	}
	// 清理：快照之后分区按 durable 的 floor 清理；空闲的 pt-idle 不能把界限压在 1。
	deadline := time.Now().Add(20 * time.Second)
	for owner.Stats().Purges == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if s := owner.Stats(); s.Snapshots == 0 || s.Purges == 0 {
		t.Fatalf("stats = %+v, want snapshots followed by purges with an idle step durable on every stream", s)
	}
	t.Logf("executed=%d injected=%d stats=%+v", participant.executed.Load(), participant.injected.Load(), owner.Stats())
}

const ptChildEnv = "ROOST_SAGA_JS_PARTICIPANT_CHILD"

// TestRealJetStreamParticipantChild 是 kill -9 用例拉起的参与方子进程：按环境变量打开 WAL + 投影 + Nest，订阅步骤，打印 READY；
// 收到 SIGTERM 时排空并把 WAL 投影完再退出（打印 STOPPED）。直接运行时跳过。
func TestRealJetStreamParticipantChild(t *testing.T) {
	if os.Getenv(ptChildEnv) != "1" {
		t.Skip("child role of TestRealJetStreamParticipantKill9")
	}
	partitions, _ := strconv.Atoi(os.Getenv("ROOST_SAGA_JS_PARTITIONS"))
	groups, _ := strconv.Atoi(os.Getenv("ROOST_SAGA_JS_GROUPS"))
	accounts, _ := strconv.Atoi(os.Getenv("ROOST_SAGA_JS_ACCOUNTS"))
	layout := Layout{Prefix: os.Getenv("ROOST_SAGA_JS_PREFIX"), Stream: os.Getenv("ROOST_SAGA_JS_STREAM"), Partitions: partitions, Groups: groups}
	mongo := ptMongoClient(t)
	participant, err := openPtParticipant(os.Getenv("ROOST_SAGA_JS_WAL"), mongo, os.Getenv("ROOST_SAGA_JS_DB"), accounts)
	if err != nil {
		t.Fatal(err)
	}
	// 一部分提交后报“结果未知”：重投（含重启之后从 Mongo 加载的回执）必须回放而不是再执行。
	participant.unknownAfterCommit = 0.1
	_, js := ptJetStream(t, os.Getenv("ROOST_NATS_CLUSTER_URL"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := participant.subscribe(ctx, js, layout); err != nil {
		t.Fatal(err)
	}
	cancel()
	fmt.Println("READY")
	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGTERM)
	<-terminate
	if err := participant.close(); err != nil {
		t.Fatal(err)
	}
	fmt.Println("STOPPED")
}

func ptMongoClient(t *testing.T) fmongo.IMongo {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; source ~/.roost-it/roost-dataengine-it/env.sh")
	}
	client, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

func ptDatabase(t *testing.T, client fmongo.IMongo) string {
	t.Helper()
	database := fmt.Sprintf("roost_sagajs3_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		// 被 kill -9 的进程留下的投影事务持锁到 transactionLifetimeLimitSeconds，dropDatabase 要等它。
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
	})
	return database
}

type ptChild struct {
	cmd     *exec.Cmd
	stopped chan bool
}

func startPtChild(t *testing.T, layout Layout, walDir, database string, accounts int) *ptChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRealJetStreamParticipantChild$", "-test.v", "-test.timeout=15m")
	cmd.Env = append(os.Environ(), ptChildEnv+"=1",
		"ROOST_SAGA_JS_PREFIX="+layout.Prefix, "ROOST_SAGA_JS_STREAM="+layout.Stream,
		"ROOST_SAGA_JS_PARTITIONS="+strconv.Itoa(layout.Partitions), "ROOST_SAGA_JS_GROUPS="+strconv.Itoa(layout.Groups),
		"ROOST_SAGA_JS_ACCOUNTS="+strconv.Itoa(accounts), "ROOST_SAGA_JS_WAL="+walDir, "ROOST_SAGA_JS_DB="+database)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &ptChild{cmd: cmd, stopped: make(chan bool, 1)}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			switch scanner.Text() {
			case "READY":
				ready <- true
			case "STOPPED":
				child.stopped <- true
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("participant child exited before READY")
		}
	case <-time.After(4 * time.Minute):
		t.Fatal("participant child not ready")
	}
	return child
}

func (c *ptChild) kill9() {
	_ = c.cmd.Process.Signal(syscall.SIGKILL)
	_, _ = c.cmd.Process.Wait()
}

// stop 发 SIGTERM，等子进程排空、把 WAL 投影完并退出。
func (c *ptChild) stop(t *testing.T) {
	t.Helper()
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.stopped:
	case <-time.After(2 * time.Minute):
		t.Fatal("participant child did not stop")
	}
	if state, err := c.cmd.Process.Wait(); err != nil || !state.Success() {
		t.Fatalf("participant child exited: %v %v", state, err)
	}
}

func TestRealJetStreamParticipantKill9(t *testing.T) {
	url := realJSURL(t)
	mongo := ptMongoClient(t)
	database := ptDatabase(t, mongo)
	layout := realLayout(t, url, 4, 2)
	conn, _ := ptJetStream(t, url)
	owner := startPtCoordinator(t, conn, layout, func(o *Options) { o.SnapshotEveryEvents = 64 })
	const accounts = 16
	walDir := t.TempDir()
	first := startPtChild(t, layout, walDir, database, accounts)
	var killed atomic.Bool
	restarted := make(chan *ptChild, 1)
	transfers := ptStartAll(t, owner, 300, 8, accounts, func(i int) {
		if i >= 100 && killed.CompareAndSwap(false, true) {
			first.kill9() // 在途的 Nest 事务、未发的结果、未投影的 WAL 都在这一刻
			go func() {
				if d, _ := time.ParseDuration(os.Getenv("ROOST_SAGA_JS_RESTART_DELAY")); d > 0 {
					time.Sleep(d) // 诊断：参与方停得比步骤预算的一部分还久
				}
				restarted <- startPtChild(t, layout, walDir, database, accounts)
			}()
		}
	})
	var second *ptChild
	select {
	case second = <-restarted:
	case <-time.After(5 * time.Minute):
		t.Fatal("the participant did not restart")
	}
	want := ptExpectedBalances(t, owner, transfers, accounts, 3*time.Minute)
	second.stop(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var docs []ptAccountDoc
	if err := mongo.Database(database).Collection("account").Find(ctx, bson.M{}, &docs); err != nil {
		t.Fatal(err)
	}
	if len(docs) != accounts {
		t.Fatalf("Mongo has %d accounts, want %d", len(docs), accounts)
	}
	receipts := 0
	for _, doc := range docs {
		n := int64(-1)
		for i := range int64(accounts) {
			if ptAccountID(i) == doc.ID {
				n = i
			}
		}
		if n < 0 {
			t.Fatalf("unexpected account %d", doc.ID)
		}
		if doc.Balance != want[n] {
			t.Errorf("account %d: Mongo balance %d, want %d (an operation took effect twice or not at all)", n, doc.Balance, want[n])
		}
		receipts += len(doc.Receipts)
	}
	t.Logf("after participant kill -9: %d receipts in Mongo, stats=%+v", receipts, owner.Stats())
}

// 同一操作的两次尝试同时进 Mongo 事务：handler 都可能跑，但只有一笔提交业务写；另一笔写回执时撞上写冲突，整个回调重跑，读到
// 前者的回执回放。核对业务写（ledger）恰好一份、两次的结论一致。
func TestRealMongoStepInboxSerializesConcurrentAttempts(t *testing.T) {
	mongo := ptMongoClient(t)
	database := ptDatabase(t, mongo)
	inbox, err := NewMongoStepInbox(mongo, database, "_pt_step_receipts", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	ledger := mongo.Database(database).Collection("ledger")
	var runs atomic.Int32
	// 每轮新建：上一轮重跑留下的进入信号不能让下一轮提前放行。
	var entered, release chan struct{}
	handler := func(ctx context.Context, command Command) (Completion, error) {
		runs.Add(1)
		if _, err := ledger.InsertOne(ctx, bson.M{"_id": command.ID, "op": command.IdempotencyKey}); err != nil {
			return Completion{}, err
		}
		entered <- struct{}{}
		<-release // 两笔事务都写了业务、都还没写回执
		return Completion{Success: true, Data: []byte(command.ID)}, nil
	}
	for round := range 3 {
		op := fmt.Sprintf("saga-1:1:%d", round)
		a, b := stepCommand(0, 1, time.Minute), stepCommand(0, 2, time.Minute)
		a.IdempotencyKey, a.ID = op, op+":1"
		b.IdempotencyKey, b.ID = op, op+":2"
		entered, release = make(chan struct{}, 8), make(chan struct{})
		results := make(chan StepOutcome, 2)
		failures := make(chan error, 2)
		for _, command := range []Command{a, b} {
			go func() {
				outcome, err := inbox.Handle(ctx, command, handler)
				if err != nil {
					failures <- err
					return
				}
				results <- outcome
			}()
		}
		<-entered
		<-entered
		close(release)
		var outcomes []StepOutcome
		for range 2 {
			select {
			case o := <-results:
				outcomes = append(outcomes, o)
			case err := <-failures:
				t.Fatalf("round %d: %v", round, err)
			}
		}
		// 重跑的那笔可能又进 handler 一次（读快照时前者还没提交）：放行之后的进入不再阻塞。
		if outcomes[0].Completion.CommandID != outcomes[1].Completion.CommandID || (outcomes[0].Kind == StepExecuted) == (outcomes[1].Kind == StepExecuted) {
			t.Fatalf("round %d outcomes %+v / %+v, want one execution and one replay of it", round, outcomes[0], outcomes[1])
		}
		rows, err := ledger.CountDocuments(ctx, bson.M{"op": op})
		if err != nil || rows != 1 {
			t.Fatalf("round %d: ledger has %d rows for the operation (%v), want exactly 1", round, rows, err)
		}
	}
	t.Logf("handler ran %d times over 3 rounds of two concurrent attempts", runs.Load())
}

// LoadPublisher：积压超过高水位关闭准入，回落到低水位重开；Remove 删键后协调器不再计入（不等过期）。
func TestRealJetStreamLoadPublisherDrivesAdmission(t *testing.T) {
	url := realJSURL(t)
	layout := realLayout(t, url, 1, 1)
	conn := dialRealJS(t, url)
	admission := AdmissionOptions{Enabled: true, HighRecords: 100, LowRecords: 50, StaleAfter: time.Second, SignalTTL: 10 * time.Second, HintEvery: 200 * time.Millisecond}
	if os.Getenv("ROOST_SAGA_MUTATE") == "noadmit" {
		admission.Enabled = false // 变异：协调器不做准入，积压再高也不关闭，本用例必须变红
	}
	owner := startRealEngine(t, conn, nil, func() Options {
		o := realEngineOptions(layout, []int{0})
		o.Admission = admission
		return o
	}())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	kv, err := conn.log.KeyValue(ctx, loadBucketConfig(layout, 3, admission.withDefaults()))
	if err != nil {
		t.Fatal(err)
	}
	var backlog atomic.Uint64
	publisher, err := NewLoadPublisher(kv, "game.1", 100*time.Millisecond, backlog.Load)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLoadPublisher(kv, "bad name", 0, backlog.Load); err == nil {
		t.Fatal("a name with a space was accepted")
	}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); publisher.Run(runCtx) }()
	waitAdmission := func(open bool, reason string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if s := owner.AdmissionState(); s.Open == open && s.Reason == reason {
				return
			}
		}
		t.Fatalf("admission never became open=%v %q: %+v", open, reason, owner.AdmissionState())
	}
	waitAdmission(true, "")
	backlog.Store(150)
	waitAdmission(false, AdmissionReasonBacklog)
	backlog.Store(70) // 滞回：低水位之上仍关
	time.Sleep(500 * time.Millisecond)
	if s := owner.AdmissionState(); s.Open {
		t.Fatalf("admission reopened above the low water mark: %+v", s)
	}
	backlog.Store(10)
	waitAdmission(true, "")
	backlog.Store(500)
	waitAdmission(false, AdmissionReasonBacklog)
	stop()
	<-done
	if err := publisher.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	// 键删掉：不经过“停更”，直接不再计入。
	waitAdmission(true, "")
}
