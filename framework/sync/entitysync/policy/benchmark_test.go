package policy

import (
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 批量真实重算：空间移动与覆盖更新分别计时；不包含网络与初始化。
func BenchmarkAOIBatchRecompute(b *testing.B) {
	for _, dense := range []bool{false, true} {
		for _, coverage := range []bool{false, true} {
			for _, workers := range []int{1, 4} {
				b.Run(fmt.Sprintf("dense=%v/coverage=%v/workers=%d", dense, coverage, workers), func(b *testing.B) {
					a, err := NewAOI(AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 4000, Y: 4000}}, BlockSize: 100, EnterRadius: 120, LeaveRadius: 150, MaxVisible: 64})
					if err != nil {
						b.Fatal(err)
					}
					check := func(err error) {
						if err != nil {
							b.Fatal(err)
						}
					}
					spacing := int64(100)
					if dense {
						spacing = 10
					}
					positions := make([]spatial.Point, 512)
					for i := range positions {
						positions[i] = spatial.Point{X: 500 + int64(i%32)*spacing, Y: 500 + int64(i/32)*spacing}
						check(a.AddSubject(int64(i+1), positions[i]))
					}
					for i := range 128 {
						check(a.AddObserver(int64(i+1000), int64(i+1000), positions[i*4], ObserverView{}))
					}
					a.Flush()
					(blockSource{a}).Flush()
					step := 0
					b.ReportAllocs()
					for b.Loop() {
						a.beginBatch()
						for i := range 32 {
							id := int64(i + 1)
							if coverage {
								var blocks []int64
								if step%2 == 0 {
									blocks = []int64{a.BlockAt(positions[i]), a.BlockAt(spatial.Point{X: positions[i].X + 100, Y: positions[i].Y})}
								}
								check(a.SetObservedBlocks(id, blocks))
							} else {
								at := positions[i]
								at.X += int64(step%2) * 80
								check(a.MoveSubject(id, at))
							}
						}
						a.endBatch(workers)
						a.Flush()
						(blockSource{a}).Flush()
						step++
					}
				})
			}
		}
	}
}

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
					if err := aoi.AddObserver(int64(i), int64(i), spatial.Point{X: int64(i) * 1000, Y: 100}, ObserverView{}); err != nil {
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

// 相同存量与相同事件：每轮替换一个成员，比较全量 Set 和增量 ApplyDelta。
// 不包含事实队列、网络或业务 DAO；不能据此推断端到端吞吐。
func BenchmarkRelationMemberChange(b *testing.B) {
	for _, size := range []int{1, 32, 1024, 8192} {
		for _, delta := range []bool{false, true} {
			b.Run(fmt.Sprintf("size=%d/delta=%v", size, delta), func(b *testing.B) {
				s := NewRelationSource("team")
				ids := make([]int64, size)
				for i := range ids {
					ids[i] = int64(i + 1)
				}
				s.Set(10001, ids)
				s.Flush()
				b.ReportAllocs()
				for b.Loop() {
					previous := ids[0]
					if previous == 1 {
						ids[0] = int64(size + 1)
					} else {
						ids[0] = 1
					}
					if delta {
						s.ApplyDelta(10001, []int64{ids[0]}, []int64{previous})
					} else {
						s.Set(10001, ids)
					}
					events := s.Flush()
					if len(events) != 2 {
						b.Fatalf("expected enter/leave, got %v", events)
					}
				}
				if len(s.members[10001]) != size {
					b.Fatal("membership count changed")
				}
				for _, id := range ids {
					if _, ok := s.members[10001][id]; !ok {
						b.Fatal("incorrect final membership")
					}
				}
			})
		}
	}
}
