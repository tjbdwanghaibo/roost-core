// Package relay 是 relay 转发服（docs/framework/RELAY.md；方案 FEATURE-2026-11-16-lobby-relay §4.4）。
//
// relay 只转发、不做逻辑：玩法跑在房主的客户端上，relay 只读 client/wire 的帧头（通道、槽位），不解析载荷。
//
//   - 连接：任意 net.Listener（TCP，或 WebSocket 接入层包出来的 net.Listener）。第一帧是 lobby 签的令牌；
//     房主的连接到达时房间才建成，成员只能进已建成的房间。
//   - 游戏通道：成员的帧加上成员槽位只转给房主；房主的帧按目标转给某个成员或全体成员。
//   - 房间通道：聊天、表情、语音，按目标直达（全体或某个槽位），房主也是收件人；另有单独限额，可被房主禁言。
//   - 控制帧：relay 告诉房主成员上下线；房主让 relay 踢人、禁言。lobby 的 leave 命令经 Server.Leave 移出账号。
//   - 报告：房间建成、成员进出、关闭时立即一条，另外每个房间定时一条心跳（Reporter，契约同 service/lobby.RoomEvent）。
//   - 房间结束：这一批房主断开就关房（hostLost 是下一批接换 host 的位置，§4.6）。
//
// 并发：每条连接一个读 goroutine（限额、按通道分发）和一个写 goroutine（有界发送队列，满了断开这一条，不阻塞别人）；
// 房间状态由 room.mu 保护，锁的顺序是 Server.mu → room.mu → conn.mu，持有 conn.mu 时不取别的锁。
// 向 lobby 的报告（Reporter）都在释放锁之后发出。
package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

// 缺省值（docs/framework/RELAY.md §4 有取值理由）。
const (
	DefaultMaxPayloadBytes     = 64 << 10
	DefaultFramesPerSecond     = 200
	DefaultRoomFramesPerSecond = 30
	DefaultRoomBytesPerSecond  = 64 << 10
	DefaultSendQueueFrames     = 1024
	DefaultSendQueueBytes      = 4 << 20
	DefaultIdleTimeout         = 30 * time.Second
	DefaultAuthTimeout         = 5 * time.Second
	DefaultWriteTimeout        = 10 * time.Second
	DefaultEmptyRoomTimeout    = 10 * time.Minute
	DefaultClosedRoomRetention = 2 * time.Minute
	DefaultHeartbeatInterval   = 5 * time.Second
)

// Config 是一个 relay 实例的设置。零值字段取上面的缺省值（WithDefaults）；Instance 必填。
type Config struct {
	// Instance 是本实例名：令牌里的 relay 实例必须等于它，etcd 广播与 NATS 报告也带它。
	Instance string
	// TokenKey 是与 lobby 共用的令牌密钥（relay.token_key_env，token.KeyFromEnv 读），至少 token.MinKeyBytes 字节。
	TokenKey []byte
	// MaxPayloadBytes 是单帧载荷上限；超过的帧（包括令牌首帧之后的任何帧）断开连接。
	MaxPayloadBytes int
	// FramesPerSecond 是每条连接每秒的帧数上限（所有通道合计，含心跳），突发量同为一秒的额度；超过断开连接。
	FramesPerSecond int
	// RoomFramesPerSecond / RoomBytesPerSecond 是房间通道另外的每秒帧数与字节数上限；超过的房间帧丢弃，连接保留，
	// 语音挤不掉游戏通道。
	RoomFramesPerSecond int
	RoomBytesPerSecond  int
	// SendQueueFrames / SendQueueBytes 是每条连接发送队列（排队 + 正在写）的上限；超过时断开这条连接。
	SendQueueFrames int
	SendQueueBytes  int
	// IdleTimeout：连接上这么久没收到任何帧（含心跳）就断开。客户端按它的三分之一左右发 RelayOpPing。
	IdleTimeout time.Duration
	// AuthTimeout：接入后这么久内没送来令牌首帧就断开。
	AuthTimeout time.Duration
	// WriteTimeout：一次写 socket 的期限；写不出去断开这条连接。
	WriteTimeout time.Duration
	// EmptyRoomTimeout：房间里只剩房主这么久就关房（成员都走了、房主还在的房间不会永远留着）。
	EmptyRoomTimeout time.Duration
	// ClosedRoomRetention：关过的房间号记住这么久，期间同一房间号的 host 令牌不能再建房。要不短于 lobby 签的令牌有效期。
	ClosedRoomRetention time.Duration
	// HeartbeatInterval：每个房间经 Reporter 发心跳的间隔，也是检查空房超时的间隔。
	HeartbeatInterval time.Duration
}

// WithDefaults 把零值字段换成缺省值。
func (c Config) WithDefaults() Config {
	setInt := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	setDur := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	setInt(&c.MaxPayloadBytes, DefaultMaxPayloadBytes)
	setInt(&c.FramesPerSecond, DefaultFramesPerSecond)
	setInt(&c.RoomFramesPerSecond, DefaultRoomFramesPerSecond)
	setInt(&c.RoomBytesPerSecond, DefaultRoomBytesPerSecond)
	setInt(&c.SendQueueFrames, DefaultSendQueueFrames)
	setInt(&c.SendQueueBytes, DefaultSendQueueBytes)
	setDur(&c.IdleTimeout, DefaultIdleTimeout)
	setDur(&c.AuthTimeout, DefaultAuthTimeout)
	setDur(&c.WriteTimeout, DefaultWriteTimeout)
	setDur(&c.EmptyRoomTimeout, DefaultEmptyRoomTimeout)
	setDur(&c.ClosedRoomRetention, DefaultClosedRoomRetention)
	setDur(&c.HeartbeatInterval, DefaultHeartbeatInterval)
	return c
}

// Validate 检查取过缺省值之后的设置。
func (c Config) Validate() error {
	switch {
	case !instancePattern.MatchString(c.Instance):
		return fmt.Errorf("relay: instance name %q must be 1-64 letters, digits, _ or - (it is the last segment of a NATS subject and an etcd key)", c.Instance)
	case len(c.TokenKey) < token.MinKeyBytes:
		return fmt.Errorf("relay: token key must be at least %d bytes", token.MinKeyBytes)
	case c.MaxPayloadBytes <= 0 || c.FramesPerSecond <= 0 || c.RoomFramesPerSecond <= 0 || c.RoomBytesPerSecond <= 0 ||
		c.SendQueueFrames <= 0 || c.SendQueueBytes <= 0:
		return errors.New("relay: limits must be positive")
	case c.RoomBytesPerSecond < c.MaxPayloadBytes:
		// 否则一帧满载的房间帧永远发不出去。
		return fmt.Errorf("relay: room_bytes_per_second %d is below max_payload_bytes %d", c.RoomBytesPerSecond, c.MaxPayloadBytes)
	case c.SendQueueBytes < 2*(c.MaxPayloadBytes+wire.HeaderSize):
		return fmt.Errorf("relay: send_queue_bytes %d must hold at least two full frames (%d)", c.SendQueueBytes, 2*(c.MaxPayloadBytes+wire.HeaderSize))
	case c.IdleTimeout <= 0 || c.AuthTimeout <= 0 || c.WriteTimeout <= 0 || c.EmptyRoomTimeout <= 0 ||
		c.ClosedRoomRetention <= 0 || c.HeartbeatInterval <= 0:
		return errors.New("relay: timeouts must be positive")
	}
	return nil
}

// instancePattern 是实例名的写法（与 lobby 的 relay 广播约定相同）。
var instancePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrStopped 是停机之后再调用 Serve 的结果。
var ErrStopped = errors.New("relay: server stopped")

// Server 是一个 relay 实例：持有本实例上的全部房间与连接。
type Server struct {
	config Config
	report Reporter
	now    func() time.Time

	mu        sync.Mutex
	stopping  bool
	rooms     map[string]*room
	closed    map[string]time.Time // 关过的房间号 → 记到什么时候（ClosedRoomRetention）
	listeners map[net.Listener]struct{}
	conns     map[*conn]struct{}
	wg        sync.WaitGroup
}

// NewServer 建一个 relay 实例。report 为 nil 时不报告（只用于测试）。
func NewServer(config Config, report Reporter) (*Server, error) {
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if report == nil {
		report = ReporterFunc(func(Report) {})
	}
	return &Server{config: config, report: report, now: time.Now,
		rooms: map[string]*room{}, closed: map[string]time.Time{}, listeners: map[net.Listener]struct{}{}, conns: map[*conn]struct{}{}}, nil
}

// Config 返回取过缺省值的设置。
func (s *Server) Config() Config { return s.config }

// Serve 在 listener 上接受连接，直到 listener 出错或 Shutdown。可以对多个 listener（TCP、WebSocket）各调用一次，
// 同一个房间里的连接可以来自不同的 listener。Shutdown 关闭 listener 后返回 nil。
func (s *Server) Serve(listener net.Listener) error {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		_ = listener.Close()
		return ErrStopped
	}
	s.listeners[listener] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, listener)
		s.mu.Unlock()
	}()
	for {
		netConn, err := listener.Accept()
		if err != nil {
			s.mu.Lock()
			stopping := s.stopping
			s.mu.Unlock()
			if stopping {
				return nil
			}
			var temporary interface{ Temporary() bool }
			if errors.As(err, &temporary) && temporary.Temporary() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		c := newConn(s, netConn)
		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			_ = netConn.Close()
			continue
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serveConn(c)
	}
}

// Run 每个 HeartbeatInterval 给每个房间发一次心跳、关掉只剩房主太久的房间、清理过期的关房记录，直到 ctx 结束。
func (s *Server) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *Server) tick() {
	now := s.now()
	s.mu.Lock()
	rooms := make([]*room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	for id, until := range s.closed {
		if !now.Before(until) {
			delete(s.closed, id)
		}
	}
	s.mu.Unlock()
	for _, r := range rooms {
		r.heartbeat(now)
	}
}

// Stats 是本实例的规模（etcd 广播用）。
type Stats struct {
	Rooms       int // 已建成的房间
	Connections int // 已接受的连接，含还在等令牌的
}

func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Rooms: len(s.rooms), Connections: len(s.conns)}
}

// Shutdown 停止接入、关闭全部房间（每条连接收到 RelayReasonShutdown 后断开），等连接收尾到 ctx 结束；
// 到期仍没收尾的连接直接关 socket。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	for listener := range s.listeners {
		_ = listener.Close()
	}
	rooms := make([]*room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, r := range rooms {
		r.close(wire.RelayReasonShutdown)
	}
	for _, c := range conns { // 还在等令牌的连接
		c.finish(wire.RelayReasonShutdown, true)
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		for _, c := range conns {
			_ = c.netConn.Close()
		}
		<-done
		return ctx.Err()
	}
}

// serveConn 是一条连接的读 goroutine：令牌 → 进房 → 读循环 → 离房 → 等写 goroutine 收尾。
func (s *Server) serveConn(c *conn) {
	go c.writeLoop()
	defer func() {
		if c.idleTimer != nil {
			c.idleTimer.Stop()
		}
		<-c.writerDone
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		s.wg.Done()
	}()
	claims, reason := c.readToken()
	if reason != wire.RelayReasonNone {
		c.finish(reason, reason != wire.RelayReasonDisconnected)
		return
	}
	r, reason := s.admit(claims, c)
	if reason != wire.RelayReasonNone {
		c.finish(reason, true)
		return
	}
	reason = c.readLoop(r)
	c.finish(reason, reason != wire.RelayReasonDisconnected)
	r.leave(c)
}

// admit 把通过验签的连接放进房间：房间不存在时，只有 host 令牌能建房（关过的房间号在 ClosedRoomRetention 内不能重建）；
// 房间已存在时由 room.join 按房间状态定角色（§4.6.5），不看令牌里的角色。
func (s *Server) admit(claims token.Claims, c *conn) (*room, wire.RelayReason) {
	now := s.now()
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return nil, wire.RelayReasonShutdown
	}
	r := s.rooms[claims.Room]
	if r == nil {
		if until, closed := s.closed[claims.Room]; (closed && now.Before(until)) || claims.Role != token.RoleHost {
			s.mu.Unlock()
			return nil, wire.RelayReasonRoomNotFound
		}
		r = newRoom(s, claims, c, now)
		s.rooms[claims.Room] = r
		s.mu.Unlock()
		s.report.Report(r.opened)
		return r, wire.RelayReasonNone
	}
	s.mu.Unlock()
	return r, r.join(claims, c, now)
}

// Leave 是 lobby 发来的 leave 命令（service/lobby.CommandLeave）：把 account 移出 room，等同于它自己断开；
// 是房主时按房主离开处理。房间或账号不在时什么也不做（命令可能迟到）。
func (s *Server) Leave(room, account string) {
	s.mu.Lock()
	r := s.rooms[room]
	s.mu.Unlock()
	if r != nil {
		r.remove(account)
	}
}

// forgetRoom 在房间关闭后把它从表里摘掉，并记下房间号，挡住还没过期的 host 令牌重建同一个房间。
func (s *Server) forgetRoom(r *room) {
	s.mu.Lock()
	if s.rooms[r.id] == r {
		delete(s.rooms, r.id)
	}
	s.closed[r.id] = s.now().Add(s.config.ClosedRoomRetention)
	s.mu.Unlock()
}
