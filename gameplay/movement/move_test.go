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

// 这些用例直接调用心跳回调（不经 Nest、不在事务里：启停与 AOI 操作立即生效），一拍的 Elapsed 由用例给出，结果只按
// 位置序列、回调顺序与登记状态断言。经真实 Nest 与心跳驱动的用例见 nest_test.go。

func newUnitDriver() *heartbeat.Driver {
	d := heartbeat.NewDriver(heartbeat.Config{Name: "movement-unit", Logger: discard})
	d.SetClock(newFakeClock().Now)
	return d
}

var errQueueFull = fmt.Errorf("interest: %w", policy.ErrInterestQueueFull)

type doneLog struct {
	results []string
}

func (l *doneLog) done(token uint64, r Result) {
	l.results = append(l.results, fmt.Sprintf("%d %s %d,%d", token, r.Reason, r.At.X, r.At.Y))
}

// MoveTo 按速度与 Elapsed 推进：10 格/秒、每拍 100ms 走一格；每次换格经 QueueMove 进 AOI、写朝向；到达后回调一次
// Arrived、停心跳，驱动上不再有这个登记。
func TestMoveToAdvancesBySpeedAndElapsedThenArrivesAndStopsTheHeartbeat(t *testing.T) {
	d := newUnitDriver()
	e := newMover(t, d, 1, nil, moverOptions{})
	space := &spaceLog{}
	if err := e.mb.Enter(7, space, spatial.Point{X: 0, Y: 0}); err != nil {
		t.Fatal(err)
	}
	var log doneLog
	token, err := e.move.MoveTo([]spatial.Point{{X: 5, Y: 0}}, 10, log.done)
	if err != nil {
		t.Fatal(err)
	}
	if !e.move.Reg().Active() || d.Registered() != 1 {
		t.Fatalf("moving without a heartbeat: active %v registered %d", e.move.Reg().Active(), d.Registered())
	}
	for range 5 {
		if err := e.move.beat(beatOf(100 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"enter 0,0", "move 1,0", "move 2,0", "move 3,0", "move 4,0", "move 5,0"}
	if got := space.take(); !slices.Equal(got, want) {
		t.Fatalf("interest saw %v, want %v", got, want)
	}
	if fmt.Sprint(log.results) != fmt.Sprintf("[%d arrived 5,0]", token) {
		t.Fatalf("done = %v", log.results)
	}
	if e.move.Moving() || e.move.Reg().Active() || d.Registered() != 0 {
		t.Fatalf("after arriving: moving %v active %v registered %d", e.move.Moving(), e.move.Reg().Active(), d.Registered())
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
	if _, err := e.move.MoveTo([]spatial.Point{{X: 0, Y: 20}}, 10, nil); err != nil {
		t.Fatal(err)
	}
	space.take()
	if err := e.move.beat(beatOf(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if got := space.take(); !slices.Equal(got, []string{"move 0,3"}) {
		t.Fatalf("a 300ms beat queued %v, want one move to 0,3", got)
	}
	if e.mb.Facing() != 9000 {
		t.Fatalf("facing %d, want 9000 (+Y)", e.mb.Facing())
	}

	// 3 格/秒、每拍 0.3 格：第 2 拍累计 0.6 → 第一格，第 5 拍 1.5 → 第二格。
	slow := newMover(t, d, 3, nil, moverOptions{})
	slowSpace := &spaceLog{}
	if err := slow.mb.Enter(7, slowSpace, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	if _, err := slow.move.MoveTo([]spatial.Point{{X: 10, Y: 0}}, 3, nil); err != nil {
		t.Fatal(err)
	}
	var xs []int64
	for range 5 {
		_ = slow.move.beat(beatOf(100 * time.Millisecond))
		xs = append(xs, slow.mb.Pos().X)
	}
	if !slices.Equal(xs, []int64{0, 1, 1, 1, 2}) {
		t.Fatalf("positions at 0.3 cells per beat: %v, want [0 1 1 1 2]", xs)
	}
}

// 打断：新的 MoveTo 取代进行中的那个，旧的以 Replaced 回调一次，之后的拍朝新目标走；调用方 Stop 不回调，停心跳。
func TestNewMoveToReplacesTheOldOne(t *testing.T) {
	d := newUnitDriver()
	e := newMover(t, d, 4, nil, moverOptions{})
	if err := e.mb.Enter(7, &spaceLog{}, spatial.Point{}); err != nil {
		t.Fatal(err)
	}
	var log doneLog
	first, _ := e.move.MoveTo([]spatial.Point{{X: 10, Y: 0}}, 10, log.done)
	_ = e.move.beat(beatOf(200 * time.Millisecond))
	second, _ := e.move.MoveTo([]spatial.Point{{X: 2, Y: 3}}, 10, log.done)
	if fmt.Sprint(log.results) != fmt.Sprintf("[%d replaced 2,0]", first) {
		t.Fatalf("replaced move: done %v", log.results)
	}
	if e.move.Token() != second || !e.move.Reg().Active() {
		t.Fatalf("second move not running: token %d active %v", e.move.Token(), e.move.Reg().Active())
	}
	_ = e.move.beat(beatOf(time.Second))
	if e.mb.Pos() != (spatial.Point{X: 2, Y: 3}) || len(log.results) != 2 {
		t.Fatalf("pos %+v done %v", e.mb.Pos(), log.results)
	}
	third, _ := e.move.MoveTo([]spatial.Point{{X: 9, Y: 9}}, 10, log.done)
	if !e.move.Stop(third) || e.move.Reg().Active() || len(log.results) != 2 {
		t.Fatalf("stop: active %v done %v", e.move.Reg().Active(), log.results)
	}
}

// 被阻挡：下一格被地形挡住，停在最后一个可走的格，回调 Blocked、停心跳。
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
	var log doneLog
	token, _ := e.move.MoveTo([]spatial.Point{{X: 6, Y: 0}}, 10, log.done)
	_ = e.move.beat(beatOf(time.Second))
	if fmt.Sprint(log.results) != fmt.Sprintf("[%d blocked 2,0]", token) || e.move.Reg().Active() {
		t.Fatalf("blocked move: done %v active %v", log.results, e.move.Reg().Active())
	}
}

// Follow：读目标最近一次发布的位置，走到距它 keep 格处停住但继续跟随；目标移动后接着走；目标销毁时回调 TargetLost；
// 目标离开本场景时回调 TargetLeft。
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
	var log doneLog
	token, err := follower.move.Follow(target.ID(), 5, 2, false, log.done)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		_ = follower.move.beat(beatOf(time.Second))
	}
	if follower.mb.Pos() != (spatial.Point{X: 8}) || !follower.move.Moving() {
		t.Fatalf("follower at %+v moving %v, want 8,0 and still following", follower.mb.Pos(), follower.move.Moving())
	}
	if err := target.mb.SetPos(spatial.Point{X: 20}); err != nil {
		t.Fatal(err)
	}
	_ = follower.move.beat(beatOf(time.Second))
	if follower.mb.Pos() != (spatial.Point{X: 13}) {
		t.Fatalf("follower at %+v after the target moved, want 13,0", follower.mb.Pos())
	}
	target.OnDestroy(0)
	_ = follower.move.beat(beatOf(time.Second))
	if fmt.Sprint(log.results) != fmt.Sprintf("[%d target lost 13,0]", token) || follower.move.Reg().Active() {
		t.Fatalf("target destroyed: done %v active %v", log.results, follower.move.Reg().Active())
	}

	other := newMover(t, d, 12, nil, moverOptions{})
	targets[other.ID()] = other
	if err := other.mb.Enter(7, &spaceLog{}, spatial.Point{X: 30}); err != nil {
		t.Fatal(err)
	}
	token, _ = follower.move.Follow(other.ID(), 5, 0, false, log.done)
	if err := other.mb.Leave(); err != nil {
		t.Fatal(err)
	}
	_ = follower.move.beat(beatOf(time.Second))
	if last := log.results[len(log.results)-1]; last != fmt.Sprintf("%d target left the scene 13,0", token) {
		t.Fatalf("target left: done %v", log.results)
	}
}

// 进出场景：进场景 Enter（位置取当时的位置），离开 Leave；离开后进行中的移动在下一拍以 LeftScene 结束；不在场景里
// 不能开始移动。离开时按配置落库一次，周期落库只在位置变过时写。
func TestEnterAndLeaveTheScene(t *testing.T) {
	d := newUnitDriver()
	var saved []string
	save := func(scene int64, at spatial.Point, _ Facing) error {
		saved = append(saved, fmt.Sprintf("%d@%d,%d", scene, at.X, at.Y))
		return nil
	}
	e := newMover(t, d, 20, nil, moverOptions{save: save, every: 5 * time.Second, onLeave: true})
	space := &spaceLog{}
	if _, err := e.move.MoveTo([]spatial.Point{{X: 1}}, 1, nil); err != ErrNotInScene {
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
	var log doneLog
	token, _ := e.move.MoveTo([]spatial.Point{{X: 10, Y: 4}}, 1, log.done)
	_ = e.move.beat(beatOf(time.Second))
	if err := e.mb.Leave(); err != nil {
		t.Fatal(err)
	}
	_ = e.move.beat(beatOf(time.Second))
	if got := space.take(); !slices.Equal(got, []string{"enter 4,4", "move 5,4", "leave"}) {
		t.Fatalf("interest saw %v", got)
	}
	if fmt.Sprint(log.results) != fmt.Sprintf("[%d left scene 5,4]", token) {
		t.Fatalf("done %v", log.results)
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
	var log doneLog
	token, _ := e.move.MoveTo([]spatial.Point{{X: 10}}, 10, log.done)
	space.refuseAll = errQueueFull
	_ = e.move.beat(beatOf(300 * time.Millisecond))
	if e.mb.Pos() != (spatial.Point{}) || !e.move.Moving() || len(log.results) != 0 {
		t.Fatalf("backpressure: pos %+v moving %v done %v", e.mb.Pos(), e.move.Moving(), log.results)
	}
	space.refuseAll = nil
	_ = e.move.beat(beatOf(100 * time.Millisecond))
	if e.mb.Pos() != (spatial.Point{X: 1}) {
		t.Fatalf("after backpressure the move did not resume from where it was: %+v", e.mb.Pos())
	}
	space.refuseAll = errBusiness
	_ = e.move.beat(beatOf(100 * time.Millisecond))
	if e.mb.Pos() != (spatial.Point{X: 1}) || fmt.Sprint(log.results) != fmt.Sprintf("[%d position refused 1,0]", token) {
		t.Fatalf("refused: pos %+v done %v", e.mb.Pos(), log.results)
	}
}
