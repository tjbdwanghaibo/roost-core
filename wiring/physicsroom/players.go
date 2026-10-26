package physicsroom

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	domain "github.com/tjbdwanghaibo/roost-core/service/physicsroom"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	"google.golang.org/protobuf/encoding/protowire"
)

// PlayersName 是 PlayerIngress 作为 Mod 的名字。
const PlayersName app.ModName = "physicsroom.players"

// Seat 由业务决定一个已认证绑定进入哪个房间、控制哪个动态刚体（开房、权限与匹配属于业务）。
// 它在该连接的首个输入上调用一次，可以阻塞（例如按需建房）；返回错误时本次输入失败，下一次输入重试。
type Seat func(ctx context.Context, binding gateway.Binding) (room domain.Ref, bodyID uint64, err error)

// Game 是 PlayerIngress 用到的 Game 侧接入能力；*gateway.GameIngress 实现它。
type Game interface {
	Pusher
	SessionReceiver(sessionID string) (gateway.Binding, uint64, bool)
}

// PlayerIngressConfig 的两个消息号属于客户端协议，由业务分配（不能为 0，也不能相同）。
// 输入载荷是 Protobuf RoomInput：uint64 sequence=1; float acceleration_x=2; float acceleration_z=3; bool jump=4。
type PlayerIngressConfig struct {
	InputMessageID uint32
	FrameMessageID uint32
	Seat           Seat
}

// PlayerIngress 是 Game 进程里把已认证连接接到物理房间的正式接线（PHYSICS-ROOM-SERVICE「装配」第 6 步）：
//
//   - OnActive 记下绑定并给它一个随连接结束而取消的上下文；OnClosed 取消它。两者只改内存表，快速返回。
//   - Dispatch 只处理 InputMessageID：解码 RoomInput；该连接的首个输入经 Seat 选房选刚体、Join，并为这个成员
//     启动唯一的 Relay；之后每个输入 Submit 到服务端保存的 Member。Binding 取自 GameIngress 的认证记录，
//     不采信客户端字段；成员令牌不出 Game 进程。
//   - Relay 随连接取消而结束并 Leave；Relay 因出站或 RPC 错误结束时关闭该连接，客户端重连得到新成员与新基线。
//
// 其他消息号返回 ErrRequestInvalid：业务有自己的协议分发时，把这个消息号转给 Dispatch。
// 作为 Mod 装配时依赖 GameIngressMod 与房间能力（本地 Mod 或 ClientMod），Provide 时接上两者，Stop 时先于它们排空。
type PlayerIngress struct {
	cfg PlayerIngressConfig

	mu      sync.Mutex
	rooms   domain.Rooms
	game    Game
	closing bool
	conns   map[gateway.Binding]*playerConn
	relays  sync.WaitGroup
}

type playerConn struct {
	mu      sync.Mutex // 串行化同一连接的 Join 与 Submit；不跨连接持有
	ctx     context.Context
	cancel  context.CancelFunc
	joined  bool
	member  domain.Member
	binding gateway.Binding
}

func NewPlayerIngress(cfg PlayerIngressConfig) (*PlayerIngress, error) {
	if cfg.InputMessageID == 0 || cfg.FrameMessageID == 0 || cfg.InputMessageID == cfg.FrameMessageID || cfg.Seat == nil {
		return nil, domain.ErrRequestInvalid
	}
	return &PlayerIngress{cfg: cfg, conns: make(map[gateway.Binding]*playerConn)}, nil
}

// Attach 接上房间能力与 GameIngress；只调用一次。之前 Ready 为 false，Gate 不会准入新绑定。
func (p *PlayerIngress) Attach(rooms domain.Rooms, game Game) error {
	if rooms == nil || game == nil {
		return domain.ErrRequestInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing || p.rooms != nil {
		return domain.ErrStopping
	}
	p.rooms, p.game = rooms, game
	return nil
}

// Ready 供 GameIngress 的准入检查使用：已接上且未关闭。
func (p *PlayerIngress) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rooms != nil && !p.closing
}

func (p *PlayerIngress) OnActive(binding gateway.Binding, _ uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return domain.ErrStopping
	}
	ctx, cancel := context.WithCancel(context.Background())
	if old := p.conns[binding]; old != nil {
		old.cancel()
	}
	p.conns[binding] = &playerConn{ctx: ctx, cancel: cancel, binding: binding}
	slog.Debug("physicsroom player active", "player", binding.PlayerID, "session", binding.SessionID)
	return nil
}

func (p *PlayerIngress) OnClosed(binding gateway.Binding, _ uint64) {
	p.mu.Lock()
	c := p.conns[binding]
	delete(p.conns, binding)
	p.mu.Unlock()
	if c != nil {
		c.cancel()
		slog.Debug("physicsroom player closed", "player", binding.PlayerID)
	}
}

func (p *PlayerIngress) Dispatch(ctx context.Context, session gateway.Session, id, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
	if id != p.cfg.InputMessageID || kind != wire.PayloadProtobuf {
		return nil, domain.ErrRequestInvalid
	}
	input, err := decodeInput(payload)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	rooms, game, closing := p.rooms, p.game, p.closing
	p.mu.Unlock()
	if closing || rooms == nil {
		return nil, domain.ErrStopping
	}
	binding, _, ok := game.SessionReceiver(session.Principal().SessionID)
	if !ok {
		return nil, gateway.ErrBindingStale
	}
	p.mu.Lock()
	c := p.conns[binding]
	p.mu.Unlock()
	if c == nil {
		return nil, gateway.ErrBindingStale
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err = c.ctx.Err(); err != nil {
		return nil, err
	}
	if !c.joined {
		if err = p.join(ctx, rooms, game, session, c); err != nil {
			return nil, err
		}
	}
	if err = rooms.Submit(ctx, c.member, input); err != nil {
		return nil, err
	}
	if input.Jump || input.Sequence%20 == 0 {
		slog.Debug("physicsroom input accepted", "player", binding.PlayerID, "sequence", input.Sequence, "acceleration", input.Acceleration, "jump", input.Jump)
	}
	return &gateway.TCPResponse{MessageID: id, Sequence: seq}, nil
}

// join 在持有 c.mu 时调用：选房、Join，并登记唯一的 Relay。Close 之后不再启动新的 Relay。
func (p *PlayerIngress) join(ctx context.Context, rooms domain.Rooms, game Game, session gateway.Session, c *playerConn) error {
	room, body, err := p.cfg.Seat(ctx, c.binding)
	if err != nil {
		return err
	}
	member, err := rooms.Join(ctx, room, c.binding, body)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		leave, cancel := context.WithTimeout(context.Background(), joinLeaveTimeout)
		defer cancel()
		_ = rooms.Leave(leave, member)
		return domain.ErrStopping
	}
	p.relays.Add(1)
	p.mu.Unlock()
	c.member, c.joined = member, true
	go func() {
		defer p.relays.Done()
		err := Relay(c.ctx, rooms, game, c.binding, member, p.cfg.FrameMessageID)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("physicsroom relay ended", "player", c.binding.PlayerID, "room", member.Room.ID, "error", err)
			_ = session.Close(err)
		}
	}()
	slog.Debug("physicsroom player joined", "player", c.binding.PlayerID, "room", room.ID, "body", body)
	return nil
}

// joinLeaveTimeout 是关闭途中刚完成的 Join 立即 Leave 的期限。
const joinLeaveTimeout = time.Second

// Close 拒绝新的绑定与输入，取消全部连接的 Relay（各自 Leave），等它们在 ctx 内退出。
// 超时返回 ctx 的错误，Relay 仍在后台结束；调用方不得先停掉房间能力所依赖的总线。
func (p *PlayerIngress) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closing = true
	conns := p.conns
	p.conns = make(map[gateway.Binding]*playerConn)
	p.mu.Unlock()
	for _, c := range conns {
		c.cancel()
	}
	done := make(chan struct{})
	go func() { p.relays.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Mod 接线：依赖 GameIngressMod 与房间能力，Provide 时 Attach，停止时 Close。
func (*PlayerIngress) Name() app.ModName { return PlayersName }
func (*PlayerIngress) DependsOn() []app.ModName {
	return []app.ModName{gatewiring.GameIngressName, domain.CapabilityName}
}
func (*PlayerIngress) Init(*viper.Viper) error { return nil }
func (p *PlayerIngress) Provide(r *app.Registry) error {
	rooms, ok := app.Lookup[domain.Rooms](r, domain.CapabilityName)
	if !ok {
		return errors.New("physicsroom players: rooms capability missing")
	}
	game, ok := app.Lookup[*gateway.GameIngress](r, gatewiring.GameIngressName)
	if !ok {
		return errors.New("physicsroom players: game ingress missing")
	}
	return p.Attach(rooms, game)
}
func (*PlayerIngress) Start() error { return nil }
func (p *PlayerIngress) Stop()      { _ = p.StopWithContext(context.Background()) }
func (p *PlayerIngress) StopWithContext(ctx context.Context) error {
	return p.Close(ctx)
}

var _ app.Mod = (*PlayerIngress)(nil)
var _ app.ModStopperWithContext = (*PlayerIngress)(nil)
var _ Game = (*gateway.GameIngress)(nil)

// decodeInput 解码 RoomInput；字段重复、类型不符、未知字段、jump 非 0/1 或缺少 sequence 均拒绝。
// 加速度的有限性与模长由 Service.Submit 检查。
func decodeInput(data []byte) (domain.Input, error) {
	var in domain.Input
	seen := uint8(0)
	for len(data) > 0 {
		field, typ, n := protowire.ConsumeTag(data)
		if n < 0 || field < 1 || field > 4 {
			return in, domain.ErrRequestInvalid
		}
		data = data[n:]
		bit := uint8(1 << field)
		if seen&bit != 0 {
			return in, domain.ErrRequestInvalid
		}
		seen |= bit
		if field == 2 || field == 3 {
			if typ != protowire.Fixed32Type {
				return in, domain.ErrRequestInvalid
			}
			v, n := protowire.ConsumeFixed32(data)
			if n < 0 {
				return in, domain.ErrRequestInvalid
			}
			data = data[n:]
			if field == 2 {
				in.Acceleration[0] = math.Float32frombits(v)
			} else {
				in.Acceleration[2] = math.Float32frombits(v)
			}
			continue
		}
		if typ != protowire.VarintType {
			return in, domain.ErrRequestInvalid
		}
		v, n := protowire.ConsumeVarint(data)
		if n < 0 {
			return in, domain.ErrRequestInvalid
		}
		data = data[n:]
		if field == 1 {
			in.Sequence = v
		} else if v > 1 {
			return in, domain.ErrRequestInvalid
		} else {
			in.Jump = v == 1
		}
	}
	if in.Sequence == 0 {
		return in, domain.ErrRequestInvalid
	}
	return in, nil
}
