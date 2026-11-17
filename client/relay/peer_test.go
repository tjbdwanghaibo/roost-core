package relay_test

// 测试对端：在测试进程里起真实的 relay（service/relay）与 lobby（service/lobby），三份 SDK 的测试共用。
//
//   - Go 的用例直接调 startPeer；
//   - C# 与 TS 的用例把本包的测试二进制当对端进程用：go test -c -o <bin> ./client/relay，再以环境变量
//     ROOST_RELAYPEER_SERVE=1 运行它（TestMain 见下），它在 stdout 打一行地址 JSON，stdin 关闭或收到 SIGINT / SIGTERM 就退出：
//
//     {"lobby_url":"http://127.0.0.1:...","relay_tcp":"127.0.0.1:...","relay_ws":"ws://127.0.0.1:.../relay","backend":"redis","game":"party"}
//
//     ROOST_RELAYPEER_WS=0 时不开 WebSocket 监听（C# 测试用它核对 lobby 对 ws 答 no_relay）。ROOST_RELAYPEER_IDLE_MS=<毫秒>
//     把 relay 的 idle_timeout 改短（C# 的单线程用例用它在几秒内核对心跳保活与“不 Pump 就被断开、靠重连回来”）。测试拿 POST <lobby_url>/harness/game-token {"account":"a"}
//     换一张测试 gt2，再按 lobby 的正式接口建房、加入。POST <lobby_url>/harness/admission {"mode":"ok|reject|down"}
//     切换 relay 向 lobby 验权的结果（缺省 ok：照 lobby 的真实答复；reject：一律拒绝，relay 发 Closed{lobby_rejected}；
//     down：一律不可用，relay 发 Closed{lobby_unavailable}），用来测自动重连遇到这两种拒绝时停还是退避重试。
//
// 放在 _test.go 里：SDK 本身不依赖 Redis 与服务端包；对端要链接 Redis 驱动（infra/storage/redis/driver），按
// TestCoreContractsDoNotLinkDrivers 的约定，非装配层的代码里只有测试可以链接驱动。
//
// 和生产装配（wiring/relay、wiring/lobby）的差别只在三处外部依赖，换成进程内的等价物：
//   - account 的 ValidateGameToken 换成本对端签的测试 token（POST /harness/game-token）：lobby 本来就经
//     lobby.GameTokenValidator 这个最小接口校验，替身只替账号体系，不替 lobby 的鉴权逻辑；
//   - relay → lobby 的 NATS 报告换成进程内按序投递（同样经 lobby.HandleRelayEventData 的 JSON 解码）；
//     lobby → relay 的 leave 命令换成直接调 relay.Server.Leave；relay 每条连接向 lobby 的验权（NATS request）换成
//     直接调 lobby.HandleRelayVerifyData，答复按 wiring/relay 的 natsAdmission 同样的规则解释；
//   - etcd 的 relay 广播换成固定的一条（本对端的 relay，房间数取实时值）。
//
// 房间目录用真实 Redis（ROOST_REDIS_TEST_ADDR，再取 ROOST_DATAENGINE_IT_REDIS_ADDR，即 ~/.roost-it 的 env.sh）；
// 都没有时退回 lobby.MemoryRooms，backend 报告用的是哪个。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/wsconn"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/service/lobby"
	relaysvc "github.com/tjbdwanghaibo/roost-core/service/relay"
)

// TestMain：设了 ROOST_RELAYPEER_SERVE=1 时本测试二进制不跑用例，只当 C# / TS 测试的对端进程。
func TestMain(m *testing.M) {
	if os.Getenv("ROOST_RELAYPEER_SERVE") == "1" {
		os.Exit(servePeer())
	}
	os.Exit(m.Run())
}

func servePeer() int {
	cfg := peerConfig{RedisAddr: redisAddrFromEnv(), WebSocket: os.Getenv("ROOST_RELAYPEER_WS") != "0"}
	if ms := os.Getenv("ROOST_RELAYPEER_IDLE_MS"); ms != "" {
		n, err := strconv.Atoi(ms)
		if err != nil || n <= 0 {
			fmt.Fprintln(os.Stderr, "relaypeer: ROOST_RELAYPEER_IDLE_MS must be a positive number of milliseconds:", ms)
			return 1
		}
		cfg.Relay.IdleTimeout = time.Duration(n) * time.Millisecond
	}
	peer, err := startTestPeer(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "relaypeer:", err)
		return 1
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
		"lobby_url": peer.LobbyURL, "relay_tcp": peer.RelayTCP, "relay_ws": peer.RelayWS, "backend": peer.Backend, "game": peerGame,
	})
	stop := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); stop <- struct{}{} }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-signals; stop <- struct{}{} }()
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := peer.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "relaypeer: close:", err)
		return 1
	}
	return 0
}

func redisAddrFromEnv() string {
	if addr := os.Getenv("ROOST_REDIS_TEST_ADDR"); addr != "" {
		return addr
	}
	return os.Getenv("ROOST_DATAENGINE_IT_REDIS_ADDR")
}

// peerGame 是对端服务的游戏名；peerMaxMembers 是它的房间人数上限。
const (
	peerGame       = "party"
	peerMaxMembers = 8
	instance       = "sdk-peer"
	subject        = "roost.sdkpeer"
)

// peerConfig 是对端的设置。
type peerConfig struct {
	// RedisAddr 是房间目录的 Redis；空时用内存目录。
	RedisAddr string
	// WebSocket 为 true 时 relay 另开 WebSocket 监听（路径 /relay），lobby 的 transport=ws 拿到它的 ws:// 地址。
	WebSocket bool
	// Relay 覆盖 relay 的限额与超时（零值取 service/relay 的缺省）；Instance 与 TokenKey 由对端填。
	Relay relaysvc.Config
}

// testPeer 是起好的对端。
type testPeer struct {
	LobbyURL string // lobby 的 HTTP 根地址
	RelayTCP string // relay 的 TCP 地址
	RelayWS  string // relay 的 WebSocket URL；没开时为空
	Backend  string // 房间目录：redis 或 memory

	relay   *relaysvc.Server
	lobby   *lobby.Service
	http    *http.Server
	closers []func(context.Context) error

	mu        sync.Mutex
	tokens    map[string]identity
	admission string // 验权的故障注入："" / "ok" 照 lobby 答复，"reject" 一律拒绝，"down" 一律不可用
}

type identity struct{ account, game string }

// startTestPeer 起 relay、lobby 与 HTTP 服务。失败时已经起的部分都关掉。
func startTestPeer(cfg peerConfig) (_ *testPeer, err error) {
	p := &testPeer{tokens: map[string]identity{}}
	defer func() {
		if err != nil {
			_ = p.Close(context.Background())
		}
	}()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}

	rooms, err := p.openRooms(cfg.RedisAddr)
	if err != nil {
		return nil, err
	}
	p.lobby, err = lobby.New(lobby.Config{
		Games:         map[string]lobby.GameRule{peerGame: {MaxMembers: peerMaxMembers}},
		Rooms:         rooms,
		Tokens:        p,
		Relays:        peerRelays{p},
		Commands:      peerCommands{p},
		Subject:       subject,
		RelayTokenKey: key,
	})
	if err != nil {
		return nil, err
	}

	// relay → lobby 的报告：relay 的 Report 必须很快返回，所以只进队列；一个 goroutine 按到达顺序交给 lobby。
	reports := make(chan []byte, 4096)
	relayCfg := cfg.Relay
	relayCfg.Instance, relayCfg.TokenKey = instance, key
	p.relay, err = relaysvc.NewServer(relayCfg, relaysvc.ReporterFunc(func(report relaysvc.Report) {
		data, err := json.Marshal(report)
		if err == nil {
			reports <- data
		}
	}), relaysvc.AdmissionFunc(p.admit))
	if err != nil {
		return nil, err
	}
	reportsDone := make(chan struct{})
	go func() {
		defer close(reportsDone)
		for data := range reports {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = p.lobby.HandleRelayEventData(ctx, data)
			cancel()
		}
	}()
	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = p.relay.Run(runCtx) }()
	p.closers = append(p.closers, func(ctx context.Context) error {
		err := p.relay.Shutdown(ctx) // 关房的报告还会进队列，之后才关队列
		stopRun()
		<-runDone
		close(reports)
		<-reportsDone
		return err
	})
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p.RelayTCP = tcp.Addr().String()
	go func() { _ = p.relay.Serve(tcp) }()
	if cfg.WebSocket {
		if p.RelayWS, err = p.serveWebSocket(); err != nil {
			return nil, err
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /harness/game-token", p.serveGameToken)
	mux.HandleFunc("POST /harness/admission", p.serveAdmission)
	mux.Handle("/", p.lobby.Handler([]string{"*"}))
	httpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p.LobbyURL = "http://" + httpListener.Addr().String()
	p.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = p.http.Serve(httpListener) }()
	p.closers = append(p.closers, p.http.Shutdown)
	return p, nil
}

// openRooms 建房间目录：有 Redis 地址就用 Redis（键前缀每次唯一，Close 时删掉），否则内存。
func (p *testPeer) openRooms(addr string) (lobby.RoomStore, error) {
	const ttl = 30 * time.Second
	if addr == "" {
		p.Backend = "memory"
		return lobby.NewMemoryRooms(ttl, nil)
	}
	client, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		return nil, fmt.Errorf("relaypeer: redis %s: %w", addr, err)
	}
	prefix := fmt.Sprintf("roost:{sdkpeer-%d}:lobby", time.Now().UnixNano())
	p.closers = append(p.closers, func(ctx context.Context) error {
		_, err := redistest.DeleteKeys(ctx, addr, prefix)
		return errors.Join(err, client.Close())
	})
	p.Backend = "redis"
	return lobby.NewRedisRooms(client, prefix, ttl)
}

// Close 先停 HTTP，再停 relay（全部连接收到 Closed{shutdown}）与报告队列，最后删掉 Redis 里本对端的键。
// 按建立的倒序执行，所以 Start 中途失败时也能调用。
func (p *testPeer) Close(ctx context.Context) error {
	var errs []error
	for i := len(p.closers) - 1; i >= 0; i-- {
		errs = append(errs, p.closers[i](ctx))
	}
	p.closers = nil
	return errors.Join(errs...)
}

// IssueGameToken 给 account 签一张 game 的测试 token（代替 account 的 IssueGameToken；形如 gt2.peer.<随机>）。
func (p *testPeer) IssueGameToken(account, game string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	gameToken := "gt2.peer." + hex.EncodeToString(raw)
	p.mu.Lock()
	p.tokens[gameToken] = identity{account: account, game: game}
	p.mu.Unlock()
	return gameToken
}

// ValidateGameToken 实现 lobby.GameTokenValidator：只认本对端签的 token，游戏不符按无效处理（与 account 一致）。
func (p *testPeer) ValidateGameToken(_ context.Context, gameToken, game string) (string, error) {
	p.mu.Lock()
	id, ok := p.tokens[gameToken]
	p.mu.Unlock()
	if !ok || id.game != game {
		return "", fmt.Errorf("%w: unknown test game token", lobby.ErrUnauthorized)
	}
	return id.account, nil
}

// serveGameToken 是 POST /harness/game-token {"account","game"} → {"token"}（game 缺省 Game）。
func (p *testPeer) serveGameToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string `json:"account"`
		Game    string `json:"game"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Account == "" {
		http.Error(w, "account required", http.StatusBadRequest)
		return
	}
	if req.Game == "" {
		req.Game = peerGame
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": p.IssueGameToken(req.Account, req.Game)})
}

// admit 是 relay 向 lobby 的验权（relay.Admission）：生产经 NATS request 发到 lobby 的 VerifySubject，这里直接调
// lobby.HandleRelayVerifyData（同样经 JSON）。答复的解释与 wiring/relay 的 natsAdmission 相同：ok 放行；unavailable
// 或空原因按不可用；其余原因是拒绝（relay.ErrAdmissionDenied）。SetAdmission 可以注入拒绝或不可用。
func (p *testPeer) admit(ctx context.Context, request relaysvc.AdmitRequest) error {
	p.mu.Lock()
	mode := p.admission
	p.mu.Unlock()
	switch mode {
	case "reject":
		return fmt.Errorf("%w: injected by the test peer", relaysvc.ErrAdmissionDenied)
	case "down":
		return errors.New("relaypeer: lobby verification injected as unavailable")
	}
	data, err := json.Marshal(lobby.RelayVerifyRequest{Room: request.Room, Account: request.Account, Relay: request.Relay, Create: request.Create})
	if err != nil {
		return err
	}
	var reply lobby.RelayVerifyReply
	if err := json.Unmarshal(p.lobby.HandleRelayVerifyData(ctx, data), &reply); err != nil {
		return err
	}
	switch {
	case reply.OK:
		return nil
	case reply.Reason == lobby.VerifyUnavailable || reply.Reason == "":
		return fmt.Errorf("relaypeer: the lobby could not verify the connection (%q)", reply.Reason)
	}
	return fmt.Errorf("%w: %s", relaysvc.ErrAdmissionDenied, reply.Reason)
}

// SetAdmission 切换验权的结果："ok"（照 lobby 的真实答复）、"reject"、"down"。
func (p *testPeer) SetAdmission(mode string) error {
	switch mode {
	case "", "ok", "reject", "down":
	default:
		return fmt.Errorf("relaypeer: admission mode %q is not ok, reject or down", mode)
	}
	p.mu.Lock()
	p.admission = mode
	p.mu.Unlock()
	return nil
}

// serveAdmission 是 POST /harness/admission {"mode":"ok|reject|down"}。
func (p *testPeer) serveAdmission(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "mode required", http.StatusBadRequest)
		return
	}
	if err := p.SetAdmission(req.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// peerRelays 是固定的 relay 目录：只有本对端的 relay。
type peerRelays struct{ p *testPeer }

func (r peerRelays) Relays(context.Context) ([]lobby.Relay, error) {
	return []lobby.Relay{{Instance: instance, TCPAddr: r.p.RelayTCP, WSAddr: r.p.RelayWS, Rooms: r.p.relay.Stats().Rooms}}, nil
}

// peerCommands 把 lobby 发给 relay 的命令直接交给 relay（生产经 NATS <subject>.relay.<instance>）。
type peerCommands struct{ p *testPeer }

func (c peerCommands) Publish(_ string, data []byte) error {
	var command lobby.RelayCommand
	if err := json.Unmarshal(data, &command); err != nil {
		return err
	}
	if command.Type == lobby.CommandLeave {
		c.p.relay.Leave(command.Room, command.Account)
	}
	return nil
}

// serveWebSocket 照 wiring/relay 的做法给 relay 开 WebSocket 监听：共用接入层 wsconn 包出的 net.Listener 交给
// relay.Server.Serve，单条消息上限是 relay 能收的最大帧（快照与转交帧可以大于 max_payload_bytes，见
// Config.MaxFrameBytes），路径 /relay。不带 Origin 的原生客户端（node 的 ws）总是放行。
func (p *testPeer) serveWebSocket() (string, error) {
	cfg := p.relay.Config()
	listener, err := wsconn.Listen("127.0.0.1:0", wsconn.Config{
		Path:             "/relay",
		AllowedOrigins:   []string{"*"},
		MaxMessageBytes:  int64(cfg.MaxFrameBytes()),
		HandshakeTimeout: 5 * time.Second,
	})
	if err != nil {
		return "", err
	}
	go func() { _ = p.relay.Serve(listener) }()
	p.closers = append(p.closers, func(context.Context) error { return listener.Close() })
	return "ws://" + listener.Addr().String() + "/relay", nil
}
