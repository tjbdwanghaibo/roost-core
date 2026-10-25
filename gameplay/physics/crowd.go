package physics

import (
	"math"

	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

const CrowdPlayers = 20
const CrowdCapsules = 50
const CrowdBallID uint64 = 71
const CrowdBodies = CrowdPlayers + CrowdCapsules + 1

// CrowdScene 延用同一围场。ID 1..50 是胶囊，51..70 是连接控制的小球，71 是大球。
func CrowdScene() []physx.Body {
	boxes := TerrainBoxes()
	bodies := terrainBodies(boxes)
	bodies = append(bodies, physx.Body{ID: CrowdBallID, Shape: physx.Sphere, Position: physx.Vec3{0, 2.1, 3}, Dimensions: physx.Vec3{2}, Mass: 80})
	for i := range CrowdCapsules {
		a := float64(i) * 2 * math.Pi / CrowdCapsules
		x, z := float32(10*math.Cos(a)), float32(10*math.Sin(a))
		bodies = append(bodies, physx.Body{ID: uint64(i + 1), Shape: physx.Capsule, Position: physx.Vec3{x, terrainHeight(boxes, x, z, .4) + 1.1, z}, Dimensions: physx.Vec3{.4, .6}, Mass: 10})
	}
	for i := range CrowdPlayers {
		a := (float64(i) + .5) * 2 * math.Pi / CrowdPlayers
		x, z := float32(18*math.Cos(a)), float32(18*math.Sin(a))
		if i < 6 {
			x, z = -15+float32(i%3)*2, -10+float32(i/3)*4
		} else if i < 12 {
			x, z = 9+float32((i-6)%3)*2, 6+float32((i-6)/3)*4
		}
		bodies = append(bodies, physx.Body{ID: uint64(CrowdCapsules + i + 1), Shape: physx.Sphere, Position: physx.Vec3{x, terrainHeight(boxes, x, z, .5) + .6, z}, Dimensions: physx.Vec3{.5}, Mass: 5})
	}
	return bodies
}

// CrowdCommands 仅生成 NPC 输入；小球输入必须来自各自的网络连接。
func CrowdCommands(tick uint64) []physx.Command {
	out := make([]physx.Command, CrowdCapsules, CrowdCapsules+CrowdPlayers)
	for i := range CrowdCapsules {
		a := float64(i) * 2 * math.Pi / CrowdCapsules
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
		out[i] = physx.Command{ID: uint64(i + 1), Acceleration: physx.Vec3{x * gain, 0, z * gain}}
	}
	return out
}
