package remoteflow

// OPEN-ITEMS C34 / REMAINING-2026-09-28 B27 第 1 批：生成链路上的 Remote 持久拒绝 + 订阅者。
//
// TestGeneratedRemoteNestFlow 的 vaultLoader 按 map 查实例、不支持卸载，拒绝收尾只能走 ErrRemoteUnloadUnsupported 的兼容分支
// （C19）。这里换成正式装配：ManagerAccess 同时是 Nest getter 与 Remote loader（Runtime.Start 给它装上 EntityRepository，
// 冷加载从真实 Mongo 读 Remote 信封文档），entitysync.Manager 接进 Nest，ConfigureUnloadResync 与 OnEntityLoaded → Rebind
// 按 kit nest Mod 的顺序接线。实体是生成的 SyncedVault（Remote 托管、sync=true）。
//
// 拒绝由两个写失败注入点给出，其余全部是正式链路与真实 Mongo / Redis / NATS：
//   - memory（Durability 0）：Remote 存储适配器替身 unreachableCommitter 让提交“没到达 Mongo”（直接返回 context.DeadlineExceeded，
//     不发任何写），与 RR-28 单测的 switchableStorage 同一形状。之后 finalizer 回源真实 Mongo 得 Unknown，写真实 Rejected 记录
//     （RejectUnresolvedRemoteCommits），这是当前正式链路上新写入唯一能到达的持久拒绝（RR-59 更正节）。
//   - async / strict / pipelined（Durability 1/2/3）：新写入在正式投影器上得不到持久拒绝（CAS 不命中走 ErrProjectionConflict fatal；
//     Remote + lease fence 混合记录在准入处被拒）。投影存储适配器替身 rejectingProjection 对选中的记录做 lease fence 跳过时
//     MongoStore 做的两件事——在 Mongo 事务里经 Backend.RejectRemoteCommitsInTransaction 写 Rejected 记录（不写数据），提交后
//     Manager.RejectRemoteTransaction——然后按已投影返回（不报 skipped，不触发 RR-30 驱逐，卸载只可能来自 Remote finalizer）。
//     它不写 DataEngine 事务标记，不模拟重启重放。
//
// 断言：实例被仅内存卸载；订阅者收到重载后的全量（Durability 1 先收到推测性的被拒绝内容，其余级别什么都没收到），
// 权威里没有该实体时收到 remove；重载后实体可写，被拒绝的修改没有写出（Mongo 与内存都等于“拒绝前 + 下一笔”）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	natsdriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
	"github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
	syncdriver "github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// faultMode 是注入点对被选中提交的处理方式。
type faultMode int

const (
	// faultReject：Remote 存储适配器上是“没到达 Mongo”；投影适配器上是持久拒绝。
	faultReject faultMode = iota
	// faultLoseReply：Remote 存储适配器上提交真实落库，但回复丢失（调用方看到截止，结果未知）。
	faultLoseReply
	// faultHold：投影适配器上只拖住这条记录，放行后照常投影（strict 确认等到截止用）。
	faultHold
)

// faultGate 是两个注入点共用的开关：arm 选中一个实体，命中的第一笔提交记下事务 ID、通知 held，并停在 release 之前。
type faultGate struct {
	mu      sync.Mutex
	armed   int64
	mode    faultMode
	tx      entity.RemoteTransactionID
	held    chan struct{}
	release chan struct{}
	opened  *sync.Once
}

func (g *faultGate) arm(id int64, mode faultMode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed, g.mode, g.tx = id, mode, entity.RemoteTransactionID{}
	g.held, g.release, g.opened = make(chan struct{}), make(chan struct{}), new(sync.Once)
}

// open 放行被拦住的提交，可重复调用；用例失败时由 t.Cleanup 调用，避免停机等在注入点上。
func (g *faultGate) open() {
	g.mu.Lock()
	opened, release := g.opened, g.release
	g.mu.Unlock()
	if opened != nil {
		opened.Do(func() { close(release) })
	}
}

func (g *faultGate) disarm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = 0
}

// take 在提交含选中实体时消费开关（只拦一次），记下事务 ID，返回 held/release 与处理方式。
func (g *faultGate) take(commits []entity.RemoteCommit) (held, release chan struct{}, mode faultMode, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.armed == 0 || !slices.ContainsFunc(commits, func(c entity.RemoteCommit) bool { return c.EntityID == g.armed }) {
		return nil, nil, 0, false
	}
	g.armed, g.tx = 0, commits[0].TransactionID
	return g.held, g.release, g.mode, true
}

func (g *faultGate) transaction() entity.RemoteTransactionID {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tx
}

// unreachableCommitter 是 Remote 存储适配器替身：选中实体的 Durability 0 提交按 faultReject 不发任何 Mongo 写、按“发出前截止”
// 返回，按 faultLoseReply 真实落库后丢掉回复；该事务的回源（finalizer 的 CommitStatus）停在 release 之前，用例据此在持久结论
// 之前检查订阅者与提交后工作。其余方法原样转给正式 MongoCommitter。
type unreachableCommitter struct {
	*remoteentity.MongoCommitter
	gate faultGate
	// pending 是被拦截事务的 release；CommitStatus 命中该事务时等它。
	pending atomic.Pointer[chan struct{}]
}

func (c *unreachableCommitter) CommitRemoteBatch(ctx context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	if held, release, mode, ok := c.gate.take(commits); ok {
		c.pending.Store(&release)
		defer close(held)
		if mode == faultLoseReply {
			if _, err := c.MongoCommitter.CommitRemoteBatch(ctx, commits); err != nil {
				return nil, err
			}
		}
		return nil, context.DeadlineExceeded
	}
	return c.MongoCommitter.CommitRemoteBatch(ctx, commits)
}

func (c *unreachableCommitter) CommitRemote(ctx context.Context, commit entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	receipts, err := c.CommitRemoteBatch(ctx, []entity.RemoteCommit{commit})
	if err != nil {
		return entity.RemoteCommitReceipt{}, err
	}
	if len(receipts) != 1 {
		return entity.RemoteCommitReceipt{}, entity.ErrRemotePersistenceIndeterminate
	}
	return receipts[0], nil
}

func (c *unreachableCommitter) CommitStatus(ctx context.Context, id entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	if release := c.pending.Load(); release != nil && id == c.gate.transaction() {
		select {
		case <-*release:
		case <-ctx.Done():
			return entity.RemoteCommitStatus{}, ctx.Err()
		}
	}
	return c.MongoCommitter.CommitStatus(ctx, id)
}

// rejectingProjection 是投影存储适配器替身：选中实体的记录停在 release 之前；faultReject 按 lease fence 跳过的形状给出
// Remote 持久拒绝（真实 Mongo 事务里写 Rejected 记录，提交后通知 Manager），不写数据，按已投影返回；faultHold 放行后照常投影。
type rejectingProjection struct {
	*engine.MongoStore
	client  fmongo.IMongo
	backend *remoteentity.Backend
	manager *remoteentity.Manager
	gate    faultGate
}

const injectedRejectCause = "remoteflow: injected durable rejection"

func (p *rejectingProjection) Project(ctx context.Context, record coredata.CommitRecord) error {
	_, err := p.ProjectFenced(ctx, record)
	return err
}

func (p *rejectingProjection) ProjectFenced(ctx context.Context, record coredata.CommitRecord) (bool, error) {
	var remote []entity.RemoteCommit
	for _, mutation := range record.Mutations {
		if mutation.Remote != nil {
			remote = append(remote, mutation.Remote.Clone())
		}
	}
	held, release, mode, ok := p.gate.take(remote)
	if !ok {
		return p.MongoStore.ProjectFenced(ctx, record)
	}
	close(held)
	select {
	case <-release:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	if mode == faultHold {
		return p.MongoStore.ProjectFenced(ctx, record)
	}
	session, err := p.client.StartSession(ctx)
	if err != nil {
		return false, err
	}
	defer session.EndSession(ctx)
	if err := session.WithTransaction(ctx, func(txCtx context.Context) error {
		return p.backend.RejectRemoteCommitsInTransaction(txCtx, remote, injectedRejectCause)
	}); err != nil {
		return false, err
	}
	p.manager.RejectRemoteTransaction(entity.RemoteTransactionID(record.ID), injectedRejectCause)
	return false, nil
}

// vaultFrames 记录 entitysync 推给会话 1 的帧。
type vaultFrames struct {
	mu     sync.Mutex
	frames [][]byte
}

func (r *vaultFrames) push(_ context.Context, _ entitysync.SessionID, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, append([]byte(nil), data...))
	return nil
}

type vaultFrame struct {
	operation frame.ObjectOperation
	ref       frame.ObjectRef
	full      bool
	balance   int64
	items     int64
}

func (f vaultFrame) String() string {
	return fmt.Sprintf("{op=%v full=%v balance=%d items=%d}", f.operation, f.full, f.balance, f.items)
}

type rejectRig struct {
	ctx       context.Context
	mongo     fmongo.IMongo
	database  string
	policy    nest.DurabilityPolicy
	access    *entity.ManagerAccess
	redis     *scopedRedis
	committer *unreachableCommitter
	projector *rejectingProjection
	manager   *remoteentity.Manager
	runtime   *engine.Runtime
	wal       *nestwal.WAL
	sync      *entitysync.Manager
	recorder  *vaultFrames
	scheduler *nest.NestMgr
	// localCommitter 是 Runtime 交给 Nest 的正式提交器（Projector），RunIsolatedTransaction 用例用它。
	localCommitter *engine.Projector
	write          nest.HandlerName
	// fatal 收 Runtime 的 onFatal（kit 里就是 Nest.Fence + RuntimeFailure）。
	fatal chan error
	// afterCommit / afterCommitOnFast 记 write handler 登记的 AfterCommit 执行次数与其中在快 worker 上的次数。
	afterCommit, afterCommitOnFast atomic.Int32
	// ref 是订阅者持有的对象，remove 帧只有 ref 没有内容。
	ref frame.ObjectRef
}

// newRejectRig 装配一套正式链路；register 在 Nest 启动前登记用例自己的 handler（可为 nil）；extra 追加 Nest 选项（例如阶段指标）。
func newRejectRig(t *testing.T, ctx context.Context, mongo fmongo.IMongo, redis fredis.IRedis, database string, policy nest.DurabilityPolicy, walOptions func(*nestwal.Options), register func(*rejectRig), extra ...nest.NestOption) *rejectRig {
	t.Helper()
	rig := &rejectRig{ctx: ctx, mongo: mongo, database: database, policy: policy, recorder: &vaultFrames{}, fatal: make(chan error, 8)}
	scoped := &scopedRedis{IRedis: redis, prefix: database + ":reject:" + policy.String() + ":"}
	rig.redis = scoped
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		scoped.keys.Range(func(key, _ any) bool {
			if _, err := redis.Del(cleanup, key.(string)); err != nil {
				t.Errorf("Redis cleanup: %v", err)
			}
			return true
		})
	})
	rig.access = entity.NewManagerAccess(entity.NewEntityManager())
	rig.committer = &unreachableCommitter{MongoCommitter: remoteentity.NewMongoCommitter(mongo, database, 1000, 0)}
	// 正式装配：ManagerAccess 是 Remote loader（支持 UnloadRemoteEntity），不再是按 map 查的 vaultLoader。
	backend, err := remoteentity.NewBackend(rig.access, rig.committer)
	if err != nil {
		t.Fatal(err)
	}
	cfg := remoteentity.DefaultConfig()
	cfg.LockTTL = 3 * time.Second
	assembly, err := remoteentity.Assemble(remoteentity.AssemblyDeps{Redis: scoped, Backend: backend}, cfg, 1000, remoteentity.MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	rig.manager = assembly.Manager
	natsClient, err := natsdriver.NewClient(fnats.DefaultConfig(remoteNatsURL()), natsdriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(natsClient.Close)
	// core subject，前缀带本次隔离库名；NewNatsSyncBus 不建 JetStream 流。
	if err := assembly.Start(ctx, syncdriver.NewNatsSyncBus(natsClient, 1000, database+".reject."+policy.String())); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := assembly.Stop(context.Background()); err != nil {
			t.Errorf("Remote stop: %v", err)
		}
	})
	store, err := engine.NewMongoStore(mongo, engine.MongoStoreConfig{DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.SetRemoteProjection(backend, assembly.Manager); err != nil {
		t.Fatal(err)
	}
	rig.projector = &rejectingProjection{MongoStore: store, client: mongo, backend: backend, manager: assembly.Manager}
	opts := nestwal.DefaultOptions(t.TempDir())
	opts.WriterVersion = nestwal.WriterVersionV2
	if walOptions != nil {
		walOptions(&opts)
	}
	rig.wal, err = nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rig.wal.Close(context.Background()) })
	projector, err := engine.NewProjector(rig.wal, rig.projector, engine.ProjectorOptions{CloseWAL: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projector.Close(context.Background()) })
	outboxStore, err := engine.NewMongoOutboxStore(store)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := engine.NewOutboxWorker(outboxStore, noEffects{}, engine.OutboxWorkerOptions{Owner: "remote-reject"})
	if err != nil {
		t.Fatal(err)
	}
	rig.localCommitter = projector
	rig.write = nest.NewHandlerName("remote_reject_write")
	onFatal := func(err error) {
		select {
		case rig.fatal <- err:
		default:
		}
	}
	rig.runtime, err = engine.NewRuntime(store, rig.wal, projector, outbox, rig.access, assembly.Manager, onFatal, engine.PipelinedRuntimeConfig{Allowlist: []string{rig.write.String()}, Async: true, AsyncWorkers: 2, AsyncQueueCap: 32})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rig.runtime.Shutdown(context.Background()); err != nil {
			t.Errorf("runtime shutdown: %v", err)
		}
	})
	// Sync 按 kit nest Mod 的接线：Nest 接 entitysync；重载出来的实体经 OnEntityLoaded → Rebind；卸载后仍有订阅者时框架重载。
	rig.sync, err = entitysync.NewManager(entitysync.ManagerConfig{Transport: entitysync.TransportFunc(rig.recorder.push)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rig.sync.Close(context.Background()) })
	unhook := rig.runtime.Repository.OnEntityLoaded(func(loaded entity.IThreadSafeEntity) {
		if state := loaded.Base().Sync(); state != nil {
			if err := rig.sync.Rebind(state); err != nil && !errors.Is(err, entitysync.ErrSubjectNotRegistered) && !errors.Is(err, entitysync.ErrSubjectRegistered) {
				t.Errorf("rebind reloaded %d: %v", loaded.ID(), err)
			}
		}
	})
	t.Cleanup(unhook)
	stopResync, err := rig.access.ConfigureUnloadResync(rig.sync, entity.UnloadResyncConfig{})
	if err != nil {
		t.Fatal(err)
	}
	options := append(rig.runtime.NestOptions(), nest.NestOptionWithGetter(rig.access), nest.NestOptionWithRemoteEntityManager(assembly.Manager),
		nest.NestOptionWithEntitySync(rig.sync), nest.NestOptionWithWorkerNumAndMsgCap(4, 256),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: 4, QueueCap: 256}, nest.WorkerPoolConfig{Workers: 4, QueueCap: 64}))
	options = append(options, extra...)
	rig.scheduler = nest.NewEngine(options...)
	rig.scheduler.MustRegisterHandlerWithMeta(rig.write, func(es []entity.IThreadSafeEntity, _ []any, _ ...nest.HandlerOption) (any, error) {
		for _, e := range es {
			vault := e.(*SyncedVault)
			vault.balance.SetValue(vault.balance.GetValue() + 1)
			vault.items.SetValue(vault.items.GetValue() + 1)
		}
		nest.AfterCommit(func() {
			rig.afterCommit.Add(1)
			if fctx.InFastWorker() {
				rig.afterCommitOnFast.Add(1)
			}
		})
		return nil, nil
	}, nest.HandlerMeta{Rollback: nest.RollbackState, Durability: policy})
	if register != nil {
		register(rig)
	}
	if err := rig.scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	// 与 kit 的停机顺序相同：先停重载，再停 Nest。t.Cleanup 后进先出，所以登记在 Nest 启动之后。
	t.Cleanup(func() { _ = rig.scheduler.Shutdown(context.Background()) })
	t.Cleanup(func() {
		if err := stopResync(context.Background()); err != nil {
			t.Errorf("stop unload resync: %v", err)
		}
	})
	return rig
}

// seed 在内存建一个 SyncedVault，登记同步并让会话 1 订阅，建立所有权；返回它的 ID 与订阅者收到的 create。
func (rig *rejectRig) seed(t *testing.T, raw int64) int64 {
	t.Helper()
	id, err := entity.BuildEntityID(raw, EntityKindSyncedVault)
	if err != nil {
		t.Fatal(err)
	}
	created, err := rig.access.Create(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindSyncedVault, Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.sync.Register(created.Base().Sync()); err != nil {
		t.Fatal(err)
	}
	if err := rig.sync.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err := rig.sync.Subscribe(1, id, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	got := rig.flushUntil(t, id, "the initial create")
	if len(got) != 1 || got[0].operation != frame.ObjectCreate || got[0].balance != 0 {
		t.Fatalf("first frame=%v, want one create with balance=0", got)
	}
	rig.ref = got[0].ref
	prepareRemoteOwnership(t, rig.ctx, rig.manager, []int64{id})
	return id
}

// take 解出目前为止发给会话 1、属于该实体的帧并清空记录。
func (rig *rejectRig) take(t *testing.T, id int64) []vaultFrame {
	t.Helper()
	rig.recorder.mu.Lock()
	frames := rig.recorder.frames
	rig.recorder.frames = nil
	rig.recorder.mu.Unlock()
	var out []vaultFrame
	for _, data := range frames {
		decoded, err := entitysync.DecodeFrame(data, frame.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range decoded.Objects {
			if len(object.Components) == 0 {
				if object.Ref == rig.ref {
					out = append(out, vaultFrame{operation: object.Operation, ref: object.Ref})
				}
				continue
			}
			for _, component := range object.Components {
				update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
				if err != nil {
					t.Fatal(err)
				}
				if update.SubjectID != id {
					continue
				}
				var snapshot syncedVaultSnapshot
				if err := bson.Unmarshal(update.Payload.BytesCopy(), &snapshot); err != nil {
					t.Fatal(err)
				}
				out = append(out, vaultFrame{operation: object.Operation, ref: object.Ref, full: update.Full, balance: snapshot.Balance, items: snapshot.Items})
			}
		}
	}
	return out
}

// flushUntil 反复 Flush 直到该实体有帧（重载在后台 worker 上完成，只等事件发生，截止是挂死保护）。
func (rig *rejectRig) flushUntil(t *testing.T, id int64, what string) []vaultFrame {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := rig.sync.Flush(rig.ctx); err != nil {
			t.Fatal(err)
		}
		if got := rig.take(t, id); len(got) > 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("no sync frame within 15s: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (rig *rejectRig) flushNone(t *testing.T, id int64, what string) {
	t.Helper()
	if err := rig.sync.Flush(rig.ctx); err != nil {
		t.Fatal(err)
	}
	if got := rig.take(t, id); len(got) != 0 {
		t.Fatalf("%s: subscriber received %v", what, got)
	}
}

// eventually 等条件成立；截止只是挂死保护，结论来自条件本身。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not reached within 15s: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (rig *rejectRig) request(ctx context.Context, id int64) error {
	_, err := rig.scheduler.Request(ctx, rig.write, id, nil)
	return err
}

// writeAfterReload 在卸载 / 重载窗口里只重试 RR-62 的可重试哨兵，其他错误直接失败。
func (rig *rejectRig) writeAfterReload(t *testing.T, id int64) {
	t.Helper()
	for attempt := 0; ; attempt++ {
		err := rig.request(rig.ctx, id)
		if err == nil {
			return
		}
		if !errors.Is(err, entity.ErrRemoteEntityReloading) || attempt == 50 {
			t.Fatalf("write after the reload (attempt %d): %v", attempt, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (rig *rejectRig) vault(t *testing.T, id int64) *SyncedVault {
	t.Helper()
	value, _ := rig.access.Manager().Get(id).(*SyncedVault)
	return value
}

func (rig *rejectRig) memory(t *testing.T, id int64) (int64, int64) {
	t.Helper()
	vault := rig.vault(t, id)
	if vault == nil {
		t.Fatalf("SyncedVault %d is not resident", id)
	}
	vault.GetMutex().Lock()
	defer vault.GetMutex().Unlock()
	return vault.balance.GetValue(), vault.items.GetValue()
}

// stored 读真实 Mongo 里两份 Remote 信封文档；found=false 表示两份都不存在。
func (rig *rejectRig) stored(t *testing.T, id int64) (version uint64, balance, items int64, found bool) {
	t.Helper()
	values := make([]int64, 0, 2)
	for _, collection := range []string{"remote_balances", "remote_items"} {
		var doc struct {
			Version uint64 `bson:"_ver"`
			Data    []byte `bson:"data"`
		}
		err := rig.mongo.Database(rig.database).Collection(collection).FindOne(rig.ctx, bson.M{"_id": id}, &doc)
		if errors.Is(err, fmongo.ErrNotFound) {
			if len(values) != 0 {
				t.Fatalf("Mongo holds %s/%d partially", collection, id)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var data struct {
			Value int64 `bson:"value"`
		}
		if err := bson.Unmarshal(doc.Data, &data); err != nil {
			t.Fatal(err)
		}
		if len(values) > 0 && doc.Version != version {
			t.Fatalf("Mongo DAO versions diverge for %d: %d vs %d", id, version, doc.Version)
		}
		version = doc.Version
		values = append(values, data.Value)
	}
	if len(values) == 0 {
		return 0, 0, 0, false
	}
	if len(values) != 2 {
		t.Fatalf("Mongo holds %d/2 DAO documents for %d", len(values), id)
	}
	return version, values[0], values[1], true
}

func (rig *rejectRig) lockFence(t *testing.T, id int64) uint64 {
	t.Helper()
	var doc struct {
		Fence uint64 `bson:"_lock_fence"`
	}
	if err := rig.mongo.Database(rig.database).Collection("remote_balances").FindOne(rig.ctx, bson.M{"_id": id}, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Fence
}

func (rig *rejectRig) assertRejectedRecord(t *testing.T, tx entity.RemoteTransactionID) {
	t.Helper()
	if tx.IsZero() {
		t.Fatal("the injection point never saw the rejected transaction")
	}
	status, err := rig.committer.MongoCommitter.CommitStatus(rig.ctx, tx)
	if err != nil || status.State != entity.RemoteCommitRejected {
		t.Fatalf("durable status of the rejected transaction=%+v err=%v, want a Rejected record in Mongo", status, err)
	}
}

func (rig *rejectRig) assertNoPending(t *testing.T) {
	t.Helper()
	if err := rig.runtime.Flush(rig.ctx); err != nil {
		t.Fatal(err)
	}
	if pending, err := rig.committer.PendingRemoteCommits(rig.ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("pending remote commits=%v err=%v", pending, err)
	}
}

func rejectTestEnv(t *testing.T) (context.Context, fmongo.IMongo, fredis.IRedis, string) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("run scripts/test-remote-generated.sh")
	}
	if remoteLoadEnabled() || os.Getenv("ROOST_REMOTE_FAULT") != "" {
		t.Skip("durable rejection cases do not run under the load / fault matrix")
	}
	database := NewBalanceDao().DbName()
	if !strings.HasPrefix(database, "roost_remote_generated_") || database == "roost_remote_generated_placeholder" {
		t.Fatal("isolated database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	mongo, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mongo.Database(database).Drop(cleanup); err != nil {
			t.Errorf("drop fixture: %v", err)
		}
		mongo.Close(context.Background())
	})
	redis, err := redisdriver.NewClient(fredis.DefaultConfig(os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = redis.Close() })
	RegisterEntity()
	return ctx, mongo, redis, database
}

// C34：持久拒绝后实例被仅内存卸载，订阅者收到重载后的权威全量，重载后可写且被拒绝的修改没有写出。
// memory 是正式链路上新写入唯一能到达的持久拒绝（Durability 0 结果未知 → finalizer 写 Rejected，RR-28 / 39 / 59）；
// async / strict / pipelined 由投影注入点给出拒绝（pipelined 即 RR-20260928-09：带 Remote 批次回退 strict 路径）。
func TestGeneratedRemoteDurableRejectionReloadsSubscribers(t *testing.T) {
	ctx, mongo, redis, database := rejectTestEnv(t)
	for index, policy := range []nest.DurabilityPolicy{nest.DurabilityMemory, nest.DurabilityAsync, nest.DurabilityStrict, nest.DurabilityPipelined} {
		if selected := os.Getenv("ROOST_REMOTE_POLICY"); selected != "" && selected != policy.String() {
			continue
		}
		t.Run(policy.String(), func(t *testing.T) {
			var walOptions func(*nestwal.Options)
			if policy == nest.DurabilityPipelined {
				// RR-20260928-11：回退 strict 路径的 pipelined 记录经 WAL.Append 提交时要在返回前 fsync。组提交间隔拉到 1 小时，
				// 让“投影器看到这条记录时 WAL 已 fsync”只可能来自 Append 自己的 requireSync，而不是周期 ticker。
				walOptions = func(opts *nestwal.Options) { opts.GroupCommitInterval = time.Hour }
			}
			rig := newRejectRig(t, ctx, mongo, redis, database, policy, walOptions, nil)
			id := rig.seed(t, int64(9100+index))

			// 第一笔正常提交：权威 version 1 / value 1，订阅者收到 1。
			if err := rig.request(ctx, id); err != nil {
				t.Fatal(err)
			}
			rig.assertNoPending(t)
			if version, balance, items, found := rig.stored(t, id); !found || version != 1 || balance != 1 || items != 1 {
				t.Fatalf("after the first write Mongo version=%d balance=%d items=%d found=%v", version, balance, items, found)
			}
			if got := rig.flushUntil(t, id, "the first committed write"); got[len(got)-1].balance != 1 {
				t.Fatalf("frames after the first write=%v", got)
			}
			old := rig.vault(t, id)

			// 第二笔被持久拒绝。
			gate := &rig.projector.gate
			if policy == nest.DurabilityMemory {
				gate = &rig.committer.gate
			}
			fenceBefore := rig.lockFence(t, id)
			afterCommitBefore := rig.afterCommit.Load()
			gate.arm(id, faultReject)
			t.Cleanup(gate.open)
			syncsBefore := rig.wal.Stats().Syncs
			replied := make(chan error, 1)
			go func() { replied <- rig.request(ctx, id) }()
			select {
			case <-gate.held:
			case <-ctx.Done():
				t.Fatal("the rejected write never reached the injection point")
			}
			switch policy {
			case nest.DurabilityMemory:
				// 从未到达 Mongo：回复“结果未知”，Sync 门冻结到持久结论，订阅者什么都没收到。
				if err := <-replied; !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
					t.Fatalf("never-reached Durability 0 write replied %v, want ErrRemotePersistenceIndeterminate", err)
				}
				rig.flushNone(t, id, "before the durable conclusion")
			case nest.DurabilityAsync:
				// Durability 1 在 Commit 返回时推测性确认 Sync：订阅者已收到之后会被拒绝的内容（RR-59 的前提）。
				if err := <-replied; err != nil {
					t.Fatalf("async write replied %v, want the speculative success", err)
				}
				if got := rig.flushUntil(t, id, "the speculative Durability 1 frame"); got[len(got)-1].balance != 2 {
					t.Fatalf("premise: speculative frames=%v, want balance=2", got)
				}
			default:
				// strict / pipelined 等投影器结论，锁外等待期间 Sync 没有确认，订阅者什么都没收到。
				rig.flushNone(t, id, "while the write waits for the projector")
				if policy == nest.DurabilityPipelined {
					if syncs := rig.wal.Stats().Syncs; syncs <= syncsBefore {
						t.Fatalf("RR-20260928-11: the pipelined Remote record reached the projector before its WAL fsync (syncs %d -> %d, group commit 1h)", syncsBefore, syncs)
					}
				}
			}
			gate.open()
			if policy == nest.DurabilityStrict || policy == nest.DurabilityPipelined {
				// RR-20260928-09：明确拒绝与 strict 同一回复（第 4 行，原因 ErrRemoteRejected）。
				if err := <-replied; !errors.Is(err, entity.ErrRemoteRejected) || !errors.Is(err, nest.ErrRemotePartRejected) {
					t.Fatalf("%s write rejected by the projector replied %v, want ErrRemotePartRejected + ErrRemoteRejected", policy, err)
				}
			}

			// finalizer 收尾：旧实例被仅内存卸载，订阅者仍在所以框架从权威重载并 Rebind。
			eventually(t, "the rejected instance is unloaded", old.IsRemoved)
			eventually(t, "the framework reloads the subscribed entity from Mongo", func() bool {
				fresh := rig.vault(t, id)
				return fresh != nil && fresh != old && rig.access.UnloadResyncStats().Reloaded >= 1
			})
			if stats := rig.access.UnloadResyncStats(); stats.Retracted != 0 || stats.Overflow != 0 {
				t.Fatalf("resync stats=%+v, want a reload without remove", stats)
			}
			got := rig.flushUntil(t, id, "the authoritative full after the reload")
			last := got[len(got)-1]
			if last.operation != frame.ObjectUpdate || !last.full || last.ref != rig.ref || last.balance != 1 || last.items != 1 {
				t.Fatalf("subscriber frames after the reload=%v, want a full ObjectUpdate of the held object with the authority value 1", got)
			}
			for _, f := range got {
				if f.balance == 2 || f.operation == frame.ObjectRemove {
					t.Fatalf("subscriber received %v after the rejection, want only the authority full", got)
				}
			}
			rig.assertRejectedRecord(t, gate.transaction())
			if version, balance, items, _ := rig.stored(t, id); version != 1 || balance != 1 || items != 1 {
				t.Fatalf("rejected write reached Mongo: version=%d balance=%d items=%d", version, balance, items)
			}
			if balance, items := rig.memory(t, id); balance != 1 || items != 1 {
				t.Fatalf("reloaded memory balance=%d items=%d, want the authority value 1", balance, items)
			}
			// RR-37：结论为拒绝时 AfterCommit 不执行。Durability 1 在 Commit 返回时已推测性确认（它的 AfterCommit 那时已执行），不在此列。
			if ran := rig.afterCommit.Load() - afterCommitBefore; policy != nest.DurabilityAsync && ran != 0 {
				t.Fatalf("AfterCommit ran %d time(s) for the rejected %s write", ran, policy)
			}

			// 重载后可写；被拒绝的 +1 没有随下一笔写出（否则是 3）。
			gate.disarm()
			rig.writeAfterReload(t, id)
			rig.assertNoPending(t)
			if version, balance, items, _ := rig.stored(t, id); version != 2 || balance != 2 || items != 2 {
				t.Fatalf("after the retried write Mongo version=%d balance=%d items=%d, want 2/2/2", version, balance, items)
			}
			if balance, items := rig.memory(t, id); balance != 2 || items != 2 {
				t.Fatalf("after the retried write memory balance=%d items=%d, want 2", balance, items)
			}
			if got := rig.flushUntil(t, id, "the write after the reload"); got[len(got)-1].balance != 2 {
				t.Fatalf("frames after the retried write=%v", got)
			}
			// RR-20260927-15 的前提：正式 authority 装配每次写都换新 fence，同一 fence 下的版本回退分支在这里不可达。
			if fence := rig.lockFence(t, id); fence <= fenceBefore {
				t.Fatalf("formal assembly reused lock fence %d -> %d across writes", fenceBefore, fence)
			}
			t.Logf("%s: rejected tx %v unloaded, reloaded from Mongo (resync %+v), subscriber got the authority full, next write 2/2", policy, gate.transaction(), rig.access.UnloadResyncStats())
		})
	}
}

// C34 remove 分支：事务内新建、权威里从未有过的实体被持久拒绝，卸载后重载得到“权威没有”，订阅者收到 remove（RR-59 §4）。
func TestGeneratedRemoteDurableRejectionOfUnpersistedEntityRemovesIt(t *testing.T) {
	ctx, mongo, redis, database := rejectTestEnv(t)
	rig := newRejectRig(t, ctx, mongo, redis, database, nest.DurabilityMemory, nil, nil)
	id := rig.seed(t, 9200)
	old := rig.vault(t, id)
	rig.committer.gate.arm(id, faultReject)
	t.Cleanup(rig.committer.gate.open)
	if err := rig.request(ctx, id); !errors.Is(err, entity.ErrRemotePersistenceIndeterminate) {
		t.Fatalf("never-reached write replied %v, want ErrRemotePersistenceIndeterminate", err)
	}
	rig.flushNone(t, id, "before the durable conclusion")
	rig.committer.gate.open()
	eventually(t, "the rejected instance is unloaded", old.IsRemoved)
	got := rig.flushUntil(t, id, "the remove after the reload found nothing")
	if len(got) != 1 || got[0].operation != frame.ObjectRemove || got[0].ref != rig.ref {
		t.Fatalf("subscriber frames=%v, want one ObjectRemove of the held object", got)
	}
	if stats := rig.access.UnloadResyncStats(); stats.Retracted != 1 || stats.Reloaded != 0 {
		t.Fatalf("resync stats=%+v, want one retract", stats)
	}
	rig.assertRejectedRecord(t, rig.committer.gate.transaction())
	if _, _, _, found := rig.stored(t, id); found {
		t.Fatal("rejected creation reached Mongo")
	}
	if rig.access.Manager().Get(id) != nil {
		t.Fatal("an entity the authority does not have is resident after the retract")
	}
	rig.assertNoPending(t)
}
