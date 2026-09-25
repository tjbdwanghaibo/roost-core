package entitysync

import (
	"context"
	"encoding/binary"
	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/sync/nettransport"
	"io"
	"net"
	"testing"
	"time"
)

// 真实异步队列 + 独立连接：慢端停止读取，正常端逐帧解码，背压只关闭慢会话。
type pipeLoadSender struct {
	conns   map[SessionID]net.Conn
	started chan struct{}
}

func (s *pipeLoadSender) SendReliable(ctx context.Context, id SessionID, raw []byte) error {
	conn := s.conns[id]
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetWriteDeadline(time.Now()) })
	defer stop()
	if id == 1 {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(raw)))
	_, err := (&net.Buffers{size[:], raw}).WriteTo(conn)
	return err
}
func TestAsyncLoadSlowConsumerDoesNotDelayHealthyBaseline(t *testing.T) {
	sender := &pipeLoadSender{conns: make(map[SessionID]net.Conn), started: make(chan struct{}, 1)}
	slowServer, slowClient := net.Pipe()
	defer slowServer.Close()
	defer slowClient.Close()
	sender.conns[1] = slowServer
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	sender.conns[2] = server
	queue, err := nettransport.NewAsyncTransport(sender, nettransport.AsyncTransportConfig{ReliableQueueSize: 1, SendTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := NewAsyncTransport(queue)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, transport, ManagerConfig{})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = queue.Close(ctx)
	}()
	received := make(chan frame.Frame, 4)
	fail := make(chan error, 1)
	go func() {
		for range 3 {
			var size [4]byte
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := io.ReadFull(client, size[:]); err != nil {
				fail <- err
				return
			}
			raw := make([]byte, binary.BigEndian.Uint32(size[:]))
			if _, err := io.ReadFull(client, raw); err != nil {
				fail <- err
				return
			}
			f, err := DecodeFrame(raw, frame.DefaultLimits())
			if err != nil {
				fail <- err
				return
			}
			received <- f
		}
	}()
	packs := 0
	s := testSubject(t, 1, &packs)
	if err := m.Register(s); err != nil {
		t.Fatal(err)
	}
	open(t, m, 1, 2)
	for _, id := range []SessionID{1, 2} {
		mustSubscribe(t, m, id, 1, entity.SyncProfile{})
	}
	var ref frame.ObjectRef
	for i := 0; i < 3; i++ {
		if i > 0 {
			s.MarkDirty(1)
		}
		mustFlush(t, m)
		if i == 0 {
			select {
			case <-sender.started:
			case <-time.After(time.Second):
				t.Fatal("slow sender never started")
			}
		}
		select {
		case f := <-received:
			if f.Tick != uint32(i+1) || f.BaseTick != uint32(i) {
				t.Fatalf("clock: %+v", f)
			}
			if i == 0 {
				ref = f.Objects[0].Ref
			} else if f.Objects[0].Ref != ref {
				t.Fatal("healthy reference reset")
			}
			update, err := DecodeSubjectUpdate(f.Objects[0].Components[0].Data, 0)
			if err != nil || update.Version != uint64(i) || (i > 0 && update.BaseVersion != uint64(i-1)) {
				t.Fatalf("version: %+v %v", update, err)
			}
		case err := <-fail:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("healthy receiver blocked by slow session")
		}
	}
	if stats := m.Stats(); stats.Sessions != 1 || stats.SessionsLost != 1 {
		t.Fatalf("slow consumer isolation: %+v", stats)
	}
	if queue.Stats().ReliableBackpressure != 1 {
		t.Fatal("slow session did not exercise backpressure")
	}
}
