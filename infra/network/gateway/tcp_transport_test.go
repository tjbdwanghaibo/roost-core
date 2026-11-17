package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
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
	config.WebSocket = WebSocketConfig{Addr: "127.0.0.1:7000", Path: "ws", AllowedOrigins: "ok.example.com, ["}
	err := ValidateTCPConfig(config)
	for _, want := range []string{"websocket.addr", "websocket.path", "websocket.allowed_origins"} {
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Errorf("validation error %v does not name %s", err, want)
		}
	}
	config.WebSocket = WebSocketConfig{} // 没配地址：不开，其余字段不检查
	if err := ValidateTCPConfig(config); err != nil {
		t.Fatalf("no websocket address: %v", err)
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
// 缓冲写满之后每次写都卡在 socket 上。
func stalledWebSocketSession(t *testing.T, writeTimeout time.Duration) *tcpSession {
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
		if err == nil {
			_ = connection.(*net.TCPConn).SetReadBuffer(4 << 10)
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
		return &tcpSession{connection: conn, writeTimeout: writeTimeout, maxPayloadBytes: 1 << 20}
	case <-time.After(2 * time.Second):
		t.Fatal("the upgraded connection never reached Accept")
		return nil
	}
}

// 会话关闭（慢连接断开、停机）立即打断 WebSocket 连接上阻塞的写，不等一分钟的写期限：tcpSession.close 先关连接再取写锁，
// wsconn.Conn.Close 立即关闭底层 socket。
func TestWebSocketSessionCloseInterruptsABlockedWrite(t *testing.T) {
	session := stalledWebSocketSession(t, time.Minute)
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
	session := stalledWebSocketSession(t, time.Minute)
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

// TCPConfig 必须保持可比较：生成工程的 TestDeclaredDefaultsAreDefaultConfig 用 != 比较整份配置，
// 所以 WebSocketConfig.AllowedOrigins 是逗号分隔的字符串而不是切片。这一行在它变得不可比较时编译失败。
var _ = TCPConfig{} == TCPConfig{}
