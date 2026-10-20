package nestgame

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/client/wire"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/wiring/selfsync"
)

// 嵌入 TCP 接入负载（ROOST_NEST_GAME_EMBEDDED=1）：玩家直连 Game 进程内的 gateway.TCPServer + TCPRuntime，
// 不经独立 Gate / NATS。接线与生成的 player TCP Mod（codegen render_player_tcp.go 的 Mod.Start）相同：
// NewTCPServer(dispatch, authenticator) → ConnectRuntime(TCPRuntime) → [ConnectHandshake] → Start。
//
//   - 普通业务：读循环 → dispatch → Nest Request → 回包入每连接出站队列，由写协程写出（生产是 ProtocolRegistry
//     → 生成 Sender → Nest，夹具与独立 Gate 模式一样直接 Request，少一次协议解码）。
//   - 场景 Sync（SYNC=1）：会话 ID 是玩家 ID，Manager 的 Transport 是 selfsync.PushPlayerFrame(TCPRuntime)，
//     与 game-demo 场景的 sceneLane 相同；会话在 AfterACK 打开（held），start 统一 Ready。
//   - 自身视图（SELF_SYNC=1）：selfsync.SelfSync 的 Handshake 接到 TCPServer，BindPlayers(TCPRuntime)，
//     与生成工程打开 access.player.self_sync 时相同。
//
// 出站指标：player_tcp_outbound_budget_exhausted_total（全进程驻留预算拒绝，正常负载期望 0）、
// player.sync.push_refused.total{outcome}、overflow / push_closed / write_error 与写协程数。每连接出站队列的
// 峰值与全进程驻留字节框架不导出（tcpSession 未导出、也没有驻留 Gauge，见 KNOWN-LIMITS“嵌入 TCP 出站”），夹具取不到。
type embeddedFrontend struct {
	loadClients
	runtime *gateway.TCPRuntime
	server  *gateway.TCPServer
	tcp     gateway.TCPConfig
}

// embeddedSceneLane 是嵌入 TCP 下场景 Sync 的 Transport：一帧就是一次 PushPlayerFrame（game-demo sceneLane 同一条）。
type embeddedSceneLane struct{ players gateway.PlayerRuntime }

func (lane embeddedSceneLane) Push(ctx context.Context, session entitysync.SessionID, frame []byte) error {
	return selfsync.PushPlayerFrame(ctx, lane.players, int64(session), 501, frame)
}

// sceneHandshake 在鉴权 ACK 写出之后为玩家打开场景会话（会话 ID = 玩家 ID），连接关闭时关掉。
type sceneHandshake struct{ load *syncGameLoad }

func (sceneHandshake) BeforeACK(context.Context, gateway.ConnectionSession, string) error { return nil }

func (h sceneHandshake) AfterACK(_ context.Context, session gateway.ConnectionSession) error {
	playerID := session.Principal().PlayerID
	return h.load.openSession(int(playerID)-1, entitysync.SessionID(playerID))
}

func (h sceneHandshake) Closed(session gateway.ConnectionSession) {
	h.load.manager.CloseSession(entitysync.SessionID(session.Principal().PlayerID))
}

// embeddedTCPConfig 是夹具的嵌入 TCP 配置：生产缺省（max_payload_bytes 1 MiB、write_timeout 5s、驻留预算 256 MiB），
// 只改监听地址、连接数上限与限流（与独立 Gate 模式的 TCPServer 相同：关掉每连接限流，连接都来自 127.0.0.1）。
// ROOST_NEST_GAME_TCP_RESIDENT_BYTES 可覆盖驻留预算（0 / 不设取框架缺省），用来人为压低预算验证拒绝路径。
func embeddedTCPConfig(t *testing.T, c gameConfig) gateway.TCPConfig {
	t.Helper()
	tcp := gateway.DefaultTCPConfig()
	tcp.Addr = "127.0.0.1:0"
	tcp.MaxConnections = max(tcp.MaxConnections, c.Players)
	tcp.MaxConnectionsPerIP = c.Players
	tcp.MaxHandshakes = min(tcp.MaxHandshakes, tcp.MaxConnections)
	tcp.RequestRate = 0
	if raw := os.Getenv("ROOST_NEST_GAME_TCP_RESIDENT_BYTES"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			t.Fatalf("ROOST_NEST_GAME_TCP_RESIDENT_BYTES=%q: want a non-negative byte count", raw)
		}
		tcp.OutboundResidentBytes = value
	}
	if tcp.OutboundResidentBytes == 0 {
		tcp.OutboundResidentBytes = gateway.DefaultTCPConfig().OutboundResidentBytes
	}
	if err := gateway.ValidateTCPConfig(tcp); err != nil {
		t.Fatal(err)
	}
	return tcp
}

func newEmbeddedFrontend(t *testing.T, engine *nest.NestMgr, name nest.HandlerName, units []*Unit, c gameConfig, runtime *gateway.TCPRuntime, stateSync *syncGameLoad) *embeddedFrontend {
	t.Helper()
	f := &embeddedFrontend{loadClients: loadClients{stream: "embedded"}, runtime: runtime, tcp: embeddedTCPConfig(t, c)}
	var err error
	f.server, err = gateway.NewTCPServer(f.tcp, func(ctx context.Context, session gateway.Session, id, seq uint32, kind wire.PayloadKind, payload []byte) (any, error) {
		in, err := decodeLoadRequest(id, kind, payload)
		if err != nil {
			return nil, err
		}
		index := int(session.Principal().PlayerID) - 1
		if _, err = engine.Request(ctx, name, units[index].ID(), nest.NewParams(in)); err != nil {
			return nil, err
		}
		return &gateway.TCPResponse{MessageID: 2, Sequence: seq}, nil
	}, loadAuthenticator(c.Players))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.server.ConnectRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if stateSync != nil {
		hooks := gateway.TCPHandshake(sceneHandshake{load: stateSync})
		if stateSync.self {
			hooks = stateSync.selfSync.Handshake()
			stateSync.selfSync.BindPlayers(runtime)
		}
		if err = f.server.ConnectHandshake(hooks); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		f.stopping.Store(true)
		if err := f.server.Stop(ctx); err != nil {
			t.Logf("embedded tcp stop: %v", err)
		}
		f.close()
	})
	var onSync func(int, []byte) error
	if stateSync != nil {
		onSync = stateSync.apply
	}
	f.connect(t, f.server.Addr().String(), c.Players, int(f.tcp.MaxPayloadBytes), onSync)
	return f
}

// outboundCounters 是嵌入 TCP 出站相关的进程级指标在某一时刻的值（metrics 默认注册表）。
// 计数器是累计值；StrandedPlayers 与 Connections 是 Gauge。
type outboundCounters struct {
	BudgetExhausted    int64 `json:"budget_exhausted"`     // player_tcp_outbound_budget_exhausted_total
	Overflow           int64 `json:"overflow"`             // player_tcp_outbound_overflow_total（每连接上限，关连接）
	PushClosed         int64 `json:"push_closed"`          // player_tcp_push_closed_total
	WriteErrors        int64 `json:"write_errors"`         // player_tcp_write_error_total
	DispatchErrors     int64 `json:"dispatch_errors"`      // player_tcp_dispatch_error_total
	DispatchTimeouts   int64 `json:"dispatch_timeouts"`    // player_tcp_dispatch_timeout_total
	PushRefusedRetried int64 `json:"push_refused_retried"` // player.sync.push_refused.total{outcome="retried"}
	PushRefusedClosed  int64 `json:"push_refused_closed"`  // player.sync.push_refused.total{outcome="closed"}
	StrandedPlayers    int64 `json:"stranded_players"`     // entitysync.self_view.stranded_players（Gauge，自身视图）
	Connections        int64 `json:"connections"`          // player_tcp_connections（Gauge）
}

func readOutboundCounters() outboundCounters {
	var out outboundCounters
	for _, m := range metrics.Snapshot() {
		switch m.Name {
		case "player_tcp_outbound_budget_exhausted_total":
			out.BudgetExhausted += m.Value
		case "player_tcp_outbound_overflow_total":
			out.Overflow += m.Value
		case "player_tcp_push_closed_total":
			out.PushClosed += m.Value
		case "player_tcp_write_error_total":
			out.WriteErrors += m.Value
		case "player_tcp_dispatch_error_total":
			out.DispatchErrors += m.Value
		case "player_tcp_dispatch_timeout_total":
			out.DispatchTimeouts += m.Value
		case selfsync.SyncPushRefusedMetric:
			switch m.Labels["outcome"] {
			case "retried":
				out.PushRefusedRetried += m.Value
			case "closed":
				out.PushRefusedClosed += m.Value
			}
		case selfsync.StrandedPlayersMetric:
			out.StrandedPlayers = m.Value
		case "player_tcp_connections":
			out.Connections = m.Value
		}
	}
	return out
}

// sub 是两次读数之间的增量；Gauge 取 after 的值。
func (after outboundCounters) sub(before outboundCounters) outboundCounters {
	return outboundCounters{
		BudgetExhausted: after.BudgetExhausted - before.BudgetExhausted, Overflow: after.Overflow - before.Overflow,
		PushClosed: after.PushClosed - before.PushClosed, WriteErrors: after.WriteErrors - before.WriteErrors,
		DispatchErrors: after.DispatchErrors - before.DispatchErrors, DispatchTimeouts: after.DispatchTimeouts - before.DispatchTimeouts,
		PushRefusedRetried: after.PushRefusedRetried - before.PushRefusedRetried, PushRefusedClosed: after.PushRefusedClosed - before.PushRefusedClosed,
		StrandedPlayers: after.StrandedPlayers, Connections: after.Connections,
	}
}

// tcpGoroutines 是按栈函数数出的嵌入 TCP 协程：每连接一个写协程（tcpSession.writeLoop）与一个读循环
// （TCPServer.serveConnection，读请求、dispatch 都在它上面）。取 goroutine profile 会短暂停顿全部协程，
// 只在负载开始前与结束后各取一次，不在被测区间内。
type tcpGoroutines struct {
	Total, WriteLoops, ServeConnections int
}

func countTCPGoroutines(t *testing.T) tcpGoroutines {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 1); err != nil {
		t.Fatal(err)
	}
	var result tcpGoroutines
	group := 0
	var writer, serve bool
	flush := func() {
		if writer {
			result.WriteLoops += group
		}
		if serve {
			result.ServeConnections += group
		}
		group, writer, serve = 0, false, false
	}
	scanner := bufio.NewScanner(&buf)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "goroutine profile: total "):
			result.Total, _ = strconv.Atoi(strings.TrimPrefix(line, "goroutine profile: total "))
		case line == "":
			flush()
		case !strings.HasPrefix(line, "#"):
			// 每组以“N @ pc...”开头。
			if count, _, ok := strings.Cut(line, " @ "); ok {
				group, _ = strconv.Atoi(count)
			}
		default:
			writer = writer || strings.Contains(line, "gateway.(*tcpSession).writeLoop")
			serve = serve || strings.Contains(line, "gateway.(*TCPServer).serveConnection")
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// embeddedReport 是嵌入 TCP 模式的专有结果；业务与 Sync 的分段沿用 Messages / Heartbeats / SyncReport。
type embeddedReport struct {
	// SceneSync / SelfSync 是本次叠加的 Sync 形态（二者互斥）。
	SceneSync, SelfSync   bool
	MaxPayloadBytes       uint32
	OutboundResidentBytes int64
	WriteTimeoutMS        float64
	// Completed / RoundTrip：模拟客户端写出请求到读到回包（服务端为主，与 GateRoundTrip 同口径、少 Gate 与 NATS 两跳）。
	Completed uint64
	RoundTrip latencyResult
	// GoroutinesAtStart 在全部连接建好、负载开始前取；GoroutinesAtEnd 在负载结束、排空客户端之前取。
	GoroutinesAtStart, GoroutinesAtEnd tcpGoroutines
	// CountersAtStart 在负载开始前读，CountersDelta 是负载开始到 Sync 收敛（finish）之后的增量。
	CountersAtStart, CountersDelta outboundCounters
	// OutboundClean：被测区间内没有预算拒绝、没有每连接溢出与关连接、没有 Sync 推送被拒。只报告，不作为通过门槛。
	OutboundClean bool
	// QueuePeak 说明每连接出站队列峰值为什么没有数字。
	QueuePeak string
}
