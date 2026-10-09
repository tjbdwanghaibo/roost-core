package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
)

type handshakeTestHooks struct {
	before func(context.Context, ConnectionSession, string) error
	after  func(context.Context, ConnectionSession) error
	closed func(ConnectionSession)
}

func (hooks handshakeTestHooks) BeforeACK(ctx context.Context, s ConnectionSession, ticket string) error {
	return hooks.before(ctx, s, ticket)
}
func (hooks handshakeTestHooks) AfterACK(ctx context.Context, s ConnectionSession) error {
	return hooks.after(ctx, s)
}
func (hooks handshakeTestHooks) Closed(s ConnectionSession) {
	if hooks.closed != nil {
		hooks.closed(s)
	}
}

func handshakeServer(t *testing.T, hooks TCPHandshake) *TCPServer {
	t.Helper()
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	server, err := NewTCPServer(config, emptyTCPDispatch, AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) {
		return Principal{PlayerID: 7, SessionID: "session", ServerID: 2}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConnectHandshake(hooks); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return server
}

func TestTCPBindFinishesBeforeACKAndActivationCanPushAfterACK(t *testing.T) {
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	server := handshakeServer(t, handshakeTestHooks{
		before: func(ctx context.Context, session ConnectionSession, ticket string) error {
			if ticket != "ticket" || len(session.ConnectionID()) != 32 || session.Principal().ServerID != 2 {
				t.Error("trusted handshake identity lost")
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		after: func(ctx context.Context, session ConnectionSession) error {
			return session.WritePacket(ctx, 55, 0, wire.PayloadProtobuf, true, []byte("activated"))
		},
		closed: func(ConnectionSession) { close(closed) },
	})
	connection, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := wire.Write(connection, []*wire.Packet{{MsgID: 0, Seq: 1, Payload: []byte("ticket")}}, 8192); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("bind not entered")
	}
	_ = connection.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, err := wire.Read(connection, 8192); err == nil {
		t.Fatal("ACK written while Bind incomplete")
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	unblock()
	ack, err := wire.Read(connection, 8192)
	if err != nil || ack.MsgID != 0 || ack.Seq != 1 || len(ack.Payload) != 0 {
		t.Fatalf("ACK=%+v err=%v", ack, err)
	}
	push, err := wire.Read(connection, 8192)
	if err != nil || push.MsgID != 55 || push.Flags != wire.FlagPush || string(push.Payload) != "activated" {
		t.Fatalf("activated push=%+v err=%v", push, err)
	}
	_ = connection.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("connection close did not release binding")
	}
}

func TestTCPRefusedOrPanickingBindNeverAcknowledgesAuthentication(t *testing.T) {
	for _, panicBefore := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "panic"}[panicBefore], func(t *testing.T) {
			closed := make(chan struct{})
			server := handshakeServer(t, handshakeTestHooks{before: func(context.Context, ConnectionSession, string) error {
				if panicBefore {
					panic("private failure")
				}
				return errors.New("bind refused")
			}, after: func(context.Context, ConnectionSession) error { t.Error("activation after refused Bind"); return nil }, closed: func(ConnectionSession) { close(closed) }})
			connection, err := net.Dial("tcp", server.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := wire.Write(connection, []*wire.Packet{{Seq: 1, Payload: []byte("ticket")}}, 8192); err != nil {
				t.Fatal(err)
			}
			_ = connection.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := wire.Read(connection, 8192); err == nil {
				t.Fatal("refused Bind got ACK")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("refused Bind not closed")
			}
		})
	}
}

func TestTCPCloseInterruptsBlockedWriteAndPushSequencesFollowStream(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	s := &tcpSession{connection: local, writeTimeout: time.Minute, maxPayloadBytes: 1024}
	written := make(chan error, 1)
	go func() {
		written <- s.WritePacket(context.Background(), 101, 0, wire.PayloadProtobuf, true, []byte("blocked"))
	}()
	// 读完头部后不读正文，确认 writer 已进入持有写锁的实际 I/O。
	header := make([]byte, wire.HeaderSize)
	if _, err := io.ReadFull(peer, header); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close(ErrSessionClosed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for blocked writer's minute-long deadline")
	}
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("blocked write succeeded after close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt write")
	}

	local, peer = net.Pipe()
	defer local.Close()
	defer peer.Close()
	s = &tcpSession{connection: local, writeTimeout: time.Second, maxPayloadBytes: 1024}
	var producers sync.WaitGroup
	for range 32 {
		producers.Go(func() {
			if err := s.WritePacket(context.Background(), 101, 0, wire.PayloadProtobuf, true, []byte("push")); err != nil {
				t.Error(err)
			}
		})
	}
	for seq := uint32(1); seq <= 32; seq++ {
		packet, err := wire.Read(peer, 1024)
		if err != nil || packet.Seq != seq {
			t.Fatalf("push stream sequence %d: packet=%v error=%v", seq, packet, err)
		}
	}
	producers.Wait()
}
