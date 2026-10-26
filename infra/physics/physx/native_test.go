//go:build physx && cgo

package physx

import (
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"math"
	"sync"
	"testing"
)

func TestBallFallsAndContactsGround(t *testing.T) {
	w, e := New(-9.81, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	for _, b := range []Body{{ID: 100, Shape: Box, Position: Vec3{0, -0.5, 0}, Dimensions: Vec3{25, 0.5, 25}}, {ID: 1, Shape: Sphere, Position: Vec3{0, 8, 0}, Dimensions: Vec3{2}, Mass: 80}} {
		if e = w.Add(b); e != nil {
			t.Fatal(e)
		}
	}
	var states []State
	for range 250 {
		states, e = w.Step(.02, nil)
		if e != nil {
			t.Fatal(e)
		}
	}
	if len(states) != 1 || math.Abs(float64(states[0].Position[1]-2)) > .05 {
		t.Fatalf("ground contact: %+v", states)
	}
	w.Close()
	if _, e = w.Step(.02, nil); e != ErrClosed {
		t.Fatal(e)
	}
}
func TestWorldLifetimeAndInputValidation(t *testing.T) {
	a, e := New(-9.81, 1)
	if e != nil {
		t.Fatal(e)
	}
	b, e := New(-9.81, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	a.Close()
	if e = b.Add(Body{ID: 1, Shape: Sphere, Position: Vec3{0, 2, 0}, Dimensions: Vec3{1}, Mass: 1}); e != nil {
		t.Fatal(e)
	}
	if _, e = b.Step(.02, []Command{{ID: 999}}); e == nil {
		t.Fatal("unknown command")
	}
	if _, e = b.Step(.02, nil); e != nil {
		t.Fatal(e)
	}
}

func TestCapsulePushesDynamicBall(t *testing.T) {
	w, e := New(-9.81, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	for _, b := range []Body{
		{ID: 100, Shape: Box, Position: Vec3{0, -.5, 0}, Dimensions: Vec3{25, .5, 25}},
		{ID: 1, Shape: Capsule, Position: Vec3{-3, 1.1, 0}, Dimensions: Vec3{.4, .6, 0}, Mass: 10},
		{ID: 2, Shape: Sphere, Position: Vec3{0, 1.1, 0}, Dimensions: Vec3{1}, Mass: 20},
	} {
		if e = w.Add(b); e != nil {
			t.Fatal(e)
		}
	}
	var states []State
	for range 100 {
		states, e = w.Step(.02, []Command{{ID: 1, Acceleration: Vec3{10, 0, 0}}})
		if e != nil {
			t.Fatal(e)
		}
	}
	if len(states) != 2 || states[1].Position[0] < .5 || states[1].Position[1] < .9 {
		t.Fatalf("capsule failed to push ball: %+v", states)
	}
}

func TestBusinessWorkerCannotStepNativeWorld(t *testing.T) {
	w, err := New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	_, release := fctx.NewContext(fctx.WithShortWorker())
	_, err = w.Step(.02, nil)
	release()
	if !errors.Is(err, fctx.ErrBlockingInBusinessWorker) {
		t.Fatalf("native step must fail before simulation: %v", err)
	}
}

// 场景和刚体的 CCD 标志不足以阻止穿透，接触对也必须启用 CCD。
func TestCCDStopsFastBallAtThinWall(t *testing.T) {
	w, err := New(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, b := range []Body{
		{ID: 100, Shape: Box, Dimensions: Vec3{.05, 5, 5}},
		{ID: 1, Shape: Sphere, Position: Vec3{-2, 0, 0}, Dimensions: Vec3{.2}, Mass: 1},
	} {
		if err := w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	states, err := w.Step(.02, []Command{{ID: 1, Acceleration: Vec3{10000, 0, 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Position[0] > -.15 {
		t.Fatalf("ball crossed wall: %+v", states[0])
	}
}

// 禁转胶囊落地的摩擦冲量不应被分配给最终又被丢弃的转动速度。
func TestLockedCapsuleContactFriction(t *testing.T) {
	w, err := New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, b := range []Body{
		{ID: 100, Shape: Box, Position: Vec3{0, -.5, 0}, Dimensions: Vec3{25, .5, 25}},
		{ID: 1, Shape: Capsule, Position: Vec3{12, 1.1, 0}, Dimensions: Vec3{.4, .6}, Mass: 10},
	} {
		if err := w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	var states []State
	for range 7 {
		states, err = w.Step(.02, []Command{{ID: 1, Acceleration: Vec3{-8, 0, 0}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	v := states[0].Velocity
	// 首次接触仍在滑动：水平减速 = 摩擦系数 × 法向速度修正。
	expected := float32(-8*.02*7) + .6*(v[1]-float32(-9.81*.02*7))
	if math.Abs(float64(v[0]-expected)) > .001 {
		t.Fatalf("locked-body friction: velocity=%v expected vx=%f", v, expected)
	}
}

// 斜坡必须作为旋转的真实碰撞体进入 SDK，不能只旋转 Unity 里的外观。
func TestRotatedRampDeflectsFallingBall(t *testing.T) {
	w, err := New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	angle := math.Pi / 9
	for _, b := range []Body{
		{ID: 100, Shape: Box, Position: Vec3{0, -.5, 0}, Dimensions: Vec3{25, .5, 25}},
		{ID: 101, Shape: Box, Position: Vec3{0, 1.5, 0}, Dimensions: Vec3{4, .25, 2}, Rotation: Quat{0, 0, float32(math.Sin(angle / 2)), float32(math.Cos(angle / 2))}},
		{ID: 1, Shape: Sphere, Position: Vec3{0, 5, 0}, Dimensions: Vec3{.5}, Mass: 5},
	} {
		if err = w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	var states []State
	for range 150 {
		states, err = w.Step(.02, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if states[0].Position[0] > -1 || states[0].Position[1] < .45 {
		t.Fatalf("ramp did not deflect ball: %+v", states[0])
	}
}

// 多房间复用同一进程级 SDK；房间使用相同 body ID，也不能共享位置、输入或关闭状态。
func TestConcurrentRoomLifecycles(t *testing.T) {
	const rooms, generations = 8, 20
	var wg sync.WaitGroup
	failures := make(chan error, rooms)
	start := make(chan struct{})
	for room := range rooms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for generation := range generations {
				if err := exerciseRoom(room); err != nil {
					failures <- fmt.Errorf("room=%d generation=%d: %w", room, generation, err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

func exerciseRoom(room int) error {
	w, err := New(0, 1)
	if err != nil {
		return err
	}
	defer w.Close()
	origin := float32(room * 100)
	if err = w.Add(Body{ID: 1, Shape: Sphere, Position: Vec3{origin, 5, 0}, Dimensions: Vec3{.5}, Mass: 1}); err != nil {
		return err
	}
	acceleration := float32(room + 1)
	var states []State
	for range 50 {
		states, err = w.Step(.02, []Command{{ID: 1, Acceleration: Vec3{acceleration, 0, 0}}})
		if err != nil {
			return err
		}
	}
	// 50 次半隐式积分：x = a * dt² * (1+...+50)，无重力、碰撞或线性阻尼。
	expected := origin + acceleration*.0004*1275
	if len(states) != 1 || states[0].ID != 1 || math.Abs(float64(states[0].Position[0]-expected)) > .01 || states[0].Position[1] != 5 {
		return fmt.Errorf("world state leaked or input lost: got=%+v expected x=%f", states, expected)
	}
	w.Close()
	if _, err = w.Step(.02, nil); !errors.Is(err, ErrClosed) {
		return fmt.Errorf("closed room accepted step: %v", err)
	}
	return nil
}

// 跳跃只能由落地判定放行；空中重复请求不能再次增加竖直速度。
func TestJumpRequiresGround(t *testing.T) {
	w, err := New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, b := range []Body{{ID: 100, Shape: Box, Position: Vec3{0, -.5, 0}, Dimensions: Vec3{10, .5, 10}}, {ID: 1, Shape: Sphere, Position: Vec3{0, .5, 0}, Dimensions: Vec3{.5}, Mass: 1}} {
		if err := w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	for range 50 {
		if _, err = w.Step(.02, nil); err != nil {
			t.Fatal(err)
		}
	}
	states, err := w.Step(.02, []Command{{ID: 1, Jump: true}})
	if err != nil {
		t.Fatal(err)
	}
	if states[0].Velocity[1] < 6 {
		t.Fatalf("ground jump rejected: %+v", states)
	}
	peak := states[0].Position[1]
	// 空中包括接近最高点的低速阶段，持续请求也不得二段跳。
	for range 45 {
		states, err = w.Step(.02, []Command{{ID: 1, Jump: true}})
		if err != nil {
			t.Fatal(err)
		}
		peak = max(peak, states[0].Position[1])
	}
	if peak < 2 || peak > 3.2 || states[0].Velocity[1] >= 0 {
		t.Fatalf("air jump or no jump: peak=%f state=%+v", peak, states)
	}
}

func TestKinematicPlatformCarriesAndRotorCollides(t *testing.T) {
	w, err := New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, b := range []Body{
		{ID: 1, Shape: Box, Motion: Kinematic, Position: Vec3{0, 0, 0}, Dimensions: Vec3{2, .25, 2}, Mass: 10},
		{ID: 2, Shape: Sphere, Position: Vec3{0, .8, 0}, Dimensions: Vec3{.5}, Mass: 1},
		{ID: 3, Shape: Box, Motion: Kinematic, Position: Vec3{10, 1, 0}, Dimensions: Vec3{3, .2, .2}, Mass: 10},
		{ID: 4, Shape: Sphere, Position: Vec3{12, .7, -1}, Dimensions: Vec3{.5}, Mass: 1},
		{ID: 100, Shape: Box, Position: Vec3{10, -.5, 0}, Dimensions: Vec3{6, .5, 6}},
	} {
		if err = w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	var states []State
	for i := 1; i <= 100; i++ {
		a := float64(i) * .01
		states, err = w.Step(.02, []Command{{ID: 1, Target: true, Position: Vec3{0, float32(i) * .04, 0}, Rotation: Quat{0, 0, 0, 1}}, {ID: 3, Target: true, Position: Vec3{10, 1, 0}, Rotation: Quat{0, float32(math.Sin(a)), 0, float32(math.Cos(a))}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	m := map[uint64]State{}
	for _, v := range states {
		m[v.ID] = v
	}
	if m[2].Position[1] < 4.65 {
		t.Fatalf("platform failed to carry: %+v", m[2])
	}
	if math.Abs(float64(m[4].Position[0]-12))+math.Abs(float64(m[4].Position[2]+1)) < .5 {
		t.Fatalf("rotor did not collide: %+v", m[4])
	}
	states, err = w.Step(.02, []Command{{ID: 1, Target: true, Position: Vec3{0, 4.04, 0}, Rotation: Quat{0, 0, 0, 1}}, {ID: 2, Jump: true}})
	if err != nil {
		t.Fatal(err)
	}
	if states[1].Velocity[1] < 7 {
		t.Fatalf("cannot jump off rising platform: %+v", states[1])
	}
	if _, err = w.Step(.02, []Command{{ID: 1, Acceleration: Vec3{1, 0, 0}}}); err == nil {
		t.Fatal("force accepted on kinematic")
	}
}
func TestDistanceGrabReleaseAndHingeMotor(t *testing.T) {
	w, err := New(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, b := range []Body{
		{ID: 1, Shape: Sphere, Position: Vec3{0, 0, 0}, Dimensions: Vec3{.2}, Mass: 1},
		{ID: 2, Shape: Sphere, Position: Vec3{2, 0, 0}, Dimensions: Vec3{.2}, Mass: 1},
		{ID: 3, Shape: Box, Motion: Dynamic, Position: Vec3{100, 0, 0}, Dimensions: Vec3{.2, 2, .2}, Mass: 10},
	} {
		if err = w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.AddJoint(Joint{ID: 1, A: 1, B: 2, MaxDistance: 1, Stiffness: 100, Damping: 10, BreakForce: 10000}); err != nil {
		t.Fatal(err)
	}
	if err = w.AddJoint(Joint{ID: 2, A: 3, Kind: Hinge, AnchorB: Vec3{100, 0, 0}, MotorSpeed: 2, MotorForce: 1000, BreakForce: 10000}); err != nil {
		t.Fatal(err)
	}
	var states []State
	for range 100 {
		states, err = w.Step(.02, []Command{{ID: 2, Acceleration: Vec3{5, 0, 0}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if states[1].Position[0]-states[0].Position[0] > 1.2 {
		t.Fatal("grab did not constrain", states)
	}
	if math.Abs(float64(states[2].AngularVelocity[0])) < 1 {
		t.Fatal("motor did not rotate", states[2])
	}
	if err = w.RemoveJoint(1); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		states, err = w.Step(.02, []Command{{ID: 2, Acceleration: Vec3{5, 0, 0}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if states[1].Position[0]-states[0].Position[0] < 3 {
		t.Fatal("release still constrained", states)
	}
	if err = w.AddJoint(Joint{ID: 3, A: 999, BreakForce: 1}); err == nil {
		t.Fatal("unknown actor accepted")
	}
}
