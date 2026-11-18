package account

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// 多个服务（services.go，方案 2026-11-16 §4.1.2）：account.services 列出服务与类型，区服归属到 kind=game 的服务；
// 没有配置时行为与以前相同。

// 没有配置 account.services：ListServices 为空；区服记录不带服务名，也写不进服务名；清单与以前一样。
func TestWithoutServicesTheServerListIsUnchanged(t *testing.T) {
	s, cfg := freshAccount(t)
	ctx := context.Background()
	for transport, accounts := range enterClients(t, s) {
		if services, err := accounts.ListServices(ctx); err != nil || len(services) != 0 {
			t.Fatalf("%s: ListServices = %v, %v; want none", transport, services, err)
		}
	}
	if _, err := s.UpsertServer(ctx, GameServer{ID: 7, Name: "seven"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.UpsertServer(ctx, GameServer{ID: 8, Name: "eight", Service: "slg"}); !errors.Is(err, ErrServerInvalid) || got.ID != 0 {
		t.Fatalf("a service name without account.services: %+v, %v", got, err)
	}
	if slices.Contains(serverIndex(t, cfg), 8) {
		t.Fatal("the refused server was put into the index")
	}
	servers, err := s.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		if server.Service != "" {
			t.Fatalf("server %d carries service %q without account.services", server.ID, server.Service)
		}
	}
	if server, err := s.GetServer(ctx, 7); err != nil || server.Service != "" {
		t.Fatalf("GetServer = %+v, %v", server, err)
	}
}

// 一个 game 服务（加 lobby、relay）：ListServices 按名字升序；没写服务名的区服归到唯一的 game 服务——新登记的写进记录，
// 配置之前登记的（存储里是空）读出来也是它；经总线相同。lobby / relay、不认识的名字不能当区服的服务。
func TestServersBelongToTheOnlyGameService(t *testing.T) {
	s, cfg := freshAccount(t)
	ctx := context.Background()
	if _, err := s.UpsertServer(ctx, GameServer{ID: 5, Name: "before"}); err != nil { // 配置 services 之前登记的
		t.Fatal(err)
	}
	cfg.Services = []ServiceSpec{{Name: "relay", Kind: ServiceKindRelay}, {Name: "slg", Kind: ServiceKindGame}, {Name: "lobby", Kind: ServiceKindLobby}}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertServer(ctx, GameServer{ID: 6, Name: "after"}); err != nil {
		t.Fatal(err)
	}
	if stored, _, _ := cfg.Servers.Get(ctx, 6); stored.Value.Service != "slg" {
		t.Fatalf("a server registered without a service was stored with %q, want slg", stored.Value.Service)
	}
	want := []ServiceSpec{{Name: "lobby", Kind: ServiceKindLobby}, {Name: "relay", Kind: ServiceKindRelay}, {Name: "slg", Kind: ServiceKindGame}}
	for transport, accounts := range enterClients(t, s) {
		services, err := accounts.ListServices(ctx)
		if err != nil || !slices.Equal(services, want) {
			t.Fatalf("%s: ListServices = %v, %v; want %v", transport, services, err, want)
		}
		servers, err := accounts.ListServers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, server := range servers {
			if server.Service != "slg" {
				t.Fatalf("%s: server %d belongs to %q, want slg", transport, server.ID, server.Service)
			}
		}
		if server, err := accounts.GetServer(ctx, 5); err != nil || server.Service != "slg" {
			t.Fatalf("%s: GetServer(5) = %+v, %v", transport, server, err)
		}
	}
	// 交出去的切片是副本。
	listed, _ := s.ListServices(ctx)
	listed[0].Name = "changed"
	if again, _ := s.ListServices(ctx); again[0].Name != "lobby" {
		t.Fatal("ListServices handed out its own slice")
	}
	for _, service := range []string{"lobby", "relay", "chess"} {
		if got, err := s.UpsertServer(ctx, GameServer{ID: 9, Name: "nine", Service: service}); !errors.Is(err, ErrServerInvalid) || got.ID != 0 {
			t.Fatalf("a server of service %q: %+v, %v", service, got, err)
		}
	}
	if slices.Contains(serverIndex(t, cfg), 9) {
		t.Fatal("a refused server was put into the index")
	}
}

// 两个 game 服务：区服号是一个全局号段，登记要写明服务名；改名不写服务名时沿用记录里的；不能跨服务合服。
// 配置之前登记、没有服务名的服（场景夹具里的 1 号）不归任何服务，清单里服务名是空的。
func TestTwoGameServicesShareOneServerNumbering(t *testing.T) {
	_, cfg := freshAccount(t)
	cfg.Services = []ServiceSpec{{Name: "slg", Kind: ServiceKindGame}, {Name: "mmo", Kind: ServiceKindGame}}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, err := s.UpsertServer(ctx, GameServer{ID: 20, Name: "which"}); !errors.Is(err, ErrServerInvalid) || got.ID != 0 {
		t.Fatalf("a server without a service under two game services: %+v, %v", got, err)
	}
	if slices.Contains(serverIndex(t, cfg), 20) {
		t.Fatal("the refused server was put into the index")
	}
	for id, service := range map[int32]string{20: "slg", 21: "slg", 30: "mmo"} {
		if got, err := s.UpsertServer(ctx, GameServer{ID: id, Name: "s", Service: service}); err != nil || got.Service != service {
			t.Fatalf("server %d of %s: %+v, %v", id, service, got, err)
		}
	}
	// 服号全局唯一：同一个服号就是同一条记录，改名不写服务名时沿用 mmo。
	if got, err := s.UpsertServer(ctx, GameServer{ID: 30, Name: "renamed"}); err != nil || got.Service != "mmo" || got.Name != "renamed" {
		t.Fatalf("renaming server 30: %+v, %v", got, err)
	}
	byService := map[string][]int32{}
	servers, err := s.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		byService[server.Service] = append(byService[server.Service], server.ID)
	}
	if !slices.Equal(byService["slg"], []int32{20, 21}) || !slices.Equal(byService["mmo"], []int32{30}) || !slices.Equal(byService[""], []int32{1}) || len(byService) != 3 {
		t.Fatalf("servers by service = %v", byService)
	}
	if _, err := s.MergeServer(ctx, MergeRequest{From: 30, Into: 20, Note: mergeNote}); !errors.Is(err, ErrServerInvalid) {
		t.Fatalf("merging mmo's server into slg's: %v; want ErrServerInvalid", err)
	}
	if source, _ := s.GetServer(ctx, 30); source.MergedInto != 0 {
		t.Fatalf("the refused merge wrote the source: %+v", source)
	}
	if _, err := s.MergeServer(ctx, MergeRequest{From: 21, Into: 20, Note: mergeNote}); err != nil {
		t.Fatalf("merging within slg: %v", err)
	}
}

// 写错的 account.services 启动不了：名字不合写法、重复、类型不认识。
func TestBadServicesAreRefusedAtStart(t *testing.T) {
	_, cfg := freshAccount(t)
	for label, services := range map[string][]ServiceSpec{
		"an upper-case name": {{Name: "Lobby", Kind: ServiceKindLobby}},
		"listed twice":       {{Name: "slg", Kind: ServiceKindGame}, {Name: "slg", Kind: ServiceKindLobby}},
		"an unknown kind":    {{Name: "chat", Kind: "chat"}},
		"no kind":            {{Name: "chat"}},
	} {
		bad := cfg
		bad.Services = services
		if _, err := New(bad); err == nil {
			t.Fatalf("%s: New accepted account.services %+v", label, services)
		}
		if err := CheckGamesAndServices(nil, "", services); err == nil {
			t.Fatalf("%s: CheckGamesAndServices accepted %+v", label, services)
		}
	}
	if err := CheckGamesAndServices([]GameSpec{{Name: "party"}}, "", []ServiceSpec{{Name: "lobby", Kind: ServiceKindLobby}}); err != nil {
		t.Fatal(err)
	}
}
