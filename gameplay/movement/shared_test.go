package movement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/gameplay/actionflow"
	"github.com/tjbdwanghaibo/roost-core/gameplay/ai"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// Remote 托管、可进入共享模式的测试实体种类。
const kindRemoteMover entity.EntityKind = 242

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category:     entity.EntityCategoryRemote,
		Kind:         kindRemoteMover,
		RemotePolicy: entity.RemotePolicyManaged,
		NoPersist:    true,
		Builder:      func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) { return nil, nil },
		Lifetime:     entity.EntityLifetimeRemoteManaged,
	})
}

type remoteMover struct {
	*entity.RemoteEntityBase
	mb    *MapBasic
	move  *Move
	flow  *actionflow.BeatRunner
	ended []endedAction
}

func (e *remoteMover) Base() *entity.EntityBase { return &e.RemoteEntityBase.EntityBase }
func (e *remoteMover) MoveComponent() *Move     { return e.move }

func transition(t *testing.T, e *remoteMover, states ...entity.RemoteOwnershipState) {
	t.Helper()
	for _, s := range states {
		if err := e.TransitionRemoteOwnership(s); err != nil {
			t.Fatal(err)
		}
	}
}

// Remote 托管实体的共享模式不允许挂心跳（维护者 2026-10-21）：
//   - 共享期间带周期落库的进场景返回 ErrHeartbeatRemoteShared，什么都不改；
//   - 共享期间启动需要周期 tick 的动作（MoveTo）：组的心跳挂不上，动作以失败结束、它的移动被停掉；
//   - 运行中进入共享模式、框架停掉了心跳：动作留在 runner 里、移动不推进，Sync 在共享期间报错，回到独占后重新挂上。
//
// 框架在下一拍停心跳由 gameplay/heartbeat 的用例覆盖，这里直接 Stop 登记模拟它的结果；不经 Nest、不在事务里。
func TestSharedRemoteEntityRefusesHeartbeatsAndResumesAfterLeavingSharedMode(t *testing.T) {
	d := newUnitDriver()
	id, err := entity.BuildEntityID(1, kindRemoteMover)
	if err != nil {
		t.Fatal(err)
	}
	e := &remoteMover{RemoteEntityBase: entity.NewRemoteEntityBase(id, entity.EntityCategoryRemote, true, kindRemoteMover)}
	transition(t, e, entity.RemoteOwnershipLocalOwned)
	host := d.NewHost(e)
	if e.mb, err = NewMapBasic(e, host, MapConfig{Fields: &MemFields{}, Save: func(int64, spatial.Point, Facing) error { return nil }, SaveEvery: time.Second}); err != nil {
		t.Fatal(err)
	}
	if e.move, err = NewMove(e.mb, MoveConfig{}); err != nil {
		t.Fatal(err)
	}
	if e.flow, err = actionflow.NewBeatRunner(actionflow.ActionRunnerConfig{Registry: newRegistry(t), GroupForKind: groupForKind,
		Hooks: actionflow.ActionRunnerHooks{
			OnEnded: func(s actionflow.ActionSnapshot, reason actionflow.ActionReason) {
				r := reason.ToActionResult(actionflow.ActionStatusSuccess)
				e.ended = append(e.ended, endedAction{id: s.ID, kind: s.Kind, status: r.Status, reason: r.Reason})
			},
			OnError: func(error) {},
		}}, actionflow.BeatConfig{Host: host}); err != nil {
		t.Fatal(err)
	}
	moveTo := func(goal spatial.Point) error {
		_, err := e.flow.Runner().Start(actMoveTo, MoveToParam{Path: []spatial.Point{goal}, Speed: 10}, 0, time.Time{})
		return err
	}

	// 共享模式下进场景（带周期落库）被拒，什么都没改。
	transition(t, e, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
	if err := e.mb.Enter(1, &spaceLog{}, spatial.Point{}); !errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) || e.mb.InScene() {
		t.Fatalf("enter in shared mode: %v in scene %v", err, e.mb.InScene())
	}
	transition(t, e, entity.RemoteOwnershipDraining, entity.RemoteOwnershipLocalOwned)
	if err := e.mb.Enter(1, &spaceLog{}, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	if err := moveTo(spatial.Point{X: 10}); err != nil || !e.flow.Reg(groupMove).Active() {
		t.Fatalf("move: %v active %v", err, e.flow.Reg(groupMove).Active())
	}

	// 运行中进入共享模式，框架停掉心跳（模拟）：动作与移动都还在，共享期间 Sync 报错，回到独占后重新挂上。
	transition(t, e, entity.RemoteOwnershipSharing, entity.RemoteOwnershipShared)
	e.flow.Reg(groupMove).Stop()
	if err := e.flow.Sync(); !errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) || e.flow.Reg(groupMove).Active() {
		t.Fatalf("runner sync in shared mode: %v active %v", err, e.flow.Reg(groupMove).Active())
	}
	if !e.move.Moving() || e.flow.Runner().Current(groupMove) == nil || len(e.ended) != 0 {
		t.Fatalf("the move or the action was dropped while the heartbeat was stopped: moving %v ended %+v", e.move.Moving(), e.ended)
	}

	// 共享期间启动新的移动：取代旧的（取消），新的挂不上心跳、以失败结束，它的移动被停掉。
	if err := moveTo(spatial.Point{X: 0, Y: 9}); err != nil {
		t.Fatal(err)
	}
	if len(e.ended) != 2 || e.ended[0].status != actionflow.ActionStatusCanceled || e.ended[1].status != actionflow.ActionStatusFailed ||
		!strings.Contains(e.ended[1].reason, "shared mode") || e.move.Moving() || e.flow.Runner().Current(groupMove) != nil {
		t.Fatalf("move started in shared mode: ended %+v moving %v", e.ended, e.move.Moving())
	}

	// 回到独占：照常挂心跳；之前停掉的组由 Sync 重新挂上。
	transition(t, e, entity.RemoteOwnershipDraining, entity.RemoteOwnershipLocalOwned)
	if err := moveTo(spatial.Point{X: 5}); err != nil || !e.flow.Reg(groupMove).Active() || !e.move.Moving() {
		t.Fatalf("move after leaving shared mode: %v active %v", err, e.flow.Reg(groupMove).Active())
	}
	e.flow.Reg(groupMove).Stop()
	if err := e.flow.Sync(); err != nil || !e.flow.Reg(groupMove).Active() {
		t.Fatalf("runner sync after leaving shared mode: %v active %v", err, e.flow.Reg(groupMove).Active())
	}
}

// ---------------------------------------------------------------------------------------------------------------
// 真实 remoteentity.Manager 下的所有权流转（D20 决定 ④、P2 第 6 条、D21 A6 / C4 的端到端验证）
//
// 上面的用例直接 Stop 登记模拟“框架停掉心跳”；下面的用例经真实的 remoteentity.Manager 做所有权流转：第一次写认领、
// EnterRemoteSharedMode / LeaveRemoteSharedMode / TransferRemoteOwnership、写准入（共享模式取分布式锁）与写门排空。
// 每个“进程”（npcProc）是一个真实 Nest 引擎 + 实体管理器 + 启动的心跳驱动 + 自己的 Manager；实体（remoteNPC）装了
// MapBasic（带周期落库）、Move、Behavior（动作组心跳、任务心跳）与 AI 心跳，接法与业务 NPC 相同。
//
// 进程内用例的 Remote 权威（所有权记录与版本锁，相当于两个进程连同一个 Redis）是 memRemoteAuthority；真实 Redis + Mongo
// 下的同一组场景见 shared_integration_test.go（-tags integration）。断言只看事件顺序、计数与最终状态；等待只用于等
// 异步执行，“这段时间里没有执行”用覆盖相关周期两倍以上的窗口（npcQuiet）观察。

const (
	kindRemoteNPC entity.EntityKind      = 243
	missionGoto   actionflow.MissionKind = 1
	npcAIPeriod                          = 200 * time.Millisecond
	npcWorkPeriod                        = 50 * time.Millisecond
	// npcQuiet 是观察“没有执行”的窗口：覆盖 AI（200ms）与动作组（50 / 100ms）的两个周期以上。
	npcQuiet = 500 * time.Millisecond
)

// npcRegistrations 是 npcStart 之后实体上在跳的心跳登记：移动组、工作组、任务、AI、周期落库。
var npcRegistrations = []string{"actionflow/1", "actionflow/2", "actionflow/mission", "ai", "map.save"}

var npcRunHandler = nest.NewHandlerName("movement_test.npc_run")

func init() {
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category:     entity.EntityCategoryRemote,
		Kind:         kindRemoteNPC,
		RemotePolicy: entity.RemotePolicyManaged,
		NoPersist:    true,
		Builder:      func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) { return nil, nil },
		Lifetime:     entity.EntityLifetimeRemoteManaged,
	})
	nest.MustRegisterHandlerWithMeta(npcRunHandler, func(es []entity.IThreadSafeEntity, params []any, _ ...nest.HandlerOption) (any, error) {
		return nil, params[0].(func(*remoteNPC) error)(es[0].(*remoteNPC))
	}, nest.HandlerMeta{Rollback: nest.RollbackUndo, Durability: nest.DurabilityAsync})
}

// --- 事件记录 ---

// npcEvent 是一条事件：seq 全局递增；who 是实例（进程名 + 第几次加载，例如 "A1"）。
type npcEvent struct {
	seq  int64
	who  string
	what string
}

// npcLog 记录一个场景里全部进程、全部实例的事件，用来核对“谁在什么时候推进了实体”。
type npcLog struct {
	mu     sync.Mutex
	events []npcEvent
}

func (l *npcLog) add(who, what string) {
	l.mu.Lock()
	l.events = append(l.events, npcEvent{seq: int64(len(l.events)) + 1, who: who, what: what})
	l.mu.Unlock()
}

// mark 返回当前位置：之后记下的事件 seq 都大于它。
func (l *npcLog) mark() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(len(l.events))
}

// since 返回 seq 大于 after 的事件；who 非空时只看这个实例。
func (l *npcLog) since(after int64, who string) []npcEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []npcEvent
	for _, ev := range l.events[after:] {
		if who == "" || ev.who == who {
			out = append(out, ev)
		}
	}
	return out
}

// ticks 数 who 在 (after, before] 之间的周期推进（AI 决策与工作动作的 tick）；before 为 0 表示到现在。
func (l *npcLog) ticks(who string, after, before int64) int {
	n := 0
	for _, ev := range l.since(after, who) {
		if (before == 0 || ev.seq <= before) && (ev.what == "ai tick" || ev.what == "work tick") {
			n++
		}
	}
	return n
}

// count 数 who 在 after 之后内容为 what 的事件。
func (l *npcLog) count(who, what string, after int64) int {
	n := 0
	for _, ev := range l.since(after, who) {
		if ev.what == what {
			n++
		}
	}
	return n
}

// --- 进程内 Remote 权威替身 ---

// memRemoteAuthority 是两个进程共用的 Remote 权威：所有权记录（规则同 Redis marker：认领一次、按期望租约 CAS、每次
// 变化推进 marker 代际，转移推进路由代际）与按实体的带 fence 版本锁。它只是真实 remoteentity.Manager 的依赖，所有权
// 状态机、写准入、写门排空与实例状态切换都是 Manager 自己的。
type memRemoteAuthority struct {
	mu     sync.Mutex
	leases map[int64]entity.RemoteEntityMarkerLease
	locks  map[int64]*memLockState
}

type memLockState struct {
	held    bool
	version int64
	fence   uint64
}

func newMemRemoteAuthority() *memRemoteAuthority {
	return &memRemoteAuthority{leases: make(map[int64]entity.RemoteEntityMarkerLease), locks: make(map[int64]*memLockState)}
}

func (a *memRemoteAuthority) GetOwnership(_ context.Context, id int64) (entity.RemoteEntityMarkerLease, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	lease, ok := a.leases[id]
	return lease, ok, nil
}

func (a *memRemoteAuthority) ClaimOwnership(_ context.Context, id int64, owner int32) (entity.RemoteEntityMarkerLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if lease, ok := a.leases[id]; ok {
		if lease.OwnerSid == owner {
			return lease, nil
		}
		return entity.RemoteEntityMarkerLease{}, fmt.Errorf("%w: owner %d", entity.ErrRemoteFenced, lease.OwnerSid)
	}
	lease := entity.RemoteEntityMarkerLease{OwnerSid: owner, MarkerEpoch: 1, RouteEpoch: 1}
	a.leases[id] = lease
	return lease, nil
}

func (a *memRemoteAuthority) cas(id int64, expected entity.RemoteEntityMarkerLease, ok func(entity.RemoteEntityMarkerLease) bool,
	next func(entity.RemoteEntityMarkerLease) entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, found := a.leases[id]
	if !found || current != expected || !ok(current) {
		return entity.RemoteEntityMarkerLease{}, errors.New("memRemoteAuthority: ownership CAS conflict")
	}
	n := next(current)
	a.leases[id] = n
	return n, nil
}

func (a *memRemoteAuthority) EnterSharedExpected(_ context.Context, id int64, expected entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	return a.cas(id, expected, func(l entity.RemoteEntityMarkerLease) bool { return !l.Shared },
		func(l entity.RemoteEntityMarkerLease) entity.RemoteEntityMarkerLease {
			l.Shared, l.MarkerEpoch = true, l.MarkerEpoch+1
			return l
		})
}

func (a *memRemoteAuthority) LeaveSharedExpected(_ context.Context, id int64, expected entity.RemoteEntityMarkerLease) (entity.RemoteEntityMarkerLease, error) {
	return a.cas(id, expected, func(l entity.RemoteEntityMarkerLease) bool { return l.Shared },
		func(l entity.RemoteEntityMarkerLease) entity.RemoteEntityMarkerLease {
			l.Shared, l.MarkerEpoch = false, l.MarkerEpoch+1
			return l
		})
}

func (a *memRemoteAuthority) TransferExpected(_ context.Context, id int64, expected entity.RemoteEntityMarkerLease, owner int32) (entity.RemoteEntityMarkerLease, error) {
	return a.cas(id, expected, func(l entity.RemoteEntityMarkerLease) bool { return l.OwnerSid != owner },
		func(l entity.RemoteEntityMarkerLease) entity.RemoteEntityMarkerLease {
			l.OwnerSid, l.MarkerEpoch, l.RouteEpoch = owner, l.MarkerEpoch+1, l.RouteEpoch+1
			return l
		})
}

// NewVersionedLock 让 memRemoteAuthority 同时是锁工厂：同一实体在两个进程里的锁共用一份状态。
func (a *memRemoteAuthority) NewVersionedLock(id int64, _ redis.VersionedLockOptions) redis.IVersionedLock {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.locks[id]
	if st == nil {
		st = &memLockState{}
		a.locks[id] = st
	}
	return &memVersionedLock{a: a, st: st}
}

// memVersionedLock 是一个进程持有的锁句柄；全部字段在 a.mu 下读写。
type memVersionedLock struct {
	a        *memRemoteAuthority
	st       *memLockState
	acquired bool
	version  int64
	fence    uint64
}

func (l *memVersionedLock) TryLock(context.Context) error {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	if l.st.held {
		return remoteentity.ErrVersionedLockNotAcquired
	}
	l.st.held = true
	l.st.fence++
	l.acquired, l.version, l.fence = true, l.st.version, l.st.fence
	return nil
}

func (l *memVersionedLock) Lock(ctx context.Context) error {
	for {
		if err := l.TryLock(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (l *memVersionedLock) Unlock(_ context.Context, version int64, _ time.Duration) error {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	if !l.acquired {
		return remoteentity.ErrVersionedLockNotOwned
	}
	l.acquired, l.st.held = false, false
	l.st.version, l.version = version, version
	return nil
}

func (l *memVersionedLock) UnlockWithRetry(ctx context.Context, version int64, ttl time.Duration, _ int, _ time.Duration) error {
	return l.Unlock(ctx, version, ttl)
}

func (l *memVersionedLock) Version() int64 {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	return l.version
}

func (l *memVersionedLock) IsAcquired() bool {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	return l.acquired
}

func (l *memVersionedLock) Fence() uint64 {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	return l.fence
}

func (l *memVersionedLock) Touch(context.Context, time.Duration) error { return nil }
func (l *memVersionedLock) Refresh(context.Context) error              { return nil }
func (l *memVersionedLock) Close() error {
	l.a.mu.Lock()
	defer l.a.mu.Unlock()
	if l.acquired {
		l.acquired, l.st.held = false, false
	}
	return nil
}

// memRemoteStorage 是不落库的 Remote 存储：remoteNPC 没有持久字段，心跳与业务消息都不产生 Remote 提交；出现提交就报错。
type memRemoteStorage struct{}

var errUnexpectedRemoteCommit = errors.New("memRemoteStorage: remoteNPC has no persistent state, no commit expected")

func (memRemoteStorage) CommitRemote(context.Context, entity.RemoteCommit) (entity.RemoteCommitReceipt, error) {
	return entity.RemoteCommitReceipt{}, errUnexpectedRemoteCommit
}
func (memRemoteStorage) CommitStatus(context.Context, entity.RemoteTransactionID) (entity.RemoteCommitStatus, error) {
	return entity.RemoteCommitStatus{}, nil
}
func (memRemoteStorage) CommitRemoteBatch(_ context.Context, commits []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	if len(commits) != 0 {
		return nil, errUnexpectedRemoteCommit
	}
	return nil, nil
}
func (memRemoteStorage) LoadRemoteSnapshot(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
	return entity.RemoteSnapshotEnvelope{}, false, nil
}
func (memRemoteStorage) PendingRemoteCommits(context.Context, int) ([]entity.RemoteCommitStatus, error) {
	return nil, nil
}
func (memRemoteStorage) MarkRemoteCommitPublished(context.Context, entity.RemoteTransactionID) error {
	return nil
}
func (memRemoteStorage) EnsureRemoteStorage(context.Context) error { return nil }

// npcManagerFactory 为一个进程构造它的 Remote Manager（loader 是这个进程的实例加载器）。
type npcManagerFactory func(t *testing.T, sid int32, loader entity.IRemoteEntityLoader) *remoteentity.Manager

// memManagers 返回共用 a 的 Manager 工厂。
func (a *memRemoteAuthority) memManagers() npcManagerFactory {
	return func(t *testing.T, sid int32, loader entity.IRemoteEntityLoader) *remoteentity.Manager {
		t.Helper()
		m := remoteentity.NewManager(a, remoteentity.DefaultConfig(), sid)
		if err := m.LockFactoryError(); err != nil {
			t.Fatal(err)
		}
		backend, err := remoteentity.NewBackend(loader, memRemoteStorage{})
		if err != nil {
			t.Fatal(err)
		}
		m.SetBackend(backend)
		m.SetOwnershipStore(a)
		t.Cleanup(func() { _ = m.StopFinalizer(context.Background()) })
		return m
	}
}

// --- 进程与实体 ---

// npcProc 是一个进程：真实 Nest 引擎、实体管理器、启动的心跳驱动与自己的 Remote Manager；它同时是 Manager 的实例加载器
// （本地查找 + 按 ID 构造新实例，相当于生成实体的 loader）。
type npcProc struct {
	t        *testing.T
	name     string
	sid      int32
	log      *npcLog
	entities *entity.EntityManager
	engine   *nest.NestMgr
	driver   *heartbeat.Driver
	remote   *remoteentity.Manager
	registry *actionflow.Registry
	client   *recordingClient
	control  *npcControl
	loads    atomic.Int32
	loadMu   sync.Mutex
	// onLoad 是实例构造后、发布前的初始化（相当于 OnInitFinish 里按 DAO 恢复的部分）；nil 表示什么都不挂。
	// 在 loadMu 下读写（排队的旧拍可能在任意时刻经写准入触发加载），用 setOnLoad 设置。
	onLoad func(*remoteNPC)
}

func newNPCProc(t *testing.T, name string, sid int32, log *npcLog, managers npcManagerFactory, shortWorkers int, delivery heartbeat.Delivery) *npcProc {
	t.Helper()
	p := &npcProc{t: t, name: name, sid: sid, log: log, entities: entity.NewEntityManager(), registry: npcRegistry(t)}
	p.remote = managers(t, sid, p)
	p.engine = nest.NewEngine(
		nest.NestOptionWithGetter(entity.NewManagerAccess(p.entities)),
		nest.NestOptionWithTransactionCommitter(nullCommitter{}),
		nest.NestOptionWithRemoteEntityManager(p.remote),
		nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers: shortWorkers, QueueCap: 4096}, nest.WorkerPoolConfig{Workers: 2, QueueCap: 1024}, nest.WorkerPoolConfig{Workers: 4, QueueCap: 4096}),
	)
	if err := p.engine.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.engine.Shutdown(context.Background()) })
	p.driver = heartbeat.NewDriver(heartbeat.Config{Name: fmt.Sprintf("npc-%s-%d", name, time.Now().UnixNano()), Logger: discard, Delivery: delivery})
	p.client = &recordingClient{NestMgr: p.engine}
	if err := p.driver.Bind(p.client, p.entities.Get); err != nil {
		t.Fatal(err)
	}
	p.control = newNPCControl(t, p)
	if err := p.driver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.driver.Stop(context.Background()) })
	return p
}

// recordingClient 记下驱动经哪条路投递了哪个实体的拍：Remote 托管实体的拍必须走单目标 Dispatch（它的写要在 I/O 池
// 取得 Remote 写批次，Nest 批量作业不接受），批量方式下其余实体走 DispatchTryBatch。
type recordingClient struct {
	*nest.NestMgr
	mu      sync.Mutex
	single  map[int64]int
	batched map[int64]int
}

func (c *recordingClient) note(m *map[int64]int, id int64) {
	c.mu.Lock()
	if *m == nil {
		*m = make(map[int64]int)
	}
	(*m)[id]++
	c.mu.Unlock()
}

func (c *recordingClient) Dispatch(ctx context.Context, name nest.HandlerName, id int64, params nest.Params, opts ...nest.SendOpt) error {
	c.note(&c.single, id)
	return c.NestMgr.Dispatch(ctx, name, id, params, opts...)
}

func (c *recordingClient) DispatchTryBatch(ctx context.Context, targets []nest.TryBatchTarget, handBack func(nest.TryBatchTarget, error)) error {
	for _, target := range targets {
		c.note(&c.batched, target.ID)
	}
	return c.NestMgr.DispatchTryBatch(ctx, targets, handBack)
}

// counts 返回 id 的拍经单目标与批量各投了多少次。
func (c *recordingClient) counts(id int64) (single, batched int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.single[id], c.batched[id]
}

// npcControl 是同一进程里的一个本地（非 Remote）实体，带一个 50ms 心跳：证明这个进程的投递方式真的生效（批量方式下
// 它的拍走 DispatchTryBatch），与 Remote 实体的拍走单目标 Dispatch 对照。
type npcControl struct {
	*entity.EntityBase
	host *heartbeat.Host
	runs atomic.Int64
}

func (c *npcControl) Base() *entity.EntityBase             { return c.EntityBase }
func (c *npcControl) OnDestroy(entity.EntityDestroyReason) { c.host.OnDestroy() }

var npcControlSeq atomic.Int64

func newNPCControl(t *testing.T, p *npcProc) *npcControl {
	t.Helper()
	c := &npcControl{EntityBase: entity.NewEntityBase(mustID(t, 900000+npcControlSeq.Add(1)), entity.EntityCategoryOther, true, kindMover)}
	c.host = p.driver.NewHost(c)
	r, err := c.host.Register("control", 50*time.Millisecond, func(heartbeat.Beat) error { c.runs.Add(1); return nil })
	if err == nil {
		err = r.Start()
	}
	if err != nil {
		t.Fatal(err)
	}
	p.entities.Add(c)
	return c
}

// expectDeliveryPaths 核对投递路径：Remote 实体 id 的拍全部单目标 Dispatch、没有一拍进批量作业；本地对照实体在批量
// 方式下走批量作业、缺省方式下走单目标，并且一直在跳。
func expectDeliveryPaths(t *testing.T, p *npcProc, delivery heartbeat.Delivery, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		single, batched := p.client.counts(id)
		if batched != 0 || single == 0 {
			t.Fatalf("%s: Remote entity %d beats: %d single-target, %d in batch jobs (want all single-target)", p.name, id, single, batched)
		}
	}
	single, batched := p.client.counts(p.control.ID())
	if delivery == heartbeat.DeliveryBatch && (batched == 0 || single != 0) || delivery != heartbeat.DeliveryBatch && (batched != 0 || single == 0) {
		t.Fatalf("%s (%s): local control entity beats: %d single-target, %d in batch jobs", p.name, delivery, single, batched)
	}
	if p.control.runs.Load() == 0 {
		t.Fatalf("%s: the local control entity never beat", p.name)
	}
}

// npcDeliveries 是场景要跑的投递方式：单目标与缺省的批量作业（D20 选项 C，heartbeat.delivery）。
var npcDeliveries = []heartbeat.Delivery{heartbeat.DeliveryDispatch, heartbeat.DeliveryBatch}

func (p *npcProc) LookupLocalRemoteEntity(id int64, _ entity.EntityKind) entity.IThreadSafeRemoteEntity {
	if e, ok := p.entities.Get(id).(*remoteNPC); ok {
		return e
	}
	return nil
}

func (p *npcProc) LoadRemoteEntity(_ context.Context, id int64, kind entity.EntityKind) (entity.IThreadSafeRemoteEntity, error) {
	if kind != kindRemoteNPC {
		return nil, fmt.Errorf("npc loader: kind %d", kind)
	}
	p.loadMu.Lock()
	defer p.loadMu.Unlock()
	if e := p.LookupLocalRemoteEntity(id, kind); e != nil {
		return e, nil
	}
	e, err := p.build(id)
	if err == nil {
		err = p.entities.TryAdd(e)
	}
	if err != nil {
		if e != nil {
			e.host.OnDestroy()
		}
		return nil, err
	}
	return e, nil
}

func (p *npcProc) setOnLoad(fn func(*remoteNPC)) {
	p.loadMu.Lock()
	p.onLoad = fn
	p.loadMu.Unlock()
}

// instance 返回进程里当前加载的实例（没有时 nil）。
func (p *npcProc) instance(id int64) *remoteNPC {
	e, _ := p.entities.Get(id).(*remoteNPC)
	return e
}

// run 在实体的锁与事务里执行 fn（经 Nest，Remote 写准入照常）。
func (p *npcProc) run(id int64, fn func(*remoteNPC) error) error {
	_, err := p.engine.Request(context.Background(), npcRunHandler, id, nest.NewParams(fn))
	return err
}

func (p *npcProc) mustRun(id int64, fn func(*remoteNPC) error) {
	p.t.Helper()
	if err := p.run(id, fn); err != nil {
		p.t.Fatal(err)
	}
}

// remoteNPC 是 Remote 托管的 NPC：MapBasic（周期落库）+ Move + Behavior（动作组心跳、任务心跳）+ AI 心跳，共用一个心跳宿主。
// 没有持久字段（HasRemoteCommitLocked 恒为 false），所以全部写都不产生 Remote 提交。
type remoteNPC struct {
	*entity.RemoteEntityBase
	who      string
	proc     *npcProc
	log      *npcLog
	host     *heartbeat.Host
	mb       *MapBasic
	move     *Move
	behavior *actionflow.Behavior
	ai       *ai.Controller
}

func (p *npcProc) build(id int64) (*remoteNPC, error) {
	e := &remoteNPC{
		RemoteEntityBase: entity.NewRemoteEntityBase(id, entity.EntityCategoryRemote, true, kindRemoteNPC),
		who:              fmt.Sprintf("%s%d", p.name, p.loads.Add(1)),
		log:              p.log,
		proc:             p,
	}
	e.host = p.driver.NewHost(e)
	var err error
	if e.mb, err = NewMapBasic(e, e.host, MapConfig{Fields: &MemFields{}, SaveEvery: time.Second,
		Save: func(int64, spatial.Point, Facing) error { e.event("save"); return nil }}); err != nil {
		return nil, err
	}
	if e.move, err = NewMove(e.mb, MoveConfig{}); err != nil {
		return nil, err
	}
	if e.ai, err = ai.NewBeatController(ai.ControllerHooks{
		Context: func(now time.Time) *ai.Context { return &ai.Context{Owner: e, ActionList: e.behavior, Now: now} },
		OnError: e.report,
	}, ai.BeatConfig{Host: e.host, Period: npcAIPeriod}); err != nil {
		return nil, err
	}
	if e.behavior, err = actionflow.NewBehavior(e, actionflow.BehaviorConfig{
		Registry: p.registry, GroupForKind: groupForKind, Beat: &actionflow.BeatConfig{Host: e.host},
		Observer: npcObserver{e}, OnError: e.report,
	}); err != nil {
		return nil, err
	}
	if p.onLoad != nil {
		p.onLoad(e)
	}
	return e, nil
}

func (e *remoteNPC) Base() *entity.EntityBase { return &e.RemoteEntityBase.EntityBase }
func (e *remoteNPC) MapBasic() *MapBasic      { return e.mb }
func (e *remoteNPC) MoveComponent() *Move     { return e.move }
func (e *remoteNPC) OnDestroy(entity.EntityDestroyReason) {
	e.mb.OnDestroy()
	e.host.OnDestroy()
}
func (e *remoteNPC) event(what string) { e.log.add(e.who, what) }
func (e *remoteNPC) report(err error)  { e.event("error " + err.Error()) }

func (*remoteNPC) HasRemoteCommitLocked(entity.RemoteTransactionOutcome) bool { return false }
func (*remoteNPC) BuildRemoteCommitLocked(entity.RemoteWriteLease, entity.RemoteTransactionOutcome) (entity.RemoteCommit, error) {
	return entity.RemoteCommit{}, errUnexpectedRemoteCommit
}
func (*remoteNPC) AcknowledgeRemoteCommit(entity.RemoteCommit) error { return nil }
func (*remoteNPC) RollbackRemoteCommit(entity.RemoteCommit)          {}

// locked 在实体锁内读状态（不经 Nest、不走 Remote 写准入）。
func locked[T any](e *remoteNPC, fn func(*remoteNPC) T) T {
	e.GetMutex().Lock()
	defer e.GetMutex().Unlock()
	return fn(e)
}

// activeRegs 是实体上业务视角在跳的心跳登记名（实体锁内读）。
func (e *remoteNPC) activeRegs() []string {
	return locked(e, func(e *remoteNPC) []string {
		var out []string
		for _, r := range []*heartbeat.Reg{e.behavior.Beat().Reg(groupMove), e.behavior.Beat().Reg(groupWork), e.behavior.MissionReg(), e.ai.BeatReg(), e.mb.saveReg} {
			if r != nil && r.Active() {
				out = append(out, r.Name())
			}
		}
		return out
	})
}

// npcPlan 是 NPC 的任务：一步，向远处 MoveTo（测试期间走不到），一分钟时限（所以任务心跳在跳），失败即任务失败。
func npcPlan() actionflow.MissionPlan {
	return actionflow.MissionPlan{Steps: []actionflow.MissionStep{{
		Action: actMoveTo, Param: MoveToParam{Path: []spatial.Point{{X: 100000}}, Speed: 20},
		OnSuccess: actionflow.MissionSuccessEnd(), OnFail: actionflow.MissionFailedEnd(), Timeout: time.Minute,
	}}}
}

// npcStart 是业务让 NPC 跑起来（在 handler 里）：进场景（周期落库）、换上 AI 策略、下发任务（任务发起移动）、再发起
// 工作动作。runner 的调用放在最后（D20 决定 3）。
func npcStart(e *remoteNPC) error {
	if !e.mb.InScene() {
		if err := e.mb.Enter(1, &spaceLog{}, spatial.Point{}); err != nil {
			return err
		}
	}
	if err := e.ai.SetStrategy(npcStrategy{}); err != nil {
		return err
	}
	if err := e.behavior.SetMission(missionGoto, npcPlan()); err != nil {
		return err
	}
	_, err := e.behavior.CreateAction(actTick, nil)
	return err
}

func npcRegistry(t testing.TB) *actionflow.Registry {
	t.Helper()
	registry := actionflow.NewRegistry()
	if err := RegisterActions(registry, actMoveTo, actFollow); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterAction(actTick, func(any) (actionflow.Action, error) { return npcWork{}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterMission(missionGoto, func() actionflow.Mission {
		return &npcMission{PlanMission: actionflow.NewPlanMission(missionGoto)}
	}); err != nil {
		t.Fatal(err)
	}
	return registry
}

// npcWork 是工作组里一直运行的动作，每 50ms tick 一次（记 "work tick"）。
type npcWork struct{}

func (npcWork) Kind() actionflow.ActionKind { return actTick }
func (npcWork) TickInterval() time.Duration { return npcWorkPeriod }
func (npcWork) Cancel(ctx *actionflow.ActionContext, _ string) {
	npcOf(ctx.Owner).event("work canceled")
}
func (npcWork) Start(*actionflow.ActionContext) (bool, actionflow.ActionResult, error) {
	return false, actionflow.ActionResult{}, nil
}
func (npcWork) Tick(ctx *actionflow.ActionContext) (bool, actionflow.ActionResult) {
	npcOf(ctx.Owner).event("work tick")
	return false, actionflow.ActionResult{}
}

func npcOf(owner entity.IThreadSafeEntity) *remoteNPC { return owner.(*remoteNPC) }

// npcMission 是计划任务，另外记下它收到的动作结束（核对“先任务、后观察者”）。
type npcMission struct{ *actionflow.PlanMission }

func (m *npcMission) OnActionEnd(ctx *actionflow.MissionContext, id int64, kind actionflow.ActionKind, reason actionflow.ActionReason) {
	npcOf(ctx.Owner).event(fmt.Sprintf("mission action-end %d %s", kind, reason.ToActionResult(actionflow.ActionStatusSuccess).Status))
	m.PlanMission.OnActionEnd(ctx, id, kind, reason)
}

// npcStrategy 是 AI 策略：每拍记 "ai tick"。
type npcStrategy struct{}

func (npcStrategy) Name() string                                                          { return "npc" }
func (npcStrategy) Init(*ai.Context) error                                                { return nil }
func (npcStrategy) Tick(ctx *ai.Context, _ time.Time)                                     { npcOf(ctx.Owner).event("ai tick") }
func (npcStrategy) CanStopByNext(ai.Strategy) bool                                        { return true }
func (npcStrategy) OnMissionEnd(*ai.Context, actionflow.Mission, actionflow.ActionReason) {}
func (npcStrategy) OnActionEnd(*ai.Context, int64, actionflow.ActionKind, actionflow.ActionReason) {
}

// npcObserver 是 Behavior 的观察者：记下动作 / 任务结束（带此刻是否还在移动，核对 Cancel 在结束通知之前），再交给 AI。
type npcObserver struct{ e *remoteNPC }

func (o npcObserver) OnActionEnd(id int64, kind actionflow.ActionKind, reason actionflow.ActionReason) {
	o.e.event(fmt.Sprintf("observer action-end %d %s moving=%v", kind, reason.ToActionResult(actionflow.ActionStatusSuccess).Status, o.e.move.Moving()))
	o.e.ai.OnActionEnd(id, kind, reason)
}

func (o npcObserver) OnMissionEnd(m actionflow.Mission, reason actionflow.ActionReason) {
	o.e.event(fmt.Sprintf("observer mission-end %s", reason.ToActionResult(actionflow.ActionStatusSuccess).Status))
	o.e.ai.OnMissionEnd(m, reason)
}

// --- 场景共用的断言 ---

// npcID 是场景里的实体 ID。
func npcID(t testing.TB, unique int64) int64 {
	t.Helper()
	id, err := entity.BuildEntityID(unique, kindRemoteNPC)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// stoppedSharedMetric 读驱动的 heartbeat.stopped_total{reason="remote_shared"}（驱动每秒 flush 一次）。
func stoppedSharedMetric(d *heartbeat.Driver) int64 {
	for _, m := range metrics.Snapshot() {
		if m.Name == heartbeat.StoppedMetric && m.Labels["driver"] == d.Name() && m.Labels["reason"] == "remote_shared" {
			return m.Value
		}
	}
	return 0
}

// expectRunning 等实例在跳全部 npcRegistrations、AI 与工作动作在推进、位置在变。
func expectRunning(t *testing.T, e *remoteNPC, stage string) {
	t.Helper()
	from := e.log.mark()
	pos := locked(e, func(e *remoteNPC) spatial.Point { return e.mb.Pos() })
	deadline := time.Now().Add(30 * time.Second)
	for {
		active := e.activeRegs()
		moved := locked(e, func(e *remoteNPC) bool { return e.mb.Pos() != pos })
		if len(active) == len(npcRegistrations) && e.log.count(e.who, "ai tick", from) >= 2 && e.log.count(e.who, "work tick", from) >= 3 && moved {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s is not running every heartbeat: active %v (want %v), moved %v, ai ticks %d, work ticks %d; driver %+v",
				stage, e.who, active, npcRegistrations, moved, e.log.count(e.who, "ai tick", from), e.log.count(e.who, "work tick", from), e.proc.driver.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// expectStoppedByShared 等进入共享模式之后框架停掉 e 上的 want 个登记：驱动的 StoppedShared 与指标各加 want、登记都
// 不在跳、驱动上只剩本地对照实体的 1 个登记；随后 npcQuiet 窗口里 e 没有任何周期推进、位置不变。返回窗口结束时的位置。
func expectStoppedByShared(t *testing.T, p *npcProc, e *remoteNPC, statsBefore heartbeat.Stats, metricBefore int64, want int, sharedMark int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%s: %d registrations stopped by shared mode", e.who, want), func() bool {
		return p.driver.Stats().StoppedShared-statsBefore.StoppedShared >= int64(want)
	})
	waitFor(t, "the stopped_total metric to be flushed", func() bool { return stoppedSharedMetric(p.driver)-metricBefore >= int64(want) })
	pos := locked(e, func(e *remoteNPC) spatial.Point { return e.mb.Pos() })
	time.Sleep(npcQuiet)
	st := p.driver.Stats()
	if got := st.StoppedShared - statsBefore.StoppedShared; got != int64(want) {
		t.Fatalf("%s: StoppedShared +%d, want +%d (one per registration); stats %+v", e.who, got, want, st)
	}
	if got := stoppedSharedMetric(p.driver) - metricBefore; got != int64(want) {
		t.Fatalf("%s: heartbeat.stopped_total{reason=remote_shared} +%d, want +%d", e.who, got, want)
	}
	if active := e.activeRegs(); len(active) != 0 || st.Registered != 1 {
		t.Fatalf("%s: still beating in shared mode: active %v, driver registered %d (want only the local control entity's)", e.who, active, st.Registered)
	}
	if n := e.log.ticks(e.who, sharedMark, 0); n != 0 {
		t.Fatalf("%s advanced %d times after entering shared mode: %v", e.who, n, e.log.since(sharedMark, e.who))
	}
	if now := locked(e, func(e *remoteNPC) spatial.Point { return e.mb.Pos() }); now != pos {
		t.Fatalf("%s moved in shared mode: %v -> %v", e.who, pos, now)
	}
}

// npcResume 是回到独占之后业务让心跳接着走（在 handler 里）：动作组与任务心跳经 BeatRunner.Sync（共享期间被框架停掉、
// 回到独占不自动恢复），AI 经重新设置当前策略，周期落库经离开再进入场景（MapBasic 没有单独的恢复入口）。
func npcResume(e *remoteNPC) error {
	if err := e.behavior.Beat().Sync(); err != nil {
		return err
	}
	if err := e.ai.SetStrategy(e.ai.Strategy()); err != nil {
		return err
	}
	if err := e.mb.Leave(); err != nil {
		return err
	}
	return e.mb.Enter(1, &spaceLog{}, e.mb.Pos())
}

// --- 场景一：独占 → 共享 → 独占 ---

// 真实 Manager 的 EnterRemoteSharedMode / LeaveRemoteSharedMode（两轮）：
//   - 进入共享后框架在下一拍停掉实体上全部 5 个登记（移动组、工作组、任务、AI、周期落库），StoppedShared 与
//     heartbeat.stopped_total{reason="remote_shared"} 各 +1/登记，之后没有任何周期推进、位置不变；
//   - 共享期间动作、任务、策略都留着（组件不感知），Sync 返回 ErrHeartbeatRemoteShared、不结束任何东西；
//   - 回到独占不自动恢复；业务 Sync / 重新设置策略 / 重新进场景后全部接着跳；
//   - 第二轮共享期间业务发起新的移动：取代任务的移动（取消，任务失败），新移动挂不上心跳、以失败结束，通知顺序为
//     任务先于观察者、Cancel（停掉移动）先于结束通知。
func TestRemoteSharedModeRoundTripStopsAndResumesEveryHeartbeat(t *testing.T) {
	for _, delivery := range npcDeliveries {
		t.Run(string(delivery), func(t *testing.T) { testSharedModeRoundTrip(t, newMemRemoteAuthority().memManagers(), 1, delivery) })
	}
}

func testSharedModeRoundTrip(t *testing.T, managers npcManagerFactory, unique int64, delivery heartbeat.Delivery) {
	ctx := context.Background()
	log := &npcLog{}
	a := newNPCProc(t, "A", 1001, log, managers, 4, delivery)
	id := npcID(t, unique)
	a.mustRun(id, npcStart) // 第一次写由 A 认领所有权（独占），并加载实例 A1
	e := a.instance(id)
	expectRunning(t, e, "first start")

	// 第一轮：进入共享。
	st, mt := a.driver.Stats(), stoppedSharedMetric(a.driver)
	if _, err := a.remote.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	shared := log.mark()
	expectStoppedByShared(t, a, e, st, mt, len(npcRegistrations), shared)
	if err := a.run(id, func(e *remoteNPC) error { return e.behavior.Beat().Sync() }); !errors.Is(err, heartbeat.ErrHeartbeatRemoteShared) {
		t.Fatalf("Sync in shared mode: %v, want ErrHeartbeatRemoteShared", err)
	}
	kept := locked(e, func(e *remoteNPC) string {
		return fmt.Sprintf("move %v work %v mission %v strategy %v moving %v", e.behavior.CurAction(groupMove) != nil, e.behavior.CurAction(groupWork) != nil,
			e.behavior.CurMission() != nil && e.behavior.CurMission().Status() == actionflow.MissionStatusRunning, e.ai.Strategy() != nil, e.move.Moving())
	})
	if kept != "move true work true mission true strategy true moving true" || log.count(e.who, "work canceled", shared) != 0 {
		t.Fatalf("shared mode dropped behavior state: %s; events %v", kept, log.since(shared, e.who))
	}

	// 回到独占：不自动恢复；业务恢复后全部接着跳。
	if _, err := a.remote.LeaveRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	left := log.mark()
	time.Sleep(npcQuiet)
	if active := e.activeRegs(); len(active) != 0 || log.ticks(e.who, left, 0) != 0 {
		t.Fatalf("heartbeats resumed by themselves after leaving shared mode: active %v, events %v", active, log.since(left, e.who))
	}
	a.mustRun(id, npcResume)
	expectRunning(t, e, "resumed after the first shared window")

	// 第二轮：共享期间发起新的移动。
	st, mt = a.driver.Stats(), stoppedSharedMetric(a.driver)
	if _, err := a.remote.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	shared = log.mark()
	expectStoppedByShared(t, a, e, st, mt, len(npcRegistrations), shared)
	order := log.mark()
	a.mustRun(id, func(e *remoteNPC) error {
		_, err := e.behavior.CreateAction(actMoveTo, MoveToParam{Path: []spatial.Point{{Y: 100000}}, Speed: 20})
		return err
	})
	var got []string
	for _, ev := range log.since(order, e.who) {
		if !strings.HasPrefix(ev.what, "error") {
			got = append(got, ev.what)
		}
	}
	// 旧移动（任务的那一步）被取代：Cancel 先停掉移动，结束先交给任务（任务按 OnFail 结束，结论带取消原因，观察者
	// 在这次动作结束的处理里先收到任务结束），再交给观察者；新移动挂不上心跳、以失败结束（原因带共享模式），
	// 它的 Cancel 在结束通知之前（moving=false）。
	want := []string{
		"mission action-end 1 canceled", "observer mission-end canceled", "observer action-end 1 canceled moving=false",
		"observer action-end 1 failed moving=false",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("notifications for a move started in shared mode:\n got %q\nwant %q\nall %v", got, want, log.since(order, e.who))
	}
	if failed := log.since(order, e.who); !strings.Contains(fmt.Sprint(failed), "shared mode") {
		t.Fatalf("the failure was not reported with the shared-mode cause: %v", failed)
	}
	if s := locked(e, func(e *remoteNPC) string {
		return fmt.Sprintf("moving %v move %v mission %v", e.move.Moving(), e.behavior.CurAction(groupMove) != nil, e.behavior.CurMission() != nil)
	}); s != "moving false move false mission false" {
		t.Fatalf("after the failed move: %s", s)
	}

	if _, err := a.remote.LeaveRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	a.mustRun(id, func(e *remoteNPC) error {
		if err := npcResume(e); err != nil {
			return err
		}
		return e.behavior.SetMission(missionGoto, npcPlan())
	})
	expectRunning(t, e, "resumed after the second shared window")
	expectDeliveryPaths(t, a, delivery, id)
}

// npcRestore 是实例加载时按 DAO 恢复的部分（相当于生成实体的 OnInitFinish，在发布之前、不在事务里）：回到场景（周期
// 落库）、换上 AI 策略。共享模式下刚加载的实例是 Unknown，这里的登记合法，由第一拍停掉。
func npcRestore(e *remoteNPC) {
	if err := e.mb.Enter(1, &spaceLog{}, spatial.Point{}); err != nil {
		e.report(err)
	}
	if err := e.ai.SetStrategy(npcStrategy{}); err != nil {
		e.report(err)
	}
}

// npcRestart 是回到独占之后业务让重新加载的实例接着跑（在 handler 里）：恢复被停掉的心跳，再下发任务与工作动作
// （runner 不持久，重载后是空的）。
func npcRestart(e *remoteNPC) error {
	if err := npcResume(e); err != nil {
		return err
	}
	if err := e.behavior.SetMission(missionGoto, npcPlan()); err != nil {
		return err
	}
	_, err := e.behavior.CreateAction(actTick, nil)
	return err
}

// expectNoAutoResume 观察 npcQuiet：回到独占之后、业务调用之前，e 上没有登记在跳、没有任何周期推进。
func expectNoAutoResume(t *testing.T, e *remoteNPC, left int64) {
	t.Helper()
	time.Sleep(npcQuiet)
	if active := e.activeRegs(); len(active) != 0 || e.log.ticks(e.who, left, 0) != 0 {
		t.Fatalf("%s: heartbeats resumed by themselves after leaving shared mode: active %v, events %v", e.who, active, e.log.since(left, e.who))
	}
}

// --- 场景二：所有权迁移 ---

// 真实 Manager 的 TransferRemoteOwnership（共享模式下迁移，生成工程的 Remote 流程即如此）：A 独占跳着 → A 进入共享
// （A1 的 5 个登记被停掉）→ A 把所有权转给 B（A1 转为 Fenced）→ B 的第一条业务消息加载 B1（加载时恢复场景与 AI，
// 实例是 Unknown、登记合法），写准入转为 Shared，B1 的 2 个登记由第一拍停掉 → B 回到独占，不自动恢复 → 业务恢复后
// B1 接着跳。全程没有双推进：A1 在进入共享之后、B1 在回到独占之前都没有任何周期推进；A 之后的业务写被 fence 拒绝。
func TestRemoteOwnershipTransferMovesTheHeartbeatsWithoutDoubleAdvance(t *testing.T) {
	for _, delivery := range npcDeliveries {
		t.Run(string(delivery), func(t *testing.T) { testOwnershipTransfer(t, newMemRemoteAuthority().memManagers(), 2, delivery) })
	}
}

func testOwnershipTransfer(t *testing.T, managers npcManagerFactory, unique int64, delivery heartbeat.Delivery) {
	ctx := context.Background()
	log := &npcLog{}
	a := newNPCProc(t, "A", 1001, log, managers, 4, delivery)
	b := newNPCProc(t, "B", 1002, log, managers, 4, delivery)
	b.setOnLoad(npcRestore)
	id := npcID(t, unique)
	a.mustRun(id, npcStart)
	ea := a.instance(id)
	expectRunning(t, ea, "A owns the entity")

	st, mt := a.driver.Stats(), stoppedSharedMetric(a.driver)
	if _, err := a.remote.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	sharedA := log.mark()
	expectStoppedByShared(t, a, ea, st, mt, len(npcRegistrations), sharedA)
	if _, err := a.remote.TransferRemoteOwnership(ctx, id, b.sid); err != nil {
		t.Fatal(err)
	}
	if s := ea.RemoteOwnershipState(); s != entity.RemoteOwnershipFenced {
		t.Fatalf("old owner's instance is %s after the transfer, want fenced", s)
	}

	stB, mtB := b.driver.Stats(), stoppedSharedMetric(b.driver)
	b.mustRun(id, func(*remoteNPC) error { return nil }) // B 的第一条消息加载 B1
	eb := b.instance(id)
	if eb == nil || eb.RemoteOwnershipState() != entity.RemoteOwnershipShared {
		t.Fatalf("new owner's instance after its first write: %v", eb)
	}
	expectStoppedByShared(t, b, eb, stB, mtB, 2, 0) // 加载时恢复的周期落库与 AI
	if _, err := b.remote.LeaveRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	left := log.mark()
	expectNoAutoResume(t, eb, left)
	b.mustRun(id, func(e *remoteNPC) error {
		if err := npcResume(e); err != nil {
			return err
		}
		if err := e.behavior.SetMission(missionGoto, npcPlan()); err != nil {
			return err
		}
		_, err := e.behavior.CreateAction(actTick, nil)
		return err
	})
	expectRunning(t, eb, "B owns the entity")

	if err := a.run(id, func(e *remoteNPC) error { return e.behavior.Beat().Sync() }); !errors.Is(err, entity.ErrRemoteFenced) {
		t.Fatalf("a write on the old owner after the transfer: %v, want ErrRemoteFenced", err)
	}
	time.Sleep(npcQuiet)
	if n := log.ticks(ea.who, sharedA, 0); n != 0 {
		t.Fatalf("double advance: the old owner's instance advanced %d times after entering shared mode: %v", n, log.since(sharedA, ea.who))
	}
	if n := log.ticks(eb.who, 0, left); n != 0 {
		t.Fatalf("double advance: the new owner's instance advanced %d times before owning it exclusively: %v", n, log.since(0, eb.who))
	}
	expectDeliveryPaths(t, a, delivery, id)
	expectDeliveryPaths(t, b, delivery, id)
}

// --- 场景三：卸载与重新加载 ---

// 真实 Manager 下仅内存卸载（DestroyReasonMemoryUnload）后由下一次写重新加载：
//   - 独占时卸载：唯一的快池 worker 被另一个实体占住，A1 的拍经写准入后排队；卸载 A1 之后放开，排队的拍不在旧实例、
//     也不在重载的实例上执行，A1 之后没有任何周期推进，A2 的推进都在 A1 之后（不重叠）；
//   - 共享时卸载：A2 进入共享（5 个登记停掉）后卸载，下一次写加载 A3（加载时恢复场景与 AI，Unknown、登记合法），
//     写准入转为 Shared，A3 的 2 个登记由第一拍停掉；回到独占不自动恢复，业务恢复后 A3 接着跳。
func TestRemoteUnloadAndReloadLeavesNoStrayBeat(t *testing.T) {
	for _, delivery := range npcDeliveries {
		t.Run(string(delivery), func(t *testing.T) { testUnloadAndReload(t, newMemRemoteAuthority().memManagers(), 3, delivery) })
	}
}

func testUnloadAndReload(t *testing.T, managers npcManagerFactory, unique int64, delivery heartbeat.Delivery) {
	ctx := context.Background()
	log := &npcLog{}
	a := newNPCProc(t, "A", 1001, log, managers, 1, delivery)
	id, blocker := npcID(t, unique), npcID(t, unique+1000)
	a.mustRun(id, npcStart)
	a.mustRun(blocker, func(*remoteNPC) error { return nil })
	e1 := a.instance(id)
	expectRunning(t, e1, "first load")

	// 独占时卸载，拍在队列里。
	entered, release := make(chan struct{}), make(chan struct{})
	blocked := make(chan error, 1)
	go func() {
		blocked <- a.run(blocker, func(*remoteNPC) error { close(entered); <-release; return nil })
	}()
	<-entered
	delivered := a.driver.Stats().Delivered
	waitFor(t, "beats of A1 to be queued behind the blocked worker", func() bool { return a.driver.Stats().Delivered-delivered >= 3 })
	if err := a.entities.Destroy(ctx, e1, entity.DestroyReasonMemoryUnload, false); err != nil {
		t.Fatal(err)
	}
	unloaded := log.mark()
	a.setOnLoad(npcRestore)
	close(release)
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
	a.mustRun(id, func(e *remoteNPC) error {
		if err := e.behavior.SetMission(missionGoto, npcPlan()); err != nil {
			return err
		}
		_, err := e.behavior.CreateAction(actTick, nil)
		return err
	})
	e2 := a.instance(id)
	if e2 == nil || e2 == e1 {
		t.Fatalf("no reloaded instance: %v", e2)
	}
	expectRunning(t, e2, "reloaded while exclusive")
	if n := log.ticks(e1.who, unloaded, 0); n != 0 {
		t.Fatalf("the unloaded instance advanced %d times: %v", n, log.since(unloaded, e1.who))
	}
	var lastOld, firstNew int64
	for _, ev := range log.since(0, "") {
		switch {
		case ev.who == e1.who && (ev.what == "ai tick" || ev.what == "work tick"):
			lastOld = ev.seq
		case ev.who == e2.who && firstNew == 0 && (ev.what == "ai tick" || ev.what == "work tick"):
			firstNew = ev.seq
		}
	}
	if firstNew <= lastOld {
		t.Fatalf("old and reloaded instances overlap: last A1 advance %d, first A2 advance %d", lastOld, firstNew)
	}
	t.Logf("exclusive unload window: driver %+v", a.driver.Stats())

	// 共享时卸载。
	st, mt := a.driver.Stats(), stoppedSharedMetric(a.driver)
	if _, err := a.remote.EnterRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	shared := log.mark()
	expectStoppedByShared(t, a, e2, st, mt, len(npcRegistrations), shared)
	if err := a.entities.Destroy(ctx, e2, entity.DestroyReasonMemoryUnload, false); err != nil {
		t.Fatal(err)
	}
	st, mt = a.driver.Stats(), stoppedSharedMetric(a.driver)
	a.mustRun(id, func(*remoteNPC) error { return nil }) // 下一次写加载 A3
	e3 := a.instance(id)
	if e3 == nil || e3 == e2 || e3.RemoteOwnershipState() != entity.RemoteOwnershipShared {
		t.Fatalf("instance reloaded in shared mode: %v", e3)
	}
	expectStoppedByShared(t, a, e3, st, mt, 2, 0)
	if _, err := a.remote.LeaveRemoteSharedMode(ctx, id); err != nil {
		t.Fatal(err)
	}
	left := log.mark()
	expectNoAutoResume(t, e3, left)
	a.mustRun(id, npcRestart)
	expectRunning(t, e3, "reloaded in shared mode, back to exclusive")
	if n := log.ticks(e2.who, shared, 0) + log.ticks(e3.who, 0, left); n != 0 {
		t.Fatalf("advanced in shared mode: %v %v", log.since(shared, e2.who), log.since(0, e3.who))
	}
	expectDeliveryPaths(t, a, delivery, id)
}
