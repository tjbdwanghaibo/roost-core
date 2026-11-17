package account

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// 游戏账号 token（game_token.go，方案 2026-11-16 §4.1.1 与 §5）：凭账号票据换、按游戏签、account 自己验。
// 需求里的数（2h、90 分钟、24h）写死；时钟在 Unix 1700000000。

// gameTokenGames 是场景里的 account.games：party 缺省寿命（2h），brawl 90 分钟。
func gameTokenGames(c *Config) {
	c.Games = []GameSpec{{Name: "party"}, {Name: "brawl", TokenTTL: 90 * time.Minute}}
}

func mustGameToken(t *testing.T, s *Service, accountID, game string) GameToken {
	t.Helper()
	token, err := s.IssueGameToken(context.Background(), mustAccountTicket(t, s, accountID), game)
	if err != nil {
		t.Fatalf("issue a %s token for %s: %v", game, accountID, err)
	}
	return token
}

// 签发与校验：形状 gt2.<过期秒>.<游戏>.<账号>.<代数>.<签名>（从未吊销的账号代数是 0），寿命按游戏；校验答账号、游戏与过期时刻；本地与经总线相同；
// 只读（不写账号记录）。
func TestGameTokensAreIssuedPerGameAndValidatedByAccount(t *testing.T) {
	s, _, cfg := pendingContractService(t, gameTokenGames)
	ctx := context.Background()
	alice := login(t, s, "alice")
	bob := login(t, s, "bob")
	before, _, _ := cfg.Accounts.Get(ctx, alice.ID)
	for transport, accounts := range enterClients(t, s) {
		ticket := mustAccountTicket(t, s, alice.ID)
		party, err := accounts.IssueGameToken(ctx, ticket, "party")
		if err != nil {
			t.Fatalf("%s: %v", transport, err)
		}
		parts := strings.Split(party.Token, ".")
		if len(parts) != 6 || parts[0] != "gt2" || parts[1] != "1700007200" || parts[2] != "party" || parts[4] != "0" || party.Game != "party" || party.ExpiresAtUnix != 1_700_007_200 {
			t.Fatalf("%s: party token = %+v, want gt2.1700007200.party.<account>.0.<signature>", transport, party)
		}
		if decoded, err := base64.RawURLEncoding.DecodeString(parts[3]); err != nil || string(decoded) != alice.ID {
			t.Fatalf("%s: the account segment decodes to %q, %v", transport, decoded, err)
		}
		if signature, err := base64.RawURLEncoding.DecodeString(parts[5]); err != nil || len(signature) != 32 {
			t.Fatalf("%s: the signature is %d bytes, %v", transport, len(signature), err)
		}
		got, err := accounts.ValidateGameToken(ctx, party.Token)
		if err != nil || got != (GameAccount{AccountID: alice.ID, Game: "party", ExpiresAtUnix: 1_700_007_200}) {
			t.Fatalf("%s: ValidateGameToken = %+v, %v", transport, got, err)
		}
		if err := got.RequireGame("party"); err != nil {
			t.Fatalf("%s: the party token for the party lobby: %v", transport, err)
		}
		brawl, err := accounts.IssueGameToken(ctx, ticket, "brawl")
		if err != nil || brawl.ExpiresAtUnix != 1_700_005_400 || !strings.HasPrefix(brawl.Token, "gt2.1700005400.brawl.") {
			t.Fatalf("%s: brawl (90 minutes) = %+v, %v", transport, brawl, err)
		}
		// 各认各的：bob 的 token 是 bob 的。
		if theirs, err := accounts.ValidateGameToken(ctx, mustGameToken(t, s, bob.ID, "party").Token); err != nil || theirs.AccountID != bob.ID {
			t.Fatalf("%s: bob's token = %+v, %v", transport, theirs, err)
		}
	}
	if after, _, _ := cfg.Accounts.Get(ctx, alice.ID); after.Version != before.Version {
		t.Fatalf("issuing or validating a game token wrote the account: version %d -> %d", before.Version, after.Version)
	}
}

// 过期：寿命一到（含正好到点的那一秒）就答 ErrSessionInvalid；寿命是系统时间，业务时间的偏移不影响它。
func TestAGameTokenExpiresOnTheSystemClock(t *testing.T) {
	s, clock, _ := newService(t, gameTokenGames)
	ctx := context.Background()
	alice := login(t, s, "alice")
	token := mustGameToken(t, s, alice.ID, "brawl").Token
	clock.advance(90*time.Minute - time.Second)
	if _, err := s.ValidateGameToken(ctx, token); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	clock.advance(time.Second)
	if got, err := s.ValidateGameToken(ctx, token); !errors.Is(err, ErrSessionInvalid) || Code(err) != 560111 || got.AccountID != "" {
		t.Fatalf("at expiry: %+v, %v; want ErrSessionInvalid", got, err)
	}
}

// 篡改：改任何一段（过期时刻、游戏、账号、代数、签名）、别的密钥签的、截断、超长、旧版本 gt1 的形状，
// 一律 ErrSessionInvalid，错误里不带 token。
func TestATamperedGameTokenIsRefused(t *testing.T) {
	s, _, _ := newService(t, gameTokenGames)
	other, _, _ := newService(t, gameTokenGames, func(c *Config) { c.SessionSecret = "another-secret" })
	ctx := context.Background()
	alice := login(t, s, "alice")
	login(t, other, "alice")
	bob := login(t, s, "bob")
	token := mustGameToken(t, s, alice.ID, "party").Token
	parts := strings.Split(token, ".")
	with := func(index int, value string) string {
		changed := append([]string(nil), parts...)
		changed[index] = value
		return strings.Join(changed, ".")
	}
	cases := map[string]string{
		"a later expiry":           with(1, "1800000000"),
		"another game":             with(2, "brawl"),
		"another account":          with(3, base64.RawURLEncoding.EncodeToString([]byte(bob.ID))),
		"another generation":       with(4, "1"),
		"a generation with a sign": with(4, "+0"),
		"a flipped signature":      with(5, base64.RawURLEncoding.EncodeToString(make([]byte, 32))),
		"signed with another key":  mustGameToken(t, other, alice.ID, "party").Token,
		"no signature":             strings.Join(parts[:5], "."),
		"an extra segment":         token + ".x",
		"another version":          with(0, "gt1"),
		"the next version":         with(0, "gt3"),
		// 旧格式 gt1（五段、没有代数），用同一把密钥、同一个域签的：版本与段数都不对，不再认（不兼容，未上线）。
		"a gt1 signed by this key": s.signTicket(gameTokenDomain, "gt1."+parts[1]+".party."+parts[3]),
		// 版本是 gt2、内容少了代数段（五段），同一把密钥签的：段数不对，不认。
		"a gt2 body without the generation": s.signTicket(gameTokenDomain, strings.Join([]string{"gt2", parts[1], "party", parts[3]}, ".")),
		"longer than the limit":             token + strings.Repeat("A", 1024),
		"empty":                             "",
		"a game name with an upper":         with(2, "Party"),
	}
	for label, forged := range cases {
		got, err := s.ValidateGameToken(ctx, forged)
		if !errors.Is(err, ErrSessionInvalid) || got.AccountID != "" {
			t.Fatalf("%s: %+v, %v; want ErrSessionInvalid", label, got, err)
		}
		if forged != "" && strings.Contains(err.Error(), forged) {
			t.Fatalf("%s: the error repeats the token: %v", label, err)
		}
	}
}

// 冒充：账号票据（at1）、进服票据（et1）、角色会话令牌都不能当游戏账号 token 用；反过来游戏账号 token 也不能当账号票据
// 换别的游戏的 token、拿清单或进服票据。
func TestOtherTicketsCannotPassAsGameTokens(t *testing.T) {
	s, _, _ := newService(t, gameTokenGames)
	ctx := context.Background()
	alice := login(t, s, "alice")
	role, err := s.CreateRole(ctx, alice.ID, 1, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.SelectRole(ctx, alice.ID, role.PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	accountTicket := mustAccountTicket(t, s, alice.ID)
	for label, other := range map[string]string{
		"an account ticket":    accountTicket,
		"an enter ticket":      mustTicket(t, s, alice.ID, 1),
		"a role session token": session.Token,
	} {
		if got, err := s.ValidateGameToken(ctx, other); !errors.Is(err, ErrSessionInvalid) || got.AccountID != "" {
			t.Fatalf("%s as a game token: %+v, %v", label, got, err)
		}
	}
	// 同一把密钥、同样的内容，换个版本号也不行：签名的域不同。
	body := strings.Split(accountTicket, ".")
	if got, err := s.ValidateGameToken(ctx, "gt2."+body[1]+".party."+body[2]+".0."+body[3]); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("an account ticket's signature on a game token body: %+v, %v", got, err)
	}
	game := mustGameToken(t, s, alice.ID, "party").Token
	if got, err := s.IssueGameToken(ctx, game, "brawl"); !errors.Is(err, ErrSessionInvalid) || got.Token != "" {
		t.Fatalf("a game token in place of the account ticket: %+v, %v", got, err)
	}
	if got, err := s.ValidateAccountTicket(ctx, game); !errors.Is(err, ErrSessionInvalid) || got.ID != "" {
		t.Fatalf("a game token as an account ticket: %+v, %v", got, err)
	}
	if entry, err := s.EnterServer(ctx, game); !errors.Is(err, ErrSessionInvalid) || entry.Role.PlayerID != 0 {
		t.Fatalf("a game token as an enter ticket: %+v, %v", entry, err)
	}
}

// 游戏不在 account.games 里：不签发（ErrGameInvalid）；配置里去掉了的游戏，已经签出去的 token 也不再认。
// 没有配置任何游戏的 account 一张也不签。名字、重复、寿命写错的配置启动不了。
func TestOnlyTheConfiguredGamesGetTokens(t *testing.T) {
	s, _, cfg := newService(t, gameTokenGames)
	ctx := context.Background()
	alice := login(t, s, "alice")
	ticket := mustAccountTicket(t, s, alice.ID)
	for _, game := range []string{"chess", "", "PARTY", "party ", "party.v2"} {
		if got, err := s.IssueGameToken(ctx, ticket, game); !errors.Is(err, ErrGameInvalid) || Code(err) != 560120 || got.Token != "" {
			t.Fatalf("game %q: %+v, %v; want ErrGameInvalid", game, got, err)
		}
	}
	party := mustGameToken(t, s, alice.ID, "party").Token
	// 同一套存储、同一把密钥，配置里只剩 brawl：party 的 token 不再认。
	cfg.Games = []GameSpec{{Name: "brawl"}}
	narrowed, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := narrowed.ValidateGameToken(ctx, party); !errors.Is(err, ErrGameInvalid) || got.AccountID != "" {
		t.Fatalf("a token for a game no longer configured: %+v, %v", got, err)
	}
	if got := narrowed.Games(); strings.Join(got, ",") != "brawl" {
		t.Fatalf("Games() = %v", got)
	}
	none, _, _ := newService(t)
	noneAlice := login(t, none, "alice")
	if got, err := none.IssueGameToken(ctx, mustAccountTicket(t, none, noneAlice.ID), "party"); !errors.Is(err, ErrGameInvalid) || got.Token != "" {
		t.Fatalf("an account service without games: %+v, %v", got, err)
	}
	for label, games := range map[string][]GameSpec{
		"a dot in the name":  {{Name: "party.v2"}},
		"an upper-case name": {{Name: "Party"}},
		"listed twice":       {{Name: "party"}, {Name: "party"}},
		"a negative ttl":     {{Name: "party", TokenTTL: -time.Second}},
		"over 24h":           {{Name: "party", TokenTTL: 24*time.Hour + time.Second}},
		"a 33-byte name":     {{Name: "p" + strings.Repeat("a", 32)}},
	} {
		bad := cfg
		bad.Games = games
		if _, err := New(bad); err == nil {
			t.Fatalf("%s: New accepted account.games %+v", label, games)
		}
	}
	edge := cfg
	edge.Games = []GameSpec{{Name: "p" + strings.Repeat("a", 31), TokenTTL: 24 * time.Hour}}
	if _, err := New(edge); err != nil {
		t.Fatalf("a 32-byte name with a 24h ttl is within the limits: %v", err)
	}
}

// 拿 A 游戏的 token 进 B 游戏：token 本身是好的（ValidateGameToken 答 A），调用方按自己的游戏核对时被拒，
// 回答与坏 token 相同（ErrSessionInvalid）。
func TestAGameTokenOnlyOpensItsOwnGame(t *testing.T) {
	s, _, _ := newService(t, gameTokenGames)
	ctx := context.Background()
	alice := login(t, s, "alice")
	party := mustGameToken(t, s, alice.ID, "party").Token
	got, err := s.ValidateGameToken(ctx, party)
	if err != nil || got.Game != "party" {
		t.Fatalf("validate: %+v, %v", got, err)
	}
	for _, game := range []string{"brawl", "", "Party"} {
		if err := got.RequireGame(game); !errors.Is(err, ErrSessionInvalid) || Code(err) != 560111 {
			t.Fatalf("the party token for the %q lobby: %v; want ErrSessionInvalid", game, err)
		}
	}
	if err := (GameAccount{}).RequireGame(""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("an empty result passed an empty game: %v", err)
	}
}

// 封号当场生效：已经签出去的 token 校验时再读一次账号，被封答 ErrIdentityDenied；被封的账号也换不到新 token。
// 账号记录没了答 ErrAccountMissing。
func TestABanRefusesGameTokensAtOnce(t *testing.T) {
	s, _, cfg := pendingContractService(t, gameTokenGames)
	ctx := context.Background()
	alice := login(t, s, "alice")
	ticket := mustAccountTicket(t, s, alice.ID)
	token := mustGameToken(t, s, alice.ID, "party").Token
	banAccount(t, cfg, alice.ID)
	for transport, accounts := range enterClients(t, s) {
		if got, err := accounts.ValidateGameToken(ctx, token); !errors.Is(err, ErrIdentityDenied) || Code(err) != 560102 || got.AccountID != "" {
			t.Fatalf("%s: a banned account's game token: %+v, %v", transport, got, err)
		}
		if got, err := accounts.IssueGameToken(ctx, ticket, "party"); !errors.Is(err, ErrIdentityDenied) || got.Token != "" {
			t.Fatalf("%s: a game token for a banned account: %+v, %v", transport, got, err)
		}
	}
	bob := login(t, s, "bob")
	bobToken := mustGameToken(t, s, bob.ID, "party").Token
	record, _, err := cfg.Accounts.Get(ctx, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Accounts.Delete(ctx, bob.ID, record); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ValidateGameToken(ctx, bobToken); !errors.Is(err, ErrAccountMissing) || got.AccountID != "" {
		t.Fatalf("a game token whose account is gone: %+v, %v", got, err)
	}
}

// 按账号吊销（交接 C6，维护者 2026-11-17 选“代数”）：RevokeGameTokens 之后，在此之前签发的 token 一律
// ErrSessionInvalid，之后签发的带新代数、照常可用；别的账号不受影响。本地与经总线相同。代数落在账号记录上：
// 同一套存储上另起的 account（重启、别的实例）也拒绝旧 token。再吊销一次，上一代也失效。
func TestRevokingGameTokensRefusesTheOnesIssuedBefore(t *testing.T) {
	s, _, cfg := pendingContractService(t, gameTokenGames)
	ctx := context.Background()
	for transport, accounts := range enterClients(t, s) {
		alice := login(t, s, "alice-"+transport)
		bob := login(t, s, "bob-"+transport)
		before := mustGameToken(t, s, alice.ID, "party").Token
		brawlBefore := mustGameToken(t, s, alice.ID, "brawl").Token
		bobs := mustGameToken(t, s, bob.ID, "party").Token
		if _, err := accounts.ValidateGameToken(ctx, before); err != nil {
			t.Fatalf("%s: before revoking: %v", transport, err)
		}
		generation, err := accounts.RevokeGameTokens(ctx, alice.ID)
		if err != nil || generation != 1 {
			t.Fatalf("%s: RevokeGameTokens = %d, %v; want generation 1", transport, generation, err)
		}
		for label, token := range map[string]string{"party": before, "brawl": brawlBefore} {
			if got, err := accounts.ValidateGameToken(ctx, token); !errors.Is(err, ErrSessionInvalid) || Code(err) != 560111 || got.AccountID != "" {
				t.Fatalf("%s: alice's %s token issued before the revocation: %+v, %v; want ErrSessionInvalid", transport, label, got, err)
			}
		}
		// 把旧 token 的代数段改成当前代数：签名盖着代数段，对不上。
		forged := strings.Split(before, ".")
		forged[4] = "1"
		if got, err := accounts.ValidateGameToken(ctx, strings.Join(forged, ".")); !errors.Is(err, ErrSessionInvalid) || got.AccountID != "" {
			t.Fatalf("%s: a revoked token with its generation rewritten to the current one: %+v, %v", transport, got, err)
		}
		if got, err := accounts.ValidateGameToken(ctx, bobs); err != nil || got.AccountID != bob.ID {
			t.Fatalf("%s: bob's token after alice's revocation: %+v, %v", transport, got, err)
		}
		after, err := accounts.IssueGameToken(ctx, mustAccountTicket(t, s, alice.ID), "party")
		if err != nil || strings.Split(after.Token, ".")[4] != "1" {
			t.Fatalf("%s: a token issued after the revocation = %+v, %v; want generation 1 in it", transport, after, err)
		}
		if got, err := accounts.ValidateGameToken(ctx, after.Token); err != nil || got.AccountID != alice.ID {
			t.Fatalf("%s: a token issued after the revocation: %+v, %v", transport, got, err)
		}
		// 代数在账号记录上，不在这个进程里。
		record, _, err := cfg.Accounts.Get(ctx, alice.ID)
		if err != nil || record.Value.GameTokenGeneration != 1 {
			t.Fatalf("%s: the stored generation is %d, %v; want 1", transport, record.Value.GameTokenGeneration, err)
		}
		restarted, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := restarted.ValidateGameToken(ctx, before); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("%s: another account instance on the same store took the revoked token: %v", transport, err)
		}
		if got, err := restarted.ValidateGameToken(ctx, after.Token); err != nil || got.AccountID != alice.ID {
			t.Fatalf("%s: another account instance refused the current token: %+v, %v", transport, got, err)
		}
		if generation, err := accounts.RevokeGameTokens(ctx, alice.ID); err != nil || generation != 2 {
			t.Fatalf("%s: the second revocation = %d, %v; want 2", transport, generation, err)
		}
		if _, err := accounts.ValidateGameToken(ctx, after.Token); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("%s: generation 1 after the second revocation: %v", transport, err)
		}
	}
}

// 吊销不碰别的：账号票据仍然换得到新 token；封号的回答不变（被封的账号吊销照样成功，校验答 ErrIdentityDenied，
// 解封之后吊销之前签的 token 也不复活）。没有这个账号、空账号答 ErrAccountMissing，什么也不写。
func TestRevokingGameTokensLeavesBansAndTicketsAlone(t *testing.T) {
	s, _, cfg := pendingContractService(t, gameTokenGames)
	ctx := context.Background()
	alice := login(t, s, "alice")
	ticket := mustAccountTicket(t, s, alice.ID)
	before := mustGameToken(t, s, alice.ID, "party").Token
	banAccount(t, cfg, alice.ID)
	if generation, err := s.RevokeGameTokens(ctx, alice.ID); err != nil || generation != 1 {
		t.Fatalf("revoking a banned account: %d, %v", generation, err)
	}
	if _, err := s.ValidateGameToken(ctx, before); !errors.Is(err, ErrIdentityDenied) {
		t.Fatalf("a banned account's revoked token: %v; want the ban's ErrIdentityDenied", err)
	}
	if _, _, err := cfg.Accounts.Update(ctx, alice.ID, func(current Account, found bool) (Account, bool, error) {
		current.Banned = false
		return current, found, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateGameToken(ctx, before); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("a token revoked while banned came back after the unban: %v", err)
	}
	got, err := s.IssueGameToken(ctx, ticket, "party")
	if err != nil {
		t.Fatalf("the account ticket after the revocation: %+v, %v", got, err)
	}
	if _, err := s.ValidateGameToken(ctx, got.Token); err != nil {
		t.Fatalf("a token from the same account ticket after the revocation: %v", err)
	}
	for _, accountID := range []string{"store:nobody", "", "  "} {
		if generation, err := s.RevokeGameTokens(ctx, accountID); !errors.Is(err, ErrAccountMissing) || generation != 0 {
			t.Fatalf("revoking %q: %d, %v; want ErrAccountMissing", accountID, generation, err)
		}
	}
	if _, found, err := cfg.Accounts.Get(ctx, "store:nobody"); err != nil || found {
		t.Fatalf("revoking a missing account created it: found %v, %v", found, err)
	}
}
