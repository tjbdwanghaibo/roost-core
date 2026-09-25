package main

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
	"net"
	"sync"
	"time"
)

// 测试信封在准入时固定 tick/计划时刻，再进入真实 AsyncTransport 队列。
// worker 不读取主循环正在变化的 tick，也不把出队时刻当成变更时刻。
type asyncLoad struct {
	base    *serverTransport
	queue   *nettransport.AsyncTransport
	adapter *entitysync.AsyncTransport
	counts  sync.Mutex
}

func newAsyncLoad(base *serverTransport) (*asyncLoad, error) {
	load := &asyncLoad{base: base}
	q, err := nettransport.NewAsyncTransport(load, nettransport.AsyncTransportConfig{MaxSessions: len(base.conns), MaxQueuedReliableBytes: 4 << 20, MaxResidentReliableBytes: 128 << 20, MaxReliableAge: 5 * time.Second, SendTimeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	load.queue = q
	load.adapter, err = entitysync.NewAsyncTransport(q)
	return load, err
}
func (l *asyncLoad) SessionOpened(id entitysync.SessionID) error { return l.adapter.SessionOpened(id) }
func (l *asyncLoad) SessionClosed(id entitysync.SessionID)       { l.adapter.SessionClosed(id) }
func (l *asyncLoad) Push(ctx context.Context, id entitysync.SessionID, raw []byte) error {
	packet := make([]byte, 24+len(raw))
	binary.LittleEndian.PutUint32(packet[:4], packetFrame)
	binary.LittleEndian.PutUint32(packet[4:8], uint32(len(raw)))
	tick, due := l.base.metadata()
	binary.LittleEndian.PutUint64(packet[8:16], uint64(tick))
	binary.LittleEndian.PutUint64(packet[16:24], uint64(due))
	copy(packet[24:], raw)
	return l.adapter.Push(ctx, id, packet)
}
func (l *asyncLoad) SendReliable(ctx context.Context, id nettransport.SessionID, packet []byte) error {
	conn := l.base.conns[int64(id)]
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	// one worker per connection preserves admitted order.
	var sendStarted time.Time
	if l.base.trace != nil {
		sendStarted = time.Now()
	}
	if _, err := (&net.Buffers{packet}).WriteTo(conn); err != nil {
		return err
	}
	l.base.traceSent(entitysync.SessionID(id), packet[24:], sendStarted)
	if int64(binary.LittleEndian.Uint64(packet[8:16])) > 0 {
		l.counts.Lock()
		l.base.frames++
		l.base.bytes += int64(len(packet) - 24)
		l.counts.Unlock()
	}
	return nil
}
func (l *asyncLoad) drain() error {
	if l == nil {
		return nil
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		stats := l.queue.Counters()
		if stats.SendErrors != 0 || stats.ReliableAbandoned != 0 {
			return errors.New("async delivery failed")
		}
		if stats.ResidentReliableBytes == 0 {
			return nil
		}
		select {
		case <-deadline.C:
			return errors.New("async drain timed out")
		case <-poll.C:
		}
	}
}

func (l *asyncLoad) MaxFrameBytes() int { return l.adapter.MaxFrameBytes() - 24 }
