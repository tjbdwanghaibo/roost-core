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

// 集合规模与并发调用单独测量，避免只用单成员重复 Set 掩盖锁内构表成本。
func BenchmarkRelationSetSize(b *testing.B) {
	for _, size := range []int{32, 1024, 8192} {
		for _, parallel := range []bool{false, true} {
			b.Run(fmt.Sprintf("size=%d/parallel=%v", size, parallel), func(b *testing.B) {
				s := NewRelationSource("friends")
				ids := make([]int64, size)
				for i := range ids {
					ids[i] = int64(i + 1)
				}
				s.Set(1, ids)
				s.Flush()
				b.ReportAllocs()
				b.ResetTimer()
				if parallel {
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							s.Set(1, ids)
						}
					})
				} else {
					for range b.N {
						s.Set(1, ids)
					}
				}
			})
		}
	}
}
