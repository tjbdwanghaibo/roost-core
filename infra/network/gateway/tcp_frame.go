// TCP 接入共用运行实现；业务协议与部署接线由调用方注入。
package gateway

import (
	"errors"
	"fmt"
	"io"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

const (
	headerSize              = wire.HeaderSize
	protocolVersion    byte = wire.Version
	flagServerPush     byte = wire.FlagPush
	flagSync           byte = wire.FlagSync
	flagLockstep       byte = wire.FlagLockstep
	maxPooledPayload        = 64 << 10
	hardMaxPayload          = 16 << 20
	hardMaxConnections      = 1000000
)

var (
	frameMagic              = [2]byte{'R', 'S'}
	errInvalidFrame         = errors.New("player tcp: invalid frame")
	ErrTransportUnavailable = errors.New("player tcp: transport unavailable")
	ErrSessionNotFound      = errors.New("player tcp: session not found")
	// errConnectionBroken marks a write that reached the connection and
	// failed there (a deadline, a reset, a closed socket), or a frame the
	// connection's outbound queue could not take because the client stopped
	// reading. Whatever part of
	// the frame went out, the stream is no longer in step with the client, so
	// the connection is closed. A frame refused before any
	// byte was written — the caller's context already done, the caller's own
	// deadline running out before the first byte, a payload
	// over the limit — is not the connection's fault and does not carry this
	// mark. write_timeout running out is the connection's fault even when
	// nothing was written.
	errConnectionBroken = errors.New("player tcp: connection cannot take the frame")
	// ErrResidentBudgetExhausted 是嵌入 TCP 因全进程出站驻留预算（player_access.tcp.outbound.resident_bytes）
	// 用尽而拒收一帧：不是这条连接读得慢，而是整个进程所有连接“排队 + 写协程在途”的字节到了上限。
	// 它不带 errConnectionBroken，连接不因此关闭；语义与独立 Gate 下 Game Ingress 的
	// nettransport.ErrResidentBudgetExhausted 相同（infra 不能依赖 framework，所以是两个哨兵），
	// Entity Sync 推送（wiring/selfsync.PushPlayerFrame）把两者都当作整轮重试。
	ErrResidentBudgetExhausted = errors.New("player tcp: process outbound resident byte budget exhausted")
)

// Wire format (network byte order): magic[2], version[1], flags[1],
// message_id[4], sequence[4], payload_length[4], payload. Message ID 0 is the
// authentication handshake before authentication, and an empty heartbeat after it.
// Heartbeats receive an empty ACK with the same sequence; all business messages require a non-zero ID.
// flags bit0 is push; bits1..2 select PB=0, Sync=1, Lockstep=2.
// The shared wire codec rejects unsupported kinds before payload allocation.
type frame struct {
	flags     byte
	messageID uint32
	sequence  uint32
	payload   []byte
}

func (server *TCPServer) readFrame(reader io.Reader) (frame, func(), error) {
	return server.readFrameLimit(reader, server.config.MaxPayloadBytes)
}

func (server *TCPServer) readFrameLimit(reader io.Reader, maxPayload uint32) (frame, func(), error) {
	var header [headerSize]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return frame{}, func() {}, err
	}
	decoded, err := wire.ParseHeader(header[:], int(maxPayload))
	if err != nil {
		return frame{}, func() {}, fmt.Errorf("%w: %w", errInvalidFrame, err)
	}
	length := decoded.PayloadSize
	var payload []byte
	pooled := false
	if length > 0 {
		buffer := server.payloadPool.Get().([]byte)
		if uint32(cap(buffer)) >= length {
			payload = buffer[:length]
			pooled = true
		} else {
			payload = make([]byte, length)
		}
		if _, err := io.ReadFull(reader, payload); err != nil {
			if pooled {
				server.payloadPool.Put(payload[:cap(payload)])
			}
			return frame{}, func() {}, err
		}
	}
	release := func() {
		if pooled && cap(payload) <= maxPooledPayload {
			server.payloadPool.Put(payload[:cap(payload)])
		}
	}
	return frame{flags: decoded.Flags, messageID: decoded.MsgID, sequence: decoded.Seq, payload: payload}, release, nil
}
