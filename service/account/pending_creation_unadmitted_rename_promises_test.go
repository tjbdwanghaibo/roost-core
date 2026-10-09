package account

// 维护者第五轮决定（account 换名释放，2026-10-06）：计划还没 admitted、名字只被别人 reserved
// （还没 committed）时，换名请求也释放 slot，与同名重试对齐。
//
// 旧行为（B9 判定表 `unadmitted other` 行 res-else 格 = limit）：同名重试在这个事实上释放
// slot 并答 ErrNameTaken（未 admitted 的计划之后没有任何事发生，释放不会留下角色）；换名请求却
// 答 ErrRoleLimit、slot 保留——玩家换名重试被挡住，直到别人的预约过期或同名重试一次。
//
// 承诺：换名请求释放这个未 admitted 的计划、用新名字建角；旧名字的预约仍归预约方；
// plan_released 计一次。已 admitted 的计划在同一名字事实上仍答 ErrRoleLimit（可能还会完成），
// 由 TestADifferentNameKeepsAPlanThatCanStillComplete/foreign_reservation_only 钉住。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/service/directory"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// lostReserveReply applies the first Reserve and loses its reply, so the plan
// stays unadmitted while its reservation exists.
type lostReserveReply struct {
	directory.Directory
	lost bool
}

func (n *lostReserveReply) Reserve(ctx context.Context, name string, owner directory.Owner, ttl time.Duration) (directory.Claim, error) {
	claim, err := n.Directory.Reserve(ctx, name, owner, ttl)
	if err == nil && !n.lost {
		n.lost = true
		return directory.Claim{}, errors.New("name reserve reply lost")
	}
	return claim, err
}

func TestADifferentNameReleasesAnUnadmittedPlanWhoseNameIsReservedElsewhere(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	s, clock, cfg := revn06AccountService(t, func(c *Config) {
		c.Metrics = sink
		c.Names = &lostReserveReply{Directory: c.Names}
	})
	ctx := context.Background()
	a := login(t, s, "revn09f-rename")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the reserve reply")
	}
	if slot := mustSlot(t, cfg, a.ID); slot.Creation.Name != "Hero" || slot.Creation.Admitted {
		t.Fatalf("fixture must leave an unadmitted plan for Hero: %+v", slot)
	}
	revn06Lapse(t, cfg, clock, "Hero")
	if _, err := cfg.Names.Reserve(ctx, "Hero", "rival", time.Minute); err != nil {
		t.Fatal(err)
	}

	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("a different name must release the unadmitted plan, got %v (slot %+v)", err, mustSlot(t, cfg, a.ID))
	}
	if knight.Name != "Knight" {
		t.Fatalf("created %+v, want Knight", knight)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID || slot.Creation.Name != "Knight" {
		t.Fatalf("slot after the fresh plan: %+v", slot)
	}
	if entry, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || !found || entry.Owner != "rival" {
		t.Fatalf("the rival's reservation changed: %+v found=%v err=%v", entry, found, err)
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 1 {
		t.Fatalf("plan release counted %d times; %s", got, sink.Events())
	}
}

// 维护者第七轮决定（O37，2026-10-06）：同一行的 free 格也释放。计划还没 admitted、它的名字已经
// 无人持有（预约过期或从未预约成功）时，换名请求释放 slot 并用新名字建角，与 res-else 格一致。
//
// 旧行为（free 格 = limit）：答 ErrRoleLimit、slot 保留——玩家只能先用旧名字重试一次才能换名，
// 而这个计划 admitted 之前什么都没发生，释放不会留下角色。
func TestADifferentNameReleasesAnUnadmittedPlanWhoseNameIsFree(t *testing.T) {
	sink := servicemetrics.NewRecorder()
	s, clock, cfg := revn06AccountService(t, func(c *Config) {
		c.Metrics = sink
		c.Names = &lostReserveReply{Directory: c.Names}
	})
	ctx := context.Background()
	a := login(t, s, "o37-rename-free")
	if _, err := s.CreateRole(ctx, a.ID, 1, "Hero"); err == nil {
		t.Fatal("fixture must lose the reserve reply")
	}
	if slot := mustSlot(t, cfg, a.ID); slot.Creation.Name != "Hero" || slot.Creation.Admitted {
		t.Fatalf("fixture must leave an unadmitted plan for Hero: %+v", slot)
	}
	revn06Lapse(t, cfg, clock, "Hero")
	if _, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || found {
		t.Fatalf("fixture must leave Hero free: found=%v err=%v", found, err)
	}

	knight, err := s.CreateRole(ctx, a.ID, 1, "Knight")
	if err != nil {
		t.Fatalf("a different name must release the unadmitted plan whose name is free, got %v (slot %+v)", err, mustSlot(t, cfg, a.ID))
	}
	if knight.Name != "Knight" {
		t.Fatalf("created %+v, want Knight", knight)
	}
	if slot := mustSlot(t, cfg, a.ID); slot.PlayerID != knight.PlayerID || slot.Creation.Name != "Knight" {
		t.Fatalf("slot after the fresh plan: %+v", slot)
	}
	if _, found, err := cfg.Names.Lookup(ctx, "Hero"); err != nil || found {
		t.Fatalf("Hero must stay free: found=%v err=%v", found, err)
	}
	if got := sink.Count("dropped:create_role.plan_released"); got != 1 {
		t.Fatalf("plan release counted %d times; %s", got, sink.Events())
	}
}
