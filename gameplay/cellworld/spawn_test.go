package cellworld

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

func TestSpawnSlotKeepsQuotaAcrossHandoffAndRespawnsAfterAck(t *testing.T) {
	ctx := context.Background()
	alive := true
	r, _ := registryForTest(t, func(context.Context, OwnerProcess) (bool, error) { return alive, nil })
	now := time.Now()
	spec := SpawnSpec{Key: "wolves-1", Home: 101, State: EntityState{ID: npcID(1), Kind: KindNPC, HP: 5}, RespawnDelay: time.Second}
	reserved, err := r.prepareSpawn(ctx, spec, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.claim(ctx, ownerClaim{Actor: spec.State.ID, Kind: KindNPC, Generation: reserved.Birth.Generation, Process: OwnerProcess{101, "a"}, Token: "first"})
	if err != nil {
		t.Fatal(err)
	}
	transfer := OwnerTransfer{ID: "cross", Target: 102, Epoch: first.Grant.Epoch + 1}
	if _, err = r.begin(ctx, first.Actor, first.Grant, transfer); err != nil {
		t.Fatal(err)
	}
	moved, err := r.accept(ctx, first.Actor, first.Grant, transfer, OwnerProcess{102, "b"})
	if err != nil {
		t.Fatal(err)
	}
	row, err := r.prepareSpawn(ctx, spec, now)
	if err != nil || row != moved {
		t.Fatalf("home refilled a migrated slot: %+v %v", row, err)
	}
	fact := deathFor(moved)
	fact.At = now.UnixMilli()
	if _, err = r.recordDeath(ctx, moved.Grant, fact); err != nil {
		t.Fatal(err)
	}
	before, err := r.prepareSpawn(ctx, spec, now.Add(2*time.Second))
	if err != nil || before.Birth.Generation != 1 {
		t.Fatal("unacknowledged reward respawned")
	}
	if _, err = r.ackDeath(ctx, moved.Actor, fact.ID, worldBaseSID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.retire(ctx, moved.Actor, moved.Grant); err != nil {
		t.Fatal(err)
	}
	waiting, err := r.prepareSpawn(ctx, spec, now.Add(time.Millisecond))
	if err != nil || !waiting.Birth.Waiting || waiting.Birth.Generation != 1 {
		t.Fatalf("lost respawn deadline: %+v %v", waiting, err)
	}
	due, err := r.prepareSpawn(ctx, spec, now.Add(2*time.Second))
	if err != nil || due.Birth.Generation != 2 || due.Birth.Waiting {
		t.Fatalf("not due: %+v %v", due, err)
	}
	again, err := r.prepareSpawn(ctx, spec, now.Add(3*time.Second))
	if err != nil || again != due {
		t.Fatal("repeated sweep advanced generation")
	}
	if _, err = r.claim(ctx, ownerClaim{Actor: first.Actor, Kind: KindNPC, Generation: 1, Process: OwnerProcess{101, "a"}, Token: "late"}); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("late previous generation accepted: %v", err)
	}
	fresh, err := r.claim(ctx, ownerClaim{Actor: first.Actor, Kind: KindNPC, Generation: 2, Process: OwnerProcess{101, "a"}, Token: "next"})
	if err != nil || fresh.Life <= first.Life || fresh.Death.ID != "" {
		t.Fatalf("new life: %+v %v", fresh, err)
	}
	alive = false
	recovered, err := r.claim(ctx, ownerClaim{Actor: first.Actor, Kind: KindNPC, Generation: 2, Process: OwnerProcess{103, "c"}, Token: "restart"})
	if err != nil || recovered.Life != fresh.Life || recovered.Birth.Generation != 2 {
		t.Fatal("restart advanced life")
	}
}
func TestSpawnCatalogStableAcrossReorderAndRestart(t *testing.T) {
	ctx := context.Background()
	store := versionstore.NewMemoryStore[int32, SpawnCatalog]()
	book, _ := NewSpawnBook(store)
	first, err := book.Reserve(ctx, 101, []string{"wolves-0", "wolves-1"})
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewSpawnBook(store)
	next, err := restarted.Reserve(ctx, 101, []string{"wolves-1", "wolves-0", "boss-0"})
	if err != nil || next["wolves-0"] != first["wolves-0"] || next["boss-0"] != 3 {
		t.Fatalf("identity changed %v %v", next, err)
	}
	if _, err = book.Reserve(ctx, 101, []string{"boss-0"}); err == nil {
		t.Fatal("silently dropped live slots")
	}
}
func TestReconcileSpawnDoesNotDuplicateMigratedNPC(t *testing.T) {
	spec, _ := loopbackSpec(t, 2, 1)
	w := startWorld(t, spec)
	home := w.cells[CellID{0, 0}].node
	slot := SpawnSpec{Key: "patrol-0", Home: home.cfg.SID, State: EntityState{ID: npcID(951), Kind: KindNPC, X: 9950, Y: 5000, VX: 400, HP: 100}, RespawnDelay: time.Second}
	ctx, cancel := w.ctx()
	defer cancel()
	if err := home.ReconcileSpawn(ctx, slot); err != nil {
		t.Fatal(err)
	}
	w.enter(1, spatial.Point{X: 9800, Y: 5000})
	eventually(t, settleWithin, "NPC migrated", func() (bool, string) {
		s, ok := w.roleOf(CellID{1, 0}, slot.State.ID)
		return ok && s.Role == "real", s.Role
	})
	for range 3 {
		if err := home.ReconcileSpawn(ctx, slot); err != nil {
			t.Fatal(err)
		}
	}
	w.assertSingleReal(slot.State.ID, CellID{1, 0})
}

// 死亡已确认、Grant 退役之前 owner 崩溃：记录停在 active + Waiting。出生巡检必须在核实旧进程已死后关闭它，
// 让槽按复活时间推进下一代，而不是永远等一个不存在的退役。
func TestSpawnSlotRespawnsAfterOwnerCrashBeforeRetire(t *testing.T) {
	spec, _ := loopbackSpec(t, 1, 1)
	w := startWorld(t, spec)
	home := w.cells[CellID{0, 0}].node
	r := home.cfg.Owners
	ctx, cancel := w.ctx()
	defer cancel()
	slot := SpawnSpec{Key: "crash-0", Home: home.cfg.SID, State: EntityState{ID: npcID(961), Kind: KindNPC, X: 5000, Y: 5000, HP: 10}}
	reserved, err := r.prepareSpawn(ctx, slot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	crashed := OwnerProcess{SID: home.cfg.SID, Incarnation: "crashed-incarnation"} // 不在存活集合里
	row, err := r.claim(ctx, ownerClaim{Actor: slot.State.ID, Kind: KindNPC, Generation: reserved.Birth.Generation, Process: crashed, Token: "before-crash"})
	if err != nil {
		t.Fatal(err)
	}
	fact := deathFor(row)
	fact.At = time.Now().UnixMilli()
	if _, err = r.recordDeath(ctx, row.Grant, fact); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ackDeath(ctx, row.Actor, fact.ID, worldBaseSID); err != nil {
		t.Fatal(err)
	}
	eventually(t, settleWithin, "slot respawned", func() (bool, string) {
		if err := home.ReconcileSpawn(ctx, slot); err != nil {
			return false, err.Error()
		}
		cur, _, err := r.store.Get(ctx, slot.State.ID)
		s, ok := w.roleOf(CellID{0, 0}, slot.State.ID)
		return err == nil && cur.Value.Birth.Generation == 2 && ok && s.Role == "real",
			fmt.Sprintf("gen=%d waiting=%v phase=%v role=%q", cur.Value.Birth.Generation, cur.Value.Birth.Waiting, cur.Value.Phase, s.Role)
	})
	w.assertSingleReal(slot.State.ID, CellID{0, 0})
}

// 活的 owner 尚未退役时，巡检不能替它关闭记录：只有核实旧进程已死才代为收尾。
func TestSpawnSweepDoesNotCloseALiveOwnersAcknowledgedDeath(t *testing.T) {
	ctx := context.Background()
	r, _ := registryForTest(t, func(context.Context, OwnerProcess) (bool, error) { return true, nil })
	spec := SpawnSpec{Key: "live-0", Home: 101, State: EntityState{ID: npcID(962), Kind: KindNPC, HP: 5}}
	reserved, _ := r.prepareSpawn(ctx, spec, time.Now())
	row, err := r.claim(ctx, ownerClaim{Actor: spec.State.ID, Kind: KindNPC, Generation: reserved.Birth.Generation, Process: OwnerProcess{102, "alive"}, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	fact := deathFor(row)
	if _, err = r.recordDeath(ctx, row.Grant, fact); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ackDeath(ctx, row.Actor, fact.ID, worldBaseSID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.closeAbandoned(ctx, spec.State.ID); !errors.Is(err, ErrOwnerConflict) {
		t.Fatalf("closed a live owner's record: %v", err)
	}
	if cur, _, _ := r.store.Get(ctx, spec.State.ID); cur.Value.Phase != ownerActive {
		t.Fatalf("phase %v", cur.Value.Phase)
	}
}
