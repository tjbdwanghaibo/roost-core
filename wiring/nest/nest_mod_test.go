package nest

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

type emptyGetter struct{}

type dataEngineNestProvider struct {
	committer corenest.TransactionCommitter
}

func (provider dataEngineNestProvider) NestOptions() []corenest.NestOption {
	return []corenest.NestOption{corenest.NestOptionWithTransactionCommitter(provider.committer)}
}

type noOpCommitter struct{}

func (noOpCommitter) Commit(context.Context, corenest.CommitRecord) error { return nil }

func (emptyGetter) Get(context.Context, int64, entity.EntityCategory) (entity.IThreadSafeEntity, error) {
	return nil, corenest.ErrEntityNotFound
}
func (emptyGetter) GetMany(context.Context, []int64, []entity.EntityCategory) ([]entity.IThreadSafeEntity, error) {
	return nil, corenest.ErrEntityNotFound
}

func TestModProvidesInstanceClientAndHealth(t *testing.T) {
	cfg := viper.New()
	cfg.Set("nest.short.queue_capacity", 8)
	cfg.Set("nest.short.workers", 2)
	cfg.Set("nest.io.workers", 4)
	cfg.Set("nest.io.queue_capacity", 7)
	cfg.Set("nest.long.workers", 3)
	cfg.Set("nest.long.queue_capacity", 9)
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(emptyGetter{})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	client, ok := app.Lookup[corenest.Client](registry, mods.ModNest)
	if !ok || client == nil {
		t.Fatal("nest.Client not registered")
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	if got := mod.Engine().Stats().IO.WorkerNum; got != 4 {
		t.Fatalf("remote workers = %d", got)
	}
	if stats := mod.Engine().Stats(); stats.Short.WorkerNum != 2 || stats.IO.QueueCap != 7 || stats.Short.QueueCap != 8 || stats.Long.WorkerNum != 3 || stats.Long.QueueCap != 9 {
		t.Fatalf("pool config=%+v", stats)
	}
	if !mod.Engine().Running() {
		t.Fatal("engine not running")
	}
	healthRegistry := app.MustLookup[*health.Registry](registry, mods.ModHealth)
	snapshot := healthRegistry.Snapshot(context.Background())
	if !snapshot.OK {
		t.Fatalf("health=%+v", snapshot)
	}
	if err := mod.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mod.Engine().Running() {
		t.Fatal("engine still running")
	}
}

func TestModSelectsDataEngineCommitterWithoutLegacyWALRuntime(t *testing.T) {
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(emptyGetter{})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if mod.Engine() == nil {
		t.Fatal("dataengine-backed Nest engine was not constructed")
	}
}

// App 单实例锁方案 §5：任何 fail-stop（失锁、DataEngine fatal、Remote fatal）都经
// RuntimeFailure.OnFail 立即围栏 Nest，拒绝新的派发；模块自己不再各写一遍“找 Nest、围栏”。
func TestRuntimeFailureFencesNestDispatch(t *testing.T) {
	cfg := viper.New()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(emptyGetter{})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mod.StopWithContext(context.Background()) })

	cause := errors.New("remote_entity fatal release failure")
	app.MustLookup[*app.RuntimeFailure](registry, mods.ModRuntimeFailure).Fail(cause)

	if err := mod.Engine().FenceError(); !errors.Is(err, corenest.ErrNestFenced) || !errors.Is(err, cause) {
		t.Fatalf("FenceError after RuntimeFailure = %v, want ErrNestFenced wrapping the cause", err)
	}
	id := int64(((uint64(999) & entity.UniqueIDMask) << entity.UniqueIDShift) | (uint64(1) & entity.EntityCategoryMask))
	if err := mod.Engine().Dispatch(context.Background(), corenest.NewHandlerName("fenced"), id, nil); !errors.Is(err, corenest.ErrNestFenced) {
		t.Fatalf("dispatch after RuntimeFailure = %v, want ErrNestFenced", err)
	}
}

type emptyOptionsProvider struct{}

func (emptyOptionsProvider) NestOptions() []corenest.NestOption { return nil }

// 无实体获取器的 Mod 不能 Init / Provide；Provide 拒绝 nil 注册表、缺数据引擎能力、数据引擎未初始化（无选项）；未 Provide 就 Start 拒绝。
func TestModRefusesMissingGetterRegistryAndDataEngine(t *testing.T) {
	cfg := viper.New()
	var none *Mod
	if err := none.Init(cfg); err != corenest.ErrGetterNotSet {
		t.Fatalf("Init on a nil mod = %v", err)
	}
	if err := none.Provide(app.NewRegistry(cfg)); err != corenest.ErrGetterNotSet {
		t.Fatalf("Provide on a nil mod = %v", err)
	}
	mod := NewMod(emptyGetter{})
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(nil); err == nil || !strings.Contains(err.Error(), "nil app registry") {
		t.Fatalf("Provide(nil) = %v", err)
	}
	if err := mod.Provide(app.NewRegistry(cfg)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Provide without the data engine capability = %v", err)
	}
	uninitialised := app.NewRegistry(cfg)
	if err := uninitialised.Register(mods.ModDataEngine, emptyOptionsProvider{}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(uninitialised); err == nil || !strings.Contains(err.Error(), "is not initialized") {
		t.Fatalf("Provide with an uninitialised data engine = %v", err)
	}
	if err := mod.Start(); err == nil || !strings.Contains(err.Error(), "engine not provided") {
		t.Fatalf("Start before Provide = %v", err)
	}
}

type countingCommitter struct{ effects int }

func (c *countingCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	c.effects += len(record.Effects)
	return nil
}

// saga.max_payload_bytes 是发起方与协调器共用的配置键：发起方进程没有这个类型的协调器时，EmitStart 也在 Nest 事务里按配置值拒绝
// 超限的意图，事务不提交（不能只按线上硬上限 4 MiB 校验、让协调器事后拒绝）。
func TestEmitStartRefusesOverTheSharedLimitWithoutALocalCoordinator(t *testing.T) {
	const limit = 1024
	cfg := viper.New()
	cfg.Set("saga.max_payload_bytes", limit)
	if err := NewMod(emptyGetter{}).Init(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coresaga.SetStartDataLimit(0) })
	start := func(key string, size int) (*countingCommitter, error) {
		committer := &countingCommitter{}
		_, err := corenest.RunIsolatedTransaction(context.Background(), committer, "start", func() (any, error) {
			return nil, coresaga.EmitStart(coresaga.StartRequest{Type: "coordinated-elsewhere", DefinitionVersion: 1, BusinessKey: key, Data: make([]byte, size)})
		})
		return committer, err
	}
	if committer, err := start("at-limit", limit); err != nil || committer.effects != 1 {
		t.Fatalf("start intent at the shared limit: err=%v effects=%d, want committed", err, committer.effects)
	}
	committer, err := start("over-limit", limit+1)
	if !errors.Is(err, coresaga.ErrInvalidRecord) || committer.effects != 0 {
		t.Fatalf("start intent over saga.max_payload_bytes with the coordinator in another process: err=%v effects=%d, want ErrInvalidRecord inside the Nest transaction and nothing committed", err, committer.effects)
	}
}

type healthSyncTransport struct{}

func (healthSyncTransport) Push(context.Context, entitysync.SessionID, []byte) error { return nil }
func TestProvidedEntitySyncHealthIsRegistered(t *testing.T) {
	cfg := viper.New()
	r := app.NewRegistry(cfg)
	if err := r.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}}); err != nil {
		t.Fatal(err)
	}
	manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: healthSyncTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	m := NewMod(emptyGetter{})
	m.entitySync = manager
	if err = m.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err = m.Provide(r); err != nil {
		t.Fatal(err)
	}
	if err = manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := app.MustLookup[*health.Registry](r, mods.ModHealth)
	for _, result := range h.Snapshot(context.Background()).Results {
		if result.Name == "entitysync" {
			if result.Status != health.StatusFail {
				t.Fatalf("closed sync: %+v", result)
			}
			return
		}
	}
	t.Fatal("entitysync health is absent from production registry")
}

// fakeDataEngineMod 只把 Nest 需要的持久化选项放进注册表，让真实的 Nest Mod 能在 App 里起来。
type fakeDataEngineMod struct{}

func (fakeDataEngineMod) Name() app.ModName       { return mods.ModDataEngine }
func (fakeDataEngineMod) Init(*viper.Viper) error { return nil }
func (fakeDataEngineMod) Provide(registry *app.Registry) error {
	return registry.Register(mods.ModDataEngine, dataEngineNestProvider{committer: noOpCommitter{}})
}
func (fakeDataEngineMod) Start() error { return nil }
func (fakeDataEngineMod) Stop()        {}

// frameProbeService 等 Nest 跑过若干帧之后，在 Nest 请求上下文之外写一行日志，然后结束 Serve 让 App 退出。
type frameProbeService struct{ wait time.Duration }

func (s *frameProbeService) Name() app.ServiceName { return "game" }
func (s *frameProbeService) Init(*app.Registry) error {
	return nil
}
func (s *frameProbeService) Serve(ctx context.Context) error {
	select {
	case <-time.After(s.wait):
	case <-ctx.Done():
		return ctx.Err()
	}
	slog.Info("frame probe")
	return errFrameProbeDone
}
func (s *frameProbeService) Shutdown(context.Context) error { return nil }

var errFrameProbeDone = errors.New("frame probe done")

// 不在 Nest 请求上下文里的日志行（请求内的帧号由 fctx 带）取运行中 Nest 的帧号：Nest 跑过若干帧之后它大于 0。
func TestLogLinesOutsideARequestCarryTheRunningNestFrame(t *testing.T) {
	dir := t.TempDir()
	a := app.New("roost-test", "0.0.0")
	a.Mods(fakeDataEngineMod{}, NewMod(emptyGetter{}))
	a.RegisterServer("game", &frameProbeService{wait: 200 * time.Millisecond})
	config := "log:\n  file: true\n  stdout: false\n  dir: " + strconv.Quote(dir) + "\n  rotate_interval: 0\nnest:\n  tick_duration: 10ms\n"
	path := filepath.Join(dir, "config.game.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	a.RootCmd().SetArgs([]string{"game", "--config", path})

	if err := a.Execute(); !errors.Is(err, errFrameProbeDone) {
		t.Fatalf("Execute err = %v, want the probe's own error", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "game-1000.log"))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "frame probe") {
			continue
		}
		match := regexp.MustCompile(`\bframe=(\d+)`).FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("the probe line has no frame field: %s", line)
		}
		frame, _ := strconv.ParseUint(match[1], 10, 64)
		if frame == 0 {
			t.Fatalf("the probe line was written after ~20 Nest frames but carries frame=0: %s", line)
		}
		return
	}
	t.Fatalf("no probe line in the file log:\n%s", raw)
}
