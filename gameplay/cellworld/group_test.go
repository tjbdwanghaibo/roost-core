package cellworld

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	fentity "github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 队伍面板（第 4 项 = A）：两个 Base 进程（sid 900、901）上的队员彼此不在视野里，经 Base 之间的快照在面板（组关系视野）
// 里看见对方；快照停了 3 个周期移除；成员变化经通知、丢了经重读收敛；换格交接时面板不闪；在视野里的队员只是一个 subject。

const (
	teamEvery = 200 * time.Millisecond
	teamHold  = 3 * teamEvery
	teamBase2 = 901
	teamID    = 77
)

type teamWorld struct {
	*world
	lb    *loopback
	bases map[int32]*testProc

	mu        sync.Mutex
	authority map[int64]Group         // 测试里的“权威”（game 里是 Remote 托管的 Team 实体）
	resyncs   map[int64]int           // GroupResync 被调用的次数，按 Player 实体
	snaps     map[[2]int32][]int64    // kindGroupSnap：(发送 sid, 收件 sid) → 快照里的角色
	notices   map[int32][]groupNotice // kindGroupNotice：收件 sid → 通知
	dropNote  bool
	resyncMod func(*NodeConfig)
}

func startTeamWorld(t *testing.T, mod func(*NodeConfig)) *teamWorld {
	spec, lb := loopbackSpec(t, 2, 1)
	tw := &teamWorld{lb: lb, bases: map[int32]*testProc{}, authority: map[int64]Group{}, resyncs: map[int64]int{},
		snaps: map[[2]int32][]int64{}, notices: map[int32][]groupNotice{}, resyncMod: mod}
	spec.baseMod = tw.baseMod(worldBaseSID)
	lb.setFault(func(subject string, m *wireMsg) (bool, bool, time.Duration) {
		tw.mu.Lock()
		defer tw.mu.Unlock()
		switch m.Kind {
		case kindGroupSnap:
			key := [2]int32{m.From, sidOfSubject(subject)}
			tw.snaps[key] = append(tw.snaps[key], m.GroupSnap.State.ID)
		case kindGroupNotice:
			tw.notices[sidOfSubject(subject)] = append(tw.notices[sidOfSubject(subject)], *m.GroupNotice)
			return tw.dropNote, false, 0
		}
		return false, false, 0
	})
	tw.world = startWorld(t, spec)
	tw.bases[worldBaseSID] = tw.world.base
	tw.startBase(teamBase2)
	return tw
}

func sidOfSubject(subject string) int32 {
	for sid := int32(100); sid < 1000; sid++ {
		if Subject("w", sid) == subject {
			return sid
		}
	}
	return 0
}

// baseMod 给 Base 进程打开队伍面板：每个玩家的组视野 ID 是 1<<53 | 玩家号；重读按测试里的“权威”在玩家自己的 Base 上
// 投一笔改成员表的请求（异步：GroupResync 只做准入）。
func (tw *teamWorld) baseMod(sid int32) func(*NodeConfig) {
	return func(cfg *NodeConfig) {
		cfg.GroupView = groupViewOf
		cfg.GroupEvery = teamEvery
		cfg.GroupResync = func(ctx context.Context, group, player int64) error {
			tw.mu.Lock()
			tw.resyncs[player]++
			g := tw.authority[group]
			base := tw.bases[sid]
			tw.mu.Unlock()
			if _, in := g.member(player); !in {
				g = Group{}
			}
			if base == nil {
				return nil
			}
			return base.engine.Dispatch(ctx, hTestGroup, player, nest.Params{testGroupCmd{set: true, group: g}})
		}
		if tw.resyncMod != nil {
			tw.resyncMod(cfg)
		}
	}
}

func groupViewOf(playerEntity int64) policy.ViewID {
	return policy.ViewID(1<<53 | fentity.GetUniqueIDFromEntityID(playerEntity))
}

func (tw *teamWorld) startBase(sid int32) *testProc {
	p := startProc(tw.t, procSpec{sid: sid, base: true, grid: tw.grid, transport: tw.lb, mod: tw.baseMod(sid)})
	tw.t.Cleanup(p.stop)
	tw.mu.Lock()
	tw.bases[sid] = p
	tw.mu.Unlock()
	return p
}

func teamMember(unique int64, sid int32) GroupMember {
	return GroupMember{Player: playerEntityID(unique), Actor: actorID(unique), SID: sid}
}

// enterOn 让 sid 上的玩家进入并等到它的客户端持有自己的 SceneActor。
func (tw *teamWorld) enterOn(sid int32, unique int64, at spatial.Point) {
	tw.t.Helper()
	base := tw.bases[sid]
	pl := base.player(unique)
	ctx, cancel := tw.ctx()
	defer cancel()
	if err := base.node.Enter(ctx, pl.ID(), actorID(unique), at, 100); err != nil {
		tw.t.Fatal(err)
	}
	waitFor(tw.t, 5*time.Second, "own scene actor on the client", func() bool {
		_, ok := base.client.held(unique)[actorID(unique)]
		return ok
	})
}

func (tw *teamWorld) setGroup(sid int32, unique int64, cmd testGroupCmd) {
	tw.t.Helper()
	ctx, cancel := tw.ctx()
	defer cancel()
	if _, err := tw.bases[sid].engine.Request(ctx, hTestGroup, playerEntityID(unique), nest.Params{cmd}); err != nil {
		tw.t.Fatal(err)
	}
}

// setAuthority 改测试里的“权威”成员表（game 里是 Team 实体上的一笔事务）。
func (tw *teamWorld) setAuthority(g Group) {
	tw.mu.Lock()
	tw.authority[g.ID] = g
	tw.mu.Unlock()
}

// form 组成队伍 g：权威写成 g，每名成员在自己的 Base 上把缓存写成 g（game 里是入队的那笔事务）。
func (tw *teamWorld) form(g Group) {
	tw.t.Helper()
	tw.setAuthority(g)
	for _, m := range g.Members {
		tw.setGroup(m.SID, fentity.GetUniqueIDFromEntityID(m.Player), testGroupCmd{set: true, group: g})
	}
}

func (tw *teamWorld) groupOf(sid int32, unique int64) Group {
	tw.t.Helper()
	ctx, cancel := tw.ctx()
	defer cancel()
	out, err := tw.bases[sid].engine.Request(ctx, hTestGroup, playerEntityID(unique), nest.Params{testGroupCmd{}})
	if err != nil {
		tw.t.Fatal(err)
	}
	return out.(Group)
}

// holds 是 sid 上 viewer 的客户端此刻持有的 target 的 SceneActor。
func (tw *teamWorld) holds(sid int32, viewer, target int64) (EntityState, bool) {
	s, ok := tw.bases[sid].client.held(viewer)[actorID(target)]
	return s, ok
}

func (tw *teamWorld) waitHolds(sid int32, viewer, target int64, what string, cond func(EntityState) bool) EntityState {
	tw.t.Helper()
	var last EntityState
	var present bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if last, present = tw.holds(sid, viewer, target); present && cond(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	tw.t.Fatalf("player %d on sid %d: timed out waiting for %s of player %d; holds it %v (%+v), node stats %+v",
		viewer, sid, what, target, present, last, tw.bases[sid].node.Stats())
	return EntityState{}
}

func (tw *teamWorld) removalOf(sid int32, viewer, target int64) (time.Time, bool) {
	for _, r := range tw.bases[sid].client.removed(viewer) {
		if r.id == actorID(target) {
			return r.at, true
		}
	}
	return time.Time{}, false
}

func (tw *teamWorld) snapsFrom(from, to int32) []int64 {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	return append([]int64(nil), tw.snaps[[2]int32{from, to}]...)
}

// 两个 Base 上彼此不在视野里的队员在面板里看见对方的位置与 HP；同一个 Base 上的队员从本进程自己的 mirror 来，不经
// Base 之间的快照（不发给自己的 sid）。
func TestGroupPanelAcrossBases(t *testing.T) {
	tw := startTeamWorld(t, nil)
	tw.enterOn(worldBaseSID, 1, spatial.Point{X: 2000, Y: 5000})
	tw.enterOn(teamBase2, 2, spatial.Point{X: 17000, Y: 5000})
	tw.enterOn(worldBaseSID, 3, spatial.Point{X: 8000, Y: 2000})
	time.Sleep(3 * teamEvery)
	for _, pair := range [][2]int64{{1, 2}, {1, 3}, {2, 1}} {
		sid := int32(worldBaseSID)
		if pair[0] == 2 {
			sid = teamBase2
		}
		if _, ok := tw.holds(sid, pair[0], pair[1]); ok {
			t.Fatalf("before the team: player %d already holds player %d", pair[0], pair[1])
		}
	}
	g := Group{ID: teamID, Ver: 1, Members: []GroupMember{teamMember(1, worldBaseSID), teamMember(2, teamBase2), teamMember(3, worldBaseSID)}}
	tw.form(g)

	far := tw.waitHolds(worldBaseSID, 1, 2, "the teammate on the other base", func(s EntityState) bool { return s.X == 17000 })
	if far.HP != 100 || far.Y != 5000 {
		t.Fatalf("player 1 sees player 2 at (%d,%d) hp %d, want (17000,5000) hp 100", far.X, far.Y, far.HP)
	}
	tw.waitHolds(teamBase2, 2, 1, "the teammate on the other base", func(s EntityState) bool { return s.X == 2000 && s.HP == 100 })
	tw.waitHolds(teamBase2, 2, 3, "the second teammate on the other base", func(s EntityState) bool { return s.X == 8000 && s.HP == 100 })
	tw.waitHolds(worldBaseSID, 1, 3, "the teammate on the same base", func(s EntityState) bool { return s.X == 8000 })

	if got := tw.snapsFrom(worldBaseSID, worldBaseSID); len(got) > 0 {
		t.Fatalf("base %d sent %d team snapshots to itself; same-base teammates come from its own mirrors", worldBaseSID, len(got))
	}
	sent := tw.snapsFrom(worldBaseSID, teamBase2)
	if !slices.Contains(sent, actorID(1)) || !slices.Contains(sent, actorID(3)) {
		t.Fatalf("base %d sent snapshots of %v to base %d, want both of its teammates", worldBaseSID, sent, teamBase2)
	}
	if got := tw.snapsFrom(teamBase2, worldBaseSID); !slices.Contains(got, actorID(2)) {
		t.Fatalf("base %d sent snapshots of %v to base %d, want player 2", teamBase2, got, worldBaseSID)
	}
	// 一个周期一份：1s 里每名队员发出的快照数在 [1s / 周期 - 2, 1s / 周期 + 2] 之间（不是每拍一份）。
	before := len(tw.snapsFrom(teamBase2, worldBaseSID))
	time.Sleep(time.Second)
	if got := len(tw.snapsFrom(teamBase2, worldBaseSID)) - before; got < int(time.Second/teamEvery)-2 || got > int(time.Second/teamEvery)+2 {
		t.Fatalf("player 2's base sent %d snapshots in 1s, want about %d (one per %v)", got, time.Second/teamEvery, teamEvery)
	}
}

// 对方 Base 进程停了：快照停止，超过 3 个周期没更新从面板移除（不早于最后一份快照之后 3 个周期）；Base 重启后成员表
// 缓存的版本为 0，下一个周期就从权威重读，面板恢复。
func TestGroupPanelExpiresWhenTheBaseStopsAndComesBack(t *testing.T) {
	tw := startTeamWorld(t, nil)
	tw.enterOn(worldBaseSID, 1, spatial.Point{X: 2000, Y: 5000})
	tw.enterOn(teamBase2, 2, spatial.Point{X: 17000, Y: 5000})
	g := Group{ID: teamID, Ver: 1, Members: []GroupMember{teamMember(1, worldBaseSID), teamMember(2, teamBase2)}}
	tw.form(g)
	tw.waitHolds(worldBaseSID, 1, 2, "the teammate", func(s EntityState) bool { return s.X == 17000 })

	stopped := time.Now()
	tw.bases[teamBase2].stop()
	// 最后一份快照最早在停之前一个周期（加一拍心跳）发出：停后 hold - 周期 - 一拍之内面板里还有它。
	time.Sleep(teamHold - teamEvery - 150*time.Millisecond)
	if _, ok := tw.holds(worldBaseSID, 1, 2); !ok {
		t.Fatalf("player 2 left player 1's panel %v after its base stopped, before 3 periods (%v) without a snapshot", time.Since(stopped), teamHold)
	}
	waitFor(t, 3*time.Second, "player 2 removed from player 1's panel", func() bool {
		_, ok := tw.removalOf(worldBaseSID, 1, 2)
		return ok
	})
	at, _ := tw.removalOf(worldBaseSID, 1, 2)
	if gone := at.Sub(stopped); gone < teamHold-teamEvery-150*time.Millisecond || gone > teamHold+500*time.Millisecond {
		t.Fatalf("player 2 left player 1's panel %v after its base stopped, want about %v (3 periods without a snapshot)", gone, teamHold)
	}

	// 重启：内存里的成员表没了，持久的组 ID 还在（game 里是 Player 的 TeamID）——版本 0 表示成员未知，Base 去权威重读。
	base := tw.startBase(teamBase2)
	pl := base.player(2)
	pl.group.v = Group{ID: teamID}
	tw.enterOn(teamBase2, 2, spatial.Point{X: 17000, Y: 5000})
	waitFor(t, 3*time.Second, "a resync of player 2 after its base restarted", func() bool {
		tw.mu.Lock()
		defer tw.mu.Unlock()
		return tw.resyncs[playerEntityID(2)] > 0
	})
	tw.waitHolds(worldBaseSID, 1, 2, "the teammate back after its base restarted", func(s EntityState) bool { return s.X == 17000 })
	tw.waitHolds(teamBase2, 2, 1, "the teammate seen from the restarted base", func(s EntityState) bool { return s.X == 2000 })
}

// 队员换格（跨 Cell 交接）：面板里的它跟着走过边界，不被删掉重建，位置一直更新。
func TestGroupPanelFollowsAHandoff(t *testing.T) {
	tw := startTeamWorld(t, nil)
	tw.enterOn(worldBaseSID, 1, spatial.Point{X: 2000, Y: 5000})
	tw.enterOn(teamBase2, 2, spatial.Point{X: 10400, Y: 5000})
	g := Group{ID: teamID, Ver: 1, Members: []GroupMember{teamMember(1, worldBaseSID), teamMember(2, teamBase2)}}
	tw.form(g)
	start := tw.waitHolds(worldBaseSID, 1, 2, "the teammate", func(s EntityState) bool { return s.X == 10400 })

	ctx, cancel := tw.ctx()
	defer cancel()
	if err := tw.bases[teamBase2].node.Move(ctx, playerEntityID(2), -1000, 0); err != nil {
		t.Fatal(err)
	}
	moved := tw.waitHolds(worldBaseSID, 1, 2, "the teammate past the border in the next cell", func(s EntityState) bool {
		return s.X < 9700 && s.Ver.Epoch > start.Ver.Epoch
	})
	if err := tw.bases[teamBase2].node.Move(ctx, playerEntityID(2), 0, 0); err != nil {
		t.Fatal(err)
	}
	if n := tw.bases[worldBaseSID].client.createCount(1, actorID(2)); n != 1 {
		t.Fatalf("player 2 was created %d times on player 1's client across the handoff, want 1", n)
	}
	if at, removed := tw.removalOf(worldBaseSID, 1, 2); removed {
		t.Fatalf("player 2 was removed from player 1's panel at %v during the handoff", at)
	}
	w := tw.world
	w.assertSingleReal(actorID(2), CellID{0, 0})
	t.Logf("panel followed the handoff: x %d -> %d, epoch %d -> %d", start.X, moved.X, start.Ver.Epoch, moved.Ver.Epoch)
}

// 成员变化经通知收敛：离队的人立刻从对方面板消失；迟到的旧通知不能把他加回来；解散后对方 Base 的缓存清空、面板清空。
func TestGroupNoticesChangeTheRoster(t *testing.T) {
	tw := startTeamWorld(t, nil)
	tw.enterOn(worldBaseSID, 1, spatial.Point{X: 2000, Y: 5000})
	tw.enterOn(teamBase2, 2, spatial.Point{X: 17000, Y: 5000})
	m1, m2 := teamMember(1, worldBaseSID), teamMember(2, teamBase2)
	g1 := Group{ID: teamID, Ver: 1, Members: []GroupMember{m1, m2}}
	tw.form(g1)
	tw.waitHolds(worldBaseSID, 1, 2, "the teammate", func(s EntityState) bool { return s.X == 17000 })

	// 2 离队：它自己的事务清空自己的成员表并通知变化前后的全部成员。
	g2 := Group{ID: teamID, Ver: 2, Members: []GroupMember{m1}}
	tw.setAuthority(g2)
	tw.setGroup(teamBase2, 2, testGroupCmd{set: true, group: Group{}, notice: &g2, former: []GroupMember{m2}})
	waitFor(t, 2*time.Second, "player 1 adopts version 2", func() bool { return tw.groupOf(worldBaseSID, 1).Ver == 2 })
	if got := tw.groupOf(worldBaseSID, 1); len(got.Members) != 1 || got.Members[0] != m1 {
		t.Fatalf("player 1's roster after player 2 left: %+v, want only player 1", got)
	}
	left := time.Now()
	waitFor(t, 2*time.Second, "player 2 removed from player 1's panel", func() bool {
		_, ok := tw.removalOf(worldBaseSID, 1, 2)
		return ok
	})
	if at, _ := tw.removalOf(worldBaseSID, 1, 2); at.Sub(left) >= teamHold {
		t.Fatalf("player 2 left the panel %v after the notice, want well before the %v expiry", at.Sub(left), teamHold)
	}

	// 迟到 / 重复的版本 1 通知：版本不更新，不采纳。
	tw.setGroup(teamBase2, 2, testGroupCmd{notice: &g1})
	time.Sleep(3 * teamEvery)
	if got := tw.groupOf(worldBaseSID, 1); got.Ver != 2 {
		t.Fatalf("a late version-1 notice moved player 1's roster to %+v", got)
	}
	if _, ok := tw.holds(worldBaseSID, 1, 2); ok {
		t.Fatal("a late version-1 notice put player 2 back in player 1's panel")
	}

	// 2 重新入队（版本 3），然后 1 解散（版本 4，成员表为空）：2 所在 Base 的缓存清空，2 的面板里不再有 1。
	g3 := Group{ID: teamID, Ver: 3, Members: []GroupMember{m1, m2}}
	tw.setAuthority(g3)
	tw.setGroup(teamBase2, 2, testGroupCmd{set: true, group: g3, notice: &g3})
	tw.waitHolds(worldBaseSID, 1, 2, "the teammate back in the team", func(s EntityState) bool { return true })
	tw.waitHolds(teamBase2, 2, 1, "the teammate back in the team", func(s EntityState) bool { return true })
	g4 := Group{ID: teamID, Ver: 4}
	tw.setAuthority(g4)
	tw.setGroup(worldBaseSID, 1, testGroupCmd{set: true, group: Group{}, notice: &g4, former: []GroupMember{m1, m2}})
	waitFor(t, 2*time.Second, "player 2's roster cleared by the disband", func() bool { return tw.groupOf(teamBase2, 2).ID == 0 })
	waitFor(t, 2*time.Second, "player 1 removed from player 2's panel", func() bool {
		_, ok := tw.holds(teamBase2, 2, 1)
		return !ok
	})
}

// 通知丢了：缓存停在旧版本，GroupResync 按周期从权威重读，面板仍然收敛（离队的人移除）。
func TestGroupResyncCoversALostNotice(t *testing.T) {
	tw := startTeamWorld(t, func(cfg *NodeConfig) { cfg.GroupResyncEvery = time.Second })
	tw.enterOn(worldBaseSID, 1, spatial.Point{X: 2000, Y: 5000})
	tw.enterOn(teamBase2, 2, spatial.Point{X: 17000, Y: 5000})
	m1, m2 := teamMember(1, worldBaseSID), teamMember(2, teamBase2)
	g1 := Group{ID: teamID, Ver: 1, Members: []GroupMember{m1, m2}}
	tw.form(g1)
	tw.waitHolds(worldBaseSID, 1, 2, "the teammate", func(s EntityState) bool { return s.X == 17000 })

	g2 := Group{ID: teamID, Ver: 2, Members: []GroupMember{m1}}
	tw.mu.Lock()
	tw.dropNote = true
	tw.authority[teamID] = g2
	before := tw.resyncs[playerEntityID(1)]
	tw.mu.Unlock()
	tw.setGroup(teamBase2, 2, testGroupCmd{set: true, group: Group{}, notice: &g2, former: []GroupMember{m2}})
	waitFor(t, 3*time.Second, "player 1 resynced to version 2", func() bool { return tw.groupOf(worldBaseSID, 1).Ver == 2 })
	tw.mu.Lock()
	after := tw.resyncs[playerEntityID(1)]
	dropped := len(tw.notices[worldBaseSID])
	tw.mu.Unlock()
	if after <= before || dropped == 0 {
		t.Fatalf("resyncs of player 1 %d -> %d, notices dropped on the way to base %d: %d; want the change to come from a resync", before, after, worldBaseSID, dropped)
	}
	waitFor(t, 2*time.Second, "player 2 removed from player 1's panel", func() bool {
		_, ok := tw.holds(worldBaseSID, 1, 2)
		return !ok
	})
}

// 在视野里的队员：空间视野与组视野是同一个 subject，客户端只持有一份；走出视野后由快照接着供，不删不重建。
func TestGroupPanelMergesWithTheSpatialView(t *testing.T) {
	tw := startTeamWorld(t, nil)
	tw.enterOn(worldBaseSID, 1, spatial.Point{X: 5000, Y: 5000})
	tw.enterOn(teamBase2, 2, spatial.Point{X: 5300, Y: 5000})
	tw.waitHolds(worldBaseSID, 1, 2, "the neighbour in view before the team", func(s EntityState) bool { return true })
	g := Group{ID: teamID, Ver: 1, Members: []GroupMember{teamMember(1, worldBaseSID), teamMember(2, teamBase2)}}
	tw.form(g)
	time.Sleep(3 * teamEvery)
	ctx, cancel := tw.ctx()
	defer cancel()
	if err := tw.bases[teamBase2].node.Move(ctx, playerEntityID(2), 1000, 0); err != nil {
		t.Fatal(err)
	}
	tw.waitHolds(worldBaseSID, 1, 2, "the teammate walked out of view", func(s EntityState) bool { return s.X > 7000 })
	if err := tw.bases[teamBase2].node.Move(ctx, playerEntityID(2), 0, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * teamEvery)
	if _, ok := tw.holds(worldBaseSID, 1, 2); !ok {
		t.Fatal("the teammate left player 1's panel after walking out of view")
	}
	if n := tw.bases[worldBaseSID].client.createCount(1, actorID(2)); n != 1 {
		t.Fatalf("player 2 was created %d times on player 1's client (in view, then out of view through the team), want 1", n)
	}
	if at, removed := tw.removalOf(worldBaseSID, 1, 2); removed {
		t.Fatalf("player 2 was removed from player 1's client at %v", at)
	}
}
