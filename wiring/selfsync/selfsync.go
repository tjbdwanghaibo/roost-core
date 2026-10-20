// Package selfsync 把“玩家看自己状态”（Entity Sync 自身视图）接到玩家接入层与 Nest Mod 自有的 Manager 上。
//
// 生命周期本身在 framework/sync/entitysync/policy.SelfView（传输无关、可单独测试）；这里只做三件接线：
//
//   - Transport：把 gateway.PlayerRuntime.PushSyncPlayer（嵌入 TCPRuntime 与独立 Gate 的 GameIngress 共用）
//     包成 entitysync.Transport。推送只准入、不等写出（出站队列契约）；结果按 PushPlayerFrame 的慢消费者规则
//     分类：接入层未就绪或进程驻留预算耗尽时整轮重试，活着的连接拒收时关闭该玩家的连接。
//   - 连接生命周期：嵌入 TCP 用 gateway.TCPHandshake 的 AfterACK / Closed，独立 Gate 用 GameIngress 的
//     OnActive / OnClosed，都转成 SelfView.Activate / Deactivate。
//   - Nest Mod 装配：EntitySyncSetup 把 Transport、SessionLost、SelfView 的安装与实体加载通知交给
//     wiring/nest.NewModWithEntitySync，Manager 的启停仍归 Nest Mod。
//
// 生成工程在 roost.yaml 的 access.player 上写 self_sync: true 时，生成器在 *_gen.go 里产出全部调用。
package selfsync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync/atomic"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	wiringnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
)

// Config 是 New 的输入。
type Config struct {
	// Entities 是实例级 ManagerAccess（生成 bootstrap 的 EntityAccess）。自身视图只读其中已发布的实体，不加载。
	Entities *entity.ManagerAccess
	// Kind 是玩家实体的 kind：subject ID = entity.BuildEntityID(playerID, Kind)。
	Kind entity.EntityKind
	// MessageID 是 Sync 推送使用的协议编号，客户端按 Sync 载荷类型 + 这个编号交给帧解码器。
	MessageID uint32
	// MaxFrameBytes 可选：在接入层上限之下再收紧一帧的上限；0 表示就用接入层上限。一帧的上限不由业务对齐：
	// 取它与接入层上限的较小值——嵌入 TCP 是 player_access.tcp.max_payload_bytes（Nest Mod Init 时经
	// EntitySyncSetup.LoadConfig 读，同一份声明），独立 Gate 是 Game Ingress 的 Config.MaxPayloadBytes
	// （生成的 IngressMod.WithSelfSync 交给 LimitFrameBytes）。超过接入层上限的帧会被接入层拒收、连接被关，
	// 客户端重连后的全量同样超限，反复断线重连，所以帧上限永远不大于接入层上限。
	MaxFrameBytes int
}

// accessConfig 是自身视图读的接入层配置：嵌入 TCP 一帧载荷的上限。键属于生成的 player TCP Mod（tcpConfig），
// 这里的声明必须与它逐字相同——App 合并声明时比较（不同即启动 / --check-config 报错），codegen 的
// TestPlayerTCPDeclarationAgreesWithKit 经快照 kitconfig_gen.go 钉住。
type accessConfig struct {
	TCP struct {
		MaxPayloadBytes uint32 `config:"max_payload_bytes" default:"1048576" example:"1048576"`
	} `config:"player_access.tcp"`
}

// ConfigSchema 是 EntitySyncSetup 并入 Nest Mod 的声明（player_access.tcp.max_payload_bytes）。
func ConfigSchema() app.ConfigSchema { return app.SchemaOf(accessConfig{}) }

// SelfSync 是一个进程的自身视图接线。New 之后交给 Nest Mod（EntitySyncSetup）与 player TCP Mod
// （BindPlayers + Handshake，或独立 Gate 的 OnActive / OnClosed）。
type SelfSync struct {
	config  Config
	players atomic.Pointer[playerRuntime]
	view    atomic.Pointer[policy.SelfView]
	// accessLimit 是从配置读到的 player_access.tcp.max_payload_bytes（loadConfig）；ingressLimit 是独立 Gate 部署下
	// Game Ingress 的载荷上限（LimitFrameBytes）。0 表示没有。Manager 创建时经 transport.MaxFrameBytes 取一次。
	accessLimit, ingressLimit atomic.Int64
}

// playerRuntime 包一层，让 atomic.Pointer 能存接口值。
type playerRuntime struct{ gateway.PlayerRuntime }

// New 校验配置。玩家推送能力在接入层 Provide 时经 BindPlayers 接上。
func New(config Config) (*SelfSync, error) {
	if config.Entities == nil || config.Entities.Manager() == nil {
		return nil, errors.New("selfsync: entity access is required")
	}
	if config.Kind == 0 {
		return nil, errors.New("selfsync: player entity kind is required")
	}
	if config.MessageID == 0 {
		return nil, errors.New("selfsync: sync push message id is required")
	}
	if config.MaxFrameBytes < 0 {
		return nil, fmt.Errorf("selfsync: max frame bytes %d is negative", config.MaxFrameBytes)
	}
	return &SelfSync{config: config}, nil
}

// MustNew 是 New，配置错误时 panic；给生成的包级变量用（配置是生成的常量，错了就是生成物不一致）。
func MustNew(config Config) *SelfSync {
	created, err := New(config)
	if err != nil {
		panic(err)
	}
	return created
}

// EntitySyncSetup 交给 wiring/nest.NewModWithEntitySync。同一个 SelfSync 只能装到一个 Mod 上。
func (s *SelfSync) EntitySyncSetup() wiringnest.EntitySyncSetup {
	return wiringnest.EntitySyncSetup{
		Config: entitysync.ManagerConfig{Name: "self_sync", Transport: transport{owner: s}},
		Configure: func(config *entitysync.ManagerConfig) {
			previous := config.SessionLost
			config.SessionLost = func(session entitysync.SessionID, cause error) {
				if previous != nil {
					previous(session, cause)
				}
				s.view.Load().SessionLost(session, cause)
			}
		},
		Attach:       s.attach,
		EntityLoaded: s.entityLoaded,
		ConfigSchema: ConfigSchema(),
		LoadConfig:   s.loadConfig,
	}
}

// loadConfig 在 Nest Mod Init、Manager 创建之前读接入层的帧上限（声明见 accessConfig）。
func (s *SelfSync) loadConfig(cfg *viper.Viper) error {
	var settings accessConfig
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("selfsync: %w", err)
	}
	s.accessLimit.Store(int64(settings.TCP.MaxPayloadBytes))
	return nil
}

// LimitFrameBytes 让一帧不超过 limit（> 0）：独立 Gate 部署下生成的 IngressMod.WithSelfSync 用 Game Ingress 的
// Config.MaxPayloadBytes 调它。必须在 Nest Mod Init（Manager 创建）之前调用——装配期构造 Mod 时；之后调用不再
// 改变已创建的 Manager，返回错误。多次调用取最小值。
func (s *SelfSync) LimitFrameBytes(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("selfsync: frame limit %d must be positive", limit)
	}
	if s.view.Load() != nil {
		return errors.New("selfsync: the entity sync manager already exists; limit frames before the Nest Mod initializes")
	}
	for {
		current := s.ingressLimit.Load()
		if current != 0 && current <= int64(limit) || s.ingressLimit.CompareAndSwap(current, int64(limit)) {
			return nil
		}
	}
}

// FrameLimit 是一帧的上限：Config.MaxFrameBytes、接入层上限（配置的 max_payload_bytes、Game Ingress 的上限）
// 中非零者的最小值；都没有时取 gateway 默认的 max_payload_bytes。Manager 创建时读一次。
func (s *SelfSync) FrameLimit() int {
	limit := int64(0)
	for _, candidate := range []int64{int64(s.config.MaxFrameBytes), s.accessLimit.Load(), s.ingressLimit.Load()} {
		if candidate > 0 && (limit == 0 || candidate < limit) {
			limit = candidate
		}
	}
	if limit == 0 {
		return int(gateway.DefaultTCPConfig().MaxPayloadBytes)
	}
	return int(limit)
}

// StrandedPlayersMetric 是自身视图“在线却没有打开的同步会话”的玩家数（Gauge，无标签，见 SelfView.Stranded）。
// 正常为 0；持续不为 0 说明有连接活着却收不到自己的状态。
const StrandedPlayersMetric = "entitysync.self_view.stranded_players"

func (s *SelfSync) attach(manager *entitysync.Manager) (func(), error) {
	view, err := policy.NewSelfView(policy.SelfViewConfig{Manager: manager, Subject: s.subject, Lookup: s.lookup})
	if err != nil {
		return nil, err
	}
	if !s.view.CompareAndSwap(nil, view) {
		view.Close()
		return nil, errors.New("selfsync: already attached to an entity sync manager")
	}
	// 登记在 SelfView 之后：同一个政策阶段里先由 SelfView 算出滞留人数，再写指标。
	unhookGauge := manager.RegisterPolicy(entitysync.PolicyHooks{Apply: func() error {
		metrics.SetGauge(StrandedPlayersMetric, nil, int64(view.Stranded()))
		return nil
	}})
	return func() {
		unhookGauge()
		view.Close()
		metrics.SetGauge(StrandedPlayersMetric, nil, 0)
		s.view.CompareAndSwap(view, nil)
	}, nil
}

func (s *SelfSync) subject(playerID int64) (int64, error) {
	return entity.BuildEntityID(playerID, s.config.Kind)
}

// lookup 只读内存里已发布的实体，不加载、不等待；在 Manager 的政策阶段调用。
func (s *SelfSync) lookup(subjectID int64) *entity.SubjectSyncState {
	loaded := s.config.Entities.Manager().Get(subjectID)
	if loaded == nil || loaded.Base() == nil {
		return nil
	}
	return loaded.Base().Sync()
}

func (s *SelfSync) entityLoaded(loaded entity.IThreadSafeEntity) {
	// kind 编码在实体 ID 里；只关心玩家实体。
	if loaded == nil || entity.GetEntityKindFromID(loaded.ID()) != s.config.Kind {
		return
	}
	s.view.Load().EntityLoaded(loaded.ID())
}

// BindPlayers 接上玩家推送能力（player TCP Mod 或 Game Ingress 在 Provide 时调用）。之前的推送整轮重试。
func (s *SelfSync) BindPlayers(players gateway.PlayerRuntime) {
	if players == nil {
		s.players.Store(nil)
		return
	}
	s.players.Store(&playerRuntime{players})
}

// Online 报告自身视图里当前有连接的玩家数；未装配时为 0。
func (s *SelfSync) Online() int { return s.view.Load().Online() }

// Stranded 报告在线却没有打开的同步会话超过几个同步周期的玩家数（SelfView.Stranded）；未装配时为 0。
func (s *SelfSync) Stranded() int { return s.view.Load().Stranded() }

// transport 是 Manager 的 Transport：一帧就是一次 PushPlayerFrame。
type transport struct{ owner *SelfSync }

// Push 只准入不等写出：嵌入 TCP 放进每连接的出站队列，独立 Gate 放进 Game Ingress 的可靠队列。
// 结果按 PushPlayerFrame 的慢消费者规则分类：接入层还没接上也是整轮重试。
func (t transport) Push(ctx context.Context, session entitysync.SessionID, frame []byte) error {
	players := t.owner.players.Load()
	if players == nil {
		return fmt.Errorf("%w: player runtime is not bound yet", entitysync.ErrRetryLater)
	}
	return PushPlayerFrame(ctx, players, int64(session), t.owner.config.MessageID, frame)
}

// PlayerConnections 是 PushPlayerFrame 需要的那部分玩家接入层（gateway.PlayerRuntime 满足它）。
type PlayerConnections interface {
	PushSyncPlayer(ctx context.Context, playerID int64, messageID uint32, payload []byte) error
	ActiveSessions(playerID int64) int
	CloseSessions(playerID int64, reason error) int
}

// ErrSyncPushRefused 是 PushPlayerFrame 关闭玩家连接时给出的关闭原因（包着接入层的原错误）。
var ErrSyncPushRefused = errors.New("selfsync: a live connection refused a sync frame")

// PushPlayerFrame 把一帧 Entity Sync 推给玩家的全部连接，并按慢消费者规则（与 entitysync.AsyncTransport 同一条）
// 交给 Manager。会话 ID 就是玩家 ID 的 Manager（自身视图、game-demo 的场景）都用它做 Transport.Push：
//
//   - ctx 已结束：原样交还，Manager 自己整轮重试；不关任何连接（停机不能把在线玩家踢掉）。
//   - 接入层还没就绪 / 正在启停（gateway.ErrTransportUnavailable、nettransport.ErrTransportClosed），
//     或整个进程的驻留字节预算耗尽（独立 Gate 下 Game Ingress 的 nettransport.ErrResidentBudgetExhausted、嵌入 TCP 的
//     gateway.ErrResidentBudgetExhausted，且这名玩家的每条连接都因此拒收）：
//     不是谁的错，交还 entitysync.ErrRetryLater，整轮重试、保留脏状态；连接与会话都不动。
//   - 玩家没有连接（每条拒收都是 gateway.ErrSessionNotFound，且此刻也数不到连接）：原样交还，Manager 关会话；
//     之后来的连接是一次新激活 / 新登录，自己会打开会话。报“没有连接”时却数得到连接，按下一条处理。
//   - 其余拒收——活着的连接收不下（可靠队列满、绑定失效、帧超限等），或只有部分连接因预算拒收：先关闭这名玩家
//     的全部连接（CloseSessions，原因是 ErrSyncPushRefused 包着原错误），再交还原错误让 Manager 关同步会话。
//     同步会话按玩家开，会话关掉之后留下的连接都会处在“连接活着、同步会话已死”的状态，没有东西再打开它，
//     所以一条也不留：客户端照常重连，新连接从全量开始。只有部分连接因预算拒收时也关：收下这一帧的连接
//     已应用了它，整轮重试会让同一 tick 的重发被它当旧包丢掉、之后的增量基线对不上。
//
// 关连接不阻塞：嵌入 TCP 关 socket（打断正在进行的写）、Game Ingress 退役绑定都只取短锁、不等 I/O；关闭通知
// 经各自的通知队列异步送达（自身视图的 Deactivate 随后到）。PushPlayerFrame 在 Manager 的 Flush 里调用
// （同步循环协程或 Drain / Stop 的调用方），不在 Nest 快池上。
func PushPlayerFrame(ctx context.Context, players PlayerConnections, playerID int64, messageID uint32, frame []byte) error {
	err := players.PushSyncPlayer(ctx, playerID, messageID, frame)
	if err == nil || (ctx != nil && ctx.Err() != nil) {
		return err
	}
	switch refusals := refusalsOf(err); {
	case errors.Is(err, gateway.ErrTransportUnavailable), errors.Is(err, nettransport.ErrTransportClosed):
		return fmt.Errorf("%w: %w", entitysync.ErrRetryLater, err)
	case every(refusals, nettransport.ErrResidentBudgetExhausted, gateway.ErrResidentBudgetExhausted) && len(refusals) >= players.ActiveSessions(playerID):
		metrics.IncCounter(SyncPushRefusedMetric, metrics.Labels{"outcome": "retried"}, 1)
		return fmt.Errorf("%w: %w", entitysync.ErrRetryLater, err)
	case every(refusals, gateway.ErrSessionNotFound) && players.ActiveSessions(playerID) == 0:
		return err
	}
	players.CloseSessions(playerID, fmt.Errorf("%w: %w", ErrSyncPushRefused, err))
	metrics.IncCounter(SyncPushRefusedMetric, metrics.Labels{"outcome": "closed"}, 1)
	return err
}

// SyncPushRefusedMetric 计 PushPlayerFrame 处理的拒收（Counter）。标签 outcome：closed（活着的连接拒收，
// 关闭了该玩家的连接）、retried（整个进程的驻留预算耗尽，整轮重试）。接入层未就绪与玩家没有连接不计。
const SyncPushRefusedMetric = "player.sync.push_refused.total"

// refusalsOf 拆开接入层按连接汇总的错误（errors.Join，每条连接一个）；单个错误就是一条。
func refusalsOf(err error) []error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

// every 报告每个错误都是 targets 之一（errors.Is）。
func every(errs []error, targets ...error) bool {
	for _, err := range errs {
		if !slices.ContainsFunc(targets, func(target error) bool { return errors.Is(err, target) }) {
			return false
		}
	}
	return len(errs) > 0
}

// MaxFrameBytes 实现 entitysync.FrameSizeLimiter：Manager 构造时取它与帧默认上限的较小值（见 FrameLimit）。
func (t transport) MaxFrameBytes() int { return t.owner.FrameLimit() }

var _ entitysync.FrameSizeLimiter = transport{}

// Handshake 返回嵌入 TCP 的握手钩子（gateway.TCPServer.ConnectHandshake）：鉴权 ACK 写出之后登记连接，
// 连接关闭时撤销。钩子只记事实、立即返回，不占握手预算。
func (s *SelfSync) Handshake() gateway.TCPHandshake { return handshake{owner: s} }

type handshake struct{ owner *SelfSync }

func (handshake) BeforeACK(context.Context, gateway.ConnectionSession, string) error { return nil }

func (h handshake) AfterACK(_ context.Context, session gateway.ConnectionSession) error {
	h.owner.view.Load().Activate(session.Principal().PlayerID, session.ConnectionID())
	return nil
}

func (h handshake) Closed(session gateway.ConnectionSession) {
	h.owner.view.Load().Deactivate(session.Principal().PlayerID, session.ConnectionID())
}

// OnActive 是独立 Gate 模式下 Game Ingress 的激活回调（wiring/gate.Options.OnActive）：Gate 已经
// Bind → ACK，这里登记连接。只记事实，不失败。
func (s *SelfSync) OnActive(binding gateway.Binding, receiver uint64) error {
	s.view.Load().Activate(binding.PlayerID, ingressConnection(receiver))
	return nil
}

// OnClosed 是 Game Ingress 的关闭回调（wiring/gate.Options.OnClosed）。
func (s *SelfSync) OnClosed(binding gateway.Binding, receiver uint64) {
	s.view.Load().Deactivate(binding.PlayerID, ingressConnection(receiver))
}

func ingressConnection(receiver uint64) string { return "ingress:" + strconv.FormatUint(receiver, 10) }
