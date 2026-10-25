//go:build physx && cgo

package physx

import (
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
	"math"
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
