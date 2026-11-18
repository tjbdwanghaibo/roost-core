package account

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 一套 account 服务多个游戏（维护者 2026-11-18）：同一个人（同一渠道 + open id）在每个游戏里是不同的账号号，
// 账号号只属于那个游戏；别的游戏的账号号与“是同一个人”的关联不交给客户端与游戏代码。封号、凭证版本、gt2 代数
// 在身份级，各游戏共用。场景里的 account.games 是 party 与 brawl（gameTokenGames，缺省游戏 party）。

func loginGame(t *testing.T, accounts Accounts, openID, game string) Account {
	t.Helper()
	account, err := accounts.Login(context.Background(), Identity{Channel: "store", OpenID: openID, Credential: "good", Game: game})
	if err != nil {
		t.Fatalf("login %s to %s: %v", openID, game, err)
	}
	return account
}

func loginGameWith(t *testing.T, accounts Accounts, openID, game, credential string) Account {
	t.Helper()
	account, err := accounts.Login(context.Background(), Identity{Channel: "store", OpenID: openID, Credential: credential, Game: game})
	if err != nil {
		t.Fatalf("login %s to %s with %q: %v", openID, game, credential, err)
	}
	return account
}

// 同一个人登录两个游戏，拿到两个不同的账号号；同一个游戏再登录拿到的还是那一个。账号号里看不出渠道与 open id，
// 交出去的账号记录上没有身份键。本地与经总线相同；存储走 Redis 时账号号来自 Redis 里的计数器。
func TestAnIdentityHasADifferentAccountInEachGame(t *testing.T) {
	s, _, _ := pendingContractService(t, gameTokenGames)
	for transport, accounts := range enterClients(t, s) {
		openID := "alice-" + transport
		party := loginGame(t, accounts, openID, "party")
		brawl := loginGame(t, accounts, openID, "brawl")
		if party.ID == "" || brawl.ID == "" || party.ID == brawl.ID {
			t.Fatalf("%s: one identity got account %q in party and %q in brawl; want two different ids", transport, party.ID, brawl.ID)
		}
		if party.Game != "party" || brawl.Game != "brawl" {
			t.Fatalf("%s: the accounts name games %q and %q", transport, party.Game, brawl.Game)
		}
		for _, account := range []Account{party, brawl} {
			if strings.Contains(account.ID, openID) || strings.Contains(account.ID, "store") || account.Identity != "" {
				t.Fatalf("%s: the account handed out links back to the identity: %+v", transport, account)
			}
		}
		again := loginGame(t, accounts, openID, "party")
		if again.ID != party.ID || again.CreatedAtUnix != party.CreatedAtUnix {
			t.Fatalf("%s: logging into party again gave %+v, first %+v", transport, again, party)
		}
		other := loginGame(t, accounts, "bob-"+transport, "party")
		if other.ID == party.ID || other.ID == brawl.ID {
			t.Fatalf("%s: bob's party account %q is one of alice's (%q, %q)", transport, other.ID, party.ID, brawl.ID)
		}
		// 账号票据交回来的账号也是这个游戏的，同样不带身份键。
		got, err := accounts.ValidateAccountTicket(context.Background(), mustAccountTicket(t, s, brawl.ID))
		if err != nil || got.ID != brawl.ID || got.Game != "brawl" || got.Identity != "" {
			t.Fatalf("%s: ValidateAccountTicket = %+v, %v", transport, got, err)
		}
	}
}

// rendezvousAccountIDs 让并发的首登都先领到号、再一起往下走：每次 NextAccountID 等到 want 次调用都到了（或等满 5 秒）
// 才返回。这样每个登录都读到“还没有映射”、各自建了账号记录，竞争确实落在写映射那一步。
type rendezvousAccountIDs struct {
	inner AccountIDAllocator
	want  int
	mu    sync.Mutex
	calls int
	all   chan struct{}
}

func (r *rendezvousAccountIDs) NextAccountID(ctx context.Context) (int64, error) {
	r.mu.Lock()
	if r.calls++; r.calls == r.want {
		close(r.all)
	}
	r.mu.Unlock()
	select {
	case <-r.all:
	case <-time.After(5 * time.Second):
	}
	return r.inner.NextAccountID(ctx)
}

// 同一个人第一次登录同一个游戏的并发请求只建一个账号号：都拿到同一个，之后再登录也是它。所有登录都在领号处汇合，
// 各自建了账号记录，以先写进身份记录的那个号为准。
func TestConcurrentFirstLoginsOfOneGameShareOneAccount(t *testing.T) {
	const logins = 8
	s, _, _ := pendingContractService(t, gameTokenGames, func(c *Config) {
		c.AccountIDs = &rendezvousAccountIDs{inner: c.AccountIDs, want: logins, all: make(chan struct{})}
	})
	ids := make([]string, logins)
	errs := make([]error, logins)
	var wg sync.WaitGroup
	for i := range logins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			account, err := s.Login(context.Background(), Identity{Channel: "store", OpenID: "racer", Credential: "good", Game: "party"})
			ids[i], errs[i] = account.ID, err
		}()
	}
	wg.Wait()
	for i := range logins {
		if errs[i] != nil {
			t.Fatalf("login %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("concurrent first logins got accounts %v; want one", ids)
		}
	}
	if again := loginGame(t, s, "racer", "party"); again.ID != ids[0] {
		t.Fatalf("a later login got %q, the race settled on %q", again.ID, ids[0])
	}
}

// A 游戏的账号票据与 gt2 在 B 游戏里什么也拿不到：票据换不到 B 的 token（只能给自己的游戏刷新），B 的大厅不认 A 的 token，
// A 账号在服上的角色不是 B 账号的。
func TestTicketsOfOneGameOpenNothingInAnother(t *testing.T) {
	s, _, _ := pendingContractService(t, gameTokenGames)
	ctx := context.Background()
	party := loginGame(t, s, "alice", "party")
	brawl := loginGame(t, s, "alice", "brawl")
	for transport, accounts := range enterClients(t, s) {
		ticket := mustAccountTicket(t, s, party.ID)
		if got, err := accounts.IssueGameToken(ctx, ticket, "brawl"); !errors.Is(err, ErrGameInvalid) || Code(err) != CodeGameInvalid || got.Token != "" {
			t.Fatalf("%s: party's account ticket asked for a brawl token: %+v, %v; want ErrGameInvalid", transport, got, err)
		}
		refreshed, err := accounts.IssueGameToken(ctx, ticket, "party")
		if err != nil {
			t.Fatalf("%s: refreshing party's own token: %v", transport, err)
		}
		got, err := accounts.ValidateGameToken(ctx, refreshed.Token)
		if err != nil || got.AccountID != party.ID || got.Game != "party" {
			t.Fatalf("%s: party token = %+v, %v; want party's account %s", transport, got, err, party.ID)
		}
		if err := got.RequireGame("brawl"); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("%s: party's token in the brawl lobby: %v", transport, err)
		}
	}
	// 签名对得上、写的游戏却不是账号的游戏（只有拿到会话密钥、或存储被改过才会出现）：票据与 token 都按无效回答。
	// 这一格钉住“账号号只在它的游戏里有效”不只靠签名覆盖游戏段。
	exp := strconv.FormatInt(s.cfg.SystemNow().Add(time.Minute).Unix(), 10)
	partyID := base64.RawURLEncoding.EncodeToString([]byte(party.ID))
	if got, err := s.ValidateAccountTicket(ctx, s.signTicket(accountTicketDomain, strings.Join([]string{accountTicketVersion, exp, "brawl", partyID}, "."))); !errors.Is(err, ErrSessionInvalid) || got.ID != "" {
		t.Fatalf("an account ticket naming brawl for party's account: %+v, %v; want ErrSessionInvalid", got, err)
	}
	if got, err := s.ValidateGameToken(ctx, s.signTicket(gameTokenDomain, strings.Join([]string{gameTokenVersion, exp, "brawl", partyID, "0"}, "."))); !errors.Is(err, ErrSessionInvalid) || got.AccountID != "" {
		t.Fatalf("a game token naming brawl for party's account: %+v, %v; want ErrSessionInvalid", got, err)
	}
	// 角色、slot 跟着游戏账号：party 账号在 1 服的角色，brawl 账号问不到。
	role, err := s.CreateRole(ctx, party.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.RoleOnServer(ctx, party.ID, 1); err != nil || got.PlayerID != role.PlayerID {
		t.Fatalf("party's role on server 1: %+v, %v", got, err)
	}
	if got, err := s.RoleOnServer(ctx, brawl.ID, 1); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("brawl's account sees party's role: %+v, %v; want ErrRoleMissing", got, err)
	}
	if _, err := s.SelectRole(ctx, brawl.ID, role.PlayerID); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("brawl's account selected party's role: %v; want ErrNotPermitted", err)
	}
}

// 封号在身份级：由任何一个游戏的账号号封，这个人在所有游戏里都登录不了、票据与 token 当场失效、建不了角；解封之后恢复。
// 别人不受影响。备注必填。
func TestAnIdentityBanBlocksEveryGame(t *testing.T) {
	s, _, _ := pendingContractService(t, gameTokenGames)
	ctx := context.Background()
	party := loginGame(t, s, "alice", "party")
	brawl := loginGame(t, s, "alice", "brawl")
	if party.ID == brawl.ID {
		t.Fatalf("one account %q serves both games; the ban below would prove nothing", party.ID)
	}
	bob := loginGame(t, s, "bob", "brawl")
	brawlTicket := mustAccountTicket(t, s, brawl.ID)
	brawlToken := mustGameToken(t, s, brawl.ID, "brawl").Token
	if _, err := s.SetIdentityBanned(ctx, party.ID, true, ""); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("a ban without a note: %v", err)
	}
	record, err := s.SetIdentityBanned(ctx, party.ID, true, "cheating, ticket 123")
	if err != nil || !record.Banned || record.Accounts["party"] != party.ID || record.Accounts["brawl"] != brawl.ID {
		t.Fatalf("SetIdentityBanned = %+v, %v", record, err)
	}
	for _, game := range []string{"party", "brawl"} {
		if _, err := s.Login(ctx, Identity{Channel: "store", OpenID: "alice", Credential: "good", Game: game}); !errors.Is(err, ErrIdentityDenied) {
			t.Fatalf("a banned identity logged into %s: %v", game, err)
		}
	}
	if _, err := s.ValidateAccountTicket(ctx, brawlTicket); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("brawl's account ticket after a ban through party: %v", err)
	}
	if _, err := s.ValidateGameToken(ctx, brawlToken); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("brawl's game token after a ban through party: %v", err)
	}
	if _, err := s.IssueEnterTicket(ctx, brawl.ID, 1); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("an enter ticket for the banned identity's brawl account: %v", err)
	}
	if _, err := s.CreateRole(ctx, brawl.ID, 1, "Alice"); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("a role for the banned identity's brawl account: %v", err)
	}
	if _, err := s.ValidateGameToken(ctx, mustGameToken(t, s, bob.ID, "brawl").Token); err != nil {
		t.Fatalf("bob after alice's ban: %v", err)
	}
	if _, err := s.SetIdentityBanned(ctx, brawl.ID, false, "appeal accepted"); err != nil {
		t.Fatal(err)
	}
	loginGame(t, s, "alice", "party")
	if _, err := s.ValidateGameToken(ctx, brawlToken); err != nil {
		t.Fatalf("brawl's token after the unban: %v", err)
	}
	if _, err := s.SetIdentityBanned(ctx, "no-such-account", true, "x"); !errors.Is(err, ErrAccountMissing) {
		t.Fatalf("banning a missing account: %v", err)
	}
}

// 凭证版本与 gt2 代数在身份级：在 party 登录时发现密码变了，brawl 的 gt2 也失效；用 party 的账号号手动吊销，brawl 的也失效。
func TestACredentialChangeInOneGameRevokesTheOthers(t *testing.T) {
	s, _, _ := pendingContractService(t, gameTokenGames, credentialVersionVerifier)
	ctx := context.Background()
	party := loginGameWith(t, s, "alice", "party", "good#p1")
	brawl := loginGameWith(t, s, "alice", "brawl", "good#p1")
	if party.ID == brawl.ID {
		t.Fatalf("one account %q serves both games; the revocation below would prove nothing", party.ID)
	}
	brawlToken := mustGameToken(t, s, brawl.ID, "brawl").Token
	loginGameWith(t, s, "alice", "party", "good#p2")
	if _, err := s.ValidateGameToken(ctx, brawlToken); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("brawl's token after the credential changed on a party login: %v; want ErrSessionInvalid", err)
	}
	// 新版本在 brawl 再登录：同一个身份已经记下 p2，不再加一。
	loginGameWith(t, s, "alice", "brawl", "good#p2")
	current := mustGameToken(t, s, brawl.ID, "brawl").Token
	if _, err := s.ValidateGameToken(ctx, current); err != nil {
		t.Fatalf("brawl's new token after logging in with the known version: %v", err)
	}
	if generation, err := s.RevokeGameTokens(ctx, party.ID); err != nil || generation != 2 {
		t.Fatalf("RevokeGameTokens through party = %d, %v; want 2", generation, err)
	}
	if _, err := s.ValidateGameToken(ctx, current); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("brawl's token after revoking through party: %v; want ErrSessionInvalid", err)
	}
}

// 登录必须落到一个游戏：没写游戏而 account 没有缺省游戏（两个游戏、没配 default_game）答 ErrGameInvalid，什么也不建；
// 写了不在 account.games 里的游戏同样拒绝；配了 default_game 时不写游戏落到它；只有一个游戏时它就是缺省；一个游戏都没有的
// account 谁也登录不了。default_game 不在 account.games 里的配置启动不了。
func TestLoginNeedsAGame(t *testing.T) {
	ctx := context.Background()
	two, _, _ := newService(t, gameTokenGames, func(c *Config) { c.DefaultGame = "" })
	for label, game := range map[string]string{"no game": "", "an unknown game": "chess", "an upper-case game": "Party"} {
		if got, err := two.Login(ctx, Identity{Channel: "store", OpenID: "alice", Credential: "good", Game: game}); !errors.Is(err, ErrGameInvalid) || got.ID != "" {
			t.Fatalf("%s with two games and no default: %+v, %v; want ErrGameInvalid", label, got, err)
		}
	}
	defaulted, _, _ := newService(t, gameTokenGames, func(c *Config) { c.DefaultGame = "brawl" })
	if got, err := defaulted.Login(ctx, Identity{Channel: "store", OpenID: "alice", Credential: "good"}); err != nil || got.Game != "brawl" {
		t.Fatalf("no game with default_game brawl: %+v, %v", got, err)
	}
	single, _, _ := newService(t, func(c *Config) { c.Games = []GameSpec{{Name: "slg"}} })
	if got, err := single.Login(ctx, Identity{Channel: "store", OpenID: "alice", Credential: "good"}); err != nil || got.Game != "slg" {
		t.Fatalf("no game with a single configured game: %+v, %v", got, err)
	}
	none, _, _ := newService(t, func(c *Config) { c.Games = nil })
	if got, err := none.Login(ctx, Identity{Channel: "store", OpenID: "alice", Credential: "good"}); !errors.Is(err, ErrGameInvalid) {
		t.Fatalf("an account service with no games: %+v, %v; want ErrGameInvalid", got, err)
	}
	_, _, cfg := newService(t, gameTokenGames)
	cfg.DefaultGame = "chess"
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted a default_game that is not in account.games")
	}
}
