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
		{name: "stale transfer seq", request: TransferRequest{ID: "t", FromSID: homeSID, To: awaySID, ExpectSeq: 3}, want: ErrTransferStale, carriesBinding: true},
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

	// 让出的目标名额可以再占：换一个 ID、按当前绑定重新发起并提交。
	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
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

	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
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
		mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", homeSID))
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

	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
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
	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", homeSID))
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
		mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", 4))
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

// nextTransfer 按角色此刻的绑定组一个发起请求：发起方是承载服，带上读到的绑定代数与换服尝试的序号。
func nextTransfer(t *testing.T, s *Service, playerID int64, id string, to int32) TransferRequest {
	t.Helper()
	binding, err := s.RoleBinding(context.Background(), playerID)
	if err != nil {
		t.Fatalf("binding of %d: %v", playerID, err)
	}
	return TransferRequest{ID: id, FromSID: binding.ServingServerID, To: to, ExpectEpoch: binding.BindEpoch, ExpectSeq: binding.TransferSeq}
}

// gatedSlots 把第一次占位写入（带 TransferID 的 Create）停在选定的位置，由测试放行：交错用 channel
// 固定，不靠 sleep。其余的写照常，faultySlots 的失败开关也照常可用（failCreate 打开时的占位写入直接
// 失败，不算“第一次”）。
type gatedSlots struct {
	*faultySlots
	// afterWrite 为 false 时停在写入之前，为 true 时停在写入之后、返回之前。
	afterWrite bool
	// loseReply 让放行之后的这次 Create 答 errInjected：写入已经生效而调用方不知道，等同于进程在占位
	// 刚写完时退出。
	loseReply bool
	reached   chan struct{}
	resume    chan struct{}
	once      sync.Once
}

func (store *gatedSlots) Create(ctx context.Context, key string, value Slot) (versionstore.Versioned[Slot], bool, error) {
	gated := false
	if value.TransferID != "" && !store.failCreate.Load() {
		store.once.Do(func() { gated = true })
	}
	if !gated {
		return store.faultySlots.Create(ctx, key, value)
	}
	if !store.afterWrite {
		close(store.reached)
		<-store.resume
	}
	stored, created, err := store.faultySlots.Create(ctx, key, value)
	if store.afterWrite {
		close(store.reached)
		<-store.resume
	}
	if store.loseReply && err == nil {
		return versionstore.Versioned[Slot]{}, false, errInjected
	}
	return stored, created, err
}

func gatedTransferFixture(t *testing.T, afterWrite, loseReply bool) (*Service, Config, Role, Session, *gatedSlots) {
	t.Helper()
	slots := &gatedSlots{
		faultySlots: &faultySlots{}, afterWrite: afterWrite, loseReply: loseReply,
		reached: make(chan struct{}), resume: make(chan struct{}),
	}
	s, cfg, role, session := transferFixture(t, func(cfg *Config) {
		slots.Store = cfg.Slots
		cfg.Slots = slots
	})
	return s, cfg, role, session, slots
}

// stalledAtHold 在另一个 goroutine 里执行 call 并等它停在占位写入的停点上；返回的函数放行它并给出结果。
func stalledAtHold(t *testing.T, slots *gatedSlots, what string, call func() (RoleBinding, error)) func() (RoleBinding, error) {
	t.Helper()
	type outcome struct {
		binding RoleBinding
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		binding, err := call()
		done <- outcome{binding, err}
	}()
	select {
	case <-slots.reached:
	case early := <-done:
		t.Fatalf("%s returned before it reached the hold write: %+v, %v", what, early.binding, early.err)
	}
	return func() (RoleBinding, error) {
		close(slots.resume)
		result := <-done
		return result.binding, result.err
	}
}

// stalledBegin 发起一次换服并等它停在占位写入的停点上。
func stalledBegin(t *testing.T, s *Service, slots *gatedSlots, playerID int64, request TransferRequest) func() (RoleBinding, error) {
	t.Helper()
	return stalledAtHold(t, slots, "Begin "+request.ID, func() (RoleBinding, error) {
		return s.BeginRoleTransfer(context.Background(), playerID, request)
	})
}

// 占位这一步与角色记录不是一次原子写：Begin 写了角色、还没占位（或刚占完位）时，这次换服被中止了。
// 迟到的 Begin 不能留下占位，也不能答“换服在途”：它答 ErrTransferStale 并带回角色现在的绑定。之后重发的
// Abort、SelectRole、下一次 Begin 都照常，目标服的名额是空的。
func TestABeginOvertakenByAnAbortLeavesNoHold(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		afterWrite bool
	}{{"stalled before the hold write", false}, {"stalled after the hold write", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg, role, session, slots := gatedTransferFixture(t, tc.afterWrite, false)
			finish := stalledBegin(t, s, slots, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-1", awaySID))
			if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "drain timed out"); err != nil {
				t.Fatalf("Abort while Begin is stalled: %v", err)
			}
			late, err := finish()
			if !errors.Is(err, ErrTransferStale) || late.ServerID != homeSID || late.Transfer != nil || late.LastTransfer == nil || late.LastTransfer.ID != "t-1" || late.LastTransfer.Committed {
				t.Fatalf("the overtaken Begin = %+v (last %+v), %v; want ErrTransferStale carrying t-1 as aborted", late, late.LastTransfer, err)
			}
			if hold, found := slotAt(t, cfg, role.AccountID, awaySID); found {
				t.Fatalf("the overtaken Begin left a hold on the target: %+v", hold)
			}
			if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "resent"); err != nil {
				t.Fatalf("resent Abort: %v", err)
			}
			mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
			mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
			mustCommit(t, s, role.PlayerID, "t-2")
			mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
		})
	}
}

// 占位刚写完进程就退出（Create 已经生效、应答丢了），而这次换服在占位写入之前已经被中止并收了尾：
// 留下的占位没有任何在途的换服认它。它不会永久占住目标服的名额——重发的 Abort、这个角色的 SelectRole、
// 下一次往这个服的 Begin、账号在这个服的建角，谁先到谁把它撤掉；中止的记录已经被后来的了结覆盖时，
// 后两个入口仍然能撤。撤的时候存储失败，下一个入口再撤。
func TestAHoldWrittenAfterItsAbortIsReleasedByTheNextEntry(t *testing.T) {
	ctx := context.Background()
	// forgotten：占位写下之前，中止的记录已经被另一次了结换掉（这里是中止一个没发起过的 ID），
	// 角色记录不再指向那个目标服。
	strand := func(t *testing.T, forgotten bool) (*Service, Config, Role, Session, *gatedSlots) {
		t.Helper()
		s, cfg, role, session, slots := gatedTransferFixture(t, false, true)
		finish := stalledBegin(t, s, slots, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-1", awaySID))
		if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "begin timed out"); err != nil {
			t.Fatalf("Abort while Begin is stalled: %v", err)
		}
		lastID := "t-1"
		if forgotten {
			lastID = "never-began"
			if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, lastID, "another caller"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := finish(); !errors.Is(err, errInjected) {
			t.Fatalf("the Begin whose hold reply was lost = %v, want the injected failure", err)
		}
		hold, found := slotAt(t, cfg, role.AccountID, awaySID)
		last := storedRole(t, cfg, role.PlayerID).Value.LastTransfer
		if !found || hold.TransferID != "t-1" || last == nil || last.ID != lastID || last.Committed || !last.Settled {
			t.Fatalf("the fixture must strand the hold of the aborted t-1 with %s settled: slot %+v (found %v), last %+v", lastID, hold, found, last)
		}
		return s, cfg, role, session, slots
	}
	stranded := func(t *testing.T) (*Service, Config, Role, Session, *gatedSlots) {
		t.Helper()
		return strand(t, false)
	}
	released := func(t *testing.T, s *Service, cfg Config, role Role, session Session) {
		t.Helper()
		if hold, found := slotAt(t, cfg, role.AccountID, awaySID); found {
			t.Fatalf("the stranded hold is still on the target: %+v", hold)
		}
		mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
	}

	t.Run("resent Abort", func(t *testing.T) {
		s, cfg, role, session, _ := stranded(t)
		if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "resent"); err != nil {
			t.Fatal(err)
		}
		released(t, s, cfg, role, session)
	})
	t.Run("SelectRole", func(t *testing.T) {
		s, cfg, role, session, _ := stranded(t)
		if _, err := s.SelectRole(ctx, role.AccountID, role.PlayerID); err != nil {
			t.Fatal(err)
		}
		released(t, s, cfg, role, session)
	})
	t.Run("next Begin to the same target", func(t *testing.T) {
		s, cfg, role, session, _ := stranded(t)
		mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
		if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-2" {
			t.Fatalf("the target slot = %+v (found %v), want the hold of t-2", hold, found)
		}
		mustCommit(t, s, role.PlayerID, "t-2")
		mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
	})
	t.Run("CreateRole on the target", func(t *testing.T) {
		s, cfg, role, session, _ := stranded(t)
		created, err := s.CreateRole(ctx, role.AccountID, awaySID, "Second")
		if err != nil || created.ServerID != awaySID || created.PlayerID == role.PlayerID {
			t.Fatalf("CreateRole on the target = %+v, %v; want a new role there", created, err)
		}
		mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
	})
	t.Run("after the record of the abort was overwritten", func(t *testing.T) {
		for _, entry := range []string{"Begin", "CreateRole"} {
			s, cfg, role, session, _ := strand(t, true)
			if entry == "Begin" {
				mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
				mustCommit(t, s, role.PlayerID, "t-2")
				mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
				continue
			}
			if created, err := s.CreateRole(ctx, role.AccountID, awaySID, "Second"); err != nil || created.ServerID != awaySID {
				t.Fatalf("CreateRole on the target = %+v, %v; want a new role there", created, err)
			}
			mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
		}
	})
	t.Run("the release fails and the next entry retries", func(t *testing.T) {
		s, cfg, role, session, slots := stranded(t)
		slots.failDelete.Store(true)
		if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "resent"); !errors.Is(err, errInjected) {
			t.Fatalf("resent Abort with the slot delete failing = %v, want the injected failure", err)
		}
		// 登录不被挡住：撤不掉只是目标服的名额还占着。
		if _, err := s.SelectRole(ctx, role.AccountID, role.PlayerID); err != nil {
			t.Fatalf("SelectRole with the slot delete failing: %v", err)
		}
		if _, found := slotAt(t, cfg, role.AccountID, awaySID); !found {
			t.Fatal("the fixture must keep the stranded hold while deletes fail")
		}
		slots.failDelete.Store(false)
		if _, err := s.SelectRole(ctx, role.AccountID, role.PlayerID); err != nil {
			t.Fatal(err)
		}
		released(t, s, cfg, role, session)
	})
}

// Begin 停在占位这一步时这次换服被提交了：迟到的 Begin 答 ErrTransferStale 并带回已提交的绑定，
// 不碰角色在新服的 slot——哪怕提交的收尾还没做完、那个 slot 还带着这次换服的标记。
func TestABeginOvertakenByACommitKeepsTheCommittedSlot(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		afterWrite bool
		// openTail：提交的收尾（清标记、删旧 slot）失败，新服的 slot 仍带着 TransferID。
		openTail bool
	}{
		{name: "stalled before the hold write", afterWrite: false},
		{name: "stalled after the hold write", afterWrite: true},
		{name: "stalled after the hold write, commit tail open", afterWrite: true, openTail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg, role, session, slots := gatedTransferFixture(t, tc.afterWrite, false)
			finish := stalledBegin(t, s, slots, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-1", awaySID))
			if tc.openTail {
				slots.failUpdate.Store(true)
				slots.failDelete.Store(true)
				if _, err := s.CommitRoleTransfer(ctx, role.PlayerID, "t-1"); !errors.Is(err, errInjected) {
					t.Fatalf("Commit with the slot writes failing = %v, want the injected failure", err)
				}
				slots.failUpdate.Store(false)
				slots.failDelete.Store(false)
				if marked, found := slotAt(t, cfg, role.AccountID, awaySID); !found || marked.TransferID != "t-1" {
					t.Fatalf("the fixture must leave the committed slot marked: %+v (found %v)", marked, found)
				}
			} else {
				mustCommit(t, s, role.PlayerID, "t-1")
			}
			late, err := finish()
			if !errors.Is(err, ErrTransferStale) || late.ServerID != awaySID || late.BindEpoch != 1 || late.Transfer != nil ||
				late.LastTransfer == nil || late.LastTransfer.ID != "t-1" || !late.LastTransfer.Committed {
				t.Fatalf("the overtaken Begin = %+v (last %+v), %v; want ErrTransferStale carrying the committed binding", late, late.LastTransfer, err)
			}
			if slot, found := slotAt(t, cfg, role.AccountID, awaySID); !found || slot.PlayerID != role.PlayerID || slot.BindEpoch != 1 {
				t.Fatalf("the overtaken Begin disturbed the committed slot: %+v (found %v)", slot, found)
			}
			if validated, err := s.ValidateSession(ctx, role.PlayerID, session.Token); err != nil || validated.ServerID != awaySID {
				t.Fatalf("ValidateSession after the overtaken Begin = %+v, %v; want server %d", validated, err, awaySID)
			}
			mustCommit(t, s, role.PlayerID, "t-1") // 重发补完收尾（没有未完的收尾时是回放）
			mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
			if stored := storedRole(t, cfg, role.PlayerID).Value; stored.Transfer != nil || !stored.LastTransfer.Settled {
				t.Fatalf("role after the resent Commit = %+v (last %+v)", stored, stored.LastTransfer)
			}
		})
	}
}

// Commit 也会写占位（Begin 停在占位之前时由它补上）。它停在这一步时这次换服被中止了：迟到的 Commit 答
// ErrTransferStale，把自己刚补的占位撤掉；占位写完进程就退出的，下一个入口撤。
func TestACommitOvertakenByAnAbortReleasesTheHoldItRecreated(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                  string
		afterWrite, loseReply bool
	}{
		{name: "stalled before the hold write"},
		{name: "stalled after the hold write", afterWrite: true},
		{name: "the hold reply is lost", loseReply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg, role, session, slots := gatedTransferFixture(t, tc.afterWrite, tc.loseReply)
			slots.failCreate.Store(true)
			if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-1", awaySID)); !errors.Is(err, errInjected) {
				t.Fatalf("Begin with the hold write failing = %v, want the injected failure", err)
			}
			slots.failCreate.Store(false)
			finish := stalledAtHold(t, slots, "Commit t-1", func() (RoleBinding, error) {
				return s.CommitRoleTransfer(ctx, role.PlayerID, "t-1")
			})
			if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "drain timed out"); err != nil {
				t.Fatalf("Abort while Commit is stalled: %v", err)
			}
			late, err := finish()
			if tc.loseReply {
				if !errors.Is(err, errInjected) {
					t.Fatalf("the Commit whose hold reply was lost = %v, want the injected failure", err)
				}
				if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-1" {
					t.Fatalf("the fixture must strand the hold of t-1: %+v (found %v)", hold, found)
				}
				if _, err := s.SelectRole(ctx, role.AccountID, role.PlayerID); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrTransferStale) || late.ServerID != homeSID || late.Transfer != nil {
				t.Fatalf("the overtaken Commit = %+v, %v; want ErrTransferStale with the role on %d", late, err, homeSID)
			}
			if hold, found := slotAt(t, cfg, role.AccountID, awaySID); found {
				t.Fatalf("the overtaken Commit left a hold on the target: %+v", hold)
			}
			mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
			mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
			mustCommit(t, s, role.PlayerID, "t-2")
			mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
		})
	}
}

// Begin 停在占位之前，这次换服被中止，另一次换服随后占了同一个目标服：迟到的 Begin 被拒绝并带回在途的
// 那一次，新一次的占位原样留着、照常提交。
func TestABeginOvertakenByAnotherTransferKeepsItsHold(t *testing.T) {
	s, cfg, role, session, slots := gatedTransferFixture(t, false, false)
	ctx := context.Background()
	finish := stalledBegin(t, s, slots, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-1", awaySID))
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "drain timed out"); err != nil {
		t.Fatal(err)
	}
	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", awaySID))
	late, err := finish()
	if !errors.Is(err, ErrTransferInProgress) || late.Transfer == nil || late.Transfer.ID != "t-2" {
		t.Fatalf("the overtaken Begin = %+v, %v; want ErrTransferInProgress carrying t-2", late, err)
	}
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-2" {
		t.Fatalf("the overtaken Begin disturbed the hold of t-2: %+v (found %v)", hold, found)
	}
	if stored := storedRole(t, cfg, role.PlayerID).Value; stored.Transfer == nil || stored.Transfer.ID != "t-2" {
		t.Fatalf("the overtaken Begin disturbed the transfer in flight: %+v", stored.Transfer)
	}
	mustCommit(t, s, role.PlayerID, "t-2")
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// 中止过的换服不会因为后来又有别的中止而被忘掉：原样重放最早那个 Begin（它带的绑定代数一直没变），
// 无论中间隔了几次发起与中止、中止的是不是发起成功的 ID，都答 ErrTransferStale，角色上没有换服、
// 目标服没有占位；重放它的 Commit 同样被拒。按当前绑定组出来的新请求照常受理，提交与提交的重发照常。
func TestAnAbortedTransferCannotBeRevivedAfterLaterAborts(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	replayed := []TransferRequest{nextTransfer(t, s, role.PlayerID, "old", awaySID)}
	mustBegin(t, s, role.PlayerID, replayed[0])
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "old", "cancel"); err != nil {
		t.Fatal(err)
	}
	for round := range 3 {
		if round == 1 {
			// 中止一个没发起过的 ID 同样会换掉 LastTransfer。
			if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "never-began", "begin timed out"); err != nil {
				t.Fatal(err)
			}
		} else {
			request := nextTransfer(t, s, role.PlayerID, fmt.Sprintf("new-%d", round), awaySID)
			mustBegin(t, s, role.PlayerID, request)
			if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, request.ID, "cancel"); err != nil {
				t.Fatal(err)
			}
			replayed = append(replayed, request)
		}
		for _, request := range replayed {
			late, err := s.BeginRoleTransfer(ctx, role.PlayerID, request)
			if !errors.Is(err, ErrTransferStale) || late.ServerID != homeSID || late.Transfer != nil {
				t.Fatalf("round %d: replayed Begin %s = %+v, %v; want ErrTransferStale with nothing in flight", round, request.ID, late, err)
			}
			if stored := storedRole(t, cfg, role.PlayerID).Value; stored.Transfer != nil {
				t.Fatalf("round %d: replayed Begin %s revived a transfer: %+v", round, request.ID, stored.Transfer)
			}
			if hold, found := slotAt(t, cfg, role.AccountID, awaySID); found {
				t.Fatalf("round %d: replayed Begin %s left a hold: %+v", round, request.ID, hold)
			}
			if refused, err := s.CommitRoleTransfer(ctx, role.PlayerID, request.ID); !errors.Is(err, ErrTransferStale) || refused.ServerID != homeSID {
				t.Fatalf("round %d: replayed Commit %s = %+v, %v; want ErrTransferStale on %d", round, request.ID, refused, err, homeSID)
			}
		}
		mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
	}

	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "current", awaySID))
	mustCommit(t, s, role.PlayerID, "current")
	if again := mustCommit(t, s, role.PlayerID, "current"); again.ServerID != awaySID || again.BindEpoch != 1 {
		t.Fatalf("resent Commit of the current transfer = %+v", again)
	}
	for _, request := range replayed {
		if late, err := s.BeginRoleTransfer(ctx, role.PlayerID, request); !errors.Is(err, ErrTransferStale) || late.ServerID != awaySID {
			t.Fatalf("replayed Begin %s after the commit = %+v, %v; want ErrTransferStale on %d", request.ID, late, err, awaySID)
		}
	}
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// “Begin 结果未知时中止这个 ID 拿确定结论”跨后来的中止仍然成立：中止一个没发起过的 ID 之后，哪怕又有
// 别的 ID 被中止（同样没发起过的，或发起之后中止的），那个迟到的 Begin 也做不起来。两个没发起过的 ID
// 接连被中止时中间没有任何一次受理的发起：挡住迟到 Begin 的只有“中止也推进 TransferSeq”。
func TestAbortOfAnUnknownTransferFencesALateBeginAcrossLaterAborts(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	// 两个发起方各自读了绑定、发出了 Begin，结果都未知，于是各自中止。
	lost := []TransferRequest{nextTransfer(t, s, role.PlayerID, "lost-1", awaySID), nextTransfer(t, s, role.PlayerID, "lost-2", thirdSID)}
	refuseLate := func(step string) {
		t.Helper()
		for _, request := range lost {
			if late, err := s.BeginRoleTransfer(ctx, role.PlayerID, request); !errors.Is(err, ErrTransferStale) || late.Transfer != nil {
				t.Fatalf("after %s: the late Begin %s = %+v, %v; want ErrTransferStale", step, request.ID, late, err)
			}
			if hold, found := slotAt(t, cfg, role.AccountID, request.To); found {
				t.Fatalf("after %s: the late Begin %s left a hold: %+v", step, request.ID, hold)
			}
		}
		mustBeBoundTo(t, s, cfg, role, session.Token, homeSID, 0)
	}
	for _, request := range lost {
		if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, request.ID, "the outcome of begin is unknown"); err != nil {
			t.Fatal(err)
		}
	}
	refuseLate("two aborts of ids that never began")

	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "retry", thirdSID))
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "retry", "leave hook refused"); err != nil {
		t.Fatal(err)
	}
	refuseLate("a later transfer was begun and aborted")
}

// TransferSeq 是换服尝试的序号：受理一次发起加 1，记下一次中止（含中止没发起过的 ID）加 1；提交、
// 被拒绝的请求、在途 ID 的重发、已中止 ID 的重发都不改它。发起必须带当前值，带旧值的被拒绝并带回当前值；
// 在途 ID 的重发不看它。它与 BindEpoch 互不影响：中止不动 BindEpoch，提交不动 TransferSeq。
func TestTransferSeqAdvancesOnEveryAcceptedBeginAndRecordedAbort(t *testing.T) {
	s, cfg, role, session := transferFixture(t)
	ctx := context.Background()
	expect := func(step string, seq, epoch uint64) {
		t.Helper()
		binding, err := s.RoleBinding(ctx, role.PlayerID)
		if err != nil || binding.TransferSeq != seq || binding.BindEpoch != epoch {
			t.Fatalf("after %s: transfer seq %d, bind epoch %d (err %v); want %d and %d", step, binding.TransferSeq, binding.BindEpoch, err, seq, epoch)
		}
		if stored := storedRole(t, cfg, role.PlayerID).Value; stored.TransferSeq != seq {
			t.Fatalf("after %s: stored transfer seq %d, want %d", step, stored.TransferSeq, seq)
		}
	}
	expect("creation", 0, 0)

	first := nextTransfer(t, s, role.PlayerID, "t-1", awaySID)
	if begun := mustBegin(t, s, role.PlayerID, first); begun.TransferSeq != 1 {
		t.Fatalf("Begin answered transfer seq %d, want 1", begun.TransferSeq)
	}
	expect("an accepted Begin", 1, 0)
	// 在途 ID 的重发：原请求（带的是受理之前的序号）与按新绑定组的请求都是回放，不再加。
	mustBegin(t, s, role.PlayerID, first)
	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-1", awaySID))
	expect("resent Begins", 1, 0)
	// 被拒绝的请求不加：另一个 ID（在途）、中止别的 ID（在途的不是它）。
	if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-other", thirdSID)); !errors.Is(err, ErrTransferInProgress) {
		t.Fatalf("Begin of another id = %v, want ErrTransferInProgress", err)
	}
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-other", "not mine"); !errors.Is(err, ErrTransferInProgress) {
		t.Fatalf("Abort of another id = %v, want ErrTransferInProgress", err)
	}
	expect("refused requests", 1, 0)

	if aborted, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "cancel"); err != nil || aborted.TransferSeq != 2 {
		t.Fatalf("Abort = %+v, %v; want transfer seq 2", aborted, err)
	}
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "resent"); err != nil {
		t.Fatal(err)
	}
	expect("an abort and its resend", 2, 0)
	if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "never-began", "begin timed out"); err != nil {
		t.Fatal(err)
	}
	expect("an abort of an id that never began", 3, 0)

	// 带旧序号的请求被拒绝，带回的绑定给出当前值；按它重发就受理。
	stale := TransferRequest{ID: "t-2", FromSID: homeSID, To: awaySID, ExpectSeq: 2}
	refused, err := s.BeginRoleTransfer(ctx, role.PlayerID, stale)
	if !errors.Is(err, ErrTransferStale) || refused.TransferSeq != 3 || refused.Transfer != nil {
		t.Fatalf("Begin with an old transfer seq = %+v, %v; want ErrTransferStale carrying seq 3", refused, err)
	}
	expect("a stale Begin", 3, 0)
	stale.ExpectSeq = refused.TransferSeq
	mustBegin(t, s, role.PlayerID, stale)
	expect("the Begin with the current seq", 4, 0)
	if committed := mustCommit(t, s, role.PlayerID, "t-2"); committed.TransferSeq != 4 || committed.BindEpoch != 1 {
		t.Fatalf("Commit = %+v, want transfer seq 4 at bind epoch 1", committed)
	}
	mustCommit(t, s, role.PlayerID, "t-2")
	expect("a commit and its resend", 4, 1)
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}

// 占不到名额而撤回的发起（ErrRoleLimit）不把 TransferSeq 退回去：那个请求已经用掉了它带的序号，
// 名额让出来之后原样重放它也不会被受理，要重读绑定。
func TestAWithdrawnBeginKeepsItsTransferSeq(t *testing.T) {
	s, cfg, role, session, slots := gatedTransferFixture(t, false, false)
	ctx := context.Background()
	request := nextTransfer(t, s, role.PlayerID, "t-1", awaySID)
	finish := stalledBegin(t, s, slots, role.PlayerID, request)
	// Begin 停在占位之前：账号在目标服建了一个角色，名额没了。
	second, err := s.CreateRole(ctx, role.AccountID, awaySID, "Second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finish(); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("Begin against a taken target = %v, want ErrRoleLimit", err)
	}
	stored := storedRole(t, cfg, role.PlayerID).Value
	if stored.Transfer != nil || stored.LastTransfer != nil || stored.TransferSeq != 1 {
		t.Fatalf("role after the withdrawn Begin = %+v (transfer %+v, last %+v); want nothing in flight, no record, transfer seq 1", stored, stored.Transfer, stored.LastTransfer)
	}
	if slot, found := slotAt(t, cfg, role.AccountID, awaySID); !found || slot.PlayerID != second.PlayerID || slot.TransferID != "" {
		t.Fatalf("the withdrawn Begin disturbed the slot of the new role: %+v (found %v)", slot, found)
	}
	if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, request); !errors.Is(err, ErrTransferStale) {
		t.Fatalf("the withdrawn Begin replayed = %v, want ErrTransferStale", err)
	}
	mustBegin(t, s, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-2", thirdSID))
	mustCommit(t, s, role.PlayerID, "t-2")
	mustBeBoundTo(t, s, cfg, role, session.Token, thirdSID, 1)
}

// 一次已经中止的换服迟到写下的占位，不会把后来在途的那一次挤掉：后一次换服的 Begin 停在占位之前
// （占位写入失败），前一次的占位这时才落地、写它的进程随即退出。重发后一次的 Begin、或直接 Commit，
// 都先撤掉那个没有人认的占位再占自己的，而不是答“名额被占”并把在途的换服撤回。
func TestAStrayHoldDoesNotDisplaceTheTransferInFlight(t *testing.T) {
	ctx := context.Background()
	for _, entry := range []string{"resent Begin", "Commit"} {
		t.Run(entry, func(t *testing.T) {
			s, cfg, role, session, slots := gatedTransferFixture(t, false, true)
			finish := stalledBegin(t, s, slots, role.PlayerID, nextTransfer(t, s, role.PlayerID, "t-1", awaySID))
			if _, err := s.AbortRoleTransfer(ctx, role.PlayerID, "t-1", "begin timed out"); err != nil {
				t.Fatal(err)
			}
			second := nextTransfer(t, s, role.PlayerID, "t-2", awaySID)
			slots.failCreate.Store(true)
			if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, second); !errors.Is(err, errInjected) {
				t.Fatalf("Begin t-2 with the hold write failing = %v, want the injected failure", err)
			}
			slots.failCreate.Store(false)
			if _, err := finish(); !errors.Is(err, errInjected) {
				t.Fatalf("the Begin whose hold reply was lost = %v, want the injected failure", err)
			}
			hold, found := slotAt(t, cfg, role.AccountID, awaySID)
			stored := storedRole(t, cfg, role.PlayerID).Value
			if !found || hold.TransferID != "t-1" || stored.Transfer == nil || stored.Transfer.ID != "t-2" {
				t.Fatalf("the fixture must leave the stray hold of t-1 under t-2 in flight: slot %+v (found %v), transfer %+v", hold, found, stored.Transfer)
			}

			if entry == "Commit" {
				mustCommit(t, s, role.PlayerID, "t-2")
			} else {
				mustBegin(t, s, role.PlayerID, second)
				if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-2" {
					t.Fatalf("the target slot after the resent Begin = %+v (found %v), want the hold of t-2", hold, found)
				}
				mustCommit(t, s, role.PlayerID, "t-2")
			}
			mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
		})
	}
}

// 占位按“这次换服提交之后的那一代绑定”认：ID 与角色都对、代数不对的占位不是这次换服的，不能拿来用——
// 提交之后它与角色的绑定代数对不上，角色就没有可用的 slot 了。这个状态正常流程写不出来（代数不变时
// 同一个 ID 的占位内容相同），这里直接往存储里放一个，确认 Begin 把它换成对的。
func TestAHoldOfAnotherBindGenerationIsReplaced(t *testing.T) {
	s, cfg, role, session, slots, _ := faultyTransferFixture(t)
	ctx := context.Background()
	request := nextTransfer(t, s, role.PlayerID, "t-1", awaySID)
	slots.failCreate.Store(true)
	if _, err := s.BeginRoleTransfer(ctx, role.PlayerID, request); !errors.Is(err, errInjected) {
		t.Fatalf("Begin with the hold write failing = %v, want the injected failure", err)
	}
	slots.failCreate.Store(false)
	source, _ := slotAt(t, cfg, role.AccountID, homeSID)
	wrong := Slot{AccountID: role.AccountID, ServerID: awaySID, PlayerID: role.PlayerID, Creation: source.Creation, TransferID: "t-1", BindEpoch: 7}
	if _, created, err := cfg.Slots.Create(ctx, slotKeyFor(role.AccountID, awaySID), wrong); err != nil || !created {
		t.Fatalf("arrange a hold of another generation: created=%v err=%v", created, err)
	}
	mustBegin(t, s, role.PlayerID, request)
	if hold, found := slotAt(t, cfg, role.AccountID, awaySID); !found || hold.TransferID != "t-1" || hold.BindEpoch != 1 {
		t.Fatalf("the target slot = %+v (found %v), want the hold of t-1 for bind epoch 1", hold, found)
	}
	mustCommit(t, s, role.PlayerID, "t-1")
	mustBeBoundTo(t, s, cfg, role, session.Token, awaySID, 1)
}
