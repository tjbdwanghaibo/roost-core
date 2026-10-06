package remoteentity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/internal/stopcontract"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	coreremote "github.com/tjbdwanghaibo/roost-core/remoteentity"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"
)

// Mirror 第 5 步：kit 只读装配 RemoteMirrorMod（docs/feature/MIRROR-STEP-5-2026-10-06.md）。
//
// kind 252：本包已用 253（interest_health_promises_test.go），两者不能撞号。
const mirrorModKind entity.EntityKind = 252

func mirrorModKey(t *testing.T, raw int64) entity.RemoteSnapshotKey {
	t.Helper()
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: mirrorModKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	id, err := entity.BuildEntityID(raw, mirrorModKind)
	if err != nil {
		t.Fatal(err)
	}
	return entity.RemoteSnapshotKey{EntityID: id, Kind: mirrorModKind, Scope: entity.RemoteSnapshotScope("guild")}
}

// emptyRedis 是没有任何键的 Redis：L2 读是未命中（ErrNil），写失败（缓存按 L2 不可用降级，B2）。
type emptyRedis struct{ fredis.IRedis }

func (emptyRedis) HGet(context.Context, string, string) ([]byte, error) { return nil, fredis.ErrNil }
func (emptyRedis) Eval(context.Context, string, []string, ...any) (any, error) {
	return nil, errors.New("emptyRedis: no scripts")
}
func (emptyRedis) Del(context.Context, ...string) (int64, error) { return 0, nil }

// plainBus 是没有可确认订阅的总线（普通 NATS 的形状）：推送关闭，按需读取。
type plainBus struct{}

func (plainBus) Publish(*fsyncbus.SyncMsg) error { return nil }
func (plainBus) Subscribe(topic string, h fsyncbus.Handler) (*fsyncbus.Subscription, error) {
	return fsyncbus.NewSubscription(topic, h, nil), nil
}

// mirrorRegistry 装好 RemoteMirrorMod 的依赖（不含 Mongo：用例用 WithMirrorLoader）。
func mirrorRegistry(t *testing.T, cfg *viper.Viper) *app.Registry {
	t.Helper()
	r := app.NewRegistry(cfg)
	if err := r.Register(mods.ModRedis, fredis.IRedis(emptyRedis{})); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(mods.ModSyncBus, fsyncbus.ISyncBus(plainBus{})); err != nil {
		t.Fatal(err)
	}
	return r
}

func startMirrorMod(t *testing.T, loader entity.RemoteSnapshotLoader) (*RemoteMirrorMod, *app.Registry) {
	t.Helper()
	cfg := viper.New()
	mod := NewRemoteMirrorMod(4100, WithMirrorLoader(loader, false))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	r := mirrorRegistry(t, cfg)
	if err := mod.Provide(r); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	return mod, r
}

// 只读：注册表里只多一个只读能力，没有写 Manager、原子 backend 或锁；能力本身拿不到发布 / 提交接口；
// 不要求 Mongo 原子 backend（换了 loader 时连 mongo Mod 都不依赖）。
func TestRemoteMirrorModRegistersOnlyReadCapability(t *testing.T) {
	mod, r := startMirrorMod(t, func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		return entity.RemoteSnapshotEnvelope{}, false, nil
	})
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })
	source, err := MirrorSource(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := source.(entity.IRemoteSnapshotPublisher); ok {
		t.Errorf("the mirror capability %T can publish snapshots; a read-only service must not carry write capability", source)
	}
	if _, ok := source.(entity.IRemoteEntityManager); ok {
		t.Errorf("the mirror capability %T is a remote entity manager", source)
	}
	if _, ok := source.(*coreremote.Manager); ok {
		t.Errorf("the mirror capability is the write Manager")
	}
	for _, name := range []app.ModName{mods.ModRemoteEntity, mods.ModRemoteEntityAtomicStore, mods.ModRedisVLock} {
		if value, ok := r.Get(name); ok {
			t.Errorf("read-only assembly registered write capability %q (%T)", name, value)
		}
	}
	if slices.Contains(mod.DependsOn(), mods.ModMongo) {
		t.Errorf("DependsOn=%v: with a supplied loader the read-only assembly needs no Mongo", mod.DependsOn())
	}
	if !slices.Contains(NewRemoteMirrorMod(1).DependsOn(), mods.ModMongo) {
		t.Errorf("the default read-only Mongo loader must depend on the mongo Mod")
	}
	// Linearizable 只在 loader 声明时开放；缺省不声明，不静默退化。
	key := mirrorModKey(t, 9100)
	if _, _, err := source.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadLinearizable}); !errors.Is(err, entity.ErrRemoteReadUnsupported) {
		t.Errorf("Linearizable read through an undeclared loader = %v, want ErrRemoteReadUnsupported", err)
	}
}

// 同一进程同时装写 owner（RemoteEntityMod 已把 Manager.SnapshotClient 登记为同一能力）与只读 Mod：
// 启动即失败，不为同一 sid 开第二个客户端。
func TestRemoteMirrorModRefusesASecondClientBesideTheOwner(t *testing.T) {
	cfg := viper.New()
	mod := NewRemoteMirrorMod(4101, WithMirrorLoader(func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		return entity.RemoteSnapshotEnvelope{}, false, nil
	}, false))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	r := mirrorRegistry(t, cfg)
	owner := coreremote.NewManager(nil, coreremote.DefaultConfig(), 4101)
	if err := r.Register(mods.ModRemoteMirror, entity.RemoteSnapshotReadOnly(owner.SnapshotClient())); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(r); err == nil || !strings.Contains(err.Error(), string(mods.ModRemoteMirror)) {
		t.Fatalf("Provide beside the owner's capability = %v, want a capability conflict", err)
	}
	// 业务读取代码在 owner 进程里拿到的是 owner 的客户端。
	if source, err := MirrorSource(r); err != nil || source != entity.RemoteSnapshotReadOnly(owner.SnapshotClient()) {
		t.Fatalf("MirrorSource in the owner process = %T, %v", source, err)
	}
}

// 严格读取（A4）：快照段与 RemoteEntityMod 同一套规则；新键 remote_entity.mirror.shutdown_timeout 是停机预算，
// 不带单位或非正值拒绝；sid 必须非零。锁 / 提交的键不读（Cluster 下没有 hash tag 的 lock_key 不影响只读装配）。
func TestRemoteMirrorModConfiguration(t *testing.T) {
	cfg := viper.New()
	cfg.Set("remote_entity.snapshot_cache_ttl", "7s")
	cfg.Set("remote_entity.cached_max_staleness", "3s")
	cfg.Set("remote_entity.snapshot_interest_subs", 160)
	cfg.Set("remote_entity.snapshot_interest_per_consumer", 10)
	cfg.Set("remote_entity.snapshot_l2_key_prefix", "deploy-a")
	cfg.Set("remote_entity.mirror.shutdown_timeout", "9s")
	cfg.Set("remote_entity.mongo.database", "guilds")
	cfg.Set("redis.cluster_addrs", []string{"127.0.0.1:7000"})
	cfg.Set("remote_entity.lock_key", "no-hash-tag")
	mod := NewRemoteMirrorMod(4102)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if mod.cfg.SnapshotCacheTTL != 7*time.Second || mod.cfg.CachedMaxStaleness != 3*time.Second ||
		mod.cfg.SnapshotInterestPerConsumer != 10 || mod.cfg.SnapshotL2KeyPrefix != "deploy-a" ||
		mod.StopBudget() != 9*time.Second || mod.database != "guilds" {
		t.Fatalf("config not applied: %+v budget=%v database=%q", mod.cfg, mod.StopBudget(), mod.database)
	}
	if got := NewRemoteMirrorMod(1); got.StopBudget() != 0 {
		t.Fatalf("stop budget before Init = %v, want 0 (not declared)", got.StopBudget())
	}
	defaults := NewRemoteMirrorMod(4103)
	if err := defaults.Init(viper.New()); err != nil || defaults.StopBudget() != defaultMirrorShutdownTimeout || defaults.database != "remote_entity" {
		t.Fatalf("defaults: err=%v budget=%v database=%q", err, defaults.StopBudget(), defaults.database)
	}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"remote_entity.mirror.shutdown_timeout", 5},
		{"remote_entity.mirror.shutdown_timeout", "0s"},
		{"remote_entity.cached_max_staleness", "0s"},
		{"remote_entity.snapshot_interest_per_consumer", 1 << 30},
		{"remote_entity.snapshot_l2_key_prefix", "{tag}"},
		{"remote_entity.snapshot_cache_ttl", "soon"},
	} {
		bad := viper.New()
		bad.Set(tc.key, tc.value)
		if err := NewRemoteMirrorMod(4104).Init(bad); err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%v: Init = %v, want a rejection naming the key", tc.key, tc.value, err)
		}
	}
	if err := NewRemoteMirrorMod(0).Init(viper.New()); err == nil {
		t.Error("Init without a sid succeeded; the consumer identity must be non-zero")
	}
	// 新键登记进 A4 的框架键：ValidateServiceConfig 一并检查类型。
	bad := viper.New()
	bad.Set("remote_entity.mirror.shutdown_timeout", 5)
	if err := app.ValidateServiceConfig(bad); err == nil || !strings.Contains(err.Error(), "remote_entity.mirror.shutdown_timeout") {
		t.Errorf("ValidateServiceConfig with a unitless shutdown timeout = %v", err)
	}
}

// 停机契约（A3 骨架）：卡在不响应取消的权威加载上时，停止如实超时、重试再等；放行后返回 nil，App 才释放
// redis / mongo / syncbus；之后再调用返回 nil。
func TestRemoteMirrorModStopContract(t *testing.T) {
	key := mirrorModKey(t, 9101)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	read := make(chan error, 1)
	var mod *RemoteMirrorMod
	var stop func(context.Context) error
	var released func() bool
	stopcontract.Check(t, stopcontract.Hooks{
		Start: func(t testing.TB) {
			mod, _ = startMirrorMod(t.(*testing.T), func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
				entered <- struct{}{}
				<-release // 不响应取消的 loader
				return entity.RemoteSnapshotEnvelope{}, false, nil
			})
			stop, released = stopcontract.CallerReleases(mod.StopWithContext)
		},
		Block: func(t testing.TB) {
			go func() {
				_, _, err := mod.client.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic})
				read <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("the read never reached the loader")
			}
		},
		Stop:     func(ctx context.Context) error { return stop(ctx) },
		Release:  func() { close(release) },
		Released: func() bool { return released() },
	})
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("the admitted read never returned")
	}
}

// 停止取消在途读取：响应取消的权威加载在 Stop 发起后返回，Stop 在预算内返回 nil；在途读取拿到错误而不是
// 一直挂着；之后的读返回 ErrSnapshotClientStopped；重复关闭返回 nil；健康检查报停止。
func TestRemoteMirrorModStopCancelsInFlightReads(t *testing.T) {
	key := mirrorModKey(t, 9102)
	entered := make(chan struct{}, 1)
	mod, r := startMirrorMod(t, func(ctx context.Context, _ entity.RemoteSnapshotKey, _ entity.RemoteReadConsistency, _ uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return entity.RemoteSnapshotEnvelope{}, false, ctx.Err()
	})
	source, err := MirrorSource(r)
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		// 调用方自己的 ctx 不会结束：只有停止能让它返回。
		_, _, err := source.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadMonotonic})
		read <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never reached the loader")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := mod.StopWithContext(ctx); err != nil {
		t.Fatalf("Stop with a cancellable read in flight = %v; the in-flight load must be cancelled by the stop", err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("the cancelled read returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the in-flight read did not return after Stop")
	}
	if _, _, err := source.ReadSnapshot(context.Background(), entity.RemoteSnapshotRead{Key: key, Consistency: entity.RemoteReadCached}); !errors.Is(err, coreremote.ErrSnapshotClientStopped) {
		t.Fatalf("read after Stop = %v, want ErrSnapshotClientStopped", err)
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatalf("second Stop = %v, want nil", err)
	}
	if result := mod.checkHealth(context.Background()); result.Status != health.StatusFail {
		t.Fatalf("health after Stop = %s (%s), want fail", result.Status, result.Message)
	}
}

// 健康信息带推送模式与兴趣拒绝数；普通 NATS 形状的总线上推送关闭是显式退化，不算故障。
func TestRemoteMirrorModHealthReportsPushMode(t *testing.T) {
	mod, _ := startMirrorMod(t, func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		return entity.RemoteSnapshotEnvelope{}, false, nil
	})
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })
	result := mod.checkHealth(context.Background())
	if result.Status != health.StatusOK || !strings.Contains(result.Message, "snapshot_push=false") || !strings.Contains(result.Message, "interest_refused=0") {
		t.Fatalf("health = %s (%s), want ok with snapshot_push=false interest_refused=0", result.Status, result.Message)
	}
}
