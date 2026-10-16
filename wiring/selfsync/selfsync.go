// Package selfsync 把“玩家看自己状态”（Entity Sync 自身视图）接到玩家接入层与 Nest Mod 自有的 Manager 上。
//
// 生命周期本身在 framework/sync/entitysync/policy.SelfView（传输无关、可单独测试）；这里只做三件接线：
//
//   - Transport：把 gateway.PlayerRuntime.PushSyncPlayer（嵌入 TCPRuntime 与独立 Gate 的 GameIngress 共用）
//     包成 entitysync.Transport。推送只准入、不等写出（出站队列契约），接入层还没就绪时整轮重试。
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
	"strconv"
	"sync/atomic"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
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
	// MaxFrameBytes 是一帧的上限，应不大于接入层的 max_payload_bytes；0 取 gateway 默认的 max_payload_bytes。
	MaxFrameBytes int
}

// SelfSync 是一个进程的自身视图接线。New 之后交给 Nest Mod（EntitySyncSetup）与 player TCP Mod
// （BindPlayers + Handshake，或独立 Gate 的 OnActive / OnClosed）。
type SelfSync struct {
	config  Config
	players atomic.Pointer[playerRuntime]
	view    atomic.Pointer[policy.SelfView]
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
	if config.MaxFrameBytes == 0 {
		config.MaxFrameBytes = int(gateway.DefaultTCPConfig().MaxPayloadBytes)
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
		Config: entitysync.ManagerConfig{Transport: transport{owner: s}},
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
	}
}

func (s *SelfSync) attach(manager *entitysync.Manager) (func(), error) {
	view, err := policy.NewSelfView(policy.SelfViewConfig{Manager: manager, Subject: s.subject, Lookup: s.lookup})
	if err != nil {
		return nil, err
	}
	if !s.view.CompareAndSwap(nil, view) {
		view.Close()
		return nil, errors.New("selfsync: already attached to an entity sync manager")
	}
	return func() {
		view.Close()
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

// transport 是 Manager 的 Transport：一帧就是一次 PushSyncPlayer。
type transport struct{ owner *SelfSync }

// Push 只准入不等写出：嵌入 TCP 放进每连接的出站队列，独立 Gate 放进 Game Ingress 的可靠队列。
// 接入层还没接上或正在启停（ErrTransportUnavailable）不是谁的错，整轮重试；其他错误（玩家没有连接、
// 连接收不下）只关这个会话，由 SelfView 判断是否重开。
func (t transport) Push(ctx context.Context, session entitysync.SessionID, frame []byte) error {
	players := t.owner.players.Load()
	if players == nil {
		return fmt.Errorf("%w: player runtime is not bound yet", entitysync.ErrRetryLater)
	}
	err := players.PushSyncPlayer(ctx, int64(session), t.owner.config.MessageID, frame)
	if errors.Is(err, gateway.ErrTransportUnavailable) {
		return fmt.Errorf("%w: %w", entitysync.ErrRetryLater, err)
	}
	return err
}

// MaxFrameBytes 实现 entitysync.FrameSizeLimiter：Manager 构造时取它与帧默认上限的较小值。
func (t transport) MaxFrameBytes() int { return t.owner.config.MaxFrameBytes }

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
