package remoteentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/viper"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	coreremote "github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	fctx "github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"github.com/tjbdwanghaibo/roost-core/infra/network/servicerpc"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 路由 RPC 的 wiring 一侧（联盟服务方案 §2.3、§2.7）。infra/network/servicerpc 定义查找接口与 CallRoutedChecked，
// 不能依赖 framework（根包边界测试），所以“Redis 登记 → 持久所有权记录”的查找实现与服务端前置检查放在这里，
// 与 RPCWriteAnswer 同处。

// routeResolver 把 coreremote.RouteLookup 接成 servicerpc.RouteResolver：没有 owner 时答 servicerpc.EntityNotFound。
type routeResolver struct {
	lookup *coreremote.RouteLookup
}

func (r routeResolver) LookupRoute(ctx context.Context, entityID int64) (int32, error) {
	owner, found, err := r.lookup.LookupOwner(ctx, entityID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, servicerpc.EntityNotFound(entityID)
	}
	return owner, nil
}

// newRouteResolver 用部署前缀（remote_entity.snapshot_l2_key_prefix）上的 Redis 登记与 owners 建查找能力。
func newRouteResolver(redis fredis.IRedis, prefix string, owners coreremote.OwnershipReader) (servicerpc.RouteResolver, error) {
	routes, err := coreremote.NewRouteStore(redis, prefix, 0)
	if err != nil {
		return nil, err
	}
	lookup, err := coreremote.NewRouteLookup(routes, owners)
	if err != nil {
		return nil, err
	}
	return routeResolver{lookup: lookup}, nil
}

// RouteSource 返回注册表里的路由查找能力（mods.ModRemoteRoute），生成的 ClientMod 把它交给
// (*servicerpc.BusClient).CallRoutedChecked。owner 进程由 RemoteEntityMod 提供（所有权记录经写 Manager 读），只读进程由
// RemoteMirrorMod 提供（所有权记录经只读的 Mongo 查询读）。在 Provide 之后（Mod 的 Start、服务的 Init）调用。
func RouteSource(r *app.Registry) (servicerpc.RouteResolver, error) {
	source, ok := app.Lookup[servicerpc.RouteResolver](r, mods.ModRemoteRoute)
	if !ok || source == nil {
		return nil, fmt.Errorf("remote route: capability %q not found: add wiring/remoteentity.NewRemoteEntityMod (owner process) or NewRemoteMirrorMod with its Mongo loader (read-only process)", mods.ModRemoteRoute)
	}
	return source, nil
}

// RoutedEntityServer 是服务端前置检查需要的框架一步（*coreremote.Manager 实现它）：确认 owner，本 sid 就冷加载并登记。
type RoutedEntityServer interface {
	ServeRoutedRequest(ctx context.Context, id int64) (owner int32, found bool, err error)
}

var _ RoutedEntityServer = (*coreremote.Manager)(nil)

// RPCRouteCheck 是路由 RPC 的服务端前置检查（联盟服务方案 §2.3），生成的 handler 在进 Nest 之前调用，签名与
// RPCWriteAnswer 同一风格（owners 是注册表里的 Remote Manager）。返回 nil 表示由本实例处理；否则把返回值作为答复
// 写进响应：code, reason := servicerpc.Error(answer)，响应的 RedirectSID 取 servicerpc.RedirectSID(answer)。
//
//   - 实体已在本地加载、所有权是本 sid：nil，不做 I/O。
//   - 否则读持久所有权记录：
//   - 没有记录：servicerpc.EntityNotFound（业务拒绝）。非创建类请求不得进入写准入，否则首写会把一个不存在的 ID 认领走；
//   - owner 是本 sid：冷加载、登记路由，nil；
//   - owner 是别的 sid：servicerpc.Redirect(owner)。
//   - 读不了所有权记录、加载失败、owners 还没装好：servicerpc.Retryable（没有执行）。
//
// 进 Nest 之后写准入照常按所有权记录 + fence 裁决（被 fence 时 RPCWriteAnswer 答改投），这一步只是省掉一次白进
// Nest。创建类方法（新实体走随机路由）不调它。要 I/O，只能在 I/O 段（RPC handler）调用。
func RPCRouteCheck(ctx context.Context, owners entity.RemoteOwnershipManager, localSID int32, id int64) error {
	if err := fctx.BlockingError("remoteentity.RPCRouteCheck"); err != nil {
		return err
	}
	if owners == nil {
		return servicerpc.Retryable(errors.New("remote_entity: ownership manager is not ready"))
	}
	var (
		owner int32
		found bool
		err   error
	)
	if server, ok := owners.(RoutedEntityServer); ok {
		owner, found, err = server.ServeRoutedRequest(ctx, id)
	} else {
		// 不是框架 Manager（替身或自建装配）：只读所有权记录，冷加载留给写准入。
		var lease entity.RemoteEntityMarkerLease
		lease, found, err = owners.GetRemoteOwnership(ctx, id)
		owner = lease.OwnerSid
	}
	switch {
	case errors.Is(err, entity.ErrRemoteKindNotOwned):
		// 这个 kind 归别的服务：调用方把请求发错了服务，不是可重试的状态。
		return err
	case err != nil:
		return servicerpc.Retryable(err)
	case !found:
		return servicerpc.EntityNotFound(id)
	case owner != localSID:
		return servicerpc.Redirect(owner, fmt.Errorf("remote_entity: entity %d is owned by sid %d", id, owner))
	default:
		return nil
	}
}

// RemotePreloadMod 是起服预加载（实体类型的显式策略 preload=startup，联盟服务方案 §2.7）：Start 时对
// WithPreloadKinds 声明的 kind 调 Manager.PreloadOwnedEntities——按 owner 枚举持久所有权记录，只加载 owner 是本 sid 的
// 独占实体并登记路由。
//
// 顺序：依赖 remote_entity 与 nest，可选依赖 dataengine（DataEngine 恢复完 WAL、Nest 启动之后才加载）；路由 RPC 的
// owner Mod 把 mods.ModRemoteEntityPreload 写进 OptionalDependsOn，于是预加载完成前它不在总线上注册 handler，调用方
// 得到 no responders（“可重试，未执行”），与恢复期间一样。枚举失败让启动失败（进程管理拉起重试）；单个实体加载失败
// 只记错误与指标，留给按需冷加载。
type RemotePreloadMod struct {
	registry *app.Registry
	// Report 是最近一次预加载的结果（诊断与测试用）。
	Report coreremote.PreloadReport
}

// NewRemotePreloadMod 返回预加载 Mod。只在 RemoteEntityMod 带了 WithPreloadKinds 时装；RemoteEntityMod 在配置检查时
// 拒绝“声明了预加载 kind 却没装这个 Mod”。
func NewRemotePreloadMod() *RemotePreloadMod { return &RemotePreloadMod{} }

func (m *RemotePreloadMod) Name() app.ModName { return mods.ModRemoteEntityPreload }

func (m *RemotePreloadMod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModRemoteEntity, mods.ModNest}
}

func (m *RemotePreloadMod) OptionalDependsOn() []app.ModName {
	return []app.ModName{mods.ModDataEngine}
}

// ConfigSchema：本 Mod 不读配置键（预加载 kind 来自实体注解，经 ModOption 传入）。
func (m *RemotePreloadMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(struct{}{}) }

func (m *RemotePreloadMod) Init(*viper.Viper) error { return nil }

func (m *RemotePreloadMod) Provide(r *app.Registry) error {
	m.registry = r
	return nil
}

// preloader 是 Start 需要的 Manager 能力。
type preloader interface {
	PreloadKinds() []entity.EntityKind
	PreloadOwnedEntities(ctx context.Context, kinds []entity.EntityKind) (coreremote.PreloadReport, error)
}

// Start 同步跑完预加载才返回：App 在它返回之后才启动依赖它的 owner Mod。
func (m *RemotePreloadMod) Start() error {
	if m == nil || m.registry == nil {
		return errors.New("remote_entity preload mod: not provided")
	}
	manager, ok := app.Lookup[entity.IRemoteEntityManager](m.registry, mods.ModRemoteEntity)
	if !ok || manager == nil {
		return fmt.Errorf("remote_entity preload mod: required capability %q not found", mods.ModRemoteEntity)
	}
	loader, ok := manager.(preloader)
	if !ok {
		return fmt.Errorf("remote_entity preload mod: %T cannot preload owned entities", manager)
	}
	kinds := loader.PreloadKinds()
	if len(kinds) == 0 {
		slog.Info("remote_entity preload mod: no preload kinds declared")
		return nil
	}
	report, err := loader.PreloadOwnedEntities(fctx.BaseContext(), kinds)
	m.Report = report
	if err != nil {
		return fmt.Errorf("remote_entity preload mod: %w", err)
	}
	slog.Info("remote_entity preload mod: preloaded owned entities", "kinds", kinds,
		"listed", report.Listed, "loaded", report.Loaded, "skipped", report.Skipped, "failed", report.Failed)
	return nil
}

func (m *RemotePreloadMod) Stop() {}

var _ app.Mod = (*RemotePreloadMod)(nil)
