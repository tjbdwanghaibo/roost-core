package movement

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/gameplay/heartbeat"
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
// 场景里不能开始移动。离开时按配置落库一次，周期落库只在位置变过时写。
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
