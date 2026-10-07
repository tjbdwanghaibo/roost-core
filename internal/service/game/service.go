package Game

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	migrations "example.com/planet/db/migrations"
	world "example.com/planet/game/entities/world"
	lifecycle "example.com/planet/game/lifecycle"
	"example.com/planet/game/settings"
	"example.com/planet/game/skills"
	accessplayer "example.com/planet/internal/access/player"
	accessplayertcp "example.com/planet/internal/access/player/tcp"
	"github.com/tjbdwanghaibo/roost-core/app"
	coreentity "github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"github.com/tjbdwanghaibo/roost-core/skill"
)

// Service is the game server. It owns this process's World, the consumers
// that turn committed effects into work (level-ups into reward mail), the
// matchmaker that turns waiting tickets into matches, the gift saga's step
// consumers, and the lockstep rooms those matches play in.
type Service struct {
	world          *world.World
	levelUpMail    fnats.IJetStreamSubscription
	stopMatchmaker func()
	stopGiftSaga   func()
	battles        *battleManager
	scene          *Scene
	stopSpawner    func()
	spawner        *spawner
	owners         *PlayerOwners
	stopFlags      func()
	stopPresence   func()
	activity       *ActivityRunner
	stopActivity   func()
	activityPhase  fnats.IJetStreamSubscription
}

func New() *Service                    { return &Service{} }
func (*Service) Name() app.ServiceName { return app.ServiceName("game") }

// ConfigSchema declares the keys this service's own code reads (game/settings):
// the App checks them before any Mod starts, together with every Mod's, and
// `--print-config` / `roost project doctor` know about them. Without it the
// activity.* and platform.* sections in this service's config are keys no
// declaration owns (maintainer decision A4 ①).
func (*Service) ConfigSchema() app.ConfigSchema { return settings.Schema() }

// Init runs after every Mod has started, so the Entity runtime, JetStream,
// Mongo and the mail client are all up. The World — this process's one
// singleton Entity — is created or loaded here, before any request is
// served; then the effect consumer subscribes, so a level-up committed while
// this process was down is delivered as soon as it is back.
func (s *Service) Init(registry *app.Registry) error {
	// Migrations before anything can load a document: a Player written by an
	// older build is upgraded on its way into the Entity, and a step that
	// registered late would let the first few loads through unmigrated.
	if err := migrations.Register(); err != nil {
		return fmt.Errorf("game: %w", err)
	}
	// The skill catalog first: a definition that does not compile must stop
	// the process here, not surface as an error on the first client request.
	// Warnings are kept for the operator to see; a release check can refuse them.
	catalog, err := skills.CompileAll(skill.DefaultCompileEnvironment())
	if err != nil {
		return fmt.Errorf("game: skill catalog: %w", err)
	}
	warnings := 0
	for _, diagnostics := range catalog.Diagnostics {
		warnings += len(diagnostics)
	}
	slog.Info("skill catalog compiled", "skills", len(catalog.Programs), "warnings", warnings)
	value, err := lifecycle.EnsureWorld(context.Background(), registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.world = value
	// The map, before anything can put a player on it. A scene is built from
	// configuration on every start (noPersist), so this is a build, not a
	// load.
	area, err := lifecycle.EnsureWorldScene(context.Background(), registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	subscription, err := startLevelUpMailer(context.Background(), registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.levelUpMail = subscription
	// The battle manager before the matchmaker: a match that forms opens a
	// lockstep room through it, so it must be published and reachable first.
	battles, err := newBattleManager(registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	if err := registry.Register(BattleCapability, battles); err != nil {
		return fmt.Errorf("game: publish battle manager: %w", err)
	}
	s.battles = battles
	// The scene before anything that can make a player visible: EnterGame
	// joins it, and a join before Start would have no room to join.
	scene, err := NewScene(registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	if err := scene.Start(context.Background()); err != nil {
		return fmt.Errorf("game: %w", err)
	}
	if err := registry.Register(SceneCapability, scene); err != nil {
		_ = scene.Close(context.Background())
		return fmt.Errorf("game: publish scene: %w", err)
	}
	s.scene = scene
	// The spawner last among the scene's users: it needs both the entity and
	// the bridge, and it starts producing subjects immediately.
	stopSpawner, spawn, err := startSpawner(context.Background(), registry, area, scene)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.stopSpawner, s.spawner = stopSpawner, spawn
	// 本进程为哪些玩家服务（playerowner.go），在问它的消费者之前发布：matchmaker 和赠礼步骤在启动时
	// 各查一次这个能力，查不到就拒绝启动。玩家绑定在哪个 sid 是 account 写进角色的静态事实，同一 sid
	// 只有一个进程由 App 单实例锁保证，所以这里只是一张本地驻留表，不碰 Redis。下面装好卸载与连接之后
	// 才 Start。
	owners, err := NewPlayerOwners(registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	// 闲置卸载丢副本用的东西，在发布之前装上：丢一个玩家就是先让他离开场景、再移出实体管理器，所以
	// 场景在上面先建好。卸载只是本地内存管理，不交出任何东西；之后在本进程重新装载时，DataEngine 的
	// 冷加载会先等这个实体在途的投影。
	players, err := lifecycle.PlayerFromRegistry(registry)
	if err != nil {
		return fmt.Errorf("game: player lifecycle for eviction: %w", err)
	}
	access, ok := app.Lookup[*coreentity.ManagerAccess](registry, mods.ModEntityRuntime)
	if !ok || access == nil || access.Manager() == nil {
		return fmt.Errorf("game: entity runtime is unavailable, so an idle player could never be unloaded")
	}
	owners.Evict(playerEvictor{scene: scene, players: players, manager: access.Manager()})
	if err := registry.Register(PlayerOwnerCapability, owners); err != nil {
		return fmt.Errorf("game: publish player owners: %w", err)
	}
	// 同一张表，再以接入边界的可选闸门名发布：从这里起每个玩家请求在到达 handler 之前都先检查，没有
	// 在本进程接入服务（登录时校验 server_id）的玩家，所有端点一起拒绝。
	if err := registry.Register(accessplayer.WriteGateCapability, accessplayer.WriteGate(owners)); err != nil {
		return fmt.Errorf("game: publish player write gate: %w", err)
	}
	s.owners = owners
	stop, err := startMatchmaker(context.Background(), registry, battles, scene)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.stopMatchmaker = stop
	// The gift saga's step consumers (gift_saga.go): the saga Mod has
	// started, so the stream exists and the coordinator is running.
	stopGift, err := startGiftSaga(context.Background(), registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.stopGiftSaga = stopGift
	// The paid-order drain: the game side of the platform service's handover
	// (purchase_drain.go). Published rather than started — it runs when a
	// player is in hand, not on a timer.
	drain, err := NewPurchaseDrain(registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	if err := registry.Register(PurchaseCapability, drain); err != nil {
		return fmt.Errorf("game: publish purchase drain: %w", err)
	}
	// The kill switches, before anything they gate: a process that comes up
	// during an incident must come up with the store closed if that is how
	// the operator left it (flags.go).
	stopFlags, err := installFeatureFlags(registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.stopFlags = stopFlags
	// What may be hot-patched, registered before anything can be patched.
	if err := installPatchPoints(); err != nil {
		return fmt.Errorf("game: %w", err)
	}
	// Chat presence follows the same session-close source the scene does:
	// without it an offline player stays in the world channel's recipient set
	// until some later push to them happens to fail (presence.go).
	stopPresence, err := watchPresence(registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.stopPresence = stopPresence
	// 传输层此时已经起来：卸载要用它判断玩家还有没有连接，停机要用它断开服务中的玩家。没有它就不卸载。
	if runtime, ok := app.Lookup[*accessplayertcp.Runtime](registry, accessplayertcp.Name); ok && runtime != nil {
		owners.Fence(runtime)
	}
	owners.Start(context.Background())
	// The timed server-wide event: the World's timer heap and the coordinator
	// loop (activity.go). After the scene, because
	// the settlement mails players who are already in the world, and before
	// the GM commands, which can close a window by hand.
	runner, stopActivity, err := startActivity(context.Background(), registry)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.activity, s.stopActivity = runner, stopActivity
	if err := registry.Register(ActivityCapability, runner); err != nil {
		return fmt.Errorf("game: publish activity runner: %w", err)
	}
	phase, err := startActivityPhaseConsumer(context.Background(), registry, runner)
	if err != nil {
		return fmt.Errorf("game: %w", err)
	}
	s.activityPhase = phase
	// The operator surface: GM commands on the admin registry the ops Mod
	// serves (gm.go). Registered last, when everything they call is up.
	if err := registerGMCommands(registry, s.spawner, s.activity); err != nil {
		return fmt.Errorf("game: %w", err)
	}
	return nil
}

// World is this process's World. It is set in Init and never nil afterwards.
func (s *Service) World() *world.World { return s.world }

func (*Service) Serve(ctx context.Context) error { <-ctx.Done(); return nil }

// Shutdown drains the consumer: in-flight effects finish or are redelivered
// to the next process holding the durable; nothing is lost either way.
//
// ctx is the App's stop context, bounded by shutdown.total_timeout; the scene
// closes under it (RR-20260930-18), so stopping the reload of unloaded
// players and closing the replication manager cannot outlive the deployment's
// grace period. It used to close under context.Background().
//
// 第一件事是断开本进程服务中的全部玩家连接（RR-20260930-23 在 App 单实例锁之下的形态）：停机可能
// 是 fail-stop（失锁、DataEngine / Remote fatal，此时 Nest 已被围栏），进程不再处理请求，连接不能
// 留在这里，客户端重连到接替的进程。会话关闭事件照常走到场景、在线状态，此时它们都还开着。
func (s *Service) Shutdown(ctx context.Context) error {
	if s.owners != nil {
		if closed := s.owners.CloseServedSessions(errServiceStopping); closed > 0 {
			slog.Info("game: disconnected the players this process served", "sessions_closed", closed)
		}
	}
	if s.stopMatchmaker != nil {
		s.stopMatchmaker()
		s.stopMatchmaker = nil
	}
	if s.stopGiftSaga != nil {
		s.stopGiftSaga()
		s.stopGiftSaga = nil
	}
	// After the matchmaker, so no new battle is opened while rooms close.
	if s.battles != nil {
		s.battles.Close()
		s.battles = nil
	}
	if s.levelUpMail != nil {
		s.levelUpMail.Drain()
		s.levelUpMail = nil
	}
	if s.stopPresence != nil {
		s.stopPresence()
		s.stopPresence = nil
	}
	if s.stopFlags != nil {
		s.stopFlags()
		s.stopFlags = nil
	}
	if s.owners != nil {
		s.owners.Stop()
		s.owners = nil
	}
	// The activity loops before the consumer that feeds them, so nothing is
	// still opening a window while the phase subscription drains.
	if s.stopActivity != nil {
		s.stopActivity()
		s.stopActivity = nil
	}
	if s.activityPhase != nil {
		s.activityPhase.Drain()
		s.activityPhase = nil
	}
	// The spawner before the scene: it takes its population down, and the
	// room has to still be there to hear about it.
	if s.stopSpawner != nil {
		s.stopSpawner()
		s.stopSpawner = nil
	}
	// The scene last among the fan-out paths: closing it stops the
	// replication loop and the transport sink, and anything still preparing
	// a frame finishes first.
	if s.scene != nil {
		if err := s.scene.Close(ctx); err != nil {
			slog.Warn("scene did not close cleanly", "err", err)
		}
		s.scene = nil
	}
	return nil
}

// errServiceStopping 是停机时断开玩家连接的原因。
var errServiceStopping = errors.New("game: this process is stopping; reconnect")

var _ app.Service = (*Service)(nil)
var _ app.ModConfigSchema = (*Service)(nil)
