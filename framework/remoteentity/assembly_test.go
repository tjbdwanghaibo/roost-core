// 装配与生命周期：缺依赖与非法配置在装配时拒绝，DAO 作用域校验，Start / Stop 的重试、取消与停机排空。

package remoteentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
)

type assembleRedis struct{ fredis.IRedis }

// atomicTestBackend adds the caller-owned atomic transaction capability the
// Assembly requires of a backend.
type atomicTestBackend struct{ *remoteTestLoader }

func (b *atomicTestBackend) ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

func TestAssembleRefusesEachMissingDependency(t *testing.T) {
	redis := assembleRedis{}
	backend := &atomicTestBackend{newRemoteTestLoader()}
	cases := []struct {
		name string
		deps AssemblyDeps
		sid  int32
		text string
	}{
		{"no redis", AssemblyDeps{Backend: backend}, 7, "redis client is required"},
		{"zero sid", AssemblyDeps{Redis: redis, Backend: backend}, 0, "non-zero sid is required"},
		{"loader without mongo", AssemblyDeps{Redis: redis, Loader: newMockLoader()}, 7, "mongo client is required"},
		{"neither loader nor backend", AssemblyDeps{Redis: redis}, 7, "atomic storage backend is required"},
		{"backend without atomic transactions", AssemblyDeps{Redis: redis, Backend: newRemoteTestLoader()}, 7, "must support caller-owned atomic transactions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assembly, err := Assemble(tc.deps, nil, tc.sid, MongoBackendConfig{})
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Assemble = (%v, %v), want error containing %q", assembly, err, tc.text)
			}
			if assembly != nil {
				t.Fatal("a refused Assemble still returned an assembly")
			}
		})
	}

	assembly, err := Assemble(AssemblyDeps{Redis: redis, Backend: backend}, nil, 7, MongoBackendConfig{})
	if err != nil {
		t.Fatalf("Assemble with every dependency = %v", err)
	}
	if assembly.Manager == nil || assembly.LockFactory == nil || assembly.AtomicStore == nil {
		t.Fatalf("assembly is missing a part: %+v", assembly)
	}
	var none *Assembly
	if err := none.Start(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("Start on a nil assembly = %v", err)
	}
	if err := (&Assembly{}).Start(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("Start on an empty assembly = %v", err)
	}
	if err := assembly.Start(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "sync bus is required") {
		t.Fatalf("Start without a bus = %v", err)
	}
}

func TestPrepareRemoteWriteBatchRefusesWithoutManagerBackendOrFinalizeSlot(t *testing.T) {
	const kind entity.EntityKind = 135
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	// 每次准入都带期限：守卫若失效，调用会卡在写门或 nil 通道上，必须在期限内红而不是挂住。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	live, other := newTestRemoteEntity(1735, 1, kind), newTestRemoteEntity(1736, 1, kind)
	ids := []int64{live.GUId()}

	var none *Manager
	if _, err := none.PrepareRemoteWriteBatch(ctx, ids); !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) {
		t.Fatalf("Prepare on a nil manager = %v", err)
	}
	if _, err := (&Manager{}).PrepareRemoteWriteBatch(ctx, ids); !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) {
		t.Fatalf("Prepare on a manager without config = %v", err)
	}

	// 未设后端：批次 id 合法也不能准入。
	bare := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	if _, err := bare.PrepareRemoteWriteBatch(ctx, ids); !errors.Is(err, entity.ErrRemoteWriteCapabilityDisabled) {
		t.Fatalf("Prepare without a backend = %v", err)
	}
	bare.mu.RLock()
	wrappers := len(bare.wrappers)
	bare.mu.RUnlock()
	if wrappers != 0 {
		t.Fatalf("a refused Prepare left %d wrappers behind", wrappers)
	}

	// finalize 槽位只有一个：第二个批次被拒，第一个 Close 后归还。
	cfg := DefaultConfig()
	cfg.AsyncFinalizeCapacity = 1
	mgr := NewManager(newMockVersionedLockFactory(), cfg, 1000)
	loader := newRemoteTestLoader()
	loader.add(live)
	loader.add(other)
	mgr.SetBackend(loader)
	mgr.SetOwnershipStore(newMockMarkerStore())
	first, err := mgr.PrepareRemoteWriteBatch(ctx, ids)
	if err != nil {
		t.Fatalf("first Prepare = %v", err)
	}
	if _, err := mgr.PrepareRemoteWriteBatch(ctx, []int64{other.GUId()}); !errors.Is(err, entity.ErrRemoteOverloaded) {
		t.Fatalf("Prepare for another entity with every finalize slot taken = %v, want ErrRemoteOverloaded", err)
	}
	if err := first.Abort(ctx, errors.New("test")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := mgr.PrepareRemoteWriteBatch(ctx, ids)
	if err != nil {
		t.Fatalf("Prepare after the slot was returned = %v", err)
	}
	_ = second.Abort(ctx, errors.New("test"))
	_ = second.Close(ctx)

	var wrapper *remoteEntityWrapper
	if _, err := wrapper.beginWrite(ctx); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("beginWrite on a nil wrapper = %v", err)
	}
	if _, err := (&remoteEntityWrapper{}).beginWrite(ctx); !errors.Is(err, entity.ErrRemoteRejected) {
		t.Fatalf("beginWrite on a wrapper without a manager = %v", err)
	}
	var batch *remoteWriteBatch
	if _, err := batch.Commit(ctx); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("Commit on a nil batch = %v", err)
	}
	if _, err := (&remoteWriteBatch{}).Commit(ctx); !errors.Is(err, entity.ErrRemoteCommitNotFinalized) {
		t.Fatalf("Commit on a batch without a manager = %v", err)
	}
}

func (b *atomicTestBackend) WriteAuthority() WriteAuthority {
	return NewMongoCommitter(newRemoteMongoFake(), "test", 7, 0).WriteAuthority()
}

// 自定义后端不能只声明原子提交而缺少持久权限源。
type noAuthorityBackend struct {
	entity.IRemoteEntityBackend
	AtomicCommitStore
}

func TestAssemblyRefusesMissingDurableAuthority(t *testing.T) {
	backend := &atomicTestBackend{newRemoteTestLoader()}
	_, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: noAuthorityBackend{backend, backend}}, nil, 7, MongoBackendConfig{})
	if err == nil || !strings.Contains(err.Error(), "durable write authority") {
		t.Fatalf("missing authority accepted: %v", err)
	}
}

func TestAssembleRejectsTheSnapshotConfigNewSnapshotClientRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"interest_ttl_zero", func(c *Config) { c.SnapshotInterestTTL = 0 }, "snapshot_interest_ttl"},
		{"load_timeout_zero", func(c *Config) { c.SnapshotLoadTimeout = 0 }, "snapshot_load_timeout"},
		{"l2_ttl_zero", func(c *Config) { c.SnapshotL2TTL = 0 }, "snapshot_l2_ttl"},
		{"cache_ttl_negative", func(c *Config) { c.SnapshotCacheTTL = -1 }, "snapshot cache ttl"},
		{"per_consumer_over_subs", func(c *Config) { c.SnapshotInterestSubs = 4; c.SnapshotInterestPerConsumer = 5 }, "snapshot_interest_per_consumer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.mutate(cfg)
			if _, err := NewSnapshotClient(cfg, SnapshotClientDeps{ConsumerSID: 7, L2: NewSnapshotL2Store(assembleRedis{}, time.Minute)}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("precondition: NewSnapshotClient should reject with %q, got %v", tc.want, err)
			}
			backend := &atomicTestBackend{newRemoteTestLoader()}
			assembly, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: backend}, cfg, 7, MongoBackendConfig{})
			if err == nil {
				t.Fatalf("Assemble accepted a snapshot config NewSnapshotClient rejects (assembly=%v)", assembly != nil)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}
	// 缺省配置照常通过。
	if _, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, DefaultConfig(), 7, MongoBackendConfig{}); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
}

// scopedTestDao 是手写 DAO：只实现 DaoInterface 与可选的 DbScope。
type scopedTestDao struct {
	testRemoteDao
	scope entity.DatabaseScope
}

func (d *scopedTestDao) DbScope() entity.DatabaseScope { return d.scope }

func registerScopedDaoKind(kind entity.EntityKind, policy entity.RemotePolicy, scope entity.DatabaseScope) {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: policy})
	entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
		Category: 1,
		Kind:     kind,
		Builder: func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return nil, errors.New("scopedTestDao kinds are never built")
		},
		DaoBuilders: []entity.DaoBuilderFunc{
			func() entity.DaoInterface { return &testRemoteDao{dirty: &testDirty{}} },
			func() entity.DaoInterface {
				return &scopedTestDao{testRemoteDao: testRemoteDao{dirty: &testDirty{}}, scope: scope}
			},
		},
		RemotePolicy: policy,
	})
}

func TestValidateRemoteManagedDaoScopes(t *testing.T) {
	sidDao := func() entity.DaoInterface {
		return &scopedTestDao{testRemoteDao: testRemoteDao{dirty: &testDirty{}}, scope: entity.DatabaseServer}
	}
	globalDao := func() entity.DaoInterface {
		return &scopedTestDao{testRemoteDao: testRemoteDao{dirty: &testDirty{}}, scope: entity.DatabaseGlobal}
	}
	plainDao := func() entity.DaoInterface { return &testRemoteDao{dirty: &testDirty{}} }
	cases := []struct {
		name    string
		builder *entity.EntityBuilderParam
		reject  bool
	}{
		{"managed/global", &entity.EntityBuilderParam{Kind: 91, RemotePolicy: entity.RemotePolicyManaged, DaoBuilders: []entity.DaoBuilderFunc{globalDao, plainDao}}, false},
		{"managed/sid", &entity.EntityBuilderParam{Kind: 92, RemotePolicy: entity.RemotePolicyManaged, DaoBuilders: []entity.DaoBuilderFunc{plainDao, sidDao}}, true},
		{"local/sid", &entity.EntityBuilderParam{Kind: 93, RemotePolicy: entity.RemotePolicyNone, DaoBuilders: []entity.DaoBuilderFunc{sidDao}}, false},
		{"mirror/sid", &entity.EntityBuilderParam{Kind: 94, RemotePolicy: entity.RemotePolicyMirror, DaoBuilders: []entity.DaoBuilderFunc{sidDao}}, false},
		{"managed/nil-dao", &entity.EntityBuilderParam{Kind: 95, RemotePolicy: entity.RemotePolicyManaged, DaoBuilders: []entity.DaoBuilderFunc{nil, func() entity.DaoInterface { return nil }}}, false},
		// RR-20260926-60：kind 定义声明 managed、手写 builder 省略 RemotePolicy（注册表按“部分重复声明”接受）；
		// 校验按 kind 的实际策略判断。
		{"kind-def-managed/builder-omits-policy/sid", &entity.EntityBuilderParam{Kind: 96, DaoBuilders: []entity.DaoBuilderFunc{sidDao}}, true},
	}
	// 校验读取注册表里 kind 的实际策略；这里只登记 kind 定义（不注册 builder），不影响本包其他 Assemble 用例。
	kindPolicy := map[entity.EntityKind]entity.RemotePolicy{91: entity.RemotePolicyManaged, 92: entity.RemotePolicyManaged, 93: entity.RemotePolicyNone, 94: entity.RemotePolicyMirror, 95: entity.RemotePolicyManaged, 96: entity.RemotePolicyManaged}
	for kind, policy := range kindPolicy {
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: policy})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := entity.ValidateRemoteManagedDaoScopes([]*entity.EntityBuilderParam{tc.builder, nil})
			if !tc.reject {
				if err != nil {
					t.Fatalf("legal combination rejected: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
				t.Fatalf("remote=managed with dbscope=sid DAO must be rejected with ErrRemoteManagedServerScopedDAO, got %v", err)
			}
			if !strings.Contains(err.Error(), "dbscope=global") || !strings.Contains(err.Error(), fmt.Sprintf("kind=%d", tc.builder.Kind)) {
				t.Fatalf("error must name the kind and the global alternative: %v", err)
			}
		})
	}
}

const rr45ChildEnv = "ROOST_RR45_ASSEMBLY_CHILD"

// Assemble / Start 读全局实体注册表；非法注册无法撤销，所以在子进程里注册，避免污染本包其他测试。
func TestAssembleRejectsRemoteManagedServerScopedDAO(t *testing.T) {
	switch os.Getenv(rr45ChildEnv) {
	case "assemble":
		// 合法组合不误伤：托管实体 + global DAO、本地实体 + sid DAO。
		registerScopedDaoKind(215, entity.RemotePolicyManaged, entity.DatabaseGlobal)
		registerScopedDaoKind(216, entity.RemotePolicyNone, entity.DatabaseServer)
		if _, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, DefaultConfig(), 7, MongoBackendConfig{}); err != nil {
			t.Fatalf("legal registrations rejected: %v", err)
		}
		registerScopedDaoKind(214, entity.RemotePolicyManaged, entity.DatabaseServer)
		asm, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, DefaultConfig(), 7, MongoBackendConfig{})
		if err == nil || asm != nil {
			t.Fatalf("Assemble accepted a remote-managed kind with a dbscope=sid DAO (asm=%v)", asm != nil)
		}
		if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
			t.Fatalf("want ErrRemoteManagedServerScopedDAO, got %v", err)
		}
		return
	case "kind_def":
		// RR-20260926-60（REPRO-2026-09-26-05 §9）：kind 经 MustRegisterEntityKindDefs 声明 managed，手写 builder 省略
		// RemotePolicy——注册表接受“部分重复声明”，所有 Remote 路径都按 managed 处理，装配校验也必须拦下 sid DAO。
		const kind entity.EntityKind = 217
		entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: kind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1,
			Kind:     kind,
			Builder: func(*entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				return nil, errors.New("never built")
			},
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &scopedTestDao{testRemoteDao: testRemoteDao{dirty: &testDirty{}}, scope: entity.DatabaseServer}
			}},
		})
		if !entity.IsEntityKindRemoteManaged(kind) {
			t.Fatal("setup: kind is not managed")
		}
		asm, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: &atomicTestBackend{newRemoteTestLoader()}}, DefaultConfig(), 7, MongoBackendConfig{})
		if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
			t.Fatalf("kind %d is remote-managed (IsEntityKindRemoteManaged=true) with a dbscope=sid DAO, but Assemble accepted it (asm=%v err=%v)", kind, asm != nil, err)
		}
		return
	case "start":
		// 装配之后才注册的手写实体，由 Start 拦下。
		a, bus := newLifecycleAssembly(t, func(context.Context) error { return nil })
		registerScopedDaoKind(214, entity.RemotePolicyManaged, entity.DatabaseServer)
		err := safeAssemblyStart(a, context.Background(), bus)
		if err == nil {
			t.Fatal("Start accepted a remote-managed kind with a dbscope=sid DAO")
		}
		if !errors.Is(err, ErrRemoteManagedServerScopedDAO) {
			t.Fatalf("want ErrRemoteManagedServerScopedDAO, got %v", err)
		}
		if bus.active.Load() != 0 {
			t.Fatalf("rejected Start left %d subscriptions", bus.active.Load())
		}
		return
	}
	for _, mode := range []string{"assemble", "start", "kind_def"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestAssembleRejectsRemoteManagedServerScopedDAO$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), rr45ChildEnv+"="+mode)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child %s failed: %v\n%s", mode, err, out)
			}
		})
	}
}

type handlerBus struct {
	mu       sync.Mutex
	handlers map[string]*fsyncbus.Subscription
}

func (*handlerBus) Publish(*fsyncbus.SyncMsg) error { return nil }
func (b *handlerBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.handlers == nil {
		b.handlers = make(map[string]*fsyncbus.Subscription)
	}
	sub := fsyncbus.NewSubscription(topic, h, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.handlers, topic)
	})
	b.handlers[topic] = sub
	return sub, nil
}

// SubscribeLive：测试直接投递，订阅即确认（Mirror 第 4 步的推送模式）。
func (b *handlerBus) SubscribeLive(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.Subscribe(topic, h)
}

// handler 返回 topic 当前订阅的投递入口；没有订阅时返回 nil。
func (b *handlerBus) handler(topic string) func(*fsyncbus.SyncMsg) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub := b.handlers[topic]
	if sub == nil {
		return nil
	}
	return func(msg *fsyncbus.SyncMsg) error { return sub.Deliver(context.Background(), msg) }
}

func interestRenewMessage(t *testing.T, interest entity.RemoteSnapshotInterest) *fsyncbus.SyncMsg {
	t.Helper()
	raw, err := mirror.MarshalPayload(remoteInterestWire{Interest: interest})
	if err != nil {
		t.Fatal(err)
	}
	env := mirror.Envelope{Topic: SyncTopicInterest, Key: remoteInterestReplicaKey(interest), Version: interest.ExpiresAt, Op: mirror.OpUpsert, Payload: raw}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return &fsyncbus.SyncMsg{Topic: SyncTopicInterest, Key: env.Key, Version: env.Version, Data: data}
}

// waitForGoroutineIn 等到某个 goroutine 的栈里出现 frame（被测试持有的锁卡住），有界。
func waitForGoroutineIn(t *testing.T, frame string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), frame) {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("no goroutine reached %s", frame)
}

// awaitStopOrCancelWhileWaiting 返回 stopped 的结果；若停止先进入 waitingFrame（在等排空），先 cancel 再取结果。
func awaitStopOrCancelWhileWaiting(t *testing.T, stopped <-chan error, cancel context.CancelFunc, waitingFrame string) error {
	t.Helper()
	buf := make([]byte, 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-stopped:
			return err
		default:
		}
		n := runtime.Stack(buf, true)
		if strings.Contains(string(buf[:n]), waitingFrame) {
			cancel()
			select {
			case err := <-stopped:
				return err
			case <-time.After(5 * time.Second):
				t.Fatal("Stop ignored its context while waiting for the handler")
			}
		}
		runtime.Gosched()
	}
	t.Fatal("Stop neither returned nor waited")
	return nil
}

func TestRemoteAssemblyStopWaitsForAnInFlightReplicaHandler(t *testing.T) {
	a, _ := newLifecycleAssembly(t, func(context.Context) error { return nil })
	bus := &handlerBus{}
	if err := a.Start(context.Background(), bus); err != nil {
		t.Fatal(err)
	}
	handler := bus.handler(SyncTopicInterest)
	if handler == nil {
		t.Fatal("interest replicator did not subscribe")
	}
	interest := entity.RemoteSnapshotInterest{Key: interestKeyFor(t, 244, 9471), ConsumerSID: 9, ExpiresAt: time.Now().Add(time.Hour).UnixNano(), Generation: 1}

	// 测试持有兴趣表的锁：投递进入 ApplyReplica 后卡在 renew 上。
	registry := a.Manager.snapshots.interests
	registry.mu.Lock()
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(registry.mu.Unlock) }
	defer unlock()
	applied := make(chan error, 1)
	go func() { applied <- handler(interestRenewMessage(t, interest)) }()
	waitForGoroutineIn(t, "remoteInterestRegistry).renewIfNeeded")

	// 第一次停止：在它等待在途 handler 时取消（预算耗尽）；修前 Stop 不等，直接返回。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- a.Stop(ctx) }()
	stopErr := awaitStopOrCancelWhileWaiting(t, stopped, cancel, "mirror.(*Replicator).StopWithContext")
	if stopErr == nil {
		t.Errorf("Stop reported success while a replica handler was still applying")
	} else if !errors.Is(stopErr, context.Canceled) {
		t.Errorf("Stop = %v, want context.Canceled", stopErr)
	}
	select {
	case err := <-applied:
		t.Fatalf("handler finished while the test still held the registry lock: %v", err)
	default:
	}

	// 退订已经发生：新投递不再进入；handler 放行后用新 ctx 重试，等到它返回才报告成功。
	if bus.handler(SyncTopicInterest) != nil {
		t.Error("timed-out Stop left the interest subscription in place")
	}
	unlock()
	if err := <-applied; err != nil {
		t.Fatalf("in-flight renew: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop after the handler returned = %v", err)
	}
	if !a.Manager.snapshots.interests.interested(interest.Key) {
		t.Fatal("the admitted renewal was lost")
	}
}

type retrySubscriptionBus struct {
	calls, active int
	failure       error
}

func (*retrySubscriptionBus) Publish(*fsyncbus.SyncMsg) error { return nil }

// SubscribeLive 让快照推送开着（Mirror 第 4 步），全部订阅都经同一个计数。
func (b *retrySubscriptionBus) SubscribeLive(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.Subscribe(topic, h)
}

func (b *retrySubscriptionBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	b.calls++
	if b.calls == 2 {
		return nil, b.failure
	}
	b.active++
	return fsyncbus.NewSubscription(topic, h, func() { b.active-- }), nil
}

func TestRemoteAssemblyRetryAfterSecondSubscriptionFailure(t *testing.T) {
	a, _ := newLifecycleAssembly(t, func(context.Context) error { return nil })
	failure := errors.New("second subscription refused")
	b := &retrySubscriptionBus{failure: failure}
	if err := a.Start(context.Background(), b); !errors.Is(err, failure) {
		t.Fatalf("first Start: %v", err)
	}
	if b.active != 0 || b.calls != 2 {
		t.Fatalf("failed Start leaked: active=%d calls=%d", b.active, b.calls)
	}
	for range 2 {
		if err := a.Start(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	// 重试订阅全部三个（快照、兴趣、兴趣续租请求），总共 2 + 3 次调用。
	if b.active != liveSubscriptions || b.calls != 2+liveSubscriptions {
		t.Fatalf("retry binding: active=%d calls=%d", b.active, b.calls)
	}
	for range 2 {
		if err := a.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if b.active != 0 {
		t.Fatalf("Stop leaked subscriptions: %d", b.active)
	}
}

type lifecycleRemoteBackend struct {
	*atomicTestBackend
	initialize func(context.Context) error
}

func (b *lifecycleRemoteBackend) EnsureRemoteStorage(ctx context.Context) error {
	return b.initialize(ctx)
}

// liveSubscriptions 是推送开着时一个 SnapshotClient 的订阅数：快照、兴趣、兴趣续租请求（O-M6-1）。
const liveSubscriptions = 3

type lifecycleRemoteBus struct {
	active atomic.Int32
	calls  atomic.Int32
}

func (*lifecycleRemoteBus) Publish(*fsyncbus.SyncMsg) error { return nil }

// SubscribeLive 让快照推送开着（Mirror 第 4 步），全部订阅都经同一个计数。
func (b *lifecycleRemoteBus) SubscribeLive(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return b.Subscribe(topic, h)
}

func (b *lifecycleRemoteBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	b.calls.Add(1)
	b.active.Add(1)
	return fsyncbus.NewSubscription(topic, h, func() { b.active.Add(-1) }), nil
}
func newLifecycleAssembly(t *testing.T, init func(context.Context) error) (*Assembly, *lifecycleRemoteBus) {
	t.Helper()
	b := &lifecycleRemoteBackend{atomicTestBackend: &atomicTestBackend{newRemoteTestLoader()}, initialize: init}
	a, err := Assemble(AssemblyDeps{Redis: assembleRedis{}, Backend: b}, DefaultConfig(), 7, MongoBackendConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := a.Stop(ctx); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return a, &lifecycleRemoteBus{}
}
func safeAssemblyStart(a *Assembly, ctx context.Context, b *lifecycleRemoteBus) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("Start panic: %v", p)
		}
	}()
	return a.Start(ctx, b)
}
func TestRemoteAssemblyDuplicateStart(t *testing.T) {
	a, b := newLifecycleAssembly(t, func(context.Context) error { return nil })
	for range 2 {
		if err := safeAssemblyStart(a, context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	if b.active.Load() != liveSubscriptions || b.calls.Load() != liveSubscriptions {
		t.Fatalf("subscriptions active=%d calls=%d", b.active.Load(), b.calls.Load())
	}
}
func TestRemoteAssemblyRetryAfterStorageFailure(t *testing.T) {
	var calls atomic.Int32
	failure := errors.New("storage unavailable")
	a, b := newLifecycleAssembly(t, func(context.Context) error {
		if calls.Add(1) == 1 {
			return failure
		}
		return nil
	})
	if err := safeAssemblyStart(a, context.Background(), b); !errors.Is(err, failure) {
		t.Fatalf("first Start: %v", err)
	}
	if b.active.Load() != 0 {
		t.Fatal("failed Start leaked subscriptions")
	}
	if err := safeAssemblyStart(a, context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if b.active.Load() != liveSubscriptions {
		t.Fatal("retry did not restore subscriptions")
	}
}
func TestRemoteAssemblyStopBeforeStartIsTerminal(t *testing.T) {
	a, b := newLifecycleAssembly(t, func(context.Context) error { return nil })
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := safeAssemblyStart(a, context.Background(), b); err == nil {
		t.Fatal("Start succeeded with a stopped finalizer")
	}
	if b.active.Load() != 0 {
		t.Fatal("stopped assembly subscribed")
	}
}
func TestRemoteAssemblyCanceledStartDoesNotSubscribe(t *testing.T) {
	a, b := newLifecycleAssembly(t, func(context.Context) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := safeAssemblyStart(a, ctx, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start=%v", err)
	}
	if b.calls.Load() != 0 {
		t.Fatal("canceled start subscribed")
	}
}

func TestRemoteAssemblyConcurrentLifecycleWaitIsCancelable(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	a, b := newLifecycleAssembly(t, func(context.Context) error { close(entered); <-release; return nil })
	defer releaseOnce()
	result := make(chan error, 1)
	go func() { result <- safeAssemblyStart(a, context.Background(), b) }()
	awaitChan(t, entered, "storage initialization")
	for _, op := range []func(context.Context) error{
		func(ctx context.Context) error { return safeAssemblyStart(a, ctx, b) }, a.Stop,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := op(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued operation=%v", err)
		}
	}
	releaseOnce()
	if err := awaitChan(t, result, "Start completion"); err != nil {
		t.Fatal(err)
	}
	if b.calls.Load() != liveSubscriptions {
		t.Fatal("concurrent Start rebound subscriptions")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.active.Load() != 0 {
		t.Fatal("Stop left subscriptions")
	}
}

func TestRemoteAssemblyStopTimeoutKeepsReplicationUntilDrained(t *testing.T) {
	a, b := newLifecycleAssembly(t, func(context.Context) error { return nil })
	// 模拟已被接纳但尚未完成的 finalizer 工作；Stop 不能提前拆走它使用的发布依赖。
	a.Manager.remote.retryWG.Add(1)
	finish := sync.OnceFunc(a.Manager.remote.retryWG.Done)
	defer finish()
	if err := safeAssemblyStart(a, context.Background(), b); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop=%v", err)
	}
	if b.active.Load() != liveSubscriptions {
		t.Fatal("timed-out Stop discarded replication")
	}
	if err := safeAssemblyStart(a, context.Background(), b); !errors.Is(err, ErrAssemblyStopped) {
		t.Fatalf("restart=%v", err)
	}
	finish()
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.active.Load() != 0 {
		t.Fatal("retry Stop leaked subscriptions")
	}
}

// Assembly.Stop 停复制的部分套共用停机契约骨架。卡住的工作是已进入 ApplyReplica 的兴趣续期（测试持有兴趣表的锁）；
// Assembly 不持有 Redis / SyncBus，“资源”是 kit Mod / App 在 Stop 返回 nil 之后才停的这些依赖（stopcontract.CallerReleases）。
// 历史：RR-20261005-NC-174。
func TestRemoteAssemblyStopContract(t *testing.T) {
	var a *Assembly
	bus := &handlerBus{}
	applied := make(chan error, 1)
	var unlock func()
	var stop func(context.Context) error
	var released func() bool
	interest := entity.RemoteSnapshotInterest{Key: interestKeyFor(t, 244, 9472), ConsumerSID: 9, ExpiresAt: time.Now().Add(time.Hour).UnixNano(), Generation: 1}
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(testing.TB) {
			a, _ = newLifecycleAssembly(t, func(context.Context) error { return nil })
			if err := a.Start(context.Background(), bus); err != nil {
				t.Fatal(err)
			}
			stop, released = stopcontract.CallerReleases(a.Stop)
		},
		Block: func(testing.TB) {
			handler := bus.handler(SyncTopicInterest)
			if handler == nil {
				t.Fatal("interest replicator did not subscribe")
			}
			registry := a.Manager.snapshots.interests
			registry.mu.Lock()
			unlock = sync.OnceFunc(registry.mu.Unlock)
			go func() { applied <- handler(interestRenewMessage(t, interest)) }()
			waitForGoroutineIn(t, "remoteInterestRegistry).renewIfNeeded")
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { unlock() },
		Released: func() bool { return released() },
	})
	if err := <-applied; err != nil {
		t.Fatalf("in-flight renew: %v", err)
	}
	if !a.Manager.snapshots.interests.interested(interest.Key) {
		t.Fatal("the admitted renewal was lost")
	}
}

// The backend needs both halves, and a storage that cannot run a transaction
// must say so rather than commit the batch piecewise.
func TestBackendRequiresBothHalvesAndAtomicStoreForTransactions(t *testing.T) {
	if _, err := NewBackend(nil, plainStorageBackend{}); err == nil {
		t.Fatal("backend without loader accepted")
	}
	if _, err := NewBackend(nopLoader{}, nil); err == nil {
		t.Fatal("backend without storage accepted")
	}
	backend, err := NewBackend(nopLoader{}, plainStorageBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyRemoteCommitsInTransaction(context.Background(), []entity.RemoteCommit{{}}); !errors.Is(err, entity.ErrRemoteAtomicBatchUnsupported) {
		t.Fatalf("non-atomic storage err = %v, want ErrRemoteAtomicBatchUnsupported", err)
	}
}
