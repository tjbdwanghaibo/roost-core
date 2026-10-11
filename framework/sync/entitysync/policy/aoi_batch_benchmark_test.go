package policy

import (
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"testing"
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
						check(a.AddObserver(int64(i+1000), positions[i*4]))
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
