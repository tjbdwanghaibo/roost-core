package account

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

// 区服入口登记（presence.go，D23）。存储缺省是 Memory（用测试时钟推进）；ROOST_REVIEW3_BACKEND=redis（或
// ROOST_REVIEW_CLUSTER）时走隔离的真实 Redis（RedisPresenceStore，到期由 Redis 自己的 TIME 判断），这时“过期”
// 要真的等过去，所以期限取 1 秒。

// lapsePresence 让登记过期：Memory 推进测试时钟，Redis 真等过期限。
func lapsePresence(clock *clock, ttlSeconds int64) {
	if redisBackend() {
		time.Sleep(time.Duration(ttlSeconds)*time.Second + 200*time.Millisecond)
		return
	}
	clock.advance(time.Duration(ttlSeconds) * time.Second)
}

func presenceIDs(t *testing.T, accounts Accounts) map[int32]ServerPresence {
	t.Helper()
	listed, err := accounts.ListServerPresence(context.Background())
	if err != nil {
		t.Fatalf("ListServerPresence: %v", err)
	}
	if !slices.IsSortedFunc(listed, func(a, b ServerPresence) int { return int(a.ServerID) - int(b.ServerID) }) {
		t.Fatalf("presence is not in server order: %+v", listed)
	}
	out := map[int32]ServerPresence{}
	for _, presence := range listed {
		out[presence.ServerID] = presence
	}
	return out
}

// 承诺：登记在期限之内给出、再报一次覆盖并重新计时、不再报的到期即消失；经总线上报与读取与本地相同。
func TestServerPresenceExpiresWhenTheReporterStops(t *testing.T) {
	s, clock, _ := pendingContractService(t)
	loopback := &loopbackBus{handlers: map[string]bus.RpcHandlerFunc{}}
	if err := RegisterHandlers(loopback, s); err != nil {
		t.Fatal(err)
	}
	remote, err := NewBusClient(loopback, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const ttl = 1
	if err := remote.ReportServerPresence(ctx, ServerPresence{ServerID: 1000, Addr: "203.0.113.7:7000", WSAddr: " wss://play.example.test/ws ", Load: 3, Tags: []string{"new"}}, ttl); err != nil {
		t.Fatal(err)
	}
	if err := remote.ReportServerPresence(ctx, ServerPresence{ServerID: 1001, Addr: " 203.0.113.8:7000 ", Load: 1}, 2*ttl); err != nil {
		t.Fatal(err)
	}
	for _, accounts := range []Accounts{s, remote} {
		got := presenceIDs(t, accounts)
		// WebSocket 地址经总线、存储原样带回（去掉两端空白）；没报的服没有 WebSocket 地址。
		if len(got) != 2 || got[1000].Addr != "203.0.113.7:7000" || got[1000].WSAddr != "wss://play.example.test/ws" || got[1000].Load != 3 ||
			!slices.Equal(got[1000].Tags, []string{"new"}) || got[1001].Addr != "203.0.113.8:7000" || got[1001].WSAddr != "" {
			t.Fatalf("presence after two reports = %+v", got)
		}
	}
	// 1000 不再报：到期即消失；1001 期限更长，还在。
	if !redisBackend() {
		clock.advance(time.Second - time.Millisecond)
		if got := presenceIDs(t, s); len(got) != 2 {
			t.Fatalf("presence disappeared before its ttl: %+v", got)
		}
		clock.advance(time.Millisecond)
	} else {
		lapsePresence(clock, ttl)
	}
	if got := presenceIDs(t, remote); len(got) != 1 || got[1001].ServerID != 1001 {
		t.Fatalf("a server that stopped reporting is still listed after its ttl: %+v", got)
	}
	// 再报一次就回来，并且以最后一次为准。
	if err := remote.ReportServerPresence(ctx, ServerPresence{ServerID: 1000, Addr: "203.0.113.9:7001", Load: 9}, ttl); err != nil {
		t.Fatal(err)
	}
	if got := presenceIDs(t, s); got[1000].Addr != "203.0.113.9:7001" || got[1000].WSAddr != "" || got[1000].Load != 9 || len(got[1000].Tags) != 0 {
		t.Fatalf("a renewed report did not replace the entry: %+v", got[1000])
	}
	// 正常下线：立刻撤掉，不等过期；撤一个没登记的服不是错误。
	if err := remote.WithdrawServerPresence(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if err := remote.WithdrawServerPresence(ctx, 4242); err != nil {
		t.Fatal(err)
	}
	if got := presenceIDs(t, s); got[1000].ServerID != 0 {
		t.Fatalf("a withdrawn server is still listed: %+v", got)
	}
}

// 承诺：不成样子的上报与超范围的期限以 ErrRangeInvalid 拒绝、什么都不写。
func TestServerPresenceRefusesBadReports(t *testing.T) {
	s, _, _ := pendingContractService(t)
	ctx := context.Background()
	good := ServerPresence{ServerID: 7, Addr: "game.example:7000"}
	long := make([]byte, MaxPresenceAddrBytes)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		name     string
		presence ServerPresence
		ttl      int64
	}{
		{"zero server", ServerPresence{Addr: "h:1"}, 30},
		{"negative server", ServerPresence{ServerID: -1, Addr: "h:1"}, 30},
		{"empty addr", ServerPresence{ServerID: 7}, 30},
		{"port only", ServerPresence{ServerID: 7, Addr: ":7000"}, 30},
		{"no port", ServerPresence{ServerID: 7, Addr: "host"}, 30},
		{"bad port", ServerPresence{ServerID: 7, Addr: "host:70000"}, 30},
		{"long addr", ServerPresence{ServerID: 7, Addr: string(long) + ":1"}, 30},
		{"negative load", ServerPresence{ServerID: 7, Addr: "h:1", Load: -1}, 30},
		{"ws addr over http", ServerPresence{ServerID: 7, Addr: "h:1", WSAddr: "https://h/ws"}, 30},
		{"ws addr without a scheme", ServerPresence{ServerID: 7, Addr: "h:1", WSAddr: "h:7080"}, 30},
		{"ws addr without a host", ServerPresence{ServerID: 7, Addr: "h:1", WSAddr: "ws://:7080/ws"}, 30},
		{"ws addr with port zero", ServerPresence{ServerID: 7, Addr: "h:1", WSAddr: "ws://h:0/ws"}, 30},
		{"ws addr with a user", ServerPresence{ServerID: 7, Addr: "h:1", WSAddr: "wss://u:p@h/ws"}, 30},
		{"long ws addr", ServerPresence{ServerID: 7, Addr: "h:1", WSAddr: "ws://" + string(long)}, 30},
		{"too many tags", ServerPresence{ServerID: 7, Addr: "h:1", Tags: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}}, 30},
		{"empty tag", ServerPresence{ServerID: 7, Addr: "h:1", Tags: []string{""}}, 30},
		{"zero ttl", good, 0},
		{"ttl over the limit", good, MaxPresenceTTLSeconds + 1},
	}
	for _, tc := range cases {
		if err := s.ReportServerPresence(ctx, tc.presence, tc.ttl); !errors.Is(err, ErrRangeInvalid) {
			t.Errorf("%s: err = %v, want ErrRangeInvalid", tc.name, err)
		}
	}
	if got := presenceIDs(t, s); len(got) != 0 {
		t.Fatalf("a refused report was stored: %+v", got)
	}
	if err := s.WithdrawServerPresence(ctx, 0); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("withdraw of server 0: err = %v", err)
	}
}

// RV-20261118-13：WebSocket 地址交给浏览器（Unity WebGL、TS SDK 缺省传输）按 WHATWG URL 解析。数字主机（末段是数字的
// 主机名、方括号 IPv6）浏览器解析不了时，CheckWebSocketAddr 在配置 / 上报阶段就拒绝（portal.websocket_servers、
// login.presence.ws_addr 与 account 收上报都经它），不让连不上的地址进区服目录；浏览器能解析但会改写成另一种样子的
// IPv4 写法（127.1、010.0.0.1、0x7f.0.0.1、末尾带点）同样拒绝并给出该写的值——别的客户端（C#、原生）未必按同样的
// 规则读它们（010 在浏览器里是八进制）。合法的点分十进制 IPv4、方括号 IPv6（含展开写法）与 DNS 名照常接受
// （含末段字母数字混合、首段是数字的 DNS 名）。
func TestCheckWebSocketAddrRefusesNumericHostsBrowsersCannotUse(t *testing.T) {
	for _, addr := range []string{
		"ws://192.0.2.300:7080/ws", // 末段越界
		"ws://192.0.2.30.1.1/ws",   // 五段
		"ws://256.0.0.1/ws",
		"ws://09.1.1.1/ws",             // 八进制里没有 9
		"ws://game.1:7080/ws",          // 末段是数字的主机名按 IPv4 解析，失败
		"ws://game.0x1f/ws",            // 末段是十六进制数同理
		"wss://[1.2.3.4]/ws",           // 方括号里只能是 IPv6
		"ws://[fe80::1%25en0]:7080/ws", // 浏览器不认区域号
	} {
		if _, err := CheckWebSocketAddr(addr); err == nil {
			t.Errorf("%q passed; a browser cannot parse it", addr)
		}
	}
	// IPv6 不写方括号：浏览器把第一个冒号之后当端口，解析失败（Node 的 new URL 报 ERR_INVALID_URL）；拒绝并提示写成 [...]。
	for _, addr := range []string{"ws://2001:db8::1:7080/ws", "wss://2001:db8::1/ws", "ws://::1:7080/ws"} {
		if _, err := CheckWebSocketAddr(addr); err == nil || !strings.Contains(err.Error(), "brackets") {
			t.Errorf("%q: err = %v; want refused, telling to put the IPv6 address in brackets", addr, err)
		}
	}
	for addr, want := range map[string]string{
		"ws://127.1:7080/ws":       "127.0.0.1",
		"ws://010.0.0.1/ws":        "8.0.0.1",
		"ws://0x7f.0.0.1/ws":       "127.0.0.1",
		"ws://192.0.2.30.:7080/ws": "192.0.2.30",
	} {
		_, err := CheckWebSocketAddr(addr)
		if err == nil || !strings.Contains(err.Error(), `"`+want+`"`) {
			t.Errorf("%q: err = %v, want refused suggesting %q (what browsers rewrite it to)", addr, err, want)
		}
	}
	for _, addr := range []string{
		"ws://192.0.2.30:7080/ws", "wss://192.0.2.30/ws", "ws://0.0.0.0:7080/ws", "ws://[::1]:7080/ws", "wss://[2001:db8::1]/ws",
		"ws://game.example:7080/ws", "wss://lb.example/game/ws?zone=1", "ws://localhost:7080/ws", "ws://game-1:7080/ws",
		"ws://1e3.example/ws", "ws://2048.example.com/ws", "ws://game.example./ws",
		"ws://[2001:db8:0:0:0:0:0:1]/ws", "ws://[::ffff:192.0.2.30]:7080/ws", // IPv6 的展开写法各家读法相同，不强求压缩
	} {
		if got, err := CheckWebSocketAddr(addr); err != nil || got != addr {
			t.Errorf("%q: got %q, %v; want accepted unchanged", addr, got, err)
		}
	}
}

// 承诺：至多 MaxServers 个没过期的服；满了之后新的服号被拒绝、已登记的照常续期；过期的不占名额。
func TestServerPresenceHoldsAtMostMaxServers(t *testing.T) {
	s, clock, _ := pendingContractService(t)
	ctx := context.Background()
	report := func(id int32, ttl int64) error {
		return s.ReportServerPresence(ctx, ServerPresence{ServerID: id, Addr: fmt.Sprintf("10.0.0.1:%d", 1000+id)}, ttl)
	}
	// 第一个服期限短，其余长：短的过期之后要腾出名额。
	if err := report(1, 1); err != nil {
		t.Fatal(err)
	}
	for id := int32(2); id <= MaxServers; id++ {
		if err := report(id, 30); err != nil {
			t.Fatalf("report %d: %v", id, err)
		}
	}
	if err := report(MaxServers+1, 30); !errors.Is(err, ErrRangeInvalid) {
		t.Fatalf("a new server past the limit: err = %v, want ErrRangeInvalid", err)
	}
	if err := report(MaxServers, 30); err != nil {
		t.Fatalf("renewing a listed server at the limit: %v", err)
	}
	if got := presenceIDs(t, s); len(got) != MaxServers || got[MaxServers+1].ServerID != 0 {
		t.Fatalf("listed %d servers at the limit, want %d without the refused one", len(got), MaxServers)
	}
	lapsePresence(clock, 1)
	if err := report(MaxServers+1, 30); err != nil {
		t.Fatalf("a new server after one expired: %v", err)
	}
	if got := presenceIDs(t, s); len(got) != MaxServers || got[1].ServerID != 0 || got[MaxServers+1].ServerID == 0 {
		t.Fatalf("after expiry: %d servers, server 1 listed=%v, server %d listed=%v", len(got), got[1].ServerID != 0, MaxServers+1, got[MaxServers+1].ServerID != 0)
	}
}
