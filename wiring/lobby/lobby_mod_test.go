package lobby

import domain "github.com/tjbdwanghaibo/roost-core/service/lobby"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fetcd "github.com/tjbdwanghaibo/roost-core/infra/network/etcd"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	redisdriver "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/redistest"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"github.com/tjbdwanghaibo/roost-core/service/account"
	"github.com/tjbdwanghaibo/roost-core/service/directory"
	"github.com/tjbdwanghaibo/roost-core/service/relay/token"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

const keyEnv = "ROOST_LOBBY_WIRING_TEST_KEY"

var testKey = "lobby-wiring-test-relay-token-key"

func validConfig(prefix string) *viper.Viper {
	cfg := viper.New()
	cfg.Set("lobby.listen", "127.0.0.1:0")
	cfg.Set("lobby.key_prefix", prefix)
	cfg.Set("lobby.games", map[string]any{"party": map[string]any{"max_members": 4}})
	cfg.Set("lobby.token_key_env", keyEnv)
	cfg.Set("lobby.relay_registry_prefix", "/test/relays/")
	cfg.Set("lobby.subject", "roost.test.lobby")
	cfg.Set("lobby.cors_origins", []string{"https://play.example.com"})
	return cfg
}

// 声明与读取一致：每个声明的键写成读不出来的值，Init 都点名它（与 wiring/config_schema_test.go 同一检查）。
func TestModLoadsWhatItDeclares(t *testing.T) {
	t.Setenv(keyEnv, testKey)
	mod := NewMod(nil)
	if err := mod.Init(validConfig("roost:test:lobby")); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	wrong := map[configschema.Kind]any{
		configschema.KindInt: "8k", configschema.KindDuration: 15, configschema.KindString: []any{"a", "b"},
		configschema.KindStrings: map[string]any{"a": 1},
	}
	checked := 0
	for _, key := range mod.ConfigSchema().Keys {
		value, ok := wrong[key.Kind]
		if !ok || strings.Contains(key.Name, "*") {
			continue
		}
		cfg := validConfig("roost:test:lobby")
		cfg.Set(key.Name, value)
		if err := NewMod(nil).Init(cfg); err == nil || !strings.Contains(err.Error(), key.Name) {
			t.Errorf("%s = %v: Init = %v; want a refusal naming the key", key.Name, value, err)
		}
		checked++
	}
	if checked < 7 {
		t.Fatalf("checked %d keys; the declaration lost keys", checked)
	}
}

func TestModInitRefusesMissingGamesAndKey(t *testing.T) {
	t.Setenv(keyEnv, testKey)
	noGames := validConfig("roost:test:lobby")
	noGames.Set("lobby.games", map[string]any{})
	if err := NewMod(nil).Init(noGames); err == nil || !strings.Contains(err.Error(), "lobby.games") {
		t.Fatalf("no games: %v", err)
	}
	zero := validConfig("roost:test:lobby")
	zero.Set("lobby.games", map[string]any{"party": map[string]any{"max_members": 0}})
	if err := NewMod(nil).Init(zero); err == nil || !strings.Contains(err.Error(), "max_members") {
		t.Fatalf("zero max members: %v", err)
	}
	t.Setenv(keyEnv, "short")
	if err := NewMod(nil).Init(validConfig("roost:test:lobby")); !errors.Is(err, token.ErrKey) {
		t.Fatalf("short key: %v", err)
	}
}

// fakeAccounts 只实现 ValidateGameToken；其余方法不该被大厅调到（调到会因为嵌入的 nil 接口 panic）。
type fakeAccounts struct {
	account.Accounts
	tokens map[string]account.GameAccount
	err    error
}

func (f fakeAccounts) ValidateGameToken(_ context.Context, gameToken string) (account.GameAccount, error) {
	if f.err != nil {
		return account.GameAccount{}, f.err
	}
	got, ok := f.tokens[gameToken]
	if !ok {
		return account.GameAccount{}, fmt.Errorf("%w: unknown test token", account.ErrSessionInvalid)
	}
	return got, nil
}

// account 的回答怎样变成大厅的鉴权结果：token 无效、游戏不符、游戏已撤、账号不在或被封都是 ErrUnauthorized；
// account 不可达原样上抛（大厅答 503）。
func TestAccountTokensMapsAccountAnswers(t *testing.T) {
	tokens := map[string]account.GameAccount{"gt-a": {AccountID: "acc-a", Game: "party"}}
	ok := AccountTokens{Accounts: fakeAccounts{tokens: tokens}}
	if got, err := ok.ValidateGameToken(context.Background(), "gt-a", "party"); err != nil || got != "acc-a" {
		t.Fatalf("valid token = %q, %v", got, err)
	}
	if _, err := ok.ValidateGameToken(context.Background(), "gt-a", "brawl"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("token for another game: %v", err)
	}
	if _, err := ok.ValidateGameToken(context.Background(), "gt-x", "party"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("invalid token: %v", err)
	}
	for _, refusal := range []error{account.ErrIdentityDenied, account.ErrAccountMissing, account.ErrGameInvalid} {
		got := AccountTokens{Accounts: fakeAccounts{err: refusal}}
		if _, err := got.ValidateGameToken(context.Background(), "gt-a", "party"); !errors.Is(err, domain.ErrUnauthorized) {
			t.Errorf("%v: err = %v, want ErrUnauthorized", refusal, err)
		}
	}
	outage := errors.New("nats: no responders")
	if _, err := (AccountTokens{Accounts: fakeAccounts{err: outage}}).ValidateGameToken(context.Background(), "gt-a", "party"); !errors.Is(err, outage) || errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("account outage: %v", err)
	}
}

// 一套 account 服务多个游戏（维护者 2026-11-18）：同一个人登录 party 与 brawl，大厅经真实的 account 认到的是两个不同的
// 账号号——房间记录、relay 令牌、relay 交给房主的都是它，别的游戏的账号号与“是同一个人”不出 account；party 的 token
// 进不了 brawl 的大厅。account 用内存存储。
func TestAccountTokensSeeOnlyTheGameAccount(t *testing.T) {
	names, err := directory.New(versionstore.NewMemoryStore[string, directory.Entry](), directory.Config{Normalize: directory.NormalizeLower, DefaultTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var next atomic.Int64
	accounts, err := account.New(account.Config{
		Accounts: versionstore.NewMemoryStore[string, account.Account](), Identities: versionstore.NewMemoryStore[string, account.IdentityRecord](),
		AccountIDs: account.NewMemoryAccountIDs(), Roles: account.NewMemoryRoles(), Servers: versionstore.NewMemoryStore[int32, account.GameServer](),
		Slots: versionstore.NewMemoryStore[string, account.Slot](), Names: names,
		Verifier: account.VerifierFunc(func(_ context.Context, identity account.Identity) (account.Verified, error) {
			return account.Verified{Channel: identity.Channel, OpenID: identity.OpenID}, nil
		}),
		Allocator: account.AllocatorFunc(func(context.Context, int32) (int64, error) { return next.Add(1), nil }),
		NameRules: account.NameValidatorFunc(func(string) error { return nil }), SessionSecret: "lobby-wiring-test",
		Games: []account.GameSpec{{Name: "party"}, {Name: "brawl"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tokens := AccountTokens{Accounts: accounts}
	seen := map[string]string{}
	for _, game := range []string{"party", "brawl"} {
		got, err := accounts.Login(ctx, account.Identity{Channel: "store", OpenID: "alice-open-id", Credential: "x", Game: game})
		if err != nil {
			t.Fatalf("login to %s: %v", game, err)
		}
		ticket, err := accounts.IssueAccountTicket(ctx, got.ID)
		if err != nil {
			t.Fatal(err)
		}
		gameToken, err := accounts.IssueGameToken(ctx, ticket.Ticket, game)
		if err != nil {
			t.Fatalf("game token for %s: %v", game, err)
		}
		lobbySees, err := tokens.ValidateGameToken(ctx, gameToken.Token, game)
		if err != nil || lobbySees != got.ID {
			t.Fatalf("the %s lobby sees %q, %v; want the %s account %q", game, lobbySees, err, game, got.ID)
		}
		if strings.Contains(lobbySees, "alice-open-id") {
			t.Fatalf("the %s lobby sees %q, which carries the channel identity", game, lobbySees)
		}
		other := map[string]string{"party": "brawl", "brawl": "party"}[game]
		if _, err := tokens.ValidateGameToken(ctx, gameToken.Token, other); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("the %s token in the %s lobby: %v; want ErrUnauthorized", game, other, err)
		}
		seen[game] = lobbySees
	}
	if seen["party"] == seen["brawl"] {
		t.Fatalf("the party and brawl lobbies both see account %q for one person; want a different account per game", seen["party"])
	}
}

// fakeNATS 记下队列订阅的处理函数与发出的命令。
type fakeNATS struct {
	mu        sync.Mutex
	handlers  map[string]fnats.MsgHandler
	queue     string
	published []string
	unsubbed  bool
}

type fakeSub struct{ owner *fakeNATS }

func (s fakeSub) Unsubscribe() error {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.owner.unsubbed = true
	return nil
}
func (s fakeSub) IsValid() bool { return true }

func (f *fakeNATS) Publish(subject string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, subject+" "+string(data))
	return nil
}
func (f *fakeNATS) Request(string, []byte, time.Duration) ([]byte, error) {
	return nil, errors.New("not used")
}
func (f *fakeNATS) Subscribe(string, fnats.MsgHandler) (fnats.ISubscription, error) {
	return nil, errors.New("the lobby subscribes with a queue group")
}
func (f *fakeNATS) QueueSubscribe(subject, queue string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handlers == nil {
		f.handlers = map[string]fnats.MsgHandler{}
	}
	f.handlers[subject], f.queue = handler, queue
	return fakeSub{f}, nil
}
func (f *fakeNATS) Drain() error { return nil }
func (f *fakeNATS) Close()       {}

func (f *fakeNATS) request(t *testing.T, subject, reply, data string) {
	t.Helper()
	f.mu.Lock()
	handler := f.handlers[subject]
	f.mu.Unlock()
	if handler == nil {
		t.Fatalf("nothing subscribed to %s", subject)
	}
	handler(&fnats.Msg{Subject: subject, Reply: reply, Data: []byte(data)})
}

func (f *fakeNATS) deliver(t *testing.T, subject string, event domain.RoomEvent) {
	t.Helper()
	f.mu.Lock()
	handler := f.handlers[subject]
	f.mu.Unlock()
	if handler == nil {
		t.Fatalf("nothing subscribed to %s", subject)
	}
	data, _ := json.Marshal(event)
	handler(&fnats.Msg{Subject: subject, Data: data})
}

type fakeEtcd struct {
	fetcd.IEtcd
	kvs []*fetcd.KV
}

func (f fakeEtcd) GetWithPrefix(_ context.Context, prefix string) ([]*fetcd.KV, error) {
	var out []*fetcd.KV
	for _, kv := range f.kvs {
		if strings.HasPrefix(kv.Key, prefix) {
			out = append(out, kv)
		}
	}
	return out, nil
}

// 真实 Redis 上的整条接线：Mod.Init / Provide（Redis 房间目录、etcd relay 发现、account 校验、NATS 命令）→ Server
// 订阅事件、监听 HTTP → 建房选中 etcd 里房间数最少的 relay → relay 报告经 NATS 到达后房间出现在列表里 → Shutdown 退订。
func TestModAndServerOnRealRedis(t *testing.T) {
	addr := os.Getenv("ROOST_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("ROOST_REDIS_TEST_ADDR is not set")
	}
	t.Setenv(keyEnv, testKey)
	prefix := redistest.Prefix(t, addr, "roost:test:{lobby-wiring}")
	redisClient, err := redisdriver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = redisClient.Close() })

	nats := &fakeNATS{}
	registry := app.NewRegistry(nil)
	for name, value := range map[app.ModName]any{
		mods.ModRedis: redisClient,
		mods.ModNats:  fnats.IClient(nats),
		mods.ModEtcd: fetcd.IEtcd(fakeEtcd{kvs: []*fetcd.KV{
			{Key: "/test/relays/relay-1", Value: `{"instance":"relay-1","tcp_addr":"10.0.0.1:7100","rooms":4}`},
			{Key: "/test/relays/relay-2", Value: `{"instance":"relay-2","tcp_addr":"10.0.0.2:7100","rooms":1}`},
		}}),
		account.CapabilityName: account.Accounts(fakeAccounts{tokens: map[string]account.GameAccount{
			"gt-host": {AccountID: "acc-host", Game: "party"}, "gt-guest": {AccountID: "acc-guest", Game: "party"},
		}}),
	} {
		if err := registry.Register(name, value); err != nil {
			t.Fatal(err)
		}
	}
	mod := NewMod(nil)
	if err := mod.Init(validConfig(prefix)); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	server := NewServer()
	if err := server.Init(registry); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for server.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("server did not start listening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	base := "http://" + server.Addr()

	post := func(path, gameToken string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+gameToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %d %v", path, resp.StatusCode, out)
		}
		return out
	}
	created := post("/rooms", "gt-host", map[string]any{"game": "party", "mode": "duel"})
	if created["relay_addr"] != "10.0.0.2:7100" {
		t.Fatalf("relay_addr = %v, want the least loaded relay-2", created["relay_addr"])
	}
	claims, err := token.Verify([]byte(testKey), created["token"].(string), "relay-2", time.Now())
	if err != nil || claims.Role != token.RoleHost || claims.MaxMembers != 4 {
		t.Fatalf("host token: %+v, %v", claims, err)
	}
	id := created["room"].(map[string]any)["id"].(string)

	nats.deliver(t, "roost.test.lobby.events", domain.RoomEvent{Type: domain.EventHeartbeat, Room: id, Relay: "relay-2", Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})
	if nats.queue != domain.EventQueue {
		t.Fatalf("queue group = %q, want %q", nats.queue, domain.EventQueue)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/rooms?game=party&mode=duel", nil)
	req.Header.Set("Authorization", "Bearer gt-guest")
	req.Header.Set("Origin", "https://play.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Rooms []domain.RoomView `json:"rooms"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listed)
	resp.Body.Close()
	if len(listed.Rooms) != 1 || listed.Rooms[0].ID != id || resp.Header.Get("Access-Control-Allow-Origin") != "https://play.example.com" {
		t.Fatalf("list: %+v, CORS %q", listed, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	post("/rooms/"+id+"/join", "gt-guest", map[string]any{"game": "party"})
	nats.deliver(t, "roost.test.lobby.events", domain.RoomEvent{Type: domain.EventMemberJoined, Room: id, Relay: "relay-2", Seq: 2, Host: "acc-host", Online: []string{"acc-host", "acc-guest"}})
	// relay 的验权请求：答复发到请求的回信主题。
	nats.request(t, "roost.test.lobby.verify", "inbox.1", `{"room":"`+id+`","account":"acc-guest","relay":"relay-2"}`)
	nats.request(t, "roost.test.lobby.verify", "inbox.2", `{"room":"`+id+`","account":"acc-stranger","relay":"relay-2"}`)
	if len(nats.published) != 2 || nats.published[0] != `inbox.1 {"ok":true}` || nats.published[1] != `inbox.2 {"ok":false,"reason":"not_member"}` {
		t.Fatalf("verification replies = %v", nats.published)
	}
	post("/rooms/"+id+"/leave", "gt-guest", map[string]any{"game": "party"})
	if len(nats.published) != 3 || !strings.HasPrefix(nats.published[2], "roost.test.lobby.relay.relay-2 ") {
		t.Fatalf("relay commands = %v", nats.published)
	}

	shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if !nats.unsubbed {
		t.Fatal("Shutdown left the relay event subscription in place")
	}
}

func TestServerInitNeedsTheMod(t *testing.T) {
	if err := NewServer().Init(app.NewRegistry(nil)); err == nil || !strings.Contains(err.Error(), string(CapabilityName)) {
		t.Fatalf("Init without the Mod: %v", err)
	}
	mod := NewMod(nil)
	if deps := fmt.Sprint(mod.DependsOn()); !strings.Contains(deps, string(account.CapabilityName)) {
		t.Fatalf("DependsOn = %s, want the account client when no validator is injected", deps)
	}
	if deps := fmt.Sprint(NewMod(AccountTokens{}).DependsOn()); strings.Contains(deps, string(account.CapabilityName)) {
		t.Fatalf("DependsOn = %s, want no account client when a validator is injected", deps)
	}
}
