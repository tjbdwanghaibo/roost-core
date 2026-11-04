// 路由 RPC 的 wiring 一侧（联盟服务方案 §2.3、§2.7）：服务端前置检查的答复、路由查找能力的登记、登记 / 预加载 kind 的
// ModOption 校验与预加载 Mod 的启动顺序。

package remoteentity

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	coreremote "github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
	"github.com/tjbdwanghaibo/roost-core/infra/network/servicerpc"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// routedServer 是 ServeRoutedRequest 的替身。
type routedServer struct {
	entity.RemoteOwnershipManager
	owner int32
	found bool
	err   error
	calls int
}

func (s *routedServer) ServeRoutedRequest(context.Context, int64) (int32, bool, error) {
	s.calls++
	return s.owner, s.found, s.err
}

func TestRPCRouteCheckAnswersFromTheOwnershipRecord(t *testing.T) {
	const local int32 = 3000
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		server  *routedServer
		outcome servicerpc.Outcome
		code    int32
	}{
		{"served here", &routedServer{owner: local, found: true}, servicerpc.OutcomeOK, servicerpc.CodeOK},
		{"no ownership record", &routedServer{}, servicerpc.OutcomeRejected, errcode.CodeEntityNotFound},
		{"owned elsewhere", &routedServer{owner: 3001, found: true}, servicerpc.OutcomeRetryable, servicerpc.CodeRedirect},
		{"authority unreachable", &routedServer{err: errors.New("mongo down")}, servicerpc.OutcomeRetryable, servicerpc.CodeRetryable},
	} {
		answer := RPCRouteCheck(ctx, tc.server, local, 1)
		if servicerpc.Classify(answer) != tc.outcome || servicerpc.Code(answer) != tc.code || tc.server.calls != 1 {
			t.Errorf("%s: answer=%v outcome=%s code=%d calls=%d, want %s / %d", tc.name, answer, servicerpc.Classify(answer), servicerpc.Code(answer), tc.server.calls, tc.outcome, tc.code)
		}
	}
	if owner, ok := servicerpc.RedirectSID(RPCRouteCheck(ctx, &routedServer{owner: 3001, found: true}, local, 1)); !ok || owner != 3001 {
		t.Errorf("redirect owner = %d,%v; want 3001", owner, ok)
	}
	if answer := RPCRouteCheck(ctx, &routedServer{err: entity.ErrRemoteKindNotOwned}, local, 1); !errors.Is(answer, entity.ErrRemoteKindNotOwned) || servicerpc.Classify(answer) == servicerpc.OutcomeRetryable {
		t.Errorf("foreign kind answer=%v outcome=%s; a request sent to the wrong service is not retryable", answer, servicerpc.Classify(answer))
	}
	if answer := RPCRouteCheck(ctx, nil, local, 1); servicerpc.Classify(answer) != servicerpc.OutcomeRetryable {
		t.Errorf("no ownership manager yet: answer=%v, want retryable", answer)
	}
	// 不是框架 Manager 时只读所有权记录（冷加载留给写准入）。
	plain := &ownershipLookup{}
	if answer := RPCRouteCheck(ctx, plain, local, 1); servicerpc.Code(answer) != errcode.CodeEntityNotFound || plain.reads != 1 {
		t.Errorf("plain ownership manager, no record: answer=%v reads=%d", answer, plain.reads)
	}
}

// fakeRemoteManager 是带预加载能力的 Remote Manager 替身：PreloadOwnedEntities 阻塞到 release 关闭。
type fakeRemoteManager struct {
	entity.IRemoteEntityManager
	kinds    []entity.EntityKind
	release  chan struct{}
	finished atomic.Bool
	err      error
}

func (m *fakeRemoteManager) PreloadKinds() []entity.EntityKind { return m.kinds }
func (m *fakeRemoteManager) PreloadOwnedEntities(ctx context.Context, kinds []entity.EntityKind) (coreremote.PreloadReport, error) {
	<-m.release
	m.finished.Store(true)
	return coreremote.PreloadReport{Listed: 2, Loaded: 2}, m.err
}

// 预加载完成前不接请求：Start 跑完整个预加载才返回（App 在它返回之后才启动依赖它的 owner Mod），枚举失败让启动失败；
// 顺序上排在 remote_entity、nest 之后，可选地排在 dataengine 之后。
func TestRemotePreloadModFinishesBeforeStartReturns(t *testing.T) {
	manager := &fakeRemoteManager{kinds: []entity.EntityKind{1}, release: make(chan struct{})}
	r := app.NewRegistry(viper.New())
	if err := r.Register(mods.ModRemoteEntity, entity.IRemoteEntityManager(manager)); err != nil {
		t.Fatal(err)
	}
	mod := NewRemotePreloadMod()
	if err := mod.Provide(r); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- mod.Start() }()
	select {
	case err := <-started:
		t.Fatalf("Start returned (%v) before the preload finished; the owner Mod would register its handlers too early", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(manager.release)
	if err := <-started; err != nil || !manager.finished.Load() || mod.Report.Loaded != 2 {
		t.Fatalf("Start = %v finished=%v report=%+v", err, manager.finished.Load(), mod.Report)
	}
	if !slices.Equal(mod.DependsOn(), []app.ModName{mods.ModRemoteEntity, mods.ModNest}) || !slices.Contains(mod.OptionalDependsOn(), mods.ModDataEngine) {
		t.Fatalf("DependsOn=%v OptionalDependsOn=%v", mod.DependsOn(), mod.OptionalDependsOn())
	}

	failing := &fakeRemoteManager{kinds: []entity.EntityKind{1}, release: make(chan struct{}), err: errors.New("enumerate: mongo down")}
	close(failing.release)
	r = app.NewRegistry(viper.New())
	_ = r.Register(mods.ModRemoteEntity, entity.IRemoteEntityManager(failing))
	mod = NewRemotePreloadMod()
	_ = mod.Provide(r)
	if err := mod.Start(); err == nil {
		t.Fatal("a failed enumeration must fail the start")
	}
}

// 登记 / 预加载 kind 在 Init 校验一次：必须是已注册的 Remote 托管 kind、不能同时声明为别的服务的；声明了预加载就要装
// 预加载 Mod（配置检查时报）。
func TestRouteAndPreloadKindsAreValidatedOnce(t *testing.T) {
	entity.MustRegisterEntityKindDefs(entity.EntityKindDef{Kind: interestHealthKind, Category: 1, RemotePolicy: entity.RemotePolicyManaged})
	cfg := viper.New()
	cfg.Set("sid", 3000)
	for _, tc := range []struct {
		name string
		opts []ModOption
		want string
	}{
		{"unregistered route kind", []ModOption{WithRouteKinds(250)}, "route kind 250 is not a registered Remote-managed"},
		{"unregistered preload kind", []ModOption{WithPreloadKinds(250)}, "preload kind 250 is not a registered Remote-managed"},
		{"route kind owned elsewhere", []ModOption{WithForeignKinds(interestHealthKind), WithRouteKinds(interestHealthKind)}, "also declared foreign"},
	} {
		if err := NewRemoteEntityMod(0, tc.opts...).Init(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Init = %v, want %q", tc.name, err, tc.want)
		}
	}
	mod := NewRemoteEntityMod(0, WithRouteKinds(interestHealthKind, interestHealthKind), WithPreloadKinds(interestHealthKind))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mod.cfg.RouteKinds, []entity.EntityKind{interestHealthKind}) || !slices.Equal(mod.cfg.PreloadKinds, []entity.EntityKind{interestHealthKind}) {
		t.Fatalf("RouteKinds=%v PreloadKinds=%v", mod.cfg.RouteKinds, mod.cfg.PreloadKinds)
	}
	none := func(app.ModName) bool { return false }
	if err := mod.CheckConfigAcrossMods(cfg, none); err == nil || !strings.Contains(err.Error(), "NewRemotePreloadMod") {
		t.Fatalf("preload kinds without the preload Mod: %v", err)
	}
	if err := mod.CheckConfigAcrossMods(cfg, func(name app.ModName) bool { return name == mods.ModRemoteEntityPreload }); err != nil {
		t.Fatal(err)
	}
	if err := NewRemoteEntityMod(0).CheckConfigAcrossMods(cfg, none); err != nil {
		t.Fatalf("a service without preload kinds needs no preload Mod: %v", err)
	}
}

// owner 进程（RemoteEntityMod）与装了 mongo 的只读进程（RemoteMirrorMod）都登记路由查找能力；只读进程换掉 Mongo
// loader 又没装 mongo 时没有这个能力，RouteSource 说清缺什么。
func TestRouteSourceIsProvidedByBothRemoteMods(t *testing.T) {
	cfg := viper.New()
	cfg.Set("sid", 1101)
	r := app.NewRegistry(cfg)
	if err := r.Register(mods.ModRedis, fredis.IRedis(&tokenRecordingRedis{})); err != nil {
		t.Fatal(err)
	}
	backend, err := coreremote.NewBackend(entity.NewManagerAccess(entity.NewEntityManager()), coreremote.NewMongoCommitter(unusedMongo{}, "remote_entity", 1101, 0))
	if err != nil {
		t.Fatal(err)
	}
	owner := NewRemoteEntityMod(0, WithBackend(backend))
	if err := owner.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := owner.Provide(r); err != nil {
		t.Fatal(err)
	}
	if _, err := RouteSource(r); err != nil {
		t.Fatalf("owner process: %v", err)
	}

	mirrorCfg := viper.New()
	mod := NewRemoteMirrorMod(4102)
	if err := mod.Init(mirrorCfg); err != nil {
		t.Fatal(err)
	}
	mr := mirrorRegistry(t, mirrorCfg)
	if err := mr.Register(mods.ModMongo, fmongo.IMongo(unusedMongo{})); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(mr); err != nil {
		t.Fatal(err)
	}
	if _, err := RouteSource(mr); err != nil {
		t.Fatalf("read-only process with mongo: %v", err)
	}
	if !slices.Contains(mod.OptionalDependsOn(), mods.ModMongo) {
		t.Fatalf("OptionalDependsOn=%v; the mirror must provide after mongo", mod.OptionalDependsOn())
	}

	noMongo := NewRemoteMirrorMod(4103, WithMirrorLoader(func(context.Context, entity.RemoteSnapshotKey, entity.RemoteReadConsistency, uint64) (entity.RemoteSnapshotEnvelope, bool, error) {
		return entity.RemoteSnapshotEnvelope{}, false, nil
	}, false))
	if err := noMongo.Init(mirrorCfg); err != nil {
		t.Fatal(err)
	}
	nr := mirrorRegistry(t, mirrorCfg)
	if err := noMongo.Provide(nr); err != nil {
		t.Fatal(err)
	}
	if _, err := RouteSource(nr); err == nil || !strings.Contains(err.Error(), string(mods.ModRemoteRoute)) {
		t.Fatalf("read-only process without mongo: %v", err)
	}
}
