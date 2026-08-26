// Package session multiplexes one robot connection: request/response
// correlation by packet seq, one-shot push waiters, and standing push
// handlers, with idempotent close fan-out. Ported from the cube robot
// service; this version additionally records a per-message latency
// histogram (robot.session.call) so load-test reports can break down which
// protocol is slow — the original had no per-call instrumentation at all.
package session

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/obs"
	"github.com/tjbdwanghaibo/cube-core/robot/protocol"
	"github.com/tjbdwanghaibo/cube-core/robot/transport"
)

var ErrClosed = errors.New("robot session: closed")

type Message struct {
	Packet    *transport.Packet
	Value     any
	DecodeErr error
}

type PushFilter func(*Message) bool
type PushHandler func(*Message)

type result struct {
	msg *Message
	err error
}

type waiter struct {
	msgID  uint32
	filter PushFilter
	ch     chan result
}

type Session struct {
	conn      transport.Conn
	protocols *protocol.Registry

	seq atomic.Uint32

	mu          sync.Mutex
	pending     map[uint32]chan result
	waiters     map[uint64]waiter
	handlers    map[uint32]map[uint64]PushHandler
	nextWaiter  uint64
	nextHandler uint64

	closeOnce sync.Once
	closed    chan struct{}
}

func New(conn transport.Conn, protocols *protocol.Registry) *Session {
	s := &Session{
		conn:      conn,
		protocols: protocols,
		pending:   make(map[uint32]chan result),
		waiters:   make(map[uint64]waiter),
		handlers:  make(map[uint32]map[uint64]PushHandler),
		closed:    make(chan struct{}),
	}
	go s.readLoop()
	return s
}

func (s *Session) SendPacket(packet *transport.Packet) error {
	if packet == nil {
		return errors.New("robot session: packet is nil")
	}
	if s == nil || s.conn == nil {
		return ErrClosed
	}
	select {
	case <-s.closed:
		return ErrClosed
	default:
	}
	return s.conn.WritePackets([]*transport.Packet{packet})
}

// Notify encodes and sends a fire-and-forget message (seq 0, no response).
func (s *Session) Notify(ctx context.Context, msgID uint32, value any) error {
	payload, err := s.protocols.Encode(msgID, value)
	if err != nil {
		return err
	}
	return s.sendWithContext(ctx, &transport.Packet{MsgID: msgID, Payload: payload})
}

// Call sends req and waits for the seq-correlated response. respID 0 accepts
// any response message id; a non-zero respID additionally asserts the id
// (protocols where request and response share one id simply pass reqID).
// Every call feeds the robot.session.call latency histogram, labeled by the
// request message id and result.
func (s *Session) Call(ctx context.Context, reqID uint32, respID uint32, req any) (*Message, error) {
	msg, err := s.call(ctx, reqID, respID, req)
	return msg, err
}

func (s *Session) call(ctx context.Context, reqID uint32, respID uint32, req any) (*Message, error) {
	if s == nil || s.conn == nil {
		return nil, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	observe := func(resultLabel string) {
		obs.ObserveHistogram("robot.session.call", obs.Labels{
			"msg":    strconv.FormatUint(uint64(reqID), 10),
			"result": resultLabel,
		}, time.Since(start))
	}
	payload, err := s.protocols.Encode(reqID, req)
	if err != nil {
		observe("encode_error")
		return nil, err
	}
	seq := s.nextSeq()
	ch := make(chan result, 1)
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		observe("closed")
		return nil, ErrClosed
	default:
	}
	s.pending[seq] = ch
	s.mu.Unlock()

	if err := s.conn.WritePackets([]*transport.Packet{{MsgID: reqID, Seq: seq, Payload: payload}}); err != nil {
		s.removePending(seq)
		observe("send_error")
		return nil, err
	}

	select {
	case <-ctx.Done():
		s.removePending(seq)
		observe("timeout")
		return nil, ctx.Err()
	case <-s.closed:
		s.removePending(seq)
		observe("closed")
		return nil, ErrClosed
	case res := <-ch:
		if res.err != nil {
			observe("error")
			return nil, res.err
		}
		if respID != 0 && res.msg != nil && res.msg.Packet != nil && res.msg.Packet.MsgID != respID {
			observe("mismatch")
			return nil, fmt.Errorf("robot session: response msg mismatch: got %d want %d", res.msg.Packet.MsgID, respID)
		}
		if res.msg != nil && res.msg.DecodeErr != nil {
			observe("decode_error")
			return nil, res.msg.DecodeErr
		}
		observe("ok")
		return res.msg, nil
	}
}

func (s *Session) WaitPush(ctx context.Context, msgID uint32, filter PushFilter) (*Message, error) {
	if s == nil {
		return nil, ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ch := make(chan result, 1)
	id := s.addWaiter(msgID, filter, ch)
	defer s.removeWaiter(id)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, ErrClosed
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		if res.msg != nil && res.msg.DecodeErr != nil {
			return nil, res.msg.DecodeErr
		}
		return res.msg, nil
	}
}

func (s *Session) RegisterPushHandler(msgID uint32, h PushHandler) func() {
	if s == nil || h == nil || msgID == 0 {
		return func() {}
	}
	s.mu.Lock()
	s.nextHandler++
	id := s.nextHandler
	if s.handlers[msgID] == nil {
		s.handlers[msgID] = make(map[uint64]PushHandler)
	}
	s.handlers[msgID][id] = h
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if handlers := s.handlers[msgID]; handlers != nil {
			delete(handlers, id)
			if len(handlers) == 0 {
				delete(s.handlers, msgID)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.closeWithErr(ErrClosed)
	return nil
}

func (s *Session) Closed() <-chan struct{} {
	if s == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return s.closed
}

func (s *Session) readLoop() {
	for {
		packet, err := s.conn.ReadPacket()
		if err != nil {
			s.closeWithErr(err)
			return
		}
		s.dispatch(packet)
	}
}

func (s *Session) dispatch(packet *transport.Packet) {
	if packet == nil {
		return
	}
	if packet.Seq != 0 {
		s.mu.Lock()
		ch := s.pending[packet.Seq]
		if ch != nil {
			delete(s.pending, packet.Seq)
		}
		s.mu.Unlock()
		if ch != nil {
			ch <- result{msg: s.decode(packet)}
			return
		}
	}

	msg := s.decode(packet)
	var waitDeliveries []chan result
	var handlers []PushHandler

	s.mu.Lock()
	for id, w := range s.waiters {
		if w.msgID != packet.MsgID {
			continue
		}
		if w.filter != nil && !w.filter(msg) {
			continue
		}
		waitDeliveries = append(waitDeliveries, w.ch)
		delete(s.waiters, id)
	}
	if byMsg := s.handlers[packet.MsgID]; len(byMsg) > 0 {
		handlers = make([]PushHandler, 0, len(byMsg))
		for _, h := range byMsg {
			handlers = append(handlers, h)
		}
	}
	s.mu.Unlock()

	for _, ch := range waitDeliveries {
		ch <- result{msg: msg}
	}
	for _, h := range handlers {
		h(msg)
	}
}

func (s *Session) decode(packet *transport.Packet) *Message {
	msg := &Message{Packet: packet}
	if s.protocols == nil {
		return msg
	}
	value, err := s.protocols.Decode(packet.MsgID, packet.Payload)
	if errors.Is(err, protocol.ErrDecoderNotFound) {
		// Unknown pushes stay raw instead of failing the session: robots
		// must tolerate protocol surface they do not model.
		return msg
	}
	msg.Value = value
	msg.DecodeErr = err
	return msg
}

func (s *Session) closeWithErr(err error) {
	if err == nil {
		err = ErrClosed
	}
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.mu.Lock()
		for seq, ch := range s.pending {
			delete(s.pending, seq)
			ch <- result{err: err}
		}
		for id, w := range s.waiters {
			delete(s.waiters, id)
			w.ch <- result{err: err}
		}
		s.handlers = make(map[uint32]map[uint64]PushHandler)
		s.mu.Unlock()
	})
}

func (s *Session) sendWithContext(ctx context.Context, packet *transport.Packet) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan error, 1)
	go func() {
		done <- s.SendPacket(packet)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	case <-s.closed:
		return ErrClosed
	}
}

func (s *Session) nextSeq() uint32 {
	for {
		seq := s.seq.Add(1)
		if seq != 0 {
			return seq
		}
	}
}

func (s *Session) removePending(seq uint32) {
	s.mu.Lock()
	delete(s.pending, seq)
	s.mu.Unlock()
}

func (s *Session) addWaiter(msgID uint32, filter PushFilter, ch chan result) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextWaiter++
	id := s.nextWaiter
	s.waiters[id] = waiter{msgID: msgID, filter: filter, ch: ch}
	return id
}

func (s *Session) removeWaiter(id uint64) {
	s.mu.Lock()
	delete(s.waiters, id)
	s.mu.Unlock()
}
