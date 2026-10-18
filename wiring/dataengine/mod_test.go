package dataengine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

type modJetStream struct{ streams int }

func (stream *modJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error {
	stream.streams++
	return nil
}
func (*modJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (*modJetStream) Subscribe(context.Context, fnats.JetStreamConsumerConfig, fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	return nil, errors.New("unused")
}

func TestDataEngineModReadsProjectionBatchByteLimit(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.projection.batch_bytes", 2<<20)
	cfg.Set("dataengine.projection.read_bytes", 8<<20)
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got := mod.cfg.projector.ReplayBatchBytes; got != 2<<20 {
		t.Fatalf("projection batch bytes=%d", got)
	}
	if got := mod.cfg.projector.ReplayReadBytes; got != 8<<20 {
		t.Fatalf("read bytes=%d", got)
	}
	cfg.Set("dataengine.projection.read_bytes", -1)
	if err := mod.Init(cfg); err == nil {
		t.Fatal("negative read budget accepted")
	}
}

// 0 取 core 缺省；负数自 A4 ① 起由声明拒绝（以前静默取缺省）。
func TestDataEngineModKeepsProjectionBatchByteDefaultForZero(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.projection.batch_bytes", 0)
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got, want := mod.cfg.projector.ReplayBatchBytes, 4<<20; got != want {
		t.Fatalf("projection batch bytes=%d want default %d", got, want)
	}
	cfg.Set("dataengine.projection.batch_bytes", -1)
	if err := NewMod().Init(cfg); err == nil || !strings.Contains(err.Error(), "dataengine.projection.batch_bytes must not be negative") {
		t.Fatalf("batch_bytes: -1: Init = %v, want a refusal naming the key", err)
	}
}

func TestEffectStreamDefaultsMatchTheDeclaration(t *testing.T) {
	effects, err := LoadEffectsConfig(nil)
	if err != nil || effects.Stream != DefaultEffectStream || effects.MaxAge != DefaultEffectMaxAge {
		t.Fatalf("LoadEffectsConfig(nil) = %+v, %v; want the declared defaults %q, %v", effects, err, DefaultEffectStream, DefaultEffectMaxAge)
	}
}

func TestDataEngineModRecoversBeforeReadyAndOwnsNestOptions(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.wal.dir", t.TempDir())
	mongoClient := mongotest.NewClient()
	jetStream := &modJetStream{}
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModMongo, fmongo.IMongo(mongoClient)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModNatsJetStream, fnats.IJetStream(jetStream)); err != nil {
		t.Fatal(err)
	}
	access := entity.NewManagerAccess(entity.NewEntityManager())
	mod := NewMod(WithEntityAccess(access))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if mod.Runtime() != nil || len(mod.NestOptions()) == 0 {
		t.Fatalf("runtime=%v options=%d; Provide must expose a lazy committer without opening WAL", mod.Runtime(), len(mod.NestOptions()))
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop()
	if mod.Runtime() == nil || !mod.Runtime().Ready() || len(mod.NestOptions()) == 0 || jetStream.streams != 1 {
		t.Fatalf("runtime=%v ready=%v options=%d streams=%d", mod.Runtime(), mod.Runtime() != nil && mod.Runtime().Ready(), len(mod.NestOptions()), jetStream.streams)
	}
}

func TestDataEngineProjectionCheckpointAndBacklogConfig(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.projection.checkpoint_records", 32)
	cfg.Set("dataengine.projection.checkpoint_interval", "10ms")
	cfg.Set("dataengine.projection.max_unacked_records", 100)
	cfg.Set("dataengine.projection.warn_unacked_records", 80)
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	opts := mod.cfg.projector
	if opts.CheckpointRecords != 32 || opts.CheckpointInterval.String() != "10ms" || opts.MaxUnackedRecords != 100 || opts.WarnUnackedRecords != 80 {
		t.Fatalf("opts=%+v", opts)
	}
	for _, key := range []string{"checkpoint_records", "checkpoint_interval", "max_unacked_records", "warn_unacked_records"} {
		invalid := viper.New()
		invalid.Set("persistence.engine", "dataengine")
		invalid.Set("dataengine.projection."+key, -1)
		if err := mod.Init(invalid); err == nil {
			t.Fatalf("accepted negative %s", key)
		}
	}
	cfg.Set("dataengine.projection.warn_unacked_records", 101)
	if err := mod.Init(cfg); err == nil {
		t.Fatal("accepted warning above hard limit")
	}
}

func TestDataEngineRemoteProjectionWorkerConfig(t *testing.T) {
	for _, value := range []int{-1, 0, 1, 8, 64, 65} {
		cfg := viper.New()
		cfg.Set("dataengine.projection.remote_workers", value)
		mod := NewMod()
		err := mod.Init(cfg)
		valid := value >= 1 && value <= 64
		if (err == nil) != valid {
			t.Fatalf("workers=%d err=%v", value, err)
		}
		if valid && mod.cfg.projector.RemoteProjectionWorkers != value {
			t.Fatalf("workers=%d configured=%d", value, mod.cfg.projector.RemoteProjectionWorkers)
		}
	}
}

// 生成工程在闲置交还租约前，按接口从 ModDataEngine 取得实体投影屏障；未装配的 Mod 不能回答“已落库”。
func TestModPublishesEntityProjectionBarrier(t *testing.T) {
	var published any = NewMod()
	waiter, ok := published.(interface {
		WaitEntityProjection(context.Context, int64) error
	})
	if !ok {
		t.Fatal("dataengine Mod does not publish WaitEntityProjection")
	}
	if err := waiter.WaitEntityProjection(context.Background(), 1); err == nil {
		t.Fatal("an unprovided Mod reported the entity's projections as landed")
	}
}

// DataEngine 驱逐被跳过的原生步骤留下的实体要在 Nest 业务池持锁执行：Mod 作为 Nest 的 committer 接收 RunLocal 并转交给启动后
// 才创建的 Projector，Nest 绑定前（启动恢复阶段）就地执行；重载回调在 Runtime 启动后才能注册。
func TestModForwardsNestLocalExecutorAndReloadHook(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.wal.dir", t.TempDir())
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModMongo, fmongo.IMongo(mongotest.NewClient())); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModNatsJetStream, fnats.IJetStream(&modJetStream{})); err != nil {
		t.Fatal(err)
	}
	access := entity.NewManagerAccess(entity.NewEntityManager())
	mod := NewMod(WithEntityAccess(access))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if _, err := mod.OnEntityLoaded(func(entity.IThreadSafeEntity) {}); err == nil {
		t.Fatal("reload hook accepted before the runtime exists")
	}
	inPlace := false
	if err := mod.runLocal(func() { inPlace = !fctx.InShortWorker() }); err != nil || !inPlace {
		t.Fatalf("unbound local executor=%v in place=%v", err, inPlace)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop()
	unhook, err := mod.OnEntityLoaded(func(entity.IThreadSafeEntity) {})
	if err != nil {
		t.Fatal(err)
	}
	unhook()
	scheduler := corenest.NewEngine(append(mod.NestOptions(), corenest.NestOptionWithGetter(access))...)
	if err := scheduler.Start(); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Shutdown(context.Background())
	onFast := false
	if err := mod.runLocal(func() { onFast = fctx.InShortWorker() }); err != nil || !onFast {
		t.Fatalf("bound local executor=%v on business worker=%v", err, onFast)
	}
}

// Mod 只负责查找能力、装配、注册与转交，这十条守卫就是它的全部职责：nil 注册表、缺实体访问、缺 Mongo / JetStream /
// Remote Entity / 原子存储 / 健康注册表各自点名拒绝（Provide 失败让进程在启动时停下，而不是 Start 时 nil 解引用）；
// 未 Provide 就 Start 拒绝；Start 之前 Commit / Enqueue 以 ErrCommitterRequired 拒绝，Nest 不能把提交交给还没打开 WAL 的引擎。

// remoteManagerStub is an entity.IRemoteEntityManager that can apply commits;
// nothing else is expected to be called.
type remoteManagerStub struct{ entity.IRemoteEntityManager }

func (remoteManagerStub) ApplyRemoteCommits(context.Context, entity.RemoteTransactionID, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

type remoteStoreStub struct{}

func (remoteStoreStub) ApplyRemoteCommitsInTransaction(context.Context, []entity.RemoteCommit) ([]entity.RemoteCommitReceipt, error) {
	return nil, nil
}

func newModConfig(t *testing.T) *viper.Viper {
	t.Helper()
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.wal.dir", t.TempDir())
	return cfg
}

func TestModProvideRefusesEachMissingCapability(t *testing.T) {
	cfg := newModConfig(t)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	type registration func(*app.Registry)
	withMongo := func(r *app.Registry) { _ = r.Register(mods.ModMongo, fmongo.IMongo(mongotest.NewClient())) }
	withJetStream := func(r *app.Registry) { _ = r.Register(mods.ModNatsJetStream, fnats.IJetStream(&modJetStream{})) }
	withRemote := func(r *app.Registry) {
		_ = r.Register(mods.ModRemoteEntity, entity.IRemoteEntityManager(remoteManagerStub{}))
	}
	withAtomic := func(r *app.Registry) {
		_ = r.Register(mods.ModRemoteEntityAtomicStore, engine.RemoteProjectionStore(remoteStoreStub{}))
	}

	if err := NewMod(WithEntityAccess(access)).Provide(nil); err == nil || !strings.Contains(err.Error(), "nil registry") {
		t.Fatalf("Provide(nil) = %v", err)
	}
	cases := []struct {
		name    string
		mod     *Mod
		regs    []registration
		text    string
		wantErr error
	}{
		{"no entity access", NewMod(), []registration{withMongo, withJetStream}, "entity access is required", nil},
		{"access without manager", NewMod(WithEntityAccess(&entity.ManagerAccess{})), []registration{withMongo, withJetStream}, "entity access is required", nil},
		{"no mongo", NewMod(WithEntityAccess(access)), []registration{withJetStream}, `capability "mongo" not found`, nil},
		{"no jetstream", NewMod(WithEntityAccess(access)), []registration{withMongo}, `capability "nats.jetstream" not found`, nil},
		{"remote projection without manager", NewMod(WithEntityAccess(access), WithRemoteProjection(true)), []registration{withMongo, withJetStream, withAtomic}, `capability "remote_entity" not found`, nil},
		{"remote projection without atomic store", NewMod(WithEntityAccess(access), WithRemoteProjection(true)), []registration{withMongo, withJetStream, withRemote}, `capability "remote_entity.atomic_store" not found`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := app.NewRegistry(cfg)
			for _, reg := range tc.regs {
				reg(registry)
			}
			if err := tc.mod.Init(cfg); err != nil {
				t.Fatal(err)
			}
			err := tc.mod.Provide(registry)
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Provide = %v, want error containing %q", err, tc.text)
			}
			if _, registered := registry.Get(mods.ModDataEngine); registered {
				t.Fatal("a refused Provide still published the dataengine capability")
			}
		})
	}
}

func TestModRefusesLifecycleCallsBeforeProvideAndStart(t *testing.T) {
	ctx := context.Background()
	cfg := newModConfig(t)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	mod := NewMod(WithEntityAccess(access))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	// 未 Provide：Start 拒绝，提交入口拒绝而不是 nil 解引用。
	if err := mod.Start(); err == nil || !strings.Contains(err.Error(), "not provided") {
		t.Fatalf("Start before Provide = %v", err)
	}
	var none *Mod
	if err := none.Start(); err == nil || !strings.Contains(err.Error(), "not provided") {
		t.Fatalf("nil Mod Start = %v", err)
	}
	record := corenest.CommitRecord{ID: corenest.TransactionID{1}}
	if err := mod.Commit(ctx, record); !errors.Is(err, corenest.ErrCommitterRequired) {
		t.Fatalf("Commit before Start = %v, want ErrCommitterRequired", err)
	}
	if ticket, err := mod.Enqueue(ctx, record); !errors.Is(err, corenest.ErrCommitterRequired) || ticket != nil {
		t.Fatalf("Enqueue before Start = (%v, %v), want ErrCommitterRequired", ticket, err)
	}

	// Provide 后、Start 前：runtime 仍为空，提交入口同样拒绝。
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModMongo, fmongo.IMongo(mongotest.NewClient())); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModNatsJetStream, fnats.IJetStream(&modJetStream{})); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Commit(ctx, record); !errors.Is(err, corenest.ErrCommitterRequired) {
		t.Fatalf("Commit after Provide, before Start = %v", err)
	}
	if _, err := mod.Enqueue(ctx, record); !errors.Is(err, corenest.ErrCommitterRequired) {
		t.Fatalf("Enqueue after Provide, before Start = %v", err)
	}
}

func TestModFatalFencesNestAndSignalsApplication(t *testing.T) {
	registry := app.NewRegistry(viper.New())
	engine := corenest.NewEngine()
	if err := registry.Register(mods.ModNest, engine); err != nil {
		t.Fatal(err)
	}
	mod := &Mod{registry: registry}
	cause := errors.New("indeterminate physical write")
	mod.onFatal(cause)
	if err := engine.FenceError(); !errors.Is(err, corenest.ErrNestFenced) || !errors.Is(err, cause) {
		t.Fatalf("fence=%v", err)
	}
	failure := app.MustLookup[*app.RuntimeFailure](registry, app.ModRuntimeFailure)
	if err := failure.Err(); !errors.Is(err, cause) {
		t.Fatalf("runtime failure=%v", err)
	}
}

// Mod.Init 的正式配置路径拒绝短于 WAL 告警窗口的事务标记保留期。
func TestTransactionReceiptRetentionExceedsWALWindow(t *testing.T) {
	for _, ttl := range []string{"1h", "24h", "24h500ms"} {
		t.Run(ttl, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("persistence.engine", "dataengine")
			cfg.Set("dataengine.transaction_receipt_ttl", ttl)
			if err := NewMod().Init(cfg); err == nil || !strings.Contains(err.Error(), "transaction_receipt_ttl") {
				t.Fatalf("transaction_receipt_ttl=%s accepted: %v", ttl, err)
			}
		})
	}
	for _, ttl := range []string{"24h1s", "720h"} {
		cfg := viper.New()
		cfg.Set("persistence.engine", "dataengine")
		cfg.Set("dataengine.transaction_receipt_ttl", ttl)
		if err := NewMod().Init(cfg); err != nil {
			t.Fatalf("valid transaction_receipt_ttl=%s: %v", ttl, err)
		}
	}
}

// dataengine.shutdown_timeout 通过 App 的停机预算接口生效：App 的 StopWithContext 路径按声明值给截止时间（总时长足够时）。
func TestDataEngineModDeclaresShutdownTimeoutAsStopBudget(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	var declared app.Mod = mod
	budgeter, ok := declared.(interface{ StopBudget() time.Duration })
	if !ok {
		t.Fatal("dataengine mod does not declare a stop budget; App splits shutdown.total_timeout evenly instead")
	}
	if got := budgeter.StopBudget(); got != 30*time.Second {
		t.Fatalf("default stop budget=%v, want dataengine.shutdown_timeout default 30s", got)
	}
	cfg.Set("dataengine.shutdown_timeout", 45*time.Second)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got := budgeter.StopBudget(); got != 45*time.Second {
		t.Fatalf("configured stop budget=%v, want 45s", got)
	}
	// 未 Init 的 Mod 不声明（<= 0 由 App 视为未声明）。
	if got := NewMod().StopBudget(); got > 0 {
		t.Fatalf("uninitialized stop budget=%v, want none", got)
	}
}
