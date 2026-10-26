package physics

import (
	"encoding/json"
	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
	"math"
	"os"
	"testing"
)

func TestMotionPlaybackGolden(t *testing.T) {
	raw, err := os.ReadFile("../../client/physics-viewer/tests/motion.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Samples []struct {
			Tick     uint64
			Position physx.Vec3
			Rotation physx.Quat
		}
	}
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	bodies := []physx.Body{{ID: 1, Position: physx.Vec3{2, 1, -3}, Rotation: physx.Quat{0, 0, 0, 1}}}
	tracks := []MotionTrack{{ID: 1, Translation: physx.Vec3{0, 2, 0}, PeriodSeconds: 4, Axis: physx.Vec3{0, 1, 0}, AngularSpeed: 1}}
	for _, sample := range golden.Samples {
		cmd := MotionCommands(bodies, tracks, sample.Tick)[0]
		for i := range 3 {
			if math.Abs(float64(cmd.Position[i]-sample.Position[i])) > 1e-5 {
				t.Fatal("position golden", sample.Tick)
			}
		}
		for i := range 4 {
			if math.Abs(float64(cmd.Rotation[i]-sample.Rotation[i])) > 1e-5 {
				t.Fatal("rotation golden", sample.Tick)
			}
		}
	}
}
