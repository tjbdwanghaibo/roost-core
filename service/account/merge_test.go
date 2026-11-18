package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// 别名合服的单元契约（docs/maintenance/FEATURE-2026-10-20-server-merge-alias.md §8）：角色身份不变，
// 承载服由 account 唯一的解析点给出，记录只有一跳。存储缺省是 Memory；ROOST_REVIEW3_BACKEND=redis 且
// ROOST_REVIEW_REDIS 指向隔离 Redis 时（CI service-redis job）走真实 Redis（pendingContractService，唯一前缀、
// 结束删除）。并发交错那一条要在写之间插入动作，固定用 Memory 包一层。

const mergeNote = "merge window 2026-10-20"

func upsertOpen(t *testing.T, s *Service, ids ...int32) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.UpsertServer(context.Background(), GameServer{ID: id, Status: ServerOpen}); err != nil {
			t.Fatal(err)
		}
	}
}

// roleOn creates a role for openID on serverID and returns it with a session.
func roleOn(t *testing.T, s *Service, openID string, serverID int32, name string) (Role, Session) {
	t.Helper()
	ctx := context.Background()
	account := login(t, s, openID)
	role, err := s.CreateRole(ctx, account.ID, serverID, name)
	if err != nil {
		t.Fatalf("create role on %d: %v", serverID, err)
	}
	session, err := s.SelectRole(ctx, account.ID, role.PlayerID)
	if err != nil {
		t.Fatalf("select role on %d: %v", serverID, err)
	}
	return role, session
}

func mustMerge(t *testing.T, s *Service, from, into int32) MergeResult {
	t.Helper()
	result, err := s.MergeServer(context.Background(), MergeRequest{From: from, Into: into, Note: mergeNote})
	if err != nil {
		t.Fatalf("merge %d into %d: %v", from, into, err)
	}
	return result
}

func storedServer(t *testing.T, cfg Config, id int32) GameServer {
	t.Helper()
	server, found, err := cfg.Servers.Get(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("server %d: found=%v err=%v", id, found, err)
	}
	return server.Value
}

// 每个返回角色 / 会话的出口都给出承载服：没有合服时等于原服；合服后原服不变、承载服是目标服。
func TestEveryRoleExitCarriesTheServingServer(t *testing.T) {
	s, _, _ := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2)
	role, session := roleOn(t, s, "u1", 2, "Alice")
	if role.ServingServerID != 2 || session.ServingServerID != 2 || session.ServerID != 2 {
		t.Fatalf("without a merge the serving server must be the origin: role %+v session %+v", role, session)
	}

	mustMerge(t, s, 2, 1)
	validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if validated.ServerID != 2 || validated.ServingServerID != 1 {
		t.Fatalf("ValidateSession after merging 2 into 1 = origin %d serving %d, want 2 / 1", validated.ServerID, validated.ServingServerID)
	}
	again, err := s.SelectRole(ctx, role.AccountID, role.PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ServerID != 2 || again.ServingServerID != 1 {
		t.Fatalf("SelectRole after the merge = origin %d serving %d, want 2 / 1", again.ServerID, again.ServingServerID)
	}
	profiled, err := s.UpdateProfile(ctx, role.AccountID, role.PlayerID, []byte("lv=3"))
	if err != nil {
		t.Fatal(err)
	}
	if profiled.ServerID != 2 || profiled.ServingServerID != 1 {
		t.Fatalf("UpdateProfile after the merge = origin %d serving %d, want 2 / 1", profiled.ServerID, profiled.ServingServerID)
	}
}

// 承载服只存在于返回值：存储里的角色记录永远不带它，否则 slot / 名字等按原服的操作会读到错的服。
func TestTheServingServerIsNeverStored(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2)
	role, session := roleOn(t, s, "u1", 2, "Alice")
	mustMerge(t, s, 2, 1)
	if _, err := s.ValidateSession(ctx, role.PlayerID, session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SelectRole(ctx, role.AccountID, role.PlayerID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateProfile(ctx, role.AccountID, role.PlayerID, []byte("p")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkLogout(ctx, role.AccountID, role.PlayerID); err != nil {
		t.Fatal(err)
	}
	stored, found, err := cfg.Roles.Get(ctx, role.PlayerID)
	if err != nil || !found {
		t.Fatalf("stored role: %v %v", found, err)
	}
	if stored.Value.ServingServerID != 0 || stored.Value.ServerID != 2 {
		t.Fatalf("stored role = origin %d serving %d, want origin 2 and no serving server", stored.Value.ServerID, stored.Value.ServingServerID)
	}
	encoded, err := versionstore.JSONCodec[Role]{}.Encode(stored.Value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "serving_server_id") {
		t.Fatalf("the stored encoding carries the serving server: %s", encoded)
	}
	// 经总线返回时它要随响应走：线上编码与存储编码是同一个 JSON 结构，非 0 时必须写出。
	wire, _ := json.Marshal(Role{ServerID: 2, ServingServerID: 1})
	if !strings.Contains(string(wire), `"serving_server_id":1`) {
		t.Fatalf("a non-zero serving server is dropped from the response encoding: %s", wire)
	}
}

// 多次合服拍平：先 3 并入 2，再 2 并入 1，3 的记录直接指向 1，解析只看一跳。
func TestRepeatedMergesAreFlattenedToOneHop(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2, 3)
	role, session := roleOn(t, s, "u3", 3, "Carol")
	mustMerge(t, s, 3, 2)
	result := mustMerge(t, s, 2, 1)
	if !slices.Equal(result.Repointed, []int32{3}) {
		t.Fatalf("merging 2 into 1 repointed %v, want [3]", result.Repointed)
	}
	if got := storedServer(t, cfg, 3).MergedInto; got != 1 {
		t.Fatalf("server 3 points at %d after the second merge, want 1 (flattened)", got)
	}
	if got := storedServer(t, cfg, 2); got.MergedInto != 1 || len(got.MergedFrom) != 0 || got.Status != ServerClosed {
		t.Fatalf("server 2 after merging into 1 = %+v", got)
	}
	if got := storedServer(t, cfg, 1).MergedFrom; !slices.Equal(got, []int32{2, 3}) {
		t.Fatalf("server 1 lists %v as merged into it, want [2 3]", got)
	}
	validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if validated.ServerID != 3 || validated.ServingServerID != 1 {
		t.Fatalf("a role of 3 after 3→2→1 = origin %d serving %d, want 3 / 1", validated.ServerID, validated.ServingServerID)
	}
}

// 解析遇到坏数据一律拒绝，不猜一个服：承载服记录缺失报 ErrServerInvalid，两跳（手工改坏或并发合服留下）
// 报 ErrServerMergeCycle。
func TestResolutionRefusesAMissingOrChainedServingServer(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2, 3)
	role, session := roleOn(t, s, "u2", 2, "Bob")
	setMerged := func(id, into int32) {
		t.Helper()
		if _, _, err := cfg.Servers.Update(ctx, id, func(current GameServer, _ bool) (GameServer, bool, error) {
			current.MergedInto = into
			return current, true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	setMerged(2, 9) // 9 was never registered
	if _, err := s.ValidateSession(ctx, role.PlayerID, session.Token); !errors.Is(err, ErrServerInvalid) {
		t.Fatalf("a role whose serving server is unknown validated: %v", err)
	}
	setMerged(2, 3)
	setMerged(3, 1) // 2 → 3 → 1: two hops
	if _, err := s.ValidateSession(ctx, role.PlayerID, session.Token); !errors.Is(err, ErrServerMergeCycle) {
		t.Fatalf("a two-hop alias validated: %v", err)
	}
	if _, err := s.SelectRole(ctx, role.AccountID, role.PlayerID); !errors.Is(err, ErrServerMergeCycle) {
		t.Fatalf("SelectRole over a two-hop alias = %v", err)
	}
}

// 已并入的服不再建角，错误说出承载服；承载服照常建角。
func TestAMergedServerRefusesNewRoles(t *testing.T) {
	s, _, _ := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2)
	mustMerge(t, s, 2, 1)
	account := login(t, s, "fresh")
	_, err := s.CreateRole(ctx, account.ID, 2, "Dana")
	if !errors.Is(err, ErrServerClosed) || !strings.Contains(err.Error(), "merged into 1") {
		t.Fatalf("CreateRole on a merged server = %v, want ErrServerClosed naming server 1", err)
	}
	// 运维把状态改回 open 也不行：并入以 MergedInto 为准，状态只是并入时顺带关闭。
	if _, err := s.UpsertServer(ctx, GameServer{ID: 2, Status: ServerOpen}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRole(ctx, account.ID, 2, "Dana"); !errors.Is(err, ErrServerClosed) {
		t.Fatalf("an open-status merged server accepted a role: %v", err)
	}
	if role, err := s.CreateRole(ctx, account.ID, 1, "Dana"); err != nil || role.ServingServerID != 1 {
		t.Fatalf("CreateRole on the serving server = %+v, %v", role, err)
	}
}

// 同一账号在两个服各有角色：合服后两个角色都能登录、各自承载在目标服，slot 与身份不变。
func TestAnAccountWithRolesOnBothServersKeepsBoth(t *testing.T) {
	s, _, _ := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2)
	onOne, sessionOne := roleOn(t, s, "twice", 1, "Ann")
	onTwo, sessionTwo := roleOn(t, s, "twice", 2, "Ann2")
	mustMerge(t, s, 2, 1)
	for _, check := range []struct {
		role    Role
		session Session
	}{{onOne, sessionOne}, {onTwo, sessionTwo}} {
		validated, err := s.ValidateSession(ctx, check.role.PlayerID, check.session.Token)
		if err != nil {
			t.Fatalf("role %d after the merge: %v", check.role.PlayerID, err)
		}
		if validated.PlayerID != check.role.PlayerID || validated.ServerID != check.role.ServerID || validated.ServingServerID != 1 {
			t.Fatalf("role %d after the merge = %+v", check.role.PlayerID, validated)
		}
	}
	// 各自的 slot 不变：在承载服上建第二个角色仍然被拒（承载服上本来就有一个）。
	if _, err := s.CreateRole(ctx, onOne.AccountID, 1, "Ann3"); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("a third role on the serving server = %v, want ErrRoleLimit", err)
	}
}

// MergeServer 的写入校验：同一对重复执行幂等；目标服自己已并入、会形成环、来源已并入别处、服不存在、
// 自己并入自己、没有备注都拒绝，且拒绝时什么也不写。
func TestMergeServerValidatesBeforeWriting(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2, 3, 4)
	first := mustMerge(t, s, 2, 1)
	if !first.Changed {
		t.Fatalf("the first merge reported no change: %+v", first)
	}
	stamp := storedServer(t, cfg, 2).MergedAtUnix
	again := mustMerge(t, s, 2, 1)
	if again.Changed || storedServer(t, cfg, 2).MergedAtUnix != stamp {
		t.Fatalf("repeating the same merge changed the records: %+v", again)
	}
	for _, refused := range []struct {
		label   string
		request MergeRequest
		want    error
	}{
		{"target is itself merged", MergeRequest{From: 3, Into: 2, Note: mergeNote}, ErrServerMergeCycle},
		{"would form a cycle", MergeRequest{From: 1, Into: 2, Note: mergeNote}, ErrServerMergeCycle},
		{"source merged elsewhere", MergeRequest{From: 2, Into: 4, Note: mergeNote}, ErrConflict},
		{"unknown source", MergeRequest{From: 9, Into: 1, Note: mergeNote}, ErrServerInvalid},
		{"unknown target", MergeRequest{From: 3, Into: 9, Note: mergeNote}, ErrServerInvalid},
		{"into itself", MergeRequest{From: 3, Into: 3, Note: mergeNote}, ErrServerInvalid},
		{"no note", MergeRequest{From: 3, Into: 1}, ErrAdminNoteRequired},
	} {
		before := []GameServer{storedServer(t, cfg, 1), storedServer(t, cfg, 2), storedServer(t, cfg, 3), storedServer(t, cfg, 4)}
		if _, err := s.MergeServer(ctx, refused.request); !errors.Is(err, refused.want) {
			t.Fatalf("%s: MergeServer = %v, want %v", refused.label, err, refused.want)
		}
		after := []GameServer{storedServer(t, cfg, 1), storedServer(t, cfg, 2), storedServer(t, cfg, 3), storedServer(t, cfg, 4)}
		for i := range before {
			if before[i].MergedInto != after[i].MergedInto || !slices.Equal(before[i].MergedFrom, after[i].MergedFrom) || before[i].Status != after[i].Status {
				t.Fatalf("%s: a refused merge changed server %d: %+v -> %+v", refused.label, before[i].ID, before[i], after[i])
			}
		}
	}
}

// -dry-run 只回答会改什么，不写。
func TestADryRunMergeWritesNothing(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2, 3)
	mustMerge(t, s, 3, 2)
	plan, err := s.MergeServer(ctx, MergeRequest{From: 2, Into: 1, Note: mergeNote, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || !plan.Changed || !slices.Equal(plan.Repointed, []int32{3}) {
		t.Fatalf("dry-run plan = %+v", plan)
	}
	if storedServer(t, cfg, 2).MergedInto != 0 || storedServer(t, cfg, 3).MergedInto != 2 || len(storedServer(t, cfg, 1).MergedFrom) != 0 {
		t.Fatal("a dry-run merge wrote records")
	}
}

// 解除合服后原服的角色回到原服，状态保持 closed（运维重新部署原服后再 upsert-server -status open），
// 承载服的索引不再列它；重复解除幂等。
func TestUnmergeServerRestoresTheOrigin(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2)
	role, session := roleOn(t, s, "u2", 2, "Bob")
	mustMerge(t, s, 2, 1)
	result, err := s.UnmergeServer(ctx, 2, "rollback after the merge check failed")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Into != 1 {
		t.Fatalf("unmerge result = %+v", result)
	}
	if got := storedServer(t, cfg, 2); got.MergedInto != 0 || got.Status != ServerClosed {
		t.Fatalf("server 2 after unmerge = %+v, want no alias and still closed", got)
	}
	if got := storedServer(t, cfg, 1).MergedFrom; len(got) != 0 {
		t.Fatalf("server 1 still lists %v as merged into it", got)
	}
	validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if validated.ServingServerID != 2 {
		t.Fatalf("after unmerge the role is served by %d, want its origin 2", validated.ServingServerID)
	}
	if again, err := s.UnmergeServer(ctx, 2, "again"); err != nil || again.Changed {
		t.Fatalf("a repeated unmerge = %+v, %v", again, err)
	}
	if _, err := s.UnmergeServer(ctx, 2, ""); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("unmerge without a note = %v", err)
	}
	if _, err := s.UnmergeServer(ctx, 9, "unknown"); !errors.Is(err, ErrServerInvalid) {
		t.Fatalf("unmerge of an unknown server = %v", err)
	}
}

// UpsertServer 改名、改状态都不碰合服字段：它们只由 MergeServer / UnmergeServer 写。
func TestUpsertServerKeepsTheMergeFields(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	upsertOpen(t, s, 2)
	mustMerge(t, s, 2, 1)
	if _, err := s.UpsertServer(ctx, GameServer{ID: 2, Name: "renamed", Status: ServerMaintenance}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertServer(ctx, GameServer{ID: 1, Name: "host"}); err != nil {
		t.Fatal(err)
	}
	if got := storedServer(t, cfg, 2); got.MergedInto != 1 || got.Name != "renamed" || got.MergeNote != mergeNote {
		t.Fatalf("server 2 after upsert = %+v", got)
	}
	if got := storedServer(t, cfg, 1).MergedFrom; !slices.Equal(got, []int32{2}) {
		t.Fatalf("server 1 after upsert lists %v, want [2]", got)
	}
}

// mergeRaceStore runs a second merge right before the first merge writes its source record — the
// interleaving two operators merging at once can produce: 2 into 1 while 1 goes into 3.
type mergeRaceStore struct {
	versionstore.Store[int32, GameServer]
	before func(id int32)
}

func (store *mergeRaceStore) Update(ctx context.Context, id int32, mutate versionstore.Mutate[GameServer]) (versionstore.Versioned[GameServer], bool, error) {
	if hook := store.before; hook != nil {
		hook(id)
	}
	return store.Store.Update(ctx, id, mutate)
}

// 跨记录的写不是原子的：两个合服并发时，最后一步复核发现承载服在途中被并入别处就报 ErrServerMergeCycle 并说出
// 修复命令，不把一个两跳的结果当作成功。
func TestConcurrentMergesThatChainAreReported(t *testing.T) {
	race := &mergeRaceStore{Store: versionstore.NewMemoryStore[int32, GameServer]()}
	s, _, _ := newService(t, func(cfg *Config) { cfg.Servers = race })
	ctx := context.Background()
	upsertOpen(t, s, 2, 3)
	fired := false
	race.before = func(id int32) {
		if id != 2 || fired {
			return
		}
		fired = true
		race.before = nil
		if _, err := s.MergeServer(ctx, MergeRequest{From: 1, Into: 3, Note: "the other operator"}); err != nil {
			t.Errorf("the racing merge: %v", err)
		}
	}
	_, err := s.MergeServer(ctx, MergeRequest{From: 2, Into: 1, Note: mergeNote})
	if !fired {
		t.Fatal("the race hook never ran; the test does not exercise the interleaving")
	}
	if !errors.Is(err, ErrServerMergeCycle) || !strings.Contains(err.Error(), "unmerge-server") {
		t.Fatalf("a merge whose target was merged meanwhile = %v, want ErrServerMergeCycle with the repair commands", err)
	}
}

// 合服前置条件（换服方案三期“合服清单加无在途换服”）：被并走的服是在途换服的来源或目标时，MergeServer（含
// dry-run）拒绝、什么也不写，并列出这些换服；了结之后同一次合服照常。与被并走的服无关的换服（别处来去、去承载服）不挡。
func TestAMergeIsRefusedWhileATransferInvolvesTheMergedServer(t *testing.T) {
	s, _, cfg := pendingContractService(t)
	ctx := context.Background()
	const merged, host, other int32 = 2, 1, 3
	upsertOpen(t, s, merged, other)
	leaving, _ := roleOn(t, s, "leaving", merged, "Leaving")
	arriving, _ := roleOn(t, s, "arriving", other, "Arriving")
	toHost, _ := roleOn(t, s, "tohost", other, "ToHost")
	mustBegin(t, s, leaving.PlayerID, transferTo("out", merged, other, 0))
	mustBegin(t, s, arriving.PlayerID, transferTo("in", other, merged, 0))
	mustBegin(t, s, toHost.PlayerID, transferTo("unrelated", other, host, 0))

	unchanged := func(label string) {
		t.Helper()
		if got := storedServer(t, cfg, merged); got.MergedInto != 0 || got.Status != ServerOpen {
			t.Fatalf("%s: a refused merge wrote server %d: %+v", label, merged, got)
		}
		if got := storedServer(t, cfg, host).MergedFrom; len(got) != 0 {
			t.Fatalf("%s: a refused merge wrote server %d's index: %v", label, host, got)
		}
	}
	for _, dryRun := range []bool{true, false} {
		result, err := s.MergeServer(ctx, MergeRequest{From: merged, Into: host, Note: mergeNote, DryRun: dryRun})
		if !errors.Is(err, ErrTransferInProgress) {
			t.Fatalf("merge (dry-run %v) with transfers in flight from and onto %d = %v, want ErrTransferInProgress", dryRun, merged, err)
		}
		var listed []string
		for _, flight := range result.TransfersInFlight {
			listed = append(listed, fmt.Sprintf("%d:%s", flight.PlayerID, flight.Transfer.ID))
		}
		slices.Sort(listed)
		if want := []string{fmt.Sprintf("%d:out", leaving.PlayerID), fmt.Sprintf("%d:in", arriving.PlayerID)}; !slices.Equal(listed, slices.Sorted(slices.Values(want))) {
			t.Fatalf("the refused merge lists %v, want %v", listed, want)
		}
		for _, want := range []string{"out", "in", "transfer-role abort"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal does not name %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "unrelated") {
			t.Fatalf("the refusal names a transfer that does not involve server %d: %v", merged, err)
		}
		unchanged(fmt.Sprintf("dry-run=%v", dryRun))
	}

	// 了结一个还不够；两个都了结之后合服照常（去承载服的那个仍在途，不挡）。
	if _, err := s.AbortRoleTransfer(ctx, leaving.PlayerID, "out", "merge window"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeServer(ctx, MergeRequest{From: merged, Into: host, Note: mergeNote}); !errors.Is(err, ErrTransferInProgress) {
		t.Fatalf("merge with the inbound transfer still in flight = %v, want ErrTransferInProgress", err)
	}
	unchanged("one settled")
	mustCommit(t, s, arriving.PlayerID, "in")
	if result := mustMerge(t, s, merged, host); !result.Changed {
		t.Fatalf("the merge after both transfers settled changed nothing: %+v", result)
	}
}

// indexedRoles 是带在途换服索引的角色存储（MemoryRoles 或 Redis 上的 redisRoles）。
type indexedRoles interface {
	versionstore.Store[int64, Role]
	TransferIndex
}

// rolesAfterIndexRead 在第一次读在途索引返回之后执行一次 afterRead：模拟“合服预检读过索引、还没读完”时别处了结了一笔换服。
type rolesAfterIndexRead struct {
	indexedRoles
	afterRead func()
}

func (r *rolesAfterIndexRead) TransfersDue(ctx context.Context, deadlineUnix int64, offset, limit int) ([]int64, error) {
	ids, err := r.indexedRoles.TransfersDue(ctx, deadlineUnix, offset, limit)
	if f := r.afterRead; f != nil {
		r.afterRead = nil
		f()
	}
	return ids, err
}

// 合服预检读在途索引时，别的换服在读的过程中了结（条目从索引里消失、后面的前移），不能让一笔从头到尾都在途、
// 以被并走的服为目标的换服漏检（RV-20261118-05）。按偏移分页时，第一页之后了结一笔，下一页从原偏移读起就跳过一条；
// 预检必须读索引的同一时刻的全集。按期限排序，被漏检的那笔排在一整页（修复前的页长 200 笔无关换服）之后。
// 存储缺省是 Memory；ROOST_REVIEW3_BACKEND=redis 时走隔离的真实 Redis（redisRoles，索引是有序集合）。
func TestAMergeCheckSeesEveryTransferWhileOthersSettle(t *testing.T) {
	// scanPageBefore 是修复前列举一次读索引的条数（按偏移翻页）：多出一页加一笔，漏检才会出现。
	const scanPageBefore = 200
	var roles *rolesAfterIndexRead
	s, _, cfg := pendingContractService(t, func(c *Config) {
		roles = &rolesAfterIndexRead{indexedRoles: c.Roles.(indexedRoles)}
		c.Roles = roles
	})
	ctx := context.Background()
	const merged, host, other int32 = 2, 1, 3
	upsertOpen(t, s, merged, other)
	// 期限逐笔递增（索引按期限排序；同分时 Redis 按成员字符串排，不是按玩家号），两种存储排出同一个顺序。
	players := make([]int64, scanPageBefore+1)
	for i := range players {
		name := fmt.Sprintf("p%d", i)
		role, _ := roleOn(t, s, name, other, name)
		target := host
		if i == scanPageBefore {
			target = merged // 期限最晚、排在最后的这一笔以被并走的服为目标
		}
		mustBegin(t, s, role.PlayerID, TransferRequest{ID: name, FromSID: other, To: target, TTLSeconds: int64(60 + i)})
		players[i] = role.PlayerID
	}
	staying := players[scanPageBefore]
	roles.afterRead = func() {
		if _, err := s.AbortRoleTransfer(ctx, players[0], "p0", "settled while the merge check reads the index"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.MergeServer(ctx, MergeRequest{From: merged, Into: host, Note: mergeNote})
	if roles.afterRead != nil {
		t.Fatal("the merge check never read the transfer index; the case proves nothing")
	}
	if storedRole(t, cfg, staying).Value.Transfer == nil {
		t.Fatal("the transfer onto the merged server is no longer in flight; the case proves nothing")
	}
	if !errors.Is(err, ErrTransferInProgress) {
		t.Errorf("merge with a transfer onto %d in flight the whole time = %v, want ErrTransferInProgress", merged, err)
	}
	if got := storedServer(t, cfg, merged); got.MergedInto != 0 {
		t.Errorf("the merge check missed the transfer onto %d and wrote the merge: %+v", merged, got)
	}

	// 列举走同一次读：读的过程中了结别的换服，仍列得出这笔。p0 已了结，补一笔期限最早的，让索引再满一页加一笔。
	early, _ := roleOn(t, s, "early", other, "early")
	mustBegin(t, s, early.PlayerID, TransferRequest{ID: "early", FromSID: other, To: host, TTLSeconds: 30})
	roles.afterRead = func() {
		if _, err := s.AbortRoleTransfer(ctx, early.PlayerID, "early", "settled while listing"); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := s.ListRoleTransfers(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if roles.afterRead != nil {
		t.Fatal("listing never read the transfer index")
	}
	if !slices.ContainsFunc(listed, func(b RoleBinding) bool { return b.PlayerID == staying }) {
		t.Fatalf("listing while another transfer settles misses player %d (%d listed)", staying, len(listed))
	}
}
