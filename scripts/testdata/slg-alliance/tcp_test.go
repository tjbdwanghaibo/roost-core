package alliance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/wiring/selfsync"
)

const fogMessageID uint32 = 10101

// 真实嵌入 TCP 接线；测试票只承认两个固定角色，不代表正式账号服务验签。
// 场景串行驱动 Push/take，count 只界定要从 socket 读的包数，包体必须来自 wire.Read。
type viewSink struct {
	t           *testing.T
	runtime     *gateway.TCPRuntime
	addr        string
	connections map[entitysync.SessionID]net.Conn
	pending     map[entitysync.SessionID]int
}

func newViewSink(t *testing.T) *viewSink {
	t.Helper()
	cfg := gateway.DefaultTCPConfig()
	cfg.Addr = "127.0.0.1:0"
	rt := gateway.NewTCPRuntime(nil, time.Second)
	server, e := gateway.NewTCPServer(cfg, func(context.Context, gateway.Session, uint32, uint32, wire.PayloadKind, []byte) (any, error) {
		return nil, errors.New("no client commands in view fixture")
	}, gateway.AuthenticatorFunc(func(_ context.Context, token string, _ net.Addr) (gateway.Principal, error) {
		for id := int64(1); id <= 2; id++ {
			if token == fmt.Sprintf("fog-ticket-%d", id) {
				return gateway.Principal{PlayerID: id, ServerID: 999 + int32(id), SessionID: fmt.Sprintf("fog-%d", id)}, nil
			}
		}
		return gateway.Principal{}, errors.New("invalid fixture ticket")
	}))
	check(t, e)
	check(t, server.ConnectRuntime(rt))
	check(t, server.Start())
	s := &viewSink{t: t, runtime: rt, addr: server.Addr().String(), connections: map[entitysync.SessionID]net.Conn{}, pending: map[entitysync.SessionID]int{}}
	t.Cleanup(func() {
		for _, c := range s.connections {
			_ = c.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		check(t, server.Stop(ctx))
	})
	for _, id := range []entitysync.SessionID{1, 2} {
		s.connect(id)
	}
	return s
}
func (s *viewSink) connect(id entitysync.SessionID) {
	t := s.t
	if old := s.connections[id]; old != nil {
		if s.pending[id] != 0 {
			t.Fatal("reconnect before consuming previous socket")
		}
		_ = old.Close()
		s.waitSessions(id, 0)
	}
	c, e := net.DialTimeout("tcp", s.addr, 3*time.Second)
	check(t, e)
	s.connections[id] = c
	check(t, c.SetDeadline(time.Now().Add(3*time.Second)))
	check(t, wire.Write(c, []*wire.Packet{{Seq: 1, Payload: []byte(fmt.Sprintf("fog-ticket-%d", id))}}, 1<<20))
	ack, e := wire.Read(c, 1<<20)
	check(t, e)
	if ack.MsgID != 0 || ack.Seq != 1 {
		t.Fatal("unexpected handshake response")
	}
	check(t, c.SetDeadline(time.Time{}))
	s.waitSessions(id, 1)
}
func (s *viewSink) waitSessions(id entitysync.SessionID, n int) {
	until := time.Now().Add(3 * time.Second)
	for s.runtime.ActiveSessions(int64(id)) != n {
		if time.Now().After(until) {
			s.t.Fatal("TCP session registration timeout")
		}
		time.Sleep(time.Millisecond)
	}
}
func (s *viewSink) Push(ctx context.Context, id entitysync.SessionID, raw []byte) error {
	if e := selfsync.PushPlayerFrame(ctx, s.runtime, int64(id), fogMessageID, raw); e != nil {
		return e
	}
	s.pending[id]++
	return nil
}
func (s *viewSink) take(id entitysync.SessionID) [][]byte {
	var frames [][]byte
	c := s.connections[id]
	check(s.t, c.SetReadDeadline(time.Now().Add(3*time.Second)))
	for s.pending[id] > 0 {
		packet, e := wire.Read(c, 1<<20)
		check(s.t, e)
		if packet.MsgID != fogMessageID || packet.Flags != wire.FlagPush|wire.FlagSync {
			s.t.Fatal("unexpected TCP sync envelope")
		}
		frames = append(frames, packet.Payload)
		s.pending[id]--
	}
	check(s.t, c.SetReadDeadline(time.Time{}))
	return frames
}
