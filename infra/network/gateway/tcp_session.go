// TCP 接入共用运行实现；业务协议与部署接线由调用方注入。
package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
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
}

func clonePrincipal(principal Principal) Principal {
	principal.Claims = maps.Clone(principal.Claims)
	return principal
}

func (session *tcpSession) Principal() Principal { return clonePrincipal(session.principal) }

func (session *tcpSession) ConnectionID() string { return session.nonce }

func (session *tcpSession) WritePacket(ctx context.Context, messageID, sequence uint32, kind wire.PayloadKind, push bool, payload []byte) error {
	if messageID == 0 || kind > wire.PayloadLockstep || (!push && (kind != wire.PayloadProtobuf || sequence == 0)) {
		return ErrInvalidRequest
	}
	if push {
		return session.pushKind(ctx, messageID, payload, byte(kind)<<1)
	}
	return session.writeFrame(ctx, 0, messageID, sequence, payload)
}

func (session *tcpSession) Reply(ctx context.Context, value any) error {
	reply, ok := value.(TCPReply)
	var response *TCPResponse
	if ok {
		response = reply.TCPResponse()
	}
	if !ok || response == nil {
		return fmt.Errorf("player tcp: unsupported response %T", value)
	}
	if response.MessageID == 0 || response.Sequence == 0 {
		return fmt.Errorf("%w: response id and sequence must be non-zero", errInvalidFrame)
	}
	return session.writeFrame(ctx, 0, response.MessageID, response.Sequence, response.Payload)
}

func (session *tcpSession) push(ctx context.Context, messageID uint32, payload []byte) error {
	return session.pushKind(ctx, messageID, payload, 0)
}

func (session *tcpSession) pushKind(ctx context.Context, messageID uint32, payload []byte, kind byte) error {
	return session.writeFrame(ctx, flagServerPush|kind, messageID, 0, payload)
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
	// failure (RR-20260926-68). Anything else closes the connection: part of
	// the frame went out (the stream is out of step), write_timeout itself ran
	// out (the connection cannot take a byte for that long — RR-52's "slow
	// enough to drop"), or the socket failed (reset, closed).
	// Consequence: a caller that keeps pushing with deadlines shorter than
	// write_timeout never closes a half-dead connection through those pushes;
	// the next push with no or a longer deadline, or the read loop's
	// idle_timeout, is what finds and closes it (RR-20260926-68; OPEN-ITEMS A12).
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
