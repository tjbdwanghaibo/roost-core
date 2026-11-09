package movement

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
	"github.com/tjbdwanghaibo/roost-core/gameplay/terrain"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 这些用例直接调用 Move 的推进接口与 MapBasic 的周期落库回调（不经 Nest、不在事务里：AOI 操作立即生效），一拍的
// Elapsed 由用例给出，结果只按位置序列、结论与登记状态断言。经真实 Nest 与心跳驱动（动作挂心跳）的用例见
// nest_test.go。

var errQueueFull = fmt.Errorf("interest: %w", policy.ErrInterestQueueFull)

func newUnitDriver() *heartbeat.Driver {
	d := heartbeat.NewDriver(heartbeat.Config{Name: "movement-unit", Logger: discard})
	d.SetClock(newFakeClock().Now)
	return d
}

func resultText(r Result) string { return fmt.Sprintf("%s %d,%d", r.Reason, r.At.X, r.At.Y) }

// Advance 按速度与 Elapsed 推进：10 格/秒、每拍 100ms 走一格；每次换格经 QueueMove 进 AOI、写朝向；走完返回
// Arrived，Move 回到空闲；Move 自己不挂任何心跳。
func TestAdvanceMovesBySpeedAndElapsedThenArrives(t *testing.T) {
	d := newUnitDriver()
	e := newMover(t, d, 1, nil, moverOptions{})
	space := &spaceLog{}
	if err := e.mb.Enter(7, space, spatial.Point{X: 0, Y: 0}); err != nil {
		t.Fatal(err)
	}
	token, err := e.move.MoveTo([]spatial.Point{{X: 5, Y: 0}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for range 5 {
		results = append(results, resultText(e.move.Advance(token, 100*time.Millisecond)))
	}
	want := []string{"enter 0,0", "move 1,0", "move 2,0", "move 3,0", "move 4,0", "move 5,0"}
	if got := space.take(); !slices.Equal(got, want) {
		t.Fatalf("interest saw %v, want %v", got, want)
	}
	if !slices.Equal(results, []string{"moving 1,0", "moving 2,0", "moving 3,0", "moving 4,0", "arrived 5,0"}) {
		t.Fatalf("results %v", results)
	}
	if e.move.Moving() || d.Registered() != 0 {
		t.Fatalf("after arriving: moving %v, registered %d (the move component has no heartbeat of its own)", e.move.Moving(), d.Registered())
	}
	if e.mb.Facing() != 0 || e.mb.Published().Pos != (spatial.Point{X: 5}) {
		t.Fatalf("facing %d published %+v", e.mb.Facing(), e.mb.Published())
	}
}

// 不追帧：掉了两拍，下一拍的 Elapsed 是 300ms，一次走三格、只入队一次（不按拍补三次）。每拍不足一格的位移累积在
// 亚格位置里，不会被取整丢掉。
func TestSkippedBeatsAreCoveredByOneElapsedAndFractionsAccumulate(t *testing.T) {
	d := newUnitDriver()
	e := newMover(t, d, 2, nil, moverOptions{})
	space := &spaceLog{}
	if err := e.mb.Enter(7, space, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	token, err := e.move.MoveTo([]spatial.Point{{X: 0, Y: 20}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	space.take()
	e.move.Advance(token, 300*time.Millisecond)
	if got := space.take(); !slices.Equal(got, []string{"move 0,3"}) {
		t.Fatalf("a 300ms beat queued %v, want one move to 0,3", got)
	}
	if e.mb.Facing() != 9000 {
		t.Fatalf("facing %d, want 9000 (+Y)", e.mb.Facing())
	}

	// 3 格/秒、每拍 0.3 格：第 2 拍累计 0.6 → 第一格，第 5 拍 1.5 → 第二格。
	slow := newMover(t, d, 3, nil, moverOptions{})
	if err := slow.mb.Enter(7, &spaceLog{}, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	slowToken, err := slow.move.MoveTo([]spatial.Point{{X: 10, Y: 0}}, 3)
	if err != nil {
		t.Fatal(err)
	}
	var xs []int64
	for range 5 {
		slow.move.Advance(slowToken, 100*time.Millisecond)
		xs = append(xs, slow.mb.Pos().X)
	}
	if !slices.Equal(xs, []int64{0, 1, 1, 1, 2}) {
		t.Fatalf("positions at 0.3 cells per beat: %v, want [0 1 1 1 2]", xs)
	}
}

// 取代：新的 MoveTo 取代进行中的那个，旧编号的 Advance 得到 Replaced、什么都不改；之后按新目标走。Stop 之后同样
// Replaced。
func TestANewMoveReplacesTheOldOne(t *testing.T) {
	d := newUnitDriver()
	e := newMover(t, d, 4, nil, moverOptions{})
	if err := e.mb.Enter(7, &spaceLog{}, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	first, _ := e.move.MoveTo([]spatial.Point{{X: 10, Y: 0}}, 10)
	e.move.Advance(first, 200*time.Millisecond)
	second, _ := e.move.MoveTo([]spatial.Point{{X: 2, Y: 3}}, 10)
	if r := e.move.Advance(first, time.Second); r.Reason != Replaced || e.mb.Pos() != (spatial.Point{X: 2}) {
		t.Fatalf("advancing the replaced move: %s, pos %+v", resultText(r), e.mb.Pos())
	}
	if r := e.move.Advance(second, time.Second); r.Reason != Arrived || r.At != (spatial.Point{X: 2, Y: 3}) {
		t.Fatalf("second move: %s", resultText(r))
	}
	third, _ := e.move.MoveTo([]spatial.Point{{X: 9, Y: 9}}, 10)
	if !e.move.Stop(third) || e.move.Moving() || e.move.Advance(third, time.Second).Reason != Replaced {
		t.Fatalf("stop: moving %v", e.move.Moving())
	}
}

// 被阻挡：下一格被地形挡住，停在最后一个可走的格，返回 Blocked。
func TestMoveToStopsWhereTheTerrainBlocks(t *testing.T) {
	d := newUnitDriver()
	terrain, err := spatial.NewGridTerrain(100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := terrain.SetBlocked(spatial.Point{X: 3, Y: 0}, true); err != nil {
		t.Fatal(err)
	}
	e := newMover(t, d, 5, nil, moverOptions{terrain: terrain})
	if err := e.mb.Enter(7, &spaceLog{}, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	token, _ := e.move.MoveTo([]spatial.Point{{X: 6, Y: 0}}, 10)
	if r := e.move.Advance(token, time.Second); resultText(r) != "blocked 2,0" || e.move.Moving() {
		t.Fatalf("blocked move: %s moving %v", resultText(r), e.move.Moving())
	}
}

// stripSurface 是测试用的带高度地面：地面格 10 单位一格（四舍五入归格），blocked 里的格不可进入，x 方向第 water 格
// 起是水（速度 500‰）；undefined 的格本身没有定义（Step(c, c) 不放行）。calls 记下每次 Step。
type stripSurface struct {
	blocked   map[int64]bool
	water     int64
	undefined map[int64]bool
	calls     []string
}

func (s *stripSurface) Cell(p spatial.Point) spatial.Point {
	return spatial.Point{X: floorDiv10(p.X + 5), Y: floorDiv10(p.Y + 5)}
}

func floorDiv10(v int64) int64 {
	if v < 0 {
		return -((-v + 9) / 10)
	}
	return v / 10
}

func (s *stripSurface) Step(from, to spatial.Point) (int32, bool) {
	s.calls = append(s.calls, fmt.Sprintf("%d→%d", from.X, to.X))
	if s.undefined[from.X] || s.blocked[to.X] && from != to {
		return 0, false
	}
	if s.water > 0 && to.X >= s.water {
		return 500, true
	}
	return 1000, true
}

// 带高度地面（Surface）：每进入一个新的地面格问一次 Step(上一格, 新格)，不放行时停在上一个采样点（Blocked）；
// 只在格变化时问，同一格里不重复问。
func TestMoveToStopsWhereTheSurfaceRefusesAStep(t *testing.T) {
	d := newUnitDriver()
	ground := &stripSurface{blocked: map[int64]bool{3: true}}
	e := newMover(t, d, 40, nil, moverOptions{surface: func() Surface { return ground }})
	if err := e.mb.Enter(7, &spaceLog{}, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	token, _ := e.move.MoveTo([]spatial.Point{{X: 60, Y: 0}}, 100)
	// 24.5 四舍五入是 25，归第 3 格：在 24 停下。
	if r := e.move.Advance(token, time.Second); resultText(r) != "blocked 24,0" || e.move.Moving() {
		t.Fatalf("surface-blocked move: %s moving %v", resultText(r), e.move.Moving())
	}
	if want := []string{"0→0", "0→1", "1→2", "2→3"}; !slices.Equal(ground.calls, want) {
		t.Fatalf("surface asked %v, want %v (once per new ground cell)", ground.calls, want)
	}
}

// 游泳：地面格的速度千分比折算路程预算，水里（500‰）同样的时间只走一半；出水后恢复。站立的格本身没有定义（地图外、
// 块没装入）时一步也不走、Blocked（不猜）。
func TestSurfaceSpeedScalesTheWalkAndAnUndefinedCellBlocks(t *testing.T) {
	d := newUnitDriver()
	ground := &stripSurface{water: 5}
	e := newMover(t, d, 41, nil, moverOptions{surface: func() Surface { return ground }})
	if err := e.mb.Enter(7, &spaceLog{}, spatial.Point{X: 50}); err != nil {
		t.Fatal(err)
	}
	token, _ := e.move.MoveTo([]spatial.Point{{X: 100, Y: 0}}, 10)
	if r := e.move.Advance(token, time.Second); r.At != (spatial.Point{X: 55}) || r.Done() {
		t.Fatalf("one second in water at 10/s and 500‰: %s, want 55,0 still moving", resultText(r))
	}
	// 从水里走回陆地：从 47 出发，前 0.5 秒在水里走 2.5 单位（过了 44.5 归第 4 格，陆地），之后 0.5 秒全速走 5 单位。
	shore := newMover(t, d, 45, nil, moverOptions{surface: func() Surface { return ground }})
	if err := shore.mb.Enter(7, &spaceLog{}, spatial.Point{X: 47}); err != nil {
		t.Fatal(err)
	}
	back, _ := shore.move.MoveTo([]spatial.Point{{X: 0, Y: 0}}, 10)
	if r := shore.move.Advance(back, time.Second); r.At.X < 39 || r.At.X > 40 {
		t.Fatalf("leaving the water: %s, want 39.5 rounded (half speed until x 44.5, then full speed)", resultText(r))
	}

	stuck := &stripSurface{undefined: map[int64]bool{2: true}}
	u := newMover(t, d, 42, nil, moverOptions{surface: func() Surface { return stuck }})
	if err := u.mb.Enter(7, &spaceLog{}, spatial.Point{X: 20}); err != nil {
		t.Fatal(err)
	}
	ut, _ := u.move.MoveTo([]spatial.Point{{X: 0, Y: 0}}, 10)
	if r := u.move.Advance(ut, time.Second); resultText(r) != "blocked 20,0" {
		t.Fatalf("standing on an undefined ground cell: %s, want blocked 20,0", resultText(r))
	}
}

// Surface 函数返回 nil（没装地形）时，推进与不设 Surface 逐位相同：同一组速度与拍长的位置序列一样，Terrain 照常挡。
func TestANilSurfaceWalksExactlyLikeNoSurface(t *testing.T) {
	d := newUnitDriver()
	terrain, err := spatial.NewGridTerrain(1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := terrain.SetBlocked(spatial.Point{X: 33, Y: 19}, true); err != nil {
		t.Fatal(err)
	}
	walk := func(unique int64, surface func() Surface) []string {
		e := newMover(t, d, unique, nil, moverOptions{terrain: terrain, surface: surface})
		if err := e.mb.Enter(7, &spaceLog{}, spatial.Point{X: 3, Y: 1}); err != nil {
			t.Fatal(err)
		}
		token, _ := e.move.MoveTo([]spatial.Point{{X: 20, Y: 9}, {X: 41, Y: 25}}, 7.3)
		var out []string
		for i := range 40 {
			r := e.move.Advance(token, time.Duration(70+i*13)*time.Millisecond)
			out = append(out, resultText(r))
			if r.Done() {
				break
			}
		}
		return out
	}
	plain, viaNil := walk(43, nil), walk(44, func() Surface { return nil })
	if !slices.Equal(plain, viaNil) {
		t.Fatalf("a nil surface changed the walk:\n plain %v\n nil   %v", plain, viaNil)
	}
	if last := plain[len(plain)-1]; !strings.HasPrefix(last, "blocked") {
		t.Fatalf("the grid wall did not stop the walk: %v", plain)
	}
}

// Follow：读目标最近一次发布的位置，走到距它 keep 格处停住但继续跟随；目标移动后接着走；目标销毁时 TargetLost；
// 目标离开本场景时 TargetLeft。
func TestFollowTracksTheTargetUntilItIsGone(t *testing.T) {
	d := newUnitDriver()
	targets := map[int64]*mover{}
	locate := LocateIn(func(id int64) entity.IThreadSafeEntity {
		if m := targets[id]; m != nil {
			return m
		}
		return nil
	})
	target := newMover(t, d, 10, nil, moverOptions{})
	targets[target.ID()] = target
	if err := target.mb.Enter(7, &spaceLog{}, spatial.Point{X: 10, Y: 0}); err != nil {
		t.Fatal(err)
	}
	follower := newMover(t, d, 11, nil, moverOptions{locate: locate})
	if err := follower.mb.Enter(7, &spaceLog{}, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	token, err := follower.move.Follow(target.ID(), 5, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	var last Result
	for range 4 {
		last = follower.move.Advance(token, time.Second)
	}
	if follower.mb.Pos() != (spatial.Point{X: 8}) || last.Done() {
		t.Fatalf("follower at %+v result %s, want 8,0 and still following", follower.mb.Pos(), resultText(last))
	}
	if err := target.mb.SetPos(spatial.Point{X: 20}); err != nil {
		t.Fatal(err)
	}
	follower.move.Advance(token, time.Second)
	if follower.mb.Pos() != (spatial.Point{X: 13}) {
		t.Fatalf("follower at %+v after the target moved, want 13,0", follower.mb.Pos())
	}
	target.OnDestroy(0)
	if r := follower.move.Advance(token, time.Second); resultText(r) != "target lost 13,0" || follower.move.Moving() {
		t.Fatalf("target destroyed: %s", resultText(r))
	}

	other := newMover(t, d, 12, nil, moverOptions{})
	targets[other.ID()] = other
	if err := other.mb.Enter(7, &spaceLog{}, spatial.Point{X: 30}); err != nil {
		t.Fatal(err)
	}
	token, _ = follower.move.Follow(other.ID(), 5, 0, false)
	if err := other.mb.Leave(); err != nil {
		t.Fatal(err)
	}
	if r := follower.move.Advance(token, time.Second); resultText(r) != "target left the scene 13,0" {
		t.Fatalf("target left: %s", resultText(r))
	}
}

// 进出场景：进场景 Enter（位置取当时的位置），离开 Leave；离开后进行中的移动在下一次推进时以 LeftScene 结束；不在
// 场景里不能开始移动。离开时按配置落库一次，周期落库只在场景、位置或朝向变过时写。
func TestEnterAndLeaveTheScene(t *testing.T) {
	d := newUnitDriver()
	var saved []string
	save := func(scene int64, at spatial.Point, _ Facing) error {
		saved = append(saved, fmt.Sprintf("%d@%d,%d", scene, at.X, at.Y))
		return nil
	}
	e := newMover(t, d, 20, nil, moverOptions{save: save, every: 5 * time.Second, onLeave: true})
	space := &spaceLog{}
	if _, err := e.move.MoveTo([]spatial.Point{{X: 1}}, 1); err != ErrNotInScene {
		t.Fatalf("move outside a scene: %v", err)
	}
	if err := e.mb.Enter(3, space, spatial.Point{X: 4, Y: 4}); err != nil {
		t.Fatal(err)
	}
	if err := e.mb.Enter(3, space, spatial.Point{X: 4, Y: 4}); err != ErrAlreadyInScene {
		t.Fatalf("second enter: %v", err)
	}
	if !e.mb.InScene() || e.mb.SceneID() != 3 || d.Registered() != 1 {
		t.Fatalf("in scene %v id %d registered %d (periodic save)", e.mb.InScene(), e.mb.SceneID(), d.Registered())
	}
	_ = e.mb.saveBeat(beatOf(5 * time.Second))
	_ = e.mb.saveBeat(beatOf(5 * time.Second))
	token, _ := e.move.MoveTo([]spatial.Point{{X: 10, Y: 4}}, 1)
	e.move.Advance(token, time.Second)
	if err := e.mb.Leave(); err != nil {
		t.Fatal(err)
	}
	if r := e.move.Advance(token, time.Second); resultText(r) != "left scene 5,4" {
		t.Fatalf("after leaving: %s", resultText(r))
	}
	if got := space.take(); !slices.Equal(got, []string{"enter 4,4", "move 5,4", "leave"}) {
		t.Fatalf("interest saw %v", got)
	}
	if !slices.Equal(saved, []string{"3@4,4", "3@5,4"}) {
		t.Fatalf("saved %v, want one periodic save and one on leave", saved)
	}
	if e.mb.InScene() || e.mb.SceneID() != 0 || e.mb.Pos() != (spatial.Point{X: 5, Y: 4}) || d.Registered() != 0 {
		t.Fatalf("after leave: in %v scene %d pos %+v registered %d", e.mb.InScene(), e.mb.SceneID(), e.mb.Pos(), d.Registered())
	}
	if err := e.mb.Leave(); err != ErrNotInScene {
		t.Fatalf("second leave: %v", err)
	}
}

// AOI 排队满（背压）时整拍作废、停在原处继续移动；其他拒绝以 Refused 结束，位置不动。
func TestRefusedPositionHoldsOnBackpressureAndEndsOtherwise(t *testing.T) {
	d := newUnitDriver()
	e := newMover(t, d, 30, nil, moverOptions{})
	space := &spaceLog{}
	if err := e.mb.Enter(7, space, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	token, _ := e.move.MoveTo([]spatial.Point{{X: 10}}, 10)
	space.refuseAll = errQueueFull
	if r := e.move.Advance(token, 300*time.Millisecond); r.Done() || e.mb.Pos() != (spatial.Point{}) || !e.move.Moving() {
		t.Fatalf("backpressure: %s pos %+v moving %v", resultText(r), e.mb.Pos(), e.move.Moving())
	}
	space.refuseAll = nil
	e.move.Advance(token, 100*time.Millisecond)
	if e.mb.Pos() != (spatial.Point{X: 1}) {
		t.Fatalf("after backpressure the move did not resume from where it was: %+v", e.mb.Pos())
	}
	space.refuseAll = errBusiness
	if r := e.move.Advance(token, 100*time.Millisecond); resultText(r) != "position refused 1,0" || e.mb.Pos() != (spatial.Point{X: 1}) {
		t.Fatalf("refused: %s pos %+v", resultText(r), e.mb.Pos())
	}
}

// flatWalker 是 chunks × chunks 块、世界高度 0（原始值 1000、基准 −100 米）的平地上的 Walker，raw 里的采样点改成给定
// 原始值。
func flatWalker(t testing.TB, chunks int32, raw map[[2]int64]uint16) *terrain.Walker {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flat.pack")
	meta := terrain.Meta{BaseRevision: 1, ChunksX: chunks, ChunksY: chunks, HeightBase: -10000, WaterLevel: -50}
	err := terrain.WritePackFile(path, meta, func(key terrain.ChunkKey) (terrain.Chunk, error) {
		c := terrain.NewChunk()
		for i := range c.Heights {
			gx := int64(key.X)*terrain.ChunkSamples + int64(i%terrain.ChunkSamples)
			gy := int64(key.Y)*terrain.ChunkSamples + int64(i/terrain.ChunkSamples)
			c.Heights[i] = 1000
			if h, ok := raw[[2]int64{gx, gy}]; ok {
				c.Heights[i] = h
			}
		}
		return c, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pack, err := terrain.OpenPack(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pack.Close() })
	view := terrain.NewView(pack)
	for y := range chunks {
		for x := range chunks {
			if err := view.LoadBase(terrain.ChunkKey{X: x, Y: y}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &terrain.Walker{View: view, Rules: terrain.DefaultWalkRules}
}

// 真实地形上的 Move（gameplay/terrain 的 Walker 作 Surface）：2 米高的坎挡住、停在前一个节点的最后一个采样点；
// 改块（SetOverlay 换版本）之后的下一拍按新地形判定——Surface 每拍取一次、Walker 每次读视图当前的块，移动不缓存地形。
func TestMoveOnATerrainWalkerFollowsTheLatestChunk(t *testing.T) {
	d := newUnitDriver()
	w := flatWalker(t, 1, map[[2]int64]uint16{{5, 2}: 1020})
	e := newMover(t, d, 46, nil, moverOptions{surface: func() Surface { return w }})
	if err := e.mb.Enter(7, &spaceLog{}, spatial.Point{X: 200, Y: 200}); err != nil {
		t.Fatal(err)
	}
	token, _ := e.move.MoveTo([]spatial.Point{{X: 800, Y: 200}}, 100)
	if r := e.move.Advance(token, 5*time.Second); resultText(r) != "blocked 449,200" {
		t.Fatalf("into a 2m wall: %s, want blocked at 449 (node 4's last point)", resultText(r))
	}
	// 把坎削平：块换一个版本，同一个 Walker 下一拍就按新数据走。
	chunk, version, _ := w.View.Chunk(terrain.ChunkKey{})
	chunk.Heights[2*terrain.ChunkSamples+5] = 1000
	if err := w.View.SetOverlay(terrain.ChunkKey{}, terrain.Overlay{BaseRevision: 1, Version: version + 1, Heights: chunk.Heights, Materials: chunk.Materials}); err != nil {
		t.Fatal(err)
	}
	token, _ = e.move.MoveTo([]spatial.Point{{X: 800, Y: 200}}, 100)
	if r := e.move.Advance(token, 5*time.Second); resultText(r) != "arrived 800,200" {
		t.Fatalf("after the wall was levelled: %s", resultText(r))
	}
}

// nopSpace 是不记录的 Space（基准用：spaceLog 会一直攒字符串）。
type nopSpace struct{}

func (nopSpace) Enter(entity.IThreadSafeEntity, spatial.Point) error     { return nil }
func (nopSpace) Leave(entity.IThreadSafeEntity) error                    { return nil }
func (nopSpace) QueueMove(entity.IThreadSafeEntity, spatial.Point) error { return nil }

// BenchmarkMoveAdvance 是一拍移动（5Hz、60 单位 / 秒，一拍 12 单位）的耗时。grid 是 P3 之前 demo 的接法（二维格子
// Terrain，每 0.5 单位采样、每进一格问一次；Surface 返回 nil），grid+walker 是开了地形之后（另外每进一个 1m 节点问一次
// Walker.Step）；none / walker 是不带格子的对照。
func BenchmarkMoveAdvance(b *testing.B) {
	w := flatWalker(b, 8, nil)
	grid, err := spatial.NewGridTerrain(51200, 51200)
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		terrain spatial.Terrain
		surface func() Surface
	}{
		{"none", nil, func() Surface { return nil }},
		{"walker", nil, func() Surface { return w }},
		{"grid", grid, func() Surface { return nil }},
		{"grid+walker", grid, func() Surface { return w }},
	} {
		b.Run(c.name, func(b *testing.B) {
			d := newUnitDriver()
			e := newMover(b, d, 47, nil, moverOptions{terrain: c.terrain, surface: c.surface})
			start := spatial.Point{X: 1000, Y: 1000}
			if err := e.mb.Enter(7, nopSpace{}, start); err != nil {
				b.Fatal(err)
			}
			var token uint64
			b.ResetTimer()
			for i := range b.N {
				if i%2000 == 0 { // 2000 拍走 24000 单位，留在 8 块（51200）里
					b.StopTimer()
					if err := e.mb.SetPos(start); err != nil {
						b.Fatal(err)
					}
					token, _ = e.move.MoveTo([]spatial.Point{{X: 50000, Y: 20000}}, 60)
					b.StartTimer()
				}
				if r := e.move.Advance(token, 200*time.Millisecond); r.Done() {
					b.Fatalf("beat %d: %s", i, resultText(r))
				}
			}
		})
	}
}
