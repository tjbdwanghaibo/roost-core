package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 票据（tickets.go）与进服（enter.go）：账号票据、进服票据都由 account 签发与校验；握手的认证器凭进服票据
// 解析“账号 ↔ 玩家号”的映射，没有就新建，不带名字。
// 存储缺省是 Memory；ROOST_REVIEW3_BACKEND=redis 时走隔离的 Redis（pendingContractService）。
// 需求里的数（10 分钟、30 分钟……）写死。

// enterClients 给出同一个服务的两种调用方式：本地与经总线。
func enterClients(t *testing.T, service *Service) map[string]Accounts {
	t.Helper()
	loopback := &loopbackBus{handlers: map[string]bus.RpcHandlerFunc{}}
	if err := RegisterHandlers(loopback, service); err != nil {
		t.Fatal(err)
	}
	client, err := NewBusClient(loopback, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Accounts{"local": service, "bus": client}
}

func mustTicket(t *testing.T, s *Service, accountID string, serverID int32) string {
	t.Helper()
	ticket, err := s.IssueEnterTicket(context.Background(), accountID, serverID)
	if err != nil {
		t.Fatalf("issue an enter ticket for %s on %d: %v", accountID, serverID, err)
	}
	return ticket.Ticket
}

func mustAccountTicket(t *testing.T, s *Service, accountID string) string {
	t.Helper()
	ticket, err := s.IssueAccountTicket(context.Background(), accountID)
	if err != nil {
		t.Fatalf("issue an account ticket for %s: %v", accountID, err)
	}
	return ticket.Ticket
}

func banAccount(t *testing.T, cfg Config, accountID string) {
	t.Helper()
	if _, _, err := cfg.Accounts.Update(context.Background(), accountID, func(current Account, found bool) (Account, bool, error) {
		if !found {
			return current, false, errors.New("no such account")
		}
		current.Banned = true
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 账号票据：account 签、account 验。形状 at1.<过期秒>.<账号>.<签名>，缺省 10 分钟；校验返回它绑定的账号；
// 签发只读；不存在或被封的账号签不出来。有效期可以另配，与会话的有效期是两个量。
func TestAccountTicketsAreIssuedAndValidatedByAccount(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	account := login(t, s, "alice")
	other := login(t, s, "bob")
	before, _, _ := cfg.Accounts.Get(ctx, account.ID)
	for transport, accounts := range enterClients(t, s) {
		issued, err := accounts.IssueAccountTicket(ctx, account.ID)
		if err != nil {
			t.Fatalf("%s: %v", transport, err)
		}
		parts := strings.Split(issued.Ticket, ".")
		// 时钟在 Unix 1700000000。
		if len(parts) != 4 || parts[0] != "at1" || parts[1] != "1700000600" || issued.ExpiresAtUnix != 1_700_000_600 {
			t.Fatalf("%s: ticket = %q (expires %d), want at1.1700000600.<account>.<signature>", transport, issued.Ticket, issued.ExpiresAtUnix)
		}
		if decoded, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil || string(decoded) != account.ID {
			t.Fatalf("%s: the account segment decodes to %q, %v", transport, decoded, err)
		}
		if signature, err := base64.RawURLEncoding.DecodeString(parts[3]); err != nil || len(signature) != 32 {
			t.Fatalf("%s: the signature is %d bytes, %v", transport, len(signature), err)
		}
		validated, err := accounts.ValidateAccountTicket(ctx, issued.Ticket)
		if err != nil || validated.ID != account.ID || validated.OpenID != "alice" {
			t.Fatalf("%s: ValidateAccountTicket = %+v, %v", transport, validated, err)
		}
		// 各认各的。
		if theirs, err := accounts.ValidateAccountTicket(ctx, mustAccountTicket(t, s, other.ID)); err != nil || theirs.ID != other.ID {
			t.Fatalf("%s: bob's ticket = %+v, %v", transport, theirs, err)
		}
		for label, accountID := range map[string]string{"an account that never logged in": "store:nobody", "an empty account id": "  "} {
			if got, err := accounts.IssueAccountTicket(ctx, accountID); Code(err) != 560103 || got.Ticket != "" {
				t.Fatalf("%s: %s: %+v, %v; want ErrAccountMissing and no ticket", transport, label, got, err)
			}
		}
	}
	if after, _, _ := cfg.Accounts.Get(ctx, account.ID); after.Version != before.Version {
		t.Fatalf("issuing or validating a ticket wrote the account: version %d -> %d", before.Version, after.Version)
	}
	// 另配的有效期只管账号票据，进服票据仍按会话的有效期。
	short, _, _ := newService(t, func(c *Config) { c.AccountTicketTTL = 2 * time.Minute })
	shortAccount := login(t, short, "alice")
	if issued, err := short.IssueAccountTicket(ctx, shortAccount.ID); err != nil || issued.ExpiresAtUnix != 1_700_000_120 {
		t.Fatalf("AccountTicketTTL of 2 minutes: %+v, %v", issued, err)
	}
	if issued, err := short.IssueEnterTicket(ctx, shortAccount.ID, 1); err != nil || issued.ExpiresAtUnix != 1_700_001_800 {
		t.Fatalf("the enter ticket's lifetime followed the account ticket's: %+v, %v", issued, err)
	}
}

// 封号对已经签出去的账号票据立即生效：校验时再读一次账号。被封的账号也签不出新的票据。
func TestABanTakesEffectOnTicketsAlreadyIssued(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	account := login(t, s, "alice")
	ticket := mustAccountTicket(t, s, account.ID)
	enter := mustTicket(t, s, account.ID, 1)
	banAccount(t, cfg, account.ID)
	if got, err := s.ValidateAccountTicket(ctx, ticket); !errors.Is(err, ErrIdentityDenied) || Code(err) != 560102 || got.ID != "" {
		t.Fatalf("a banned account's ticket: %+v, %v", got, err)
	}
	if got, err := s.IssueAccountTicket(ctx, account.ID); !errors.Is(err, ErrIdentityDenied) || got.Ticket != "" {
		t.Fatalf("issuing to a banned account: %+v, %v", got, err)
	}
	if got, err := s.IssueEnterTicket(ctx, account.ID, 1); !errors.Is(err, ErrIdentityDenied) || got.Ticket != "" {
		t.Fatalf("an enter ticket for a banned account: %+v, %v", got, err)
	}
	if entry, err := s.EnterServer(ctx, enter); !errors.Is(err, ErrIdentityDenied) || entry.Role.PlayerID != 0 {
		t.Fatalf("a banned account entering with an old ticket: %+v, %v", entry, err)
	}
	if _, found := slotAt(t, cfg, account.ID, 1); found {
		t.Fatal("a banned account's enter left a slot")
	}
}

// 无效的票据一律 ErrSessionInvalid、回答一模一样、什么也不写。两种票据各过一遍同样的清单：改动任何一个字符、
// 换内容留旧签名、另一个版本（签名无误）、不带域前缀或带着另一种票据的域签的、过期、别的密钥、超长；
// 三种凭证（账号票据、进服票据、角色会话令牌）互相冒充都过不了。
func TestEveryBadTicketIsRefusedTheSameWay(t *testing.T) {
	s, clock, cfg := pendingContractService(t)
	ctx := context.Background()
	account := login(t, s, "alice")
	role, session := roleOn(t, s, "bob", 1, "Bob")
	other, _, _ := newService(t, func(c *Config) { c.SessionSecret = "another-session-secret" })
	otherAccount := login(t, other, "alice")
	accountTicket, enterTicket := mustAccountTicket(t, s, account.ID), mustTicket(t, s, account.ID, 1)
	sign := func(domain, body string) string {
		mac := hmac.New(sha256.New, []byte(cfg.SessionSecret))
		mac.Write([]byte(domain))
		mac.Write([]byte(body))
		return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	snapshot := func() string {
		_, slot := slotAt(t, cfg, account.ID, 1)
		return fmt.Sprintf("slot=%v bob.version=%d", slot, storedRole(t, cfg, role.PlayerID).Version)
	}
	before := snapshot()

	type kind struct {
		name, ticket, version, domain, otherDomain string
		foreign                                    string
		present                                    func(string) error
	}
	kinds := []kind{
		{
			name: "account ticket", ticket: accountTicket, version: "at1", domain: "roost.account.account-ticket\n", otherDomain: "roost.account.enter-ticket\n",
			foreign: mustAccountTicket(t, other, otherAccount.ID),
			present: func(ticket string) error {
				got, err := s.ValidateAccountTicket(ctx, ticket)
				if got.ID != "" {
					return fmt.Errorf("an account was returned: %+v", got)
				}
				return err
			},
		},
		{
			name: "enter ticket", ticket: enterTicket, version: "et1", domain: "roost.account.enter-ticket\n", otherDomain: "roost.account.account-ticket\n",
			foreign: mustTicket(t, other, otherAccount.ID, 1),
			present: func(ticket string) error {
				entry, err := s.EnterServer(ctx, ticket)
				if entry.Role.PlayerID != 0 || entry.Created {
					return fmt.Errorf("an entry was returned: %+v", entry)
				}
				return err
			},
		},
	}
	var common string
	for _, k := range kinds {
		refused := func(label, bad string) {
			t.Helper()
			err := k.present(bad)
			if !errors.Is(err, ErrSessionInvalid) || Code(err) != 560111 {
				t.Fatalf("%s, %s: %v; want ErrSessionInvalid", k.name, label, err)
			}
			if common == "" {
				common = err.Error()
			}
			if err.Error() != common {
				t.Fatalf("%s, %s: the refusal %q differs from %q", k.name, label, err, common)
			}
			if strings.Contains(err.Error(), k.version+".") || strings.Contains(err.Error(), account.ID) {
				t.Fatalf("%s, %s: the refusal carries the ticket or the account: %v", k.name, label, err)
			}
		}
		// 夹具自检：没改动的票据验得过（只验票据本身，不进服——下面要断言零写入）。
		if _, ok := s.verifyTicket(k.domain, k.version, len(strings.Split(k.ticket, ".")), k.ticket); !ok {
			t.Fatalf("%s: fixture: the good ticket does not verify", k.name)
		}
		for index := range k.ticket {
			replacement := byte('A')
			if k.ticket[index] == 'A' {
				replacement = 'B'
			}
			refused(fmt.Sprintf("changed at byte %d", index), k.ticket[:index]+string(replacement)+k.ticket[index+1:])
		}
		parts := strings.Split(k.ticket, ".")
		last := len(parts) - 1
		swapped := append([]string(nil), parts...)
		swapped[last-1] = base64.RawURLEncoding.EncodeToString([]byte(role.AccountID))
		refused("another account under the same signature", strings.Join(swapped, "."))
		later := append([]string(nil), parts...)
		later[1] = "1900000000"
		refused("a later expiry under the same signature", strings.Join(later, "."))
		body := strings.Join(parts[:last], ".")
		nextVersion := strings.Replace(body, k.version, k.version[:2]+"2", 1)
		refused("a correctly signed ticket of another version", sign(k.domain, nextVersion))
		refused("signed without the domain prefix", sign("", body))
		refused("signed under the other ticket's domain", sign(k.otherDomain, body))
		refused("an empty ticket", "")
		refused("one segment short", body)
		// 签名无误、账号段是空的：内容不成样子也是无效的票据，不是“账号不存在”。
		refused("a correctly signed ticket without an account", sign(k.domain, strings.Join(parts[:last-1], ".")+"."))
		refused("a role session token", session.Token)
		refused("a session handshake ticket", "session:"+session.Token)
		refused("an oversized ticket", k.ticket+strings.Repeat("A", 1025))
		refused("signed by another secret", k.foreign)
	}
	// 三种凭证互不冒充：账号票据当进服票据、进服票据当账号票据、两者当角色会话令牌。
	if entry, err := s.EnterServer(ctx, accountTicket); !errors.Is(err, ErrSessionInvalid) || entry.Role.PlayerID != 0 {
		t.Fatalf("an account ticket entered a server: %+v, %v", entry, err)
	}
	if got, err := s.ValidateAccountTicket(ctx, enterTicket); !errors.Is(err, ErrSessionInvalid) || got.ID != "" {
		t.Fatalf("an enter ticket validated as an account ticket: %+v, %v", got, err)
	}
	for label, ticket := range map[string]string{"an account ticket": accountTicket, "an enter ticket": enterTicket} {
		if _, err := s.ValidateSession(ctx, role.PlayerID, ticket); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("%s passed as a role session token: %v", label, err)
		}
	}

	// 寿命：账号票据 10 分钟，进服票据 30 分钟；差一秒到期还能用，到点起不能用。
	clock.advance(10*time.Minute - time.Second)
	if _, err := s.ValidateAccountTicket(ctx, accountTicket); err != nil {
		t.Fatalf("an account ticket one second before expiry: %v", err)
	}
	clock.advance(time.Second)
	if _, err := s.ValidateAccountTicket(ctx, accountTicket); !errors.Is(err, ErrSessionInvalid) || err.Error() != common {
		t.Fatalf("an account ticket at its expiry second: %v", err)
	}
	clock.advance(20 * time.Minute)
	if _, err := s.EnterServer(ctx, enterTicket); !errors.Is(err, ErrSessionInvalid) || err.Error() != common {
		t.Fatalf("an enter ticket at its expiry second: %v", err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("refused tickets wrote something: %s -> %s", before, after)
	}
	fresh := mustTicket(t, s, account.ID, 1)
	clock.advance(30*time.Minute - time.Second)
	if entry, err := s.EnterServer(ctx, fresh); err != nil || !entry.Created {
		t.Fatalf("an enter ticket one second before expiry: %+v, %v", entry, err)
	}
}

// 进服票据的形状、绑定与寿命；签发是只读的；拒绝的几种情况。区服的状态不影响签发，合服后带出承载服。
func TestIssueEnterTicketBindsTheAccountTheServerAndTheSessionLifetime(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	if _, err := s.UpsertServer(ctx, GameServer{ID: 2, Name: "s2", Status: ServerMaintenance}); err != nil {
		t.Fatal(err)
	}
	upsertOpen(t, s, 3)
	mustMerge(t, s, 3, 1)
	account := login(t, s, "alice")
	before, _, _ := cfg.Accounts.Get(ctx, account.ID)

	for transport, accounts := range enterClients(t, s) {
		issued, err := accounts.IssueEnterTicket(ctx, account.ID, 1)
		if err != nil {
			t.Fatalf("%s: %v", transport, err)
		}
		parts := strings.Split(issued.Ticket, ".")
		// 时钟在 Unix 1700000000；会话有效期缺省 30 分钟。
		if len(parts) != 5 || parts[0] != "et1" || parts[1] != "1700001800" || parts[2] != "1" || issued.ExpiresAtUnix != 1_700_001_800 {
			t.Fatalf("%s: ticket = %q (expires %d), want et1.1700001800.1.<account>.<signature>", transport, issued.Ticket, issued.ExpiresAtUnix)
		}
		if decoded, err := base64.RawURLEncoding.DecodeString(parts[3]); err != nil || string(decoded) != account.ID {
			t.Fatalf("%s: the account segment decodes to %q, %v", transport, decoded, err)
		}
		if issued.ServerID != 1 || issued.ServingServerID != 1 {
			t.Fatalf("%s: issued = %+v", transport, issued)
		}
		// 维护中的服照常签；并入 1 的 3 服照实签给 3，带出承载服 1。
		if maintenance, err := accounts.IssueEnterTicket(ctx, account.ID, 2); err != nil || maintenance.ServingServerID != 2 {
			t.Fatalf("%s: a server in maintenance: %+v, %v", transport, maintenance, err)
		}
		merged, err := accounts.IssueEnterTicket(ctx, account.ID, 3)
		if err != nil || merged.ServerID != 3 || merged.ServingServerID != 1 || !strings.HasPrefix(merged.Ticket, "et1.1700001800.3.") {
			t.Fatalf("%s: a merged server: %+v, %v; want a ticket for 3 served by 1", transport, merged, err)
		}
		for label, ask := range map[string]struct {
			accountID string
			serverID  int32
			code      int32
		}{
			"an account that never logged in": {"store:nobody", 1, 560103},
			"an empty account id":             {" ", 1, 560103},
			"a server nobody registered":      {account.ID, 77, 560104},
			"server id zero":                  {account.ID, 0, 560104},
			"a negative server id":            {account.ID, -1, 560104},
		} {
			got, err := accounts.IssueEnterTicket(ctx, ask.accountID, ask.serverID)
			if Code(err) != ask.code || got.Ticket != "" {
				t.Fatalf("%s: %s: %+v, %v; want code %d and no ticket", transport, label, got, err, ask.code)
			}
		}
	}
	// 只读：没有建映射、没有 slot、账号记录没动。
	if _, err := s.RoleOnServer(ctx, account.ID, 1); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("issuing a ticket created a role: %v", err)
	}
	if _, found := slotAt(t, cfg, account.ID, 1); found {
		t.Fatal("issuing a ticket created a slot")
	}
	if after, _, _ := cfg.Accounts.Get(ctx, account.ID); after.Version != before.Version {
		t.Fatalf("issuing a ticket wrote the account: version %d -> %d", before.Version, after.Version)
	}
	// 承载服解析不了（并入一个没登记的服）时不签。
	if _, _, err := cfg.Servers.Update(ctx, 2, func(current GameServer, _ bool) (GameServer, bool, error) {
		current.MergedInto = 99
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.IssueEnterTicket(ctx, account.ID, 2); !errors.Is(err, ErrServerInvalid) || got.Ticket != "" {
		t.Fatalf("a server merged into an unknown one: %+v, %v", got, err)
	}
	// PeekEnterTicket 只读服号、不验证：伪造的票据也读得出来，但进不了服。
	issued := mustTicket(t, s, account.ID, 1)
	if sid, ok := PeekEnterTicket(issued); !ok || sid != 1 {
		t.Fatalf("PeekEnterTicket = (%d, %v)", sid, ok)
	}
	parts := strings.Split(issued, ".")
	forged := strings.Join([]string{parts[0], parts[1], "7", parts[3], "AAAA"}, ".")
	if sid, ok := PeekEnterTicket(forged); !ok || sid != 7 {
		t.Fatalf("a forged ticket peeks as (%d, %v); peeking does not verify", sid, ok)
	}
	if _, err := s.EnterServer(ctx, forged); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("the forged ticket entered: %v", err)
	}
	for label, bad := range map[string]string{
		"empty": "", "an account ticket": mustAccountTicket(t, s, account.ID), "another version": "et2" + issued[3:],
		"server id zero":     strings.Join([]string{parts[0], parts[1], "0", parts[3], parts[4]}, "."),
		"a non-canonical id": strings.Join([]string{parts[0], parts[1], "01", parts[3], parts[4]}, "."),
		"an empty account":   strings.Join([]string{parts[0], parts[1], parts[2], "", parts[4]}, "."),
		"oversized":          issued + strings.Repeat("A", 1025),
	} {
		if sid, ok := PeekEnterTicket(bad); ok {
			t.Fatalf("%s: PeekEnterTicket = (%d, true)", label, sid)
		}
	}
}

// 新号凭票进服：建出“账号 ↔ 玩家号”的映射，Created 为 true，记一次登录；它没有名字，也没有碰名字目录。
// 同一张票据再来（重连）是同一个玩家号、Created 为 false。之后 RoleOnServer 问得到，
// “入口自己选角”那条路（SelectRole / ValidateSession）照样认它；带名字的 CreateRole 在这个名额上是名额已占。
// 反过来，CreateRole 建的带名字的角色，凭票进服拿到的就是它。
func TestEnterServerCreatesTheMappingOnceThenFindsIt(t *testing.T) {
	s, clock, cfg := pendingContractService(t)
	ctx := context.Background()
	account := login(t, s, "alice")
	ticket := mustTicket(t, s, account.ID, 1)
	clients := enterClients(t, s)

	entry, err := clients["bus"].EnterServer(ctx, ticket)
	if err != nil {
		t.Fatalf("the first enter: %v", err)
	}
	role := entry.Role
	if !entry.Created || role.PlayerID == 0 || role.Name != "" || role.AccountID != account.ID || role.ServerID != 1 || role.ServingServerID != 1 ||
		role.CreatedAtUnix != 1_700_000_000 || role.LastLoginAtUnix != 1_700_000_000 || role.BindEpoch != 0 || role.Transfer != nil || role.CreationID == "" {
		t.Fatalf("the first enter = %+v (created %v), want a new nameless role on server 1 that just logged in", role, entry.Created)
	}
	slot, found := slotAt(t, cfg, account.ID, 1)
	if !found || slot.PlayerID != role.PlayerID || slot.Creation.Name != "" || slot.Creation.Admitted || slot.Creation.Claim.Token != "" {
		t.Fatalf("the slot = %+v; want a published plan without a name and without a name claim", slot)
	}
	clock.advance(5 * time.Minute)
	for transport, accounts := range clients {
		again, err := accounts.EnterServer(ctx, ticket)
		if err != nil {
			t.Fatalf("%s: entering again with the same ticket: %v", transport, err)
		}
		if again.Created || again.Role.PlayerID != role.PlayerID || again.Role.LastLoginAtUnix != 1_700_000_300 {
			t.Fatalf("%s: the second enter = %+v (created %v), want the same role, not created, with a fresh login time", transport, again.Role, again.Created)
		}
	}
	if fresh, err := s.EnterServer(ctx, mustTicket(t, s, account.ID, 1)); err != nil || fresh.Created || fresh.Role.PlayerID != role.PlayerID {
		t.Fatalf("a fresh ticket: %+v, %v", fresh, err)
	}
	if found, err := s.RoleOnServer(ctx, account.ID, 1); err != nil || found.PlayerID != role.PlayerID {
		t.Fatalf("RoleOnServer after entering = %+v, %v", found, err)
	}
	session, err := s.SelectRole(ctx, account.ID, role.PlayerID)
	if err != nil {
		t.Fatalf("SelectRole of a mapping made by EnterServer: %v", err)
	}
	if validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token); err != nil || validated.PlayerID != role.PlayerID {
		t.Fatalf("ValidateSession: %+v, %v", validated, err)
	}
	// 这个名额已经有角色：带名字的 CreateRole 是名额已占。
	if _, err := s.CreateRole(ctx, account.ID, 1, "Hero"); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("CreateRole on a slot EnterServer filled: %v, want ErrRoleLimit", err)
	}
	// 同一个账号在另一个服是另一个玩家号。
	upsertOpen(t, s, 2)
	second, err := s.EnterServer(ctx, mustTicket(t, s, account.ID, 2))
	if err != nil || !second.Created || second.Role.PlayerID == role.PlayerID || second.Role.ServerID != 2 {
		t.Fatalf("the same account on server 2: %+v, %v", second, err)
	}
	// 一张票据只进它绑定的服。
	if mine, err := s.EnterServer(ctx, ticket); err != nil || mine.Role.ServerID != 1 || mine.Role.PlayerID != role.PlayerID {
		t.Fatalf("the ticket for server 1 answered %+v, %v", mine, err)
	}
	// CreateRole 建的带名字的角色：凭票进服就是它，名字留着。
	named, _ := roleOn(t, s, "carol", 1, "Carol")
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, named.AccountID, 1)); err != nil || entry.Created || entry.Role.PlayerID != named.PlayerID || entry.Role.Name != "Carol" {
		t.Fatalf("entering a role CreateRole made: %+v, %v", entry, err)
	}
}

// 同一张票据并发握手：只有一个映射，所有调用拿到的是同一个玩家号，恰好一个 Created。
func TestConcurrentEntriesWithOneTicketCreateOneMapping(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	account := login(t, s, "alice")
	ticket := mustTicket(t, s, account.ID, 1)
	const callers = 16
	entries := make([]ServerEntry, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			entries[i], errs[i] = s.EnterServer(ctx, ticket)
		}()
	}
	close(start)
	wg.Wait()
	created, playerID := 0, int64(0)
	for i := range callers {
		if errs[i] != nil {
			// 存储竞争可以让个别调用答“稍后重试”，但不能答别的，更不能建出第二个映射。
			if !errors.Is(errs[i], ErrConflict) && !errors.Is(errs[i], versionstore.ErrConflict) {
				t.Fatalf("caller %d: %v", i, errs[i])
			}
			continue
		}
		if playerID == 0 {
			playerID = entries[i].Role.PlayerID
		}
		if entries[i].Role.PlayerID != playerID {
			t.Fatalf("caller %d got player %d, want %d", i, entries[i].Role.PlayerID, playerID)
		}
		if entries[i].Created {
			created++
		}
	}
	if playerID == 0 || created != 1 {
		t.Fatalf("%d callers reported Created, want exactly one (player %d)", created, playerID)
	}
	slot, found := slotAt(t, cfg, account.ID, 1)
	if !found || slot.PlayerID != playerID {
		t.Fatalf("the slot = %+v (found %v), want it to name player %d", slot, found, playerID)
	}
	if again, err := s.EnterServer(ctx, ticket); err != nil || again.Created || again.Role.PlayerID != playerID {
		t.Fatalf("entering afterwards: %+v, %v", again, err)
	}
}

// 建映射做到一半停下（角色记录写下了、应答丢了，slot 还没发布）：下一次进服接上同一个计划把它做完，
// 拿到的是同一个玩家号——不会卡住，也不会建出第二个。停着的这段时间里，带名字的 CreateRole 是名额已占，
// 运维也不能把它当悬挂的计划释放。
func TestEnterServerFinishesAMappingThatStoppedHalfway(t *testing.T) {
	var roles *lostReplyRoleCreate
	s, _, cfg := pendingContractService(t, func(c *Config) {
		roles = &lostReplyRoleCreate{Store: c.Roles}
		c.Roles = roles
	})
	ctx := context.Background()
	account := login(t, s, "alice")
	ticket := mustTicket(t, s, account.ID, 1)
	if entry, err := s.EnterServer(ctx, ticket); err == nil || entry.Role.PlayerID != 0 {
		t.Fatalf("fixture: the first enter was supposed to lose its reply: %+v, %v", entry, err)
	}
	pending := mustSlot(t, cfg, account.ID)
	if pending.PlayerID != 0 || pending.Creation.Name != "" || roles.first == 0 || pending.Creation.PlayerID != roles.first {
		t.Fatalf("fixture: want an unnamed pending plan with the role record written, got %+v (role %d)", pending, roles.first)
	}
	if _, err := s.RoleOnServer(ctx, account.ID, 1); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("an unpublished mapping answered RoleOnServer: %v", err)
	}
	for _, name := range []string{"Hero", "Knight"} {
		if _, err := s.CreateRole(ctx, account.ID, 1, name); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("CreateRole(%s) while EnterServer's plan is pending: %v, want ErrRoleLimit", name, err)
		}
	}
	if _, err := s.ResolvePendingCreation(ctx, account.ID, 1, "operator tries to release it"); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("an operator released an unnamed plan: %v", err)
	}
	if still := mustSlot(t, cfg, account.ID); still.Creation.ID != pending.Creation.ID || still.PlayerID != 0 {
		t.Fatalf("the refusals changed the pending slot: %+v", still)
	}
	entry, err := s.EnterServer(ctx, ticket)
	if err != nil {
		t.Fatalf("the next enter did not finish the plan: %v", err)
	}
	if entry.Role.PlayerID != roles.first || entry.Role.Name != "" || !entry.Created {
		t.Fatalf("the retry = %+v (created %v), want player %d published by this call", entry.Role, entry.Created, roles.first)
	}
	if again, err := s.EnterServer(ctx, ticket); err != nil || again.Created || again.Role.PlayerID != roles.first {
		t.Fatalf("entering again: %+v, %v", again, err)
	}
}

// 名额上停着 CreateRole 留下的带名字的待定计划：进服按同名重试把它续完，拿到的是带那个名字的角色。
// 那个计划的名字已经被别人占走时，这一次得到 ErrNameTaken、计划被释放；再进一次建出无名的映射。
func TestEnterServerDrivesANamedPlanLeftByCreateRole(t *testing.T) {
	ctx := context.Background()
	var roles *lostReplyRoleCreate
	s, _, _ := pendingContractService(t, func(c *Config) {
		roles = &lostReplyRoleCreate{Store: c.Roles}
		c.Roles = roles
	})
	account := login(t, s, "alice")
	if _, err := s.CreateRole(ctx, account.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture: CreateRole was supposed to lose its reply")
	}
	entry, err := s.EnterServer(ctx, mustTicket(t, s, account.ID, 1))
	if err != nil || entry.Role.PlayerID != roles.first || entry.Role.Name != "Hero" || !entry.Created {
		t.Fatalf("entering over a pending named plan = %+v (created %v), %v; want it finished as Hero", entry.Role, entry.Created, err)
	}

	// 名字被别人占走的死计划。
	dead, cfg, _, loser, winner := deadRolePlan(t)
	ticket := mustTicket(t, dead, loser.ID, 1)
	if entry, err := dead.EnterServer(ctx, ticket); !errors.Is(err, ErrNameTaken) || entry.Role.PlayerID != 0 {
		t.Fatalf("entering over a plan whose name was committed elsewhere: %+v, %v; want ErrNameTaken", entry, err)
	}
	if _, found := slotAt(t, cfg, loser.ID, 1); found {
		t.Fatal("the dead plan still holds the slot")
	}
	entry, err = dead.EnterServer(ctx, ticket)
	if err != nil || !entry.Created || entry.Role.Name != "" || entry.Role.PlayerID == winner.PlayerID {
		t.Fatalf("the next enter = %+v (created %v), %v; want a fresh nameless mapping", entry.Role, entry.Created, err)
	}
}

// slotsServingStale 让接下来的 remaining 次读 slot 拿到 stale（一份过去的快照）：造出“读到的时候还没发布、
// 写的时候别人已经发布了”的交错，不靠并发的运气。
type slotsServingStale struct {
	versionstore.Store[string, Slot]
	stale     *versionstore.Versioned[Slot]
	remaining int
}

func (store *slotsServingStale) Get(ctx context.Context, key string) (versionstore.Versioned[Slot], bool, error) {
	if store.stale != nil && store.remaining > 0 {
		store.remaining--
		return *store.stale, true, nil
	}
	return store.Store.Get(ctx, key)
}

func (store *slotsServingStale) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	return store.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

// 两次握手交错：后一次读到的还是“计划待定”，等它走到发布那一步，映射已经被前一次发布了。
// 它拿到的是同一个玩家号，但 Created 为 false——只有真正发布的那一次是 true。
func TestAnEntryThatLosesThePublishRaceDoesNotReportCreated(t *testing.T) {
	var roles *lostReplyRoleCreate
	var slots *slotsServingStale
	s, _, cfg := newService(t, func(c *Config) {
		roles = &lostReplyRoleCreate{Store: c.Roles}
		c.Roles = roles
		slots = &slotsServingStale{Store: c.Slots}
		c.Slots = slots
	})
	ctx := context.Background()
	account := login(t, s, "alice")
	ticket := mustTicket(t, s, account.ID, 1)
	if _, err := s.EnterServer(ctx, ticket); err == nil {
		t.Fatal("fixture: the first enter was supposed to lose its reply")
	}
	pending, found, err := cfg.Slots.Get(ctx, slotKeyFor(account.ID, 1))
	if err != nil || !found || pending.Value.PlayerID != 0 {
		t.Fatalf("fixture: want an unpublished plan: %+v found=%v err=%v", pending.Value, found, err)
	}
	winner, err := s.EnterServer(ctx, ticket)
	if err != nil || !winner.Created {
		t.Fatalf("the entry that publishes the mapping: %+v, %v", winner, err)
	}
	// 落后的那一次：RoleOnServer 与建计划各读一次 slot，读到的都是发布之前的样子。
	slots.stale, slots.remaining = &pending, 2
	late, err := s.EnterServer(ctx, ticket)
	if err != nil {
		t.Fatalf("the entry that lost the race: %v", err)
	}
	if slots.remaining != 0 {
		t.Fatalf("fixture: the late entry read the slot %d times fewer than expected", slots.remaining)
	}
	if late.Created || late.Role.PlayerID != winner.Role.PlayerID {
		t.Fatalf("the late entry = %+v (created %v), want the same player and Created false", late.Role, late.Created)
	}
}

// 新建映射遵守区服的准入，与 CreateRole 相同：服不是 open、服已并入别的服、名额被换服占着，各答各的码，
// 什么也不留下；已有角色的不受区服状态影响。分配器给了一个别人在用的玩家号时不建映射、释放名额。
func TestEnterServerCreatesUnderTheServerRulesOfCreateRole(t *testing.T) {
	var taken atomic.Int64
	s, _, cfg := pendingContractService(t, func(c *Config) {
		inner := c.Allocator
		c.Allocator = AllocatorFunc(func(ctx context.Context, serverID int32) (int64, error) {
			if id := taken.Swap(0); id != 0 {
				return id, nil // 一次性的坏回答：一个已经属于别人的玩家号
			}
			return inner.Allocate(ctx, serverID)
		})
	})
	ctx := context.Background()
	upsertOpen(t, s, 2, 3, 4)
	veteran, _ := roleOn(t, s, "veteran", 2, "Veteran")
	onMerged, _ := roleOn(t, s, "merged", 3, "Merged")
	account := login(t, s, "newcomer")

	refused := func(label string, serverID int32, want error, code int32) {
		t.Helper()
		entry, err := s.EnterServer(ctx, mustTicket(t, s, account.ID, serverID))
		if !errors.Is(err, want) || Code(err) != code || entry.Role.PlayerID != 0 || entry.Created {
			t.Fatalf("%s: %+v, %v; want code %d", label, entry, err, code)
		}
		if _, found := slotAt(t, cfg, account.ID, serverID); found {
			t.Fatalf("%s: the refusal left a slot on server %d", label, serverID)
		}
	}
	for _, status := range []ServerStatus{ServerMaintenance, ServerFull, ServerClosed} {
		if _, err := s.UpsertServer(ctx, GameServer{ID: 2, Name: "s2", Status: status}); err != nil {
			t.Fatal(err)
		}
		refused("a server that is "+string(status), 2, ErrServerClosed, 560105)
		// 已有角色的照常进。
		if entry, err := s.EnterServer(ctx, mustTicket(t, s, veteran.AccountID, 2)); err != nil || entry.Role.PlayerID != veteran.PlayerID || entry.Created {
			t.Fatalf("an existing role on a server that is %s: %+v, %v", status, entry, err)
		}
	}
	// 3 并入 1：没有角色的账号在 3 上建不了；3 上的老角色凭 3 的票据进，由 1 承载。
	mustMerge(t, s, 3, 1)
	refused("a server merged into another", 3, ErrServerClosed, 560105)
	entry, err := s.EnterServer(ctx, mustTicket(t, s, onMerged.AccountID, 3))
	if err != nil || entry.Role.PlayerID != onMerged.PlayerID || entry.Role.ServerID != 3 || entry.Role.ServingServerID != 1 {
		t.Fatalf("an old role of the merged server: %+v, %v; want origin 3 served by 1", entry.Role, err)
	}

	// 分配器给了 veteran 的玩家号：不建映射，名额释放；分配器恢复之后照常建。
	taken.Store(veteran.PlayerID)
	refused("a player id that is already in use", 1, ErrConflict, 560113)
	if still := storedRole(t, cfg, veteran.PlayerID); still.Value.AccountID != veteran.AccountID {
		t.Fatalf("the colliding create overwrote the other account's role: %+v", still.Value)
	}
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, account.ID, 1)); err != nil || !entry.Created || entry.Role.PlayerID == veteran.PlayerID {
		t.Fatalf("after the allocator recovered: %+v, %v", entry, err)
	}

	// 换服在途：源服上照常进、带着 Transfer；目标服的名额被占位占着，账号在那里建不了映射。
	mover, _ := roleOn(t, s, "mover", 1, "Mover")
	mustBegin(t, s, mover.PlayerID, nextTransfer(t, s, mover.PlayerID, "t-1", 4))
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, mover.AccountID, 1)); err != nil || entry.Role.Transfer == nil || entry.Role.Transfer.To != 4 || entry.Role.ServerID != 1 {
		t.Fatalf("a role in transfer entering its source: %+v, %v", entry.Role, err)
	}
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, mover.AccountID, 4)); !errors.Is(err, ErrRoleLimit) || entry.Role.PlayerID != 0 {
		t.Fatalf("entering the target of a transfer in flight: %+v, %v; want ErrRoleLimit (the hold)", entry, err)
	}
	if hold, found := slotAt(t, cfg, mover.AccountID, 4); !found || hold.TransferID != "t-1" {
		t.Fatalf("the refused enter disturbed the transfer hold: %+v found=%v", hold, found)
	}
	// 提交之后：凭目标服的票据进到同一个角色，绑定代数加一。
	mustCommit(t, s, mover.PlayerID, "t-1")
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, mover.AccountID, 4)); err != nil || entry.Role.PlayerID != mover.PlayerID || entry.Role.ServerID != 4 || entry.Role.BindEpoch != 1 || entry.Created {
		t.Fatalf("after the commit: %+v, %v", entry.Role, err)
	}

	// 一次已经中止的换服迟到写下的占位（没有角色记录认它）不挡进服：撤掉它，照常新建映射。
	strayer, _ := roleOn(t, s, "strayer", 1, "Strayer")
	mustBegin(t, s, strayer.PlayerID, nextTransfer(t, s, strayer.PlayerID, "t-stray", 4))
	if _, err := s.AbortRoleTransfer(ctx, strayer.PlayerID, "t-stray", "changed my mind"); err != nil {
		t.Fatal(err)
	}
	home, _ := slotAt(t, cfg, strayer.AccountID, 1)
	stray := Slot{AccountID: strayer.AccountID, ServerID: 4, PlayerID: strayer.PlayerID, Creation: home.Creation, TransferID: "t-stray", BindEpoch: 1}
	if _, created, err := cfg.Slots.Create(ctx, slotKeyFor(strayer.AccountID, 4), stray); err != nil || !created {
		t.Fatalf("plant a stray hold: created=%v err=%v", created, err)
	}
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, strayer.AccountID, 4)); err != nil || !entry.Created || entry.Role.PlayerID == strayer.PlayerID || entry.Role.ServerID != 4 {
		t.Fatalf("entering a server where only a stray hold stands: %+v, %v; want a new mapping", entry, err)
	}

	// slot 与角色对不上：ErrConflict，不新建、不交出别人的角色。
	if _, _, err := cfg.Slots.Update(ctx, slotKeyFor(veteran.AccountID, 2), func(current Slot, found bool) (Slot, bool, error) {
		if !found {
			return current, false, errors.New("no slot to edit")
		}
		current.PlayerID = mover.PlayerID
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, veteran.AccountID, 2)); !errors.Is(err, ErrConflict) || entry.Role.PlayerID != 0 {
		t.Fatalf("a slot that names another account's role: %+v, %v; want ErrConflict", entry, err)
	}
}

// 两个时钟：票据的寿命按系统时间（它是真实时间里的安全界限），最近登录时间按业务时间
// （业务时间可以被拨快拨慢，凭证不能因此提前失效或多活）。
func TestTicketsLiveOnTheSystemClockAndLoginsOnTheBusinessClock(t *testing.T) {
	system := &clock{now: time.Unix(1_700_000_000, 0)}
	business := &clock{now: time.Unix(1_800_000_000, 0)} // 业务时间比系统时间快了三年多
	s, _, _ := newService(t, func(c *Config) { c.SystemNow, c.Now = system.Now, business.Now })
	ctx := context.Background()
	account := login(t, s, "alice")
	accountTicket, err := s.IssueAccountTicket(ctx, account.ID)
	if err != nil || accountTicket.ExpiresAtUnix != 1_700_000_600 {
		t.Fatalf("account ticket = %+v, %v; want expiry 10 minutes after the SYSTEM time", accountTicket, err)
	}
	issued, err := s.IssueEnterTicket(ctx, account.ID, 1)
	if err != nil || issued.ExpiresAtUnix != 1_700_001_800 {
		t.Fatalf("enter ticket = %+v, %v; want expiry 30 minutes after the SYSTEM time", issued, err)
	}
	entry, err := s.EnterServer(ctx, issued.Ticket)
	if err != nil || entry.Role.LastLoginAtUnix != 1_800_000_000 || entry.Role.CreatedAtUnix != 1_800_000_000 {
		t.Fatalf("entry = %+v, %v; want the login stamped with the BUSINESS time", entry.Role, err)
	}
	// 业务时间再往前拨一年，票据不受影响；系统时间走到期限，票据失效。
	business.advance(365 * 24 * time.Hour)
	if _, err := s.EnterServer(ctx, issued.Ticket); err != nil {
		t.Fatalf("moving the business clock expired the enter ticket: %v", err)
	}
	if _, err := s.ValidateAccountTicket(ctx, accountTicket.Ticket); err != nil {
		t.Fatalf("moving the business clock expired the account ticket: %v", err)
	}
	system.advance(10 * time.Minute)
	if _, err := s.ValidateAccountTicket(ctx, accountTicket.Ticket); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("the account ticket outlived its system-time expiry: %v", err)
	}
	system.advance(20 * time.Minute)
	if _, err := s.EnterServer(ctx, issued.Ticket); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("the enter ticket outlived its system-time expiry: %v", err)
	}
}
