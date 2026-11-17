package relay

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
)

// conn 是一条客户端连接。
//
// 读 goroutine（Server.serveConn）独占 reader、account 之后的读循环与三个限额桶；slot 由 room 在锁内写入，
// 写入发生在读循环开始之前，之后不再改变（顶号是换一条新的 conn）。
// 写 goroutine（writeLoop）是唯一写 socket 的地方：发送队列由 mu 保护，计数包含已取走、正在写的那一批，
// 所以上限就是这条连接最多驻留的帧数与字节数；放不下时断开这一条连接，放帧的一方（房间、别的连接）从不等待。
type conn struct {
	server  *Server
	netConn net.Conn
	reader  *bufio.Reader
	account string
	slot    uint32

	frames, roomFrames, roomBytes bucket

	// 空闲检测不用读期限：WebSocket 接入（coder/websocket 的 NetConn）在读期限到期时直接关掉整条连接，relay 就来不及
	// 告诉客户端原因。改由计时器检查最后一帧的时间，超时走 finish（先发 Closed{idle_timeout} 再关），TCP 与 WebSocket 一样。
	lastFrame atomic.Int64 // 最近一帧（或开始计时）的 UnixNano
	idleLimit atomic.Int64 // 当前的空闲上限：令牌阶段是 AuthTimeout，进房后是 IdleTimeout
	idleTimer *time.Timer

	// changingSeen 是这条连接最近一次收到 host_changing 时房间的 changeGen（由 room.mu 保护）：每次换 host 最多一条。
	changingSeen uint64

	mu           sync.Mutex
	queue        [][]byte
	queuedFrames int
	queuedBytes  int
	closing      bool
	reason       wire.RelayReason // 第一次 finish 的原因，离房通知与报告用它
	wake         chan struct{}
	writerDone   chan struct{}
}

func newConn(s *Server, netConn net.Conn) *conn {
	cfg := s.config
	return &conn{server: s, netConn: netConn, reader: bufio.NewReader(netConn),
		frames: newBucket(cfg.FramesPerSecond), roomFrames: newBucket(cfg.RoomFramesPerSecond), roomBytes: newBucket(cfg.RoomBytesPerSecond),
		wake: make(chan struct{}, 1), writerDone: make(chan struct{})}
}

// readToken 读第一帧：message_id=0、flags=0 的令牌帧，载荷是 lobby 签的 rt1 令牌，按本实例名与共用密钥验签。
func (c *conn) readToken() (token.Claims, wire.RelayReason) {
	cfg := c.server.config
	c.watchIdle(cfg.AuthTimeout)
	p, err := wire.ReadRelay(c.reader, token.MaxBytes)
	if err != nil {
		return token.Claims{}, readFailure(err)
	}
	if p.RelayChannel() != wire.RelayNone || p.MsgID != 0 {
		return token.Claims{}, wire.RelayReasonProtocol
	}
	claims, err := token.Verify(cfg.TokenKey, string(p.Payload), cfg.Instance, c.server.now())
	if err != nil {
		return token.Claims{}, wire.RelayReasonTokenRejected
	}
	c.account = claims.Account
	c.idleLimit.Store(int64(cfg.IdleTimeout))
	c.touch()
	c.idleTimer.Reset(cfg.IdleTimeout) // 上限可能比 AuthTimeout 短：按新上限重新计时
	return claims, wire.RelayReasonNone
}

// readLoop 逐帧读、扣限额、按通道交给房间，返回连接结束的原因。
func (c *conn) readLoop(r *room) wire.RelayReason {
	for {
		p, err := c.readFrame()
		if err != nil {
			return readFailure(err)
		}
		now := time.Now()
		c.lastFrame.Store(now.UnixNano())
		if !c.frames.take(now, 1) {
			return wire.RelayReasonRateLimited
		}
		if p == nil { // 超大的快照或转交：载荷已丢掉、已回 Rejected{too_big}，连接保留
			continue
		}
		switch p.RelayChannel() {
		case wire.RelayGame:
			r.forwardGame(c, p)
		case wire.RelayRoom:
			// 房间通道超出自己的限额只丢这一帧，不断开：语音的突发不能把游戏连接拖下线。
			if c.roomFrames.ready(now, 1) && c.roomBytes.ready(now, len(p.Payload)) {
				c.roomFrames.tokens--
				c.roomBytes.tokens -= float64(len(p.Payload))
				r.forwardRoom(c, p)
			}
		case wire.RelayControl:
			if reason := c.control(r, p); reason != wire.RelayReasonNone {
				return reason
			}
		default: // 第二个令牌帧
			return wire.RelayReasonProtocol
		}
	}
}

// readFrame 读一帧。快照与转交帧的载荷上限是 snapshot_max_bytes（快照另加 8 字节 seq）：超过它但不到两倍时，
// 丢掉载荷、回 Rejected{too_big}、返回 nil 帧，连接保留（§4.6.3“超限拒收并告诉房主”）；再大的按超大帧断开。
// 其余帧的上限是 MaxPayloadBytes。上限在申请载荷内存之前按帧头检查。
func (c *conn) readFrame() (*wire.Packet, error) {
	cfg := c.server.config
	head, err := c.reader.Peek(wire.HeaderSize)
	if err != nil {
		return nil, err
	}
	hard := stateFrameHardLimit(cfg)
	h, err := wire.ParseRelayHeader(head, max(cfg.MaxPayloadBytes, hard))
	if err != nil {
		return nil, err
	}
	limit := cfg.MaxPayloadBytes
	if h.RelayChannel() == wire.RelayControl && (h.MsgID == wire.RelayOpSnapshot || h.MsgID == wire.RelayOpTransfer) {
		limit = stateFrameLimit(cfg, h.MsgID)
		if size := int(h.PayloadSize); size > limit && size <= hard {
			return nil, c.discardOversizedState(h)
		}
	}
	if int(h.PayloadSize) > limit {
		return nil, wire.ErrPacketTooBig
	}
	return wire.ReadRelay(c.reader, limit)
}

// discardOversizedState 丢掉一帧超大的快照或转交（不进内存），回 Rejected{too_big}。快照的 seq 带回去，
// 房主据此知道是哪一份被拒。
func (c *conn) discardOversizedState(h wire.Header) error {
	if _, err := c.reader.Discard(wire.HeaderSize); err != nil {
		return err
	}
	body, seq := int(h.PayloadSize), uint64(0)
	if h.MsgID == wire.RelayOpSnapshot {
		var raw [8]byte
		if _, err := io.ReadFull(c.reader, raw[:]); err != nil {
			return err
		}
		seq, body = binary.BigEndian.Uint64(raw[:]), body-8
	}
	if _, err := c.reader.Discard(body); err != nil {
		return err
	}
	c.enqueueControl(wire.RelayOpRejected, 0, wire.EncodeRelayRejected(wire.RelayRejected{Op: h.MsgID, Code: wire.RelayRejectTooBig, Seq: seq}))
	return nil
}

// stateFrameLimit 是快照或转交帧的载荷上限；stateFrameHardLimit 是两倍于它的硬顶（超过就断开）。
func stateFrameLimit(cfg Config, op uint32) int {
	if op == wire.RelayOpSnapshot {
		return cfg.SnapshotMaxBytes + 8
	}
	return cfg.SnapshotMaxBytes
}

func stateFrameHardLimit(cfg Config) int { return 2*cfg.SnapshotMaxBytes + 8 }

// control 处理客户端发来的控制帧。踢人、禁言、转交、候选顺序、快照只认当前房主，host_ready 只认当前候选
// （都在 room 里核对）；不认识的操作码丢弃，留给以后的协议扩展。
func (c *conn) control(r *room, p *wire.Packet) wire.RelayReason {
	switch p.MsgID {
	case wire.RelayOpPing:
		if len(p.Payload) > wire.RelayMaxPingBytes {
			return wire.RelayReasonProtocol
		}
		c.enqueueControl(wire.RelayOpPong, 0, p.Payload)
	case wire.RelayOpKick:
		r.kick(c, p.Seq)
	case wire.RelayOpMute:
		if len(p.Payload) != 1 || p.Payload[0] > 1 {
			return wire.RelayReasonProtocol
		}
		r.mute(c, p.Seq, p.Payload[0] == 1)
	case wire.RelayOpTransfer:
		r.transfer(c, p.Seq, p.Payload)
	case wire.RelayOpLeave:
		return wire.RelayReasonLeft
	case wire.RelayOpSuccessors:
		slots, err := wire.DecodeRelaySuccessors(p.Payload)
		if err != nil {
			return wire.RelayReasonProtocol
		}
		r.setSuccessors(c, slots)
	case wire.RelayOpSnapshot:
		snapshot, err := wire.DecodeRelaySnapshot(p.Payload)
		if err != nil {
			return wire.RelayReasonProtocol
		}
		r.storeSnapshot(c, snapshot)
	case wire.RelayOpHostReady:
		epoch, err := wire.DecodeRelayEpoch(p.Payload)
		if err != nil {
			return wire.RelayReasonProtocol
		}
		r.hostReady(c, epoch)
	}
	return wire.RelayReasonNone
}

// watchIdle 开始空闲检测：limit 内没有新帧就 finish(RelayReasonIdleTimeout)。计时器每个上限周期最多触发一次。
func (c *conn) watchIdle(limit time.Duration) {
	c.idleLimit.Store(int64(limit))
	c.touch()
	c.idleTimer = time.AfterFunc(limit, c.checkIdle)
}

func (c *conn) touch() { c.lastFrame.Store(time.Now().UnixNano()) }

// setIdleLimit 换空闲上限（成为房主时换成 HostIdleTimeout，降为成员时换回 IdleTimeout），按最后一帧的时间重新计时。
// 由房间在锁内调用；计时器在令牌阶段就已建好。
func (c *conn) setIdleLimit(limit time.Duration) {
	c.idleLimit.Store(int64(limit))
	if c.idleTimer != nil {
		c.idleTimer.Reset(max(limit-time.Since(time.Unix(0, c.lastFrame.Load())), 0))
	}
}

func (c *conn) checkIdle() {
	c.mu.Lock()
	closing := c.closing
	c.mu.Unlock()
	if closing {
		return
	}
	limit := time.Duration(c.idleLimit.Load())
	idle := time.Since(time.Unix(0, c.lastFrame.Load()))
	if idle >= limit {
		c.finish(wire.RelayReasonIdleTimeout, true)
		return
	}
	c.idleTimer.Reset(limit - idle)
}

// enqueueControl 往发送队列放一个 relay 生成的控制帧。
func (c *conn) enqueueControl(op, slot uint32, payload []byte) {
	data, err := wire.EncodeRelay([]*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: op, Seq: slot, Payload: payload}}, max(len(payload), 1))
	if err == nil {
		c.enqueue(data)
	}
}

// enqueue 把一帧编码好的字节放进发送队列，不阻塞。队列（含正在写的一批）放不下时，按慢连接断开这一条：
// 立即关 socket，不再给它写任何东西。data 可能与别的收件人共用，只读。
func (c *conn) enqueue(data []byte) {
	cfg := c.server.config
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	if c.queuedFrames+1 > cfg.SendQueueFrames || c.queuedBytes+len(data) > cfg.SendQueueBytes {
		c.closing, c.reason = true, wire.RelayReasonSlowConsumer
		c.mu.Unlock()
		_ = c.netConn.Close()
		c.signal()
		return
	}
	c.queue = append(c.queue, data)
	c.queuedFrames++
	c.queuedBytes += len(data)
	c.mu.Unlock()
	c.signal()
}

// finish 结束这条连接（只有第一次生效）。notify 为 true 时先把 RelayOpClosed{reason} 排在队尾，写 goroutine 把队列写完后
// 关 socket；为 false 时立即关 socket。
func (c *conn) finish(reason wire.RelayReason, notify bool) {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.closing, c.reason = true, reason
	if notify {
		data, err := wire.EncodeRelay([]*wire.Packet{{Flags: wire.FlagRelayControl, MsgID: wire.RelayOpClosed, Payload: []byte{byte(reason)}}}, 1)
		if err == nil {
			c.queue = append(c.queue, data)
			c.queuedFrames++
			c.queuedBytes += len(data)
		}
	}
	c.mu.Unlock()
	if !notify {
		_ = c.netConn.Close()
	}
	c.signal()
}

// closeReason 是连接结束的原因；连接还没 finish 过时按断开算。
func (c *conn) closeReason() wire.RelayReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reason == wire.RelayReasonNone {
		return wire.RelayReasonDisconnected
	}
	return c.reason
}

func (c *conn) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// writeLoop 是唯一写 socket 的 goroutine：每次取走整个队列一次写出，写完再扣计数；
// 写出错或超过 WriteTimeout 就关连接。连接 finish 之后把剩下的队列写完再关 socket。
//
// 队列里每一项是一个完整的帧。net.Buffers 在 TCP 上合成一次 writev；在别的 net.Conn 上逐项 Write——WebSocket
// 接入层（infra/network/wsconn）一次 Write 是一条消息，所以浏览器收到的每条消息恰好是一帧。不要把多帧拼成一项再写。
func (c *conn) writeLoop() {
	defer close(c.writerDone)
	defer c.netConn.Close()
	cfg := c.server.config
	for {
		c.mu.Lock()
		batch, closing := c.queue, c.closing
		c.queue = nil
		c.mu.Unlock()
		if len(batch) == 0 {
			if closing {
				return
			}
			<-c.wake
			continue
		}
		size := 0
		for _, data := range batch {
			size += len(data)
		}
		_ = c.netConn.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout))
		buffers := net.Buffers(batch)
		_, err := buffers.WriteTo(c.netConn)
		c.mu.Lock()
		c.queuedFrames -= len(batch)
		c.queuedBytes -= size
		c.mu.Unlock()
		if err != nil {
			c.finish(wire.RelayReasonDisconnected, false)
			return
		}
	}
}

// readFailure 把读错误换成原因码：帧太大、帧头非法、空闲超时，其余算断开。
func readFailure(err error) wire.RelayReason {
	var timeout net.Error
	switch {
	case errors.Is(err, wire.ErrPacketTooBig):
		return wire.RelayReasonFrameTooBig
	case errors.Is(err, wire.ErrInvalidPacket):
		return wire.RelayReasonProtocol
	case errors.As(err, &timeout) && timeout.Timeout():
		return wire.RelayReasonIdleTimeout
	}
	return wire.RelayReasonDisconnected
}

// bucket 是令牌桶：每秒补 rate 个，最多存 rate 个（突发量等于一秒的额度）。只由连接的读 goroutine 使用。
type bucket struct {
	rate, tokens float64
	last         time.Time
}

func newBucket(rate int) bucket { return bucket{rate: float64(rate), tokens: float64(rate)} }

// ready 补足到 now 之后，看够不够 n 个（不扣）。
func (b *bucket) ready(now time.Time, n int) bool {
	if !b.last.IsZero() {
		b.tokens = min(b.rate, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	return b.tokens >= float64(n)
}

// take 够 n 个就扣掉并返回 true。
func (b *bucket) take(now time.Time, n int) bool {
	if !b.ready(now, n) {
		return false
	}
	b.tokens -= float64(n)
	return true
}
