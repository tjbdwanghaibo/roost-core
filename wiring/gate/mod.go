package gate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/admin"
	"github.com/tjbdwanghaibo/roost-core/service/account"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

const GateName app.ModName = "access.gate"
const GameIngressName app.ModName = "access.game_ingress"
const PlayerRuntimeName app.ModName = "access.player.runtime"

// Options 显式接入现有 App/业务能力；未提供鉴权时采用正式 Accounts。
// Ready 必须快速读已维护的准入状态，不能每包执行 I/O 或拿 Entity 锁。
type Options struct {
	// RegisterDiscovery 在接入启动后发布候选，停止排空后注销。
	// 搭配 etcd.WithoutServiceRegistration，避免两个 Mod 抢同一个登记。
	RegisterDiscovery bool
	AdvertiseAddr     string
	Config            gateway.Config
	TCP               gateway.TCPConfig
	Authenticator     gateway.Authenticator
	ResolveGame       func(context.Context, int32) (gateway.ProcessIdentity, error)
	Ready             func() bool
	OnActive          func(gateway.Binding, uint64) error
	OnClosed          func(gateway.Binding, uint64)
	// OnActivated 只对 Game Ingress 有效：OnActive 成功之后的通知，带上 Game 在绑定时自己认证得到的
	// Principal（含 Claims）的副本。没有返回值，不能拒绝绑定（gateway.GameDependencies.OnActivated）。
	OnActivated func(gateway.Binding, uint64, gateway.Principal)
	DependsOn   []app.ModName
	// GateService / GameService 是 Gate 进程与它所接的玩家服务进程的实际服务名（app.RegisterServer 的名字）。
	// App 按实际服务名持单实例锁 <key_prefix>:<服务名>:<sid>，发现候选也按它登记与查找；协议角色 gate / game
	// 只出现在 NATS 主题与收件箱里，与服务名无关。Gate 与 Game Ingress 两端必须写同一对名字；空串取 "gate" / "game"。
	GateService string
	GameService string
}

func DefaultOptions() Options {
	config := gateway.DefaultConfig()
	tcp := gateway.DefaultTCPConfig()
	tcp.MaxPayloadBytes = uint32(config.MaxPayloadBytes)
	return Options{Config: config, TCP: tcp}
}

// serviceNames 返回两端的实际服务名（缺省 gate / game）。
func (options Options) serviceNames() (gate, game string, err error) {
	gate, game = strings.TrimSpace(options.GateService), strings.TrimSpace(options.GameService)
	if gate == "" {
		gate = "gate"
	}
	if game == "" {
		game = "game"
	}
	for _, name := range []string{gate, game} {
		if strings.ContainsAny(name, ": \t\r\n") {
			return "", "", fmt.Errorf("gate wiring: service name %q must not contain ':' or spaces (it is a segment of the singleton lock key)", name)
		}
	}
	if gate == game {
		return "", "", fmt.Errorf("gate wiring: GateService and GameService are both %q; the Gate and the player service are separate services", gate)
	}
	return gate, game, nil
}

type connections struct {
	identity gateway.ProcessIdentity
	// gateService / gameService 是协议角色 gate / game 对应的实际服务名：单实例身份核对与发现都用它们。
	gateService, gameService string
	client                   fnats.RawClient
	checker                  app.SingletonIdentityChecker
	ready                    func() bool
	auth                     gateway.Authenticator
	discovery                etcd.IDiscovery
	registered               bool
}

func connect(registry *app.Registry, options Options, role string) (connections, error) {
	var result connections
	var err error
	if result.gateService, result.gameService, err = options.serviceNames(); err != nil {
		return result, err
	}
	identity, ok := app.Lookup[app.SingletonIncarnation](registry, app.ModSingletonIncarnation)
	if !ok {
		return result, errors.New("gate wiring: singleton must be enabled")
	}
	result.identity = gateway.ProcessIdentity{ServerID: identity.Sid, Incarnation: identity.Token}
	// 本端的锁键 <key_prefix>:<服务名>:<sid> 必须就是 Options 写的服务名：对端按这个名字核对本端身份，
	// 名字不符时启动后的每次握手 / 绑定都会被拒，所以在这里拒绝启动（Key 为空只出现在不经 App 的测试接线里）。
	if own := result.service(role); identity.Key != "" && !strings.HasSuffix(identity.Key, ":"+own+":"+strconv.Itoa(int(identity.Sid))) {
		return result, fmt.Errorf("gate wiring: this process holds singleton lock %q but is wired as %s service %q; set Options.GateService / GameService to the actual service names", identity.Key, role, own)
	}
	if options.RegisterDiscovery {
		result.discovery, ok = app.Lookup[etcd.IDiscovery](registry, mods.ModEtcdDiscov)
		if !ok {
			return result, errors.New("gate wiring: discovery registration capability missing")
		}
	}
	result.checker, ok = app.Lookup[app.SingletonIdentityChecker](registry, app.ModSingletonIdentityChecker)
	if !ok {
		return result, errors.New("gate wiring: singleton identity checker missing")
	}
	raw, ok := app.Lookup[fnats.RawClient](registry, mods.ModNatsRaw)
	if !ok {
		return result, errors.New("gate wiring: raw NATS missing")
	}
	factory, ok := raw.(fnats.InboxClientFactory)
	if !ok {
		return result, errors.New("gate wiring: raw NATS cannot scope inbox")
	}
	prefix, err := gateway.InboxPrefix(options.Config.Namespace, role, result.identity)
	if err != nil {
		return result, err
	}
	result.client, err = factory.ForInbox(prefix)
	if err != nil {
		return result, err
	}
	failure, ok := app.Lookup[*app.RuntimeFailure](registry, app.ModRuntimeFailure)
	if !ok {
		return result, errors.New("gate wiring: runtime failure capability missing")
	}
	// 失锁 fail-stop 的入口先 Fence，新准入立即观察状态；后续 graceful Stop 保留依赖。
	var healthy atomic.Bool
	healthy.Store(true)
	failure.OnFail(func(error) { healthy.Store(false) })
	if options.Ready == nil {
		return result, errors.New("gate wiring: explicit business readiness required")
	}
	result.ready = func() bool { return healthy.Load() && options.Ready() }
	result.auth = options.Authenticator
	if result.auth == nil {
		accounts, ok := app.Lookup[account.Accounts](registry, account.CapabilityName)
		if !ok {
			return result, errors.New("gate wiring: Accounts missing")
		}
		result.auth = AccountAuthenticator{Accounts: accounts}
	}
	return result, nil
}

// matches 把 gateway 按协议角色发起的身份核对换成实际服务名（App 的锁键用服务名），incarnation 照常核对。
func (deps connections) matches(ctx context.Context, role string, identity gateway.ProcessIdentity) (bool, error) {
	return deps.checker.Matches(ctx, deps.service(role), identity.ServerID, identity.Incarnation)
}

// service 是协议角色对应的实际服务名；gate / game 以外的角色（system 广播发起方）原样当服务名。
func (deps connections) service(role string) string {
	switch role {
	case "gate":
		return deps.gateService
	case "game":
		return deps.gameService
	}
	return role
}
func dependencies(options Options) []app.ModName {
	names := append([]app.ModName{mods.ModNats}, options.DependsOn...)
	if options.RegisterDiscovery {
		names = append(names, mods.ModEtcd)
	}
	if options.Authenticator == nil {
		names = append(names, account.CapabilityName)
	}
	return names
}

type GateMod struct {
	options     Options
	runtime     *gateway.Gate
	server      *gateway.TCPServer
	connections connections
}

func NewGateMod(options Options) *GateMod     { return &GateMod{options: options} }
func (*GateMod) Name() app.ModName            { return GateName }
func (mod *GateMod) DependsOn() []app.ModName { return dependencies(mod.options) }
func (mod *GateMod) Init(*viper.Viper) error {
	if err := mod.options.Config.Validate(); err != nil {
		return err
	}
	if !mod.options.TCP.Enabled {
		return errors.New("gate wiring: Gate TCP must be enabled")
	}
	if int(mod.options.TCP.MaxPayloadBytes) != mod.options.Config.MaxPayloadBytes {
		return errors.New("gate wiring: TCP and internal payload limits must match explicitly")
	}
	return gateway.ValidateTCPConfig(mod.options.TCP)
}
func (mod *GateMod) Provide(registry *app.Registry) error {
	deps, err := connect(registry, mod.options, "gate")
	if err != nil {
		return err
	}
	if mod.options.ResolveGame == nil {
		if deps.discovery == nil {
			return errors.New("gate wiring: fixed-SID resolver missing")
		}
		mod.options.ResolveGame = FixedGameResolver(deps.discovery, deps.gameService)
	}
	mod.connections = deps
	runtime, err := gateway.NewGate(mod.options.Config, deps.identity, gateway.GateDependencies{Client: deps.client, QueueFactory: AsyncQueueFactory, Ready: deps.ready, ResolveGame: mod.options.ResolveGame, Matches: deps.matches})
	if err != nil {
		return err
	}
	mod.runtime = runtime
	server, err := gateway.NewTCPServer(mod.options.TCP, runtime.Forward, deps.auth)
	if err != nil {
		return err
	}
	mod.server = server
	if err = server.ConnectHandshake(runtime); err != nil {
		return err
	}
	return registry.Register(GateName, runtime)
}
func (mod *GateMod) Start() error {
	if mod.runtime == nil || mod.server == nil {
		return errors.New("gate wiring: not provided")
	}
	if err := mod.runtime.Start(); err != nil {
		return err
	}
	if err := mod.server.Start(); err != nil {
		return err
	}
	return mod.connections.register(mod.connections.gateService, mod.options.AdvertiseAddr, mod.StopBudget())
}
func (mod *GateMod) StopBudget() time.Duration { return mod.options.TCP.ShutdownTimeout }
func (mod *GateMod) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), mod.StopBudget())
	defer cancel()
	_ = mod.StopWithContext(ctx)
}
func (mod *GateMod) StopWithContext(ctx context.Context) error {
	if mod.runtime != nil {
		if err := mod.runtime.Stop(ctx); err != nil {
			return err
		}
	}
	if mod.server != nil {
		if err := mod.server.Stop(ctx); err != nil {
			return err
		}
		mod.server = nil
	}
	if err := mod.connections.deregister(ctx); err != nil {
		return err
	}
	mod.runtime = nil
	return nil
}
func (mod *GateMod) Addr() string {
	if mod.server == nil || mod.server.Addr() == nil {
		return ""
	}
	return mod.server.Addr().String()
}

// WebSocketAddr 是 Gate 的 WebSocket 监听实际地址（Options.TCP.WebSocket.Addr，GATEWAY.md“WebSocket 接入”）；
// 没开或未启动时为空。WebSocket 连接与 TCP 连接走同一个 TCPServer：握手、票据、路由、出站预算都相同。
func (mod *GateMod) WebSocketAddr() string {
	if mod.server == nil || mod.server.WebSocketAddr() == nil {
		return ""
	}
	return mod.server.WebSocketAddr().String()
}

type GameIngressMod struct {
	options     Options
	dispatch    gateway.TCPDispatch
	encoder     gateway.TCPEncoder
	runtime     *gateway.GameIngress
	connections connections
}

func NewGameIngressMod(dispatch gateway.TCPDispatch, encoder gateway.TCPEncoder, options Options) *GameIngressMod {
	return &GameIngressMod{dispatch: dispatch, encoder: encoder, options: options}
}
func (*GameIngressMod) Name() app.ModName            { return GameIngressName }
func (mod *GameIngressMod) DependsOn() []app.ModName { return dependencies(mod.options) }
func (mod *GameIngressMod) Init(*viper.Viper) error {
	if mod.dispatch == nil {
		return errors.New("gate wiring: formal dispatcher missing")
	}
	return mod.options.Config.Validate()
}
func (mod *GameIngressMod) Provide(registry *app.Registry) error {
	deps, err := connect(registry, mod.options, "game")
	if err != nil {
		return err
	}
	mod.connections = deps
	mod.runtime, err = gateway.NewGameIngress(mod.options.Config, deps.identity, gateway.GameDependencies{Client: deps.client, Authenticator: deps.auth, Dispatch: mod.dispatch, Encoder: mod.encoder, QueueFactory: AsyncQueueFactory, Ready: deps.ready, Matches: deps.matches, OnActive: mod.options.OnActive, OnClosed: mod.options.OnClosed, OnActivated: mod.options.OnActivated})
	if err != nil {
		return fmt.Errorf("game ingress wiring: %w", err)
	}
	if err := registry.RegisterBatch(app.Capability{Name: GameIngressName, Value: mod.runtime}, app.Capability{Name: PlayerRuntimeName, Value: gateway.PlayerRuntime(mod.runtime)}); err != nil {
		return err
	}
	return RegisterPlayerAdminCommands(registry, mod.runtime)
}

// RegisterPlayerAdminCommands 把玩家接入层的运维命令（entity.kick，gateway.RegisterAdminCommands）挂到 App 的 admin
// 注册表，经 ops 的 /admin/execute 调用（同一套 token 与审计）。一个进程只有一种玩家接入，由它在 Provide 时调用一次：
// 独立 Gate 部署下是 GameIngressMod（上面），嵌入 TCP 是生成的 player TCP Mod（internal/access/player/tcp/server_gen.go）。
func RegisterPlayerAdminCommands(registry *app.Registry, players gateway.PlayerRuntime) error {
	commands, ok := app.Lookup[*admin.Registry](registry, app.ModAdmin)
	if !ok || commands == nil {
		return fmt.Errorf("gate wiring: capability %q not found", app.ModAdmin)
	}
	if err := gateway.RegisterAdminCommands(commands, players); err != nil {
		return fmt.Errorf("gate wiring: %w", err)
	}
	return nil
}
func (mod *GameIngressMod) Start() error {
	if mod.runtime == nil {
		return errors.New("gate wiring: not provided")
	}
	if err := mod.runtime.Start(); err != nil {
		return err
	}
	return mod.connections.register(mod.connections.gameService, mod.options.AdvertiseAddr, mod.StopBudget())
}
func (mod *GameIngressMod) StopBudget() time.Duration {
	return max(10*time.Second, mod.options.Config.RequestTimeout+mod.options.Config.Outbound.MaxAge)
}
func (mod *GameIngressMod) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), mod.StopBudget())
	defer cancel()
	_ = mod.StopWithContext(ctx)
}
func (mod *GameIngressMod) StopWithContext(ctx context.Context) error {
	if mod.runtime == nil {
		return nil
	}
	if err := mod.runtime.Stop(ctx); err != nil {
		return err
	}
	if err := mod.connections.deregister(ctx); err != nil {
		return err
	}
	mod.runtime = nil
	return nil
}

var _ app.Mod = (*GateMod)(nil)
var _ app.Mod = (*GameIngressMod)(nil)
