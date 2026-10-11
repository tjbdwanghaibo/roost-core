package policy

import (
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 空闲与稀疏变化分别测量事件收集成本；不把地图初始化计入每轮成本。
func BenchmarkAOIEventCollection(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, move := range []bool{false, true} {
			b.Run(fmt.Sprintf("observers=%d/move=%v", count, move), func(b *testing.B) {
				aoi, err := NewAOI(AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: int64(count)*1000 + 1000, Y: 1000}}, BlockSize: 100, EnterRadius: 50, LeaveRadius: 60})
				if err != nil {
					b.Fatal(err)
				}
				for i := 1; i <= count; i++ {
					if err := aoi.AddObserver(int64(i), spatial.Point{X: int64(i) * 1000, Y: 100}); err != nil {
						b.Fatal(err)
					}
				}
				if err := aoi.AddSubject(-1, spatial.Point{X: 1000, Y: 100}); err != nil {
					b.Fatal(err)
				}
				aoi.Flush()
				blocks := blockSource{manager: aoi}
				blocks.Flush()
				b.ReportAllocs()
				step := 0
				for b.Loop() {
					if move {
						x := int64(1000 + (step%2)*100)
						if err := aoi.MoveSubject(-1, spatial.Point{X: x, Y: 100}); err != nil {
							b.Fatal(err)
						}
						step++
					}
					aoi.Flush()
					blocks.Flush()
				}
			})
		}
	}
}

func BenchmarkRelationUpdates(b *testing.B) {
	for _, count := range []int{10, 10000} {
		for _, forget := range []bool{false, true} {
			b.Run(fmt.Sprintf("observers=%d/forget=%v", count, forget), func(b *testing.B) {
				source := NewRelationSource("team")
				for i := 1; i <= count; i++ {
					source.Set(int64(i), []int64{int64(i + count)})
				}
				source.Flush()
				b.ReportAllocs()
				for b.Loop() {
					if forget {
						source.Forget(int64(count + 1))
					}
					source.Set(1, []int64{int64(count + 1)})
					source.Flush()
				}
			})
		}
	}
}
