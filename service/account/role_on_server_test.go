package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
)

// RoleOnServer：账号在某个服上的角色是谁。登录入口靠它让换了设备的客户端认回自己的角色。
// 各种 slot 状态各答什么写在 Service.RoleOnServer 的注释里，这里一条一条钉住。
// 存储缺省是 Memory；ROOST_REVIEW3_BACKEND=redis 时与其余契约测试一样走隔离的 Redis（pendingContractService）。

// roleOnServerClients 给出同一个服务的两种调用方式：本地与经总线（请求、响应都过一遍总线编码）。
func roleOnServerClients(t *testing.T, service *Service) map[string]Accounts {
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

// 有角色的服上问得到它，带着承载服；没有角色的服、别的账号、没登记的服都是 ErrRoleMissing；
// 参数不成样子的不读存储。本地调用与经总线调用的回答相同，而且什么都不写。
func TestRoleOnServerReturnsTheAccountsRoleOnThatServer(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2)
	role, _ := roleOn(t, s, "owner", 1, "Alice")
	other := login(t, s, "stranger")
	before := storedRole(t, cfg, role.PlayerID)

	for transport, accounts := range roleOnServerClients(t, s) {
		found, err := accounts.RoleOnServer(ctx, role.AccountID, 1)
		if err != nil {
			t.Fatalf("%s: RoleOnServer on the role's server: %v", transport, err)
		}
		if found.PlayerID != role.PlayerID || found.Name != "Alice" || found.AccountID != role.AccountID ||
			found.ServerID != 1 || found.ServingServerID != 1 || found.Transfer != nil || found.CreatedAtUnix != role.CreatedAtUnix {
			t.Fatalf("%s: RoleOnServer = %+v, want role %d (Alice) bound to and served by server 1", transport, found, role.PlayerID)
		}
		for label, ask := range map[string]struct {
			accountID string
			serverID  int32
			want      error
			code      int32
		}{
			"a registered server without a role": {role.AccountID, 2, ErrRoleMissing, CodeRoleMissing},
			"a server nobody registered":         {role.AccountID, 77, ErrRoleMissing, CodeRoleMissing},
			"another account on the same server": {other.ID, 1, ErrRoleMissing, CodeRoleMissing},
			"an account that never logged in":    {"store:nobody", 1, ErrRoleMissing, CodeRoleMissing},
			"an empty account id":                {"  ", 1, ErrAccountMissing, CodeAccountMissing},
			"server id zero":                     {role.AccountID, 0, ErrServerInvalid, CodeServerInvalid},
			"a negative server id":               {role.AccountID, -3, ErrServerInvalid, CodeServerInvalid},
		} {
			got, err := accounts.RoleOnServer(ctx, ask.accountID, ask.serverID)
			if !errors.Is(err, ask.want) || Code(err) != ask.code {
				t.Fatalf("%s: %s: err = %v (code %d), want %v", transport, label, err, Code(err), ask.want)
			}
			if got.PlayerID != 0 || got.Name != "" {
				t.Fatalf("%s: %s: a refusal carried a role: %+v", transport, label, got)
			}
		}
	}

	// 只读：角色记录的版本与登录时间都没动（SelectRole 会改 LastLoginAtUnix）。
	after := storedRole(t, cfg, role.PlayerID)
	if after.Version != before.Version || after.Value.LastLoginAtUnix != before.Value.LastLoginAtUnix {
		t.Fatalf("RoleOnServer wrote the role: version %d -> %d", before.Version, after.Version)
	}
}

// 合服之后按绑定服问：并入 1 服的 2 服角色用 2 问，原服不变、承载服是 1；用 1 问到的是账号在 1 服自己的
// 角色（没有就是没有）。承载服解析不了时不猜。
func TestRoleOnServerCarriesTheServingServerAfterAMerge(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2, 3)
	onTwo, _ := roleOn(t, s, "merged", 2, "Bob")
	bothOne, _ := roleOn(t, s, "twice", 1, "Ann")
	bothTwo, _ := roleOn(t, s, "twice", 2, "Ann2")
	mustMerge(t, s, 2, 1)

	for transport, accounts := range roleOnServerClients(t, s) {
		found, err := accounts.RoleOnServer(ctx, onTwo.AccountID, 2)
		if err != nil || found.PlayerID != onTwo.PlayerID || found.ServerID != 2 || found.ServingServerID != 1 {
			t.Fatalf("%s: the merged server's role = %+v, %v; want origin 2 served by 1", transport, found, err)
		}
		if _, err := accounts.RoleOnServer(ctx, onTwo.AccountID, 1); !errors.Is(err, ErrRoleMissing) {
			t.Fatalf("%s: asking the serving server for a role bound to the merged one = %v, want ErrRoleMissing", transport, err)
		}
		one, err := accounts.RoleOnServer(ctx, bothOne.AccountID, 1)
		if err != nil || one.PlayerID != bothOne.PlayerID || one.ServerID != 1 || one.ServingServerID != 1 {
			t.Fatalf("%s: the role on the serving server = %+v, %v", transport, one, err)
		}
		two, err := accounts.RoleOnServer(ctx, bothTwo.AccountID, 2)
		if err != nil || two.PlayerID != bothTwo.PlayerID || two.ServerID != 2 || two.ServingServerID != 1 {
			t.Fatalf("%s: the same account's role on the merged server = %+v, %v", transport, two, err)
		}
	}

	// 2 → 3 → 1 两跳：与 SelectRole 一样拒绝，不交出一个不知道由谁承载的角色。
	for id, into := range map[int32]int32{2: 3, 3: 1} {
		if _, _, err := cfg.Servers.Update(ctx, id, func(current GameServer, _ bool) (GameServer, bool, error) {
			current.MergedInto = into
			return current, true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.RoleOnServer(ctx, onTwo.AccountID, 2); !errors.Is(err, ErrServerMergeCycle) || got.PlayerID != 0 {
		t.Fatalf("RoleOnServer over a two-hop alias = %+v, %v; want ErrServerMergeCycle", got, err)
	}
}

// 换服：在途时角色仍在源服（带着 Transfer），目标服的占位不算角色；提交之后反过来；中止之后回到原样。
// 提交了但 slot 上的收尾没做完（占位标记还在、旧服 slot 还没删）时回答与收尾之后相同。
func TestRoleOnServerFollowsTheBindingThroughATransfer(t *testing.T) {
	s, cfg, role, _ := transferFixture(t)
	ctx := context.Background()
	ask := func(when string, serverID int32) (Role, error) {
		t.Helper()
		local, localErr := s.RoleOnServer(ctx, role.AccountID, serverID)
		remote, remoteErr := roleOnServerClients(t, s)["bus"].RoleOnServer(ctx, role.AccountID, serverID)
		if Code(localErr) != Code(remoteErr) || local.PlayerID != remote.PlayerID || local.ServerID != remote.ServerID ||
			(local.Transfer == nil) != (remote.Transfer == nil) || local.BindEpoch != remote.BindEpoch {
			t.Fatalf("%s, server %d: local (%+v, %v) and bus (%+v, %v) disagree", when, serverID, local, localErr, remote, remoteErr)
		}
		return local, localErr
	}

	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	home, err := ask("in flight", homeSID)
	if err != nil || home.PlayerID != role.PlayerID || home.ServerID != homeSID || home.ServingServerID != homeSID ||
		home.Transfer == nil || home.Transfer.ID != "t-1" || home.Transfer.To != awaySID {
		t.Fatalf("in flight, the source = %+v, %v; want the role with its transfer", home, err)
	}
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-1" {
		t.Fatalf("fixture: no hold on the target: %+v found=%v", hold, found)
	}
	if got, err := ask("in flight", awaySID); !errors.Is(err, ErrRoleMissing) || got.PlayerID != 0 {
		t.Fatalf("in flight, the hold on the target answered %+v, %v; want ErrRoleMissing", got, err)
	}

	// 中止：回到没有换服的样子。
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if home, err := ask("aborted", homeSID); err != nil || home.PlayerID != role.PlayerID || home.Transfer != nil || home.BindEpoch != 0 {
		t.Fatalf("aborted, the source = %+v, %v", home, err)
	}
	if _, err := ask("aborted", awaySID); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("aborted, the target = %v, want ErrRoleMissing", err)
	}
	// 中止之后才落地的占位（没有角色记录认它）同样不算角色；RoleOnServer 只读，不替别的入口撤它。
	homeSlot, _ := slotAt(t, cfg, role.AccountID, homeSID)
	stray := Slot{AccountID: role.AccountID, ServerID: awaySID, PlayerID: role.PlayerID, Creation: homeSlot.Creation, TransferID: "t-1", BindEpoch: 1}
	if _, created, err := cfg.Slots.Create(ctx, slotKeyFor(role.AccountID, awaySID), stray); err != nil || !created {
		t.Fatalf("plant a stray hold: created=%v err=%v", created, err)
	}
	if _, err := ask("stray hold", awaySID); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("a stray hold answered %v, want ErrRoleMissing", err)
	}
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-1" {
		t.Fatalf("RoleOnServer released the stray hold (found=%v): it must not write", found)
	}

	// 再换一次并提交：角色到了目标服。
	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
	mustCommit(t, s, role.PlayerID, "t-2")
	away, err := ask("committed", awaySID)
	if err != nil || away.PlayerID != role.PlayerID || away.ServerID != awaySID || away.ServingServerID != awaySID || away.BindEpoch != 1 || away.Transfer != nil {
		t.Fatalf("committed, the target = %+v, %v; want the role bound there at epoch 1", away, err)
	}
	if _, err := ask("committed", homeSID); !errors.Is(err, ErrRoleMissing) {
		t.Fatalf("committed, the source = %v, want ErrRoleMissing", err)
	}

	// 把收尾撤销回“刚提交、还没收尾”的样子：新服 slot 仍带占位标记，旧服 slot 还在。回答不变。
	if _, _, err := cfg.Slots.Update(ctx, slotKeyFor(role.AccountID, awaySID), func(current Slot, found bool) (Slot, bool, error) {
		if !found {
			return current, false, errors.New("no slot on the target after the commit")
		}
		current.TransferID = "t-2"
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := Slot{AccountID: role.AccountID, ServerID: homeSID, PlayerID: role.PlayerID, Creation: stray.Creation}
	if _, created, err := cfg.Slots.Create(ctx, slotKeyFor(role.AccountID, homeSID), old); err != nil || !created {
		t.Fatalf("put the old slot back: created=%v err=%v", created, err)
	}
	if away, err := ask("committed, unsettled", awaySID); err != nil || away.PlayerID != role.PlayerID || away.ServerID != awaySID || away.BindEpoch != 1 {
		t.Fatalf("committed but unsettled, the target = %+v, %v", away, err)
	}
	if got, err := ask("committed, unsettled", homeSID); !errors.Is(err, ErrRoleMissing) || got.PlayerID != 0 {
		t.Fatalf("committed but unsettled, the leftover slot on the source answered %+v, %v; want ErrRoleMissing", got, err)
	}
}

// 建角计划还没发布（角色记录写下了、回复丢了，slot 的 PlayerID 还是 0）：不算有角色，也不把计划里的名字
// 交出去；同名重试续完之后才问得到。
func TestRoleOnServerDoesNotAnswerForAnUnpublishedPlan(t *testing.T) {
	var roles *lostReplyRoleCreate
	s, _, cfg := pendingContractService(t, func(c *Config) {
		roles = &lostReplyRoleCreate{Store: c.Roles}
		c.Roles = roles
	})
	ctx := context.Background()
	account := login(t, s, "pending")
	if _, err := s.CreateRole(ctx, account.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the role create reply")
	}
	if slot := mustSlot(t, cfg, account.ID); slot.PlayerID != 0 || slot.Creation.Name != "Hero" || roles.first == 0 {
		t.Fatalf("fixture: want a pending plan for Hero with the role record written, got slot %+v, role %d", slot, roles.first)
	}
	got, err := s.RoleOnServer(ctx, account.ID, 1)
	if !errors.Is(err, ErrRoleMissing) || got.PlayerID != 0 || got.Name != "" {
		t.Fatalf("a pending plan answered %+v, %v; want ErrRoleMissing and no role", got, err)
	}
	if strings.Contains(err.Error(), "Hero") {
		t.Fatalf("the refusal tells the pending name: %v", err)
	}
	// SelectRole 对同一个角色的回答是“还在对账”，两边一致地不认它。
	if _, err := s.SelectRole(ctx, account.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("SelectRole of the unpublished role = %v, want ErrConflict", err)
	}
	role, err := s.CreateRole(ctx, account.ID, 1, "Hero")
	if err != nil {
		t.Fatalf("the same-name retry did not finish the plan: %v", err)
	}
	found, err := s.RoleOnServer(ctx, account.ID, 1)
	if err != nil || found.PlayerID != role.PlayerID || found.PlayerID != roles.first || found.Name != "Hero" {
		t.Fatalf("after the retry RoleOnServer = %+v, %v; want role %d", found, err, role.PlayerID)
	}
}

// slot 与角色对不上的记录一律 ErrConflict：宁可不答，也不把别的账号的角色、或 SelectRole 不认的角色交出去。
func TestRoleOnServerRefusesASlotThatDoesNotMatchItsRole(t *testing.T) {
	ctx := context.Background()
	for label, corrupt := range map[string]func(t *testing.T, cfg Config, mine, theirs Role){
		"the slot names another account": func(t *testing.T, cfg Config, mine, theirs Role) {
			editSlot(t, cfg, mine.AccountID, func(slot *Slot) { slot.AccountID = theirs.AccountID })
		},
		"the slot names another server": func(t *testing.T, cfg Config, mine, _ Role) {
			editSlot(t, cfg, mine.AccountID, func(slot *Slot) { slot.ServerID = 9 })
		},
		"the slot points at another account's role": func(t *testing.T, cfg Config, mine, theirs Role) {
			editSlot(t, cfg, mine.AccountID, func(slot *Slot) { slot.PlayerID = theirs.PlayerID })
		},
		"the slot is a copy of another account's slot": func(t *testing.T, cfg Config, mine, theirs Role) {
			// 连建角计划都照抄：slot 与那个角色“对得上”，只有角色记录上的账号说明它不是问的这个账号的。
			foreign, _ := slotAt(t, cfg, theirs.AccountID, 1)
			editSlot(t, cfg, mine.AccountID, func(slot *Slot) { slot.PlayerID, slot.Creation = foreign.PlayerID, foreign.Creation })
		},
		"the slot points at a role that does not exist": func(t *testing.T, cfg Config, mine, _ Role) {
			editSlot(t, cfg, mine.AccountID, func(slot *Slot) { slot.PlayerID = 424242 })
		},
		"the slot belongs to another bind generation": func(t *testing.T, cfg Config, mine, _ Role) {
			editSlot(t, cfg, mine.AccountID, func(slot *Slot) { slot.BindEpoch = 5 })
		},
		"the slot carries another creation plan": func(t *testing.T, cfg Config, mine, _ Role) {
			editSlot(t, cfg, mine.AccountID, func(slot *Slot) { slot.Creation.ID = "someone-elses-plan" })
		},
		"the role has no creation identity": func(t *testing.T, cfg Config, mine, _ Role) {
			if _, _, err := cfg.Roles.Update(context.Background(), mine.PlayerID, func(current Role, _ bool) (Role, bool, error) {
				current.CreationID = ""
				return current, true, nil
			}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		s, _, cfg := pendingContractService(t)
		mine, _ := roleOn(t, s, "mine-"+strings.ReplaceAll(label, " ", "-"), 1, "Mine"+label[len(label)-6:])
		theirs, _ := roleOn(t, s, "theirs-"+strings.ReplaceAll(label, " ", "-"), 1, "Theirs"+label[len(label)-6:])
		if found, err := s.RoleOnServer(ctx, mine.AccountID, 1); err != nil || found.PlayerID != mine.PlayerID {
			t.Fatalf("%s: before the corruption RoleOnServer = %+v, %v", label, found, err)
		}
		corrupt(t, cfg, mine, theirs)
		got, err := s.RoleOnServer(ctx, mine.AccountID, 1)
		if !errors.Is(err, ErrConflict) || Code(err) != CodeConflict {
			t.Fatalf("%s: err = %v, want ErrConflict", label, err)
		}
		if got.PlayerID != 0 || got.AccountID != "" || got.Name != "" {
			t.Fatalf("%s: the refusal carried a role: %+v", label, got)
		}
	}
}

// editSlot 直接改存储里 accountID 在 1 服的 slot：造出服务自己不会写出来的记录。
func editSlot(t *testing.T, cfg Config, accountID string, edit func(*Slot)) {
	t.Helper()
	if _, _, err := cfg.Slots.Update(context.Background(), slotKeyFor(accountID, 1), func(current Slot, found bool) (Slot, bool, error) {
		if !found {
			return current, false, errors.New("no slot to edit")
		}
		edit(&current)
		return current, true, nil
	}); err != nil {
		t.Fatal(err)
	}
}
