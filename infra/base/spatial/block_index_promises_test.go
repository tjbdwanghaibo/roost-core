package spatial

import (
	"math"
	"testing"
)

// RR-20261005-NC-143：NewBlockIndex 接受的合法边界上，BlockRect 必须是一个落在 bounds 内、包含其块内全部点的
// 半开矩形。旧实现用 Min + (x+1)*blockSize 算最后一块的右/下边界，在 int64 上界附近溢出成负数，
// 返回倒置矩形（经 BlockRects 传给调用方），BlockIndex 说点在第 1 块、BlockRect(1) 却不包含它。
func TestBlockRectsStayInsideBoundsAtTheInt64Edge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bounds    Rect
		blockSize int64
		probes    []Point
	}{
		{
			name:      "upper edge, two huge blocks",
			bounds:    Rect{Max: Point{X: math.MaxInt64, Y: 1}},
			blockSize: 1 << 62,
			probes:    []Point{{X: 0}, {X: 1<<62 - 1}, {X: 1 << 62}, {X: math.MaxInt64 - 1}},
		},
		{
			name:      "upper edge on both axes",
			bounds:    Rect{Min: Point{X: 1, Y: 1}, Max: Point{X: math.MaxInt64, Y: math.MaxInt64}},
			blockSize: 1 << 62,
			probes:    []Point{{X: 1, Y: 1}, {X: math.MaxInt64 - 1, Y: math.MaxInt64 - 1}, {X: math.MaxInt64 / 2, Y: math.MaxInt64 - 1}},
		},
		{
			name:      "lower edge control",
			bounds:    Rect{Min: Point{X: math.MinInt64 + 1}, Max: Point{X: 0, Y: 1}},
			blockSize: 1 << 62,
			probes:    []Point{{X: math.MinInt64 + 1}, {X: -1}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index, err := NewBlockIndex(tc.bounds, tc.blockSize)
			if err != nil {
				t.Fatalf("NewBlockIndex refused a configuration this test relies on: %v", err)
			}
			for block := int64(0); block < index.cols*index.rows; block++ {
				rect := index.BlockRect(block)
				if rect.Min.X >= rect.Max.X || rect.Min.Y >= rect.Max.Y {
					t.Fatalf("block %d rect %+v is empty or inverted", block, rect)
				}
				if rect.Min.X < tc.bounds.Min.X || rect.Min.Y < tc.bounds.Min.Y || rect.Max.X > tc.bounds.Max.X || rect.Max.Y > tc.bounds.Max.Y {
					t.Fatalf("block %d rect %+v leaves bounds %+v", block, rect, tc.bounds)
				}
			}
			for _, point := range tc.probes {
				block := index.BlockIndex(point)
				if block < 0 {
					t.Fatalf("point %+v inside bounds has no block", point)
				}
				if rect := index.BlockRect(block); !rect.Contains(point) {
					t.Fatalf("point %+v is in block %d but BlockRect(%d) = %+v does not contain it", point, block, block, rect)
				}
				blocks, ok := index.BlockRects(Rect{Min: point, Max: Point{X: point.X + 1, Y: point.Y + 1}})
				if !ok || len(blocks) != 1 || !blocks[block].Contains(point) {
					t.Fatalf("BlockRects around %+v = %+v (ok %v), want block %d containing it", point, blocks, ok, block)
				}
			}
		})
	}
}
