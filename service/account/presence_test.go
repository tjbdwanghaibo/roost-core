package account

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
