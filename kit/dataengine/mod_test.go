package dataengine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
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

func TestDataEngineModDefaultsToCanonicalWALWriterV2(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got := mod.cfg.wal.WriterVersion; got != 2 {
		t.Fatalf("writer version=%d, want 2", got)
	}

	cfg.Set("dataengine.wal.writer_version", 1)
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if got := mod.cfg.wal.WriterVersion; got != 1 {
		t.Fatalf("explicit compatibility writer version=%d, want 1", got)
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
	stream, maxAge, err := EffectStreamRetention(nil)
	if err != nil || stream != DefaultEffectStream || maxAge != DefaultEffectMaxAge {
		t.Fatalf("EffectStreamRetention(nil) = %q, %v, %v; want the declared defaults %q, %v", stream, maxAge, err, DefaultEffectStream, DefaultEffectMaxAge)
	}
}

func TestDataEngineModRecoversBeforeReadyAndOwnsNestOptions(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.wal.writer_version", 2)
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

// RR-20260926-31：生成工程在闲置交还租约前，按接口从 ModDataEngine 取得实体投影屏障；
// 未装配的 Mod 不能回答“已落库”。
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

// RR-20260926-30：DataEngine 驱逐被跳过的原生步骤留下的实体要在 Nest 快池持锁执行；Mod 作为 Nest 的
// committer 接收 RunLocal 并转交给启动后才创建的 Projector。Nest 绑定前（启动恢复阶段）就地执行。
// 重载回调在 Runtime 启动后才能注册。
func TestModForwardsNestLocalExecutorAndReloadHook(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.wal.writer_version", 2)
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
	if err := mod.runLocal(func() { inPlace = !fctx.InFastWorker() }); err != nil || !inPlace {
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
	if err := mod.runLocal(func() { onFast = fctx.InFastWorker() }); err != nil || !onFast {
		t.Fatalf("bound local executor=%v on fast worker=%v", err, onFast)
	}
}
