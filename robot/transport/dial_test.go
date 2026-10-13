package transport

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// websocket 拨号受 ctx 与 Config.DialTimeout 约束：端点接受 TCP 却不回 HTTP 升级响应（指错端口、
// 网关卡住、黑洞代理）时，握手在超时或 ctx 取消时返回，connect 动作与 runner.Stop 不会被无限挡住。
// 历史：RR-20261005-NC-163

// silentListener 接受连接但从不回复。
func silentListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close() // 放开修前仍卡在握手里的拨号 goroutine
		}
	})
	return listener.Addr().String()
}

func TestWebSocketDialHonorsDialTimeoutAndContext(t *testing.T) {
	addr := silentListener(t)
	cases := map[string]struct {
		ctxTimeout  time.Duration
		dialTimeout time.Duration
	}{
		"dial_timeout": {ctxTimeout: time.Minute, dialTimeout: 200 * time.Millisecond},
		"ctx_cancel":   {ctxTimeout: 200 * time.Millisecond, dialTimeout: time.Minute},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.ctxTimeout)
			defer cancel()
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				conn, err := Dial(ctx, Config{Type: "ws", Endpoint: "ws://" + addr + "/", DialTimeout: tc.dialTimeout})
				if conn != nil {
					_ = conn.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("dial against a silent endpoint succeeded")
				}
				t.Logf("dial returned after %v: %v", time.Since(start).Round(time.Millisecond), err)
			case <-time.After(3 * time.Second):
				t.Fatalf("websocket dial still blocked after 3s (ctx %v, DialTimeout %v): the handshake ignores both", tc.ctxTimeout, tc.dialTimeout)
			}
		})
	}
}

// 正常的 websocket 端点仍能在超时内完成握手并往返一个包。
func TestWebSocketDialStillReachesARealEndpoint(t *testing.T) {
	server := httptest.NewServer(websocket.Handler(func(ws *websocket.Conn) {
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err == nil {
			_ = websocket.Message.Send(ws, data)
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, Config{Type: "ws", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/", DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WritePackets([]*Packet{{MsgID: 7, Seq: 1, Payload: []byte("hi")}}); err != nil {
		t.Fatal(err)
	}
	packet, err := conn.ReadPacket()
	if err != nil || packet.MsgID != 7 || packet.Seq != 1 || string(packet.Payload) != "hi" {
		t.Fatalf("echo = %+v, %v", packet, err)
	}
}
