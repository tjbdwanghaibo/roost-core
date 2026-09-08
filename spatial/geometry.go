// Package spatial provides game-agnostic two-dimensional integer-grid
// primitives without entity, scene, protocol, or gameplay dependencies.
//
// Scope: a uniform-grid ID index (BlockIndex), four-direction grid A*, and
// incremental interest management — InterestManager for one room
// (enter/leave hysteresis, distance bands for LOD, visible-set caps) and
// InterestCluster for seamless multi-room worlds on a shared coordinate
// plane (boundary mirroring, blink-free border migration). Cross-room
// seamlessness is within one process; cross-process handover additionally
// needs remote-entity ownership fences and subscription replication and is
// not provided here. There is still no Z axis and no navmesh; object data
// beyond ids and positions stays with the caller.
package spatial

import (
	"math"
	"math/bits"
)

type Point struct {
	X int64
	Y int64
}

func saturatingAdd(a, b int64) int64 {
	sum := a + b
	if b > 0 && sum < a {
		return math.MaxInt64
	}
	if b < 0 && sum > a {
		return math.MinInt64
	}
	return sum
}

func saturatingSub(a, b int64) int64 {
	if b == math.MinInt64 {
		return saturatingAdd(saturatingAdd(a, math.MaxInt64), 1)
	}
	return saturatingAdd(a, -b)
}

// Rect is half-open: Min is inclusive and Max is exclusive.
type Rect struct {
	Min Point
	Max Point
}

func NormalizeRect(rect Rect) Rect {
	if rect.Min.X > rect.Max.X {
		rect.Min.X, rect.Max.X = rect.Max.X, rect.Min.X
	}
	if rect.Min.Y > rect.Max.Y {
		rect.Min.Y, rect.Max.Y = rect.Max.Y, rect.Min.Y
	}
	return rect
}

func (r Rect) Empty() bool {
	r = NormalizeRect(r)
	return r.Min.X >= r.Max.X || r.Min.Y >= r.Max.Y
}

func (r Rect) Contains(point Point) bool {
	r = NormalizeRect(r)
	return point.X >= r.Min.X && point.X < r.Max.X && point.Y >= r.Min.Y && point.Y < r.Max.Y
}

func DistanceSquared(a, b Point) int64 {
	hi, lo := squaredDistance128(a, b)
	const maxInt64 = uint64(^uint64(0) >> 1)
	if hi != 0 || lo > maxInt64 {
		return int64(maxInt64)
	}
	return int64(lo)
}

// WithinDistance compares Euclidean distance without overflowing at int64
// coordinate extremes.
func WithinDistance(a, b Point, radius int64) bool {
	if radius < 0 {
		return false
	}
	distanceHi, distanceLo := squaredDistance128(a, b)
	radiusHi, radiusLo := bits.Mul64(uint64(radius), uint64(radius))
	return distanceHi < radiusHi || distanceHi == radiusHi && distanceLo <= radiusLo
}

func squaredDistance128(a, b Point) (uint64, uint64) {
	dx, dy := absDiff(a.X, b.X), absDiff(a.Y, b.Y)
	dxHi, dxLo := bits.Mul64(dx, dx)
	dyHi, dyLo := bits.Mul64(dy, dy)
	lo, carry := bits.Add64(dxLo, dyLo, 0)
	hi, overflow := bits.Add64(dxHi, dyHi, carry)
	if overflow != 0 {
		return ^uint64(0), ^uint64(0)
	}
	return hi, lo
}

func absDiff(a, b int64) uint64 {
	if a >= b {
		return uint64(a) - uint64(b)
	}
	return uint64(b) - uint64(a)
}
