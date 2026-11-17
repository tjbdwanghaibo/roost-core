package wsconn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testConfig() Config {
	return Config{Path: "/ws", MaxMessageBytes: 1 << 20, HandshakeTimeout: 2 * time.Second}
}

// pair 起一个 Listen 监听，拨一条客户端连接并 Accept 到服务端一侧。readBuffer > 0 时把客户端 socket 的接收缓冲设小
// （停止读取的慢客户端）。客户端读写用的 context 在测试结束前不取消：coder/websocket 在它取消时关闭整条连接。
func pair(t *testing.T, readBuffer int) (*Conn, *websocket.Conn) {
	t.Helper()
	listener, err := Listen("127.0.0.1:0", testConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	client := dial(t, "ws://"+listener.Addr().String()+"/ws", readBuffer, nil)
	select {
	case conn := <-accepted:
		t.Cleanup(func() { _ = conn.Close() })
		return conn.(*Conn), client
	case <-time.After(2 * time.Second):
		t.Fatal("the upgraded connection never reached Accept")
		return nil, nil
	}
}

func dial(t *testing.T, address string, readBuffer int, header http.Header) *websocket.Conn {
	t.Helper()
	client, _, err := dialResponse(t, address, readBuffer, header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	return client
}

func dialResponse(t *testing.T, address string, readBuffer int, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, addr)
		if err == nil && readBuffer > 0 {
			_ = conn.(*net.TCPConn).SetReadBuffer(readBuffer)
		}
		return conn, err
	}}
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}, HTTPHeader: header})
}

// 读期限真的生效：读在进行中到期时返回 os.ErrDeadlineExceeded，并包裹 net.ErrClosed（库关闭了整条连接）。
func TestReadDeadlineEndsABlockedRead(t *testing.T) {
	server, _ := pair(t, 0)
	started := time.Now()
	_ = server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := server.Read(make([]byte, 16))
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read past its deadline: %v, want os.ErrDeadlineExceeded and net.ErrClosed", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the read ended %v after a 100ms deadline", elapsed)
	}
}

// 期限在读写开始前就已过去：什么也不做、返回 os.ErrDeadlineExceeded，不包裹 net.ErrClosed；重设期限后连接照常可用
// （与 TCP 一致：调用方据此把它当作拒绝而不是断线）。
func TestAnAlreadyExpiredDeadlineRefusesWithoutClosing(t *testing.T) {
	server, client := pair(t, 0)
	_ = server.SetWriteDeadline(time.Now().Add(-time.Second))
	time.Sleep(20 * time.Millisecond) // 让 NetConn 的计时器在没有写的时候到期
	if _, err := server.Write([]byte("refused")); !errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, net.ErrClosed) {
		t.Fatalf("write with an expired deadline: %v, want os.ErrDeadlineExceeded without net.ErrClosed", err)
	}
	_ = server.SetReadDeadline(time.Now().Add(-time.Second))
	time.Sleep(20 * time.Millisecond)
	if _, err := server.Read(make([]byte, 4)); !errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, net.ErrClosed) {
		t.Fatalf("read with an expired deadline: %v, want os.ErrDeadlineExceeded without net.ErrClosed", err)
	}

	_ = server.SetDeadline(time.Time{})
	if _, err := server.Write([]byte("after")); err != nil {
		t.Fatalf("write after clearing the deadline: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, message, err := client.Read(ctx); err != nil || string(message) != "after" {
		t.Fatalf("client read %q, %v", message, err)
	}
	if err := client.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(server, got); err != nil || string(got) != "ping" {
		t.Fatalf("server read %q, %v", got, err)
	}
}

// 写期限真的生效：对端停止读取、缓冲写满后，写在期限处失败（os.ErrDeadlineExceeded），不会挂住。
func TestWriteDeadlineEndsAWriteToAStalledPeer(t *testing.T) {
	server, _ := pair(t, 4<<10)
	chunk := bytes.Repeat([]byte{1}, 256<<10)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_ = server.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		started := time.Now()
		_, err := server.Write(chunk)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("write to a stalled peer failed with %v, want os.ErrDeadlineExceeded", err)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("the write ended %v after a 200ms deadline", elapsed)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("writes to a peer that reads nothing never hit the write deadline")
		}
	}
}

// Close 立即打断阻塞的写与读，不等写期限、不等对端回关闭帧。
func TestCloseInterruptsABlockedWriteAndRead(t *testing.T) {
	server, _ := pair(t, 4<<10)
	_ = server.SetWriteDeadline(time.Now().Add(time.Minute))
	writeDone := make(chan error, 1)
	go func() {
		chunk := bytes.Repeat([]byte{1}, 256<<10)
		for {
			if _, err := server.Write(chunk); err != nil {
				writeDone <- err
				return
			}
		}
	}()
	readDone := make(chan error, 1)
	go func() {
		_, err := server.Read(make([]byte, 16))
		readDone <- err
	}()
	time.Sleep(300 * time.Millisecond) // 停读的对端早已塞满缓冲，写此时卡在 socket 上
	select {
	case err := <-writeDone:
		t.Fatalf("the write to a stalled peer returned before Close: %v", err)
	default:
	}
	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	for name, done := range map[string]chan error{"Close": closed, "write": writeDone, "read": readDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s still blocked 1s after Close (write deadline a minute away)", name)
		}
	}
	if err := server.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second Close = %v, want net.ErrClosed", err)
	}
}

// WriteMessage 的各段恰好成为一条二进制消息；客户端发来的消息在服务端是一条字节流，消息边界不影响读取。
func TestWriteMessageIsOneMessageAndReadsAreAStream(t *testing.T) {
	server, client := pair(t, 0)
	if n, err := server.WriteMessage([]byte("head"), []byte("-"), []byte("payload")); err != nil || n != 12 {
		t.Fatalf("WriteMessage = %d, %v", n, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	kind, message, err := client.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || string(message) != "head-payload" {
		t.Fatalf("client read %v %q, %v; want one binary message", kind, message, err)
	}
	for _, part := range []string{"ab", "cdef", "g"} {
		if err := client.Write(ctx, websocket.MessageBinary, []byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	got := make([]byte, 7)
	if _, err := io.ReadFull(server, got); err != nil || string(got) != "abcdefg" {
		t.Fatalf("server read %q, %v", got, err)
	}
}

// 单条消息超过 MaxMessageBytes：读在越过上限时失败，连接以 1009 关闭。
func TestAMessageOverTheLimitClosesTheConnection(t *testing.T) {
	config := testConfig()
	config.MaxMessageBytes = 64
	listener, err := Listen("127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	client := dial(t, "ws://"+listener.Addr().String()+"/ws", 0, nil)
	server := <-accepted
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte{1}, 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(server, make([]byte, 64)); err != nil {
		t.Fatalf("a message of exactly the limit: %v", err)
	}
	// 库的读上限在越过上限之后的下一次读失败（它为读结束帧多放过一个字节），所以超出部分最多先交出 1 个字节。
	if err := client.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte{1}, 100)); err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadFull(server, make([]byte, 100))
	if read > 65 {
		t.Fatalf("read %d bytes of a 100-byte message with a 64-byte limit", read)
	}
	if !errors.Is(err, websocket.ErrMessageTooBig) {
		t.Fatalf("reading a message over the limit: %v, want ErrMessageTooBig", err)
	}
	if _, _, err := client.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("client saw %v, want close 1009", err)
	}
}

// Origin 规则：不带 Origin 放行；白名单为空时任何 Origin（含同源）都拒绝；配了白名单按 host 或 scheme://host 匹配。
func TestOriginRules(t *testing.T) {
	request := func(host, origin string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/ws", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	allow := []string{"game.example.com", "https://*.cdn.example.com", "localhost:*"}
	for _, check := range []struct {
		origin   string
		patterns []string
		want     bool
	}{
		{"", nil, true},
		{"http://gate.example.com", nil, false}, // 同源也拒绝：白名单为空
		{"https://game.example.com", allow, true},
		{"https://GAME.EXAMPLE.COM", allow, true},
		{"https://a.cdn.example.com", allow, true},
		{"http://a.cdn.example.com", allow, false},
		{"http://localhost:5173", allow, true},
		{"https://game.example.com.evil.net", allow, false},
		{"null", allow, false},
		{"://bad", allow, false},
	} {
		if got := originAllowed(request("gate.example.com", check.origin), check.patterns); got != check.want {
			t.Errorf("Origin %q with %v: allowed = %v, want %v", check.origin, check.patterns, got, check.want)
		}
	}
	if err := ValidateOrigins([]string{"ok.example.com", "[", ""}); err == nil {
		t.Fatal("bad patterns were accepted")
	}
}

// 挂载模式：NewListener 作为 http.Handler 挂到调用方的 HTTP 服务上。被拒的升级通知 Rejected；Close 之后 Accept
// 返回 net.ErrClosed，新的升级请求得到 503。
func TestMountedListenerHandsOverAndStops(t *testing.T) {
	rejected := make(chan error, 4)
	config := testConfig()
	config.AllowedOrigins = []string{"game.example.com"}
	config.Rejected = func(_ *http.Request, err error) { rejected <- err }
	listener, err := NewListener(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/relay", listener)
	server := httptest.NewServer(mux)
	defer server.Close()
	address := "ws" + server.URL[len("http"):] + "/relay"

	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			accepted <- conn
		}
	}()
	client := dial(t, address, 0, http.Header{"Origin": {"https://game.example.com"}})
	conn := <-accepted
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageBinary, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 2)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "hi" {
		t.Fatalf("read %q, %v", got, err)
	}
	_ = conn.Close()

	if _, response, err := dialResponse(t, address, 0, http.Header{"Origin": {"https://evil.example.com"}}); err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("a foreign Origin upgraded or got %v: %v", response, err)
	}
	select {
	case err := <-rejected:
		if !errors.Is(err, ErrOriginRejected) {
			t.Fatalf("Rejected got %v, want ErrOriginRejected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Rejected was not called for the foreign Origin")
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
	}
	if _, response, err := dialResponse(t, address, 0, nil); err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an upgrade after Close got %v: %v, want 503", response, err)
	}
}

// 升级好却没人 Accept 的连接最多等 HandshakeTimeout 就被关闭；Listen 的监听只接 config.Path。
func TestAnUpgradeNobodyAcceptsIsClosedAndOtherPathsAre404(t *testing.T) {
	config := testConfig()
	config.HandshakeTimeout = 200 * time.Millisecond
	listener, err := Listen("127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client := dial(t, "ws://"+listener.Addr().String()+"/ws", 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := client.Read(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("an upgrade nobody accepted stayed open: %v", err)
	}
	if _, response, err := dialResponse(t, "ws://"+listener.Addr().String()+"/other", 0, nil); err == nil || response == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("another path got %v: %v, want 404", response, err)
	}
}
