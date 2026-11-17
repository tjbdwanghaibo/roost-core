// Package relay 是开房间场景（房主的客户端就是 host）的 Go 客户端 SDK：lobby 的 HTTP 调用（Lobby）与 relay 的 TCP 连接
// （Client）。方案 FEATURE-2026-11-16-lobby-relay §4.5；契约见 docs/framework/LOBBY.md、docs/framework/RELAY.md。
//
// 帧格式不另起一套：relay 帧就是 client/wire 的 RS v2 帧，通道（游戏 / 房间 / 控制）在 flags 的 bits3..4，seq 是槽位，
// 编解码直接用 wire.EncodeRelay / ReadRelay。C# 与 TS 两份 SDK 用 client/spec/relay-packets.json 对齐同一组字节。
//
// 流程：
//
//	lobby := relay.NewLobby(lobbyURL, relay.StaticGameToken(gt1), nil)
//	ticket, err := lobby.CreateRoom(ctx, relay.CreateRoomRequest{Game: "party", Mode: "duel"}) // 或 JoinRoom
//	client := relay.NewClient(lobby, relay.Options{Game: "party"})
//	client.OnGame(func(f relay.Frame) { ... })            // 注册回调，都在读 goroutine 上依次调用
//	welcome, err := client.Connect(ctx, ticket)           // 首帧发令牌，等 relay 的 Welcome
//	client.SendGame(relay.All, 101, payload)               // 房主：发给全体成员；成员：只到房主
//
// 心跳：每 PingInterval 发一个 RelayOpPing（relay 的空闲超时缺省 30s，客户端缺省 10s 一次），relay 原样回 Pong，
// 据此测 RTT；ReadTimeout 内一帧都没收到就当断线。断线后重连要重新向 lobby 要令牌：Rejoin 调 lobby 的 join
// （对已在房间里的人幂等）再连 relay；Options.Reconnect 打开时 Client 自己按退避重试。
//
// 并发：Client 的方法可以从任意 goroutine 调用。回调都在这条连接的读 goroutine 上依次执行，回调里可以 Send、Kick、
// Close，但不能等 Done（会等自己）；回调要快，慢回调会拖住收帧，进而被 relay 当成慢连接断开。
package relay

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

// All 是目标槽位 0：发给全体（不含自己）。
const All = wire.RelayAll

// Frame 是收到或要发的一帧 relay 帧。
type Frame struct {
	// Channel 是通道：wire.RelayGame、wire.RelayRoom 或 wire.RelayControl。
	Channel wire.RelayChannel
	// Slot：收到的数据帧是发送者的槽位；要发的数据帧是目标槽位（All 为全体；成员的游戏帧只到房主，目标不看）；
	// 控制帧是控制对象的槽位。
	Slot uint32
	// MsgID：数据帧上由游戏自用（可以为 0）；控制帧上是操作码（wire.RelayOp*）。
	MsgID uint32
	// Flags 是数据帧上归游戏的 bits0..2（wire.FlagPush、FlagSync、FlagLockstep），relay 原样带过；通道位不在这里。
	// 控制帧上不用。
	Flags byte
	// Payload 是载荷。收到的帧独占它；发出时不保留引用。
	Payload []byte
}

// Welcome 是连上 relay 后的身份：自己的槽位、是不是房主、房主的槽位。
type Welcome struct {
	Slot     uint32
	Host     bool
	HostSlot uint32
}

// Member 是成员上下线的通知（只有房主收到）。Reason 在上线时是 0（新加入）或 wire.RelayReasonReplaced
// （同一账号重连顶掉了旧连接，槽位不变）；下线时是下线原因。
type Member struct {
	Slot    uint32
	Account string
	Reason  wire.RelayReason
}

// Disconnect 是一条连接结束的通知。Reconnecting 为 true 表示 Client 正按 Options.Reconnect 重连，之后会有
// OnReconnected 或者一条 Reconnecting 为 false 的 Disconnect；Reconnecting 为 false 的通知是最后一条，
// 之后 Done 关闭。Reason 是 relay 的 Closed 原因（没收到 Closed 时是 wire.RelayReasonDisconnected）；
// 本地 Close 时 Reason 是 wire.RelayReasonNone、Err 是 ErrClosed。
type Disconnect struct {
	Reason       wire.RelayReason
	Err          error
	Reconnecting bool
}

var (
	// ErrClosed：Client 已经 Close。
	ErrClosed = errors.New("relay: client closed")
	// ErrNotConnected：当前没有连接（还没 Connect、正在重连或已经断开）。
	ErrNotConnected = errors.New("relay: not connected")
	// ErrConnected：已经连着，不能再 Connect / Rejoin。
	ErrConnected = errors.New("relay: already connected")
	// ErrNotRoomHost：踢人、禁言只有房主能发。
	ErrNotRoomHost = errors.New("relay: only the room host may do this")
)

// ClosedError 是 relay 发来 RelayOpClosed 之后断开的连接：令牌被拒、房间不存在、满员、被踢、房间关闭等。
type ClosedError struct {
	Reason wire.RelayReason
}

func (e *ClosedError) Error() string { return "relay: closed by relay: " + e.Reason.String() }

// ReconnectPolicy 是断线后的自动重连。MaxAttempts 为 0 时不自动重连（缺省，与 Roost.Client 的 AutoReconnect 一致）。
// 每次尝试都先向 lobby 要新令牌（join 幂等），再连 relay；两次尝试之间按指数退避，从 MinBackoff 翻倍到 MaxBackoff。
//
// 只有连接自己断掉（读写出错、超时，没收到 Closed）或 relay 以 idle_timeout 断开时才重连；被踢、房间关闭、
// 被顶号、经 lobby 离开、relay 停机等原因不重连。尝试中 lobby 答 4xx（房间没了、被拒）或 relay 答终止性的 Closed
// 时立即停止。
type ReconnectPolicy struct {
	MaxAttempts int
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
}

// Options 是 Client 的设置。零值字段取缺省值。
type Options struct {
	// Game 是房间的游戏，Rejoin 调 lobby 的 join 时用。必填。
	Game string
	// Password 是房间密码，Rejoin 时带上：席位已被让出（预留过期）的人重新加入要它。
	Password string
	// Dial 建 TCP 连接；nil 用 net.Dialer。可以换成走代理的拨号。
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// ConnectTimeout 是一次连接（拨号、发令牌、等 Welcome）的期限，缺省 10s。
	ConnectTimeout time.Duration
	// PingInterval 是心跳间隔，缺省 10s（relay 的 idle_timeout 缺省 30s，取它的三分之一）。
	PingInterval time.Duration
	// ReadTimeout：这么久一帧都没收到就当断线，缺省 30s。要大于 PingInterval（每个 Ping 都有 Pong）。
	ReadTimeout time.Duration
	// WriteTimeout 是一次写 socket 的期限，缺省 10s；写不出去就断开。
	WriteTimeout time.Duration
	// MaxPayloadBytes 是收帧的载荷上限，缺省 wire.DefaultMaxPayload（1 MiB）；要不小于 relay 的 max_payload_bytes。
	MaxPayloadBytes int
	// Reconnect 是自动重连，缺省关闭。
	Reconnect ReconnectPolicy
}

func (o Options) withDefaults() Options {
	if o.Dial == nil {
		o.Dial = (&net.Dialer{}).DialContext
	}
	setDur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	setDur(&o.ConnectTimeout, 10*time.Second)
	setDur(&o.PingInterval, 10*time.Second)
	setDur(&o.ReadTimeout, 30*time.Second)
	setDur(&o.WriteTimeout, 10*time.Second)
	setDur(&o.Reconnect.MinBackoff, 500*time.Millisecond)
	setDur(&o.Reconnect.MaxBackoff, 5*time.Second)
	if o.MaxPayloadBytes <= 0 {
		o.MaxPayloadBytes = wire.DefaultMaxPayload
	}
	if o.ReadTimeout <= o.PingInterval {
		o.ReadTimeout = 3 * o.PingInterval
	}
	return o
}

// handlers 是回调。由 Client.mu 保护，调用时先在锁内取出、锁外调用。
type handlers struct {
	game, room               func(Frame)
	memberJoined, memberLeft func(Member)
	muted                    func(bool)
	disconnected             func(Disconnect)
	reconnected              func(Welcome)
	control                  map[uint32]func(Frame)
	// 换 host 的回调位置（尚未接线，见 OnYouAreHost）。
	youAreHost, hostChanging, hostChanged, hostResumed func(Frame)
}

// session 是一条已进房的 relay 连接。
type session struct {
	netConn net.Conn
	reader  *bufio.Reader
	welcome Welcome
	writeMu sync.Mutex
	stop    chan struct{} // 连接结束时关闭，停掉心跳
}

// Client 是一个玩家在一个房间里的 relay 连接（含断线重连）。用 NewClient 建，Close 之后不能再用。
type Client struct {
	lobby *Lobby
	opts  Options
	base  time.Time // Ping 载荷的时间基准（单调时钟）

	ctx    context.Context // Close 时取消：打断重连的退避与 lobby 调用
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	running bool // 有连接，或正在重连
	roomID  string
	current *session
	role    Welcome
	rtt     time.Duration
	done    chan struct{}
	h       handlers
}

// NewClient 建一个 Client。lobby 用于断线后重新要令牌；只用 Connect、不重连时可以为 nil。
func NewClient(lobby *Lobby, opts Options) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done)
	return &Client{lobby: lobby, opts: opts.withDefaults(), base: time.Now(), ctx: ctx, cancel: cancel, done: done,
		h: handlers{control: map[uint32]func(Frame){}}}
}

// OnGame 注册游戏通道的回调：房主收到成员的帧（Slot 是成员槽位），成员收到房主的帧（Slot 是房主槽位）。
func (c *Client) OnGame(fn func(Frame)) { c.mu.Lock(); c.h.game = fn; c.mu.Unlock() }

// OnRoom 注册房间通道的回调（聊天、表情、语音），Slot 是发送者槽位。
func (c *Client) OnRoom(fn func(Frame)) { c.mu.Lock(); c.h.room = fn; c.mu.Unlock() }

// OnMemberJoined / OnMemberLeft 注册成员上下线的回调（只有房主收到）。
func (c *Client) OnMemberJoined(fn func(Member)) { c.mu.Lock(); c.h.memberJoined = fn; c.mu.Unlock() }
func (c *Client) OnMemberLeft(fn func(Member))   { c.mu.Lock(); c.h.memberLeft = fn; c.mu.Unlock() }

// OnMuted 注册被房主禁言 / 解除禁言的回调（true 为禁言）。被禁言时房间通道的帧会被 relay 丢弃，游戏通道照常。
func (c *Client) OnMuted(fn func(bool)) { c.mu.Lock(); c.h.muted = fn; c.mu.Unlock() }

// OnDisconnected 注册连接结束的回调（见 Disconnect）。
func (c *Client) OnDisconnected(fn func(Disconnect)) {
	c.mu.Lock()
	c.h.disconnected = fn
	c.mu.Unlock()
}

// OnReconnected 注册自动重连成功的回调，参数是新连接的身份（成员重连后槽位可能变了）。
func (c *Client) OnReconnected(fn func(Welcome)) { c.mu.Lock(); c.h.reconnected = fn; c.mu.Unlock() }

// 换 host 的回调位置（方案 §4.6）。操作码在 64～127，由 host 组定编码表并追加进 client/spec/relay-packets.json；
// 这一批还没有接线：relay 发来这些操作码时按“不认识的操作码”丢弃（注册了 OnControl 的照样交给它）。接线时在
// controlHandlers 里加对应操作码、把载荷解成结构，再调用这些回调（参数会从原始帧换成解好的结构）。

// OnYouAreHost 注册“自己成了新房主”（you_are_host）的回调。尚未接线。
func (c *Client) OnYouAreHost(fn func(Frame)) { c.mu.Lock(); c.h.youAreHost = fn; c.mu.Unlock() }

// OnHostChanging 注册“换 host 开始、暂停发给房主的游戏帧”（host_changing）的回调。尚未接线。
func (c *Client) OnHostChanging(fn func(Frame)) { c.mu.Lock(); c.h.hostChanging = fn; c.mu.Unlock() }

// OnHostChanged 注册“换 host 完成”（host_changed）的回调。尚未接线。
func (c *Client) OnHostChanged(fn func(Frame)) { c.mu.Lock(); c.h.hostChanged = fn; c.mu.Unlock() }

// OnHostResumed 注册“房主在宽限期内回来了”（host_resumed）的回调。尚未接线。
func (c *Client) OnHostResumed(fn func(Frame)) { c.mu.Lock(); c.h.hostResumed = fn; c.mu.Unlock() }

// OnControl 注册某个控制帧操作码的回调，是控制帧的扩展口：relay 发来的这个操作码的每一帧都交给它
// （内置处理过的操作码先做内置处理，例如 MemberJoined 先解码再调 OnMemberJoined，然后再调这里）。
// 不认识、也没注册回调的操作码丢弃，与 relay 的做法一致。fn 为 nil 时撤销。
func (c *Client) OnControl(op uint32, fn func(Frame)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn == nil {
		delete(c.h.control, op)
		return
	}
	c.h.control[op] = fn
}

// Connect 用 lobby 给的 Ticket（建房或加入的答复）连上 relay：首帧发令牌，等到 Welcome 返回。
// relay 拒绝时返回 *ClosedError。成功后 Client 在后台收帧、发心跳，直到连接结束（Done 关闭）。
func (c *Client) Connect(ctx context.Context, ticket Ticket) (Welcome, error) {
	if err := c.reserve(); err != nil {
		return Welcome{}, err
	}
	sess, err := c.dial(ctx, ticket.RelayAddr, ticket.Token)
	if err != nil {
		c.release()
		return Welcome{}, err
	}
	return sess.welcome, c.start(ticket.Room.ID, sess)
}

// Rejoin 重新向 lobby 要令牌（join 对已在房间里的人幂等）再连 relay，用于关掉自动重连时由业务自己重连。
// 只能在之前 Connect 过、当前没有连接时调用。
func (c *Client) Rejoin(ctx context.Context) (Welcome, error) {
	if err := c.reserve(); err != nil {
		return Welcome{}, err
	}
	sess, err := c.rejoin(ctx)
	if err != nil {
		c.release()
		return Welcome{}, err
	}
	c.mu.Lock()
	room := c.roomID
	c.mu.Unlock()
	return sess.welcome, c.start(room, sess)
}

func (c *Client) reserve() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
		return ErrClosed
	case c.running:
		return ErrConnected
	}
	c.running = true
	return nil
}

func (c *Client) release() {
	c.mu.Lock()
	c.running = false
	c.mu.Unlock()
}

// start 记下新连接并起读 goroutine。Close 与拨号并发时（拨号期间 Close 了）关掉刚建的连接。
func (c *Client) start(roomID string, sess *session) error {
	c.mu.Lock()
	if c.closed {
		c.running = false
		c.mu.Unlock()
		_ = sess.netConn.Close()
		return ErrClosed
	}
	c.roomID, c.current, c.role = roomID, sess, sess.welcome
	done := make(chan struct{})
	c.done = done
	c.mu.Unlock()
	go c.run(sess, done)
	return nil
}

// Done 在当前这一轮连接（含自动重连）彻底结束、最后一个回调返回之后关闭。没有连接过时返回已关闭的通道。
func (c *Client) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

// Welcome 返回当前连接的身份；没有连接时是最后一次连接的身份。
func (c *Client) Welcome() Welcome { c.mu.Lock(); defer c.mu.Unlock(); return c.role }

// IsHost 报告自己是不是房主（以 relay 的 Welcome 为准）。
func (c *Client) IsHost() bool { return c.Welcome().Host }

// RoomID 是 Connect 时的房间号。
func (c *Client) RoomID() string { c.mu.Lock(); defer c.mu.Unlock(); return c.roomID }

// RTT 是最近一次心跳的往返时延；还没收到 Pong 时为 0。
func (c *Client) RTT() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.rtt }

// Close 断开连接并停止重连。不经 lobby 离开：席位在 lobby 里按 relay 的报告让出；要立即让出席位用 Leave。
// 不等读 goroutine 收尾（回调里也可以调）；要等就在回调之外等 Done。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	sess := c.current
	c.mu.Unlock()
	c.cancel()
	if sess != nil {
		_ = sess.netConn.Close()
	}
	return nil
}

// Leave 经 lobby 离开房间（让出席位；房主离开由 relay 走换 host），然后 Close。返回 lobby 的错误。
func (c *Client) Leave(ctx context.Context) error {
	c.mu.Lock()
	room := c.roomID
	c.mu.Unlock()
	var err error
	if c.lobby != nil && room != "" {
		_, err = c.lobby.LeaveRoom(ctx, room, c.opts.Game)
	}
	_ = c.Close()
	return err
}

// SendGame 发游戏通道的帧。房主：target 是成员槽位或 All（全体成员）；成员：只到房主，target 不看（写 All 即可）。
func (c *Client) SendGame(target, msgID uint32, payload []byte) error {
	return c.Send(Frame{Channel: wire.RelayGame, Slot: target, MsgID: msgID, Payload: payload})
}

// SendRoom 发房间通道的帧（聊天、表情、语音）：target 是某个槽位（房主也可以）或 All（除自己以外的全体）。
// 房间通道有单独的限额，超出的帧被 relay 丢弃；被禁言时也被丢弃。
func (c *Client) SendRoom(target, msgID uint32, payload []byte) error {
	return c.Send(Frame{Channel: wire.RelayRoom, Slot: target, MsgID: msgID, Payload: payload})
}

// Kick 让 relay 踢出槽位 slot 上的成员（只有房主能发）。被踢的账号在房间存续期间不能再进。
func (c *Client) Kick(slot uint32) error {
	if !c.IsHost() {
		return ErrNotRoomHost
	}
	return c.SendControl(wire.RelayOpKick, slot, nil)
}

// Mute 禁言（on 为 true）或解除禁言槽位 slot 的房间通道（只有房主能发）。
func (c *Client) Mute(slot uint32, on bool) error {
	if !c.IsHost() {
		return ErrNotRoomHost
	}
	flag := byte(0)
	if on {
		flag = 1
	}
	return c.SendControl(wire.RelayOpMute, slot, []byte{flag})
}

// SendControl 发一个控制帧。踢人、禁言、心跳有专门的方法；它是控制帧的扩展口，换 host 的控制帧
// （host_ready、transfer、successors、snapshot 等，操作码 64～127）下一批经它发出。
func (c *Client) SendControl(op, slot uint32, payload []byte) error {
	return c.Send(Frame{Channel: wire.RelayControl, Slot: slot, MsgID: op, Payload: payload})
}

// Send 发一帧。数据帧带上 Flags 的游戏位，控制帧只带通道位。没有连接时返回 ErrNotConnected；
// 写失败时断开连接（之后按 Disconnect 通知）并返回错误。
func (c *Client) Send(f Frame) error {
	data, err := encodeFrame(f, c.opts.MaxPayloadBytes)
	if err != nil {
		return err
	}
	c.mu.Lock()
	sess := c.current
	c.mu.Unlock()
	if sess == nil {
		return ErrNotConnected
	}
	return c.write(sess, data)
}

// encodeFrame 把 Frame 编成 relay 帧：通道位 + （数据帧的）游戏位，seq 是目标槽位。
func encodeFrame(f Frame, maxPayload int) ([]byte, error) {
	var flags byte
	switch f.Channel {
	case wire.RelayGame:
		flags = wire.FlagRelayGame | f.Flags&gameFlags
	case wire.RelayRoom:
		flags = wire.FlagRelayRoom | f.Flags&gameFlags
	case wire.RelayControl:
		flags = wire.FlagRelayControl
	default:
		return nil, fmt.Errorf("relay: frame channel %d is not game, room or control", f.Channel)
	}
	return wire.EncodeRelay([]*wire.Packet{{Flags: flags, MsgID: f.MsgID, Seq: f.Slot, Payload: f.Payload}}, maxPayload)
}

// encodeToken 编令牌首帧：message_id=0、flags=0（不是 relay 通道帧），载荷是 rt1 令牌原文。
func encodeToken(relayToken string) ([]byte, error) {
	return wire.EncodeRelay([]*wire.Packet{{Payload: []byte(relayToken)}}, max(len(relayToken), 1))
}

// gameFlags 是数据帧上归游戏的 flags 位（推送位与载荷类型）。
const gameFlags = wire.FlagPush | wire.FlagSync | wire.FlagLockstep

func (c *Client) write(sess *session, data []byte) error {
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	_ = sess.netConn.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
	if _, err := sess.netConn.Write(data); err != nil {
		_ = sess.netConn.Close() // 读 goroutine 随之结束，按断线通知
		return fmt.Errorf("relay: write: %w", err)
	}
	return nil
}

// dial 拨号、发令牌首帧（message_id=0、flags=0，载荷是 rt1 令牌原文）、等第一帧：Welcome 就进房成功，
// Closed 就是 relay 拒绝（*ClosedError）。relay 总是先发 Welcome，之后才有别的帧。
func (c *Client) dial(ctx context.Context, addr, relayToken string) (*session, error) {
	if addr == "" || strings.Contains(addr, "://") {
		return nil, fmt.Errorf("relay: %q is not a TCP address (the Go SDK connects over TCP; ask the lobby for transport %q)", addr, TransportTCP)
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.ConnectTimeout)
	defer cancel()
	netConn, err := c.opts.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("relay: dial %s: %w", addr, err)
	}
	// ctx 结束时打断下面阻塞的读写。
	stop := context.AfterFunc(ctx, func() { _ = netConn.SetDeadline(time.Unix(1, 0)) })
	sess, err := c.handshake(netConn, relayToken)
	if !stop() || err != nil {
		_ = netConn.Close()
		if err == nil || ctx.Err() != nil {
			err = fmt.Errorf("relay: connect %s: %w", addr, context.Cause(ctx))
		}
		return nil, err
	}
	_ = netConn.SetDeadline(time.Time{})
	return sess, nil
}

func (c *Client) handshake(netConn net.Conn, relayToken string) (*session, error) {
	data, err := encodeToken(relayToken)
	if err != nil {
		return nil, err
	}
	if _, err := netConn.Write(data); err != nil {
		return nil, fmt.Errorf("relay: send token: %w", err)
	}
	reader := bufio.NewReader(netConn)
	p, err := wire.ReadRelay(reader, c.opts.MaxPayloadBytes)
	if err != nil {
		return nil, fmt.Errorf("relay: read welcome: %w", err)
	}
	if p.RelayChannel() == wire.RelayControl && p.MsgID == wire.RelayOpClosed && len(p.Payload) == 1 {
		return nil, &ClosedError{Reason: wire.RelayReason(p.Payload[0])}
	}
	if p.RelayChannel() != wire.RelayControl || p.MsgID != wire.RelayOpWelcome {
		return nil, fmt.Errorf("%w: first relay frame is not a welcome (flags %08b, op %d)", wire.ErrInvalidPacket, p.Flags, p.MsgID)
	}
	welcome, err := wire.DecodeRelayWelcome(p.Payload)
	if err != nil {
		return nil, err
	}
	return &session{netConn: netConn, reader: reader, stop: make(chan struct{}),
		welcome: Welcome{Slot: p.Seq, Host: welcome.Host, HostSlot: welcome.HostSlot}}, nil
}

// run 是一轮连接的读 goroutine：收帧直到连接结束，按策略自动重连，最后关闭 done。
func (c *Client) run(sess *session, done chan struct{}) {
	defer close(done)
	for {
		reason, err := c.serve(sess)
		c.mu.Lock()
		closed := c.closed
		c.current = nil
		c.mu.Unlock()
		if closed {
			reason, err = wire.RelayReasonNone, ErrClosed
		}
		retry := !closed && c.opts.Reconnect.MaxAttempts > 0 && retryableReason(reason)
		c.emitDisconnect(Disconnect{Reason: reason, Err: err, Reconnecting: retry})
		if !retry {
			c.release()
			return
		}
		next, err := c.reconnect()
		if err == nil {
			c.mu.Lock()
			if c.closed {
				err = ErrClosed
			} else {
				c.current, c.role = next, next.welcome
			}
			c.mu.Unlock()
			if err != nil {
				_ = next.netConn.Close()
			}
		}
		if err != nil {
			final := Disconnect{Reason: wire.RelayReasonDisconnected, Err: err}
			var closedErr *ClosedError
			if errors.As(err, &closedErr) {
				final.Reason = closedErr.Reason
			}
			if errors.Is(err, ErrClosed) || c.ctx.Err() != nil {
				final = Disconnect{Reason: wire.RelayReasonNone, Err: ErrClosed}
			}
			c.emitDisconnect(final)
			c.release()
			return
		}
		c.mu.Lock()
		fn := c.h.reconnected
		c.mu.Unlock()
		if fn != nil {
			fn(next.welcome)
		}
		sess = next
	}
}

// serve 收帧、分发，直到连接结束；返回结束原因（relay 的 Closed 原因，或 RelayReasonDisconnected / Protocol）。
func (c *Client) serve(sess *session) (wire.RelayReason, error) {
	go c.heartbeat(sess)
	defer close(sess.stop)
	defer sess.netConn.Close()
	for {
		_ = sess.netConn.SetReadDeadline(time.Now().Add(c.opts.ReadTimeout))
		p, err := wire.ReadRelay(sess.reader, c.opts.MaxPayloadBytes)
		if err != nil {
			if errors.Is(err, wire.ErrInvalidPacket) || errors.Is(err, wire.ErrPacketTooBig) {
				return wire.RelayReasonProtocol, err
			}
			return wire.RelayReasonDisconnected, err
		}
		if reason, err := c.dispatch(p); reason != wire.RelayReasonNone {
			return reason, err
		}
	}
}

// heartbeat 每 PingInterval 发一个 Ping，载荷是发出时刻（相对 base 的单调纳秒，8 字节大端），Pong 原样带回。
func (c *Client) heartbeat(sess *session) {
	ticker := time.NewTicker(c.opts.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sess.stop:
			return
		case <-ticker.C:
			payload := binary.BigEndian.AppendUint64(nil, uint64(time.Since(c.base)))
			data, err := encodeFrame(Frame{Channel: wire.RelayControl, MsgID: wire.RelayOpPing, Payload: payload}, c.opts.MaxPayloadBytes)
			if err != nil || c.write(sess, data) != nil {
				return
			}
		}
	}
}

// dispatch 把一帧交给回调。返回非 RelayReasonNone 时连接结束。
func (c *Client) dispatch(p *wire.Packet) (wire.RelayReason, error) {
	f := Frame{Channel: p.RelayChannel(), Slot: p.Seq, MsgID: p.MsgID, Flags: p.Flags & gameFlags, Payload: p.Payload}
	c.mu.Lock()
	h := c.h
	user := c.h.control[p.MsgID]
	c.mu.Unlock()
	switch f.Channel {
	case wire.RelayGame:
		if h.game != nil {
			h.game(f)
		}
	case wire.RelayRoom:
		if h.room != nil {
			h.room(f)
		}
	case wire.RelayControl:
		if builtin := controlHandlers[f.MsgID]; builtin != nil {
			if reason, err := builtin(c, h, f); reason != wire.RelayReasonNone {
				return reason, err
			}
		}
		if user != nil {
			user(f)
		}
	default:
		return wire.RelayReasonProtocol, fmt.Errorf("%w: relay frame without a channel", wire.ErrInvalidPacket)
	}
	return wire.RelayReasonNone, nil
}

// controlHandlers 是 relay 发来的控制帧的内置处理：操作码 → 解码并调对应回调。返回非 RelayReasonNone 结束连接。
//
// 扩展点：换 host 的控制帧（wire 的 64～127：you_are_host、host_changing、host_changed、host_resumed、transfer、
// successors、snapshot）下一批在这张表里加一项、在 handlers 里加对应的回调；不在表里的操作码只交给 OnControl。
var controlHandlers = map[uint32]func(c *Client, h handlers, f Frame) (wire.RelayReason, error){
	wire.RelayOpWelcome: func(c *Client, _ handlers, f Frame) (wire.RelayReason, error) {
		welcome, err := wire.DecodeRelayWelcome(f.Payload)
		if err != nil {
			return wire.RelayReasonProtocol, err
		}
		c.mu.Lock()
		c.role = Welcome{Slot: f.Slot, Host: welcome.Host, HostSlot: welcome.HostSlot}
		c.mu.Unlock()
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpMemberJoined: func(_ *Client, h handlers, f Frame) (wire.RelayReason, error) {
		return memberEvent(h.memberJoined, f)
	},
	wire.RelayOpMemberLeft: func(_ *Client, h handlers, f Frame) (wire.RelayReason, error) {
		return memberEvent(h.memberLeft, f)
	},
	wire.RelayOpClosed: func(_ *Client, _ handlers, f Frame) (wire.RelayReason, error) {
		if len(f.Payload) != 1 || f.Payload[0] == 0 {
			return wire.RelayReasonProtocol, fmt.Errorf("%w: relay closed frame", wire.ErrInvalidPacket)
		}
		reason := wire.RelayReason(f.Payload[0])
		return reason, &ClosedError{Reason: reason}
	},
	wire.RelayOpMuted: func(_ *Client, h handlers, f Frame) (wire.RelayReason, error) {
		if len(f.Payload) != 1 || f.Payload[0] > 1 {
			return wire.RelayReasonProtocol, fmt.Errorf("%w: relay muted frame", wire.ErrInvalidPacket)
		}
		if h.muted != nil {
			h.muted(f.Payload[0] == 1)
		}
		return wire.RelayReasonNone, nil
	},
	wire.RelayOpPong: func(c *Client, _ handlers, f Frame) (wire.RelayReason, error) {
		if len(f.Payload) == 8 { // 别的长度不是本 SDK 发的 Ping，不算时延
			sent := time.Duration(binary.BigEndian.Uint64(f.Payload))
			c.mu.Lock()
			c.rtt = time.Since(c.base) - sent
			c.mu.Unlock()
		}
		return wire.RelayReasonNone, nil
	},
}

func memberEvent(fn func(Member), f Frame) (wire.RelayReason, error) {
	member, err := wire.DecodeRelayMember(f.Payload)
	if err != nil {
		return wire.RelayReasonProtocol, err
	}
	if fn != nil {
		fn(Member{Slot: f.Slot, Account: member.Account, Reason: member.Reason})
	}
	return wire.RelayReasonNone, nil
}

func (c *Client) emitDisconnect(d Disconnect) {
	c.mu.Lock()
	fn := c.h.disconnected
	c.mu.Unlock()
	if fn != nil {
		fn(d)
	}
}

// retryableReason 报告这种结束原因值不值得自动重连：只有连接自己断了（没收到 Closed）或 relay 判空闲超时。
func retryableReason(reason wire.RelayReason) bool {
	return reason == wire.RelayReasonDisconnected || reason == wire.RelayReasonIdleTimeout
}

// reconnect 按退避一次次 rejoin，直到成功、次数用完、遇到不值得重试的错误或 Close。
func (c *Client) reconnect() (*session, error) {
	policy := c.opts.Reconnect
	backoff := policy.MinBackoff
	var lastErr error
	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		timer := time.NewTimer(backoff)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return nil, ErrClosed
		case <-timer.C:
		}
		sess, err := c.rejoin(c.ctx)
		if err == nil {
			return sess, nil
		}
		if c.ctx.Err() != nil {
			return nil, ErrClosed
		}
		lastErr = err
		if !retryableError(err) {
			return nil, err
		}
		backoff = min(2*backoff, policy.MaxBackoff)
	}
	return nil, fmt.Errorf("relay: reconnect gave up after %d attempts: %w", policy.MaxAttempts, lastErr)
}

// rejoin 向 lobby 要新令牌（join 幂等）再连 relay。
func (c *Client) rejoin(ctx context.Context) (*session, error) {
	c.mu.Lock()
	room := c.roomID
	c.mu.Unlock()
	if c.lobby == nil || room == "" {
		return nil, errors.New("relay: rejoin needs a lobby and a room from an earlier Connect")
	}
	ticket, err := c.lobby.JoinRoom(ctx, room, JoinRoomRequest{Game: c.opts.Game, Password: c.opts.Password, Transport: TransportTCP})
	if err != nil {
		return nil, err
	}
	return c.dial(ctx, ticket.RelayAddr, ticket.Token)
}

// retryableError 报告一次重连尝试的失败值不值得再试：lobby 的 5xx、网络错误值得；lobby 的 4xx（房间没了、
// 被拒、满员）与 relay 的终止性 Closed 不值得。令牌被拒也再试一次：每次尝试都拿新令牌。
func retryableError(err error) bool {
	var lobbyErr *LobbyError
	if errors.As(err, &lobbyErr) {
		return lobbyErr.Temporary()
	}
	var closedErr *ClosedError
	if errors.As(err, &closedErr) {
		return retryableReason(closedErr.Reason) || closedErr.Reason == wire.RelayReasonTokenRejected
	}
	return true
}
