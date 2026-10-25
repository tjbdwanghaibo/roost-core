//go:build physx && cgo

package physics

import (
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	"math"
	"testing"
	"time"
)

func TestRunUsesBusinessInputAndStopsOnCommitFailure(t *testing.T) {
	w, err := physx.New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err = w.Add(physx.Body{ID: 99, Shape: physx.Sphere, Position: physx.Vec3{0, 5, 0}, Dimensions: physx.Vec3{1}, Mass: 1}); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("commit rejected")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	err = Run(ctx, w, RunConfig{Epoch: 9, Step: time.Millisecond}, func(_ context.Context, f Frame) error {
		calls++
		if f.Tick != 1 || f.Epoch != 9 || len(f.States) != 1 || f.States[0].ID != 99 {
			t.Fatalf("unexpected frame: %+v", f)
		}
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("must stop at failed commit: calls=%d err=%v", calls, err)
	}
}

// 固定初态、固定 ID/tick 输入带；验证同一构建的重复运行，不承诺跨引擎确定性。
func TestComparisonSceneRepeatable(t *testing.T) {
	run := func() [][]physx.State {
		w, err := physx.New(-9.81, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		for _, b := range ComparisonScene() {
			if err := w.Add(b); err != nil {
				t.Fatal(err)
			}
		}
		frames := make([][]physx.State, 600)
		for tick := range frames {
			frames[tick], err = w.Step(StepSeconds, Commands(uint64(tick+1)))
			if err != nil {
				t.Fatal(err)
			}
		}
		return frames
	}
	a, b := run(), run()
	for tick := range a {
		for i := range a[tick] {
			if a[tick][i] != b[tick][i] {
				t.Fatalf("tick=%d body=%d differs: %+v / %+v", tick+1, i, a[tick][i], b[tick][i])
			}
		}
	}
}

// 检查完整世界而非 AOI 子集，避免离开所有玩家视野的物体悄悄掉出场地。
func TestCrowdTerrainContainsMinuteOfMotion(t *testing.T) {
	w, err := physx.New(-9.81, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, b := range CrowdScene() {
		if err = w.Add(b); err != nil {
			t.Fatal(err)
		}
	}
	for tick := uint64(1); tick <= 3000; tick++ {
		commands := CrowdCommands(tick)
		for i := range CrowdPlayers {
			a := (float64(i) + .5) * 2 * math.Pi / CrowdPlayers
			x, z := float32(-math.Cos(a)), float32(-math.Sin(a))
			switch ((tick - 1) / 150) % 4 {
			case 1:
				x, z = -z, x
			case 2:
				x, z = -x, -z
			case 3:
				x, z = z, -x
			}
			gain := float32(8 + i%3*2)
			commands = append(commands, physx.Command{ID: uint64(CrowdCapsules + i + 1), Acceleration: physx.Vec3{x * gain, 0, z * gain}})
		}
		states, err := w.Step(StepSeconds, commands)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range states {
			if s.Position[1] < -.1 || math.Abs(float64(s.Position[0])) > 25.1 || math.Abs(float64(s.Position[2])) > 25.1 {
				t.Fatalf("escaped terrain: tick=%d body=%+v", tick, s)
			}
		}
	}
}

// 一个房间提交失败不能停止另一个房间；取消后先等 Run 退出再关闭原生世界。
func TestRoomFailureAndCancellationAreIsolated(t *testing.T) {
	failedWorld, err := physx.New(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer failedWorld.Close()
	healthyWorld, err := physx.New(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer healthyWorld.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	healthyCtx, stopHealthy := context.WithCancel(ctx)
	defer stopHealthy()
	failed := make(chan error, 1)
	healthy := make(chan error, 1)
	rejected := errors.New("room commit rejected")
	go func() {
		failed <- Run(ctx, failedWorld, RunConfig{Epoch: 1, Step: time.Millisecond}, func(context.Context, Frame) error { return rejected })
	}()
	ticks := 0
	go func() {
		healthy <- Run(healthyCtx, healthyWorld, RunConfig{Epoch: 2, Step: time.Millisecond}, func(_ context.Context, f Frame) error {
			ticks++
			if ticks == 10 {
				stopHealthy()
			}
			return nil
		})
	}()
	if err := <-failed; !errors.Is(err, rejected) {
		t.Fatalf("failed room: %v", err)
	}
	if err := <-healthy; !errors.Is(err, context.Canceled) || ticks < 10 {
		t.Fatalf("healthy room ticks=%d err=%v", ticks, err)
	}
}
