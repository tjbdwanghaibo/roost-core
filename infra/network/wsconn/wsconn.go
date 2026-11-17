// Package wsconn 是共用的 WebSocket 接入层（维护者 2026-11-17，lobby-relay 方案 §4.7.1、§6 第 9 条）：
// 用 coder/websocket 接受升级，把升级好的连接经 websocket.NetConn 包成 net.Conn，交给已有的流式会话处理。
// 玩家 TCP 接入（infra/network/gateway 的 TCPServer，嵌入 TCP 与独立 Gate 都是它）与 relay 共用这一个适配器：
// 它只做接入，握手、票据、路由、限额都是调用方那条 TCP 会话路径原样处理。
//
// 用法有两种，产出的都是 *Listener（一个 net.Listener）：
//   - Listen(address, config)：自己起一个 HTTP 服务，只在 config.Path 上接受升级；
//   - NewListener(config, addr)：不监听端口，把 *Listener 当 http.Handler 挂到调用方已有的 HTTP 服务上。
//
// 调用方照常 Accept → 服务连接 → Close。连接是字节流：客户端发来的一帧拆成几条消息、几帧合在一条消息里都能读；
// 服务端经 Conn.WriteMessage 写的一组字节是恰好一条二进制消息（gateway 用它把一个 RS v2 帧写成一条消息，
// 浏览器按消息取帧）。TLS 由前置负载均衡终结，这一层只说明文 ws。
package wsconn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Config 是一个 WebSocket 监听的接入规则。
type Config struct {
	// Path 是 Listen 起的 HTTP 服务接受升级的路径（精确匹配，别的路径 404）。NewListener 挂到调用方的路由上时，
	// 路径由调用方的路由决定，这里不检查。
	Path string
	// AllowedOrigins 是允许的浏览器页面来源（Origin 头）。规则：
	//   - 不带 Origin 的请求（原生客户端、服务端工具）总是放行：Origin 只防浏览器里的跨站页面，身份仍由首帧票据认证；
	//   - 带 Origin 的请求必须匹配列表里的一项，否则 403。列表为空就拒绝所有带 Origin 的请求，与请求 Host 相同的
	//     同源页面也不例外（失败即关闭：游戏页面通常与接入地址不同域，前置负载均衡还可能改写 Host，按 Host 判同源靠不住）。
	// 每一项按 path.Match 不分大小写匹配：不含 "://" 时匹配 Origin 的 host[:port]（如 "game.example.com"、
	// "*.example.com"），含 "://" 时匹配 scheme://host[:port]（如 "https://game.example.com"）。"*" 放行任何来源，
	// 只用于开发。
	AllowedOrigins []string
	// MaxMessageBytes 是客户端单条消息的字节上限，超出时读失败（websocket.ErrMessageTooBig）、连接以 1009（消息过大）
	// 关闭。库按流读消息、事先不知道消息长度：上限之内的字节照常交出，超出部分最多先交出 1 个字节（库为读到消息结束
	// 多放过一个字节），下一次读就失败。玩家接入取一个最大帧（max_payload_bytes + 帧头）。必须为正。
	MaxMessageBytes int64
	// HandshakeTimeout 限制升级请求：Listen 起的 HTTP 服务读请求头与回应答的期限，以及升级好的连接等调用方
	// Accept 的期限（过期就关闭这条连接）。必须为正。
	HandshakeTimeout time.Duration
	// Rejected 在一个升级请求被拒时调用（Origin 不允许时 err 包裹 ErrOriginRejected；不是合法的 WebSocket 升级）；
	// nil 时不通知。回调在请求的处理协程里执行，要快、不能阻塞（计数、记日志）。
	Rejected func(request *http.Request, err error)
	// TrustedProxies 是可信的前置代理（七层负载均衡）的地址，每项是 CIDR（10.0.0.0/8）或单个地址（10.0.0.5）。
	// 维护者 2026-11-17 定（交接 C3 选项 A）：
	//   - 为空（缺省）：完全不看请求头，Conn.RemoteAddr 是 TCP 对端。否则任何客户端都能自己带一个头冒充别的地址；
	//   - 直连对端不在列表里：不看请求头，用对端地址；
	//   - 直连对端在列表里：X-Forwarded-For 的各项从右往左看，跳过在列表里的，第一个不在列表里的就是客户端地址
	//     （最左边是客户端自己能填的，不能取）；取不出时退回对端地址并经 ForwardedFallback 报告；
	//   - 只认 X-Forwarded-For，Forwarded（RFC 7239）与 X-Real-IP 不看。
	// 全零前缀（0.0.0.0/0、::/0）等于相信任何人自己填的头，Validate 拒绝；IPv4 映射的 IPv6 前缀也拒绝（写成 IPv4）。
	TrustedProxies []string
	// ForwardedFallback 在直连对端是可信代理、却从 X-Forwarded-For 取不出客户端地址、退回对端地址时调用，reason 是
	// FallbackMissing / FallbackInvalid / FallbackAllTrusted 之一（低基数，可直接做指标标签）；nil 时不通知。
	// 回调在请求的处理协程里执行，要快、不能阻塞。
	ForwardedFallback func(request *http.Request, reason string)
}

// ForwardedFallback 的原因。
const (
	// FallbackMissing：没有 X-Forwarded-For，或它只有空白。
	FallbackMissing = "missing"
	// FallbackInvalid：从右往左剥到的某一项不是 IP 地址（空项、"unknown"、主机名等）。
	FallbackInvalid = "invalid"
	// FallbackAllTrusted：每一项都在可信列表里，没有不可信的客户端地址可取。
	FallbackAllTrusted = "all_trusted"
)

// forwardedForHeader 是唯一读的转发头（Config.TrustedProxies）。
const forwardedForHeader = "X-Forwarded-For"

// ValidateTrustedProxies 检查可信代理列表的每一项（写法见 Config.TrustedProxies），错误按项列出。
func ValidateTrustedProxies(entries []string) error {
	_, err := parseTrustedProxies(entries)
	return err
}

// parseTrustedProxies 把列表解析成前缀；单个地址按全长前缀（/32、/128）。
func parseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	var problems []error
	for _, entry := range entries {
		if strings.TrimSpace(entry) == "" || strings.ContainsAny(entry, " \t\r\n") {
			problems = append(problems, fmt.Errorf("wsconn: trusted proxy %q is empty or contains space", entry))
			continue
		}
		prefix, err := netip.ParsePrefix(entry)
		if err != nil && !strings.Contains(entry, "/") {
			var addr netip.Addr
			if addr, err = netip.ParseAddr(entry); err == nil {
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			}
		}
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("wsconn: trusted proxy %q is not a CIDR or an IP address: %w", entry, err))
		case prefix.Bits() == 0:
			problems = append(problems, fmt.Errorf("wsconn: trusted proxy %q trusts every address, so any client could forge X-Forwarded-For; list the load balancers' own addresses", entry))
		case prefix.Addr().Is4In6():
			problems = append(problems, fmt.Errorf("wsconn: trusted proxy %q is an IPv4-mapped IPv6 prefix; write it as IPv4", entry))
		default:
			prefixes = append(prefixes, prefix.Masked())
		}
	}
	return prefixes, errors.Join(problems...)
}

// forwardedClient 按 Config.TrustedProxies 的规则决定一个升级请求的客户端地址。client 无效（!IsValid）表示用 TCP
// 对端地址；fallback 非空表示对端是可信代理、却取不出客户端地址（Config.ForwardedFallback 的原因）。
func forwardedClient(request *http.Request, trusted []netip.Prefix) (client netip.Addr, fallback string) {
	if len(trusted) == 0 {
		return netip.Addr{}, "" // 规则 1：没配可信代理，请求头一律不看
	}
	peer, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil || !isTrusted(peer.Addr().Unmap(), trusted) {
		return netip.Addr{}, "" // 规则 2：对端不可信（或不是 IP 对端），不看请求头
	}
	var hops []string
	for _, value := range request.Header.Values(forwardedForHeader) {
		hops = append(hops, strings.Split(value, ",")...)
	}
	if strings.TrimSpace(strings.Join(hops, "")) == "" {
		return netip.Addr{}, FallbackMissing
	}
	// 规则 3：从右往左剥掉可信代理自己追加的项，第一个不可信的就是客户端。多行头按出现顺序拼接（RFC 7230 §3.2.2）。
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := parseHop(strings.TrimSpace(hops[i]))
		if !ok {
			return netip.Addr{}, FallbackInvalid
		}
		if !isTrusted(hop, trusted) {
			return hop, ""
		}
	}
	return netip.Addr{}, FallbackAllTrusted
}

// parseHop 解析 X-Forwarded-For 的一项：IP 地址，或带端口的 ip:port / [ipv6]:port（个别代理会带端口）。IPv4 映射的
// IPv6 地址还原成 IPv4，与 TCP 对端的写法一致，按 IP 计数时同一个客户端只有一个键。
func parseHop(value string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		addrPort, portErr := netip.ParseAddrPort(value)
		if portErr != nil {
			return netip.Addr{}, false
		}
		addr = addrPort.Addr()
	}
	return addr.Unmap().WithZone(""), true
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// Validate 检查配置；AllowedOrigins、TrustedProxies 的写法错误按项列出。
func (config Config) Validate() error {
	var problems []error
	if config.MaxMessageBytes <= 0 {
		problems = append(problems, fmt.Errorf("wsconn: max message bytes %d must be positive", config.MaxMessageBytes))
	}
	if config.HandshakeTimeout <= 0 {
		problems = append(problems, fmt.Errorf("wsconn: handshake timeout %v must be positive", config.HandshakeTimeout))
	}
	problems = append(problems, ValidatePath(config.Path), ValidateOrigins(config.AllowedOrigins), ValidateTrustedProxies(config.TrustedProxies))
	return errors.Join(problems...)
}

// ValidatePath 要求路径以 "/" 开头、不含查询串与空白。
func ValidatePath(value string) error {
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?# \t\r\n") {
		return fmt.Errorf("wsconn: path %q must start with / and carry no query, fragment or space", value)
	}
	return nil
}

// ValidateOrigins 检查来源白名单的每一项：非空、没有空白，是合法的 path.Match 模式。
func ValidateOrigins(patterns []string) error {
	var problems []error
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" || strings.ContainsAny(pattern, " \t\r\n") {
			problems = append(problems, fmt.Errorf("wsconn: allowed origin %q is empty or contains space", pattern))
			continue
		}
		if _, err := path.Match(pattern, ""); err != nil {
			problems = append(problems, fmt.Errorf("wsconn: allowed origin %q: %w", pattern, err))
		}
	}
	return errors.Join(problems...)
}

// originAllowed 按 Config.AllowedOrigins 的规则判断一个升级请求的来源。
func originAllowed(request *http.Request, patterns []string) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	for _, pattern := range patterns {
		target := parsed.Host
		if strings.Contains(pattern, "://") {
			target = parsed.Scheme + "://" + parsed.Host
		}
		if matched, _ := path.Match(strings.ToLower(pattern), strings.ToLower(target)); matched {
			return true
		}
	}
	return false
}

// ErrOriginRejected 是升级请求因 Origin 不在白名单被拒（403）时 Config.Rejected 收到的错误。
var ErrOriginRejected = errors.New("wsconn: origin not allowed")

// Listener 是一个 net.Listener：Accept 交出经 HTTP 升级、包成 *Conn 的 WebSocket 连接。它本身也是
// http.Handler（ServeHTTP 做升级）。升级成功的连接在 ServeHTTP 里等调用方 Accept，最多等 HandshakeTimeout。
type Listener struct {
	config  Config
	addr    net.Addr
	trusted []netip.Prefix // config.TrustedProxies 解析后的前缀
	// accepted 不带缓冲：ServeHTTP 交出连接的那一刻，调用方的 Accept 正在取，连接的所有权随之转给调用方。
	accepted chan *Conn
	done     chan struct{}

	mu       sync.Mutex
	closed   bool
	handlers sync.WaitGroup // 在途的 ServeHTTP；Close 等它们返回

	// server / served 只在 Listen 起的监听上有：Close 关闭它，再等 Serve 返回。
	server *http.Server
	served chan struct{}
}

// NewListener 返回不监听端口的 *Listener：调用方把它当 http.Handler 挂到自己的 HTTP 服务上，再把它当 net.Listener
// 交给会话的 accept 循环。addr 是 Addr() 报告的地址（通常是那个 HTTP 服务的监听地址），可以为 nil。
func NewListener(config Config, addr net.Addr) (*Listener, error) {
	if config.Path == "" {
		config.Path = "/" // 挂载模式不看路径；只为通过 Validate
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	trusted, _ := parseTrustedProxies(config.TrustedProxies) // Validate 已检查
	if addr == nil {
		addr = websocketAddr{}
	}
	return &Listener{config: config, addr: addr, trusted: trusted, accepted: make(chan *Conn), done: make(chan struct{})}, nil
}

// Listen 在 address 上起一个只接受 WebSocket 升级的 HTTP 服务（路径 config.Path，精确匹配）。
func Listen(address string, config Config) (*Listener, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	socket, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	listener, err := NewListener(config, socket.Addr())
	if err != nil {
		_ = socket.Close()
		return nil, err
	}
	upgradePath := config.Path
	listener.server = &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != upgradePath {
				http.NotFound(writer, request)
				return
			}
			listener.ServeHTTP(writer, request)
		}),
		// 期限只管升级请求本身：net/http 在 Hijack 时清除连接上的期限，升级后的读写期限由会话经 Conn 设置。
		ReadHeaderTimeout: config.HandshakeTimeout,
		ReadTimeout:       config.HandshakeTimeout,
		WriteTimeout:      config.HandshakeTimeout,
		IdleTimeout:       config.HandshakeTimeout,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}
	listener.served = make(chan struct{})
	go func() {
		defer close(listener.served)
		_ = listener.server.Serve(socket)
	}()
	return listener, nil
}

// ServeHTTP 检查 Origin、按可信代理规则定出客户端地址、完成升级，把连接交给 Accept。被拒的请求得到 HTTP 错误应答
// （403 / 426 等），不会出现在 Accept 里。
func (listener *Listener) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	listener.mu.Lock()
	if listener.closed {
		listener.mu.Unlock()
		http.Error(writer, "listener closed", http.StatusServiceUnavailable)
		return
	}
	listener.handlers.Add(1)
	listener.mu.Unlock()
	defer listener.handlers.Done()

	if !originAllowed(request, listener.config.AllowedOrigins) {
		listener.reject(request, fmt.Errorf("%w: %q", ErrOriginRejected, request.Header.Get("Origin")))
		http.Error(writer, "origin not allowed", http.StatusForbidden)
		return
	}
	// Origin 已在上面按本包的规则检查（列表为空时连同源页面也拒绝），这里关掉库自己的同源放行。压缩保持关闭（库的缺省）。
	socket, err := websocket.Accept(writer, request, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		listener.reject(request, err) // Accept 已经写了错误应答
		return
	}
	conn := newConn(socket, listener.config.MaxMessageBytes)
	client, fallback := forwardedClient(request, listener.trusted)
	if client.IsValid() {
		// 客户端端口不在 X-Forwarded-For 里，写 0；按 IP 计数与日志只用主机部分。
		conn.client = net.TCPAddrFromAddrPort(netip.AddrPortFrom(client, 0))
	} else if fallback != "" && listener.config.ForwardedFallback != nil {
		listener.config.ForwardedFallback(request, fallback)
	}
	timer := time.NewTimer(listener.config.HandshakeTimeout)
	defer timer.Stop()
	select {
	case listener.accepted <- conn:
	case <-listener.done:
		_ = conn.Close()
	case <-timer.C:
		_ = conn.Close()
	}
}

func (listener *Listener) reject(request *http.Request, err error) {
	if listener.config.Rejected != nil {
		listener.config.Rejected(request, err)
	}
}

// Accept 等下一条升级好的连接；Close 之后返回包裹 net.ErrClosed 的错误。
func (listener *Listener) Accept() (net.Conn, error) {
	select {
	case conn := <-listener.accepted:
		return conn, nil
	case <-listener.done:
		return nil, &net.OpError{Op: "accept", Net: "websocket", Addr: listener.addr, Err: net.ErrClosed}
	}
}

// Close 停止接受：之后到达的升级请求得到 503，正在等 Accept 的已升级连接被关闭；Listen 起的 HTTP 服务关闭监听。
// 返回前等在途的 ServeHTTP 与 Serve 返回。已经交给调用方的连接不受影响，由调用方关闭。重复调用返回 nil。
func (listener *Listener) Close() error {
	listener.mu.Lock()
	if listener.closed {
		listener.mu.Unlock()
		return nil
	}
	listener.closed = true
	close(listener.done)
	listener.mu.Unlock()
	var err error
	if listener.server != nil {
		err = listener.server.Close()
	}
	listener.handlers.Wait()
	if listener.served != nil {
		<-listener.served
	}
	return err
}

// Addr 是监听地址（Listen 的实际端口；NewListener 传入的地址）。
func (listener *Listener) Addr() net.Addr { return listener.addr }

var _ net.Listener = (*Listener)(nil)
var _ http.Handler = (*Listener)(nil)

type websocketAddr struct{}

func (websocketAddr) Network() string { return "websocket" }
func (websocketAddr) String() string  { return "websocket" }

// Conn 是一条升级好的 WebSocket 连接，读写经 websocket.NetConn（二进制消息）。与 *net.TCPConn 对齐的三点：
//
//   - 读写期限：SetReadDeadline / SetWriteDeadline 真的生效，到期的读写返回 errors.Is(err, os.ErrDeadlineExceeded)
//     的错误。区别在于期限在一次读写进行中到期时，coder/websocket 取消它并关闭整条连接（TCP 只打断这一次）；
//     这种错误同时包裹 net.ErrClosed，调用方据此知道连接已经不能用，不要当成“什么也没写、连接还在”的拒绝。
//     期限在读写开始之前就已过去时立即拒绝、什么也不做，连接仍可用，错误不包裹 net.ErrClosed。
//     与 TCP 一样，换期限即生效：旧期限不再作用于之后的读写。为此期限只在一次读写进行期间交给 NetConn（见 deadline）。
//   - Close 立即关闭底层 socket（websocket.Conn.CloseNow），打断阻塞中的读写，不做 WebSocket 关闭握手、不等对端：
//     与关闭 TCP socket 一样，浏览器看到 1006。会话的慢连接断开与停机依赖这一点。
//   - RemoteAddr 是客户端地址：直连对端是 Config.TrustedProxies 里的代理时，是从 X-Forwarded-For 取出的地址
//     （端口为 0）；否则（没配可信代理、对端不可信、取不出时退回）是 TCP 对端地址。PeerAddr 总是 TCP 对端地址，
//     日志里两者一起写，经负载均衡时能看出是哪个代理转来的。
type Conn struct {
	net.Conn // websocket.NetConn
	socket   *websocket.Conn
	// client 是 RemoteAddr 报告的转发客户端地址；nil 表示用 TCP 对端地址。升级时写一次，之后只读。
	client net.Addr

	read, write deadline // 两个方向各自的期限；读写都经它们进行

	closeOnce sync.Once
	closeErr  error
}

func newConn(socket *websocket.Conn, maxMessageBytes int64) *Conn {
	stream := websocket.NetConn(context.Background(), socket, websocket.MessageBinary)
	// NetConn 把读上限设成 -1（不限），这里按配置重新设：单条消息超出时库以 1009 关闭连接，读返回错误。
	socket.SetReadLimit(maxMessageBytes)
	conn := &Conn{Conn: stream, socket: socket}
	conn.read.apply, conn.write.apply = stream.SetReadDeadline, stream.SetWriteDeadline
	return conn
}

// RemoteAddr 是客户端地址（见类型注释）。
func (conn *Conn) RemoteAddr() net.Addr {
	if conn.client != nil {
		return conn.client
	}
	return conn.Conn.RemoteAddr()
}

// PeerAddr 是 TCP 对端地址：前置七层负载均衡时是负载均衡的地址。
func (conn *Conn) PeerAddr() net.Addr { return conn.Conn.RemoteAddr() }

func (conn *Conn) Read(buffer []byte) (int, error) {
	return conn.read.do(func() (int, error) { return conn.Conn.Read(buffer) })
}

// Write 把 buffer 写成一条二进制消息。
func (conn *Conn) Write(buffer []byte) (int, error) {
	return conn.write.do(func() (int, error) { return conn.Conn.Write(buffer) })
}

// WriteMessage 把 parts 依次拼成恰好一条二进制消息写出（gateway 写一帧：帧头 + 载荷）。多段时先复制进一块缓冲：
// NetConn 一次 Write 就是一条消息，没有分段写同一条消息的入口。返回写出的字节数；出错时为 0（NetConn 不报告部分写出）。
func (conn *Conn) WriteMessage(parts ...[]byte) (int64, error) {
	if len(parts) == 1 {
		n, err := conn.Write(parts[0])
		return int64(n), err
	}
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	buffer := takeBuffer(total)
	for _, part := range parts {
		*buffer = append(*buffer, part...)
	}
	n, err := conn.Write(*buffer)
	putBuffer(buffer)
	return int64(n), err
}

func (conn *Conn) SetDeadline(at time.Time) error {
	conn.read.set(at)
	conn.write.set(at)
	return nil
}

func (conn *Conn) SetReadDeadline(at time.Time) error {
	conn.read.set(at)
	return nil
}

func (conn *Conn) SetWriteDeadline(at time.Time) error {
	conn.write.set(at)
	return nil
}

// Close 见类型注释。第一次调用返回关闭的结果（已被对端关闭也算成功），之后的调用返回包裹 net.ErrClosed 的错误。
func (conn *Conn) Close() error {
	first := false
	conn.closeOnce.Do(func() {
		first = true
		err := conn.socket.CloseNow()
		// NetConn.Close 停掉它的期限计时器、取消读写 context；底层连接已关，它不会再等关闭握手。
		_ = conn.Conn.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			conn.closeErr = err
		}
	})
	if !first {
		return fmt.Errorf("wsconn: close: %w", net.ErrClosed)
	}
	return conn.closeErr
}

// deadline 是一个方向（读或写）的期限。coder/websocket 的 NetConn 用一个计时器实现期限，到期回调在自己的 goroutine 里
// 判断“这时有没有读写在进行”：有就取消它并关闭整条连接，没有就让之后的读写失败。计时器若一直挂着，就会出两种错
// （2026-11-17，relay 成员在心跳整 10s 点被断开）：
//   - 旧期限误杀新的读写：调用方每次写前设 now+期限、写完不清（relay 的 writeLoop、Gate 的会话写）。下一次写恰在上一次
//     期限的到期点开始时，计时器已触发、回调还没跑，新期限的 Reset 拦不住它；新的写先拿到锁，回调就把它当成“写在期限处
//     未完成”，关掉连接。读一侧看到 use of closed network connection，写一侧可能毫无错误。
//   - 已过的期限：NetConn 把它当成 1ns 后到期，紧接着的写要么照样写出，要么被回调关掉连接，而不是 TCP 那样立即拒绝。
//
// 所以期限只在一次读写进行期间交给 NetConn：开始时已过期就立即拒绝（不碰 NetConn）；否则把期限设上、读写、再清掉。
// 读写进行期间调用方换期限，照 net.Conn 的约定立即作用于这次读写。剩下的边界：一次读写恰好在期限处结束、回调已触发而
// 还没跑时，它仍可能关掉随后的那次读写——那次期限确实到了，与“读写在期限处未完成”同样处理。
type deadline struct {
	op    sync.Mutex // 同一方向一次一个读写：设期限 → 读写 → 清期限不能与另一次交错（NetConn 本来也逐个执行）
	mu    sync.Mutex // 保护 at、busy，并与设期限互斥
	at    time.Time  // 零值表示没有期限
	busy  bool       // 有读写在进行，期限已交给 NetConn
	apply func(time.Time) error
}

func (d *deadline) set(at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at = at
	if d.busy {
		_ = d.apply(at)
	}
}

// do 在期限之内执行一次读写 op。
func (d *deadline) do(op func() (int, error)) (int, error) {
	d.op.Lock()
	defer d.op.Unlock()
	d.mu.Lock()
	at := d.at
	if !at.IsZero() && !time.Now().Before(at) {
		d.mu.Unlock()
		return 0, fmt.Errorf("wsconn: %w before the read or write started", os.ErrDeadlineExceeded)
	}
	d.busy = true
	_ = d.apply(at)
	d.mu.Unlock()

	n, err := op()

	d.mu.Lock()
	d.busy = false
	_ = d.apply(time.Time{}) // 停掉计时器：旧期限不再作用于之后的读写
	at = d.at
	d.mu.Unlock()
	if err != nil {
		err = deadlineError(err, at)
	}
	return n, err
}

// deadlineError 把期限到期造成的错误变成 errors.Is(err, os.ErrDeadlineExceeded)（见 Conn 的注释）。期限没到的错误原样返回。
func deadlineError(err error, at time.Time) error {
	if at.IsZero() || time.Now().Before(at) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// NetConn 在读写开始前发现期限已过（回调在没有读写时到期，见 deadline 的边界）：什么也没做，连接仍可用。
		return fmt.Errorf("%w: %w", os.ErrDeadlineExceeded, err)
	}
	// 期限在读写进行中到期：NetConn 取消了这次读写，coder/websocket 随之关闭整条连接。
	return fmt.Errorf("%w: the connection is closed: %w: %w", os.ErrDeadlineExceeded, net.ErrClosed, err)
}

// 写缓冲池：只回收不超过 64 KiB 的缓冲，大帧用完即弃，池里不常驻大块内存。
const pooledBufferBytes = 64 << 10

var bufferPool = sync.Pool{New: func() any { buffer := make([]byte, 0, 4<<10); return &buffer }}

func takeBuffer(size int) *[]byte {
	if size > pooledBufferBytes {
		buffer := make([]byte, 0, size)
		return &buffer
	}
	buffer := bufferPool.Get().(*[]byte)
	*buffer = (*buffer)[:0]
	return buffer
}

func putBuffer(buffer *[]byte) {
	if cap(*buffer) <= pooledBufferBytes {
		bufferPool.Put(buffer)
	}
}
