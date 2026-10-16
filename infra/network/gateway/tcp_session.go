// TCP 接入共用运行实现；业务协议与部署接线由调用方注入。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

type tcpSession struct {
	connection      net.Conn
	principal       Principal
	nonce           string
	writeTimeout    time.Duration
	maxPayloadBytes uint32
	writeMu         sync.Mutex
	closeOnce       sync.Once
	closeErr        error
	serverSequence  atomic.Uint32
	// cancel ends the connection's context. Close calls it so a tcpSession that
	// is closed from outside — replaced by a new login, CloseSessions — stops
	// the request its connection is still running instead of only its socket.
	cancel context.CancelCauseFunc

	// 内嵌接入的出站队列：推送、回包与心跳 ACK 按入队顺序由唯一的 writeLoop 写出。
	// 推送方（场景 Sync Flush、Lockstep Tick、业务 PushPlayer）是逐会话串行的循环，
	// 同步写 socket 时一个停读的客户端会让排在它后面的每个连接每轮都晚到 write_timeout；
	// 入队只做有界准入，收不下（条数或字节超限）就关闭这一条连接，与独立 Gate 的慢连接策略一致。
	// 独立 Gate 自己的 AsyncTransport 已经排队并调用 WritePacket 直写，不经过这里。
	// queueMu 只保护队列本身，从不在持有时写 socket；writeLoop 每次取走整批再逐帧写。
	// queuedFrames / queuedBytes 记“排队 + 在途”：入队时加，writeLoop 写出一帧后才减，
	// 被取走、还卡在慢 socket 上的那一批仍占额度，上限因此就是这条连接最多驻留的帧数与字节数。
	// 读循环直写快路径的那一帧不经过队列，不计入。
	queueMu        sync.Mutex
	queue          []outboundFrame
	queuedFrames   int
	queuedBytes    int64
	maxQueuedBytes int64
	// writing 表示有人正在按队列顺序写 socket：writeLoop 写一批，或读循环走直写快路径。
	writing bool
	wake    chan struct{}
	closed  atomic.Bool
	// done 是连接 context 的 Done：会话关闭、连接返回或服务器停止时结束，writeLoop 与等待写出的回包随之返回。
	done       <-chan struct{}
	writerDone chan struct{}
}

// outboundFrame 是一帧已校验、调用方字节已复制的出站帧。服务推送的序号不在入队时分配，
// 由 writeLoop 在写出时分配，外部序号因此始终等于实际 socket 写顺序。
type outboundFrame struct {
	flags     byte
	messageID uint32
	sequence  uint32
	payload   []byte
	// written 非 nil 时，writeLoop 写完（或连接失败）后把结果送回等待的入队方。
	written chan error
}

// outboundQueueFrames 与 outboundQueueBytes 是每连接出站队列（排队 + writeLoop 已取走未写完）的上限。字节上限是内存界，至少容纳两个最大帧；
// 条数上限只防海量小帧，要大于健康连接一次突发的帧数——生产者入队远快于逐帧写 socket，
// 一个循环连推几十帧给同一连接是正常的。积压超过上限的客户端读得太慢，断开后由重连恢复全量 / 追帧。
// 空闲连接不预分配队列；最坏驻留为 max_connections × 字节上限，只有全部连接同时停读才会达到。
const (
	outboundQueueFrames   = 1024
	minOutboundQueueBytes = 2 << 20
)

var errOutboundQueueFull = errors.New("player tcp: outbound queue full")

func outboundQueueBytes(maxPayloadBytes uint32) int64 {
	return max(minOutboundQueueBytes, 2*(int64(maxPayloadBytes)+headerSize))
}

// newTCPSession 创建会话并启动它的 writeLoop；连接 ctx 结束（Close 会结束它）时 writeLoop 退出，
// 调用方在释放连接前等 writerDone。
func newTCPSession(connectionCtx context.Context, cancel context.CancelCauseFunc, connection net.Conn, principal Principal, config TCPConfig) *tcpSession {
	session := &tcpSession{
		connection: connection, principal: clonePrincipal(principal), nonce: newBindingToken(),
		writeTimeout: config.WriteTimeout, maxPayloadBytes: config.MaxPayloadBytes, cancel: cancel,
		wake: make(chan struct{}, 1), maxQueuedBytes: outboundQueueBytes(config.MaxPayloadBytes),
		done: connectionCtx.Done(), writerDone: make(chan struct{}),
	}
	go session.writeLoop()
	return session
}

func clonePrincipal(principal Principal) Principal {
	principal.Claims = maps.Clone(principal.Claims)
	return principal
}

func (session *tcpSession) Principal() Principal { return clonePrincipal(session.principal) }

func (session *tcpSession) ConnectionID() string { return session.nonce }

// WritePacket 是独立 Gate 的出站写：Gate 的 AsyncTransport 已为每连接排队并有自己的发送期限，
// 这里同步写 socket，结果直接回给那条队列；它与 writeLoop 共用 writeMu 与推送序号。
func (session *tcpSession) WritePacket(ctx context.Context, messageID, sequence uint32, kind wire.PayloadKind, push bool, payload []byte) error {
	if messageID == 0 || kind > wire.PayloadLockstep || (!push && (kind != wire.PayloadProtobuf || sequence == 0)) {
		return ErrInvalidRequest
	}
	if push {
		return session.writeFrame(ctx, flagServerPush|byte(kind)<<1, messageID, 0, payload)
	}
	return session.writeFrame(ctx, 0, messageID, sequence, payload)
}

// Reply 是业务可见的回包入口（异步任务也可能调用）：与推送一样只入队，返回 nil 表示本连接已准入，
// 不在调用方 goroutine 上等 socket，业务 worker 调用它也不会阻塞。读循环自己的回包走 replyWritten。
func (session *tcpSession) Reply(ctx context.Context, value any) error {
	response, err := tcpResponse(value)
	if err != nil {
		return err
	}
	_, err = session.enqueue(ctx, outboundFrame{messageID: response.MessageID, sequence: response.Sequence, payload: response.Payload}, false)
	return err
}

// replyWritten 是读循环写回包：入队后等它写出，再读下一个请求。
func (session *tcpSession) replyWritten(ctx context.Context, value any) error {
	response, err := tcpResponse(value)
	if err != nil {
		return err
	}
	return session.writeQueued(ctx, 0, response.MessageID, response.Sequence, response.Payload)
}

func tcpResponse(value any) (*TCPResponse, error) {
	reply, ok := value.(TCPReply)
	var response *TCPResponse
	if ok {
		response = reply.TCPResponse()
	}
	if !ok || response == nil {
		return nil, fmt.Errorf("player tcp: unsupported response %T", value)
	}
	if response.MessageID == 0 || response.Sequence == 0 {
		return nil, fmt.Errorf("%w: response id and sequence must be non-zero", errInvalidFrame)
	}
	return response, nil
}

// pushKind 把一帧服务推送放进出站队列，不等待写出：返回 nil 只表示本连接已准入。
// 调用方 ctx 只在入队前检查；入队后由 writeLoop 按 write_timeout 写出。
func (session *tcpSession) pushKind(ctx context.Context, messageID uint32, payload []byte, kind byte) error {
	_, err := session.enqueue(ctx, outboundFrame{flags: flagServerPush | kind, messageID: messageID, payload: payload}, false)
	return err
}

// pushWritten 把一帧服务推送放进出站队列并等它写出，给能阻塞、需要随链路放慢的调用方
// （Lockstep 追帧页：Match 在自己的后台协程里每会话一页在途地发送）。实时推送不得用它，用 pushKind 只准入：
// 场景 Sync Flush、Lockstep Tick 逐会话串行推送，等写出会让一个停读连接拖住其后所有连接。
// 返回 nil 表示已写进 socket；写出失败或连接关闭立即返回 errConnectionBroken（写协程同时关闭连接）；
// ctx 结束时这一帧若还在排队就撤回、不再写出，已被写协程取走的照常写完，调用方都得到 ctx 的错误。
// 不走 writeQueued 的直写快路径：调用方的 ctx 截止若早于 write_timeout，直写写出半帧会关闭连接，
// 经写协程写出则只受 write_timeout 约束，超时只是调用方不再等。
func (session *tcpSession) pushWritten(ctx context.Context, messageID uint32, payload []byte, kind byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	written, err := session.enqueue(ctx, outboundFrame{flags: flagServerPush | kind, messageID: messageID, payload: payload}, true)
	if err != nil {
		return err
	}
	select {
	case err := <-written:
		return err
	case <-session.done:
		select {
		case err := <-written:
			return err
		default:
		}
		return fmt.Errorf("%w: %w", errConnectionBroken, ErrSessionClosed)
	case <-ctx.Done():
		if !session.withdraw(written) {
			// 已被写协程取走：可能恰好写完，有结果就如实返回。
			select {
			case err := <-written:
				return err
			default:
			}
		}
		return ctx.Err()
	}
}

// withdraw 撤回还在排队、尚未被写协程取走的那一帧（按它的 written 通道认），归还它占的出站额度。
// 帧的推送序号在写出时才分配，撤回不会在客户端看到的序号里留下空洞。
func (session *tcpSession) withdraw(written <-chan error) bool {
	session.queueMu.Lock()
	defer session.queueMu.Unlock()
	for index, next := range session.queue {
		if next.written == written {
			session.queue = slices.Delete(session.queue, index, index+1)
			session.queuedFrames--
			session.queuedBytes -= int64(len(next.payload)) + headerSize
			return true
		}
	}
	return false
}

// writeQueued 让读循环的回包与心跳 ACK 按出站队列顺序写出并等它写完。等待发生在该连接自己的读循环上，
// 只拖住这一个客户端：handler 里先发的推送仍先于回包写出，回包写完才读下一个请求，与同步写时一致。
// 队列为空且没人在写时直接写（不复制、不交给 writeLoop），省掉每个请求一次协程切换；
// 直写期间到达的推送照常入队，排在回包之后。
func (session *tcpSession) writeQueued(ctx context.Context, flags byte, messageID, sequence uint32, payload []byte) error {
	session.queueMu.Lock()
	direct := !session.writing && len(session.queue) == 0 && !session.closed.Load()
	if direct {
		session.writing = true
	}
	session.queueMu.Unlock()
	if direct {
		err := session.writeFrame(ctx, flags, messageID, sequence, payload)
		session.finishWriting()
		if errors.Is(err, errConnectionBroken) {
			session.close(fmt.Errorf("%w: reply not written: %w", ErrSessionClosed, err))
		}
		return err
	}
	written, err := session.enqueue(ctx, outboundFrame{flags: flags, messageID: messageID, sequence: sequence, payload: payload}, true)
	if err != nil {
		return err
	}
	select {
	case err := <-written:
		return err
	case <-session.done:
		return fmt.Errorf("%w: %w", errConnectionBroken, ErrSessionClosed)
	}
}

// enqueue 对一帧做出站准入：调用方 ctx 已结束或 payload 超限是拒绝，不关闭连接；
// 会话已关闭返回 errConnectionBroken；排队 + 在途的条数或字节超限说明客户端读得太慢，关闭这条连接并返回 errConnectionBroken。
// 准入不取 writeMu，writeLoop 正卡在慢 socket 上时入队也立即返回。payload 在这里复制，调用返回后调用方可复用。
func (session *tcpSession) enqueue(ctx context.Context, next outboundFrame, wait bool) (<-chan error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := session.maxPayloadBytes
	if limit == 0 {
		limit = hardMaxPayload
	}
	if uint64(len(next.payload)) > uint64(limit) {
		return nil, fmt.Errorf("%w: outbound payload %d exceeds %d", errInvalidFrame, len(next.payload), limit)
	}
	if session.closed.Load() || session.wake == nil {
		return nil, fmt.Errorf("%w: %w", errConnectionBroken, ErrSessionClosed)
	}
	if len(next.payload) > 0 {
		next.payload = append([]byte(nil), next.payload...)
	}
	if wait {
		next.written = make(chan error, 1)
	}
	size := int64(len(next.payload)) + headerSize
	session.queueMu.Lock()
	if session.queuedFrames >= outboundQueueFrames || session.queuedBytes+size > session.maxQueuedBytes {
		session.queueMu.Unlock()
		return nil, session.overflow()
	}
	session.queue = append(session.queue, next)
	session.queuedFrames++
	session.queuedBytes += size
	session.queueMu.Unlock()
	select {
	case session.wake <- struct{}{}:
	default:
	}
	return next.written, nil
}

func (session *tcpSession) overflow() error {
	metrics.IncCounter("player_tcp_outbound_overflow_total", nil, 1)
	session.close(fmt.Errorf("%w: %w", ErrSessionClosed, errOutboundQueueFull))
	return fmt.Errorf("%w: %w", errConnectionBroken, errOutboundQueueFull)
}

// writeLoop 是内嵌接入每连接唯一的出站写协程。一帧在 write_timeout 内写不出、写出半帧或 socket 失败，
// 都关闭连接并退出；队列里剩下的帧随连接一起丢弃，客户端重连后恢复。
func (session *tcpSession) writeLoop() {
	defer close(session.writerDone)
	var batch []outboundFrame
	for {
		select {
		case <-session.done:
			return
		case <-session.wake:
		}
		session.queueMu.Lock()
		if session.writing || len(session.queue) == 0 {
			// 读循环正在直写：它结束时会再唤醒这里。
			session.queueMu.Unlock()
			continue
		}
		batch, session.queue = session.queue, batch[:0]
		session.writing = true
		session.queueMu.Unlock()
		for index, next := range batch {
			err := session.writeFrame(context.Background(), next.flags, next.messageID, next.sequence, next.payload)
			session.queueMu.Lock()
			session.queuedFrames--
			session.queuedBytes -= int64(len(next.payload)) + headerSize
			session.queueMu.Unlock()
			if next.written != nil {
				next.written <- err
			}
			if err != nil {
				session.close(fmt.Errorf("%w: outbound frame not written: %w", ErrSessionClosed, err))
				return
			}
			batch[index] = outboundFrame{}
		}
		session.finishWriting()
	}
}

// finishWriting 交还写权；写的期间有帧入队就唤醒 writeLoop 接着写。
func (session *tcpSession) finishWriting() {
	session.queueMu.Lock()
	session.writing = false
	pending := len(session.queue) > 0
	session.queueMu.Unlock()
	if pending {
		select {
		case session.wake <- struct{}{}:
		default:
		}
	}
}

func (session *tcpSession) writeFrame(ctx context.Context, flags byte, messageID, sequence uint32, payload []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	limit := session.maxPayloadBytes
	if limit == 0 {
		limit = hardMaxPayload
	}
	if uint64(len(payload)) > uint64(limit) {
		return fmt.Errorf("%w: outbound payload %d exceeds %d", errInvalidFrame, len(payload), limit)
	}
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	if session.closeErr != nil {
		return fmt.Errorf("%w: %w", errConnectionBroken, ErrSessionClosed)
	}
	// Checked again under the lock: a context that ended while another write
	// held it has written nothing, and must not be mistaken for a write the
	// connection failed (which closes it).
	if err := ctx.Err(); err != nil {
		return err
	}
	writeDeadline := time.Now().Add(session.writeTimeout)
	callerDeadline := false
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(writeDeadline) {
		writeDeadline, callerDeadline = contextDeadline, true
	}
	if err := session.connection.SetWriteDeadline(writeDeadline); err != nil {
		return fmt.Errorf("%w: %w", errConnectionBroken, err)
	}
	// 服务推送的外部序号与实际 socket 写顺序在同一临界区分配。
	if flags&flagServerPush != 0 {
		sequence = session.serverSequence.Add(1)
		if sequence == 0 {
			sequence = session.serverSequence.Add(1)
		}
	}
	header, err := (wire.Header{Flags: flags, MsgID: messageID, Seq: sequence, PayloadSize: uint32(len(payload))}).Encode(int(limit))
	if err != nil {
		return fmt.Errorf("%w: %w", errInvalidFrame, err)
	}
	buffers := net.Buffers{header[:], payload}
	written, err := buffers.WriteTo(session.connection)
	if err == nil {
		return nil
	}
	// The caller's own deadline — earlier than write_timeout — ran out before
	// a single byte went out (it can expire between the check above and the
	// write system call): nothing reached the stream, so it is still in step
	// and this is a refusal like a context already done, not a connection
	// failure. Anything else closes the connection: part of
	// the frame went out (the stream is out of step), write_timeout itself ran
	// out (the connection cannot take a byte for that long — slow enough to
	// drop), or the socket failed (reset, closed).
	// Consequence: a caller that keeps writing with deadlines shorter than
	// write_timeout never closes a half-dead connection through those writes;
	// the next write with no or a longer deadline, or the read loop's
	// idle_timeout, is what finds and closes it. Only the direct writers pass
	// such deadlines (the handshake ACK, standalone Gate's WritePacket); the
	// embedded writeLoop writes with write_timeout alone.
	if written == 0 && callerDeadline && errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("%w: nothing written before the caller's deadline: %w", context.DeadlineExceeded, err)
	}
	metrics.IncCounter("player_tcp_write_error_total", nil, 1)
	return fmt.Errorf("%w: %w", errConnectionBroken, err)
}

func (session *tcpSession) Close(reason error) error {
	session.close(reason)
	return session.closeErr
}

// close closes the connection once and reports whether this call did it.
func (session *tcpSession) close(reason error) (first bool) {
	session.closeOnce.Do(func() {
		first = true
		session.closed.Store(true)
		if reason == nil {
			reason = ErrSessionClosed
		}
		// net.Conn 支持与 Write 并发 Close。先关闭 socket 打断阻塞写，
		// 再取得状态锁；否则关闭通知会被慢客户端占住整个写期限。
		closeErr := session.connection.Close()
		session.writeMu.Lock()
		session.closeErr = errors.Join(reason, closeErr)
		session.writeMu.Unlock()
		if session.cancel != nil {
			session.cancel(reason)
		}
	})
	return first
}

var _ Session = (*tcpSession)(nil)
