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
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 进服（enter.go）：登录入口签进服票据，握手的认证器凭票据解析角色、没有就新建。
// 存储缺省是 Memory；ROOST_REVIEW3_BACKEND=redis 时走隔离的 Redis（pendingContractService）。
// 需求里的数（30 分钟、560120……）写死。

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

// 票据的形状、绑定与寿命；签发是只读的；拒绝的几种情况。区服的状态不影响签发，合服后带出承载服。
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
		if signature, err := base64.RawURLEncoding.DecodeString(parts[4]); err != nil || len(signature) != 32 {
			t.Fatalf("%s: the signature is %d bytes, %v", transport, len(signature), err)
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
	// 只读：没有建角、没有 slot、账号记录没动。
	if _, err := s.RoleOnServer(ctx, account.ID, 1); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("issuing a ticket created a role: %v", err)
	}
	if _, found := slotAt(t, cfg, account.ID, 1); found {
		t.Fatal("issuing a ticket created a slot")
	}
	if after, _, _ := cfg.Accounts.Get(ctx, account.ID); after.Version != before.Version {
		t.Fatalf("issuing a ticket wrote the account: version %d -> %d", before.Version, after.Version)
	}
	// 被封的账号签不出票据。
	banAccount(t, cfg, account.ID)
	if got, err := s.IssueEnterTicket(ctx, account.ID, 1); !errors.Is(err, ErrIdentityDenied) || got.Ticket != "" {
		t.Fatalf("a banned account: %+v, %v", got, err)
	}
	// 承载服解析不了（并入一个没登记的服）时不签。
	if _, _, err := cfg.Servers.Update(ctx, 2, func(current GameServer, _ bool) (GameServer, bool, error) {
		current.MergedInto = 99
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	bob := login(t, s, "bob")
	if got, err := s.IssueEnterTicket(ctx, bob.ID, 2); !errors.Is(err, ErrServerInvalid) || got.Ticket != "" {
		t.Fatalf("a server merged into an unknown one: %+v, %v", got, err)
	}
}

// 新号凭票进服：建一个角色，Created 为 true，记一次登录；同一张票据再来（重连）是同一个角色、Created 为 false，
// 这时给的名字被忽略。之后 RoleOnServer 问得到它，“入口自己选角”那条路（SelectRole / ValidateSession）照样认它。
func TestEnterServerCreatesTheRoleOnceThenFindsIt(t *testing.T) {
	s, clock, _ := pendingContractService(t)
	ctx := context.Background()
	account := login(t, s, "alice")
	ticket := mustTicket(t, s, account.ID, 1)
	clients := enterClients(t, s)

	entry, err := clients["bus"].EnterServer(ctx, ticket, "Hero")
	if err != nil {
		t.Fatalf("the first enter: %v", err)
	}
	role := entry.Role
	if !entry.Created || role.PlayerID == 0 || role.Name != "Hero" || role.AccountID != account.ID || role.ServerID != 1 || role.ServingServerID != 1 ||
		role.CreatedAtUnix != 1_700_000_000 || role.LastLoginAtUnix != 1_700_000_000 || role.BindEpoch != 0 || role.Transfer != nil {
		t.Fatalf("the first enter = %+v (created %v), want a new role Hero on server 1 that just logged in", role, entry.Created)
	}
	clock.advance(5 * time.Minute)
	for transport, accounts := range clients {
		again, err := accounts.EnterServer(ctx, ticket, "AnotherName")
		if err != nil {
			t.Fatalf("%s: entering again with the same ticket: %v", transport, err)
		}
		if again.Created || again.Role.PlayerID != role.PlayerID || again.Role.Name != "Hero" || again.Role.LastLoginAtUnix != 1_700_000_300 {
			t.Fatalf("%s: the second enter = %+v (created %v), want the same role, not created, with a fresh login time", transport, again.Role, again.Created)
		}
		// 已有角色时不需要名字。
		if nameless, err := accounts.EnterServer(ctx, ticket, ""); err != nil || nameless.Role.PlayerID != role.PlayerID || nameless.Created {
			t.Fatalf("%s: entering an existing role without a name: %+v, %v", transport, nameless, err)
		}
	}
	// 另一张票据（新的一次登录）也是这个角色。
	if fresh, err := s.EnterServer(ctx, mustTicket(t, s, account.ID, 1), "Hero"); err != nil || fresh.Created || fresh.Role.PlayerID != role.PlayerID {
		t.Fatalf("a fresh ticket: %+v, %v", fresh, err)
	}
	if found, err := s.RoleOnServer(ctx, account.ID, 1); err != nil || found.PlayerID != role.PlayerID {
		t.Fatalf("RoleOnServer after entering = %+v, %v", found, err)
	}
	session, err := s.SelectRole(ctx, account.ID, role.PlayerID)
	if err != nil {
		t.Fatalf("SelectRole of a role made by EnterServer: %v", err)
	}
	if validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token); err != nil || validated.PlayerID != role.PlayerID {
		t.Fatalf("ValidateSession: %+v, %v", validated, err)
	}
	// 同一个账号在另一个服是另一个角色、另一张票据。
	upsertOpen(t, s, 2)
	other, err := s.EnterServer(ctx, mustTicket(t, s, account.ID, 2), "HeroTwo")
	if err != nil || !other.Created || other.Role.PlayerID == role.PlayerID || other.Role.ServerID != 2 {
		t.Fatalf("the same account on server 2: %+v, %v", other, err)
	}
	// 一张票据只进它绑定的服：1 服的票据进不了 2 服的角色。
	if mine, err := s.EnterServer(ctx, ticket, ""); err != nil || mine.Role.ServerID != 1 {
		t.Fatalf("the ticket for server 1 answered %+v, %v", mine, err)
	}
}

// 无效的票据一律 ErrSessionInvalid、回答一模一样、什么也不写：改动任何一个字符、过期、别的密钥、
// 另一个版本（签名无误）、超长、拿角色会话令牌冒充。反过来，进服票据也冒充不了角色会话令牌。
func TestEnterServerRefusesEveryBadTicketTheSameWay(t *testing.T) {
	s, clock, cfg := pendingContractService(t)
	ctx := context.Background()
	account := login(t, s, "alice")
	ticket := mustTicket(t, s, account.ID, 1)
	role, session := roleOn(t, s, "bob", 1, "Bob")
	writes := func() string {
		_, slot := slotAt(t, cfg, account.ID, 1)
		stored := storedRole(t, cfg, role.PlayerID)
		return fmt.Sprintf("slot=%v bob.version=%d", slot, stored.Version)
	}
	before := writes()
	var common string
	refused := func(label, bad string) {
		t.Helper()
		entry, err := s.EnterServer(ctx, bad, "Hero")
		if !errors.Is(err, ErrSessionInvalid) || Code(err) != 560111 || entry.Role.PlayerID != 0 || entry.Created {
			t.Fatalf("%s: %+v, %v; want ErrSessionInvalid and nothing else", label, entry, err)
		}
		if common == "" {
			common = err.Error()
		}
		if err.Error() != common {
			t.Fatalf("%s: the refusal %q differs from %q", label, err, common)
		}
		if strings.Contains(err.Error(), "et1.") || strings.Contains(err.Error(), account.ID) {
			t.Fatalf("%s: the refusal carries the ticket or the account: %v", label, err)
		}
	}
	for index := range ticket {
		replacement := byte('A')
		if ticket[index] == 'A' {
			replacement = 'B'
		}
		refused(fmt.Sprintf("changed at byte %d", index), ticket[:index]+string(replacement)+ticket[index+1:])
	}
	parts := strings.Split(ticket, ".")
	refused("bob's account under alice's signature", strings.Join([]string{parts[0], parts[1], parts[2], base64.RawURLEncoding.EncodeToString([]byte(role.AccountID)), parts[4]}, "."))
	refused("another server under the same signature", strings.Join([]string{parts[0], parts[1], "2", parts[3], parts[4]}, "."))
	refused("a later expiry under the same signature", strings.Join([]string{parts[0], "1900000000", parts[2], parts[3], parts[4]}, "."))
	nextVersion := "et2." + strings.Join(parts[1:4], ".")
	refused("a correctly signed ticket of another version", nextVersion+"."+base64.RawURLEncoding.EncodeToString(s.enterTicketMAC(nextVersion)))
	// 签名的输入带着域前缀：同一把密钥、同样的内容、不带域前缀签出来的不认（进服票据与别的凭证各有各的域）。
	body := strings.Join(parts[:4], ".")
	plain := hmac.New(sha256.New, []byte(cfg.SessionSecret))
	plain.Write([]byte(body))
	refused("signed without the domain prefix", body+"."+base64.RawURLEncoding.EncodeToString(plain.Sum(nil)))
	refused("an empty ticket", "")
	refused("four segments", strings.Join(parts[:4], "."))
	refused("a role session token", session.Token)
	refused("a session handshake ticket", "session:"+session.Token)
	refused("an oversized ticket", ticket+strings.Repeat("A", 1025))
	other, _, _ := pendingContractService(t, func(c *Config) { c.SessionSecret = "another-session-secret" })
	otherAccount := login(t, other, "alice")
	refused("signed by another secret", mustTicket(t, other, otherAccount.ID, 1))
	// 进服票据不是角色会话令牌。
	if _, err := s.ValidateSession(ctx, role.PlayerID, ticket); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("an enter ticket passed as a session token: %v", err)
	}

	// 过期：差一秒还能用（下面单独验证），到点起不能用。先看到点。
	clock.advance(30 * time.Minute)
	refused("at the expiry second", ticket)
	if after := writes(); after != before {
		t.Fatalf("refused tickets wrote something: %s -> %s", before, after)
	}
	// 差一秒到期的票据照常进服。
	fresh := mustTicket(t, s, account.ID, 1)
	clock.advance(30*time.Minute - time.Second)
	if entry, err := s.EnterServer(ctx, fresh, "Hero"); err != nil || !entry.Created {
		t.Fatalf("one second before expiry: %+v, %v", entry, err)
	}
}

// PeekEnterTicket 读出票据里写的账号与服，不验证签名；形状不对的读不出来。
func TestPeekEnterTicketReadsWithoutVerifying(t *testing.T) {
	s, _, _ := pendingContractService(t)
	account := login(t, s, "alice")
	ticket := mustTicket(t, s, account.ID, 1)
	if accountID, serverID, ok := PeekEnterTicket(ticket); !ok || accountID != account.ID || serverID != 1 {
		t.Fatalf("PeekEnterTicket = (%q, %d, %v)", accountID, serverID, ok)
	}
	parts := strings.Split(ticket, ".")
	forged := strings.Join([]string{parts[0], parts[1], "7", base64.RawURLEncoding.EncodeToString([]byte("store:mallory")), "AAAA"}, ".")
	if accountID, serverID, ok := PeekEnterTicket(forged); !ok || accountID != "store:mallory" || serverID != 7 {
		t.Fatalf("a forged ticket peeks as (%q, %d, %v); peeking does not verify", accountID, serverID, ok)
	}
	if _, err := s.EnterServer(context.Background(), forged, "Mallory"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("the forged ticket was accepted by EnterServer: %v", err)
	}
	for label, bad := range map[string]string{
		"empty":                "",
		"a session token":      "abc.def",
		"another version":      "et2" + ticket[3:],
		"server id zero":       strings.Join([]string{parts[0], parts[1], "0", parts[3], parts[4]}, "."),
		"a non-canonical id":   strings.Join([]string{parts[0], parts[1], "01", parts[3], parts[4]}, "."),
		"an empty account":     strings.Join([]string{parts[0], parts[1], parts[2], "", parts[4]}, "."),
		"an undecodable field": strings.Join([]string{parts[0], parts[1], parts[2], "!!!", parts[4]}, "."),
		"oversized":            ticket + strings.Repeat("A", 1025),
	} {
		if accountID, serverID, ok := PeekEnterTicket(bad); ok {
			t.Fatalf("%s: PeekEnterTicket = (%q, %d, true)", label, accountID, serverID)
		}
	}
}

// 同一张票据并发握手：只有一个角色，所有调用拿到的是它，恰好一个 Created。
func TestConcurrentEntriesWithOneTicketCreateOneRole(t *testing.T) {
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
			entries[i], errs[i] = s.EnterServer(ctx, ticket, "Hero")
		}()
	}
	close(start)
	wg.Wait()
	created, playerID := 0, int64(0)
	for i := range callers {
		if errs[i] != nil {
			// 存储竞争可以让个别调用答“稍后重试”，但不能答别的，更不能建出第二个角色。
			if !errors.Is(errs[i], ErrConflict) && !errors.Is(errs[i], versionstore.ErrConflict) {
				t.Fatalf("caller %d: %v", i, errs[i])
			}
			continue
		}
		if playerID == 0 {
			playerID = entries[i].Role.PlayerID
		}
		if entries[i].Role.PlayerID != playerID || entries[i].Role.Name != "Hero" {
			t.Fatalf("caller %d got role %+v, want player %d", i, entries[i].Role, playerID)
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
	if again, err := s.EnterServer(ctx, ticket, "Hero"); err != nil || again.Created || again.Role.PlayerID != playerID {
		t.Fatalf("entering afterwards: %+v, %v", again, err)
	}
}

// 建角的应答丢失之后（角色记录写下了、slot 还没发布）：握手方用同一个名字重试，接上同一个计划把它做完，
// 拿到的是同一个玩家号——不会卡住，也不会建出第二个角色。换一个名字重试则按建角的规则被拒。
func TestEnterServerFinishesAPlanWhoseReplyWasLost(t *testing.T) {
	var roles *lostReplyRoleCreate
	s, _, cfg := pendingContractService(t, func(c *Config) {
		roles = &lostReplyRoleCreate{Store: c.Roles}
		c.Roles = roles
	})
	ctx := context.Background()
	account := login(t, s, "alice")
	ticket := mustTicket(t, s, account.ID, 1)
	if entry, err := s.EnterServer(ctx, ticket, "Hero"); err == nil || entry.Role.PlayerID != 0 {
		t.Fatalf("fixture: the first enter was supposed to lose its reply: %+v, %v", entry, err)
	}
	if slot := mustSlot(t, cfg, account.ID); slot.PlayerID != 0 || roles.first == 0 {
		t.Fatalf("fixture: want a pending plan with the role record written, got %+v", slot)
	}
	// 换名字：原计划还能完成，名额被它占着。
	if _, err := s.EnterServer(ctx, ticket, "Knight"); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("a retry under another name: %v, want ErrRoleLimit while the first plan can still complete", err)
	}
	// 没给名字：这一步需要名字。
	if _, err := s.EnterServer(ctx, ticket, " "); !errors.Is(err, ErrRoleNameRequired) {
		t.Fatalf("a retry without a name: %v", err)
	}
	entry, err := s.EnterServer(ctx, ticket, "Hero")
	if err != nil {
		t.Fatalf("the same-name retry did not finish the plan: %v", err)
	}
	if entry.Role.PlayerID != roles.first || entry.Role.Name != "Hero" || !entry.Created {
		t.Fatalf("the retry = %+v (created %v), want player %d published by this call", entry.Role, entry.Created, roles.first)
	}
	if again, err := s.EnterServer(ctx, ticket, "Hero"); err != nil || again.Created || again.Role.PlayerID != roles.first {
		t.Fatalf("entering again: %+v, %v", again, err)
	}
}

// 新建走的是 CreateRole 的规则：没给名字、名字不合规则、名字被占、服不是 open、服已并入别的服、名额被换服占着，
// 各答各的码，什么也不留下；已有角色的不受区服状态影响。封号对新号老号都立即生效。
func TestEnterServerCreatesUnderTheRulesOfCreateRole(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2, 3, 4)
	veteran, _ := roleOn(t, s, "veteran", 2, "Veteran")
	onMerged, _ := roleOn(t, s, "merged", 3, "Merged")
	account := login(t, s, "newcomer")

	refused := func(label string, serverID int32, name string, want error, code int32) {
		t.Helper()
		entry, err := s.EnterServer(ctx, mustTicket(t, s, account.ID, serverID), name)
		if !errors.Is(err, want) || Code(err) != code || entry.Role.PlayerID != 0 || entry.Created {
			t.Fatalf("%s: %+v, %v; want code %d", label, entry, err, code)
		}
		if _, found := slotAt(t, cfg, account.ID, serverID); found {
			t.Fatalf("%s: the refusal left a slot on server %d", label, serverID)
		}
	}
	refused("no name", 1, "", ErrRoleNameRequired, 560120)
	refused("a blank name", 1, "   ", ErrRoleNameRequired, 560120)
	refused("a name the rules refuse", 1, "x", ErrNameInvalid, 560106) // simpleNameRules：2～16 个字符
	refused("a name someone else holds", 1, "Veteran", ErrNameTaken, 560107)

	for _, status := range []ServerStatus{ServerMaintenance, ServerFull, ServerClosed} {
		if _, err := s.UpsertServer(ctx, GameServer{ID: 2, Name: "s2", Status: status}); err != nil {
			t.Fatal(err)
		}
		refused("a server that is "+string(status), 2, "Newbie", ErrServerClosed, 560105)
		// 已有角色的照常进。
		if entry, err := s.EnterServer(ctx, mustTicket(t, s, veteran.AccountID, 2), ""); err != nil || entry.Role.PlayerID != veteran.PlayerID || entry.Created {
			t.Fatalf("an existing role on a server that is %s: %+v, %v", status, entry, err)
		}
	}
	// 3 并入 1：没有角色的账号在 3 上建不了；3 上的老角色凭 3 的票据进，由 1 承载。
	mustMerge(t, s, 3, 1)
	refused("a server merged into another", 3, "Newbie", ErrServerClosed, 560105)
	entry, err := s.EnterServer(ctx, mustTicket(t, s, onMerged.AccountID, 3), "")
	if err != nil || entry.Role.PlayerID != onMerged.PlayerID || entry.Role.ServerID != 3 || entry.Role.ServingServerID != 1 {
		t.Fatalf("an old role of the merged server: %+v, %v; want origin 3 served by 1", entry.Role, err)
	}

	// 换服在途：源服上照常进、带着 Transfer；目标服的名额被占位占着，账号在那里建不了角。
	mover, _ := roleOn(t, s, "mover", 1, "Mover")
	mustBegin(t, s, mover.PlayerID, nextTransfer(t, s, mover.PlayerID, "t-1", 4))
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, mover.AccountID, 1), ""); err != nil || entry.Role.Transfer == nil || entry.Role.Transfer.To != 4 || entry.Role.ServerID != 1 {
		t.Fatalf("a role in transfer entering its source: %+v, %v", entry.Role, err)
	}
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, mover.AccountID, 4), "MoverTwo"); !errors.Is(err, ErrRoleLimit) || entry.Role.PlayerID != 0 {
		t.Fatalf("entering the target of a transfer in flight: %+v, %v; want ErrRoleLimit (the hold)", entry, err)
	}
	// 提交之后：凭目标服的票据进到同一个角色，绑定代数加一。
	mustCommit(t, s, mover.PlayerID, "t-1")
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, mover.AccountID, 4), ""); err != nil || entry.Role.PlayerID != mover.PlayerID || entry.Role.ServerID != 4 || entry.Role.BindEpoch != 1 || entry.Created {
		t.Fatalf("after the commit: %+v, %v", entry.Role, err)
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
	if entry, err := s.EnterServer(ctx, mustTicket(t, s, veteran.AccountID, 2), "VeteranTwo"); !errors.Is(err, ErrConflict) || entry.Role.PlayerID != 0 {
		t.Fatalf("a slot that names another account's role: %+v, %v; want ErrConflict", entry, err)
	}

	// 封号：签出去的票据立刻进不来，老号新号都一样。
	oldTicket, newTicket := mustTicket(t, s, veteran.AccountID, 2), mustTicket(t, s, account.ID, 1)
	lastLogin := storedRole(t, cfg, veteran.PlayerID).Value.LastLoginAtUnix
	banAccount(t, cfg, veteran.AccountID)
	banAccount(t, cfg, account.ID)
	if entry, err := s.EnterServer(ctx, oldTicket, ""); !errors.Is(err, ErrIdentityDenied) || Code(err) != 560102 || entry.Role.PlayerID != 0 {
		t.Fatalf("a banned account with a role: %+v, %v", entry, err)
	}
	if entry, err := s.EnterServer(ctx, newTicket, "Banned"); !errors.Is(err, ErrIdentityDenied) || entry.Role.PlayerID != 0 {
		t.Fatalf("a banned account without a role: %+v, %v", entry, err)
	}
	if after := storedRole(t, cfg, veteran.PlayerID).Value.LastLoginAtUnix; after != lastLogin {
		t.Fatal("a refused enter stamped the banned account's role")
	}
	if _, found := slotAt(t, cfg, account.ID, 1); found {
		t.Fatal("a banned account's enter left a slot")
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

// 两次握手交错：后一次读到的还是“计划待定”，等它走到发布那一步，角色已经被前一次发布了。
// 它拿到的是同一个角色，但 Created 为 false——只有真正发布的那一次是 true。
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
	if _, err := s.EnterServer(ctx, ticket, "Hero"); err == nil {
		t.Fatal("fixture: the first enter was supposed to lose its reply")
	}
	pending, found, err := cfg.Slots.Get(ctx, slotKeyFor(account.ID, 1))
	if err != nil || !found || pending.Value.PlayerID != 0 || !pending.Value.Creation.Admitted {
		t.Fatalf("fixture: want an admitted, unpublished plan: %+v found=%v err=%v", pending.Value, found, err)
	}
	winner, err := s.EnterServer(ctx, ticket, "Hero")
	if err != nil || !winner.Created {
		t.Fatalf("the entry that publishes the role: %+v, %v", winner, err)
	}
	// 落后的那一次：RoleOnServer 与建角各读一次 slot，读到的都是发布之前的样子。
	slots.stale, slots.remaining = &pending, 2
	late, err := s.EnterServer(ctx, ticket, "Hero")
	if err != nil {
		t.Fatalf("the entry that lost the race: %v", err)
	}
	if slots.remaining != 0 {
		t.Fatalf("fixture: the late entry read the slot %d times fewer than expected", slots.remaining)
	}
	if late.Created || late.Role.PlayerID != winner.Role.PlayerID {
		t.Fatalf("the late entry = %+v (created %v), want the same role and Created false", late.Role, late.Created)
	}
}

// 两个时钟：票据的寿命按系统时间（它是真实时间里的安全界限），最近登录时间按业务时间
// （业务时间可以被拨快拨慢，凭证不能因此提前失效或多活）。
func TestEnterTicketsLiveOnTheSystemClockAndLoginsOnTheBusinessClock(t *testing.T) {
	system := &clock{now: time.Unix(1_700_000_000, 0)}
	business := &clock{now: time.Unix(1_800_000_000, 0)} // 业务时间比系统时间快了三年多
	s, _, _ := newService(t, func(c *Config) { c.SystemNow, c.Now = system.Now, business.Now })
	ctx := context.Background()
	account := login(t, s, "alice")
	issued, err := s.IssueEnterTicket(ctx, account.ID, 1)
	if err != nil || issued.ExpiresAtUnix != 1_700_001_800 {
		t.Fatalf("issued = %+v, %v; want expiry 30 minutes after the SYSTEM time", issued, err)
	}
	entry, err := s.EnterServer(ctx, issued.Ticket, "Hero")
	if err != nil || entry.Role.LastLoginAtUnix != 1_800_000_000 || entry.Role.CreatedAtUnix != 1_800_000_000 {
		t.Fatalf("entry = %+v, %v; want the login stamped with the BUSINESS time", entry.Role, err)
	}
	// 业务时间再往前拨一年，票据不受影响；系统时间走到期限，票据失效。
	business.advance(365 * 24 * time.Hour)
	if _, err := s.EnterServer(ctx, issued.Ticket, ""); err != nil {
		t.Fatalf("moving the business clock expired the ticket: %v", err)
	}
	system.advance(30 * time.Minute)
	if _, err := s.EnterServer(ctx, issued.Ticket, ""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("the ticket outlived its system-time expiry: %v", err)
	}
}
