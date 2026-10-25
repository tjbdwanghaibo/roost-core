package physics

import (
	_ "embed"
	"encoding/json"
	"math"

	"github.com/tjbdwanghaibo/roost-core/infra/physics/physx"
)

// 同一资源复制到 Unity Resources；坐标、半尺寸和坡度不在两端手工维护两份。
//
//go:embed terrain.json
var terrainJSON []byte

type TerrainBox struct {
	Name        string     `json:"name"`
	Position    physx.Vec3 `json:"position"`
	HalfExtents physx.Vec3 `json:"half_extents"`
	TiltDegrees float32    `json:"tilt_degrees"`
}

func TerrainBoxes() []TerrainBox {
	var scene struct {
		Boxes []TerrainBox `json:"boxes"`
	}
	if err := json.Unmarshal(terrainJSON, &scene); err != nil {
		panic("invalid embedded physics terrain: " + err.Error())
	}
	return scene.Boxes
}
func terrainBodies(boxes []TerrainBox) []physx.Body {
	out := make([]physx.Body, 0, len(boxes))
	for i, b := range boxes {
		angle := float64(b.TiltDegrees) * math.Pi / 180
		out = append(out, physx.Body{ID: uint64(1001 + i), Shape: physx.Box, Position: b.Position, Dimensions: b.HalfExtents, Rotation: physx.Quat{0, 0, float32(math.Sin(angle / 2)), float32(math.Cos(angle / 2))}})
	}
	return out
}

// 场景只使用绕 Z 轴倾斜的 box；求顶面高度并采样出生体积，避免直接生成在平台/坡道内。
func terrainHeight(boxes []TerrainBox, x, z, radius float32) float32 {
	height := float64(0)
	for _, ox := range []float32{-radius, 0, radius} {
		for _, oz := range []float32{-radius, 0, radius} {
			for _, b := range boxes {
				if math.Abs(float64(z+oz-b.Position[2])) > float64(b.HalfExtents[2]) {
					continue
				}
				angle := float64(b.TiltDegrees) * math.Pi / 180
				sine, cosine := math.Sin(angle), math.Cos(angle)
				dx := float64(x + ox - b.Position[0])
				dy := (float64(b.HalfExtents[1]) + sine*dx) / cosine
				localX := cosine*dx + sine*dy
				if math.Abs(localX) <= float64(b.HalfExtents[0]) {
					height = math.Max(height, float64(b.Position[1])+dy)
				}
			}
		}
	}
	return float32(height)
}
