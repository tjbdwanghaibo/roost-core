package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/infra/network/wsconn"
)

// testTransport 是表驱动用例的一种接入（GATEWAY.md“WebSocket 接入”）：握手、收发的关键用例在 TCP 与 WebSocket 上
// 各跑一遍，服务端是同一个 TCPServer，WebSocket 连接经 wsconn 包成 net.Conn 走同一条会话路径。
type testTransport struct {
	name string
	// configure 打开这种接入需要的监听；TCP 不用改。
	configure func(*TCPConfig)
	// dial 连上 server，返回客户端一侧的字节流（还没认证）。readBuffer > 0 时把客户端 socket 的接收缓冲设成它
	// （停止读取的慢客户端用）。WebSocket 客户端的流是 websocket.NetConn：每次 Write 是一条消息，
	// 用 tcpSession.writeFrame 写帧时帧头与载荷各是一条消息（net.Buffers 逐段写），正好覆盖“一帧拆成多条消息”。
	dial func(t testing.TB, server *TCPServer, readBuffer int) net.Conn
}

var testTransports = []testTransport{
	{name: "tcp", configure: func(*TCPConfig) {}, dial: dialTCPClient},
	{name: "websocket", configure: withWebSocket, dial: dialWebSocketClient},
}

// forEachTransport 在每种接入上跑一遍 run。
func forEachTransport(t *testing.T, run func(*testing.T, testTransport)) {
	for _, transport := range testTransports {
		t.Run(transport.name, func(t *testing.T) { run(t, transport) })
	}
}

func withWebSocket(config *TCPConfig) {
	config.WebSocket.Addr = "127.0.0.1:0" // Path 留空：取缺省 /ws
}

func dialTCPClient(t testing.TB, server *TCPServer, readBuffer int) net.Conn {
	t.Helper()
	connection, err := net.Dial("tcp", server.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if readBuffer > 0 {
		_ = connection.(*net.TCPConn).SetReadBuffer(readBuffer)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

// dialWebSocket 拨号 WebSocket 监听，返回库的连接（不包 NetConn），header 是额外的请求头（Origin）。
func dialWebSocket(t testing.TB, server *TCPServer, readBuffer int, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	addr := server.WebSocketAddr()
	if addr == nil {
		t.Fatal("the server has no WebSocket listener")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		var dialer net.Dialer
		connection, err := dialer.DialContext(ctx, network, address)
		if err == nil && readBuffer > 0 {
			_ = connection.(*net.TCPConn).SetReadBuffer(readBuffer)
		}
		return connection, err
	}}
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws://"+addr.String()+server.config.WebSocket.path(), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport}, HTTPHeader: header,
	})
}

func dialWebSocketClient(t testing.TB, server *TCPServer, readBuffer int) net.Conn {
	t.Helper()
	socket, _, err := dialWebSocket(t, server, readBuffer, nil)
	if err != nil {
		t.Fatal(err)
	}
	connection := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
	t.Cleanup(func() { _ = socket.CloseNow() })
	return connection
}

// dialAuthenticatedOver 经 transport 连上 server、发认证帧并读到 ack。
func dialAuthenticatedOver(t testing.TB, transport testTransport, server *TCPServer, token string) net.Conn {
	t.Helper()
	return authenticate(t, server, transport.dial(t, server, 0), token)
}

func authenticate(t testing.TB, server *TCPServer, connection net.Conn, token string) net.Conn {
	t.Helper()
	client := &tcpSession{connection: connection, writeTimeout: time.Second}
	if err := client.writeFrame(context.Background(), 0, 0, 1, []byte(token)); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	ack, release, err := server.readFrame(connection)
	if err != nil {
		t.Fatalf("authentication ack: %v", err)
	}
	release()
	if ack.messageID != 0 || ack.sequence != 1 {
		t.Fatalf("unexpected ack %+v", ack)
	}
	_ = connection.SetReadDeadline(time.Time{})
	return connection
}

// 流式会话不看消息边界：客户端把一帧拆成几次写（WebSocket 上是几条消息、TCP 上是几个报文段），或把几帧合成一次写
// （一条消息），服务端都按帧读出、按序应答。
func TestFramesSplitAcrossWritesOrMergedIntoOneAreRead(t *testing.T) {
	forEachTransport(t, func(t *testing.T, transport testTransport) {
		server := dispatchServer(t, "1s", func(_ *tcpTestContext, payload string) (string, error) {
			return "echo:" + payload, nil
		}, transport.configure)
		connection := dialAuthenticatedOver(t, transport, server, "split")
		encode := func(sequence uint32, payload string) []byte {
			var buffer bytes.Buffer
			if err := wire.Write(&buffer, []*wire.Packet{{MsgID: stuckRequestID, Seq: sequence, Payload: []byte(payload)}}, 1024); err != nil {
				t.Fatal(err)
			}
			return buffer.Bytes()
		}
		expect := func(sequence uint32, payload string) {
			t.Helper()
			_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
			got, release, err := server.readFrame(connection)
			if err != nil {
				t.Fatalf("answer to %d: %v", sequence, err)
			}
			defer release()
			if got.messageID != stuckResponseID || got.sequence != sequence || string(got.payload) != "echo:"+payload {
				t.Fatalf("answer to %d = id %d seq %d %q", sequence, got.messageID, got.sequence, got.payload)
			}
		}

		// 一帧拆成三次写：帧头的前 5 字节、帧头剩下的部分加载荷的前 2 字节、载荷剩下的部分。
		split := encode(2, "split-frame")
		for _, piece := range [][]byte{split[:5], split[5 : wire.HeaderSize+2], split[wire.HeaderSize+2:]} {
			if _, err := connection.Write(piece); err != nil {
				t.Fatal(err)
			}
		}
		expect(2, "split-frame")

		// 三帧合在一次写里，最后一帧的尾巴放进下一次写。
		merged := append(append(encode(3, "first"), encode(4, "second")...), encode(5, "third")...)
		if _, err := connection.Write(merged[:len(merged)-3]); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Write(merged[len(merged)-3:]); err != nil {
			t.Fatal(err)
		}
		expect(3, "first")
		expect(4, "second")
		expect(5, "third")
	})
}

// 服务端写给 WebSocket 客户端的每一帧恰好是一条二进制消息（帧头 + 载荷），浏览器按消息取帧，不用自己拼流。
func TestWebSocketServerWritesOneFramePerMessage(t *testing.T) {
	server := dispatchServer(t, "1s", func(_ *tcpTestContext, payload string) (string, error) {
		return "echo:" + payload, nil
	}, withWebSocket)
	socket, _, err := dialWebSocket(t, server, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	send := func(sequence, messageID uint32, payload string) {
		t.Helper()
		var buffer bytes.Buffer
		if err := wire.Write(&buffer, []*wire.Packet{{MsgID: messageID, Seq: sequence, Payload: []byte(payload)}}, 1024); err != nil {
			t.Fatal(err)
		}
		if err := socket.Write(ctx, websocket.MessageBinary, buffer.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() *wire.Packet {
		t.Helper()
		kind, message, err := socket.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.MessageBinary {
			t.Fatalf("message type %v, want binary", kind)
		}
		packet, err := wire.Read(bytes.NewReader(message), 1024)
		if err != nil {
			t.Fatalf("message is not one frame: %v (%d bytes)", err, len(message))
		}
		if size := wire.HeaderSize + len(packet.Payload); size != len(message) {
			t.Fatalf("message carries %d bytes, its frame %d: one message must hold exactly one frame", len(message), size)
		}
		return packet
	}
	send(1, 0, "ticket")
	if ack := receive(); ack.MsgID != 0 || ack.Seq != 1 || len(ack.Payload) != 0 {
		t.Fatalf("ack = %+v", ack)
	}
	send(2, stuckRequestID, "hello")
	if reply := receive(); reply.MsgID != stuckResponseID || reply.Seq != 2 || string(reply.Payload) != "echo:hello" {
		t.Fatalf("reply = %+v", reply)
	}
}

// Origin 白名单（wsconn.Config.AllowedOrigins）：为空时拒绝所有带 Origin 的浏览器请求，连同源页面也拒绝；不带 Origin
// 的原生客户端放行；配了白名单只放行匹配的来源。被拒的升级得到 403、不占连接槽、计入 reason=origin。
func TestWebSocketOriginIsCheckedAgainstTheAllowList(t *testing.T) {
	rejected := func() int64 { return labeledCounter("player_tcp_websocket_rejected_total", "reason", "origin") }
	upgrade := func(server *TCPServer, origin string) (int, error) {
		t.Helper()
		header := http.Header{}
		if origin != "" {
			header.Set("Origin", origin)
		}
		socket, response, err := dialWebSocket(t, server, 0, header)
		if err == nil {
			_ = socket.CloseNow()
			return http.StatusSwitchingProtocols, nil
		}
		if response == nil {
			return 0, err
		}
		return response.StatusCode, err
	}

	closed := dispatchServer(t, "1s", func(*tcpTestContext, string) (string, error) { return "", nil }, withWebSocket)
	sameHost := "http://" + closed.WebSocketAddr().String()
	before := rejected()
	for _, origin := range []string{"https://game.example.com", sameHost} {
		if status, err := upgrade(closed, origin); status != http.StatusForbidden {
			t.Errorf("empty allow list: Origin %q got status %d (%v), want 403", origin, status, err)
		}
	}
	if got := rejected() - before; got != 2 {
		t.Errorf("origin rejections counted = %d, want 2", got)
	}
	if status, err := upgrade(closed, ""); status != http.StatusSwitchingProtocols {
		t.Errorf("no Origin header (native client): status %d (%v), want the upgrade", status, err)
	}

	open := dispatchServer(t, "1s", func(*tcpTestContext, string) (string, error) { return "", nil }, withWebSocket, func(c *TCPConfig) {
		c.WebSocket.AllowedOrigins = "game.example.com, https://*.cdn.example.com"
	})
	for origin, want := range map[string]int{
		"https://game.example.com":      http.StatusSwitchingProtocols,
		"http://GAME.example.com":       http.StatusSwitchingProtocols,
		"https://eu.cdn.example.com":    http.StatusSwitchingProtocols,
		"http://eu.cdn.example.com":     http.StatusForbidden, // 带 scheme 的模式连 scheme 一起匹配
		"https://evil.example.com":      http.StatusForbidden,
		"https://game.example.com.evil": http.StatusForbidden,
		"null":                          http.StatusForbidden,
	} {
		if status, err := upgrade(open, origin); status != want {
			t.Errorf("allow list: Origin %q got status %d (%v), want %d", origin, status, err, want)
		}
	}
	waitReleased(t, open, 0)
}

// connectWebSocketThrough 从 127.0.0.1 带 header（X-Forwarded-For）拨 WebSocket 并认证；返回认证的结果。
// 按 IP 上限被拒的连接升级成功后在 acceptLoop 被关闭，认证读不到 ack。
func connectWebSocketThrough(t *testing.T, server *TCPServer, header http.Header, token string) error {
	t.Helper()
	socket, _, err := dialWebSocket(t, server, 0, header)
	if err != nil {
		return err
	}
	t.Cleanup(func() { _ = socket.CloseNow() })
	connection := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
	client := &tcpSession{connection: connection, writeTimeout: time.Second}
	if err := client.writeFrame(context.Background(), 0, 0, 1, []byte(token)); err != nil {
		return err
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	ack, release, err := server.readFrame(connection)
	if err != nil {
		return err
	}
	release()
	if ack.messageID != 0 || ack.sequence != 1 {
		return fmt.Errorf("unexpected ack %+v", ack)
	}
	return nil
}

// 七层负载均衡后的按 IP 上限（GATEWAY.md §6“部署注意”）：websocket.trusted_proxies 列出负载均衡时，同一个代理后面
// 不同的客户端地址各算各的，同一个客户端地址的第二条连接被拒；对端可信却没带 X-Forwarded-For 的连接退回按代理的地址
// 计数（更严），并计 player_tcp_websocket_forwarded_fallback_total；不配可信代理时请求头不起作用。
func TestWebSocketPerIPLimitCountsTheForwardedClientBehindATrustedProxy(t *testing.T) {
	perIP := func() int64 { return labeledCounter("player_tcp_connection_rejected_total", "reason", "per_ip") }
	fallback := func() int64 {
		return labeledCounter("player_tcp_websocket_forwarded_fallback_total", "reason", wsconn.FallbackMissing)
	}
	noop := func(*tcpTestContext, string) (string, error) { return "", nil }
	limitOne := func(trusted string) func(*TCPConfig) {
		return func(c *TCPConfig) { c.MaxConnectionsPerIP, c.WebSocket.TrustedProxies = 1, trusted }
	}
	through := func(client string) http.Header { return http.Header{"X-Forwarded-For": {client + ", 127.0.0.1"}} }

	behindProxy := dispatchServer(t, "1s", noop, withWebSocket, limitOne("127.0.0.1/32"))
	for _, client := range []string{"1.1.1.1", "2.2.2.2"} {
		if err := connectWebSocketThrough(t, behindProxy, through(client), "client-"+client); err != nil {
			t.Fatalf("client %s behind the trusted proxy, limit 1 per IP: %v", client, err)
		}
	}
	before := perIP()
	if err := connectWebSocketThrough(t, behindProxy, through("1.1.1.1"), "client-1.1.1.1-again"); err == nil {
		t.Fatal("a second connection of client 1.1.1.1 was accepted over the per-IP limit of 1")
	}
	if got := perIP() - before; got != 1 {
		t.Errorf("per-IP rejections counted = %d, want 1", got)
	}
	waitReleased(t, behindProxy, 2)

	// 对端可信、请求头缺失：两条都按代理的地址计数，第二条被拒；两次退回都计指标。
	noHeader := dispatchServer(t, "1s", noop, withWebSocket, limitOne("127.0.0.1/32"))
	beforeFallback := fallback()
	if err := connectWebSocketThrough(t, noHeader, nil, "first"); err != nil {
		t.Fatalf("first connection without X-Forwarded-For: %v", err)
	}
	if err := connectWebSocketThrough(t, noHeader, nil, "second"); err == nil {
		t.Fatal("a second connection without X-Forwarded-For was not counted on the proxy's address")
	}
	if got := fallback() - beforeFallback; got != 2 {
		t.Errorf("forwarded fallbacks counted = %d, want 2", got)
	}
	waitReleased(t, noHeader, 1)

	// 不配可信代理：请求头不起作用，不同的 X-Forwarded-For 仍是同一个对端地址。
	untrusted := dispatchServer(t, "1s", noop, withWebSocket, limitOne(""))
	if err := connectWebSocketThrough(t, untrusted, through("1.1.1.1"), "first"); err != nil {
		t.Fatal(err)
	}
	if err := connectWebSocketThrough(t, untrusted, through("2.2.2.2"), "second"); err == nil {
		t.Fatal("without trusted proxies a client-supplied X-Forwarded-For escaped the per-IP limit")
	}
	waitReleased(t, untrusted, 1)
}

// 单条消息的上限是一个最大帧（max_payload_bytes + 帧头）：恰好一个最大帧的消息照常处理；超出的消息在读过上限时
// 让连接以 1009 关闭。库按流读消息、不预先知道消息长度，所以上限之内已经读完的帧照常处理，越过上限的部分不会交给帧读取。
func TestWebSocketMessageLargerThanOneFrameIsRefused(t *testing.T) {
	const maxPayload = 1024
	server := dispatchServer(t, "1s", func(_ *tcpTestContext, payload string) (string, error) {
		return "ok", nil
	}, withWebSocket, func(c *TCPConfig) { c.MaxPayloadBytes = maxPayload })
	// 客户端的读写 context 在测试结束前不能取消：coder/websocket 在读写用过的 context 取消时关闭整条连接。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session := func(token string) *websocket.Conn {
		t.Helper()
		socket, _, err := dialWebSocket(t, server, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = socket.CloseNow() })
		var buffer bytes.Buffer
		_ = wire.Write(&buffer, []*wire.Packet{{Seq: 1, Payload: []byte(token)}}, 1024)
		if err := socket.Write(ctx, websocket.MessageBinary, buffer.Bytes()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := socket.Read(ctx); err != nil {
			t.Fatalf("ack: %v", err)
		}
		return socket
	}
	frameOf := func(payload int) []byte {
		var buffer bytes.Buffer
		if err := wire.Write(&buffer, []*wire.Packet{{MsgID: stuckRequestID, Seq: 2, Payload: bytes.Repeat([]byte{'x'}, payload)}}, maxPayload); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}

	largest := session("largest") // 会话号各不相同：同一会话号的新连接会顶掉旧连接
	if err := largest.Write(ctx, websocket.MessageBinary, frameOf(maxPayload)); err != nil {
		t.Fatal(err)
	}
	if _, reply, err := largest.Read(ctx); err != nil || !bytes.HasSuffix(reply, []byte("ok")) {
		t.Fatalf("a message of exactly one largest frame: reply %q, %v", reply, err)
	}

	oversized := session("oversized")
	// 一个合法的最大帧后面跟着第二帧：每帧都没超，消息超了。第二帧越过上限，不能得到应答。
	second := frameOf(8)
	binary.BigEndian.PutUint32(second[8:12], 3) // 第二帧的序号是 3（RS v2 帧头 [8:12]）
	if err := oversized.Write(ctx, websocket.MessageBinary, append(frameOf(maxPayload), second...)); err != nil {
		t.Fatal(err)
	}
	replies := 0
	for {
		_, reply, err := oversized.Read(ctx)
		if err != nil {
			if status := websocket.CloseStatus(err); status != websocket.StatusMessageTooBig {
				t.Fatalf("after an oversized message the client read %v (close status %v), want close 1009", err, status)
			}
			break
		}
		if packet, _ := wire.Read(bytes.NewReader(reply), maxPayload); packet != nil && packet.Seq == 3 {
			t.Fatal("the frame past the message limit was answered")
		}
		replies++
	}
	if replies > 1 {
		t.Fatalf("%d replies to an oversized message, want at most the one frame inside the limit", replies)
	}
	waitReleased(t, server, 1)
}

// WebSocket 监听的配置按键校验，监听随 Stop 关闭（之后拨号失败）。
func TestWebSocketConfigIsValidatedAndTheListenerStopsWithTheServer(t *testing.T) {
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:7000"
	config.WebSocket = WebSocketConfig{Addr: "127.0.0.1:7000", Path: "ws", AllowedOrigins: "ok.example.com, [", TrustedProxies: "10.0.0.0/8, 0.0.0.0/0"}
	err := ValidateTCPConfig(config)
	for _, want := range []string{"websocket.addr", "websocket.path", "websocket.allowed_origins", "websocket.trusted_proxies"} {
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Errorf("validation error %v does not name %s", err, want)
		}
	}
	// 可信代理写成全零前缀等于相信任何人自己填的头：单独写错这一项也被拒。
	config.WebSocket = WebSocketConfig{Addr: "127.0.0.1:7001", TrustedProxies: "::/0"}
	if err := ValidateTCPConfig(config); err == nil || !strings.Contains(err.Error(), "websocket.trusted_proxies") {
		t.Errorf("trusted_proxies ::/0: %v, want a refusal naming websocket.trusted_proxies", err)
	}
	config.WebSocket = WebSocketConfig{Path: "ws", AllowedOrigins: "["} // 没配地址：不开，路径与来源不检查
	if err := ValidateTCPConfig(config); err != nil {
		t.Fatalf("no websocket address: %v", err)
	}
	// trusted_proxies 写了就检查，监听没开也一样；生成的配置声明经 WebSocketConfig.Validate 在 --check-config 里同样拒绝。
	config.WebSocket = WebSocketConfig{TrustedProxies: "0.0.0.0/0"}
	if err := ValidateTCPConfig(config); err == nil || !strings.Contains(err.Error(), "player_access.tcp.websocket.trusted_proxies") {
		t.Errorf("trusted_proxies 0.0.0.0/0 without a websocket address: %v, want a refusal", err)
	}
	if err := config.WebSocket.Validate("player_access.gate.websocket."); err == nil || !strings.Contains(err.Error(), "player_access.gate.websocket.trusted_proxies") {
		t.Errorf("WebSocketConfig.Validate names the section it is given: %v", err)
	}

	server := dispatchServer(t, "1s", func(*tcpTestContext, string) (string, error) { return "", nil }, withWebSocket)
	addr := server.WebSocketAddr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if server.WebSocketAddr() != nil {
		t.Error("WebSocketAddr after Stop is not nil")
	}
	if connection, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = connection.Close()
		t.Fatalf("the WebSocket port %s still accepts after Stop", addr)
	}
}

// stalledWebSocketSession 是一条真实 WebSocket 连接服务端一侧的会话（wsconn.Conn），客户端连上后不读：
// 缓冲写满之后每次写都卡在 socket 上。readBuffer > 0 时把客户端 socket 的接收缓冲设成这么大（很快就塞满）；
// 之后还要读完的用例传 0 用缺省缓冲——Linux 在连接建立时按设的大小钳住接收窗口，4 KiB 的窗口小于一个 MSS，
// 读起来一秒只有几十 KB。返回的客户端连接供用例之后再开始读。会话的单帧上限是 32 MiB。
func stalledWebSocketSession(t *testing.T, writeTimeout time.Duration, readBuffer int) (*tcpSession, *websocket.Conn) {
	t.Helper()
	listener, err := wsconn.Listen("127.0.0.1:0", wsconn.Config{Path: "/ws", MaxMessageBytes: 1 << 21, HandshakeTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		var dialer net.Dialer
		connection, err := dialer.DialContext(ctx, network, address)
		if err == nil && readBuffer > 0 {
			_ = connection.(*net.TCPConn).SetReadBuffer(readBuffer)
		}
		return connection, err
	}}
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws://"+listener.Addr().String()+"/ws", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	select {
	case conn := <-accepted:
		t.Cleanup(func() { _ = conn.Close() })
		return &tcpSession{connection: conn, writeTimeout: writeTimeout, maxPayloadBytes: 32 << 20}, client
	case <-time.After(2 * time.Second):
		t.Fatal("the upgraded connection never reached Accept")
		return nil, nil
	}
}

// 会话关闭（慢连接断开、停机）立即打断 WebSocket 连接上阻塞的写，不等一分钟的写期限：tcpSession.close 先关连接再取写锁，
// wsconn.Conn.Close 立即关闭底层 socket。
func TestWebSocketSessionCloseInterruptsABlockedWrite(t *testing.T) {
	session, _ := stalledWebSocketSession(t, time.Minute, 4<<10)
	payload := bytes.Repeat([]byte{1}, 512<<10)
	written := make(chan error, 1)
	go func() {
		for {
			if err := session.WritePacket(context.Background(), 101, 0, wire.PayloadProtobuf, true, payload); err != nil {
				written <- err
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-written:
		t.Fatalf("writes to a client that reads nothing returned before Close: %v", err)
	default:
	}
	closed := make(chan error, 1)
	go func() { closed <- session.Close(ErrSessionClosed) }()
	for name, done := range map[string]chan error{"Close": closed, "write": written} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s still blocked 1s after Close; the write deadline is a minute away", name)
		}
	}
}

// 调用方期限（独立 Gate 的单帧发送预算）在 WebSocket 写的过程中到期：库已关闭整条连接，这一写报告连接坏了
// （errConnectionBroken），不能当成“什么也没写、连接还在”的拒绝。慢连接因此被断开，与 TCP 上写到一半的结果相同。
func TestACallerDeadlineThatEndsAWebSocketWriteBreaksTheConnection(t *testing.T) {
	session, _ := stalledWebSocketSession(t, time.Minute, 4<<10)
	payload := bytes.Repeat([]byte{1}, 512<<10)
	for attempt := 0; ; attempt++ {
		if attempt == 256 {
			t.Fatal("128MiB of writes never filled the stalled client's buffers")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := session.WritePacket(ctx, 101, 0, wire.PayloadProtobuf, true, payload)
		cancel()
		if err == nil {
			continue
		}
		if !errors.Is(err, errConnectionBroken) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a write cut off by the caller's deadline mid-message returned %v; want errConnectionBroken (the library closed the connection)", err)
		}
		return
	}
}

// 上一帧的期限（这里是独立 Gate 推送的单帧发送预算，调用方期限）在两帧之间到期、下一帧恰在那时开始写：下一帧有自己的
// 期限，不能被上一帧的期限断开（TCP 上 SetWriteDeadline 换期限即生效，旧期限不再作用）。修复前 wsconn 把期限原样交给
// coder/websocket 的 NetConn，它的期限计时器一直挂着，到期回调晚于新一帧拿到写锁时把新一帧当成“写在期限处未完成”，
// 关掉整条连接——relay 成员在心跳整 10s 点被断开是同一个原因（wsconn 的 TestAStaleWriteDeadlineDoesNotCloseALaterWrite）。
// 调度次序的固定方法见那个用例：GOMAXPROCS(1)、占住处理器跨过两个到期点、由唤醒计时器把测试 goroutine 排在回调之前；
// “写真的卡住”也照那个用例作为前提显式检查（观察到写已开始才读，写不经阻塞就写完按前提不成立失败），客户端用缺省接收
// 缓冲、靠 32 MiB 的帧塞满两端缓冲（v1.54.0 审查 RV-20261118-04 的同类问题：原来 4 KiB 缓冲加 1 MiB，Linux 上 5 秒内
// 只读到约 25 万字节就读超时）。-race 下 runnext 随机化，修复撤掉时只部分轮次变红。
func TestAnEarlierFramesDeadlineDoesNotBreakTheNextWebSocketWrite(t *testing.T) {
	session, client := stalledWebSocketSession(t, time.Minute, 0)
	observed := &writeObservedConn{Conn: session.connection.(*wsconn.Conn)}
	session.connection = observed
	big := bytes.Repeat([]byte{2}, 32<<20)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	stale := time.Now().Add(3 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), stale)
	err := session.WritePacket(ctx, 101, 0, wire.PayloadProtobuf, true, []byte("small"))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	wake := make(chan struct{})
	time.AfterFunc(time.Until(stale)+time.Millisecond, func() { close(wake) })
	for time.Now().Before(stale.Add(2 * time.Millisecond)) { // 不让出处理器；总时长远小于 10ms 的异步抢占
	}
	<-wake

	var written atomic.Bool
	read := make(chan error, 1)
	go func() {
		for !observed.writing.Load() { // 同步条件：写已开始，且测试 goroutine 卡在写里
			if written.Load() {
				read <- errors.New("precondition: the frame was written without blocking, so the stale deadline was never exercised")
				return
			}
			runtime.Gosched()
		}
		runtime.Gosched() // 让排在前面的旧期限回调先跑（修复前它在这里关掉连接）
		readCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		client.SetReadLimit(64 << 20) // 客户端缺省只收 32KiB 的消息
		for _, want := range []int{wire.HeaderSize + len("small"), wire.HeaderSize + len(big)} {
			if _, message, err := client.Read(readCtx); err != nil || len(message) != want {
				read <- fmt.Errorf("client read a %d-byte message, %v; want %d bytes", len(message), err, want)
				return
			}
		}
		read <- nil
	}()
	err = session.WritePacket(context.Background(), 101, 0, wire.PayloadProtobuf, true, big)
	written.Store(true)
	if err != nil {
		t.Fatalf("a frame written under write_timeout (a minute) failed: %v (the previous frame's deadline closed the connection); peer read: %v", err, <-read)
	}
	if err := <-read; err != nil {
		t.Fatalf("the frame was written but the peer did not get it whole: %v", err)
	}
}

// writeObservedConn 在 wsconn.Conn 外面记下“有一帧正在写”：测试据此观察写已开始。
type writeObservedConn struct {
	*wsconn.Conn
	writing atomic.Bool
}

func (c *writeObservedConn) WriteMessage(parts ...[]byte) (int64, error) {
	c.writing.Store(true)
	defer c.writing.Store(false)
	return c.Conn.WriteMessage(parts...)
}

// TCPConfig 必须保持可比较：生成工程的 TestDeclaredDefaultsAreDefaultConfig 用 != 比较整份配置，
// 所以 WebSocketConfig.AllowedOrigins 是逗号分隔的字符串而不是切片。这一行在它变得不可比较时编译失败。
var _ = TCPConfig{} == TCPConfig{}

// C# 客户端（client/dotnet/Roost.Client，Unity 托管路线同用）经 WebSocket 连嵌入 TCP 的玩家接入（维护者 2026-11-18）：
// 真实 C# 进程（ROOST_CLIENT_TEST_DLL，scripts/test-client-dotnet.sh 构建后设置；没设时跳过）用 ConnectionOptions.WebSocketUrl
// 连本用例的 TCPServer 的 WebSocket 监听，两种调度各跑一遍：缺省（线程池），以及 PumpDriven（Unity WebGL 的单线程调度：
// 单线程环境 + 像浏览器桥接那样只在轮询里交字节的传输，凡是线程池、计时器上的续行都记违规）。C# 一侧核对握手、请求应答、
// 校时控制帧、推送、心跳撑过 idle_timeout、服务端断开后自动重连（新连接的代号更大）、请求超时、PumpDriven 下不 Pump 就被
// idle_timeout 断开而恢复后重连回来、连接超时由 Pump 触发；本用例另核对服务端看到的连接代号非 0、同一会话重连后更大。
//
// 请求协议（stuckRequestID → stuckResponseID，载荷是文本）：gen 回本连接的代号；push:<x> 回 ok 并推一条 pushTestMessageID；
// close 回 closing 后关掉会话；slow 1.5s 后才回；其余原样回。
func TestTheCSharpClientWorksOverWebSocket(t *testing.T) {
	dll := csharpClientDLL(t)
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	withWebSocket(&config)
	config.IdleTimeout = time.Second
	config.DispatchTimeout = 5 * time.Second
	runtime := NewTCPRuntime(func(_ uint32, value any) ([]byte, error) { return value.([]byte), nil }, config.LoginTimeout)
	var mu sync.Mutex
	generations := map[string][]uint64{} // SessionID → 各条连接 gen 时的代号，按出现顺序
	dispatch := func(_ context.Context, session Session, msgID, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		if msgID != stuckRequestID || kind != wire.PayloadProtobuf {
			return nil, ErrInvalidRequest
		}
		text, reply := string(payload), string(payload)
		sessionID := session.Principal().SessionID
		switch {
		case text == "gen":
			generation := session.(ConnectionSession).Generation()
			mu.Lock()
			if list := generations[sessionID]; len(list) == 0 || list[len(list)-1] != generation {
				generations[sessionID] = append(list, generation)
			}
			mu.Unlock()
			reply = strconv.FormatUint(generation, 10)
		case strings.HasPrefix(text, "push:"):
			reply = "ok"
			go func() {
				time.Sleep(20 * time.Millisecond)
				if err := runtime.PushSession(context.Background(), sessionID, pushTestMessageID, []byte(strings.TrimPrefix(text, "push:"))); err != nil {
					t.Errorf("push to %s: %v", sessionID, err)
				}
			}()
		case text == "close":
			reply = "closing"
			go func() { time.Sleep(50 * time.Millisecond); _ = session.Close(errors.New("closed by the test")) }()
		case text == "slow":
			time.Sleep(1500 * time.Millisecond)
		}
		return &TCPResponse{MessageID: stuckResponseID, Sequence: seq, Payload: []byte(reply)}, nil
	}
	players := map[string]int64{"ws-threaded": 1, "ws-detector": 2, "ws-pump": 3}
	server, err := NewTCPServer(config, dispatch, AuthenticatorFunc(func(_ context.Context, token string, _ net.Addr) (Principal, error) {
		if players[token] == 0 {
			return Principal{}, errors.New("unknown test ticket")
		}
		return Principal{PlayerID: players[token], SessionID: token}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ConnectRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(ctx)
	})
	url := "ws://" + server.WebSocketAddr().String() + server.config.WebSocket.path()
	runCSharpClient(t, dll, "websocket", url, strconv.FormatUint(uint64(stuckRequestID), 10),
		strconv.FormatUint(uint64(stuckResponseID), 10), strconv.FormatUint(uint64(pushTestMessageID), 10))
	mu.Lock()
	defer mu.Unlock()
	for _, sessionID := range []string{"ws-threaded", "ws-pump"} {
		list := generations[sessionID]
		if len(list) < 2 {
			t.Errorf("%s: the server saw generations %v, want one per connection across a reconnect", sessionID, list)
			continue
		}
		for i, generation := range list {
			if generation == 0 || (i > 0 && generation <= list[i-1]) {
				t.Errorf("%s: connection generations %v are not non-zero and increasing", sessionID, list)
				break
			}
		}
	}
}

// 同一个 C# 客户端经 WebSocket 连独立 Gate（Gate → 进程内 NATS 替身 → Game Ingress）：PumpDriven 单线程调度下握手、
// 请求应答（Game 原样回）、心跳撑过 Gate 的 idle_timeout、不 Pump 被 Gate 断开后恢复 Pump 重连回来，全程零违规。
func TestTheCSharpClientWorksOverWebSocketThroughAStandaloneGate(t *testing.T) {
	dll := csharpClientDLL(t)
	broker := newLoopbackBroker()
	gateID := ProcessIdentity{ServerID: 11, Incarnation: "gate000000000001"}
	gameID := ProcessIdentity{ServerID: 22, Incarnation: "game000000000001"}
	principal := Principal{PlayerID: 7, ServerID: gameID.ServerID, SessionID: "ws-gate"}
	auth := AuthenticatorFunc(func(context.Context, string, net.Addr) (Principal, error) { return principal, nil })
	startStandaloneGame(t, broker, gameID, auth, nil)
	server := startStandaloneGate(t, broker, gateID, gameID, auth, withWebSocket, func(config *TCPConfig) { config.IdleTimeout = time.Second })
	url := "ws://" + server.WebSocketAddr().String() + server.config.WebSocket.path()
	runCSharpClient(t, dll, "websocket-gate", url, strconv.FormatUint(uint64(stuckRequestID), 10), strconv.FormatUint(uint64(stuckResponseID), 10))
}

// csharpClientDLL 返回构建好的 C# 测试程序（Roost.Client.Tests.dll）；没设 ROOST_CLIENT_TEST_DLL 或没有 dotnet 时跳过。
func csharpClientDLL(t *testing.T) string {
	t.Helper()
	dll := os.Getenv("ROOST_CLIENT_TEST_DLL")
	if dll == "" {
		t.Skip("ROOST_CLIENT_TEST_DLL not set (scripts/test-client-dotnet.sh builds the C# client and runs this test)")
	}
	if _, err := exec.LookPath("dotnet"); err != nil {
		t.Skip(".NET SDK unavailable")
	}
	return dll
}

// runCSharpClient 跑 C# 测试程序的一种模式（金样路径之后的参数），等它退出；非 0 退出时带上输出失败。
func runCSharpClient(t *testing.T, dll string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := exec.CommandContext(ctx, "dotnet", append([]string{dll, "../../../client/spec/packets.json"}, args...)...)
	client.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1")
	output, err := client.CombinedOutput()
	t.Logf("C# client %s:\n%s", args[0], output)
	if err != nil {
		t.Fatalf("the C# client (%s) failed: %v", args[0], err)
	}
}

// 不是回归用例：WebGL 冒烟（client/dotnet/unity-webgl-smoke 的 GateSmoke，serve-gate.mjs 起它）用的玩家接入对端。只在
// ROOST_GATEPEER_SERVE=1 时运行：缺省配置（DefaultTCPConfig，idle_timeout 等与生成工程的缺省相同）加 WebSocket 监听，放行
// 127.0.0.1 / localhost 的页面来源，任何票据都认、请求原样回；第一行标准输出是 GATEPEER {ws, idle_timeout_ms}，标准输入关闭时停。
// ROOST_GATEPEER_WS_ADDR 指定 WebSocket 监听地址（缺省 127.0.0.1:0）。
func TestServeAWebSocketPlayerAccessForTheBrowserSmoke(t *testing.T) {
	if os.Getenv("ROOST_GATEPEER_SERVE") != "1" {
		t.Skip("only for the WebGL smoke (client/dotnet/unity-webgl-smoke/serve-gate.mjs)")
	}
	config := DefaultTCPConfig()
	config.Addr = "127.0.0.1:0"
	config.WebSocket.Addr = "127.0.0.1:0"
	if addr := os.Getenv("ROOST_GATEPEER_WS_ADDR"); addr != "" {
		config.WebSocket.Addr = addr
	}
	config.WebSocket.AllowedOrigins = "127.0.0.1:*,localhost:*"
	dispatch := func(_ context.Context, _ Session, msgID, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		if kind != wire.PayloadProtobuf {
			return nil, ErrInvalidRequest
		}
		return &TCPResponse{MessageID: msgID, Sequence: seq, Payload: payload}, nil
	}
	var players atomic.Int64
	server, err := NewTCPServer(config, dispatch, AuthenticatorFunc(func(_ context.Context, token string, _ net.Addr) (Principal, error) {
		id := players.Add(1) // 每条连接一个玩家：页面刷新不顶号
		return Principal{PlayerID: id, SessionID: token + "-" + strconv.FormatInt(id, 10)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Stop(ctx)
	}()
	fmt.Printf("GATEPEER {\"ws\":%q,\"idle_timeout_ms\":%d}\n",
		"ws://"+server.WebSocketAddr().String()+server.config.WebSocket.path(), config.IdleTimeout.Milliseconds())
	_, _ = io.Copy(io.Discard, os.Stdin)
}
