package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"github.com/tjbdwanghaibo/roost-core/service/directory"
)

// 单个角色换服（transfer.go）的契约。不变量：
//
//   - 角色记录是唯一的权威：提交还是中止由它的一次版本比较决定，已提交的不能被中止，已中止的不能被提交。
//   - 角色任何时刻都可用：slot 先加后删，删只发生在角色记录说了算之后。
//   - 任何一步中途停下，重发同一个请求补完；同一个 ID 重发幂等。
//
// 存储缺省是 Memory；ROOST_REVIEW3_BACKEND=redis 且 ROOST_REVIEW_REDIS 指向隔离 Redis 时走真实 Redis
// （pendingContractService）。注入故障的用例在选定的存储外面包一层，两种存储都跑。

const (
	homeSID  int32 = 1
	awaySID  int32 = 2
	thirdSID int32 = 3
)

// transferFixture 给出一个在 homeSID 上建好角色的服务；awaySID、thirdSID 已登记并开放。
func transferFixture(t *testing.T, mutate ...func(*Config)) (*Service, Config, Role, Session) {
	t.Helper()
	s, _, cfg := pendingContractService(t, mutate...)
	upsertOpen(t, s, awaySID, thirdSID)
	role, session := roleOn(t, s, "mover", homeSID, "Mover")
	return s, cfg, role, session
}

func transferTo(id string, from, to int32, epoch uint64) TransferRequest {
	return TransferRequest{ID: id, FromSID: from, To: to, ExpectEpoch: epoch}
}

func mustBegin(t *testing.T, s *Service, playerID int64, request TransferRequest) RoleBinding {
	t.Helper()
	binding, err := s.BeginRoleTransfer(context.Background(), playerID, request)
	if err != nil {
		t.Fatalf("begin transfer %s of %d from %d to %d: %v", request.ID, playerID, request.FromSID, request.To, err)
	}
	return binding
}

func mustCommit(t *testing.T, s *Service, playerID int64, id string) RoleBinding {
	t.Helper()
	binding, err := s.CommitRoleTransfer(context.Background(), playerID, id)
	if err != nil {
		t.Fatalf("commit transfer %s of %d: %v", id, playerID, err)
	}
	return binding
}

func storedRole(t *testing.T, cfg Config, playerID int64) versionstore.Versioned[Role] {
	t.Helper()
	role, found, err := cfg.Roles.Get(context.Background(), playerID)
	if err != nil || !found {
		t.Fatalf("role %d: found=%v err=%v", playerID, found, err)
	}
	return role
}

func slotAt(t *testing.T, cfg Config, accountID string, serverID int32) (Slot, bool) {
	t.Helper()
	slot, found, err := cfg.Slots.Get(context.Background(), slotKeyFor(accountID, serverID))
	if err != nil {
		t.Fatalf("slot of %s on %d: %v", accountID, serverID, err)
	}
	return slot.Value, found
}

// mustBeBoundTo 核对角色绑定在 serverID 上并且可用：ValidateSession、SelectRole、UpdateProfile 都给出
// 这个服并通过 roleReady，slot 属于这个角色、属于当前这一代绑定。
func mustBeBoundTo(t *testing.T, s *Service, cfg Config, role Role, token string, serverID int32, epoch uint64) {
	t.Helper()
	ctx := context.Background()
	validated, err := s.ValidateSession(ctx, role.PlayerID, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if validated.ServerID != serverID || validated.ServingServerID != serverID || validated.BindEpoch != epoch {
		t.Fatalf("ValidateSession = server %d serving %d epoch %d, want %d / %d / %d",
			validated.ServerID, validated.ServingServerID, validated.BindEpoch, serverID, serverID, epoch)
	}
	session, err := s.SelectRole(ctx, role.AccountID, role.PlayerID)
	if err != nil {
		t.Fatalf("SelectRole: %v", err)
	}
	if session.ServerID != serverID || session.ServingServerID != serverID {
		t.Fatalf("SelectRole = server %d serving %d, want %d", session.ServerID, session.ServingServerID, serverID)
	}
	if _, err := s.UpdateProfile(ctx, role.AccountID, role.PlayerID, []byte("p")); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	slot, found := slotAt(t, cfg, role.AccountID, serverID)
	if !found || slot.PlayerID != role.PlayerID || slot.Creation.ID != role.CreationID || slot.BindEpoch != epoch {
		t.Fatalf("slot on %d = %+v (found %v), want the role's slot at epoch %d", serverID, slot, found, epoch)
	}
}

// Begin 记下换服并在目标服占位；角色仍绑定源服。同一个 ID 重发幂等，另一次换服被拒。
func TestBeginRoleTransferRecordsTheTransferAndHoldsTheTargetSlot(t *testing.T) {
	s, cfg, role, _ := transferFixture(t)
	ctx := context.Background()

	before, err := s.RoleBinding(ctx, role.PlayerID)
	if err != nil {
		t.Fatal(err)
	}
	if before.PlayerID != role.PlayerID || before.ServerID != homeSID || before.ServingServerID != homeSID || before.BindEpoch != 0 || before.Transfer != nil || before.LastTransfer != nil {
		t.Fatalf("binding of a role that never moved = %+v", before)
	}

	binding := mustBegin(t, s, role.PlayerID, TransferRequest{ID: "t-1", FromSID: homeSID, To: awaySID, Note: "player asked"})
	transfer := binding.Transfer
	if binding.ServerID != homeSID || binding.ServingServerID != homeSID || binding.BindEpoch != 0 || transfer == nil {
		t.Fatalf("binding after Begin = %+v, want the source server with a transfer in flight", binding)
	}
	if transfer.ID != "t-1" || transfer.From != homeSID || transfer.To != awaySID || transfer.Epoch != 0 || transfer.Note != "player asked" ||
		transfer.StartedAtUnix == 0 || transfer.DeadlineUnix != transfer.StartedAtUnix+int64(DefaultTransferTTL/time.Second) {
		t.Fatalf("transfer = %+v, want t-1 from %d to %d with the default deadline", transfer, homeSID, awaySID)
	}
	stored := storedRole(t, cfg, role.PlayerID)
	if stored.Value.ServerID != homeSID || stored.Value.Transfer == nil || stored.Value.Transfer.ID != "t-1" {
		t.Fatalf("stored role after Begin = %+v", stored.Value)
	}
	hold, found := slotAt(t, cfg, role.AccountID, awaySID)
	if !found || hold.TransferID != "t-1" || hold.PlayerID != role.PlayerID || hold.Creation.ID != role.CreationID || hold.BindEpoch != 1 || hold.ServerID != awaySID {
		t.Fatalf("target slot after Begin = %+v (found %v), want the hold of t-1 for epoch 1", hold, found)
	}
	if source, found := slotAt(t, cfg, role.AccountID, homeSID); !found || source.TransferID != "" || source.BindEpoch != 0 {
		t.Fatalf("source slot after Begin = %+v (found %v), want it untouched", source, found)
	}
	if read, err := s.RoleBinding(ctx, role.PlayerID); err != nil || read.Transfer == nil || *read.Transfer != *transfer {
		t.Fatalf("RoleBinding during the transfer = %+v, %v", read, err)
	}

	// 同一个 ID 重发：同一个结果，什么也不写。
	again, err := s.BeginRoleTransfer(ctx, role.PlayerID, TransferRequest{ID: "t-1", FromSID: homeSID, To: awaySID, Note: "player asked"})
	if err != nil || again.Transfer == nil || *again.Transfer != *transfer {
		t.Fatalf("resent Begin = %+v, %v; want the same transfer", again, err)
	}
	if after := storedRole(t, cfg, role.PlayerID); after.Version != stored.Version {
		t.Fatalf("a resent Begin wrote the role again: version %d -> %d", stored.Version, after.Version)
	}

	// 另一个 ID、或同一个 ID 换了目标：拒绝，并带回在途的那一次。
	for _, request := range []TransferRequest{transferTo("t-2", homeSID, thirdSID, 0), transferTo("t-1", homeSID, thirdSID, 0)} {
		refused, err := s.BeginRoleTransfer(ctx, role.PlayerID, request)
		if !errors.Is(err, ErrTransferInProgress) {
			t.Fatalf("Begin %s to %d during t-1 = %v, want ErrTransferInProgress", request.ID, request.To, err)
		}
		if refused.Transfer == nil || refused.Transfer.ID != "t-1" || refused.Transfer.To != awaySID {
			t.Fatalf("the refusal does not carry the transfer in flight: %+v", refused)
		}
	}
	if _, found := slotAt(t, cfg, role.AccountID, thirdSID); found {
		t.Fatal("a refused Begin left a hold on the third server")
	}
}

// Begin 的每一种拒绝都什么也不写：角色没有 Transfer，目标服没有多出占位。
func TestBeginRoleTransferRefusesWithoutWriting(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, s *Service, cfg Config, role Role)
		request TransferRequest
		want    error
		// carriesBinding：拒绝要带回当前绑定，调用方据此重读。
		carriesBinding bool
	}{
		{name: "stale epoch", request: transferTo("t", homeSID, awaySID, 7), want: ErrTransferStale, carriesBinding: true},
		{name: "wrong source", request: transferTo("t", thirdSID, awaySID, 0), want: ErrTransferStale, carriesBinding: true},
		{name: "unknown target", request: transferTo("t", homeSID, 99, 0), want: ErrServerInvalid},
		{name: "non-positive target", request: transferTo("t", homeSID, 0, 0), want: ErrServerInvalid},
		{name: "target is the current server", request: transferTo("t", homeSID, homeSID, 0), want: ErrServerInvalid},
		{name: "target merged into another server", request: transferTo("t", homeSID, thirdSID, 0), want: ErrServerClosed,
			arrange: func(t *testing.T, s *Service, _ Config, _ Role) { mustMerge(t, s, thirdSID, awaySID) }},
		{name: "target closed", request: transferTo("t", homeSID, awaySID, 0), want: ErrServerClosed,
			arrange: func(t *testing.T, s *Service, _ Config, _ Role) {
				if _, err := s.UpsertServer(ctx, GameServer{ID: awaySID, Status: ServerClosed}); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "target in maintenance", request: transferTo("t", homeSID, awaySID, 0), want: ErrServerClosed,
			arrange: func(t *testing.T, s *Service, _ Config, _ Role) {
				if _, err := s.UpsertServer(ctx, GameServer{ID: awaySID, Status: ServerMaintenance}); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "account already has a role on the target", request: transferTo("t", homeSID, awaySID, 0), want: ErrRoleLimit,
			arrange: func(t *testing.T, s *Service, _ Config, role Role) {
				if _, err := s.CreateRole(ctx, role.AccountID, awaySID, "Second"); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "account has a pending creation plan on the target", request: transferTo("t", homeSID, awaySID, 0), want: ErrRoleLimit,
			arrange: func(t *testing.T, _ *Service, cfg Config, role Role) {
				plan := Slot{AccountID: role.AccountID, ServerID: awaySID, Creation: RoleCreation{ID: "pending-plan", PlayerID: 777, Name: "Pending"}}
				if _, created, err := cfg.Slots.Create(ctx, slotKeyFor(role.AccountID, awaySID), plan); err != nil || !created {
					t.Fatalf("arrange a pending plan: created=%v err=%v", created, err)
				}
			}},
		{name: "empty transfer id", request: transferTo("  ", homeSID, awaySID, 0), want: ErrRangeInvalid},
		{name: "negative ttl", request: TransferRequest{ID: "t", FromSID: homeSID, To: awaySID, TTLSeconds: -1}, want: ErrRangeInvalid},
		{name: "ttl above the bound", request: TransferRequest{ID: "t", FromSID: homeSID, To: awaySID, TTLSeconds: int64(MaxTransferTTL/time.Second) + 1}, want: ErrRangeInvalid},
		{name: "oversized note", request: TransferRequest{ID: "t", FromSID: homeSID, To: awaySID, Note: strings.Repeat("n", MaxAdminNoteBytes+1)}, want: ErrRangeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg, role, session := transferFixture(t)
			if tc.arrange != nil {
				tc.arrange(t, s, cfg, role)
			}
			targetBefore, targetFound := slotAt(t, cfg, role.AccountID, tc.request.To)
			binding, err := s.BeginRoleTransfer(ctx, role.PlayerID, tc.request)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Begin = %v, want %v", err, tc.want)
			}
			if tc.carriesBinding && (binding.PlayerID != role.PlayerID || binding.ServerID != homeSID || binding.BindEpoch != 0) {
				t.Fatalf("the refusal carries %+v, want the current binding on %d at epoch 0", binding, homeSID)
			}
			if stored := storedRole(t, cfg, role.PlayerID); stored.Value.Transfer != nil || stored.Value.ServerID != homeSID {
				t.Fatalf("a refused Begin changed the role: %+v", stored.Value)
			}
			if after, found := slotAt(t, cfg, role.AccountID, tc.request.To); found != targetFound || after != targetBefore {
				t.Fatalf("a refused Begin changed the target slot: %+v (found %v) -> %+v (found %v)", targetBefore, targetFound, after, found)
			}
			mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
		})
	}

	t.Run("missing role", func(t *testing.T) {
		s, _, _, _ := transferFixture(t)
		if _, err := s.BeginRoleTransfer(ctx, 424242, transferTo("t", homeSID, awaySID, 0)); !errors.Is(err, ErrRoleMissing) {
			t.Fatalf("Begin for a missing role = %v, want ErrRoleMissing", err)
		}
		if _, err := s.RoleBinding(ctx, 424242); !errors.Is(err, ErrRoleMissing) {
			t.Fatalf("RoleBinding for a missing role = %v, want ErrRoleMissing", err)
		}
	})
	t.Run("role whose creation is not published", func(t *testing.T) {
		s, cfg, role, _ := transferFixture(t)
		unready := Role{CreationID: "unpublished", PlayerID: 515151, AccountID: role.AccountID, ServerID: thirdSID, Name: "Ghost"}
		if _, created, err := cfg.Roles.Create(ctx, unready.PlayerID, unready); err != nil || !created {
			t.Fatalf("arrange an unpublished role: created=%v err=%v", created, err)
		}
		if _, err := s.BeginRoleTransfer(ctx, unready.PlayerID, transferTo("t", thirdSID, awaySID, 0)); !errors.Is(err, ErrConflict) {
			t.Fatalf("Begin for an unpublished role = %v, want ErrConflict", err)
		}
		if _, err := s.RoleBinding(ctx, unready.PlayerID); !errors.Is(err, ErrConflict) {
			t.Fatalf("RoleBinding for an unpublished role = %v, want ErrConflict", err)
		}
		if stored := storedRole(t, cfg, unready.PlayerID); stored.Value.Transfer != nil {
			t.Fatalf("a refused Begin wrote the unpublished role: %+v", stored.Value)
		}
	})
}

// 换服在途：角色照常在源服可用，ValidateSession / SelectRole 带着 Transfer；目标服的名额已被占住，
// 在那里建角是 role_limit（同名也是），运维也不能把占位当作悬挂的建角计划释放。
func TestARoleInTransferStaysPlayableOnTheSourceAndHoldsTheTarget(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))

	validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token)
	if err != nil || validated.ServerID != homeSID || validated.ServingServerID != homeSID || validated.Transfer == nil || validated.Transfer.ID != "t-1" {
		t.Fatalf("ValidateSession during the transfer = %+v, %v; want the source server and the transfer", validated, err)
	}
	selected, err := s.SelectRole(ctx, role.AccountID, role.PlayerID)
	if err != nil || selected.ServerID != homeSID || selected.Transfer == nil || selected.Transfer.To != awaySID {
		t.Fatalf("SelectRole during the transfer = %+v, %v; want the source server and the transfer", selected, err)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)

	for _, name := range []string{"Other", role.Name} {
		if created, err := s.CreateRole(ctx, role.AccountID, awaySID, name); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("CreateRole %q on the held target = %+v, %v; want ErrRoleLimit", name, created, err)
		}
	}
	if _, err := s.ResolvePendingCreation(ctx, role.AccountID, awaySID, "not a creation plan"); !errors.Is(err, ErrNotResolvable) || !strings.Contains(err.Error(), "t-1") {
		t.Fatalf("ResolvePendingCreation on a transfer hold = %v, want ErrNotResolvable naming the transfer", err)
	}
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-1" {
		t.Fatalf("the hold did not survive the refusals: %+v (found %v)", hold, found)
	}
}

// Commit 把角色绑定到目标服：ValidateSession / SelectRole 给新服并通过 roleReady，旧服的 slot 让出。
// 同一个 ID 重发幂等。
func TestCommitRoleTransferRebindsTheRole(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, TransferRequest{ID: "t-1", FromSID: homeSID, To: awaySID, Note: "player asked"})

	binding := mustCommit(t, s, role.PlayerID, "t-1")
	last := binding.LastTransfer
	if binding.ServerID != awaySID || binding.ServingServerID != awaySID || binding.BindEpoch != 1 || binding.Transfer != nil || last == nil {
		t.Fatalf("binding after Commit = %+v, want server %d at epoch 1 with nothing in flight", binding, awaySID)
	}
	if last.ID != "t-1" || last.From != homeSID || last.To != awaySID || !last.Committed || last.Epoch != 1 || last.AtUnix == 0 || last.Note != "player asked" {
		t.Fatalf("LastTransfer after Commit = %+v", last)
	}
	// 旧会话票据仍然有效（令牌不含服），读到的是新服。
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
	if slot, _ := slotAt(t, cfg, role.AccountID, awaySID); slot.TransferID != "" {
		t.Fatalf("the target slot still carries the hold marker after Commit: %+v", slot)
	}
	if old, found := slotAt(t, cfg, role.AccountID, homeSID); found {
		t.Fatalf("the source slot outlived the commit: %+v", old)
	}
	if stored := storedRole(t, cfg, role.PlayerID); stored.Value.LastTransfer == nil || !stored.Value.LastTransfer.Settled {
		t.Fatalf("the committed transfer is not marked settled: %+v", stored.Value.LastTransfer)
	}

	again, err := s.CommitRoleTransfer(ctx, role.PlayerID, "t-1")
	if err != nil || again.ServerID != awaySID || again.BindEpoch != 1 || again.LastTransfer == nil || again.LastTransfer.ID != "t-1" {
		t.Fatalf("resent Commit = %+v, %v; want the committed binding", again, err)
	}
	// 让出的名额可以再用：同一个账号在旧服建一个新角色。
	fresh, err := s.CreateRole(ctx, role.AccountID, homeSID, "Fresh")
	if err != nil || fresh.PlayerID == role.PlayerID || fresh.ServerID != homeSID {
		t.Fatalf("CreateRole on the vacated server = %+v, %v; want a new role there", fresh, err)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// Abort 撤掉占位，角色留在源服可登录；同一个 ID 重发幂等。已中止的 ID 不能再提交，也不能重新发起。
func TestAbortRoleTransferKeepsTheRoleOnTheSource(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))

	binding, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "drain timed out")
	if err != nil {
		t.Fatal(err)
	}
	last := binding.LastTransfer
	if binding.ServerID != homeSID || binding.BindEpoch != 0 || binding.Transfer != nil || last == nil ||
		last.ID != "t-1" || last.Committed || last.Epoch != 0 || last.From != homeSID || last.To != awaySID || last.Note != "drain timed out" {
		t.Fatalf("binding after Abort = %+v (last %+v), want the source server with t-1 recorded as aborted", binding, last)
	}
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); found {
		t.Fatalf("the hold outlived the abort: %+v", hold)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
	if validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token); err != nil || validated.Transfer != nil {
		t.Fatalf("ValidateSession after Abort = %+v, %v; want no transfer", validated, err)
	}

	if again, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "resent"); err != nil || again.LastTransfer == nil || again.LastTransfer.Note != "drain timed out" {
		t.Fatalf("resent Abort = %+v, %v; want the recorded abort", again, err)
	}
	// 已中止的不能提交、不能重新发起：迟到的重复请求不会把一次已经放弃的换服做成。
	for name, call := range map[string]func() (RoleBinding, error){
		"Commit": func() (RoleBinding, error) { return s.CommitRoleTransfer(ctx, role.PlayerID, "t-1") },
		"Begin": func() (RoleBinding, error) {
			return s.BeginRoleTransfer(ctx, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
		},
	} {
		refused, err := call()
		if !errors.Is(err, ErrTransferStale) || refused.ServerID != homeSID || refused.Transfer != nil {
			t.Fatalf("%s of the aborted t-1 = %+v, %v; want ErrTransferStale with the binding on %d", name, refused, err, homeSID)
		}
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)

	// 让出的目标名额可以再占：换一个 ID 重新发起并提交。
	mustBegin(t, s, role.PlayerID, transferTo("t-2", homeSID, awaySID, 0))
	mustCommit(t, s, role.PlayerID, "t-2")
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// Abort 撞上已提交：答 ErrTransferStale 并带回已提交的绑定，什么也不撤。
func TestAbortAfterCommitReportsTheCommittedBinding(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	mustCommit(t, s, role.PlayerID, "t-1")

	binding, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "too late")
	if !errors.Is(err, ErrTransferStale) {
		t.Fatalf("Abort of a committed transfer = %v, want ErrTransferStale", err)
	}
	if binding.ServerID != awaySID || binding.BindEpoch != 1 || binding.LastTransfer == nil || !binding.LastTransfer.Committed {
		t.Fatalf("the refusal carries %+v, want the committed binding on %d", binding, awaySID)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// 中止一个从没发起成功的 ID：记下它，迟到的 Begin 不能再用这个 ID 把换服做起来。有别的换服在途时拒绝。
func TestAbortOfAnUnknownTransferFencesALateBegin(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()

	binding, err := s.AbortRoleTransfer(ctx, role.PlayerID, "never-began", "begin timed out")
	if err != nil || binding.ServerID != homeSID || binding.LastTransfer == nil || binding.LastTransfer.ID != "never-began" || binding.LastTransfer.Committed {
		t.Fatalf("Abort of an id that never began = %+v, %v; want it recorded as aborted", binding, err)
	}
	if late, err := s.BeginRoleTransfer(ctx, role.PlayerID, transferTo("never-began", homeSID, awaySID, 0)); !errors.Is(err, ErrTransferStale) || late.Transfer != nil {
		t.Fatalf("a late Begin of the aborted id = %+v, %v; want ErrTransferStale", late, err)
	}
	if _, found := slotAt(t, cfg, role.AccountID, awaySID); found {
		t.Fatal("the late Begin left a hold")
	}

	mustBegin(t, s, role.PlayerID, transferTo("t-2", homeSID, awaySID, 0))
	if other, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-9", "not mine"); !errors.Is(err, ErrTransferInProgress) || other.Transfer == nil || other.Transfer.ID != "t-2" {
		t.Fatalf("Abort of another id during t-2 = %+v, %v; want ErrTransferInProgress carrying t-2", other, err)
	}
	if other, err := s.CommitRoleTransfer(ctx, role.PlayerID, "t-9"); !errors.Is(err, ErrTransferStale) || other.Transfer == nil || other.Transfer.ID != "t-2" {
		t.Fatalf("Commit of another id during t-2 = %+v, %v; want ErrTransferStale carrying t-2", other, err)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
}

// faultySlots 在选定的 slot 存储外面包一层，开关打开时让对应的写失败（写没有发生）。
type faultySlots struct {
	versionstore.Store[string, Slot]
	failCreate, failUpdate, failDelete atomic.Bool
}

var errInjected = errors.New("injected store failure")

func (store *faultySlots) Create(ctx context.Context, key string, value Slot) (versionstore.Versioned[Slot], bool, error) {
	if store.failCreate.Load() {
		return versionstore.Versioned[Slot]{}, false, errInjected
	}
	return store.Store.Create(ctx, key, value)
}

func (store *faultySlots) Update(ctx context.Context, key string, mutate versionstore.Mutate[Slot]) (versionstore.Versioned[Slot], bool, error) {
	if store.failUpdate.Load() {
		return versionstore.Versioned[Slot]{}, false, errInjected
	}
	return store.Store.Update(ctx, key, mutate)
}

func (store *faultySlots) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	if store.failDelete.Load() {
		return errInjected
	}
	return store.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

// faultyRoles 让角色记录的写失败（写没有发生）。
type faultyRoles struct {
	versionstore.Store[int64, Role]
	failUpdate atomic.Bool
}

func (store *faultyRoles) Update(ctx context.Context, key int64, mutate versionstore.Mutate[Role]) (versionstore.Versioned[Role], bool, error) {
	if store.failUpdate.Load() {
		return versionstore.Versioned[Role]{}, false, errInjected
	}
	return store.Store.Update(ctx, key, mutate)
}

func faultyTransferFixture(t *testing.T) (*Service, Config, Role, Session, *faultySlots, *faultyRoles) {
	t.Helper()
	slots, roles := &faultySlots{}, &faultyRoles{}
	s, cfg, role, session := transferFixture(t, func(cfg *Config) {
		slots.Store, roles.Store = cfg.Slots, cfg.Roles
		cfg.Slots, cfg.Roles = slots, roles
	})
	return s, cfg, role, session, slots, roles
}

// Begin 写了角色、还没占位就停下：重发同一个 Begin 补上占位。
func TestBeginInterruptedBeforeTheHoldIsFinishedByResending(t *testing.T) {
	s, cfg, role, session, slots, _ := faultyTransferFixture(t)
	ctx := context.Background()
	slots.failCreate.Store(true)
	if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0)); !errors.Is(err, errInjected) {
		t.Fatalf("Begin with the hold write failing = %v, want the injected failure", err)
	}
	if stored := storedRole(t, cfg, role.PlayerID); stored.Value.Transfer == nil || stored.Value.Transfer.ID != "t-1" {
		t.Fatalf("the fixture must stop after the role write: %+v", stored.Value)
	}
	if _, found := slotAt(t, cfg, role.AccountID, awaySID); found {
		t.Fatal("the fixture must stop before the hold exists")
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)

	slots.failCreate.Store(false)
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-1" || hold.PlayerID != role.PlayerID {
		t.Fatalf("the resent Begin did not finish the hold: %+v (found %v)", hold, found)
	}
	mustCommit(t, s, role.PlayerID, "t-1")
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// 同样停在占位之前，不重发 Begin 而是直接 Commit：Commit 先把占位补上再改绑。
func TestCommitRecreatesAMissingHoldBeforeRebinding(t *testing.T) {
	s, cfg, role, session, slots, _ := faultyTransferFixture(t)
	ctx := context.Background()
	slots.failCreate.Store(true)
	if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0)); !errors.Is(err, errInjected) {
		t.Fatalf("Begin with the hold write failing = %v, want the injected failure", err)
	}
	// 占位写不进去时 Commit 不能改绑：改了就是一个绑定在新服、却没有 slot 的角色。
	if _, err := s.CommitRoleTransfer(ctx, role.PlayerID, "t-1"); !errors.Is(err, errInjected) {
		t.Fatalf("Commit with the hold write failing = %v, want the injected failure", err)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)

	slots.failCreate.Store(false)
	mustCommit(t, s, role.PlayerID, "t-1")
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
	if _, found := slotAt(t, cfg, role.AccountID, homeSID); found {
		t.Fatal("the source slot outlived the commit")
	}
}

// Commit 的角色写失败：什么也没变，重发补完。
func TestCommitInterruptedBeforeTheRoleWriteChangesNothing(t *testing.T) {
	s, cfg, role, session, _, roles := faultyTransferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	roles.failUpdate.Store(true)
	if _, err := s.CommitRoleTransfer(ctx, role.PlayerID, "t-1"); !errors.Is(err, errInjected) {
		t.Fatalf("Commit with the role write failing = %v, want the injected failure", err)
	}
	roles.failUpdate.Store(false)
	if stored := storedRole(t, cfg, role.PlayerID); stored.Value.ServerID != homeSID || stored.Value.Transfer == nil {
		t.Fatalf("a failed Commit changed the role: %+v", stored.Value)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
	mustCommit(t, s, role.PlayerID, "t-1")
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// Commit 改绑之后、收尾（清占位标记、删旧 slot）之前停下：角色已经在新服可用；重发同一个 Commit 补完收尾。
func TestCommitInterruptedAfterTheRoleWriteIsFinishedByResending(t *testing.T) {
	s, cfg, role, session, slots, _ := faultyTransferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	slots.failUpdate.Store(true)
	slots.failDelete.Store(true)
	if _, err := s.CommitRoleTransfer(ctx, role.PlayerID, "t-1"); !errors.Is(err, errInjected) {
		t.Fatalf("Commit with the slot writes failing = %v, want the injected failure", err)
	}
	stored := storedRole(t, cfg, role.PlayerID)
	if stored.Value.ServerID != awaySID || stored.Value.BindEpoch != 1 || stored.Value.LastTransfer == nil || stored.Value.LastTransfer.Settled {
		t.Fatalf("the fixture must stop after the role write with the tail open: %+v (last %+v)", stored.Value, stored.Value.LastTransfer)
	}
	if old, found := slotAt(t, cfg, role.AccountID, homeSID); !found || old.PlayerID != role.PlayerID {
		t.Fatalf("the fixture must leave the source slot behind: %+v (found %v)", old, found)
	}
	// 收尾没做完不影响登录：新服的 slot 已经属于这一代绑定。
	validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token)
	if err != nil || validated.ServerID != awaySID || validated.ServingServerID != awaySID {
		t.Fatalf("ValidateSession with the tail open = %+v, %v; want server %d", validated, err, awaySID)
	}
	// 收尾补完之前，旧服的名额仍按已占用回答，而不是把已经换走的角色当作旧服的角色交回去。
	for _, name := range []string{role.Name, "Other"} {
		if created, err := s.CreateRole(ctx, role.AccountID, homeSID, name); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("CreateRole %q on the source before the tail = %+v, %v; want ErrRoleLimit", name, created, err)
		}
	}

	slots.failUpdate.Store(false)
	slots.failDelete.Store(false)
	binding := mustCommit(t, s, role.PlayerID, "t-1")
	if binding.ServerID != awaySID || binding.BindEpoch != 1 {
		t.Fatalf("resent Commit = %+v", binding)
	}
	if old, found := slotAt(t, cfg, role.AccountID, homeSID); found {
		t.Fatalf("the resent Commit left the source slot: %+v", old)
	}
	if slot, _ := slotAt(t, cfg, role.AccountID, awaySID); slot.TransferID != "" {
		t.Fatalf("the resent Commit left the hold marker: %+v", slot)
	}
	if after := storedRole(t, cfg, role.PlayerID); !after.Value.LastTransfer.Settled || after.Value.BindEpoch != 1 {
		t.Fatalf("the resent Commit did not settle the transfer (or bumped the epoch again): %+v (last %+v)", after.Value, after.Value.LastTransfer)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// 没有人重发 Commit 时，收尾由下一次读到这条未了结记录的写入口补完：SelectRole（玩家重新登录），
// 或下一次 Begin（换回去的那一次，否则旧服的 slot 会挡住它）。
func TestAnOpenCommitTailIsFinishedByTheNextSelectRoleOrBegin(t *testing.T) {
	stopAfterRoleWrite := func(t *testing.T) (*Service, Config, Role, Session) {
		s, cfg, role, session, slots, _ := faultyTransferFixture(t)
		mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
		slots.failUpdate.Store(true)
		slots.failDelete.Store(true)
		if _, err := s.CommitRoleTransfer(context.Background(), role.PlayerID, "t-1"); !errors.Is(err, errInjected) {
			t.Fatalf("Commit with the slot writes failing = %v, want the injected failure", err)
		}
		slots.failUpdate.Store(false)
		slots.failDelete.Store(false)
		if _, found := slotAt(t, cfg, role.AccountID, homeSID); !found {
			t.Fatal("the fixture must leave the source slot behind")
		}
		return s, cfg, role, session
	}
	t.Run("SelectRole", func(t *testing.T) {
		s, cfg, role, session := stopAfterRoleWrite(t)
		if _, err := s.SelectRole(context.Background(), role.AccountID, role.PlayerID); err != nil {
			t.Fatal(err)
		}
		if old, found := slotAt(t, cfg, role.AccountID, homeSID); found {
			t.Fatalf("SelectRole left the source slot: %+v", old)
		}
		if stored := storedRole(t, cfg, role.PlayerID); !stored.Value.LastTransfer.Settled {
			t.Fatalf("SelectRole did not settle the transfer: %+v", stored.Value.LastTransfer)
		}
		mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
	})
	t.Run("Begin back to the source", func(t *testing.T) {
		s, cfg, role, session := stopAfterRoleWrite(t)
		mustBegin(t, s, role.PlayerID, transferTo("t-2", awaySID, homeSID, 1))
		hold, found := slotAt(t, cfg, role.AccountID, homeSID)
		if !found || hold.TransferID != "t-2" || hold.BindEpoch != 2 {
			t.Fatalf("the slot on the old source = %+v (found %v), want the hold of t-2", hold, found)
		}
		mustCommit(t, s, role.PlayerID, "t-2")
		mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 2)
		if old, found := slotAt(t, cfg, role.AccountID, awaySID); found {
			t.Fatalf("the slot on %d outlived the transfer back: %+v", awaySID, old)
		}
	})
}

// Abort 写了角色、还没撤占位就停下：角色已经留在源服；重发同一个 Abort 撤掉占位。
func TestAbortInterruptedAfterTheRoleWriteIsFinishedByResending(t *testing.T) {
	s, cfg, role, session, slots, _ := faultyTransferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	slots.failDelete.Store(true)
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "hook vetoed"); !errors.Is(err, errInjected) {
		t.Fatalf("Abort with the hold delete failing = %v, want the injected failure", err)
	}
	if stored := storedRole(t, cfg, role.PlayerID); stored.Value.Transfer != nil || stored.Value.LastTransfer == nil || stored.Value.LastTransfer.Committed || stored.Value.LastTransfer.Settled {
		t.Fatalf("the fixture must stop after the role write with the tail open: %+v (last %+v)", stored.Value, stored.Value.LastTransfer)
	}
	if _, found := slotAt(t, cfg, role.AccountID, awaySID); !found {
		t.Fatal("the fixture must leave the hold behind")
	}
	// 已经记为中止：这时的 Commit 不能把它做成。
	if _, err := s.CommitRoleTransfer(ctx, role.PlayerID, "t-1"); !errors.Is(err, ErrTransferStale) {
		t.Fatalf("Commit after the abort was recorded = %v, want ErrTransferStale", err)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)

	slots.failDelete.Store(false)
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "resent"); err != nil {
		t.Fatal(err)
	}
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); found {
		t.Fatalf("the resent Abort left the hold: %+v", hold)
	}
	if created, err := s.CreateRole(ctx, role.AccountID, awaySID, "Second"); err != nil || created.ServerID != awaySID {
		t.Fatalf("CreateRole on the released target = %+v, %v", created, err)
	}
}

// 中止之后占位没撤掉、也没有人重发 Abort：下一次往同一个服发起时先把它撤掉，不会被自己留下的占位挡住。
func TestAnOpenAbortTailIsFinishedByTheNextBegin(t *testing.T) {
	s, cfg, role, session, slots, _ := faultyTransferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	slots.failDelete.Store(true)
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "hook vetoed"); !errors.Is(err, errInjected) {
		t.Fatalf("Abort with the hold delete failing = %v, want the injected failure", err)
	}
	slots.failDelete.Store(false)
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-1" {
		t.Fatalf("the fixture must leave the hold of t-1 behind: %+v (found %v)", hold, found)
	}

	mustBegin(t, s, role.PlayerID, transferTo("t-2", homeSID, awaySID, 0))
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-2" {
		t.Fatalf("the target slot = %+v (found %v), want the hold of t-2", hold, found)
	}
	mustCommit(t, s, role.PlayerID, "t-2")
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// 迟到的收尾不会删掉角色后来换回来的 slot：1 → 2（t-1）之后又 2 → 1（t-2），再拿 t-1 提交之后的角色
// 快照去跑一遍收尾（一次卡了很久才继续的 Commit 重发就是这样）。slot 记着自己属于哪一代绑定，收尾只删
// 代数更旧的。
func TestALateCommitTailNeverDeletesTheSlotTheRoleCameBackTo(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	mustCommit(t, s, role.PlayerID, "t-1")
	afterFirst := storedRole(t, cfg, role.PlayerID).Value.clone()
	if afterFirst.LastTransfer == nil || afterFirst.LastTransfer.ID != "t-1" || !afterFirst.LastTransfer.Committed {
		t.Fatalf("the fixture must have committed t-1: %+v", afterFirst.LastTransfer)
	}
	afterFirst.LastTransfer.Settled = false // 那一次重发读到的是收尾之前的记录
	mustBegin(t, s, role.PlayerID, transferTo("t-2", awaySID, homeSID, 1))
	mustCommit(t, s, role.PlayerID, "t-2")
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 2)

	if _, err := s.settleTransfer(ctx, afterFirst); err != nil {
		t.Fatalf("a late tail of t-1: %v", err)
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 2)
	if last := storedRole(t, cfg, role.PlayerID).Value.LastTransfer; last == nil || last.ID != "t-2" || !last.Settled {
		t.Fatalf("the late tail of t-1 rewrote the record of t-2: %+v", last)
	}
}

// 可用的 slot 必须属于角色当前这一代绑定：PlayerID 与建角计划都对、代数不对的 slot（换服的占位、提交后
// 没删掉的旧 slot）不算，所以一个角色同一时刻至多有一个可用的 slot。
func TestASlotOfAnotherBindGenerationIsNotUsable(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	key := slotKeyFor(role.AccountID, homeSID)
	setEpoch := func(epoch uint64) {
		t.Helper()
		if _, _, err := cfg.Slots.Update(ctx, key, func(current Slot, _ bool) (Slot, bool, error) {
			current.BindEpoch = epoch
			return current, true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setEpoch(1)
	if _, err := s.ValidateSession(ctx, role.PlayerID, session.Token); !errors.Is(err, ErrConflict) {
		t.Fatalf("ValidateSession against a slot of another generation = %v, want ErrConflict", err)
	}
	if _, err := s.RoleBinding(ctx, role.PlayerID); !errors.Is(err, ErrConflict) {
		t.Fatalf("RoleBinding against a slot of another generation = %v, want ErrConflict", err)
	}
	setEpoch(0)
	mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
}

// countingNames 数名字目录被读写了几次。
type countingNames struct {
	directory.Directory
	lookups, reserves atomic.Int64
}

func (names *countingNames) Lookup(ctx context.Context, raw string) (directory.Entry, bool, error) {
	names.lookups.Add(1)
	return names.Directory.Lookup(ctx, raw)
}

func (names *countingNames) Reserve(ctx context.Context, raw string, owner directory.Owner, ttl time.Duration) (directory.Claim, error) {
	names.reserves.Add(1)
	return names.Directory.Reserve(ctx, raw, owner, ttl)
}

// 换服不改名，名字目录不分服（方案 §8 第一条）。换过服的角色，名字在目录里的 owner 仍是建角时的
// “旧 slot 键/计划 ID”，新服的 slot 键算出来的 owner 与它对不上——这不影响任何路径：建角判定表对已发布的
// slot 不读名字（同名返回角色、别的名字 role_limit、运维 unresolvable），名字仍然全局唯一。account 没有
// 改名接口（UpdateProfile 不改名），所以不存在“换服后改名时按新 owner 释放旧名字”的路径。
func TestATransferredRoleKeepsItsNameUnderTheCreationOwner(t *testing.T) {
	names := &countingNames{}
	s, cfg, role, _ := transferFixture(t, func(cfg *Config) {
		names.Directory = cfg.Names
		cfg.Names = names
	})
	ctx := context.Background()
	source, _ := slotAt(t, cfg, role.AccountID, homeSID)
	creationOwnerBefore := creationOwner(slotKeyFor(role.AccountID, homeSID), source.Creation)
	mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
	mustCommit(t, s, role.PlayerID, "t-1")

	entry, held, err := cfg.Names.Lookup(ctx, role.Name)
	if err != nil || !held || entry.State != directory.StateCommitted || entry.Owner != creationOwnerBefore {
		t.Fatalf("the name after the transfer = %+v (held %v, err %v), want it committed to the creation owner %q", entry, held, err, creationOwnerBefore)
	}
	moved, _ := slotAt(t, cfg, role.AccountID, awaySID)
	if derived := creationOwner(slotKeyFor(role.AccountID, awaySID), moved.Creation); derived == entry.Owner {
		t.Fatalf("the moved slot derives the same owner %q; the premise of this test is gone", derived)
	}
	if moved.Creation != source.Creation {
		t.Fatalf("the moved slot's creation plan = %+v, want the plan copied from the source %+v", moved.Creation, source.Creation)
	}

	// 已发布的 slot：两种建角请求都不读、不占名字。
	names.lookups.Store(0)
	names.reserves.Store(0)
	same, err := s.CreateRole(ctx, role.AccountID, awaySID, role.Name)
	if err != nil || same.PlayerID != role.PlayerID || same.ServerID != awaySID {
		t.Fatalf("same-name CreateRole on the new server = %+v, %v; want the moved role", same, err)
	}
	if _, err := s.CreateRole(ctx, role.AccountID, awaySID, "Other"); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("another name on the new server = %v, want ErrRoleLimit", err)
	}
	if _, err := s.ResolvePendingCreation(ctx, role.AccountID, awaySID, "published"); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("ResolvePendingCreation on the moved slot = %v, want ErrNotResolvable", err)
	}
	if names.lookups.Load() != 0 || names.reserves.Load() != 0 {
		t.Fatalf("a published slot read the name directory: %d lookups, %d reserves", names.lookups.Load(), names.reserves.Load())
	}

	// 名字仍然全局唯一：别的账号拿不到；本账号在旧服用同名建新角色也拿不到，并且不会把旧服的名额卡住。
	other := login(t, s, "someone-else")
	if _, err := s.CreateRole(ctx, other.ID, homeSID, role.Name); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("another account taking the moved role's name = %v, want ErrNameTaken", err)
	}
	if _, err := s.CreateRole(ctx, role.AccountID, homeSID, role.Name); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("the same account re-creating the name on the old server = %v, want ErrNameTaken", err)
	}
	fresh, err := s.CreateRole(ctx, role.AccountID, homeSID, "Fresh")
	if err != nil || fresh.PlayerID == role.PlayerID {
		t.Fatalf("a new name on the old server after the refusal = %+v, %v", fresh, err)
	}
	if after, held, err := cfg.Names.Lookup(ctx, role.Name); err != nil || !held || after.Owner != creationOwnerBefore {
		t.Fatalf("the refusals disturbed the moved role's name: %+v (held %v, err %v)", after, held, err)
	}
}

// 与别名合服的交叉：一期拒绝涉及“已并入另一服”的服——角色的绑定服已并入别处（它由承载服的进程服务，
// 原服身份还挂在别名上），或目标服已并入别处。承载着别的服（别人并入它）的服不算，照常可作源或目标。
func TestTransfersInvolvingAMergedAwayServerAreRefused(t *testing.T) {
	ctx := context.Background()
	t.Run("source merged away", func(t *testing.T) {
		s, cfg, role, session := transferFixture(t)
		mustMerge(t, s, homeSID, awaySID) // 角色的绑定服 1 并入 2，由 2 承载
		if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, transferTo("t", awaySID, thirdSID, 0)); !errors.Is(err, ErrServerClosed) {
			t.Fatalf("Begin for a role of a merged-away server = %v, want ErrServerClosed", err)
		}
		// 发起方必须是承载服：原服的 sid 已经没有进程了。
		if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, transferTo("t", homeSID, thirdSID, 0)); !errors.Is(err, ErrTransferStale) {
			t.Fatalf("Begin from the merged-away sid = %v, want ErrTransferStale", err)
		}
		if stored := storedRole(t, cfg, role.PlayerID); stored.Value.Transfer != nil {
			t.Fatalf("a refused Begin changed the role: %+v", stored.Value)
		}
		if validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token); err != nil || validated.ServerID != homeSID || validated.ServingServerID != awaySID {
			t.Fatalf("the merged role after the refusals = %+v, %v", validated, err)
		}
	})
	t.Run("target merged away while the transfer is in flight", func(t *testing.T) {
		s, cfg, role, session := transferFixture(t)
		mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, thirdSID, 0))
		mustMerge(t, s, thirdSID, awaySID)
		binding, err := s.CommitRoleTransfer(ctx, role.PlayerID, "t-1")
		if !errors.Is(err, ErrServerClosed) || binding.ServerID != homeSID || binding.Transfer == nil {
			t.Fatalf("Commit onto a server merged away meanwhile = %+v, %v; want ErrServerClosed with the transfer still in flight", binding, err)
		}
		mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
		if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "target was merged"); err != nil {
			t.Fatal(err)
		}
		if _, found := slotAt(t, cfg, role.AccountID, thirdSID); found {
			t.Fatal("the hold on the merged server outlived the abort")
		}
		mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
	})
	t.Run("source merged away while the transfer is in flight", func(t *testing.T) {
		// 角色正要离开的服被并走：提交照常，角色落在目标服，与合服再无关系。
		s, cfg, role, session := transferFixture(t)
		mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, thirdSID, 0))
		mustMerge(t, s, homeSID, awaySID)
		mustCommit(t, s, role.PlayerID, "t-1")
		mustBeBoundTo(t, s, cfg, role, session.Token, thirdSID, 1)
	})
	t.Run("a server that hosts merged servers is an ordinary source and target", func(t *testing.T) {
		s, cfg, role, session := transferFixture(t)
		mustMerge(t, s, thirdSID, awaySID) // 2 承载着 3
		mustBegin(t, s, role.PlayerID, transferTo("t-1", homeSID, awaySID, 0))
		mustCommit(t, s, role.PlayerID, "t-1")
		mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
		upsertOpen(t, s, 4)
		mustBegin(t, s, role.PlayerID, transferTo("t-2", awaySID, 4, 1))
		mustCommit(t, s, role.PlayerID, "t-2")
		mustBeBoundTo(t, s, cfg, role, session.Token, 4, 2)
	})
}

// 人工提交（accountctl）必须留下操作者的备注；备注写进 LastTransfer。
func TestCommitRoleTransferWithNoteRecordsTheOperatorsNote(t *testing.T) {
	s, _, role, _ := transferFixture(t)
	ctx := context.Background()
	mustBegin(t, s, role.PlayerID, TransferRequest{ID: "t-1", FromSID: homeSID, To: awaySID, Note: "began by the game"})
	if _, err := s.CommitRoleTransferWithNote(ctx, role.PlayerID, "t-1", "  "); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("a manual commit without a note = %v, want ErrAdminNoteRequired", err)
	}
	if binding, err := s.RoleBinding(ctx, role.PlayerID); err != nil || binding.ServerID != homeSID || binding.Transfer == nil {
		t.Fatalf("a refused manual commit changed the binding: %+v, %v", binding, err)
	}
	binding, err := s.CommitRoleTransferWithNote(ctx, role.PlayerID, "t-1", "source 1 exited; wal-drained ok")
	if err != nil || binding.ServerID != awaySID || binding.LastTransfer == nil || binding.LastTransfer.Note != "source 1 exited; wal-drained ok" {
		t.Fatalf("manual commit = %+v, %v; want the operator's note on the record", binding, err)
	}
}

// 认证器写、登录端点读的换服标记：值里带目标 sid 与换服 ID，ID 可以含冒号。
func TestTransferClaimValueRoundTrips(t *testing.T) {
	for _, transfer := range []RoleTransfer{{ID: "t-1", To: 1001}, {ID: "rehome:42:abc", To: 7}} {
		to, id, ok := ParseTransferClaim(TransferClaimValue(transfer))
		if !ok || to != transfer.To || id != transfer.ID {
			t.Fatalf("round trip of %+v = (%d, %q, %v)", transfer, to, id, ok)
		}
	}
	for _, malformed := range []string{"", "t-1", "abc:t-1", "0:t-1", "-3:t-1", "1001:"} {
		if to, id, ok := ParseTransferClaim(malformed); ok {
			t.Fatalf("ParseTransferClaim(%q) = (%d, %q, true), want a refusal", malformed, to, id)
		}
	}
}

// 并发：同一个角色的多次 Begin 只有一个成功；Begin 与目标服上的建角抢同一个名额只有一个成功；同一次换服的
// Commit 与 Abort 只有一个生效，角色始终可用。
func TestConcurrentTransferRequestsHaveOneWinner(t *testing.T) {
	ctx := context.Background()
	t.Run("begins", func(t *testing.T) {
		s, cfg, role, session := transferFixture(t)
		const racers = 6
		var wg sync.WaitGroup
		var won atomic.Int64
		errs := make([]error, racers)
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				target := awaySID
				if i%2 == 1 {
					target = thirdSID
				}
				_, errs[i] = s.BeginRoleTransfer(ctx, role.PlayerID, transferTo(fmt.Sprintf("t-%d", i), homeSID, target, 0))
				if errs[i] == nil {
					won.Add(1)
				}
			}()
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("%d of %d racing Begins succeeded, want 1: %v", won.Load(), racers, errs)
		}
		for i, err := range errs {
			if err != nil && !errors.Is(err, ErrTransferInProgress) {
				t.Fatalf("racer %d lost with %v, want ErrTransferInProgress", i, err)
			}
		}
		transfer := storedRole(t, cfg, role.PlayerID).Value.Transfer
		if transfer == nil {
			t.Fatal("the winning Begin recorded no transfer")
		}
		holds := 0
		for _, sid := range []int32{awaySID, thirdSID} {
			if hold, found := slotAt(t, cfg, role.AccountID, sid); found {
				holds++
				if hold.TransferID != transfer.ID || sid != transfer.To {
					t.Fatalf("a losing Begin left a hold on %d: %+v (winner %+v)", sid, hold, transfer)
				}
			}
		}
		if holds != 1 {
			t.Fatalf("%d holds after the race, want exactly the winner's", holds)
		}
		mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
	})
	t.Run("begin against a create on the target", func(t *testing.T) {
		s, _, cfg := pendingContractService(t)
		upsertOpen(t, s, awaySID)
		for round := range 12 {
			role, session := roleOn(t, s, fmt.Sprintf("racer-%d", round), homeSID, fmt.Sprintf("Racer%d", round))
			var wg sync.WaitGroup
			var beginErr, createErr error
			var created Role
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, beginErr = s.BeginRoleTransfer(ctx, role.PlayerID, transferTo("t", homeSID, awaySID, 0))
			}()
			go func() {
				defer wg.Done()
				created, createErr = s.CreateRole(ctx, role.AccountID, awaySID, fmt.Sprintf("Alt%d", round))
			}()
			wg.Wait()
			stored := storedRole(t, cfg, role.PlayerID).Value
			slot, found := slotAt(t, cfg, role.AccountID, awaySID)
			switch {
			case beginErr == nil && errors.Is(createErr, ErrRoleLimit):
				if stored.Transfer == nil || !found || slot.TransferID != "t" {
					t.Fatalf("round %d: Begin won but role %+v / slot %+v (found %v)", round, stored, slot, found)
				}
			case createErr == nil && errors.Is(beginErr, ErrRoleLimit):
				if stored.Transfer != nil || !found || slot.PlayerID != created.PlayerID || slot.TransferID != "" {
					t.Fatalf("round %d: CreateRole won but role %+v / slot %+v (found %v)", round, stored, slot, found)
				}
			default:
				t.Fatalf("round %d: begin=%v create=%v; want exactly one winner and ErrRoleLimit for the other", round, beginErr, createErr)
			}
			mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
		}
	})
	t.Run("commit against abort", func(t *testing.T) {
		s, _, cfg := pendingContractService(t)
		upsertOpen(t, s, awaySID)
		for round := range 12 {
			role, session := roleOn(t, s, fmt.Sprintf("settler-%d", round), homeSID, fmt.Sprintf("Settler%d", round))
			mustBegin(t, s, role.PlayerID, transferTo("t", homeSID, awaySID, 0))
			var wg sync.WaitGroup
			var commitErr, abortErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, commitErr = s.CommitRoleTransfer(ctx, role.PlayerID, "t")
			}()
			go func() {
				defer wg.Done()
				_, abortErr = s.AbortRoleTransfer(ctx, role.PlayerID, "t", "race")
			}()
			wg.Wait()
			switch {
			case commitErr == nil && errors.Is(abortErr, ErrTransferStale):
				mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
				if _, found := slotAt(t, cfg, role.AccountID, homeSID); found {
					t.Fatalf("round %d: Commit won but the source slot is still there", round)
				}
			case abortErr == nil && errors.Is(commitErr, ErrTransferStale):
				mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
				if _, found := slotAt(t, cfg, role.AccountID, awaySID); found {
					t.Fatalf("round %d: Abort won but the hold is still there", round)
				}
			default:
				t.Fatalf("round %d: commit=%v abort=%v; want exactly one to take effect and ErrTransferStale for the other", round, commitErr, abortErr)
			}
		}
	})
}
