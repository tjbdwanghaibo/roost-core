package account

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/kit/service/directory"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// RR-20261001-06：建角计划的名字租约过期、名字被别的账号确认提交之后，这个账号在
// 该区服必须还能建角。旧行为：slot 永远停在 pending（Admitted 后只有"确定的建角前
// 名字冲突"才释放），同名重试 ErrNameTaken、换名 ErrRoleLimit，没有任何入口释放。
//
// 三条承诺：
//   1. 名字被他人 **确认提交**（committed）后，同名重试仍答 ErrNameTaken，但顺手释放
//      slot——这个计划已经不可能再提交它的名字（Commit 要求 token 与当前条目一致，
//      committed 条目只有 owner 能 Release，本包没有人调 Release），于是也不可能发布角色；
//      丢失回复留下的未发布角色记录保留且不可用。之后换名建角成功。
//   2. 名字只是被他人 **预约**（reserved）时不自动释放——预约会过期，计划仍可能完成；
//      这时由 owner-only 的 Admin.ResolvePendingCreation 带备注放弃计划。
//   3. ResolvePendingCreation 只接受它能证明安全的状态：有计划身份、未发布、名字不被
//      本计划自己持有；备注必填并记到账号上；读后 slot 有变化（并发重试再次 admit 或
//      发布）时释放被版本围栏挡住、报 ErrConflict。
//
// ROOST_REVIEW3_BACKEND=redis 且 ROOST_REVIEW_REDIS 设置时走真实 Redis（名字租约真等
// 过期，TTL 1s），否则 Memory（拨时钟）。

// pendingClaimTTL is the name reservation TTL the tests run with. Memory tests
// drive the clock, so the default is fine; the Redis directory keeps real time,
// so the TTL is short enough to wait for.
func pendingClaimTTL() time.Duration {
	if os.Getenv("ROOST_REVIEW3_BACKEND") == "redis" {
		return time.Second
	}
	return DefaultClaimTTL
}

// lapseNameClaim makes the current reservation of name expire: on Memory by
// advancing the shared clock, on Redis by waiting for the directory to report
// the key absent (a condition, not a fixed sleep).
func lapseNameClaim(t *testing.T, cfg Config, clock *clock, name string) {
	t.Helper()
	if os.Getenv("ROOST_REVIEW3_BACKEND") != "redis" {
		clock.advance(time.Minute)
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, found, err := cfg.Names.Lookup(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("name %q reservation did not lapse within 10s", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// losingPlan leaves account a with an admitted plan for "Hero" whose role
// Create reply was lost, then lets the reservation lapse. roles.first is the
// orphan role id the lost reply created.
func losingPlan(t *testing.T, mutate ...func(*Config)) (*Service, *clock, Config, *review3LostRole, Account) {
	t.Helper()
	var roles *review3LostRole
	s, clock, cfg := review3AccountService(t, append([]func(*Config){func(c *Config) {
		roles = &review3LostRole{Store: c.Roles}
		c.Roles = roles
		c.ClaimTTL = pendingClaimTTL()
	}}, mutate...)...)
	a := login(t, s, "rr06-loser")
	if _, err := s.CreateRole(context.Background(), a.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the role create reply")
	}
	lapseNameClaim(t, cfg, clock, "Hero")
	return s, clock, cfg, roles, a
}

func TestNameCommittedElsewhereReleasesThePendingSlot(t *testing.T) {
	s, _, cfg, roles, a := losingPlan(t)
	ctx := context.Background()
	b := login(t, s, "rr06-winner")
	winner, err := s.CreateRole(ctx, b.ID, 1, "Hero")
	if err != nil {
		t.Fatalf("winner could not take the lapsed name: %v", err)
	}
	if _, err = s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same-name retry after a foreign commit: got %v, want ErrNameTaken", err)
	}
	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("account %s cannot create any role on server 1 after its name was committed elsewhere: %v", a.ID, err)
	}
	if knight.PlayerID == roles.first {
		t.Fatalf("the abandoned plan's player id %d was reused for a different name", roles.first)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID {
		t.Fatalf("slot after the fresh plan: %+v", slot)
	}
	// The orphan the lost reply left behind stays unplayable; the winner's role is untouched.
	if _, err = s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphan role %d became playable: %v", roles.first, err)
	}
	if _, err = s.SelectRole(ctx, b.ID, winner.PlayerID); err != nil {
		t.Fatalf("winner's role broke: %v", err)
	}
	if entry, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found || entry.State != directory.StateCommitted || entry.Owner != directory.Owner(slotKeyFor(b.ID, 1)+"/"+winner.CreationID) {
		t.Fatalf("winner's name entry changed: %+v found=%v err=%v", entry, found, err)
	}
}

func mustSlot(t *testing.T, cfg Config, accountID string) Slot {
	t.Helper()
	slot, found, err := cfg.Slots.Get(context.Background(), slotKeyFor(accountID, 1))
	if err != nil || !found {
		t.Fatalf("slot of %s: found=%v err=%v", accountID, found, err)
	}
	return slot.Value
}

// A foreign RESERVATION is not final: it lapses, and the plan can still
// complete. The slot and its proof stay, exactly as RR-20260929-19 pinned.
func TestNameReservedElsewhereKeepsThePendingSlot(t *testing.T) {
	s, _, cfg, roles, a := losingPlan(t)
	ctx := context.Background()
	if _, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same-name retry under a foreign reservation: got %v, want ErrNameTaken", err)
	}
	if slot := mustSlot(t, cfg, a.ID); !slot.Creation.Admitted || slot.Creation.PlayerID != roles.first {
		t.Fatalf("admitted plan was released under a mere reservation: %+v", slot)
	}
	if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); !errors.Is(err, ErrRoleLimit) {
		t.Fatalf("a different name under a pending plan: got %v, want ErrRoleLimit", err)
	}
}

func TestResolvePendingCreationFreesTheSlotAndRecordsTheNote(t *testing.T) {
	s, clock, cfg, roles, a := losingPlan(t)
	ctx := context.Background()
	if _, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "   "); !errors.Is(err, ErrAdminNoteRequired) {
		t.Fatalf("empty note: got %v, want ErrAdminNoteRequired", err)
	}
	if _, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1)); err != nil || !found {
		t.Fatalf("a refused call touched the slot: found=%v err=%v", found, err)
	}
	clock.advance(time.Second)
	plan, err := s.ResolvePendingCreation(ctx, a.ID, 1, "ticket 42: player wants another name")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.PlayerID != roles.first || plan.Name != "Hero" || !plan.Admitted {
		t.Fatalf("returned plan is not the abandoned one: %+v (orphan %d)", plan, roles.first)
	}
	if _, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1)); err != nil || found {
		t.Fatalf("slot survived the resolution: found=%v err=%v", found, err)
	}
	acct, found, err := cfg.Accounts.Get(ctx, a.ID)
	if err != nil || !found || acct.Value.AdminNote != "ticket 42: player wants another name" || acct.Value.AdminActionAtUnix != clock.Now().Unix() {
		t.Fatalf("note not recorded on the account: %+v found=%v err=%v", acct.Value, found, err)
	}
	// The account creates again, with a fresh plan and a fresh id.
	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("account still blocked after resolution: %v", err)
	}
	if knight.PlayerID == roles.first || knight.CreationID == plan.ID {
		t.Fatalf("fresh plan reused the abandoned identity: %+v vs %+v", knight, plan)
	}
	if _, err = s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphan role %d became playable: %v", roles.first, err)
	}
	if _, err = s.ResolvePendingCreation(ctx, a.ID, 1, "again"); !errors.Is(err, ErrNotResolvable) {
		t.Fatalf("resolving a published slot: got %v, want ErrNotResolvable", err)
	}
	// Login still works and does not erase the note.
	again := login(t, s, "rr06-loser")
	if again.AdminNote != "ticket 42: player wants another name" {
		t.Fatalf("login erased the operator note: %+v", again)
	}
}

// What the entry refuses, because it cannot prove the release safe.
func TestResolvePendingCreationRefusesWhatItCannotProve(t *testing.T) {
	ctx := context.Background()
	t.Run("no slot", func(t *testing.T) {
		s, _, _ := newService(t)
		a := login(t, s, "rr06-empty")
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("got %v, want ErrNotResolvable", err)
		}
	})
	t.Run("legacy empty slot", func(t *testing.T) {
		s, _, cfg := newService(t)
		a := login(t, s, "rr06-legacy")
		if _, _, err := cfg.Slots.Create(ctx, slotKeyFor(a.ID, 1), Slot{AccountID: a.ID, ServerID: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("got %v, want ErrNotResolvable", err)
		}
		if _, found, err := cfg.Slots.Get(ctx, slotKeyFor(a.ID, 1)); err != nil || !found {
			t.Fatalf("legacy slot was guessed away: found=%v err=%v", found, err)
		}
	})
	t.Run("live own reservation", func(t *testing.T) {
		var roles *review3LostRole
		s, _, cfg := newService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
		a := login(t, s, "rr06-live")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the role reply")
		}
		// The reservation has not lapsed: an attempt may be in flight.
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("got %v, want ErrNotResolvable", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first {
			t.Fatalf("slot changed by a refusal: %+v", slot)
		}
		// And the plan does complete by the ordinary path.
		if hero, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil || hero.PlayerID != roles.first {
			t.Fatalf("same-name retry: %+v %v", hero, err)
		}
	})
	t.Run("own committed name", func(t *testing.T) {
		s, clock, cfg := newService(t, func(c *Config) { c.Names = &review3LostCommit{Directory: c.Names} })
		a := login(t, s, "rr06-committed")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the commit reply")
		}
		clock.advance(time.Minute)
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("got %v, want ErrNotResolvable", err)
		}
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil {
			t.Fatalf("same-name retry no longer recovers the committed name: %v", err)
		}
		if _, err := s.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrNotResolvable) {
			t.Fatalf("published slot: got %v, want ErrNotResolvable", err)
		}
		if name, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found || name.State != directory.StateCommitted {
			t.Fatalf("name entry: %+v found=%v err=%v", name, found, err)
		}
	})
	t.Run("slot moved after the decision", func(t *testing.T) {
		var roles *review3LostRole
		s, clock, cfg := newService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
		a := login(t, s, "rr06-moved")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the role reply")
		}
		clock.advance(time.Minute)
		// A same-name retry lands between the operator's read and the
		// release: the plan re-admits (version bump) and publishes.
		cfg.Slots = &slotsRacingResolution{Store: cfg.Slots, race: func() {
			if hero, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil || hero.PlayerID != roles.first {
				t.Errorf("racing retry: %+v %v", hero, err)
			}
		}}
		racer, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := racer.ResolvePendingCreation(ctx, a.ID, 1, "note"); !errors.Is(err, ErrConflict) {
			t.Fatalf("release landed on a slot that moved: got %v, want ErrConflict", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != roles.first {
			t.Fatalf("published role lost its slot: %+v", slot)
		}
		if _, err := s.SelectRole(ctx, a.ID, roles.first); err != nil {
			t.Fatalf("published role not playable: %v", err)
		}
	})
}

// slotsRacingResolution runs race once, after ResolvePendingCreation decided
// (slot and name read, note written) and immediately before its DeleteIf —
// the narrowest window a concurrent retry can land in.
type slotsRacingResolution struct {
	versionstore.Store[string, Slot]
	race func()
	ran  bool
}

func (s *slotsRacingResolution) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	if !s.ran {
		s.ran = true
		s.race()
	}
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}
