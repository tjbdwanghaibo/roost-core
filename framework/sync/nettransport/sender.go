// Package nettransport is the network side of entity sync: the UDP / KCP /
// QUIC session transports, the AEAD datagram wrapper, and AsyncTransport, the
// per-session reliable queue entitysync pushes frames through (backpressure
// closes the session). Unreliable datagrams go straight to a DatagramSender;
// lockstep does that. Payloads are opaque bytes here; what a frame means is
// frame's and entitysync's job.
package nettransport

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

var (
	ErrTransportRequired     = errors.New("nettransport: downstream transport is required")
	ErrTransportClosed       = errors.New("nettransport: transport is closed")
	ErrSessionNotRegistered  = errors.New("nettransport: session is not registered")
	ErrSessionAlreadyExists  = errors.New("nettransport: session is already registered")
	ErrSessionFailed         = errors.New("nettransport: session transport has failed")
	ErrSessionLimit          = errors.New("nettransport: session limit exceeded")
	ErrReliableBackpressure  = errors.New("nettransport: reliable queue is full")
	ErrReliableExpired       = errors.New("nettransport: reliable message expired")
	ErrReliableMessageTooBig = errors.New("nettransport: reliable message is too large")
	ErrRouteNotBound         = errors.New("nettransport: session network route is not bound")
	ErrProtocolConfig        = errors.New("nettransport: invalid protocol configuration")
	ErrPayloadTooLarge       = errors.New("nettransport: protocol payload is too large")
	ErrAuthentication        = errors.New("nettransport: packet authentication failed")
)

// ErrResidentBudgetExhausted 是 AsyncTransport 因“全部会话共用的驻留字节预算”（MaxResidentReliableBytes）
// 用尽而拒收：不是这个会话收得慢，而是整个进程待发的可靠字节到了上限。它同时满足
// errors.Is(err, ErrReliableBackpressure)，只看背压的调用方语义不变；需要区分的调用方（例如把它当作
// “整轮重试”而不是“这个接收者跟不上”）用 errors.Is(err, ErrResidentBudgetExhausted)。
var ErrResidentBudgetExhausted = fmt.Errorf("%w: global resident byte budget", ErrReliableBackpressure)

// DefaultMaxDatagram is the payload bound a conservative path MTU leaves for
// one unreliable datagram; the protocol transports default to it.
const DefaultMaxDatagram = 1200

type DatagramSender interface {
	SendDatagram(context.Context, SessionID, []byte) error
}

// DatagramPayloadLimiter is implemented by datagram senders that know the
// largest payload they accept — after their own envelope (AEAD header and
// tag, FEC / OOB framing). Callers that must size packets up front (the
// lockstep Match's broadcast budget) read it instead of assuming a constant:
// the KCP and QUIC transports default to DefaultMaxDatagram, and the AEAD UDP
// transport's 1232-byte packet leaves 1232-UDPEnvelopeOverhead.
type DatagramPayloadLimiter interface {
	MaxDatagramPayload() int
}

type DatagramBatchSender interface {
	SendDatagramBatch(context.Context, SessionID, [][]byte) error
}

type ReliableSender interface {
	SendReliable(context.Context, SessionID, []byte) error
}

// ReliablePacer 由“发送会随链路放慢”的可靠通道实现：SendReliable 等写出（嵌入 TCP 的 lockstep.TCPSender），
// 或发送窗口满即阻塞（KCP、QUIC）。没实现或返回 false 的通道视为只准入（独立 Gate 的 gate.Transport：
// 返回 nil 只表示 Game 本地收下），调用方（lockstep Match 的快照下发）要自己按固定预算节流。
type ReliablePacer interface {
	PacesReliableToLink() bool
}

// ReliablePayloadLimiter 由声明单条可靠消息上限的通道实现（KCP / QUIC 的 MaxReliableBytes、
// AsyncTransport、嵌入 TCP 与 Gate 的单包上限）。需要预先切块的调用方（lockstep 快照分块）据此核对块大小。
type ReliablePayloadLimiter interface {
	MaxReliableBytes() int
}

// CompositeTransport joins independent unreliable and reliable network lanes.
// A QUIC adapter, for example, can supply a DATAGRAM sender and a stream sender.
type CompositeTransport struct {
	Datagrams DatagramSender
	Reliable  ReliableSender
}

func (transport CompositeTransport) SendDatagram(ctx context.Context, session SessionID, payload []byte) error {
	if isNilInterface(transport.Datagrams) {
		return ErrTransportRequired
	}
	return transport.Datagrams.SendDatagram(ctx, session, payload)
}

func (transport CompositeTransport) SendDatagramBatch(ctx context.Context, session SessionID, packets [][]byte) error {
	if isNilInterface(transport.Datagrams) {
		return ErrTransportRequired
	}
	if batch, ok := transport.Datagrams.(DatagramBatchSender); ok {
		return batch.SendDatagramBatch(ctx, session, packets)
	}
	for _, packet := range packets {
		if err := transport.Datagrams.SendDatagram(ctx, session, packet); err != nil {
			return err
		}
	}
	return nil
}

// MaxDatagramPayload forwards the datagram lane's declared bound (0 when it
// declares none).
func (transport CompositeTransport) MaxDatagramPayload() int {
	if limiter, ok := transport.Datagrams.(DatagramPayloadLimiter); ok && !isNilInterface(transport.Datagrams) {
		return limiter.MaxDatagramPayload()
	}
	return 0
}

// PacesReliableToLink 转发可靠通道的声明（没有声明时为 false，按只准入处理）。
func (transport CompositeTransport) PacesReliableToLink() bool {
	pacer, ok := transport.Reliable.(ReliablePacer)
	return ok && !isNilInterface(transport.Reliable) && pacer.PacesReliableToLink()
}

// MaxReliableBytes 转发可靠通道声明的单条上限（没有声明时为 0）。
func (transport CompositeTransport) MaxReliableBytes() int {
	if limiter, ok := transport.Reliable.(ReliablePayloadLimiter); ok && !isNilInterface(transport.Reliable) {
		return limiter.MaxReliableBytes()
	}
	return 0
}

func (transport CompositeTransport) SendReliable(ctx context.Context, session SessionID, payload []byte) error {
	if isNilInterface(transport.Reliable) {
		return ErrTransportRequired
	}
	return transport.Reliable.SendReliable(ctx, session, payload)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type writeDeadlineSetter interface {
	SetWriteDeadline(time.Time) error
}

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

func interruptWriteOnCancel(ctx context.Context, setter writeDeadlineSetter) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = setter.SetWriteDeadline(time.Now())
	})
	return func() {
		if !stop() {
			<-done
		}
		_ = setter.SetWriteDeadline(time.Time{})
	}
}

func interruptReadOnCancel(ctx context.Context, setter readDeadlineSetter) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = setter.SetReadDeadline(time.Now())
	})
	return func() {
		if !stop() {
			<-done
		}
		_ = setter.SetReadDeadline(time.Time{})
	}
}

var _ Transport = CompositeTransport{}

var _ DatagramBatchTransport = CompositeTransport{}

var (
	_ ReliablePacer          = CompositeTransport{}
	_ ReliablePayloadLimiter = CompositeTransport{}
)
