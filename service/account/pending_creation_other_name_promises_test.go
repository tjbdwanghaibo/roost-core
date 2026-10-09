package account

// RR-20261001-06 残余（2026-10-05 N06 复核）：名字被他人确认提交、计划已经不可能完成时，
// 换一个名字建角也必须能释放旧计划。
//
// 旧行为：自动释放只写在同名重试的 ErrKeyTaken 分支里；createRole 对“另一个名字”
// 直接答 ErrRoleLimit（“another name is pending”），既不判断旧计划是否已死，也不告诉
// 客户端旧计划的名字。客户端第一次建角失败后换名（玩家最自然的操作）就和修复前一样
// 永久卡在这个区服，只能人工 Admin.ResolvePendingCreation。
//
// 承诺：
//   1. 旧计划的名字已 committed 给别的 owner 时，换名请求按同一证明释放旧 slot、
//      用新计划建角；旧计划可能留下的未发布角色仍不可玩，赢家不受影响。
//   2. 旧计划仍可能完成（名字被自己 reserved/committed，或只被别人 reserved）时
//      换名仍答 ErrRoleLimit，slot 不动。
//   3. 释放失败时报错、slot 保留，下一次换名请求重试释放并成功；补偿失败计
//      dropped:rollback.failed（RR-20261005-NC-50）。
//
// 后端：默认 Memory（拨时钟）；ROOST_REVIEW3_BACKEND=redis 且 ROOST_REVIEW_REDIS 指向隔离
// Redis 时用真实 Redis，
// 键前缀 revn06:account:<纳秒>，名字租约 1s 真等过期。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"

	"github.com/tjbdwanghaibo/roost-core/service/directory"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// revn06Redis follows the sibling RR-06 tests' gate (ROOST_REVIEW3_BACKEND=redis plus
// ROOST_REVIEW_REDIS), which the CI Redis job sets; "" means Memory.
func revn06Redis() string {
	if os.Getenv("ROOST_REVIEW3_BACKEND") != "redis" {
		return ""
	}
	return os.Getenv("ROOST_REVIEW_REDIS")
}

// revn06AccountService is newService, optionally over the isolated Redis.
func revn06AccountService(t *testing.T, mutate ...func(*Config)) (*Service, *clock, Config) {
	t.Helper()
	addr := revn06Redis()
	if addr == "" {
		return newService(t, mutate...)
	}
	client, err := driver.NewClient(fredis.DefaultConfig(addr))
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("revn06:account:%d", time.Now().UnixNano())
	t.Cleanup(func() {
		client.Close()
		c := goredis.NewClient(&goredis.Options{Addr: addr})
		defer c.Close()
		keys, err := c.Keys(context.Background(), prefix+":*").Result()
		if err == nil && len(keys) > 0 {
			err = c.Del(context.Background(), keys...).Err()
		}
		if err != nil {
			t.Errorf("cleanup %s: %v", prefix, err)
		}
	})
	stores, err := NewRedisStores(client, prefix, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	base := func(c *Config) {
		c.Accounts, c.Roles, c.Servers, c.Slots, c.Names = stores.Accounts, stores.Roles, stores.Servers, stores.Slots, stores.Names
		c.ClaimTTL = time.Second
	}
	return newService(t, append([]func(*Config){base}, mutate...)...)
}

// revn06Lapse lets the current reservation of name expire.
func revn06Lapse(t *testing.T, cfg Config, clock *clock, name string) {
	t.Helper()
	if revn06Redis() == "" {
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

// revn06DeadPlan leaves account "revn06-loser" with an admitted plan for
// "Hero" whose role Create reply was lost, lets the reservation lapse and has
// another account commit "Hero". The loser's plan can no longer complete.
func revn06DeadPlan(t *testing.T, mutate ...func(*Config)) (*Service, Config, *review3LostRole, Account, Role) {
	t.Helper()
	var roles *review3LostRole
	s, clock, cfg := revn06AccountService(t, append([]func(*Config){func(c *Config) {
		roles = &review3LostRole{Store: c.Roles}
		c.Roles = roles
	}}, mutate...)...)
	ctx := context.Background()
	a := login(t, s, "revn06-loser")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the role create reply")
	}
	revn06Lapse(t, cfg, clock, "Hero")
	b := login(t, s, "revn06-winner")
	winner, err := s.CreateRole(ctx, b.ID, 1, "Hero")
	if err != nil {
		t.Fatalf("winner could not take the lapsed name: %v", err)
	}
	return s, cfg, roles, a, winner
}

func TestADifferentNameReleasesAPlanWhoseNameWasCommittedElsewhere(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	s, cfg, roles, a, winner := revn06DeadPlan(t, func(c *Config) { c.Metrics = sink })
	ctx := context.Background()

	// No same-name retry first: the player simply picks another name.
	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("account %s cannot create a role under another name after its pending name was committed elsewhere: %v", a.ID, err)
	}
	if knight.PlayerID == roles.first || knight.Name != "Knight" {
		t.Fatalf("fresh plan reused the abandoned plan: %+v (orphan %d)", knight, roles.first)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID || slot.Creation.Name != "Knight" {
		t.Fatalf("slot after the fresh plan: %+v", slot)
	}
	if _, err := s.SelectRole(ctx, a.ID, knight.PlayerID); err != nil {
		t.Fatalf("new role not playable: %v", err)
	}
	if _, err := s.SelectRole(ctx, a.ID, roles.first); !errors.Is(err, ErrConflict) {
		t.Fatalf("orphan role %d became playable: %v", roles.first, err)
	}
	if entry, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found || entry.State != directory.StateCommitted ||
		entry.Owner != directory.Owner(slotKeyFor(winner.AccountID, 1)+"/"+winner.CreationID) {
		t.Fatalf("winner's name entry changed: %+v found=%v err=%v", entry, found, err)
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 1 {
		t.Fatalf("plan release counted %d times; %s", got, sink.Events())
	}
	if got := sink.Count("dropped:rollback.failed"); got != 0 {
		t.Fatalf("a successful release counted %d failures; %s", got, sink.Events())
	}
}

// What a different name must NOT release: plans that can still complete.
func TestADifferentNameKeepsAPlanThatCanStillComplete(t *testing.T) {
	ctx := context.Background()
	t.Run("own live reservation", func(t *testing.T) {
		var roles *review3LostRole
		s, _, cfg := revn06AccountService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
		a := login(t, s, "revn06-live")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the role reply")
		}
		if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("got %v, want ErrRoleLimit", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first || slot.Creation.Name != "Hero" {
			t.Fatalf("slot changed: %+v", slot)
		}
		if hero, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil || hero.PlayerID != roles.first {
			t.Fatalf("same-name retry: %+v %v", hero, err)
		}
	})
	t.Run("own committed name", func(t *testing.T) {
		s, clock, cfg := revn06AccountService(t, func(c *Config) { c.Names = &review3LostCommit{Directory: c.Names} })
		a := login(t, s, "revn06-committed")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the commit reply")
		}
		clock.advance(time.Minute) // a committed entry never lapses; only time moves
		if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("got %v, want ErrRoleLimit", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.Creation.Name != "Hero" || slot.PlayerID != 0 {
			t.Fatalf("slot changed: %+v", slot)
		}
		if hero, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err != nil || hero.Name != "Hero" {
			t.Fatalf("same-name retry no longer completes the plan: %+v %v", hero, err)
		}
	})
	t.Run("foreign reservation only", func(t *testing.T) {
		var roles *review3LostRole
		s, clock, cfg := revn06AccountService(t, func(c *Config) { roles = &review3LostRole{Store: c.Roles}; c.Roles = roles })
		a := login(t, s, "revn06-reserved")
		if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
			t.Fatal("fixture must lose the role reply")
		}
		revn06Lapse(t, cfg, clock, "Hero")
		if _, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); !errors.Is(err, ErrRoleLimit) {
			t.Fatalf("got %v, want ErrRoleLimit", err)
		}
		if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first || !slot.Creation.Admitted {
			t.Fatalf("plan released under a mere reservation: %+v", slot)
		}
	})
}

// slotsFailingDeleteOnce loses the first conditional delete — the release —
// while every other slot operation is real.
type slotsFailingDeleteOnce struct {
	versionstore.Store[string, Slot]
	failed bool
}

func (s *slotsFailingDeleteOnce) DeleteIf(ctx context.Context, key string, expect versionstore.Versioned[Slot], match func(Slot) bool) error {
	if !s.failed {
		s.failed = true
		return fmt.Errorf("slots: connection reset")
	}
	return s.Store.(versionstore.ConditionalDeleter[string, Slot]).DeleteIf(ctx, key, expect, match)
}

func TestAFailedReleaseOfADeadPlanIsCountedAndRetriedByTheNextName(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	var slots *slotsFailingDeleteOnce
	s, cfg, roles, a, _ := revn06DeadPlan(t, func(c *Config) {
		c.Metrics = sink
		slots = &slotsFailingDeleteOnce{Store: c.Slots}
		c.Slots = slots
	})
	ctx := context.Background()
	if _, err := s.CreateRole(ctx, a.ID, 1, "Knight"); err == nil {
		t.Fatal("a create whose release failed reported success")
	}
	if !slots.failed {
		t.Fatal("the different-name create never tried to release the dead plan")
	}
	if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first {
		t.Fatalf("slot after a failed release: %+v", slot)
	}
	if got := sink.Count("dropped:rollback.failed"); got != 1 {
		t.Fatalf("a failed release counted %d times, want 1; %s", got, sink.Events())
	}
	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("the next attempt did not retry the release: %v", err)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID {
		t.Fatalf("slot after the retried release: %+v", slot)
	}
}

// NC-50：同名重试路径的补偿失败同样要计数（Config.Metrics 的“failed rollback”信号）。
func TestAFailedReleaseOnTheSameNamePathIsCounted(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	var slots *slotsFailingDeleteOnce
	s, cfg, roles, a, _ := revn06DeadPlan(t, func(c *Config) {
		c.Metrics = sink
		slots = &slotsFailingDeleteOnce{Store: c.Slots}
		c.Slots = slots
	})
	ctx := context.Background()
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same-name retry after a foreign commit: got %v, want ErrNameTaken", err)
	}
	if !slots.failed {
		t.Fatal("the same-name retry never tried to release the dead plan")
	}
	if slot := mustSlot(t, cfg, a.ID); slot.Creation.PlayerID != roles.first {
		t.Fatalf("slot after a failed release: %+v", slot)
	}
	if got := sink.Count("dropped:rollback.failed"); got != 1 {
		t.Fatalf("a failed compensation counted %d times, want 1; %s", got, sink.Events())
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 0 {
		t.Fatalf("a failed release was counted as released; %s", sink.Events())
	}
}
